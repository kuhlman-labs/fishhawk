package dispatchwatchdog

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// pgDispatchedStage creates a real run + stage and transitions the stage into
// the dispatched state through the real repo, so migration 0072's trigger
// stamps stages.dispatched_at. Returns the run + stage.
func pgDispatchedStage(t *testing.T, repo run.Repository) (*run.Run, *run.Stage) {
	t.Helper()
	ctx := context.Background()
	r, err := repo.CreateRun(ctx, run.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef",
		TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	s, err := repo.CreateStage(ctx, run.CreateStageParams{
		RunID:        r.ID,
		Sequence:     0,
		Type:         run.StageTypeImplement,
		ExecutorKind: run.ExecutorAgent,
		ExecutorRef:  "claude-code",
	})
	if err != nil {
		t.Fatalf("create stage: %v", err)
	}
	if _, err := repo.TransitionStage(ctx, s.ID, run.StageStateDispatched, nil); err != nil {
		t.Fatalf("transition to dispatched: %v", err)
	}
	return r, s
}

// TestDispatchWatchdog_FailsWedgedButHeartbeatingStage is the honest end-to-end
// acceptance criterion (#2744): a real Postgres trigger stamps dispatched_at, a
// real progress heartbeat bumps updated_at, and the real Ticker still fails the
// stage on its DISPATCH-relative deadline — proving a heartbeat cannot suppress
// the watchdog. This reddens if handleStage's deadline base is switched from
// DispatchedAt back to UpdatedAt (approach step 12a).
func TestDispatchWatchdog_FailsWedgedButHeartbeatingStage(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	repo := run.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	store, ok := repo.(run.StageProgressStore)
	if !ok {
		t.Fatal("postgres repo does not implement run.StageProgressStore")
	}

	r, s := pgDispatchedStage(t, repo)

	timeout := timescale.D(time.Hour)

	// Backdate the DISPATCH clock well past the deadline (a stage dispatched
	// long ago), WITHOUT moving state — the 0072 trigger's transition predicate
	// is false on this same-state UPDATE, so the explicit dispatched_at sticks.
	// This is what separates the two clocks: dispatched_at is stale, but the
	// heartbeats below keep updated_at fresh right up to "now".
	if _, err := pool.Exec(ctx, `UPDATE stages SET dispatched_at = now() - $2::interval WHERE id = $1`,
		s.ID, (2 * timeout).String()); err != nil {
		t.Fatalf("backdate dispatched_at: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := store.RecordStageProgress(ctx, s.ID, run.StageProgress{
			LastEvent:  "assistant",
			ReportedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("record heartbeat %d: %v", i, err)
		}
	}

	// Now ≈ real time: PAST the dispatch deadline (dispatched_at + timeout is in
	// the past) but INSIDE an updated_at-relative deadline (updated_at is fresh),
	// so only a dispatch-relative deadline fires. Switching handleStage's base to
	// UpdatedAt reddens this.
	now := time.Now().UTC()
	tick := &Ticker{
		Repo:    repo,
		Audit:   auditRepo,
		Timeout: timeout,
		Now:     func() time.Time { return now },
	}
	tick.Tick(ctx)

	got, err := repo.GetStage(ctx, s.ID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	if got.State != run.StageStateFailed {
		t.Fatalf("stage state = %s, want failed (a wedged-but-heartbeating stage must fail on its dispatch deadline)", got.State)
	}
	if got.FailureCategory == nil || *got.FailureCategory != run.FailureC {
		t.Errorf("FailureCategory = %v, want C", got.FailureCategory)
	}
	if got.FailureReason == nil || !strings.Contains(*got.FailureReason, "last heartbeat") {
		t.Errorf("FailureReason = %v, want the wedged_after_checkin shape", got.FailureReason)
	}
	// Pin the reason<->mode coupling to the mode CONSTANT end-to-end (#2744 fix-up).
	if got.FailureReason == nil || !strings.Contains(*got.FailureReason, string(modeWedgedAfterCheckin)) {
		t.Errorf("FailureReason = %v, want it to carry the mode token %q", got.FailureReason, modeWedgedAfterCheckin)
	}

	// The audit payload carries mode=wedged_after_checkin.
	entries, err := auditRepo.ListForRunByCategory(ctx, r.ID, CategoryDispatchWatchdogElapsed)
	if err != nil {
		t.Fatalf("ListForRunByCategory: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("dispatch_watchdog_elapsed entries = %d, want 1", len(entries))
	}
	var payload map[string]any
	if err := json.Unmarshal(entries[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal audit payload: %v", err)
	}
	if payload["mode"] != "wedged_after_checkin" {
		t.Errorf("audit mode = %v, want wedged_after_checkin", payload["mode"])
	}
}

// TestDispatchWatchdog_DoesNotFailHealthyStageWithinBudget is the false-positive
// guard: an identical heartbeating dispatched stage, ticked with Now INSIDE the
// budget, is untouched. This is the pin against reintroducing the regression on
// long implement passes.
func TestDispatchWatchdog_DoesNotFailHealthyStageWithinBudget(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	repo := run.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	store, ok := repo.(run.StageProgressStore)
	if !ok {
		t.Fatal("postgres repo does not implement run.StageProgressStore")
	}

	_, s := pgDispatchedStage(t, repo)
	if _, err := store.RecordStageProgress(ctx, s.ID, run.StageProgress{LastEvent: "assistant", ReportedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("record heartbeat: %v", err)
	}

	timeout := timescale.D(time.Hour)
	tick := &Ticker{
		Repo:    repo,
		Audit:   auditRepo,
		Timeout: timeout,
		// Now sits WELL inside the dispatch budget.
		Now: func() time.Time { return time.Now().UTC().Add(timeout / 10) },
	}
	tick.Tick(ctx)

	got, err := repo.GetStage(ctx, s.ID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	if got.State != run.StageStateDispatched {
		t.Errorf("stage state = %s, want dispatched (a healthy long-running stage must not be failed)", got.State)
	}
}
