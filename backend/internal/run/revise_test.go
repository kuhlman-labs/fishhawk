package run_test

import (
	"context"
	"errors"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// planStageAtGate builds a plan stage parked at awaiting_approval — what
// RevisePlanStage walks at the start of every call. newStage's default
// fixture is already a plan stage, so only the state is supplied.
func planStageAtGate(state run.StageState) (*memRepo, *run.Stage) {
	stage := newStage(state)
	repo := newMemRepo(stage)
	return repo, stage
}

func TestRevisePlanStage_ReopensAwaitingApprovalToPending(t *testing.T) {
	repo, stage := planStageAtGate(run.StageStateAwaitingApproval)

	dec, err := run.RevisePlanStage(context.Background(), repo, stage.ID, run.ReviseOptions{
		PriorPassCount: 0,
		MaxPasses:      1,
		HardCeiling:    3,
	})
	if err != nil {
		t.Fatalf("RevisePlanStage: %v", err)
	}
	if dec.PriorState != run.StageStateAwaitingApproval {
		t.Errorf("PriorState = %q, want awaiting_approval", dec.PriorState)
	}
	if dec.Stage.State != run.StageStatePending {
		t.Errorf("post-revise state = %q, want pending", dec.Stage.State)
	}
	if dec.RemainingBudget != 0 {
		t.Errorf("RemainingBudget = %d, want 0 (1 of 1 used)", dec.RemainingBudget)
	}
	if dec.Forced {
		t.Errorf("Forced = true, want false (first pass within budget)")
	}
}

func TestRevisePlanStage_RefusesNonPlanStage(t *testing.T) {
	stage := newStage(run.StageStateAwaitingApproval)
	stage.Type = run.StageTypeImplement
	repo := newMemRepo(stage)

	_, err := run.RevisePlanStage(context.Background(), repo, stage.ID, run.ReviseOptions{MaxPasses: 1, HardCeiling: 3})
	if !errors.Is(err, run.ErrReviseNotApplicable) {
		t.Fatalf("err = %v, want ErrReviseNotApplicable", err)
	}
}

func TestRevisePlanStage_RefusesWrongState(t *testing.T) {
	// A plan stage that is running (not parked at the gate) is not a
	// revise candidate.
	repo, stage := planStageAtGate(run.StageStateRunning)

	_, err := run.RevisePlanStage(context.Background(), repo, stage.ID, run.ReviseOptions{MaxPasses: 1, HardCeiling: 3})
	if !errors.Is(err, run.ErrReviseNotApplicable) {
		t.Fatalf("err = %v, want ErrReviseNotApplicable", err)
	}
	cur, _ := repo.GetStage(context.Background(), stage.ID)
	if cur.State != run.StageStateRunning {
		t.Errorf("state = %q, want unchanged (running)", cur.State)
	}
}

func TestRevisePlanStage_RefusesWhenBudgetExhausted(t *testing.T) {
	repo, stage := planStageAtGate(run.StageStateAwaitingApproval)

	// One pass already consumed against a default bound of 1.
	_, err := run.RevisePlanStage(context.Background(), repo, stage.ID, run.ReviseOptions{
		PriorPassCount: 1,
		MaxPasses:      1,
		HardCeiling:    3,
	})
	if !errors.Is(err, run.ErrReviseBudgetExhausted) {
		t.Fatalf("err = %v, want ErrReviseBudgetExhausted", err)
	}
	// The stage must NOT have been re-opened — the bound is checked before
	// any transition.
	cur, _ := repo.GetStage(context.Background(), stage.ID)
	if cur.State != run.StageStateAwaitingApproval {
		t.Errorf("state = %q, want unchanged (awaiting_approval)", cur.State)
	}
}

func TestRevisePlanStage_RemainingBudgetWithHigherBound(t *testing.T) {
	repo, stage := planStageAtGate(run.StageStateAwaitingApproval)

	dec, err := run.RevisePlanStage(context.Background(), repo, stage.ID, run.ReviseOptions{
		PriorPassCount: 1,
		MaxPasses:      3,
		HardCeiling:    3,
	})
	if err != nil {
		t.Fatalf("RevisePlanStage: %v", err)
	}
	if dec.RemainingBudget != 1 {
		t.Errorf("RemainingBudget = %d, want 1 (3 - 2 used)", dec.RemainingBudget)
	}
}

func TestRevisePlanStage_ForceAdditionalPassGrantsPassPastBudget(t *testing.T) {
	// The normal budget (1) is spent, but the operator override grants one
	// pass beyond it, still under the hard ceiling of 3.
	repo, stage := planStageAtGate(run.StageStateAwaitingApproval)

	dec, err := run.RevisePlanStage(context.Background(), repo, stage.ID, run.ReviseOptions{
		PriorPassCount:      1,
		MaxPasses:           1,
		HardCeiling:         3,
		ForceAdditionalPass: true,
	})
	if err != nil {
		t.Fatalf("RevisePlanStage with override: %v", err)
	}
	if !dec.Forced {
		t.Errorf("Forced = false, want true (override carried it past the normal budget)")
	}
	if dec.Stage.State != run.StageStatePending {
		t.Errorf("post-revise state = %q, want pending", dec.Stage.State)
	}
	if dec.RemainingBudget != 0 {
		t.Errorf("RemainingBudget = %d, want 0 (forced pass past the normal budget)", dec.RemainingBudget)
	}
}

func TestRevisePlanStage_HardCeilingWinsEvenWhenForced(t *testing.T) {
	// At the hard ceiling the override can NOT push past — the ceiling is
	// the absolute stop, surfaced as the distinct ErrReviseCeilingReached.
	repo, stage := planStageAtGate(run.StageStateAwaitingApproval)

	_, err := run.RevisePlanStage(context.Background(), repo, stage.ID, run.ReviseOptions{
		PriorPassCount:      3,
		MaxPasses:           1,
		HardCeiling:         3,
		ForceAdditionalPass: true,
	})
	if !errors.Is(err, run.ErrReviseCeilingReached) {
		t.Fatalf("err = %v, want ErrReviseCeilingReached", err)
	}
	if errors.Is(err, run.ErrReviseBudgetExhausted) {
		t.Errorf("err = %v, want the distinct ceiling error, not budget_exhausted", err)
	}
	cur, _ := repo.GetStage(context.Background(), stage.ID)
	if cur.State != run.StageStateAwaitingApproval {
		t.Errorf("state = %q, want unchanged (awaiting_approval)", cur.State)
	}
}
