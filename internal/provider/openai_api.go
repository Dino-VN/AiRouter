package provider

// openai_api.go — OpenAI-compatible API provider
//
// Unlike Codex and Antigravity, this provider does not run an OAuth flow.
// Operators register an API key alongside the base URL (and a model list
// they want to expose); the proxy forwards chat-completions and responses
// requests verbatim. This makes any OpenAI-compatible endpoint —
// https://api.openai.com/v1, Azure OpenAI, OpenRouter, vLLM, LocalAI,
// Ollama's OpenAI shim — addressable from the same UI as the OAuth-backed
// providers.
//
// The credential is the API key. There is no refresh: the access token is
// always "valid" until the operator rotates the key. Refresh() is a no-op
// that returns the stored credential, so the TokenManager's hot path does
// not need a special case.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"aihub/internal/model"
)

// ErrNoAPIKey signals that a connection has no API key. The error is wrapped
// by callers so a missing credential surfaces as a 5xx rather than silently
// forwarding a request with no Authorization header.
var ErrNoAPIKey = errors.New("provider: connection has no API key")

// openaiAPIProvider implements Provider for an API-key based OpenAI-compatible
// endpoint. It is the only provider in this package that does not honour the
// OAuth portion of the interface; BeginAuth/CompleteAuth return a clear error
// so the UI can branch on IsOAuth rather than discovering the unsupported
// path at runtime.
type openaiAPIProvider struct {
	http *http.Client
	log  *slog.Logger
}

func newOpenAIProvider(client *http.Client, logger *slog.Logger) *openaiAPIProvider {
	return &openaiAPIProvider{http: client, log: logger}
}

func (p *openaiAPIProvider) ID() model.Provider  { return model.ProviderOpenAI }
func (p *openaiAPIProvider) DisplayName() string { return "OpenAI API" }

// LoopbackPort and CallbackPath are not applicable to API-key providers. They
// return zero values so the registry's OAuth loopback listeners are not opened
// for this provider.
func (p *openaiAPIProvider) LoopbackPort() int    { return 0 }
func (p *openaiAPIProvider) CallbackPath() string { return "" }

// BeginAuth is unsupported: API-key providers have no consent screen. The UI
// must skip the OAuth route for providers where model.Provider.IsOAuth()
// returns false; calling this method is a programming error and the error
// makes that obvious rather than returning an empty AuthRequest.
func (p *openaiAPIProvider) BeginAuth(context.Context, AuthOptions) (*AuthRequest, error) {
	return nil, fmt.Errorf("openai-api: provider does not use OAuth; create a connection via the API-key endpoint")
}

func (p *openaiAPIProvider) CompleteAuth(context.Context, *model.OAuthSession, string) (*AuthResult, error) {
	return nil, fmt.Errorf("openai-api: provider does not use OAuth")
}

// Refresh is a no-op: the API key never expires. Returning the stored
// credential keeps the TokenManager's hot path uniform — callers do not need
// to special-case API-key providers before they call Refresh.
func (p *openaiAPIProvider) Refresh(_ context.Context, cred *model.Credential) (*AuthResult, error) {
	if cred == nil || cred.AccessToken == "" {
		return nil, fmt.Errorf("%w: openai-api connection has no API key stored", ErrNoAPIKey)
	}
	return &AuthResult{Credential: cred}, nil
}

// FetchQuota reports what an OpenAI-compatible endpoint exposes. OpenAI itself
// does not publish a usage endpoint accessible with a project API key, so the
// quota is the plan string (when the operator set one) plus a short note
// explaining where usage lives. For self-hosted gateways the operator can
// override the note by setting Metadata["quota_note"] at connection creation.
func (p *openaiAPIProvider) FetchQuota(_ context.Context, conn *model.Connection) (*model.UpstreamQuota, error) {
	if conn == nil {
		return nil, fmt.Errorf("openai-api: nil connection")
	}
	if conn.Credential == nil || conn.Credential.AccessToken == "" {
		return nil, fmt.Errorf("%w: openai-api connection %s", ErrNoAPIKey, conn.Label)
	}
	quota := &model.UpstreamQuota{Plan: conn.Plan, UpdatedAt: time.Now()}
	if conn.Quota != nil {
		// Preserve windows previously captured from response headers (some
		// OpenAI-compatible gateways emit x-ratelimit-* headers).
		quota.Windows = conn.Quota.Windows
	}
	if note, _ := conn.Metadata["quota_note"].(string); note != "" {
		quota.Note = note
	} else if len(quota.Windows) == 0 {
		quota.Note = "OpenAI-compatible APIs do not publish a usage endpoint. " +
			"Usage is recorded by this proxy; visit the Usage tab for per-key totals. " +
			"Rate-limit windows populate automatically when the upstream emits " +
			"x-ratelimit-* headers on a proxied request."
	}
	return quota, nil
}

// UserAgent exposes a stable User-Agent for this provider's upstream calls.
// The proxy layer reads it via the userAgentProvider interface hook used by
// the Antigravity executor.
func (p *openaiAPIProvider) UserAgent(_ context.Context) string {
	return "aihub-openai-api/1.0 (+https://github.com/Dino-VN/AiRouter)"
}

// openAIAPIBaseURL extracts the operator-configured base URL from a
// connection's metadata. The fallback is the canonical OpenAI endpoint so a
// connection created without metadata still talks to https://api.openai.com/v1.
// A trailing slash is removed here so the executor can append "/chat/completions"
// without producing a doubled separator.
func OpenAIAPIBaseURL(conn *model.Connection) string {
	return openAIAPIBaseURL(conn)
}

func openAIAPIBaseURL(conn *model.Connection) string {
	if conn == nil {
		return "https://api.openai.com/v1"
	}
	if raw, _ := conn.Metadata["base_url"].(string); raw != "" {
		return strings.TrimRight(raw, "/")
	}
	return "https://api.openai.com/v1"
}

// OpenAIAPIModels returns the operator-configured model list for an OpenAI
// API connection. When unset, returns nil — the catalog then falls back to a
// built-in list. Both branches let operators expose a curated subset (e.g.
// only the models their gateway actually serves) without editing code.
func OpenAIAPIModels(conn *model.Connection) []string {
	return openAIAPIModels(conn)
}

func openAIAPIModels(conn *model.Connection) []string {
	if conn == nil {
		return nil
	}
	raw, _ := conn.Metadata["models"].([]any)
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if id, ok := item.(string); ok && strings.TrimSpace(id) != "" {
			out = append(out, strings.TrimSpace(id))
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// OpenAIAPIExtraHeaders pulls operator-configured extra headers (e.g.
// "OpenAI-Beta: assistants=v2", "Helicone-Auth: ...") out of a connection's
// metadata map. The executor adds them to every upstream call so the same
// connection can target any OpenAI-compatible gateway that expects vendor
// headers beyond Authorization.
func OpenAIAPIExtraHeaders(conn *model.Connection) http.Header {
	return openAIAPIExtraHeaders(conn)
}

func openAIAPIExtraHeaders(conn *model.Connection) http.Header {
	out := http.Header{}
	if conn == nil || conn.Metadata == nil {
		return out
	}
	raw, _ := conn.Metadata["extra_headers"].(map[string]any)
	for k, v := range raw {
		switch typed := v.(type) {
		case string:
			out.Set(k, typed)
		case []any:
			for _, item := range typed {
				if s, ok := item.(string); ok {
					out.Add(k, s)
				}
			}
		}
	}
	return out
}
