package run

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/google/uuid"
)

// ErrReviveNotApplicable is returned by ReviveRun when the target run
// cannot be re-admitted by a batch re-park: it is not in the failed
// state (only failed → running is a valid run-level reopen), it has no
// failed stage to re-open, or at least one of its failed stages is in a
// non-retryable failure category (category-B, D-rejected, or a stage
// with no recorded category). Handlers map this to a 422 Unprocessable
// Entity whose message names the blocking stage.
var ErrReviveNotApplicable = errors.New("revive not applicable")

// ReviveStageRestore records what ReviveRun did to a single failed
// stage, for the audit trail and the handler's response. Captured so
// the run_revived audit entry lists each re-parked stage without a
// reader having to walk back to the prior stage-failed entries.
type ReviveStageRestore struct {
	// StageID is the re-parked stage.
	StageID uuid.UUID
	// StageType is the stage's kind (plan/implement/review/…), surfaced
	// so the audit payload and response read without a second lookup.
	StageType StageType
	// PriorCategory is the stage's failure category before the revive,
	// captured pre-transition.
	PriorCategory FailureCategory
	// PriorReason is the stage's failure_reason from before the revive.
	PriorReason string
	// RestoredState is the pre-dispatch state the stage was re-parked to
	// (pending for A/C, awaiting_approval for a D SLA-timeout gate,
	// awaiting_children for a decomposed-parent implement per #1891).
	RestoredState StageState
}

// ReviveDecision summarizes what ReviveRun did across every failed
// stage, for the audit trail and the handler's response.
type ReviveDecision struct {
	// Run is the post-revive run row (in running).
	Run *Run
	// Stages lists each re-parked stage's restoration, ordered by the
	// stage sequence.
	Stages []ReviveStageRestore
}

// ReviveRun re-admits a terminal-FAILED run for another operator turn by
// re-parking every failed stage in its correct gate-ordered pre-dispatch
// state and flipping the run failed → running — the single operator verb
// that replaces the retry-without-dispatch dance (#1915).
//
// The sequence is:
//
//  1. Require the run be in state failed. cancelled / succeeded / running
//     refuse with ErrReviveNotApplicable — runRetryTransitions (transition.go)
//     admits only failed → running, so a revive on any other state has no
//     defined meaning and must be refused before any mutation.
//  2. Collect every failed stage. Zero failed stages refuses (nothing to
//     re-park).
//  3. PRE-VALIDATE that EVERY failed stage is retryable via RetryableFailure
//     BEFORE any mutation. A category-B stage, a D-rejected stage, or a
//     failed stage with no recorded category refuses the WHOLE revive with
//     ErrReviveNotApplicable naming the blocking stage — no partial state.
//     This is the load-bearing no-partial-mutation guard: without it, a
//     batch that re-parked the first few stages and then hit a non-retryable
//     stage would leave the run half-re-parked.
//  4. Apply RetryStage per failed stage, reusing its existing per-category
//     targets (A/C → pending, D SLA-timeout → awaiting_approval,
//     decomposed-parent implement → awaiting_children per #1891). Each
//     RetryStage bumps the stage's SelfRetryCount, so revive consumes
//     per-stage retry budget exactly like fishhawk_retry_stage — it is a
//     batch retry-shaped re-open, not a budget bypass.
//  5. Reopen the run failed → running via RetryRun.
//
// CRUCIALLY ReviveRun performs NO orchestrator handoff and never
// dispatches — it re-parks only. Dispatch happens later at each stage's
// proper gate turn via the existing verbs. Because no Advance fires
// mid-revive, the #1700 wrong-order re-dispatch corruption is structurally
// impossible: a re-parked stage simply sits in its pre-dispatch state until
// the operator acts.
func ReviveRun(ctx context.Context, repo Repository, runID uuid.UUID) (*ReviveDecision, error) {
	runRow, err := repo.GetRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("ReviveRun: get run: %w", err)
	}

	if runRow.State != StateFailed {
		return nil, fmt.Errorf("%w: run is in state %q (only failed runs can be revived)",
			ErrReviveNotApplicable, runRow.State)
	}

	stages, err := repo.ListStagesForRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("ReviveRun: list stages: %w", err)
	}

	// Collect failed stages in a deterministic order (by sequence) so the
	// audit payload and response are stable across calls.
	failed := make([]*Stage, 0, len(stages))
	for _, s := range stages {
		if s.State == StageStateFailed {
			failed = append(failed, s)
		}
	}
	if len(failed) == 0 {
		return nil, fmt.Errorf("%w: run has no failed stages to re-park", ErrReviveNotApplicable)
	}
	sort.Slice(failed, func(i, j int) bool { return failed[i].Sequence < failed[j].Sequence })

	// PRE-VALIDATE before any mutation: every failed stage must be
	// retryable. A single non-retryable stage refuses the whole revive so
	// the run is never left half-re-parked (no-partial-mutation).
	for _, s := range failed {
		if s.FailureCategory == nil {
			return nil, fmt.Errorf("%w: failed %s stage %s has no FailureCategory recorded, so its retryability cannot be confirmed; a fresh run is the right next step",
				ErrReviveNotApplicable, s.Type, s.ID)
		}
		reason := ""
		if s.FailureReason != nil {
			reason = *s.FailureReason
		}
		if !RetryableFailure(*s.FailureCategory, reason) {
			return nil, fmt.Errorf("%w: failed %s stage %s is category %s (%s), which is not retryable; revive re-parks retryable failures only, so a spec/workflow change or a fresh run is required",
				ErrReviveNotApplicable, s.Type, s.ID, *s.FailureCategory, s.FailureCategory.Description())
		}
	}

	// Every failed stage is retryable. Re-park each via the existing
	// RetryStage per-category targets (reusing its decomposed-parent
	// awaiting_children restore, #1891). No orchestrator handoff — revive
	// re-parks only; dispatch happens later at the stage's proper turn.
	restores := make([]ReviveStageRestore, 0, len(failed))
	for _, s := range failed {
		dec, err := RetryStage(ctx, repo, s.ID, RetryOptions{})
		if err != nil {
			return nil, fmt.Errorf("ReviveRun: re-park %s stage %s: %w", s.Type, s.ID, err)
		}
		restores = append(restores, ReviveStageRestore{
			StageID:       dec.Stage.ID,
			StageType:     dec.Stage.Type,
			PriorCategory: dec.PriorCategory,
			PriorReason:   dec.PriorReason,
			RestoredState: dec.Stage.State,
		})
	}

	updatedRun, err := repo.RetryRun(ctx, runID, StateRunning)
	if err != nil {
		return nil, fmt.Errorf("ReviveRun: reopen run failed → running: %w", err)
	}

	return &ReviveDecision{
		Run:    updatedRun,
		Stages: restores,
	}, nil
}
