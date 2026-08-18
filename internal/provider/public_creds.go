package provider

// public_creds.go — public OAuth client credentials for upstream CLIs.
//
// The values here are publicly distributed by the upstream CLIs/IDEs
// themselves — they ship inside their binaries or web apps. OAuth client
// credentials for native/installed apps using PKCE are public by design
// (see https://developers.google.com/identity/protocols/oauth2/native-app):
// they identify the calling app to the consent screen but cannot on
// their own grant access to anything. The user still has to consent.
//
// The set here was decoded from OmniRoute
// (github.com/diegosouzapw/OmniRoute) which ships them XOR-masked to
// keep pattern scanners quiet; the plaintext values are identical to
// the ones that ship in the upstream binaries.
//
// GitHub's secret scanner still flags Google OAuth client secrets
// (the GOCSPX-… prefix) even when they ship publicly in upstream
// binaries. The operator accepts the scanner alert per push rather
// than masking the values: the plaintext is the single source of
// truth, and the unblock link the scanner prints is shared across
// every future push so the cost is one click per release.
//
// Today only Antigravity and Codex are wired into aihub's OAuth flow.
// The rest are recorded here as exported helpers so that when a new
// provider is added, the credentials do not have to be
// reverse-engineered again — and so that operators who want to
// override any of them (e.g. to point Antigravity at an internal
// mirror) have a single env var to set per provider.

import (
	"os"
	"strings"
)

// publicCreds holds the embedded plaintext values for every public
// OAuth client the project knows about. They are written as plain
// string constants so they are easy to audit; security-through-
// obscurity would not help here (the values ship in upstream
// binaries anyone can read).
type publicCreds struct {
	antigravityID     string
	antigravitySecret string

	codexID string

	// Google AI Studio / Gemini CLI OAuth client (public, PKCE).
	geminiID     string
	geminiSecret string

	// Anthropic Claude Code CLI OAuth client (public, PKCE).
	claudeID string

	// GitHub Copilot CLI OAuth app id (public, device flow).
	githubCopilotID string

	// xAI Grok Build CLI OAuth client (public, import-token flow).
	grokID string

	// Moonshot Kimi Coding CLI OAuth client (public).
	kimiID string
}

// embeddedPublicCreds is the value-of-record. Edits here should be
// mirrored in the comments above each field so a reviewer can verify
// the value against the upstream CLI's binary.
var embeddedPublicCreds = publicCreds{
	// Antigravity IDE — Google OAuth 2.0 installed-app client.
	// Confirmed byte-for-byte identical in OmniRoute's
	// open-sse/utils/publicCreds.ts EMBEDDED_DEFAULTS.antigravity_id
	// and antigravity_alt.
	antigravityID:     "1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com",
	antigravitySecret: "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",

	// Codex CLI — OpenAI Auth0 client.
	// Confirmed byte-for-byte identical in OmniRoute's
	// EMBEDDED_DEFAULTS.codex_id.
	codexID: "app_EMoamEEZ73f0CkXaXp7hrann",

	// Google AI Studio / Gemini CLI — Google OAuth 2.0
	// installed-app client. Distinct from Antigravity's client even
	// though both are Google: this one is the AI Studio desktop
	// client's, used by `gemini` CLI to enrol free-tier access.
	geminiID:     "681255809395-oo8ft2oprdrnp9e3aqf6av3hmdib135j.apps.googleusercontent.com",
	geminiSecret: "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",

	// Anthropic Claude Code CLI — OAuth client UUID.
	claudeID: "9d1c250a-e61b-44d9-88ed-5944d1962f5e",

	// GitHub Copilot CLI — public OAuth app, device code flow.
	githubCopilotID: "Iv1.b507a08c87ecfe98",

	// xAI Grok Build CLI — public OAuth client UUID.
	grokID: "b1a00492-073a-47ea-816f-4c329264a828",

	// Moonshot Kimi Coding CLI — public OAuth client UUID.
	kimiID: "17e5f671-d194-4dfb-9706-5516cb48c098",
}

// resolvePublicCred returns the env-var override when one is set, and
// falls back to the embedded default otherwise. The pattern mirrors
// OmniRoute's resolvePublicCred: an empty env value is treated as
// unset, so operators who explicitly want to disable a credential can
// leave the env var empty rather than try to null it out.
//
// Each call site picks its own env-var name (e.g.
// AIHUB_ANTIGRAVITY_OAUTH_CLIENT_ID) so providers do not collide with
// each other and operators can target the override they care about
// without reading the source.
func resolvePublicCred(envName, embedded string) string {
	if v := strings.TrimSpace(os.Getenv(envName)); v != "" {
		return v
	}
	return embedded
}

// antigravityOAuthClientID resolves the Antigravity IDE's Google OAuth
// client id, honouring the AIHUB_ANTIGRAVITY_OAUTH_CLIENT_ID env var
// when set. This is the client id that appears on Google's consent
// screen when a user signs an Antigravity account in.
func antigravityOAuthClientID() string {
	return resolvePublicCred("AIHUB_ANTIGRAVITY_OAUTH_CLIENT_ID", embeddedPublicCreds.antigravityID)
}

// antigravityOAuthClientSecret resolves the matching client secret.
// It is also public; OmniRoute ships it in open-sse/utils/publicCreds.ts.
func antigravityOAuthClientSecret() string {
	return resolvePublicCred("AIHUB_ANTIGRAVITY_OAUTH_CLIENT_SECRET", embeddedPublicCreds.antigravitySecret)
}

// codexOAuthClientID resolves the Codex CLI's OpenAI Auth0 client id,
// honouring AIHUB_CODEX_OAUTH_CLIENT_ID when set.
func codexOAuthClientID() string {
	return resolvePublicCred("AIHUB_CODEX_OAUTH_CLIENT_ID", embeddedPublicCreds.codexID)
}

// The remaining credentials are not yet wired into any provider — they
// are recorded so adding Gemini, Claude, GitHub Copilot, Grok or Kimi
// support later is a matter of writing the provider itself, not of
// recovering the client id from a binary. Until then they are exported
// so a future provider package can import them without re-deriving the
// values.

// GeminiOAuthClientID is the Google AI Studio / Gemini CLI public OAuth
// client id. Use AIHUB_GEMINI_OAUTH_CLIENT_ID to override.
func GeminiOAuthClientID() string {
	return resolvePublicCred("AIHUB_GEMINI_OAUTH_CLIENT_ID", embeddedPublicCreds.geminiID)
}

// GeminiOAuthClientSecret is the matching client secret.
func GeminiOAuthClientSecret() string {
	return resolvePublicCred("AIHUB_GEMINI_OAUTH_CLIENT_SECRET", embeddedPublicCreds.geminiSecret)
}

// ClaudeOAuthClientID is the Anthropic Claude Code CLI public OAuth
// client UUID. Use AIHUB_CLAUDE_OAUTH_CLIENT_ID to override.
func ClaudeOAuthClientID() string {
	return resolvePublicCred("AIHUB_CLAUDE_OAUTH_CLIENT_ID", embeddedPublicCreds.claudeID)
}

// GitHubCopilotClientID is the GitHub Copilot CLI public OAuth app id
// (device code flow). Use AIHUB_GITHUB_COPILOT_OAUTH_CLIENT_ID to override.
func GitHubCopilotClientID() string {
	return resolvePublicCred("AIHUB_GITHUB_COPILOT_OAUTH_CLIENT_ID", embeddedPublicCreds.githubCopilotID)
}

// GrokOAuthClientID is the xAI Grok Build CLI public OAuth client UUID.
// Use AIHUB_GROK_OAUTH_CLIENT_ID to override.
func GrokOAuthClientID() string {
	return resolvePublicCred("AIHUB_GROK_OAUTH_CLIENT_ID", embeddedPublicCreds.grokID)
}

// KimiOAuthClientID is the Moonshot Kimi Coding CLI public OAuth client
// UUID. Use AIHUB_KIMI_OAUTH_CLIENT_ID to override.
func KimiOAuthClientID() string {
	return resolvePublicCred("AIHUB_KIMI_OAUTH_CLIENT_ID", embeddedPublicCreds.kimiID)
}
