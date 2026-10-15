package run

import (
	"encoding/json"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
)

// InfraFailureParkRetryBudget is the number of category-C failures of ONE
// acceptance stage the run absorbs by parking (#4356). Each park writes one
// stage-scoped CategoryStageInfraParked audit row; once the stage carries
// this many park rows for EARLIER failure episodes, its next category-C
// failure — the 4th consecutive one — fails the run and writes a
// CategoryStageInfraParkBudgetExhausted row naming the exhausted budget.
//
// The budget counts ONLY those park rows, never Stage.SelfRetryCount, which
// also counts category-A retries and operator category-B override retries
// (#4356 approval condition C1). "Consecutive" holds by construction: a
// category-A/B acceptance failure is not parkable and fails the run, and a
// recorded acceptance outcome for the stage (it executed and shipped a
// verdict) resets the count.
const InfraFailureParkRetryBudget = 3

// Audit categories the #4356 acceptance infrastructure park writes. Both are
// stage-scoped (StageID = the failed acceptance stage) and internal (no
// issue-comment surface).
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
// server's pr_merged and acceptance_outcome_recorded categories, which this
// package cannot import.
const (
	auditCategoryPRMerged                  = "pr_merged"
	auditCategoryAcceptanceOutcomeRecorded = "acceptance_outcome_recorded"
)

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
// so a malformed row can only tighten the budget, never loosen it.
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
	return h
}

// ClassifyInfraFailurePark decides whether a failed stage parks the run
// instead of failing it (#4356). Category C is the retryable infrastructure
// class (RetryableFailure): the change is not at fault, so a category-C
// acceptance failure keeps the run non-terminal — the stage stays failed and
// retry-eligible, the run keeps its merge-candidate line entry — until the
// operator retries or cancels, or the budget is spent.
//
// The type allow-list is deliberately acceptance-only: decomposition fan-in
// resolves a parent only on TERMINAL children, so parking other stage types
// needs its own design (#4356 approval condition C4).
//
// The one carve-out is the POST-MERGE shape (approval condition C2): the
// run's PR merge was observed (pr_merged), or a review stage was resolved
// succeeded by the merge resolution (resolveReviewStageOnMerge) — the merge
// overtook a pre-merge acceptance, so the run completes as today. A
// post-deploy acceptance (a deploy stage sequenced before it) validates the
// deployed environment after the merge and is never carved out. Every other
// category-C acceptance failure parks, whether or not another stage is open:
// a parked run still terminalizes through retry or cancel.
func ClassifyInfraFailurePark(stages []*Stage, failed *Stage, h InfraParkHistory) InfraParkDecision {
	if failed == nil || failed.State != StageStateFailed {
		return InfraParkNotApplicable
	}
	if failed.Type != StageTypeAcceptance {
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
// pre-merge acceptance stage.
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
