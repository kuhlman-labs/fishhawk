package run_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// livenessLister type-asserts the concrete postgres repo to the optional
// run.DispatchLivenessLister capability — NOT part of run.Repository, exactly
// how the dispatch watchdog reaches it.
func livenessLister(t *testing.T, repo run.Repository) run.DispatchLivenessLister {
	t.Helper()
	l, ok := repo.(run.DispatchLivenessLister)
	if !ok {
		t.Fatalf("postgres repo does not implement run.DispatchLivenessLister")
	}
	return l
}

// dispatchStage drives a freshly-created (pending) stage into the dispatched
// state through the real repo, so migration 0072's transition-keyed trigger
// stamps dispatched_at.
func dispatchStage(t *testing.T, repo run.Repository, stageID uuid.UUID) {
	t.Helper()
	if _, err := repo.TransitionStage(context.Background(), stageID, run.StageStateDispatched, nil); err != nil {
		t.Fatalf("transition to dispatched: %v", err)
	}
}

// rawDispatchedAt reads the committed stages.dispatched_at directly, bypassing
// the domain layer, so a test asserts on persisted state rather than a return
// value.
func rawDispatchedAt(t *testing.T, pool *pgxpool.Pool, stageID uuid.UUID) *time.Time {
	t.Helper()
	var ts *time.Time
	if err := pool.QueryRow(context.Background(), `SELECT dispatched_at FROM stages WHERE id = $1`, stageID).Scan(&ts); err != nil {
		t.Fatalf("read dispatched_at: %v", err)
	}
	return ts
}

func livenessFor(t *testing.T, rows []run.DispatchedStageLiveness, stageID uuid.UUID) run.DispatchedStageLiveness {
	t.Helper()
	for _, l := range rows {
		if l.StageID == stageID {
			return l
		}
	}
	t.Fatalf("stage %s not in dispatched-liveness list", stageID)
	return run.DispatchedStageLiveness{}
}

// TestDispatchedAt_StampedOnTransitionToDispatched pins the trigger's happy
// path: a stage transitioned into dispatched has a non-nil dispatched_at, and
// the liveness list reflects it.
func TestDispatchedAt_StampedOnTransitionToDispatched(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	repo := run.NewPostgresRepository(pool)
	lister := livenessLister(t, repo)

	r := makeRun(t, repo)
	s := makeStage(t, repo, r.ID, 0)
	// Before dispatch the column is NULL (a pending stage is not in the list).
	if got := rawDispatchedAt(t, pool, s.ID); got != nil {
		t.Errorf("dispatched_at before dispatch = %v, want nil", got)
	}

	dispatchStage(t, repo, s.ID)

	if got := rawDispatchedAt(t, pool, s.ID); got == nil {
		t.Fatal("dispatched_at after transition = nil, want a stamp")
	}
	rows, err := lister.ListDispatchedStageLiveness(ctx)
	if err != nil {
		t.Fatalf("ListDispatchedStageLiveness: %v", err)
	}
	l := livenessFor(t, rows, s.ID)
	if l.DispatchedAt == nil {
		t.Error("liveness DispatchedAt = nil, want the stamp")
	}
	if l.RunID != r.ID {
		t.Errorf("liveness RunID = %s, want %s", l.RunID, r.ID)
	}
	if l.LastHeartbeatAt != nil {
		t.Errorf("liveness LastHeartbeatAt = %v, want nil (no heartbeat yet)", l.LastHeartbeatAt)
	}
}

// TestDispatchedAt_NotBumpedByProgressHeartbeat is the LOAD-BEARING pin: a
// progress-only heartbeat ADVANCES updated_at (the 0001 trigger) but leaves
// dispatched_at byte-identical (the 0072 trigger's transition predicate is
// false). It reads COMMITTED state after each write, not a return value. This is
// the test that goes RED if the trigger's `OLD.state IS DISTINCT FROM
// 'dispatched'` clause is dropped (approach step 12b).
func TestDispatchedAt_NotBumpedByProgressHeartbeat(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	repo := run.NewPostgresRepository(pool)
	store := progressStore(t, repo)

	r := makeRun(t, repo)
	s := makeStage(t, repo, r.ID, 0)
	dispatchStage(t, repo, s.ID)

	dispatchedBefore := rawDispatchedAt(t, pool, s.ID)
	if dispatchedBefore == nil {
		t.Fatal("dispatched_at nil after dispatch")
	}
	var updatedBefore time.Time
	if err := pool.QueryRow(ctx, `SELECT updated_at FROM stages WHERE id = $1`, s.ID).Scan(&updatedBefore); err != nil {
		t.Fatalf("read updated_at: %v", err)
	}

	// Force a distinct transaction clock so the updated_at advance is
	// unambiguous, then heartbeat.
	time.Sleep(2 * time.Millisecond)
	applied, err := store.RecordStageProgress(ctx, s.ID, run.StageProgress{
		LastEvent:  "assistant",
		ReportedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("RecordStageProgress: %v", err)
	}
	if !applied {
		t.Fatal("heartbeat not applied on a dispatched stage")
	}

	dispatchedAfter := rawDispatchedAt(t, pool, s.ID)
	if dispatchedAfter == nil {
		t.Fatal("dispatched_at nil after heartbeat")
	}
	var updatedAfter time.Time
	if err := pool.QueryRow(ctx, `SELECT updated_at FROM stages WHERE id = $1`, s.ID).Scan(&updatedAfter); err != nil {
		t.Fatalf("read updated_at (after): %v", err)
	}

	if !updatedAfter.After(updatedBefore) {
		t.Errorf("updated_at did not advance on a heartbeat: before=%v after=%v", updatedBefore, updatedAfter)
	}
	if !dispatchedAfter.Equal(*dispatchedBefore) {
		t.Errorf("dispatched_at MOVED on a progress heartbeat: before=%v after=%v (the transition-keyed trigger predicate is the control that forbids this)", dispatchedBefore, dispatchedAfter)
	}
}

// TestDispatchedAt_RestampedOnRedispatch pins the retry budget: a re-dispatch
// (dispatched → running → awaiting_input → pending → dispatched) RE-stamps
// dispatched_at FORWARD, so a retried stage gets a fresh full budget.
func TestDispatchedAt_RestampedOnRedispatch(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	repo := run.NewPostgresRepository(pool)

	r := makeRun(t, repo)
	s := makeStage(t, repo, r.ID, 0)
	dispatchStage(t, repo, s.ID)
	first := rawDispatchedAt(t, pool, s.ID)
	if first == nil {
		t.Fatal("first dispatched_at nil")
	}

	time.Sleep(2 * time.Millisecond)
	// Walk back to a re-dispatchable state and dispatch again.
	for _, to := range []run.StageState{
		run.StageStateRunning,
		run.StageStateAwaitingInput,
		run.StageStatePending,
		run.StageStateDispatched,
	} {
		if _, err := repo.TransitionStage(ctx, s.ID, to, nil); err != nil {
			t.Fatalf("transition to %s: %v", to, err)
		}
	}

	second := rawDispatchedAt(t, pool, s.ID)
	if second == nil {
		t.Fatal("second dispatched_at nil")
	}
	if !second.After(*first) {
		t.Errorf("dispatched_at not re-stamped forward on re-dispatch: first=%v second=%v", first, second)
	}
}

// TestDispatchedAt_RedispatchWithStaleHeartbeatReportsNeverCheckedIn is the
// #2744 CONDITION 1 retry-path pin: a heartbeat from the PREVIOUS dispatch
// attempt must NOT classify a fresh, un-checked-in attempt as
// wedged_after_checkin. After a re-dispatch, the stale heartbeat predates the
// new dispatch, so the attempt-relative read reports LastHeartbeatAt nil
// (never_checked_in), not the leftover heartbeat.
func TestDispatchedAt_RedispatchWithStaleHeartbeatReportsNeverCheckedIn(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	repo := run.NewPostgresRepository(pool)
	store := progressStore(t, repo)
	lister := livenessLister(t, repo)

	r := makeRun(t, repo)
	s := makeStage(t, repo, r.ID, 0)

	// Attempt 1: dispatch and check in with a heartbeat stamped in the past.
	dispatchStage(t, repo, s.ID)
	staleHeartbeat := time.Now().UTC().Add(-time.Hour)
	if _, err := store.RecordStageProgress(ctx, s.ID, run.StageProgress{LastEvent: "assistant", ReportedAt: staleHeartbeat}); err != nil {
		t.Fatalf("record attempt-1 heartbeat: %v", err)
	}

	// Transition away and re-dispatch (attempt 2), WITHOUT a new heartbeat.
	time.Sleep(2 * time.Millisecond)
	for _, to := range []run.StageState{
		run.StageStateRunning,
		run.StageStateAwaitingInput,
		run.StageStatePending,
		run.StageStateDispatched,
	} {
		if _, err := repo.TransitionStage(ctx, s.ID, to, nil); err != nil {
			t.Fatalf("transition to %s: %v", to, err)
		}
	}

	rows, err := lister.ListDispatchedStageLiveness(ctx)
	if err != nil {
		t.Fatalf("ListDispatchedStageLiveness: %v", err)
	}
	l := livenessFor(t, rows, s.ID)
	if l.DispatchedAt == nil {
		t.Fatal("re-dispatched stage DispatchedAt nil")
	}
	if l.LastHeartbeatAt != nil {
		t.Errorf("LastHeartbeatAt = %v on a fresh un-checked-in attempt, want nil (a stale prior-attempt heartbeat must not read as the current attempt's check-in)", l.LastHeartbeatAt)
	}
}

// TestListDispatchedStageLiveness_MapsHeartbeatReportedAt: a heartbeat recorded
// AFTER the current dispatch surfaces as LastHeartbeatAt equal to its
// ReportedAt, alongside both timestamps and the run id.
func TestListDispatchedStageLiveness_MapsHeartbeatReportedAt(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	repo := run.NewPostgresRepository(pool)
	store := progressStore(t, repo)
	lister := livenessLister(t, repo)

	r := makeRun(t, repo)
	s := makeStage(t, repo, r.ID, 0)
	dispatchStage(t, repo, s.ID)

	reportedAt := time.Now().UTC().Truncate(time.Millisecond)
	if _, err := store.RecordStageProgress(ctx, s.ID, run.StageProgress{LastEvent: "tool_use", ReportedAt: reportedAt}); err != nil {
		t.Fatalf("record heartbeat: %v", err)
	}

	rows, err := lister.ListDispatchedStageLiveness(ctx)
	if err != nil {
		t.Fatalf("ListDispatchedStageLiveness: %v", err)
	}
	l := livenessFor(t, rows, s.ID)
	if l.LastHeartbeatAt == nil {
		t.Fatal("LastHeartbeatAt = nil after a fresh heartbeat")
	}
	if !l.LastHeartbeatAt.Equal(reportedAt) {
		t.Errorf("LastHeartbeatAt = %v, want %v (the recorded ReportedAt)", l.LastHeartbeatAt, reportedAt)
	}
	if l.RunID != r.ID {
		t.Errorf("RunID = %s, want %s", l.RunID, r.ID)
	}
	if l.UpdatedAt.IsZero() {
		t.Error("UpdatedAt is zero, want the row's updated_at")
	}
}

// TestListDispatchedStageLiveness_NilHeartbeatWithoutProgress: a dispatched
// stage with no heartbeat yields LastHeartbeatAt nil.
func TestListDispatchedStageLiveness_NilHeartbeatWithoutProgress(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	repo := run.NewPostgresRepository(pool)
	lister := livenessLister(t, repo)

	r := makeRun(t, repo)
	s := makeStage(t, repo, r.ID, 0)
	dispatchStage(t, repo, s.ID)

	rows, err := lister.ListDispatchedStageLiveness(ctx)
	if err != nil {
		t.Fatalf("ListDispatchedStageLiveness: %v", err)
	}
	if l := livenessFor(t, rows, s.ID); l.LastHeartbeatAt != nil {
		t.Errorf("LastHeartbeatAt = %v with no recorded progress, want nil", l.LastHeartbeatAt)
	}
}

// TestListDispatchedStageLiveness_UndecodableProgressDegradesToNil: a corrupt
// stored payload reads back as LastHeartbeatAt nil rather than erroring the read
// (fail-open on READ), mirroring StageProgress decode.
func TestListDispatchedStageLiveness_UndecodableProgressDegradesToNil(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	repo := run.NewPostgresRepository(pool)
	lister := livenessLister(t, repo)

	r := makeRun(t, repo)
	s := makeStage(t, repo, r.ID, 0)
	dispatchStage(t, repo, s.ID)

	// Valid JSONB but not a StageProgress object.
	if _, err := pool.Exec(ctx, `UPDATE stages SET progress = $2 WHERE id = $1`, s.ID, []byte(`[1,2,3]`)); err != nil {
		t.Fatalf("seed corrupt payload: %v", err)
	}

	rows, err := lister.ListDispatchedStageLiveness(ctx)
	if err != nil {
		t.Fatalf("ListDispatchedStageLiveness must not error on an undecodable payload: %v", err)
	}
	if l := livenessFor(t, rows, s.ID); l.LastHeartbeatAt != nil {
		t.Errorf("LastHeartbeatAt = %v for an undecodable payload, want nil (fail-open read)", l.LastHeartbeatAt)
	}
}
