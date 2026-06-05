package run_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// implementStage builds an implement stage in the given state — what
// FixupStage walks at the start of every call. Reuses newStage's
// fixture and overrides the type, since the default fixture is a plan
// stage.
func implementStage(t *testing.T, state run.StageState) (*memRepo, *run.Stage) {
	t.Helper()
	stage := newStage(state)
	stage.Type = run.StageTypeImplement
	repo := newMemRepo(stage)
	return repo, stage
}

// implementWithReview builds an implement stage and a review stage
// sharing one RunID — the push_and_open_pr shape (#780): the implement
// stage has succeeded (PR opened) while the review stage holds the gate.
func implementWithReview(t *testing.T, implState, reviewState run.StageState) (*memRepo, *run.Stage, *run.Stage) {
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
	repo := newMemRepo(impl, review)
	return repo, impl, review
}

func TestFixupStage_ReopensAwaitingApprovalToPending(t *testing.T) {
	repo, stage := implementStage(t, run.StageStateAwaitingApproval)

	dec, err := run.FixupStage(context.Background(), repo, stage.ID, run.FixupOptions{
		PriorPassCount: 0,
		MaxPasses:      1,
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

	_, err := run.FixupStage(context.Background(), repo, stage.ID, run.FixupOptions{MaxPasses: 1})
	if !errors.Is(err, run.ErrFixupNotApplicable) {
		t.Fatalf("err = %v, want ErrFixupNotApplicable", err)
	}
}

func TestFixupStage_RefusesWrongState(t *testing.T) {
	// An implement stage that is running (not parked at the gate) is
	// not a fix-up candidate.
	repo, stage := implementStage(t, run.StageStateRunning)

	_, err := run.FixupStage(context.Background(), repo, stage.ID, run.FixupOptions{MaxPasses: 1})
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

	dec, err := run.FixupStage(ctx, repo, impl.ID, run.FixupOptions{MaxPasses: 1})
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

	_, err := run.FixupStage(ctx, repo, impl.ID, run.FixupOptions{MaxPasses: 1})
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

	_, err := run.FixupStage(ctx, repo, stage.ID, run.FixupOptions{MaxPasses: 1})
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

	_, err := run.FixupStage(ctx, repo, impl.ID, run.FixupOptions{MaxPasses: 1})
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

func TestFixupStage_RemainingBudgetWithHigherBound(t *testing.T) {
	repo, stage := implementStage(t, run.StageStateAwaitingApproval)

	dec, err := run.FixupStage(context.Background(), repo, stage.ID, run.FixupOptions{
		PriorPassCount: 1,
		MaxPasses:      3,
	})
	if err != nil {
		t.Fatalf("FixupStage: %v", err)
	}
	if dec.RemainingBudget != 1 {
		t.Errorf("RemainingBudget = %d, want 1 (3 - 2 used)", dec.RemainingBudget)
	}
}
