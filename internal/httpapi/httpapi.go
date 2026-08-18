// Package httpapi exposes the whole HTTP surface of the server: the
// provider-compatible proxy endpoints under /v1 and /v1beta, the REST API the
// web UI drives under /api, and the embedded UI itself.
//
// Two authentication schemes meet here. The proxy endpoints accept a proxy API
// key (`ah-…`) in whichever header the client SDK uses, while the /api endpoints
// take the short-lived JWT access token the login flow issues. Both resolve to a
// *model.User, and every listing, quota check and connection lookup is scoped to
// that user unless they hold the admin role.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"aihub/internal/authn"
	"aihub/internal/config"
	"aihub/internal/model"
	"aihub/internal/provider"
	"aihub/internal/proxy"
	"aihub/internal/store"
	"aihub/internal/webui"
)

// maxRequestBody bounds a completion request body. Image parts make these large,
// so the limit is generous.
const maxRequestBody = 32 << 20

// maxJSONBody bounds an admin request body.
const maxJSONBody = 1 << 20

// Deps are the collaborators the HTTP layer needs.
type Deps struct {
	Config   *config.Config
	Store    *store.Store
	Issuer   *authn.Issuer
	Registry *provider.Registry
	Router   *proxy.Router
	Logger   *slog.Logger
	Version  string
}

// Server owns the routing table and the request-scoped policy checks.
type Server struct {
	cfg      *config.Config
	store    *store.Store
	issuer   *authn.Issuer
	registry *provider.Registry
	router   *proxy.Router
	log      *slog.Logger
	version  string

	ui         http.Handler
	inflight   *inflight
	loginGuard *throttle
	started    time.Time
}

// New builds the HTTP handler for the whole application.
func New(deps Deps) http.Handler {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}

	s := &Server{
		cfg:        deps.Config,
		store:      deps.Store,
		issuer:     deps.Issuer,
		registry:   deps.Registry,
		router:     deps.Router,
		log:        logger,
		version:    deps.Version,
		ui:         webui.Handler(),
		inflight:   newInflight(),
		loginGuard: newThrottle(),
		started:    time.Now(),
	}

	if s.cfg != nil && s.cfg.EnableLocalOAuthListeners {
		s.startLoopbackListeners()
	}

	mux := http.NewServeMux()
	s.route(mux)
	return s.recoverer(s.logging(s.cors(mux)))
}

// route registers every endpoint. Patterns are ordered from most to least
// specific only for readability; the mux itself resolves by specificity.
func (s *Server) route(mux *http.ServeMux) {
	// ---- public -----------------------------------------------------------
	mux.HandleFunc("GET /healthz", s.health)
	mux.Handle("GET /api/meta", s.json(s.meta))
	// First-run setup. It only works while the deployment has no accounts, and
	// answers 409 afterwards, so it stays registered rather than being wired up
	// conditionally at boot.
	mux.Handle("POST /api/setup", s.json(s.setup))
	mux.Handle("POST /api/auth/login", s.json(s.login))
	mux.Handle("POST /api/auth/refresh", s.json(s.refresh))
	mux.Handle("POST /api/auth/logout", s.json(s.logout))
	// The loopback OAuth redirect lands here, either directly (when the server
	// itself runs on the vendor's callback port) or via a local listener.
	mux.HandleFunc("GET /oauth/callback", s.oauthCallback)

	// ---- authenticated web UI --------------------------------------------
	mux.Handle("GET /api/auth/me", s.user(s.json(s.me)))
	mux.Handle("POST /api/auth/password", s.user(s.json(s.changePassword)))
	mux.Handle("GET /api/auth/sessions", s.user(s.json(s.listWebSessions)))
	mux.Handle("DELETE /api/auth/sessions/{id}", s.user(s.json(s.revokeWebSession)))

	mux.Handle("GET /api/providers", s.user(s.json(s.listProviders)))
	mux.Handle("GET /api/models", s.user(s.json(s.listCatalog)))

	mux.Handle("GET /api/connections", s.user(s.json(s.listConnections)))
	mux.Handle("POST /api/connections", s.user(s.json(s.createAPIKeyConnection)))
	mux.Handle("GET /api/connections/{id}", s.user(s.json(s.getConnection)))
	mux.Handle("PATCH /api/connections/{id}", s.user(s.json(s.patchConnection)))
	mux.Handle("DELETE /api/connections/{id}", s.user(s.json(s.deleteConnection)))
	mux.Handle("POST /api/connections/{id}/refresh", s.user(s.json(s.refreshConnection)))
	mux.Handle("POST /api/connections/{id}/quota", s.user(s.json(s.refreshConnectionQuota)))

	// "Temporary" connections: OAuth attempts that exist only until they are
	// redeemed, cancelled or expire.
	mux.Handle("GET /api/oauth/sessions", s.user(s.json(s.listOAuthSessions)))
	mux.Handle("POST /api/oauth/sessions", s.user(s.json(s.startOAuth)))
	mux.Handle("POST /api/oauth/sessions/{id}/complete", s.user(s.json(s.completeOAuth)))
	mux.Handle("POST /api/oauth/sessions/{id}/cancel", s.user(s.json(s.cancelOAuth)))
	mux.Handle("DELETE /api/oauth/sessions/{id}", s.user(s.json(s.cancelOAuth)))

	mux.Handle("GET /api/keys", s.user(s.json(s.listAPIKeys)))
	mux.Handle("POST /api/keys", s.user(s.json(s.createAPIKey)))
	mux.Handle("POST /api/keys/{id}/revoke", s.user(s.json(s.revokeAPIKey)))
	mux.Handle("DELETE /api/keys/{id}", s.user(s.json(s.deleteAPIKey)))

	mux.Handle("GET /api/quota", s.user(s.json(s.getOwnQuota)))
	mux.Handle("GET /api/usage/summary", s.user(s.json(s.usageSummary)))
	mux.Handle("GET /api/usage/series", s.user(s.json(s.usageSeries)))
	mux.Handle("GET /api/usage/breakdown", s.user(s.json(s.usageBreakdown)))
	mux.Handle("GET /api/usage/records", s.user(s.json(s.usageRecords)))

	// ---- admin -----------------------------------------------------------
	mux.Handle("GET /api/admin/overview", s.admin(s.json(s.adminOverview)))
	mux.Handle("GET /api/admin/users", s.admin(s.json(s.listUsers)))
	mux.Handle("POST /api/admin/users", s.admin(s.json(s.createUser)))
	mux.Handle("GET /api/admin/users/{id}", s.admin(s.json(s.getUser)))
	mux.Handle("PATCH /api/admin/users/{id}", s.admin(s.json(s.patchUser)))
	mux.Handle("DELETE /api/admin/users/{id}", s.admin(s.json(s.deleteUser)))
	mux.Handle("PUT /api/admin/users/{id}/quota", s.admin(s.json(s.putUserQuota)))
	mux.Handle("POST /api/admin/users/{id}/password", s.admin(s.json(s.setUserPassword)))

	// Anything else under /api is a client mistake, not a UI route.
	mux.Handle("/api/", s.json(func(http.ResponseWriter, *http.Request) *apiError {
		return errorf(http.StatusNotFound, "not_found", "no such endpoint")
	}))

	// ---- proxy -----------------------------------------------------------
	mux.HandleFunc("GET /v1/models", s.listProxyModels)
	mux.HandleFunc("GET /v1/models/{model}", s.getProxyModel)
	mux.HandleFunc("POST /v1/chat/completions", s.proxyChat)
	mux.HandleFunc("POST /v1/responses", s.proxyResponses)
	mux.HandleFunc("POST /v1/messages", s.proxyMessages)
	mux.HandleFunc("GET /v1beta/models", s.listGeminiModels)
	mux.HandleFunc("POST /v1beta/models/{model}", s.proxyGemini)
	mux.HandleFunc("GET /v1beta/models/{model}", s.getGeminiModel)

	// ---- web UI ----------------------------------------------------------
	mux.Handle("/", s.ui)
}

// ---------------------------------------------------------------------------
// Middleware
// ---------------------------------------------------------------------------

// recoverer turns a panic into a 500 instead of dropping the connection.
func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				// http.ErrAbortHandler is the documented way to abort a response.
				if recovered == http.ErrAbortHandler {
					panic(recovered)
				}
				s.log.Error("panic serving request", "method", r.Method, "path", r.URL.Path,
					"panic", recovered, "stack", string(debug.Stack()))
				writeAPIError(w, errorf(http.StatusInternalServerError, "internal", "internal server error"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// logging records one line per request. Proxy requests log their own outcome
// with token counts, so only the status is added here.
func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)

		// Static assets would drown out everything else.
		if !strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasPrefix(r.URL.Path, "/v1") {
			return
		}
		level := slog.LevelInfo
		if recorder.status >= 500 {
			level = slog.LevelError
		} else if recorder.status >= 400 {
			level = slog.LevelWarn
		}
		s.log.Log(r.Context(), level, "request",
			"method", r.Method, "path", r.URL.Path, "status", recorder.status,
			"bytes", recorder.written, "duration", time.Since(started).Round(time.Millisecond))
	})
}

// cors lets browser-based SDKs call the proxy from another origin. Credentials
// are deliberately not allowed: the refresh cookie must stay same-origin, so a
// cross-origin caller has to present an API key or access token explicitly.
func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			header := w.Header()
			header.Set("Access-Control-Allow-Origin", origin)
			header.Add("Vary", "Origin")
			header.Set("Access-Control-Allow-Methods", "GET, POST, PATCH, PUT, DELETE, OPTIONS")
			header.Set("Access-Control-Allow-Headers",
				"Authorization, Content-Type, X-Api-Key, X-Goog-Api-Key, Anthropic-Version, "+
					"Anthropic-Beta, OpenAI-Beta, X-Aihub-Provider, X-Aihub-Connection")
			header.Set("Access-Control-Max-Age", "600")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// statusRecorder captures the status code and size for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written int64
	wrote   bool
}

func (r *statusRecorder) WriteHeader(status int) {
	if !r.wrote {
		r.status = status
		r.wrote = true
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	r.wrote = true
	n, err := r.ResponseWriter.Write(p)
	r.written += int64(n)
	return n, err
}

// Flush keeps streaming responses working through the wrapper.
func (r *statusRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// ---------------------------------------------------------------------------
// Handler plumbing
// ---------------------------------------------------------------------------

// handlerFunc is a handler that reports failure instead of writing it, so every
// /api endpoint produces the same error envelope.
type handlerFunc func(http.ResponseWriter, *http.Request) *apiError

// json adapts a handlerFunc to http.Handler.
func (s *Server) json(h handlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if apiErr := h(w, r); apiErr != nil {
			writeAPIError(w, apiErr)
		}
	})
}

// user requires a valid access token and attaches the caller to the context.
func (s *Server) user(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller, apiErr := s.authenticateWeb(r)
		if apiErr != nil {
			writeAPIError(w, apiErr)
			return
		}
		next.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), caller)))
	})
}

// admin requires the admin role on top of a valid access token.
func (s *Server) admin(next http.Handler) http.Handler {
	return s.user(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !callerFrom(r.Context()).User.IsAdmin() {
			writeAPIError(w, errorf(http.StatusForbidden, "forbidden", "administrator access required"))
			return
		}
		next.ServeHTTP(w, r)
	}))
}

// ---------------------------------------------------------------------------
// Caller identity
// ---------------------------------------------------------------------------

// principal is the authenticated caller of one request.
type principal struct {
	User *model.User
	// Claims is set when the caller presented a web-UI access token.
	Claims *authn.Claims
	// APIKey is set when the caller presented a proxy API key.
	APIKey *model.APIKey
}

type ctxKey int

const ctxPrincipal ctxKey = iota

func withPrincipal(ctx context.Context, caller *principal) context.Context {
	return context.WithValue(ctx, ctxPrincipal, caller)
}

// callerFrom returns the authenticated caller. It is only called from handlers
// behind an auth middleware, so a missing value is a routing bug.
func callerFrom(ctx context.Context) *principal {
	caller, _ := ctx.Value(ctxPrincipal).(*principal)
	if caller == nil {
		return &principal{User: &model.User{}}
	}
	return caller
}

// authenticateWeb resolves the bearer access token of an /api request.
func (s *Server) authenticateWeb(r *http.Request) (*principal, *apiError) {
	token := bearerToken(r)
	if token == "" {
		return nil, errorf(http.StatusUnauthorized, "unauthenticated", "missing bearer token")
	}
	return s.principalForAccessToken(r.Context(), token)
}

// principalForAccessToken resolves a web-UI access token into its account. The
// proxy endpoints share it, because they accept an access token as well as an
// API key.
func (s *Server) principalForAccessToken(ctx context.Context, token string) (*principal, *apiError) {
	claims, err := s.issuer.Verify(token)
	if err != nil {
		return nil, errorf(http.StatusUnauthorized, "unauthenticated", "invalid or expired access token")
	}
	userID, err := claims.UserID()
	if err != nil {
		return nil, errorf(http.StatusUnauthorized, "unauthenticated", "malformed access token subject")
	}

	user, err := s.store.GetUser(ctx, userID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, errorf(http.StatusUnauthorized, "unauthenticated", "account no longer exists")
		}
		return nil, storeError(err, "load account")
	}
	if user.Status != model.StatusActive {
		return nil, errorf(http.StatusForbidden, "account_"+user.Status, "this account is %s", user.Status)
	}
	return &principal{User: user, Claims: claims}, nil
}

// bearerToken reads the Authorization header.
func bearerToken(r *http.Request) string {
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	if header == "" {
		return ""
	}
	if len(header) > 7 && strings.EqualFold(header[:7], "bearer ") {
		return strings.TrimSpace(header[7:])
	}
	return header
}

// ---------------------------------------------------------------------------
// Concurrency limiting
// ---------------------------------------------------------------------------

// inflight caps how many proxied requests one user may have running at once.
// The count is per process: with several replicas each enforces its own share,
// which is the honest trade-off for keeping this off the hot database path.
type inflight struct {
	mu     sync.Mutex
	counts map[uuid.UUID]int
}

func newInflight() *inflight { return &inflight{counts: map[uuid.UUID]int{}} }

// acquire reserves a slot, reporting false when the user is at their limit. A
// limit of zero means unlimited.
func (i *inflight) acquire(userID uuid.UUID, limit int) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if limit > 0 && i.counts[userID] >= limit {
		return false
	}
	i.counts[userID]++
	return true
}

func (i *inflight) release(userID uuid.UUID) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if remaining := i.counts[userID] - 1; remaining > 0 {
		i.counts[userID] = remaining
	} else {
		delete(i.counts, userID)
	}
}

// ---------------------------------------------------------------------------
// Errors and JSON
// ---------------------------------------------------------------------------

// apiError is the error envelope every /api endpoint returns.
type apiError struct {
	// Status is the HTTP status to answer with.
	Status int `json:"-"`
	// Code is a stable machine-readable slug for the UI to branch on.
	Code string `json:"code,omitempty"`
	// Message is human-readable and safe to show a user.
	Message string `json:"message"`
	// Fields carries per-field validation messages for form rendering.
	Fields map[string]string `json:"fields,omitempty"`
}

func (e *apiError) Error() string { return e.Message }

// errorf builds an apiError with a formatted message.
func errorf(status int, code, format string, args ...any) *apiError {
	return &apiError{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

// invalid reports a validation failure against named fields.
func invalid(fields map[string]string) *apiError {
	return &apiError{
		Status:  http.StatusUnprocessableEntity,
		Code:    "invalid",
		Message: "some fields are invalid",
		Fields:  fields,
	}
}

// storeError maps a persistence failure onto a client-facing error.
func storeError(err error, action string) *apiError {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrNotFound):
		return errorf(http.StatusNotFound, "not_found", "%s: not found", action)
	case errors.Is(err, store.ErrConflict):
		return errorf(http.StatusConflict, "conflict", "%s: already exists", action)
	case errors.Is(err, context.Canceled):
		return errorf(499, "cancelled", "request cancelled")
	default:
		return errorf(http.StatusInternalServerError, "internal", "%s: %s", action, err.Error())
	}
}

// writeJSON encodes a payload, using a buffer so a marshal failure cannot leave
// a half-written body behind.
func writeJSON(w http.ResponseWriter, status int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"could not encode response","code":"internal"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeAPIError writes the error envelope.
func writeAPIError(w http.ResponseWriter, apiErr *apiError) {
	if apiErr.Status == 499 {
		// The client is gone; there is nothing to write to.
		return
	}
	writeJSON(w, apiErr.Status, apiErr)
}

// decodeJSON reads a bounded JSON body into dst, rejecting unknown fields so a
// typo in the UI surfaces immediately instead of being silently ignored.
func decodeJSON(r *http.Request, dst any) *apiError {
	decoder := json.NewDecoder(io.LimitReader(r.Body, maxJSONBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return errorf(http.StatusBadRequest, "empty_body", "a JSON body is required")
		}
		return errorf(http.StatusBadRequest, "malformed_json", "could not read the request body: %s", err.Error())
	}
	return nil
}

// pathUUID reads a {name} path parameter as a UUID.
func pathUUID(r *http.Request, name string) (uuid.UUID, *apiError) {
	raw := r.PathValue(name)
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, errorf(http.StatusBadRequest, "invalid_id", "%q is not a valid id", raw)
	}
	return id, nil
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

// health answers the liveness probe, including database reachability.
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	status := http.StatusOK
	dbState := "ok"
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := s.store.Pool().Ping(ctx); err != nil {
		status = http.StatusServiceUnavailable
		dbState = err.Error()
	}
	writeJSON(w, status, map[string]any{
		"status":   map[bool]string{true: "ok", false: "degraded"}[status == http.StatusOK],
		"version":  s.version,
		"database": dbState,
		"uptime":   time.Since(s.started).Round(time.Second).String(),
	})
}

// meta describes the deployment to the login page, before anyone is signed in.
// needs_setup is what sends a brand-new deployment to the setup screen instead
// of the sign-in form.
func (s *Server) meta(w http.ResponseWriter, r *http.Request) *apiError {
	providers := make([]map[string]any, 0, 2)
	for _, p := range s.registry.All() {
		providers = append(providers, map[string]any{
			"id":            p.ID(),
			"display_name":  p.DisplayName(),
			"loopback_port": p.LoopbackPort(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":          s.version,
		"providers":        providers,
		"public_url":       s.cfg.PublicURL,
		"api_key_prefix":   store.APIKeyPrefix,
		"local_oauth":      s.cfg.EnableLocalOAuthListeners,
		"min_password_len": authn.MinPasswordLength,
		"needs_setup":      s.needsSetup(r),
		"username_rule":    usernameRequirement,
	})
	return nil
}

// clientIP resolves the caller's address, trusting proxy headers only when
// configured to.
func (s *Server) clientIP(r *http.Request) string {
	if s.cfg != nil && s.cfg.TrustProxyHeaders {
		if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
			if first, _, ok := strings.Cut(forwarded, ","); ok {
				return strings.TrimSpace(first)
			}
			return strings.TrimSpace(forwarded)
		}
		if real := strings.TrimSpace(r.Header.Get("X-Real-Ip")); real != "" {
			return real
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// queryInt reads an integer query parameter.
func queryInt(r *http.Request, name string, fallback int) int {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}

// queryBool reads a boolean query parameter.
func queryBool(r *http.Request, name string) bool {
	value, err := strconv.ParseBool(strings.TrimSpace(r.URL.Query().Get(name)))
	return err == nil && value
}

// trim is shorthand for the validation code below.
func trim(s string) string { return strings.TrimSpace(s) }

// ptr returns a pointer to v, for the partial-update structs in the store.
func ptr[T any](v T) *T { return &v }

// jsonArray replaces a nil slice with an empty one so that it marshals as []
// rather than null.
//
// This matters because the UI is written in TypeScript against these responses:
// a field typed Model[] that arrives as null turns the first .length or .map
// into a runtime TypeError, and a fresh install is exactly when the slices are
// empty. Handlers that build their own slices use make([]T, 0, n) and need
// nothing; wrap values that come straight from the store, the router or the
// registry, where nil is the natural zero value.
func jsonArray[T any](in []T) []T {
	if in == nil {
		return []T{}
	}
	return in
}
