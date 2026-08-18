// Package proxy translates client API dialects into upstream provider calls.
//
// A request arrives in one of four client formats (OpenAI chat completions,
// OpenAI responses, Anthropic messages, Gemini generateContent), is converted
// into the canonical intermediate representation in ir.go, and is then rendered
// for whichever provider serves the requested model. Responses travel the same
// path backwards: provider frames become canonical events, and a renderer writes
// them in the client's own dialect. Where a client format and a provider
// protocol coincide the frames are forwarded verbatim instead.
package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"aihub/internal/model"
	"aihub/internal/provider"
	"aihub/internal/store"
)

// maxAttempts bounds how many connections one request may try.
const maxAttempts = 3

// Router selects a connection and drives one proxied request end to end.
type Router struct {
	store    *store.Store
	registry *provider.Registry
	log      *slog.Logger

	// antigravityFilter screens requests bound for the Antigravity upstream
	// for non-Antigravity coding-client names inside the `system` field. It
	// is nil when the filter is disabled, so the hot path pays nothing.
	antigravityFilter *AntigravityFilter

	executors map[model.Provider]executor
}

// NewRouter wires the per-provider executors.
func NewRouter(st *store.Store, registry *provider.Registry, logger *slog.Logger) *Router {
	// The registry's own client caps every request with an absolute timeout,
	// which would cut a long completion short, so streaming gets a client with
	// the same transport and no deadline of its own. The request context is what
	// bounds a proxied call.
	client := &http.Client{Transport: registry.HTTPClient().Transport}

	r := &Router{
		store:     st,
		registry:  registry,
		log:       logger,
		executors: map[model.Provider]executor{},
	}
	r.executors[model.ProviderCodex] = newCodexExecutor(client, registry.Tokens(), logger)
	if vendor, err := registry.Get(model.ProviderAntigravity); err == nil {
		r.executors[model.ProviderAntigravity] = newAntigravityExecutor(client, registry.Tokens(), vendor, logger)
	}
	if vendor, err := registry.Get(model.ProviderOpenAI); err == nil {
		r.executors[model.ProviderOpenAI] = newOpenAIExecutor(client, registry.Tokens(), vendor, logger)
	}
	return r
}

// SetAntigravityFilter installs a coding-client filter that screens requests
// bound for the Antigravity upstream. Passing nil disables the filter; this
// is the default, so existing callers that never call this method are
// unaffected. The method is on Router (not the constructor) so that the
// config layer can stay in charge of constructing the filter and the rest of
// the binary keeps a single NewRouter entry point.
func (r *Router) SetAntigravityFilter(filter *AntigravityFilter) {
	r.antigravityFilter = filter
	if filter == nil {
		r.log.Info("antigravity coding filter disabled")
	} else {
		r.log.Info("antigravity coding filter enabled", "mode", filter.Mode())
	}
}

// SetDebugRequests flips the per-executor debug flag on or off. When
// on, the Antigravity and Codex executors log the request body, the
// response status, headers and body (bounded) for every upstream
// call. Off by default so the log stays quiet on a healthy deployment.
func (r *Router) SetDebugRequests(on bool) {
	if ex, ok := r.executors[model.ProviderAntigravity].(*antigravityExecutor); ok {
		ex.debug = on
	}
	if ex, ok := r.executors[model.ProviderCodex].(*codexExecutor); ok {
		ex.debug = on
	}
	if on {
		r.log.Info("debug requests enabled for antigravity + codex executors")
	} else {
		r.log.Info("debug requests disabled")
	}
}

// Call describes one proxied completion request.
type Call struct {
	// Format is the dialect the client is speaking.
	Format Format
	// Body is the raw request body.
	Body []byte
	// PathModel is the model taken from the URL rather than the body, as the
	// Gemini API does.
	PathModel string
	// ForceStream overrides the body's stream flag when the URL decides it.
	ForceStream *bool

	// User owns the request; connections and usage are attributed to them.
	User *model.User
	// APIKeyID records which proxy key was used, when one was.
	APIKeyID *uuid.UUID
	// AllowShared lets the request fall back to the shared connection pool.
	AllowShared bool
	// AllowedProviders, when non-empty, restricts which providers may serve it.
	AllowedProviders []string
	// Provider pins the request to one provider instead of resolving the model.
	Provider model.Provider
	// ConnectionID pins the request to one connection.
	ConnectionID *uuid.UUID
}

// Outcome is what the router did, for the access log and the usage ledger.
type Outcome struct {
	Model        string
	Provider     model.Provider
	ConnectionID *uuid.UUID
	Stream       bool
	Status       int
	Usage        Usage
	Latency      time.Duration
	Err          error
}

// Complete proxies one request and writes the response. The returned Outcome is
// also recorded in the usage ledger, so callers only need it for logging.
func (r *Router) Complete(ctx context.Context, w http.ResponseWriter, call Call) Outcome {
	started := time.Now()
	outcome := Outcome{Status: http.StatusOK}

	req, err := ParseRequest(call.Format, call.Body, ParseOptions{
		Model:       call.PathModel,
		ForceStream: call.ForceStream,
	})
	if err != nil {
		return r.fail(ctx, w, call, outcome, started, asAPIError("", err))
	}
	outcome.Model = req.Model
	outcome.Stream = req.Stream

	providerID, apiErr := r.resolveProvider(call, req.Model)
	if apiErr != nil {
		return r.fail(ctx, w, call, outcome, started, apiErr)
	}
	outcome.Provider = providerID

	// The antigravity coding filter screens requests bound for the Antigravity
	// upstream. It runs after provider resolution so the policy checks above
	// (allowed providers, model allow list) take precedence, and before the
	// request is dispatched so a blocked body never reaches the upstream.
	// Rewrite mode may replace the body; the request is then re-parsed so the
	// executor sees the rewritten instructions rather than the original ones.
	if r.antigravityFilter != nil && providerID == model.ProviderAntigravity {
		rewritten, decision := r.antigravityFilter.Apply(call.Body)
		if decision.Blocked {
			blockErr := newAPIError(http.StatusForbidden, "invalid_request_error",
				"request blocked because it matches a configured non-Antigravity coding software keyword: "+
					decision.Detail)
			blockErr.Code = "blocked_by_antigravity_coding_filter"
			return r.fail(ctx, w, call, outcome, started, blockErr)
		}
		if rewritten != nil {
			call.Body = rewritten
			if req2, perr := ParseRequest(call.Format, call.Body, ParseOptions{
				Model:       call.PathModel,
				ForceStream: call.ForceStream,
			}); perr == nil {
				req = req2
			} else {
				r.log.Warn("antigravity filter: rewritten body failed to re-parse; forwarding original",
					"error", perr)
			}
		}
	}

	ex, ok := r.executors[providerID]
	if !ok {
		return r.fail(ctx, w, call, outcome, started, newAPIError(http.StatusNotImplemented,
			"invalid_request_error", "provider "+string(providerID)+" is not available in this build"))
	}

	candidates, apiErr := r.candidates(ctx, call, providerID)
	if apiErr != nil {
		return r.fail(ctx, w, call, outcome, started, apiErr)
	}

	raw := ex.passthrough(call.Format)
	stream, conn, apiErr := r.dispatch(ctx, ex, candidates, req, raw)
	if apiErr != nil {
		return r.fail(ctx, w, call, outcome, started, apiErr)
	}
	defer stream.Close()

	outcome.ConnectionID = &conn.ID
	// The connection is claimed as soon as it answers, so concurrent requests
	// rotate instead of piling onto the same account.
	if err := r.store.TouchConnectionUsed(ctx, conn.ID); err != nil {
		r.log.Debug("could not record connection use", "connection", conn.ID, "error", err)
	}
	r.captureQuota(ctx, conn, stream.Header)

	// From here on the response headers belong to the renderer: any further
	// failure has to be reported inside the stream, not as a status code.
	outcome.Usage, apiErr = r.render(w, call, req, stream, raw)
	if apiErr != nil {
		outcome.Status = apiErr.Status
		outcome.Err = apiErr
		if apiErr.Retryable {
			r.penalize(ctx, conn, apiErr)
		}
	}

	outcome.Latency = time.Since(started)
	r.record(ctx, call, outcome)
	return outcome
}

// render drives the response half of the exchange.
func (r *Router) render(w http.ResponseWriter, call Call, req *Request,
	stream *upstreamStream, raw bool) (Usage, *APIError) {

	var (
		renderer Renderer
		passthru *rawRenderer
	)
	if raw {
		passthru = newRawRenderer(w, req.Stream, call.Format)
		renderer = passthru
	} else {
		renderer = NewRenderer(call.Format, w, req.Stream)
	}
	renderer.Begin(req.Model)

	var streamErr *APIError
	if passthru != nil {
		for {
			frame, ok, err := stream.NextRaw()
			if err != nil {
				streamErr = asAPIError(stream.providerID, err)
				break
			}
			if !ok {
				break
			}
			if err = passthru.HandleRaw(frame); err != nil {
				// The client is gone; there is nobody left to report to.
				return stream.observedUsage(), nil
			}
		}
		if len(stream.aggregate) > 0 {
			passthru.SetFinal(stream.aggregate)
		}
		passthru.SetUsage(stream.observedUsage())
	} else {
		for {
			ev, ok, err := stream.Next()
			if err != nil {
				streamErr = asAPIError(stream.providerID, err)
				break
			}
			if !ok {
				break
			}
			if err = renderer.Handle(ev); err != nil {
				return renderer.Usage(), nil
			}
		}
	}

	if streamErr != nil {
		renderer.WriteError(streamErr)
		return renderer.Usage(), streamErr
	}
	if err := renderer.Finish(); err != nil {
		// A write failure at this point means the client disconnected.
		r.log.Debug("could not finish response", "error", err)
	}
	return renderer.Usage(), nil
}

// dispatch tries each candidate connection until one accepts the request.
func (r *Router) dispatch(ctx context.Context, ex executor, candidates []*model.Connection,
	req *Request, raw bool) (*upstreamStream, *model.Connection, *APIError) {

	var last *APIError
	attempts := 0

	for _, conn := range candidates {
		if attempts >= maxAttempts {
			break
		}
		attempts++

		// A rejected token gets one forced refresh before the connection is
		// written off: the stored access token may simply have been revoked
		// out from under us.
		for try := 0; try < 2; try++ {
			stream, err := ex.send(ctx, conn, req, sendOptions{Raw: raw})
			if err == nil {
				stream.providerID = ex.providerID()
				return stream, conn, nil
			}
			last = asAPIError(ex.providerID(), err)

			if try == 0 && (last.Upstream == http.StatusUnauthorized || last.Upstream == http.StatusForbidden) {
				if _, refreshErr := r.registry.Tokens().ForceRefresh(ctx, conn); refreshErr == nil {
					continue
				}
			}
			break
		}

		if ctx.Err() != nil {
			return nil, nil, asAPIError(ex.providerID(), ctx.Err())
		}
		if !last.Retryable {
			return nil, nil, last
		}
		r.penalize(ctx, conn, last)
	}

	if last == nil {
		last = newAPIError(http.StatusServiceUnavailable, "api_error",
			"no upstream connection accepted the request")
	}
	return nil, nil, last
}

// resolveProvider decides which upstream serves a model.
func (r *Router) resolveProvider(call Call, modelID string) (model.Provider, *APIError) {
	resolved := call.Provider
	if resolved == "" {
		if found, ok := r.registry.Catalog().ProviderFor(modelID); ok {
			resolved = found
		} else {
			resolved = guessProvider(modelID)
		}
	}
	if resolved == "" {
		return "", newAPIError(http.StatusNotFound, "model_not_found",
			"unknown model "+strconv.Quote(modelID)+"; call /v1/models for the list this key can use")
	}
	if len(call.AllowedProviders) > 0 && !containsFold(call.AllowedProviders, string(resolved)) {
		return "", newAPIError(http.StatusForbidden, "permission_error",
			"your account is not allowed to use the "+string(resolved)+" provider")
	}
	return resolved, nil
}

// guessProvider falls back to naming conventions when the catalog has not heard
// of a model. Being permissive here keeps a newly released model usable before
// the catalog catches up.
func guessProvider(modelID string) model.Provider {
	id := strings.ToLower(normalizeModel(modelID))
	switch {
	case strings.HasPrefix(id, "gemini"), strings.Contains(id, "claude"),
		strings.Contains(id, "gpt-oss"), strings.Contains(id, "grok"):
		return model.ProviderAntigravity
	case strings.Contains(id, "codex"), strings.HasPrefix(id, "gpt-"),
		strings.HasPrefix(id, "o1"), strings.HasPrefix(id, "o3"), strings.HasPrefix(id, "o4"):
		return model.ProviderCodex
	default:
		return ""
	}
}

// candidates lists the connections that may serve this request, best first.
func (r *Router) candidates(ctx context.Context, call Call, providerID model.Provider) ([]*model.Connection, *APIError) {
	if call.ConnectionID != nil {
		conn, err := r.store.GetConnection(ctx, *call.ConnectionID)
		if err != nil {
			return nil, newAPIError(http.StatusNotFound, "invalid_request_error", "unknown connection")
		}
		if conn.OwnerID != call.User.ID && conn.Scope != model.ScopeShared && !call.User.IsAdmin() {
			return nil, newAPIError(http.StatusForbidden, "permission_error", "that connection belongs to somebody else")
		}
		if conn.Provider != providerID {
			return nil, newAPIError(http.StatusBadRequest, "invalid_request_error",
				"connection "+conn.Label+" is a "+string(conn.Provider)+" account, but the model needs "+string(providerID))
		}
		if !conn.Usable(time.Now()) || conn.Credential == nil {
			return nil, newAPIError(http.StatusServiceUnavailable, "api_error",
				"connection "+conn.Label+" is not currently usable: "+firstNonEmpty(conn.LastError, conn.Status))
		}
		return []*model.Connection{conn}, nil
	}

	list, err := r.store.SelectCandidates(ctx, call.User.ID, providerID, call.AllowShared)
	if err != nil {
		return nil, newAPIError(http.StatusInternalServerError, "api_error", "could not list connections: "+err.Error())
	}
	if len(list) == 0 {
		return nil, newAPIError(http.StatusServiceUnavailable, "api_error",
			"no usable "+string(providerID)+" connection; sign one in from the web UI first")
	}
	// The store returns least-recently-used first; weight then promotes the
	// accounts an operator wants to lean on.
	sort.SliceStable(list, func(a, b int) bool { return list[a].Weight > list[b].Weight })
	return list, nil
}

// penalize records an upstream failure and, when the provider asked us to back
// off, keeps the connection out of rotation for a while.
func (r *Router) penalize(ctx context.Context, conn *model.Connection, apiErr *APIError) {
	cooldown := time.Duration(0)
	status := model.ConnStatusError

	switch apiErr.Upstream {
	case http.StatusTooManyRequests:
		cooldown = 60 * time.Second
		if seconds := retryAfterSeconds(apiErr.Message); seconds > 0 {
			cooldown = time.Duration(seconds) * time.Second
		}
	case http.StatusUnauthorized, http.StatusForbidden:
		cooldown = 30 * time.Second
		status = model.ConnStatusExpired
	default:
		if apiErr.Upstream >= 500 {
			cooldown = 15 * time.Second
		}
	}

	// Bookkeeping must survive the request being cancelled.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := r.store.MarkConnectionError(writeCtx, conn.ID, apiErr.Message, cooldown, status); err != nil {
		r.log.Warn("could not record connection failure", "connection", conn.ID, "error", err)
	}
}

// captureQuota stores the rate-limit snapshot a provider attaches to responses.
// Codex has no quota endpoint, so these headers are the only source of truth.
func (r *Router) captureQuota(ctx context.Context, conn *model.Connection, header http.Header) {
	if conn.Provider != model.ProviderCodex || header == nil {
		return
	}
	quota := provider.CodexQuotaFromHeaders(header, conn.Plan)
	if quota == nil {
		return
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	r.registry.Tokens().RecordQuota(writeCtx, conn, quota)
	conn.Quota = quota
}

// fail reports a pre-stream error in the client's dialect and books the attempt.
func (r *Router) fail(ctx context.Context, w http.ResponseWriter, call Call,
	outcome Outcome, started time.Time, apiErr *APIError) Outcome {

	outcome.Status = apiErr.Status
	outcome.Err = apiErr
	outcome.Latency = time.Since(started)

	if apiErr.Status != 499 {
		writeFormatError(w, nil, false, call.Format, apiErr)
	}
	r.record(ctx, call, outcome)
	return outcome
}

// record appends the request to the usage ledger.
func (r *Router) record(ctx context.Context, call Call, outcome Outcome) {
	if call.User == nil {
		return
	}
	rec := &model.UsageRecord{
		UserID:           call.User.ID,
		APIKeyID:         call.APIKeyID,
		ConnectionID:     outcome.ConnectionID,
		Provider:         string(outcome.Provider),
		Model:            outcome.Model,
		ClientFormat:     string(call.Format),
		StatusCode:       outcome.Status,
		Stream:           outcome.Stream,
		PromptTokens:     outcome.Usage.PromptTokens,
		CompletionTokens: outcome.Usage.CompletionTokens,
		ReasoningTokens:  outcome.Usage.ReasoningTokens,
		CachedTokens:     outcome.Usage.CachedTokens,
		TotalTokens:      outcome.Usage.Total(),
		LatencyMS:        outcome.Latency.Milliseconds(),
	}
	if outcome.Err != nil {
		rec.Error = outcome.Err.Error()
	}

	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := r.store.RecordUsage(writeCtx, rec); err != nil {
		r.log.Warn("could not record usage", "user", call.User.ID, "error", err)
	}
}

// AvailableModels lists the models a user can actually reach, given the
// connections they own and, optionally, the shared pool.
func (r *Router) AvailableModels(ctx context.Context, user *model.User, allowShared bool) []provider.ModelInfo {
	conns, err := r.store.ListConnections(ctx, store.ConnectionFilter{
		OwnerID:       user.ID,
		IncludeShared: allowShared,
		UsableOnly:    true,
	})
	if err != nil {
		r.log.Warn("could not list connections for model catalog", "user", user.ID, "error", err)
		return nil
	}

	catalog := r.registry.Catalog()
	seen := map[string]bool{}
	var out []provider.ModelInfo
	for _, conn := range conns {
		if !conn.Usable(time.Now()) {
			continue
		}
		for _, info := range catalog.ForPlan(conn.Provider, conn.Plan) {
			if seen[info.ID] {
				continue
			}
			seen[info.ID] = true
			out = append(out, info)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out
}

// retryAfterSeconds pulls the back-off hint out of a formatted error message.
func retryAfterSeconds(message string) int {
	idx := strings.Index(message, "retry-after ")
	if idx < 0 {
		return 0
	}
	rest := message[idx+len("retry-after "):]
	end := strings.IndexFunc(rest, func(r rune) bool { return r < '0' || r > '9' })
	if end == 0 {
		return 0
	}
	if end > 0 {
		rest = rest[:end]
	}
	seconds, err := strconv.Atoi(rest)
	if err != nil || seconds <= 0 || seconds > 3600 {
		return 0
	}
	return seconds
}

func containsFold(list []string, want string) bool {
	for _, item := range list {
		if strings.EqualFold(strings.TrimSpace(item), want) {
			return true
		}
	}
	return false
}
