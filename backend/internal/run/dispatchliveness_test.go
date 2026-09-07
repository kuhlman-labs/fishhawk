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

// rawReportedAt reads the committed stages.progress -> reported_at directly,
// bypassing the domain decode, so a test can assert against the value Postgres
// actually stored rather than one a caller supplied.
func rawReportedAt(t *testing.T, pool *pgxpool.Pool, stageID uuid.UUID) time.Time {
	t.Helper()
	var ts time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT (progress->>'reported_at')::timestamptz FROM stages WHERE id = $1`, stageID).Scan(&ts); err != nil {
		t.Fatalf("read stored reported_at: %v", err)
	}
	return ts.UTC()
}

// dbAnchoredHeartbeat returns an instant derived from the stage row's OWN
// DB-stamped dispatched_at, offset by the caller's (timescale-scaled) delta,
// so both sides of any comparison against dispatched_at sit in ONE clock domain
// and the ordering holds BY CONSTRUCTION at any host/container skew (#3048;
// timescale.D supplies the offset per the AGENTS.md rule).
//
// ITS ROLE NARROWED with #3084. The PRODUCTION write is now DB-stamped, so
// routing an anchored instant through store.RecordStageProgress no longer seeds
// anything — jsonb_set overwrites it. What remains is two uses: supplying
// DB-anchored instants to the RAW seeder (seedProgressAt) for the boundary and
// prior-attempt fixtures, and supplying a deliberately SKEWED caller value to
// the real store in the skew tests, where the point is precisely that it is
// discarded.
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
	// No ReportedAt is passed: since #3084 the write stamps it in the DATABASE
	// and any caller value is discarded. This test never reads LastHeartbeatAt
	// anyway — it asserts only that updated_at advanced and dispatched_at did
	// not, which the jsonb_set does not touch.
	applied, err := store.RecordStageProgress(ctx, s.ID, run.StageProgress{
		LastEvent: "assistant",
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

	// Attempt 1: dispatch and check in NATURALLY — no backdated seed. Since
	// #3084 the write stamps reported_at from the DATABASE clock, and the
	// re-dispatch below re-stamps dispatched_at forward from that SAME clock, so
	// the prior-attempt ordering (heartbeat < second dispatch) holds BY
	// CONSTRUCTION. The #3048 anchoring this used to need is subsumed: there is
	// no longer a second clock to anchor against.
	dispatchStage(t, repo, s.ID)
	if _, err := store.RecordStageProgress(ctx, s.ID, run.StageProgress{LastEvent: "assistant"}); err != nil {
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
// AFTER the current dispatch surfaces as LastHeartbeatAt, alongside both
// timestamps and the run id.
//
// Since #3084 it can no longer assert equality against a CALLER-supplied value
// (the DB stamps it), so it asserts against the row's OWN stored reported_at
// read straight out of the JSONB. That doubles as the round-trip proof that
// Postgres's to_jsonb(now()) rendering parses back into a Go time.Time without
// loss — an offset-less rendering would fail the decode and read back nil.
func TestListDispatchedStageLiveness_MapsHeartbeatReportedAt(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	repo := run.NewPostgresRepository(pool)
	store := progressStore(t, repo)
	lister := livenessLister(t, repo)

	r := makeRun(t, repo)
	s := makeStage(t, repo, r.ID, 0)
	dispatchStage(t, repo, s.ID)

	if _, err := store.RecordStageProgress(ctx, s.ID, run.StageProgress{LastEvent: "tool_use"}); err != nil {
		t.Fatalf("record heartbeat: %v", err)
	}
	dispatchedAt := rawDispatchedAt(t, pool, s.ID)
	if dispatchedAt == nil {
		t.Fatal("dispatched_at nil after dispatch")
	}
	stored := rawReportedAt(t, pool, s.ID)

	rows, err := lister.ListDispatchedStageLiveness(ctx)
	if err != nil {
		t.Fatalf("ListDispatchedStageLiveness: %v", err)
	}
	l := livenessFor(t, rows, s.ID)
	if l.LastHeartbeatAt == nil {
		t.Fatal("LastHeartbeatAt = nil after a fresh heartbeat")
	}
	if l.LastHeartbeatAt.Before(*dispatchedAt) {
		t.Errorf("LastHeartbeatAt = %v is before dispatched_at = %v", l.LastHeartbeatAt, dispatchedAt)
	}
	if !l.LastHeartbeatAt.Equal(stored) {
		t.Errorf("LastHeartbeatAt = %v, want the row's stored reported_at %v (the to_jsonb(now()) rendering must round-trip losslessly)", l.LastHeartbeatAt, stored)
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
// `hb.After` turns THIS test red while the prior-attempt tests stay green.
//
// SEEDED RAW since #3084. The production write now stamps reported_at from the
// database, so it can no longer land at EXACTLY the dispatch instant and no
// caller can ask it to. seedProgressAt writes the row's own dispatched_at into
// reported_at directly — both sides still DB-clocked, offset still exactly zero.
func TestListDispatchedStageLiveness_HeartbeatEqualToDispatchIsCurrent(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	repo := run.NewPostgresRepository(pool)
	lister := livenessLister(t, repo)

	r := makeRun(t, repo)
	s := makeStage(t, repo, r.ID, 0)
	dispatchStage(t, repo, s.ID)

	hb := dbAnchoredHeartbeat(t, pool, s.ID, 0)
	seedProgressAt(t, pool, s.ID, hb)

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

// seedProgressAt writes a progress payload with a caller-named reported_at
// DIRECTLY to the column, bypassing store.RecordStageProgress.
//
// It exists because the production write is now DATABASE-stamped (#3084):
// RecordStageProgress's jsonb_set overwrites reported_at with Postgres now(),
// so no caller — including a test — can name a heartbeat instant through the
// domain method any more. That is the point of the change, but two controls in
// this file NEED to name one (the equal-instant boundary, and a prior-attempt
// fixture). Both seed through here instead. The instant is written as a
// timestamptz rendered by to_jsonb, i.e. the same rendering the production
// UPDATE produces, so the read-side decode under test is the real one.
func seedProgressAt(t *testing.T, pool *pgxpool.Pool, stageID uuid.UUID, instant time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE stages
		    SET progress = jsonb_build_object(
		          'last_event', 'assistant',
		          'turns_this_attempt', 0,
		          'tokens_this_attempt', 0,
		          'reported_at', to_jsonb($2::timestamptz))
		  WHERE id = $1`, stageID, instant); err != nil {
		t.Fatalf("seed progress at %v: %v", instant, err)
	}
}

// TestListDispatchedStageLiveness_BackendClockLagDoesNotHideAFreshHeartbeat is
// the #3084 acceptance criterion: a backend host whose clock LAGS the database
// must not make a genuinely fresh heartbeat read as absent.
//
// The caller stamps an hour BEFORE the row's own dispatch instant — exactly the
// shape a lagging backend produces at ingest (the magnitude is exaggerated from
// ordinary NTP skew for determinism; the DIRECTION is the faithful part). With
// the stamp taken in the DATABASE the caller's value is discarded, so the
// heartbeat lands at-or-after dispatched_at and the stage classifies
// wedged_after_checkin rather than never_checked_in.
//
// Observed RED against pre-change code (the -1h value persisted verbatim,
// hb.Before was true, LastHeartbeatAt was nil).
func TestListDispatchedStageLiveness_BackendClockLagDoesNotHideAFreshHeartbeat(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	repo := run.NewPostgresRepository(pool)
	store := progressStore(t, repo)
	lister := livenessLister(t, repo)

	r := makeRun(t, repo)
	s := makeStage(t, repo, r.ID, 0)
	dispatchStage(t, repo, s.ID)

	lagging := dbAnchoredHeartbeat(t, pool, s.ID, -timescale.D(time.Hour))
	if _, err := store.RecordStageProgress(ctx, s.ID, run.StageProgress{
		LastEvent:  "assistant",
		ReportedAt: lagging,
	}); err != nil {
		t.Fatalf("record heartbeat from a lagging backend clock: %v", err)
	}

	rows, err := lister.ListDispatchedStageLiveness(ctx)
	if err != nil {
		t.Fatalf("ListDispatchedStageLiveness: %v", err)
	}
	l := livenessFor(t, rows, s.ID)
	if l.DispatchedAt == nil {
		t.Fatal("DispatchedAt = nil after dispatch")
	}
	if l.LastHeartbeatAt == nil {
		t.Fatalf("LastHeartbeatAt = nil for a FRESH heartbeat ingested under a backend clock lagging the database by %v — the process stamp reached the column and the attempt-relative filter discarded a live check-in (never_checked_in reported for a wedged_after_checkin stage)", timescale.D(time.Hour))
	}
	if l.LastHeartbeatAt.Before(*l.DispatchedAt) {
		t.Errorf("LastHeartbeatAt = %v is BEFORE dispatched_at = %v: the heartbeat was not stamped by the database", l.LastHeartbeatAt, l.DispatchedAt)
	}
	if !l.LastHeartbeatAt.After(lagging) {
		t.Errorf("LastHeartbeatAt = %v, want the DATABASE instant (the caller's lagging %v must be discarded)", l.LastHeartbeatAt, lagging)
	}
}

// TestListDispatchedStageLiveness_PriorAttemptHeartbeatStaysNeverCheckedInUnderSkewEitherDirection
// is the anti-blanket-weakening pin for #3084: the fix must NOT be "stop
// comparing". A heartbeat from a PREVIOUS dispatch attempt must still read as
// absent after a re-dispatch, under skew in BOTH directions.
//
// The backend-AHEAD arm is the load-bearing one: a fix that weakened or removed
// the attempt-relative comparison would report wedged_after_checkin there, since
// the caller's +1h value would sit after the re-dispatch stamp. With the DB
// stamp, the caller's value never reaches the column at all and the heartbeat
// carries its real (pre-re-dispatch) instant in both arms.
func TestListDispatchedStageLiveness_PriorAttemptHeartbeatStaysNeverCheckedInUnderSkewEitherDirection(t *testing.T) {
	for _, tc := range []struct {
		name   string
		offset time.Duration
	}{
		{"backend behind the database", -timescale.D(time.Hour)},
		{"backend ahead of the database", timescale.D(time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := pgtest.NewPool(t)
			repo := run.NewPostgresRepository(pool)
			store := progressStore(t, repo)
			lister := livenessLister(t, repo)

			r := makeRun(t, repo)
			s := makeStage(t, repo, r.ID, 0)

			// Attempt 1: dispatch and check in, with the caller's process clock
			// skewed by tc.offset relative to the database.
			dispatchStage(t, repo, s.ID)
			skewed := dbAnchoredHeartbeat(t, pool, s.ID, tc.offset)
			if _, err := store.RecordStageProgress(ctx, s.ID, run.StageProgress{
				LastEvent:  "assistant",
				ReportedAt: skewed,
			}); err != nil {
				t.Fatalf("record attempt-1 heartbeat: %v", err)
			}

			// Attempt 2: re-dispatch WITHOUT a new heartbeat, so the 0072
			// trigger re-stamps dispatched_at forward past the recorded one.
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
				t.Errorf("LastHeartbeatAt = %v on a fresh un-checked-in attempt (caller skew %v), want nil — a prior-attempt heartbeat must not read as the current attempt's check-in", l.LastHeartbeatAt, tc.offset)
			}
		})
	}
}
