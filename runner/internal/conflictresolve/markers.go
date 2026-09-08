// Package conflictresolve holds the PURE decision logic behind the runner's
// conflict-resolution confinement gate (#3202): git conflict-marker
// recognition, the hunk-level partition check that bounds an agent's
// resolution to the conflicted hunks, and baseline verification over
// captured merge-state structs.
//
// The package runs no git and performs no I/O. Every input is a captured
// value, so the gate's decision logic carries its own table-test coverage
// and the runner's git wiring (runner/cmd/fishhawk-runner/conflictresolve.go)
// is left with capture and execution only. Long-form contract: README.md.
package conflictresolve

import "bytes"

// MarkerKind classifies a single line of a conflicted file as one of git's
// conflict markers, or as ordinary content.
type MarkerKind int

// The four marker forms. MarkerBase is the diff3/zdiff3 common-ancestor
// marker, emitted only when merge.conflictStyle is diff3 or zdiff3, so the
// partition must RECOGNISE it without REQUIRING it.
const (
	MarkerNone MarkerKind = iota
	MarkerOurs
	MarkerBase
	MarkerSep
	MarkerTheirs
)

// markerRunLen is the exact number of marker characters git emits. A line
// carrying more than seven (`<<<<<<<<`) is content, not a marker.
const markerRunLen = 7

// ClassifyMarkerLine reports which conflict marker a line is, if any.
//
// A marker line is EXACTLY seven identical marker characters at line start,
// followed by end-of-line or by a space (git appends a label after that
// space). Anything else — an indented marker, a longer run, a mixed run — is
// ordinary content. The line may carry a trailing CR: git writes markers
// with the file's own line endings, so a CRLF file's markers arrive as
// "<<<<<<< ours\r".
//
// The caller passes a line WITHOUT its trailing LF.
func ClassifyMarkerLine(line []byte) MarkerKind {
	line = bytes.TrimSuffix(line, []byte("\r"))
	if len(line) < markerRunLen {
		return MarkerNone
	}
	var kind MarkerKind
	switch line[0] {
	case '<':
		kind = MarkerOurs
	case '|':
		kind = MarkerBase
	case '=':
		kind = MarkerSep
	case '>':
		kind = MarkerTheirs
	default:
		return MarkerNone
	}
	for i := 1; i < markerRunLen; i++ {
		if line[i] != line[0] {
			return MarkerNone
		}
	}
	if len(line) == markerRunLen || line[markerRunLen] == ' ' {
		return kind
	}
	return MarkerNone
}

// line is one physical line of a byte slice: its content without the
// trailing LF, plus the offsets that bound it INCLUDING that LF.
type line struct {
	text  []byte
	start int
	end   int // one past the trailing LF, or len(b) for a final unterminated line
}

// eachLine calls fn for every line of b in order.
func eachLine(b []byte, fn func(line)) {
	for off := 0; off < len(b); {
		idx := bytes.IndexByte(b[off:], '\n')
		if idx < 0 {
			fn(line{text: b[off:], start: off, end: len(b)})
			return
		}
		end := off + idx + 1
		fn(line{text: b[off : off+idx], start: off, end: end})
		off = end
	}
}

// ContainsMarkerLine reports whether b carries any conflict-marker line.
//
// The gate applies this to the REPLACEMENT regions an agent wrote, never to
// a whole file: a repository may legitimately contain a line of seven equals
// signs, and that line survives into every accepted resolution as part of a
// non-conflict segment.
func ContainsMarkerLine(b []byte) bool {
	found := false
	eachLine(b, func(l line) {
		if !found && ClassifyMarkerLine(l.text) != MarkerNone {
			found = true
		}
	})
	return found
}
