package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	neturl "net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/childcancel"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

func TestReconcileOrphanChildren_RequiresDB(t *testing.T) {
	t.Setenv("FISHHAWKD_DATABASE_URL", "")
	var errOut strings.Builder
	if got := runReconcileOrphanChildrenWith(nil, io.Discard, &errOut); got != exitUsage {
		t.Errorf("exit = %d, want %d", got, exitUsage)
	}
	if !strings.Contains(errOut.String(), "--db or FISHHAWKD_DATABASE_URL is required") {
		t.Errorf("stderr = %q, want the --db hint", errOut.String())
	}
}

func TestReconcileOrphanChildren_BadFlag(t *testing.T) {
	if got := run([]string{"reconcile-orphan-children", "--no-such-flag"}, io.Discard); got != exitUsage {
		t.Errorf("exit = %d, want %d", got, exitUsage)
	}
	var errOut strings.Builder
	if got := runReconcileOrphanChildrenWith([]string{"--db", "postgres://x", "extra"}, io.Discard, &errOut); got != exitUsage ||
		!strings.Contains(errOut.String(), `unexpected argument "extra"`) {
		t.Errorf("positional arg: exit = %d, stderr %q; want %d and the unexpected-argument line", got, errOut.String(), exitUsage)
	}
}

func TestReconcileOrphanChildren_ConnectFailure(t *testing.T) {
	var errOut strings.Builder
	got := runReconcileOrphanChildrenWith([]string{"--db", "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1"}, io.Discard, &errOut)
	if got != exitFailure || !strings.Contains(errOut.String(), "connect:") {
		t.Errorf("exit = %d, stderr %q; want %d and a connect error", got, errOut.String(), exitFailure)
	}
}

// TestReconcileOrphanChildren_ScanFailure: a database without the schema fails
// the orphan scan closed with exit 1 and writes nothing.
func TestReconcileOrphanChildren_ScanFailure(t *testing.T) {
	u, err := neturl.Parse(pgtest.NewURL(t))
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	u.Path = "/postgres" // the maintenance database carries no runs table
	var out, errOut strings.Builder
	if got := runReconcileOrphanChildrenWith([]string{"--db", u.String(), "--apply"}, &out, &errOut); got != exitFailure ||
		!strings.Contains(errOut.String(), "scan:") || strings.Contains(out.String(), "applied:") {
		t.Errorf("exit = %d, stdout %q, stderr %q; want %d, a scan error and no apply", got, out.String(), errOut.String(), exitFailure)
	}
}

// TestReconcileOrphanChildren_DryRunThenApply is the end-to-end backfill over
// real Postgres. Seeded: a running and a pending child under a cancelled and a
// succeeded parent (the two orphans), a child of a FAILED parent (kept — the
// revivable parent), a cancelled child of the cancelled parent (terminal) and
// a running non-child. The dry run lists exactly the two orphans and writes
// nothing; --apply cancels exactly those with parent_terminal /
// orphan_backfill rows; a second --apply finds 0 and appends nothing.
// Counterfactual (6): deleting the `if !*apply` early return makes the dry run
// cancel the orphans, which the post-dry-run re-read catches.
func TestReconcileOrphanChildren_DryRunThenApply(t *testing.T) {
	ctx := context.Background()
	url := pgtest.NewURL(t)
	pool, err := postgres.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	runs := runpkg.NewPostgresRepository(pool)
	au := audit.NewPostgresRepository(pool)

	mk := func(parent *uuid.UUID, states ...runpkg.State) *runpkg.Run {
		t.Helper()
		r, err := runs.CreateRun(ctx, runpkg.CreateRunParams{
			Repo: "x/y", WorkflowID: "feature_change", WorkflowSHA: "abc",
			TriggerSource: runpkg.TriggerCLI, DecomposedFrom: parent, ParentRunID: parent,
		})
		if err != nil {
			t.Fatalf("create run: %v", err)
		}
		for _, st := range states {
			if r, err = runs.TransitionRun(ctx, r.ID, st); err != nil {
				t.Fatalf("transition -> %s: %v", st, err)
			}
		}
		return r
	}
	cancelledParent := mk(nil, runpkg.StateRunning, runpkg.StateCancelled)
	succeededParent := mk(nil, runpkg.StateRunning, runpkg.StateSucceeded)
	failedParent := mk(nil, runpkg.StateRunning, runpkg.StateFailed)
	orphanA := mk(&cancelledParent.ID, runpkg.StateRunning)
	orphanB := mk(&succeededParent.ID)
	kept := mk(&failedParent.ID, runpkg.StateRunning)
	terminal := mk(&cancelledParent.ID, runpkg.StateCancelled)
	nonChild := mk(nil, runpkg.StateRunning)
	if _, err := runs.CreateStage(ctx, runpkg.CreateStageParams{
		RunID: orphanA.ID, Sequence: 0, Type: runpkg.StageTypeImplement,
		ExecutorKind: runpkg.ExecutorAgent, ExecutorRef: "claude-code",
	}); err != nil {
		t.Fatalf("create stage: %v", err)
	}

	state := func(id uuid.UUID) runpkg.State {
		t.Helper()
		r, err := runs.GetRun(ctx, id)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		return r.State
	}
	rows := func(id uuid.UUID) []*audit.Entry {
		t.Helper()
		es, err := au.ListForRunByCategory(ctx, id, childcancel.Category)
		if err != nil {
			t.Fatalf("list audit: %v", err)
		}
		return es
	}
	all := []uuid.UUID{orphanA.ID, orphanB.ID, kept.ID, terminal.ID, nonChild.ID}
	initial := map[uuid.UUID]runpkg.State{}
	for _, id := range all {
		initial[id] = state(id)
	}

	// Dry run.
	var out, errOut bytes.Buffer
	if got := runReconcileOrphanChildrenWith([]string{"--db", url}, &out, &errOut); got != exitOK {
		t.Fatalf("dry run exit = %d; stderr %s", got, errOut.String())
	}
	o := out.String()
	if strings.Count(o, "orphan child=") != 2 || !strings.Contains(o, orphanA.ID.String()) || !strings.Contains(o, orphanB.ID.String()) ||
		!strings.Contains(o, "dry-run: 2 orphan(s); re-run with --apply to cancel them") {
		t.Errorf("dry-run output:\n%s\nwant exactly the two orphans and the dry-run summary", o)
	}
	if !strings.Contains(o, "child="+orphanA.ID.String()+" child_state=running parent="+cancelledParent.ID.String()+" parent_state=cancelled implement_state=pending live=false") ||
		!strings.Contains(o, "child="+orphanB.ID.String()+" child_state=pending parent="+succeededParent.ID.String()+" parent_state=succeeded implement_state=- live=false") {
		t.Errorf("dry-run output missing an orphan line:\n%s", o)
	}
	for _, id := range all {
		if got := state(id); got != initial[id] {
			t.Errorf("dry run changed %s: %s -> %s", id, initial[id], got)
		}
		if n := len(rows(id)); n != 0 {
			t.Errorf("dry run appended %d rows on %s", n, id)
		}
	}

	// Apply.
	out.Reset()
	if got := runReconcileOrphanChildrenWith([]string{"--db", url, "--apply"}, &out, &errOut); got != exitOK {
		t.Fatalf("apply exit = %d; out %s stderr %s", got, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "applied: 2 orphan(s): cancelled=2 skipped_terminal=0 failed=0") {
		t.Errorf("apply output:\n%s", out.String())
	}
	for _, id := range []uuid.UUID{orphanA.ID, orphanB.ID} {
		if got := state(id); got != runpkg.StateCancelled {
			t.Errorf("orphan %s state = %s, want cancelled", id, got)
		}
		es := rows(id)
		if len(es) != 1 {
			t.Fatalf("orphan %s rows = %d, want 1", id, len(es))
		}
		var p map[string]any
		_ = json.Unmarshal(es[0].Payload, &p)
		if p["reason"] != childcancel.ReasonParentTerminal || p["cancel_source"] != childcancel.SourceBackfill {
			t.Errorf("orphan %s row = %v, want parent_terminal / orphan_backfill", id, p)
		}
	}
	for _, id := range []uuid.UUID{kept.ID, terminal.ID, nonChild.ID} {
		if got := state(id); got != initial[id] {
			t.Errorf("apply changed non-orphan %s: %s -> %s", id, initial[id], got)
		}
		if n := len(rows(id)); n != 0 {
			t.Errorf("apply appended %d rows on non-orphan %s", n, id)
		}
	}

	// Second apply converges.
	out.Reset()
	if got := runReconcileOrphanChildrenWith([]string{"--db", url, "--apply"}, &out, &errOut); got != exitOK {
		t.Fatalf("second apply exit = %d", got)
	}
	if !strings.Contains(out.String(), "applied: 0 orphan(s)") {
		t.Errorf("second apply output:\n%s", out.String())
	}
	for _, id := range []uuid.UUID{orphanA.ID, orphanB.ID} {
		if n := len(rows(id)); n != 1 {
			t.Errorf("second apply: %s rows = %d, want 1", id, n)
		}
	}
}

// TestReportReconcile_ExitCodes: a failed child, and a cancelled child whose
// row failed to append, each make --apply exit 1; skipped and clean cancels
// exit 0.
func TestReportReconcile_ExitCodes(t *testing.T) {
	ok := childcancel.ChildResult{ChildRunID: uuid.New(), Outcome: childcancel.OutcomeCancelled}
	skip := childcancel.ChildResult{ChildRunID: uuid.New(), Outcome: childcancel.OutcomeSkippedTerminal}
	failed := childcancel.ChildResult{ChildRunID: uuid.New(), Outcome: childcancel.OutcomeFailed, Err: errors.New("transition boom")}
	appendErr := childcancel.ChildResult{ChildRunID: uuid.New(), Outcome: childcancel.OutcomeCancelled, Err: errors.New("append boom")}
	for name, tc := range map[string]struct {
		in   []childcancel.ChildResult
		want int
		line string
	}{
		"clean":        {[]childcancel.ChildResult{ok, skip}, exitOK, "cancelled=1 skipped_terminal=1 failed=0"},
		"failed child": {[]childcancel.ChildResult{ok, failed}, exitFailure, "failed=1"},
		"append error": {[]childcancel.ChildResult{appendErr}, exitFailure, "cancelled=1 skipped_terminal=0 failed=1"},
	} {
		t.Run(name, func(t *testing.T) {
			var out strings.Builder
			if got := reportReconcile(&out, tc.in); got != tc.want || !strings.Contains(out.String(), tc.line) {
				t.Errorf("exit = %d, out:\n%s\nwant %d and %q", got, out.String(), tc.want, tc.line)
			}
		})
	}
}
