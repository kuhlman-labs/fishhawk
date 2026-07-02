package run

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// ErrAcceptanceReopenNotApplicable is returned by ReopenAcceptanceStage when
// the stage's type or current state does not support a class-2 re-open (E31.8
// / #1536): a non-acceptance stage, an acceptance stage that is not settled
// `succeeded`, or an acceptance stage inside a terminal run. The caller (the
// acceptance triage) treats it as a routing refusal and degrades to a paged
// disposition rather than acting, so a mis-classification never corrupts a
// transition — it lands on the human.
var ErrAcceptanceReopenNotApplicable = errors.New("acceptance reopen not applicable")

// AcceptanceReopenDecision summarizes what ReopenAcceptanceStage did, for the
// caller's audit trail.
type AcceptanceReopenDecision struct {
	// Stage is the post-re-open acceptance stage row, in pending.
	Stage *Stage
	// PriorState is the acceptance stage state before the re-open (always
	// succeeded on the admitted path), captured for the audit payload.
	PriorState StageState
}

// ReopenAcceptanceStage is the class-2 verb (E31.8 / #1536): it re-opens a
// SETTLED acceptance stage (succeeded → pending) so the orchestrator rebuilds
// a fresh preview and re-runs validation, the honest handling of a flake /
// environment-blocked verdict.
//
// It deliberately does NOT reuse run.RetryStage/handleRetryStage: the retry
// decision tree operates on FAILED stages, but a valid failed acceptance
// VERDICT leaves the acceptance STAGE `succeeded` (E31.7: a failed verdict is
// not a runner failure). A stage-level failure keeps riding the existing
// retry path untouched — that is why run.RetryStage is not widened here.
//
// Refuses with ErrAcceptanceReopenNotApplicable when the stage is not an
// acceptance stage, not in `succeeded` (a stage-level failure rides the retry
// path; anything else is not the settled gate a re-run must re-open), or the
// run is terminal (no live gate to flow the re-open back into). The
// succeeded → pending transition is admitted at the repo layer by
// stageFixupTransitions (keyed by state, not stage type).
func ReopenAcceptanceStage(ctx context.Context, repo Repository, stageID uuid.UUID) (*AcceptanceReopenDecision, error) {
	stage, err := repo.GetStage(ctx, stageID)
	if err != nil {
		return nil, fmt.Errorf("ReopenAcceptanceStage: get stage: %w", err)
	}
	if stage.Type != StageTypeAcceptance {
		return nil, fmt.Errorf("%w: stage is type %q (only an acceptance stage can be re-opened)",
			ErrAcceptanceReopenNotApplicable, stage.Type)
	}
	if stage.State != StageStateSucceeded {
		return nil, fmt.Errorf("%w: stage is in state %q (only a succeeded acceptance stage can be re-opened; a failed stage rides the retry path)",
			ErrAcceptanceReopenNotApplicable, stage.State)
	}

	r, err := repo.GetRun(ctx, stage.RunID)
	if err != nil {
		return nil, fmt.Errorf("ReopenAcceptanceStage: get run: %w", err)
	}
	if r.State.IsTerminal() {
		return nil, fmt.Errorf("%w: run %s is already terminal (%s); a completed run's acceptance stage cannot be re-opened",
			ErrAcceptanceReopenNotApplicable, r.ID, r.State)
	}

	prior := stage.State
	updated, err := repo.TransitionStage(ctx, stageID, StageStatePending, nil)
	if err != nil {
		return nil, fmt.Errorf("ReopenAcceptanceStage: %s → pending: %w", prior, err)
	}
	return &AcceptanceReopenDecision{Stage: updated, PriorState: prior}, nil
}
