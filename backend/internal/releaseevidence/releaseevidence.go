// Package releaseevidence assembles the merged-run evidence between two
// refs (a previous release tag/SHA .. a candidate ref) into a
// ReleaseEvidence model. It is E33's wave-0 assembly layer (ADR-051
// option B, evidence half): the pure query type that resolves every
// merged PR in a ref range, maps each to its Fishhawk run lineage, and
// per change collects the approved plan summary, both reviewer verdicts,
// the acceptance outcome, deferred concerns, and per-PR cost.
//
// The package is deliberately assembly-only: it owns no HTTP surface
// (E33.2 owns the endpoint) and mutates no state. It reads through the
// EXISTING run / audit / concern / artifact repositories plus a small
// MergedPRResolver seam over the GitHub commit walk, so the assembly
// logic is unit-testable offline with a faked resolver.
//
// Honesty constraint (ADR-051): a PR in range with no resolvable
// Fishhawk run — a human-led or loop-bypassing change — is emitted as a
// reduced-evidence ChangeEvidence explicitly marked ReducedEvidence,
// never fabricated with invented verdicts or acceptance.
package releaseevidence

import "context"

// MergedPR is one merged pull request the resolver associated with a
// commit in the compared ref range. Number is the de-dup key across the
// range (a squash/merge commit and any follow-on commits all point at
// the same PR).
type MergedPR struct {
	URL      string
	Number   int
	Title    string
	MergeSHA string
}

// MergedPRResolver resolves the merged PRs whose landing commits fall in
// the (base, head] range. It is the ONE GitHub-touching seam the
// assembler depends on, so the assembly tests fake it and run offline;
// GitHubResolver is the production implementation over
// githubclient.CompareCommits + ListPullRequestsForCommit.
type MergedPRResolver interface {
	// MergedPRsInRange returns every merged PR associated with a commit
	// in (base, head], de-duped by PR number. Order is unspecified.
	MergedPRsInRange(ctx context.Context, repo string, base, head string) ([]MergedPR, error)
}

// ReviewerVerdict mirrors the implement_reviewed audit payload's
// {reviewer_model, verdict} pair — one entry per configured reviewer
// (a feature_change run is reviewed by two agents concurrently).
type ReviewerVerdict struct {
	ReviewerModel string
	Verdict       string
}

// AcceptanceOutcome mirrors the acceptance_outcome_recorded audit
// payload's settled disposition. The plan named this {Verdict, Basis};
// the live payload (backend/internal/server/acceptance.go) carries no
// "basis" field — the decision-useful companion to the verdict is the
// FailureMode split (error | assertion_fail, empty on a passed verdict),
// so that is what is surfaced here.
type AcceptanceOutcome struct {
	Verdict     string
	FailureMode string
}

// ConcernSummary mirrors the reviewer-concern fields (concern.Concern)
// carried through for a deferred concern.
type ConcernSummary struct {
	Severity string
	Category string
	Note     string
}

// ChangeEvidence is the assembled evidence for one merged PR in range.
//
// A loop-merged change (LoopMerged=true) carries the plan summary, both
// reviewer verdicts, the acceptance outcome, deferred concerns, and the
// per-PR cost summed across every run sharing the PR URL. A reduced
// change (ReducedEvidence=true) is a human-led / loop-bypassing PR with
// no resolvable Fishhawk run: its verdict/acceptance fields stay nil and
// ReducedReason names why, so the honesty constraint holds — the reader
// can never mistake an absent record for a passing one.
type ChangeEvidence struct {
	PullRequestURL    string
	PullRequestNumber int
	Title             string

	// PlanSummary is the approved plan's summary; PlanLink is the PR URL
	// (the plan artifact carries no external link of its own). Both empty
	// when no plan artifact resolves — including a plan-stage-less
	// recovery child, for which the plan is resolved via the parent_run_id
	// walk (the get_plan precedent) so the evidence still surfaces.
	PlanSummary string
	PlanLink    string

	ReviewerVerdicts  []ReviewerVerdict
	AcceptanceOutcome *AcceptanceOutcome
	DeferredConcerns  []ConcernSummary

	// CostUSD is the sum of CostUSDTotal across every run on this PR (the
	// mergedPRCostFor precedent): the total cost to land the change.
	CostUSD float64
	// RunCount is how many runs share this PR URL.
	RunCount int

	// LoopMerged reports the PR mapped to >=1 Fishhawk run. ReducedEvidence
	// is its negation for a PR in range with no resolvable run; ReducedReason
	// names why the evidence is reduced (empty when LoopMerged).
	LoopMerged      bool
	ReducedEvidence bool
	ReducedReason   string
}

// ReleaseEvidence is the assembled evidence for a release: every merged
// change between PreviousRef and CandidateRef, with the per-release cost
// total. TotalCostUSD is by construction the sum of every
// ChangeEvidence.CostUSD, so the release total equals the sum of the
// per-PR rollups (the cost-parity invariant the tests assert).
type ReleaseEvidence struct {
	Repo         string
	PreviousRef  string
	CandidateRef string
	Changes      []ChangeEvidence
	TotalCostUSD float64
}
