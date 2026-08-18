package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"aihub/internal/authn"
	"aihub/internal/model"
	"aihub/internal/store"
)

const (
	// refreshCookieName holds the rotating refresh token. It is httpOnly so the
	// SPA never has to store a long-lived credential itself.
	refreshCookieName = "aihub_refresh"
	// refreshCookiePath keeps the cookie off every other endpoint, including the
	// proxy routes.
	refreshCookiePath = "/api/auth"
)

// authResponse is what the UI receives after a successful login or refresh.
type authResponse struct {
	AccessToken string       `json:"access_token"`
	TokenType   string       `json:"token_type"`
	ExpiresAt   time.Time    `json:"expires_at"`
	ExpiresIn   int64        `json:"expires_in"`
	User        *model.User  `json:"user"`
	Quota       *model.Quota `json:"quota,omitempty"`
	// RefreshToken is also set in an httpOnly cookie. It is returned in the body
	// for non-browser clients (scripts, the CLI) that have no cookie jar.
	RefreshToken string `json:"refresh_token,omitempty"`
}

// login exchanges a username and password for an access token and a refresh
// session.
func (s *Server) login(w http.ResponseWriter, r *http.Request) *apiError {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if apiErr := decodeJSON(r, &body); apiErr != nil {
		return apiErr
	}
	username := trim(body.Username)
	if username == "" || body.Password == "" {
		return invalid(map[string]string{"username": "required", "password": "required"})
	}

	// The throttle is keyed on the address, not the username, so guessing many
	// accounts from one host is as slow as guessing one.
	guardKey := s.clientIP(r)
	if wait, ok := s.loginGuard.allow(guardKey); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		return errorf(http.StatusTooManyRequests, "too_many_attempts",
			"too many failed sign-in attempts; try again in %s", wait.Round(time.Second))
	}

	user, err := s.store.GetUserByUsername(r.Context(), username)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return storeError(err, "look up account")
	}
	// A missing account and a wrong password must be indistinguishable, and must
	// cost the same amount of time, so the bcrypt comparison always runs.
	hash := ""
	if user != nil {
		hash = user.PasswordHash
	}
	if !authn.CheckPassword(hash, body.Password) || user == nil {
		s.loginGuard.fail(guardKey)
		return errorf(http.StatusUnauthorized, "invalid_credentials", "incorrect username or password")
	}
	if user.Status != model.StatusActive {
		return errorf(http.StatusForbidden, "account_"+user.Status, "this account is %s", user.Status)
	}
	s.loginGuard.reset(guardKey)

	resp, apiErr := s.issueSession(w, r, user)
	if apiErr != nil {
		return apiErr
	}
	if err = s.store.TouchUserLogin(r.Context(), user.ID); err != nil {
		s.log.Warn("could not record login time", "user", user.ID, "error", err)
	}

	writeJSON(w, http.StatusOK, resp)
	return nil
}

// refresh rotates a refresh session and mints a new access token. The old
// refresh token stops working, so a stolen one is usable at most once.
func (s *Server) refresh(w http.ResponseWriter, r *http.Request) *apiError {
	token := s.refreshTokenFrom(r)
	if token == "" {
		return errorf(http.StatusUnauthorized, "unauthenticated", "no refresh token supplied")
	}

	sess, user, err := s.store.LookupWebSession(r.Context(), token)
	if err != nil {
		s.clearRefreshCookie(w, r)
		return errorf(http.StatusUnauthorized, "unauthenticated", "the session has expired; sign in again")
	}

	expiresAt := time.Now().Add(s.refreshTTL())
	rotated, err := s.store.RotateWebSession(r.Context(), sess.ID, expiresAt)
	if err != nil {
		s.clearRefreshCookie(w, r)
		return errorf(http.StatusUnauthorized, "unauthenticated", "the session has expired; sign in again")
	}

	access, accessExpiry, err := s.issuer.Issue(user, sess.ID)
	if err != nil {
		return errorf(http.StatusInternalServerError, "internal", "could not issue an access token")
	}
	s.setRefreshCookie(w, r, rotated, time.Until(expiresAt))

	quota, err := s.store.GetQuota(r.Context(), user.ID)
	if err != nil {
		return storeError(err, "load quota")
	}

	writeJSON(w, http.StatusOK, &authResponse{
		AccessToken:  access,
		TokenType:    "Bearer",
		ExpiresAt:    accessExpiry,
		ExpiresIn:    int64(time.Until(accessExpiry).Seconds()),
		User:         user,
		Quota:        &quota,
		RefreshToken: rotated,
	})
	return nil
}

// logout revokes the caller's refresh session. It is deliberately tolerant: an
// already-invalid token still clears the cookie and reports success.
func (s *Server) logout(w http.ResponseWriter, r *http.Request) *apiError {
	if token := s.refreshTokenFrom(r); token != "" {
		if err := s.store.RevokeWebSessionByToken(r.Context(), token); err != nil {
			s.log.Debug("could not revoke session by token", "error", err)
		}
	}
	// A caller that only has an access token can still end its own session.
	if access := bearerToken(r); access != "" {
		if claims, err := s.issuer.Verify(access); err == nil && claims.SessionID != "" {
			if id, parseErr := uuid.Parse(claims.SessionID); parseErr == nil {
				if err = s.store.RevokeWebSession(r.Context(), id); err != nil {
					s.log.Debug("could not revoke session by id", "error", err)
				}
			}
		}
	}

	s.clearRefreshCookie(w, r)
	writeJSON(w, http.StatusOK, map[string]string{"status": "signed out"})
	return nil
}

// me returns the signed-in account together with the limits applied to it.
func (s *Server) me(w http.ResponseWriter, r *http.Request) *apiError {
	caller := callerFrom(r.Context())

	quota, err := s.store.GetQuota(r.Context(), caller.User.ID)
	if err != nil {
		return storeError(err, "load quota")
	}
	connections, err := s.store.CountConnections(r.Context(), caller.User.ID)
	if err != nil {
		return storeError(err, "count connections")
	}
	keys, err := s.store.CountAPIKeys(r.Context(), caller.User.ID)
	if err != nil {
		return storeError(err, "count api keys")
	}
	pending, err := s.store.CountPendingOAuthSessions(r.Context(), caller.User.ID)
	if err != nil {
		return storeError(err, "count pending connections")
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"user":  caller.User,
		"quota": quota,
		"counts": map[string]int{
			"connections":         connections,
			"api_keys":            keys,
			"pending_connections": pending,
		},
	})
	return nil
}

// changePassword updates the caller's own password and re-issues a session, so
// every other browser is signed out but this one keeps working.
func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) *apiError {
	caller := callerFrom(r.Context())

	var body struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if apiErr := decodeJSON(r, &body); apiErr != nil {
		return apiErr
	}
	if !authn.CheckPassword(caller.User.PasswordHash, body.CurrentPassword) {
		return errorf(http.StatusForbidden, "invalid_credentials", "the current password is incorrect")
	}
	hash, err := authn.HashPassword(body.NewPassword)
	if err != nil {
		return invalid(map[string]string{"new_password": err.Error()})
	}

	if _, err = s.store.UpdateUser(r.Context(), caller.User.ID, store.UserUpdate{PasswordHash: &hash}); err != nil {
		return storeError(err, "update password")
	}
	if err = s.store.RevokeUserWebSessions(r.Context(), caller.User.ID); err != nil {
		return storeError(err, "revoke sessions")
	}

	caller.User.PasswordHash = hash
	resp, apiErr := s.issueSession(w, r, caller.User)
	if apiErr != nil {
		return apiErr
	}
	writeJSON(w, http.StatusOK, resp)
	return nil
}

// listWebSessions shows where the account is signed in.
func (s *Server) listWebSessions(w http.ResponseWriter, r *http.Request) *apiError {
	caller := callerFrom(r.Context())

	sessions, err := s.store.ListWebSessions(r.Context(), caller.User.ID)
	if err != nil {
		return storeError(err, "list sessions")
	}

	current := caller.Claims.SessionID
	out := make([]map[string]any, 0, len(sessions))
	for _, sess := range sessions {
		out = append(out, map[string]any{
			"id":         sess.ID,
			"user_agent": sess.UserAgent,
			"ip":         sess.IP,
			"created_at": sess.CreatedAt,
			"expires_at": sess.ExpiresAt,
			"current":    sess.ID.String() == current,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
	return nil
}

// revokeWebSession signs one browser out.
func (s *Server) revokeWebSession(w http.ResponseWriter, r *http.Request) *apiError {
	caller := callerFrom(r.Context())
	id, apiErr := pathUUID(r, "id")
	if apiErr != nil {
		return apiErr
	}

	// The store revokes by id alone, so ownership is checked here.
	sessions, err := s.store.ListWebSessions(r.Context(), caller.User.ID)
	if err != nil {
		return storeError(err, "list sessions")
	}
	owned := false
	for _, sess := range sessions {
		if sess.ID == id {
			owned = true
			break
		}
	}
	if !owned {
		return errorf(http.StatusNotFound, "not_found", "no such session")
	}
	if err = s.store.RevokeWebSession(r.Context(), id); err != nil {
		return storeError(err, "revoke session")
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
	return nil
}

// ---------------------------------------------------------------------------
// Session plumbing
// ---------------------------------------------------------------------------

// issueSession creates a refresh session and its first access token.
func (s *Server) issueSession(w http.ResponseWriter, r *http.Request, user *model.User) (*authResponse, *apiError) {
	ttl := s.refreshTTL()
	sess := &model.WebSession{
		UserID:    user.ID,
		UserAgent: r.UserAgent(),
		IP:        s.clientIP(r),
		ExpiresAt: time.Now().Add(ttl),
	}
	refreshToken, err := s.store.CreateWebSession(r.Context(), sess)
	if err != nil {
		return nil, storeError(err, "create session")
	}

	access, expiresAt, err := s.issuer.Issue(user, sess.ID)
	if err != nil {
		return nil, errorf(http.StatusInternalServerError, "internal", "could not issue an access token")
	}
	s.setRefreshCookie(w, r, refreshToken, ttl)

	quota, err := s.store.GetQuota(r.Context(), user.ID)
	if err != nil {
		return nil, storeError(err, "load quota")
	}

	return &authResponse{
		AccessToken:  access,
		TokenType:    "Bearer",
		ExpiresAt:    expiresAt,
		ExpiresIn:    int64(time.Until(expiresAt).Seconds()),
		User:         user,
		Quota:        &quota,
		RefreshToken: refreshToken,
	}, nil
}

// refreshTokenFrom reads the refresh token from the cookie, the body or a
// header, in that order.
func (s *Server) refreshTokenFrom(r *http.Request) string {
	if cookie, err := r.Cookie(refreshCookieName); err == nil && cookie.Value != "" {
		return cookie.Value
	}
	if header := trim(r.Header.Get("X-Refresh-Token")); header != "" {
		return header
	}
	var body struct {
		RefreshToken string `json:"refresh_token"`
	}
	if r.Body != nil {
		// A missing or empty body is normal here: the cookie is the usual carrier.
		_ = decodeJSON(r, &body)
	}
	return trim(body.RefreshToken)
}

func (s *Server) setRefreshCookie(w http.ResponseWriter, r *http.Request, token string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     refreshCookieName,
		Value:    token,
		Path:     refreshCookiePath,
		HttpOnly: true,
		Secure:   s.secureCookies(r),
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(ttl),
		MaxAge:   int(ttl.Seconds()),
	})
}

func (s *Server) clearRefreshCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     refreshCookieName,
		Value:    "",
		Path:     refreshCookiePath,
		HttpOnly: true,
		Secure:   s.secureCookies(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// secureCookies reports whether the connection is (or terminates as) HTTPS.
// Marking the cookie Secure on a plain-HTTP deployment would silently drop it,
// which is why this is detected rather than assumed.
func (s *Server) secureCookies(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if s.cfg != nil {
		if s.cfg.TrustProxyHeaders && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
			return true
		}
		if strings.HasPrefix(s.cfg.PublicURL, "https://") {
			return true
		}
	}
	return false
}

func (s *Server) refreshTTL() time.Duration {
	if s.cfg != nil && s.cfg.RefreshTokenTTL > 0 {
		return s.cfg.RefreshTokenTTL
	}
	return 30 * 24 * time.Hour
}

// ---------------------------------------------------------------------------
// Sign-in throttle
// ---------------------------------------------------------------------------

const (
	// maxLoginFailures is how many wrong passwords one address may try before it
	// has to wait.
	maxLoginFailures = 8
	// loginLockout is how long the address is refused after that.
	loginLockout = 5 * time.Minute
)

// throttle counts failed sign-in attempts per key. It is per process and
// memory-only: the point is to make online guessing slow, not to be a durable
// audit trail.
type throttle struct {
	mu      sync.Mutex
	entries map[string]*throttleEntry
}

type throttleEntry struct {
	failures int
	until    time.Time
}

func newThrottle() *throttle { return &throttle{entries: map[string]*throttleEntry{}} }

// allow reports whether the key may attempt a sign-in, and if not, how long is
// left on its lockout.
func (t *throttle) allow(key string) (time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	entry := t.entries[key]
	if entry == nil {
		return 0, true
	}
	if wait := time.Until(entry.until); wait > 0 {
		return wait, false
	}
	// The lockout elapsed, so the counter starts over.
	delete(t.entries, key)
	return 0, true
}

func (t *throttle) fail(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	entry := t.entries[key]
	if entry == nil {
		entry = &throttleEntry{}
		t.entries[key] = entry
	}
	entry.failures++
	if entry.failures >= maxLoginFailures {
		entry.until = time.Now().Add(loginLockout)
		entry.failures = 0
	}

	// Opportunistic cleanup keeps the map from growing without bound; there is no
	// separate janitor for it.
	if len(t.entries) > 4096 {
		now := time.Now()
		for k, e := range t.entries {
			if e.until.Before(now) {
				delete(t.entries, k)
			}
		}
	}
}

func (t *throttle) reset(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.entries, key)
}
