package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// GetSetting reads a JSON setting into out. It returns ErrNotFound when the key
// has never been written.
func (s *Store) GetSetting(ctx context.Context, key string, out any) (time.Time, error) {
	var (
		raw       []byte
		updatedAt time.Time
	)
	err := s.pool.QueryRow(ctx,
		`SELECT value, updated_at FROM settings WHERE key = $1`, key).Scan(&raw, &updatedAt)
	if err != nil {
		return time.Time{}, mapErr(err)
	}
	if out != nil {
		if err = json.Unmarshal(raw, out); err != nil {
			return updatedAt, fmt.Errorf("decode setting %q: %w", key, err)
		}
	}
	return updatedAt, nil
}

// PutSetting writes a JSON setting.
func (s *Store) PutSetting(ctx context.Context, key string, value any) error {
	// jsonbText, not json.Marshal: the column is jsonb and []byte would arrive
	// as a bytea literal. See its comment in connections.go.
	raw, err := jsonbText(value)
	if err != nil {
		return fmt.Errorf("encode setting %q: %w", key, err)
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO settings (key, value, updated_at) VALUES ($1, $2, now())
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, key, raw)
	return mapErr(err)
}

// DeleteSetting removes a setting.
func (s *Store) DeleteSetting(ctx context.Context, key string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM settings WHERE key = $1`, key)
	return mapErr(err)
}

// ListSettings returns every setting as raw JSON, for the admin settings page.
func (s *Store) ListSettings(ctx context.Context) (map[string]json.RawMessage, error) {
	rows, err := s.pool.Query(ctx, `SELECT key, value FROM settings ORDER BY key`)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	out := map[string]json.RawMessage{}
	for rows.Next() {
		var (
			key string
			raw []byte
		)
		if err = rows.Scan(&key, &raw); err != nil {
			return nil, mapErr(err)
		}
		out[key] = json.RawMessage(raw)
	}
	return out, mapErr(rows.Err())
}
