package wavecoverage_test

import (
	"reflect"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/wavecoverage"
)

// TestCovered is the table that pins every enumerated mode of the shared
// coverage predicate, independent of all three callers. The predicate is the
// single source of truth for "are this dependent child's predecessors merged?",
// so each mode is asserted here rather than only through a caller's fixture.
func TestCovered(t *testing.T) {
	cases := []struct {
		name          string
		dependsOn     []int
		sliceRunID    map[int]string
		integrated    []string
		wantCovered   bool
		wantMissing   []int
		modeRationale string
	}{
		{
			name:          "empty depends_on is covered",
			dependsOn:     nil,
			sliceRunID:    map[int]string{0: "run-0"},
			integrated:    nil,
			wantCovered:   true,
			wantMissing:   nil,
			modeRationale: "wave-0 / no-dependency child: nothing to wait for",
		},
		{
			name:          "every dependency run id present is covered",
			dependsOn:     []int{0, 1},
			sliceRunID:    map[int]string{0: "run-0", 1: "run-1", 2: "run-2"},
			integrated:    []string{"run-0", "run-1"},
			wantCovered:   true,
			wantMissing:   nil,
			modeRationale: "the newest integration merged both predecessors",
		},
		{
			name:          "one dependency run id absent is not covered",
			dependsOn:     []int{0, 1, 2},
			sliceRunID:    map[int]string{0: "run-0", 1: "run-1", 2: "run-2"},
			integrated:    []string{"run-0", "run-2"},
			wantCovered:   false,
			wantMissing:   []int{1},
			modeRationale: "the STALE-integration case: an entry exists but misses slice 1",
		},
		{
			name:          "dependency slice with no minted sibling is not covered and is reported",
			dependsOn:     []int{0, 3},
			sliceRunID:    map[int]string{0: "run-0"},
			integrated:    []string{"run-0"},
			wantCovered:   false,
			wantMissing:   []int{3},
			modeRationale: "fail closed rather than depend on the wave-order guard running first",
		},
		{
			name:          "integrated set of unrelated run ids only is not covered",
			dependsOn:     []int{1, 0},
			sliceRunID:    map[int]string{0: "run-0", 1: "run-1"},
			integrated:    []string{"someone-elses-run", "another-run"},
			wantCovered:   false,
			wantMissing:   []int{0, 1},
			modeRationale: "missing is ASCENDING even though depends_on was given descending",
		},
		{
			name:          "duplicate dependency index is reported once",
			dependsOn:     []int{2, 2},
			sliceRunID:    map[int]string{2: "run-2"},
			integrated:    nil,
			wantCovered:   false,
			wantMissing:   []int{2},
			modeRationale: "missing is duplicate-free so an operator payload reads cleanly",
		},
		{
			name:          "minted sibling with an empty run id is not covered",
			dependsOn:     []int{0},
			sliceRunID:    map[int]string{0: ""},
			integrated:    []string{""},
			wantCovered:   false,
			wantMissing:   []int{0},
			modeRationale: "an empty run id must never match an empty entry in the integrated set",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			covered, missing := wavecoverage.Covered(tc.dependsOn, tc.sliceRunID, tc.integrated)
			if covered != tc.wantCovered {
				t.Errorf("covered = %v, want %v (%s)", covered, tc.wantCovered, tc.modeRationale)
			}
			if !reflect.DeepEqual(missing, tc.wantMissing) {
				t.Errorf("missing = %v, want %v (%s)", missing, tc.wantMissing, tc.modeRationale)
			}
		})
	}
}

// TestCovered_DoesNotMutateDependsOn pins that the predicate never sorts the
// caller's slice in place: dependsOn aliases the parent's approved plan, which
// the server reads concurrently for other children.
func TestCovered_DoesNotMutateDependsOn(t *testing.T) {
	deps := []int{2, 0, 1}
	wavecoverage.Covered(deps, map[int]string{}, nil)
	if want := []int{2, 0, 1}; !reflect.DeepEqual(deps, want) {
		t.Errorf("dependsOn mutated to %v, want %v left untouched", deps, want)
	}
}
