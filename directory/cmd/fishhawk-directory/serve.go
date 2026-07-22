package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/directory/internal/store"
	"github.com/kuhlman-labs/fishhawk/directory/pkg/routing"
)

const (
	// envDatabaseURL is the directory's OWN database — it holds only
	// (provider, account_key) -> home_region and is never a cell database.
	envDatabaseURL = "FISHHAWK_DIRECTORY_DATABASE_URL"
	envAddr        = "FISHHAWK_DIRECTORY_ADDR"

	defaultAddr = ":8081"
)

// shutdownGrace bounds the graceful drain on SIGTERM.
const shutdownGrace = 10 * time.Second

// runServe starts the directory HTTP server.
//
// Startup fails CLOSED and in a fixed order — database URL, then
// configuration, then the database itself — so a misconfiguration is an
// actionable message naming the env var rather than a listener serving a
// half-configured router. Nothing is opened before everything is validated.
func runServe(args []string, logSink io.Writer) int {
	fs := flag.NewFlagSet("fishhawk-directory serve", flag.ContinueOnError)
	fs.SetOutput(logSink)
	addr := fs.String("addr", envOr(envAddr, defaultAddr), "listen address")
	dbURL := fs.String("db", envOr(envDatabaseURL, ""), "postgres URL for the directory database")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	if *dbURL == "" {
		_, _ = fmt.Fprintf(logSink, "fishhawk-directory serve: --db or %s is required\n", envDatabaseURL)
		return exitUsage
	}

	cfg, err := routing.LoadConfigFromEnv()
	if err != nil {
		_, _ = fmt.Fprintf(logSink, "fishhawk-directory serve: %v\n", err)
		return exitUsage
	}

	logger := newLogger(logSink)
	if cfg.AdminToken == "" {
		// Deliberately not a startup abort: unset means every request to
		// BOTH surfaces is refused with 503 (ADR-062 A2.5). Say so loudly
		// so it is never mistaken for a working deployment.
		logger.Warn("operator credential is unset; the directory will refuse every request",
			slog.String("env", routing.EnvAdminToken))
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := store.MigrateUp(*dbURL); err != nil {
		logger.Error("directory migrations failed", slog.String("error", err.Error()))
		return exitFailure
	}

	pool, err := pgxpool.New(ctx, *dbURL)
	if err != nil {
		logger.Error("connect to the directory database failed", slog.String("error", err.Error()))
		return exitFailure
	}
	defer pool.Close()

	router, err := routing.NewPostgres(cfg, pool)
	if err != nil {
		logger.Error("build router failed", slog.String("error", err.Error()))
		return exitFailure
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           newHandler(router),
		ReadHeaderTimeout: 10 * time.Second,
	}

	logger.Info("directory listening",
		slog.String("addr", *addr),
		slog.Int("regions", len(cfg.Regions)),
		slog.Any("routed_paths", cfg.RoutedPaths))

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		if err != nil {
			logger.Error("listen failed", slog.String("error", err.Error()))
			return exitFailure
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("graceful shutdown failed", slog.String("error", err.Error()))
			return exitFailure
		}
		logger.Info("directory stopped")
	}
	return exitOK
}

// newHandler mounts the router under an unauthenticated /healthz. Health is
// the one surface that must answer without the operator credential — a
// liveness probe has none, and gating it would make an unconfigured
// directory look dead rather than closed.
func newHandler(router http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	mux.Handle("/", router)
	return mux
}

// runMigrate applies or rolls back the directory's own migrations.
func runMigrate(args []string, logSink io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(logSink, "fishhawk-directory migrate: direction (up|down) required")
		return exitUsage
	}
	direction, rest := args[0], args[1:]

	fs := flag.NewFlagSet("fishhawk-directory migrate", flag.ContinueOnError)
	fs.SetOutput(logSink)
	dbURL := fs.String("db", envOr(envDatabaseURL, ""), "postgres URL for the directory database")
	if err := fs.Parse(rest); err != nil {
		return exitUsage
	}
	if *dbURL == "" {
		_, _ = fmt.Fprintf(logSink, "fishhawk-directory migrate: --db or %s is required\n", envDatabaseURL)
		return exitUsage
	}

	logger := newLogger(logSink)
	switch direction {
	case "up":
		if err := store.MigrateUp(*dbURL); err != nil {
			logger.Error("migrate up failed", slog.String("error", err.Error()))
			return exitFailure
		}
		logger.Info("migrate up complete")
	case "down":
		if err := store.MigrateDown(*dbURL); err != nil {
			logger.Error("migrate down failed", slog.String("error", err.Error()))
			return exitFailure
		}
		logger.Info("migrate down complete")
	default:
		_, _ = fmt.Fprintf(logSink, "fishhawk-directory migrate: unknown direction %q (want up|down)\n", direction)
		return exitUsage
	}
	return exitOK
}

func newLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo}))
}
