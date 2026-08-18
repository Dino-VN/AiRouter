package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"aihub/internal/model"
)

const connectionColumns = `c.id, c.owner_id, c.provider, c.label, c.account_email, c.account_id,
	c.project_id, c.plan, c.status, c.scope, c.weight, c.metadata, c.quota, c.quota_updated_at,
	c.disabled_until, c.last_error, c.last_used_at, c.token_expires_at, c.created_at, c.updated_at`

// ConnectionFilter narrows a connection listing.
type ConnectionFilter struct {
	// OwnerID limits results to one owner. Zero means "every owner" and is
	// only used by admin listings.
	OwnerID uuid.UUID
	// IncludeShared adds connections other users marked as shared.
	IncludeShared bool
	// Provider limits results to a single provider when non-empty.
	Provider model.Provider
	// UsableOnly drops disabled or cooling-down connections.
	UsableOnly bool
}

// CreateConnection stores a new upstream account with its credential sealed.
func (s *Store) CreateConnection(ctx context.Context, conn *model.Connection, cred *model.Credential) error {
	if conn.ID == uuid.Nil {
		conn.ID = uuid.New()
	}
	if conn.Weight <= 0 {
		conn.Weight = 1
	}
	if conn.Status == "" {
		conn.Status = model.ConnStatusActive
	}
	if conn.Scope == "" {
		conn.Scope = model.ScopePrivate
	}

	sealed, err := s.sealCredential(cred)
	if err != nil {
		return err
	}
	metadata, err := marshalJSONB(conn.Metadata)
	if err != nil {
		return err
	}

	row := s.pool.QueryRow(ctx, `
		INSERT INTO connections (id, owner_id, provider, label, account_email, account_id, project_id,
		                         plan, status, scope, weight, secret, metadata, token_expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		RETURNING created_at, updated_at`,
		conn.ID, conn.OwnerID, string(conn.Provider), conn.Label, strings.ToLower(conn.AccountEmail),
		conn.AccountID, conn.ProjectID, conn.Plan, conn.Status, conn.Scope, conn.Weight,
		sealed, metadata, conn.TokenExpiresAt)
	if err = row.Scan(&conn.CreatedAt, &conn.UpdatedAt); err != nil {
		return mapErr(err)
	}
	conn.Credential = cred
	return nil
}

// GetConnection loads one connection including its decrypted credential.
func (s *Store) GetConnection(ctx context.Context, id uuid.UUID) (*model.Connection, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT `+connectionColumns+`, u.username, c.secret
		FROM connections c JOIN users u ON u.id = c.owner_id
		WHERE c.id = $1`, id)

	conn, sealed, err := scanConnectionWithSecret(row)
	if err != nil {
		return nil, err
	}
	if conn.Credential, err = s.openCredential(sealed); err != nil {
		return nil, err
	}
	return conn, nil
}

// ListConnections returns connections matching the filter, with a 24h request
// count for the UI.
func (s *Store) ListConnections(ctx context.Context, filter ConnectionFilter) ([]*model.Connection, error) {
	where := []string{"1 = 1"}
	args := []any{}
	next := 1

	if filter.OwnerID != uuid.Nil {
		if filter.IncludeShared {
			where = append(where, fmt.Sprintf("(c.owner_id = $%d OR c.scope = 'shared')", next))
		} else {
			where = append(where, fmt.Sprintf("c.owner_id = $%d", next))
		}
		args = append(args, filter.OwnerID)
		next++
	}
	if filter.Provider != "" {
		where = append(where, fmt.Sprintf("c.provider = $%d", next))
		args = append(args, string(filter.Provider))
		next++
	}
	if filter.UsableOnly {
		where = append(where, "c.status <> 'disabled'")
		where = append(where, "(c.disabled_until IS NULL OR c.disabled_until <= now())")
	}

	query := `
		SELECT ` + connectionColumns + `, u.username,
		       COALESCE((SELECT count(*) FROM usage_records ur
		                 WHERE ur.connection_id = c.id AND ur.created_at > now() - interval '24 hours'), 0)
		FROM connections c JOIN users u ON u.id = c.owner_id
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY c.provider, c.created_at`

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var out []*model.Connection
	for rows.Next() {
		conn, count, scanErr := scanConnectionWithCount(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		conn.RequestCount24h = count
		out = append(out, conn)
	}
	return out, mapErr(rows.Err())
}

// SelectCandidates returns usable connections for a provider ordered
// least-recently-used first, so traffic spreads across accounts.
func (s *Store) SelectCandidates(ctx context.Context, ownerID uuid.UUID, provider model.Provider, includeShared bool) ([]*model.Connection, error) {
	ownership := "c.owner_id = $1"
	if includeShared {
		ownership = "(c.owner_id = $1 OR c.scope = 'shared')"
	}
	query := `
		SELECT ` + connectionColumns + `, u.username, c.secret
		FROM connections c JOIN users u ON u.id = c.owner_id
		WHERE ` + ownership + `
		  AND c.provider = $2
		  AND c.status <> 'disabled'
		  AND (c.disabled_until IS NULL OR c.disabled_until <= now())
		ORDER BY c.last_used_at ASC NULLS FIRST, c.created_at ASC`

	rows, err := s.pool.Query(ctx, query, ownerID, string(provider))
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var out []*model.Connection
	for rows.Next() {
		conn, sealed, scanErr := scanConnectionWithSecret(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		cred, credErr := s.openCredential(sealed)
		if credErr != nil {
			// A single unreadable credential (rotated encryption key) must not
			// break selection for the rest of the pool.
			conn.Status = model.ConnStatusError
			conn.LastError = credErr.Error()
			continue
		}
		conn.Credential = cred
		out = append(out, conn)
	}
	return out, mapErr(rows.Err())
}

// CountConnections returns how many connections an owner has.
func (s *Store) CountConnections(ctx context.Context, ownerID uuid.UUID) (int, error) {
	var count int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM connections WHERE owner_id = $1`, ownerID).Scan(&count)
	return count, mapErr(err)
}

// FindConnectionByAccount locates an existing connection for the same upstream
// account, used to refresh instead of duplicating on re-login.
func (s *Store) FindConnectionByAccount(ctx context.Context, ownerID uuid.UUID, provider model.Provider, accountEmail string) (*model.Connection, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT `+connectionColumns+`, u.username, c.secret
		FROM connections c JOIN users u ON u.id = c.owner_id
		WHERE c.owner_id = $1 AND c.provider = $2 AND lower(c.account_email) = lower($3)`,
		ownerID, string(provider), accountEmail)

	conn, sealed, err := scanConnectionWithSecret(row)
	if err != nil {
		return nil, err
	}
	if conn.Credential, err = s.openCredential(sealed); err != nil {
		return nil, err
	}
	return conn, nil
}

// CredentialUpdate carries a refreshed credential plus the account facts that
// may have changed with it.
type CredentialUpdate struct {
	Credential *model.Credential
	AccountID  string
	Plan       string
	ProjectID  string
	Status     string
	ClearError bool
}

// UpdateCredential persists a refreshed credential.
func (s *Store) UpdateCredential(ctx context.Context, id uuid.UUID, update CredentialUpdate) error {
	sealed, err := s.sealCredential(update.Credential)
	if err != nil {
		return err
	}

	sets := []string{"secret = $2", "updated_at = now()", "token_expires_at = $3"}
	args := []any{id, sealed, nullTime(update.Credential.ExpiresAt)}
	next := 4

	if update.AccountID != "" {
		sets = append(sets, fmt.Sprintf("account_id = $%d", next))
		args = append(args, update.AccountID)
		next++
	}
	if update.Plan != "" {
		sets = append(sets, fmt.Sprintf("plan = $%d", next))
		args = append(args, update.Plan)
		next++
	}
	if update.ProjectID != "" {
		sets = append(sets, fmt.Sprintf("project_id = $%d", next))
		args = append(args, update.ProjectID)
		next++
	}
	if update.Status != "" {
		sets = append(sets, fmt.Sprintf("status = $%d", next))
		args = append(args, update.Status)
		next++
	}
	if update.ClearError {
		sets = append(sets, "last_error = ''")
	}

	tag, err := s.pool.Exec(ctx,
		`UPDATE connections SET `+strings.Join(sets, ", ")+` WHERE id = $1`, args...)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ConnectionSettings are the operator-editable fields of a connection.
type ConnectionSettings struct {
	Label  *string
	Scope  *string
	Weight *int
	Status *string
}

// UpdateConnectionSettings applies a partial settings update.
func (s *Store) UpdateConnectionSettings(ctx context.Context, id uuid.UUID, settings ConnectionSettings) error {
	sets := []string{"updated_at = now()"}
	args := []any{id}
	next := 2

	if settings.Label != nil {
		sets = append(sets, fmt.Sprintf("label = $%d", next))
		args = append(args, *settings.Label)
		next++
	}
	if settings.Scope != nil {
		sets = append(sets, fmt.Sprintf("scope = $%d", next))
		args = append(args, *settings.Scope)
		next++
	}
	if settings.Weight != nil {
		sets = append(sets, fmt.Sprintf("weight = $%d", next))
		args = append(args, *settings.Weight)
		next++
	}
	if settings.Status != nil {
		sets = append(sets, fmt.Sprintf("status = $%d", next))
		args = append(args, *settings.Status)
		next++
		// Re-enabling clears any cooldown left over from the previous failure.
		if *settings.Status == model.ConnStatusActive {
			sets = append(sets, "disabled_until = NULL", "last_error = ''")
		}
	}

	tag, err := s.pool.Exec(ctx,
		`UPDATE connections SET `+strings.Join(sets, ", ")+` WHERE id = $1`, args...)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// TouchConnectionUsed records that a connection just served a request.
func (s *Store) TouchConnectionUsed(ctx context.Context, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `UPDATE connections SET last_used_at = now() WHERE id = $1`, id)
	return mapErr(err)
}

// MarkConnectionError records a failure and optionally cools the connection down.
func (s *Store) MarkConnectionError(ctx context.Context, id uuid.UUID, message string, cooldown time.Duration, status string) error {
	if status == "" {
		status = model.ConnStatusError
	}
	var until *time.Time
	if cooldown > 0 {
		t := time.Now().Add(cooldown)
		until = &t
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE connections
		SET last_error = $2, status = $3, disabled_until = $4, updated_at = now()
		WHERE id = $1`, id, truncate(message, 2000), status, until)
	return mapErr(err)
}

// ClearConnectionError marks a connection healthy again.
func (s *Store) ClearConnectionError(ctx context.Context, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE connections
		SET last_error = '', status = 'active', disabled_until = NULL, updated_at = now()
		WHERE id = $1 AND status <> 'disabled'`, id)
	return mapErr(err)
}

// UpdateConnectionQuota stores the latest upstream quota snapshot.
func (s *Store) UpdateConnectionQuota(ctx context.Context, id uuid.UUID, quota *model.UpstreamQuota) error {
	payload, err := jsonbText(quota)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		UPDATE connections SET quota = $2, quota_updated_at = now(), updated_at = now()
		WHERE id = $1`, id, payload)
	return mapErr(err)
}

// UpdateConnectionPlan records the plan/tier reported by the provider.
func (s *Store) UpdateConnectionPlan(ctx context.Context, id uuid.UUID, plan string) error {
	if strings.TrimSpace(plan) == "" {
		return nil
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE connections SET plan = $2, updated_at = now() WHERE id = $1 AND plan <> $2`, id, plan)
	return mapErr(err)
}

// DeleteConnection removes a connection.
func (s *Store) DeleteConnection(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM connections WHERE id = $1`, id)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) sealCredential(cred *model.Credential) ([]byte, error) {
	if cred == nil {
		cred = &model.Credential{}
	}
	plaintext, err := json.Marshal(cred)
	if err != nil {
		return nil, fmt.Errorf("marshal credential: %w", err)
	}
	sealed, err := s.box.Seal(plaintext)
	if err != nil {
		return nil, fmt.Errorf("seal credential: %w", err)
	}
	return sealed, nil
}

func (s *Store) openCredential(sealed []byte) (*model.Credential, error) {
	if len(sealed) == 0 {
		return &model.Credential{}, nil
	}
	plaintext, err := s.box.Open(sealed)
	if err != nil {
		return nil, fmt.Errorf("open credential: %w", err)
	}
	var cred model.Credential
	if err = json.Unmarshal(plaintext, &cred); err != nil {
		return nil, fmt.Errorf("unmarshal credential: %w", err)
	}
	return &cred, nil
}

func scanConnectionBase(row rowScanner, extra ...any) (*model.Connection, error) {
	var (
		conn         model.Connection
		provider     string
		metadataRaw  []byte
		quotaRaw     []byte
		quotaUpdated *time.Time
		disabled     *time.Time
		lastUsed     *time.Time
		tokenExpires *time.Time
	)
	dest := []any{
		&conn.ID, &conn.OwnerID, &provider, &conn.Label, &conn.AccountEmail, &conn.AccountID,
		&conn.ProjectID, &conn.Plan, &conn.Status, &conn.Scope, &conn.Weight, &metadataRaw,
		&quotaRaw, &quotaUpdated, &disabled, &conn.LastError, &lastUsed, &tokenExpires,
		&conn.CreatedAt, &conn.UpdatedAt, &conn.OwnerUsername,
	}
	dest = append(dest, extra...)
	if err := row.Scan(dest...); err != nil {
		return nil, mapErr(err)
	}

	conn.Provider = model.Provider(provider)
	conn.QuotaUpdatedAt = quotaUpdated
	conn.DisabledUntil = disabled
	conn.LastUsedAt = lastUsed
	conn.TokenExpiresAt = tokenExpires

	if len(metadataRaw) > 0 {
		_ = json.Unmarshal(metadataRaw, &conn.Metadata)
	}
	if len(quotaRaw) > 0 {
		var quota model.UpstreamQuota
		if err := json.Unmarshal(quotaRaw, &quota); err == nil {
			if quota.UpdatedAt.IsZero() && quotaUpdated != nil {
				quota.UpdatedAt = *quotaUpdated
			}
			conn.Quota = &quota
		}
	}
	return &conn, nil
}

func scanConnectionWithSecret(row rowScanner) (*model.Connection, []byte, error) {
	var sealed []byte
	conn, err := scanConnectionBase(row, &sealed)
	if err != nil {
		return nil, nil, err
	}
	return conn, sealed, nil
}

func scanConnectionWithCount(row rowScanner) (*model.Connection, int64, error) {
	var count int64
	conn, err := scanConnectionBase(row, &count)
	if err != nil {
		return nil, 0, err
	}
	return conn, count, nil
}

// jsonbText encodes a value for a jsonb column.
//
// It deliberately hands pgx a string rather than the []byte json.Marshal
// returns. The pool runs in QueryExecModeExec (see dbx.Connect), where pgx
// inlines parameters into the SQL text instead of using the extended protocol,
// and it renders a []byte as a bytea hex literal: '\x7b2261...'. A jsonb column
// rejects that with SQLSTATE 22P02, invalid input syntax for type json. A string
// is inlined as an ordinary quoted literal, which works in either mode.
func jsonbText(v any) (string, error) {
	payload, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("marshal jsonb: %w", err)
	}
	return string(payload), nil
}

// marshalJSONB encodes a metadata map for a nullable jsonb column, storing an
// empty map as SQL NULL. The return type is any so that NULL travels as an
// untyped nil: a typed nil []byte would be inlined as '\x', which is not JSON
// either. See jsonbText for the rest.
func marshalJSONB(v map[string]any) (any, error) {
	if len(v) == 0 {
		return nil, nil
	}
	text, err := jsonbText(v)
	if err != nil {
		return nil, err
	}
	return text, nil
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit]
}
