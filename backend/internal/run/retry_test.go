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

	dec, err := run.RetryStage(context.Background(), repo, stage.ID, run.RetryOptions{})
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

	_, err := run.RetryStage(context.Background(), repo, stage.ID, run.RetryOptions{})
	if !errors.Is(err, run.ErrRetryNotApplicable) {
		t.Fatalf("err = %v, want ErrRetryNotApplicable", err)
	}
}

func TestRetryStage_BNotApplicable(t *testing.T) {
	repo, stage := failedStage(t, run.FailureB, "forbidden_paths violated: backend/internal/policy/secret.go")

	_, err := run.RetryStage(context.Background(), repo, stage.ID, run.RetryOptions{})
	if !errors.Is(err, run.ErrRetryNotApplicable) {
		t.Errorf("err = %v, want ErrRetryNotApplicable", err)
	}
}

// #698: OverrideB admits a genuine category-B failure onto the A/C
// failed → pending path. The stage re-opens to pending (it re-runs and
// the policy gate re-evaluates the new diff — the override does not
// bypass the gate) and the decision is flagged Overridden so the
// handler can write the distinct stage_override_retried audit.
func TestRetryStage_BOverrideReopensToPending(t *testing.T) {
	repo, stage := failedStage(t, run.FailureB, "forbidden_paths violated: backend/internal/policy/secret.go")

	dec, err := run.RetryStage(context.Background(), repo, stage.ID, run.RetryOptions{OverrideB: true})
	if err != nil {
		t.Fatalf("RetryStage with OverrideB: %v", err)
	}
	if dec.PriorCategory != run.FailureB {
		t.Errorf("PriorCategory = %q, want B", dec.PriorCategory)
	}
	if !dec.Overridden {
		t.Error("dec.Overridden = false, want true for a B override")
	}
	if dec.Stage.State != run.StageStatePending {
		t.Errorf("post-override state = %q, want pending", dec.Stage.State)
	}
	if dec.Stage.FailureCategory != nil || dec.Stage.FailureReason != nil {
		t.Errorf("post-override stage still carries failure metadata: %+v", dec.Stage)
	}
}

// OverrideB only relaxes category B. A category-D rejection (the other
// non-retryable case) stays refused even with the override set — the
// escape hatch is scoped to constraint/policy failures.
func TestRetryStage_OverrideDoesNotApplyToDRejection(t *testing.T) {
	repo, stage := failedStage(t, run.FailureD, "gate rejected by approver")

	_, err := run.RetryStage(context.Background(), repo, stage.ID, run.RetryOptions{OverrideB: true})
	if !errors.Is(err, run.ErrRetryNotApplicable) {
		t.Errorf("err = %v, want ErrRetryNotApplicable (override is B-only)", err)
	}
}

// E8.6: A and C retries now transition the stage back to pending
// and let the caller (the handler) hand off to the orchestrator
// for re-dispatch. The decision tree itself just does the
// state-machine move.

func TestRetryStage_ATransitionsToPending(t *testing.T) {
	repo, stage := failedStage(t, run.FailureA, "agent crashed: SIGSEGV")

	dec, err := run.RetryStage(context.Background(), repo, stage.ID, run.RetryOptions{})
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

	dec, err := run.RetryStage(context.Background(), repo, stage.ID, run.RetryOptions{})
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

	_, err := run.RetryStage(context.Background(), repo, stage.ID, run.RetryOptions{})
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

	_, err := run.RetryStage(context.Background(), repo, stage.ID, run.RetryOptions{})
	if !errors.Is(err, run.ErrRetryNotApplicable) {
		t.Errorf("err = %v, want ErrRetryNotApplicable", err)
	}
}

func TestRetryableFailure(t *testing.T) {
	cases := []struct {
		name   string
		cat    run.FailureCategory
		reason string
		want   bool
	}{
		{"A agent failure", run.FailureA, "agent crashed", true},
		{"B policy violation", run.FailureB, "scope violation", false},
		{"C infrastructure", run.FailureC, "runner OOM", true},
		{"D sla timeout", run.FailureD, "sla_timeout: 5h elapsed (deadline 4h)", true},
		{"D gate rejected", run.FailureD, "gate rejected by approver", false},
		{"D other variant", run.FailureD, "some future D reason", false},
		{"unknown category", run.FailureCategory("Z"), "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := run.RetryableFailure(tc.cat, tc.reason); got != tc.want {
				t.Errorf("RetryableFailure(%q, %q) = %v, want %v", tc.cat, tc.reason, got, tc.want)
			}
		})
	}
}

func TestImplementFailureRetryable(t *testing.T) {
	mkImpl := func(state run.StageState, cat *run.FailureCategory, reason string) *run.Stage {
		s := &run.Stage{Type: run.StageTypeImplement, State: state, FailureCategory: cat}
		if reason != "" {
			s.FailureReason = &reason
		}
		return s
	}
	catC := run.FailureC
	catB := run.FailureB

	cases := []struct {
		name   string
		stages []*run.Stage
		want   bool
	}{
		{"failed implement category C", []*run.Stage{mkImpl(run.StageStateFailed, &catC, "infra")}, true},
		{"failed implement category B", []*run.Stage{mkImpl(run.StageStateFailed, &catB, "policy")}, false},
		{"failed implement no category", []*run.Stage{mkImpl(run.StageStateFailed, nil, "")}, false},
		{"no failed implement stage", []*run.Stage{mkImpl(run.StageStateSucceeded, nil, "")}, false},
		{"no stages", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := run.ImplementFailureRetryable(tc.stages); got != tc.want {
				t.Errorf("ImplementFailureRetryable = %v, want %v", got, tc.want)
			}
		})
	}
}
