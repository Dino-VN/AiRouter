// Package model holds the domain types shared by the store, the proxy and the
// HTTP API.
package model

import (
	"time"

	"github.com/google/uuid"
)

// Role enumerates web-UI permission levels.
type Role string

const (
	// RoleAdmin can manage every user, connection and global setting.
	RoleAdmin Role = "admin"
	// RoleUser can only manage resources they own.
	RoleUser Role = "user"
)

// Valid reports whether r is a known role.
func (r Role) Valid() bool { return r == RoleAdmin || r == RoleUser }

// Status values shared by users and API keys.
const (
	StatusActive    = "active"
	StatusSuspended = "suspended"
	StatusRevoked   = "revoked"
)

// Provider identifies an upstream account type.
type Provider string

const (
	// ProviderCodex is an OpenAI ChatGPT (Codex) OAuth account.
	ProviderCodex Provider = "codex"
	// ProviderAntigravity is a Google Antigravity OAuth account.
	ProviderAntigravity Provider = "antigravity"
	// ProviderOpenAI is a custom OpenAI-compatible API endpoint
	// authenticated with an API key rather than an OAuth flow. Operators
	// register the base URL (e.g. https://api.openai.com/v1, or any
	// OpenAI-compatible gateway), the API key, and the model list they
	// want to expose; the proxy forwards chat-completions and responses
	// requests verbatim, so this provider works against OpenAI itself,
	// Azure OpenAI, OpenRouter, vLLM, LocalAI, Ollama's OpenAI shim, etc.
	ProviderOpenAI Provider = "openai"
)

// Valid reports whether p is a provider this build supports.
func (p Provider) Valid() bool {
	return p == ProviderCodex || p == ProviderAntigravity || p == ProviderOpenAI
}

// Providers lists every supported provider.
func Providers() []Provider {
	return []Provider{ProviderCodex, ProviderAntigravity, ProviderOpenAI}
}

// IsOAuth reports whether a provider uses the OAuth refresh flow. The proxy and
// store layer use this to skip token refresh, loopback listeners, and OAuth
// sessions for API-key providers like ProviderOpenAI.
func (p Provider) IsOAuth() bool {
	return p == ProviderCodex || p == ProviderAntigravity
}

// Connection scopes.
const (
	// ScopePrivate restricts a connection to its owner.
	ScopePrivate = "private"
	// ScopeShared lets every active user route through the connection. Only
	// admins may create shared connections.
	ScopeShared = "shared"
)

// Connection lifecycle states.
const (
	ConnStatusActive   = "active"
	ConnStatusExpired  = "expired"
	ConnStatusDisabled = "disabled"
	ConnStatusError    = "error"
)

// User is a web-UI account. Accounts are identified by a username, not an email
// address: the only address this project cares about belongs to the upstream
// provider account on a Connection (see Connection.AccountEmail).
type User struct {
	ID           uuid.UUID  `json:"id"`
	Username     string     `json:"username"`
	DisplayName  string     `json:"display_name"`
	Role         Role       `json:"role"`
	Status       string     `json:"status"`
	PasswordHash string     `json:"-"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	LastLoginAt  *time.Time `json:"last_login_at,omitempty"`
}

// IsAdmin reports whether the user holds the admin role.
func (u *User) IsAdmin() bool { return u != nil && u.Role == RoleAdmin }

// Username length bounds. The limits live here rather than in the HTTP layer
// because the first-run setup handler, the admin create-user handler and the
// env-based bootstrap all have to agree on them.
const (
	UsernameMinLen = 3
	UsernameMaxLen = 32
)

// ValidUsername reports whether a handle can be used as a login identifier:
// ASCII letters, digits, dot, underscore or hyphen, starting and ending with a
// letter or digit. Comparison elsewhere is case-insensitive, so "Ann" and "ann"
// are the same account and only one of them can exist.
func ValidUsername(name string) bool {
	if len(name) < UsernameMinLen || len(name) > UsernameMaxLen {
		return false
	}
	for i := 0; i < len(name); i++ {
		switch c := name[i]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '_', c == '-':
			// Separators are allowed between characters but not at either end,
			// which keeps handles like "-bob" or "bob." out of the table.
			if i == 0 || i == len(name)-1 {
				return false
			}
		default:
			// Anything else, including every byte of a multi-byte rune, is out:
			// a login handle that renders differently in two fonts is a trap.
			return false
		}
	}
	return true
}

// Quota holds the per-user limits enforced before a request reaches an upstream
// provider. Zero means "unlimited" for every numeric field.
type Quota struct {
	UserID           uuid.UUID `json:"user_id"`
	RequestsPerDay   int64     `json:"requests_per_day"`
	TokensPerDay     int64     `json:"tokens_per_day"`
	RequestsPerMonth int64     `json:"requests_per_month"`
	TokensPerMonth   int64     `json:"tokens_per_month"`
	MaxConnections   int       `json:"max_connections"`
	MaxAPIKeys       int       `json:"max_api_keys"`
	AllowedProviders []string  `json:"allowed_providers"`
	AllowSharedPool  bool      `json:"allow_shared_pool"`
	ConcurrentLimit  int       `json:"concurrent_limit"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// DefaultQuota returns the limits applied to a freshly created user.
func DefaultQuota(userID uuid.UUID) Quota {
	return Quota{
		UserID:          userID,
		RequestsPerDay:  2000,
		TokensPerDay:    0,
		MaxConnections:  5,
		MaxAPIKeys:      5,
		AllowSharedPool: true,
		ConcurrentLimit: 8,
	}
}

// UnlimitedQuota returns limits suitable for an administrator.
func UnlimitedQuota(userID uuid.UUID) Quota {
	return Quota{UserID: userID, AllowSharedPool: true}
}

// Credential is the decrypted secret material for one upstream account.
type Credential struct {
	// AccessToken is the bearer token sent upstream.
	AccessToken string `json:"access_token,omitempty"`
	// RefreshToken renews AccessToken.
	RefreshToken string `json:"refresh_token,omitempty"`
	// IDToken is kept for Codex: its claims carry the account id and plan.
	IDToken string `json:"id_token,omitempty"`
	// ExpiresAt is when AccessToken stops working.
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	// LastRefresh records the most recent successful refresh.
	LastRefresh time.Time `json:"last_refresh,omitempty"`
}

// Expired reports whether the access token is past (or within skew of) expiry.
func (c *Credential) Expired(skew time.Duration) bool {
	if c == nil || c.AccessToken == "" {
		return true
	}
	if c.ExpiresAt.IsZero() {
		return false
	}
	return time.Now().Add(skew).After(c.ExpiresAt)
}

// QuotaWindow is one upstream rate-limit bucket (Codex reports a short rolling
// window and a weekly window).
type QuotaWindow struct {
	Name            string     `json:"name"`
	UsedPercent     float64    `json:"used_percent"`
	WindowMinutes   int        `json:"window_minutes,omitempty"`
	ResetsInSeconds int64      `json:"resets_in_seconds,omitempty"`
	ResetsAt        *time.Time `json:"resets_at,omitempty"`
}

// CreditBalance is the Antigravity paid-tier credit pool.
type CreditBalance struct {
	CreditType string  `json:"credit_type"`
	Amount     float64 `json:"amount"`
	Minimum    float64 `json:"minimum"`
	TierID     string  `json:"tier_id,omitempty"`
	Available  bool    `json:"available"`
}

// UpstreamQuota is the provider-reported quota snapshot for a connection.
type UpstreamQuota struct {
	Plan      string         `json:"plan,omitempty"`
	Windows   []QuotaWindow  `json:"windows,omitempty"`
	Credits   *CreditBalance `json:"credits,omitempty"`
	Note      string         `json:"note,omitempty"`
	UpdatedAt time.Time      `json:"updated_at,omitempty"`
}

// Connection is one upstream provider account usable for proxying.
type Connection struct {
	ID              uuid.UUID      `json:"id"`
	OwnerID         uuid.UUID      `json:"owner_id"`
	OwnerUsername   string         `json:"owner_username,omitempty"`
	Provider        Provider       `json:"provider"`
	Label           string         `json:"label"`
	AccountEmail    string         `json:"account_email"`
	AccountID       string         `json:"account_id,omitempty"`
	ProjectID       string         `json:"project_id,omitempty"`
	Plan            string         `json:"plan,omitempty"`
	Status          string         `json:"status"`
	Scope           string         `json:"scope"`
	Weight          int            `json:"weight"`
	DisabledUntil   *time.Time     `json:"disabled_until,omitempty"`
	LastError       string         `json:"last_error,omitempty"`
	LastUsedAt      *time.Time     `json:"last_used_at,omitempty"`
	TokenExpiresAt  *time.Time     `json:"token_expires_at,omitempty"`
	Quota           *UpstreamQuota `json:"quota,omitempty"`
	QuotaUpdatedAt  *time.Time     `json:"quota_updated_at,omitempty"`
	Metadata        map[string]any `json:"metadata,omitempty"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
	RequestCount24h int64          `json:"request_count_24h,omitempty"`

	// Credential is only populated on internal reads; it is never serialised
	// to API clients.
	Credential *Credential `json:"-"`
}

// Usable reports whether the connection may currently be selected.
func (c *Connection) Usable(now time.Time) bool {
	if c == nil {
		return false
	}
	if c.Status == ConnStatusDisabled {
		return false
	}
	if c.DisabledUntil != nil && now.Before(*c.DisabledUntil) {
		return false
	}
	return true
}

// APIKey is a proxy credential minted for a user.
type APIKey struct {
	ID            uuid.UUID  `json:"id"`
	UserID        uuid.UUID  `json:"user_id"`
	Name          string     `json:"name"`
	Prefix        string     `json:"prefix"`
	Status        string     `json:"status"`
	AllowedModels []string   `json:"allowed_models,omitempty"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	LastUsedAt    *time.Time `json:"last_used_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	RequestCount  int64      `json:"request_count,omitempty"`

	// Secret is only set on creation, the single time the plaintext key is
	// available.
	Secret string `json:"secret,omitempty"`
}

// UsageRecord is one proxied request.
type UsageRecord struct {
	ID               int64      `json:"id"`
	CreatedAt        time.Time  `json:"created_at"`
	UserID           uuid.UUID  `json:"user_id"`
	APIKeyID         *uuid.UUID `json:"api_key_id,omitempty"`
	ConnectionID     *uuid.UUID `json:"connection_id,omitempty"`
	Provider         string     `json:"provider"`
	Model            string     `json:"model"`
	ClientFormat     string     `json:"client_format"`
	StatusCode       int        `json:"status_code"`
	Stream           bool       `json:"stream"`
	PromptTokens     int64      `json:"prompt_tokens"`
	CompletionTokens int64      `json:"completion_tokens"`
	ReasoningTokens  int64      `json:"reasoning_tokens"`
	CachedTokens     int64      `json:"cached_tokens"`
	TotalTokens      int64      `json:"total_tokens"`
	LatencyMS        int64      `json:"latency_ms"`
	Error            string     `json:"error,omitempty"`
}

// OAuth session lifecycle states.
const (
	OAuthPending   = "pending"
	OAuthCompleted = "completed"
	OAuthFailed    = "failed"
	OAuthCancelled = "cancelled"
	OAuthExpired   = "expired"
)

// OAuthSession is an in-flight (temporary) connection attempt. It exists from
// the moment the UI asks for an authorization URL until the callback is
// redeemed, cancelled or expires.
type OAuthSession struct {
	ID            uuid.UUID  `json:"id"`
	UserID        uuid.UUID  `json:"user_id"`
	OwnerUsername string     `json:"owner_username,omitempty"`
	Provider      Provider   `json:"provider"`
	State         string     `json:"state"`
	CodeVerifier  string     `json:"-"`
	RedirectURI   string     `json:"redirect_uri"`
	AuthURL       string     `json:"auth_url"`
	Label         string     `json:"label"`
	TargetScope   string     `json:"target_scope"`
	Status        string     `json:"status"`
	Error         string     `json:"error,omitempty"`
	ConnectionID  *uuid.UUID `json:"connection_id,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	ExpiresAt     time.Time  `json:"expires_at"`
	CompletedAt   *time.Time `json:"completed_at,omitempty"`
}

// WebSession is a refresh-token session for the web UI.
type WebSession struct {
	ID        uuid.UUID  `json:"id"`
	UserID    uuid.UUID  `json:"user_id"`
	UserAgent string     `json:"user_agent"`
	IP        string     `json:"ip"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// UsageTotals aggregates usage over a period.
type UsageTotals struct {
	Requests         int64 `json:"requests"`
	Errors           int64 `json:"errors"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

// UsageBucket is one point on a usage time series.
type UsageBucket struct {
	Bucket time.Time `json:"bucket"`
	UsageTotals
}

// UsageBreakdown groups usage by an arbitrary key (model or provider).
type UsageBreakdown struct {
	Key string `json:"key"`
	UsageTotals
}
