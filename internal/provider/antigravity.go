package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"aihub/internal/model"
)

// Antigravity reuses Google's installed-application OAuth client shipped with
// the Antigravity IDE, so the redirect must stay on its fixed loopback port.
// The client_id and client_secret are public values embedded in the Antigravity
// IDE binary; they are resolved from internal/provider/public_creds.go so
// operators can override them via AIHUB_ANTIGRAVITY_OAUTH_CLIENT_ID and
// AIHUB_ANTIGRAVITY_OAUTH_CLIENT_SECRET (e.g. to point Antigravity at an
// internal mirror without forking the binary).
const (
	antigravityLoopbackPort = 51121
	antigravityCallbackPath = "/oauth-callback"
	antigravityRedirectURI  = "http://localhost:51121/oauth-callback"

	antigravityAuthEndpoint     = "https://accounts.google.com/o/oauth2/v2/auth"
	antigravityTokenEndpoint    = "https://oauth2.googleapis.com/token"
	antigravityUserInfoEndpoint = "https://www.googleapis.com/oauth2/v2/userinfo?alt=json"

	// AntigravityAPIEndpoint serves completions; the daily endpoint handles
	// onboarding.
	AntigravityAPIEndpoint      = "https://cloudcode-pa.googleapis.com"
	antigravityDailyAPIEndpoint = "https://daily-cloudcode-pa.googleapis.com"
	antigravityAPIVersion       = "v1internal"

	// antigravityFallbackVersion is used when the live IDE version cannot be
	// resolved; it only affects the User-Agent string.
	antigravityFallbackVersion = "2.2.1"
	antigravityVersionManifest = "https://antigravity-hub-auto-updater-974169037036.us-central1.run.app/manifest/latest-arm64-mac.yml"
	antigravityNodeAPIClient   = "gl-node/22.21.1"
)

// antigravityScopes are the scopes the IDE requests.
var antigravityScopes = []string{
	"https://www.googleapis.com/auth/cloud-platform",
	"https://www.googleapis.com/auth/userinfo.email",
	"https://www.googleapis.com/auth/userinfo.profile",
	"https://www.googleapis.com/auth/cclog",
	"https://www.googleapis.com/auth/experimentsandconfigs",
}

type antigravityProvider struct {
	http *http.Client
	log  *slog.Logger

	versionMu      sync.Mutex
	version        string
	versionFetched time.Time
}

func newAntigravity(client *http.Client, logger *slog.Logger) *antigravityProvider {
	return &antigravityProvider{http: client, log: logger, version: antigravityFallbackVersion}
}

func (p *antigravityProvider) ID() model.Provider   { return model.ProviderAntigravity }
func (p *antigravityProvider) DisplayName() string  { return "Antigravity (Google)" }
func (p *antigravityProvider) LoopbackPort() int    { return antigravityLoopbackPort }
func (p *antigravityProvider) CallbackPath() string { return antigravityCallbackPath }

func (p *antigravityProvider) BeginAuth(_ context.Context, opts AuthOptions) (*AuthRequest, error) {
	state := opts.State
	if state == "" {
		var err error
		if state, err = randomToken(32); err != nil {
			return nil, err
		}
	}
	redirect := opts.RedirectURI
	if redirect == "" {
		redirect = antigravityRedirectURI
	}

	params := url.Values{
		"access_type":   {"offline"},
		"client_id":     {antigravityOAuthClientID()},
		"prompt":        {"consent"},
		"redirect_uri":  {redirect},
		"response_type": {"code"},
		"scope":         {strings.Join(antigravityScopes, " ")},
		"state":         {state},
	}

	return &AuthRequest{
		AuthURL:     antigravityAuthEndpoint + "?" + params.Encode(),
		State:       state,
		RedirectURI: redirect,
		TTL:         15 * time.Minute,
		Instructions: "Pick the Google account with Antigravity access. The browser will be " +
			"redirected to http://localhost:51121/oauth-callback?code=... — copy that full URL " +
			"(even if the page fails to load) and paste it back here.",
	}, nil
}

func (p *antigravityProvider) CompleteAuth(ctx context.Context, sess *model.OAuthSession, code string) (*AuthResult, error) {
	redirect := sess.RedirectURI
	if redirect == "" {
		redirect = antigravityRedirectURI
	}
	form := url.Values{
		"code":          {code},
		"client_id":     {antigravityOAuthClientID()},
		"client_secret": {antigravityOAuthClientSecret()},
		"redirect_uri":  {redirect},
		"grant_type":    {"authorization_code"},
	}

	tok, err := p.token(ctx, form)
	if err != nil {
		return nil, err
	}
	cred := credentialFromToken(tok, nil)

	res := &AuthResult{Credential: cred, Metadata: map[string]any{}}
	if email, err := p.userEmail(ctx, cred.AccessToken); err != nil {
		p.log.Warn("antigravity: userinfo lookup failed", "error", err)
	} else {
		res.AccountEmail = email
		res.AccountID = email
	}

	// Resolve (and if necessary provision) the Cloud AI Companion project the
	// completion API requires.
	info, err := p.loadCodeAssist(ctx, cred.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("antigravity: loadCodeAssist: %w", err)
	}
	res.ProjectID = info.projectID
	res.Plan = info.plan
	if res.ProjectID == "" {
		projectID, onboardErr := p.onboardUser(ctx, cred.AccessToken, info.tierID)
		if onboardErr != nil {
			return nil, fmt.Errorf("antigravity: onboardUser: %w", onboardErr)
		}
		res.ProjectID = projectID
	}
	if info.tierID != "" {
		res.Metadata["tier_id"] = info.tierID
	}
	return res, nil
}

func (p *antigravityProvider) Refresh(ctx context.Context, cred *model.Credential) (*AuthResult, error) {
	if cred == nil || cred.RefreshToken == "" {
		return nil, fmt.Errorf("%w: no refresh token stored", ErrCredentialRevoked)
	}
	form := url.Values{
		"client_id":     {antigravityOAuthClientID()},
		"client_secret": {antigravityOAuthClientSecret()},
		"refresh_token": {cred.RefreshToken},
		"grant_type":    {"refresh_token"},
	}

	tok, err := p.token(ctx, form)
	if err != nil {
		return nil, err
	}
	return &AuthResult{Credential: credentialFromToken(tok, cred)}, nil
}

func (p *antigravityProvider) FetchQuota(ctx context.Context, conn *model.Connection) (*model.UpstreamQuota, error) {
	if conn == nil {
		return nil, fmt.Errorf("antigravity: nil connection")
	}
	if conn.Credential == nil || conn.Credential.AccessToken == "" {
		return nil, fmt.Errorf("antigravity: connection %s has no access token; sign the account in again",
			conn.Label)
	}

	// The first call may hit a token that has been revoked or rotated since
	// the last refresh. A 401 from loadCodeAssist means the access token is
	// no good; refresh it once and retry so an operator's "refresh quota"
	// click succeeds even on a connection that has sat idle past its token
	// lifetime.
	info, err := p.loadCodeAssist(ctx, conn.Credential.AccessToken)
	if err != nil && errors.Is(err, ErrCredentialRevoked) && conn.Credential.RefreshToken != "" {
		refreshed, refreshErr := p.Refresh(ctx, conn.Credential)
		if refreshErr != nil {
			return nil, fmt.Errorf("antigravity: quota fetch failed (%w) and refresh also failed: %v", err, refreshErr)
		}
		conn.Credential = refreshed.Credential
		info, err = p.loadCodeAssist(ctx, refreshed.Credential.AccessToken)
	}
	if err != nil {
		return nil, err
	}

	quota := &model.UpstreamQuota{Plan: info.plan, UpdatedAt: time.Now()}
	if info.credits != nil {
		quota.Credits = info.credits
	}

	// Pull the per-window rate-limit summary. The endpoint is
	// /v1internal:retrieveUserQuotaSummary and it returns one group per
	// tier (e.g. "Gemini" and "Claude, ChatGPT" for paid plans) with a
	// list of buckets describing how much of each window is left.
	// Failure here degrades gracefully — the credit snapshot from
	// loadCodeAssist is still returned so the operator sees something.
	if summary, sErr := p.retrieveUserQuotaSummary(ctx, conn.Credential.AccessToken, info.projectID); sErr == nil && summary != nil {
		for _, g := range summary.groups {
			for _, b := range g.buckets {
				used := 100.0 - (b.remainingFraction * 100.0)
				if used < 0 {
					used = 0
				}
				if used > 100 {
					used = 100
				}
				window := model.QuotaWindow{
					Name:        g.displayName + " · " + b.label,
					UsedPercent: used,
				}
				if b.window != "" {
					window.Name = g.displayName + " · " + b.window
				}
				if b.resetTime != "" {
					if t, parseErr := time.Parse(time.RFC3339, b.resetTime); parseErr == nil {
						window.ResetsAt = &t
						if t.After(time.Now()) {
							window.ResetsInSeconds = int64(t.Sub(time.Now()).Seconds())
						}
					}
				}
				if b.window != "" {
					switch strings.ToLower(b.window) {
					case "5h", "five-hour", "five_hour":
						window.WindowMinutes = 5 * 60
					case "weekly", "week":
						window.WindowMinutes = 7 * 24 * 60
					}
				}
				quota.Windows = append(quota.Windows, window)
			}
		}
		if len(quota.Windows) == 0 {
			quota.Note = "Account is on a " + info.plan + " tier; the upstream did not report any rate-limit windows yet."
		}
	}
	if info.credits == nil && len(quota.Windows) == 0 {
		quota.Note = "This account has no Google One AI credit pool and no per-window rate-limit summary; usage falls back to the free tier."
	}
	return quota, nil
}

// userQuotaSummary is the parsed shape of /v1internal:retrieveUserQuotaSummary.
// The endpoint returns one group per tier (Gemini tier, Claude+ChatGPT tier,
// etc.) and each group carries a list of buckets describing how much of each
// rate-limit window is left. This shape mirrors CLIProxyAPI's
// AntigravityQuotaSummaryPayload so the numbers match what the official
// Antigravity CLI displays.
type userQuotaSummary struct {
	groups []userQuotaGroup
}

type userQuotaGroup struct {
	displayName string
	description string
	buckets     []userQuotaBucket
}

type userQuotaBucket struct {
	bucketID          string
	label             string
	window            string
	resetTime         string
	remainingFraction float64
	description       string
}

// retrieveUserQuotaSummary calls POST /v1internal:retrieveUserQuotaSummary
// with the project ID and returns the per-tier rate-limit windows. The call
// is bounded by the caller's request context; a failure returns nil rather
// than an error so the caller can fall back to whatever loadCodeAssist
// already provided.
func (p *antigravityProvider) retrieveUserQuotaSummary(ctx context.Context, accessToken, projectID string) (*userQuotaSummary, error) {
	if accessToken == "" {
		return nil, fmt.Errorf("antigravity: no access token")
	}
	if projectID == "" {
		return nil, fmt.Errorf("antigravity: no project id; cannot fetch quota summary")
	}

	payload, err := json.Marshal(map[string]any{"project": projectID})
	if err != nil {
		return nil, fmt.Errorf("marshal quota summary request: %w", err)
	}
	endpoint := fmt.Sprintf("%s/%s:retrieveUserQuotaSummary", AntigravityAPIEndpoint, antigravityAPIVersion)
	body, err := p.callAPI(ctx, endpoint, accessToken, payload, false)
	if err != nil {
		return nil, err
	}

	var raw struct {
		Groups []struct {
			DisplayName string `json:"displayName"`
			Display     string `json:"display_name"`
			Description string `json:"description"`
			Buckets     []struct {
				BucketID           string  `json:"bucketId"`
				BucketIDAlt        string  `json:"bucket_id"`
				DisplayName        string  `json:"displayName"`
				Display            string  `json:"display_name"`
				Window             string  `json:"window"`
				ResetTime          string  `json:"resetTime"`
				ResetTimeAlt       string  `json:"reset_time"`
				RemainingFraction  float64 `json:"remainingFraction"`
				RemainingFraction2 float64 `json:"remaining_fraction"`
				Description        string  `json:"description"`
			} `json:"buckets"`
		} `json:"groups"`
	}
	if err = json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode quota summary: %w", err)
	}

	out := &userQuotaSummary{}
	for _, g := range raw.Groups {
		group := userQuotaGroup{
			displayName: firstNonEmptyString(g.DisplayName, g.Display),
			description: g.Description,
		}
		if group.displayName == "" {
			group.displayName = "Quota"
		}
		for _, b := range g.Buckets {
			remaining := b.RemainingFraction
			if remaining == 0 {
				remaining = b.RemainingFraction2
			}
			bucket := userQuotaBucket{
				bucketID:          firstNonEmptyString(b.BucketID, b.BucketIDAlt),
				label:             firstNonEmptyString(b.DisplayName, b.Display, b.BucketID, b.BucketIDAlt),
				window:            b.Window,
				resetTime:         firstNonEmptyString(b.ResetTime, b.ResetTimeAlt),
				remainingFraction: remaining,
				description:       b.Description,
			}
			group.buckets = append(group.buckets, bucket)
		}
		out.groups = append(out.groups, group)
	}
	return out, nil
}

// firstNonEmptyString returns the first non-empty argument, or "" when all
// are empty. Mirrors firstNonEmpty from the proxy package but scoped to
// this file to avoid an import cycle.
func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// token performs a Google OAuth token call.
func (p *antigravityProvider) token(ctx context.Context, form url.Values) (*tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, antigravityTokenEndpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("antigravity token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := p.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("antigravity token request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("antigravity token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		detail := strings.TrimSpace(string(body))
		if strings.Contains(detail, "invalid_grant") || strings.Contains(detail, "unauthorized_client") {
			return nil, fmt.Errorf("%w: google token endpoint %d: %s",
				ErrCredentialRevoked, resp.StatusCode, truncateForError(detail))
		}
		return nil, fmt.Errorf("google token endpoint %d: %s", resp.StatusCode, truncateForError(detail))
	}

	var tok tokenResponse
	if err = json.Unmarshal(body, &tok); err != nil {
		return nil, fmt.Errorf("decode google token response: %w", err)
	}
	if tok.AccessToken == "" {
		return nil, fmt.Errorf("google token response contained no access_token")
	}
	return &tok, nil
}

// userEmail reads the signed-in account's email address.
func (p *antigravityProvider) userEmail(ctx context.Context, accessToken string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, antigravityUserInfoEndpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")

	resp, err := p.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("userinfo %d: %s", resp.StatusCode, truncateForError(string(body)))
	}

	var payload struct {
		Email string `json:"email"`
		ID    string `json:"id"`
	}
	if err = json.Unmarshal(body, &payload); err != nil {
		return "", err
	}
	return payload.Email, nil
}

// codeAssistInfo is what loadCodeAssist tells us about an account.
type codeAssistInfo struct {
	projectID string
	tierID    string
	plan      string
	credits   *model.CreditBalance
}

// flexFloat accepts a JSON number or a numeric string, which the CodeAssist API
// mixes for credit amounts.
type flexFloat float64

func (f *flexFloat) UnmarshalJSON(data []byte) error {
	trimmed := strings.Trim(string(data), `"`)
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	parsed, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		return fmt.Errorf("parse number %s: %w", data, err)
	}
	*f = flexFloat(parsed)
	return nil
}

func (p *antigravityProvider) loadCodeAssist(ctx context.Context, accessToken string) (*codeAssistInfo, error) {
	payload := []byte(`{"metadata":{"ideType":"ANTIGRAVITY"}}`)
	endpoint := fmt.Sprintf("%s/%s:loadCodeAssist", AntigravityAPIEndpoint, antigravityAPIVersion)

	body, err := p.callAPI(ctx, endpoint, accessToken, payload, false)
	if err != nil {
		return nil, err
	}

	var raw struct {
		CloudaicompanionProject string `json:"cloudaicompanionProject"`
		ProjectID               string `json:"projectId"`
		Project                 string `json:"project"`
		CurrentTier             struct {
			ID string `json:"id"`
		} `json:"currentTier"`
		AllowedTiers []struct {
			ID        string `json:"id"`
			IsDefault bool   `json:"isDefault"`
		} `json:"allowedTiers"`
		PaidTier struct {
			ID               string `json:"id"`
			AvailableCredits []struct {
				CreditType                  string    `json:"creditType"`
				CreditAmount                flexFloat `json:"creditAmount"`
				MinimumCreditAmountForUsage flexFloat `json:"minimumCreditAmountForUsage"`
			} `json:"availableCredits"`
		} `json:"paidTier"`
	}
	if err = json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode loadCodeAssist response: %w", err)
	}

	info := &codeAssistInfo{}
	for _, candidate := range []string{raw.CloudaicompanionProject, raw.ProjectID, raw.Project} {
		if strings.TrimSpace(candidate) != "" {
			info.projectID = strings.TrimSpace(candidate)
			break
		}
	}

	// Tier resolution mirrors the IDE: prefer the default allowed tier, then the
	// current tier, then the free tier.
	for _, tier := range raw.AllowedTiers {
		if tier.IsDefault && tier.ID != "" {
			info.tierID = tier.ID
			break
		}
	}
	if info.tierID == "" {
		info.tierID = raw.CurrentTier.ID
	}
	if info.tierID == "" {
		info.tierID = "free-tier"
	}

	info.plan = raw.PaidTier.ID
	if info.plan == "" {
		info.plan = info.tierID
	}

	for _, credit := range raw.PaidTier.AvailableCredits {
		if credit.CreditType != "GOOGLE_ONE_AI" {
			continue
		}
		info.credits = &model.CreditBalance{
			CreditType: credit.CreditType,
			Amount:     float64(credit.CreditAmount),
			Minimum:    float64(credit.MinimumCreditAmountForUsage),
			TierID:     raw.PaidTier.ID,
			Available:  float64(credit.CreditAmount) >= float64(credit.MinimumCreditAmountForUsage),
		}
		break
	}
	return info, nil
}

// onboardUser provisions a Cloud AI Companion project for accounts that do not
// have one yet. It is a long-running operation, so the result is polled.
func (p *antigravityProvider) onboardUser(ctx context.Context, accessToken, tierID string) (string, error) {
	if tierID == "" {
		tierID = "free-tier"
	}
	request := map[string]any{
		"tier_id": tierID,
		"metadata": map[string]any{
			"ide_type":    "ANTIGRAVITY",
			"ide_version": p.ideVersion(ctx),
			"ide_name":    "antigravity",
		},
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	endpoint := fmt.Sprintf("%s/%s:onboardUser", antigravityDailyAPIEndpoint, antigravityAPIVersion)

	for attempt := 0; attempt < 5; attempt++ {
		body, callErr := p.callAPI(ctx, endpoint, accessToken, payload, true)
		if callErr != nil {
			return "", callErr
		}

		var raw struct {
			Done     bool `json:"done"`
			Response struct {
				CloudaicompanionProject struct {
					ID string `json:"id"`
				} `json:"cloudaicompanionProject"`
			} `json:"response"`
		}
		if err = json.Unmarshal(body, &raw); err != nil {
			return "", fmt.Errorf("decode onboardUser response: %w", err)
		}
		if raw.Done {
			if raw.Response.CloudaicompanionProject.ID == "" {
				return "", fmt.Errorf("onboardUser finished without a project id")
			}
			return raw.Response.CloudaicompanionProject.ID, nil
		}

		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return "", fmt.Errorf("onboardUser did not finish in time; retry in a minute")
}

// callAPI posts JSON to a CodeAssist endpoint.
func (p *antigravityProvider) callAPI(ctx context.Context, endpoint, accessToken string, payload []byte, onboarding bool) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("User-Agent", p.userAgent(ctx, onboarding))
	if onboarding {
		req.Header.Set("X-Goog-Api-Client", antigravityNodeAPIClient)
	}

	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized {
			return nil, fmt.Errorf("%w: codeassist %d: %s",
				ErrCredentialRevoked, resp.StatusCode, truncateForError(string(body)))
		}
		return nil, fmt.Errorf("codeassist %s returned %d: %s",
			endpoint, resp.StatusCode, truncateForError(string(body)))
	}
	return body, nil
}

// userAgent returns the User-Agent the Antigravity IDE sends on content
// requests. The upstream backend inspects this header to gate access to the
// paid tiers, so the value must match what the official IDE emits — including
// the platform token, which OmniRoute (#8098) has shown is pinned to
// darwin/arm64 regardless of the host this binary happens to run on.
//
// OmniRoute distinguishes an IDE profile (`antigravity/ide/<version>
// darwin/arm64`) from a CLI profile (`antigravity/cli/<version>
// (aidev_client; os_type=darwin; arch=arm64; auth_method=consumer)`).
// This binary only ever sends the IDE profile, matching the desktop app
// the operator signed in with.
func (p *antigravityProvider) userAgent(ctx context.Context, long bool) string {
	version := p.ideVersion(ctx)
	ua := fmt.Sprintf("antigravity/ide/%s darwin/arm64", version)
	if long {
		// The onboarding endpoint expects the IDE's Node-API client suffix.
		ua += " " + antigravityNodeAPIClient
	}
	return ua
}

// UserAgent exposes the completion-request User-Agent to the proxy layer.
func (p *antigravityProvider) UserAgent(ctx context.Context) string {
	return p.userAgent(ctx, false)
}

// ContentHeaderXGoogApiClient is the value Antigravity's IDE attaches to
// the X-Goog-Api-Client header on content requests. The backend uses this
// together with the User-Agent to identify the calling client; missing it
// on a request whose User-Agent looks like the IDE is one of the signals
// that triggers the "Resource has been exhausted" rejection (#resource
// exhausted) even on accounts that still have GOOGLE_ONE_AI credits.
const antigravityContentXGoogApiClient = "gl-node/22.21.1"

// ideVersion returns the current Antigravity version, cached for an hour. The
// upstream API only uses it for telemetry, so a stale value is harmless.
func (p *antigravityProvider) ideVersion(ctx context.Context) string {
	p.versionMu.Lock()
	if time.Since(p.versionFetched) < time.Hour && p.version != "" {
		version := p.version
		p.versionMu.Unlock()
		return version
	}
	p.versionMu.Unlock()

	version := p.fetchIDEVersion(ctx)

	p.versionMu.Lock()
	defer p.versionMu.Unlock()
	p.versionFetched = time.Now()
	if version != "" {
		p.version = version
	}
	if p.version == "" {
		p.version = antigravityFallbackVersion
	}
	return p.version
}

func (p *antigravityProvider) fetchIDEVersion(ctx context.Context) string {
	reqCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, antigravityVersionManifest, nil)
	if err != nil {
		return ""
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return ""
	}
	// The manifest is YAML; the only field needed is `version: x.y.z`.
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "version:"); ok {
			return strings.Trim(strings.TrimSpace(rest), `"'`)
		}
	}
	return ""
}

// credentialFromToken builds a credential, carrying the previous refresh token
// forward when Google omits it (which it does on refresh).
func credentialFromToken(tok *tokenResponse, previous *model.Credential) *model.Credential {
	cred := &model.Credential{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		IDToken:      tok.IDToken,
		LastRefresh:  time.Now(),
	}
	if tok.ExpiresIn > 0 {
		cred.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	} else {
		cred.ExpiresAt = time.Now().Add(50 * time.Minute)
	}
	if previous != nil {
		if cred.RefreshToken == "" {
			cred.RefreshToken = previous.RefreshToken
		}
		if cred.IDToken == "" {
			cred.IDToken = previous.IDToken
		}
	}
	return cred
}
