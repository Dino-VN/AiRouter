package httpapi

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"aihub/internal/model"
	"aihub/internal/provider"
	"aihub/internal/store"
)

// maxPendingOAuth bounds how many half-finished sign-ins one account may hold.
// Each one occupies a state token and a row, and abandoned attempts are the
// normal case, so they are capped rather than trusted to be cleaned up.
const maxPendingOAuth = 6

// listProviders describes the upstreams this build can sign in to, with the
// caller's own connection counts.
func (s *Server) listProviders(w http.ResponseWriter, r *http.Request) *apiError {
	caller := callerFrom(r.Context())

	quota, err := s.store.GetQuota(r.Context(), caller.User.ID)
	if err != nil {
		return storeError(err, "load quota")
	}
	conns, err := s.store.ListConnections(r.Context(), store.ConnectionFilter{OwnerID: caller.User.ID})
	if err != nil {
		return storeError(err, "list connections")
	}
	owned := map[model.Provider]int{}
	usable := map[model.Provider]int{}
	now := time.Now()
	for _, conn := range conns {
		owned[conn.Provider]++
		if conn.Usable(now) {
			usable[conn.Provider]++
		}
	}

	catalog := s.registry.Catalog()
	out := make([]map[string]any, 0, 2)
	for _, vendor := range s.registry.All() {
		id := vendor.ID()
		out = append(out, map[string]any{
			"id":            id,
			"display_name":  vendor.DisplayName(),
			"loopback_port": vendor.LoopbackPort(),
			"callback_path": vendor.CallbackPath(),
			"redirect_uri":  loopbackRedirect(vendor),
			"allowed":       providerAllowed(quota, id),
			"connections":   owned[id],
			"usable":        usable[id],
			"models":        len(catalog.ForProvider(id)),
			"auto_callback": s.cfg != nil && s.cfg.EnableLocalOAuthListeners,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{"providers": out})
	return nil
}

// listCatalog reports which models the caller can actually reach, and what the
// full catalog knows about.
func (s *Server) listCatalog(w http.ResponseWriter, r *http.Request) *apiError {
	caller := callerFrom(r.Context())

	allowShared, apiErr := s.allowShared(r, caller)
	if apiErr != nil {
		return apiErr
	}
	catalog := s.registry.Catalog()

	writeJSON(w, http.StatusOK, map[string]any{
		"models":       jsonArray(s.router.AvailableModels(r.Context(), caller.User, allowShared)),
		"catalog":      jsonArray(catalog.All()),
		"refreshed_at": catalog.RefreshedAt(),
	})
	return nil
}

// listConnections returns the caller's upstream accounts, plus shared ones when
// their quota allows using the shared pool. Admins may ask for every account.
func (s *Server) listConnections(w http.ResponseWriter, r *http.Request) *apiError {
	caller := callerFrom(r.Context())

	allowShared, apiErr := s.allowShared(r, caller)
	if apiErr != nil {
		return apiErr
	}

	filter := store.ConnectionFilter{
		OwnerID:       caller.User.ID,
		IncludeShared: allowShared,
		UsableOnly:    queryBool(r, "usable_only"),
	}
	if queryBool(r, "all") {
		if !caller.User.IsAdmin() {
			return errorf(http.StatusForbidden, "forbidden", "only an admin can list every connection")
		}
		filter.OwnerID = uuid.Nil
	}
	if raw := trim(r.URL.Query().Get("provider")); raw != "" {
		id := model.Provider(strings.ToLower(raw))
		if !id.Valid() {
			return invalid(map[string]string{"provider": "unknown provider " + raw})
		}
		filter.Provider = id
	}

	conns, err := s.store.ListConnections(r.Context(), filter)
	if err != nil {
		return storeError(err, "list connections")
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"connections": jsonArray(conns),
		"user_id":     caller.User.ID,
	})
	return nil
}

// getConnection returns one account in detail.
func (s *Server) getConnection(w http.ResponseWriter, r *http.Request) *apiError {
	conn, apiErr := s.loadConnection(r, false)
	if apiErr != nil {
		return apiErr
	}

	// The credential itself never leaves the process; its expiry does, because
	// the UI shows when a re-sign-in is due.
	writeJSON(w, http.StatusOK, map[string]any{
		"connection":     conn,
		"token_expired":  conn.Credential == nil || conn.Credential.Expired(0),
		"has_refresh":    conn.Credential != nil && conn.Credential.RefreshToken != "",
		"last_refreshed": credentialRefreshedAt(conn),
	})
	return nil
}

// patchConnection changes the operator-controlled settings of an account.
func (s *Server) patchConnection(w http.ResponseWriter, r *http.Request) *apiError {
	caller := callerFrom(r.Context())
	conn, apiErr := s.loadConnection(r, true)
	if apiErr != nil {
		return apiErr
	}

	var body struct {
		Label  *string `json:"label"`
		Scope  *string `json:"scope"`
		Weight *int    `json:"weight"`
		Status *string `json:"status"`
	}
	if apiErr = decodeJSON(r, &body); apiErr != nil {
		return apiErr
	}

	settings := store.ConnectionSettings{}
	if body.Label != nil {
		label := trim(*body.Label)
		if label == "" {
			return invalid(map[string]string{"label": "must not be empty"})
		}
		settings.Label = &label
	}
	if body.Scope != nil {
		scope := strings.ToLower(trim(*body.Scope))
		switch scope {
		case model.ScopePrivate:
		case model.ScopeShared:
			// Sharing an account exposes its quota to every user, so it stays an
			// admin decision.
			if !caller.User.IsAdmin() {
				return errorf(http.StatusForbidden, "forbidden", "only an admin can share a connection")
			}
		default:
			return invalid(map[string]string{"scope": "must be private or shared"})
		}
		settings.Scope = &scope
	}
	if body.Weight != nil {
		weight := *body.Weight
		if weight < 1 || weight > 100 {
			return invalid(map[string]string{"weight": "must be between 1 and 100"})
		}
		settings.Weight = &weight
	}
	if body.Status != nil {
		status := strings.ToLower(trim(*body.Status))
		// Only the two states an operator owns are settable; "error" and "expired"
		// are set by the router from what upstream said.
		if status != model.ConnStatusActive && status != model.ConnStatusDisabled {
			return invalid(map[string]string{"status": "must be active or disabled"})
		}
		settings.Status = &status
	}
	if settings.Label == nil && settings.Scope == nil && settings.Weight == nil && settings.Status == nil {
		return invalid(map[string]string{"body": "nothing to update"})
	}

	if err := s.store.UpdateConnectionSettings(r.Context(), conn.ID, settings); err != nil {
		return storeError(err, "update connection")
	}
	// Re-enabling an account should also clear the failure that disabled it, or
	// the cooldown would keep it out of rotation.
	if settings.Status != nil && *settings.Status == model.ConnStatusActive {
		if err := s.store.ClearConnectionError(r.Context(), conn.ID); err != nil {
			s.log.Warn("could not clear connection error", "connection", conn.ID, "error", err)
		}
	}

	updated, err := s.store.GetConnection(r.Context(), conn.ID)
	if err != nil {
		return storeError(err, "reload connection")
	}
	writeJSON(w, http.StatusOK, map[string]any{"connection": updated})
	return nil
}

// deleteConnection removes an upstream account.
func (s *Server) deleteConnection(w http.ResponseWriter, r *http.Request) *apiError {
	conn, apiErr := s.loadConnection(r, true)
	if apiErr != nil {
		return apiErr
	}
	if err := s.store.DeleteConnection(r.Context(), conn.ID); err != nil {
		return storeError(err, "delete connection")
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	return nil
}

// refreshConnection forces a token refresh, which is how a user recovers an
// account that went into error without signing in again.
func (s *Server) refreshConnection(w http.ResponseWriter, r *http.Request) *apiError {
	conn, apiErr := s.loadConnection(r, true)
	if apiErr != nil {
		return apiErr
	}

	if _, err := s.registry.Tokens().ForceRefresh(r.Context(), conn); err != nil {
		return errorf(http.StatusBadGateway, "refresh_failed",
			"could not refresh the token: %s; the account probably has to be signed in again", err)
	}

	updated, err := s.store.GetConnection(r.Context(), conn.ID)
	if err != nil {
		return storeError(err, "reload connection")
	}
	writeJSON(w, http.StatusOK, map[string]any{"connection": updated})
	return nil
}

// refreshConnectionQuota asks the provider what is left on the account.
func (s *Server) refreshConnectionQuota(w http.ResponseWriter, r *http.Request) *apiError {
	conn, apiErr := s.loadConnection(r, true)
	if apiErr != nil {
		return apiErr
	}

	quota, err := s.registry.Tokens().RefreshQuota(r.Context(), conn)
	if err != nil {
		return errorf(http.StatusBadGateway, "quota_unavailable", "could not read the quota: %s", err)
	}
	if quota == nil {
		// Codex only reports its windows in response headers, so an account that
		// has not served a request yet has nothing to show.
		writeJSON(w, http.StatusOK, map[string]any{
			"quota": nil,
			"note":  "this provider only reports quota while serving requests; send one request and check again",
		})
		return nil
	}

	writeJSON(w, http.StatusOK, map[string]any{"quota": quota})
	return nil
}

// ---------------------------------------------------------------------------
// Temporary connections: in-flight OAuth attempts
// ---------------------------------------------------------------------------

// listOAuthSessions lists sign-in attempts. These are the "temporary
// connections": rows that exist only between asking for a consent URL and
// redeeming (or abandoning) it.
func (s *Server) listOAuthSessions(w http.ResponseWriter, r *http.Request) *apiError {
	caller := callerFrom(r.Context())

	owner := caller.User.ID
	if queryBool(r, "all") {
		if !caller.User.IsAdmin() {
			return errorf(http.StatusForbidden, "forbidden", "only an admin can list every attempt")
		}
		owner = uuid.Nil
	}
	// Expiring first means the list never shows a stale "pending" row that the
	// provider would already refuse.
	if _, err := s.store.ExpireOAuthSessions(r.Context()); err != nil {
		s.log.Warn("could not expire oauth sessions", "error", err)
	}

	sessions, err := s.store.ListOAuthSessions(r.Context(), owner, queryBool(r, "pending"), queryInt(r, "limit", 25))
	if err != nil {
		return storeError(err, "list connection attempts")
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"sessions":    jsonArray(sessions),
		"max_pending": maxPendingOAuth,
	})
	return nil
}

// startOAuth opens a sign-in attempt and returns the consent URL.
func (s *Server) startOAuth(w http.ResponseWriter, r *http.Request) *apiError {
	caller := callerFrom(r.Context())

	var body struct {
		Provider    string `json:"provider"`
		Label       string `json:"label"`
		Scope       string `json:"scope"`
		RedirectURI string `json:"redirect_uri"`
	}
	if apiErr := decodeJSON(r, &body); apiErr != nil {
		return apiErr
	}

	providerID := model.Provider(strings.ToLower(trim(body.Provider)))
	if !providerID.Valid() {
		return invalid(map[string]string{"provider": "must be codex or antigravity"})
	}
	vendor, err := s.registry.Get(providerID)
	if err != nil {
		return errorf(http.StatusNotImplemented, "provider_unavailable", "%s is not available in this build", providerID)
	}

	quota, err := s.store.GetQuota(r.Context(), caller.User.ID)
	if err != nil {
		return storeError(err, "load quota")
	}
	if !providerAllowed(quota, providerID) {
		return errorf(http.StatusForbidden, "provider_not_allowed",
			"your account is not allowed to use %s", providerID)
	}
	if quota.MaxConnections > 0 {
		count, countErr := s.store.CountConnections(r.Context(), caller.User.ID)
		if countErr != nil {
			return storeError(countErr, "count connections")
		}
		if count >= quota.MaxConnections {
			return errorf(http.StatusConflict, "connection_limit",
				"you already have %d of %d allowed connections; remove one first",
				count, quota.MaxConnections)
		}
	}

	if _, err = s.store.ExpireOAuthSessions(r.Context()); err != nil {
		s.log.Warn("could not expire oauth sessions", "error", err)
	}
	pending, err := s.store.CountPendingOAuthSessions(r.Context(), caller.User.ID)
	if err != nil {
		return storeError(err, "count connection attempts")
	}
	if pending >= maxPendingOAuth {
		return errorf(http.StatusConflict, "too_many_pending",
			"you have %d unfinished sign-ins; cancel one before starting another", pending)
	}

	scope := strings.ToLower(trim(body.Scope))
	switch scope {
	case "":
		scope = model.ScopePrivate
	case model.ScopePrivate:
	case model.ScopeShared:
		if !caller.User.IsAdmin() {
			return errorf(http.StatusForbidden, "forbidden", "only an admin can share a connection")
		}
	default:
		return invalid(map[string]string{"scope": "must be private or shared"})
	}

	authReq, err := vendor.BeginAuth(r.Context(), provider.AuthOptions{RedirectURI: trim(body.RedirectURI)})
	if err != nil {
		return errorf(http.StatusBadGateway, "auth_start_failed", "could not build the consent URL: %s", err)
	}

	ttl := authReq.TTL
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	sess := &model.OAuthSession{
		UserID:       caller.User.ID,
		Provider:     providerID,
		State:        authReq.State,
		CodeVerifier: authReq.CodeVerifier,
		RedirectURI:  authReq.RedirectURI,
		AuthURL:      authReq.AuthURL,
		Label:        trim(body.Label),
		TargetScope:  scope,
		Status:       model.OAuthPending,
		ExpiresAt:    time.Now().Add(ttl),
	}
	if err = s.store.CreateOAuthSession(r.Context(), sess); err != nil {
		return storeError(err, "record the connection attempt")
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"session":      sess,
		"instructions": authReq.Instructions,
		"expires_in":   int64(ttl.Seconds()),
		// True when this server is listening on the provider's loopback port, in
		// which case the browser finishes the flow on its own.
		"auto_callback": s.cfg != nil && s.cfg.EnableLocalOAuthListeners,
	})
	return nil
}

// completeOAuth redeems a pending attempt from a pasted code or callback URL.
func (s *Server) completeOAuth(w http.ResponseWriter, r *http.Request) *apiError {
	caller := callerFrom(r.Context())
	id, apiErr := pathUUID(r, "id")
	if apiErr != nil {
		return apiErr
	}

	var body struct {
		Code string `json:"code"`
		// URL accepts the whole redirect the browser landed on, which is what a
		// user can actually copy.
		URL string `json:"url"`
	}
	if apiErr = decodeJSON(r, &body); apiErr != nil {
		return apiErr
	}

	sess, err := s.store.GetOAuthSession(r.Context(), id)
	if err != nil {
		return storeError(err, "load the connection attempt")
	}
	if sess.UserID != caller.User.ID && !caller.User.IsAdmin() {
		return errorf(http.StatusNotFound, "not_found", "no such connection attempt")
	}

	code, apiErr := authCodeFrom(body.Code, body.URL, sess.State)
	if apiErr != nil {
		return apiErr
	}

	conn, apiErr := s.redeem(r.Context(), sess, code)
	if apiErr != nil {
		return apiErr
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"connection": conn,
		"session_id": sess.ID,
		"status":     model.OAuthCompleted,
	})
	return nil
}

// cancelOAuth abandons a pending attempt.
func (s *Server) cancelOAuth(w http.ResponseWriter, r *http.Request) *apiError {
	caller := callerFrom(r.Context())
	id, apiErr := pathUUID(r, "id")
	if apiErr != nil {
		return apiErr
	}

	sess, err := s.store.GetOAuthSession(r.Context(), id)
	if err != nil {
		return storeError(err, "load the connection attempt")
	}
	if sess.UserID != caller.User.ID && !caller.User.IsAdmin() {
		return errorf(http.StatusNotFound, "not_found", "no such connection attempt")
	}
	if sess.Status != model.OAuthPending {
		return errorf(http.StatusConflict, "not_pending", "this attempt is already %s", sess.Status)
	}
	if err = s.store.CancelOAuthSession(r.Context(), id); err != nil {
		return storeError(err, "cancel the connection attempt")
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": model.OAuthCancelled})
	return nil
}

// oauthCallback is where the provider's browser redirect lands. It is
// unauthenticated on purpose: the `state` parameter is the only thing that can
// identify the attempt, and it is a 32-byte secret held by this server.
//
// It answers HTML rather than JSON because a human is looking at it.
func (s *Server) oauthCallback(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	state := trim(query.Get("state"))
	if state == "" {
		s.callbackPage(w, http.StatusBadRequest, "Missing state",
			"This callback URL has no state parameter, so it cannot be matched to a sign-in.")
		return
	}

	sess, err := s.store.GetOAuthSessionByState(r.Context(), state)
	if err != nil {
		s.callbackPage(w, http.StatusNotFound, "Unknown sign-in",
			"This sign-in attempt is not on record. It may have expired or already been used.")
		return
	}

	// The provider reports a refusal in the query string rather than by status.
	if reason := firstNonEmpty(trim(query.Get("error_description")), trim(query.Get("error"))); reason != "" {
		if failErr := s.store.FailOAuthSession(r.Context(), sess.ID, reason); failErr != nil {
			s.log.Warn("could not record oauth failure", "session", sess.ID, "error", failErr)
		}
		s.callbackPage(w, http.StatusBadRequest, "Sign-in refused", reason)
		return
	}

	code := trim(query.Get("code"))
	if code == "" {
		s.callbackPage(w, http.StatusBadRequest, "Missing code", "The callback carried no authorization code.")
		return
	}

	// The browser is on the loopback listener, whose request context dies with
	// this response; the exchange is short but must not be cancelled halfway.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 60*time.Second)
	defer cancel()

	conn, apiErr := s.redeem(ctx, sess, code)
	if apiErr != nil {
		s.callbackPage(w, apiErr.Status, "Could not finish the sign-in", apiErr.Message)
		return
	}

	s.callbackPage(w, http.StatusOK, "Connected",
		fmt.Sprintf("%s is now connected as %s. You can close this tab and go back to AI Hub.",
			conn.Provider, conn.AccountEmail))
}

// redeem exchanges the authorization code and turns the attempt into a stored
// connection. It is shared by the pasted-code endpoint and the browser callback.
func (s *Server) redeem(ctx context.Context, sess *model.OAuthSession, code string) (*model.Connection, *apiError) {
	if sess.Status != model.OAuthPending {
		return nil, errorf(http.StatusConflict, "not_pending", "this attempt is already %s", sess.Status)
	}
	if !sess.ExpiresAt.IsZero() && time.Now().After(sess.ExpiresAt) {
		if err := s.store.FailOAuthSession(ctx, sess.ID, "expired before it was redeemed"); err != nil {
			s.log.Warn("could not expire oauth session", "session", sess.ID, "error", err)
		}
		return nil, errorf(http.StatusGone, "expired", "this sign-in expired; start a new one")
	}

	vendor, err := s.registry.Get(sess.Provider)
	if err != nil {
		return nil, errorf(http.StatusNotImplemented, "provider_unavailable",
			"%s is not available in this build", sess.Provider)
	}

	result, err := vendor.CompleteAuth(ctx, sess, code)
	if err != nil {
		if failErr := s.store.FailOAuthSession(ctx, sess.ID, err.Error()); failErr != nil {
			s.log.Warn("could not record oauth failure", "session", sess.ID, "error", failErr)
		}
		return nil, errorf(http.StatusBadGateway, "exchange_failed",
			"the provider rejected the authorization code: %s", err)
	}
	if result.Credential == nil || result.Credential.AccessToken == "" {
		if failErr := s.store.FailOAuthSession(ctx, sess.ID, "no access token returned"); failErr != nil {
			s.log.Warn("could not record oauth failure", "session", sess.ID, "error", failErr)
		}
		return nil, errorf(http.StatusBadGateway, "exchange_failed", "the provider returned no access token")
	}

	conn, apiErr := s.storeConnection(ctx, sess, result)
	if apiErr != nil {
		if failErr := s.store.FailOAuthSession(ctx, sess.ID, apiErr.Message); failErr != nil {
			s.log.Warn("could not record oauth failure", "session", sess.ID, "error", failErr)
		}
		return nil, apiErr
	}
	if err = s.store.CompleteOAuthSession(ctx, sess.ID, conn.ID); err != nil {
		s.log.Warn("could not close oauth session", "session", sess.ID, "error", err)
	}

	// A first quota read makes the new card in the UI useful straight away. It is
	// best-effort: Codex has no quota endpoint at all.
	if quota, quotaErr := s.registry.Tokens().RefreshQuota(ctx, conn); quotaErr != nil {
		s.log.Debug("initial quota read failed", "connection", conn.ID, "error", quotaErr)
	} else if quota != nil {
		conn.Quota = quota
	}

	return conn, nil
}

// storeConnection creates the connection, or refreshes the credential of the one
// that already holds this account. Signing the same account in twice is a repair
// action, not a way to accumulate duplicates.
func (s *Server) storeConnection(ctx context.Context, sess *model.OAuthSession,
	result *provider.AuthResult) (*model.Connection, *apiError) {

	accountEmail := strings.ToLower(trim(result.AccountEmail))

	if accountEmail != "" {
		existing, err := s.store.FindConnectionByAccount(ctx, sess.UserID, sess.Provider, accountEmail)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, storeError(err, "look for an existing connection")
		}
		if existing != nil {
			update := store.CredentialUpdate{
				Credential: result.Credential,
				AccountID:  result.AccountID,
				Plan:       result.Plan,
				ProjectID:  result.ProjectID,
				Status:     model.ConnStatusActive,
				ClearError: true,
			}
			if err = s.store.UpdateCredential(ctx, existing.ID, update); err != nil {
				return nil, storeError(err, "store the credential")
			}
			refreshed, err := s.store.GetConnection(ctx, existing.ID)
			if err != nil {
				return nil, storeError(err, "reload connection")
			}
			return refreshed, nil
		}
	}

	conn := &model.Connection{
		OwnerID:        sess.UserID,
		Provider:       sess.Provider,
		Label:          firstNonEmpty(sess.Label, accountEmail, string(sess.Provider)),
		AccountEmail:   accountEmail,
		AccountID:      result.AccountID,
		ProjectID:      result.ProjectID,
		Plan:           result.Plan,
		Status:         model.ConnStatusActive,
		Scope:          firstNonEmpty(sess.TargetScope, model.ScopePrivate),
		Weight:         1,
		Metadata:       result.Metadata,
		TokenExpiresAt: nonZeroTime(result.Credential.ExpiresAt),
	}
	if err := s.store.CreateConnection(ctx, conn, result.Credential); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, errorf(http.StatusConflict, "duplicate_connection",
				"that account is already connected")
		}
		return nil, storeError(err, "store the connection")
	}
	return conn, nil
}

// ---------------------------------------------------------------------------
// Loopback listeners
// ---------------------------------------------------------------------------

// startLoopbackListeners binds the vendor callback ports on localhost so a
// browser redirect completes the sign-in without anybody copying a URL. Both
// vendors only accept loopback redirects registered with their own OAuth client,
// so the port numbers are theirs, not ours.
//
// Failing to bind is not fatal: the copy-and-paste flow still works, and another
// instance (or the real CLI) may legitimately own the port.
func (s *Server) startLoopbackListeners() {
	for _, vendor := range s.registry.All() {
		port := vendor.LoopbackPort()
		if port <= 0 {
			continue
		}

		address := net.JoinHostPort("127.0.0.1", fmt.Sprint(port))
		listener, err := net.Listen("tcp", address)
		if err != nil {
			s.log.Warn("oauth loopback port unavailable; use the copy-and-paste flow",
				"provider", vendor.ID(), "address", address, "error", err)
			continue
		}

		// Every path is served: providers append their own query string, and a
		// mismatched path would leave the user with an unusable page.
		server := &http.Server{
			Handler:           http.HandlerFunc(s.oauthCallback),
			ReadHeaderTimeout: 10 * time.Second,
		}
		s.log.Info("listening for oauth callbacks", "provider", vendor.ID(), "address", address,
			"path", vendor.CallbackPath())
		go func(id model.Provider) {
			if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				s.log.Warn("oauth loopback listener stopped", "provider", id, "error", err)
			}
		}(vendor.ID())
	}
}

// callbackPage renders the small HTML page the browser lands on.
func (s *Server) callbackPage(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)

	accent := "#16a34a"
	if status >= 400 {
		accent = "#dc2626"
	}
	// The values are provider- and error-supplied, so they are escaped rather
	// than interpolated raw.
	page := `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>` + html.EscapeString(title) + ` · AI Hub</title>
<style>
  :root { color-scheme: light dark }
  body { margin:0; min-height:100vh; display:grid; place-items:center;
         font:16px/1.6 ui-sans-serif,system-ui,-apple-system,"Segoe UI",sans-serif;
         background:#0b0d12; color:#e6e8ee }
  .card { max-width:34rem; padding:2rem 2.25rem; border-radius:1rem; background:#141821;
          border:1px solid #232a37; box-shadow:0 20px 60px rgba(0,0,0,.45) }
  h1 { margin:0 0 .5rem; font-size:1.35rem; color:` + accent + ` }
  p { margin:0; color:#a8b0c0 }
</style></head>
<body><div class="card">
  <h1>` + html.EscapeString(title) + `</h1>
  <p>` + html.EscapeString(detail) + `</p>
</div></body></html>`

	if _, err := w.Write([]byte(page)); err != nil {
		s.log.Debug("could not write callback page", "error", err)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// loadConnection resolves the {id} path value and authorises the caller. When
// mutate is false a shared connection is also readable, because it is already
// listed to everyone who may route through it.
func (s *Server) loadConnection(r *http.Request, mutate bool) (*model.Connection, *apiError) {
	caller := callerFrom(r.Context())
	id, apiErr := pathUUID(r, "id")
	if apiErr != nil {
		return nil, apiErr
	}

	conn, err := s.store.GetConnection(r.Context(), id)
	if err != nil {
		return nil, storeError(err, "load connection")
	}
	if conn.OwnerID == caller.User.ID || caller.User.IsAdmin() {
		return conn, nil
	}
	if !mutate && conn.Scope == model.ScopeShared {
		allowShared, sharedErr := s.allowShared(r, caller)
		if sharedErr != nil {
			return nil, sharedErr
		}
		if allowShared {
			return conn, nil
		}
	}
	// Somebody else's connection is reported as missing rather than forbidden, so
	// ids cannot be probed.
	return nil, errorf(http.StatusNotFound, "not_found", "no such connection")
}

// allowShared reports whether the caller may use connections other people
// shared.
func (s *Server) allowShared(r *http.Request, caller *principal) (bool, *apiError) {
	if caller.User.IsAdmin() {
		return true, nil
	}
	quota, err := s.store.GetQuota(r.Context(), caller.User.ID)
	if err != nil {
		return false, storeError(err, "load quota")
	}
	return quota.AllowSharedPool, nil
}

// providerAllowed reports whether a quota permits a provider. An empty list
// means every provider is allowed.
func providerAllowed(quota model.Quota, id model.Provider) bool {
	if len(quota.AllowedProviders) == 0 {
		return true
	}
	for _, name := range quota.AllowedProviders {
		if strings.EqualFold(trim(name), string(id)) {
			return true
		}
	}
	return false
}

// authCodeFrom accepts either a bare authorization code or the whole redirect
// URL the browser landed on, which is what users can realistically copy.
func authCodeFrom(code, rawURL, wantState string) (string, *apiError) {
	code = trim(code)
	rawURL = trim(rawURL)

	// A pasted URL in the code field is a common enough mistake to just handle.
	if rawURL == "" && (strings.HasPrefix(code, "http://") || strings.HasPrefix(code, "https://")) {
		rawURL, code = code, ""
	}
	if rawURL != "" {
		parsed, err := url.Parse(rawURL)
		if err != nil {
			return "", invalid(map[string]string{"url": "could not be parsed as a URL"})
		}
		query := parsed.Query()
		if reason := firstNonEmpty(trim(query.Get("error_description")), trim(query.Get("error"))); reason != "" {
			return "", errorf(http.StatusBadRequest, "provider_error", "the provider refused the sign-in: %s", reason)
		}
		if state := trim(query.Get("state")); state != "" && wantState != "" && state != wantState {
			// A mismatched state is exactly what the parameter exists to catch.
			return "", errorf(http.StatusBadRequest, "state_mismatch",
				"that URL belongs to a different sign-in attempt")
		}
		code = trim(query.Get("code"))
	}
	if code == "" {
		return "", invalid(map[string]string{"code": "supply the authorization code or the full callback URL"})
	}
	return code, nil
}

// loopbackRedirect is the redirect URI a provider's OAuth client expects.
func loopbackRedirect(vendor provider.Provider) string {
	if vendor.LoopbackPort() <= 0 {
		return ""
	}
	return fmt.Sprintf("http://localhost:%d%s", vendor.LoopbackPort(), vendor.CallbackPath())
}

func credentialRefreshedAt(conn *model.Connection) *time.Time {
	if conn.Credential == nil || conn.Credential.LastRefresh.IsZero() {
		return nil
	}
	return &conn.Credential.LastRefresh
}

func nonZeroTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
