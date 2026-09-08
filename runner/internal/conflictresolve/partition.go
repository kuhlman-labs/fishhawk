package conflictresolve

import (
	"bytes"
	"errors"
	"sort"
)

// Reason is a NAMED refusal reason. Exactly one is reported per violation, so
// the runner can report to the backend which rule refused a resolution rather
// than a generic failure. The empty Reason means accepted.
type Reason string

// Partition-check reasons.
const (
	// ReasonNone is the accepted case.
	ReasonNone Reason = ""
	// ReasonOutsideHunk means the resolution does not preserve the file's
	// non-conflict segments in order — the agent edited outside a hunk.
	ReasonOutsideHunk Reason = "conflict_resolution_outside_hunk"
	// ReasonResidualMarker means the resolution carries a conflict-marker
	// line that INTERSECTS a replacement region — whether the replacement
	// wrote the whole line or only the bytes that complete one across a
	// segment boundary.
	ReasonResidualMarker Reason = "conflict_resolution_residual_marker"
	// ReasonMalformedMarkers means the captured marker file is not a
	// well-formed sequence of conflict blocks.
	ReasonMalformedMarkers Reason = "conflict_resolution_malformed_markers"
	// ReasonPartitionUndecidable means the segment match exhausted its step
	// budget. Fail closed: an undecidable partition is refused, never
	// accepted.
	ReasonPartitionUndecidable Reason = "conflict_resolution_partition_undecidable"
	// ReasonBinaryConflict means git could not merge the file textually, so
	// there are no markers to partition and no mechanical resolution to
	// confine.
	ReasonBinaryConflict Reason = "conflict_resolution_binary_conflict"
	// ReasonDeleteModifyContent means a delete/modify resolution is neither
	// side's full content nor the deletion.
	ReasonDeleteModifyContent Reason = "conflict_resolution_delete_modify_content"
)

// ErrMalformedMarkers is returned by Partition for a byte slice that is not a
// well-formed sequence of conflict blocks.
var ErrMalformedMarkers = errors.New("conflictresolve: malformed conflict markers")

// partitionStepBudget bounds the segment-matching search. It is a var so the
// budget-exhaustion branch is reachable from a table test without a
// multi-megabyte fixture.
var partitionStepBudget = 1 << 20

// Partition splits a conflicted file's bytes into its ordered NON-CONFLICT
// segments: the text before the first conflict block, between consecutive
// blocks, and after the last one. A file with N conflict blocks yields N+1
// segments, each carrying its own trailing newline, so
// seg0 + block1 + seg1 + ... + blockN + segN reconstructs the input exactly.
//
// Only `<<<<<<<` opens a block. The `|||||||`, `=======` and `>>>>>>>` forms
// are significant only INSIDE one, so a repository whose content legitimately
// begins with a line of seven equals signs partitions as ordinary content
// rather than as a malformed file.
func Partition(markerBytes []byte) ([][]byte, error) {
	const (
		stateOutside = iota
		stateOurs
		stateBase
		stateTheirs
	)
	var (
		segs     [][]byte
		state    = stateOutside
		segStart = 0
		bad      = false
	)
	eachLine(markerBytes, func(l line) {
		if bad {
			return
		}
		kind := ClassifyMarkerLine(l.text)
		switch state {
		case stateOutside:
			if kind == MarkerOurs {
				segs = append(segs, markerBytes[segStart:l.start])
				state = stateOurs
			}
		case stateOurs:
			switch kind {
			case MarkerBase:
				state = stateBase
			case MarkerSep:
				state = stateTheirs
			case MarkerOurs, MarkerTheirs:
				bad = true
			}
		case stateBase:
			switch kind {
			case MarkerSep:
				state = stateTheirs
			case MarkerOurs, MarkerBase, MarkerTheirs:
				bad = true
			}
		case stateTheirs:
			switch kind {
			case MarkerTheirs:
				segStart = l.end
				state = stateOutside
			case MarkerOurs, MarkerBase, MarkerSep:
				bad = true
			}
		}
	})
	if bad || state != stateOutside {
		return nil, ErrMalformedMarkers
	}
	return append(segs, markerBytes[segStart:]), nil
}

// AcceptResolution decides whether resolvedBytes is a confined resolution of
// the conflicted file git wrote as markerBytes.
//
// It accepts iff resolvedBytes == seg0 + X1 + seg1 + ... + XN + segN for some
// replacement regions X1..XN, AND no conflict-marker line of the RESULTING
// FILE intersects any Xi. That is the hunk-level confinement property: every
// byte the agent wrote lies inside a region git itself marked as conflicted,
// every byte git did NOT mark is preserved verbatim and in order, and no
// opening marker survives into the committed file.
//
// The residual-marker rule is evaluated over the PHYSICAL LINES OF THE WHOLE
// RESOLUTION rather than over the substring resolvedBytes[p:q], because a
// marker line can be assembled ACROSS a replacement/segment boundary: a
// replacement contributing a lone `<` immediately before a preserved
// `<<<<<< HEAD` line yields `<<<<<<< HEAD` in the committed file while
// neither piece carries a marker on its own (#3202 review). A marker line
// lying WHOLLY inside a preserved segment is still accepted — a repository
// may legitimately contain a line of seven equals signs.
//
// The search is a reachable-position sweep rather than a greedy scan, because
// a segment may occur more than once in the resolution and an earlier match
// can be the wrong one. It fails CLOSED when its step budget is exhausted.
func AcceptResolution(markerBytes, resolvedBytes []byte) Reason {
	segs, err := Partition(markerBytes)
	if err != nil {
		return ReasonMalformedMarkers
	}
	if len(segs) == 1 {
		// No conflict block: there is no hunk to edit, so only the captured
		// bytes themselves are a confined resolution.
		if bytes.Equal(resolvedBytes, segs[0]) {
			return ReasonNone
		}
		return ReasonOutsideHunk
	}
	if !bytes.HasPrefix(resolvedBytes, segs[0]) {
		return ReasonOutsideHunk
	}

	steps := 0
	reach := map[int]bool{len(segs[0]): true}
	sawMarker := false
	marks := markerLineSpans(resolvedBytes)
	for i := 1; i < len(segs); i++ {
		next := make(map[int]bool)
		for p := range reach {
			for off := p; off <= len(resolvedBytes); {
				steps++
				if steps > partitionStepBudget {
					return ReasonPartitionUndecidable
				}
				idx := bytes.Index(resolvedBytes[off:], segs[i])
				if idx < 0 {
					break
				}
				q := off + idx
				if marks.touches(p, q) {
					sawMarker = true
				} else {
					next[q+len(segs[i])] = true
				}
				off = q + 1
			}
		}
		reach = next
		if len(reach) == 0 {
			break
		}
	}
	if reach[len(resolvedBytes)] {
		return ReasonNone
	}
	if sawMarker {
		return ReasonResidualMarker
	}
	return ReasonOutsideHunk
}

// markerSpans holds the [start,end) byte spans of a resolution's
// conflict-marker physical lines, in ascending order. Spans are disjoint, so
// both bounds increase monotonically and an intersection test is one binary
// search.
type markerSpans [][2]int

// markerLineSpans records every conflict-marker physical line of b.
func markerLineSpans(b []byte) markerSpans {
	var out markerSpans
	eachLine(b, func(l line) {
		if ClassifyMarkerLine(l.text) != MarkerNone {
			out = append(out, [2]int{l.start, l.end})
		}
	})
	return out
}

// touches reports whether any recorded marker line INTERSECTS the half-open
// replacement span [p, q).
//
// The degenerate p == q case is deliberately not vacuous: an EMPTY
// replacement between two preserved segments still admits a marker line that
// straddles their junction, and the same predicate catches it, because a line
// with start < q == p and end > p is exactly a line crossing position p.
func (m markerSpans) touches(p, q int) bool {
	i := sort.Search(len(m), func(i int) bool { return m[i][1] > p })
	return i < len(m) && m[i][0] < q
}
