package mcpserver

import (
	"strings"
	"testing"
)

// runbookFixupRoutingHeading is the exact heading the E106.27 (#5212) fix-up
// routing judgement section ships under.
const runbookFixupRoutingHeading = "### Fix-up routing: sizing, cost and recovery"

// runbookFixupRoutingSection slices the EMBEDDED runbook from the fix-up
// routing heading to the next same-level heading. A missing heading is a
// t.Fatal rather than an empty slice, so the presence assertions below cannot
// pass vacuously.
func runbookFixupRoutingSection(t *testing.T) string {
	t.Helper()
	start := strings.Index(runbookMarkdown, runbookFixupRoutingHeading)
	if start < 0 {
		t.Fatalf("runbook.md is missing the %q section heading — the fix-up routing judgement section is absent or its heading was renamed", runbookFixupRoutingHeading)
	}
	rest := runbookMarkdown[start+len(runbookFixupRoutingHeading):]
	if end := strings.Index(rest, "\n### "); end >= 0 {
		return rest[:end]
	}
	return rest
}

// TestRunbook_DocumentsFixupRouting pins the fix-up routing judgement rules of
// the #4745 inventory that #5212 folds into the runbook, one anchor per rule,
// asserted on the embedded copy the MCP runbook resource serves. Anchors are
// matched on the flattened section (markup stripped, lowercased, whitespace
// folded) so a re-wrap does not redden them.
func TestRunbook_DocumentsFixupRouting(t *testing.T) {
	flat := flattenSection(runbookFixupRoutingSection(t))
	for _, c := range []struct {
		rule   string
		anchor string
	}{
		// Sizing.
		{"R5 one concern per pass", "one substantive concern per pass"},
		{"R13 high concern alone", "route a high-severity concern alone"},
		{"R7 one counterfactual cycle", "one counterfactual cycle per pass"},
		// Cost.
		{"R14 re-acceptance cost", "re-opens a settled acceptance stage"},
		{"R14 approval cost", "dismiss an existing pull request approval"},
		{"R2 defer after acceptance", "prefer fishhawk_defer_concern"},
		{"R11 triage disposition", "acceptance_triage_decided"},
		{"R11 triage spent a pass", "fixup_dispatched"},
		// Follow-through.
		{"R8/R9 trigger anchor", "fixup_trigger_sequence"},
		{"R8/R9 one verdict per reviewer", "one terminal verdict per configured reviewer"},
		{"R8/R9 failed verdict counts", "implement_review_failed"},
		{"R8/R9 skipped verdict counts", "implement_review_skipped"},
		{"R6 dropped item", "dropped"},
		{"R6 operator_evidence re-route", "attach operator_evidence"},
		{"R6 self-report disagreement", "self-report"},
		{"R1 close it yourself", "fishhawk_resolve_concerns"},
		{"R1 waive or defer", "fishhawk_waive_concern or fishhawk_defer_concern"},
		{"R12 confirmed is not a reproduction", "not an executed reproduction"},
		// Recovery.
		{"R3 fold findings", "dated constraints block"},
		{"R3 cancel", "fishhawk_cancel_run"},
		{"R3 cancel leaves the PR open", "does not close the pull request"},
		{"R4 no-commit refund", "do not repeat the same route unchanged"},
		{"J2 death refund", "once the cause is fixed"},
		{"R16 minted token path", "installation_token_received"},
		{"R16 ambient fallback path", "base_auth_token_unavailable"},
		{"R16 most recent event (A5)", "the most recent installation_token_received / base_auth_token_unavailable event before the runner_failed line"},
		{"R17 refund only on A or C", "only when it died category a or c"},
		{"R17 local reaper reports C", "a locally reaped runner reports category c"},
		{"R17 category B consumes", "category-b (policy) failure consumes a pass"},
		{"R17 reopen error", "verify with fishhawk_get_gate_view"},
	} {
		if !strings.Contains(flat, c.anchor) {
			t.Errorf("fix-up routing runbook section missing %s anchor %q", c.rule, c.anchor)
		}
	}
	// A5: the credential event is the most recent one before runner_failed,
	// not whichever line happens to precede it.
	if strings.Contains(flat, "the event logged just before") {
		t.Errorf("fix-up routing runbook section says %q; name the most recent installation_token_received / base_auth_token_unavailable event before the runner_failed line", "the event logged just before")
	}
}

// TestRunbook_FixupRoutingSectionIsRepoAgnostic pins ADR-110 for the section:
// the runbook ships to every connecting repository, so the fix-up routing
// judgement must name product mechanisms only. Section-scoped, because the
// rest of runbook.md legitimately links this repository's issues.
func TestRunbook_FixupRoutingSectionIsRepoAgnostic(t *testing.T) {
	section := runbookFixupRoutingSection(t)
	for _, banned := range []string{
		"kuhlman-labs",
		"https://github.com/",
		"scripts/",
		"backend/internal/",
		".fishhawk/",
	} {
		if strings.Contains(section, banned) {
			t.Errorf("fix-up routing runbook section contains repository-specific string %q; describe the product mechanism instead", banned)
		}
	}
}

// TestRunbook_NoStaleNoOpBurnsBudgetClaim pins the correction of the stale
// local-drive claim that a no-op fix-up burns the budget and wedges the run.
// A fix-up that produces no commit records fixup_no_changes, re-parks the
// review gate and is refunded against the budget. The check runs over the
// WHOLE embedded runbook, whitespace-folded, because the stale sentence was
// hard-wrapped across two lines: a per-line grep would not find it.
func TestRunbook_NoStaleNoOpBurnsBudgetClaim(t *testing.T) {
	flat := flattenSection(runbookMarkdown)
	for _, stale := range []string{
		"a no-op fixup (zero diff) still burns the budget",
		"still burns the",
		"wedges the run",
	} {
		if strings.Contains(flat, stale) {
			t.Errorf("runbook.md still carries the stale no-op fix-up claim %q; a no-commit fix-up is refunded (fixup_no_changes) and re-parks the review gate — state the refund rule instead", stale)
		}
	}
}
