package httpapi

// openai_endpoints.go — group, create, scan and add-key handlers for the
// OpenAI-compatible provider.
//
// Each "endpoint" is a logical profile identified by its base URL: a label,
// the base URL the upstream lives at, an optional curated model list, and
// any number of API keys. Profiles are stored as ordinary connection rows
// (provider=openai, metadata.base_url set); multiple keys against the same
// base URL are separate connection rows grouped together by base_url.
//
// The four handlers below are the REST surface the new /providers/:id page
// in the web UI drives. They are deliberately thin: the heavy lifting
// (key storage, model catalog, rate-limit headers) is shared with the
// existing /api/connections machinery.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"aihub/internal/model"
	"aihub/internal/store"
)

// OpenAIEndpoint is the wire shape returned by GET /api/openai/endpoints.
// It groups every connection row sharing the same base_url under one
// logical profile, which is how the UI renders each OpenAI-compatible
// endpoint as its own card on /providers.
type OpenAIEndpoint struct {
	// BaseURL is the upstream base (without trailing slash). Two profiles
	// with the same normalised base URL are the same profile.
	BaseURL string `json:"base_url"`
	// Label is the operator-supplied name shown in the UI. It is taken
	// from the most recently created connection against this base URL so
	// that renaming a profile is just editing that connection's label.
	Label string `json:"label"`
	// Models is the operator-curated list of model ids the profile
	// advertises. When empty the UI offers to scan the upstream's
	// /v1/models endpoint instead.
	Models []string `json:"models,omitempty"`
	// Keys is the per-API-key state (one row per connection sharing
	// this base URL). Each entry surfaces enough to render a row in the
	// detail page without a follow-up fetch.
	Keys []OpenAIEndpointKey `json:"keys"`
	// CreatedAt is the earliest connection's creation time, so a freshly
	// added profile still sorts before one that has existed for a while.
	CreatedAt time.Time `json:"created_at"`
	// UsableCount is how many keys are currently routable.
	UsableCount int `json:"usable_count"`
	// KeyCount is len(Keys), surfaced separately so the UI can show
	// "no keys yet" without parsing the array.
	KeyCount int `json:"key_count"`
}

// OpenAIEndpointKey is one API key inside an endpoint group.
type OpenAIEndpointKey struct {
	ID               string     `json:"id"`
	Label            string     `json:"label"`
	AccountEmail     string     `json:"account_email"`
	Plan             string     `json:"plan,omitempty"`
	Status           string     `json:"status"`
	Scope            string     `json:"scope"`
	Weight           int        `json:"weight"`
	DisabledUntil    *time.Time `json:"disabled_until,omitempty"`
	LastError        string     `json:"last_error,omitempty"`
	LastUsedAt       *time.Time `json:"last_used_at,omitempty"`
	RequestCount24h  int64      `json:"request_count_24h,omitempty"`
	HasAPIKey        bool       `json:"has_api_key"`
	QuotaNote        string     `json:"quota_note,omitempty"`
	ExtraHeadersKeys []string   `json:"extra_headers_keys,omitempty"`
}

// listOpenAIEndpoints groups every OpenAI-compatible connection the caller
// owns by base_url and returns one profile per group. Shared connections
// are included when the caller's quota allows the shared pool, matching
// the behaviour of /api/connections.
func (s *Server) listOpenAIEndpoints(w http.ResponseWriter, r *http.Request) *apiError {
	caller := callerFrom(r.Context())

	allowShared, apiErr := s.allowShared(r, caller)
	if apiErr != nil {
		return apiErr
	}

	filter := store.ConnectionFilter{
		OwnerID:       caller.User.ID,
		IncludeShared: allowShared,
		Provider:      model.ProviderOpenAI,
	}
	if queryBool(r, "all") {
		if !caller.User.IsAdmin() {
			return errorf(http.StatusForbidden, "forbidden", "only an admin can list every endpoint")
		}
		filter.OwnerID = uuid.Nil
	}

	conns, err := s.store.ListConnections(r.Context(), filter)
	if err != nil {
		return storeError(err, "list openai connections")
	}

	groups := groupOpenAIConnections(conns)
	writeJSON(w, http.StatusOK, map[string]any{"endpoints": jsonArray(groups)})
	return nil
}

// groupOpenAIConnections collapses a flat connection list into one
// OpenAIEndpoint per distinct base_url. The function is exported for tests
// in a later commit; today it is only used by listOpenAIEndpoints.
func groupOpenAIConnections(conns []*model.Connection) []OpenAIEndpoint {
	now := time.Now()
	byBase := map[string]*OpenAIEndpoint{}
	order := []string{}

	for _, conn := range conns {
		base := normaliseOpenAIBaseURL(conn)
		if base == "" {
			// A connection with no base_url falls back to the canonical
			// endpoint so the row is still visible in the UI rather than
			// disappearing into an "unknown" group.
			base = "https://api.openai.com/v1"
		}
		group, ok := byBase[base]
		if !ok {
			group = &OpenAIEndpoint{BaseURL: base, CreatedAt: conn.CreatedAt}
			byBase[base] = group
			order = append(order, base)
		}
		// Adopt the most recent non-empty label so renaming a single key
		// renames the whole profile in the UI without an extra round trip.
		if conn.Label != "" {
			group.Label = conn.Label
		}
		if conn.CreatedAt.Before(group.CreatedAt) {
			group.CreatedAt = conn.CreatedAt
		}
		if models := openAIModelsFromConn(conn); len(models) > 0 && len(group.Models) == 0 {
			// Keep the first non-empty curated list rather than union them;
			// merging would silently lose the operator's choice if two
			// keys disagreed on the model set.
			group.Models = models
		}

		key := OpenAIEndpointKey{
			ID:               conn.ID.String(),
			Label:            conn.Label,
			AccountEmail:     conn.AccountEmail,
			Plan:             conn.Plan,
			Status:           conn.Status,
			Scope:            conn.Scope,
			Weight:           conn.Weight,
			DisabledUntil:    conn.DisabledUntil,
			LastError:        conn.LastError,
			LastUsedAt:       conn.LastUsedAt,
			RequestCount24h:  conn.RequestCount24h,
			HasAPIKey:        conn.Credential != nil && conn.Credential.AccessToken != "",
			ExtraHeadersKeys: openAIExtraHeaderKeys(conn),
		}
		if conn.Quota != nil {
			key.QuotaNote = conn.Quota.Note
		}
		group.Keys = append(group.Keys, key)
		group.KeyCount++
		if conn.Usable(now) {
			group.UsableCount++
		}
	}

	out := make([]OpenAIEndpoint, 0, len(order))
	for _, base := range order {
		out = append(out, *byBase[base])
	}
	return out
}

// normaliseOpenAIBaseURL pulls the operator-configured base_url out of a
// connection's metadata, lower-cases the host (URLs that differ only by
// case are the same upstream) and strips the trailing slash so
// "https://api.openai.com/v1/" and "https://api.openai.com/v1" collide.
func normaliseOpenAIBaseURL(conn *model.Connection) string {
	if conn == nil {
		return ""
	}
	raw, _ := conn.Metadata["base_url"].(string)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return strings.TrimRight(strings.TrimSpace(raw), "/")
	}
	parsed.Host = strings.ToLower(parsed.Host)
	return strings.TrimRight(parsed.String(), "/")
}

// openAIModelsFromConn returns the curated model list stored in a
// connection's metadata. The slice is empty when the operator has not
// curated one, which is the signal the UI uses to offer a scan.
func openAIModelsFromConn(conn *model.Connection) []string {
	if conn == nil {
		return nil
	}
	raw, _ := conn.Metadata["models"].([]any)
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if id, ok := item.(string); ok && strings.TrimSpace(id) != "" {
			out = append(out, strings.TrimSpace(id))
		}
	}
	return out
}

// openAIExtraHeaderKeys surfaces the keys of operator-configured extra
// headers so the UI can list them without leaking their values.
func openAIExtraHeaderKeys(conn *model.Connection) []string {
	if conn == nil {
		return nil
	}
	raw, _ := conn.Metadata["extra_headers"].(map[string]any)
	out := make([]string, 0, len(raw))
	for k := range raw {
		out = append(out, k)
	}
	return out
}

// createOpenAIEndpoint creates a new OpenAI-compatible profile (a single
// connection row carrying the base URL and label). The first API key is
// optional here: an operator who just wants to bookmark an endpoint and
// add keys later can do so without filling in a key they may not have yet.
//
// Body:
//
//	{
//	  "label": "My OpenAI",
//	  "base_url": "https://api.openai.com/v1",
//	  "api_key": "sk-...",          // optional
//	  "models": ["gpt-4o"],         // optional curated list
//	  "extra_headers": {...},       // optional vendor headers
//	  "quota_note": "...",          // optional
//	  "scope": "private"|"shared",  // optional, default private
//	  "weight": 1                    // optional, 1-100
//	}
func (s *Server) createOpenAIEndpoint(w http.ResponseWriter, r *http.Request) *apiError {
	caller := callerFrom(r.Context())

	var body struct {
		Label        string            `json:"label"`
		BaseURL      string            `json:"base_url"`
		APIKey       string            `json:"api_key"`
		Models       []string          `json:"models"`
		ExtraHeaders map[string]string `json:"extra_headers"`
		QuotaNote    string            `json:"quota_note"`
		Scope        string            `json:"scope"`
		Weight       *int              `json:"weight"`
	}
	if apiErr := decodeJSON(r, &body); apiErr != nil {
		return apiErr
	}

	label := strings.TrimSpace(body.Label)
	if label == "" {
		return invalid(map[string]string{"label": "must not be empty"})
	}
	baseURL := strings.TrimRight(strings.TrimSpace(body.BaseURL), "/")
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	if _, err := url.Parse(baseURL); err != nil {
		return invalid(map[string]string{"base_url": "must be a valid URL: " + err.Error()})
	}

	quota, err := s.store.GetQuota(r.Context(), caller.User.ID)
	if err != nil {
		return storeError(err, "load quota")
	}
	if !providerAllowed(quota, model.ProviderOpenAI) {
		return errorf(http.StatusForbidden, "provider_not_allowed",
			"your account is not allowed to use %s", model.ProviderOpenAI)
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

	scope := strings.ToLower(strings.TrimSpace(body.Scope))
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

	weight := 1
	if body.Weight != nil {
		weight = *body.Weight
		if weight < 1 || weight > 100 {
			return invalid(map[string]string{"weight": "must be between 1 and 100"})
		}
	}

	metadata := map[string]any{"base_url": baseURL}
	if len(body.Models) > 0 {
		cleaned := make([]any, 0, len(body.Models))
		for _, m := range body.Models {
			if id := strings.TrimSpace(m); id != "" {
				cleaned = append(cleaned, id)
			}
		}
		if len(cleaned) > 0 {
			metadata["models"] = cleaned
		}
	}
	if len(body.ExtraHeaders) > 0 {
		raw, _ := json.Marshal(body.ExtraHeaders)
		var asAny map[string]any
		_ = json.Unmarshal(raw, &asAny)
		if len(asAny) > 0 {
			metadata["extra_headers"] = asAny
		}
	}
	if note := strings.TrimSpace(body.QuotaNote); note != "" {
		metadata["quota_note"] = note
	}

	// The credential is the API key. When the operator defers adding a key
	// we still need a row to hold the profile, so we persist an empty
	// credential and the UI shows the "Add API key" CTA prominently.
	cred := &model.Credential{AccessToken: strings.TrimSpace(body.APIKey)}
	conn := &model.Connection{
		OwnerID:    caller.User.ID,
		Provider:   model.ProviderOpenAI,
		Label:      label,
		Plan:       "",
		Status:     model.ConnStatusActive,
		Scope:      scope,
		Weight:     weight,
		Metadata:   metadata,
		Credential: cred,
	}
	if conn.Credential.AccessToken == "" {
		// An empty access token means the row is not routable yet. Mark it
		// as disabled so the candidates selector skips it; the operator
		// enables the row implicitly by adding a key via the detail page.
		conn.Status = model.ConnStatusDisabled
		conn.LastError = "no API key set; add one to enable this connection"
	}

	if err = s.store.CreateConnection(r.Context(), conn, conn.Credential); err != nil {
		return storeError(err, "create connection")
	}

	writeJSON(w, http.StatusCreated, map[string]any{"connection": conn})
	return nil
}

// addOpenAIKey attaches another API key to an existing endpoint profile.
// The profile is identified by its base URL (path parameter, URL-encoded);
// the new key becomes its own connection row sharing the same base_url.
//
// Body:
//
//	{
//	  "label": "OpenAI prod key",
//	  "api_key": "sk-...",
//	  "scope": "private"|"shared",  // optional
//	  "weight": 1                   // optional
//	}
//
// The endpoint's curated models and extra headers are inherited from the
// first connection sharing the base_url; the operator can edit them per
// key from the existing PATCH /api/connections/{id} endpoint if needed.
func (s *Server) addOpenAIKey(w http.ResponseWriter, r *http.Request) *apiError {
	caller := callerFrom(r.Context())

	baseURL, apiErr := decodeOpenAIBaseURLParam(r)
	if apiErr != nil {
		return apiErr
	}

	var body struct {
		Label  string `json:"label"`
		APIKey string `json:"api_key"`
		Scope  string `json:"scope"`
		Weight *int   `json:"weight"`
	}
	if apiErr = decodeJSON(r, &body); apiErr != nil {
		return apiErr
	}

	apiKey := strings.TrimSpace(body.APIKey)
	if apiKey == "" {
		return invalid(map[string]string{"api_key": "is required"})
	}
	label := strings.TrimSpace(body.Label)
	if label == "" {
		label = baseURL
	}

	// Inherit the curated models and extra headers from the first profile
	// that already exists against this base URL, so the new key behaves
	// consistently with the others. If no profile exists yet (the operator
	// hit POST /keys directly without createOpenAIEndpoint) we fall back
	// to a plain new connection with just the base_url.
	inherited := map[string]any{"base_url": baseURL}
	existing, err := s.store.ListConnections(r.Context(), store.ConnectionFilter{
		OwnerID:  caller.User.ID,
		Provider: model.ProviderOpenAI,
		BaseURL:  baseURL,
	})
	if err != nil {
		return storeError(err, "look up existing endpoint")
	}
	if len(existing) > 0 {
		// Adopt the curated models and extra headers but not the label;
		// each key has its own label so the operator can tell them apart
		// in the detail view.
		if models := openAIModelsFromConn(existing[0]); len(models) > 0 {
			cleaned := make([]any, 0, len(models))
			for _, m := range models {
				cleaned = append(cleaned, m)
			}
			inherited["models"] = cleaned
		}
		if raw, _ := existing[0].Metadata["extra_headers"].(map[string]any); len(raw) > 0 {
			inherited["extra_headers"] = raw
		}
		if note, _ := existing[0].Metadata["quota_note"].(string); note != "" {
			inherited["quota_note"] = note
		}
	}

	quota, qerr := s.store.GetQuota(r.Context(), caller.User.ID)
	if qerr != nil {
		return storeError(qerr, "load quota")
	}
	if !providerAllowed(quota, model.ProviderOpenAI) {
		return errorf(http.StatusForbidden, "provider_not_allowed",
			"your account is not allowed to use %s", model.ProviderOpenAI)
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

	scope := strings.ToLower(strings.TrimSpace(body.Scope))
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

	weight := 1
	if body.Weight != nil {
		weight = *body.Weight
		if weight < 1 || weight > 100 {
			return invalid(map[string]string{"weight": "must be between 1 and 100"})
		}
	}

	conn := &model.Connection{
		OwnerID:    caller.User.ID,
		Provider:   model.ProviderOpenAI,
		Label:      label,
		Status:     model.ConnStatusActive,
		Scope:      scope,
		Weight:     weight,
		Metadata:   inherited,
		Credential: &model.Credential{AccessToken: apiKey},
	}
	if err = s.store.CreateConnection(r.Context(), conn, conn.Credential); err != nil {
		return storeError(err, "create connection")
	}

	writeJSON(w, http.StatusCreated, map[string]any{"connection": conn})
	return nil
}

// scanOpenAIModels calls the upstream's GET /v1/models endpoint with one
// of the caller's existing API keys for the requested base URL and
// returns the model ids it advertises. The scan is read-only and
// short-lived; nothing is persisted.
//
// Query params:
//
//   - ?api_key=sk-...  Override the key to use (useful when the operator
//     has not stored the key yet and just wants to see what an endpoint
//     serves). Falls back to any stored key against the base URL when
//     omitted.
//   - ?refresh=1       Re-fetch even when a curated list is already set.
//
// The response shape is the same as OpenAI's /v1/models so the UI can
// render the result without any per-provider code.
func (s *Server) scanOpenAIModels(w http.ResponseWriter, r *http.Request) *apiError {
	caller := callerFrom(r.Context())

	baseURL, apiErr := decodeOpenAIBaseURLParam(r)
	if apiErr != nil {
		return apiErr
	}

	apiKey := strings.TrimSpace(r.URL.Query().Get("api_key"))
	if apiKey == "" {
		// Look up any stored key for this endpoint that the caller owns.
		conns, err := s.store.ListConnections(r.Context(), store.ConnectionFilter{
			OwnerID:  caller.User.ID,
			Provider: model.ProviderOpenAI,
			BaseURL:  baseURL,
		})
		if err != nil {
			return storeError(err, "look up existing endpoint")
		}
		for _, conn := range conns {
			if conn.Credential != nil && conn.Credential.AccessToken != "" {
				apiKey = conn.Credential.AccessToken
				break
			}
		}
	}
	if apiKey == "" {
		return errorf(http.StatusBadRequest, "no_api_key",
			"no API key stored for %s and none supplied in the api_key query param", baseURL)
	}

	models, err := fetchOpenAIModels(r.Context(), baseURL, apiKey)
	if err != nil {
		return errorf(http.StatusBadGateway, "upstream_unreachable",
			"could not fetch models from %s: %s", baseURL, err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"base_url": baseURL,
		"models":   models,
		"count":    len(models),
	})
	return nil
}

// fetchOpenAIModels calls GET <base>/models on an OpenAI-compatible
// endpoint and returns the model ids it advertises. The call has a tight
// timeout (10 s) because the operator is sitting in front of the UI
// waiting for the result.
//
// The function accepts both OpenAI's own /v1/models shape and the
// minimal "list of strings" shape some self-hosted gateways emit.
func fetchOpenAIModels(ctx context.Context, baseURL, apiKey string) ([]string, error) {
	url := strings.TrimRight(baseURL, "/") + "/models"
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call upstream: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("upstream returned %d: %s", resp.StatusCode, truncateHTTP(string(body), 400))
	}

	// Try OpenAI's own shape first: {"data": [{"id": "..."}, ...]}.
	var envelope struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err = json.Unmarshal(body, &envelope); err == nil && len(envelope.Data) > 0 {
		out := make([]string, 0, len(envelope.Data))
		for _, m := range envelope.Data {
			if id := strings.TrimSpace(m.ID); id != "" {
				out = append(out, id)
			}
		}
		return out, nil
	}

	// Fall back to a top-level array of strings (some self-hosted gateways
	// emit this) or an array of objects with an "id" field.
	var asArray []any
	if err = json.Unmarshal(body, &asArray); err == nil {
		out := make([]string, 0, len(asArray))
		for _, item := range asArray {
			switch typed := item.(type) {
			case string:
				if id := strings.TrimSpace(typed); id != "" {
					out = append(out, id)
				}
			case map[string]any:
				if id, _ := typed["id"].(string); strings.TrimSpace(id) != "" {
					out = append(out, strings.TrimSpace(id))
				}
			}
		}
		if len(out) > 0 {
			return out, nil
		}
	}

	// Final fallback: the body itself is a single string per line (rare,
	// but some console emulators emit it). Splitting on newlines gives us
	// at least a non-empty list rather than returning a parse error.
	if len(strings.Fields(string(body))) > 0 {
		return strings.Fields(string(body)), nil
	}
	return nil, fmt.Errorf("upstream response did not contain a model list")
}

// decodeOpenAIBaseURLParam reads the {base_url} path parameter back into
// its original form. Go's chi router already URL-decodes path segments,
// so the only remaining normalisation is stripping the trailing slash.
func decodeOpenAIBaseURLParam(r *http.Request) (string, *apiError) {
	raw := strings.Trim(r.PathValue("base_url"), "/")
	if raw == "" {
		return "", invalid(map[string]string{"base_url": "is required"})
	}
	// chi decodes for us; we just verify the result parses as a URL so
	// the upstream call site does not have to.
	if _, err := url.Parse(raw); err != nil {
		return "", invalid(map[string]string{"base_url": "must be a valid URL: " + err.Error()})
	}
	return raw, nil
}

// truncateHTTP bounds how much of an error body we propagate upstream,
// so a 5xx from a misconfigured gateway does not blow up the log line.
// Mirrors the truncate helper in internal/store/connections.go but
// scoped to this package to avoid an import cycle.
func truncateHTTP(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}
