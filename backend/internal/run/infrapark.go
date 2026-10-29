package run

import (
	"encoding/json"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
)

// InfraFailureParkRetryBudget is the number of category-C failures of ONE
// acceptance or implement stage the run absorbs by parking (#4356, #4426).
// Each park writes one stage-scoped CategoryStageInfraParked audit row; once
// the stage carries this many park rows for EARLIER failure episodes, its
// next category-C failure — the 4th one — fails the run and writes a
// CategoryStageInfraParkBudgetExhausted row naming the exhausted budget.
//
// The budget counts ONLY those park rows, never Stage.SelfRetryCount, which
// also counts category-A retries and operator category-B override retries
// (#4356 approval condition C1). For an acceptance stage the count is
// "consecutive" by construction: a category-A/B acceptance failure is not
// parkable and fails the run, and a recorded acceptance outcome for the stage
// (it executed and shipped a verdict) resets the count. An implement stage
// has no such reset — an acceptance_outcome_recorded row is never scoped to
// it — so its budget counts every park row over the stage's LIFETIME, which
// is stricter and fails toward today's fail-the-run path (#4426).
const InfraFailureParkRetryBudget = 3

// Audit categories the #4356 / #4426 infrastructure park writes. Both are
// stage-scoped (StageID = the failed acceptance or implement stage) and
// internal (no issue-comment surface).
const (
	// CategoryStageInfraParked is written once per parked failure episode.
	// Its payload's failed_at + retry_ordinal identify the episode, so an
	// Advance re-entry on the same parked failure neither writes a second
	// row nor counts its own row against the budget.
	CategoryStageInfraParked = "stage_infra_parked"
	// CategoryStageInfraParkBudgetExhausted is written when a category-C
	// failure past the budget fails the run.
	CategoryStageInfraParkBudgetExhausted = "stage_infra_park_budget_exhausted"
)

// auditCategoryPRMerged / auditCategoryAcceptanceOutcomeRecorded mirror the
// server's pr_merged and acceptance_outcome_recorded categories, and
// auditCategoryPlanDecomposed mirrors the plan_decomposed row
// orchestrator.emitPlanDecomposed writes on a decomposed parent; this package
// imports neither.
const (
	auditCategoryPRMerged                  = "pr_merged"
	auditCategoryAcceptanceOutcomeRecorded = "acceptance_outcome_recorded"
	auditCategoryPlanDecomposed            = "plan_decomposed"
)

// infraParkReopenedPassCategories are the server's pass-trigger categories
// (#4426 R4): a fix-up, conflict-resolution or merge-candidate-verify pass
// re-opens a finished implement stage and writes one of these rows scoped to
// it. This package cannot import the server, so the list is mirrored and
// bound to the server constants by InfraParkReopenedPassCategories' mirror
// test in backend/internal/server.
var infraParkReopenedPassCategories = []string{
	"stage_fixup_triggered",
	"stage_conflict_resolution_triggered",
	"stage_merge_candidate_verify_triggered",
}

// infraParkStagePushCategories are the server's head-producing categories
// (#4426 approval condition C1): an implement PR open, a fix-up push and a
// decomposition-child push. A row of one of them scoped to the failed
// implement stage proves the stage already produced a head, so its failure is
// a re-opened pass even when the pass's trigger row was never persisted (a
// failed stage_fixup_triggered append is only logged after the stage was
// re-opened, #4815).
var infraParkStagePushCategories = []string{
	"pull_request_opened",
	"fixup_pushed",
	"child_pushed",
}

// InfraParkReopenedPassCategories returns a copy of the pass-trigger
// categories ReadInfraParkHistory reads as a re-opened implement pass.
func InfraParkReopenedPassCategories() []string {
	return append([]string(nil), infraParkReopenedPassCategories...)
}

func isInfraParkReopenedPassCategory(category string) bool {
	for _, c := range infraParkReopenedPassCategories {
		if c == category {
			return true
		}
	}
	return false
}

func isInfraParkStagePushCategory(category string) bool {
	for _, c := range infraParkStagePushCategories {
		if c == category {
			return true
		}
	}
	return false
}

// InfraParkDecision is ClassifyInfraFailurePark's verdict.
type InfraParkDecision int

// Infra-park decisions.
const (
	// InfraParkNotApplicable: the failure takes today's path (completeRun
	// fails the run).
	InfraParkNotApplicable InfraParkDecision = iota
	// InfraParkRetryable: park the run — it stays running, the stage stays
	// failed and retry-eligible (failed -> pending via RetryStage).
	InfraParkRetryable
	// InfraParkBudgetExhausted: the stage already parked
	// InfraFailureParkRetryBudget times; fail the run and record why.
	InfraParkBudgetExhausted
)

// InfraParkHistory is the audit-derived input to ClassifyInfraFailurePark,
// built by ReadInfraParkHistory from the run's audit entries so Advance and
// the stalesweep settled walk decide from the same facts.
type InfraParkHistory struct {
	// PriorParks counts CategoryStageInfraParked rows for the failed stage
	// recorded for EARLIER failure episodes since the stage's last
	// acceptance_outcome_recorded row.
	PriorParks int
	// CurrentEpisodeParked reports a park row already recorded for the
	// stage's CURRENT failure episode (an Advance re-entry).
	CurrentEpisodeParked bool
	// MergeObserved reports a pr_merged row on the run.
	MergeObserved bool
	// DecomposedParent reports a plan_decomposed row on the run: the run is a
	// decomposed parent, whose category-C implement failure is the fan-in
	// verdict that its children failed, not an infrastructure fault (#4426).
	DecomposedParent bool
	// ReopenedPass reports that the failed stage's failure belongs to a
	// re-opened pass (#4426 R4): a pass-trigger row
	// (InfraParkReopenedPassCategories) scoped to the stage, or StagePushed.
	// A pass's recovery owns its own restore, so its failure keeps today's
	// fail-the-run path instead of parking.
	ReopenedPass bool
	// StagePushed reports a pull_request_opened, fixup_pushed or child_pushed
	// row scoped to the failed stage: the stage already produced a head, so
	// it is a re-opened pass whether or not a trigger row survived (#4426
	// approval condition C1). It implies ReopenedPass.
	StagePushed bool
}

// InfraParkEpisode returns the identity of the stage's current failure
// episode: its ended_at (RFC3339Nano, empty when unset) and its retry
// ordinal. RetryStageState clears ended_at and bumps self_retry_count on
// every failed -> pending re-open, so each re-failed episode carries a
// distinct pair. SelfRetryCount is used here ONLY as an episode identity,
// never as a budget count.
func InfraParkEpisode(failed *Stage) (failedAt string, retryOrdinal int) {
	if failed == nil {
		return "", 0
	}
	if failed.EndedAt != nil {
		failedAt = failed.EndedAt.UTC().Format(time.RFC3339Nano)
	}
	return failedAt, failed.SelfRetryCount
}

// infraParkEpisodePayload is the episode-identity subset of a
// CategoryStageInfraParked payload.
type infraParkEpisodePayload struct {
	FailedAt     string `json:"failed_at"`
	RetryOrdinal int    `json:"retry_ordinal"`
}

// ReadInfraParkHistory derives the park history for failed from the run's
// audit entries (any order; ListForRun returns them ascending by sequence).
// A park row whose payload does not decode is counted as an earlier episode,
// so a malformed row can only tighten the budget, never loosen it. The
// implement-only exclusions (DecomposedParent, ReopenedPass) are derived from
// the same entries, so Advance and the stalesweep settled walk decide from
// the same facts.
func ReadInfraParkHistory(entries []*audit.Entry, failed *Stage) InfraParkHistory {
	var h InfraParkHistory
	if failed == nil {
		return h
	}
	curAt, curOrdinal := InfraParkEpisode(failed)
	// The newest acceptance outcome recorded for this stage resets the
	// consecutive count: parks before it belong to an episode chain that
	// ended in an executed verdict.
	var resetSeq int64 = -1
	for _, e := range entries {
		if e == nil {
			continue
		}
		if e.Category == auditCategoryPRMerged {
			h.MergeObserved = true
		}
		if e.Category == auditCategoryPlanDecomposed {
			h.DecomposedParent = true
		}
		if e.StageID != nil && *e.StageID == failed.ID && isInfraParkReopenedPassCategory(e.Category) {
			h.ReopenedPass = true
		}
		if e.StageID != nil && *e.StageID == failed.ID && isInfraParkStagePushCategory(e.Category) {
			h.StagePushed = true
		}
		if e.Category == auditCategoryAcceptanceOutcomeRecorded && e.StageID != nil && *e.StageID == failed.ID && e.Sequence > resetSeq {
			resetSeq = e.Sequence
		}
	}
	for _, e := range entries {
		if e == nil || e.Category != CategoryStageInfraParked || e.StageID == nil || *e.StageID != failed.ID {
			continue
		}
		if e.Sequence <= resetSeq {
			continue
		}
		var p infraParkEpisodePayload
		if err := json.Unmarshal(e.Payload, &p); err == nil && p.FailedAt == curAt && p.RetryOrdinal == curOrdinal {
			h.CurrentEpisodeParked = true
			continue
		}
		h.PriorParks++
	}
	h.ReopenedPass = h.ReopenedPass || h.StagePushed
	return h
}

// InfraParkHistoryOnReadError returns the history to classify failed with
// when the audit read that supplies ReadInfraParkHistory's entries FAILED,
// and ok=true only when that history decides InfraParkNotApplicable (#4426
// approval condition C1). The read error counts as a re-opened pass, so an
// implement failure answers ok=true and the run fails as it did before #4426
// — a failed read never produces a new park. An acceptance failure ignores
// ReopenedPass, so it answers ok=false: its park cannot be bounded without
// its history, and the caller surfaces the error with no transition so the
// next Advance re-decides (#4356).
func InfraParkHistoryOnReadError(stages []*Stage, failed *Stage) (InfraParkHistory, bool) {
	h := InfraParkHistory{ReopenedPass: true}
	return h, ClassifyInfraFailurePark(stages, failed, h) == InfraParkNotApplicable
}

// ClassifyInfraFailurePark decides whether a failed stage parks the run
// instead of failing it (#4356, #4426). Category C is the retryable
// infrastructure class (RetryableFailure): the change is not at fault, so a
// category-C acceptance or implement failure keeps the run non-terminal — the
// stage stays failed and retry-eligible — until the operator retries or
// cancels, or the budget is spent.
//
// The type allow-list is acceptance + implement. Plan, review and deploy
// stages keep today's fail-the-run path. Decomposition fan-in waits on every
// NON-terminal child, so a parked child keeps its parent awaiting_children
// and the parent settles once the retried child succeeds (#4426). An
// implement failure has two implement-only exclusions, both decided from the
// run's own audit history (ReadInfraParkHistory):
//   - a decomposed PARENT (DecomposedParent): its category-C implement
//     failure is the fan-in verdict that its children failed, not an
//     infrastructure fault;
//   - a RE-OPENED PASS (ReopenedPass): a fix-up, conflict-resolution or
//     merge-candidate-verify pass, or a stage that already produced a head.
//     Each pass producer owns its own recovery and deliberately falls through
//     to the fail-the-run path when that recovery did not restore (#4426 R4),
//     so parking it could leave a half-restored review stage or a consumed
//     pass trigger a retry would replay as a plain implement.
//
// The carve-out shared by both types is the POST-MERGE shape (#4356 approval
// condition C2): the run's PR merge was observed (pr_merged), or a review
// stage was resolved succeeded by the merge resolution
// (resolveReviewStageOnMerge) — the merge overtook the failed stage, so the
// run completes as today. A
// post-deploy acceptance (a deploy stage sequenced before it) validates the
// deployed environment after the merge and is never carved out. Every other
// category-C acceptance, or first-execution non-decomposed-parent implement,
// failure parks, whether or not another stage is open: a parked run still
// terminalizes through retry or cancel.
func ClassifyInfraFailurePark(stages []*Stage, failed *Stage, h InfraParkHistory) InfraParkDecision {
	if failed == nil || failed.State != StageStateFailed {
		return InfraParkNotApplicable
	}
	switch failed.Type {
	case StageTypeAcceptance:
	case StageTypeImplement:
		if h.DecomposedParent || h.ReopenedPass {
			return InfraParkNotApplicable
		}
	default:
		return InfraParkNotApplicable
	}
	if failed.FailureCategory == nil || *failed.FailureCategory != FailureC {
		return InfraParkNotApplicable
	}
	postMergeShape := infraParkPostMergeShape(stages, failed, h.MergeObserved)
	if postMergeShape {
		return InfraParkNotApplicable
	}
	if h.PriorParks >= InfraFailureParkRetryBudget {
		return InfraParkBudgetExhausted
	}
	return InfraParkRetryable
}

// infraParkPostMergeShape reports the C2 carve-out: the merge overtook a
// pre-merge failed stage.
func infraParkPostMergeShape(stages []*Stage, failed *Stage, mergeObserved bool) bool {
	for _, s := range stages {
		if s != nil && s.ID != failed.ID && s.Type == StageTypeDeploy && s.Sequence < failed.Sequence {
			return false
		}
	}
	if mergeObserved {
		return true
	}
	for _, s := range stages {
		if s != nil && s.ID != failed.ID && s.Type == StageTypeReview && s.State == StageStateSucceeded {
			return true
		}
	}
	return false
}
