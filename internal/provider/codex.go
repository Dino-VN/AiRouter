package provider

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"aihub/internal/model"
)

// Codex OAuth endpoints and the client the ChatGPT desktop/CLI tooling uses.
const (
	codexAuthURL     = "https://auth.openai.com/oauth/authorize"
	codexTokenURL    = "https://auth.openai.com/oauth/token"
	codexClientID    = "app_EMoamEEZ73f0CkXaXp7hrann"
	codexRedirectURI = "http://localhost:1455/auth/callback"
	codexScope       = "openid email profile offline_access"

	// codexLoopbackPort is the only port the OAuth client will redirect to.
	codexLoopbackPort  = 1455
	codexCallbackPath  = "/auth/callback"
	codexAuthClaimsKey = "https://api.openai.com/auth"
)

// ErrCredentialRevoked marks a refresh failure that retrying cannot fix; the
// connection needs a fresh interactive login.
var ErrCredentialRevoked = errors.New("provider: credential revoked, re-authentication required")

type codexProvider struct {
	http *http.Client
	log  *slog.Logger
}

func newCodex(client *http.Client, logger *slog.Logger) *codexProvider {
	return &codexProvider{http: client, log: logger}
}

func (p *codexProvider) ID() model.Provider   { return model.ProviderCodex }
func (p *codexProvider) DisplayName() string  { return "Codex (ChatGPT)" }
func (p *codexProvider) LoopbackPort() int    { return codexLoopbackPort }
func (p *codexProvider) CallbackPath() string { return codexCallbackPath }

func (p *codexProvider) BeginAuth(_ context.Context, opts AuthOptions) (*AuthRequest, error) {
	state := opts.State
	if state == "" {
		var err error
		if state, err = randomToken(32); err != nil {
			return nil, err
		}
	}
	verifier, err := randomToken(96)
	if err != nil {
		return nil, err
	}
	redirect := opts.RedirectURI
	if redirect == "" {
		redirect = codexRedirectURI
	}

	challenge := sha256.Sum256([]byte(verifier))
	params := url.Values{
		"client_id":                  {codexClientID},
		"response_type":              {"code"},
		"redirect_uri":               {redirect},
		"scope":                      {codexScope},
		"state":                      {state},
		"code_challenge":             {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method":      {"S256"},
		"prompt":                     {"login"},
		"id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow":  {"true"},
	}

	return &AuthRequest{
		AuthURL:      codexAuthURL + "?" + params.Encode(),
		State:        state,
		CodeVerifier: verifier,
		RedirectURI:  redirect,
		TTL:          15 * time.Minute,
		Instructions: "Sign in to ChatGPT in the opened tab. The browser will be redirected to " +
			"http://localhost:1455/auth/callback?code=... — copy that full URL (even if the page " +
			"fails to load) and paste it back here.",
	}, nil
}

func (p *codexProvider) CompleteAuth(ctx context.Context, sess *model.OAuthSession, code string) (*AuthResult, error) {
	redirect := sess.RedirectURI
	if redirect == "" {
		redirect = codexRedirectURI
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {codexClientID},
		"code":          {code},
		"redirect_uri":  {redirect},
		"code_verifier": {sess.CodeVerifier},
	}

	tok, err := p.token(ctx, form)
	if err != nil {
		return nil, err
	}
	return p.result(tok)
}

func (p *codexProvider) Refresh(ctx context.Context, cred *model.Credential) (*AuthResult, error) {
	if cred == nil || cred.RefreshToken == "" {
		return nil, fmt.Errorf("%w: no refresh token stored", ErrCredentialRevoked)
	}
	form := url.Values{
		"client_id":     {codexClientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {cred.RefreshToken},
		"scope":         {"openid profile email"},
	}

	tok, err := p.token(ctx, form)
	if err != nil {
		return nil, err
	}
	// A refresh response may omit the refresh token; keep the previous one.
	if tok.RefreshToken == "" {
		tok.RefreshToken = cred.RefreshToken
	}
	if tok.IDToken == "" {
		tok.IDToken = cred.IDToken
	}
	return p.result(tok)
}

func (p *codexProvider) FetchQuota(_ context.Context, conn *model.Connection) (*model.UpstreamQuota, error) {
	// ChatGPT reports Codex usage only in the response headers of a real
	// completion call, so there is no quota endpoint to poll. Keep whatever
	// the last proxied request observed (so the UI continues to show rate
	// limit windows when they exist), and refresh the plan from the ID token
	// — that part is always up to date.
	quota := &model.UpstreamQuota{UpdatedAt: time.Now()}
	if conn.Quota != nil {
		quota.Windows = conn.Quota.Windows
	}
	quota.Plan = conn.Plan
	if conn.Credential != nil && conn.Credential.IDToken != "" {
		if info, err := parseCodexIDToken(conn.Credential.IDToken); err == nil && info.Plan != "" {
			quota.Plan = info.Plan
		}
	}
	if len(quota.Windows) == 0 {
		// Make the "no data" state self-explanatory in the UI rather than
		// looking like a bug. Operators reading this know exactly what to do:
		// route a single chat completion through this connection, and the
		// response headers populate the windows on the next refresh.
		quota.Note = "ChatGPT does not expose a Codex quota endpoint. The " +
			"x-codex-primary-* and x-codex-secondary-* rate-limit headers " +
			"are captured on every proxied completion and shown here once " +
			"this connection has served at least one request."
	} else {
		// Explain where the windows came from so the operator can tell a
		// stale snapshot (e.g. headers from before a plan upgrade) apart
		// from a fresh one.
		quota.Note = "Captured from response headers of the last proxied " +
			"completion. ChatGPT does not expose a Codex quota endpoint, " +
			"so these windows are only as fresh as the most recent request."
	}
	return quota, nil
}

// tokenResponse is the shape of both the code exchange and the refresh response.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
}

func (p *codexProvider) token(ctx context.Context, form url.Values) (*tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexTokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("codex token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := p.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("codex token request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("codex token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		detail := strings.TrimSpace(string(body))
		// The refresh token has been rotated away or revoked; retrying is futile.
		if strings.Contains(detail, "refresh_token_reused") ||
			strings.Contains(detail, "invalid_grant") ||
			resp.StatusCode == http.StatusUnauthorized {
			return nil, fmt.Errorf("%w: codex token endpoint %d: %s",
				ErrCredentialRevoked, resp.StatusCode, truncateForError(detail))
		}
		return nil, fmt.Errorf("codex token endpoint %d: %s", resp.StatusCode, truncateForError(detail))
	}

	var tok tokenResponse
	if err = json.Unmarshal(body, &tok); err != nil {
		return nil, fmt.Errorf("decode codex token response: %w", err)
	}
	if tok.AccessToken == "" {
		return nil, fmt.Errorf("codex token response contained no access_token")
	}
	return &tok, nil
}

func (p *codexProvider) result(tok *tokenResponse) (*AuthResult, error) {
	expiresAt := time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	if tok.ExpiresIn <= 0 {
		// ChatGPT access tokens last an hour; assume the shorter side.
		expiresAt = time.Now().Add(50 * time.Minute)
	}

	res := &AuthResult{
		Credential: &model.Credential{
			AccessToken:  tok.AccessToken,
			RefreshToken: tok.RefreshToken,
			IDToken:      tok.IDToken,
			ExpiresAt:    expiresAt,
			LastRefresh:  time.Now(),
		},
		Metadata: map[string]any{},
	}

	if tok.IDToken != "" {
		info, err := parseCodexIDToken(tok.IDToken)
		if err != nil {
			p.log.Warn("codex: could not parse id_token", "error", err)
		} else {
			res.AccountEmail = info.Email
			res.AccountID = info.AccountID
			res.Plan = info.Plan
			if info.UserID != "" {
				res.Metadata["chatgpt_user_id"] = info.UserID
			}
			if len(info.Organizations) > 0 {
				res.Metadata["organizations"] = info.Organizations
			}
		}
	}
	return res, nil
}

// codexIDTokenInfo is the subset of the ID token claims that matters.
type codexIDTokenInfo struct {
	Email         string
	AccountID     string
	Plan          string
	UserID        string
	Organizations []string
}

// parseCodexIDToken reads the account facts out of the ID token. The token was
// just received over TLS from the issuer, so the claims are read without
// verifying the signature.
func parseCodexIDToken(idToken string) (codexIDTokenInfo, error) {
	var info codexIDTokenInfo

	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return info, fmt.Errorf("id_token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return info, fmt.Errorf("decode id_token payload: %w", err)
	}

	var claims struct {
		Email             string `json:"email"`
		PreferredUsername string `json:"preferred_username"`
		Auth              struct {
			ChatGPTAccountID string `json:"chatgpt_account_id"`
			ChatGPTPlanType  string `json:"chatgpt_plan_type"`
			ChatGPTUserID    string `json:"chatgpt_user_id"`
			Organizations    []struct {
				ID    string `json:"id"`
				Role  string `json:"role"`
				Title string `json:"title"`
			} `json:"organizations"`
		} `json:"https://api.openai.com/auth"`
	}
	if err = json.Unmarshal(payload, &claims); err != nil {
		return info, fmt.Errorf("decode id_token claims: %w", err)
	}

	info.Email = claims.Email
	if info.Email == "" {
		info.Email = claims.PreferredUsername
	}
	info.AccountID = claims.Auth.ChatGPTAccountID
	info.Plan = claims.Auth.ChatGPTPlanType
	info.UserID = claims.Auth.ChatGPTUserID
	for _, org := range claims.Auth.Organizations {
		label := org.Title
		if label == "" {
			label = org.ID
		}
		info.Organizations = append(info.Organizations, label)
	}
	return info, nil
}

// Rate-limit headers ChatGPT attaches to Codex completion responses.
const (
	codexHeaderPrimaryUsed    = "x-codex-primary-used-percent"
	codexHeaderPrimaryWindow  = "x-codex-primary-window-minutes"
	codexHeaderPrimaryReset   = "x-codex-primary-reset-after-seconds"
	codexHeaderSecondaryUsed  = "x-codex-secondary-used-percent"
	codexHeaderSecondaryWin   = "x-codex-secondary-window-minutes"
	codexHeaderSecondaryReset = "x-codex-secondary-reset-after-seconds"
)

// CodexQuotaFromHeaders extracts the usage windows ChatGPT reports on a Codex
// response. It returns nil when the response carried no rate-limit headers.
func CodexQuotaFromHeaders(header http.Header, plan string) *model.UpstreamQuota {
	windows := make([]model.QuotaWindow, 0, 2)
	now := time.Now()

	add := func(name, usedKey, windowKey, resetKey string) {
		raw := header.Get(usedKey)
		if raw == "" {
			return
		}
		used, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return
		}
		w := model.QuotaWindow{Name: name, UsedPercent: used}
		if minutes, err := strconv.Atoi(header.Get(windowKey)); err == nil {
			w.WindowMinutes = minutes
		}
		if secs, err := strconv.ParseInt(header.Get(resetKey), 10, 64); err == nil && secs > 0 {
			w.ResetsInSeconds = secs
			resetsAt := now.Add(time.Duration(secs) * time.Second)
			w.ResetsAt = &resetsAt
		}
		windows = append(windows, w)
	}

	add("primary", codexHeaderPrimaryUsed, codexHeaderPrimaryWindow, codexHeaderPrimaryReset)
	add("secondary", codexHeaderSecondaryUsed, codexHeaderSecondaryWin, codexHeaderSecondaryReset)

	if len(windows) == 0 {
		return nil
	}
	return &model.UpstreamQuota{Plan: plan, Windows: windows, UpdatedAt: now}
}

// randomToken returns n random bytes encoded as unpadded base64url.
func randomToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("random token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func truncateForError(s string) string {
	const limit = 400
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}
