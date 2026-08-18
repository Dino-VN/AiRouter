// Package config loads runtime configuration from environment variables.
//
// Everything the binary needs can be supplied through the environment (or a
// .env file sitting next to the binary), so a release can be shipped as a
// single executable plus a database URL.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// Config is the fully resolved application configuration.
type Config struct {
	// Listen is the address the HTTP server binds to.
	Listen string
	// DatabaseURL is the PostgreSQL connection string.
	DatabaseURL string
	// DataDir holds files that must survive restarts but are not in the DB
	// (currently only the auto-generated encryption key).
	DataDir string
	// PublicURL is the externally reachable base URL, used when rendering
	// OAuth instructions in the UI.
	PublicURL string

	// JWTSecret signs the short-lived access tokens issued to the web UI.
	JWTSecret []byte
	// EncryptionKey (32 bytes) encrypts upstream credentials at rest.
	EncryptionKey []byte

	// AccessTokenTTL is how long a UI access token stays valid.
	AccessTokenTTL time.Duration
	// RefreshTokenTTL is how long a UI refresh session stays valid.
	RefreshTokenTTL time.Duration

	// BootstrapAdminUsername / BootstrapAdminPassword create the first admin
	// account before the server starts listening. Both are empty by default,
	// which leaves the first account to the setup screen the UI shows while the
	// database has no users. They exist for unattended deployments, where nobody
	// is going to open a browser to finish the install.
	BootstrapAdminUsername string
	BootstrapAdminPassword string

	// ProxyURL routes upstream provider traffic through an HTTP/SOCKS proxy.
	ProxyURL string
	// RequestTimeout bounds a single non-streaming upstream call.
	RequestTimeout time.Duration

	// EnableLocalOAuthListeners starts loopback listeners on the ports the
	// vendor OAuth clients redirect to (Codex 1455, Antigravity 51121) so a
	// browser on this machine completes the flow without copy/paste.
	EnableLocalOAuthListeners bool

	// UsageRetentionDays prunes usage_records older than this (0 = keep all).
	UsageRetentionDays int

	// TrustProxyHeaders makes the server read the client IP from
	// X-Forwarded-For / X-Real-IP.
	TrustProxyHeaders bool

	// AntigravityFilterMode controls the coding-client filter applied to
	// requests bound for the Antigravity upstream. Valid values are "off"
	// (default, filter disabled), "block" (reject matching requests with HTTP
	// 403), and "rewrite" (replace matched names with "Antigravity" and
	// forward). See internal/proxy/antifilter.go for the matching logic.
	AntigravityFilterMode string
	// AntigravityFilterUseDefault toggles the built-in keyword preset.
	AntigravityFilterUseDefault bool
	// AntigravityFilterCustomMappings is the operator-supplied mapping
	// expression: a comma- or newline-delimited "from: to" string, e.g.
	// "Cursor: Antigravity\nWindsurf: Antigravity".
	AntigravityFilterCustomMappings string

	// generatedKeyPath records where an auto-generated encryption key was
	// written, so main() can warn about it.
	GeneratedKeyPath string
}

// Load reads configuration from the environment, applying defaults.
func Load() (*Config, error) {
	// A .env next to the working directory or the binary is convenient for
	// single-file deployments; missing files are not an error.
	_ = godotenv.Load()
	if exe, err := os.Executable(); err == nil {
		_ = godotenv.Load(filepath.Join(filepath.Dir(exe), ".env"))
	}

	cfg := &Config{
		Listen:                    env("AIHUB_LISTEN", ":8317"),
		DatabaseURL:               strings.TrimSpace(firstEnv("AIHUB_DATABASE_URL", "DATABASE_URL")),
		DataDir:                   env("AIHUB_DATA_DIR", defaultDataDir()),
		PublicURL:                 strings.TrimRight(env("AIHUB_PUBLIC_URL", ""), "/"),
		AccessTokenTTL:            envDuration("AIHUB_ACCESS_TOKEN_TTL", 30*time.Minute),
		RefreshTokenTTL:           envDuration("AIHUB_REFRESH_TOKEN_TTL", 30*24*time.Hour),
		BootstrapAdminUsername:    strings.TrimSpace(env("AIHUB_ADMIN_USERNAME", "")),
		BootstrapAdminPassword:    env("AIHUB_ADMIN_PASSWORD", ""),
		ProxyURL:                  strings.TrimSpace(env("AIHUB_PROXY_URL", "")),
		RequestTimeout:            envDuration("AIHUB_REQUEST_TIMEOUT", 10*time.Minute),
		EnableLocalOAuthListeners: envBool("AIHUB_LOCAL_OAUTH_LISTENERS", false),
		UsageRetentionDays:        envInt("AIHUB_USAGE_RETENTION_DAYS", 90),
		TrustProxyHeaders:         envBool("AIHUB_TRUST_PROXY_HEADERS", false),

		// Antigravity coding filter. Defaults to "rewrite" so a fresh
		// deployment screens Antigravity-bound requests out of the box:
		// system-prompt fields mentioning Claude Code / Codex / OpenCode
		// / Cursor / Windsurf / Cline / Aider / Continue.dev / etc. are
		// silently rewritten to "Antigravity" instead of being blocked,
		// which keeps upstream compatibility while hiding the
		// originating client name. Set to "off" to disable, "block" to
		// reject instead.
		AntigravityFilterMode:           strings.ToLower(env("AIHUB_ANTIGRAVITY_FILTER_MODE", "rewrite")),
		AntigravityFilterUseDefault:     envBool("AIHUB_ANTIGRAVITY_FILTER_USE_DEFAULT_KEYWORDS", true),
		AntigravityFilterCustomMappings: env("AIHUB_ANTIGRAVITY_FILTER_CUSTOM_MAPPINGS", ""),
	}

	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("AIHUB_DATABASE_URL (or DATABASE_URL) is required")
	}

	// Validate the antigravity filter mode early so a typo is loud at boot
	// instead of silently disabling the filter.
	switch cfg.AntigravityFilterMode {
	case "", "off", "block", "rewrite":
		// ok
	default:
		return nil, fmt.Errorf("AIHUB_ANTIGRAVITY_FILTER_MODE must be one of: off, block, rewrite; got %q",
			cfg.AntigravityFilterMode)
	}

	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir %s: %w", cfg.DataDir, err)
	}

	secret, err := resolveSecret(cfg.DataDir, "AIHUB_JWT_SECRET", "jwt.secret", 32)
	if err != nil {
		return nil, err
	}
	cfg.JWTSecret = secret.value
	if secret.generated {
		cfg.GeneratedKeyPath = secret.path
	}

	encKey, err := resolveSecret(cfg.DataDir, "AIHUB_ENCRYPTION_KEY", "encryption.key", 32)
	if err != nil {
		return nil, err
	}
	if len(encKey.value) != 32 {
		return nil, fmt.Errorf("AIHUB_ENCRYPTION_KEY must decode to exactly 32 bytes, got %d", len(encKey.value))
	}
	cfg.EncryptionKey = encKey.value
	if encKey.generated {
		cfg.GeneratedKeyPath = encKey.path
	}

	return cfg, nil
}

type resolvedSecret struct {
	value     []byte
	path      string
	generated bool
}

// resolveSecret reads a secret from the environment, falling back to a file in
// the data dir, and generating + persisting one when neither exists. This keeps
// "download the binary and run it" working while still using a stable key
// across restarts.
func resolveSecret(dataDir, envKey, fileName string, size int) (resolvedSecret, error) {
	if raw := strings.TrimSpace(os.Getenv(envKey)); raw != "" {
		decoded, err := decodeSecret(raw)
		if err != nil {
			return resolvedSecret{}, fmt.Errorf("%s: %w", envKey, err)
		}
		return resolvedSecret{value: decoded}, nil
	}

	path := filepath.Join(dataDir, fileName)
	if data, err := os.ReadFile(path); err == nil {
		decoded, decErr := decodeSecret(strings.TrimSpace(string(data)))
		if decErr != nil {
			return resolvedSecret{}, fmt.Errorf("%s: %w", path, decErr)
		}
		return resolvedSecret{value: decoded, path: path}, nil
	}

	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return resolvedSecret{}, fmt.Errorf("generate %s: %w", fileName, err)
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(buf)), 0o600); err != nil {
		return resolvedSecret{}, fmt.Errorf("persist %s: %w", path, err)
	}
	return resolvedSecret{value: buf, path: path, generated: true}, nil
}

// decodeSecret accepts hex or raw text. Hex is preferred because it round-trips
// through shell environments without quoting surprises.
func decodeSecret(raw string) ([]byte, error) {
	if decoded, err := hex.DecodeString(raw); err == nil && len(decoded) > 0 {
		return decoded, nil
	}
	if len(raw) < 16 {
		return nil, fmt.Errorf("secret must be at least 16 characters (or hex-encoded)")
	}
	return []byte(raw), nil
}

func defaultDataDir() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".aihub")
	}
	return ".aihub"
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return v
	}
	return fallback
}

func firstEnv(keys ...string) string {
	for _, key := range keys {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	return ""
}

func envBool(key string, fallback bool) bool {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(raw)
	if err != nil {
		return fallback
	}
	return parsed
}

func envInt(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return parsed
}

func envDuration(key string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return fallback
	}
	return parsed
}
