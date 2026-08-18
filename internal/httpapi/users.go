package httpapi

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"aihub/internal/authn"
	"aihub/internal/model"
	"aihub/internal/store"
)

// adminOverview is the data behind the admin landing page: one request that says
// how the whole deployment is doing.
func (s *Server) adminOverview(w http.ResponseWriter, r *http.Request) *apiError {
	ctx := r.Context()

	users, err := s.store.ListUsers(ctx)
	if err != nil {
		return storeError(err, "list users")
	}
	var active, admins int
	for _, user := range users {
		if user.Status == model.StatusActive {
			active++
		}
		if user.IsAdmin() {
			admins++
		}
	}

	conns, err := s.store.ListConnections(ctx, store.ConnectionFilter{})
	if err != nil {
		return storeError(err, "list connections")
	}
	now := time.Now()
	byProvider := map[string]int{}
	byStatus := map[string]int{}
	shared, usable := 0, 0
	for _, conn := range conns {
		byProvider[string(conn.Provider)]++
		byStatus[conn.Status]++
		if conn.Scope == model.ScopeShared {
			shared++
		}
		if conn.Usable(now) {
			usable++
		}
	}

	pending, err := s.store.ListOAuthSessions(ctx, uuid.Nil, true, 50)
	if err != nil {
		return storeError(err, "list pending connections")
	}
	keys, err := s.store.ListAPIKeys(ctx, uuid.Nil)
	if err != nil {
		return storeError(err, "list api keys")
	}
	activeKeys := 0
	for _, key := range keys {
		if key.Status == model.StatusActive {
			activeKeys++
		}
	}

	day, err := s.store.UsageTotals(ctx, store.UsageFilter{Since: now.Add(-24 * time.Hour)})
	if err != nil {
		return storeError(err, "aggregate usage")
	}
	month, err := s.store.UsageTotals(ctx, store.UsageFilter{Since: now.AddDate(0, 0, -30)})
	if err != nil {
		return storeError(err, "aggregate usage")
	}
	topModels, err := s.store.UsageBreakdown(ctx, store.UsageFilter{Since: now.AddDate(0, 0, -30)}, "model")
	if err != nil {
		return storeError(err, "break usage down by model")
	}
	topUsers, err := s.store.UsageBreakdown(ctx, store.UsageFilter{Since: now.AddDate(0, 0, -30)}, "user")
	if err != nil {
		return storeError(err, "break usage down by user")
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"version":        s.version,
		"started_at":     s.started,
		"uptime_seconds": int64(time.Since(s.started).Seconds()),
		"users": map[string]int{
			"total":  len(users),
			"active": active,
			"admins": admins,
		},
		"connections": map[string]any{
			"total":       len(conns),
			"usable":      usable,
			"shared":      shared,
			"by_provider": byProvider,
			"by_status":   byStatus,
		},
		"pending_connections": jsonArray(pending),
		"api_keys": map[string]int{
			"total":  len(keys),
			"active": activeKeys,
		},
		"usage": map[string]any{
			"last_24h": day,
			"last_30d": month,
		},
		"top_models":         head(topModels, 5),
		"top_users":          head(topUsers, 5),
		"catalog_refreshed":  s.registry.Catalog().RefreshedAt(),
		"providers_enabled":  s.providerIDs(),
		"database_reachable": s.store.Pool().Ping(ctx) == nil,
	})
	return nil
}

// listUsers returns every account with the numbers an admin needs to triage
// them. Connections, keys and usage are fetched in bulk rather than per user, so
// the page costs a fixed number of queries.
func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) *apiError {
	ctx := r.Context()

	users, err := s.store.ListUsers(ctx)
	if err != nil {
		return storeError(err, "list users")
	}
	conns, err := s.store.ListConnections(ctx, store.ConnectionFilter{})
	if err != nil {
		return storeError(err, "list connections")
	}
	keys, err := s.store.ListAPIKeys(ctx, uuid.Nil)
	if err != nil {
		return storeError(err, "list api keys")
	}
	usage, err := s.store.UsageBreakdown(ctx, store.UsageFilter{Since: time.Now().AddDate(0, 0, -30)}, "user")
	if err != nil {
		return storeError(err, "break usage down by user")
	}

	connCount := map[uuid.UUID]int{}
	for _, conn := range conns {
		connCount[conn.OwnerID]++
	}
	keyCount := map[uuid.UUID]int{}
	for _, key := range keys {
		if key.Status == model.StatusActive {
			keyCount[key.UserID]++
		}
	}
	usageByUser := map[string]model.UsageTotals{}
	for _, row := range usage {
		usageByUser[row.Key] = row.UsageTotals
	}

	out := make([]map[string]any, 0, len(users))
	for _, user := range users {
		// A missing quota row means the defaults apply, which GetQuota already
		// resolves for us.
		quota, quotaErr := s.store.GetQuota(ctx, user.ID)
		if quotaErr != nil {
			return storeError(quotaErr, "load quota")
		}
		out = append(out, map[string]any{
			"user":  user,
			"quota": quota,
			"counts": map[string]int{
				"connections": connCount[user.ID],
				"api_keys":    keyCount[user.ID],
			},
			"usage_30d": usageByUser[user.ID.String()],
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{"users": out})
	return nil
}

// createUser adds an account. When no password is given one is generated and
// returned exactly once, which is the usual way an admin invites somebody.
func (s *Server) createUser(w http.ResponseWriter, r *http.Request) *apiError {
	var body struct {
		Username    string       `json:"username"`
		DisplayName string       `json:"display_name"`
		Password    string       `json:"password"`
		Role        string       `json:"role"`
		Status      string       `json:"status"`
		Quota       *model.Quota `json:"quota"`
	}
	if apiErr := decodeJSON(r, &body); apiErr != nil {
		return apiErr
	}

	username := trim(body.Username)
	fields := map[string]string{}
	if !model.ValidUsername(username) {
		fields["username"] = usernameRequirement
	}
	role := model.Role(strings.ToLower(trim(body.Role)))
	if role == "" {
		role = model.RoleUser
	}
	if !role.Valid() {
		fields["role"] = "must be admin or user"
	}
	status := strings.ToLower(trim(body.Status))
	if status == "" {
		status = model.StatusActive
	}
	if !validUserStatus(status) {
		fields["status"] = "must be active, suspended or revoked"
	}
	if len(fields) > 0 {
		return invalid(fields)
	}

	// A generated password is returned in the response body; there is no mail
	// sender in this deployment, so the admin passes it on out of band.
	password := body.Password
	generated := false
	if password == "" {
		var err error
		if password, err = generatePassword(); err != nil {
			return errorf(http.StatusInternalServerError, "internal", "could not generate a password")
		}
		generated = true
	}
	hash, err := authn.HashPassword(password)
	if err != nil {
		return invalid(map[string]string{"password": err.Error()})
	}

	user := &model.User{
		Username:     username,
		DisplayName:  firstNonEmpty(trim(body.DisplayName), username),
		Role:         role,
		Status:       status,
		PasswordHash: hash,
	}
	quota := model.DefaultQuota(uuid.Nil)
	if role == model.RoleAdmin {
		quota = model.UnlimitedQuota(uuid.Nil)
	}
	if body.Quota != nil {
		quota = *body.Quota
	}
	if apiErr := validateQuota(&quota); apiErr != nil {
		return apiErr
	}

	if err = s.store.CreateUser(r.Context(), user, quota); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return errorf(http.StatusConflict, "username_taken", "an account with that username already exists")
		}
		return storeError(err, "create user")
	}

	payload := map[string]any{"user": user}
	if generated {
		payload["password"] = password
		payload["password_note"] = "shown once; the account should change it after signing in"
	}
	writeJSON(w, http.StatusCreated, payload)
	return nil
}

// getUser is the admin drill-down for one account.
func (s *Server) getUser(w http.ResponseWriter, r *http.Request) *apiError {
	id, apiErr := pathUUID(r, "id")
	if apiErr != nil {
		return apiErr
	}
	ctx := r.Context()

	user, err := s.store.GetUser(ctx, id)
	if err != nil {
		return storeError(err, "load user")
	}
	quota, err := s.store.GetQuota(ctx, id)
	if err != nil {
		return storeError(err, "load quota")
	}
	conns, err := s.store.ListConnections(ctx, store.ConnectionFilter{OwnerID: id})
	if err != nil {
		return storeError(err, "list connections")
	}
	keys, err := s.store.ListAPIKeys(ctx, id)
	if err != nil {
		return storeError(err, "list api keys")
	}
	pending, err := s.store.ListOAuthSessions(ctx, id, true, 20)
	if err != nil {
		return storeError(err, "list pending connections")
	}
	day, err := s.store.UsageWindow(ctx, id, time.Now().Add(-24*time.Hour))
	if err != nil {
		return storeError(err, "aggregate usage")
	}
	month, err := s.store.UsageWindow(ctx, id, time.Now().AddDate(0, 0, -30))
	if err != nil {
		return storeError(err, "aggregate usage")
	}
	sessions, err := s.store.ListWebSessions(ctx, id)
	if err != nil {
		return storeError(err, "list sessions")
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"user":                user,
		"quota":               quota,
		"connections":         jsonArray(conns),
		"api_keys":            jsonArray(keys),
		"pending_connections": jsonArray(pending),
		"usage": map[string]any{
			"last_24h": day,
			"last_30d": month,
		},
		"web_sessions": len(sessions),
	})
	return nil
}

// patchUser changes the display name, role or status of an account.
func (s *Server) patchUser(w http.ResponseWriter, r *http.Request) *apiError {
	caller := callerFrom(r.Context())
	id, apiErr := pathUUID(r, "id")
	if apiErr != nil {
		return apiErr
	}

	var body struct {
		DisplayName *string `json:"display_name"`
		Role        *string `json:"role"`
		Status      *string `json:"status"`
	}
	if apiErr = decodeJSON(r, &body); apiErr != nil {
		return apiErr
	}

	update := store.UserUpdate{}
	if body.DisplayName != nil {
		name := trim(*body.DisplayName)
		if name == "" {
			return invalid(map[string]string{"display_name": "must not be empty"})
		}
		update.DisplayName = &name
	}
	if body.Role != nil {
		role := model.Role(strings.ToLower(trim(*body.Role)))
		if !role.Valid() {
			return invalid(map[string]string{"role": "must be admin or user"})
		}
		// Losing your own admin rights would lock you out of this very page.
		if id == caller.User.ID && role != model.RoleAdmin {
			return errorf(http.StatusConflict, "self_demotion",
				"you cannot remove your own admin role; ask another admin to do it")
		}
		if role != model.RoleAdmin {
			if apiErr = s.protectLastAdmin(r, id); apiErr != nil {
				return apiErr
			}
		}
		update.Role = &role
	}
	if body.Status != nil {
		status := strings.ToLower(trim(*body.Status))
		if !validUserStatus(status) {
			return invalid(map[string]string{"status": "must be active, suspended or revoked"})
		}
		if id == caller.User.ID && status != model.StatusActive {
			return errorf(http.StatusConflict, "self_lockout", "you cannot suspend your own account")
		}
		if status != model.StatusActive {
			if apiErr = s.protectLastAdmin(r, id); apiErr != nil {
				return apiErr
			}
		}
		update.Status = &status
	}
	if update.DisplayName == nil && update.Role == nil && update.Status == nil {
		return invalid(map[string]string{"body": "nothing to update"})
	}

	user, err := s.store.UpdateUser(r.Context(), id, update)
	if err != nil {
		return storeError(err, "update user")
	}
	// A role or status change has to take effect immediately, and access tokens
	// carry the old role until they expire, so the sessions go.
	if update.Role != nil || update.Status != nil {
		if err = s.store.RevokeUserWebSessions(r.Context(), id); err != nil {
			s.log.Warn("could not revoke sessions after user change", "user", id, "error", err)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"user": user})
	return nil
}

// deleteUser removes an account along with everything the schema cascades from
// it: connections, keys, sessions and usage rows.
func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) *apiError {
	caller := callerFrom(r.Context())
	id, apiErr := pathUUID(r, "id")
	if apiErr != nil {
		return apiErr
	}
	if id == caller.User.ID {
		return errorf(http.StatusConflict, "self_deletion", "you cannot delete your own account")
	}
	if apiErr = s.protectLastAdmin(r, id); apiErr != nil {
		return apiErr
	}

	if err := s.store.DeleteUser(r.Context(), id); err != nil {
		return storeError(err, "delete user")
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	return nil
}

// putUserQuota replaces one account's limits.
func (s *Server) putUserQuota(w http.ResponseWriter, r *http.Request) *apiError {
	id, apiErr := pathUUID(r, "id")
	if apiErr != nil {
		return apiErr
	}

	var quota model.Quota
	if apiErr = decodeJSON(r, &quota); apiErr != nil {
		return apiErr
	}
	if apiErr = validateQuota(&quota); apiErr != nil {
		return apiErr
	}
	// The path wins over anything the body claims.
	quota.UserID = id

	if _, err := s.store.GetUser(r.Context(), id); err != nil {
		return storeError(err, "load user")
	}
	saved, err := s.store.UpsertQuota(r.Context(), quota)
	if err != nil {
		return storeError(err, "save quota")
	}

	writeJSON(w, http.StatusOK, map[string]any{"quota": saved})
	return nil
}

// setUserPassword resets somebody else's password, generating one when none is
// supplied. Every session of that account is dropped.
func (s *Server) setUserPassword(w http.ResponseWriter, r *http.Request) *apiError {
	id, apiErr := pathUUID(r, "id")
	if apiErr != nil {
		return apiErr
	}

	var body struct {
		Password string `json:"password"`
	}
	if apiErr = decodeJSON(r, &body); apiErr != nil {
		return apiErr
	}

	password := body.Password
	generated := false
	if password == "" {
		var err error
		if password, err = generatePassword(); err != nil {
			return errorf(http.StatusInternalServerError, "internal", "could not generate a password")
		}
		generated = true
	}
	hash, err := authn.HashPassword(password)
	if err != nil {
		return invalid(map[string]string{"password": err.Error()})
	}

	if _, err = s.store.UpdateUser(r.Context(), id, store.UserUpdate{PasswordHash: &hash}); err != nil {
		return storeError(err, "update password")
	}
	if err = s.store.RevokeUserWebSessions(r.Context(), id); err != nil {
		s.log.Warn("could not revoke sessions after password reset", "user", id, "error", err)
	}

	payload := map[string]any{"status": "updated"}
	if generated {
		payload["password"] = password
	}
	writeJSON(w, http.StatusOK, payload)
	return nil
}

// ---------------------------------------------------------------------------
// Shared checks
// ---------------------------------------------------------------------------

// protectLastAdmin refuses a change that would leave the deployment with no way
// in. It is the one invariant the schema cannot express.
func (s *Server) protectLastAdmin(r *http.Request, target uuid.UUID) *apiError {
	users, err := s.store.ListUsers(r.Context())
	if err != nil {
		return storeError(err, "list users")
	}
	for _, user := range users {
		if user.ID == target || !user.IsAdmin() || user.Status != model.StatusActive {
			continue
		}
		return nil
	}
	return errorf(http.StatusConflict, "last_admin",
		"this is the only active admin account; promote another admin first")
}

// validateQuota rejects negative limits and unknown providers, and normalises
// the provider list.
func validateQuota(quota *model.Quota) *apiError {
	fields := map[string]string{}
	if quota.RequestsPerDay < 0 {
		fields["requests_per_day"] = "must not be negative (0 means unlimited)"
	}
	if quota.TokensPerDay < 0 {
		fields["tokens_per_day"] = "must not be negative (0 means unlimited)"
	}
	if quota.RequestsPerMonth < 0 {
		fields["requests_per_month"] = "must not be negative (0 means unlimited)"
	}
	if quota.TokensPerMonth < 0 {
		fields["tokens_per_month"] = "must not be negative (0 means unlimited)"
	}
	if quota.MaxConnections < 0 {
		fields["max_connections"] = "must not be negative (0 means unlimited)"
	}
	if quota.MaxAPIKeys < 0 {
		fields["max_api_keys"] = "must not be negative (0 means unlimited)"
	}
	if quota.ConcurrentLimit < 0 {
		fields["concurrent_limit"] = "must not be negative (0 means unlimited)"
	}

	cleaned := make([]string, 0, len(quota.AllowedProviders))
	for _, name := range quota.AllowedProviders {
		id := model.Provider(strings.ToLower(trim(name)))
		if id == "" {
			continue
		}
		if !id.Valid() {
			fields["allowed_providers"] = "unknown provider " + string(id)
			continue
		}
		cleaned = append(cleaned, string(id))
	}
	sort.Strings(cleaned)
	quota.AllowedProviders = cleaned

	if len(fields) > 0 {
		return invalid(fields)
	}
	return nil
}

func validUserStatus(status string) bool {
	switch status {
	case model.StatusActive, model.StatusSuspended, model.StatusRevoked:
		return true
	}
	return false
}

// usernameRequirement is the one wording used by every handler that validates a
// handle, so the setup screen and the admin form never disagree about the rule.
const usernameRequirement = "3-32 characters: letters, digits, dot, underscore or hyphen, " +
	"starting and ending with a letter or digit"

// generatePassword mints a random initial password.
func generatePassword() (string, error) {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// head returns at most n elements of a slice, and never nil: every caller feeds
// the result straight into a JSON response.
func head[T any](list []T, n int) []T {
	if len(list) <= n {
		return jsonArray(list)
	}
	return list[:n]
}

// providerIDs lists the providers this build actually wired up.
func (s *Server) providerIDs() []string {
	vendors := s.registry.All()
	out := make([]string, 0, len(vendors))
	for _, vendor := range vendors {
		out = append(out, string(vendor.ID()))
	}
	sort.Strings(out)
	return out
}
