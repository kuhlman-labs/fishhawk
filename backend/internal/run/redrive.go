package run

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// ErrRedriveNotApplicable is returned by RedriveChild when the target
// run is not a re-drivable decomposition child: it was not minted by a
// parent fanout (DecomposedFrom is nil), it is not in the failed state,
// or it has no failed implement stage to re-open. Handlers map this to
// a 422 Unprocessable Entity with a category-specific explanation.
var ErrRedriveNotApplicable = errors.New("redrive not applicable")

// RedriveDecision summarizes what RedriveChild did, for the audit
// trail and the handler's response.
type RedriveDecision struct {
	// PriorCategory is the implement stage's failure category before
	// the re-drive. Captured pre-transition so the audit entry records
	// which failure was re-driven verbatim.
	PriorCategory FailureCategory

	// PriorReason is the implement stage's failure_reason from before
	// the re-drive, for the same audit-trail reason.
	PriorReason string

	// Stage is the post-redrive implement stage row (in pending).
	Stage *Stage

	// Run is the post-redrive run row (in running).
	Run *Run
}

// RedriveChild re-opens a failed decomposition child run so the
// orchestrator can re-dispatch it (#698). It is the operator recovery
// action for a parent parked in awaiting_children because every failed
// child was in a retryable category (A/C, or D-timeout) — see
// run.RetryableFailure and the parent-resolution paths that park
// rather than resolve.
//
// The sequence is:
//
//  1. Validate the run is a failed decomposition child
//     (DecomposedFrom != nil && State == failed). Anything else is
//     ErrRedriveNotApplicable — re-driving a non-child or a
//     non-failed run has no defined meaning.
//  2. Locate its single failed implement stage and reset it
//     failed → pending via repo.RetryStage (the A/C retry path).
//  3. Reopen the run failed → running via repo.RetryRun.
//
// Un-terminal-ing the run (step 3) is mandatory: orchestrator.Advance
// no-ops on a terminal run, so resetting only the stage would leave the
// child stuck. The handler calls Orchestrator.Advance after this
// returns to walk pending → dispatched and fire workflow_dispatch.
//
// On success the parked parent reconciles on the child's next terminal
// transition through the unchanged parent-resolution logic.
func RedriveChild(ctx context.Context, repo Repository, childRunID uuid.UUID) (*RedriveDecision, error) {
	runRow, err := repo.GetRun(ctx, childRunID)
	if err != nil {
		return nil, fmt.Errorf("RedriveChild: get run: %w", err)
	}

	if runRow.DecomposedFrom == nil {
		return nil, fmt.Errorf("%w: run was not minted by a decomposition fanout (no parent), so there is no parked parent to reconcile",
			ErrRedriveNotApplicable)
	}
	if runRow.State != StateFailed {
		return nil, fmt.Errorf("%w: run is in state %q (only failed child runs can be re-driven)",
			ErrRedriveNotApplicable, runRow.State)
	}

	stages, err := repo.ListStagesForRun(ctx, childRunID)
	if err != nil {
		return nil, fmt.Errorf("RedriveChild: list stages: %w", err)
	}
	var implement *Stage
	for _, s := range stages {
		if s.Type == StageTypeImplement && s.State == StageStateFailed {
			implement = s
			break
		}
	}
	if implement == nil {
		return nil, fmt.Errorf("%w: child run has no failed implement stage to re-open",
			ErrRedriveNotApplicable)
	}
	if implement.FailureCategory == nil {
		return nil, fmt.Errorf("%w: failed implement stage has no FailureCategory recorded", ErrRedriveNotApplicable)
	}

	priorCat := *implement.FailureCategory
	priorReason := ""
	if implement.FailureReason != nil {
		priorReason = *implement.FailureReason
	}

	// Reset the implement stage failed → pending (the A/C dispatch
	// path) before reopening the run, then un-terminal the run so
	// Advance can re-dispatch. RetryStage clears the stage's failure
	// metadata; RetryRun reuses UpdateRunState (runs carry none).
	updatedStage, err := repo.RetryStage(ctx, implement.ID, StageStatePending)
	if err != nil {
		return nil, fmt.Errorf("RedriveChild: reset implement stage failed → pending: %w", err)
	}
	updatedRun, err := repo.RetryRun(ctx, childRunID, StateRunning)
	if err != nil {
		return nil, fmt.Errorf("RedriveChild: reopen run failed → running: %w", err)
	}

	return &RedriveDecision{
		PriorCategory: priorCat,
		PriorReason:   priorReason,
		Stage:         updatedStage,
		Run:           updatedRun,
	}, nil
}
