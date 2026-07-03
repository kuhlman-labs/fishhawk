package refinement

// precheck.go runs the plan-acceptance rule set over every drafted child at
// refinement intake (E34.5 / #1596, ADR-052). It is the intake sibling of the
// server's plan-gate acceptance pre-check: both dispatch through the ONE
// exported plan.EvaluateAcceptanceCriteria, so a rule added to the shared set
// applies to intake and the plan gate at once with no second copy to drift.
//
// The result is advisory. A child flagged no_blocking_criterion marks the
// draft needs_attention so the preview shows the defect before the operator
// decides — but approval remains legal (the operator can approve anyway). This
// is why EpicDraft.Validate no longer hard-rejects a criteria-less child: the
// child must be able to REACH the preview for the gate to be informational
// rather than a 422.

import (
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
)

// CriteriaPrecheck is the intake acceptance-criteria pre-check result for a
// whole draft: the per-child findings plus a draft-level NeedsAttention marker
// (true when ANY child is flagged). Children is always non-nil ([] not null) so
// a reader can distinguish "checked and clean" from "never checked", matching
// the plan-gate contract.
type CriteriaPrecheck struct {
	NeedsAttention bool                 `json:"needs_attention"`
	Children       []ChildCriteriaCheck `json:"children"`
}

// ChildCriteriaCheck is one child's intake pre-check. Ordinal is the 1-based
// sibling ordinal (matching Waves / the preview ordering). NeedsAttention is
// true when the child's findings include no_blocking_criterion (the issue's
// needs-attention trigger — an unjustified missing blocking criterion); other
// findings render but do not set the marker. Findings is always non-nil.
type ChildCriteriaCheck struct {
	Ordinal        int                      `json:"ordinal"`
	NeedsAttention bool                     `json:"needs_attention,omitempty"`
	Findings       []plan.AcceptanceFinding `json:"findings"`
}

// EvaluateDraftCriteria runs the shared plan-acceptance rule set over every
// child of the draft and returns the advisory intake pre-check.
//
// Each child's []string acceptance_criteria maps into a synthetic
// plan.Verification: the criterion text becomes both the AcceptanceCriterion ID
// (so an empty/whitespace criterion fires empty_id and two identical criteria
// fire duplicate_id) and its Statement. Blocking is left nil (the schema
// default is blocking=true), and Source is left empty — so the provenance rules
// (missing_source_ref / missing_rationale) cannot fire on the intake shape by
// construction, yet the SAME evaluator runs so any rule added to the shared set
// applies at intake with no duplication.
//
// The epic's out_of_scope prose is the only out-of-scope justification that
// exists at draft time (children carry none), so a non-empty epic out_of_scope
// supplies Verification.OutOfScope for EVERY child — suppressing
// no_blocking_criterion across the draft (the justified escape hatch).
func EvaluateDraftCriteria(d EpicDraft) CriteriaPrecheck {
	var oos []string
	if trimmed := strings.TrimSpace(d.Epic.OutOfScope); trimmed != "" {
		oos = []string{trimmed}
	}

	children := make([]ChildCriteriaCheck, 0, len(d.Children))
	needsAttention := false
	for i, c := range d.Children {
		mapped := make([]plan.AcceptanceCriterion, 0, len(c.AcceptanceCriteria))
		for _, s := range c.AcceptanceCriteria {
			mapped = append(mapped, plan.AcceptanceCriterion{
				ID:        strings.TrimSpace(s),
				Statement: s,
			})
		}
		findings := plan.EvaluateAcceptanceCriteria(plan.Verification{
			AcceptanceCriteria: mapped,
			OutOfScope:         oos,
		})
		childFlagged := false
		for _, f := range findings {
			if f.Rule == plan.RuleNoBlockingCriterion {
				childFlagged = true
				break
			}
		}
		if childFlagged {
			needsAttention = true
		}
		children = append(children, ChildCriteriaCheck{
			Ordinal:        i + 1,
			NeedsAttention: childFlagged,
			Findings:       findings,
		})
	}

	return CriteriaPrecheck{
		NeedsAttention: needsAttention,
		Children:       children,
	}
}
