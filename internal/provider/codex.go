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
	codexRedirectURI = "http://localhost:1455/auth/callback"
	codexScope       = "openid email profile offline_access"

	// codexClientID is resolved at call time from public_creds.go so
	// operators can override it via AIHUB_CODEX_OAUTH_CLIENT_ID (e.g.
	// to point Codex at an internal mirror without forking the binary).
	// The default is the public OpenAI Auth0 client id the Codex CLI
	// itself ships — the same value OmniRoute embeds.

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
		"client_id":                  {codexOAuthClientID()},
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
		"client_id":     {codexOAuthClientID()},
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
		"client_id":     {codexOAuthClientID()},
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

// codexUsageURL is the ChatGPT backend endpoint the Codex CLI itself hits
// to read its rate-limit windows. The response shape is documented at
// CodexUsagePayload below; the per-window fields are mirrored from
// CLIProxyAPI's parser so the operator sees the same numbers the
// upstream CLI would.
const codexUsageURL = "https://chatgpt.com/backend-api/wham/usage"

// codexUserAgent identifies this client to the wham/usage endpoint. The
// Codex CLI itself sends "codex_cli_rs/<version> (...)" and the backend
// refuses unknown originators.
const codexUsageUserAgent = "codex_cli_rs/0.51.0 (Linux 6.1.0; x86_64) aihub"

func (p *codexProvider) FetchQuota(ctx context.Context, conn *model.Connection) (*model.UpstreamQuota, error) {
	// Always start from whatever windows were already captured on proxied
	// responses (the x-codex-primary-* / x-codex-secondary-* headers), so a
	// failure to reach wham/usage does not blank out a connection that
	// has just served a request and therefore has fresh numbers.
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

	// Actively poll the wham/usage endpoint so an operator's "refresh
	// quota" click produces fresh numbers even when the connection has
	// not just served a request. The call is bounded by the caller's
	// request context; failure falls through to whatever headers were
	// captured previously.
	if conn.Credential != nil && conn.Credential.AccessToken != "" {
		if usage, err := p.fetchCodexUsage(ctx, conn); err == nil && usage != nil {
			if usage.Plan != "" {
				quota.Plan = usage.Plan
			}
			// Replace the snapshot rather than merging: the upstream's
			// view is authoritative, and stale headers from before a
			// plan upgrade should not survive a successful wham/usage
			// fetch.
			if len(usage.Windows) > 0 {
				quota.Windows = usage.Windows
			}
			quota.Note = usage.Note
		}
	}

	if len(quota.Windows) == 0 {
		quota.Note = "ChatGPT's wham/usage endpoint is not reachable from " +
			"this server, and no x-codex-primary-* / x-codex-secondary-* " +
			"headers have been captured on a proxied completion yet. Send " +
			"one request through this connection and refresh; the response " +
			"headers populate the windows here."
	}
	return quota, nil
}

// codexUsageQuota is the parsed shape of wham/usage that this provider
// needs to surface back to the caller. The wire shape carries more
// (rate_limit_reset_credits, code_review_rate_limit, additional_rate_limits)
// than the operator-facing UI shows today; only the windows the UI
// already understands are extracted here.
type codexUsageQuota struct {
	Plan    string
	Windows []model.QuotaWindow
	Note    string
}

// fetchCodexUsage calls GET https://chatgpt.com/backend-api/wham/usage
// with the account's access token and parses its rate-limit windows.
//
// The endpoint reports:
//   - rate_limit.primary_window: 5-hour rolling window (used_percent, limit_window_seconds, reset_after_seconds, reset_at)
//   - rate_limit.secondary_window: weekly window for paid plans, monthly for the free tier
//   - code_review_rate_limit: the same shape, but for the code review tool
//   - additional_rate_limits: per-feature rate limits (e.g. image generation)
//   - rate_limit_reset_credits: top-up credits the operator can spend
//
// We mirror the classification logic from CLIProxyAPI's parser so the
// numbers shown here match what the Codex CLI itself displays.
func (p *codexProvider) fetchCodexUsage(ctx context.Context, conn *model.Connection) (*codexUsageQuota, error) {
	if conn.Credential == nil || conn.Credential.AccessToken == "" {
		return nil, fmt.Errorf("codex: connection has no access token")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codexUsageURL, nil)
	if err != nil {
		return nil, fmt.Errorf("codex usage request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+conn.Credential.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", codexUsageUserAgent)
	if conn.AccountID != "" {
		req.Header.Set("Chatgpt-Account-Id", conn.AccountID)
	}

	resp, err := p.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("codex usage request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("codex usage response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("codex usage endpoint returned %d: %s",
			resp.StatusCode, truncateForError(string(body)))
	}

	return parseCodexUsageBody(body)
}

// parseCodexUsageBody extracts the rate-limit windows from a wham/usage
// response. The shape mirrors CLIProxyAPI's CodexUsagePayload.
func parseCodexUsageBody(body []byte) (*codexUsageQuota, error) {
	var raw struct {
		PlanType             string                      `json:"plan_type"`
		RateLimit            *codexUsageRateLimit        `json:"rate_limit,omitempty"`
		CodeReviewRateLimit  *codexUsageRateLimit        `json:"code_review_rate_limit,omitempty"`
		AdditionalRateLimits []codexUsageAdditionalLimit `json:"additional_rate_limits,omitempty"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode codex usage: %w", err)
	}

	out := &codexUsageQuota{Plan: raw.PlanType}
	now := time.Now()
	const fiveHourSeconds int64 = 18000
	const weekSeconds int64 = 604800
	const minMonth int64 = 28 * 24 * 60 * 60
	const maxMonth int64 = 31 * 24 * 60 * 60

	add := func(name string, w *codexUsageWindow, limitReached, allowed *bool) {
		if w == nil {
			return
		}
		win := model.QuotaWindow{Name: name}
		if w.UsedPercent != nil {
			win.UsedPercent = *w.UsedPercent
		} else if (limitReached != nil && *limitReached) || (allowed != nil && !*allowed) {
			win.UsedPercent = 100
		}
		if w.LimitWindowSeconds != nil {
			win.WindowMinutes = int(*w.LimitWindowSeconds / 60)
		}
		if w.ResetAfterSeconds != nil && *w.ResetAfterSeconds > 0 {
			win.ResetsInSeconds = *w.ResetAfterSeconds
			resetsAt := now.Add(time.Duration(*w.ResetAfterSeconds) * time.Second)
			win.ResetsAt = &resetsAt
		}
		if w.ResetAt != nil && !w.ResetAt.IsZero() {
			win.ResetsAt = w.ResetAt
			if win.ResetsInSeconds == 0 && w.ResetAt.After(now) {
				win.ResetsInSeconds = int64(w.ResetAt.Sub(now).Seconds())
			}
		}
		out.Windows = append(out.Windows, win)
	}

	// Primary window is the 5-hour rolling window; secondary is weekly
	// (paid) or monthly (free). Classification uses limit_window_seconds
	// when present, falling back to positional primary/secondary ordering
	// for older payloads.
	pickClassified := func(limit *codexUsageRateLimit, name5h, nameWeek, nameMonth string) {
		if limit == nil {
			return
		}
		var fiveHour, weekly *codexUsageWindow
		for _, w := range []*codexUsageWindow{limit.PrimaryWindow, limit.SecondaryWindow} {
			if w == nil || w.LimitWindowSeconds == nil {
				continue
			}
			secs := *w.LimitWindowSeconds
			if secs == fiveHourSeconds && fiveHour == nil {
				fiveHour = w
			} else if (secs == weekSeconds || (secs >= minMonth && secs <= maxMonth)) && weekly == nil {
				weekly = w
			}
		}
		if fiveHour == nil {
			fiveHour = limit.PrimaryWindow
		}
		if weekly == nil {
			weekly = limit.SecondaryWindow
		}
		add(name5h, fiveHour, limit.LimitReached, limit.Allowed)
		weeklyName := nameWeek
		if weekly != nil && weekly.LimitWindowSeconds != nil {
			secs := *weekly.LimitWindowSeconds
			if secs >= minMonth && secs <= maxMonth {
				weeklyName = nameMonth
			}
		}
		add(weeklyName, weekly, limit.LimitReached, limit.Allowed)
	}

	pickClassified(raw.RateLimit, "primary", "secondary", "monthly")
	pickClassified(raw.CodeReviewRateLimit, "code-review-primary", "code-review-secondary", "code-review-monthly")
	for _, extra := range raw.AdditionalRateLimits {
		name := strings.TrimSpace(extra.LimitName)
		if name == "" {
			name = strings.TrimSpace(extra.MeteredFeature)
		}
		if name == "" {
			name = "additional"
		}
		pickClassified(extra.RateLimit, name+"-primary", name+"-secondary", name+"-monthly")
	}

	return out, nil
}

// codexUsageRateLimit is one rate-limit group inside the wham/usage
// response. Fields are pointer-typed so a missing key is distinguishable
// from a zero value (a 0% used window is a perfectly valid report).
type codexUsageRateLimit struct {
	Allowed         *bool             `json:"allowed,omitempty"`
	LimitReached    *bool             `json:"limit_reached,omitempty"`
	PrimaryWindow   *codexUsageWindow `json:"primary_window,omitempty"`
	SecondaryWindow *codexUsageWindow `json:"secondary_window,omitempty"`
}

// codexUsageWindow is one window inside a rate-limit group.
type codexUsageWindow struct {
	UsedPercent        *float64   `json:"used_percent,omitempty"`
	LimitWindowSeconds *int64     `json:"limit_window_seconds,omitempty"`
	ResetAfterSeconds  *int64     `json:"reset_after_seconds,omitempty"`
	ResetAt            *time.Time `json:"reset_at,omitempty"`
}

// codexUsageAdditionalLimit is one entry in additional_rate_limits.
type codexUsageAdditionalLimit struct {
	LimitName      string               `json:"limit_name,omitempty"`
	MeteredFeature string               `json:"metered_feature,omitempty"`
	RateLimit      *codexUsageRateLimit `json:"rate_limit,omitempty"`
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
