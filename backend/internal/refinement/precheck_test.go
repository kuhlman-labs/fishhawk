package refinement

import (
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
)

// childFinding returns the first finding of the given rule for the child at the
// given 1-based ordinal, or nil.
func childFinding(pc CriteriaPrecheck, ordinal int, rule string) *plan.AcceptanceFinding {
	for i := range pc.Children {
		if pc.Children[i].Ordinal != ordinal {
			continue
		}
		for j := range pc.Children[i].Findings {
			if pc.Children[i].Findings[j].Rule == rule {
				return &pc.Children[i].Findings[j]
			}
		}
	}
	return nil
}

func childCheck(pc CriteriaPrecheck, ordinal int) *ChildCriteriaCheck {
	for i := range pc.Children {
		if pc.Children[i].Ordinal == ordinal {
			return &pc.Children[i]
		}
	}
	return nil
}

// (intake mode: no_blocking_criterion) A zero-criteria child with an EMPTY epic
// out_of_scope fires no_blocking_criterion, sets the child AND draft
// needs_attention, and names the child by ordinal.
func TestEvaluateDraftCriteria_NoCriteriaEmptyOutOfScope_Flags(t *testing.T) {
	d := EpicDraft{
		Epic: EpicSpec{Summary: "e", Scope: "s", OutOfScope: "  "},
		Children: []ChildDraft{
			{Summary: "c1", Proposal: "p1", AcceptanceCriteria: []string{"it works"}},
			{Summary: "c2", Proposal: "p2", AcceptanceCriteria: nil},
		},
	}
	pc := EvaluateDraftCriteria(d)
	if !pc.NeedsAttention {
		t.Fatal("draft-level NeedsAttention must be true when a child is flagged")
	}
	// child 1 has a criterion (blocking by default) -> clean.
	if c := childCheck(pc, 1); c == nil || c.NeedsAttention {
		t.Fatalf("child 1 should be clean; got %+v", c)
	}
	// child 2 has no criteria and no out_of_scope justification -> flagged.
	if childFinding(pc, 2, plan.RuleNoBlockingCriterion) == nil {
		t.Fatalf("child 2 must fire no_blocking_criterion; got %+v", pc.Children)
	}
	if c := childCheck(pc, 2); c == nil || !c.NeedsAttention {
		t.Fatalf("child 2 NeedsAttention must be true; got %+v", c)
	}
}

// (intake mode: out_of_scope suppression) A zero-criteria child is CLEAN when
// the epic carries a non-empty out_of_scope — the justified escape hatch, which
// is epic-granular (children carry no out_of_scope of their own at draft time).
func TestEvaluateDraftCriteria_NoCriteriaWithOutOfScope_Clean(t *testing.T) {
	d := EpicDraft{
		Epic: EpicSpec{Summary: "e", Scope: "s", OutOfScope: "perf tuning deferred"},
		Children: []ChildDraft{
			{Summary: "c1", Proposal: "p1", AcceptanceCriteria: nil},
		},
	}
	pc := EvaluateDraftCriteria(d)
	if pc.NeedsAttention {
		t.Fatalf("a non-empty epic out_of_scope must suppress no_blocking_criterion; got %+v", pc)
	}
	if childFinding(pc, 1, plan.RuleNoBlockingCriterion) != nil {
		t.Fatalf("child 1 must be clean under the out_of_scope escape hatch; got %+v", pc.Children)
	}
	if c := childCheck(pc, 1); c == nil || len(c.Findings) != 0 {
		t.Fatalf("child 1 findings must be empty; got %+v", c)
	}
}

// (intake mode: empty_id) A whitespace-only criterion maps to an empty
// synthetic id and fires empty_id. It does NOT set needs_attention (only
// no_blocking_criterion does) — but the finding renders.
func TestEvaluateDraftCriteria_WhitespaceCriterion_EmptyID(t *testing.T) {
	d := EpicDraft{
		Epic: EpicSpec{Summary: "e", Scope: "s", OutOfScope: "x"},
		Children: []ChildDraft{
			{Summary: "c1", Proposal: "p1", AcceptanceCriteria: []string{"   "}},
		},
	}
	pc := EvaluateDraftCriteria(d)
	if childFinding(pc, 1, plan.RuleEmptyID) == nil {
		t.Fatalf("a whitespace-only criterion must fire empty_id; got %+v", pc.Children)
	}
	// empty_id alone does not flip needs_attention (the epic out_of_scope also
	// suppresses no_blocking_criterion here).
	if pc.NeedsAttention {
		t.Errorf("empty_id must not set needs_attention; got %+v", pc)
	}
}

// (intake mode: duplicate_id) Two textually-identical criteria map to the same
// synthetic id and fire duplicate_id.
func TestEvaluateDraftCriteria_DuplicateCriterionText_DuplicateID(t *testing.T) {
	d := EpicDraft{
		Epic: EpicSpec{Summary: "e", Scope: "s", OutOfScope: "x"},
		Children: []ChildDraft{
			{Summary: "c1", Proposal: "p1", AcceptanceCriteria: []string{"same", "same"}},
		},
	}
	pc := EvaluateDraftCriteria(d)
	f := childFinding(pc, 1, plan.RuleDuplicateID)
	if f == nil {
		t.Fatalf("two identical criteria must fire duplicate_id; got %+v", pc.Children)
	}
	if f.CriterionID != "same" {
		t.Errorf("duplicate_id CriterionID = %q, want same", f.CriterionID)
	}
}

// (negative) The provenance rules missing_source_ref / missing_rationale can
// NOT fire on the intake mapping by construction (Source is left empty), even
// though the SAME evaluator runs — pinning the intake-shape invariant so a
// future per-child provenance field is a deliberate change, not an accident.
func TestEvaluateDraftCriteria_ProvenanceRulesNeverFire(t *testing.T) {
	d := EpicDraft{
		Epic: EpicSpec{Summary: "e", Scope: "s", OutOfScope: ""},
		Children: []ChildDraft{
			{Summary: "c1", Proposal: "p1", AcceptanceCriteria: []string{"a", "b"}},
			{Summary: "c2", Proposal: "p2", AcceptanceCriteria: nil},
		},
	}
	pc := EvaluateDraftCriteria(d)
	for _, ord := range []int{1, 2} {
		if childFinding(pc, ord, plan.RuleMissingSourceRef) != nil {
			t.Errorf("child %d: missing_source_ref must never fire at intake", ord)
		}
		if childFinding(pc, ord, plan.RuleMissingRationale) != nil {
			t.Errorf("child %d: missing_rationale must never fire at intake", ord)
		}
	}
}

// (clean contract) Every child's Findings is non-nil ([] not null) so a reader
// can distinguish checked-and-clean from never-checked.
func TestEvaluateDraftCriteria_FindingsNonNil(t *testing.T) {
	d := EpicDraft{
		Epic: EpicSpec{Summary: "e", Scope: "s", OutOfScope: "x"},
		Children: []ChildDraft{
			{Summary: "c1", Proposal: "p1", AcceptanceCriteria: []string{"it works"}},
		},
	}
	pc := EvaluateDraftCriteria(d)
	if pc.Children == nil {
		t.Fatal("Children must be non-nil")
	}
	if pc.Children[0].Findings == nil {
		t.Fatal("a clean child's Findings must be [] not null")
	}
}

// (#2845 E54.31, shared rule set) A drafted criterion carrying the #2833 true
// positive surfaces missing_live_validation_marker at INTAKE. This proves the
// single-evaluator claim empirically rather than by assertion: precheck.go needs
// NO edit for this rule to reach intake — it passes plan.EvaluateAcceptanceCriteria's
// findings through wholesale and only special-cases RuleNoBlockingCriterion — so
// a rule added to the shared set applies to intake and the plan gate at once.
func TestEvaluateDraftCriteria_MissingLiveValidationMarker_SurfacedAtIntake(t *testing.T) {
	d := EpicDraft{
		Epic: EpicSpec{Summary: "e", Scope: "s", OutOfScope: "perf deferred"},
		Children: []ChildDraft{
			{Summary: "c1", Proposal: "p1", AcceptanceCriteria: []string{"the parsed issue body renders in the preview"}},
			{Summary: "c2", Proposal: "p2", AcceptanceCriteria: []string{"A real backlog_grooming run against this repository reaches its approval gate"}},
		},
	}
	pc := EvaluateDraftCriteria(d)

	if f := childFinding(pc, 2, plan.RuleMissingLiveValidationMarker); f == nil {
		t.Fatalf("child 2 must surface missing_live_validation_marker at intake; got %+v", pc.Children)
	}
	// The drivable child stays clean, so the rule is a signal at intake too.
	if f := childFinding(pc, 1, plan.RuleMissingLiveValidationMarker); f != nil {
		t.Errorf("child 1 is drivable and must not be flagged; got %+v", *f)
	}
	// Advisory: only no_blocking_criterion sets NeedsAttention, so this finding
	// renders without flagging the child.
	if c := childCheck(pc, 2); c == nil || c.NeedsAttention {
		t.Errorf("missing_live_validation_marker must render without setting NeedsAttention; got %+v", c)
	}
}

// TestEvaluateDraftCriteria_AllSkipShapedDraft_NotBlocked is CONDITION 1's
// refinement half (#3026): the second consumer of the shared rule set must not
// be BLOCKED by the new advisory. It asserts the non-blocked OUTCOME directly —
// the draft-level and child-level NeedsAttention markers stay FALSE — not
// merely that a findings slice does or does not contain an entry.
//
// It also records, as a behavioral fact rather than a prose claim, that
// all_criteria_skip_expected is structurally INERT at intake and is so TWICE
// over:
//
//  1. EvaluateDraftCriteria maps a child's []string acceptance_criteria into a
//     synthetic plan.Verification that sets only ID and Statement — never
//     SkipExpected or ExpectationBasis. AcceptanceSkippableAllSkipWithBasis is
//     therefore false for every possible draft, so the rule cannot fire here no
//     matter what the criteria prose says (the criteria below deliberately
//     WORD themselves as skip-expected, and still draw nothing).
//  2. Even if it did fire, refinement's needs-attention trigger keys on
//     no_blocking_criterion ALONE, so the marker would stay false.
//
// The plan for this change expected case 1 to be a positive assertion (an
// all-skip draft PRODUCING the finding). It cannot be: the intake shape has no
// channel for the skip markers. Asserting the inertness is the honest and
// stronger statement, and it is what the counterfactual below discriminates on.
func TestEvaluateDraftCriteria_AllSkipShapedDraft_NotBlocked(t *testing.T) {
	d := EpicDraft{
		Epic: EpicSpec{Summary: "e", Scope: "s"},
		Children: []ChildDraft{{
			Summary: "c1",
			AcceptanceCriteria: []string{
				"skip_expected: the acceptance stage short-circuits, expectation_basis is the unit test",
				"skip_expected: the payload records the headline, expectation_basis is the server test",
			},
		}},
	}

	pc := EvaluateDraftCriteria(d)

	// NON-BLOCKED OUTCOME — the assertion Condition 1 asks for.
	if pc.NeedsAttention {
		t.Errorf("draft NeedsAttention = true; an advisory acceptance rule must never mark the draft needs_attention")
	}
	child := childCheck(pc, 1)
	if child == nil {
		t.Fatal("want a child check at ordinal 1")
	}
	if child.NeedsAttention {
		t.Errorf("child NeedsAttention = true; an advisory acceptance rule must never mark a child needs_attention")
	}

	// Inertness (1): the rule cannot fire through the intake mapping.
	if childFinding(pc, 1, plan.RuleAllCriteriaSkipExpected) != nil {
		t.Errorf("all_criteria_skip_expected fired at intake; the mapping sets no skip markers, so it must be inert here: %+v", child.Findings)
	}

	// A mixed-shaped draft is likewise unflagged and unblocked.
	mixed := EvaluateDraftCriteria(EpicDraft{
		Epic:     EpicSpec{Summary: "e", Scope: "s"},
		Children: []ChildDraft{{Summary: "c1", AcceptanceCriteria: []string{"the plan gate admits the plan"}}},
	})
	if mixed.NeedsAttention {
		t.Errorf("mixed draft NeedsAttention = true, want false")
	}
	if childFinding(mixed, 1, plan.RuleAllCriteriaSkipExpected) != nil {
		t.Errorf("all_criteria_skip_expected fired on a mixed draft: %+v", mixed.Children[0].Findings)
	}
}
