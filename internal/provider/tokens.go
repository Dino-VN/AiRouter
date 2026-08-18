package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"aihub/internal/model"
	"aihub/internal/store"
)

// refreshSkew renews an access token slightly before it actually expires.
const refreshSkew = 2 * time.Minute

// TokenManager keeps connection credentials fresh. Concurrent requests for the
// same connection share a single refresh so a provider never sees a stampede
// (and, for Codex, never sees a reused refresh token).
type TokenManager struct {
	registry *Registry
	store    *store.Store
	log      *slog.Logger

	mu       sync.Mutex
	inflight map[uuid.UUID]*refreshCall
}

type refreshCall struct {
	done chan struct{}
	cred *model.Credential
	err  error
}

func newTokenManager(registry *Registry, st *store.Store, logger *slog.Logger) *TokenManager {
	return &TokenManager{
		registry: registry,
		store:    st,
		log:      logger,
		inflight: map[uuid.UUID]*refreshCall{},
	}
}

// Ensure returns a credential whose access token is currently valid, refreshing
// and persisting it when needed. conn.Credential is updated in place.
func (m *TokenManager) Ensure(ctx context.Context, conn *model.Connection) (*model.Credential, error) {
	if conn == nil {
		return nil, errors.New("provider: nil connection")
	}
	if conn.Credential != nil && !conn.Credential.Expired(refreshSkew) {
		return conn.Credential, nil
	}
	return m.refresh(ctx, conn)
}

// ForceRefresh renews a credential even if the current token still looks valid.
func (m *TokenManager) ForceRefresh(ctx context.Context, conn *model.Connection) (*model.Credential, error) {
	return m.refresh(ctx, conn)
}

func (m *TokenManager) refresh(ctx context.Context, conn *model.Connection) (*model.Credential, error) {
	m.mu.Lock()
	if call, ok := m.inflight[conn.ID]; ok {
		m.mu.Unlock()
		select {
		case <-call.done:
			if call.err != nil {
				return nil, call.err
			}
			conn.Credential = call.cred
			return call.cred, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &refreshCall{done: make(chan struct{})}
	m.inflight[conn.ID] = call
	m.mu.Unlock()

	// The refresh itself must not be cancelled by the first caller giving up:
	// the provider may have already rotated the refresh token, and losing the
	// new one would lock the connection out permanently.
	refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	go func() {
		defer cancel()
		call.cred, call.err = m.doRefresh(refreshCtx, conn)
		close(call.done)

		m.mu.Lock()
		delete(m.inflight, conn.ID)
		m.mu.Unlock()
	}()

	select {
	case <-call.done:
		if call.err != nil {
			return nil, call.err
		}
		conn.Credential = call.cred
		return call.cred, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (m *TokenManager) doRefresh(ctx context.Context, conn *model.Connection) (*model.Credential, error) {
	p, err := m.registry.Get(conn.Provider)
	if err != nil {
		return nil, err
	}

	// Re-read the connection: another process may have refreshed it already.
	if fresh, loadErr := m.store.GetConnection(ctx, conn.ID); loadErr == nil {
		if fresh.Credential != nil && !fresh.Credential.Expired(refreshSkew) {
			conn.Credential = fresh.Credential
			return fresh.Credential, nil
		}
		if fresh.Credential != nil && fresh.Credential.RefreshToken != "" {
			conn.Credential = fresh.Credential
		}
	}

	result, err := p.Refresh(ctx, conn.Credential)
	if err != nil {
		status := model.ConnStatusError
		cooldown := 2 * time.Minute
		if errors.Is(err, ErrCredentialRevoked) {
			status = model.ConnStatusExpired
			cooldown = 0
		}
		if markErr := m.store.MarkConnectionError(ctx, conn.ID, err.Error(), cooldown, status); markErr != nil {
			m.log.Warn("could not record refresh failure", "connection", conn.ID, "error", markErr)
		}
		conn.Status = status
		conn.LastError = err.Error()
		return nil, fmt.Errorf("refresh %s connection %s: %w", conn.Provider, conn.Label, err)
	}

	update := store.CredentialUpdate{
		Credential: result.Credential,
		AccountID:  result.AccountID,
		Plan:       result.Plan,
		ProjectID:  result.ProjectID,
		Status:     model.ConnStatusActive,
		ClearError: true,
	}
	if err = m.store.UpdateCredential(ctx, conn.ID, update); err != nil {
		return nil, fmt.Errorf("persist refreshed credential: %w", err)
	}

	conn.Credential = result.Credential
	conn.Status = model.ConnStatusActive
	conn.LastError = ""
	conn.DisabledUntil = nil
	conn.TokenExpiresAt = nullableTime(result.Credential.ExpiresAt)
	if result.AccountID != "" {
		conn.AccountID = result.AccountID
	}
	if result.Plan != "" {
		conn.Plan = result.Plan
	}
	if result.ProjectID != "" {
		conn.ProjectID = result.ProjectID
	}

	m.log.Debug("refreshed credential", "provider", conn.Provider, "connection", conn.ID,
		"expires_at", result.Credential.ExpiresAt)
	return result.Credential, nil
}

// RefreshQuota reads the provider-reported quota for a connection and stores it.
func (m *TokenManager) RefreshQuota(ctx context.Context, conn *model.Connection) (*model.UpstreamQuota, error) {
	p, err := m.registry.Get(conn.Provider)
	if err != nil {
		return nil, err
	}
	if _, err = m.Ensure(ctx, conn); err != nil {
		return nil, err
	}

	quota, err := p.FetchQuota(ctx, conn)
	if err != nil {
		return nil, err
	}
	if quota == nil {
		return nil, nil
	}
	if err = m.store.UpdateConnectionQuota(ctx, conn.ID, quota); err != nil {
		return nil, err
	}
	if quota.Plan != "" && quota.Plan != conn.Plan {
		if err = m.store.UpdateConnectionPlan(ctx, conn.ID, quota.Plan); err != nil {
			m.log.Warn("could not record plan", "connection", conn.ID, "error", err)
		}
		conn.Plan = quota.Plan
	}
	conn.Quota = quota
	return quota, nil
}

// RecordQuota stores a quota snapshot observed while proxying a request.
func (m *TokenManager) RecordQuota(ctx context.Context, conn *model.Connection, quota *model.UpstreamQuota) {
	if quota == nil {
		return
	}
	if err := m.store.UpdateConnectionQuota(ctx, conn.ID, quota); err != nil {
		m.log.Debug("could not store observed quota", "connection", conn.ID, "error", err)
	}
}

func nullableTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
