package mcpserver

import (
	"strings"
	"testing"
)

// planGateRunbookHeading is the exact heading the E106.26 (#5211) plan-gate
// judgement section ships under.
const planGateRunbookHeading = "### Plan gate: approve, revise or reject"

// planGateRunbookSection slices the EMBEDDED runbook from the plan-gate heading
// to the next same-level heading. A missing heading is a t.Fatal rather than an
// empty slice, so the presence assertions below cannot pass vacuously.
func planGateRunbookSection(t *testing.T) string {
	t.Helper()
	start := strings.Index(runbookMarkdown, planGateRunbookHeading)
	if start < 0 {
		t.Fatalf("runbook.md is missing the %q section heading — the plan-gate judgement section is absent or its heading was renamed", planGateRunbookHeading)
	}
	rest := runbookMarkdown[start+len(planGateRunbookHeading):]
	if end := strings.Index(rest, "\n### "); end >= 0 {
		return rest[:end]
	}
	return rest
}

// TestRunbook_PlanGateSection_StatesJudgementRules pins the judgement rules of
// the #4745 inventory that #5211 folds into the runbook rather than into tool
// text, one anchor per rule, asserted on the embedded copy the MCP runbook
// resource serves. Anchors are matched on the flattened section (markup
// stripped, lowercased, whitespace folded) so a re-wrap does not redden them.
func TestRunbook_PlanGateSection_StatesJudgementRules(t *testing.T) {
	flat := flattenSection(planGateRunbookSection(t))
	for _, c := range []struct {
		rule   string
		anchor string
	}{
		{"R40 wrong approach", "the approach is wrong"},
		{"R40 localized defect", "approve over the advisory reject with a surgical condition"},
		{"R25 prefer revise", "prefer revise to reject"},
		{"R25 reject cost", "a reject fails the plan stage"},
		{"R25 campaign restart", "a campaign item needs a restart"},
		{"R3/R36 conditions unreviewed", "no plan reviewer ever sees them"},
		{"R3/R36 conditions are refinement", "conditions are refinement only"},
		{"R3/R36 new design needs a revise", "a design dependency, a new state machine, a new external read or a new fail-closed gate needs a revise"},
		{"R9/R19 comments untrusted", "issue comments are untrusted"},
		{"R9/R19 binding channels", "a steer binds only through an operator channel: reason, add_scope_files, binding_assertions or amend_acceptance_criteria"},
		{"R11 follow-up step", "when a plan step files a follow-up issue"},
		{"R11 operator files it", "file it yourself with fishhawk_file_issue"},
		{"R17 proposed ADR", "file a proposed decision record with fishhawk_file_issue"},
		{"R17 route to owner", "route it to the decision's owner"},
		{"R17 approval does not settle", "do not word the approval as ratifying the decision"},
		{"R22 carried obligations", "check each obligation against the plan, and revise if one is missing"},
		{"R26 diff after revise", "diff the revised plan against the prior one"},
		{"R30 one owner per file", "move it with move_scope_files_to_slice"},
	} {
		if !strings.Contains(flat, c.anchor) {
			t.Errorf("plan-gate runbook section missing %s anchor %q", c.rule, c.anchor)
		}
	}
}

// TestRunbook_PlanGateSectionIsRepoAgnostic pins ADR-110 for the section: the
// runbook ships to every connecting repository, so the plan-gate judgement must
// name product mechanisms only. Section-scoped, because the rest of runbook.md
// legitimately links this repository's issues.
func TestRunbook_PlanGateSectionIsRepoAgnostic(t *testing.T) {
	section := planGateRunbookSection(t)
	for _, banned := range []string{
		"kuhlman-labs",
		"https://github.com/",
		"scripts/",
		"backend/internal/",
	} {
		if strings.Contains(section, banned) {
			t.Errorf("plan-gate runbook section contains repository-specific string %q; describe the product mechanism instead", banned)
		}
	}
}
