package conflictresolve

import (
	"bytes"
	"errors"
)

// Reason is the single named refusal reason a rule contributes. It is the
// string the runner reports to the backend, so every value is stable wire text.
type Reason string

// ReasonNone is the accept verdict — no rule was broken.
const ReasonNone Reason = ""

// Partition-side reasons.
const (
	// ReasonOutsideHunk is an edit to a preserved (non-conflict) segment.
	ReasonOutsideHunk Reason = "conflict_resolution_edit_outside_hunk"
	// ReasonResidualMarker is a conflict-marker line surviving in the
	// ASSEMBLED result at a position a replacement region reaches.
	ReasonResidualMarker Reason = "conflict_resolution_residual_marker"
	// ReasonMalformedMarkers is a conflict-marker sequence git would not have
	// written — the file cannot be partitioned, so nothing can be authorized.
	ReasonMalformedMarkers Reason = "conflict_resolution_malformed_markers"
	// ReasonPartitionUndecidable is the fail-closed verdict when the accept
	// sweep exhausts its step budget.
	ReasonPartitionUndecidable Reason = "conflict_resolution_partition_undecidable"
)

// ErrMalformedMarkers is returned by Partition for a marker sequence git would
// not have produced.
var ErrMalformedMarkers = errors.New("conflictresolve: malformed conflict-marker sequence")

// errStepBudget is internal: it surfaces as ReasonPartitionUndecidable.
var errStepBudget = errors.New("conflictresolve: partition step budget exhausted")

// partitionStepBudget bounds the accept sweep. The budget is spent across BOTH
// sweeps of one AcceptResolution call, so the total work per file is bounded
// rather than the work per sweep. It is a var so tests can shrink it to drive
// the fail-closed ReasonPartitionUndecidable branch.
var partitionStepBudget = 100000

// Split is a conflicted file broken into its ordered NON-CONFLICT segments.
// A file carrying N conflict blocks yields exactly N+1 segments (a file with no
// block yields one segment holding the whole file).
type Split struct {
	Segments [][]byte
	Blocks   int
}

// partition state machine states.
const (
	stOutside = iota
	stOurs
	stBase
	stTheirs
)

// Partition splits a conflicted file's bytes at the conflict blocks git wrote.
//
// OUTSIDE a block only the opening `<<<<<<<` line is significant: a bare
// `=======` or `>>>>>>>` line in ordinary content (a reStructuredText underline,
// say) is content, not a malformed sequence. INSIDE a block every transition git
// would not have written returns ErrMalformedMarkers, as does a block left open
// at end of file.
func Partition(b []byte) (Split, error) {
	state := stOutside
	segStart := 0
	var out Split
	var perr error

	eachLine(b, func(sp lineSpan) bool {
		kind := ClassifyMarkerLine(b[sp.Start:sp.End])
		switch state {
		case stOutside:
			if kind == MarkerOurs {
				out.Segments = append(out.Segments, b[segStart:sp.Start])
				state = stOurs
			}
		case stOurs:
			switch kind {
			case MarkerBase:
				state = stBase
			case MarkerSeparator:
				state = stTheirs
			case MarkerOurs, MarkerTheirs:
				perr = ErrMalformedMarkers
				return false
			}
		case stBase:
			switch kind {
			case MarkerSeparator:
				state = stTheirs
			case MarkerOurs, MarkerBase, MarkerTheirs:
				perr = ErrMalformedMarkers
				return false
			}
		case stTheirs:
			switch kind {
			case MarkerTheirs:
				state = stOutside
				out.Blocks++
				segStart = sp.Next
			case MarkerOurs, MarkerBase, MarkerSeparator:
				perr = ErrMalformedMarkers
				return false
			}
		}
		return true
	})

	if perr != nil {
		return Split{}, perr
	}
	if state != stOutside {
		return Split{}, ErrMalformedMarkers
	}
	out.Segments = append(out.Segments, b[segStart:])
	return out, nil
}

// AcceptResolution decides whether resolved is a hunk-confined resolution of the
// conflicted bytes git wrote.
//
// It accepts iff resolved == seg0 + X1 + seg1 + ... + XN + segN for some
// replacement regions X1..XN — every preserved segment must survive byte-exact,
// in order, with the first anchored at the start and the last at the end.
//
// The residual-marker rule is a property of the ASSEMBLED RESULT, not of each
// replacement in isolation: the marker-line spans of the whole candidate are
// computed once, and an assembly is refused when a marker line intersects a
// replacement region. A zero-length replacement between two preserved segments
// still admits a marker line straddling their junction, so it is probed too. A
// marker line lying WHOLLY inside a preserved segment is accepted — it came from
// the file's own non-conflict content.
func AcceptResolution(markerBytes, resolved []byte) Reason {
	p, err := Partition(markerBytes)
	if err != nil {
		return ReasonMalformedMarkers
	}
	steps := 0
	plain, err := embeds(p.Segments, resolved, nil, &steps)
	if err != nil {
		return ReasonPartitionUndecidable
	}
	if !plain {
		return ReasonOutsideHunk
	}
	aware, err := embeds(p.Segments, resolved, markerLineSpans(resolved), &steps)
	if err != nil {
		return ReasonPartitionUndecidable
	}
	if !aware {
		return ReasonResidualMarker
	}
	return ReasonNone
}

// embeds runs the reachable-position sweep. It reports whether some assembly of
// segs spells resolved exactly, with every replacement region clear of markers.
//
// It is NOT a greedy scan: a segment may occur more than once and an earlier
// match can be the wrong one, so every occurrence is carried forward as a
// reachable end position and the last segment must land exactly at len(resolved).
func embeds(segs [][]byte, resolved []byte, markers []lineSpan, steps *int) (bool, error) {
	if !bytes.HasPrefix(resolved, segs[0]) {
		return false, nil
	}
	reach := []int{len(segs[0])}

	for _, seg := range segs[1:] {
		var next []int
		for j := reach[0]; j <= len(resolved); {
			*steps++
			if *steps > partitionStepBudget {
				return false, errStepBudget
			}
			k := bytes.Index(resolved[j:], seg)
			if k < 0 {
				break
			}
			at := j + k
			if reachableFrom(reach, at, markers) {
				next = append(next, at+len(seg))
			}
			j = at + 1
		}
		if len(next) == 0 {
			return false, nil
		}
		// next is built in strictly ascending order of `at`, so it is already
		// sorted and duplicate-free.
		reach = next
	}

	for _, r := range reach {
		if r == len(resolved) {
			return true, nil
		}
	}
	return false, nil
}

// reachableFrom reports whether some already-reachable position p <= at leaves a
// replacement region [p, at) clear of marker lines. Only the LARGEST such p is
// examined: a smaller p only widens the region, so it can never unblock one the
// largest p is blocked on.
func reachableFrom(reach []int, at int, markers []lineSpan) bool {
	for i := len(reach) - 1; i >= 0; i-- {
		p := reach[i]
		if p > at {
			continue
		}
		return !markerCrosses(p, at, markers)
	}
	return false
}

// markerCrosses reports whether any marker line intersects the region [a, b).
//
// The half-open overlap test also gives the ZERO-LENGTH region (a == b, two
// preserved segments abutting) exactly the semantics that case needs: it
// degenerates to m.Start < a && a < m.Next, so a junction landing strictly
// INSIDE a marker line refuses, while a junction at that line's first or last
// byte leaves the line wholly within one preserved segment and is accepted. No
// separate branch is needed, and TestMarkerCrosses pins all three positions.
func markerCrosses(a, b int, markers []lineSpan) bool {
	for _, m := range markers {
		if a < m.Next && m.Start < b {
			return true
		}
	}
	return false
}
