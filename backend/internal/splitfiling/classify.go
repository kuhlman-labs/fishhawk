package splitfiling

import (
	"fmt"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
)

// ContractClassification is how the terminal (contract) phase of a split
// proposal is handled: keep the transitional names permanently and delete only
// (the default), or take a one-time governed cap exception because an atomic
// rename cannot fit one in-cap PR.
type ContractClassification string

const (
	// ClassificationDeleteOnly is the default: the contract phase deletes the
	// transitional names and keeps the new names as they are, fitting one in-cap
	// PR. It is also the fail-safe when reachability evidence is absent or the cap
	// is unresolved — honoring the issue's stated default of keeping transitional
	// names permanently.
	ClassificationDeleteOnly ContractClassification = "delete-only"
	// ClassificationGovernedException is chosen only when the contract phase's
	// reachability-derived file count exceeds the resolved implement cap, so the
	// atomic rename overflows one in-cap PR and needs an operator-authored,
	// admin-merged cap exception.
	ClassificationGovernedException ContractClassification = "governed-exception"
)

// PhaseEvidence is the neutral input mirror of the server's
// PlanReachabilityPhase / runner reachability.PhaseResult: one split-proposal
// phase's declared-vs-derived file counts. DerivedCount > DeclaredCount means the
// phase's symbols leak into sibling phases. This package owns the mirror so it
// takes no server dependency.
type PhaseEvidence struct {
	Index         int
	Title         string
	DeclaredCount int
	DerivedCount  int
}

// Classify decides how the contract (terminal) phase of the proposal is handled.
// It returns ClassificationDeleteOnly by default and ClassificationGovernedException
// ONLY when the contract phase's reachability DerivedCount exceeds cap (an atomic
// rename cannot ship as one in-cap PR).
//
// It fails safe to ClassificationDeleteOnly on every uncertain branch:
//   - cap <= 0 (unresolved implement cap),
//   - the proposal has no phases,
//   - no reachability evidence for the contract phase index.
//
// This honors the triggering issue's stated default: keep the transitional names
// permanently and delete only, unless the evidence positively shows naming
// genuinely cannot be transitional (the rename overflows the cap).
func Classify(proposal plan.SplitProposal, evidence []PhaseEvidence, cap int) ContractClassification {
	if cap <= 0 {
		return ClassificationDeleteOnly
	}
	contractIdx := len(proposal.Phases) - 1
	if contractIdx < 0 {
		return ClassificationDeleteOnly
	}
	ev, ok := findEvidence(evidence, contractIdx)
	if !ok {
		return ClassificationDeleteOnly
	}
	if ev.DerivedCount > cap {
		return ClassificationGovernedException
	}
	return ClassificationDeleteOnly
}

// findEvidence returns the PhaseEvidence for the given 0-based phase index.
func findEvidence(evidence []PhaseEvidence, index int) (PhaseEvidence, bool) {
	for _, e := range evidence {
		if e.Index == index {
			return e, true
		}
	}
	return PhaseEvidence{}, false
}

// CapExceptionDraft is the in-memory-only artifact pair for a governed-exception
// contract phase: a unified-diff-style spec diff raising the implement
// max_files_changed cap, and a PR body derived from the atomicity evidence. Both
// are strings only and are NEVER written to disk — .fishhawk/** is agent-forbidden,
// so the operator authors and admin-merges the real change; these ride the audit
// payload and fishhawk_get_plan as a draft.
type CapExceptionDraft struct {
	SpecDiff string
	PRBody   string
}

// DraftCapException renders the cap-exception draft for a governed-exception
// contract phase, or returns nil when Classify yields delete-only. The spec diff
// literally raises max_files_changed from cap to the contract phase's DerivedCount;
// the PR body names the atomicity evidence (derived-vs-declared counts) and states
// explicitly that the change must be operator-authored and admin-merged because
// .fishhawk/** is agent-forbidden.
func DraftCapException(proposal plan.SplitProposal, evidence []PhaseEvidence, cap int) *CapExceptionDraft {
	if Classify(proposal, evidence, cap) != ClassificationGovernedException {
		return nil
	}
	// Governed-exception guarantees the contract-phase evidence exists and its
	// DerivedCount > cap (both checked by Classify above).
	contractIdx := len(proposal.Phases) - 1
	ev, _ := findEvidence(evidence, contractIdx)
	return &CapExceptionDraft{
		SpecDiff: renderSpecDiff(cap, ev.DerivedCount),
		PRBody:   renderPRBody(ev, cap),
	}
}

// renderSpecDiff renders a unified-diff-style spec diff raising the implement
// stage's max_files_changed from oldCap to newCap.
func renderSpecDiff(oldCap, newCap int) string {
	return fmt.Sprintf(`--- a/.fishhawk/workflows.yaml
+++ b/.fishhawk/workflows.yaml
@@ implement stage constraints @@
-        max_files_changed: %d
+        max_files_changed: %d
`, oldCap, newCap)
}

// renderPRBody renders the operator-facing PR body for the cap-exception change.
func renderPRBody(ev PhaseEvidence, oldCap int) string {
	title := strings.TrimSpace(ev.Title)
	if title == "" {
		title = fmt.Sprintf("phase %d", ev.Index+1)
	}
	return fmt.Sprintf(`## Summary

Raise the implement-stage `+"`max_files_changed`"+` cap from %d to %d for the contract phase %q of the approved split proposal.

The contract phase's atomic rename touches %d files by symbol reachability (derived) versus %d declared — the rename cannot be partitioned into an in-cap PR without producing a non-compiling intermediate, so it needs a one-time governed cap exception rather than a permanent transitional name.

## Notes

**Operator-authored, admin-merged.** `+"`.fishhawk/**`"+` is agent-forbidden: an implement agent cannot author or self-merge this spec change. An operator must author this diff and admin-merge it.

Parent-close is deferred to follow-up #%d (E50.6).
`, oldCap, ev.DerivedCount, title, ev.DerivedCount, ev.DeclaredCount, DeferralIssue)
}
