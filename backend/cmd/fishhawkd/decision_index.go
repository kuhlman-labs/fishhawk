package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"

	"github.com/kuhlman-labs/fishhawk/backend/internal/decisionindex"
	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
)

const decisionIndexUsage = "Usage: fishhawkd decision-index [backfill|check] [--db <url>] [--rebuild] [--dry-run] [--page-size N] [--limit N]"

// runDecisionIndex is the operator entry point for decision_index (E75.2 /
// #3730, ADR-082): a derived projection of the decision-bearing audit
// entries. Logic lives in backend/internal/decisionindex.
//
//	fishhawkd decision-index backfill [--db <url>] [--rebuild] [--dry-run] [--page-size N]
//	fishhawkd decision-index check    [--db <url>] [--limit N]
//
// backfill reconstructs the index from the chain (idempotent; --rebuild
// truncates first). check runs the missing-row gap check and exits 1 when a
// TRUE gap exists, so it can be wired into a cron; orphaned entries (whose run
// row no longer exists) are reported but never fail it.
func runDecisionIndex(args []string, logSink io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(logSink, "fishhawkd decision-index: mode required (backfill or check)")
		_, _ = fmt.Fprintln(logSink, decisionIndexUsage)
		return exitUsage
	}
	mode, rest := args[0], args[1:]
	if mode != "backfill" && mode != "check" {
		_, _ = fmt.Fprintf(logSink, "fishhawkd decision-index: unknown mode %q (want backfill or check)\n", mode)
		_, _ = fmt.Fprintln(logSink, decisionIndexUsage)
		return exitUsage
	}

	fs := flag.NewFlagSet("fishhawkd decision-index "+mode, flag.ContinueOnError)
	fs.SetOutput(logSink)
	dbURL := fs.String("db", envOr("FISHHAWKD_DATABASE_URL", ""), "postgres URL")
	rebuild := fs.Bool("rebuild", false, "backfill: truncate the index before reconstructing it")
	dryRun := fs.Bool("dry-run", false, "backfill: extract and count without writing")
	pageSize := fs.Int("page-size", decisionindex.DefaultPageSize, "backfill: audit entries per keyset page")
	limit := fs.Int("limit", 50, "check: maximum gaps / orphaned sequences to list (counts are never truncated)")
	if err := fs.Parse(rest); err != nil {
		return exitUsage
	}
	if *dbURL == "" {
		_, _ = fmt.Fprintln(logSink, "fishhawkd decision-index: --db or FISHHAWKD_DATABASE_URL is required")
		return exitUsage
	}

	logger := newLogger(logSink)
	ctx := context.Background()
	pool, err := postgres.Connect(ctx, *dbURL)
	if err != nil {
		logger.Error("decision-index: connect failed", slog.String("error", err.Error()))
		return exitFailure
	}
	defer pool.Close()

	if mode == "check" {
		return runDecisionIndexCheck(ctx, decisionindex.NewStore(pool), *limit, logSink, logger)
	}

	sum, err := decisionindex.Backfill(ctx, pool, decisionindex.Options{
		Rebuild: *rebuild, DryRun: *dryRun, PageSize: *pageSize,
	})
	if err != nil {
		logger.Error("decision-index backfill: failed", slog.String("error", err.Error()))
		return exitFailure
	}
	logger.Info("decision-index backfill complete",
		slog.Bool("rebuild", sum.Rebuild),
		slog.Bool("dry_run", sum.DryRun),
		slog.Int("entries_scanned", sum.EntriesScanned),
		slog.Int("rows_extracted", sum.RowsExtracted),
		slog.Int("rows_written", sum.RowsWritten),
		slog.Int("rows_skipped_no_run", sum.RowsSkippedNoRun),
		slog.Any("skipped_no_run_sequences", sum.SkippedNoRunSequences),
		slog.Any("unmapped_categories", sum.UnmappedCategories),
		slog.Int("gaps_remaining", sum.GapsRemaining),
		slog.Int("orphaned_entries", sum.OrphanedEntries),
	)
	return exitOK
}

func runDecisionIndexCheck(ctx context.Context, store *decisionindex.Store, limit int, out io.Writer, logger *slog.Logger) int {
	rep, err := store.Gaps(ctx, limit)
	if err != nil {
		logger.Error("decision-index check: failed", slog.String("error", err.Error()))
		return exitFailure
	}
	_, _ = fmt.Fprintf(out, "decision-index check: gaps=%d orphaned_entries=%d\n", rep.GapCount, rep.OrphanedCount)
	for _, g := range rep.Gaps {
		_, _ = fmt.Fprintf(out, "  gap sequence=%d category=%s run=%s ts=%s\n",
			g.SourceSequence, g.Category, g.RunID, g.Timestamp.Format("2006-01-02T15:04:05Z07:00"))
	}
	if rep.OrphanedCount > 0 {
		_, _ = fmt.Fprintf(out, "  orphaned (run row absent; not a gap) sequences=%v\n", rep.OrphanedSequences)
	}
	if rep.GapCount > 0 {
		_, _ = fmt.Fprintln(out, "decision-index check: FAIL — run `fishhawkd decision-index backfill` to index the gaps")
		return exitFailure
	}
	return exitOK
}
