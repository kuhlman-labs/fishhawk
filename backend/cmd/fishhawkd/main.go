// Command fishhawkd is the Fishhawk backend control plane.
//
// E3.2 (https://github.com/kuhlman-labs/fishhawk/issues/42) wires up the
// HTTP server with graceful shutdown, the middleware stack, and the
// /healthz endpoint. Runs and stages, the policy evaluator, and the
// REST API surface land in subsequent issues under epic E3 (#3).
package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
	"github.com/kuhlman-labs/fishhawk/backend/internal/version"
)

const exitFailure = 1

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

// run is split out from main so tests can drive it without exiting
// the test process. Returns the intended process exit code.
func run(args []string, logSink io.Writer) int {
	fs := flag.NewFlagSet("fishhawkd", flag.ContinueOnError)
	fs.SetOutput(logSink)
	addr := fs.String("addr", envOr("FISHHAWKD_ADDR", ":8080"), "listen address")
	if err := fs.Parse(args); err != nil {
		return exitFailure
	}

	logger := slog.New(slog.NewJSONHandler(logSink, &slog.HandlerOptions{Level: slog.LevelInfo}))
	logger = logger.With(
		slog.String("service", "fishhawkd"),
		slog.String("version", version.Version),
	)
	slog.SetDefault(logger)

	srv := server.New(server.Config{
		Addr:   *addr,
		Logger: logger,
	})

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
	return 0
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
