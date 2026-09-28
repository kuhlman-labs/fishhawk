package decisionindex

import (
	"sort"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
)

// TestDecisionBearingCategories_AllRegistered pins every decision-bearing
// category against the curated audit registry, so a renamed or retired category
// is caught here instead of silently ceasing to be indexed.
// COUNTERFACTUAL: add an unregistered key to decisionBearing — RED.
func TestDecisionBearingCategories_AllRegistered(t *testing.T) {
	for _, c := range DecisionBearingCategories() {
		if !audit.IsKnownCategory(c) {
			t.Errorf("decision-bearing category %q is not in audit.KnownCategories", c)
		}
	}
}

// TestDecisionBearingCategories_ClosedNineMemberSet pins the closed set: nine
// categories, sorted, each mapping to a DISTINCT class, and the returned slice
// is a fresh copy a caller cannot use to mutate the set.
func TestDecisionBearingCategories_ClosedNineMemberSet(t *testing.T) {
	want := map[string]DecisionClass{
		"approval_submitted":             ClassPlanApproval,
		"concern_waived":                 ClassConcernWaive,
		"concern_deferred":               ClassConcernDefer,
		"concern_addressed_by_condition": ClassConcernAddressedByCondition,
		"scope_amendment_decided":        ClassScopeAmendment,
		"acceptance_triage_arbitrated":   ClassAcceptanceArbitration,
		"merge_verdict_recorded":         ClassMergeVerdict,
		"clarification_answered":         ClassClarification,
		"grooming_disposition_recorded":  ClassGroomingDisposition,
	}
	got := DecisionBearingCategories()
	if len(got) != len(want) {
		t.Fatalf("DecisionBearingCategories() = %v (%d), want %d members", got, len(got), len(want))
	}
	if !sort.StringsAreSorted(got) {
		t.Errorf("DecisionBearingCategories() = %v, want sorted", got)
	}
	seen := map[DecisionClass]string{}
	for _, c := range got {
		class, ok := ClassFor(c)
		if !ok || class != want[c] {
			t.Errorf("ClassFor(%q) = %q, %v; want %q, true", c, class, ok, want[c])
		}
		if prev, dup := seen[class]; dup {
			t.Errorf("class %q is shared by %q and %q; each class maps to exactly one category", class, prev, c)
		}
		seen[class] = c
	}
	got[0] = "mutated"
	if DecisionBearingCategories()[0] == "mutated" {
		t.Error("DecisionBearingCategories() returned the shared backing slice")
	}
}

// TestIsDecisionBearing covers both sides of the predicate.
// COUNTERFACTUAL: make IsDecisionBearing return true unconditionally — RED on
// the non-decision rows.
func TestIsDecisionBearing(t *testing.T) {
	for _, c := range DecisionBearingCategories() {
		if !IsDecisionBearing(c) {
			t.Errorf("IsDecisionBearing(%q) = false, want true", c)
		}
	}
	for _, c := range []string{"run_started", "escalation_fired", "approval_conditions_truncated", ""} {
		if IsDecisionBearing(c) {
			t.Errorf("IsDecisionBearing(%q) = true, want false", c)
		}
		if _, ok := ClassFor(c); ok {
			t.Errorf("ClassFor(%q) ok = true, want false", c)
		}
	}
}
