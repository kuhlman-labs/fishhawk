package run_test

import (
	"context"
	"errors"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// memRepo from failure_test.go satisfies run.Repository for the
// retry-helper tests too. Reuses the same fixture builder.

// failedStage builds a stage in failed state with the given category
// and reason — what RetryStage walks at the start of every call.
func failedStage(t *testing.T, cat run.FailureCategory, reason string) (*memRepo, *run.Stage) {
	t.Helper()
	stage := newStage(run.StageStateFailed)
	stage.FailureCategory = &cat
	stage.FailureReason = &reason
	repo := newMemRepo(stage)
	return repo, stage
}

func TestRetryStage_DTimeoutReopensTheGate(t *testing.T) {
	repo, stage := failedStage(t, run.FailureD, "sla_timeout: 5h elapsed (deadline 4h)")

	dec, err := run.RetryStage(context.Background(), repo, stage.ID)
	if err != nil {
		t.Fatalf("RetryStage: %v", err)
	}

	if dec.PriorCategory != run.FailureD {
		t.Errorf("PriorCategory = %q, want D", dec.PriorCategory)
	}
	if dec.PriorReason != "sla_timeout: 5h elapsed (deadline 4h)" {
		t.Errorf("PriorReason = %q", dec.PriorReason)
	}
	if dec.Stage.State != run.StageStateAwaitingApproval {
		t.Errorf("post-retry state = %q, want awaiting_approval", dec.Stage.State)
	}
	if dec.Stage.FailureCategory != nil || dec.Stage.FailureReason != nil {
		t.Errorf("post-retry stage still carries failure metadata: %+v", dec.Stage)
	}
}

func TestRetryStage_DRejectedRefused(t *testing.T) {
	repo, stage := failedStage(t, run.FailureD, "gate rejected by approver")

	_, err := run.RetryStage(context.Background(), repo, stage.ID)
	if !errors.Is(err, run.ErrRetryNotApplicable) {
		t.Fatalf("err = %v, want ErrRetryNotApplicable", err)
	}
}

func TestRetryStage_BNotApplicable(t *testing.T) {
	repo, stage := failedStage(t, run.FailureB, "forbidden_paths violated: backend/internal/policy/secret.go")

	_, err := run.RetryStage(context.Background(), repo, stage.ID)
	if !errors.Is(err, run.ErrRetryNotApplicable) {
		t.Errorf("err = %v, want ErrRetryNotApplicable", err)
	}
}

// E8.6: A and C retries now transition the stage back to pending
// and let the caller (the handler) hand off to the orchestrator
// for re-dispatch. The decision tree itself just does the
// state-machine move.

func TestRetryStage_ATransitionsToPending(t *testing.T) {
	repo, stage := failedStage(t, run.FailureA, "agent crashed: SIGSEGV")

	dec, err := run.RetryStage(context.Background(), repo, stage.ID)
	if err != nil {
		t.Fatalf("RetryStage: %v", err)
	}
	if dec.PriorCategory != run.FailureA {
		t.Errorf("PriorCategory = %q, want A", dec.PriorCategory)
	}
	if dec.Stage.State != run.StageStatePending {
		t.Errorf("post-retry state = %q, want pending", dec.Stage.State)
	}
	if dec.Stage.FailureCategory != nil || dec.Stage.FailureReason != nil {
		t.Errorf("post-retry stage still carries failure metadata: %+v", dec.Stage)
	}
}

func TestRetryStage_CTransitionsToPending(t *testing.T) {
	repo, stage := failedStage(t, run.FailureC, "dispatch_watchdog: 70m elapsed (deadline 60m)")

	dec, err := run.RetryStage(context.Background(), repo, stage.ID)
	if err != nil {
		t.Fatalf("RetryStage: %v", err)
	}
	if dec.PriorCategory != run.FailureC {
		t.Errorf("PriorCategory = %q, want C", dec.PriorCategory)
	}
	if dec.Stage.State != run.StageStatePending {
		t.Errorf("post-retry state = %q, want pending", dec.Stage.State)
	}
}

func TestRetryStage_NonFailedStageRefused(t *testing.T) {
	stage := newStage(run.StageStateAwaitingApproval)
	repo := newMemRepo(stage)

	_, err := run.RetryStage(context.Background(), repo, stage.ID)
	if !errors.Is(err, run.ErrRetryNotApplicable) {
		t.Errorf("err = %v, want ErrRetryNotApplicable", err)
	}
}

func TestRetryStage_FailedWithoutCategoryRefused(t *testing.T) {
	// Defensive: if the database row carries state=failed but no
	// FailureCategory (which the schema check forbids on insert,
	// but might happen mid-migration), refuse cleanly.
	stage := newStage(run.StageStateFailed)
	stage.FailureCategory = nil
	repo := newMemRepo(stage)

	_, err := run.RetryStage(context.Background(), repo, stage.ID)
	if !errors.Is(err, run.ErrRetryNotApplicable) {
		t.Errorf("err = %v, want ErrRetryNotApplicable", err)
	}
}
