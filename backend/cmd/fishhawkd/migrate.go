package main

import (
	"flag"
	"fmt"
	"io"
	"log/slog"

	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
)

// runMigrate dispatches to the up or down direction.
//
//	fishhawkd migrate up
//	fishhawkd migrate down
//
// The database URL comes from --db or FISHHAWKD_DATABASE_URL.
func runMigrate(args []string, logSink io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(logSink, "fishhawkd migrate: direction (up|down) required")
		return exitUsage
	}
	direction, rest := args[0], args[1:]

	fs := flag.NewFlagSet("fishhawkd migrate", flag.ContinueOnError)
	fs.SetOutput(logSink)
	dbURL := fs.String("db", envOr("FISHHAWKD_DATABASE_URL", ""), "postgres URL")
	if err := fs.Parse(rest); err != nil {
		return exitFailure
	}
	if *dbURL == "" {
		_, _ = fmt.Fprintln(logSink, "fishhawkd migrate: --db or FISHHAWKD_DATABASE_URL is required")
		return exitUsage
	}

	logger := newLogger(logSink)

	switch direction {
	case "up":
		if err := postgres.MigrateUp(*dbURL); err != nil {
			logger.Error("migrate up failed", slog.String("error", err.Error()))
			return exitFailure
		}
		logger.Info("migrate up complete")
	case "down":
		if err := postgres.MigrateDown(*dbURL); err != nil {
			logger.Error("migrate down failed", slog.String("error", err.Error()))
			return exitFailure
		}
		logger.Info("migrate down complete")
	default:
		_, _ = fmt.Fprintf(logSink, "fishhawkd migrate: unknown direction %q (want up|down)\n", direction)
		return exitUsage
	}
	return exitOK
}
