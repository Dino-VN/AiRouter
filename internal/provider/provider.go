// Package provider implements the OAuth flows, token refresh and quota lookups
// for every supported upstream (Codex and Antigravity).
package provider

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"aihub/internal/config"
	"aihub/internal/model"
	"aihub/internal/store"
)

// AuthOptions parameterises the start of an OAuth flow.
type AuthOptions struct {
	// RedirectURI overrides the provider default. Both vendors only accept
	// loopback redirects, so this is normally left empty.
	RedirectURI string
	// State is the CSRF token; generated when empty.
	State string
}

// AuthRequest is what the UI needs to walk a user through consent.
type AuthRequest struct {
	AuthURL      string
	State        string
	CodeVerifier string
	RedirectURI  string
	// Instructions explains how to hand the callback back to the server.
	Instructions string
	// TTL is how long the attempt stays valid.
	TTL time.Duration
}

// AuthResult is the outcome of a code exchange or a refresh.
type AuthResult struct {
	Credential   *model.Credential
	AccountEmail string
	AccountID    string
	Plan         string
	ProjectID    string
	Metadata     map[string]any
}

// Provider is one upstream account type.
type Provider interface {
	// ID is the stable provider key.
	ID() model.Provider
	// DisplayName is shown in the UI.
	DisplayName() string
	// LoopbackPort is the port the vendor OAuth client redirects to, or 0.
	LoopbackPort() int
	// CallbackPath is the path component of the loopback redirect.
	CallbackPath() string
	// BeginAuth builds the consent URL.
	BeginAuth(ctx context.Context, opts AuthOptions) (*AuthRequest, error)
	// CompleteAuth exchanges an authorization code for a credential.
	CompleteAuth(ctx context.Context, sess *model.OAuthSession, code string) (*AuthResult, error)
	// Refresh renews an access token from a refresh token.
	Refresh(ctx context.Context, cred *model.Credential) (*AuthResult, error)
	// FetchQuota reads the provider-reported quota for a connection.
	FetchQuota(ctx context.Context, conn *model.Connection) (*model.UpstreamQuota, error)
}

// Registry resolves providers and owns the shared HTTP client, model catalog and
// token refresher.
type Registry struct {
	providers map[model.Provider]Provider
	catalog   *Catalog
	tokens    *TokenManager
	http      *http.Client
	log       *slog.Logger
}

// NewRegistry builds the registry for every supported provider.
func NewRegistry(cfg *config.Config, st *store.Store, logger *slog.Logger) (*Registry, error) {
	client, err := newHTTPClient(cfg.ProxyURL, cfg.RequestTimeout)
	if err != nil {
		return nil, err
	}
	// OAuth and metadata calls are short; streaming uses its own client.
	authClient, err := newHTTPClient(cfg.ProxyURL, 90*time.Second)
	if err != nil {
		return nil, err
	}

	r := &Registry{
		providers: map[model.Provider]Provider{},
		catalog:   NewCatalog(authClient, logger),
		http:      client,
		log:       logger,
	}
	r.providers[model.ProviderCodex] = newCodex(authClient, logger)
	r.providers[model.ProviderAntigravity] = newAntigravity(authClient, logger)
	r.providers[model.ProviderOpenAI] = newOpenAIProvider(authClient, logger)
	r.tokens = newTokenManager(r, st, logger)
	return r, nil
}

// Get returns a provider by id.
func (r *Registry) Get(id model.Provider) (Provider, error) {
	p, ok := r.providers[id]
	if !ok {
		return nil, fmt.Errorf("unsupported provider %q", id)
	}
	return p, nil
}

// All returns every provider, in a stable order.
func (r *Registry) All() []Provider {
	out := make([]Provider, 0, len(r.providers))
	for _, id := range model.Providers() {
		if p, ok := r.providers[id]; ok {
			out = append(out, p)
		}
	}
	return out
}

// Catalog exposes the model catalog.
func (r *Registry) Catalog() *Catalog { return r.catalog }

// Tokens exposes the credential refresher.
func (r *Registry) Tokens() *TokenManager { return r.tokens }

// HTTPClient is the client used for upstream API traffic.
func (r *Registry) HTTPClient() *http.Client { return r.http }

// newHTTPClient builds a client that optionally routes through a proxy.
func newHTTPClient(proxyURL string, timeout time.Duration) (*http.Client, error) {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: time.Second,
		ForceAttemptHTTP2:     true,
	}
	if proxyURL != "" {
		parsed, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("parse AIHUB_PROXY_URL: %w", err)
		}
		transport.Proxy = http.ProxyURL(parsed)
	}
	return &http.Client{Transport: transport, Timeout: timeout}, nil
}
