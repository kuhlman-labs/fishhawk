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

// Server categories and values the #4816 reaped-live-merge-candidate-pass arm
// reads. This package cannot import the server, so each is mirrored here and
// bound to the server's own constant (and, for the trigger, to the server's
// own populated()) by TestInfraParkReopenedPassCategoriesMatchServer in
// backend/internal/server.
const (
	infraParkFixupTriggerCategory              = "stage_fixup_triggered"
	infraParkConflictResolutionTriggerCategory = "stage_conflict_resolution_triggered"
	// infraParkMergeCandidateTriggerCategory is the server's
	// stage_merge_candidate_verify_triggered row: a verify-only pass re-opened
	// the implement stage (ADR-090 D3).
	infraParkMergeCandidateTriggerCategory = "stage_merge_candidate_verify_triggered"
	// infraParkMergeCandidateVerifiedCategory is the server's
	// merge_candidate_verified row. A same-stage row at or after the newest
	// trigger consumes it (server liveMergeCandidateTrigger).
	infraParkMergeCandidateVerifiedCategory = "merge_candidate_verified"
	// infraParkOperatorReapCategory is the server's dispatch_reaper_failed row,
	// written by the reap-failure endpoint. The dispatch watchdog's
	// dispatch_watchdog_elapsed row is deliberately NOT mirrored or read: only
	// an operator reap produces parking evidence (#4816 N1).
	infraParkOperatorReapCategory = "dispatch_reaper_failed"
)

// infraParkMergeCandidateCauses mirrors the server's validMergeCandidateCause
// enumeration. A trigger with any other cause is one the server would not
// serve, so its retry replays a plain implement and must not park.
var infraParkMergeCandidateCauses = []string{"base_advance", "conflict_resolution", "fan_in"}

// InfraParkRunnerLostReason is the reap reason that marks a runner loss — the
// only operator-reap reason that produces parking evidence (#4816 approval
// condition C2). A manual fishhawk_reap_stage names it; the detached reaper's
// synthesized or runner_failed-parsed reasons, the zero-exit strand reaper's
// reason and the concurrency-slot spawn-failure compensation do not, so they
// keep today's fail-the-run path.
const InfraParkRunnerLostReason = "runner_lost"

// InfraParkReopenedPassMergeCandidateVerify is the reopened_pass value the
// orchestrator writes on a stage_infra_parked row when the parked failure is
// an operator-reaped live merge-candidate verify-only pass (#4816). A
// first-execution implement or acceptance park row carries no reopened_pass key.
const InfraParkReopenedPassMergeCandidateVerify = "merge_candidate_verify"

// InfraParkOperatorReapCategory returns the mirrored dispatch_reaper_failed
// category ReadInfraParkHistory reads as an operator reap.
func InfraParkOperatorReapCategory() string { return infraParkOperatorReapCategory }

// InfraParkMergeCandidateVerifiedCategory returns the mirrored
// merge_candidate_verified category ReadInfraParkHistory reads as the
// consumption of a merge-candidate trigger.
func InfraParkMergeCandidateVerifiedCategory() string {
	return infraParkMergeCandidateVerifiedCategory
}

// infraParkReopenedPassCategories are the server's pass-trigger categories
// (#4426 R4): a fix-up, conflict-resolution or merge-candidate-verify pass
// re-opens a finished implement stage and writes one of these rows scoped to
// it. This package cannot import the server, so the list is mirrored and
// bound to the server constants by InfraParkReopenedPassCategories' mirror
// test in backend/internal/server.
var infraParkReopenedPassCategories = []string{
	infraParkFixupTriggerCategory,
	infraParkConflictResolutionTriggerCategory,
	infraParkMergeCandidateTriggerCategory,
}

// infraParkStagePushCategories are the server's head-producing categories
// (#4426 approval condition C1): an implement PR open, a fix-up push and a
// decomposition-child push. A row of one of them scoped to the failed
// implement stage proves the stage already produced a head, so its failure is
// a re-opened pass even when no trigger row for the pass is on the chain. Since
// #4815 a failed stage_fixup_triggered append refuses the fix-up before the
// re-open, so the fix-up path no longer produces that shape; the carve-out
// stays for chains written before that change and is independent of it.
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
	// ReapedLiveMergeCandidatePass reports that the failure is an
	// operator-reaped runner loss of a LIVE merge-candidate verify-only pass
	// (#4816), whose retry replays the same pass. All of these hold for the
	// failed stage:
	//   - its newest stage_merge_candidate_verify_triggered row exists and is
	//     populated exactly as the server's populated() requires;
	//   - no same-stage merge_candidate_verified row at or after it (live);
	//   - no same-stage stage_fixup_triggered or
	//     stage_conflict_resolution_triggered row after it (newest pass);
	//   - a same-stage dispatch_reaper_failed row carrying NO reported_by (an
	//     operator reap, not a run-bound runner self-report), reason
	//     InfraParkRunnerLostReason, and the stage's CURRENT failure episode
	//     (failed_at + retry_ordinal, InfraParkEpisode) stamped by the reap.
	// It overrides ReopenedPass and StagePushed in the implement guard without
	// redefining them.
	ReapedLiveMergeCandidatePass bool
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

// infraParkMergeCandidateTrigger is the anchor subset of the server's
// mergeCandidateVerifyTrigger, with the same JSON tags and field types, so a
// payload decodes (or fails to decode) here exactly as it does on the server.
type infraParkMergeCandidateTrigger struct {
	Branch                         string `json:"branch"`
	BaseRef                        string `json:"base_ref"`
	ExpectedHeadSHA                string `json:"expected_head_sha"`
	Cause                          string `json:"cause"`
	VerifyCommand                  string `json:"verify_command"`
	PriorState                     string `json:"prior_state,omitempty"`
	ReparkedReviewStageID          string `json:"reparked_review_stage_id,omitempty"`
	SupersedesFixupTriggerSequence int64  `json:"supersedes_fixup_trigger_sequence,omitempty"`
}

// infraParkMergeCandidateTriggerPopulated mirrors the server's
// mergeCandidateVerifyTrigger.populated() EXACTLY, cause enumeration included.
// A trigger the server would not serve replays as a plain implement, so it
// must not park.
func infraParkMergeCandidateTriggerPopulated(raw json.RawMessage) bool {
	var t infraParkMergeCandidateTrigger
	if err := json.Unmarshal(raw, &t); err != nil {
		return false
	}
	causeOK := false
	for _, c := range infraParkMergeCandidateCauses {
		if c == t.Cause {
			causeOK = true
		}
	}
	return t.Branch != "" && t.BaseRef != "" && t.ExpectedHeadSHA != "" && causeOK && t.VerifyCommand != ""
}

// infraParkOperatorReapPayload is the subset of a dispatch_reaper_failed
// payload the #4816 arm reads.
type infraParkOperatorReapPayload struct {
	Reason       string `json:"reason"`
	FailedAt     string `json:"failed_at"`
	RetryOrdinal int    `json:"retry_ordinal"`
	ReportedBy   string `json:"reported_by"`
}

// infraParkOperatorReapInEpisode reports whether a dispatch_reaper_failed
// payload is an OPERATOR runner-loss reap of the stage's CURRENT failure
// episode (curAt, curOrdinal). It refuses:
//   - an undecodable payload;
//   - a run-bound runner self-report (reported_by present, #3598): only an
//     operator reap produces parking evidence (#4816 N1), even if a future
//     change stamped the run-bound arm;
//   - any reason other than InfraParkRunnerLostReason (#4816 approval
//     condition C2): a category-C reap that is not a runner loss fails the run;
//   - an unstamped row (written before the stamp, decoding as ("", 0)), and a
//     row stamped for another episode. The episode is the stage's own
//     (ended_at, self_retry_count) pair, which RetryStageState changes on
//     every failed -> pending re-open, so it cannot be lost or reused.
func infraParkOperatorReapInEpisode(raw json.RawMessage, curAt string, curOrdinal int) bool {
	var p infraParkOperatorReapPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return false
	}
	if p.ReportedBy != "" {
		return false
	}
	if p.Reason != InfraParkRunnerLostReason {
		return false
	}
	return p.FailedAt != "" && p.FailedAt == curAt && p.RetryOrdinal == curOrdinal
}

// infraParkReapedLiveMergeCandidatePass derives
// InfraParkHistory.ReapedLiveMergeCandidatePass from the newest same-stage
// sequences ReadInfraParkHistory tracked (-1 when absent) and the newest
// trigger's payload.
func infraParkReapedLiveMergeCandidatePass(triggerSeq, verifiedSeq, otherPassSeq int64, trigger json.RawMessage, operatorReap bool) bool {
	live := verifiedSeq < triggerSeq
	newestPass := triggerSeq > otherPassSeq
	return triggerSeq >= 0 && live && newestPass && operatorReap && infraParkMergeCandidateTriggerPopulated(trigger)
}

// ReadInfraParkHistory derives the park history for failed from the run's
// audit entries (any order; ListForRun returns them ascending by sequence).
// A park row whose payload does not decode is counted as an earlier episode,
// so a malformed row can only tighten the budget, never loosen it. The
// implement-only exclusions (DecomposedParent, ReopenedPass) and the #4816
// override (ReapedLiveMergeCandidatePass) are derived from the same entries,
// so Advance and the stalesweep settled walk decide from the same facts.
//
// Per-producer decision for a category-C implement failure of a re-opened
// merge-candidate verify-only pass (#4816; the full table, with the pinning
// test per row, is in backend/internal/run/README.md "Infrastructure park"):
//   - PARK: an operator-scoped reap (fishhawk_reap_stage, or the reap-failure
//     endpoint under an operator token or session, conditional or not) whose
//     reason is runner_lost, of a live, newest, populated pass, whose row is
//     stamped with the stage's current episode.
//   - FAIL: the run-bound runner self-report (#3598) and the dispatch watchdog
//     (neither produces parking evidence); an operator reap with any other
//     reason (the detached reaper's non-zero-exit and zero-exit strand
//     reasons, the concurrency-slot spawn-failure compensation); every
//     producer's recovery fallback that writes no operator reap row; a reap
//     after the trigger was consumed or superseded by a newer fix-up or
//     conflict-resolution pass; a decomposed parent; and any failure whose
//     only operator-reap row is from an earlier episode or unstamped.
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
	// #4816: the newest same-stage merge-candidate trigger (and its payload),
	// merge_candidate_verified row and fix-up / conflict-resolution trigger,
	// plus whether an operator runner-loss reap of the current episode exists.
	// The episode match takes no sequence floor: its identity is the stage's
	// own (ended_at, self_retry_count) pair.
	var triggerSeq, verifiedSeq, otherPassSeq int64 = -1, -1, -1
	var trigger json.RawMessage
	operatorReap := false
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
		if e.StageID != nil && *e.StageID == failed.ID {
			switch e.Category {
			case infraParkMergeCandidateTriggerCategory:
				if e.Sequence > triggerSeq {
					triggerSeq, trigger = e.Sequence, e.Payload
				}
			case infraParkMergeCandidateVerifiedCategory:
				if e.Sequence > verifiedSeq {
					verifiedSeq = e.Sequence
				}
			case infraParkFixupTriggerCategory, infraParkConflictResolutionTriggerCategory:
				if e.Sequence > otherPassSeq {
					otherPassSeq = e.Sequence
				}
			case infraParkOperatorReapCategory:
				if infraParkOperatorReapInEpisode(e.Payload, curAt, curOrdinal) {
					operatorReap = true
				}
			}
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
	h.ReapedLiveMergeCandidatePass = infraParkReapedLiveMergeCandidatePass(triggerSeq, verifiedSeq, otherPassSeq, trigger, operatorReap)
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
// The one re-opened pass that parks (#4816) is ReapedLiveMergeCandidatePass:
// an operator's runner-loss reap of a live, newest, populated merge-candidate
// verify-only pass, stamped for the current episode. Its trigger is still
// live, so the server's prompt resolves it first and exclusively (ADR-090 D3)
// and a retry replays the same verify-only pass; the review stage was never
// re-opened by a verify-only pass's recovery, so nothing is half-restored.
// A decomposed parent still never parks. The per-producer decision table is
// on ReadInfraParkHistory.
//
// The carve-out shared by both types is the POST-MERGE shape (#4356 approval
// condition C2): the run's PR merge was observed (pr_merged), or a review
// stage was resolved succeeded by the merge resolution
// (resolveReviewStageOnMerge) — the merge overtook the failed stage, so the
// run completes as today. A
// post-deploy acceptance (a deploy stage sequenced before it) validates the
// deployed environment after the merge and is never carved out. Every other
// category-C acceptance, first-execution non-decomposed-parent implement, or
// #4816 operator-reaped live verify-pass implement failure parks, whether or
// not another stage is open: a parked run still terminalizes through retry or
// cancel. The category-C check, the C2 carve-out and the lifetime park budget
// are shared with the #4816 arm, so an operator-reaped live verify pass parks
// at most InfraFailureParkRetryBudget times.
func ClassifyInfraFailurePark(stages []*Stage, failed *Stage, h InfraParkHistory) InfraParkDecision {
	if failed == nil || failed.State != StageStateFailed {
		return InfraParkNotApplicable
	}
	switch failed.Type {
	case StageTypeAcceptance:
	case StageTypeImplement:
		if h.DecomposedParent || (h.ReopenedPass && !h.ReapedLiveMergeCandidatePass) {
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
