package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/google/uuid"

	"aihub/internal/model"
)

// GenerateRefreshToken returns an opaque refresh token for the web UI.
func GenerateRefreshToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate refresh token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// hashToken derives the stored form of an opaque token.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// CreateWebSession issues a refresh session and returns the plaintext token.
func (s *Store) CreateWebSession(ctx context.Context, sess *model.WebSession) (string, error) {
	token, err := GenerateRefreshToken()
	if err != nil {
		return "", err
	}
	if sess.ID == uuid.Nil {
		sess.ID = uuid.New()
	}
	row := s.pool.QueryRow(ctx, `
		INSERT INTO web_sessions (id, user_id, token_hash, user_agent, ip, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING created_at`,
		sess.ID, sess.UserID, hashToken(token), truncate(sess.UserAgent, 400), sess.IP, sess.ExpiresAt)
	if err = row.Scan(&sess.CreatedAt); err != nil {
		return "", mapErr(err)
	}
	return token, nil
}

// LookupWebSession resolves a refresh token to its session and user. Revoked or
// expired sessions are reported as ErrNotFound so callers cannot distinguish
// them from a forged token.
func (s *Store) LookupWebSession(ctx context.Context, token string) (*model.WebSession, *model.User, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT s.id, s.user_id, s.user_agent, s.ip, s.created_at, s.expires_at, s.revoked_at,
		       u.id, u.username, u.password_hash, u.display_name, u.role, u.status,
		       u.created_at, u.updated_at, u.last_login_at
		FROM web_sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1`, hashToken(token))

	var (
		sess        model.WebSession
		revokedAt   *time.Time
		user        model.User
		role        string
		lastLoginAt *time.Time
	)
	err := row.Scan(&sess.ID, &sess.UserID, &sess.UserAgent, &sess.IP, &sess.CreatedAt,
		&sess.ExpiresAt, &revokedAt,
		&user.ID, &user.Username, &user.PasswordHash, &user.DisplayName, &role, &user.Status,
		&user.CreatedAt, &user.UpdatedAt, &lastLoginAt)
	if err != nil {
		return nil, nil, mapErr(err)
	}
	sess.RevokedAt = revokedAt
	user.Role = model.Role(role)
	user.LastLoginAt = lastLoginAt

	if revokedAt != nil || time.Now().After(sess.ExpiresAt) {
		return nil, nil, ErrNotFound
	}
	if user.Status != model.StatusActive {
		return nil, nil, fmt.Errorf("account is %s", user.Status)
	}
	return &sess, &user, nil
}

// RotateWebSession replaces a session's token, returning the new plaintext. This
// is called on every refresh so a stolen refresh token is single-use.
func (s *Store) RotateWebSession(ctx context.Context, id uuid.UUID, expiresAt time.Time) (string, error) {
	token, err := GenerateRefreshToken()
	if err != nil {
		return "", err
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE web_sessions SET token_hash = $2, expires_at = $3
		WHERE id = $1 AND revoked_at IS NULL`, id, hashToken(token), expiresAt)
	if err != nil {
		return "", mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return "", ErrNotFound
	}
	return token, nil
}

// RevokeWebSession logs one session out.
func (s *Store) RevokeWebSession(ctx context.Context, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE web_sessions SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, id)
	return mapErr(err)
}

// RevokeWebSessionByToken logs out the holder of a refresh token.
func (s *Store) RevokeWebSessionByToken(ctx context.Context, token string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE web_sessions SET revoked_at = now() WHERE token_hash = $1 AND revoked_at IS NULL`,
		hashToken(token))
	return mapErr(err)
}

// RevokeUserWebSessions logs a user out everywhere, used when their password or
// status changes.
func (s *Store) RevokeUserWebSessions(ctx context.Context, userID uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE web_sessions SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, userID)
	return mapErr(err)
}

// ListWebSessions returns a user's active sessions.
func (s *Store) ListWebSessions(ctx context.Context, userID uuid.UUID) ([]*model.WebSession, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, user_id, user_agent, ip, created_at, expires_at, revoked_at
		FROM web_sessions
		WHERE user_id = $1 AND revoked_at IS NULL AND expires_at > now()
		ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var out []*model.WebSession
	for rows.Next() {
		var (
			sess      model.WebSession
			revokedAt *time.Time
		)
		if err = rows.Scan(&sess.ID, &sess.UserID, &sess.UserAgent, &sess.IP,
			&sess.CreatedAt, &sess.ExpiresAt, &revokedAt); err != nil {
			return nil, mapErr(err)
		}
		sess.RevokedAt = revokedAt
		out = append(out, &sess)
	}
	return out, mapErr(rows.Err())
}

// PruneWebSessions removes expired or long-revoked sessions.
func (s *Store) PruneWebSessions(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM web_sessions
		WHERE expires_at < now() OR revoked_at < now() - interval '7 days'`)
	if err != nil {
		return 0, mapErr(err)
	}
	return tag.RowsAffected(), nil
}
