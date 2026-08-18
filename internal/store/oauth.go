package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"aihub/internal/model"
)

const oauthColumns = `s.id, s.user_id, s.provider, s.state, s.redirect_uri, s.auth_url, s.label,
	s.target_scope, s.status, s.error, s.connection_id, s.created_at, s.expires_at, s.completed_at,
	u.username`

// CreateOAuthSession stores a pending ("temporary") connection attempt.
func (s *Store) CreateOAuthSession(ctx context.Context, sess *model.OAuthSession) error {
	if sess.ID == uuid.Nil {
		sess.ID = uuid.New()
	}
	if sess.Status == "" {
		sess.Status = model.OAuthPending
	}
	if sess.TargetScope == "" {
		sess.TargetScope = model.ScopePrivate
	}

	row := s.pool.QueryRow(ctx, `
		INSERT INTO oauth_sessions (id, user_id, provider, state, code_verifier, redirect_uri,
		                            auth_url, label, target_scope, status, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING created_at`,
		sess.ID, sess.UserID, string(sess.Provider), sess.State, sess.CodeVerifier, sess.RedirectURI,
		sess.AuthURL, sess.Label, sess.TargetScope, sess.Status, sess.ExpiresAt)
	return mapErr(row.Scan(&sess.CreatedAt))
}

// GetOAuthSession loads a session by id, including its PKCE verifier.
func (s *Store) GetOAuthSession(ctx context.Context, id uuid.UUID) (*model.OAuthSession, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT `+oauthColumns+`, s.code_verifier
		FROM oauth_sessions s JOIN users u ON u.id = s.user_id
		WHERE s.id = $1`, id)
	return scanOAuthSession(row, true)
}

// GetOAuthSessionByState resolves the `state` parameter returned by the provider.
func (s *Store) GetOAuthSessionByState(ctx context.Context, state string) (*model.OAuthSession, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT `+oauthColumns+`, s.code_verifier
		FROM oauth_sessions s JOIN users u ON u.id = s.user_id
		WHERE s.state = $1`, state)
	return scanOAuthSession(row, true)
}

// ListOAuthSessions returns recent attempts, newest first. A zero userID lists
// every user's attempts (admin view).
func (s *Store) ListOAuthSessions(ctx context.Context, userID uuid.UUID, pendingOnly bool, limit int) ([]*model.OAuthSession, error) {
	where := "1 = 1"
	args := []any{}
	if userID != uuid.Nil {
		where += " AND s.user_id = $1"
		args = append(args, userID)
	}
	if pendingOnly {
		where += " AND s.status = 'pending' AND s.expires_at > now()"
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	query := fmt.Sprintf(`
		SELECT `+oauthColumns+`
		FROM oauth_sessions s JOIN users u ON u.id = s.user_id
		WHERE %s
		ORDER BY s.created_at DESC
		LIMIT %d`, where, limit)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var out []*model.OAuthSession
	for rows.Next() {
		sess, scanErr := scanOAuthSession(rows, false)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, sess)
	}
	return out, mapErr(rows.Err())
}

// CountPendingOAuthSessions reports how many attempts a user has in flight.
func (s *Store) CountPendingOAuthSessions(ctx context.Context, userID uuid.UUID) (int, error) {
	var count int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM oauth_sessions
		WHERE user_id = $1 AND status = 'pending' AND expires_at > now()`, userID).Scan(&count)
	return count, mapErr(err)
}

// CompleteOAuthSession links a finished attempt to the connection it produced
// and drops the verifier.
func (s *Store) CompleteOAuthSession(ctx context.Context, id, connectionID uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE oauth_sessions
		SET status = 'completed', connection_id = $2, code_verifier = '', error = '',
		    completed_at = now(), updated_at = now()
		WHERE id = $1`, id, connectionID)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// FailOAuthSession records why an attempt could not be finished.
func (s *Store) FailOAuthSession(ctx context.Context, id uuid.UUID, reason string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE oauth_sessions SET status = 'failed', error = $2, updated_at = now()
		WHERE id = $1`, id, truncate(reason, 1000))
	return mapErr(err)
}

// CancelOAuthSession discards a pending attempt.
func (s *Store) CancelOAuthSession(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE oauth_sessions
		SET status = 'cancelled', code_verifier = '', updated_at = now()
		WHERE id = $1 AND status = 'pending'`, id)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ExpireOAuthSessions flips timed-out pending attempts to expired and drops the
// verifiers, so stale secrets do not linger in the database.
func (s *Store) ExpireOAuthSessions(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE oauth_sessions
		SET status = 'expired', code_verifier = '', updated_at = now()
		WHERE status = 'pending' AND expires_at <= now()`)
	if err != nil {
		return 0, mapErr(err)
	}
	return tag.RowsAffected(), nil
}

// PruneOAuthSessions deletes finished attempts older than the cutoff.
func (s *Store) PruneOAuthSessions(ctx context.Context, olderThan time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM oauth_sessions WHERE status <> 'pending' AND updated_at < $1`, olderThan)
	if err != nil {
		return 0, mapErr(err)
	}
	return tag.RowsAffected(), nil
}

func scanOAuthSession(row rowScanner, withVerifier bool) (*model.OAuthSession, error) {
	var (
		sess         model.OAuthSession
		provider     string
		connectionID *uuid.UUID
		completedAt  *time.Time
		verifier     string
	)
	dest := []any{
		&sess.ID, &sess.UserID, &provider, &sess.State, &sess.RedirectURI, &sess.AuthURL,
		&sess.Label, &sess.TargetScope, &sess.Status, &sess.Error, &connectionID,
		&sess.CreatedAt, &sess.ExpiresAt, &completedAt, &sess.OwnerUsername,
	}
	if withVerifier {
		dest = append(dest, &verifier)
	}
	if err := row.Scan(dest...); err != nil {
		return nil, mapErr(err)
	}

	sess.Provider = model.Provider(provider)
	sess.ConnectionID = connectionID
	sess.CompletedAt = completedAt
	sess.CodeVerifier = verifier
	return &sess, nil
}
