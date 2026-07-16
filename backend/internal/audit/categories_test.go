package audit

import (
	"sort"
	"testing"
)

// TestIsKnownCategory pins membership for a sample of real canonical
// categories (true) and garbage / wrong-surface strings (false). The
// scope_amendment_pending case is the #1764 reproduction: the runner-log
// event string is NOT a known audit category, so a wait armed on it must be
// rejectable.
func TestIsKnownCategory(t *testing.T) {
	known := []string{
		"scope_amendment_requested",
		"implement_reviewed",
		"plan_reviewed",
		"fixup_pushed",
		"acceptance_outcome_recorded",
		"plan_review_started",
		"plan_review_failed",
		"plan_review_skipped",
		"run_completed",
		"deployment_outcome_recorded",
		"run_revived",
		"merge_verdict_recorded", // E48.7 / #1954 operator merge-verdict chain entry
	}
	for _, c := range known {
		if !IsKnownCategory(c) {
			t.Errorf("IsKnownCategory(%q) = false, want true (canonical category missing from registry)", c)
		}
	}

	unknown := []string{
		"scope_amendment_pending", // the #1764 runner-log event, NOT an audit category
		"implement_review",        // truncated
		"",                        // empty
		"garbage_not_a_category",
		"IMPLEMENT_REVIEWED", // wrong case
	}
	for _, c := range unknown {
		if IsKnownCategory(c) {
			t.Errorf("IsKnownCategory(%q) = true, want false", c)
		}
	}
}

// TestSuggestCategories_RanksNearest is the #1764 reproduction: the
// misspelled/wrong-surface "scope_amendment_pending" must surface the real
// "scope_amendment_requested" among its nearest suggestions, so the operator
// who typed the runner-log event string is pointed at the audit category.
//
// Note the plan's prose asserted "requested first"; with the full registry
// that is mechanically not the closest by Levenshtein — the equally-real
// "scope_amendment_decided" (suffix edit "pending"→"decided" = 6) beats
// "pending"→"requested" (= 8). Both are correct scope_amendment_* siblings
// and both land in the top suggestions, which is the property that matters:
// the fail-loud message names the right family. We assert membership among
// the two nearest rather than distorting the specified metric or dropping a
// genuine category to force a first-place tie.
func TestSuggestCategories_RanksNearest(t *testing.T) {
	got := SuggestCategories("scope_amendment_pending", 3)
	if len(got) == 0 {
		t.Fatal("SuggestCategories returned no suggestions")
	}
	if len(got) > 3 {
		t.Errorf("returned %d suggestions, want <= 3", len(got))
	}
	// scope_amendment_requested is one of the two nearest scope_amendment_*
	// siblings — the operator sees it and fixes the wrong-surface string.
	nearestTwo := got
	if len(nearestTwo) > 2 {
		nearestTwo = nearestTwo[:2]
	}
	found := false
	for _, c := range nearestTwo {
		if c == "scope_amendment_requested" {
			found = true
		}
	}
	if !found {
		t.Errorf("scope_amendment_requested not among the two nearest suggestions; got=%v", got)
	}
}

// TestSuggestCategories_MaxCapAndDeterminism proves the max cap is honored
// and the ranking is deterministic across repeated calls (stable tie-break).
func TestSuggestCategories_MaxCapAndDeterminism(t *testing.T) {
	if got := SuggestCategories("plan_reviewd", 0); got != nil {
		t.Errorf("max=0 must return nil, got %v", got)
	}
	if got := SuggestCategories("plan_reviewd", -1); got != nil {
		t.Errorf("negative max must return nil, got %v", got)
	}

	first := SuggestCategories("plan_reviewd", 5)
	if len(first) != 5 {
		t.Fatalf("max=5 returned %d suggestions, want exactly 5", len(first))
	}
	// A near-miss of "plan_reviewed" should rank it first.
	if first[0] != "plan_reviewed" {
		t.Errorf("nearest to plan_reviewd = %q, want plan_reviewed; full=%v", first[0], first)
	}
	for i := 0; i < 5; i++ {
		again := SuggestCategories("plan_reviewd", 5)
		if len(again) != len(first) {
			t.Fatalf("non-deterministic length: %d vs %d", len(again), len(first))
		}
		for j := range first {
			if again[j] != first[j] {
				t.Fatalf("non-deterministic order at %d: %q vs %q", j, again[j], first[j])
			}
		}
	}

	// A max larger than the registry returns the whole registry, not a panic.
	all := SuggestCategories("x", len(KnownCategories)+50)
	if len(all) != len(KnownCategories) {
		t.Errorf("oversized max returned %d, want the full registry size %d", len(all), len(KnownCategories))
	}
}

// TestKnownCategoryList_NonEmptyAndSorted asserts the exported sorted view
// is non-empty, sorted, and a defensive copy (mutating it does not corrupt
// the shared registry order).
func TestKnownCategoryList_NonEmptyAndSorted(t *testing.T) {
	list := KnownCategoryList()
	if len(list) == 0 {
		t.Fatal("KnownCategoryList is empty")
	}
	if len(list) != len(KnownCategories) {
		t.Errorf("list length %d != map length %d", len(list), len(KnownCategories))
	}
	if !sort.StringsAreSorted(list) {
		t.Errorf("KnownCategoryList is not sorted: %v", list)
	}
	// Every listed entry is a known category and vice versa.
	for _, c := range list {
		if !IsKnownCategory(c) {
			t.Errorf("listed category %q is not IsKnownCategory", c)
		}
	}

	// Mutating the returned slice must not affect a subsequent call.
	list[0] = "zzz_mutated"
	fresh := KnownCategoryList()
	if fresh[0] == "zzz_mutated" {
		t.Error("KnownCategoryList returned a shared backing array; callers can corrupt it")
	}
}
