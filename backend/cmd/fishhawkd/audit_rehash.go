package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"

	"github.com/kuhlman-labs/fishhawk/backend/internal/auditrehash"
	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
)

// runAuditRehash is the CLI entry point for the canonical-hash
// data migration (#302). Logic lives in
// backend/internal/auditrehash so it can be testcontainers-tested
// without dragging in the rest of the command.
//
//	fishhawkd audit-rehash [--db <url>] [--dry-run]
func runAuditRehash(args []string, logSink io.Writer) int {
	fs := flag.NewFlagSet("fishhawkd audit-rehash", flag.ContinueOnError)
	fs.SetOutput(logSink)
	dbURL := fs.String("db", envOr("FISHHAWKD_DATABASE_URL", ""), "postgres URL")
	dryRun := fs.Bool("dry-run", false, "report what would change without writing")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *dbURL == "" {
		_, _ = fmt.Fprintln(logSink, "fishhawkd audit-rehash: --db or FISHHAWKD_DATABASE_URL is required")
		return exitUsage
	}

	logger := newLogger(logSink)
	ctx := context.Background()

	pool, err := postgres.Connect(ctx, *dbURL)
	if err != nil {
		logger.Error("audit-rehash: connect failed", slog.String("error", err.Error()))
		return exitFailure
	}
	defer pool.Close()

	summary, err := auditrehash.RehashAllChains(ctx, pool, *dryRun)
	if err != nil {
		logger.Error("audit-rehash: failed", slog.String("error", err.Error()))
		return exitFailure
	}
	logger.Info("audit-rehash complete",
		slog.Bool("dry_run", *dryRun),
		slog.Int("chains", summary.Chains),
		slog.Int("entries_total", summary.EntriesTotal),
		slog.Int("entries_changed", summary.EntriesChanged),
	)
	return exitOK
}
