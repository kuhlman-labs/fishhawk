package spec_test

import (
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

func TestValidAgentVersionRange(t *testing.T) {
	cases := []struct {
		name    string
		rng     string
		wantErr bool
	}{
		{name: "two_bound_range", rng: ">=2.1 <2.2", wantErr: false},
		{name: "single_lower_bound", rng: ">=2.1", wantErr: false},
		{name: "exact_eq", rng: "=3.0.1", wantErr: false},
		{name: "exact_double_eq", rng: "==3.0.1", wantErr: false},
		{name: "full_triple", rng: ">=2.1.5 <3.0.0", wantErr: false},
		{name: "major_only", rng: ">=2", wantErr: false},
		{name: "less_and_greater", rng: ">1.0 <=2.0", wantErr: false},
		{name: "empty", rng: "", wantErr: true},
		{name: "whitespace_only", rng: "   ", wantErr: true},
		{name: "no_operator", rng: "2.1", wantErr: true},
		{name: "bad_version", rng: ">=abc", wantErr: true},
		{name: "four_part_version", rng: ">=1.2.3.4", wantErr: true},
		{name: "trailing_dot", rng: ">=2.", wantErr: true},
		{name: "operator_only", rng: ">=", wantErr: true},
		{name: "one_good_one_bad", rng: ">=2.1 <foo", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := spec.ValidAgentVersionRange(tc.rng)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidAgentVersionRange(%q) err = %v, wantErr = %v", tc.rng, err, tc.wantErr)
			}
		})
	}
}

func TestMatchAgentVersionRange(t *testing.T) {
	cases := []struct {
		name           string
		rng            string
		probed         string
		wantMatched    bool
		wantComparable bool
	}{
		// In range.
		{name: "in_range", rng: ">=2.1 <2.2", probed: "2.1.5", wantMatched: true, wantComparable: true},
		// Free-form probe: extract the first semver token.
		{name: "in_range_freeform", rng: ">=2.1 <2.2", probed: "2.1.5 (Claude Code)", wantMatched: true, wantComparable: true},
		{name: "in_range_prefixed", rng: ">=0.30 <0.31", probed: "codex 0.30.2", wantMatched: true, wantComparable: true},
		// Below the range.
		{name: "below", rng: ">=2.1 <2.2", probed: "2.0.9", wantMatched: false, wantComparable: true},
		// Above the range.
		{name: "above", rng: ">=2.1 <2.2", probed: "2.2.0", wantMatched: false, wantComparable: true},
		// Boundary: >= is inclusive.
		{name: "lower_boundary_inclusive", rng: ">=2.1 <2.2", probed: "2.1.0", wantMatched: true, wantComparable: true},
		// Boundary: < is exclusive.
		{name: "upper_boundary_exclusive", rng: ">=2.1 <2.2", probed: "2.2", wantMatched: false, wantComparable: true},
		// Partial-bound normalization: ">=2.1" == ">=2.1.0".
		{name: "partial_bound_equal", rng: "=2.1", probed: "2.1.0", wantMatched: true, wantComparable: true},
		{name: "partial_bound_not_equal", rng: "=2.1", probed: "2.1.3", wantMatched: false, wantComparable: true},
		// Unextractable probe → uncomparable → callers degrade to proceed.
		{name: "unknown_sentinel", rng: ">=2.1 <2.2", probed: "unknown", wantMatched: false, wantComparable: false},
		{name: "no_digits", rng: ">=2.1 <2.2", probed: "claude (dev build)", wantMatched: false, wantComparable: false},
		{name: "empty_probe", rng: ">=2.1 <2.2", probed: "", wantMatched: false, wantComparable: false},
		// Malformed range → uncomparable (defensive; callers pre-validate).
		{name: "malformed_range", rng: ">=foo", probed: "2.1.5", wantMatched: false, wantComparable: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			matched, comparable, _ := spec.MatchAgentVersionRange(tc.rng, tc.probed)
			if matched != tc.wantMatched || comparable != tc.wantComparable {
				t.Errorf("MatchAgentVersionRange(%q, %q) = (matched=%v, comparable=%v), want (matched=%v, comparable=%v)",
					tc.rng, tc.probed, matched, comparable, tc.wantMatched, tc.wantComparable)
			}
		})
	}
}

// TestMatchAgentVersionRange_MalformedRangeReturnsErr asserts the defensive
// err return fires on a malformed range (callers validate at parse time, so
// this is a belt-and-suspenders path).
func TestMatchAgentVersionRange_MalformedRangeReturnsErr(t *testing.T) {
	_, _, err := spec.MatchAgentVersionRange(">=foo", "2.1.5")
	if err == nil {
		t.Error("MatchAgentVersionRange with a malformed range: err = nil, want non-nil")
	}
}
