package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"aihub/internal/model"
	"aihub/internal/provider"
)

const (
	// antigravityStreamPath streams Server-Sent Events; the plain path answers
	// with a single JSON object.
	antigravityStreamPath = "/v1internal:streamGenerateContent?alt=sse"
	antigravitySinglePath = "/v1internal:generateContent"

	// antigravityClientLabel is the userAgent field inside the request envelope,
	// which is separate from the HTTP User-Agent header.
	antigravityClientLabel = "antigravity"
	antigravityRequestType = "agent"

	// antigravityMaxBody bounds a non-streaming response body.
	antigravityMaxBody = 64 << 20
)

// userAgentProvider is implemented by providers that expose the User-Agent their
// vendor client sends.
type userAgentProvider interface {
	UserAgent(ctx context.Context) string
}

// antigravityExecutor proxies to Google's CodeAssist backend, which speaks the
// Gemini protocol inside a thin envelope.
type antigravityExecutor struct {
	client *http.Client
	tokens *provider.TokenManager
	vendor provider.Provider
	log    *slog.Logger
	// debug, when true, logs the request body, response status, headers
	// and body (bounded to 16 KiB) for every upstream call. The router
	// sets this from config.DebugRequests at construction time.
	debug bool
}

func newAntigravityExecutor(client *http.Client, tokens *provider.TokenManager, vendor provider.Provider, logger *slog.Logger) *antigravityExecutor {
	return &antigravityExecutor{client: client, tokens: tokens, vendor: vendor, log: logger}
}

func (e *antigravityExecutor) providerID() model.Provider { return model.ProviderAntigravity }

// passthrough forwards Gemini traffic verbatim; only the envelope is stripped.
func (e *antigravityExecutor) passthrough(format Format) bool {
	return format == FormatGemini
}

// antigravityProjectEnsurer is implemented by providers that can
// re-resolve a connection's project id at request time. The Antigravity
// provider implements it to dodge the "Cloud Code Private API has not
// been used in project …" 403 the upstream returns when the stored
// project id has gone stale (project deleted, API disabled, account
// re-onboarded by the IDE between requests). The interface lives here
// so the executor does not have to import the provider package.
type antigravityProjectEnsurer interface {
	EnsureProjectID(ctx context.Context, conn *model.Connection) error
}

func (e *antigravityExecutor) send(ctx context.Context, conn *model.Connection, req *Request, opts sendOptions) (*upstreamStream, error) {
	cred, err := e.tokens.Ensure(ctx, conn)
	if err != nil {
		return nil, asAPIError(model.ProviderAntigravity, err)
	}

	// Re-resolve the connection's project id before building the
	// envelope. This is the same defensive lookup OmniRoute does in
	// open-sse/services/antigravityProjectBootstrap.ts before every
	// content request: loadCodeAssist tells us which project Google
	// currently associates with the access token, and if it does not
	// match what we stored at onboarding time the upstream will 403
	// with "Cloud Code Private API has not been used in project …"
	// on the very next call. OnboardUser is called as a fallback when
	// loadCodeAssist reports no project at all.
	if ensurer, ok := e.vendor.(antigravityProjectEnsurer); ok {
		if ensureErr := ensurer.EnsureProjectID(ctx, conn); ensureErr != nil {
			// Non-fatal: log and forward so the upstream can return its
			// own error if the stored id really is no good.
			e.log.Debug("antigravity: project id refresh failed; forwarding with stored id",
				"connection", conn.ID, "error", ensureErr)
		}
	}

	inner := req.Raw
	if !opts.Raw {
		if inner, err = buildGeminiRequest(req); err != nil {
			return nil, asAPIError(model.ProviderAntigravity, err)
		}
	}
	body, err := wrapAntigravityRequest(req.Model, conn.ProjectID, inner, opts.Raw)
	if err != nil {
		return nil, asAPIError(model.ProviderAntigravity, err)
	}

	url := provider.AntigravityAPIEndpoint + antigravitySinglePath
	if req.Stream {
		url = provider.AntigravityAPIEndpoint + antigravityStreamPath
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, asAPIError(model.ProviderAntigravity, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	if agent, ok := e.vendor.(userAgentProvider); ok {
		httpReq.Header.Set("User-Agent", agent.UserAgent(ctx))
	}
	// X-Goog-Api-Client is NOT set on content requests. OmniRoute's
	// applyAntigravityClientProfileHeaders removes this header for both
	// the IDE and CLI profiles (see ABSENT_CONTENT_IDENTITY_HEADERS in
	// open-sse/services/antigravityClientProfile.ts); sending it with a
	// CLI User-Agent confuses the backend's identity gate and is one of
	// the things that triggered "Resource has been exhausted" 429s on
	// free-tier accounts. The header is only sent on the onboarding
	// endpoint (loadCodeAssist + onboardUser), which uses the IDE
	// Node-API client profile — see antigravity.go callAPI().
	// x-goog-user-project routes the request to the right Cloud Code
	// project; without it the backend falls back to the access token's
	// own project, which on a freshly-onboarded account may not have a
	// paid tier attached yet.
	if conn.ProjectID != "" {
		httpReq.Header.Set("X-Goog-User-Project", conn.ProjectID)
	}
	if req.Stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	} else {
		httpReq.Header.Set("Accept", "application/json")
	}

	if e.debug {
		e.logDebug("antigravity upstream request",
			"connection", conn.ID,
			"url", url,
			"stream", req.Stream,
			"project_id", conn.ProjectID,
			"model", req.Model,
			"request_body", truncateForLog(string(body), 16*1024),
		)
	}

	resp, err := e.client.Do(httpReq)
	if err != nil {
		return nil, asAPIError(model.ProviderAntigravity, err)
	}

	// Retry loop. OmniRoute's sendAntigravityRequest (open-sse/
	// executors/antigravity/executeAttempt.ts) handles two retry-able
	// shapes on this endpoint: 403 project-route errors (the proxy
	// drops X-Goog-User-Project and retries) and 429 transient RPM/TPM
	// rate limits (the proxy waits a short backoff and retries the
	// same connection). A request can hit both shapes in sequence —
	// the 403 retry can return 429, or vice versa — so the two retry
	// strategies must chain rather than be mutually-exclusive. The
	// loop below allows up to maxAntigravityRetries attempts and
	// tracks which strategies have already been used so we do not
	// loop forever on the same shape.
	const maxAntigravityRetries = 3
	var retriedProjectRoute, retriedTransient429 bool
	for attempt := 0; attempt < maxAntigravityRetries; attempt++ {
		if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
			break
		}

		body := readErrorBody(resp.Body)
		resp.Body.Close()
		if e.debug {
			e.logDebug("antigravity upstream error body",
				"connection", conn.ID,
				"attempt", attempt,
				"status", resp.StatusCode,
				"error_body", truncateForLog(string(body), 16*1024),
			)
		}

		// Decide whether to retry. The order matters: the 403
		// project-route retry refreshes the project id and drops
		// X-Goog-User-Project, the 429 transient retry sleeps 3 s
		// and resends with the same project id. Both strategies are
		// mutually-exclusive on the same attempt but can run in
		// sequence across attempts (a 403 retry that comes back
		// 429 will hit the 429 branch on the next iteration).
		var retryReq *http.Request
		var retryReason string
		switch {
		case resp.StatusCode == http.StatusForbidden &&
			antigravityGeoBlocked(body):
			// Geo-blocked is terminal — retrying the same
			// connection cannot help, and cooling it down
			// for a day stops the router from re-selecting
			// it on every request. Surface a clear "set
			// AIHUB_PROXY_URL" message so the operator knows
			// the actual fix.
			return nil, &APIError{
				Status: http.StatusForbidden,
				Type:   "geo_blocked",
				Code:   "unsupported_location",
				Message: "antigravity: Google Code Assist rejected this request because the server's egress region is not supported. " +
					"Gemini Code Assist for individuals (free-tier) is only available from a short list of regions; the server's current egress IP falls outside that list. " +
					"Route upstream traffic through a proxy in a supported region (US/EU/SG) by setting AIHUB_PROXY_URL, or deploy the proxy itself in a supported region.\n\n" +
					truncate(string(body), 1500),
				Upstream:  resp.StatusCode,
				Retryable: false,
			}
		case resp.StatusCode == http.StatusForbidden &&
			antigravityProjectRouteError(body) && !retriedProjectRoute:
			if ensurer, ok := e.vendor.(antigravityProjectEnsurer); ok {
				e.log.Info("antigravity: 403 project-route error; refreshing project id and retrying once",
					"connection", conn.ID, "project", conn.ProjectID, "attempt", attempt)
				if ensureErr := ensurer.EnsureProjectID(ctx, conn); ensureErr != nil {
					e.log.Debug("antigravity: project id refresh on 403 failed",
						"connection", conn.ID, "error", ensureErr)
				}
			}
			retryReason = "403 project-route"
			retriedProjectRoute = true
		case resp.StatusCode == http.StatusTooManyRequests &&
			antigravityTransientRateLimit(body) && !retriedTransient429:
			// Wait a short backoff. The request context is the one
			// the client gave us, so honour cancellation but do not
			// bound the wait on it alone — a 1-second context would
			// defeat the retry.
			select {
			case <-ctx.Done():
				return nil, asAPIError(model.ProviderAntigravity, ctx.Err())
			case <-time.After(3 * time.Second):
			}
			retryReason = "429 transient"
			retriedTransient429 = true
		default:
			// Not retry-able (or already retried this shape):
			// surface the upstream error. Detect the 403
			// project-route shape's enable-API console link so
			// the operator knows to open the GCP console and
			// click Enable — the proxy already did what it could.
			if resp.StatusCode == http.StatusForbidden && antigravityProjectRouteError(body) {
				return nil, &APIError{
					Status: http.StatusForbidden,
					Type:   "project_route_error",
					Code:   "cloud_code_api_disabled",
					Message: "antigravity: the project Google assigned to this connection (" + conn.ProjectID + ") does not have the Cloud Code API enabled. " +
						"Open the link the upstream returned and click \"Enable\", then send another request.\n\n" +
						truncate(string(body), 1500),
					Upstream:  resp.StatusCode,
					Retryable: false,
				}
			}
			return nil, apiErrorFromResponse(model.ProviderAntigravity,
				resp.StatusCode, resp.Header, body)
		}

		// Build the retry request. The envelope is rebuilt with a
		// fresh sessionId so Google does not dedupe the second
		// request as a replay of the first.
		body2, bodyErr := wrapAntigravityRequest(req.Model, conn.ProjectID, inner, opts.Raw)
		if bodyErr != nil {
			return nil, asAPIError(model.ProviderAntigravity, bodyErr)
		}
		retryReq, retryErr := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body2))
		if retryErr != nil {
			return nil, asAPIError(model.ProviderAntigravity, retryErr)
		}
		retryReq.Header.Set("Content-Type", "application/json")
		retryReq.Header.Set("Authorization", "Bearer "+cred.AccessToken)
		if agent, ok := e.vendor.(userAgentProvider); ok {
			retryReq.Header.Set("User-Agent", agent.UserAgent(ctx))
		}
		// OmniRoute's sendAntigravityRequest removes x-goog-user-project
		// on a 403 retry (see executeAttempt.ts:366) — some projects
		// reject that header when the Cloud Code API is enabled-but-
		// stale, and dropping it lets Google re-resolve the project
		// from the access token. The 429 transient retry keeps the
		// header because the project itself is fine; only the per-minute
		// bucket is full.
		if retryReason == "429 transient" && conn.ProjectID != "" {
			retryReq.Header.Set("X-Goog-User-Project", conn.ProjectID)
		}
		if req.Stream {
			retryReq.Header.Set("Accept", "text/event-stream")
		} else {
			retryReq.Header.Set("Accept", "application/json")
		}
		if e.debug {
			e.logDebug("antigravity upstream retry",
				"connection", conn.ID,
				"reason", retryReason,
				"attempt", attempt,
				"project_id", conn.ProjectID,
				"request_body", truncateForLog(string(body2), 16*1024),
			)
		}
		retryResp, retryErr := e.client.Do(retryReq)
		if retryErr != nil {
			return nil, asAPIError(model.ProviderAntigravity, retryErr)
		}
		if e.debug {
			e.logDebug("antigravity upstream retry response status",
				"connection", conn.ID,
				"reason", retryReason,
				"attempt", attempt,
				"status", retryResp.StatusCode,
				"content_type", retryResp.Header.Get("Content-Type"),
			)
		}
		// Replace resp so the loop's next iteration (or the
		// post-loop success path) sees the retry's result.
		resp = retryResp
		opts.RetryedProjectRoute = retriedProjectRoute || retriedTransient429
	}

	stream := &upstreamStream{Header: resp.Header, Body: resp.Body}
	if req.Stream {
		stream.scanner = newSSEScanner(resp.Body)
	} else {
		// A single JSON object: turn it into one frame so both consumers see the
		// same shape.
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, antigravityMaxBody))
		resp.Body.Close()
		stream.Body = nil
		if readErr != nil {
			return nil, asAPIError(model.ProviderAntigravity, readErr)
		}
		unwrapped := antigravityUnwrap(raw)
		if len(unwrapped) == 0 {
			unwrapped = raw
		}
		stream.pre = []sseEvent{{Data: string(unwrapped)}}
		stream.aggregate = unwrapped
	}

	if opts.Raw {
		stream.sniff = geminiSniffUsage
		stream.rewrite = func(frame sseEvent) (sseEvent, bool) {
			unwrapped := antigravityUnwrap([]byte(frame.Data))
			if len(unwrapped) == 0 {
				return frame, true
			}
			return sseEvent{Name: frame.Name, Data: string(unwrapped)}, true
		}
		return stream, nil
	}

	decoder := newGeminiDecoder(nil)
	stream.decode = func(frame sseEvent) ([]Event, error) {
		payload := []byte(frame.Data)
		if unwrapped := antigravityUnwrap(payload); len(unwrapped) > 0 {
			payload = unwrapped
		}
		if err := antigravityFrameError(payload); err != nil {
			return nil, err
		}
		return decoder.decode(payload)
	}
	stream.trailer = func() []Event { return []Event{decoder.finalEvent()} }
	return stream, nil
}

// ---------------------------------------------------------------------------
// Envelope
// ---------------------------------------------------------------------------

// wrapAntigravityRequest puts a Gemini body inside the CodeAssist envelope and
// applies the quirks the backend requires.
func wrapAntigravityRequest(modelID, projectID string, inner []byte, raw bool) ([]byte, error) {
	var request map[string]any
	if err := json.Unmarshal(inner, &request); err != nil {
		return nil, fmt.Errorf("parse request body: %w", err)
	}

	// Safety settings are not accepted here; the backend applies its own.
	delete(request, "safetySettings")
	// These belong to the envelope, not the inner request.
	delete(request, "model")
	delete(request, "project")

	if raw {
		sanitizeGeminiToolSchemas(request)
	}

	isClaude := strings.Contains(strings.ToLower(modelID), "claude")
	if isClaude {
		// Anthropic models behind this backend reject unvalidated tool calls.
		toolConfig, _ := request["toolConfig"].(map[string]any)
		if toolConfig == nil {
			toolConfig = map[string]any{}
			request["toolConfig"] = toolConfig
		}
		callingConfig, _ := toolConfig["functionCallingConfig"].(map[string]any)
		if callingConfig == nil {
			callingConfig = map[string]any{}
			toolConfig["functionCallingConfig"] = callingConfig
		}
		callingConfig["mode"] = "VALIDATED"
	} else if generation, ok := request["generationConfig"].(map[string]any); ok {
		// Every other model on this backend errors out when an output cap is set.
		delete(generation, "maxOutputTokens")
		if len(generation) == 0 {
			delete(request, "generationConfig")
		}
	}

	request["sessionId"] = uuid.NewString()

	envelope := map[string]any{
		"model":       modelID,
		"userAgent":   antigravityClientLabel,
		"requestType": antigravityRequestType,
		"requestId":   "agent-" + uuid.NewString(),
		"request":     request,
		// The Cloud Code backend gates tier access on this field. Without
		// it the request is billed against the free tier — which on a
		// paid account that still has GOOGLE_ONE_AI credits surfaces as
		// "Resource has been exhausted (e.g. check quota)" on the very
		// first request, even though the operator's account is fine.
		// OmniRoute (open-sse/services/usage/antigravity.ts) and
		// CLIProxyAPI both inject this explicitly; we mirror the same
		// field here so the upstream applies the paid tier the operator
		// paid for.
		"enabledCreditTypes": []string{"GOOGLE_ONE_AI"},
	}
	if projectID != "" {
		envelope["project"] = projectID
	}

	encoded, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("encode antigravity request: %w", err)
	}
	return encoded, nil
}

// sanitizeGeminiToolSchemas rewrites the schemas inside a forwarded Gemini body.
// Only declarations are touched: a functionCall's arguments are data being
// replayed, not a schema, and must survive untouched.
func sanitizeGeminiToolSchemas(request map[string]any) {
	tools, _ := request["tools"].([]any)
	for _, entry := range tools {
		tool, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		declarations, _ := tool["functionDeclarations"].([]any)
		for _, item := range declarations {
			declaration, ok := item.(map[string]any)
			if !ok {
				continue
			}
			// The newer JSON-Schema field name is not understood here.
			if schema, exists := declaration["parametersJsonSchema"]; exists {
				delete(declaration, "parametersJsonSchema")
				if _, hasParameters := declaration["parameters"]; !hasParameters {
					declaration["parameters"] = schema
				}
			}
			if schema, exists := declaration["parameters"]; exists {
				if cleaned := sanitizeSchemaNode(schema); cleaned != nil {
					declaration["parameters"] = cleaned
				} else {
					delete(declaration, "parameters")
				}
			}
		}
	}

	if generation, ok := request["generationConfig"].(map[string]any); ok {
		if schema, exists := generation["responseJsonSchema"]; exists {
			delete(generation, "responseJsonSchema")
			if _, hasSchema := generation["responseSchema"]; !hasSchema {
				generation["responseSchema"] = schema
			}
		}
		if schema, exists := generation["responseSchema"]; exists {
			if cleaned := sanitizeSchemaNode(schema); cleaned != nil {
				generation["responseSchema"] = cleaned
			} else {
				delete(generation, "responseSchema")
			}
		}
	}
}

// antigravityUnwrap peels the {"response": …} envelope off a reply. It returns
// nil when the payload is not wrapped.
func antigravityUnwrap(data []byte) []byte {
	trimmed := bytes.TrimSpace(data)
	if !bytes.HasPrefix(trimmed, []byte("{")) {
		return nil
	}
	var envelope struct {
		Response json.RawMessage `json:"response"`
	}
	if err := json.Unmarshal(trimmed, &envelope); err != nil {
		return nil
	}
	if len(envelope.Response) == 0 {
		return nil
	}
	return envelope.Response
}

// antigravityFrameError reports an error object delivered inside the stream.
func antigravityFrameError(payload []byte) error {
	trimmed := bytes.TrimSpace(payload)
	if !bytes.HasPrefix(trimmed, []byte("{")) || !bytes.Contains(trimmed, []byte(`"error"`)) {
		return nil
	}
	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(trimmed, &envelope); err != nil || len(envelope.Error) == 0 {
		return nil
	}
	message, code, errType := parseUpstreamError(trimmed)
	if message == "" {
		return nil
	}
	return &APIError{
		Status:   http.StatusBadGateway,
		Type:     firstNonEmpty(errType, "api_error"),
		Code:     code,
		Message:  "antigravity: " + truncate(message, 2000),
		Upstream: http.StatusBadGateway,
	}
}

// geminiSniffUsage reads usageMetadata out of a raw frame.
func geminiSniffUsage(frame sseEvent) *Usage {
	payload := []byte(frame.Data)
	if unwrapped := antigravityUnwrap(payload); len(unwrapped) > 0 {
		payload = unwrapped
	}
	var chunk struct {
		UsageMetadata *struct {
			PromptTokenCount        int64 `json:"promptTokenCount"`
			CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
			ThoughtsTokenCount      int64 `json:"thoughtsTokenCount"`
			CachedContentTokenCount int64 `json:"cachedContentTokenCount"`
			TotalTokenCount         int64 `json:"totalTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(payload, &chunk); err != nil || chunk.UsageMetadata == nil {
		return nil
	}
	return &Usage{
		PromptTokens:     chunk.UsageMetadata.PromptTokenCount,
		CompletionTokens: chunk.UsageMetadata.CandidatesTokenCount,
		ReasoningTokens:  chunk.UsageMetadata.ThoughtsTokenCount,
		CachedTokens:     chunk.UsageMetadata.CachedContentTokenCount,
		TotalTokens:      chunk.UsageMetadata.TotalTokenCount,
	}
}

// antigravityProjectRouteError reports whether body is the Cloud Code
// "Cloud Code Private API has not been used in project …" 403 (or one
// of its siblings). These errors all mean the project id on the
// request is no longer usable upstream and the executor should
// re-resolve it through loadCodeAssist + onboardUser before retrying.
//
// The shape mirrors OmniRoute's recoverableProject403 classifier
// (open-sse/services/errorClassifier.ts:348), which itself was tuned
// against the Cloud Code backend's error wording over many releases.
func antigravityProjectRouteError(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	text := strings.ToLower(string(body))
	return strings.Contains(text, "has not been used in project") ||
		strings.Contains(text, "service_disabled") ||
		strings.Contains(text, "accessnotconfiguredured") ||
		strings.Contains(text, "permission_denied") ||
		strings.Contains(text, "it is disabled")
}

// logDebug wraps e.log.Debug so the call sites stay readable. It is
// only ever called under the e.debug guard, so the structured-logging
// argument list is built unconditionally — slog already short-circuits
// arguments at the API level when the level is filtered out, and the
// debug path here means the level filter has already been turned off
// by AIHUB_DEBUG_REQUESTS=true.
func (e *antigravityExecutor) logDebug(msg string, args ...any) {
	if e == nil || e.log == nil {
		return
	}
	e.log.Debug(msg, args...)
}

// truncateForLog bounds the size of a string so a single chat
// completion with a 100-KB body does not flood the log. The default
// 16 KiB is enough to see the system prompt and the first turn, which
// is what an operator chasing an upstream 400 needs.
func truncateForLog(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}

// antigravityTransientRateLimit reports whether body is the Cloud Code
// 429 that means "transient per-minute RPM/TPM rate limit", as opposed
// to "depleted quota". OmniRoute's accountFallback.ts:202-204 spells
// the distinction out:
//
//	"narrower than a bare 'has been exhausted' — that generic phrase also
//	appears in Gemini's transient RPM/TPM 429 body ('Resource has been
//	exhausted (e.g. check quota).'), which must stay RATE_LIMIT_EXCEEDED,
//	not terminal."
//
// Real quota exhaustion uses the more specific phrasings matched by
// antigravityQuotaExhausted below; everything else that says "Resource
// has been exhausted" / "RESOURCE_EXHAUSTED" / "rate limit" is treated
// as transient and retried with a short backoff before failing the
// request.
func antigravityTransientRateLimit(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	text := strings.ToLower(string(body))
	if antigravityQuotaExhausted(text) {
		return false
	}
	return strings.Contains(text, "resource has been exhausted") ||
		strings.Contains(text, "resource_exhausted") ||
		strings.Contains(text, "rate limit") ||
		strings.Contains(text, "rate_limit") ||
		strings.Contains(text, "too many requests") ||
		strings.Contains(text, "per minute") ||
		strings.Contains(text, "rpm") ||
		strings.Contains(text, "try again")
}

// antigravityQuotaExhausted reports whether body carries one of the
// specific signals OmniRoute (accountFallback.ts CREDITS_EXHAUSTED_SIGNALS)
// uses to mark an account as terminal-credits-exhausted. These are
// per-day / per-month quota depletions (or paid-tier credit
// depletions) — the connection should be cooled down for a long
// time and the router should fail over to another account, not
// retry the same one.
func antigravityQuotaExhausted(loweredBody string) bool {
	if loweredBody == "" {
		return false
	}
	return strings.Contains(loweredBody, "exceeded your current usage quota") ||
		strings.Contains(loweredBody, "credit_balance_too_low") ||
		strings.Contains(loweredBody, "your credit balance is too low") ||
		strings.Contains(loweredBody, "credits exhausted") ||
		strings.Contains(loweredBody, "out of credits") ||
		strings.Contains(loweredBody, "payment required") ||
		strings.Contains(loweredBody, "free tier of the model has been exhausted") ||
		strings.Contains(loweredBody, "tier has been exhausted") ||
		strings.Contains(loweredBody, "insufficient balance") ||
		strings.Contains(loweredBody, "insufficient_balance") ||
		strings.Contains(loweredBody, "insufficient account balance") ||
		strings.Contains(loweredBody, "insufficient credit balance") ||
		strings.Contains(loweredBody, "insufficient credits") ||
		strings.Contains(loweredBody, "insufficient credit") ||
		strings.Contains(loweredBody, "quota_exhausted") ||
		strings.Contains(loweredBody, "quota exhausted") ||
		strings.Contains(loweredBody, "quota reached") ||
		strings.Contains(loweredBody, "enable overages") ||
		strings.Contains(loweredBody, "individual quota") ||
		strings.Contains(loweredBody, "daily limit") ||
		strings.Contains(loweredBody, "exhausted your capacity") ||
		strings.Contains(loweredBody, "free tier")
}

// antigravityGeoBlocked reports whether body carries Google's
// regional-availability refusal ("User location is not supported for
// the API use", "is not currently available in your location",
// "UNSUPPORTED_LOCATION", etc.). The Cloud Code backend only offers
// Gemini Code Assist for individuals (free-tier) from a short list of
// regions; a server egressing outside that list sees this 403 on
// every request, regardless of which account is used.
//
// OmniRoute classifies this as GEO_BLOCKED in
// open-sse/services/errorClassifier.ts:127. This binary does the same
// here so the operator gets a clear "set AIHUB_PROXY_URL" message
// instead of an opaque 502 or a misleading "Cloud Code API disabled"
// project-route error.
func antigravityGeoBlocked(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	text := strings.ToLower(string(body))
	return strings.Contains(text, "unsupported_location") ||
		strings.Contains(text, "user location is not supported") ||
		strings.Contains(text, "location is not supported") ||
		strings.Contains(text, "not supported for the api use") ||
		strings.Contains(text, "region is not supported") ||
		strings.Contains(text, "unsupported location") ||
		strings.Contains(text, "not available in your location") ||
		strings.Contains(text, "not available in your region") ||
		strings.Contains(text, "not currently available in your location")
}
