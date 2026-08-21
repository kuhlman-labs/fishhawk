package plan_test

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
)

// --- grooming_report sibling (#2235 / E54.3) ---
//
// Every INVALID fixture below is seeded BY CONSTRUCTION — a hand-written JSON
// literal for the array under test, assembled into the shared envelope by
// groomingDoc. No test calls plan.ValidateGroomingReport in its own setup, so a
// RED lands on the behavioral assertion rather than on a fixture-setup failure
// (#2235 binding condition F5).

// groomingItemRefJSON renders one item_ref literal.
func groomingItemRefJSON(typ, id, url string) string {
	return `{"type":"` + typ + `","id":"` + id + `","url":"` + url + `"}`
}

const (
	gr2235Ref = `{"type":"github_issue","id":"kuhlman-labs/fishhawk#2235","url":"https://github.com/kuhlman-labs/fishhawk/issues/2235"}`
	gr2240Ref = `{"type":"github_issue","id":"kuhlman-labs/fishhawk#2240","url":"https://github.com/kuhlman-labs/fishhawk/issues/2240"}`
)

// groomingDoc assembles a grooming_report document from the six entry arrays,
// each supplied as a raw JSON array literal. The envelope (kind, versions,
// ticket/agent provenance, summary) is always valid, so a rejection is
// attributable to the array the case is exercising.
func groomingDoc(ordering, duplicates, hygiene, dependencies, drift, decomposition string) []byte {
	return []byte(`{
  "kind": "grooming_report",
  "report_version": "grooming_report_v1",
  "ticket_reference": {"type":"github_issue","url":"https://github.com/kuhlman-labs/fishhawk/issues/2235","id":"kuhlman-labs/fishhawk#2235"},
  "generated_by": {"agent":"claude-code","model":"claude-opus-5","timestamp":"2026-08-21T14:02:11Z"},
  "summary": "Groomed four alpha-phase items.",
  "ordering": ` + ordering + `,
  "duplicates": ` + duplicates + `,
  "hygiene_defects": ` + hygiene + `,
  "dependency_edges": ` + dependencies + `,
  "vision_drift": ` + drift + `,
  "decomposition_suggestions": ` + decomposition + `
}`)
}

// groomingMinimal is the smallest fully-valid report: one cited ordering entry
// at rank 1, every other class explicitly empty.
func groomingMinimal() []byte {
	return groomingDoc(
		`[{"id":"ordering:github/kuhlman-labs/fishhawk#2235","item_ref":`+gr2235Ref+`,"rank":1,"score":9.5,"rubric_citations":[{"rubric_id":"V1"}]}]`,
		`[]`, `[]`, `[]`, `[]`, `[]`)
}

func TestValidateGroomingReport_Minimal(t *testing.T) {
	if err := plan.ValidateGroomingReport(groomingMinimal()); err != nil {
		t.Fatalf("ValidateGroomingReport(minimal): %v", err)
	}
}

// TestValidateGroomingReport_Example round-trips the canonical example through
// BOTH the validator and the typed parser, asserting the decoded per-class
// counts and that every ordering entry decodes at least one rubric citation —
// the DONE-MEANS behavioral proof that the shipped example exercises all six
// entry classes and satisfies the citation rule.
func TestValidateGroomingReport_Example(t *testing.T) {
	body, err := os.ReadFile("../../../docs/spec/examples/grooming-report-v1-example.json")
	if err != nil {
		t.Fatalf("read canonical example: %v", err)
	}
	if err := plan.ValidateGroomingReport(body); err != nil {
		t.Fatalf("ValidateGroomingReport(example): %v", err)
	}
	gr, err := plan.ParseGroomingReport(body)
	if err != nil {
		t.Fatalf("ParseGroomingReport(example): %v", err)
	}
	if gr.Kind != plan.KindGroomingReport {
		t.Errorf("Kind = %q, want %q", gr.Kind, plan.KindGroomingReport)
	}
	if gr.ReportVersion != plan.GroomingReportVersion {
		t.Errorf("ReportVersion = %q, want %q", gr.ReportVersion, plan.GroomingReportVersion)
	}
	counts := map[string]int{
		"ordering":                  len(gr.Ordering),
		"duplicates":                len(gr.Duplicates),
		"hygiene_defects":           len(gr.HygieneDefects),
		"dependency_edges":          len(gr.DependencyEdges),
		"vision_drift":              len(gr.VisionDrift),
		"decomposition_suggestions": len(gr.DecompositionSuggestions),
	}
	want := map[string]int{
		"ordering": 4, "duplicates": 1, "hygiene_defects": 1,
		"dependency_edges": 1, "vision_drift": 1, "decomposition_suggestions": 1,
	}
	for k, w := range want {
		if counts[k] != w {
			t.Errorf("example %s count = %d, want %d", k, counts[k], w)
		}
	}
	for i, e := range gr.Ordering {
		if len(e.RubricCitations) == 0 {
			t.Errorf("example /ordering/%d decodes zero rubric citations", i)
		}
	}
	if gr.CharterRef == nil || gr.CharterRef.Path == "" {
		t.Error("example must pin a charter_ref (optional in the schema, present in the example)")
	}
}

// TestValidateGroomingReport_OrderingEntryWithoutCitation_Rejected is the
// acceptance-criterion proof: an ordering entry with no rubric citation is
// REJECTED, not merely undecorated (charter §5.1). Two SELF-PAIRED cases —
// `rubric_citations` ABSENT and `rubric_citations: []` — each reddens
// independently: the first when `required` is deleted, the second when
// `minItems: 1` is deleted.
func TestValidateGroomingReport_OrderingEntryWithoutCitation_Rejected(t *testing.T) {
	cases := []struct {
		name  string
		entry string
	}{
		{
			name:  "citations absent",
			entry: `[{"id":"ordering:github/kuhlman-labs/fishhawk#2235","item_ref":` + gr2235Ref + `,"rank":1,"score":9.5}]`,
		},
		{
			name:  "citations empty array",
			entry: `[{"id":"ordering:github/kuhlman-labs/fishhawk#2235","item_ref":` + gr2235Ref + `,"rank":1,"score":9.5,"rubric_citations":[]}]`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := groomingDoc(tc.entry, `[]`, `[]`, `[]`, `[]`, `[]`)
			var se *plan.SchemaError
			err := plan.ValidateGroomingReport(body)
			if !errors.As(err, &se) {
				t.Fatalf("ValidateGroomingReport: err = %v, want *SchemaError", err)
			}
			if !schemaErrorMentions(se, "/ordering/0/rubric_citations") {
				t.Errorf("SchemaError should name /ordering/0/rubric_citations; got %v", se)
			}
		})
	}
}

// schemaErrorMentions reports whether any violation's path names want (the
// absent case reports the violation at the parent object, so accept a prefix
// relationship in either direction).
func schemaErrorMentions(se *plan.SchemaError, want string) bool {
	if strings.Contains(se.Error(), want) {
		return true
	}
	for _, v := range se.Violations {
		if v.Path == want || strings.HasPrefix(want, v.Path) {
			return true
		}
	}
	return false
}

// TestValidateGroomingReport_UnknownField_Rejected pins the closed shape:
// deleting any additionalProperties:false reddens it.
func TestValidateGroomingReport_UnknownField_Rejected(t *testing.T) {
	body := groomingDoc(
		`[{"id":"ordering:github/kuhlman-labs/fishhawk#2235","item_ref":`+gr2235Ref+`,"rank":1,"score":9.5,"rubric_citations":[{"rubric_id":"V1"}],"weight":3}]`,
		`[]`, `[]`, `[]`, `[]`, `[]`)
	var se *plan.SchemaError
	if err := plan.ValidateGroomingReport(body); !errors.As(err, &se) {
		t.Fatalf("ValidateGroomingReport: err = %v, want *SchemaError", err)
	}
}

// TestValidateGroomingReport_DuplicateEntryID_Rejected: ids are unique across
// the WHOLE report, not per array. The two entries below are in DIFFERENT
// arrays but would share an id only if the class prefixes matched — so the case
// uses two ordering entries over the same item (which the derivation makes
// identical by construction), pairing the offending value with ITSELF.
func TestValidateGroomingReport_DuplicateEntryID_Rejected(t *testing.T) {
	body := groomingDoc(
		`[{"id":"ordering:github/kuhlman-labs/fishhawk#2235","item_ref":`+gr2235Ref+`,"rank":1,"score":9.5,"rubric_citations":[{"rubric_id":"V1"}]},
		  {"id":"ordering:github/kuhlman-labs/fishhawk#2235","item_ref":`+gr2235Ref+`,"rank":2,"score":8.0,"rubric_citations":[{"rubric_id":"V2"}]}]`,
		`[]`, `[]`, `[]`, `[]`, `[]`)
	var se *plan.SemanticError
	err := plan.ValidateGroomingReport(body)
	if !errors.As(err, &se) {
		t.Fatalf("ValidateGroomingReport: err = %v, want *SemanticError", err)
	}
	if !strings.Contains(se.Error(), "ordering:github/kuhlman-labs/fishhawk#2235") {
		t.Errorf("SemanticError should name the repeated id; got %v", se)
	}
}

// TestValidateGroomingReport_EntryIDClassMismatch_Rejected: an `ordering:`
// -prefixed id inside `duplicates` is a routing bug.
func TestValidateGroomingReport_EntryIDClassMismatch_Rejected(t *testing.T) {
	body := groomingDoc(
		`[{"id":"ordering:github/kuhlman-labs/fishhawk#2235","item_ref":`+gr2235Ref+`,"rank":1,"score":9.5,"rubric_citations":[{"rubric_id":"V1"}]}]`,
		`[{"id":"ordering:github/kuhlman-labs/fishhawk#2235+github/kuhlman-labs/fishhawk#2240","pair":[`+gr2235Ref+`,`+gr2240Ref+`],"basis":"overlap","confidence":"low"}]`,
		`[]`, `[]`, `[]`, `[]`)
	var se *plan.SemanticError
	err := plan.ValidateGroomingReport(body)
	if !errors.As(err, &se) {
		t.Fatalf("ValidateGroomingReport: err = %v, want *SemanticError", err)
	}
	// Assert the CLASS-PREFIX rule's own identity, not merely "some
	// SemanticError". The recomposition rule (c) would also reject this
	// document — an `ordering:` id can never recompose from a duplicate pair —
	// so a test satisfied by any rejection would stay green with the
	// class-prefix check deleted and pin nothing.
	if !strings.Contains(se.Error(), "must start with the class prefix") {
		t.Errorf("SemanticError should be the class-prefix rejection; got %v", se)
	}
	if !strings.Contains(se.Error(), `"duplicate:"`) {
		t.Errorf("SemanticError should name the expected class prefix; got %v", se)
	}
}

// TestValidateGroomingReport_PerRunMintedID_Rejected: an id that is a plausible
// random per-run token rather than a recomposition of its own item_ref. A
// per-run id is exactly what would break #2240's run-over-run diff.
func TestValidateGroomingReport_PerRunMintedID_Rejected(t *testing.T) {
	body := groomingDoc(
		`[{"id":"ordering:8f14e45fceea167a5a36dedd4bea2543","item_ref":`+gr2235Ref+`,"rank":1,"score":9.5,"rubric_citations":[{"rubric_id":"V1"}]}]`,
		`[]`, `[]`, `[]`, `[]`, `[]`)
	var se *plan.SemanticError
	err := plan.ValidateGroomingReport(body)
	if !errors.As(err, &se) {
		t.Fatalf("ValidateGroomingReport: err = %v, want *SemanticError", err)
	}
	if !strings.Contains(se.Error(), "ordering:github/kuhlman-labs/fishhawk#2235") {
		t.Errorf("SemanticError should name the expected derived id; got %v", se)
	}
}

// TestValidateGroomingReport_DuplicateRank_Rejected and _GappedRank_Rejected
// pin the permutation rule the schema's per-entry minimum:1 cannot express.
func TestValidateGroomingReport_DuplicateRank_Rejected(t *testing.T) {
	body := groomingDoc(
		`[{"id":"ordering:github/kuhlman-labs/fishhawk#2235","item_ref":`+gr2235Ref+`,"rank":1,"score":9.5,"rubric_citations":[{"rubric_id":"V1"}]},
		  {"id":"ordering:github/kuhlman-labs/fishhawk#2240","item_ref":`+gr2240Ref+`,"rank":1,"score":8.0,"rubric_citations":[{"rubric_id":"V2"}]}]`,
		`[]`, `[]`, `[]`, `[]`, `[]`)
	var se *plan.SemanticError
	err := plan.ValidateGroomingReport(body)
	if !errors.As(err, &se) {
		t.Fatalf("ValidateGroomingReport: err = %v, want *SemanticError", err)
	}
	if !strings.Contains(se.Error(), "duplicate rank 1") {
		t.Errorf("SemanticError should name the duplicated rank; got %v", se)
	}
}

func TestValidateGroomingReport_GappedRank_Rejected(t *testing.T) {
	body := groomingDoc(
		`[{"id":"ordering:github/kuhlman-labs/fishhawk#2235","item_ref":`+gr2235Ref+`,"rank":1,"score":9.5,"rubric_citations":[{"rubric_id":"V1"}]},
		  {"id":"ordering:github/kuhlman-labs/fishhawk#2240","item_ref":`+gr2240Ref+`,"rank":3,"score":8.0,"rubric_citations":[{"rubric_id":"V2"}]}]`,
		`[]`, `[]`, `[]`, `[]`, `[]`)
	var se *plan.SemanticError
	err := plan.ValidateGroomingReport(body)
	if !errors.As(err, &se) {
		t.Fatalf("ValidateGroomingReport: err = %v, want *SemanticError", err)
	}
	if !strings.Contains(se.Error(), "rank 2 is missing") {
		t.Errorf("SemanticError should name the missing rank; got %v", se)
	}
}

// TestValidateGroomingReport_SelfReferentialPair_Rejected and
// _SelfReferentialEdge_Rejected pin the distinctness rule. Both fixtures are
// self-paired BY CONSTRUCTION — the same item on both endpoints, and each id is
// the CORRECT derivation of that degenerate pair/edge — so every other semantic
// rule accepts them: the class prefix matches, the id recomposes, it is unique,
// and no ordering rank is touched. Each therefore reddens only on the
// distinctness rule, and each asserts the rule's own message rather than merely
// "some *SemanticError".
func TestValidateGroomingReport_SelfReferentialPair_Rejected(t *testing.T) {
	body := groomingDoc(
		`[{"id":"ordering:github/kuhlman-labs/fishhawk#2235","item_ref":`+gr2235Ref+`,"rank":1,"score":9.5,"rubric_citations":[{"rubric_id":"V1"}]}]`,
		`[{"id":"duplicate:github/kuhlman-labs/fishhawk#2235+github/kuhlman-labs/fishhawk#2235","pair":[`+gr2235Ref+`,`+gr2235Ref+`],"basis":"identical title","confidence":"high"}]`,
		`[]`, `[]`, `[]`, `[]`)
	var se *plan.SemanticError
	err := plan.ValidateGroomingReport(body)
	if !errors.As(err, &se) {
		t.Fatalf("ValidateGroomingReport: err = %v, want *SemanticError", err)
	}
	if !strings.Contains(se.Error(), "must relate two DISTINCT items") {
		t.Errorf("SemanticError should be the distinctness rejection; got %v", se)
	}
	if !strings.Contains(se.Error(), "/duplicates/0/pair") {
		t.Errorf("SemanticError should name the offending entry; got %v", se)
	}
}

func TestValidateGroomingReport_SelfReferentialEdge_Rejected(t *testing.T) {
	body := groomingDoc(
		`[{"id":"ordering:github/kuhlman-labs/fishhawk#2235","item_ref":`+gr2235Ref+`,"rank":1,"score":9.5,"rubric_citations":[{"rubric_id":"V1"}]}]`,
		`[]`, `[]`,
		`[{"id":"dependency:github/kuhlman-labs/fishhawk#2235+github/kuhlman-labs/fishhawk#2235","from":`+gr2235Ref+`,"to":`+gr2235Ref+`,"basis":"self","kind":"depends_on"}]`,
		`[]`, `[]`)
	var se *plan.SemanticError
	err := plan.ValidateGroomingReport(body)
	if !errors.As(err, &se) {
		t.Fatalf("ValidateGroomingReport: err = %v, want *SemanticError", err)
	}
	if !strings.Contains(se.Error(), "must relate two DISTINCT items") {
		t.Errorf("SemanticError should be the distinctness rejection; got %v", se)
	}
	if !strings.Contains(se.Error(), "/dependency_edges/0") {
		t.Errorf("SemanticError should name the offending entry; got %v", se)
	}
}

// TestValidateGroomingReport_DistinctEndpointsAccepted is the distinctness
// rule's positive control: the same two classes with DIFFERENT endpoints stay
// valid, so the rule rejects the degenerate case and nothing wider.
func TestValidateGroomingReport_DistinctEndpointsAccepted(t *testing.T) {
	body := groomingDoc(
		`[{"id":"ordering:github/kuhlman-labs/fishhawk#2235","item_ref":`+gr2235Ref+`,"rank":1,"score":9.5,"rubric_citations":[{"rubric_id":"V1"}]}]`,
		`[{"id":"duplicate:github/kuhlman-labs/fishhawk#2235+github/kuhlman-labs/fishhawk#2240","pair":[`+gr2235Ref+`,`+gr2240Ref+`],"basis":"overlap","confidence":"low"}]`,
		`[]`,
		`[{"id":"dependency:github/kuhlman-labs/fishhawk#2240+github/kuhlman-labs/fishhawk#2235","from":`+gr2240Ref+`,"to":`+gr2235Ref+`,"basis":"needs the schema first","kind":"depends_on"}]`,
		`[]`, `[]`)
	if err := plan.ValidateGroomingReport(body); err != nil {
		t.Fatalf("ValidateGroomingReport(distinct endpoints): err = %v, want nil", err)
	}
}

// TestGroomingEntryID_DependencyDirectionPreserved is condition F1's proof: a
// depends_on edge is DIRECTIONAL, so A→B and B→A derive DIFFERENT ids and
// coexist in one report without tripping report-wide id uniqueness. A duplicate
// PAIR, by contrast, is UNORDERED and sorts to one orientation-stable id.
func TestGroomingEntryID_DependencyDirectionPreserved(t *testing.T) {
	a := plan.ItemRef{Type: "github_issue", ID: "kuhlman-labs/fishhawk#2240", URL: "https://example.test/a"}
	b := plan.ItemRef{Type: "github_issue", ID: "kuhlman-labs/fishhawk#2235", URL: "https://example.test/b"}

	ab := plan.GroomingEntryID(plan.GroomingClassDependency, "", a, b)
	ba := plan.GroomingEntryID(plan.GroomingClassDependency, "", b, a)
	if ab == ba {
		t.Fatalf("dependency ids must encode direction: A→B and B→A both derived %q", ab)
	}
	if want := "dependency:github/kuhlman-labs/fishhawk#2240+github/kuhlman-labs/fishhawk#2235"; ab != want {
		t.Errorf("A→B id = %q, want %q (from-key then to-key, unsorted)", ab, want)
	}

	// A duplicate pair IS unordered: both orientations derive one id.
	dupAB := plan.GroomingEntryID(plan.GroomingClassDuplicate, "", a, b)
	dupBA := plan.GroomingEntryID(plan.GroomingClassDuplicate, "", b, a)
	if dupAB != dupBA {
		t.Errorf("duplicate ids must be orientation-stable: %q vs %q", dupAB, dupBA)
	}

	// Both directions coexist in ONE report without tripping uniqueness.
	body := groomingDoc(
		`[{"id":"ordering:github/kuhlman-labs/fishhawk#2235","item_ref":`+gr2235Ref+`,"rank":1,"score":9.5,"rubric_citations":[{"rubric_id":"V1"}]}]`,
		`[]`, `[]`,
		`[{"id":"`+ab+`","from":`+gr2240Ref+`,"to":`+gr2235Ref+`,"basis":"forward","kind":"depends_on"},
		  {"id":"`+ba+`","from":`+gr2235Ref+`,"to":`+gr2240Ref+`,"basis":"reverse","kind":"depends_on"}]`,
		`[]`, `[]`)
	if err := plan.ValidateGroomingReport(body); err != nil {
		t.Fatalf("both edge directions must coexist in one report: %v", err)
	}
}

// TestGroomingEntryID_SeparatorCollisionFree pins the id derivation as
// COLLISION-FREE for the pair/edge classes, whose ids are a flat string join of
// two item keys. The pair below is the concrete counterexample from the fix-up
// review: with an unescaped join, {"a", "b+github/c"} and {"a+github/b", "c"}
// both derive `duplicate:github/a+github/b+github/c` — two DIFFERENT proposals
// under ONE id, which would make report-wide uniqueness spuriously reject a
// report carrying both and let #2240's run-over-run diff conflate them.
//
// Counterfactual vehicle: deleting itemKeyEscaper (returning the raw lowercased
// key) makes the two ids EQUAL and reddens the first assertion.
func TestGroomingEntryID_SeparatorCollisionFree(t *testing.T) {
	ref := func(id string) plan.ItemRef {
		return plan.ItemRef{Type: "github_issue", ID: id, URL: "https://example.test/" + id}
	}

	// The two pairs are DISTINCT proposals; their ids must differ.
	p1 := plan.GroomingEntryID(plan.GroomingClassDuplicate, "", ref("a"), ref("b+github/c"))
	p2 := plan.GroomingEntryID(plan.GroomingClassDuplicate, "", ref("a+github/b"), ref("c"))
	if p1 == p2 {
		t.Fatalf("distinct duplicate pairs collided on one id: %q", p1)
	}
	// Same property for the directional edge id.
	e1 := plan.GroomingEntryID(plan.GroomingClassDependency, "", ref("a"), ref("b+github/c"))
	e2 := plan.GroomingEntryID(plan.GroomingClassDependency, "", ref("a+github/b"), ref("c"))
	if e1 == e2 {
		t.Fatalf("distinct dependency edges collided on one id: %q", e1)
	}
	// The ':' class/qualifier separator is escaped for the same reason.
	q1 := plan.GroomingEntryID(plan.GroomingClassHygiene, "b", ref("a:x"))
	q2 := plan.GroomingEntryID(plan.GroomingClassHygiene, "x:b", ref("a"))
	if q1 == q2 {
		t.Fatalf("distinct hygiene entries collided on one id: %q", q1)
	}
	// Escaping is a NO-OP for every schema-valid id, so published ids are
	// unchanged by this layer.
	if got, want := plan.GroomingEntryID(plan.GroomingClassOrdering, "", ref("kuhlman-labs/fishhawk#2235")),
		"ordering:github/kuhlman-labs/fishhawk#2235"; got != want {
		t.Errorf("derived id for a schema-valid item id = %q, want %q (escaping must be a no-op here)", got, want)
	}
}

// TestGroomingReportSchema_ItemRefIDExcludesSeparators is the INPUT-BOUNDARY
// layer of the same property: the canonical schema's item_ref.id character class
// must not admit either entry-id separator, so a report can never carry an id
// whose flat join is ambiguous.
//
// The assertion is on the ERROR IDENTITY (*SchemaError naming /item_ref/id), not
// merely on "rejected": both layers reject a '+'-bearing id, so re-admitting '+'
// to the character class moves the rejection to the recomposition rule
// (*SemanticError) and reddens the errors.As. Validation is pure — there is no
// committed state to read instead.
func TestGroomingReportSchema_ItemRefIDExcludesSeparators(t *testing.T) {
	for _, tc := range []struct{ name, id string }{
		{"plus is the pair/edge key separator", "acme/repo#1+github/other#2"},
		{"colon is the class/qualifier separator", "acme/repo#1:2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			itemRef := groomingItemRefJSON("github_issue", tc.id, "https://example.test/x")
			// The declared id is the plain lowercased join a naive derivation
			// would produce, hand-written by construction.
			body := groomingDoc(
				`[{"id":"ordering:github/`+strings.ToLower(tc.id)+`","item_ref":`+itemRef+`,"rank":1,"score":9.5,"rubric_citations":[{"rubric_id":"V1"}]}]`,
				`[]`, `[]`, `[]`, `[]`, `[]`)
			var se *plan.SchemaError
			err := plan.ValidateGroomingReport(body)
			if !errors.As(err, &se) {
				t.Fatalf("ValidateGroomingReport: err = %v (%T), want *SchemaError from the item_ref.id pattern", err, err)
			}
			if !schemaErrorMentions(se, "/ordering/0/item_ref/id") {
				t.Errorf("SchemaError should name /ordering/0/item_ref/id; got %v", se)
			}
		})
	}
}

// TestGroomingEntryID_ProviderDiscriminated is condition F3's proof: every
// item_ref.type the schema admits has a defined derivation, and identical
// textual coordinates on two forges do NOT collide on one key.
func TestGroomingEntryID_ProviderDiscriminated(t *testing.T) {
	gh := plan.ItemRef{Type: "github_issue", ID: "acme/platform#17", URL: "https://github.test/x"}
	gl := plan.ItemRef{Type: "gitlab_issue", ID: "acme/platform#17", URL: "https://gitlab.test/x"}
	jira := plan.ItemRef{Type: "jira_issue", ID: "PLAT-412", URL: "https://jira.test/x"}

	ghID := plan.GroomingEntryID(plan.GroomingClassOrdering, "", gh)
	glID := plan.GroomingEntryID(plan.GroomingClassOrdering, "", gl)
	if ghID == glID {
		t.Fatalf("identical coordinates on two forges must not collide; both derived %q", ghID)
	}
	for _, tc := range []struct{ got, want string }{
		{ghID, "ordering:github/acme/platform#17"},
		{glID, "ordering:gitlab/acme/platform#17"},
		{plan.GroomingEntryID(plan.GroomingClassOrdering, "", jira), "ordering:jira/plat-412"},
	} {
		if tc.got != tc.want {
			t.Errorf("derived id = %q, want %q", tc.got, tc.want)
		}
	}

	// Every type the SCHEMA admits must be derivable — a schema that accepts
	// input its own contract cannot process is the defect F3 forbids.
	for _, typ := range schemaItemRefTypes(t) {
		id := plan.GroomingEntryID(plan.GroomingClassOrdering, "",
			plan.ItemRef{Type: typ, ID: "x/y#1", URL: "https://example.test/1"})
		if strings.Contains(id, "unsupported-item-ref-type") {
			t.Errorf("schema admits item_ref.type %q but GroomingEntryID cannot derive a key for it (got %q)", typ, id)
		}
	}
}

// schemaItemRefTypes reads the item-ref type enum straight out of the canonical
// schema, so widening the enum without adding a derivation case fails here.
func schemaItemRefTypes(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile("../../../docs/spec/grooming-report-v1.schema.json")
	if err != nil {
		t.Fatalf("read canonical schema: %v", err)
	}
	var doc struct {
		Defs struct {
			ItemRef struct {
				Properties struct {
					Type struct {
						Enum []string `json:"enum"`
					} `json:"type"`
				} `json:"properties"`
			} `json:"item-ref"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode canonical schema: %v", err)
	}
	got := doc.Defs.ItemRef.Properties.Type.Enum
	if len(got) == 0 {
		t.Fatal("canonical schema declares no item-ref type enum")
	}
	return got
}

// TestGroomingEntryID_QualifierLowercased is condition F4's proof: the
// derivation lowercases the qualifier so it matches the schema's lowercase-only
// qualifier segment, and the recomposition check compares lowercased. An
// uppercase charter id therefore recomposes to the same id.
func TestGroomingEntryID_QualifierLowercased(t *testing.T) {
	ref := plan.ItemRef{Type: "github_issue", ID: "kuhlman-labs/fishhawk#2235", URL: "https://example.test/x"}
	got := plan.GroomingEntryID(plan.GroomingClassVisionDrift, "NG2", ref)
	if want := "vision_drift:github/kuhlman-labs/fishhawk#2235:ng2"; got != want {
		t.Fatalf("qualifier must be lowercased: got %q, want %q", got, want)
	}
	// End to end: an UPPERCASE charter_ref_id with the lowercased id validates.
	body := groomingDoc(
		`[{"id":"ordering:github/kuhlman-labs/fishhawk#2235","item_ref":`+gr2235Ref+`,"rank":1,"score":9.5,"rubric_citations":[{"rubric_id":"V1"}]}]`,
		`[]`, `[]`, `[]`,
		`[{"id":"`+got+`","item_ref":`+gr2235Ref+`,"basis":"non_goal","charter_ref_id":"NG2","detail":"drifts"}]`,
		`[]`)
	if err := plan.ValidateGroomingReport(body); err != nil {
		t.Fatalf("uppercase charter_ref_id must recompose to the lowercased qualifier: %v", err)
	}
}

// TestDetectArtifactKind_GroomingReport pins the discriminator routing: the new
// sibling is detected, and a plan (no "kind") still defaults to the plan kind.
func TestDetectArtifactKind_GroomingReport(t *testing.T) {
	k, err := plan.DetectArtifactKind(groomingMinimal())
	if err != nil {
		t.Fatalf("DetectArtifactKind: %v", err)
	}
	if k != plan.ArtifactKindGroomingReport {
		t.Fatalf("kind = %q, want %q", k, plan.ArtifactKindGroomingReport)
	}
	// ValidateArtifact must route to the grooming validator, not the frozen
	// plan schema (which would reject these bytes with a different error).
	if err := plan.ValidateArtifact(groomingMinimal()); err != nil {
		t.Errorf("ValidateArtifact(grooming_report): %v", err)
	}
}

func TestValidateGroomingReport_ParseErrors(t *testing.T) {
	var pe *plan.ParseError
	if err := plan.ValidateGroomingReport(nil); !errors.As(err, &pe) {
		t.Errorf("empty: err = %v, want *ParseError", err)
	}
	if err := plan.ValidateGroomingReport([]byte("{not json")); !errors.As(err, &pe) {
		t.Errorf("non-JSON: err = %v, want *ParseError", err)
	}
	if _, err := plan.ParseGroomingReport([]byte("{not json")); !errors.As(err, &pe) {
		t.Errorf("ParseGroomingReport non-JSON: err = %v, want *ParseError", err)
	}
}

// TestEmbeddedGroomingReportSchemaHash_MatchesCanonical proves the embedded
// mirror the hash is computed over is the canonical schema (the sync-schemas
// contract), and that the hash differs from the plan schema's.
func TestEmbeddedGroomingReportSchemaHash_MatchesCanonical(t *testing.T) {
	h := plan.EmbeddedGroomingReportSchemaHash()
	if len(h) != 64 {
		t.Fatalf("EmbeddedGroomingReportSchemaHash() = %q, want 64 hex chars", h)
	}
	if h == plan.EmbeddedSchemaHash() {
		t.Error("grooming-report schema hash must differ from the plan schema hash")
	}
	canonical, err := os.ReadFile("../../../docs/spec/grooming-report-v1.schema.json")
	if err != nil {
		t.Fatalf("read canonical schema: %v", err)
	}
	mirror, err := os.ReadFile("schemas/grooming-report-v1.schema.json")
	if err != nil {
		t.Fatalf("read embedded mirror: %v", err)
	}
	if string(canonical) != string(mirror) {
		t.Error("embedded grooming-report schema drifted from docs/spec/ — run scripts/sync-schemas")
	}
}
