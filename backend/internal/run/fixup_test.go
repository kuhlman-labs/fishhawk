package run_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// fixupRepo wraps memRepo with a run-state-aware GetRun: FixupStage
// checks the run state on every eligible call (#968), and the shared
// memRepo's GetRun is not armed for that. Run ids without an explicit
// override serve a running run.
type fixupRepo struct {
	*memRepo
	runStates map[uuid.UUID]run.State
}

func newFixupRepo(s ...*run.Stage) *fixupRepo {
	return &fixupRepo{memRepo: newMemRepo(s...), runStates: map[uuid.UUID]run.State{}}
}

// setRunState arms GetRun to serve the given state for a run id —
// used to assert FixupStage refuses to re-open stages inside a
// terminal run (#968).
func (r *fixupRepo) setRunState(id uuid.UUID, state run.State) {
	r.runStates[id] = state
}

func (r *fixupRepo) GetRun(_ context.Context, id uuid.UUID) (*run.Run, error) {
	state := run.StateRunning
	if s, ok := r.runStates[id]; ok {
		state = s
	}
	return &run.Run{ID: id, State: state}, nil
}

// implementStage builds an implement stage in the given state — what
// FixupStage walks at the start of every call. Reuses newStage's
// fixture and overrides the type, since the default fixture is a plan
// stage.
func implementStage(t *testing.T, state run.StageState) (*fixupRepo, *run.Stage) {
	t.Helper()
	stage := newStage(state)
	stage.Type = run.StageTypeImplement
	repo := newFixupRepo(stage)
	return repo, stage
}

// implementWithReview builds an implement stage and a review stage
// sharing one RunID — the push_and_open_pr shape (#780): the implement
// stage has succeeded (PR opened) while the review stage holds the gate.
func implementWithReview(t *testing.T, implState, reviewState run.StageState) (*fixupRepo, *run.Stage, *run.Stage) {
	t.Helper()
	runID := uuid.New()
	impl := newStage(implState)
	impl.Type = run.StageTypeImplement
	impl.RunID = runID
	impl.Sequence = 1
	review := newStage(reviewState)
	review.Type = run.StageTypeReview
	review.RunID = runID
	review.Sequence = 2
	repo := newFixupRepo(impl, review)
	return repo, impl, review
}

func TestFixupStage_ReopensAwaitingApprovalToPending(t *testing.T) {
	repo, stage := implementStage(t, run.StageStateAwaitingApproval)

	dec, err := run.FixupStage(context.Background(), repo, stage.ID, run.FixupOptions{
		PriorPassCount: 0,
		MaxPasses:      1,
		HardCeiling:    3,
	})
	if err != nil {
		t.Fatalf("FixupStage: %v", err)
	}
	if dec.PriorState != run.StageStateAwaitingApproval {
		t.Errorf("PriorState = %q, want awaiting_approval", dec.PriorState)
	}
	if dec.Stage.State != run.StageStatePending {
		t.Errorf("post-fixup state = %q, want pending", dec.Stage.State)
	}
	if dec.RemainingBudget != 0 {
		t.Errorf("RemainingBudget = %d, want 0 (1 of 1 used)", dec.RemainingBudget)
	}
}

func TestFixupStage_RefusesNonImplementStage(t *testing.T) {
	// Default fixture is a plan stage parked at the gate.
	stage := newStage(run.StageStateAwaitingApproval)
	repo := newMemRepo(stage)

	_, err := run.FixupStage(context.Background(), repo, stage.ID, run.FixupOptions{MaxPasses: 1, HardCeiling: 3})
	if !errors.Is(err, run.ErrFixupNotApplicable) {
		t.Fatalf("err = %v, want ErrFixupNotApplicable", err)
	}
}

func TestFixupStage_RefusesWrongState(t *testing.T) {
	// An implement stage that is running (not parked at the gate) is
	// not a fix-up candidate.
	repo, stage := implementStage(t, run.StageStateRunning)

	_, err := run.FixupStage(context.Background(), repo, stage.ID, run.FixupOptions{MaxPasses: 1, HardCeiling: 3})
	if !errors.Is(err, run.ErrFixupNotApplicable) {
		t.Fatalf("err = %v, want ErrFixupNotApplicable", err)
	}
	// State must be untouched.
	cur, _ := repo.GetStage(context.Background(), stage.ID)
	if cur.State != run.StageStateRunning {
		t.Errorf("state = %q, want unchanged (running)", cur.State)
	}
}

func TestFixupStage_RefusesWhenBudgetExhausted(t *testing.T) {
	repo, stage := implementStage(t, run.StageStateAwaitingApproval)

	// One pass already consumed against a default bound of 1.
	_, err := run.FixupStage(context.Background(), repo, stage.ID, run.FixupOptions{
		PriorPassCount: 1,
		MaxPasses:      1,
		HardCeiling:    3,
	})
	if !errors.Is(err, run.ErrFixupBudgetExhausted) {
		t.Fatalf("err = %v, want ErrFixupBudgetExhausted", err)
	}
	// The stage must NOT have been re-opened — the bound is checked
	// before any transition.
	cur, _ := repo.GetStage(context.Background(), stage.ID)
	if cur.State != run.StageStateAwaitingApproval {
		t.Errorf("state = %q, want unchanged (awaiting_approval)", cur.State)
	}
}

func TestFixupStage_ReopensSucceededWithOpenReviewGate(t *testing.T) {
	// push_and_open_pr flow: implement succeeded, review still at the gate.
	repo, impl, review := implementWithReview(t, run.StageStateSucceeded, run.StageStateAwaitingApproval)
	ctx := context.Background()

	dec, err := run.FixupStage(ctx, repo, impl.ID, run.FixupOptions{MaxPasses: 1, HardCeiling: 3})
	if err != nil {
		t.Fatalf("FixupStage: %v", err)
	}
	if dec.PriorState != run.StageStateSucceeded {
		t.Errorf("PriorState = %q, want succeeded", dec.PriorState)
	}
	if dec.Stage.State != run.StageStatePending {
		t.Errorf("implement state = %q, want pending", dec.Stage.State)
	}
	if dec.ReparkedReview == nil {
		t.Fatal("ReparkedReview = nil, want the re-parked review stage")
	}
	if dec.ReparkedReview.ID != review.ID {
		t.Errorf("ReparkedReview.ID = %s, want %s", dec.ReparkedReview.ID, review.ID)
	}
	if dec.ReparkedReview.State != run.StageStatePending {
		t.Errorf("re-parked review state = %q, want pending", dec.ReparkedReview.State)
	}
	// The re-open cleared the implement stage's terminal ended_at.
	cur, _ := repo.GetStage(ctx, impl.ID)
	if cur.EndedAt != nil {
		t.Errorf("implement EndedAt = %v, want nil after re-open", cur.EndedAt)
	}
}

func TestFixupStage_RefusesSucceededWhenReviewAlreadyResolved(t *testing.T) {
	// Review gate already closed (merged/succeeded) — not a fix-up
	// candidate; neither stage may be touched.
	repo, impl, review := implementWithReview(t, run.StageStateSucceeded, run.StageStateSucceeded)
	ctx := context.Background()

	_, err := run.FixupStage(ctx, repo, impl.ID, run.FixupOptions{MaxPasses: 1, HardCeiling: 3})
	if !errors.Is(err, run.ErrFixupNotApplicable) {
		t.Fatalf("err = %v, want ErrFixupNotApplicable", err)
	}
	if cur, _ := repo.GetStage(ctx, impl.ID); cur.State != run.StageStateSucceeded {
		t.Errorf("implement state = %q, want unchanged (succeeded)", cur.State)
	}
	if cur, _ := repo.GetStage(ctx, review.ID); cur.State != run.StageStateSucceeded {
		t.Errorf("review state = %q, want unchanged (succeeded)", cur.State)
	}
}

func TestFixupStage_RefusesSucceededWhenNoReviewStage(t *testing.T) {
	// Succeeded implement with no review stage in the run.
	repo, stage := implementStage(t, run.StageStateSucceeded)
	ctx := context.Background()

	_, err := run.FixupStage(ctx, repo, stage.ID, run.FixupOptions{MaxPasses: 1, HardCeiling: 3})
	if !errors.Is(err, run.ErrFixupNotApplicable) {
		t.Fatalf("err = %v, want ErrFixupNotApplicable", err)
	}
	if cur, _ := repo.GetStage(ctx, stage.ID); cur.State != run.StageStateSucceeded {
		t.Errorf("implement state = %q, want unchanged (succeeded)", cur.State)
	}
}

func TestFixupStage_ReparkFailureLeavesImplementSucceeded(t *testing.T) {
	// Partial-failure safety (#780): if the review re-park fails, the
	// implement stage must stay succeeded — never orphaned in pending.
	repo, impl, review := implementWithReview(t, run.StageStateSucceeded, run.StageStateAwaitingApproval)
	repo.failTransition(review.ID, errors.New("re-park boom"))
	ctx := context.Background()

	_, err := run.FixupStage(ctx, repo, impl.ID, run.FixupOptions{MaxPasses: 1, HardCeiling: 3})
	if err == nil {
		t.Fatal("FixupStage returned nil error on re-park failure")
	}
	if cur, _ := repo.GetStage(ctx, impl.ID); cur.State != run.StageStateSucceeded {
		t.Errorf("implement state = %q, want unchanged (succeeded) on re-park failure", cur.State)
	}
	if cur, _ := repo.GetStage(ctx, review.ID); cur.State != run.StageStateAwaitingApproval {
		t.Errorf("review state = %q, want unchanged (awaiting_approval)", cur.State)
	}
}

func TestFixupStage_ImplementReopenFailureLeavesReviewReparked(t *testing.T) {
	// Partial-failure direction #2 (#780): re-park succeeds but the implement
	// re-open fails. Because re-park runs FIRST and the implement re-open
	// LAST, the run is left review=pending, implement=succeeded — a benign,
	// recoverable state: the review simply re-dispatches and re-parks, the PR
	// merges normally, and the fix-up silently no-ops (the operator can
	// re-fire). The implement is never orphaned in pending without its
	// review re-parked, which is the dangerous direction this ordering avoids.
	repo, impl, review := implementWithReview(t, run.StageStateSucceeded, run.StageStateAwaitingApproval)
	repo.failTransition(impl.ID, errors.New("re-open boom"))
	ctx := context.Background()

	_, err := run.FixupStage(ctx, repo, impl.ID, run.FixupOptions{MaxPasses: 1, HardCeiling: 3})
	if err == nil {
		t.Fatal("FixupStage returned nil error on implement re-open failure")
	}
	if cur, _ := repo.GetStage(ctx, impl.ID); cur.State != run.StageStateSucceeded {
		t.Errorf("implement state = %q, want unchanged (succeeded) on re-open failure", cur.State)
	}
	if cur, _ := repo.GetStage(ctx, review.ID); cur.State != run.StageStatePending {
		t.Errorf("review state = %q, want pending (re-parked before the failed implement re-open)", cur.State)
	}
}

// --- RestoreFixupStage (fix-up recovery, #788) ---

func TestRestoreFixupStage_PushFlowRestoresSucceededAndRepark(t *testing.T) {
	// push_and_open_pr flow: the fix-up re-dispatch failed, so the
	// implement stage is `failed` and the re-parked review stage is
	// `pending`. Recovery restores implement → succeeded and review →
	// awaiting_approval.
	repo, impl, review := implementWithReview(t, run.StageStateFailed, run.StageStatePending)
	cat := run.FailureA
	reason := "agent crashed mid fix-up"
	impl.FailureCategory = &cat
	impl.FailureReason = &reason
	repo.mu.Lock()
	repo.stages[impl.ID].FailureCategory = &cat
	repo.stages[impl.ID].FailureReason = &reason
	repo.mu.Unlock()
	ctx := context.Background()

	rec, err := run.RestoreFixupStage(ctx, repo, impl.ID, run.StageStateSucceeded, &review.ID)
	if err != nil {
		t.Fatalf("RestoreFixupStage: %v", err)
	}
	if rec.Stage.State != run.StageStateSucceeded {
		t.Errorf("implement state = %q, want succeeded", rec.Stage.State)
	}
	// The captured prior failure metadata is surfaced for the audit entry.
	if rec.PriorFailureCategory == nil || *rec.PriorFailureCategory != run.FailureA {
		t.Errorf("PriorFailureCategory = %v, want A", rec.PriorFailureCategory)
	}
	if rec.PriorFailureReason == nil || *rec.PriorFailureReason != reason {
		t.Errorf("PriorFailureReason = %v, want %q", rec.PriorFailureReason, reason)
	}
	// The restored implement stage has its stale failure metadata cleared.
	cur, _ := repo.GetStage(ctx, impl.ID)
	if cur.FailureCategory != nil {
		t.Errorf("restored FailureCategory = %v, want nil", cur.FailureCategory)
	}
	if cur.FailureReason != nil {
		t.Errorf("restored FailureReason = %v, want nil", cur.FailureReason)
	}
	// The review stage was re-parked back to its gate.
	if rec.RestoredReview == nil || rec.RestoredReview.ID != review.ID {
		t.Fatalf("RestoredReview = %+v, want the re-parked review stage", rec.RestoredReview)
	}
	curReview, _ := repo.GetStage(ctx, review.ID)
	if curReview.State != run.StageStateAwaitingApproval {
		t.Errorf("review state = %q, want awaiting_approval (restored)", curReview.State)
	}
}

func TestRestoreFixupStage_CommitYourselfRestoresAwaitingApproval(t *testing.T) {
	// commit-yourself flow: the implement stage is its own gate, so there
	// is no separate review stage. Recovery restores implement → awaiting_approval.
	repo, impl := implementStage(t, run.StageStateFailed)
	cat := run.FailureB
	reason := "implement review rejected"
	repo.mu.Lock()
	repo.stages[impl.ID].FailureCategory = &cat
	repo.stages[impl.ID].FailureReason = &reason
	repo.mu.Unlock()
	ctx := context.Background()

	rec, err := run.RestoreFixupStage(ctx, repo, impl.ID, run.StageStateAwaitingApproval, nil)
	if err != nil {
		t.Fatalf("RestoreFixupStage: %v", err)
	}
	if rec.Stage.State != run.StageStateAwaitingApproval {
		t.Errorf("implement state = %q, want awaiting_approval", rec.Stage.State)
	}
	if rec.RestoredReview != nil {
		t.Errorf("RestoredReview = %+v, want nil (no separate review stage)", rec.RestoredReview)
	}
	cur, _ := repo.GetStage(ctx, impl.ID)
	if cur.FailureCategory != nil || cur.FailureReason != nil {
		t.Errorf("restored failure metadata = (%v, %v), want both nil", cur.FailureCategory, cur.FailureReason)
	}
}

func TestRestoreFixupStage_NotFailedIsNoOp(t *testing.T) {
	// The implement stage is not currently failed — nothing to recover.
	// RestoreFixupStage signals ErrFixupRecoveryNotApplicable and touches
	// nothing.
	repo, impl := implementStage(t, run.StageStateSucceeded)
	ctx := context.Background()

	_, err := run.RestoreFixupStage(ctx, repo, impl.ID, run.StageStateSucceeded, nil)
	if !errors.Is(err, run.ErrFixupRecoveryNotApplicable) {
		t.Fatalf("err = %v, want ErrFixupRecoveryNotApplicable", err)
	}
	if cur, _ := repo.GetStage(ctx, impl.ID); cur.State != run.StageStateSucceeded {
		t.Errorf("implement state = %q, want unchanged (succeeded)", cur.State)
	}
}

func TestRestoreFixupStage_RejectsInvalidPriorState(t *testing.T) {
	// Only succeeded / awaiting_approval are restorable gate states.
	repo, impl := implementStage(t, run.StageStateFailed)
	ctx := context.Background()

	_, err := run.RestoreFixupStage(ctx, repo, impl.ID, run.StageStatePending, nil)
	if err == nil {
		t.Fatal("RestoreFixupStage accepted prior_state=pending; want an error")
	}
	if errors.Is(err, run.ErrFixupRecoveryNotApplicable) {
		t.Errorf("err = %v, want a validation error (not the not-applicable no-op)", err)
	}
}

func TestRestoreFixupStage_ReparkFailureLeavesImplementFailed(t *testing.T) {
	// Partial-failure ordering (#788): if the review re-park fails, the
	// implement stage must stay `failed` — never restored to a healthy
	// state while the review gate is still gone.
	repo, impl, review := implementWithReview(t, run.StageStateFailed, run.StageStatePending)
	repo.failTransition(review.ID, errors.New("re-park boom"))
	ctx := context.Background()

	_, err := run.RestoreFixupStage(ctx, repo, impl.ID, run.StageStateSucceeded, &review.ID)
	if err == nil {
		t.Fatal("RestoreFixupStage returned nil error on re-park failure")
	}
	if cur, _ := repo.GetStage(ctx, impl.ID); cur.State != run.StageStateFailed {
		t.Errorf("implement state = %q, want unchanged (failed) on re-park failure", cur.State)
	}
}

func TestFixupStage_RemainingBudgetWithHigherBound(t *testing.T) {
	repo, stage := implementStage(t, run.StageStateAwaitingApproval)

	dec, err := run.FixupStage(context.Background(), repo, stage.ID, run.FixupOptions{
		PriorPassCount: 1,
		MaxPasses:      3,
		HardCeiling:    3,
	})
	if err != nil {
		t.Fatalf("FixupStage: %v", err)
	}
	if dec.RemainingBudget != 1 {
		t.Errorf("RemainingBudget = %d, want 1 (3 - 2 used)", dec.RemainingBudget)
	}
}

func TestFixupStage_ForceAdditionalPassGrantsPassPastBudget(t *testing.T) {
	// The normal budget (1) is spent, but the operator override grants one
	// pass beyond it, still under the hard ceiling of 3.
	repo, stage := implementStage(t, run.StageStateAwaitingApproval)

	dec, err := run.FixupStage(context.Background(), repo, stage.ID, run.FixupOptions{
		PriorPassCount:      1,
		MaxPasses:           1,
		HardCeiling:         3,
		ForceAdditionalPass: true,
	})
	if err != nil {
		t.Fatalf("FixupStage with override: %v", err)
	}
	if !dec.Forced {
		t.Errorf("Forced = false, want true (override carried it past the normal budget)")
	}
	if dec.Stage.State != run.StageStatePending {
		t.Errorf("post-fixup state = %q, want pending", dec.Stage.State)
	}
	// RemainingBudget reflects the NORMAL budget only, so a forced pass
	// reports 0.
	if dec.RemainingBudget != 0 {
		t.Errorf("RemainingBudget = %d, want 0 (forced pass past the normal budget)", dec.RemainingBudget)
	}
}

func TestFixupStage_HardCeilingWinsEvenWhenForced(t *testing.T) {
	// At the hard ceiling the override can NOT push past — the ceiling is
	// the absolute stop, surfaced as the distinct ErrFixupCeilingReached.
	repo, stage := implementStage(t, run.StageStateAwaitingApproval)

	_, err := run.FixupStage(context.Background(), repo, stage.ID, run.FixupOptions{
		PriorPassCount:      3,
		MaxPasses:           1,
		HardCeiling:         3,
		ForceAdditionalPass: true,
	})
	if !errors.Is(err, run.ErrFixupCeilingReached) {
		t.Fatalf("err = %v, want ErrFixupCeilingReached", err)
	}
	if errors.Is(err, run.ErrFixupBudgetExhausted) {
		t.Errorf("err = %v, want the distinct ceiling error, not budget_exhausted", err)
	}
	// The stage must NOT have been re-opened — the ceiling is checked
	// before any transition.
	cur, _ := repo.GetStage(context.Background(), stage.ID)
	if cur.State != run.StageStateAwaitingApproval {
		t.Errorf("state = %q, want unchanged (awaiting_approval)", cur.State)
	}
}

func TestFixupStage_RefusesTerminalRun(t *testing.T) {
	// #968 defense-in-depth: a terminal run's stages must never be
	// re-opened — there is no live gate for the fix-up to flow back into,
	// so the re-opened stages would strand at pending forever. Refused for
	// both the normal and the operator-forced pass, for every terminal
	// run state.
	for _, runState := range []run.State{run.StateSucceeded, run.StateFailed, run.StateCancelled} {
		for _, forced := range []bool{false, true} {
			repo, impl, review := implementWithReview(t, run.StageStateSucceeded, run.StageStateAwaitingApproval)
			repo.setRunState(impl.RunID, runState)
			priorPasses := 0
			if forced {
				// The forced pass models the incident shape: normal budget
				// spent, override requested.
				priorPasses = 1
			}

			_, err := run.FixupStage(context.Background(), repo, impl.ID, run.FixupOptions{
				PriorPassCount:      priorPasses,
				MaxPasses:           1,
				HardCeiling:         3,
				ForceAdditionalPass: forced,
			})
			if !errors.Is(err, run.ErrFixupNotApplicable) {
				t.Fatalf("run %s forced=%v: err = %v, want ErrFixupNotApplicable", runState, forced, err)
			}
			// Neither stage may be touched.
			if cur, _ := repo.GetStage(context.Background(), impl.ID); cur.State != run.StageStateSucceeded {
				t.Errorf("run %s forced=%v: implement state = %q, want unchanged (succeeded)", runState, forced, cur.State)
			}
			if cur, _ := repo.GetStage(context.Background(), review.ID); cur.State != run.StageStateAwaitingApproval {
				t.Errorf("run %s forced=%v: review state = %q, want unchanged (awaiting_approval)", runState, forced, cur.State)
			}
		}
	}
}

func TestFixupStage_ForcedPassReparksReviewOnRunningRun(t *testing.T) {
	// #968 regression: a FORCED pass on a healthy running run must re-park
	// the review stage identically to the normal-budget branch — the
	// incident's first hypothesis was that the forced branch skipped the
	// re-park; this pins that it does not.
	repo, impl, review := implementWithReview(t, run.StageStateSucceeded, run.StageStateAwaitingApproval)

	dec, err := run.FixupStage(context.Background(), repo, impl.ID, run.FixupOptions{
		PriorPassCount:      1,
		MaxPasses:           1,
		HardCeiling:         3,
		ForceAdditionalPass: true,
	})
	if err != nil {
		t.Fatalf("FixupStage forced on running run: %v", err)
	}
	if !dec.Forced {
		t.Errorf("Forced = false, want true")
	}
	if dec.Stage.State != run.StageStatePending {
		t.Errorf("implement state = %q, want pending", dec.Stage.State)
	}
	if dec.ReparkedReview == nil {
		t.Fatal("ReparkedReview = nil, want the re-parked review stage (forced branch must re-park identically to the normal branch)")
	}
	if dec.ReparkedReview.ID != review.ID {
		t.Errorf("ReparkedReview.ID = %s, want %s", dec.ReparkedReview.ID, review.ID)
	}
	if dec.ReparkedReview.State != run.StageStatePending {
		t.Errorf("re-parked review state = %q, want pending", dec.ReparkedReview.State)
	}
}

func TestFixupStage_NotForcedAtBudgetStillBudgetExhausted(t *testing.T) {
	// Default (no override) at the normal budget still refuses with
	// budget_exhausted, not the ceiling error — the ceiling has headroom.
	repo, stage := implementStage(t, run.StageStateAwaitingApproval)

	_, err := run.FixupStage(context.Background(), repo, stage.ID, run.FixupOptions{
		PriorPassCount: 1,
		MaxPasses:      1,
		HardCeiling:    3,
	})
	if !errors.Is(err, run.ErrFixupBudgetExhausted) {
		t.Fatalf("err = %v, want ErrFixupBudgetExhausted", err)
	}
	if errors.Is(err, run.ErrFixupCeilingReached) {
		t.Errorf("err = %v, want budget_exhausted, not the ceiling error", err)
	}
}

// implementReviewAcceptance builds the acceptance-time shape (E31.8 / #1536):
// an implement stage, a review stage, and an acceptance stage sharing one
// RunID. It is what an acceptance-driven fix-up walks.
func implementReviewAcceptance(t *testing.T, implState, reviewState, acceptanceState run.StageState) (*fixupRepo, *run.Stage, *run.Stage, *run.Stage) {
	t.Helper()
	runID := uuid.New()
	impl := newStage(implState)
	impl.Type = run.StageTypeImplement
	impl.RunID = runID
	impl.Sequence = 1
	review := newStage(reviewState)
	review.Type = run.StageTypeReview
	review.RunID = runID
	review.Sequence = 2
	acc := newStage(acceptanceState)
	acc.Type = run.StageTypeAcceptance
	acc.RunID = runID
	acc.Sequence = 3
	repo := newFixupRepo(impl, review, acc)
	return repo, impl, review, acc
}

func TestFixupStage_AcceptanceMode_ReparksSucceededReviewAndReopensAcceptance(t *testing.T) {
	// Canonical acceptance-time shape: implement + review + acceptance all
	// succeeded. The fix-up re-parks the succeeded review, re-opens the
	// succeeded acceptance stage, and re-opens the implement stage.
	repo, impl, review, acc := implementReviewAcceptance(t, run.StageStateSucceeded, run.StageStateSucceeded, run.StageStateSucceeded)
	ctx := context.Background()

	dec, err := run.FixupStage(ctx, repo, impl.ID, run.FixupOptions{
		MaxPasses:         1,
		HardCeiling:       3,
		AcceptanceStageID: &acc.ID,
	})
	if err != nil {
		t.Fatalf("FixupStage: %v", err)
	}
	if dec.Stage.State != run.StageStatePending {
		t.Errorf("implement state = %q, want pending", dec.Stage.State)
	}
	if dec.ReparkedReview == nil || dec.ReparkedReview.ID != review.ID || dec.ReparkedReview.State != run.StageStatePending {
		t.Errorf("ReparkedReview = %+v, want review %s at pending", dec.ReparkedReview, review.ID)
	}
	if dec.ReopenedAcceptance == nil || dec.ReopenedAcceptance.ID != acc.ID || dec.ReopenedAcceptance.State != run.StageStatePending {
		t.Errorf("ReopenedAcceptance = %+v, want acceptance %s at pending", dec.ReopenedAcceptance, acc.ID)
	}
	if cur, _ := repo.GetStage(ctx, acc.ID); cur.State != run.StageStatePending {
		t.Errorf("acceptance stage in repo = %q, want pending", cur.State)
	}
}

func TestFixupStage_AcceptanceMode_ReparksAwaitingApprovalReview(t *testing.T) {
	repo, impl, review, acc := implementReviewAcceptance(t, run.StageStateSucceeded, run.StageStateAwaitingApproval, run.StageStateSucceeded)
	ctx := context.Background()

	dec, err := run.FixupStage(ctx, repo, impl.ID, run.FixupOptions{
		MaxPasses: 1, HardCeiling: 3, AcceptanceStageID: &acc.ID,
	})
	if err != nil {
		t.Fatalf("FixupStage: %v", err)
	}
	if dec.ReparkedReview == nil || dec.ReparkedReview.ID != review.ID || dec.ReparkedReview.State != run.StageStatePending {
		t.Errorf("ReparkedReview = %+v, want review re-parked to pending", dec.ReparkedReview)
	}
}

func TestFixupStage_AcceptanceMode_ReviewAlreadyPendingUntouched(t *testing.T) {
	repo, impl, review, acc := implementReviewAcceptance(t, run.StageStateSucceeded, run.StageStatePending, run.StageStateSucceeded)
	ctx := context.Background()

	dec, err := run.FixupStage(ctx, repo, impl.ID, run.FixupOptions{
		MaxPasses: 1, HardCeiling: 3, AcceptanceStageID: &acc.ID,
	})
	if err != nil {
		t.Fatalf("FixupStage: %v", err)
	}
	// An already-pending review is not re-parked.
	if dec.ReparkedReview != nil {
		t.Errorf("ReparkedReview = %+v, want nil (review already pending)", dec.ReparkedReview)
	}
	if cur, _ := repo.GetStage(ctx, review.ID); cur.State != run.StageStatePending {
		t.Errorf("review state = %q, want unchanged (pending)", cur.State)
	}
	if dec.ReopenedAcceptance == nil || dec.ReopenedAcceptance.State != run.StageStatePending {
		t.Errorf("ReopenedAcceptance = %+v, want acceptance re-opened to pending", dec.ReopenedAcceptance)
	}
}

func TestFixupStage_AcceptanceMode_NoReviewStageProceeds(t *testing.T) {
	// Implement + acceptance only, no review stage: the fix-up proceeds with
	// no re-park (the acceptance re-open still forces re-validation).
	runID := uuid.New()
	impl := newStage(run.StageStateSucceeded)
	impl.Type = run.StageTypeImplement
	impl.RunID = runID
	acc := newStage(run.StageStateSucceeded)
	acc.Type = run.StageTypeAcceptance
	acc.RunID = runID
	repo := newFixupRepo(impl, acc)
	ctx := context.Background()

	dec, err := run.FixupStage(ctx, repo, impl.ID, run.FixupOptions{
		MaxPasses: 1, HardCeiling: 3, AcceptanceStageID: &acc.ID,
	})
	if err != nil {
		t.Fatalf("FixupStage: %v", err)
	}
	if dec.ReparkedReview != nil {
		t.Errorf("ReparkedReview = %+v, want nil (no review stage)", dec.ReparkedReview)
	}
	if dec.Stage.State != run.StageStatePending {
		t.Errorf("implement state = %q, want pending", dec.Stage.State)
	}
	if dec.ReopenedAcceptance == nil || dec.ReopenedAcceptance.State != run.StageStatePending {
		t.Errorf("ReopenedAcceptance = %+v, want acceptance re-opened to pending", dec.ReopenedAcceptance)
	}
}

func TestFixupStage_AcceptanceMode_AcceptanceReopenFailureLeavesImplementSucceeded(t *testing.T) {
	// Partial-failure ordering (E31.8): the acceptance re-open runs BEFORE the
	// implement re-open, so an acceptance-reopen failure returns before the
	// implement stage is touched — it stays succeeded.
	repo, impl, _, acc := implementReviewAcceptance(t, run.StageStateSucceeded, run.StageStatePending, run.StageStateSucceeded)
	repo.failTransition(acc.ID, errors.New("acceptance re-open boom"))
	ctx := context.Background()

	_, err := run.FixupStage(ctx, repo, impl.ID, run.FixupOptions{
		MaxPasses: 1, HardCeiling: 3, AcceptanceStageID: &acc.ID,
	})
	if err == nil {
		t.Fatal("FixupStage returned nil error on acceptance re-open failure")
	}
	if cur, _ := repo.GetStage(ctx, impl.ID); cur.State != run.StageStateSucceeded {
		t.Errorf("implement state = %q, want unchanged (succeeded) on acceptance re-open failure", cur.State)
	}
}

func TestFixupStage_NilAcceptanceStageID_ClassicBehavior(t *testing.T) {
	// A nil AcceptanceStageID preserves the classic implement-review fix-up:
	// no acceptance re-open, and ReopenedAcceptance stays nil.
	repo, impl, review := implementWithReview(t, run.StageStateSucceeded, run.StageStateAwaitingApproval)
	ctx := context.Background()

	dec, err := run.FixupStage(ctx, repo, impl.ID, run.FixupOptions{MaxPasses: 1, HardCeiling: 3})
	if err != nil {
		t.Fatalf("FixupStage: %v", err)
	}
	if dec.ReopenedAcceptance != nil {
		t.Errorf("ReopenedAcceptance = %+v, want nil on the classic path", dec.ReopenedAcceptance)
	}
	if dec.ReparkedReview == nil || dec.ReparkedReview.ID != review.ID {
		t.Errorf("ReparkedReview = %+v, want the classic re-park", dec.ReparkedReview)
	}
}
