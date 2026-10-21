package run_test

import (
	"context"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// progressStore type-asserts the concrete postgres repo to the optional
// StageProgressStore capability. It is NOT part of run.Repository, so every
// test reaches it through this assertion — mirroring how the server wires it.
func progressStore(t *testing.T, repo run.Repository) run.StageProgressStore {
	t.Helper()
	store, ok := repo.(run.StageProgressStore)
	if !ok {
		t.Fatalf("postgres repo does not implement run.StageProgressStore")
	}
	return store
}

func TestRecordAndReadStageProgress_RoundTrip(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	repo := run.NewPostgresRepository(pool)
	store := progressStore(t, repo)

	r := makeRun(t, repo)
	s := makeStage(t, repo, r.ID, 0)

	p := run.StageProgress{
		LastEvent:         "assistant",
		TurnsThisAttempt:  9,
		TokensThisAttempt: 13402,
		ReportedAt:        time.Unix(1700000000, 0).UTC(),
	}
	applied, err := store.RecordStageProgress(ctx, s.ID, p)
	if err != nil {
		t.Fatalf("RecordStageProgress: %v", err)
	}
	if !applied {
		t.Fatal("applied = false on a non-terminal stage, want true")
	}

	got, err := store.StageProgressByID(ctx, s.ID)
	if err != nil {
		t.Fatalf("StageProgressByID: %v", err)
	}
	if got == nil {
		t.Fatal("StageProgressByID = nil after a recorded heartbeat")
	}
	if got.LastEvent != "assistant" || got.TurnsThisAttempt != 9 || got.TokensThisAttempt != 13402 {
		t.Errorf("round-trip mismatch: got %+v", *got)
	}

	// A stage that never reported reads back nil, not an error.
	other := makeStage(t, repo, r.ID, 1)
	none, err := store.StageProgressByID(ctx, other.ID)
	if err != nil {
		t.Fatalf("StageProgressByID (unreported): %v", err)
	}
	if none != nil {
		t.Errorf("StageProgressByID (unreported) = %+v, want nil", *none)
	}
}

func TestStageProgressForRun_MapsReportedStages(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	repo := run.NewPostgresRepository(pool)
	store := progressStore(t, repo)

	r := makeRun(t, repo)
	s0 := makeStage(t, repo, r.ID, 0)
	s1 := makeStage(t, repo, r.ID, 1)
	_ = makeStage(t, repo, r.ID, 2) // never reports — must be absent from the map

	if _, err := store.RecordStageProgress(ctx, s0.ID, run.StageProgress{LastEvent: "assistant", TurnsThisAttempt: 3}); err != nil {
		t.Fatalf("record s0: %v", err)
	}
	if _, err := store.RecordStageProgress(ctx, s1.ID, run.StageProgress{LastEvent: "tool_use", TurnsThisAttempt: 7}); err != nil {
		t.Fatalf("record s1: %v", err)
	}

	m, err := store.StageProgressForRun(ctx, r.ID)
	if err != nil {
		t.Fatalf("StageProgressForRun: %v", err)
	}
	if len(m) != 2 {
		t.Fatalf("map size = %d, want 2 (only reported stages)", len(m))
	}
	if m[s0.ID].TurnsThisAttempt != 3 || m[s1.ID].LastEvent != "tool_use" {
		t.Errorf("map mismatch: %+v", m)
	}
}

// TestRecordStageProgress_TerminalStageNotApplied pins the terminal refusal at
// the persistence layer: the UPDATE's WHERE state NOT IN (terminal) predicate
// matches zero rows once the stage settled, so applied is false AND the column
// stays nil. The stage is driven terminal BY CONSTRUCTION through the normal
// transitions, never by the control under test.
func TestRecordStageProgress_TerminalStageNotApplied(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	repo := run.NewPostgresRepository(pool)
	store := progressStore(t, repo)

	r := makeRun(t, repo)
	s := makeStage(t, repo, r.ID, 0)
	// Drive the stage terminal via the normal transition path.
	for _, to := range []run.StageState{run.StageStateDispatched, run.StageStateRunning, run.StageStateSucceeded} {
		if _, err := repo.TransitionStage(ctx, s.ID, to, nil); err != nil {
			t.Fatalf("transition to %s: %v", to, err)
		}
	}

	applied, err := store.RecordStageProgress(ctx, s.ID, run.StageProgress{LastEvent: "assistant", TurnsThisAttempt: 1})
	if err != nil {
		t.Fatalf("RecordStageProgress (terminal): %v", err)
	}
	if applied {
		t.Error("applied = true on a terminal stage, want false (terminal refusal)")
	}
	// Committed state, not just the return: the column was never written.
	got, err := store.StageProgressByID(ctx, s.ID)
	if err != nil {
		t.Fatalf("StageProgressByID: %v", err)
	}
	if got != nil {
		t.Errorf("terminal stage progress = %+v, want nil (write refused)", *got)
	}
}

// TestStageProgressByID_UndecodablePayloadDegradesToNil pins the fail-open READ
// path: a corrupt stored JSONB payload reads back as nil rather than erroring.
// The garbage is written directly to the column (bypassing the marshal path) so
// the decode is exercised.
func TestStageProgressByID_UndecodablePayloadDegradesToNil(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	repo := run.NewPostgresRepository(pool)
	store := progressStore(t, repo)

	r := makeRun(t, repo)
	s := makeStage(t, repo, r.ID, 0)

	// A JSON value that is valid JSONB but NOT a StageProgress object — a bare
	// array decodes into the struct with an error.
	if _, err := pool.Exec(ctx, `UPDATE stages SET progress = $2 WHERE id = $1`, s.ID, []byte(`[1,2,3]`)); err != nil {
		t.Fatalf("seed corrupt payload: %v", err)
	}

	got, err := store.StageProgressByID(ctx, s.ID)
	if err != nil {
		t.Fatalf("StageProgressByID must not error on an undecodable payload: %v", err)
	}
	if got != nil {
		t.Errorf("undecodable payload = %+v, want nil (fail-open read)", *got)
	}

	// The run-level read likewise omits the stage rather than erroring.
	m, err := store.StageProgressForRun(ctx, r.ID)
	if err != nil {
		t.Fatalf("StageProgressForRun must not error on an undecodable payload: %v", err)
	}
	if _, present := m[s.ID]; present {
		t.Errorf("undecodable stage present in run map: %+v", m)
	}
}

// TestRecordStageProgress_ReportedAtIsDatabaseStamped is the DONE-MEANS pin for
// #3084: the stored reported_at is the DATABASE's instant, and a caller-supplied
// value is DISCARDED.
//
// The caller seeds the 1970 epoch — an instant no clock in any deployment
// produces — so a pass cannot be a coincidence of the two clocks happening to
// agree. It also doubles as the to_jsonb RENDERING guard (approval condition 3):
// Postgres renders a timestamptz inside jsonb as ISO-8601 WITH an offset, which
// Go's RFC3339 unmarshal accepts. Were the spelling changed to
// `to_jsonb(now() AT TIME ZONE 'UTC')` the rendering would carry NO offset,
// RFC3339 would reject it, and the fail-open decode would degrade the read to
// nil — which this test fails on.
func TestRecordStageProgress_ReportedAtIsDatabaseStamped(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	repo := run.NewPostgresRepository(pool)
	store := progressStore(t, repo)

	r := makeRun(t, repo)
	s := makeStage(t, repo, r.ID, 0)

	epoch := time.Unix(0, 0).UTC()
	applied, err := store.RecordStageProgress(ctx, s.ID, run.StageProgress{
		LastEvent:  "assistant",
		ReportedAt: epoch,
	})
	if err != nil {
		t.Fatalf("RecordStageProgress: %v", err)
	}
	if !applied {
		t.Fatal("applied = false on a non-terminal stage, want true")
	}

	// The database's own clock, read in the same domain the stamp came from.
	var dbNow time.Time
	if err := pool.QueryRow(ctx, `SELECT now()`).Scan(&dbNow); err != nil {
		t.Fatalf("read database now(): %v", err)
	}

	got, err := store.StageProgressByID(ctx, s.ID)
	if err != nil {
		t.Fatalf("StageProgressByID: %v", err)
	}
	if got == nil {
		t.Fatal("StageProgressByID = nil after a recorded heartbeat — the stored reported_at did not decode (to_jsonb rendering must stay RFC3339-parseable; `to_jsonb(now() AT TIME ZONE 'UTC')` renders without an offset and fails here)")
	}
	if got.ReportedAt.Equal(epoch) {
		t.Fatalf("stored ReportedAt = %v, the caller's value verbatim — the database did not stamp it", got.ReportedAt)
	}
	window := timescale.D(time.Minute)
	if d := got.ReportedAt.Sub(dbNow.UTC()); d > window || d < -window {
		t.Errorf("stored ReportedAt = %v is %v from the database's own now() = %v (want within %v) — it is not the database's instant", got.ReportedAt, d, dbNow.UTC(), window)
	}
}

// TestPostgres_RecordStageProgress_PhaseFieldsRoundTrip pins the #4023 phase
// fields through the real JSONB column: a gate-phase heartbeat reads back every
// field (a zero-valued pointer included, as PRESENT), and a phase-less heartbeat
// stores NONE of the five keys, so a pre-#4023 row and a phase-less write stay
// byte-identical in shape.
func TestPostgres_RecordStageProgress_PhaseFieldsRoundTrip(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	repo := run.NewPostgresRepository(pool)
	store := progressStore(t, repo)

	r := makeRun(t, repo)
	gate := makeStage(t, repo, r.ID, 0)
	legacy := makeStage(t, repo, r.ID, 1)

	intp := func(v int) *int { return &v }
	if _, err := store.RecordStageProgress(ctx, gate.ID, run.StageProgress{
		LastEvent: "gate:verify", TurnsThisAttempt: 9, TokensThisAttempt: 13402,
		Phase: "gate", PhaseElapsedSeconds: intp(312), GateBudgetSeconds: intp(5400),
		GateRemainingSeconds: intp(0), GateVerifying: true,
	}); err != nil {
		t.Fatalf("RecordStageProgress (gate): %v", err)
	}
	got, err := store.StageProgressByID(ctx, gate.ID)
	if err != nil || got == nil {
		t.Fatalf("StageProgressByID (gate) = %+v, %v", got, err)
	}
	if got.Phase != "gate" || !got.GateVerifying {
		t.Errorf("phase = %q gate_verifying = %v, want gate/true", got.Phase, got.GateVerifying)
	}
	if got.PhaseElapsedSeconds == nil || *got.PhaseElapsedSeconds != 312 ||
		got.GateBudgetSeconds == nil || *got.GateBudgetSeconds != 5400 ||
		got.GateRemainingSeconds == nil || *got.GateRemainingSeconds != 0 {
		t.Errorf("phase seconds round-trip mismatch: %+v", *got)
	}

	if _, err := store.RecordStageProgress(ctx, legacy.ID, run.StageProgress{
		LastEvent: "assistant", TurnsThisAttempt: 1, TokensThisAttempt: 2,
	}); err != nil {
		t.Fatalf("RecordStageProgress (legacy): %v", err)
	}
	var keys []string
	if err := pool.QueryRow(ctx,
		`SELECT array_agg(k ORDER BY k) FROM stages, jsonb_object_keys(progress) AS k WHERE id = $1`,
		legacy.ID).Scan(&keys); err != nil {
		t.Fatalf("read stored keys: %v", err)
	}
	want := []string{"last_event", "reported_at", "tokens_this_attempt", "turns_this_attempt"}
	if len(keys) != len(want) {
		t.Fatalf("phase-less row stored keys %v, want exactly %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Errorf("phase-less row stored keys %v, want exactly %v", keys, want)
			break
		}
	}
	lg, err := store.StageProgressByID(ctx, legacy.ID)
	if err != nil || lg == nil {
		t.Fatalf("StageProgressByID (legacy) = %+v, %v", lg, err)
	}
	if lg.Phase != "" || lg.PhaseElapsedSeconds != nil || lg.GateBudgetSeconds != nil ||
		lg.GateRemainingSeconds != nil || lg.GateVerifying {
		t.Errorf("phase-less row decoded phase fields: %+v", *lg)
	}
}
