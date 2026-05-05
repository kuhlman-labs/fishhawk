package run

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// ErrRetryNotApplicable is returned by RetryStage when the stage's
// current state or failure category isn't a retriable one.
// Handlers map this to a 422 Unprocessable Entity with an
// explanation of *why* the retry was refused, so callers can
// distinguish a permanent "this isn't retriable" from a transient
// "try again later".
var ErrRetryNotApplicable = errors.New("retry not applicable")

// ErrRetryNotImplemented was returned for category-A and -C
// retries before E8.6 (#173) wired the orchestrator. Kept around
// for callers that switch on it (the handler still maps it to
// 501) but no path in this package returns it as of E8.6.
var ErrRetryNotImplemented = errors.New("retry not implemented")

// RetryDecision summarizes what RetryStage did, for the audit
// trail and the handler's response.
type RetryDecision struct {
	// PriorCategory is the failure category the stage carried
	// before the retry. Captured pre-transition so the audit entry
	// records "we retried a stage that failed-D" verbatim.
	PriorCategory FailureCategory

	// PriorReason is the failure_reason from before the retry,
	// for the same audit-trail reason.
	PriorReason string

	// Stage is the post-retry stage row. Its FailureCategory and
	// FailureReason are nil; State is the retry target.
	Stage *Stage
}

// RetryStage re-opens a failed stage when the current failure
// category supports it. Per MVP_SPEC §6:
//
//	A (agent failure)            → failed → pending. Caller hands
//	                               off to the orchestrator, which
//	                               walks pending → dispatched and
//	                               fires workflow_dispatch.
//	B (constraint/policy)        → not retriable; the workflow
//	                               or spec needs to change first.
//	                               Returns ErrRetryNotApplicable.
//	C (infrastructure)           → failed → pending. Same handoff
//	                               as A — fresh runner instance,
//	                               fresh signing key.
//	D, sla_timeout sub-reason    → failed → awaiting_approval.
//	                               updated_at restarts via the
//	                               trigger; SLA ticker measures
//	                               from the new value. No
//	                               orchestrator handoff needed.
//	D, gate-rejected sub-reason  → not retriable; the approver
//	                               said no, a fresh run is the
//	                               right next step.
//
// On success returns the new Stage (in pending for A/C, in
// awaiting_approval for D-timeout) and the prior failure detail
// for the caller to put in the audit entry. On a non-retriable
// case returns ErrRetryNotApplicable.
//
// The orchestrator handoff for A/C lives in the handler, not here:
// run depends on nothing external; orchestrator depends on run.
// Inverting that would create a cycle.
func RetryStage(ctx context.Context, repo Repository, stageID uuid.UUID) (*RetryDecision, error) {
	stage, err := repo.GetStage(ctx, stageID)
	if err != nil {
		return nil, fmt.Errorf("RetryStage: get stage: %w", err)
	}

	if stage.State != StageStateFailed {
		return nil, fmt.Errorf("%w: stage is in state %q (only failed stages can be retried)",
			ErrRetryNotApplicable, stage.State)
	}
	if stage.FailureCategory == nil {
		return nil, fmt.Errorf("%w: failed stage has no FailureCategory recorded", ErrRetryNotApplicable)
	}

	priorCat := *stage.FailureCategory
	priorReason := ""
	if stage.FailureReason != nil {
		priorReason = *stage.FailureReason
	}

	switch priorCat {
	case FailureA, FailureC:
		// Re-dispatch path: state-machine moves stage back to
		// pending, then the handler invokes the orchestrator,
		// which transitions pending → dispatched and fires
		// workflow_dispatch. Same flow whether the prior failure
		// was an agent crash (A) or an infra timeout (C); both
		// produce a fresh runner with a fresh signing key.
		updated, err := repo.RetryStage(ctx, stageID, StageStatePending)
		if err != nil {
			return nil, fmt.Errorf("RetryStage: failed → pending: %w", err)
		}
		return &RetryDecision{
			PriorCategory: priorCat,
			PriorReason:   priorReason,
			Stage:         updated,
		}, nil
	case FailureB:
		return nil, fmt.Errorf("%w: category B failures (constraint/policy) require a spec or workflow change, not a retry",
			ErrRetryNotApplicable)
	case FailureD:
		// Distinguish SLA timeout (retriable — re-open the gate)
		// from approver rejection (not retriable — they said no).
		// The reason prefix is set in two places:
		//   - sla.handleStage emits "sla_timeout: <elapsed> elapsed (deadline <d>)"
		//   - approvals.advanceStage emits "gate rejected by approver"
		// Match the timeout prefix; everything else under D is the
		// rejection path or future variants we haven't seen.
		if !strings.HasPrefix(priorReason, "sla_timeout") {
			return nil, fmt.Errorf("%w: category D failures other than SLA timeout require a fresh run",
				ErrRetryNotApplicable)
		}
		updated, err := repo.RetryStage(ctx, stageID, StageStateAwaitingApproval)
		if err != nil {
			return nil, fmt.Errorf("RetryStage: re-open gate: %w", err)
		}
		return &RetryDecision{
			PriorCategory: priorCat,
			PriorReason:   priorReason,
			Stage:         updated,
		}, nil
	}

	return nil, fmt.Errorf("%w: unknown failure category %q", ErrRetryNotApplicable, priorCat)
}
