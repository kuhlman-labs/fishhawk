// Package releasenotes renders a releaseevidence.ReleaseEvidence model into
// deterministic, evidence-linked release-notes markdown (E33.2 / #1587,
// ADR-051 option B).
//
// It is the render half of E33: releaseevidence owns the assembly (it is
// declared assembly-only and states "E33.2 owns the endpoint"), and this
// package owns the markdown projection over that model. The server endpoint
// (backend/internal/server/release_notes.go) imports both — assemble, then
// render.
//
// Output is byte-stable: fields are emitted in a fixed order and cost is
// formatted with fixed precision, so the golden-file tests are reproducible.
// The honesty constraint (ADR-051) is preserved at the render layer too: a
// reduced-evidence change (a human-led / loop-bypassing PR with no resolvable
// Fishhawk run) is rendered with an explicit reduced-evidence marker and its
// ReducedReason, omitting the verdict/acceptance/cost fields that would
// otherwise fabricate evidence it does not have.
package releasenotes

import (
	"fmt"
	"io"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/releaseevidence"
)

// Render returns the release-notes markdown for ev.
func Render(ev *releaseevidence.ReleaseEvidence) string {
	var b strings.Builder
	renderInto(&b, ev)
	return b.String()
}

// RenderTo writes the release-notes markdown for ev to w, returning the write
// error. It is the io.Writer convenience over Render.
func RenderTo(w io.Writer, ev *releaseevidence.ReleaseEvidence) error {
	_, err := io.WriteString(w, Render(ev))
	return err
}

// renderInto writes the release-notes markdown into b. A nil ev renders an
// empty-release document so a caller never has to special-case it.
func renderInto(b *strings.Builder, ev *releaseevidence.ReleaseEvidence) {
	if ev == nil {
		b.WriteString("# Release notes\n\n_No release evidence._\n")
		return
	}

	// Header: repo, ref range, the advisory semver-bump hint, and the
	// per-release cost rollup.
	fmt.Fprintf(b, "# Release notes: %s\n\n", ev.Repo)
	fmt.Fprintf(b, "Range: `%s..%s`\n\n", ev.PreviousRef, ev.CandidateRef)
	// Advisory semver-bump hint (E33.4 classifier, wired in E33.5): the
	// heuristic recommendation derived from this same evidence, rendered where
	// the reserved slot used to sit so preview and persisted notes both carry
	// it. It is advisory only — the operator ratifies the actual version at cut
	// time; a wrong hint has no failure mode beyond an operator override.
	fmt.Fprintf(b, "%s\n\n", releaseevidence.ClassifyBump(ev).PreviewLine())
	fmt.Fprintf(b, "Total cost: $%.2f\n\n", ev.TotalCostUSD)

	b.WriteString("## Changes\n\n")
	if len(ev.Changes) == 0 {
		b.WriteString("_No merged changes in range._\n")
		return
	}
	for i := range ev.Changes {
		renderChange(b, &ev.Changes[i])
	}
}

// renderChange writes one change's section. A reduced-evidence change carries
// only the header, the PR link, and the explicit reduced-evidence marker; a
// loop-merged change carries the full evidence trail.
func renderChange(b *strings.Builder, ce *releaseevidence.ChangeEvidence) {
	fmt.Fprintf(b, "### #%d: %s\n\n", ce.PullRequestNumber, ce.Title)
	fmt.Fprintf(b, "- PR: %s\n", ce.PullRequestURL)

	if ce.ReducedEvidence {
		// Honesty constraint (ADR-051): no fabricated verdict/acceptance/cost.
		// Mark the entry explicitly and name why the evidence is reduced.
		fmt.Fprintf(b, "\n> **Reduced evidence.** %s\n\n", ce.ReducedReason)
		return
	}

	if ce.PlanLink != "" {
		fmt.Fprintf(b, "- Plan: %s\n", ce.PlanLink)
	}
	if ce.PlanSummary != "" {
		fmt.Fprintf(b, "\n%s\n", ce.PlanSummary)
	}

	if len(ce.ReviewerVerdicts) > 0 {
		b.WriteString("\nReviewer verdicts:\n")
		for _, rv := range ce.ReviewerVerdicts {
			fmt.Fprintf(b, "- %s: %s\n", rv.ReviewerModel, rv.Verdict)
		}
	}

	if ce.AcceptanceOutcome != nil {
		if ce.AcceptanceOutcome.FailureMode != "" {
			fmt.Fprintf(b, "\nAcceptance: %s (failure mode: %s)\n",
				ce.AcceptanceOutcome.Verdict, ce.AcceptanceOutcome.FailureMode)
		} else {
			fmt.Fprintf(b, "\nAcceptance: %s\n", ce.AcceptanceOutcome.Verdict)
		}
	}

	if len(ce.DeferredConcerns) > 0 {
		b.WriteString("\nDeferred concerns:\n")
		for _, c := range ce.DeferredConcerns {
			fmt.Fprintf(b, "- [%s/%s] %s\n", c.Severity, c.Category, c.Note)
		}
	}

	fmt.Fprintf(b, "\nCost: $%.2f\n\n", ce.CostUSD)
}
