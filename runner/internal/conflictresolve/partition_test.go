package conflictresolve

import (
	"errors"
	"fmt"
	"testing"
)

// oneBlock is the ordinary two-sided conflict git writes with the default
// conflict style.
const oneBlock = "before\n" +
	"<<<<<<< HEAD\n" +
	"ours\n" +
	"=======\n" +
	"theirs\n" +
	">>>>>>> base\n" +
	"after\n"

// oneBlockDiff3 is the same conflict under merge.conflictStyle=diff3, which adds
// the common-ancestor section. The partition must RECOGNISE the `|||||||` form
// without REQUIRING it, so both fixtures yield the same segments.
const oneBlockDiff3 = "before\n" +
	"<<<<<<< HEAD\n" +
	"ours\n" +
	"||||||| merged common ancestors\n" +
	"orig\n" +
	"=======\n" +
	"theirs\n" +
	">>>>>>> base\n" +
	"after\n"

// operatorFixture is the exact byte sequence the operator reproduced round 1's
// residual-marker defect with (#3202, planning constraints round 2). It
// partitions into "before\n" and "<<<<<< HEAD\n" — the trailing line carries SIX
// `<`, so it is ordinary content, not a marker.
const operatorFixture = "before\n" +
	"<<<<<<< HEAD\n" +
	"ours\n" +
	"=======\n" +
	"theirs\n" +
	">>>>>>> base\n" +
	"<<<<<< HEAD\n"

func segmentStrings(t *testing.T, body string) ([]string, int) {
	t.Helper()
	sp, err := Partition([]byte(body))
	if err != nil {
		t.Fatalf("Partition(%q): unexpected error %v", body, err)
	}
	out := make([]string, len(sp.Segments))
	for i, s := range sp.Segments {
		out[i] = string(s)
	}
	return out, sp.Blocks
}

func TestPartition(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		blocks int
		want   []string
	}{
		{"no conflict at all", "alpha\nbeta\n", 0, []string{"alpha\nbeta\n"}},
		{"empty file", "", 0, []string{""}},
		{"one block", oneBlock, 1, []string{"before\n", "after\n"}},
		{"one block diff3 style", oneBlockDiff3, 1, []string{"before\n", "after\n"}},
		{
			"two blocks",
			"a\n<<<<<<< H\n1\n=======\n2\n>>>>>>> B\nb\n<<<<<<< H\n3\n=======\n4\n>>>>>>> B\nc\n",
			2,
			[]string{"a\n", "b\n", "c\n"},
		},
		{
			"block at file start and end",
			"<<<<<<< H\n1\n=======\n2\n>>>>>>> B\n",
			1,
			[]string{"", ""},
		},
		{
			"unterminated final line",
			"before\n<<<<<<< H\n1\n=======\n2\n>>>>>>> B\ntail",
			1,
			[]string{"before\n", "tail"},
		},
		{
			// Outside a block only the opening marker is significant: a bare
			// separator line in ordinary content (a reStructuredText underline,
			// say) is content, not a malformed sequence.
			"stray separator outside a block is content",
			"title\n=======\nbody\n",
			0,
			[]string{"title\n=======\nbody\n"},
		},
		{
			"stray closing marker outside a block is content",
			"a\n>>>>>>> x\nb\n",
			0,
			[]string{"a\n>>>>>>> x\nb\n"},
		},
		{
			"stray base marker outside a block is content",
			"a\n||||||| x\nb\n",
			0,
			[]string{"a\n||||||| x\nb\n"},
		},
		{
			"eight character run is not a marker",
			"a\n<<<<<<<< H\nb\n",
			0,
			[]string{"a\n<<<<<<<< H\nb\n"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, blocks := segmentStrings(t, tc.body)
			if blocks != tc.blocks {
				t.Errorf("blocks = %d, want %d", blocks, tc.blocks)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("segments = %q, want %q", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("segment %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
			if len(got) != blocks+1 {
				t.Errorf("segments = %d, want blocks+1 = %d", len(got), blocks+1)
			}
		})
	}
}

func TestPartitionMalformed(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"nested opening inside ours", "<<<<<<< a\n<<<<<<< b\n"},
		{"closing marker while in ours", "<<<<<<< a\nx\n>>>>>>> b\n"},
		{"repeated base marker", "<<<<<<< a\n||||||| b\n||||||| c\n"},
		{"opening marker while in base", "<<<<<<< a\n||||||| b\n<<<<<<< c\n"},
		{"closing marker while in base", "<<<<<<< a\n||||||| b\n>>>>>>> c\n"},
		{"opening marker while in theirs", "<<<<<<< a\n=======\n<<<<<<< b\n"},
		{"base marker while in theirs", "<<<<<<< a\n=======\n||||||| b\n"},
		{"repeated separator while in theirs", "<<<<<<< a\n=======\n=======\n"},
		{"block left open at end of file", "before\n<<<<<<< a\nours\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sp, err := Partition([]byte(tc.body))
			if !errors.Is(err, ErrMalformedMarkers) {
				t.Fatalf("Partition(%q) err = %v, want ErrMalformedMarkers", tc.body, err)
			}
			if sp.Segments != nil || sp.Blocks != 0 {
				t.Errorf("malformed partition returned %+v, want the zero value", sp)
			}
		})
	}
}

func TestAcceptResolution(t *testing.T) {
	cases := []struct {
		name     string
		original string
		resolved string
		want     Reason
	}{
		{"keep ours", oneBlock, "before\nours\nafter\n", ReasonNone},
		{"keep theirs", oneBlock, "before\ntheirs\nafter\n", ReasonNone},
		{"hand merged hunk", oneBlock, "before\nours and theirs\nafter\n", ReasonNone},
		{"drop the hunk entirely", oneBlock, "before\nafter\n", ReasonNone},
		{"diff3 fixture keeps ours", oneBlockDiff3, "before\nours\nafter\n", ReasonNone},
		{"unchanged file with no conflict", "alpha\n", "alpha\n", ReasonNone},

		{"edit inside a preserved segment", oneBlock, "beforeX\nours\nafter\n", ReasonOutsideHunk},
		{"leading preserved segment deleted", oneBlock, "ours\nafter\n", ReasonOutsideHunk},
		{"trailing preserved segment deleted", oneBlock, "before\nours\n", ReasonOutsideHunk},
		{"content appended past the last segment", oneBlock, "before\nours\nafter\nextra\n", ReasonOutsideHunk},
		{"unrelated rewrite of a clean file", "alpha\n", "beta\n", ReasonOutsideHunk},

		{
			"live marker left in the replacement region",
			oneBlock,
			"before\n<<<<<<< HEAD\nours\nafter\n",
			ReasonResidualMarker,
		},
		{
			// The round-1 defect, reproduced byte for byte. The only replacement
			// region here is the SINGLE byte `<` between "before\n" and the
			// preserved "<<<<<< HEAD\n" segment, so a per-replacement marker scan
			// sees nothing; the assembled result carries a live opening marker.
			"operator fixture: assembled result carries a live opening marker",
			operatorFixture,
			"before\n<<<<<<< HEAD\n",
			ReasonResidualMarker,
		},
		{
			// Control for the case above: a seven-character run lying WHOLLY
			// inside a preserved segment came from the file's own content and
			// must stay ACCEPTED.
			"marker line wholly inside a preserved segment is accepted",
			"title\n=======\n<<<<<<< HEAD\nours\n=======\ntheirs\n>>>>>>> base\nafter\n",
			"title\n=======\nours\nafter\n",
			ReasonNone,
		},

		{"malformed original", "<<<<<<< a\nours\n", "anything\n", ReasonMalformedMarkers},

		{
			// A segment can occur more than once and the FIRST occurrence can be
			// the wrong one: a greedy scan would commit to the "xx\n" at offset 0
			// and then fail to end at the file's end.
			"repeated segment where the earlier match is wrong",
			"<<<<<<< H\nA\n=======\nB\n>>>>>>> x\nxx\n",
			"xx\nxx\n",
			ReasonNone,
		},
		{
			// Exercises the sweep carrying a reachable position that is PAST an
			// occurrence of the next segment: the "zz\n" at offset 0 is
			// unreachable and must be skipped, not accepted.
			"occurrence before the reachable frontier is skipped",
			"<<<<<<< H\nA\n=======\nB\n>>>>>>> x\nxx\n<<<<<<< H\nC\n=======\nD\n>>>>>>> x\nzz\n",
			"zz\nxx\nzz\n",
			ReasonNone,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AcceptResolution([]byte(tc.original), []byte(tc.resolved)); got != tc.want {
				t.Errorf("AcceptResolution(%q, %q) = %q, want %q", tc.original, tc.resolved, got, tc.want)
			}
		})
	}
}

func TestAcceptResolutionStepBudgetFailsClosed(t *testing.T) {
	// A resolution the real budget ACCEPTS must FAIL CLOSED, not fall through to
	// an accept, when the sweep cannot finish. The budget is shared across both
	// sweeps of one call, so budget 1 exhausts inside the first (plain) sweep and
	// budget 2 exhausts inside the second (marker-aware) one: both
	// ReasonPartitionUndecidable arms get a case.
	for _, budget := range []int{1, 2} {
		t.Run(fmt.Sprintf("budget %d", budget), func(t *testing.T) {
			orig := partitionStepBudget
			t.Cleanup(func() { partitionStepBudget = orig })
			partitionStepBudget = budget

			if got := AcceptResolution([]byte(oneBlock), []byte(resolvedOurs)); got != ReasonPartitionUndecidable {
				t.Fatalf("AcceptResolution under budget %d = %q, want %q", budget, got, ReasonPartitionUndecidable)
			}
		})
	}
}

func TestReachableFrom(t *testing.T) {
	markers := []lineSpan{{Start: 10, End: 17, Next: 18}}
	cases := []struct {
		name    string
		reach   []int
		at      int
		markers []lineSpan
		want    bool
	}{
		{"clear replacement region", []int{4}, 8, markers, true},
		{"only reachable position is blocked by a marker", []int{4}, 20, markers, false},
		{
			// The LARGEST p <= at is the one examined: a smaller p only widens
			// the region, so it can never unblock what the largest is blocked on.
			"later reachable position clears what an earlier one would not",
			[]int{4, 18}, 20, markers, true,
		},
		{
			// Defensive: every reachable position is past the occurrence, so the
			// occurrence is unreachable and must be skipped, never accepted.
			"every reachable position is past the occurrence",
			[]int{20, 30}, 8, markers, false,
		},
		{"no reachable positions at all", nil, 8, markers, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := reachableFrom(tc.reach, tc.at, tc.markers); got != tc.want {
				t.Errorf("reachableFrom(%v, %d) = %v, want %v", tc.reach, tc.at, got, tc.want)
			}
		})
	}
}

func TestMarkerCrosses(t *testing.T) {
	markers := []lineSpan{{Start: 10, End: 17, Next: 18}}
	cases := []struct {
		name string
		a, b int
		want bool
	}{
		{"region wholly before the marker", 0, 10, false},
		{"region wholly after the marker", 18, 20, false},
		{"region overlapping the marker start", 8, 12, true},
		{"region covering the marker newline only", 17, 18, true},
		{"zero length junction strictly inside the marker", 12, 12, true},
		{"zero length junction at the marker start", 10, 10, false},
		{"zero length junction at the marker end", 18, 18, false},
		{"no markers at all", 0, 50, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ms := markers
			if tc.name == "no markers at all" {
				ms = nil
			}
			if got := markerCrosses(tc.a, tc.b, ms); got != tc.want {
				t.Errorf("markerCrosses(%d, %d) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}
