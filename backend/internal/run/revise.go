package run

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// ErrReviseNotApplicable is returned by RevisePlanStage when the stage's
// type or current state does not support a plan-revise re-open. A revise
// re-plans a PLAN stage parked at its approval gate, so any non-plan
// stage, or a plan stage not in awaiting_approval, is refused. Handlers
// map this to a 409 Conflict (revise_not_applicable) so the caller learns
// *why* the revise was refused (wrong stage type, or the stage is not
// parked at the approval gate) rather than a generic failure.
var ErrReviseNotApplicable = errors.New("revise not applicable")

// ErrReviseBudgetExhausted is returned by RevisePlanStage when the plan
// stage has already consumed its bounded revise passes. The bound is
// operator-configured (default 1) and never an unbounded auto-loop; once
// it is hit, the operator may grant ONE bounded override pass
// (ReviseOptions.ForceAdditionalPass) up to the hard ceiling, or a fresh
// run (a reject → new-run replan) is the next step. Handlers map this to
// a 409 Conflict.
var ErrReviseBudgetExhausted = errors.New("revise budget exhausted")

// ErrReviseCeilingReached is returned by RevisePlanStage when the plan
// stage has consumed the absolute hard ceiling of revise passes
// (ReviseOptions.HardCeiling). It is the hard stop the operator override
// (ForceAdditionalPass) can never push past — distinct from
// ErrReviseBudgetExhausted, which only signals the NORMAL budget is spent
// (one more pass is still available via the override). Once the ceiling
// is reached the only path forward is a reject → fresh-run replan.
// Handlers map this to a 409 Conflict, placed BEFORE the budget-exhausted
// arm so the distinct error is not masked.
var ErrReviseCeilingReached = errors.New("revise ceiling reached")

// ReviseOptions modulates RevisePlanStage's bounded-pass decision. The
// caller supplies the prior-pass count and the configured maximum;
// RevisePlanStage owns the comparison so the refuse-at-bound rule has a
// single home. Mirrors FixupOptions.
type ReviseOptions struct {
	// PriorPassCount is how many revise passes the plan stage has already
	// consumed. The handler derives this by counting the stage's prior
	// plan_revised audit entries (the durable record — there is no
	// dedicated column), so the bound holds across restarts.
	PriorPassCount int

	// MaxPasses is the configured NORMAL upper bound on revise passes for
	// the stage (default 1). RevisePlanStage refuses with
	// ErrReviseBudgetExhausted when PriorPassCount >= MaxPasses and the
	// operator has not set ForceAdditionalPass.
	MaxPasses int

	// ForceAdditionalPass is the bounded operator override: when true,
	// RevisePlanStage admits ONE pass beyond MaxPasses, still capped by
	// HardCeiling. The admitted pass is marked ReviseDecision.Forced so the
	// handler can audit it.
	ForceAdditionalPass bool

	// HardCeiling is the absolute cap on total revise passes per stage,
	// supplied by the caller (mirroring how MaxPasses is injected). It
	// ALWAYS wins — even with ForceAdditionalPass set: when
	// PriorPassCount >= HardCeiling RevisePlanStage refuses with
	// ErrReviseCeilingReached before touching any stage. A zero/unset
	// HardCeiling means no override headroom, so callers that want the
	// override MUST set it.
	HardCeiling int
}

// ReviseDecision summarizes what RevisePlanStage did, for the audit
// trail and the handler's response.
type ReviseDecision struct {
	// PriorState is the plan stage state before the re-open
	// (awaiting_approval). Captured pre-transition so the audit entry
	// records what was re-opened.
	PriorState StageState

	// Stage is the post-re-open plan stage row, in pending.
	Stage *Stage

	// RemainingBudget is MaxPasses minus the pass count *after* this
	// re-open (never negative). The handler surfaces it in the audit
	// receipt and the MCP tool reports it as the remaining budget. It
	// reflects the NORMAL budget only, so a forced override pass (which
	// runs past MaxPasses) reports 0 here.
	RemainingBudget int

	// Forced is true when this pass was admitted ONLY because the operator
	// set ForceAdditionalPass — i.e. the normal budget was already spent
	// (PriorPassCount >= MaxPasses) but the hard ceiling had headroom. The
	// handler records it on the audit entry so the forced override is
	// durably attributable.
	Forced bool
}

// RevisePlanStage re-opens a plan stage parked at its approval gate so
// the planner can re-plan IN PLACE against a binding operator design
// constraint (E22.X / #1099). It is the plan-stage analogue of
// FixupStage: a bounded, operator-gated re-open of a HEALTHY gate (the
// plan is sound but needs a design tweak the operator wants the agent to
// apply, rather than a hand-edit or a wholesale reject → fresh-run
// replan).
//
// Preconditions, both refused with ErrReviseNotApplicable:
//
//   - the stage must be a plan stage; revise re-plans, so implement/review
//     stages are not eligible.
//   - the plan stage must be parked at awaiting_approval (its approval
//     gate). A stage in any other state has no parked gate to re-open.
//
// The bound decision (checked before any stage is touched — revise is
// never an unbounded auto-loop) is, in order:
//
//   - PriorPassCount >= HardCeiling -> ErrReviseCeilingReached. The hard
//     stop ALWAYS wins, even when ForceAdditionalPass is set.
//   - else PriorPassCount >= MaxPasses && !ForceAdditionalPass ->
//     ErrReviseBudgetExhausted. The normal budget is spent and no override
//     was requested.
//   - otherwise admit, marking the decision Forced when the override
//     carried it past the normal budget (PriorPassCount >= MaxPasses).
//
// On success the plan stage moves to pending via the existing
// TransitionStage repo verb (the revise edge is admitted by
// ValidStageReviseTransition). The caller then hands off to the
// orchestrator, which walks pending → dispatched and fires
// workflow_dispatch — exactly the retry/fix-up handoff. The orchestrator
// handoff lives in the handler, not here: run depends on nothing
// external, so inverting that would create a cycle.
//
// The rendered operator constraint and the prior-plan base are the
// handler's responsibility (they require the audit log + artifact store,
// which run does not depend on); RevisePlanStage owns only the
// state-machine and bound decisions.
func RevisePlanStage(ctx context.Context, repo Repository, stageID uuid.UUID, opts ReviseOptions) (*ReviseDecision, error) {
	stage, err := repo.GetStage(ctx, stageID)
	if err != nil {
		return nil, fmt.Errorf("RevisePlanStage: get stage: %w", err)
	}

	if stage.Type != StageTypePlan {
		return nil, fmt.Errorf("%w: stage is type %q (only a plan stage can be revised)",
			ErrReviseNotApplicable, stage.Type)
	}
	if stage.State != StageStateAwaitingApproval {
		return nil, fmt.Errorf("%w: stage is in state %q (only a plan stage parked at its approval gate can be revised)",
			ErrReviseNotApplicable, stage.State)
	}

	// The hard ceiling ALWAYS wins, even when the operator forced an
	// additional pass — it is the absolute stop the override cannot push
	// past.
	if opts.PriorPassCount >= opts.HardCeiling {
		return nil, fmt.Errorf("%w: %d of %d revise passes already used (hard ceiling)",
			ErrReviseCeilingReached, opts.PriorPassCount, opts.HardCeiling)
	}
	// The normal budget is spent and no override was requested.
	if opts.PriorPassCount >= opts.MaxPasses && !opts.ForceAdditionalPass {
		return nil, fmt.Errorf("%w: %d of %d revise passes already used",
			ErrReviseBudgetExhausted, opts.PriorPassCount, opts.MaxPasses)
	}
	// Admitted. The pass is forced only when the override carried it past
	// the normal budget (the ceiling check above already guaranteed
	// headroom).
	forced := opts.PriorPassCount >= opts.MaxPasses

	priorState := stage.State

	updated, err := repo.TransitionStage(ctx, stageID, StageStatePending, nil)
	if err != nil {
		return nil, fmt.Errorf("RevisePlanStage: %s → pending: %w", priorState, err)
	}

	remaining := opts.MaxPasses - (opts.PriorPassCount + 1)
	if remaining < 0 {
		remaining = 0
	}

	return &ReviseDecision{
		PriorState:      priorState,
		Stage:           updated,
		RemainingBudget: remaining,
		Forced:          forced,
	}, nil
}
