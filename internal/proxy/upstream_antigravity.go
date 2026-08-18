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
	// The Cloud Code backend gates tier access on the X-Goog-Api-Client
	// header matching what the IDE's Node-API client sends. Missing it on a
	// request whose User-Agent otherwise looks like the IDE is one of the
	// signals that triggers "Resource has been exhausted" even on accounts
	// that still have GOOGLE_ONE_AI credits (OmniRoute #8098).
	httpReq.Header.Set("X-Goog-Api-Client", "gl-node/22.21.1")
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
	if e.debug {
		e.logDebug("antigravity upstream response status",
			"connection", conn.ID,
			"status", resp.StatusCode,
			"content_type", resp.Header.Get("Content-Type"),
		)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body := readErrorBody(resp.Body)
		resp.Body.Close()
		if e.debug {
			e.logDebug("antigravity upstream error body",
				"connection", conn.ID,
				"status", resp.StatusCode,
				"error_body", truncateForLog(string(body), 16*1024),
			)
		}

		// Cloud Code 403 with "has not been used in project …"
		// means the project id we just sent is stale: Google
		// disabled the API on it, deleted it, or the IDE
		// re-onboarded the account to a different project
		// since we last looked. Force a fresh onboard via
		// EnsureProjectID (which calls loadCodeAssist again
		// and, when that returns nothing, provisions a new
		// project through onboardUser), then retry the
		// request once with the new project id. A second 403
		// with the same shape is allowed to surface — we do
		// not loop.
		if resp.StatusCode == http.StatusForbidden &&
			antigravityProjectRouteError(body) && !opts.RetryedProjectRoute {
			if ensurer, ok := e.vendor.(antigravityProjectEnsurer); ok {
				e.log.Info("antigravity: 403 project-route error; refreshing project id and retrying once",
					"connection", conn.ID, "project", conn.ProjectID)
				if ensureErr := ensurer.EnsureProjectID(ctx, conn); ensureErr != nil {
					e.log.Debug("antigravity: project id refresh on 403 failed",
						"connection", conn.ID, "error", ensureErr)
				}
				// Rebuild the envelope with the (possibly) new project id.
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
				retryReq.Header.Set("X-Goog-Api-Client", "gl-node/22.21.1")
				// OmniRoute's sendAntigravityRequest removes
				// x-goog-user-project on a 403 retry (see
				// open-sse/executors/antigravity/executeAttempt.ts:366).
				// Some projects reject the header when the
				// Cloud Code API is enabled-but-stale, and
				// dropping it lets Google re-resolve the
				// project from the access token. The
				// envelope body still carries the project
				// id so the upstream knows which project
				// to bill against when the request lands.
				// Do NOT set X-Goog-User-Project here.
				if req.Stream {
					retryReq.Header.Set("Accept", "text/event-stream")
				} else {
					retryReq.Header.Set("Accept", "application/json")
				}

				retryResp, retryErr := e.client.Do(retryReq)
				if retryErr != nil {
					return nil, asAPIError(model.ProviderAntigravity, retryErr)
				}
				if retryResp.StatusCode < 200 || retryResp.StatusCode > 299 {
					// The retry also failed. When it failed with the
					// same project-route shape, surface the upstream's
					// enable-API console link verbatim so the operator
					// knows to open the GCP console and click Enable —
					// the proxy already did what it could.
					defer retryResp.Body.Close()
					retryBody := readErrorBody(retryResp.Body)
					if antigravityProjectRouteError(retryBody) {
						return nil, &APIError{
							Status: http.StatusForbidden,
							Type:   "project_route_error",
							Code:   "cloud_code_api_disabled",
							Message: "antigravity: the project Google assigned to this connection (" + conn.ProjectID + ") does not have the Cloud Code API enabled. " +
								"The proxy already retried once after re-running loadCodeAssist + onboardUser, but Google returned the same project. " +
								"Open the link the upstream returned and click \"Enable\", then send another request — the proxy will pick up the change without a restart.\n\n" +
								truncate(string(retryBody), 1500),
							Upstream:  retryResp.StatusCode,
							Retryable: false,
						}
					}
					return nil, apiErrorFromResponse(model.ProviderAntigravity,
						retryResp.StatusCode, retryResp.Header, retryBody)
				}
				// Retry succeeded: replace the original response so the
				// rest of send() consumes it as if the 403 never happened.
				resp = retryResp
				opts.RetryedProjectRoute = true
			}
		} else {
			return nil, apiErrorFromResponse(model.ProviderAntigravity, resp.StatusCode, resp.Header, body)
		}
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
