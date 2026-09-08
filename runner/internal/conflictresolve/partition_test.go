package conflictresolve

import (
	"errors"
	"testing"
)

const (
	// twoWay is the default-conflictStyle presentation.
	twoWay = "alpha\nbeta\n" +
		"<<<<<<< HEAD\nours\n=======\ntheirs\n>>>>>>> origin/main\n" +
		"gamma\n"
	// diff3 carries the common-ancestor section.
	diff3 = "alpha\n" +
		"<<<<<<< HEAD\nours\n||||||| merged common ancestors\nbase\n=======\ntheirs\n>>>>>>> origin/main\n" +
		"omega\n"
	// twoBlocks has two conflict blocks and therefore three segments.
	twoBlocks = "head\n" +
		"<<<<<<< HEAD\na-ours\n=======\na-theirs\n>>>>>>> origin/main\n" +
		"middle\n" +
		"<<<<<<< HEAD\nb-ours\n=======\nb-theirs\n>>>>>>> origin/main\n" +
		"tail\n"
	// leadingSep opens with a line that LOOKS like a separator but is content:
	// nothing has opened a conflict block yet.
	leadingSep = "=======\nalpha\n" +
		"<<<<<<< HEAD\nours\n=======\ntheirs\n>>>>>>> origin/main\n" +
		"end\n"
	// crlf is a CRLF file: git wrote the markers with the file's own endings.
	crlf = "alpha\r\n" +
		"<<<<<<< HEAD\r\nours\r\n=======\r\ntheirs\r\n>>>>>>> origin/main\r\n" +
		"gamma\r\n"
)

func TestPartition(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"two-way", twoWay, []string{"alpha\nbeta\n", "gamma\n"}},
		{"diff3", diff3, []string{"alpha\n", "omega\n"}},
		{"two blocks", twoBlocks, []string{"head\n", "middle\n", "tail\n"}},
		{"leading separator is content", leadingSep, []string{"=======\nalpha\n", "end\n"}},
		{"crlf", crlf, []string{"alpha\r\n", "gamma\r\n"}},
		{"no conflict block", "alpha\nbeta\n", []string{"alpha\nbeta\n"}},
		{"empty file", "", []string{""}},
		{"empty leading and trailing segments", "<<<<<<< HEAD\no\n=======\nt\n>>>>>>> x\n", []string{"", ""}},
		{"stray theirs outside a block is content", ">>>>>>> stray\nalpha\n", []string{">>>>>>> stray\nalpha\n"}},
		{"final line unterminated", "alpha\n<<<<<<< HEAD\no\n=======\nt\n>>>>>>> x\ntail", []string{"alpha\n", "tail"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Partition([]byte(tc.in))
			if err != nil {
				t.Fatalf("Partition: unexpected error %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("Partition returned %d segments %q, want %d %q", len(got), asStrings(got), len(tc.want), tc.want)
			}
			for i := range got {
				if string(got[i]) != tc.want[i] {
					t.Fatalf("segment %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestPartitionRejectsMalformed(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"nested ours", "<<<<<<< HEAD\n<<<<<<< HEAD\no\n=======\nt\n>>>>>>> x\n"},
		{"theirs before separator", "<<<<<<< HEAD\no\n>>>>>>> x\n"},
		{"double base section", "<<<<<<< HEAD\no\n||||||| b\nb1\n||||||| b\n=======\nt\n>>>>>>> x\n"},
		{"ours inside base section", "<<<<<<< HEAD\no\n||||||| b\n<<<<<<< HEAD\n=======\nt\n>>>>>>> x\n"},
		{"theirs inside base section", "<<<<<<< HEAD\no\n||||||| b\n>>>>>>> x\n"},
		{"second separator inside theirs", "<<<<<<< HEAD\no\n=======\nt\n=======\n>>>>>>> x\n"},
		{"ours inside theirs", "<<<<<<< HEAD\no\n=======\n<<<<<<< HEAD\n>>>>>>> x\n"},
		{"base inside theirs", "<<<<<<< HEAD\no\n=======\n||||||| b\n>>>>>>> x\n"},
		{"unterminated block", "alpha\n<<<<<<< HEAD\no\n=======\nt\n"},
		{"unterminated at ours", "alpha\n<<<<<<< HEAD\no\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Partition([]byte(tc.in)); !errors.Is(err, ErrMalformedMarkers) {
				t.Fatalf("Partition(%q) error = %v, want ErrMalformedMarkers", tc.in, err)
			}
			if got := AcceptResolution([]byte(tc.in), []byte("anything")); got != ReasonMalformedMarkers {
				t.Fatalf("AcceptResolution = %q, want %q", got, ReasonMalformedMarkers)
			}
		})
	}
}

func TestAcceptResolution(t *testing.T) {
	tests := []struct {
		name     string
		marker   string
		resolved string
		want     Reason
	}{
		{"keeps ours", twoWay, "alpha\nbeta\nours\ngamma\n", ReasonNone},
		{"keeps theirs", twoWay, "alpha\nbeta\ntheirs\ngamma\n", ReasonNone},
		{"keeps both, hand merged", twoWay, "alpha\nbeta\nours\ntheirs\ngamma\n", ReasonNone},
		{"keeps neither, empty hunk", twoWay, "alpha\nbeta\ngamma\n", ReasonNone},
		{"writes novel content inside the hunk", twoWay, "alpha\nbeta\nreconciled\ngamma\n", ReasonNone},
		{"diff3 keeps the base side", diff3, "alpha\nbase\nomega\n", ReasonNone},
		{"two blocks resolved independently", twoBlocks, "head\na-ours\nmiddle\nb-theirs\ntail\n", ReasonNone},
		{"crlf keeps ours", crlf, "alpha\r\nours\r\ngamma\r\n", ReasonNone},
		{"segment that legitimately looks like a marker is preserved", leadingSep, "=======\nalpha\nours\nend\n", ReasonNone},
		{"segment text also appearing inside the hunk", twoWay, "alpha\nbeta\ngamma\nextra\ngamma\n", ReasonNone},
		{"no conflict block, byte-identical", "alpha\nbeta\n", "alpha\nbeta\n", ReasonNone},

		{"drops a non-conflict segment", twoWay, "alpha\nbeta\nours\n", ReasonOutsideHunk},
		{"drops the leading segment", twoWay, "ours\ngamma\n", ReasonOutsideHunk},
		{"reorders the segments", twoWay, "gamma\nours\nalpha\nbeta\n", ReasonOutsideHunk},
		{"mutates a non-conflict segment by one byte", twoWay, "alpha\nbetb\nours\ngamma\n", ReasonOutsideHunk},
		{"edits the middle segment of a two-block file", twoBlocks, "head\na-ours\nmiddle-edited\nb-ours\ntail\n", ReasonOutsideHunk},
		{"appends after the trailing segment", twoWay, "alpha\nbeta\nours\ngamma\nappended\n", ReasonOutsideHunk},
		{"no conflict block, any edit at all", "alpha\nbeta\n", "alpha\nbeta\ngamma\n", ReasonOutsideHunk},

		{"leaves the file exactly as git wrote it", twoWay, twoWay, ReasonResidualMarker},
		{"leaves a separator inside the hunk", twoWay, "alpha\nbeta\nours\n=======\ntheirs\ngamma\n", ReasonResidualMarker},
		{"leaves a diff3 base marker inside the hunk", diff3, diff3, ReasonResidualMarker},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := AcceptResolution([]byte(tc.marker), []byte(tc.resolved)); got != tc.want {
				t.Fatalf("AcceptResolution = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAcceptResolution_RejectsMutatedNonConflictSegment is the counterfactual
// vehicle for the partition check: the mutated segment is seeded BY
// CONSTRUCTION, not by calling the control in setup.
func TestAcceptResolution_RejectsMutatedNonConflictSegment(t *testing.T) {
	if got := AcceptResolution([]byte(twoWay), []byte("alpha\nbetb\nours\ngamma\n")); got != ReasonOutsideHunk {
		t.Fatalf("AcceptResolution = %q, want %q", got, ReasonOutsideHunk)
	}
}

// TestAcceptResolution_RejectsResidualMarker pairs the marker file with
// ITSELF, so a byte-exact comparison alone cannot refuse it: every segment is
// preserved in order and only the residual-marker rule can reject.
func TestAcceptResolution_RejectsResidualMarker(t *testing.T) {
	if got := AcceptResolution([]byte(twoWay), []byte(twoWay)); got != ReasonResidualMarker {
		t.Fatalf("AcceptResolution = %q, want %q", got, ReasonResidualMarker)
	}
}

func TestAcceptResolution_FailsClosedWhenBudgetExhausted(t *testing.T) {
	prev := partitionStepBudget
	partitionStepBudget = 1
	t.Cleanup(func() { partitionStepBudget = prev })

	if got := AcceptResolution([]byte(twoWay), []byte("alpha\nbeta\nours\ngamma\n")); got != ReasonPartitionUndecidable {
		t.Fatalf("AcceptResolution = %q, want %q", got, ReasonPartitionUndecidable)
	}
}

func asStrings(in [][]byte) []string {
	out := make([]string, len(in))
	for i := range in {
		out[i] = string(in[i])
	}
	return out
}
