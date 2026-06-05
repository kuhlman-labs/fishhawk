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
	// PriorState is the implement stage state before the re-open:
	// awaiting_approval on the commit-yourself flow, or succeeded on the
	// push_and_open_pr flow (#780). Captured pre-transition so the audit
	// entry records what was re-opened.
	PriorState StageState

	// Stage is the post-re-open implement stage row, in pending.
	Stage *Stage

	// ReparkedReview is the run's review stage after it was re-parked
	// awaiting_approval → pending on the push_and_open_pr flow, so the
	// re-dispatched implement stage flows back into a fresh review.
	// Nil on the commit-yourself (awaiting_approval) flow, where the
	// implement stage is its own gate and there is no separate review
	// stage to re-park.
	ReparkedReview *Stage

	// RemainingBudget is MaxPasses minus the pass count *after* this
	// re-open (never negative). The handler surfaces it in the audit
	// receipt and the MCP tool reports it as the remaining budget.
	RemainingBudget int
}

// FixupStage re-opens an implement stage parked at (or held open by) the
// review gate so the agent can make a bounded, operator-gated fix-up pass
// against advisory implement-review concerns (E22.X / #762, #780).
//
// Preconditions, all refused with ErrFixupNotApplicable:
//
//   - the stage must be an implement stage; fix-up routes concerns back
//     to the implement agent, so plan/review stages are not eligible.
//   - the implement stage must be re-openable, in one of two shapes:
//   - awaiting_approval — the commit-yourself flow: the implement
//     stage is its own review gate. Re-opened awaiting_approval →
//     pending; no separate review stage to re-park.
//   - succeeded — the push_and_open_pr flow (#780): the implement
//     stage committed and opened the PR, and the human gate is a
//     SEPARATE review stage. Applicable ONLY while that review stage
//     is still parked at awaiting_approval (the PR is open, not
//     merged); refused once the review stage has merged/succeeded or
//     when the run has no review stage at all.
//
// When opts.PriorPassCount >= opts.MaxPasses the bound is already
// spent and FixupStage refuses with ErrFixupBudgetExhausted before
// touching any stage — fix-up is never an unbounded auto-loop.
//
// On success the implement stage moves to pending via the existing
// TransitionStage repo verb (the fix-up edge is admitted by
// ValidStageFixupTransition); on the push_and_open_pr flow the review
// stage is also re-parked awaiting_approval → pending so the
// re-dispatched implement stage flows back into a fresh review. The
// caller then hands off to the orchestrator, which walks pending →
// dispatched and fires workflow_dispatch — exactly the retry handoff.
// The orchestrator handoff lives in the handler, not here: run depends
// on nothing external, so inverting that would create a cycle.
//
// Partial-failure safety (#780): on the push_and_open_pr flow the review
// re-park is performed FIRST, so the implement succeeded → pending
// re-open is the LAST mutation. A re-park failure returns before the
// implement stage is touched, leaving it succeeded (never orphaned in
// pending without its review gate re-parked, which would let a later
// dispatch re-run implement without the fix-up concerns).
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

	// Decide applicability and, on the push_and_open_pr flow, locate the
	// review stage that must be re-parked alongside the implement re-open.
	var reviewToRepark *Stage
	switch stage.State {
	case StageStateAwaitingApproval:
		// Commit-yourself flow: the implement stage is its own gate.
	case StageStateSucceeded:
		// push_and_open_pr flow: applicable only while the run's review
		// stage is still open at its gate.
		review, err := findOpenReviewStage(ctx, repo, stage.RunID)
		if err != nil {
			return nil, err
		}
		reviewToRepark = review
	default:
		return nil, fmt.Errorf("%w: stage is in state %q (only an implement stage awaiting approval, or one that succeeded with its review gate still open, can be fixed up)",
			ErrFixupNotApplicable, stage.State)
	}

	if opts.PriorPassCount >= opts.MaxPasses {
		return nil, fmt.Errorf("%w: %d of %d fix-up passes already used",
			ErrFixupBudgetExhausted, opts.PriorPassCount, opts.MaxPasses)
	}

	priorState := stage.State

	// Re-park the review stage FIRST so the implement re-open is the last
	// mutation (partial-failure safety, #780). On the commit-yourself flow
	// reviewToRepark is nil and this is skipped.
	if reviewToRepark != nil {
		reparked, err := repo.TransitionStage(ctx, reviewToRepark.ID, StageStatePending, nil)
		if err != nil {
			return nil, fmt.Errorf("FixupStage: re-park review %s → pending: %w", reviewToRepark.State, err)
		}
		reviewToRepark = reparked
	}

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
		ReparkedReview:  reviewToRepark,
		RemainingBudget: remaining,
	}, nil
}

// findOpenReviewStage returns the run's review stage when it is still
// parked at awaiting_approval — the push_and_open_pr precondition for
// re-opening a succeeded implement stage (#780). It refuses with
// ErrFixupNotApplicable when the run has no review stage, or its review
// stage has already merged/succeeded (the gate is closed; a fix-up
// commit onto a merged PR is not meaningful). v0 workflows define a
// single review stage (MVP_SPEC §4.1); the first one awaiting approval
// is selected.
func findOpenReviewStage(ctx context.Context, repo Repository, runID uuid.UUID) (*Stage, error) {
	stages, err := repo.ListStagesForRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("FixupStage: list stages for run: %w", err)
	}
	for _, s := range stages {
		if s.Type == StageTypeReview && s.State == StageStateAwaitingApproval {
			return s, nil
		}
	}
	return nil, fmt.Errorf("%w: implement stage succeeded but the run has no review stage awaiting approval (the review gate is not open or already resolved)",
		ErrFixupNotApplicable)
}
