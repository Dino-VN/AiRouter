package httpapi

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"aihub/internal/model"
	"aihub/internal/store"
)

// maxKeyNameLen keeps labels readable in the UI.
const maxKeyNameLen = 80

// listAPIKeys returns the caller's proxy keys. The secret is never included: it
// only exists in the response that created it.
func (s *Server) listAPIKeys(w http.ResponseWriter, r *http.Request) *apiError {
	caller := callerFrom(r.Context())

	owner := caller.User.ID
	if queryBool(r, "all") {
		if !caller.User.IsAdmin() {
			return errorf(http.StatusForbidden, "forbidden", "only an admin can list every key")
		}
		owner = uuid.Nil
	}

	keys, err := s.store.ListAPIKeys(r.Context(), owner)
	if err != nil {
		return storeError(err, "list api keys")
	}
	quota, err := s.store.GetQuota(r.Context(), caller.User.ID)
	if err != nil {
		return storeError(err, "load quota")
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"keys":     jsonArray(keys),
		"max_keys": quota.MaxAPIKeys,
		"prefix":   store.APIKeyPrefix,
	})
	return nil
}

// createAPIKey mints a proxy key and returns the plaintext once.
func (s *Server) createAPIKey(w http.ResponseWriter, r *http.Request) *apiError {
	caller := callerFrom(r.Context())

	var body struct {
		Name          string   `json:"name"`
		AllowedModels []string `json:"allowed_models"`
		// ExpiresInDays is friendlier than an absolute timestamp for a form.
		ExpiresInDays int        `json:"expires_in_days"`
		ExpiresAt     *time.Time `json:"expires_at"`
	}
	if apiErr := decodeJSON(r, &body); apiErr != nil {
		return apiErr
	}

	name := trim(body.Name)
	if name == "" {
		name = "key created " + time.Now().Format("2006-01-02")
	}
	if len(name) > maxKeyNameLen {
		return invalid(map[string]string{"name": "must be at most 80 characters"})
	}

	quota, err := s.store.GetQuota(r.Context(), caller.User.ID)
	if err != nil {
		return storeError(err, "load quota")
	}
	if quota.MaxAPIKeys > 0 {
		count, countErr := s.store.CountAPIKeys(r.Context(), caller.User.ID)
		if countErr != nil {
			return storeError(countErr, "count api keys")
		}
		if count >= quota.MaxAPIKeys {
			return errorf(http.StatusConflict, "key_limit",
				"you already have %d of %d allowed keys; revoke one first", count, quota.MaxAPIKeys)
		}
	}

	expiresAt := body.ExpiresAt
	if body.ExpiresInDays > 0 {
		expiresAt = ptr(time.Now().AddDate(0, 0, body.ExpiresInDays))
	}
	if expiresAt != nil && !expiresAt.After(time.Now()) {
		return invalid(map[string]string{"expires_at": "must be in the future"})
	}

	// An allow-list of models is checked on every proxied request, so it is
	// normalised here rather than at request time.
	allowed := make([]string, 0, len(body.AllowedModels))
	seen := map[string]bool{}
	for _, id := range body.AllowedModels {
		id = strings.ToLower(trim(id))
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		allowed = append(allowed, id)
	}
	sort.Strings(allowed)

	key := &model.APIKey{
		UserID:        caller.User.ID,
		Name:          name,
		Status:        model.StatusActive,
		AllowedModels: allowed,
		ExpiresAt:     expiresAt,
	}
	secret, err := s.store.CreateAPIKey(r.Context(), key)
	if err != nil {
		return storeError(err, "create api key")
	}
	key.Secret = secret

	writeJSON(w, http.StatusCreated, map[string]any{
		"key":  key,
		"note": "copy this key now; it is not stored and cannot be shown again",
	})
	return nil
}

// revokeAPIKey disables a key without losing its usage history.
func (s *Server) revokeAPIKey(w http.ResponseWriter, r *http.Request) *apiError {
	key, apiErr := s.loadAPIKey(r)
	if apiErr != nil {
		return apiErr
	}
	if err := s.store.RevokeAPIKey(r.Context(), key.ID); err != nil {
		return storeError(err, "revoke api key")
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": model.StatusRevoked})
	return nil
}

// deleteAPIKey removes a key outright.
func (s *Server) deleteAPIKey(w http.ResponseWriter, r *http.Request) *apiError {
	key, apiErr := s.loadAPIKey(r)
	if apiErr != nil {
		return apiErr
	}
	if err := s.store.DeleteAPIKey(r.Context(), key.ID); err != nil {
		return storeError(err, "delete api key")
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	return nil
}

// loadAPIKey resolves {id} and checks that the caller owns it.
func (s *Server) loadAPIKey(r *http.Request) (*model.APIKey, *apiError) {
	caller := callerFrom(r.Context())
	id, apiErr := pathUUID(r, "id")
	if apiErr != nil {
		return nil, apiErr
	}

	key, err := s.store.GetAPIKey(r.Context(), id)
	if err != nil {
		return nil, storeError(err, "load api key")
	}
	if key.UserID != caller.User.ID && !caller.User.IsAdmin() {
		return nil, errorf(http.StatusNotFound, "not_found", "no such api key")
	}
	return key, nil
}
