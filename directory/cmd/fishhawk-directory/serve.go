package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/kuhlman-labs/fishhawk/directory/internal/routing"
	"github.com/kuhlman-labs/fishhawk/directory/internal/store"
)

// EnvDatabaseURL / EnvAddr are the process-level knobs; the routing
// configuration is read by routing.LoadConfig.
const (
	envDatabaseURL = "FISHHAWK_DIRECTORY_DATABASE_URL"
	envAddr        = "FISHHAWK_DIRECTORY_ADDR"

	defaultAddr = ":8090"
)

// serveConfig is the fully-validated startup configuration.
type serveConfig struct {
	addr        string
	databaseURL string
	routing     routing.Config
}

// loadServeConfig parses flags and environment into a validated config.
//
// It fails closed rather than starting a directory that cannot route:
// a missing database URL, and any routing-configuration defect
// (unsupported/unconfigured regions, missing handoff secret — see
// routing.LoadConfig) abort startup.
func loadServeConfig(args []string, logSink io.Writer, getenv func(string) string) (serveConfig, error) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(logSink)
	addr := fs.String("addr", envOr(getenv, envAddr, defaultAddr), "listen address")
	if err := fs.Parse(args); err != nil {
		return serveConfig{}, fmt.Errorf("parse flags: %w", err)
	}

	dbURL := strings.TrimSpace(getenv(envDatabaseURL))
	if dbURL == "" {
		return serveConfig{}, fmt.Errorf("%s is required", envDatabaseURL)
	}

	cfg, err := routing.LoadConfig(getenv)
	if err != nil {
		return serveConfig{}, err
	}
	return serveConfig{addr: *addr, databaseURL: dbURL, routing: cfg}, nil
}

// runServe migrates the directory database, mounts the router, and
// serves until SIGINT/SIGTERM.
func runServe(args []string, logSink io.Writer, getenv func(string) string) int {
	log := slog.New(slog.NewTextHandler(logSink, nil))

	cfg, err := loadServeConfig(args, logSink, getenv)
	if err != nil {
		log.Error("fishhawk-directory: startup configuration invalid", "error", err)
		return exitFailure
	}

	if err := store.MigrateUp(cfg.databaseURL); err != nil {
		log.Error("fishhawk-directory: migrate", "error", err)
		return exitFailure
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := store.Connect(ctx, cfg.databaseURL)
	if err != nil {
		log.Error("fishhawk-directory: connect database", "error", err)
		return exitFailure
	}
	defer pool.Close()

	router := routing.New(store.New(pool), cfg.routing, routing.WithLogger(log))
	srv := &http.Server{
		Addr:              cfg.addr,
		Handler:           router.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("fishhawk-directory: listening",
			"addr", cfg.addr,
			"supported_regions", strings.Join(cfg.routing.SupportedRegions, ","))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		if err != nil {
			log.Error("fishhawk-directory: serve", "error", err)
			return exitFailure
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Error("fishhawk-directory: shutdown", "error", err)
			return exitFailure
		}
	}
	return exitOK
}

// runMigrate applies or rolls back directory migrations.
func runMigrate(args []string, logSink io.Writer, getenv func(string) string) int {
	dbURL := strings.TrimSpace(getenv(envDatabaseURL))
	if dbURL == "" {
		_, _ = fmt.Fprintf(logSink, "fishhawk-directory: %s is required\n", envDatabaseURL)
		return exitFailure
	}
	direction := "up"
	if len(args) > 0 {
		direction = args[0]
	}
	var err error
	switch direction {
	case "up":
		err = store.MigrateUp(dbURL)
	case "down":
		err = store.MigrateDown(dbURL)
	default:
		_, _ = fmt.Fprintf(logSink, "fishhawk-directory: unknown migrate direction %q (want up|down)\n", direction)
		return exitUsage
	}
	if err != nil {
		_, _ = fmt.Fprintf(logSink, "fishhawk-directory: migrate %s: %v\n", direction, err)
		return exitFailure
	}
	return exitOK
}

func envOr(getenv func(string) string, key, def string) string {
	if v := strings.TrimSpace(getenv(key)); v != "" {
		return v
	}
	return def
}
