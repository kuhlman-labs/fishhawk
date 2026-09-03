package run_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
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

// dbAnchoredHeartbeat returns a heartbeat instant derived from the stage row's
// OWN DB-stamped dispatched_at, offset by the caller's (timescale-scaled)
// delta. It is the fix for #3048: seeding a comparative heartbeat from the Go
// process's time.Now() puts the two sides of ListDispatchedStageLiveness's
// attempt-relative hb.Before(*l.DispatchedAt) comparison in DIFFERENT clock
// domains — the host's and the Postgres container's — and host/container skew
// is unbounded, so no margin makes the ordering safe. Anchoring the seed to the
// value it will be compared against puts both sides in ONE clock domain, and
// the ordering then holds BY CONSTRUCTION at any skew; timescale.D supplies the
// offset per the AGENTS.md rule.
//
// NO TRUNCATION is applied to the anchor, and that is deliberate. Measured
// inside pgtest's own postgres:16-alpine container: Postgres now() stamps at
// MICROSECOND resolution (dispatched_at=2026-08-31T18:22:40.629627Z, raw ::text
// "2026-08-31 18:22:40.629627+00", Nanosecond()%1000 == 0), and the
// stages.progress JSONB round-trip is LOSSLESS — it neither truncates nor
// rounds. That same instant marshalled to "reported_at":
// "2026-08-31T18:22:40.629627Z", decoded back Equal with delta 0s, and a
// deliberate sub-microsecond probe (2026-08-31T12:00:00.123456789Z) round-tripped
// byte-identical, delta 0s. The mechanism is why: a time.Time marshals to an
// RFC3339Nano STRING and jsonb stores JSON strings verbatim, so there is no
// timestamptz coercion in that path to truncate or round at all.
//
// That losslessness is what makes offset == 0 name the dispatch instant
// EXACTLY, and therefore what makes the equal-instant boundary expressible at
// all (see TestListDispatchedStageLiveness_HeartbeatEqualToDispatchIsCurrent).
// A truncating helper would push a 0-offset seed up to 999µs EARLIER than the
// row's real stamp, hb.Before would be true, and the boundary test would fail
// deterministically.
//
// A NULL dispatched_at means the anchor is unavailable and the caller's premise
// is void, so this fails loudly rather than degrading to the host clock — which
// is precisely the dependency being removed.
func dbAnchoredHeartbeat(t *testing.T, pool *pgxpool.Pool, stageID uuid.UUID, offset time.Duration) time.Time {
	t.Helper()
	d := rawDispatchedAt(t, pool, stageID)
	if d == nil {
		t.Fatalf("dispatched_at is NULL for stage %s: cannot anchor a heartbeat to the row's own dispatch clock", stageID)
	}
	return d.UTC().Add(offset)
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
	// This ReportedAt is deliberately NOT DB-anchored (#3048 leaves it alone).
	// It is a NON-COMPARATIVE fixture: this test never reads LastHeartbeatAt and
	// never compares the seeded value against dispatched_at — it asserts only
	// that updated_at advanced and dispatched_at did not. There is no
	// cross-clock dependency here, so anchoring it would be churn in code that
	// was never at risk. A future blanket "no time.Now() in this file" grep
	// should read this comment rather than "fix" the line.
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
	// #3048: DB-anchored, against ATTEMPT 1's stamp — taken BEFORE the
	// re-dispatch below re-stamps dispatched_at forward, which is exactly the
	// ordering this test needs (the seed must predate the SECOND stamp, and it
	// predates the FIRST by a scaled hour). A one-hour margin dwarfs any
	// plausible skew, but skew is unbounded in principle, so the anchor makes
	// the stale direction hold by construction rather than by margin.
	staleHeartbeat := dbAnchoredHeartbeat(t, pool, s.ID, -timescale.D(time.Hour))
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

	// #3048: the seed is DB-ANCHORED — derived from this row's own DB-stamped
	// dispatched_at rather than the host's time.Now() — so the assertion below
	// no longer depends on host/container clock agreement.
	reportedAt := dbAnchoredHeartbeat(t, pool, s.ID, timescale.D(time.Second))
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
	// Doubles as an in-test proof that the stages.progress JSONB round-trip is
	// lossless: reportedAt carries the row's full microsecond-resolution stamp.
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

// TestListDispatchedStageLiveness_HeartbeatEqualToDispatchIsCurrent pins the
// EQUAL-INSTANT boundary of the attempt-relative filter: a heartbeat reported at
// EXACTLY the dispatch instant belongs to the CURRENT attempt, not a prior one.
// That is the difference between the shipped `!hb.Before(*l.DispatchedAt)` and
// the strict spelling `hb.After(*l.DispatchedAt)`.
//
// The offset is ZERO deliberately and MUST STAY ZERO. A strictly-positive offset
// (+1ms, +1µs, anything) passes under BOTH spellings, so the test would go green
// while pinning nothing — a control that cannot fail, which is worse than no
// boundary test because it also suppresses the signal that the boundary is
// untested. Offset 0 is only expressible because the round-trip is lossless (see
// dbAnchoredHeartbeat). Proof of discriminating power is the manual
// counterfactual recorded in the PR body: weakening the production filter to
// `hb.After` turns THIS test red while the stale-heartbeat test stays green.
func TestListDispatchedStageLiveness_HeartbeatEqualToDispatchIsCurrent(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	repo := run.NewPostgresRepository(pool)
	store := progressStore(t, repo)
	lister := livenessLister(t, repo)

	r := makeRun(t, repo)
	s := makeStage(t, repo, r.ID, 0)
	dispatchStage(t, repo, s.ID)

	hb := dbAnchoredHeartbeat(t, pool, s.ID, 0)
	if _, err := store.RecordStageProgress(ctx, s.ID, run.StageProgress{LastEvent: "tool_use", ReportedAt: hb}); err != nil {
		t.Fatalf("record equal-instant heartbeat: %v", err)
	}

	rows, err := lister.ListDispatchedStageLiveness(ctx)
	if err != nil {
		t.Fatalf("ListDispatchedStageLiveness: %v", err)
	}
	l := livenessFor(t, rows, s.ID)
	// The equality premise depends on dispatched_at NOT being re-stamped by the
	// progress-only UPDATE. Re-assert it so a future trigger change that broke
	// the premise fails loudly here instead of making this test silently vacuous.
	if l.DispatchedAt == nil {
		t.Fatal("DispatchedAt = nil after an equal-instant heartbeat")
	}
	if !l.DispatchedAt.Equal(hb) {
		t.Fatalf("dispatched_at MOVED under a progress-only heartbeat: anchor=%v now=%v (the equal-instant premise is void)", hb, l.DispatchedAt)
	}
	if l.LastHeartbeatAt == nil {
		t.Fatal("LastHeartbeatAt = nil at the exact dispatch instant, want the heartbeat (a check-in AT the dispatch instant belongs to the current attempt)")
	}
	if !l.LastHeartbeatAt.Equal(hb) {
		t.Errorf("LastHeartbeatAt = %v, want %v (the equal-instant ReportedAt)", l.LastHeartbeatAt, hb)
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
