package plan

import "testing"

// ptrBool is a small helper for building explicit *bool blocking values.
func ptrBool(b bool) *bool { return &b }

// findingFor returns the first finding matching rule, or nil.
func findingFor(findings []AcceptanceFinding, rule string) *AcceptanceFinding {
	for i := range findings {
		if findings[i].Rule == rule {
			return &findings[i]
		}
	}
	return nil
}

// (rule: no_blocking_criterion) No criterion is effectively blocking and
// out_of_scope is empty -> the presence-level finding fires.
func TestEvaluateAcceptanceCriteria_NoBlockingCriterion(t *testing.T) {
	v := Verification{
		AcceptanceCriteria: []AcceptanceCriterion{
			{ID: "a1", Statement: "does a thing", Source: CriterionSourceExplicit, SourceRef: "#1", Blocking: ptrBool(false)},
		},
	}
	findings := EvaluateAcceptanceCriteria(v)
	if findingFor(findings, RuleNoBlockingCriterion) == nil {
		t.Fatalf("want no_blocking_criterion; got %+v", findings)
	}
}

// (rule: no_blocking_criterion suppression) A non-empty out_of_scope is the
// justified escape hatch — no presence-level finding even with no blocking
// criterion.
func TestEvaluateAcceptanceCriteria_OutOfScopeSuppressesNoBlocking(t *testing.T) {
	v := Verification{
		OutOfScope: []string{"performance tuning deferred to a follow-up"},
	}
	findings := EvaluateAcceptanceCriteria(v)
	if findingFor(findings, RuleNoBlockingCriterion) != nil {
		t.Fatalf("out_of_scope must suppress no_blocking_criterion; got %+v", findings)
	}
	if len(findings) != 0 {
		t.Fatalf("want zero findings; got %+v", findings)
	}
}

// (default) An omitted (nil) blocking is effectively blocking, so a single
// omitted-blocking criterion does NOT trip no_blocking_criterion.
func TestEvaluateAcceptanceCriteria_OmittedBlockingIsBlocking(t *testing.T) {
	v := Verification{
		AcceptanceCriteria: []AcceptanceCriterion{
			{ID: "a1", Statement: "does a thing", Source: CriterionSourceExplicit, SourceRef: "#1"},
		},
	}
	if !CriterionBlocking(v.AcceptanceCriteria[0]) {
		t.Fatal("an omitted blocking must default to true")
	}
	findings := EvaluateAcceptanceCriteria(v)
	if findingFor(findings, RuleNoBlockingCriterion) != nil {
		t.Fatalf("an omitted-blocking criterion must not flag; got %+v", findings)
	}
}

// (rule: missing_source_ref) An explicit criterion with no source_ref fires,
// naming the criterion id.
func TestEvaluateAcceptanceCriteria_MissingSourceRef(t *testing.T) {
	v := Verification{
		AcceptanceCriteria: []AcceptanceCriterion{
			{ID: "a1", Statement: "does a thing", Source: CriterionSourceExplicit, Blocking: ptrBool(true)},
		},
	}
	f := findingFor(EvaluateAcceptanceCriteria(v), RuleMissingSourceRef)
	if f == nil {
		t.Fatalf("want missing_source_ref")
	}
	if f.CriterionID != "a1" {
		t.Errorf("CriterionID = %q, want a1", f.CriterionID)
	}
}

// (rule: missing_rationale) An inferred criterion with no rationale fires.
func TestEvaluateAcceptanceCriteria_MissingRationale(t *testing.T) {
	v := Verification{
		AcceptanceCriteria: []AcceptanceCriterion{
			{ID: "a1", Statement: "does a thing", Source: CriterionSourceInferred, Blocking: ptrBool(true)},
		},
	}
	f := findingFor(EvaluateAcceptanceCriteria(v), RuleMissingRationale)
	if f == nil {
		t.Fatalf("want missing_rationale")
	}
	if f.CriterionID != "a1" {
		t.Errorf("CriterionID = %q, want a1", f.CriterionID)
	}
}

// (rule: empty_id) A criterion with an empty id fires.
func TestEvaluateAcceptanceCriteria_EmptyID(t *testing.T) {
	v := Verification{
		AcceptanceCriteria: []AcceptanceCriterion{
			{ID: "", Statement: "does a thing", Source: CriterionSourceExplicit, SourceRef: "#1", Blocking: ptrBool(true)},
		},
	}
	if findingFor(EvaluateAcceptanceCriteria(v), RuleEmptyID) == nil {
		t.Fatalf("want empty_id")
	}
}

// (rule: duplicate_id) Two criteria sharing an id fire duplicate_id naming it.
func TestEvaluateAcceptanceCriteria_DuplicateID(t *testing.T) {
	v := Verification{
		AcceptanceCriteria: []AcceptanceCriterion{
			{ID: "dup", Statement: "first", Source: CriterionSourceExplicit, SourceRef: "#1", Blocking: ptrBool(true)},
			{ID: "dup", Statement: "second", Source: CriterionSourceExplicit, SourceRef: "#2", Blocking: ptrBool(true)},
		},
	}
	f := findingFor(EvaluateAcceptanceCriteria(v), RuleDuplicateID)
	if f == nil {
		t.Fatalf("want duplicate_id")
	}
	if f.CriterionID != "dup" {
		t.Errorf("CriterionID = %q, want dup", f.CriterionID)
	}
}

// (clean contract) A fully clean criteria set returns a NON-NIL empty slice, so
// a payload can distinguish "checked and clean" ([]) from "never checked".
func TestEvaluateAcceptanceCriteria_CleanReturnsNonNilEmpty(t *testing.T) {
	v := Verification{
		AcceptanceCriteria: []AcceptanceCriterion{
			{ID: "a1", Statement: "does a thing", Source: CriterionSourceExplicit, SourceRef: "#1", Blocking: ptrBool(true)},
			{ID: "a2", Statement: "inferred one", Source: CriterionSourceInferred, Rationale: "derived from the issue", Blocking: ptrBool(false)},
		},
	}
	findings := EvaluateAcceptanceCriteria(v)
	if findings == nil {
		t.Fatal("findings must be non-nil ([] not null) on a clean set")
	}
	if len(findings) != 0 {
		t.Fatalf("want zero findings on a clean set; got %+v", findings)
	}
}
