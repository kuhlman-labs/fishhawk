package spec

import (
	"os"
	"strings"
	"testing"
)

// reportFor migrates src and returns the rendered eligibility report.
func reportFor(t *testing.T, src string) string {
	t.Helper()
	res, err := MigrateBytes([]byte(src))
	if err != nil {
		t.Fatalf("MigrateBytes: %v", err)
	}
	if res.Report == nil {
		t.Fatal("the eligibility report is emitted on every path and must never be nil")
	}
	return res.Report.Render()
}

// TestReport_TranslatedGateEnumeratesResolvedMembers: a gate translated
// from the roles map renders its resolved members under the ENUMERABLE
// heading.
func TestReport_TranslatedGateEnumeratesResolvedMembers(t *testing.T) {
	got := reportFor(t, minimalV1)
	for _, want := range []string{
		"gate /workflows/feature_change/stages/0/gates/0 (stage: plan)",
		"before: approvers: {any_of: [founder]}",
		"after:  approvals: {count: 1, members: [kuhlman-labs]}",
		"enumerable from this document:",
		"      - kuhlman-labs",
		"not resolvable from this document:\n      (none)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report is missing %q\n---\n%s", want, got)
		}
	}
}

// TestReport_UnresolvedPredicatesRenderSymbolically is the rule this file
// exists for: a gate governed by min_permission / member_of renders those
// predicates as THEMSELVES under an explicit not-resolvable heading with
// the forge-resolved marker — never as an empty set, which would read as
// "nobody can approve this" and invite exactly the wrong edit.
func TestReport_UnresolvedPredicatesRenderSymbolically(t *testing.T) {
	src := strings.Replace(minimalV1,
		"            approvers:\n              any_of: [founder]",
		"            approvals:\n              count: 2\n              min_permission: write\n              member_of: acme/reviewers", 1)
	got := reportFor(t, src)

	for _, want := range []string{
		"min_permission >= write   (forge-resolved at run time)",
		"member_of = acme/reviewers   (forge-resolved at run time)",
		"change: none",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report is missing %q\n---\n%s", want, got)
		}
	}
	// The rendering must be demonstrably NON-EMPTY: the enumerable half
	// is legitimately empty here, and the whole point is that the block
	// still says who can approve.
	idx := strings.Index(got, "not resolvable from this document:")
	if idx < 0 {
		t.Fatalf("no not-resolvable heading in:\n%s", got)
	}
	if strings.Contains(got[idx:idx+120], "(none)") {
		t.Errorf("the unresolved half rendered as (none) for a gate governed only by forge predicates:\n%s", got)
	}
}

// TestReport_CountAndNotOnlyGate: a gate with no members and no forge
// predicate prints the explicit no-further-constraint line rather than
// two empty lists.
func TestReport_CountAndNotOnlyGate(t *testing.T) {
	src := strings.Replace(minimalV1,
		"            approvers:\n              any_of: [founder]",
		"            approvals:\n              count: 1\n              not: [author, agent]", 1)
	got := reportFor(t, src)
	if !strings.Contains(got, "(no approver constraint beyond count/not)") {
		t.Errorf("missing the explicit no-further-constraint line\n---\n%s", got)
	}
	if !strings.Contains(got, "not: [author, agent]") {
		t.Errorf("the rendering must carry the declared exclusion\n---\n%s", got)
	}
}

// TestReport_WideningNoteFires: every gate migrated out of the legacy
// grammar emits no `not:`, which WIDENS eligibility relative to the
// shipped presets' default. The report says so rather than adding the
// exclusion silently.
func TestReport_WideningNoteFires(t *testing.T) {
	got := reportFor(t, minimalV1)
	if !strings.Contains(got, "WIDENED") || !strings.Contains(got, "not: [author, agent]") {
		t.Errorf("the widening note must fire on a migrated gate\n---\n%s", got)
	}
	// And it must NOT fire for a gate carried through verbatim.
	src := strings.Replace(minimalV1,
		"            approvers:\n              any_of: [founder]",
		"            approvals:\n              count: 1\n              members: [kuhlman-labs]", 1)
	if unchanged := reportFor(t, src); strings.Contains(unchanged, "WIDENED") {
		t.Errorf("an already-approvals gate must not report a widening\n---\n%s", unchanged)
	}
}

// TestReport_EmittedOnRefusalAndNoOpPaths: the report is the product even
// when the bytes are not.
func TestReport_EmittedOnRefusalAndNoOpPaths(t *testing.T) {
	t.Run("refusal", func(t *testing.T) {
		src := strings.Replace(minimalV1, "              any_of: [founder]", "              any_of: [reviewers]", 1)
		got := reportFor(t, src)
		if !strings.Contains(got, "before: approvers: {any_of: [reviewers]}") {
			t.Errorf("the refusal path must still render the gate's source predicate\n---\n%s", got)
		}
		if !strings.Contains(got, "not computed") {
			t.Errorf("a refused gate must state that no eligibility change was computed\n---\n%s", got)
		}
	})

	t.Run("already v2 no-op", func(t *testing.T) {
		src, err := os.ReadFile(goldenV2)
		if err != nil {
			t.Fatalf("read v2 golden: %v", err)
		}
		got := reportFor(t, string(src))
		if !strings.Contains(got, "kuhlman-labs") || !strings.Contains(got, "change: none") {
			t.Errorf("the no-op path must render every gate unchanged\n---\n%s", got)
		}
	})

	t.Run("version major 0 refusal", func(t *testing.T) {
		src := strings.Replace(minimalV1, `version: "1.6"`, `version: "0.7"`, 1)
		if got := reportFor(t, src); !strings.Contains(got, "stage: plan") {
			t.Errorf("the R9 path must still render the document's gates\n---\n%s", got)
		}
	})
}

// TestReport_RefusalStillNamesSourceEligiblePrincipals: a gate whose
// translation refused still lists the principals resolvable from the roles
// map on the source side, so the mandatory report names who could approve
// on each side even on a refusal path.
func TestReport_RefusalStillNamesSourceEligiblePrincipals(t *testing.T) {
	// R1: all_of over a multi-member role refuses, but the source-side
	// union resolves fully from the roles map.
	src := strings.NewReplacer(
		"  founder:\n    members: [\"@kuhlman-labs\"]\n",
		"  role_a:\n    members: [\"@x\", \"@y\"]\n  role_b:\n    members: [\"@z\"]\n",
		"              any_of: [founder]", "              all_of: [role_a, role_b]",
	).Replace(minimalV1)
	got := reportFor(t, src)
	if !strings.Contains(got, "not computed") {
		t.Fatalf("the gate must be marked refused\n---\n%s", got)
	}
	for _, want := range []string{"      - x", "      - y", "      - z"} {
		if !strings.Contains(got, want) {
			t.Errorf("a refusal report must enumerate the source-side eligible principals, missing %q\n---\n%s", want, got)
		}
	}
}

// TestReport_BudgetStageUSDNote: the report notes per budget-bearing stage
// that no limit_usd ceiling was derivable and one must be added by hand.
// The codemod never fabricates a USD figure — no token-to-USD rate exists.
func TestReport_BudgetStageUSDNote(t *testing.T) {
	src := strings.Replace(minimalV1, "        produces:\n",
		"        budget:\n          max_tokens: 200000\n          max_runtime_minutes: 15\n        produces:\n", 1)
	got := reportFor(t, src)
	if !strings.Contains(got, "limit_usd") {
		t.Errorf("the report must note the missing USD ceiling\n---\n%s", got)
	}
	if !strings.Contains(got, "  - plan") {
		t.Errorf("the note must name the budget-bearing stage (plan)\n---\n%s", got)
	}
	if strings.Contains(got, "$") {
		t.Errorf("the report must not fabricate a USD figure\n---\n%s", got)
	}
	// A document with no budget block emits no note.
	if noBudget := reportFor(t, minimalV1); strings.Contains(noBudget, "limit_usd") {
		t.Errorf("a document with no budget stage must emit no USD note\n---\n%s", noBudget)
	}
}

// TestReport_NoApprovalGates covers the degenerate document.
func TestReport_NoApprovalGates(t *testing.T) {
	r := &EligibilityReport{}
	if got := r.Render(); !strings.Contains(got, "declares no approval gates") {
		t.Errorf("want the no-gates line, got:\n%s", got)
	}
}

// TestReport_UnnamedStage covers a gate on a stage with no readable id.
func TestReport_UnnamedStage(t *testing.T) {
	r := &EligibilityReport{Gates: []GateEligibility{{Path: "/g", Change: changeNone}}}
	if got := r.Render(); !strings.Contains(got, "(unnamed)") {
		t.Errorf("want the unnamed-stage placeholder, got:\n%s", got)
	}
}

// TestReport_NamesServerSideAuthority: the codemod validates the spec
// document, not `needs` / `from_stage` referent resolution, and the
// report says so.
func TestReport_NamesServerSideAuthority(t *testing.T) {
	if got := reportFor(t, minimalV1); !strings.Contains(got, "Server-side validation remains the authority") {
		t.Errorf("the report must state the limit of its own authority\n---\n%s", got)
	}
}
