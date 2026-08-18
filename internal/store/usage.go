package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"aihub/internal/model"
)

// RecordUsage appends one proxied request to the ledger.
func (s *Store) RecordUsage(ctx context.Context, rec *model.UsageRecord) error {
	err := s.pool.QueryRow(ctx, `
		INSERT INTO usage_records (user_id, api_key_id, connection_id, provider, model, client_format,
		                           status_code, stream, prompt_tokens, completion_tokens,
		                           reasoning_tokens, cached_tokens, total_tokens, latency_ms, error)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		RETURNING id, created_at`,
		rec.UserID, rec.APIKeyID, rec.ConnectionID, rec.Provider, rec.Model, rec.ClientFormat,
		rec.StatusCode, rec.Stream, rec.PromptTokens, rec.CompletionTokens, rec.ReasoningTokens,
		rec.CachedTokens, rec.TotalTokens, rec.LatencyMS, truncate(rec.Error, 2000),
	).Scan(&rec.ID, &rec.CreatedAt)
	return mapErr(err)
}

// UsageWindow counts requests and tokens for a user since a point in time. It
// backs quota enforcement, so it reads committed rows only.
func (s *Store) UsageWindow(ctx context.Context, userID uuid.UUID, since time.Time) (model.UsageTotals, error) {
	var totals model.UsageTotals
	err := s.pool.QueryRow(ctx, `
		SELECT count(*),
		       COALESCE(sum(CASE WHEN status_code >= 400 THEN 1 ELSE 0 END), 0),
		       COALESCE(sum(prompt_tokens), 0),
		       COALESCE(sum(completion_tokens), 0),
		       COALESCE(sum(total_tokens), 0)
		FROM usage_records
		WHERE user_id = $1 AND created_at >= $2`, userID, since).Scan(
		&totals.Requests, &totals.Errors, &totals.PromptTokens, &totals.CompletionTokens, &totals.TotalTokens)
	return totals, mapErr(err)
}

// UsageFilter narrows a usage listing or aggregation.
type UsageFilter struct {
	UserID   uuid.UUID
	Since    time.Time
	Until    time.Time
	Provider string
	Model    string
	Limit    int
	Offset   int
}

func (f UsageFilter) where(startArg int) (string, []any, int) {
	clauses := []string{"1 = 1"}
	args := []any{}
	next := startArg

	if f.UserID != uuid.Nil {
		clauses = append(clauses, fmt.Sprintf("user_id = $%d", next))
		args = append(args, f.UserID)
		next++
	}
	if !f.Since.IsZero() {
		clauses = append(clauses, fmt.Sprintf("created_at >= $%d", next))
		args = append(args, f.Since)
		next++
	}
	if !f.Until.IsZero() {
		clauses = append(clauses, fmt.Sprintf("created_at < $%d", next))
		args = append(args, f.Until)
		next++
	}
	if f.Provider != "" {
		clauses = append(clauses, fmt.Sprintf("provider = $%d", next))
		args = append(args, f.Provider)
		next++
	}
	if f.Model != "" {
		clauses = append(clauses, fmt.Sprintf("model = $%d", next))
		args = append(args, f.Model)
		next++
	}
	return strings.Join(clauses, " AND "), args, next
}

// ListUsage returns raw usage rows, newest first.
func (s *Store) ListUsage(ctx context.Context, filter UsageFilter) ([]*model.UsageRecord, error) {
	where, args, next := filter.where(1)
	limit := filter.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := fmt.Sprintf(`
		SELECT id, created_at, user_id, api_key_id, connection_id, provider, model, client_format,
		       status_code, stream, prompt_tokens, completion_tokens, reasoning_tokens,
		       cached_tokens, total_tokens, latency_ms, error
		FROM usage_records
		WHERE %s
		ORDER BY created_at DESC
		LIMIT $%d OFFSET $%d`, where, next, next+1)
	args = append(args, limit, max(filter.Offset, 0))

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var out []*model.UsageRecord
	for rows.Next() {
		var (
			rec          model.UsageRecord
			apiKeyID     *uuid.UUID
			connectionID *uuid.UUID
		)
		if err = rows.Scan(&rec.ID, &rec.CreatedAt, &rec.UserID, &apiKeyID, &connectionID,
			&rec.Provider, &rec.Model, &rec.ClientFormat, &rec.StatusCode, &rec.Stream,
			&rec.PromptTokens, &rec.CompletionTokens, &rec.ReasoningTokens, &rec.CachedTokens,
			&rec.TotalTokens, &rec.LatencyMS, &rec.Error); err != nil {
			return nil, mapErr(err)
		}
		rec.APIKeyID = apiKeyID
		rec.ConnectionID = connectionID
		out = append(out, &rec)
	}
	return out, mapErr(rows.Err())
}

// UsageTotals aggregates over a filter.
func (s *Store) UsageTotals(ctx context.Context, filter UsageFilter) (model.UsageTotals, error) {
	where, args, _ := filter.where(1)
	var totals model.UsageTotals
	err := s.pool.QueryRow(ctx, `
		SELECT count(*),
		       COALESCE(sum(CASE WHEN status_code >= 400 THEN 1 ELSE 0 END), 0),
		       COALESCE(sum(prompt_tokens), 0),
		       COALESCE(sum(completion_tokens), 0),
		       COALESCE(sum(total_tokens), 0)
		FROM usage_records WHERE `+where, args...).Scan(
		&totals.Requests, &totals.Errors, &totals.PromptTokens, &totals.CompletionTokens, &totals.TotalTokens)
	return totals, mapErr(err)
}

// UsageSeries buckets usage by hour or day for charting.
func (s *Store) UsageSeries(ctx context.Context, filter UsageFilter, bucket string) ([]model.UsageBucket, error) {
	unit := "hour"
	if bucket == "day" {
		unit = "day"
	}
	where, args, _ := filter.where(1)
	query := fmt.Sprintf(`
		SELECT date_trunc('%s', created_at) AS bucket,
		       count(*),
		       COALESCE(sum(CASE WHEN status_code >= 400 THEN 1 ELSE 0 END), 0),
		       COALESCE(sum(prompt_tokens), 0),
		       COALESCE(sum(completion_tokens), 0),
		       COALESCE(sum(total_tokens), 0)
		FROM usage_records
		WHERE %s
		GROUP BY 1
		ORDER BY 1`, unit, where)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var out []model.UsageBucket
	for rows.Next() {
		var b model.UsageBucket
		if err = rows.Scan(&b.Bucket, &b.Requests, &b.Errors, &b.PromptTokens,
			&b.CompletionTokens, &b.TotalTokens); err != nil {
			return nil, mapErr(err)
		}
		out = append(out, b)
	}
	return out, mapErr(rows.Err())
}

// UsageBreakdown groups usage by "model", "provider" or "user".
func (s *Store) UsageBreakdown(ctx context.Context, filter UsageFilter, dimension string) ([]model.UsageBreakdown, error) {
	column := "model"
	switch dimension {
	case "provider":
		column = "provider"
	case "user":
		column = "user_id::text"
	case "model":
		column = "model"
	}
	where, args, _ := filter.where(1)
	query := fmt.Sprintf(`
		SELECT %s AS key,
		       count(*),
		       COALESCE(sum(CASE WHEN status_code >= 400 THEN 1 ELSE 0 END), 0),
		       COALESCE(sum(prompt_tokens), 0),
		       COALESCE(sum(completion_tokens), 0),
		       COALESCE(sum(total_tokens), 0)
		FROM usage_records
		WHERE %s
		GROUP BY 1
		ORDER BY 2 DESC
		LIMIT 50`, column, where)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var out []model.UsageBreakdown
	for rows.Next() {
		var b model.UsageBreakdown
		if err = rows.Scan(&b.Key, &b.Requests, &b.Errors, &b.PromptTokens,
			&b.CompletionTokens, &b.TotalTokens); err != nil {
			return nil, mapErr(err)
		}
		out = append(out, b)
	}
	return out, mapErr(rows.Err())
}

// PruneUsage deletes records older than the retention window.
func (s *Store) PruneUsage(ctx context.Context, olderThan time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM usage_records WHERE created_at < $1`, olderThan)
	if err != nil {
		return 0, mapErr(err)
	}
	return tag.RowsAffected(), nil
}
