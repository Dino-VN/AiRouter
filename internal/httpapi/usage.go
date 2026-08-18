package httpapi

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"aihub/internal/model"
	"aihub/internal/store"
)

// maxUsagePage bounds one page of the request log.
const maxUsagePage = 500

// getOwnQuota answers the "how much have I got left" page: the configured limits,
// what has been spent against them, and what each upstream account itself
// reports.
func (s *Server) getOwnQuota(w http.ResponseWriter, r *http.Request) *apiError {
	caller := callerFrom(r.Context())
	ctx := r.Context()

	target := caller.User.ID
	if raw := trim(r.URL.Query().Get("user_id")); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			return invalid(map[string]string{"user_id": "must be a uuid"})
		}
		if id != caller.User.ID && !caller.User.IsAdmin() {
			return errorf(http.StatusForbidden, "forbidden", "only an admin can read another account's quota")
		}
		target = id
	}

	quota, err := s.store.GetQuota(ctx, target)
	if err != nil {
		return storeError(err, "load quota")
	}
	now := time.Now().UTC()
	day, err := s.store.UsageWindow(ctx, target, startOfDay(now))
	if err != nil {
		return storeError(err, "aggregate usage")
	}
	month, err := s.store.UsageWindow(ctx, target, startOfMonth(now))
	if err != nil {
		return storeError(err, "aggregate usage")
	}
	connections, err := s.store.CountConnections(ctx, target)
	if err != nil {
		return storeError(err, "count connections")
	}
	keys, err := s.store.CountAPIKeys(ctx, target)
	if err != nil {
		return storeError(err, "count api keys")
	}

	// The upstream snapshots are what the provider says is left, which is a
	// different thing from the local quota and is the number users actually ask
	// about.
	conns, err := s.store.ListConnections(ctx, store.ConnectionFilter{OwnerID: target})
	if err != nil {
		return storeError(err, "list connections")
	}
	upstream := make([]map[string]any, 0, len(conns))
	for _, conn := range conns {
		upstream = append(upstream, map[string]any{
			"connection_id":    conn.ID,
			"provider":         conn.Provider,
			"label":            conn.Label,
			"account_email":    conn.AccountEmail,
			"plan":             conn.Plan,
			"status":           conn.Status,
			"usable":           conn.Usable(time.Now()),
			"quota":            conn.Quota,
			"quota_updated_at": conn.QuotaUpdatedAt,
			"requests_24h":     conn.RequestCount24h,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"quota": quota,
		"used": map[string]any{
			"day":         day,
			"month":       month,
			"connections": connections,
			"api_keys":    keys,
		},
		"remaining": map[string]any{
			"requests_today":   remaining(quota.RequestsPerDay, day.Requests),
			"tokens_today":     remaining(quota.TokensPerDay, day.TotalTokens),
			"requests_month":   remaining(quota.RequestsPerMonth, month.Requests),
			"tokens_month":     remaining(quota.TokensPerMonth, month.TotalTokens),
			"connection_slots": remaining(int64(quota.MaxConnections), int64(connections)),
			"api_key_slots":    remaining(int64(quota.MaxAPIKeys), int64(keys)),
		},
		"windows": map[string]any{
			"day_started_at":   startOfDay(now),
			"month_started_at": startOfMonth(now),
		},
		"upstream": upstream,
	})
	return nil
}

// usageSummary totals the standard windows in one request.
func (s *Server) usageSummary(w http.ResponseWriter, r *http.Request) *apiError {
	scope, apiErr := s.usageScope(r)
	if apiErr != nil {
		return apiErr
	}
	ctx := r.Context()
	now := time.Now().UTC()

	windows := []struct {
		name  string
		since time.Time
	}{
		{"today", startOfDay(now)},
		{"last_24h", now.Add(-24 * time.Hour)},
		{"last_7d", now.AddDate(0, 0, -7)},
		{"last_30d", now.AddDate(0, 0, -30)},
		{"month", startOfMonth(now)},
	}

	totals := map[string]model.UsageTotals{}
	for _, window := range windows {
		filter := scope
		filter.Since = window.since
		result, err := s.store.UsageTotals(ctx, filter)
		if err != nil {
			return storeError(err, "aggregate usage")
		}
		totals[window.name] = result
	}

	// The 30-day breakdowns make the dashboard cards useful without a second
	// round trip.
	monthFilter := scope
	monthFilter.Since = now.AddDate(0, 0, -30)
	byModel, err := s.store.UsageBreakdown(ctx, monthFilter, "model")
	if err != nil {
		return storeError(err, "break usage down by model")
	}
	byProvider, err := s.store.UsageBreakdown(ctx, monthFilter, "provider")
	if err != nil {
		return storeError(err, "break usage down by provider")
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"totals":      totals,
		"by_model":    head(byModel, 10),
		"by_provider": jsonArray(byProvider),
		"scope":       usageScopeLabel(scope),
	})
	return nil
}

// usageSeries returns a time series for the charts.
func (s *Server) usageSeries(w http.ResponseWriter, r *http.Request) *apiError {
	filter, apiErr := s.usageFilter(r)
	if apiErr != nil {
		return apiErr
	}

	bucket := strings.ToLower(trim(r.URL.Query().Get("bucket")))
	if bucket == "" {
		bucket = "day"
	}
	if bucket != "hour" && bucket != "day" {
		return invalid(map[string]string{"bucket": "must be hour or day"})
	}
	if filter.Since.IsZero() {
		if bucket == "hour" {
			filter.Since = time.Now().UTC().Add(-24 * time.Hour)
		} else {
			filter.Since = time.Now().UTC().AddDate(0, 0, -30)
		}
	}

	series, err := s.store.UsageSeries(r.Context(), filter, bucket)
	if err != nil {
		return storeError(err, "build the usage series")
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"bucket": bucket,
		"since":  filter.Since,
		"series": jsonArray(series),
	})
	return nil
}

// usageBreakdown groups usage by model, provider or user.
func (s *Server) usageBreakdown(w http.ResponseWriter, r *http.Request) *apiError {
	caller := callerFrom(r.Context())
	filter, apiErr := s.usageFilter(r)
	if apiErr != nil {
		return apiErr
	}

	dimension := strings.ToLower(trim(r.URL.Query().Get("dimension")))
	if dimension == "" {
		dimension = "model"
	}
	switch dimension {
	case "model", "provider":
	case "user":
		// Grouping by user is only meaningful, and only permitted, for an admin.
		if !caller.User.IsAdmin() {
			return errorf(http.StatusForbidden, "forbidden", "only an admin can group usage by user")
		}
	default:
		return invalid(map[string]string{"dimension": "must be model, provider or user"})
	}
	if filter.Since.IsZero() {
		filter.Since = time.Now().UTC().AddDate(0, 0, -30)
	}

	rows, err := s.store.UsageBreakdown(r.Context(), filter, dimension)
	if err != nil {
		return storeError(err, "break usage down")
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"dimension": dimension,
		"since":     filter.Since,
		"rows":      jsonArray(rows),
	})
	return nil
}

// usageRecords is the paginated request log.
func (s *Server) usageRecords(w http.ResponseWriter, r *http.Request) *apiError {
	filter, apiErr := s.usageFilter(r)
	if apiErr != nil {
		return apiErr
	}

	filter.Limit = queryInt(r, "limit", 50)
	if filter.Limit <= 0 || filter.Limit > maxUsagePage {
		filter.Limit = maxUsagePage
	}
	filter.Offset = queryInt(r, "offset", 0)
	if filter.Offset < 0 {
		filter.Offset = 0
	}

	records, err := s.store.ListUsage(r.Context(), filter)
	if err != nil {
		return storeError(err, "list usage")
	}
	totals, err := s.store.UsageTotals(r.Context(), filter)
	if err != nil {
		return storeError(err, "aggregate usage")
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"records": jsonArray(records),
		"totals":  totals,
		"limit":   filter.Limit,
		"offset":  filter.Offset,
	})
	return nil
}

// ---------------------------------------------------------------------------
// Query parsing
// ---------------------------------------------------------------------------

// usageScope resolves whose usage the caller is asking about. Users can only see
// their own; admins can ask for one user or for everyone.
func (s *Server) usageScope(r *http.Request) (store.UsageFilter, *apiError) {
	caller := callerFrom(r.Context())
	filter := store.UsageFilter{UserID: caller.User.ID}

	query := r.URL.Query()
	if raw := trim(query.Get("user_id")); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			return filter, invalid(map[string]string{"user_id": "must be a uuid"})
		}
		if id != caller.User.ID && !caller.User.IsAdmin() {
			return filter, errorf(http.StatusForbidden, "forbidden", "only an admin can read another account's usage")
		}
		filter.UserID = id
	}
	if queryBool(r, "all") {
		if !caller.User.IsAdmin() {
			return filter, errorf(http.StatusForbidden, "forbidden", "only an admin can read deployment-wide usage")
		}
		filter.UserID = uuid.Nil
	}
	return filter, nil
}

// usageFilter is usageScope plus the time window and the provider/model filters.
func (s *Server) usageFilter(r *http.Request) (store.UsageFilter, *apiError) {
	filter, apiErr := s.usageScope(r)
	if apiErr != nil {
		return filter, apiErr
	}
	query := r.URL.Query()

	if raw := trim(query.Get("range")); raw != "" {
		window, err := parseRange(raw)
		if err != nil || window <= 0 {
			return filter, invalid(map[string]string{"range": "use a duration such as 24h, 7d or 720h"})
		}
		filter.Since = time.Now().UTC().Add(-window)
	}
	if raw := trim(query.Get("since")); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return filter, invalid(map[string]string{"since": "must be an RFC 3339 timestamp"})
		}
		filter.Since = parsed
	}
	if raw := trim(query.Get("until")); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return filter, invalid(map[string]string{"until": "must be an RFC 3339 timestamp"})
		}
		filter.Until = parsed
	}
	if raw := trim(query.Get("provider")); raw != "" {
		id := model.Provider(strings.ToLower(raw))
		if !id.Valid() {
			return filter, invalid(map[string]string{"provider": "unknown provider " + raw})
		}
		filter.Provider = string(id)
	}
	if raw := trim(query.Get("model")); raw != "" {
		filter.Model = raw
	}
	return filter, nil
}

// parseRange lets "7d" mean what everybody expects, which time.ParseDuration
// alone does not accept.
func parseRange(raw string) (time.Duration, error) {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if days, ok := strings.CutSuffix(raw, "d"); ok {
		count, err := strconv.Atoi(days)
		if err != nil {
			return 0, err
		}
		return time.Duration(count) * 24 * time.Hour, nil
	}
	return time.ParseDuration(raw)
}

func usageScopeLabel(filter store.UsageFilter) string {
	if filter.UserID == uuid.Nil {
		return "deployment"
	}
	return "user"
}

// remaining reports how much of a limit is left. A zero limit means unlimited,
// which is reported as a nil rather than a misleading number.
func remaining(limit, used int64) *int64 {
	if limit <= 0 {
		return nil
	}
	left := limit - used
	if left < 0 {
		left = 0
	}
	return &left
}

// startOfDay and startOfMonth define the quota windows. They are UTC so a
// deployment's limits do not shift with the server's local timezone.
func startOfDay(now time.Time) time.Time {
	utc := now.UTC()
	return time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
}

func startOfMonth(now time.Time) time.Time {
	utc := now.UTC()
	return time.Date(utc.Year(), utc.Month(), 1, 0, 0, 0, 0, time.UTC)
}
