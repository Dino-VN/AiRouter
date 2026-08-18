package store

import (
        "context"
        "fmt"
        "strings"
        "time"

        "github.com/google/uuid"

        "aihub/internal/model"
)

const userColumns = `id, username, password_hash, display_name, role, status, created_at, updated_at, last_login_at`

// CreateUser inserts a user together with its quota row.
func (s *Store) CreateUser(ctx context.Context, user *model.User, quota model.Quota) error {
        return s.insertUser(ctx, user, quota, false)
}

// CreateFirstAdmin inserts a user only while the users table is still empty.
// That guard is what makes the unauthenticated first-run setup endpoint safe to
// expose: it lives inside the INSERT, so two concurrent setup requests cannot
// both create an owner the way a separate count-then-insert would allow.
// ErrConflict means somebody else completed setup first.
func (s *Store) CreateFirstAdmin(ctx context.Context, user *model.User, quota model.Quota) error {
        return s.insertUser(ctx, user, quota, true)
}

func (s *Store) insertUser(ctx context.Context, user *model.User, quota model.Quota, onlyIfFirst bool) error {
        if user.ID == uuid.Nil {
                user.ID = uuid.New()
        }
        // The capitalisation the account was created with is kept; uniqueness and
        // lookups both go through lower(username), so "Ann" and "ann" are the same
        // account without either spelling being rewritten.
        user.Username = strings.TrimSpace(user.Username)
        quota.UserID = user.ID

        tx, err := s.pool.Begin(ctx)
        if err != nil {
                return fmt.Errorf("create user: begin: %w", err)
        }
        defer func() { _ = tx.Rollback(ctx) }()

        // The casts are explicit because the guarded form feeds the parameters
        // through a SELECT list, where the server has less to infer types from.
        insert := `
                INSERT INTO users (id, username, password_hash, display_name, role, status)
                VALUES ($1, $2, $3, $4, $5, $6)
                RETURNING created_at, updated_at`
        if onlyIfFirst {
                insert = `
                INSERT INTO users (id, username, password_hash, display_name, role, status)
                SELECT $1::uuid, $2::text, $3::text, $4::text, $5::text, $6::text
                WHERE NOT EXISTS (SELECT 1 FROM users)
                RETURNING created_at, updated_at`
        }

        row := tx.QueryRow(ctx, insert,
                user.ID, user.Username, user.PasswordHash, user.DisplayName, string(user.Role), user.Status)
        if err = row.Scan(&user.CreatedAt, &user.UpdatedAt); err != nil {
                if mapped := mapErr(err); onlyIfFirst && mapped == ErrNotFound {
                        // The WHERE NOT EXISTS guard matched nothing, so a user already exists.
                        return ErrConflict
                }
                return mapErr(err)
        }

        if _, err = tx.Exec(ctx, `
                INSERT INTO user_quotas (user_id, requests_per_day, tokens_per_day, requests_per_month,
                                         tokens_per_month, max_connections, max_api_keys, allowed_providers,
                                         allow_shared_pool, concurrent_limit)
                VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
                quota.UserID, quota.RequestsPerDay, quota.TokensPerDay, quota.RequestsPerMonth,
                quota.TokensPerMonth, quota.MaxConnections, quota.MaxAPIKeys, providerList(quota.AllowedProviders),
                quota.AllowSharedPool, quota.ConcurrentLimit); err != nil {
                return mapErr(err)
        }

        if err = tx.Commit(ctx); err != nil {
                return fmt.Errorf("create user: commit: %w", err)
        }
        return nil
}

// GetUser loads a user by id.
func (s *Store) GetUser(ctx context.Context, id uuid.UUID) (*model.User, error) {
        row := s.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE id = $1`, id)
        return scanUser(row)
}

// GetUserByUsername loads a user by (case-insensitive) username.
func (s *Store) GetUserByUsername(ctx context.Context, username string) (*model.User, error) {
        row := s.pool.QueryRow(ctx,
                `SELECT `+userColumns+` FROM users WHERE lower(username) = lower($1)`,
                strings.TrimSpace(username))
        return scanUser(row)
}

// ListUsers returns every user ordered by creation time.
func (s *Store) ListUsers(ctx context.Context) ([]*model.User, error) {
        rows, err := s.pool.Query(ctx, `SELECT `+userColumns+` FROM users ORDER BY created_at`)
        if err != nil {
                return nil, mapErr(err)
        }
        defer rows.Close()

        var users []*model.User
        for rows.Next() {
                user, scanErr := scanUser(rows)
                if scanErr != nil {
                        return nil, scanErr
                }
                users = append(users, user)
        }
        return users, mapErr(rows.Err())
}

// CountUsers returns the total number of users.
func (s *Store) CountUsers(ctx context.Context) (int64, error) {
        var count int64
        err := s.pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&count)
        return count, mapErr(err)
}

// UserUpdate carries the mutable fields of a user. Nil fields are left alone.
type UserUpdate struct {
        DisplayName  *string
        Role         *model.Role
        Status       *string
        PasswordHash *string
}

// UpdateUser applies a partial update.
func (s *Store) UpdateUser(ctx context.Context, id uuid.UUID, update UserUpdate) (*model.User, error) {
        sets := []string{"updated_at = now()"}
        args := []any{id}
        next := 2

        if update.DisplayName != nil {
                sets = append(sets, fmt.Sprintf("display_name = $%d", next))
                args = append(args, *update.DisplayName)
                next++
        }
        if update.Role != nil {
                sets = append(sets, fmt.Sprintf("role = $%d", next))
                args = append(args, string(*update.Role))
                next++
        }
        if update.Status != nil {
                sets = append(sets, fmt.Sprintf("status = $%d", next))
                args = append(args, *update.Status)
                next++
        }
        if update.PasswordHash != nil {
                sets = append(sets, fmt.Sprintf("password_hash = $%d", next))
                args = append(args, *update.PasswordHash)
                next++
        }

        query := `UPDATE users SET ` + strings.Join(sets, ", ") + ` WHERE id = $1 RETURNING ` + userColumns
        return scanUser(s.pool.QueryRow(ctx, query, args...))
}

// TouchUserLogin records a successful login.
func (s *Store) TouchUserLogin(ctx context.Context, id uuid.UUID) error {
        _, err := s.pool.Exec(ctx, `UPDATE users SET last_login_at = now() WHERE id = $1`, id)
        return mapErr(err)
}

// DeleteUser removes a user and everything they own.
func (s *Store) DeleteUser(ctx context.Context, id uuid.UUID) error {
        tag, err := s.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, id)
        if err != nil {
                return mapErr(err)
        }
        if tag.RowsAffected() == 0 {
                return ErrNotFound
        }
        return nil
}

// GetQuota loads a user's limits, falling back to defaults if the row is gone.
func (s *Store) GetQuota(ctx context.Context, userID uuid.UUID) (model.Quota, error) {
        var quota model.Quota
        err := s.pool.QueryRow(ctx, `
                SELECT user_id, requests_per_day, tokens_per_day, requests_per_month, tokens_per_month,
                       max_connections, max_api_keys, allowed_providers, allow_shared_pool,
                       concurrent_limit, updated_at
                FROM user_quotas WHERE user_id = $1`, userID).Scan(
                &quota.UserID, &quota.RequestsPerDay, &quota.TokensPerDay, &quota.RequestsPerMonth,
                &quota.TokensPerMonth, &quota.MaxConnections, &quota.MaxAPIKeys, &quota.AllowedProviders,
                &quota.AllowSharedPool, &quota.ConcurrentLimit, &quota.UpdatedAt)
        if err != nil {
                if mapped := mapErr(err); mapped == ErrNotFound {
                        return model.DefaultQuota(userID), nil
                }
                return quota, mapErr(err)
        }
        return quota, nil
}

// UpsertQuota writes a user's limits.
func (s *Store) UpsertQuota(ctx context.Context, quota model.Quota) (model.Quota, error) {
        err := s.pool.QueryRow(ctx, `
                INSERT INTO user_quotas (user_id, requests_per_day, tokens_per_day, requests_per_month,
                                         tokens_per_month, max_connections, max_api_keys, allowed_providers,
                                         allow_shared_pool, concurrent_limit, updated_at)
                VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, now())
                ON CONFLICT (user_id) DO UPDATE SET
                        requests_per_day   = EXCLUDED.requests_per_day,
                        tokens_per_day     = EXCLUDED.tokens_per_day,
                        requests_per_month = EXCLUDED.requests_per_month,
                        tokens_per_month   = EXCLUDED.tokens_per_month,
                        max_connections    = EXCLUDED.max_connections,
                        max_api_keys       = EXCLUDED.max_api_keys,
                        allowed_providers  = EXCLUDED.allowed_providers,
                        allow_shared_pool  = EXCLUDED.allow_shared_pool,
                        concurrent_limit   = EXCLUDED.concurrent_limit,
                        updated_at         = now()
                RETURNING updated_at`,
                quota.UserID, quota.RequestsPerDay, quota.TokensPerDay, quota.RequestsPerMonth,
                quota.TokensPerMonth, quota.MaxConnections, quota.MaxAPIKeys, providerList(quota.AllowedProviders),
                quota.AllowSharedPool, quota.ConcurrentLimit).Scan(&quota.UpdatedAt)
        return quota, mapErr(err)
}

type rowScanner interface {
        Scan(dest ...any) error
}

func scanUser(row rowScanner) (*model.User, error) {
        var (
                user        model.User
                role        string
                lastLoginAt *time.Time
        )
        err := row.Scan(&user.ID, &user.Username, &user.PasswordHash, &user.DisplayName, &role,
                &user.Status, &user.CreatedAt, &user.UpdatedAt, &lastLoginAt)
        if err != nil {
                return nil, mapErr(err)
        }
        user.Role = model.Role(role)
        user.LastLoginAt = lastLoginAt
        return &user, nil
}

// providerList normalises a nil slice to an empty one so the text[] column never
// receives NULL.
func providerList(in []string) []string {
        if in == nil {
                return []string{}
        }
        return in
}
