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

// ErrRetryNotImplemented is returned for failure categories whose
// retry path needs work that hasn't shipped yet (today: A and C,
// which require orchestrator-driven re-dispatch — tracked under
// follow-up issues). Handlers map to 501 so a caller can tell
// "we'll get to it" from "this can never be retried."
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
//	A (agent failure)            → re-dispatch the runner. NOT YET
//	                               IMPLEMENTED — needs orchestrator
//	                               work; returns ErrRetryNotImplemented.
//	B (constraint/policy)        → not retriable; the workflow
//	                               or spec needs to change first.
//	                               Returns ErrRetryNotApplicable.
//	C (infrastructure)           → re-dispatch. NOT YET IMPLEMENTED;
//	                               same orchestrator dependency as A.
//	D, sla_timeout sub-reason    → re-open the gate (failed →
//	                               awaiting_approval). updated_at
//	                               restarts implicitly via the
//	                               trigger; SLA ticker measures
//	                               from the new value.
//	D, gate-rejected sub-reason  → not retriable; the approver
//	                               said no, a fresh run is the
//	                               right next step.
//
// On success returns the new Stage and the prior failure detail
// for the caller to put in the audit entry. On a non-retriable
// case returns ErrRetryNotApplicable / ErrRetryNotImplemented.
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
		return nil, fmt.Errorf("%w: category %s retry needs orchestrator re-dispatch (E8.3 follow-up)",
			ErrRetryNotImplemented, priorCat)
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
