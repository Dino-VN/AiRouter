package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"aihub/internal/model"
	"aihub/internal/provider"
	"aihub/internal/proxy"
	"aihub/internal/store"
)

// Headers a client can use to steer routing explicitly instead of letting the
// model id decide.
const (
	headerProvider   = "X-Aihub-Provider"
	headerConnection = "X-Aihub-Connection"
)

// proxyChat serves POST /v1/chat/completions.
func (s *Server) proxyChat(w http.ResponseWriter, r *http.Request) {
	s.serveProxy(w, r, proxy.FormatOpenAIChat, "", nil)
}

// proxyResponses serves POST /v1/responses.
func (s *Server) proxyResponses(w http.ResponseWriter, r *http.Request) {
	s.serveProxy(w, r, proxy.FormatOpenAIResponses, "", nil)
}

// proxyMessages serves POST /v1/messages.
func (s *Server) proxyMessages(w http.ResponseWriter, r *http.Request) {
	s.serveProxy(w, r, proxy.FormatAnthropic, "", nil)
}

// proxyGemini serves POST /v1beta/models/{model}:{action}. The Gemini API puts
// both the model and the streaming decision in the path.
func (s *Server) proxyGemini(w http.ResponseWriter, r *http.Request) {
	modelID, action, _ := strings.Cut(r.PathValue("model"), ":")
	modelID = trim(modelID)

	var forceStream *bool
	switch action {
	case "generateContent":
		forceStream = ptr(false)
	case "streamGenerateContent":
		forceStream = ptr(true)
	case "":
		proxy.WriteError(w, proxy.FormatGemini, http.StatusBadRequest, "invalid_request_error",
			"the path must name an action, as in /v1beta/models/"+modelID+":generateContent")
		return
	default:
		// countTokens and the embedding methods have no equivalent on either
		// upstream, so they are refused rather than silently mis-served.
		proxy.WriteError(w, proxy.FormatGemini, http.StatusNotImplemented, "invalid_request_error",
			"this proxy does not implement models."+action)
		return
	}
	if modelID == "" {
		proxy.WriteError(w, proxy.FormatGemini, http.StatusBadRequest, "invalid_request_error",
			"the path must name a model")
		return
	}

	s.serveProxy(w, r, proxy.FormatGemini, modelID, forceStream)
}

// serveProxy authenticates, enforces the caller's quota and hands the request to
// the router, which owns the upstream call and the response.
func (s *Server) serveProxy(w http.ResponseWriter, r *http.Request, format proxy.Format,
	pathModel string, forceStream *bool) {

	caller, apiErr := s.authenticateProxy(r)
	if apiErr != nil {
		writeProxyError(w, format, apiErr)
		return
	}

	body, apiErr := readProxyBody(r)
	if apiErr != nil {
		writeProxyError(w, format, apiErr)
		return
	}

	quota, err := s.store.GetQuota(r.Context(), caller.User.ID)
	if err != nil {
		writeProxyError(w, format, storeError(err, "load quota"))
		return
	}
	if apiErr = s.enforceModelAllowList(caller, pathModel, body); apiErr != nil {
		writeProxyError(w, format, apiErr)
		return
	}
	if apiErr = s.enforceQuota(r.Context(), caller.User.ID, quota); apiErr != nil {
		writeProxyError(w, format, apiErr)
		return
	}

	// The concurrency limit is held for the whole upstream call, streaming
	// included, which is the point: it bounds simultaneous load per account.
	if !s.inflight.acquire(caller.User.ID, quota.ConcurrentLimit) {
		writeProxyError(w, format, errorf(http.StatusTooManyRequests, "concurrency_limit",
			"you already have %d requests in flight, which is your limit", quota.ConcurrentLimit))
		return
	}
	defer s.inflight.release(caller.User.ID)

	call := proxy.Call{
		Format:           format,
		Body:             body,
		PathModel:        pathModel,
		ForceStream:      forceStream,
		User:             caller.User,
		AllowShared:      quota.AllowSharedPool || caller.User.IsAdmin(),
		AllowedProviders: quota.AllowedProviders,
	}
	if caller.APIKey != nil {
		call.APIKeyID = &caller.APIKey.ID
	}
	if apiErr = applyRoutingHeaders(r, &call); apiErr != nil {
		writeProxyError(w, format, apiErr)
		return
	}

	ctx := r.Context()
	if timeout := s.requestTimeout(); timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	outcome := s.router.Complete(ctx, w, call)

	// The router already recorded the usage row; this line is for the operator
	// watching the log.
	level := logLevelForStatus(outcome.Status)
	s.log.Log(ctx, level, "proxied request",
		"user", caller.User.Username, "format", format, "model", outcome.Model,
		"provider", outcome.Provider, "connection", outcome.ConnectionID,
		"stream", outcome.Stream, "status", outcome.Status,
		"prompt_tokens", outcome.Usage.PromptTokens,
		"completion_tokens", outcome.Usage.CompletionTokens,
		"total_tokens", outcome.Usage.Total(),
		"latency", outcome.Latency.Round(time.Millisecond),
		"error", errorText(outcome.Err))

	if caller.APIKey != nil {
		// Detached from the request context so a cancelled stream still records
		// that the key was used.
		touchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err = s.store.TouchAPIKey(touchCtx, caller.APIKey.ID); err != nil {
			s.log.Debug("could not record api key use", "key", caller.APIKey.ID, "error", err)
		}
	}
}

// ---------------------------------------------------------------------------
// Model listings
// ---------------------------------------------------------------------------

// listProxyModels serves GET /v1/models in the OpenAI shape.
func (s *Server) listProxyModels(w http.ResponseWriter, r *http.Request) {
	models, apiErr := s.availableModels(r)
	if apiErr != nil {
		writeProxyError(w, proxy.FormatOpenAIChat, apiErr)
		return
	}

	data := make([]map[string]any, 0, len(models))
	for _, info := range models {
		data = append(data, openAIModel(info))
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// getProxyModel serves GET /v1/models/{model}.
func (s *Server) getProxyModel(w http.ResponseWriter, r *http.Request) {
	models, apiErr := s.availableModels(r)
	if apiErr != nil {
		writeProxyError(w, proxy.FormatOpenAIChat, apiErr)
		return
	}

	wanted := trim(r.PathValue("model"))
	for _, info := range models {
		if strings.EqualFold(info.ID, wanted) {
			writeJSON(w, http.StatusOK, openAIModel(info))
			return
		}
	}
	writeProxyError(w, proxy.FormatOpenAIChat, errorf(http.StatusNotFound, "model_not_found",
		"no model %q is available to this key", wanted))
}

// listGeminiModels serves GET /v1beta/models in the Google shape.
func (s *Server) listGeminiModels(w http.ResponseWriter, r *http.Request) {
	models, apiErr := s.availableModels(r)
	if apiErr != nil {
		writeProxyError(w, proxy.FormatGemini, apiErr)
		return
	}

	list := make([]map[string]any, 0, len(models))
	for _, info := range models {
		list = append(list, geminiModel(info))
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": list})
}

// getGeminiModel serves GET /v1beta/models/{model}.
func (s *Server) getGeminiModel(w http.ResponseWriter, r *http.Request) {
	models, apiErr := s.availableModels(r)
	if apiErr != nil {
		writeProxyError(w, proxy.FormatGemini, apiErr)
		return
	}

	// Clients ask for "models/gemini-2.5-pro", and some append an action.
	wanted, _, _ := strings.Cut(trim(r.PathValue("model")), ":")
	wanted = strings.TrimPrefix(wanted, "models/")
	for _, info := range models {
		if strings.EqualFold(info.ID, wanted) {
			writeJSON(w, http.StatusOK, geminiModel(info))
			return
		}
	}
	writeProxyError(w, proxy.FormatGemini, errorf(http.StatusNotFound, "model_not_found",
		"no model %q is available to this key", wanted))
}

// availableModels authenticates the caller and lists what they can reach.
func (s *Server) availableModels(r *http.Request) ([]provider.ModelInfo, *apiError) {
	caller, apiErr := s.authenticateProxy(r)
	if apiErr != nil {
		return nil, apiErr
	}
	quota, err := s.store.GetQuota(r.Context(), caller.User.ID)
	if err != nil {
		return nil, storeError(err, "load quota")
	}

	models := s.router.AvailableModels(r.Context(), caller.User,
		quota.AllowSharedPool || caller.User.IsAdmin())

	// A key restricted to a few models should only advertise those.
	if caller.APIKey != nil && len(caller.APIKey.AllowedModels) > 0 {
		filtered := make([]provider.ModelInfo, 0, len(models))
		for _, info := range models {
			if modelAllowed(caller.APIKey.AllowedModels, info.ID) {
				filtered = append(filtered, info)
			}
		}
		models = filtered
	}
	if len(quota.AllowedProviders) > 0 {
		filtered := make([]provider.ModelInfo, 0, len(models))
		for _, info := range models {
			if providerAllowed(quota, info.Provider) {
				filtered = append(filtered, info)
			}
		}
		models = filtered
	}
	return models, nil
}

// openAIModel renders one model the way the OpenAI models endpoint does, with a
// few extra descriptive fields clients are free to ignore.
func openAIModel(info provider.ModelInfo) map[string]any {
	created := info.Created
	if created == 0 {
		created = 1700000000
	}
	out := map[string]any{
		"id":       info.ID,
		"object":   "model",
		"created":  created,
		"owned_by": firstNonEmpty(info.OwnedBy, string(info.Provider)),
		"provider": info.Provider,
	}
	if info.DisplayName != "" {
		out["display_name"] = info.DisplayName
	}
	if info.ContextLength > 0 {
		out["context_length"] = info.ContextLength
	}
	if info.MaxCompletionTokens > 0 {
		out["max_completion_tokens"] = info.MaxCompletionTokens
	}
	return out
}

// geminiModel renders one model the way the Google models endpoint does.
func geminiModel(info provider.ModelInfo) map[string]any {
	out := map[string]any{
		"name":                       "models/" + info.ID,
		"displayName":                firstNonEmpty(info.DisplayName, info.ID),
		"description":                info.Description,
		"supportedGenerationMethods": []string{"generateContent", "streamGenerateContent"},
		"provider":                   info.Provider,
	}
	if info.ContextLength > 0 {
		out["inputTokenLimit"] = info.ContextLength
	}
	if info.MaxCompletionTokens > 0 {
		out["outputTokenLimit"] = info.MaxCompletionTokens
	}
	return out
}

// ---------------------------------------------------------------------------
// Authentication
// ---------------------------------------------------------------------------

// authenticateProxy accepts a proxy API key in any of the four places the three
// client SDKs put credentials, and also accepts a web access token so the UI can
// call the proxy on the signed-in user's behalf.
func (s *Server) authenticateProxy(r *http.Request) (*principal, *apiError) {
	token := proxyToken(r)
	if token == "" {
		return nil, errorf(http.StatusUnauthorized, "unauthenticated",
			"missing credentials; send an API key as \"Authorization: Bearer %s…\"", store.APIKeyPrefix)
	}

	if strings.HasPrefix(token, store.APIKeyPrefix) {
		key, user, err := s.store.AuthenticateAPIKey(r.Context(), token)
		if err != nil {
			switch {
			case errors.Is(err, store.ErrNotFound):
				return nil, errorf(http.StatusUnauthorized, "invalid_api_key", "that API key is not valid")
			case errors.Is(err, store.ErrUnauthorized):
				// Revoked, expired, or the owner is suspended: the message is
				// specific because the caller already proved they hold the key.
				return nil, errorf(http.StatusUnauthorized, "invalid_api_key",
					"this API key cannot be used: %s", strings.TrimPrefix(err.Error(), store.ErrUnauthorized.Error()+": "))
			default:
				return nil, storeError(err, "authenticate api key")
			}
		}
		return &principal{User: user, APIKey: key}, nil
	}

	return s.principalForAccessToken(r.Context(), token)
}

// proxyToken reads the credential from wherever the client put it: OpenAI and
// Anthropic SDKs use Authorization or x-api-key, the Google SDKs use
// x-goog-api-key or a ?key= parameter.
func proxyToken(r *http.Request) string {
	if token := bearerToken(r); token != "" {
		return token
	}
	for _, header := range []string{"X-Api-Key", "X-Goog-Api-Key", "Api-Key"} {
		if value := trim(r.Header.Get(header)); value != "" {
			return value
		}
	}
	return trim(r.URL.Query().Get("key"))
}

// ---------------------------------------------------------------------------
// Policy
// ---------------------------------------------------------------------------

// enforceQuota rejects a request that would exceed the account's own limits.
//
// The counters are read from committed usage rows, so a burst of simultaneous
// requests can overshoot a limit slightly. Making this exact would mean holding a
// row lock across every upstream call, which costs far more than the overshoot.
func (s *Server) enforceQuota(ctx context.Context, userID uuid.UUID, quota model.Quota) *apiError {
	now := time.Now().UTC()

	if quota.RequestsPerDay > 0 || quota.TokensPerDay > 0 {
		day, err := s.store.UsageWindow(ctx, userID, startOfDay(now))
		if err != nil {
			return storeError(err, "read today's usage")
		}
		if quota.RequestsPerDay > 0 && day.Requests >= quota.RequestsPerDay {
			return quotaExceeded("daily request", day.Requests, quota.RequestsPerDay, startOfDay(now).AddDate(0, 0, 1))
		}
		if quota.TokensPerDay > 0 && day.TotalTokens >= quota.TokensPerDay {
			return quotaExceeded("daily token", day.TotalTokens, quota.TokensPerDay, startOfDay(now).AddDate(0, 0, 1))
		}
	}

	if quota.RequestsPerMonth > 0 || quota.TokensPerMonth > 0 {
		month, err := s.store.UsageWindow(ctx, userID, startOfMonth(now))
		if err != nil {
			return storeError(err, "read this month's usage")
		}
		if quota.RequestsPerMonth > 0 && month.Requests >= quota.RequestsPerMonth {
			return quotaExceeded("monthly request", month.Requests, quota.RequestsPerMonth,
				startOfMonth(now).AddDate(0, 1, 0))
		}
		if quota.TokensPerMonth > 0 && month.TotalTokens >= quota.TokensPerMonth {
			return quotaExceeded("monthly token", month.TotalTokens, quota.TokensPerMonth,
				startOfMonth(now).AddDate(0, 1, 0))
		}
	}
	return nil
}

func quotaExceeded(kind string, used, limit int64, resets time.Time) *apiError {
	return errorf(http.StatusTooManyRequests, "quota_exceeded",
		"your %s quota is used up (%d of %d); it resets at %s",
		kind, used, limit, resets.Format(time.RFC3339))
}

// enforceModelAllowList applies the per-key model restriction. The model may come
// from the path (Gemini) or the body (everything else).
func (s *Server) enforceModelAllowList(caller *principal, pathModel string, body []byte) *apiError {
	if caller.APIKey == nil || len(caller.APIKey.AllowedModels) == 0 {
		return nil
	}
	requested := firstNonEmpty(pathModel, peekModel(body))
	if requested == "" {
		return errorf(http.StatusBadRequest, "model_required",
			"this API key is restricted to specific models, so the request must name one")
	}
	if !modelAllowed(caller.APIKey.AllowedModels, requested) {
		return errorf(http.StatusForbidden, "model_not_allowed",
			"this API key may not use %q", requested)
	}
	return nil
}

// modelAllowed matches an id against an allow-list. A trailing "*" is a prefix
// wildcard, which is how a key gets access to a whole family of models.
func modelAllowed(allowed []string, id string) bool {
	id = strings.ToLower(trim(id))
	for _, pattern := range allowed {
		pattern = strings.ToLower(trim(pattern))
		switch {
		case pattern == "" || pattern == "*":
			return true
		case strings.HasSuffix(pattern, "*"):
			if strings.HasPrefix(id, strings.TrimSuffix(pattern, "*")) {
				return true
			}
		case pattern == id:
			return true
		}
	}
	return false
}

// applyRoutingHeaders honours the explicit provider and connection overrides.
func applyRoutingHeaders(r *http.Request, call *proxy.Call) *apiError {
	if raw := trim(r.Header.Get(headerProvider)); raw != "" {
		id := model.Provider(strings.ToLower(raw))
		if !id.Valid() {
			return errorf(http.StatusBadRequest, "invalid_provider", "unknown provider %q", raw)
		}
		call.Provider = id
	}
	if raw := trim(r.Header.Get(headerConnection)); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			return errorf(http.StatusBadRequest, "invalid_connection", "%q is not a valid connection id", raw)
		}
		call.ConnectionID = &id
	}
	return nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// readProxyBody reads the request body under a hard size cap.
func readProxyBody(r *http.Request) ([]byte, *apiError) {
	if r.Body == nil {
		return nil, errorf(http.StatusBadRequest, "empty_body", "a JSON body is required")
	}
	// One byte over the cap is read so the difference between "at the limit" and
	// "over it" is detectable.
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody+1))
	if err != nil {
		return nil, errorf(http.StatusBadRequest, "unreadable_body", "could not read the request body: %s", err)
	}
	if len(body) > maxRequestBody {
		return nil, errorf(http.StatusRequestEntityTooLarge, "body_too_large",
			"the request body is larger than %d MiB", maxRequestBody>>20)
	}
	if len(body) == 0 {
		return nil, errorf(http.StatusBadRequest, "empty_body", "a JSON body is required")
	}
	return body, nil
}

// peekModel pulls the model id out of a request body without parsing the rest.
func peekModel(body []byte) string {
	var envelope struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return ""
	}
	return trim(envelope.Model)
}

// writeProxyError reports a pre-upstream failure in the client's own dialect.
func writeProxyError(w http.ResponseWriter, format proxy.Format, apiErr *apiError) {
	if apiErr.Status == 499 {
		return
	}
	message := apiErr.Message
	// Field-level detail is useful, and the proxy envelopes have nowhere to put
	// it, so it is folded into the message.
	for field, problem := range apiErr.Fields {
		message += "; " + field + ": " + problem
	}
	proxy.WriteError(w, format, apiErr.Status, errorTypeFor(apiErr.Status), message)
}

// errorTypeFor maps a status onto the error type OpenAI and Anthropic clients
// expect to see.
func errorTypeFor(status int) string {
	switch {
	case status == http.StatusUnauthorized:
		return "authentication_error"
	case status == http.StatusForbidden:
		return "permission_error"
	case status == http.StatusNotFound:
		return "not_found_error"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status == http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case status >= 500:
		return "api_error"
	default:
		return "invalid_request_error"
	}
}

func (s *Server) requestTimeout() time.Duration {
	if s.cfg != nil && s.cfg.RequestTimeout > 0 {
		return s.cfg.RequestTimeout
	}
	return 10 * time.Minute
}

// logLevelForStatus keeps a client's own mistake out of the error log.
func logLevelForStatus(status int) slog.Level {
	switch {
	case status >= 500:
		return slog.LevelError
	case status >= 400:
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
