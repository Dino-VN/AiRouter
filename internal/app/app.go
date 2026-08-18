// Package app wires the configuration, database, and HTTP server together.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"aihub/internal/authn"
	"aihub/internal/config"
	"aihub/internal/cryptobox"
	"aihub/internal/dbx"
	"aihub/internal/httpapi"
	"aihub/internal/model"
	"aihub/internal/provider"
	"aihub/internal/proxy"
	"aihub/internal/store"
	"aihub/internal/webui"
)

// App is a fully wired application instance.
type App struct {
	cfg     *config.Config
	log     *slog.Logger
	version string

	pool     *pgxpool.Pool
	store    *store.Store
	issuer   *authn.Issuer
	registry *provider.Registry
	router   *proxy.Router
}

// New connects to the database, applies migrations and, when the environment
// asks for it, creates the first admin account.
func New(ctx context.Context, cfg *config.Config, logger *slog.Logger, version string) (*App, error) {
	box, err := cryptobox.New(cfg.EncryptionKey)
	if err != nil {
		return nil, err
	}

	pool, err := dbx.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}

	applied, err := dbx.Migrate(ctx, pool)
	if err != nil {
		pool.Close()
		return nil, err
	}
	if len(applied) > 0 {
		logger.Info("database migrated", "versions", applied)
	}

	st := store.New(pool, box)
	registry, err := provider.NewRegistry(cfg, st, logger)
	if err != nil {
		pool.Close()
		return nil, err
	}

	a := &App{
		cfg:      cfg,
		log:      logger,
		version:  version,
		pool:     pool,
		store:    st,
		issuer:   authn.NewIssuer(cfg.JWTSecret, cfg.AccessTokenTTL),
		registry: registry,
		router:   proxy.NewRouter(st, registry, logger),
	}

	// Install the antigravity coding filter when one of block or rewrite modes
	// is selected. "off" leaves the filter unset, so the router's hot path
	// skips it entirely.
	if filter := buildAntigravityFilter(cfg, logger); filter != nil {
		a.router.SetAntigravityFilter(filter)
	}
	// Debug logging: when AIHUB_DEBUG_REQUESTS=true, the antigravity and
	// codex executors log the request body, response status and body
	// (bounded) for every upstream call. The default is off so the log
	// stays quiet on a healthy deployment.
	a.router.SetDebugRequests(cfg.DebugRequests)

	if err = a.bootstrapAdmin(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	if cfg.GeneratedKeyPath != "" {
		logger.Warn("generated a new secret key; back it up or set AIHUB_ENCRYPTION_KEY, "+
			"otherwise stored provider credentials become unreadable",
			"path", cfg.GeneratedKeyPath)
	}
	return a, nil
}

// Close releases the database pool.
func (a *App) Close() {
	if a.pool != nil {
		a.pool.Close()
	}
}

// Serve runs the HTTP server until ctx is cancelled.
func (a *App) Serve(ctx context.Context) error {
	handler := httpapi.New(httpapi.Deps{
		Config:   a.cfg,
		Store:    a.store,
		Issuer:   a.issuer,
		Registry: a.registry,
		Router:   a.router,
		Logger:   a.log,
		Version:  a.version,
	})

	srv := &http.Server{
		Addr:              a.cfg.Listen,
		Handler:           handler,
		ReadHeaderTimeout: 20 * time.Second,
		// Streaming responses can outlive any sane write deadline, so writes are
		// bounded by the per-request context instead.
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
	}

	janitorCtx, stopJanitor := context.WithCancel(ctx)
	defer stopJanitor()
	go a.janitor(janitorCtx)
	go a.refreshCatalog(janitorCtx)

	if !webui.Built() {
		a.log.Warn("this binary contains the placeholder web UI; build the real one with `make ui` and rebuild")
	}

	errCh := make(chan error, 1)
	go func() {
		a.log.Info("listening", "addr", a.cfg.Listen, "version", a.version)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		a.log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// ResetPassword sets a new password for one account and signs it out
// everywhere. It is the way back in when the last admin password is lost, since
// the setup screen only ever appears while the database has no accounts at all.
func (a *App) ResetPassword(ctx context.Context, username, password string) error {
	hash, err := authn.HashPassword(password)
	if err != nil {
		return err
	}
	user, err := a.store.GetUserByUsername(ctx, username)
	if err != nil {
		return fmt.Errorf("look up %s: %w", username, err)
	}
	if _, err = a.store.UpdateUser(ctx, user.ID, store.UserUpdate{PasswordHash: &hash}); err != nil {
		return err
	}
	// Force every browser session for that account to re-authenticate.
	return a.store.RevokeUserWebSessions(ctx, user.ID)
}

// bootstrapAdmin creates the first admin account from the environment. It is an
// escape hatch for unattended deployments; with AIHUB_ADMIN_USERNAME unset,
// which is the default, nothing happens here and the web UI offers its setup
// screen instead.
func (a *App) bootstrapAdmin(ctx context.Context) error {
	username := a.cfg.BootstrapAdminUsername
	if username == "" {
		if a.cfg.BootstrapAdminPassword != "" {
			// A password with no account to attach it to is a typo worth
			// reporting: staying silent would leave the operator waiting for an
			// account that is never going to appear.
			a.log.Warn("AIHUB_ADMIN_PASSWORD is set but AIHUB_ADMIN_USERNAME is not, so no account " +
				"was created; either set both, or finish setup in the web UI")
		}
		return nil
	}
	if !model.ValidUsername(username) {
		return fmt.Errorf("AIHUB_ADMIN_USERNAME %q must be %d-%d characters of letters, digits, dot, "+
			"underscore or hyphen, starting and ending with a letter or digit",
			username, model.UsernameMinLen, model.UsernameMaxLen)
	}

	count, err := a.store.CountUsers(ctx)
	if err != nil {
		return err
	}
	if count > 0 {
		return nil
	}

	password := a.cfg.BootstrapAdminPassword
	generated := false
	if password == "" {
		password, err = randomPassword()
		if err != nil {
			return err
		}
		generated = true
	}

	hash, err := authn.HashPassword(password)
	if err != nil {
		return fmt.Errorf("bootstrap admin: %w", err)
	}
	user := &model.User{
		Username:     username,
		DisplayName:  "Administrator",
		Role:         model.RoleAdmin,
		Status:       model.StatusActive,
		PasswordHash: hash,
	}
	// CreateFirstAdmin, not CreateUser: the count above is a separate statement,
	// so the insert carries the same condition and two replicas starting at once
	// cannot both create an owner.
	if err = a.store.CreateFirstAdmin(ctx, user, model.UnlimitedQuota(user.ID)); err != nil {
		if errors.Is(err, store.ErrConflict) {
			// Somebody else got there first, which is the outcome we wanted.
			return nil
		}
		return fmt.Errorf("bootstrap admin: %w", err)
	}

	if generated {
		// Printed once: there is no other way to recover it.
		a.log.Warn("created the first admin account with a generated password",
			"username", user.Username, "password", password)
	} else {
		a.log.Info("created the first admin account", "username", user.Username)
	}
	return nil
}

// janitor performs periodic maintenance: expiring stale OAuth attempts, pruning
// dead web sessions and trimming usage history.
func (a *App) janitor(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	run := func() {
		opCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		if n, err := a.store.ExpireOAuthSessions(opCtx); err != nil {
			a.log.Warn("expire oauth sessions", "error", err)
		} else if n > 0 {
			a.log.Debug("expired oauth sessions", "count", n)
		}
		if _, err := a.store.PruneOAuthSessions(opCtx, time.Now().Add(-7*24*time.Hour)); err != nil {
			a.log.Warn("prune oauth sessions", "error", err)
		}
		if _, err := a.store.PruneWebSessions(opCtx); err != nil {
			a.log.Warn("prune web sessions", "error", err)
		}
		if a.cfg.UsageRetentionDays > 0 {
			cutoff := time.Now().AddDate(0, 0, -a.cfg.UsageRetentionDays)
			if n, err := a.store.PruneUsage(opCtx, cutoff); err != nil {
				a.log.Warn("prune usage", "error", err)
			} else if n > 0 {
				a.log.Info("pruned usage history", "rows", n, "older_than", cutoff)
			}
		}
	}

	run()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

// refreshCatalog keeps the model list current. The binary ships with a seed
// catalog, so a failure here only means clients see the models this build knew
// about, which is why it is logged and otherwise ignored.
func (a *App) refreshCatalog(ctx context.Context) {
	catalog := a.registry.Catalog()

	attempt := func() {
		if err := catalog.Refresh(ctx); err != nil {
			a.log.Debug("could not refresh the model catalog", "error", err)
			return
		}
		a.log.Info("model catalog refreshed", "models", len(catalog.All()))
	}

	// The first attempt waits a moment so a cold start is not blocked behind a
	// network call nobody is waiting for.
	select {
	case <-ctx.Done():
		return
	case <-time.After(10 * time.Second):
		attempt()
	}

	ticker := time.NewTicker(6 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			attempt()
		}
	}
}

// buildAntigravityFilter constructs the antigravity coding filter from the
// configuration the operator supplied via environment variables. Returning
// nil when mode is "off" lets the caller short-circuit the filter entirely.
// A malformed custom_mappings string is reported loudly at boot time: the
// filter is still installed so the rest of the configuration is honoured, but
// the operator sees a warning before any request is screened.
func buildAntigravityFilter(cfg *config.Config, logger *slog.Logger) *proxy.AntigravityFilter {
	mode := proxy.AntigravityFilterMode(cfg.AntigravityFilterMode)
	if mode == proxy.AntigravityFilterOff || mode == "" {
		return nil
	}

	var customMappings []proxy.AntigravityMapping
	if strings.TrimSpace(cfg.AntigravityFilterCustomMappings) != "" {
		parsed, err := proxy.ParseAntigravityCustomMappings(cfg.AntigravityFilterCustomMappings)
		if err != nil {
			logger.Error("antigravity filter: ignoring custom_mappings due to parse error",
				"error", err, "raw", cfg.AntigravityFilterCustomMappings)
		} else {
			customMappings = parsed
		}
	}

	return proxy.NewAntigravityFilter(mode, cfg.AntigravityFilterUseDefault, customMappings)
}
