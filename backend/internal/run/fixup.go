package run

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// ErrFixupNotApplicable is returned by FixupStage when the stage's
// type or current state does not support a fix-up re-open. Handlers
// map this to a 422 Unprocessable Entity so the caller learns *why*
// the fix-up was refused (wrong stage type, or the stage is not parked
// at the review gate) rather than a generic failure.
var ErrFixupNotApplicable = errors.New("fixup not applicable")

// ErrFixupBudgetExhausted is returned by FixupStage when the stage has
// already consumed its bounded fix-up passes. The bound is operator-
// configured (default 1) and never an unbounded auto-loop; once it is
// hit, a fresh run (or an operator hand-edit) is the next step.
// Handlers map this to a 422 Unprocessable Entity.
var ErrFixupBudgetExhausted = errors.New("fixup budget exhausted")

// FixupOptions modulates FixupStage's bounded-pass decision. The
// caller supplies the prior-pass count and the configured maximum;
// FixupStage owns the comparison so the refuse-at-bound rule has a
// single home.
type FixupOptions struct {
	// PriorPassCount is how many fix-up passes the stage has already
	// consumed. The handler derives this by counting the stage's prior
	// stage_fixup_triggered audit entries (the durable record — there
	// is no dedicated column), so the bound holds across restarts.
	PriorPassCount int

	// MaxPasses is the configured upper bound on fix-up passes for the
	// stage (default 1). FixupStage refuses with ErrFixupBudgetExhausted
	// when PriorPassCount >= MaxPasses.
	MaxPasses int
}

// FixupDecision summarizes what FixupStage did, for the audit trail
// and the handler's response.
type FixupDecision struct {
	// PriorState is the stage state before the re-open (always
	// awaiting_approval on the success path). Captured pre-transition so
	// the audit entry records what was re-opened.
	PriorState StageState

	// Stage is the post-re-open stage row, in pending.
	Stage *Stage

	// RemainingBudget is MaxPasses minus the pass count *after* this
	// re-open (never negative). The handler surfaces it in the audit
	// receipt and the MCP tool reports it as the remaining budget.
	RemainingBudget int
}

// FixupStage re-opens an implement stage parked at the review gate so
// the agent can make a bounded, operator-gated fix-up pass against
// advisory implement-review concerns (E22.X / #762).
//
// Preconditions, all refused with ErrFixupNotApplicable:
//
//   - the stage must be an implement stage; fix-up routes concerns back
//     to the implement agent, so plan/review stages are not eligible.
//   - the stage must be parked at awaiting_approval (the review gate);
//     ValidStageFixupTransition admits only awaiting_approval → pending.
//
// When opts.PriorPassCount >= opts.MaxPasses the bound is already
// spent and FixupStage refuses with ErrFixupBudgetExhausted before
// touching the stage — fix-up is never an unbounded auto-loop.
//
// On success the stage moves awaiting_approval → pending via the
// existing TransitionStage repo verb (the fix-up edge is admitted by
// ValidStageFixupTransition); the caller then hands off to the
// orchestrator, which walks pending → dispatched and fires
// workflow_dispatch — exactly the retry handoff. The orchestrator
// handoff lives in the handler, not here: run depends on nothing
// external, so inverting that would create a cycle.
//
// The recorded approve_with_concerns verdict and the concern-index
// validation are the handler's responsibility (they require the audit
// log, which run does not depend on); FixupStage owns only the
// state-machine and bound decisions.
func FixupStage(ctx context.Context, repo Repository, stageID uuid.UUID, opts FixupOptions) (*FixupDecision, error) {
	stage, err := repo.GetStage(ctx, stageID)
	if err != nil {
		return nil, fmt.Errorf("FixupStage: get stage: %w", err)
	}

	if stage.Type != StageTypeImplement {
		return nil, fmt.Errorf("%w: stage is type %q (only implement stages can be fixed up)",
			ErrFixupNotApplicable, stage.Type)
	}
	if !ValidStageFixupTransition(stage.State, StageStatePending) {
		return nil, fmt.Errorf("%w: stage is in state %q (only an implement stage awaiting approval can be fixed up)",
			ErrFixupNotApplicable, stage.State)
	}

	if opts.PriorPassCount >= opts.MaxPasses {
		return nil, fmt.Errorf("%w: %d of %d fix-up passes already used",
			ErrFixupBudgetExhausted, opts.PriorPassCount, opts.MaxPasses)
	}

	priorState := stage.State
	updated, err := repo.TransitionStage(ctx, stageID, StageStatePending, nil)
	if err != nil {
		return nil, fmt.Errorf("FixupStage: %s → pending: %w", priorState, err)
	}

	remaining := opts.MaxPasses - (opts.PriorPassCount + 1)
	if remaining < 0 {
		remaining = 0
	}

	return &FixupDecision{
		PriorState:      priorState,
		Stage:           updated,
		RemainingBudget: remaining,
	}, nil
}
