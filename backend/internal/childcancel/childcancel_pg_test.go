package childcancel

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// pgCreateRun creates a run (optionally a decomposition child of parent) and
// drives it to state through the real state machine.
func pgCreateRun(t *testing.T, runs run.Repository, parent *uuid.UUID, state run.State) *run.Run {
	t.Helper()
	ctx := context.Background()
	r, err := runs.CreateRun(ctx, run.CreateRunParams{
		Repo: "x/y", WorkflowID: "feature_change", WorkflowSHA: "abc",
		TriggerSource: run.TriggerCLI, DecomposedFrom: parent, ParentRunID: parent,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if state == run.StatePending {
		return r
	}
	if r, err = runs.TransitionRun(ctx, r.ID, run.StateRunning); err != nil {
		t.Fatalf("run -> running: %v", err)
	}
	if state != run.StateRunning {
		if r, err = runs.TransitionRun(ctx, r.ID, state); err != nil {
			t.Fatalf("run -> %s: %v", state, err)
		}
	}
	return r
}

// verifyChain recomputes every entry hash on runID's chain and checks the
// prev_hash links.
func verifyChain(t *testing.T, au audit.Repository, runID uuid.UUID) []*audit.Entry {
	t.Helper()
	entries, err := au.ListForRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("list chain: %v", err)
	}
	var prev *string
	for i, e := range entries {
		if (prev == nil) != (e.PrevHash == nil) || (prev != nil && *prev != *e.PrevHash) {
			t.Fatalf("entry %d prev_hash = %v, want %v", i, e.PrevHash, prev)
		}
		h, err := audit.ComputeEntryHash(audit.HashInputs{
			RunID: e.RunID, StageID: e.StageID, Timestamp: e.Timestamp, Category: e.Category,
			ActorKind: e.ActorKind, ActorSubject: e.ActorSubject, Payload: e.Payload, PrevHash: e.PrevHash,
		})
		if err != nil || h != e.EntryHash {
			t.Fatalf("entry %d hash = %q (err %v), stored %q", i, h, err, e.EntryHash)
		}
		eh := e.EntryHash
		prev = &eh
	}
	return entries
}

// TestPostgres_CascadeFromParent_CancelsChildrenAndAppendsChainedRows spans the
// real run state machine, the real chained audit store and the
// DedupedChainAppender capability: a cancelled parent's pending and running
// children are cancelled, a terminal child is skipped, every child chain
// verifies, and a replay from a STALE running snapshot (the racing-sink
// shape: the skip guard cannot fire and the same-state TransitionRun succeeds)
// is rejected by the deduped append under the run-row lock.
func TestPostgres_CascadeFromParent_CancelsChildrenAndAppendsChainedRows(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	runs := run.NewPostgresRepository(pool)
	au := audit.NewPostgresRepository(pool)
	if _, ok := au.(audit.DedupedChainAppender); !ok {
		t.Fatal("postgres audit repository lost the DedupedChainAppender capability")
	}

	parent := pgCreateRun(t, runs, nil, run.StateRunning)
	pending := pgCreateRun(t, runs, &parent.ID, run.StatePending)
	running := pgCreateRun(t, runs, &parent.ID, run.StateRunning)
	succeeded := pgCreateRun(t, runs, &parent.ID, run.StateSucceeded)
	impl, err := runs.CreateStage(ctx, run.CreateStageParams{
		RunID: running.ID, Sequence: 0, Type: run.StageTypeImplement,
		ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
	})
	if err != nil {
		t.Fatalf("create implement stage: %v", err)
	}
	staleRunning := *running // snapshot taken before the cascade

	if parent, err = runs.TransitionRun(ctx, parent.ID, run.StateCancelled); err != nil {
		t.Fatalf("cancel parent: %v", err)
	}
	results, err := CascadeFromParent(ctx, runs, au, parent, "operator_cancel")
	if err != nil {
		t.Fatalf("CascadeFromParent: %v", err)
	}
	outcomes := map[uuid.UUID]Outcome{}
	for _, r := range results {
		if r.Err != nil {
			t.Errorf("child %s err: %v", r.ChildRunID, r.Err)
		}
		outcomes[r.ChildRunID] = r.Outcome
	}
	want := map[uuid.UUID]Outcome{pending.ID: OutcomeCancelled, running.ID: OutcomeCancelled, succeeded.ID: OutcomeSkippedTerminal}
	for id, o := range want {
		if outcomes[id] != o {
			t.Errorf("child %s outcome = %q, want %q", id, outcomes[id], o)
		}
	}

	for id, wantState := range map[uuid.UUID]run.State{pending.ID: run.StateCancelled, running.ID: run.StateCancelled, succeeded.ID: run.StateSucceeded} {
		got, err := runs.GetRun(ctx, id)
		if err != nil || got.State != wantState {
			t.Errorf("child %s re-read state = %v (err %v), want %s", id, got, err, wantState)
		}
	}

	rowsOf := func(id uuid.UUID) []*audit.Entry {
		t.Helper()
		var out []*audit.Entry
		for _, e := range verifyChain(t, au, id) {
			if e.Category == Category {
				out = append(out, e)
			}
		}
		return out
	}
	for _, id := range []uuid.UUID{pending.ID, running.ID} {
		rows := rowsOf(id)
		if len(rows) != 1 {
			t.Fatalf("child %s: %d %s rows, want 1", id, len(rows), Category)
		}
		var p map[string]any
		_ = json.Unmarshal(rows[0].Payload, &p)
		if p["parent_run_id"] != parent.ID.String() || p["reason"] != ReasonParentCancelled || p["cancel_source"] != "operator_cancel" ||
			rows[0].ActorKind == nil || *rows[0].ActorKind != audit.ActorSystem {
			t.Errorf("child %s row = %v actor %v", id, p, rows[0].ActorKind)
		}
		if id == running.ID && (p["implement_stage_id"] != impl.ID.String() || p["implement_stage_state"] != string(run.StageStatePending)) {
			t.Errorf("running child row stage = %v / %v, want %s / pending", p["implement_stage_id"], p["implement_stage_state"], impl.ID)
		}
	}
	if rows := rowsOf(succeeded.ID); len(rows) != 0 {
		t.Errorf("succeeded child rows = %d, want 0", len(rows))
	}

	// Replay from the stale running snapshot: the transition is a same-state
	// success, so only the deduped append keeps the chain at one row.
	replay := CancelChild(ctx, runs, au, &staleRunning, parent.ID, parent.State, ReasonParentCancelled, "run_budget_exceeded")
	if replay.Outcome != OutcomeCancelled || replay.Appended || replay.Err != nil {
		t.Errorf("replay = %+v, want cancelled, not appended, no error", replay)
	}
	if rows := rowsOf(running.ID); len(rows) != 1 {
		t.Errorf("after replay: %d rows, want 1 (deduped under the run-row lock)", len(rows))
	}
}

// TestPostgres_FindOrphansAndReconcile: the backfill core over real Postgres —
// only children of cancelled/succeeded parents are found, Reconcile cancels
// them, and a second scan finds nothing.
func TestPostgres_FindOrphansAndReconcile(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	runs := run.NewPostgresRepository(pool)
	au := audit.NewPostgresRepository(pool)

	cancelled := pgCreateRun(t, runs, nil, run.StateCancelled)
	failed := pgCreateRun(t, runs, nil, run.StateFailed)
	orphan := pgCreateRun(t, runs, &cancelled.ID, run.StateRunning)
	kept := pgCreateRun(t, runs, &failed.ID, run.StatePending)

	orphans, err := FindOrphans(ctx, runs)
	if err != nil {
		t.Fatalf("FindOrphans: %v", err)
	}
	if len(orphans) != 1 || orphans[0].Child.ID != orphan.ID {
		t.Fatalf("orphans = %+v, want only %s", orphans, orphan.ID)
	}
	res := Reconcile(ctx, runs, au, orphans)
	if len(res) != 1 || res[0].Outcome != OutcomeCancelled || !res[0].Appended {
		t.Fatalf("Reconcile = %+v", res)
	}
	if again, err := FindOrphans(ctx, runs); err != nil || len(again) != 0 {
		t.Errorf("second FindOrphans = (%v, %v), want none", again, err)
	}
	if got, _ := runs.GetRun(ctx, kept.ID); got.State != run.StatePending {
		t.Errorf("failed parent's child state = %q, want pending", got.State)
	}
	verifyChain(t, au, orphan.ID)
}
