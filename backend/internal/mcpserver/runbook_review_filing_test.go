package mcpserver

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// reviewFilingServedSchemaDescription walks the SERVED tools/list schema of
// toolName (its outputSchema when output is true, else its inputSchema) down
// the property path and returns the leaf's description. A "[]" segment
// descends into an array's items; a local "$ref": "#/$defs/<Name>" is followed
// at every node. It reads the schema an agent receives, not the Go struct tag,
// so it reddens when the registered tool stops serving the field (#5216).
func reviewFilingServedSchemaDescription(t *testing.T, tools map[string]*mcp.Tool, toolName string, output bool, path ...string) string {
	t.Helper()
	tool, ok := tools[toolName]
	if !ok {
		t.Fatalf("%s not registered over ListTools", toolName)
	}
	served := tool.InputSchema
	if output {
		served = tool.OutputSchema
	}
	if served == nil {
		t.Fatalf("%s: no schema on the wire (output=%v)", toolName, output)
	}
	raw, err := json.Marshal(served)
	if err != nil {
		t.Fatalf("%s: marshal schema: %v", toolName, err)
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatalf("%s: decode schema: %v", toolName, err)
	}
	defs, _ := root["$defs"].(map[string]any)
	resolve := func(node map[string]any) map[string]any {
		ref, _ := node["$ref"].(string)
		if ref == "" {
			return node
		}
		def, _ := defs[strings.TrimPrefix(ref, "#/$defs/")].(map[string]any)
		if def == nil {
			t.Fatalf("%s: unresolvable $ref %q in served schema", toolName, ref)
		}
		return def
	}
	node := resolve(root)
	for _, seg := range path {
		var next map[string]any
		if seg == "[]" {
			next, _ = node["items"].(map[string]any)
		} else {
			props, _ := node["properties"].(map[string]any)
			next, _ = props[seg].(map[string]any)
		}
		if next == nil {
			t.Fatalf("%s: served schema has no %q along %v", toolName, seg, path)
		}
		node = resolve(next)
	}
	desc, _ := node["description"].(string)
	if desc == "" {
		t.Fatalf("%s: served schema property %v carries no description", toolName, path)
	}
	return desc
}

// reviewFilingRunbookHeading is the exact heading the #5216 review, filing and
// grooming judgement section ships under.
const reviewFilingRunbookHeading = "### Review arbitration, filing and grooming judgement"

// reviewFilingRunbookSection slices the EMBEDDED runbook from the #5216
// heading to the next same-level heading. A missing heading is a t.Fatal
// rather than an empty slice, so the presence assertions below cannot pass
// vacuously.
func reviewFilingRunbookSection(t *testing.T) string {
	t.Helper()
	start := strings.Index(runbookMarkdown, reviewFilingRunbookHeading)
	if start < 0 {
		t.Fatalf("runbook.md is missing the %q section heading — the #5216 judgement section is absent or its heading was renamed", reviewFilingRunbookHeading)
	}
	rest := runbookMarkdown[start+len(reviewFilingRunbookHeading):]
	if end := strings.Index(rest, "\n### "); end >= 0 {
		return rest[:end]
	}
	return rest
}

// TestRunbook_ReviewFilingSection_StatesJudgementRules pins the #4745
// inventory rules #5216 folds into the runbook as operator judgement, one or
// more anchors per rule, asserted on the embedded copy the MCP runbook resource
// serves. Anchors match the flattened section (markup stripped, lowercased,
// whitespace folded) so a re-wrap does not redden them.
func TestRunbook_ReviewFilingSection_StatesJudgementRules(t *testing.T) {
	flat := flattenSection(reviewFilingRunbookSection(t))
	for _, c := range []struct {
		rule   string
		anchor string
	}{
		{"R11/R16 split on the tree", "adjudicate a split on the tree, not on the prose"},
		{"R11/R16 check the rejecting claim", "check the rejecting reviewer's claim against the tree"},
		{"R19 grep the head", "grep the run's head (merge_candidate_verdict.head_sha on the gate view)"},
		{"R18 failed is not a verdict", "a failed review is not a verdict"},
		{"R18 never counted", "never count it as an approve or a reject"},
		{"R18 rerun eligibility (W1)", "fishhawk_rerun_review re-runs only an implement-review round of advisory authority that recorded zero verdicts"},
		{"R18 plan failure route (W1)", "a failed plan review is answered by fishhawk_revise_plan or by your approve or reject decision itself"},
		{"R18 gating or partial round (W1)", "for a gating review, or a round where another reviewer succeeded, take the legal next step"},
		{"R18 no quota label promised (W5d)", "read the failure reason for the cause"},
		{"R12 all-red battery", "an all-red counterfactual battery proves only the controls it mutates"},
		{"R12 cross-entity cases", "list the cross-entity eligibility cases separately (parent and child, run and stage, scan and apply"},
		{"R6 defer non-blocking", "defer a real but non-blocking hardening"},
		{"R6 verbatim reason", "its reason is shown verbatim to every later re-review of the stage, so make it self-contained"},
		{"R17 false-positive shape", "a false-positive reason starts with false_positive:, gives the evidence that disproves the finding"},
		{"R17 authorization", "names who authorized the waive when you act on delegated authority"},
		{"R14 search by symbol", "search the tracker yourself by a code symbol or path from the change"},
		{"R14 charter degrade keeps candidates (W2)", "a charter-side degrade_reason (charter_undeclared, charter_unresolved, charter_rubric_unparsed or seam_unwired: the charter or its rubric could not be read) leaves the duplicate candidates valid"},
		{"R14 incomplete scan (W2)", "reader_unavailable or reader_error means the scan did not complete"},
		{"R14 budget or panic keyed on scanned_items (W2)", "budget_exceeded or hook_panic when scanned_items is 0 (either can also come from the charter read, which leaves the scan intact)"},
		{"R15 campaign items", "the scan does not read a running campaign's item list"},
		{"R15 campaign status", "fishhawk_get_campaign_status"},
		{"R5 decision records", "search the existing decision records and epics by topic"},
		{"R7 successor epic", "create the successor epic before the parent fills"},
		{"R7 actual count (W3)", "the child cap counts the parent's actual sub-issues and is refused before anything is created"},
		{"R7 ordinal is not capacity (W3)", "so it is not the parent's child count"},
		{"R3 per-child tier", "tier each epic child on its own"},
		{"R3 no blanket", "do not blanket every child at the most sensitive member's tier"},
		{"R13 namespace default", "is a namespace default, not a triage decision"},
		{"R1 relabel by hand", "relabel refused tier proposals by hand"},
		{"R1 applied nothing", "applied nothing, not even the clerical labels beside the tier"},
		{"R20 budget tail resurfaces", "resurfaces on the next grooming run, so approve that run"},
		{"R20 plan re-approval (W4)", "do not re-approve this run's plan gate for either: a plan re-approval launches a fresh apply"},
		{"R20 confirm gate unaffected (W4)", "attesting the confirm gate (approve_review_gate) is unaffected and is the legal next step"},
	} {
		if !strings.Contains(flat, c.anchor) {
			t.Errorf("review/filing runbook section missing %s anchor %q", c.rule, c.anchor)
		}
	}
}

// TestRunbook_ReviewFilingSection_NoBlanketRerun pins approval condition W1:
// the section never prescribes fishhawk_rerun_review as the general remedy for
// a failed review. Every mention of the verb must sit in the sentence that
// states its narrow eligibility.
func TestRunbook_ReviewFilingSection_NoBlanketRerun(t *testing.T) {
	flat := flattenSection(reviewFilingRunbookSection(t))
	const eligibility = "fishhawk_rerun_review re-runs only an implement-review round of advisory authority that recorded zero verdicts"
	if n, m := strings.Count(flat, "fishhawk_rerun_review"), strings.Count(flat, eligibility); n != m {
		t.Errorf("fishhawk_rerun_review appears %d time(s) but only %d inside its eligibility statement — a failed plan or gating review must not be offered a re-run", n, m)
	}
	for _, banned := range []string{"re-run the round with fishhawk_rerun_review", "rerun the round", "once the provider recovers"} {
		if strings.Contains(flat, banned) {
			t.Errorf("section prescribes a blanket re-run: %q", banned)
		}
	}
}

// TestRunbook_GroomingSection_AnchorRecheck pins #5216 R2 in the grooming loop
// section: the anchor body is the request, so it is re-read before each start
// and whenever the charter is amended.
func TestRunbook_GroomingSection_AnchorRecheck(t *testing.T) {
	flat := flattenSection(groomingSection(t))
	for _, anchor := range []string{
		"a stale anchor body steers the groom wrong",
		"re-read it before each start and whenever the charter is amended",
	} {
		if !strings.Contains(flat, anchor) {
			t.Errorf("grooming runbook section missing R2 anchor %q", anchor)
		}
	}
}

// TestRunbook_ReviewFilingSectionIsRepoAgnostic pins ADR-110 for the section:
// the runbook ships to every connecting repository, so the judgement names
// product mechanisms only. Section-scoped, because the rest of runbook.md
// legitimately links this repository's issues.
func TestRunbook_ReviewFilingSectionIsRepoAgnostic(t *testing.T) {
	section := reviewFilingRunbookSection(t)
	for _, banned := range []string{
		"kuhlman-labs",
		"https://github.com/",
		"scripts/",
		"backend/internal/",
		"#",
		"autonomy:low",
		"METHODOLOGY",
	} {
		if strings.Contains(section, banned) {
			t.Errorf("review/filing runbook section contains repository-specific string %q; describe the product mechanism instead", banned)
		}
	}
}

// TestGroomingApplyStatus_ServedSchemaNamesPlanGateNotConfirmGate pins
// approval condition W4 on the SERVED fishhawk_get_run_status outputSchema:
// both grooming_apply_status counts that warn against re-approving name the
// PLAN gate re-approval that launches a fresh apply, and say attesting the
// confirm gate (approve_review_gate) is unaffected and is the legal next step.
// A bare "re-approving does not apply them" leaves the gate ambiguous.
func TestGroomingApplyStatus_ServedSchemaNamesPlanGateNotConfirmGate(t *testing.T) {
	tools := fixupGuidanceTools(t)
	for _, field := range []string{"tier_not_authorized", "budget_exhausted"} {
		desc := fixupGuidanceFlatten(reviewFilingServedSchemaDescription(t, tools, "fishhawk_get_run_status", true, "grooming_apply_status", field))
		for _, want := range []string{
			"do not re-approve this run's plan gate",
			"a plan re-approval launches a fresh apply",
			"attesting this run's confirm gate (approve_review_gate) is unaffected",
			"legal next step",
		} {
			if !strings.Contains(desc, want) {
				t.Errorf("W4: served grooming_apply_status.%s schema lacks %q:\n%s", field, want, desc)
			}
		}
		reviewFilingAssertRepoAgnostic(t, "grooming_apply_status."+field+" schema", desc)
	}
}
