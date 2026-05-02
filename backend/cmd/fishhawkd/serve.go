package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"

	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
	"github.com/kuhlman-labs/fishhawk/backend/internal/version"
)

// runServe boots the HTTP server with graceful SIGINT/SIGTERM
// handling. Returns the intended process exit code.
func runServe(args []string, logSink io.Writer) int {
	fs := flag.NewFlagSet("fishhawkd serve", flag.ContinueOnError)
	fs.SetOutput(logSink)
	addr := fs.String("addr", envOr("FISHHAWKD_ADDR", ":8080"), "listen address")
	dbURL := fs.String("db", envOr("FISHHAWKD_DATABASE_URL", ""),
		"postgres URL; when empty, /v0/runs endpoints respond 503")
	if err := fs.Parse(args); err != nil {
		return exitFailure
	}

	logger := newLogger(logSink)

	cfg := server.Config{Addr: *addr, Logger: logger}

	// Wire the run repository when a DB URL is supplied. Without
	// one the server still boots — /healthz works and any
	// repository-dependent handler returns 503 — so operators can
	// smoke-test a deploy before pointing it at production data.
	var pool *pgxpool.Pool
	if *dbURL != "" {
		var err error
		pool, err = pgxpool.New(context.Background(), *dbURL)
		if err != nil {
			logger.Error("db pool create failed", slog.String("error", err.Error()))
			return exitFailure
		}
		defer pool.Close()
		cfg.RunRepo = runpkg.NewPostgresRepository(pool)
		logger.Info("run repository configured", slog.String("driver", "postgres"))
	} else {
		logger.Warn("FISHHAWKD_DATABASE_URL not set; /v0/runs endpoints will respond 503")
	}

	srv := server.New(cfg)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start() }()

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	case err := <-errCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("server start failed", slog.String("error", err.Error()))
			return exitFailure
		}
	}

	if err := srv.Shutdown(context.Background()); err != nil {
		logger.Error("shutdown failed", slog.String("error", err.Error()))
		return exitFailure
	}
	logger.Info("shutdown complete")
	return exitOK
}

// newLogger returns a slog logger writing JSON to logSink with the
// service / version pair pre-attached.
func newLogger(logSink io.Writer) *slog.Logger {
	logger := slog.New(slog.NewJSONHandler(logSink, &slog.HandlerOptions{Level: slog.LevelInfo}))
	logger = logger.With(
		slog.String("service", "fishhawkd"),
		slog.String("version", version.Version),
	)
	slog.SetDefault(logger)
	return logger
}
