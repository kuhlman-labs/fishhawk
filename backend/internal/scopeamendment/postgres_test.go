package scopeamendment_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/scopeamendment"
)

// harness bundles the repo plus a run + implement stage the
// amendments can hang off.
type harness struct {
	pool    *pgxpool.Pool
	repo    scopeamendment.Repository
	runRepo run.Repository
	runID   uuid.UUID
	stageID uuid.UUID
}

func newHarness(t *testing.T) harness {
	t.Helper()
	url := pgtest.NewURL(t)
	if err := postgres.MigrateUp(url); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	runRepo := run.NewPostgresRepository(pool)
	r, err := runRepo.CreateRun(context.Background(), run.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef",
		TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	st, err := runRepo.CreateStage(context.Background(), run.CreateStageParams{
		RunID:        r.ID,
		Sequence:     1,
		Type:         run.StageTypeImplement,
		ExecutorKind: run.ExecutorAgent,
		ExecutorRef:  "fishhawk/runner@v1",
	})
	if err != nil {
		t.Fatalf("create stage: %v", err)
	}
	return harness{
		pool:    pool,
		repo:    scopeamendment.NewPostgresRepository(pool),
		runRepo: runRepo,
		runID:   r.ID,
		stageID: st.ID,
	}
}

func (h harness) create(t *testing.T, paths ...scopeamendment.PathEntry) *scopeamendment.Amendment {
	t.Helper()
	if len(paths) == 0 {
		paths = []scopeamendment.PathEntry{{Path: "backend/internal/server/extra.go", Operation: scopeamendment.OperationModify}}
	}
	a, err := h.repo.Create(context.Background(), scopeamendment.CreateParams{
		RunID:   h.runID,
		StageID: h.stageID,
		Paths:   paths,
		Reason:  "the seam needs this file",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return a
}

func TestPostgres_Create_RoundTrips(t *testing.T) {
	h := newHarness(t)
	a := h.create(t,
		scopeamendment.PathEntry{Path: "a/b.go", Operation: scopeamendment.OperationModify},
		scopeamendment.PathEntry{Path: "c/new.go", Operation: scopeamendment.OperationCreate},
	)
	if a.ID == uuid.Nil {
		t.Error("ID is zero")
	}
	if a.Status != scopeamendment.StatusPending {
		t.Errorf("Status = %q, want pending", a.Status)
	}
	if a.RequestedAt.IsZero() {
		t.Error("RequestedAt zero")
	}
	got, err := h.repo.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if len(got.Paths) != 2 || got.Paths[0].Path != "a/b.go" || got.Paths[1].Operation != scopeamendment.OperationCreate {
		t.Errorf("Paths round-trip mismatch: %+v", got.Paths)
	}
	if got.Reason != "the seam needs this file" {
		t.Errorf("Reason = %q", got.Reason)
	}
}

func TestPostgres_Create_RejectsInvalidPaths(t *testing.T) {
	h := newHarness(t)
	_, err := h.repo.Create(context.Background(), scopeamendment.CreateParams{
		RunID:   h.runID,
		StageID: h.stageID,
		Paths:   []scopeamendment.PathEntry{{Path: "../escape.go", Operation: scopeamendment.OperationModify}},
		Reason:  "r",
	})
	if err == nil {
		t.Fatal("Create with '..' path should error")
	}
}

func TestPostgres_ListByRun_OldestFirst(t *testing.T) {
	h := newHarness(t)
	first := h.create(t, scopeamendment.PathEntry{Path: "first.go", Operation: scopeamendment.OperationModify})
	second := h.create(t, scopeamendment.PathEntry{Path: "second.go", Operation: scopeamendment.OperationModify})

	items, err := h.repo.ListByRun(context.Background(), h.runID)
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("len = %d, want 2", len(items))
	}
	if items[0].ID != first.ID || items[1].ID != second.ID {
		t.Errorf("order wrong: got [%s, %s], want [%s, %s]",
			items[0].ID, items[1].ID, first.ID, second.ID)
	}
	// Foreign run lists empty.
	empty, err := h.repo.ListByRun(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("ListByRun(foreign): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("foreign run returned %d rows", len(empty))
	}
}

func TestPostgres_CountByStage_StatusBlind(t *testing.T) {
	h := newHarness(t)
	a := h.create(t)
	h.create(t)
	// Deny the first — the count must still include it (budget bounds
	// operator interruptions, not approvals).
	if _, err := h.repo.Decide(context.Background(), scopeamendment.DecideParams{
		ID: a.ID, Status: scopeamendment.StatusDenied, Reason: "no", DecidedBy: "github:operator",
	}); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	n, err := h.repo.CountByStage(context.Background(), h.stageID)
	if err != nil {
		t.Fatalf("CountByStage: %v", err)
	}
	if n != 2 {
		t.Errorf("count = %d, want 2 (denied rows still consume budget)", n)
	}
}

func TestPostgres_Decide_ApproveThenConflict(t *testing.T) {
	h := newHarness(t)
	a := h.create(t)
	decided, err := h.repo.Decide(context.Background(), scopeamendment.DecideParams{
		ID: a.ID, Status: scopeamendment.StatusApproved, Reason: "ok", DecidedBy: "github:operator",
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decided.Status != scopeamendment.StatusApproved {
		t.Errorf("Status = %q, want approved", decided.Status)
	}
	if decided.DecidedBy == nil || *decided.DecidedBy != "github:operator" {
		t.Errorf("DecidedBy = %v", decided.DecidedBy)
	}
	if decided.DecidedAt == nil {
		t.Error("DecidedAt nil after decision")
	}
	// Second decide → ErrAlreadyDecided.
	_, err = h.repo.Decide(context.Background(), scopeamendment.DecideParams{
		ID: a.ID, Status: scopeamendment.StatusDenied, Reason: "flip", DecidedBy: "github:other",
	})
	if !errors.Is(err, scopeamendment.ErrAlreadyDecided) {
		t.Errorf("err = %v, want ErrAlreadyDecided", err)
	}
}

func TestPostgres_Decide_NotFound(t *testing.T) {
	h := newHarness(t)
	_, err := h.repo.Decide(context.Background(), scopeamendment.DecideParams{
		ID: uuid.New(), Status: scopeamendment.StatusApproved, Reason: "r", DecidedBy: "github:operator",
	})
	if !errors.Is(err, scopeamendment.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestPostgres_Decide_RejectsBadStatus(t *testing.T) {
	h := newHarness(t)
	a := h.create(t)
	_, err := h.repo.Decide(context.Background(), scopeamendment.DecideParams{
		ID: a.ID, Status: scopeamendment.StatusPending, Reason: "r", DecidedBy: "github:operator",
	})
	if err == nil {
		t.Fatal("Decide with status=pending should error")
	}
}

// --- #4127 lapse ---

// newRunStage creates a fresh run + implement stage on the harness pool.
func (h harness) newRunStage(t *testing.T) (uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	r, err := h.runRepo.CreateRun(ctx, run.CreateRunParams{
		Repo: "kuhlman-labs/fishhawk", WorkflowID: "feature_change", WorkflowSHA: "deadbeef", TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	return r.ID, h.newStage(t, r.ID, 1)
}

func (h harness) newStage(t *testing.T, runID uuid.UUID, seq int) uuid.UUID {
	t.Helper()
	st, err := h.runRepo.CreateStage(context.Background(), run.CreateStageParams{
		RunID: runID, Sequence: seq, Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, ExecutorRef: "fishhawk/runner@v1",
	})
	if err != nil {
		t.Fatalf("create stage: %v", err)
	}
	return st.ID
}

func (h harness) createOn(t *testing.T, runID, stageID uuid.UUID, path string) *scopeamendment.Amendment {
	t.Helper()
	a, err := h.repo.Create(context.Background(), scopeamendment.CreateParams{
		RunID: runID, StageID: stageID, Reason: "needed",
		Paths: []scopeamendment.PathEntry{{Path: path, Operation: scopeamendment.OperationModify}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return a
}

func (h harness) get(t *testing.T, id uuid.UUID) *scopeamendment.Amendment {
	t.Helper()
	a, err := h.repo.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("GetByID(%s): %v", id, err)
	}
	return a
}

// walk moves a stage through the given states with the run repository.
func (h harness) walk(t *testing.T, stageID uuid.UUID, states ...run.StageState) {
	t.Helper()
	for _, to := range states {
		var completion *run.StageCompletion
		if to == run.StageStateFailed {
			cat, reason := run.FailureB, "scope"
			completion = &run.StageCompletion{FailureCategory: &cat, FailureReason: &reason}
		}
		if _, err := h.runRepo.TransitionStage(context.Background(), stageID, to, completion); err != nil {
			t.Fatalf("stage → %s: %v", to, err)
		}
	}
}

type lapsedEntry struct {
	AmendmentID    string `json:"amendment_id"`
	StageID        string `json:"stage_id"`
	Trigger        string `json:"trigger"`
	DecisionReason string `json:"decision_reason"`
	Source         string `json:"source"`
}

func (h harness) lapsedEntries(t *testing.T, runID uuid.UUID) []lapsedEntry {
	t.Helper()
	entries, err := audit.NewPostgresRepository(h.pool).ListForRunByCategory(context.Background(), runID, scopeamendment.CategoryLapsed)
	if err != nil {
		t.Fatalf("ListForRunByCategory: %v", err)
	}
	out := make([]lapsedEntry, 0, len(entries))
	for _, e := range entries {
		var le lapsedEntry
		if err := json.Unmarshal(e.Payload, &le); err != nil {
			t.Fatalf("unmarshal lapsed payload: %v", err)
		}
		if e.ActorKind == nil || *e.ActorKind != audit.ActorSystem {
			t.Errorf("lapsed entry actor_kind = %v, want system", e.ActorKind)
		}
		out = append(out, le)
	}
	return out
}

func assertLapsed(t *testing.T, a *scopeamendment.Amendment, wantTrigger string) {
	t.Helper()
	if a.Status != scopeamendment.StatusLapsed {
		t.Errorf("amendment %s status = %q, want lapsed", a.ID, a.Status)
		return
	}
	if a.DecidedAt == nil {
		t.Errorf("amendment %s decided_at nil, want the lapse time", a.ID)
	}
	if a.DecidedBy != nil {
		t.Errorf("amendment %s decided_by = %q, want nil (no operator decided it)", a.ID, *a.DecidedBy)
	}
	if a.DecisionReason == nil || !strings.Contains(*a.DecisionReason, wantTrigger) {
		t.Errorf("amendment %s decision_reason = %v, want it to name %q", a.ID, a.DecisionReason, wantTrigger)
	}
}

// TestPostgres_LapseHooks_CrossBoundary is the end-to-end test of the #4127
// lapse rule (option b): run repo → InstallLapseHooks (the function fishhawkd
// calls) → scopeamendment repo → audit chain, on one pgtest database.
//
//   - a stage that SUCCEEDS lapses its pending request (one stage_terminal
//     entry);
//   - a stage that FAILS on a live run keeps its requests pending, and a
//     later approve lands and survives the retry (the fold reads approved
//     rows);
//   - that same run then CANCELLED lapses the remaining pending request (one
//     run_terminal entry) and leaves the approved one untouched.
//
// MECHANISM: with the hooks absent nothing else touches a row, so it stays
// pending and the lapsed assertions fail; with failed added to the lapsing
// stage states the failed-stage rows lapse and the "still pending" assertion
// fails.
func TestPostgres_LapseHooks_CrossBoundary(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if l := scopeamendment.InstallLapseHooks(h.runRepo, h.repo, slog.New(slog.NewTextHandler(io.Discard, nil))); l == nil {
		t.Fatal("InstallLapseHooks returned nil for the postgres repositories")
	}

	// (1) stage succeeds with an undecided request → lapsed.
	succeeded := h.create(t)
	h.walk(t, h.stageID, run.StageStateDispatched, run.StageStateRunning, run.StageStateSucceeded)
	assertLapsed(t, h.get(t, succeeded.ID), "stage succeeded")
	if got := h.lapsedEntries(t, h.runID); len(got) != 1 || got[0].AmendmentID != succeeded.ID.String() ||
		got[0].Source != scopeamendment.LapseSourceStageTerminal || got[0].Trigger != "stage succeeded" {
		t.Errorf("lapsed entries on the succeeded stage's run = %+v, want one stage_terminal entry for %s", got, succeeded.ID)
	}

	// (2) a stage that fails on a live run keeps its requests pending.
	liveRun, failedStage := h.newRunStage(t)
	if _, err := h.runRepo.TransitionRun(ctx, liveRun, run.StateRunning); err != nil {
		t.Fatalf("run → running: %v", err)
	}
	toApprove := h.createOn(t, liveRun, failedStage, "a.go")
	stillPending := h.createOn(t, liveRun, failedStage, "b.go")
	h.walk(t, failedStage, run.StageStateDispatched, run.StageStateRunning, run.StageStateFailed)
	for _, a := range []*scopeamendment.Amendment{toApprove, stillPending} {
		if got := h.get(t, a.ID); got.Status != scopeamendment.StatusPending || got.DecidedAt != nil {
			t.Errorf("amendment %s on a failed stage of a live run: status = %q decided_at = %v, want still pending", a.ID, got.Status, got.DecidedAt)
		}
	}
	if got := h.lapsedEntries(t, liveRun); len(got) != 0 {
		t.Errorf("lapsed entries after a stage failure on a live run = %+v, want none", got)
	}
	// ... and a late approval lands and survives the retry.
	approved, err := h.repo.Decide(ctx, scopeamendment.DecideParams{
		ID: toApprove.ID, Status: scopeamendment.StatusApproved, Reason: "ok", DecidedBy: "github:operator",
	})
	if err != nil || approved.Status != scopeamendment.StatusApproved {
		t.Fatalf("Decide(approve) on the failed stage's pending request = (%v, %v), want approved", approved, err)
	}
	if _, err := h.runRepo.RetryStage(ctx, failedStage, run.StageStatePending); err != nil {
		t.Fatalf("RetryStage(failed → pending): %v", err)
	}
	items, err := h.repo.ListByRun(ctx, liveRun)
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	folded := 0
	for _, a := range items {
		if a.StageID == failedStage && a.Status == scopeamendment.StatusApproved {
			folded++
		}
	}
	if folded != 1 {
		t.Errorf("approved rows on the retried stage = %d, want 1 (the fold source)", folded)
	}

	// (3) that same run then cancelled → the remaining pending request lapses.
	if _, err := h.runRepo.TransitionRun(ctx, liveRun, run.StateCancelled); err != nil {
		t.Fatalf("run → cancelled: %v", err)
	}
	assertLapsed(t, h.get(t, stillPending.ID), "run cancelled")
	if got := h.get(t, toApprove.ID); got.Status != scopeamendment.StatusApproved || got.DecidedBy == nil || *got.DecidedBy != "github:operator" {
		t.Errorf("approved amendment after the run was cancelled = %+v, want it untouched", got)
	}
	if got := h.lapsedEntries(t, liveRun); len(got) != 1 || got[0].AmendmentID != stillPending.ID.String() ||
		got[0].Source != scopeamendment.LapseSourceRunTerminal || got[0].Trigger != "run cancelled" {
		t.Errorf("lapsed entries after the run was cancelled = %+v, want one run_terminal entry for %s", got, stillPending.ID)
	}
	// The first run's chain gained nothing from the second run's lapse.
	if got := h.lapsedEntries(t, h.runID); len(got) != 1 {
		t.Errorf("lapsed entries on the first run = %d, want still 1", len(got))
	}
}

// TestPostgres_Decide_LapsedRefused: a lapsed row refuses approve AND deny
// with ErrLapsed (not ErrAlreadyDecided), and the row is unchanged.
func TestPostgres_Decide_LapsedRefused(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a := h.create(t)
	lapser := h.repo.(scopeamendment.Lapser)
	if _, err := lapser.LapseForStage(ctx, scopeamendment.LapseParams{
		StageID: h.stageID, Trigger: "stage succeeded", Source: scopeamendment.LapseSourceStageTerminal,
	}); err != nil {
		t.Fatalf("LapseForStage: %v", err)
	}
	before := h.get(t, a.ID)
	for _, status := range []scopeamendment.Status{scopeamendment.StatusApproved, scopeamendment.StatusDenied} {
		_, err := h.repo.Decide(ctx, scopeamendment.DecideParams{ID: a.ID, Status: status, Reason: "late", DecidedBy: "github:operator"})
		if !errors.Is(err, scopeamendment.ErrLapsed) || errors.Is(err, scopeamendment.ErrAlreadyDecided) {
			t.Errorf("Decide(%s) on a lapsed row: err = %v, want ErrLapsed (and not ErrAlreadyDecided)", status, err)
		}
	}
	after := h.get(t, a.ID)
	if after.Status != scopeamendment.StatusLapsed || after.DecidedBy != nil ||
		*after.DecisionReason != *before.DecisionReason || !after.DecidedAt.Equal(*before.DecidedAt) {
		t.Errorf("lapsed row after refused decides = %+v, want unchanged from %+v", after, before)
	}
}

// TestPostgres_LapseForStage_OnlyPendingRows: only the pending row lapses, the
// approved row is untouched, exactly one entry is written, and a repeat call
// lapses nothing and writes nothing.
func TestPostgres_LapseForStage_OnlyPendingRows(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	approved := h.create(t)
	if _, err := h.repo.Decide(ctx, scopeamendment.DecideParams{ID: approved.ID, Status: scopeamendment.StatusApproved, Reason: "ok", DecidedBy: "github:operator"}); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	approvedBefore := h.get(t, approved.ID)
	pending := h.create(t)
	lapser := h.repo.(scopeamendment.Lapser)
	p := scopeamendment.LapseParams{StageID: h.stageID, Trigger: "stage cancelled", Source: scopeamendment.LapseSourceStageTerminal}
	lapsed, err := lapser.LapseForStage(ctx, p)
	if err != nil {
		t.Fatalf("LapseForStage: %v", err)
	}
	if len(lapsed) != 1 || lapsed[0].ID != pending.ID {
		t.Fatalf("lapsed = %+v, want only the pending row %s", lapsed, pending.ID)
	}
	assertLapsed(t, h.get(t, pending.ID), "stage cancelled")
	got := h.get(t, approved.ID)
	if got.Status != scopeamendment.StatusApproved || *got.DecisionReason != *approvedBefore.DecisionReason ||
		got.DecidedBy == nil || *got.DecidedBy != "github:operator" || !got.DecidedAt.Equal(*approvedBefore.DecidedAt) {
		t.Errorf("approved row after the lapse = %+v, want byte-unchanged from %+v", got, approvedBefore)
	}
	if n := len(h.lapsedEntries(t, h.runID)); n != 1 {
		t.Errorf("lapsed entries = %d, want 1", n)
	}
	again, err := lapser.LapseForStage(ctx, p)
	if err != nil || len(again) != 0 {
		t.Errorf("repeat LapseForStage = (%v, %v), want no rows", again, err)
	}
	if n := len(h.lapsedEntries(t, h.runID)); n != 1 {
		t.Errorf("lapsed entries after a repeat lapse = %d, want still 1", n)
	}
}

// TestPostgres_Lapse_RejectsBadParams covers the lapse input guards.
func TestPostgres_Lapse_RejectsBadParams(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a := h.create(t)
	lapser := h.repo.(scopeamendment.Lapser)
	ok := scopeamendment.LapseParams{StageID: h.stageID, RunID: h.runID, Trigger: "stage succeeded", Source: scopeamendment.LapseSourceStageTerminal}
	cases := map[string]func() error{
		"stage: nil stage_id": func() error { p := ok; p.StageID = uuid.Nil; _, err := lapser.LapseForStage(ctx, p); return err },
		"run: nil run_id":     func() error { p := ok; p.RunID = uuid.Nil; _, err := lapser.LapseForRun(ctx, p); return err },
		"stage: blank trigger": func() error {
			p := ok
			p.Trigger = "  "
			_, err := lapser.LapseForStage(ctx, p)
			return err
		},
		"run: empty source": func() error { p := ok; p.Source = ""; _, err := lapser.LapseForRun(ctx, p); return err },
	}
	for name, call := range cases {
		if err := call(); err == nil {
			t.Errorf("%s: err = nil, want a refusal", name)
		}
	}
	if got := h.get(t, a.ID); got.Status != scopeamendment.StatusPending {
		t.Errorf("row after refused lapses = %q, want still pending", got.Status)
	}
}

// TestPostgres_LapseStranded_BackfillIdempotent reproduces the pre-#4127 data
// shape (stage/run states written with NO hooks installed) and pins the
// startup backfill: rows on succeeded/cancelled/superseded stages and on a
// terminal run lapse, a failed stage on a live run and a running stage keep
// theirs, the issue's acceptance count reaches 0, and a second pass is a no-op.
func TestPostgres_LapseStranded_BackfillIdempotent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	setStage := func(id uuid.UUID, state string) {
		t.Helper()
		if _, err := h.pool.Exec(ctx, `UPDATE stages SET state = $2 WHERE id = $1`, id, state); err != nil {
			t.Fatalf("seed stage state %s: %v", state, err)
		}
	}
	// Live run (h.runID, pending): one stage per state.
	type seeded struct {
		a       *scopeamendment.Amendment
		trigger string // "" = must stay pending
	}
	var rows []seeded
	for i, st := range []struct{ state, trigger string }{
		{"succeeded", "stage succeeded"},
		{"cancelled", "stage cancelled"},
		{"superseded", "stage superseded"},
		{"failed", ""},
		{"running", ""},
	} {
		stageID := h.newStage(t, h.runID, 10+i)
		setStage(stageID, st.state)
		rows = append(rows, seeded{h.createOn(t, h.runID, stageID, st.state+".go"), st.trigger})
	}
	// Terminal (failed) run: its failed stage's row lapses on the RUN.
	deadRun, deadStage := h.newRunStage(t)
	setStage(deadStage, "failed")
	if _, err := h.pool.Exec(ctx, `UPDATE runs SET state = 'failed' WHERE id = $1`, deadRun); err != nil {
		t.Fatalf("seed run state: %v", err)
	}
	rows = append(rows, seeded{h.createOn(t, deadRun, deadStage, "dead.go"), "run failed"})

	lapser := h.repo.(scopeamendment.Lapser)
	n, err := lapser.LapseStranded(ctx)
	if err != nil {
		t.Fatalf("LapseStranded: %v", err)
	}
	if n != 4 {
		t.Errorf("LapseStranded = %d, want 4", n)
	}
	for _, r := range rows {
		got := h.get(t, r.a.ID)
		if r.trigger == "" {
			if got.Status != scopeamendment.StatusPending {
				t.Errorf("row %s (%s) status = %q, want still pending", r.a.ID, r.a.Paths[0].Path, got.Status)
			}
			continue
		}
		assertLapsed(t, got, r.trigger)
	}
	entries := append(h.lapsedEntries(t, h.runID), h.lapsedEntries(t, deadRun)...)
	if len(entries) != 4 {
		t.Errorf("lapsed entries = %d, want 4", len(entries))
	}
	for _, e := range entries {
		if e.Source != scopeamendment.LapseSourceStartupBackfill {
			t.Errorf("lapsed entry %+v source = %q, want startup_backfill", e, e.Source)
		}
	}
	var stale int
	if err := h.pool.QueryRow(ctx, `SELECT count(*) FROM scope_amendments sa
JOIN stages s ON s.id = sa.stage_id JOIN runs r ON r.id = sa.run_id
WHERE sa.status = 'pending' AND (s.state IN ('succeeded','cancelled','superseded') OR r.state IN ('succeeded','failed','cancelled'))`).Scan(&stale); err != nil {
		t.Fatalf("acceptance count: %v", err)
	}
	if stale != 0 {
		t.Errorf("stale pending rows after the backfill = %d, want 0", stale)
	}
	again, err := lapser.LapseStranded(ctx)
	if err != nil || again != 0 {
		t.Errorf("second LapseStranded = (%d, %v), want (0, nil)", again, err)
	}
	if got := len(h.lapsedEntries(t, h.runID)) + len(h.lapsedEntries(t, deadRun)); got != 4 {
		t.Errorf("lapsed entries after a second pass = %d, want still 4", got)
	}
}

// TestLapseOnTerminal_DetachedFromCallerCancel: both observer bodies lapse
// even when the caller's ctx is already cancelled. MECHANISM: a cancelled ctx
// makes pgx fail BeginFunc immediately, so without context.WithoutCancel the
// row stays pending.
func TestLapseOnTerminal_DetachedFromCallerCancel(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	lapser := h.repo.(scopeamendment.Lapser)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	onStage := h.create(t)
	scopeamendment.LapseOnStageTerminal(ctx, lapser, logger, h.stageID, string(run.StageStateSucceeded))
	assertLapsed(t, h.get(t, onStage.ID), "stage succeeded")

	runID, stageID := h.newRunStage(t)
	onRun := h.createOn(t, runID, stageID, "x.go")
	scopeamendment.LapseOnRunTerminal(ctx, lapser, logger, runID, string(run.StateCancelled))
	assertLapsed(t, h.get(t, onRun.ID), "run cancelled")
}

// fakeLapser records lapse calls and returns canned results.
type fakeLapser struct {
	stageCalls []scopeamendment.LapseParams
	runCalls   []scopeamendment.LapseParams
	rows       int
	stranded   int
	err        error
}

func (f *fakeLapser) result() ([]*scopeamendment.Amendment, error) {
	if f.err != nil {
		return nil, f.err
	}
	return make([]*scopeamendment.Amendment, f.rows), nil
}

func (f *fakeLapser) LapseForStage(_ context.Context, p scopeamendment.LapseParams) ([]*scopeamendment.Amendment, error) {
	f.stageCalls = append(f.stageCalls, p)
	return f.result()
}

func (f *fakeLapser) LapseForRun(_ context.Context, p scopeamendment.LapseParams) ([]*scopeamendment.Amendment, error) {
	f.runCalls = append(f.runCalls, p)
	return f.result()
}

func (f *fakeLapser) LapseStranded(context.Context) (int, error) { return f.stranded, f.err }

func bufLogger() (*slog.Logger, *strings.Builder) {
	var buf strings.Builder
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

// TestLapseOnTerminal_StateFilter pins rule (b): a stage lapses its requests
// only on succeeded/cancelled/superseded — NOT on failed or a live state —
// and a run lapses on every terminal state.
func TestLapseOnTerminal_StateFilter(t *testing.T) {
	ctx := context.Background()
	for state, want := range map[string]bool{
		"succeeded": true, "cancelled": true, "superseded": true,
		"failed": false, "running": false, "pending": false, "awaiting_approval": false,
	} {
		f := &fakeLapser{}
		scopeamendment.LapseOnStageTerminal(ctx, f, nil, uuid.New(), state)
		if got := len(f.stageCalls) == 1; got != want {
			t.Errorf("stage %s: lapsed = %v, want %v", state, got, want)
		}
		if want && (f.stageCalls[0].Trigger != "stage "+state || f.stageCalls[0].Source != scopeamendment.LapseSourceStageTerminal) {
			t.Errorf("stage %s: params = %+v, want trigger %q source stage_terminal", state, f.stageCalls[0], "stage "+state)
		}
	}
	for state, want := range map[string]bool{
		"succeeded": true, "failed": true, "cancelled": true, "running": false, "pending": false,
	} {
		f := &fakeLapser{}
		scopeamendment.LapseOnRunTerminal(ctx, f, nil, uuid.New(), state)
		if got := len(f.runCalls) == 1; got != want {
			t.Errorf("run %s: lapsed = %v, want %v", state, got, want)
		}
		if want && (f.runCalls[0].Trigger != "run "+state || f.runCalls[0].Source != scopeamendment.LapseSourceRunTerminal) {
			t.Errorf("run %s: params = %+v, want trigger %q source run_terminal", state, f.runCalls[0], "run "+state)
		}
	}
}

// TestLapseOnTerminal_LogsErrorsAndCounts: a lapse error is a WARN naming the
// stage/run id (never propagated, no panic); a non-zero lapse is an INFO.
func TestLapseOnTerminal_LogsErrorsAndCounts(t *testing.T) {
	ctx := context.Background()
	stageID, runID := uuid.New(), uuid.New()

	logger, buf := bufLogger()
	failing := &fakeLapser{err: errors.New("db down")}
	scopeamendment.LapseOnStageTerminal(ctx, failing, logger, stageID, "succeeded")
	scopeamendment.LapseOnRunTerminal(ctx, failing, logger, runID, "failed")
	out := buf.String()
	for _, want := range []string{"level=WARN", "stage_id=" + stageID.String(), "run_id=" + runID.String(), "db down", "startup backfill"} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q missing %q", out, want)
		}
	}

	logger, buf = bufLogger()
	scopeamendment.LapseOnStageTerminal(ctx, &fakeLapser{rows: 2}, logger, stageID, "cancelled")
	if out := buf.String(); !strings.Contains(out, "level=INFO") || !strings.Contains(out, "lapsed=2") {
		t.Errorf("log after lapsing 2 rows = %q, want an INFO with lapsed=2", out)
	}
	logger, buf = bufLogger()
	scopeamendment.LapseOnStageTerminal(ctx, &fakeLapser{}, logger, stageID, "cancelled")
	if out := buf.String(); out != "" {
		t.Errorf("log after lapsing nothing = %q, want none", out)
	}
}

// TestInstallLapseHooks_WarnsOnMissingCapability: a repository lacking a
// capability is a WARN, never a silent no-op.
func TestInstallLapseHooks_WarnsOnMissingCapability(t *testing.T) {
	logger, buf := bufLogger()
	if l := scopeamendment.InstallLapseHooks(struct{}{}, struct{}{}, logger); l != nil {
		t.Errorf("InstallLapseHooks(non-Lapser) = %v, want nil", l)
	}
	if out := buf.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "Lapser capability") {
		t.Errorf("log = %q, want a WARN naming the Lapser capability", out)
	}

	logger, buf = bufLogger()
	f := &fakeLapser{}
	if l := scopeamendment.InstallLapseHooks(struct{}{}, f, logger); l != f {
		t.Errorf("InstallLapseHooks(non-observable run repo) = %v, want the lapser (the backfill still runs)", l)
	}
	for _, want := range []string{"StageTerminalObservable", "TerminalRunObservable"} {
		found := false
		for _, line := range strings.Split(buf.String(), "\n") {
			if strings.Contains(line, "level=WARN") && strings.Contains(line, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("log = %q, want a WARN line naming %s", buf.String(), want)
		}
	}
}

// TestBackfillStranded_Logging: nil lapser is a no-op, an error is a WARN, a
// non-zero count is an INFO, zero logs nothing.
func TestBackfillStranded_Logging(t *testing.T) {
	ctx := context.Background()
	logger, buf := bufLogger()
	scopeamendment.BackfillStranded(ctx, nil, logger)
	scopeamendment.BackfillStranded(ctx, &fakeLapser{}, logger)
	if out := buf.String(); out != "" {
		t.Errorf("log for nil lapser / zero lapsed = %q, want none", out)
	}
	scopeamendment.BackfillStranded(ctx, &fakeLapser{stranded: 1, err: errors.New("one stage failed")}, logger)
	if out := buf.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "one stage failed") {
		t.Errorf("log after a failed backfill = %q, want a WARN with the error", out)
	}
	logger, buf = bufLogger()
	scopeamendment.BackfillStranded(ctx, &fakeLapser{stranded: 53}, logger)
	if out := buf.String(); !strings.Contains(out, "level=INFO") || !strings.Contains(out, "lapsed=53") {
		t.Errorf("log after lapsing 53 rows = %q, want an INFO with lapsed=53", out)
	}
}
