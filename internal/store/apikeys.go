package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"aihub/internal/model"
)

// APIKeyPrefix is the human-recognisable prefix of every proxy key.
const APIKeyPrefix = "ah-"

const apiKeyColumns = `k.id, k.user_id, k.name, k.prefix, k.status, k.allowed_models,
	k.expires_at, k.last_used_at, k.created_at`

// GenerateAPIKey returns a fresh random proxy key.
func GenerateAPIKey() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate api key: %w", err)
	}
	return APIKeyPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// HashAPIKey derives the stored lookup hash for a plaintext key.
func HashAPIKey(secret string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(secret)))
	return hex.EncodeToString(sum[:])
}

// CreateAPIKey mints a key for a user and returns it with the plaintext secret
// populated exactly once.
func (s *Store) CreateAPIKey(ctx context.Context, key *model.APIKey) (string, error) {
	secret, err := GenerateAPIKey()
	if err != nil {
		return "", err
	}
	if key.ID == uuid.Nil {
		key.ID = uuid.New()
	}
	if key.Status == "" {
		key.Status = model.StatusActive
	}
	key.Prefix = secret[:min(len(secret), 11)]

	row := s.pool.QueryRow(ctx, `
		INSERT INTO api_keys (id, user_id, name, prefix, key_hash, status, allowed_models, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING created_at`,
		key.ID, key.UserID, key.Name, key.Prefix, HashAPIKey(secret), key.Status,
		providerList(key.AllowedModels), key.ExpiresAt)
	if err = row.Scan(&key.CreatedAt); err != nil {
		return "", mapErr(err)
	}
	key.Secret = secret
	return secret, nil
}

// ListAPIKeys returns a user's keys (or every key when userID is zero).
func (s *Store) ListAPIKeys(ctx context.Context, userID uuid.UUID) ([]*model.APIKey, error) {
	query := `
		SELECT ` + apiKeyColumns + `,
		       COALESCE((SELECT count(*) FROM usage_records ur WHERE ur.api_key_id = k.id), 0)
		FROM api_keys k`
	args := []any{}
	if userID != uuid.Nil {
		query += ` WHERE k.user_id = $1`
		args = append(args, userID)
	}
	query += ` ORDER BY k.created_at DESC`

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var out []*model.APIKey
	for rows.Next() {
		key, scanErr := scanAPIKey(rows, true)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, key)
	}
	return out, mapErr(rows.Err())
}

// GetAPIKey loads one key by id.
func (s *Store) GetAPIKey(ctx context.Context, id uuid.UUID) (*model.APIKey, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+apiKeyColumns+` FROM api_keys k WHERE k.id = $1`, id)
	return scanAPIKey(row, false)
}

// AuthenticateAPIKey resolves a plaintext key to its key record and owner.
// Expired, revoked or suspended principals are rejected.
func (s *Store) AuthenticateAPIKey(ctx context.Context, secret string) (*model.APIKey, *model.User, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT `+apiKeyColumns+`,
		       u.id, u.username, u.password_hash, u.display_name, u.role, u.status,
		       u.created_at, u.updated_at, u.last_login_at
		FROM api_keys k JOIN users u ON u.id = k.user_id
		WHERE k.key_hash = $1`, HashAPIKey(secret))

	var (
		key           model.APIKey
		expiresAt     *time.Time
		lastUsedAt    *time.Time
		user          model.User
		role          string
		userLastLogin *time.Time
	)
	err := row.Scan(&key.ID, &key.UserID, &key.Name, &key.Prefix, &key.Status, &key.AllowedModels,
		&expiresAt, &lastUsedAt, &key.CreatedAt,
		&user.ID, &user.Username, &user.PasswordHash, &user.DisplayName, &role, &user.Status,
		&user.CreatedAt, &user.UpdatedAt, &userLastLogin)
	if err != nil {
		return nil, nil, mapErr(err)
	}
	key.ExpiresAt = expiresAt
	key.LastUsedAt = lastUsedAt
	user.Role = model.Role(role)
	user.LastLoginAt = userLastLogin

	if key.Status != model.StatusActive {
		return nil, nil, fmt.Errorf("%w: api key is %s", ErrUnauthorized, key.Status)
	}
	if key.ExpiresAt != nil && time.Now().After(*key.ExpiresAt) {
		return nil, nil, fmt.Errorf("%w: api key expired", ErrUnauthorized)
	}
	if user.Status != model.StatusActive {
		return nil, nil, fmt.Errorf("%w: account is %s", ErrUnauthorized, user.Status)
	}
	return &key, &user, nil
}

// TouchAPIKey records last usage. Errors are non-fatal for the caller.
func (s *Store) TouchAPIKey(ctx context.Context, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `UPDATE api_keys SET last_used_at = now() WHERE id = $1`, id)
	return mapErr(err)
}

// CountAPIKeys returns how many active keys a user holds.
func (s *Store) CountAPIKeys(ctx context.Context, userID uuid.UUID) (int, error) {
	var count int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM api_keys WHERE user_id = $1 AND status = 'active'`, userID).Scan(&count)
	return count, mapErr(err)
}

// RevokeAPIKey marks a key unusable without deleting its usage history.
func (s *Store) RevokeAPIKey(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE api_keys SET status = 'revoked' WHERE id = $1 AND status <> 'revoked'`, id)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteAPIKey removes a key entirely.
func (s *Store) DeleteAPIKey(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM api_keys WHERE id = $1`, id)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func scanAPIKey(row rowScanner, withCount bool) (*model.APIKey, error) {
	var (
		key        model.APIKey
		expiresAt  *time.Time
		lastUsedAt *time.Time
		count      int64
	)
	dest := []any{&key.ID, &key.UserID, &key.Name, &key.Prefix, &key.Status, &key.AllowedModels,
		&expiresAt, &lastUsedAt, &key.CreatedAt}
	if withCount {
		dest = append(dest, &count)
	}
	if err := row.Scan(dest...); err != nil {
		return nil, mapErr(err)
	}
	key.ExpiresAt = expiresAt
	key.LastUsedAt = lastUsedAt
	key.RequestCount = count
	return &key, nil
}
