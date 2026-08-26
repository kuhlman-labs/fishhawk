package plan

import (
	"strings"
	"testing"
)

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

// (predicate: AcceptanceSkippableOutOfScope) The full truth table of the
// out_of_scope-declared AND zero-acceptance_criteria skip condition (#1657):
// true only when out_of_scope is non-empty and no criteria are present.
func TestAcceptanceSkippableOutOfScope_TruthTable(t *testing.T) {
	blocking := true
	cases := []struct {
		name string
		v    Verification
		want bool
	}{
		{
			name: "out_of_scope + no criteria -> skippable",
			v:    Verification{OutOfScope: []string{"deletion deferred"}},
			want: true,
		},
		{
			name: "out_of_scope + >=1 criteria -> NOT skippable",
			v: Verification{
				OutOfScope: []string{"deletion deferred"},
				AcceptanceCriteria: []AcceptanceCriterion{
					{ID: "a1", Statement: "does a thing", Source: CriterionSourceExplicit, SourceRef: "#1", Blocking: &blocking},
				},
			},
			want: false,
		},
		{
			name: "no out_of_scope + no criteria -> NOT skippable",
			v:    Verification{},
			want: false,
		},
		{
			name: "no out_of_scope + >=1 criteria -> NOT skippable (both non-empty guard)",
			v: Verification{
				AcceptanceCriteria: []AcceptanceCriterion{
					{ID: "a1", Statement: "does a thing", Source: CriterionSourceExplicit, SourceRef: "#1", Blocking: &blocking},
				},
			},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AcceptanceSkippableOutOfScope(tc.v); got != tc.want {
				t.Errorf("AcceptanceSkippableOutOfScope = %v, want %v", got, tc.want)
			}
		})
	}
}

// (predicate: AcceptanceSkippableEmptyCriteria) The full truth table of the
// zero-acceptance_criteria AND zero-out_of_scope short-circuit condition
// (#1728): true only when BOTH are empty. Deliberately disjoint from
// AcceptanceSkippableOutOfScope (which requires out_of_scope non-empty), so the
// out_of_scope-present case is false here (that is E38.3's domain).
func TestAcceptanceSkippableEmptyCriteria_TruthTable(t *testing.T) {
	blocking := true
	cases := []struct {
		name string
		v    Verification
		want bool
	}{
		{
			name: "no criteria + no out_of_scope -> skippable",
			v:    Verification{},
			want: true,
		},
		{
			name: "out_of_scope present + no criteria -> NOT skippable (E38.3's domain)",
			v:    Verification{OutOfScope: []string{"deletion deferred"}},
			want: false,
		},
		{
			name: "criteria present + no out_of_scope -> NOT skippable",
			v: Verification{
				AcceptanceCriteria: []AcceptanceCriterion{
					{ID: "a1", Statement: "does a thing", Source: CriterionSourceExplicit, SourceRef: "#1", Blocking: &blocking},
				},
			},
			want: false,
		},
		{
			name: "both criteria and out_of_scope present -> NOT skippable",
			v: Verification{
				OutOfScope: []string{"deletion deferred"},
				AcceptanceCriteria: []AcceptanceCriterion{
					{ID: "a1", Statement: "does a thing", Source: CriterionSourceExplicit, SourceRef: "#1", Blocking: &blocking},
				},
			},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AcceptanceSkippableEmptyCriteria(tc.v); got != tc.want {
				t.Errorf("AcceptanceSkippableEmptyCriteria = %v, want %v", got, tc.want)
			}
		})
	}
}

// (predicate: AcceptanceSkippableAllSkipWithBasis) The full truth table of the
// all-skip-with-basis short-circuit condition (#1748): true only when there is
// at least one criterion AND every criterion carries skip_expected with a
// non-empty (trimmed) expectation_basis. A single drivable criterion, a marked
// criterion with an empty/whitespace basis, or an empty criteria set all yield
// false. It is disjoint from AcceptanceSkippableEmptyCriteria (which requires
// zero criteria), so an empty set is false here.
func TestAcceptanceSkippableAllSkipWithBasis_TruthTable(t *testing.T) {
	cases := []struct {
		name string
		v    Verification
		want bool
	}{
		{
			name: "all marked with basis -> skippable",
			v: Verification{
				AcceptanceCriteria: []AcceptanceCriterion{
					{ID: "a1", Statement: "webhook fires", Source: CriterionSourceInferred, Rationale: "external", SkipExpected: true, ExpectationBasis: "validated in webhook_integration_test.go with a fake"},
					{ID: "a2", Statement: "issue closes", Source: CriterionSourceInferred, Rationale: "external", SkipExpected: true, ExpectationBasis: "validated in closer_e2e_test.go"},
				},
			},
			want: true,
		},
		{
			name: "mixed: one drivable (no marker) -> NOT skippable",
			v: Verification{
				AcceptanceCriteria: []AcceptanceCriterion{
					{ID: "a1", Statement: "webhook fires", Source: CriterionSourceInferred, Rationale: "external", SkipExpected: true, ExpectationBasis: "validated in webhook_integration_test.go"},
					{ID: "a2", Statement: "GET returns 200", Source: CriterionSourceExplicit, SourceRef: "#1"},
				},
			},
			want: false,
		},
		{
			name: "marked but empty basis -> NOT skippable",
			v: Verification{
				AcceptanceCriteria: []AcceptanceCriterion{
					{ID: "a1", Statement: "webhook fires", Source: CriterionSourceInferred, Rationale: "external", SkipExpected: true, ExpectationBasis: ""},
				},
			},
			want: false,
		},
		{
			name: "marked but whitespace-only basis -> NOT skippable",
			v: Verification{
				AcceptanceCriteria: []AcceptanceCriterion{
					{ID: "a1", Statement: "webhook fires", Source: CriterionSourceInferred, Rationale: "external", SkipExpected: true, ExpectationBasis: "   \t "},
				},
			},
			want: false,
		},
		{
			name: "empty criteria -> NOT skippable (disjoint from empty-criteria predicate)",
			v:    Verification{},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AcceptanceSkippableAllSkipWithBasis(tc.v); got != tc.want {
				t.Errorf("AcceptanceSkippableAllSkipWithBasis = %v, want %v", got, tc.want)
			}
		})
	}
}

// (selector: LiveValidationCriteria) The truth table of the #2045 live-validation
// selector: it returns EXACTLY the subset of criteria marked
// requires_live_validation, in order, and a non-nil empty slice when none is
// marked or the criteria set is empty. It keys ONLY on RequiresLiveValidation —
// skip_expected/expectation_basis are orthogonal (a criterion can be
// live-validation-marked whether or not it is also skip-marked).
func TestLiveValidationCriteria_TruthTable(t *testing.T) {
	live := func(id string) AcceptanceCriterion {
		return AcceptanceCriterion{ID: id, Statement: "live target", Source: CriterionSourceInferred, Rationale: "needs a live forge", RequiresLiveValidation: true, SkipExpected: true, ExpectationBasis: "validated in integration_test.go"}
	}
	plain := func(id string) AcceptanceCriterion {
		return AcceptanceCriterion{ID: id, Statement: "drivable", Source: CriterionSourceExplicit, SourceRef: "#1"}
	}
	cases := []struct {
		name    string
		v       Verification
		wantIDs []string
	}{
		{
			name:    "none marked -> empty selection",
			v:       Verification{AcceptanceCriteria: []AcceptanceCriterion{plain("a1"), plain("a2")}},
			wantIDs: []string{},
		},
		{
			name:    "all marked -> all selected in order",
			v:       Verification{AcceptanceCriteria: []AcceptanceCriterion{live("a1"), live("a2")}},
			wantIDs: []string{"a1", "a2"},
		},
		{
			name:    "mixed -> only the marked subset, order preserved",
			v:       Verification{AcceptanceCriteria: []AcceptanceCriterion{plain("a1"), live("a2"), plain("a3"), live("a4")}},
			wantIDs: []string{"a2", "a4"},
		},
		{
			name:    "empty criteria -> empty selection",
			v:       Verification{},
			wantIDs: []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := LiveValidationCriteria(tc.v)
			if got == nil {
				t.Fatal("LiveValidationCriteria must return a non-nil slice")
			}
			gotIDs := make([]string, len(got))
			for i, c := range got {
				gotIDs[i] = c.ID
			}
			if len(gotIDs) != len(tc.wantIDs) {
				t.Fatalf("selected ids = %v, want %v", gotIDs, tc.wantIDs)
			}
			for i := range gotIDs {
				if gotIDs[i] != tc.wantIDs[i] {
					t.Fatalf("selected ids = %v, want %v", gotIDs, tc.wantIDs)
				}
			}
			// #2347: the count wrapper the short-circuit emit site uses must agree
			// with the selector over the SAME table — it exists so the emit site
			// does not re-walk the criteria itself, so a divergence between the two
			// would put a wrong criteria_live_validation into the audit payload
			// while the selector's own tests stayed green.
			if n := LiveValidationCriteriaCount(tc.v); n != len(tc.wantIDs) {
				t.Errorf("LiveValidationCriteriaCount = %d, want %d (must agree with LiveValidationCriteria)", n, len(tc.wantIDs))
			}
		})
	}
}

// TestAcceptanceNotValidatedVocabulary pins the #2347 constant set. These values
// travel through an audit payload into the server gate, the MCP classifier, and
// two render surfaces — several of which MIRROR rather than import them (the
// #875 no-import seam) — so their literal bytes are the contract, not an
// implementation detail. A silent rename here would degrade every short-circuited
// run to the defensive settled-outcome-unknown arm rather than failing to compile.
func TestAcceptanceNotValidatedVocabulary(t *testing.T) {
	if AcceptanceVerdictNotValidated != "not_validated" {
		t.Errorf("AcceptanceVerdictNotValidated = %q, want %q", AcceptanceVerdictNotValidated, "not_validated")
	}
	if AcceptanceOutcomeNotValidated != "not_validated" {
		t.Errorf("AcceptanceOutcomeNotValidated = %q, want %q", AcceptanceOutcomeNotValidated, "not_validated")
	}
	if AcceptanceCriteriaLiveValidationKey != "criteria_live_validation" {
		t.Errorf("AcceptanceCriteriaLiveValidationKey = %q, want %q", AcceptanceCriteriaLiveValidationKey, "criteria_live_validation")
	}
	// The verdict must be a THIRD value, never colliding with the two wire
	// verdicts — the gate switches on it, and a collision would make a
	// short-circuit indistinguishable from a validator-shipped pass.
	for _, wire := range []string{"passed", "failed"} {
		if AcceptanceVerdictNotValidated == wire {
			t.Errorf("AcceptanceVerdictNotValidated collides with the wire verdict %q", wire)
		}
	}
	// Likewise the outcome word must not collide with the binary render
	// vocabulary the status templates already switch on.
	for _, render := range []string{"accepted", "rejected"} {
		if AcceptanceOutcomeNotValidated == render {
			t.Errorf("AcceptanceOutcomeNotValidated collides with the render word %q", render)
		}
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

// findingsFor returns every finding matching rule.
func findingsFor(findings []AcceptanceFinding, rule string) []AcceptanceFinding {
	out := []AcceptanceFinding{}
	for _, f := range findings {
		if f.Rule == rule {
			out = append(out, f)
		}
	}
	return out
}

// (#2512 vocabulary) The three undecidable constants carry the exact strings
// the wire, the audit payload, and the render templates key on. They are
// deliberately equal in VALUE but distinct in MEANING (row result vs derived
// verdict vs render outcome), so each is pinned by name — a rename that
// collapses one into another is caught here rather than at a surface that
// silently renders the wrong word.
func TestUndecidableVocabulary_Values(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  string
		want string
	}{
		{"result", AcceptanceResultUndecidable, "undecidable"},
		{"verdict", AcceptanceVerdictUndecidable, "undecidable"},
		{"outcome", AcceptanceOutcomeUndecidable, "undecidable"},
		{"rule", RuleUndecidableCriterion, "undecidable_criterion"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
	// The partition's load-bearing disjointness: undecidable is NOT the
	// pre-spawn not_validated verdict. They are different dispositions and a
	// merge surface must be able to tell them apart.
	if AcceptanceVerdictUndecidable == AcceptanceVerdictNotValidated {
		t.Error("undecidable and not_validated must be distinct verdict strings")
	}
	if AcceptanceOutcomeUndecidable == AcceptanceOutcomeNotValidated {
		t.Error("undecidable and not_validated must be distinct outcome strings")
	}
}

// (rule: undecidable_criterion) A criterion naming a capability the sandbox
// lacks is flagged, naming the criterion id and the capability.
func TestUnevaluableCriteria_FlagsCapabilityStatements(t *testing.T) {
	for _, tc := range []struct {
		name       string
		statement  string
		wantDetail string
	}{
		{"mcp", "an operator making an MCP tool call sees the new state", "a live MCP client"},
		{"operator session", "a real operator session shows the merge banner", "a real operator session"},
		{"deployed", "the deployed environment serves the new endpoint", "a running external instance"},
		{"forge", "a live GitHub round-trip closes the issue", "a live forge round-trip"},
		{"webhook", "a real webhook delivery from the forge reopens the run", "a real webhook delivery"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := Verification{
				AcceptanceCriteria: []AcceptanceCriterion{
					{ID: "a1", Statement: tc.statement, Source: CriterionSourceExplicit, SourceRef: "#2512"},
				},
			}
			f := findingFor(EvaluateAcceptanceCriteria(v), RuleUndecidableCriterion)
			if f == nil {
				t.Fatalf("want undecidable_criterion for %q", tc.statement)
			}
			if f.CriterionID != "a1" {
				t.Errorf("CriterionID = %q, want a1", f.CriterionID)
			}
			if !strings.Contains(f.Detail, tc.wantDetail) {
				t.Errorf("Detail = %q, want it to name %q", f.Detail, tc.wantDetail)
			}
		})
	}
}

// (exemption) A criterion already marked skip_expected with a non-whitespace
// expectation_basis is the SANCTIONED declaration for undecidable_criterion —
// it is NOT re-flagged by THAT rule. This is the control the #2512 plan calls
// out: re-flagging a correctly-marked criterion trains the operator to ignore
// the rule.
//
// #2845 PAIRED HOLE CASE. This is exactly the shape the live-validation hole
// takes: because undecidable_criterion exempts here, a criterion naming a LIVE
// forge round-trip drew NO finding at all and silently lost its auto-filed
// operator-validation walk. undecidable_criterion's behaviour is UNCHANGED (it
// still exempts, asserted below); missing_live_validation_marker, whose
// exemption is requires_live_validation ALONE, is what now flags it. So the
// criterion is no longer silent — the original comment's claim that it draws no
// finding is no longer true and is corrected here.
func TestUnevaluableCriteria_ExemptsSkipExpectedWithBasis(t *testing.T) {
	v := Verification{
		AcceptanceCriteria: []AcceptanceCriterion{
			{
				ID: "a1", Statement: "a live GitHub round-trip closes the issue",
				Source: CriterionSourceExplicit, SourceRef: "#2512",
				SkipExpected: true, ExpectationBasis: "pinned by the fake-forge integration test",
			},
		},
	}
	findings := EvaluateAcceptanceCriteria(v)
	if f := findingFor(findings, RuleUndecidableCriterion); f != nil {
		t.Fatalf("a skip_expected criterion with a basis must not be flagged; got %+v", *f)
	}
	if f := findingFor(findings, RuleMissingLiveValidationMarker); f == nil {
		t.Fatalf("#2845: the live-target hole — skip_expected-with-basis must still draw missing_live_validation_marker; got %+v", findings)
	}
}

// (exemption) requires_live_validation is the second sanctioned declaration and
// exempts on its own.
func TestUnevaluableCriteria_ExemptsRequiresLiveValidation(t *testing.T) {
	v := Verification{
		AcceptanceCriteria: []AcceptanceCriterion{
			{
				ID: "a1", Statement: "a live GitHub round-trip closes the issue",
				Source: CriterionSourceExplicit, SourceRef: "#2512",
				RequiresLiveValidation: true,
			},
		},
	}
	if f := findingFor(EvaluateAcceptanceCriteria(v), RuleUndecidableCriterion); f != nil {
		t.Fatalf("a requires_live_validation criterion must not be flagged; got %+v", *f)
	}
}

// (exemption boundary) skip_expected with a WHITESPACE-ONLY expectation_basis
// is NOT a sanctioned declaration — the basis is what makes the marking
// meaningful — so the criterion is still flagged. Pairs with the
// AcceptanceSkippableAllSkipWithBasis whitespace rule so the two predicates
// treat a blank basis identically.
func TestUnevaluableCriteria_WhitespaceBasisIsNotAnExemption(t *testing.T) {
	v := Verification{
		AcceptanceCriteria: []AcceptanceCriterion{
			{
				ID: "a1", Statement: "a live GitHub round-trip closes the issue",
				Source: CriterionSourceExplicit, SourceRef: "#2512",
				SkipExpected: true, ExpectationBasis: "   ",
			},
		},
	}
	if f := findingFor(EvaluateAcceptanceCriteria(v), RuleUndecidableCriterion); f == nil {
		t.Fatal("a whitespace-only expectation_basis must not exempt the criterion")
	}
}

// (no false positive) An ordinary drivable criterion — including one that
// mentions the forge in passing without naming a live round-trip — is not
// flagged. An advisory rule that fires on ordinary prose is an ignored rule.
func TestUnevaluableCriteria_DrivableCriterionNotFlagged(t *testing.T) {
	v := Verification{
		AcceptanceCriteria: []AcceptanceCriterion{
			{ID: "a1", Statement: "posting a verdict with an undecidable row records verdict=undecidable", Source: CriterionSourceExplicit, SourceRef: "#2512"},
			{ID: "a2", Statement: "the github issue number is rendered in the status comment", Source: CriterionSourceExplicit, SourceRef: "#2512"},
		},
	}
	if got := findingsFor(EvaluateAcceptanceCriteria(v), RuleUndecidableCriterion); len(got) != 0 {
		t.Fatalf("want no undecidable_criterion findings; got %+v", got)
	}
}

// (one criterion, one finding) A statement naming TWO lacked capabilities
// yields exactly one finding, so a downstream count of findings is a count of
// criteria rather than of phrase hits.
func TestUnevaluableCriteria_OneFindingPerCriterion(t *testing.T) {
	v := Verification{
		AcceptanceCriteria: []AcceptanceCriterion{
			{ID: "a1", Statement: "a real operator session drives a live GitHub round-trip through the MCP client", Source: CriterionSourceExplicit, SourceRef: "#2512"},
		},
	}
	if got := findingsFor(EvaluateAcceptanceCriteria(v), RuleUndecidableCriterion); len(got) != 1 {
		t.Fatalf("want exactly 1 finding; got %d: %+v", len(got), got)
	}
}

// (case-insensitivity) Matching is case-insensitive over the statement.
func TestUnevaluableCriteria_CaseInsensitive(t *testing.T) {
	v := Verification{
		AcceptanceCriteria: []AcceptanceCriterion{
			{ID: "a1", Statement: "A LIVE GITHUB ROUND-TRIP CLOSES THE ISSUE", Source: CriterionSourceExplicit, SourceRef: "#2512"},
		},
	}
	if f := findingFor(EvaluateAcceptanceCriteria(v), RuleUndecidableCriterion); f == nil {
		t.Fatal("matching must be case-insensitive")
	}
}

// (shared rule set) The matcher rides EvaluateAcceptanceCriteria, so both
// consumers of the shared set get it from one place. Calling the matcher
// directly and calling the evaluator must agree on what fires.
func TestEvaluateAcceptanceCriteria_IncludesUnevaluableFindings(t *testing.T) {
	v := Verification{
		AcceptanceCriteria: []AcceptanceCriterion{
			{ID: "a1", Statement: "a live GitHub round-trip closes the issue", Source: CriterionSourceExplicit, SourceRef: "#2512"},
		},
	}
	direct := UnevaluableCriteria(v)
	viaSet := findingsFor(EvaluateAcceptanceCriteria(v), RuleUndecidableCriterion)
	if len(direct) != 1 || len(viaSet) != 1 || direct[0] != viaSet[0] {
		t.Fatalf("evaluator and matcher disagree: direct=%+v viaSet=%+v", direct, viaSet)
	}
}

// (non-nil contract) The matcher returns [] not nil on a clean plan, matching
// EvaluateAcceptanceCriteria's payload contract.
func TestUnevaluableCriteria_NonNilOnCleanPlan(t *testing.T) {
	if got := UnevaluableCriteria(Verification{}); got == nil {
		t.Fatal("UnevaluableCriteria must return a non-nil slice")
	}
}

// ---------------------------------------------------------------------------
// missing_live_validation_marker (#2845, E54.31)
//
// FIXTURE-COLLISION SWEEP (run BEFORE any production edit, per the approved
// plan's step 1). `grep -rniE '\b(live|real|actual|production|genuine)\b'` over
// the four test files in scope found:
//   - backend/internal/refinement/precheck_test.go — ZERO qualifier occurrences.
//   - backend/internal/prompt/prompt_test.go — ZERO inside criterion statements
//     (all hits are prompt-literal assertions or unrelated prose).
//   - acceptance_check_test.go — five INCIDENTAL hits, none of which fire the
//     final matcher: :304 "live target" (no action noun, and
//     RequiresLiveValidation anyway); :467 "an operator making an MCP tool call"
//     (no qualifier); :468 "a real operator session shows the merge banner"
//     (operator/session are not action nouns); :471 "a real webhook delivery
//     from the forge reopens the run" ("run" is 8 tokens from "real" and there
//     is no against-phrase — promoted to an explicit near-miss row below);
//     :549-560 the drivable controls. Plus the INTENTIONAL "a live GitHub
//     round-trip" / "the deployed environment" hits, which fire via M1 by
//     design and whose assertions are all rule-FILTERED, so they stay green.
//   - acceptance_precheck_test.go — three intentional "a live GitHub round-trip
//     closes the originating issue" hits (:473, :526, :576), same disposition.
//
// No fixture reword was required.
// ---------------------------------------------------------------------------

// (discrimination — issue AC4, operator defect 2) EIGHT sandbox-validatable
// statements that each carry a real qualifier/target collocation and MUST NOT
// fire. The first four are the operator's verbatim fixtures; the next three are
// sharper ones carrying an explicit against-phrase; the last is the incidental
// collision the sweep found at :471.
//
// Two of these are green for reasons worth stating, because a later corpus
// addition would silently flip them:
//
//   - "a real run of the verify gate against the committed tree" is green ONLY
//     because "tree" is absent from externalTargetNouns — greenness by ABSENCE.
//     Conjunct 1 and the "against" of conjunct 2 both hold; adding "tree" to the
//     noun corpus would turn this row red with no other change. (Recorded per
//     operator condition C1.)
//   - "a real apply against the localhost preview records the audit entry" is
//     green because conjunct 2 fails outright (neither "localhost" nor
//     "preview" is an external-target noun), NOT because of conjunct 3. The
//     conjunct-3 counterfactual therefore uses the "fake tracker" row, which is
//     the only row where conjunct 2 holds and conjunct 3 is what saves it.
func TestMissingLiveValidationMarker_NearMissDrivableStatements(t *testing.T) {
	for _, tc := range []struct {
		name      string
		statement string
	}{
		{"operator/actual issue body", "the actual issue body is parsed"},
		{"operator/real repo path", "the real repo path is resolved from the sandbox checkout"},
		{"operator/real run of the test suite", "a real run of the test suite regenerates the pages"},
		{"operator/production label list", "the production label list in config is read"},
		{"against a committed tree", "a real run of the verify gate against the committed tree"},
		{"against a fake tracker", "the actual walk issue is filed against the fake tracker in the integration test"},
		{"against a localhost preview", "a real apply against the localhost preview records the audit entry"},
		{"incidental webhook fixture", "a real webhook delivery from the forge reopens the run"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := Verification{
				AcceptanceCriteria: []AcceptanceCriterion{
					{ID: "a1", Statement: tc.statement, Source: CriterionSourceExplicit, SourceRef: "#2845"},
				},
			}
			if got := findingsFor(EvaluateAcceptanceCriteria(v), RuleMissingLiveValidationMarker); len(got) != 0 {
				t.Fatalf("drivable statement %q must not fire missing_live_validation_marker; got %+v", tc.statement, got)
			}
		})
	}
}

// (M1 false-positive row — operator condition C2) M1 REUSES phrases written for
// a rule with the looser either-marking exemption, so a sandbox-validatable
// statement carrying an inherited liveTarget phrase now fires where it was
// previously silent. This row and the residual row below were added and RUN to
// decide, with the result in hand, whether M1 should honour the sandbox-marker
// negation.
//
// OBSERVED RESULT and DECISION: it should, and it does. The negation rescues
// the row that names its stand-in. It does NOT rescue the row that carries the
// phrase in sandbox-validatable prose with no marker at all — see
// TestMissingLiveValidationMarker_M1ResidualFalsePositive, which pins that
// residual rather than deleting the fixture.
//
// Narrowing was applied per C2's resolution — narrowing what M1 CONSIDERS, NOT
// editing unevaluableCapabilities' phrase strings, which stay byte-identical.
func TestMissingLiveValidationMarker_M1SandboxMarkerNegation(t *testing.T) {
	v := Verification{
		AcceptanceCriteria: []AcceptanceCriterion{
			{ID: "a1", Statement: "the github api client retries on 502 in the fake transport test", Source: CriterionSourceExplicit, SourceRef: "#2845"},
		},
	}
	if got := findingsFor(EvaluateAcceptanceCriteria(v), RuleMissingLiveValidationMarker); len(got) != 0 {
		t.Fatalf("an M1 phrase in prose naming its own stand-in must not fire; got %+v", got)
	}
}

// (M1 residual — operator condition C2, recorded honestly) A liveTarget phrase
// in sandbox-validatable prose carrying NO sandbox marker still fires. This is
// a KNOWN residual false positive, not an oversight: the only narrowing that
// would suppress it — additionally demanding a live-action noun — also drops
// the genuine true positive "the deployed environment serves the new endpoint",
// so per the plan's binding decision rule the fixture is kept and the residual
// stated rather than the corpus narrowed past a true positive.
//
// This test pins the CURRENT observed behaviour so a future narrowing that
// fixes it flips this test visibly instead of passing unnoticed.
func TestMissingLiveValidationMarker_M1ResidualFalsePositive(t *testing.T) {
	v := Verification{
		AcceptanceCriteria: []AcceptanceCriterion{
			{ID: "a1", Statement: "the deployed environment config template is rendered", Source: CriterionSourceExplicit, SourceRef: "#2845"},
		},
	}
	if got := findingsFor(EvaluateAcceptanceCriteria(v), RuleMissingLiveValidationMarker); len(got) != 1 {
		t.Fatalf("documented M1 residual: want exactly 1 finding (see the rule's doc comment); got %d: %+v", len(got), got)
	}
}

// DONE-MEANS BEHAVIORAL TEST (#1169). The two statements the SHIPPED detector
// missed, verbatim from the four runs #2845 documents, as ordinary unmarked
// blocking criteria. This is the observable output of the change: a comment-only
// or no-op touch of acceptance_check.go fails here.
func TestMissingLiveValidationMarker_FourRunRegressionCorpus(t *testing.T) {
	for _, tc := range []struct {
		name      string
		id        string
		statement string
	}{
		{"#2822 criterion 6", "live-walk", "A live walk is recorded: one real grooming run against this repo's backlog"},
		{"#2833 criterion 6", "grooming-run", "A real backlog_grooming run against this repository reaches its approval gate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := Verification{
				AcceptanceCriteria: []AcceptanceCriterion{
					{ID: tc.id, Statement: tc.statement, Source: CriterionSourceExplicit, SourceRef: "#2845"},
				},
			}
			got := findingsFor(EvaluateAcceptanceCriteria(v), RuleMissingLiveValidationMarker)
			if len(got) != 1 {
				t.Fatalf("want exactly 1 missing_live_validation_marker for %q; got %d: %+v", tc.statement, len(got), got)
			}
			if got[0].CriterionID != tc.id {
				t.Errorf("CriterionID = %q, want %q", got[0].CriterionID, tc.id)
			}
		})
	}
}

// (THE HOLE — the defect #2845 documents) "Validate against live GitHub" marked
// skip_expected with a basis and NO requires_live_validation is EXEMPT from
// undecidable_criterion, so before this rule it drew no finding at all and
// silently lost its operator-validation walk. The marker-only exemption is what
// closes that: the criterion IS flagged.
func TestMissingLiveValidationMarker_SkipExpectedWithBasisIsNotAnExemption(t *testing.T) {
	v := Verification{
		AcceptanceCriteria: []AcceptanceCriterion{
			{
				ID: "live-github", Statement: "Validate against live GitHub",
				Source: CriterionSourceExplicit, SourceRef: "#2845",
				SkipExpected: true, ExpectationBasis: "pinned by the fake-forge integration test",
			},
		},
	}
	findings := EvaluateAcceptanceCriteria(v)
	f := findingFor(findings, RuleMissingLiveValidationMarker)
	if f == nil {
		t.Fatalf("skip_expected-with-basis must NOT exempt a live-target criterion; got %+v", findings)
	}
	if f.CriterionID != "live-github" {
		t.Errorf("CriterionID = %q, want live-github", f.CriterionID)
	}
	// And the older rule stays silent on it — which is exactly why the hole
	// existed and why this rule is not redundant with it.
	if u := findingFor(findings, RuleUndecidableCriterion); u != nil {
		t.Errorf("undecidable_criterion's behaviour must be unchanged (still exempt); got %+v", *u)
	}
}

// (remedy wording — operator condition C3) Criterion `finding-names-the-criterion`
// requires the Detail to carry BOTH halves of the remedy. Without a literal
// assertion the prose can drift while every other test stays green.
func TestMissingLiveValidationMarker_DetailNamesBothHalvesOfTheRemedy(t *testing.T) {
	v := Verification{
		AcceptanceCriteria: []AcceptanceCriterion{
			{ID: "a1", Statement: "a live GitHub round-trip closes the issue", Source: CriterionSourceExplicit, SourceRef: "#2845"},
		},
	}
	f := findingFor(EvaluateAcceptanceCriteria(v), RuleMissingLiveValidationMarker)
	if f == nil {
		t.Fatal("want a missing_live_validation_marker finding")
	}
	for _, want := range []string{
		// Half 1: the marker must be PAIRED, and the pairing is what files the walk.
		"set requires_live_validation: true",
		"pair it with skip_expected: true plus an expectation_basis",
		"auto-files the tracked operator-validation walk",
		// Half 2: the weaker marking silently loses the walk.
		"A skip_expected-only marking silently loses that walk.",
	} {
		if !strings.Contains(f.Detail, want) {
			t.Errorf("Detail must carry %q\n---\n%s", want, f.Detail)
		}
	}
}

// (WORKING-PATH CONTROL — issue AC3) A correctly-marked criterion is
// undisturbed: requires_live_validation paired with skip_expected + basis draws
// NO finding from EITHER rule, and the live-validation selectors still pick it
// up so the walk-filing path this issue says already works is provably intact.
//
// COUNTERFACTUAL: deleting the `c.RequiresLiveValidation` exemption in
// MissingLiveValidationMarker reddens this test. Executed and observed.
func TestMissingLiveValidationMarker_CorrectlyMarkedCriterionUndisturbed(t *testing.T) {
	v := Verification{
		AcceptanceCriteria: []AcceptanceCriterion{
			{
				ID: "live-walk", Statement: "A live walk is recorded: one real grooming run against this repo's backlog",
				Source: CriterionSourceExplicit, SourceRef: "#2845",
				RequiresLiveValidation: true,
				SkipExpected:           true, ExpectationBasis: "pinned by the fake-tracker grooming integration test",
			},
		},
	}
	findings := EvaluateAcceptanceCriteria(v)
	if f := findingFor(findings, RuleMissingLiveValidationMarker); f != nil {
		t.Fatalf("a requires_live_validation criterion must not be flagged; got %+v", *f)
	}
	if f := findingFor(findings, RuleUndecidableCriterion); f != nil {
		t.Fatalf("a requires_live_validation criterion must not be flagged undecidable either; got %+v", *f)
	}
	selected := LiveValidationCriteria(v)
	if len(selected) != 1 || selected[0].ID != "live-walk" {
		t.Fatalf("LiveValidationCriteria must still select the marked criterion; got %+v", selected)
	}
	if n := LiveValidationCriteriaCount(v); n != 1 {
		t.Errorf("LiveValidationCriteriaCount = %d, want 1", n)
	}
}

// (no cross-rule suppression) A wholly-unmarked live-target criterion draws
// EXACTLY one finding from EACH rule — one finding per criterion per rule. The
// doubling is deliberate and complementary, not a bug.
func TestMissingLiveValidationMarker_BothRulesFireWithoutSuppression(t *testing.T) {
	v := Verification{
		AcceptanceCriteria: []AcceptanceCriterion{
			{ID: "a1", Statement: "a live GitHub round-trip closes the issue", Source: CriterionSourceExplicit, SourceRef: "#2845"},
		},
	}
	findings := EvaluateAcceptanceCriteria(v)
	if got := findingsFor(findings, RuleUndecidableCriterion); len(got) != 1 {
		t.Errorf("want exactly 1 undecidable_criterion; got %d: %+v", len(got), got)
	}
	if got := findingsFor(findings, RuleMissingLiveValidationMarker); len(got) != 1 {
		t.Errorf("want exactly 1 missing_live_validation_marker; got %d: %+v", len(got), got)
	}
}

// (EXCLUDED CLASS — the principled non-coverage boundary) The external-TRIGGER
// capabilities — a real webhook delivery, an MCP tool call, an operator session
// — are NOT live targets. For them skip_expected with a basis is the
// doctrinally complete marking and no operator-validation walk is owed, so
// NEITHER rule fires. This test is where that decision is recorded; widening is
// a one-line liveTarget flip in unevaluableCapabilities.
func TestMissingLiveValidationMarker_ExternalTriggerClassExcluded(t *testing.T) {
	for _, tc := range []struct {
		name      string
		statement string
	}{
		{"webhook", "a real webhook delivery from the forge reopens the run"},
		{"mcp", "an operator making an MCP tool call sees the new state"},
		{"operator session", "a real operator session shows the merge banner"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := Verification{
				AcceptanceCriteria: []AcceptanceCriterion{
					{
						ID: "a1", Statement: tc.statement,
						Source: CriterionSourceExplicit, SourceRef: "#2845",
						SkipExpected: true, ExpectationBasis: "pinned by the fake-forge integration test",
					},
				},
			}
			findings := EvaluateAcceptanceCriteria(v)
			if f := findingFor(findings, RuleMissingLiveValidationMarker); f != nil {
				t.Errorf("the external-trigger class must not demand the marker; got %+v", *f)
			}
			if f := findingFor(findings, RuleUndecidableCriterion); f != nil {
				t.Errorf("skip_expected-with-basis must still exempt undecidable_criterion; got %+v", *f)
			}
		})
	}
}

// (M1/M2 independence) Each matcher fires ON ITS OWN, so neither is dead code
// carried by the other.
func TestMissingLiveValidationMarker_M1AndM2FireIndependently(t *testing.T) {
	// M1 only: a liveTarget corpus phrase, with neither proximity conjunct.
	m1Statement := "the deployed environment serves the new endpoint"
	if livenessProximityMatch(acceptanceTokens(m1Statement)) {
		t.Fatal("premise broken: this statement was chosen because M2 must NOT match it")
	}
	m1 := Verification{AcceptanceCriteria: []AcceptanceCriterion{
		{ID: "a1", Statement: m1Statement, Source: CriterionSourceExplicit, SourceRef: "#2845"},
	}}
	if got := findingsFor(EvaluateAcceptanceCriteria(m1), RuleMissingLiveValidationMarker); len(got) != 1 {
		t.Fatalf("M1 must fire alone; got %+v", got)
	}

	// M2 only: no corpus phrase anywhere, purely the three-conjunct match.
	m2Statement := "a real grooming run against this repo's backlog is recorded"
	if liveTargetCorpusMatch(m2Statement) {
		t.Fatal("premise broken: this statement was chosen because M1 must NOT match it")
	}
	m2 := Verification{AcceptanceCriteria: []AcceptanceCriterion{
		{ID: "a1", Statement: m2Statement, Source: CriterionSourceExplicit, SourceRef: "#2845"},
	}}
	if got := findingsFor(EvaluateAcceptanceCriteria(m2), RuleMissingLiveValidationMarker); len(got) != 1 {
		t.Fatalf("M2 must fire alone; got %+v", got)
	}
}

// (case-insensitivity) Matching is case-insensitive over the statement.
func TestMissingLiveValidationMarker_CaseInsensitive(t *testing.T) {
	v := Verification{
		AcceptanceCriteria: []AcceptanceCriterion{
			{ID: "a1", Statement: "A REAL BACKLOG_GROOMING RUN AGAINST THIS REPOSITORY REACHES ITS APPROVAL GATE", Source: CriterionSourceExplicit, SourceRef: "#2845"},
		},
	}
	if f := findingFor(EvaluateAcceptanceCriteria(v), RuleMissingLiveValidationMarker); f == nil {
		t.Fatal("matching must be case-insensitive")
	}
}

// (shared rule set) The matcher rides EvaluateAcceptanceCriteria, so both
// consumers get it from ONE place. Calling it directly and via the evaluator
// must agree exactly.
func TestEvaluateAcceptanceCriteria_IncludesLiveValidationMarkerFindings(t *testing.T) {
	v := Verification{
		AcceptanceCriteria: []AcceptanceCriterion{
			{ID: "a1", Statement: "A real backlog_grooming run against this repository reaches its approval gate", Source: CriterionSourceExplicit, SourceRef: "#2845"},
		},
	}
	direct := MissingLiveValidationMarker(v)
	viaSet := findingsFor(EvaluateAcceptanceCriteria(v), RuleMissingLiveValidationMarker)
	if len(direct) != 1 || len(viaSet) != 1 || direct[0] != viaSet[0] {
		t.Fatalf("evaluator and matcher disagree: direct=%+v viaSet=%+v", direct, viaSet)
	}
}

// (non-nil contract) The matcher returns [] not nil on a clean plan.
func TestMissingLiveValidationMarker_NonNilOnCleanPlan(t *testing.T) {
	if got := MissingLiveValidationMarker(Verification{}); got == nil {
		t.Fatal("MissingLiveValidationMarker must return a non-nil slice")
	}
}

// (rule name) The wire contract consumers key on.
func TestMissingLiveValidationMarker_RuleName(t *testing.T) {
	if RuleMissingLiveValidationMarker != "missing_live_validation_marker" {
		t.Errorf("rule = %q, want missing_live_validation_marker", RuleMissingLiveValidationMarker)
	}
}
