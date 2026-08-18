// Command aihub is a self-contained, multi-tenant proxy for Codex and
// Antigravity accounts with an embedded web UI.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"aihub/internal/app"
	"aihub/internal/config"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	var (
		showVersion = flag.Bool("version", false, "print the version and exit")
		migrateOnly = flag.Bool("migrate", false, "apply database migrations and exit")
		logLevel    = flag.String("log-level", "info", "log level: debug, info, warn, error")
		resetPW     = flag.String("reset-password", "",
			"reset one account's password and exit, as username:newpassword")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("aihub", version)
		return
	}

	// Parsed before anything connects, so a mistyped flag fails immediately
	// instead of after a database round trip. Cut splits on the first colon only,
	// which leaves colons usable inside the password.
	var resetUser, resetPass string
	if *resetPW != "" {
		var ok bool
		resetUser, resetPass, ok = strings.Cut(*resetPW, ":")
		resetUser = strings.TrimSpace(resetUser)
		if !ok || resetUser == "" || resetPass == "" {
			fmt.Fprintln(os.Stderr, "-reset-password expects username:newpassword")
			os.Exit(2)
		}
	}

	logger := newLogger(*logLevel)
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		logger.Error("configuration error", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	application, err := app.New(ctx, cfg, logger, version)
	if err != nil {
		logger.Error("startup failed", "error", err)
		os.Exit(1)
	}
	defer application.Close()

	switch {
	case *migrateOnly:
		logger.Info("migrations applied, exiting as requested")
	case *resetPW != "":
		if err = application.ResetPassword(ctx, resetUser, resetPass); err != nil {
			logger.Error("reset password failed", "username", resetUser, "error", err)
			os.Exit(1)
		}
		logger.Info("password reset", "username", resetUser)
	default:
		if err = application.Serve(ctx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("server stopped", "error", err)
			os.Exit(1)
		}
	}
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}
