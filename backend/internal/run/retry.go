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

// RetryOptions modulates RetryStage's per-category decision. The
// zero value preserves the default semantics documented on
// RetryStage.
type RetryOptions struct {
	// OverrideB, when true, re-opens a genuine category-B
	// (constraint/policy) failed stage via the same failed → pending
	// path as A/C, instead of refusing it with ErrRetryNotApplicable.
	//
	// This is the audited operator escape hatch for #698. It does NOT
	// accept the failing diff or bypass the policy gate: re-opening to
	// pending re-runs the stage and the policy gate re-evaluates the
	// new diff exactly as it would for an A/C retry. The override only
	// changes whether the stage is allowed to re-run at all — not what
	// the gate decides about the re-run. The handler requires a
	// recorded reason and writes a distinct stage_override_retried
	// audit entry; the default (false) leaves B non-retryable.
	OverrideB bool
}

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

	// Overridden reports that the retry was admitted only because the
	// caller passed RetryOptions.OverrideB on a category-B failure.
	// The handler keys the distinct stage_override_retried audit entry
	// off this so the explicit operator override stays separable from
	// an ordinary retry in any later audit analysis.
	Overridden bool
}

// RetryableFailure reports whether a stage failure in the given
// category (with the given failure_reason) can be re-opened by a
// straightforward retry, as opposed to needing a fresh run or a
// spec/workflow change. It is the single source of truth shared by
// RetryStage and by the decomposition parent-resolution paths
// (childcompletion.resolveParent, orchestrator.maybeAdvanceDecomposedParent),
// which use it to decide whether to park a parent awaiting re-drive
// rather than resolve it to failed. Per MVP_SPEC §6:
//
//	A (agent failure)            → retryable.
//	B (constraint/policy)        → NOT retryable (spec/workflow change).
//	C (infrastructure)           → retryable.
//	D, sla_timeout sub-reason    → retryable (re-open the gate).
//	D, gate-rejected sub-reason  → NOT retryable (approver said no).
//
// The reason argument only matters for category D, where it
// distinguishes the SLA-timeout sub-reason (retryable) from
// approver rejection and other D variants (not retryable). The
// "sla_timeout" prefix is emitted by sla.handleStage; everything
// else under D is the rejection path or future variants.
func RetryableFailure(cat FailureCategory, reason string) bool {
	switch cat {
	case FailureA, FailureC:
		return true
	case FailureB:
		return false
	case FailureD:
		return strings.HasPrefix(reason, "sla_timeout")
	default:
		return false
	}
}

// ImplementFailureRetryable reports whether the implement stage among
// the given run stages failed in a retryable category. The
// decomposition parent-resolution paths
// (childcompletion.resolveParent, orchestrator.maybeAdvanceDecomposedParent)
// call this per failed child to decide whether to park the parent
// awaiting re-drive (every failed child retryable) or resolve it to
// failed (at least one non-retryable). Returns false when there is no
// failed implement stage or it carries no failure category — parking
// is only safe when every failed child's failure is positively
// confirmed recoverable, so an unclassifiable failure resolves the
// parent rather than parking it indefinitely.
func ImplementFailureRetryable(stages []*Stage) bool {
	return implementFailureMatches(stages, RetryableFailure)
}

// RecoverableInDecomposition reports whether a decomposition child's
// stage failure can be recovered IN PLACE by an operator (via
// fishhawk_resume_run's in-place re-drive), as opposed to forcing the
// parent fan-out to resolve to failed. It is strictly broader than
// RetryableFailure: every retryable failure is recoverable, AND a
// genuine category-B (constraint/policy) failure is recoverable too,
// because the recover path folds operator-supplied scope amendments and
// re-runs the child rather than re-running the identical stage.
//
// This predicate is exclusive to the decomposition recover path. The
// auto-retry / retry_stage path keeps using RetryableFailure, so B stays
// non-retryable there — only the operator-gated recover broadens to B.
// D-rejection (approver said no) and unclassifiable failures remain
// non-recoverable, matching RetryableFailure.
func RecoverableInDecomposition(cat FailureCategory, reason string) bool {
	return RetryableFailure(cat, reason) || cat == FailureB
}

// ImplementFailureRecoverable reports whether the implement stage among
// the given run stages failed in a category that the decomposition
// recover path can re-drive in place (RecoverableInDecomposition). The
// parent-resolution paths gate the awaiting_children park on this rather
// than ImplementFailureRetryable, so a parent whose only failed child is
// category B parks awaiting re-drive instead of resolving to failed-C.
// Returns false when there is no failed implement stage or it carries no
// failure category — parking is only safe when every failed child's
// failure is positively confirmed recoverable, so an unclassifiable
// failure resolves the parent rather than parking it indefinitely.
func ImplementFailureRecoverable(stages []*Stage) bool {
	return implementFailureMatches(stages, RecoverableInDecomposition)
}

// implementFailureMatches finds the failed implement stage among stages
// and reports pred(category, reason) for it. Returns false when there is
// no failed implement stage or it carries no failure category — the
// shared core of ImplementFailureRetryable and ImplementFailureRecoverable.
func implementFailureMatches(stages []*Stage, pred func(FailureCategory, string) bool) bool {
	for _, s := range stages {
		if s.Type == StageTypeImplement && s.State == StageStateFailed {
			if s.FailureCategory == nil {
				return false
			}
			reason := ""
			if s.FailureReason != nil {
				reason = *s.FailureReason
			}
			return pred(*s.FailureCategory, reason)
		}
	}
	return false
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
//
// opts.OverrideB is the audited operator escape hatch (#698): set it
// to admit a genuine category-B failure onto the A/C failed → pending
// path, which re-runs the stage so the policy gate re-evaluates the
// new diff. It never bypasses the gate or accepts the B-violating
// diff. The default (zero value) leaves B non-retryable.
func RetryStage(ctx context.Context, repo Repository, stageID uuid.UUID, opts RetryOptions) (*RetryDecision, error) {
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

	retryable := RetryableFailure(priorCat, priorReason)
	overridden := false
	if !retryable && priorCat == FailureB && opts.OverrideB {
		// Audited operator override (#698): admit a genuine category-B
		// failure onto the A/C failed → pending path. This does NOT
		// accept the B-violating diff or skip the gate — re-opening to
		// pending re-runs the stage and the policy gate re-evaluates
		// the fresh diff. The handler enforces a recorded reason and
		// writes the distinct stage_override_retried audit entry.
		retryable = true
		overridden = true
	}

	if !retryable {
		// Not retryable: return a category-specific explanation so
		// the handler's 422 tells the caller *why* a fresh run (or a
		// spec change) is the right next step instead of a retry.
		switch priorCat {
		case FailureB:
			return nil, fmt.Errorf("%w: category B failures (constraint/policy) require a spec or workflow change, not a retry",
				ErrRetryNotApplicable)
		case FailureD:
			// The reason prefix is set in two places:
			//   - sla.handleStage emits "sla_timeout: <elapsed> elapsed (deadline <d>)"
			//   - approvals.advanceStage emits "gate rejected by approver"
			// RetryableFailure matched neither the timeout prefix, so
			// this is the rejection path or a future D variant.
			return nil, fmt.Errorf("%w: category D failures other than SLA timeout require a fresh run",
				ErrRetryNotApplicable)
		default:
			return nil, fmt.Errorf("%w: unknown failure category %q", ErrRetryNotApplicable, priorCat)
		}
	}

	// Retryable. Category D (SLA timeout) re-opens the gate
	// directly into awaiting_approval — updated_at restarts via the
	// trigger and the SLA ticker measures from the new value, no
	// orchestrator handoff. A/C re-dispatch through pending: the
	// handler invokes the orchestrator, which walks pending →
	// dispatched and fires workflow_dispatch with a fresh runner and
	// signing key (same flow whether the prior failure was an agent
	// crash (A) or an infra timeout (C)).
	target := StageStatePending
	if priorCat == FailureD {
		target = StageStateAwaitingApproval
	}
	updated, err := repo.RetryStage(ctx, stageID, target)
	if err != nil {
		return nil, fmt.Errorf("RetryStage: failed → %s: %w", target, err)
	}
	return &RetryDecision{
		PriorCategory: priorCat,
		PriorReason:   priorReason,
		Stage:         updated,
		Overridden:    overridden,
	}, nil
}
