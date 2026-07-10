package releaseevidence

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/cost"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Audit categories the assembler reads. Declared here rather than
// imported from internal/server to keep this a low-level assembly
// package with no dependency on the HTTP layer; the string values are
// the same the server writes.
const (
	categoryImplementReviewed         = "implement_reviewed"
	categoryAcceptanceOutcomeRecorded = "acceptance_outcome_recorded"
	categoryCostRecorded              = "cost_recorded"
)

// runListLimit bounds the per-PR ListRuns query. Runs-per-PR is small
// (the original plus any fixup / recovery children), so a generous fixed
// cap covers the lineage without pagination — the mergedPRCostFor
// precedent (ListRunsFilter.Limit must be > 0).
const runListLimit = 1000

// planChainDepth caps the parent_run_id walk resolvePlanSummary follows
// so a corrupt parent_run_id cycle can't loop forever. Mirrors the
// prompt-side loadApprovedPlanForRun cap (retryPlanChainDepth = 8): an
// auto-retry / recovery chain is at most a handful of links.
const planChainDepth = 8

// Assembler assembles ReleaseEvidence from the merged-run lineage. It
// reads through the existing run / audit / concern / artifact
// repositories plus the MergedPRResolver seam; it owns no state and
// makes no direct GitHub call (the resolver does).
type Assembler struct {
	Runs      run.Repository
	Audit     audit.Repository
	Concerns  concern.Repository
	Artifacts artifact.Repository
	PRs       MergedPRResolver
}

// Assemble resolves every merged PR in (previousRef, candidateRef],
// assembles per-change evidence, and rolls the per-release cost total as
// the sum of the per-PR rollups so the parity invariant holds by
// construction. A resolver or ListRuns failure is returned as an error;
// all per-change evidence lookups (plan / verdicts / acceptance /
// concerns / cost) are best-effort — a single malformed payload or
// missing sub-record degrades that field without failing the release.
//
// N+1 GetRun / ListStagesForRun / ListForRunByCategory loops are
// acceptable for v0 (the calibration.go precedent): a release spans a
// bounded number of PRs, each with a handful of runs.
func (a *Assembler) Assemble(ctx context.Context, repo, previousRef, candidateRef string) (*ReleaseEvidence, error) {
	prs, err := a.PRs.MergedPRsInRange(ctx, repo, previousRef, candidateRef)
	if err != nil {
		return nil, err
	}

	ev := &ReleaseEvidence{
		Repo:         repo,
		PreviousRef:  previousRef,
		CandidateRef: candidateRef,
	}
	for _, pr := range prs {
		ce, err := a.assembleChange(ctx, pr)
		if err != nil {
			return nil, err
		}
		ev.Changes = append(ev.Changes, ce)
		// TotalCostUSD is the sum of the per-PR rollups by construction, so
		// the release total equals the sum of the per-change costs (the
		// cost-parity invariant).
		ev.TotalCostUSD += ce.CostUSD
	}
	return ev, nil
}

// assembleChange assembles one merged PR's evidence. It maps the PR to
// its runs via pull_request_url equality (the mergedPRCostFor
// precedent); a PR with no resolvable run is emitted reduced-evidence,
// never fabricated. Reviewer verdicts, acceptance, deferred concerns,
// and the plan walk resolve from the SELECTED succeeded run (binding
// condition 1); cost sums across ALL runs on the PR.
func (a *Assembler) assembleChange(ctx context.Context, pr MergedPR) (ChangeEvidence, error) {
	ce := ChangeEvidence{
		PullRequestURL:    pr.URL,
		PullRequestNumber: pr.Number,
		Title:             pr.Title,
	}

	url := pr.URL
	runs, err := a.Runs.ListRuns(ctx, run.ListRunsFilter{
		PullRequestURL: &url,
		Limit:          runListLimit,
	})
	if err != nil {
		return ChangeEvidence{}, err
	}
	if len(runs) == 0 {
		// Honesty constraint (ADR-051): a human-led / loop-bypassing PR in
		// range with no Fishhawk run is reduced-evidence, with no fabricated
		// verdicts or acceptance.
		ce.ReducedEvidence = true
		ce.ReducedReason = "no Fishhawk run resolved for this merged PR (human-led or loop-bypassing change)"
		return ce, nil
	}

	ce.LoopMerged = true
	ce.RunCount = len(runs)
	// Cost sums the cost_recorded ledger across ALL runs on the PR
	// (including a failed recovery parent), via cost.AggregateRunCost — the
	// independent rollup the parity test cross-checks against.
	ce.CostUSD = a.perPRCost(ctx, runs)

	primary := selectPrimaryRun(runs)
	if primary == nil {
		return ce, nil
	}

	// Plan resolves via the parent_run_id walk so a plan-stage-less recovery
	// child still surfaces the parent's approved plan (the get_plan
	// precedent). PlanLink is the PR URL when a plan resolved (the plan
	// artifact carries no external link of its own).
	if summary, found := a.resolvePlanSummary(ctx, primary.ID); found {
		ce.PlanSummary = summary
		ce.PlanLink = pr.URL
	}
	ce.ReviewerVerdicts = a.reviewerVerdicts(ctx, primary.ID)
	ce.AcceptanceOutcome = a.acceptanceOutcome(ctx, primary.ID)
	ce.DeferredConcerns = a.deferredConcerns(ctx, primary.ID)

	return ce, nil
}

// selectPrimaryRun picks the primary evidence run: the NEWEST
// terminal-succeeded run on the PR (binding condition 1). A recovery /
// rerun history makes the earliest run a failed parent, so the earliest
// run must NOT be chosen. When no run on the PR reached succeeded
// (e.g. a merge that bypassed a clean terminal), it falls back to the
// newest run overall so evidence still resolves rather than fabricating.
// nil only for an empty slice.
func selectPrimaryRun(runs []*run.Run) *run.Run {
	var primary *run.Run
	for _, rn := range runs {
		if rn.State != run.StateSucceeded {
			continue
		}
		if primary == nil || rn.CreatedAt.After(primary.CreatedAt) {
			primary = rn
		}
	}
	if primary != nil {
		return primary
	}
	for _, rn := range runs {
		if primary == nil || rn.CreatedAt.After(primary.CreatedAt) {
			primary = rn
		}
	}
	return primary
}

// resolvePlanSummary walks parent_run_id upward from runID until it finds
// a run carrying a standard_v1 plan artifact, or the chain ends / hits
// the depth cap. Best-effort: a repo IO failure or a corrupt cycle
// degrades to ("", false) rather than failing the assembly.
func (a *Assembler) resolvePlanSummary(ctx context.Context, runID uuid.UUID) (string, bool) {
	current := runID
	for depth := 0; depth < planChainDepth; depth++ {
		summary, found, err := a.tryLoadPlanSummary(ctx, current)
		if err != nil {
			return "", false
		}
		if found {
			return summary, true
		}
		rn, err := a.Runs.GetRun(ctx, current)
		if err != nil {
			return "", false
		}
		if rn.ParentRunID == nil {
			return "", false
		}
		current = *rn.ParentRunID
	}
	return "", false
}

// tryLoadPlanSummary returns the standard_v1 plan summary on the single
// run identified by runID: (summary, true, nil) on a hit; ("", false,
// nil) when the run has no plan stage or no usable plan artifact (caller
// walks to the parent); ("", false, err) on repo IO failure. A malformed
// plan payload is skipped as a miss, not an error.
func (a *Assembler) tryLoadPlanSummary(ctx context.Context, runID uuid.UUID) (string, bool, error) {
	stages, err := a.Runs.ListStagesForRun(ctx, runID)
	if err != nil {
		return "", false, err
	}
	var planStageID uuid.UUID
	for _, st := range stages {
		if st.Type == run.StageTypePlan {
			planStageID = st.ID
			break
		}
	}
	if planStageID == uuid.Nil {
		return "", false, nil
	}
	arts, err := a.Artifacts.ListForStage(ctx, planStageID)
	if err != nil {
		return "", false, err
	}
	var picked *artifact.Artifact
	for _, ar := range arts {
		if ar.Kind != artifact.KindPlan {
			continue
		}
		if ar.SchemaVersion == nil || *ar.SchemaVersion != "standard_v1" {
			continue
		}
		if picked == nil || ar.CreatedAt.After(picked.CreatedAt) {
			picked = ar
		}
	}
	if picked == nil {
		return "", false, nil
	}
	var p plan.Plan
	if err := json.Unmarshal(picked.Content, &p); err != nil {
		return "", false, nil
	}
	return p.Summary, true, nil
}

// reviewerVerdicts decodes the run's implement_reviewed entries into
// {reviewer_model, verdict} pairs. A feature_change run has two
// concurrent reviewers, so two entries are expected. Best-effort: a list
// failure yields nil; a malformed entry is skipped.
func (a *Assembler) reviewerVerdicts(ctx context.Context, runID uuid.UUID) []ReviewerVerdict {
	entries, err := a.Audit.ListForRunByCategory(ctx, runID, categoryImplementReviewed)
	if err != nil {
		return nil
	}
	var out []ReviewerVerdict
	for _, e := range entries {
		var p struct {
			ReviewerModel string `json:"reviewer_model"`
			Verdict       string `json:"verdict"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			continue
		}
		out = append(out, ReviewerVerdict{ReviewerModel: p.ReviewerModel, Verdict: p.Verdict})
	}
	return out
}

// acceptanceOutcome returns the LATEST acceptance_outcome_recorded
// outcome for the run (entries are sequence-ascending, so the last decoded
// wins), or nil when the run has none. Best-effort: a list failure yields
// nil; a malformed entry is skipped.
func (a *Assembler) acceptanceOutcome(ctx context.Context, runID uuid.UUID) *AcceptanceOutcome {
	entries, err := a.Audit.ListForRunByCategory(ctx, runID, categoryAcceptanceOutcomeRecorded)
	if err != nil {
		return nil
	}
	var latest *AcceptanceOutcome
	for _, e := range entries {
		var p struct {
			Verdict     string `json:"verdict"`
			FailureMode string `json:"failure_mode"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			continue
		}
		latest = &AcceptanceOutcome{Verdict: p.Verdict, FailureMode: p.FailureMode}
	}
	return latest
}

// deferredConcerns returns the run's concerns in the deferred terminal
// state (a concern converted into a tracked follow-up). Best-effort: a
// list failure yields nil.
func (a *Assembler) deferredConcerns(ctx context.Context, runID uuid.UUID) []ConcernSummary {
	concerns, err := a.Concerns.ListByRun(ctx, runID)
	if err != nil {
		return nil
	}
	var out []ConcernSummary
	for _, c := range concerns {
		if c.State != concern.StateDeferred {
			continue
		}
		out = append(out, ConcernSummary{Severity: c.Severity, Category: c.Category, Note: c.Note})
	}
	return out
}

// perPRCost sums the cost_recorded ledger across every run on the PR via
// cost.AggregateRunCost — the failed parent's spend counts too (binding
// condition 1: the cost rollup sums ALL runs).
func (a *Assembler) perPRCost(ctx context.Context, runs []*run.Run) float64 {
	var total float64
	for _, rn := range runs {
		total += a.runCost(ctx, rn.ID)
	}
	return total
}

// runCost folds one run's cost_recorded entries into its total USD via
// cost.AggregateRunCost (the runCostSummary precedent). Best-effort: a
// list failure yields 0; a malformed entry is skipped.
func (a *Assembler) runCost(ctx context.Context, runID uuid.UUID) float64 {
	entries, err := a.Audit.ListForRunByCategory(ctx, runID, categoryCostRecorded)
	if err != nil {
		return 0
	}
	costEntries := make([]cost.RunCostEntry, 0, len(entries))
	for _, e := range entries {
		var p struct {
			USD    float64 `json:"usd"`
			Source string  `json:"source"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			continue
		}
		costEntries = append(costEntries, cost.RunCostEntry{Source: p.Source, USD: p.USD})
	}
	return cost.AggregateRunCost(costEntries).TotalUSD
}
