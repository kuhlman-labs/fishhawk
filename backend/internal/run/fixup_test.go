package run_test

import (
	"context"
	"errors"
	"testing"

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
