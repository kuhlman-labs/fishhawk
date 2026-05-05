package run

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// FailStage transitions a stage to the Failed terminal state with
// the supplied category + reason. It walks the canonical state
// path from whichever non-terminal state the stage is currently
// in, so call sites never need to think about whether they're
// failing from running, awaiting_approval, dispatched, or pending:
//
//	pending           → failed
//	dispatched        → running → failed   (e.g. agent never reported)
//	running           → failed             (e.g. policy violation post-trace)
//	awaiting_approval → failed             (e.g. SLA elapsed, gate rejected)
//
// FailStage does NOT append an audit entry. Cause-specific entries
// (policy_evaluated, approval_sla_elapsed, approval_submitted) live
// at the call site and carry the structured payload that explains
// *why* the failure happened. Keeping the audit emission with the
// caller means the per-run hash chain stays in the caller's
// transaction-shaped control flow.
//
// Returns an error if cat isn't one of the four canonical
// categories — a typo here would silently corrupt the stage row,
// so reject early.
func FailStage(
	ctx context.Context,
	repo Repository,
	stageID uuid.UUID,
	cat FailureCategory,
	reason string,
) (*Stage, error) {
	if !cat.Valid() {
		return nil, fmt.Errorf("FailStage: invalid category %q", cat)
	}

	stage, err := repo.GetStage(ctx, stageID)
	if err != nil {
		return nil, fmt.Errorf("FailStage: get stage: %w", err)
	}

	// Dispatched stages must walk through Running first; the state
	// machine forbids dispatched → failed without it (see
	// transition.go). Single-step otherwise.
	if stage.State == StageStateDispatched {
		if _, err := repo.TransitionStage(ctx, stageID, StageStateRunning, nil); err != nil {
			return nil, fmt.Errorf("FailStage: dispatched → running: %w", err)
		}
	}

	cat2 := cat
	reason2 := reason
	out, err := repo.TransitionStage(ctx, stageID, StageStateFailed, &StageCompletion{
		FailureCategory: &cat2,
		FailureReason:   &reason2,
	})
	if err != nil {
		return nil, fmt.Errorf("FailStage: %s → failed: %w", stage.State, err)
	}
	return out, nil
}
