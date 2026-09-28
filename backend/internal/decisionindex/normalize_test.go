package decisionindex

import (
	"reflect"
	"testing"
)

// TestNormalizeConcernCategory_Table covers every alias in the seeded table,
// surface variants of each family, and the unmapped posture: an unknown value
// comes back AS ITSELF (surface-normalized) with mapped=false — never dropped,
// never coerced.
// COUNTERFACTUAL: make NormalizeConcernCategory return ("", true) for an
// unknown value — RED on the unmapped rows.
func TestNormalizeConcernCategory_Table(t *testing.T) {
	for alias, canonical := range concernCategoryAliases {
		got, mapped := NormalizeConcernCategory(alias)
		if got != canonical || !mapped {
			t.Errorf("NormalizeConcernCategory(%q) = %q, %v; want %q, true", alias, got, mapped, canonical)
		}
	}
	cases := []struct {
		raw, want string
		mapped    bool
	}{
		{"  Correctness ", "correctness", true},
		{"DATA LOSS", "correctness", true},
		{"data__loss", "correctness", true},
		{"Test_Coverage", "testing", true},
		{"test - coverage", "testing", true},
		{"PERF", "performance", true},
		{"Docs", "documentation", true},
		{"Simplification", "maintainability", true},
		{"Scoping", "scope", true},
		{"flakiness-under-race", "flakiness-under-race", false},
		{"  Flakiness Under_Race ", "flakiness-under-race", false},
		{"", "", false},
		{" _- ", "", false},
	}
	for _, tc := range cases {
		got, mapped := NormalizeConcernCategory(tc.raw)
		if got != tc.want || mapped != tc.mapped {
			t.Errorf("NormalizeConcernCategory(%q) = %q, %v; want %q, %v", tc.raw, got, mapped, tc.want, tc.mapped)
		}
	}
}

// TestNormalizeConcernCategory_SeededFromDeferDefectCategories pins the
// correctness family to backend/internal/server/defer_concern.go's
// deferDefectCategories (the six categories that default a deferred concern to a
// `bug` follow-up). If that map changes, update concernCategoryAliases and this
// list together.
func TestNormalizeConcernCategory_SeededFromDeferDefectCategories(t *testing.T) {
	for _, c := range []string{"correctness", "bug", "security", "logic", "regression", "data-loss"} {
		if got, mapped := NormalizeConcernCategory(c); got != CorrectnessConcernCategory || !mapped {
			t.Errorf("NormalizeConcernCategory(%q) = %q, %v; want correctness, true", c, got, mapped)
		}
	}
}

// TestCanonicalConcernCategories pins the sorted canonical set, and that every
// canonical key is itself a mapped alias (a fixed point of normalization).
func TestCanonicalConcernCategories(t *testing.T) {
	want := []string{"correctness", "documentation", "maintainability", "performance", "scope", "testing"}
	got := CanonicalConcernCategories()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CanonicalConcernCategories() = %v, want %v", got, want)
	}
	for _, c := range got {
		if n, mapped := NormalizeConcernCategory(c); n != c || !mapped {
			t.Errorf("canonical %q normalizes to %q, %v; want itself, true", c, n, mapped)
		}
	}
}
