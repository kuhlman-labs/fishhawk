package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/childcancel"
	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

const reconcileOrphanChildrenUsage = "Usage: fishhawkd reconcile-orphan-children [--db <url>] [--apply]"

// reconcileOrphanChildrenTimeout bounds the whole one-shot scan + apply.
const reconcileOrphanChildrenTimeout = 5 * time.Minute

// runReconcileOrphanChildren is the one-shot backfill for decomposition
// children orphaned before the #4186 cancel cascade existed:
//
//	fishhawkd reconcile-orphan-children [--db <url>] [--apply]
//
// It lists every non-terminal (pending/running) decomposition child whose
// parent is cancelled or succeeded (childcancel.FindOrphans; a FAILED parent
// is excluded because ReviveRun can re-admit it). The default is a DRY RUN
// that writes nothing. With --apply it cancels each orphan through the run
// state machine and appends one decomposition_child_cancelled row per child
// (reason parent_terminal, cancel_source orphan_backfill). Exit 0 on success
// (including a dry run), 1 on a connect/scan failure or when any child failed
// to cancel or its row failed to append, 2 on a usage error.
func runReconcileOrphanChildren(args []string, logSink io.Writer) int {
	return runReconcileOrphanChildrenWith(args, os.Stdout, logSink)
}

func runReconcileOrphanChildrenWith(args []string, out, logSink io.Writer) int {
	fs := flag.NewFlagSet("fishhawkd reconcile-orphan-children", flag.ContinueOnError)
	fs.SetOutput(logSink)
	dbURL := fs.String("db", envOr("FISHHAWKD_DATABASE_URL", ""), "postgres URL")
	apply := fs.Bool("apply", false, "cancel the listed orphans (default: dry run, writes nothing)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(logSink, "fishhawkd reconcile-orphan-children: unexpected argument %q\n%s\n", fs.Arg(0), reconcileOrphanChildrenUsage)
		return exitUsage
	}
	if *dbURL == "" {
		_, _ = fmt.Fprintf(logSink, "fishhawkd reconcile-orphan-children: --db or FISHHAWKD_DATABASE_URL is required\n%s\n", reconcileOrphanChildrenUsage)
		return exitUsage
	}

	ctx, cancel := context.WithTimeout(context.Background(), reconcileOrphanChildrenTimeout)
	defer cancel()
	pool, err := postgres.Connect(ctx, *dbURL)
	if err != nil {
		_, _ = fmt.Fprintf(logSink, "fishhawkd reconcile-orphan-children: connect: %v\n", err)
		return exitFailure
	}
	defer pool.Close()
	runs := runpkg.NewPostgresRepository(pool)
	au := audit.NewPostgresRepository(pool)

	orphans, err := childcancel.FindOrphans(ctx, runs)
	if err != nil {
		_, _ = fmt.Fprintf(logSink, "fishhawkd reconcile-orphan-children: scan: %v\n", err)
		return exitFailure
	}
	for _, o := range orphans {
		implState, live := orphanImplement(ctx, runs, o.Child.ID)
		_, _ = fmt.Fprintf(out, "orphan child=%s child_state=%s parent=%s parent_state=%s implement_state=%s live=%t\n",
			o.Child.ID, o.Child.State, *o.Child.DecomposedFrom, o.ParentState, implState, live)
	}
	if !*apply {
		_, _ = fmt.Fprintf(out, "dry-run: %d orphan(s); re-run with --apply to cancel them\n", len(orphans))
		return exitOK
	}

	return reportReconcile(out, childcancel.Reconcile(ctx, runs, au, orphans))
}

// reportReconcile prints one line per applied child plus a summary, and
// returns exitFailure when any child failed to cancel OR was cancelled but its
// audit row failed to append (the state change landed, the record did not).
func reportReconcile(out io.Writer, results []childcancel.ChildResult) int {
	var cancelled, skipped, failed int
	for _, r := range results {
		switch r.Outcome {
		case childcancel.OutcomeCancelled:
			cancelled++
		case childcancel.OutcomeSkippedTerminal:
			skipped++
		case childcancel.OutcomeFailed:
			failed++
		}
		line := fmt.Sprintf("child=%s parent=%s outcome=%s live=%t", r.ChildRunID, r.ParentRunID, r.Outcome, r.LiveStage)
		if r.Err != nil {
			line += " error=" + r.Err.Error()
			if r.Outcome == childcancel.OutcomeCancelled {
				failed++
			}
		}
		_, _ = fmt.Fprintln(out, line)
	}
	_, _ = fmt.Fprintf(out, "applied: %d orphan(s): cancelled=%d skipped_terminal=%d failed=%d\n", len(results), cancelled, skipped, failed)
	if failed > 0 {
		return exitFailure
	}
	return exitOK
}

// orphanImplement reads a child's implement stage state for the listing,
// best-effort: an unreadable or absent stage prints "-".
func orphanImplement(ctx context.Context, runs runpkg.Repository, childID uuid.UUID) (string, bool) {
	stages, err := runs.ListStagesForRun(ctx, childID)
	if err != nil {
		return "-", false
	}
	for _, st := range stages {
		if st.Type == runpkg.StageTypeImplement {
			return string(st.State), st.State == runpkg.StageStateDispatched || st.State == runpkg.StageStateRunning
		}
	}
	return "-", false
}
