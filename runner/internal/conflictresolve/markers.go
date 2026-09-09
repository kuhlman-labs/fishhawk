// Package conflictresolve holds the pure decision logic for the runner's
// bounded conflict-resolution pass (#3202): conflict-marker classification,
// the hunk-level partition and accept check, and the baseline/observed
// verification that decides whether the agent stayed inside the conflicted
// hunks git itself produced.
//
// The package performs NO I/O and shells out to NO git: every input is a
// value the runner captured, so every decision is table-testable. It is the
// sole owner of the accept/refuse decision — the runner git wiring captures,
// calls, and acts on the verdict, but never re-derives it.
package conflictresolve

import "bytes"

// MarkerKind classifies one line of a conflicted file against the four
// conflict-marker forms git writes.
type MarkerKind uint8

const (
	// MarkerNone is ordinary content — the common case.
	MarkerNone MarkerKind = iota
	// MarkerOurs is the `<<<<<<<` line opening a conflict block.
	MarkerOurs
	// MarkerBase is the `|||||||` common-ancestor line. It is emitted ONLY
	// under merge.conflictStyle=diff3/zdiff3, so the partition RECOGNISES it
	// but never REQUIRES it.
	MarkerBase
	// MarkerSeparator is the `=======` line between the two sides.
	MarkerSeparator
	// MarkerTheirs is the `>>>>>>>` line closing a conflict block.
	MarkerTheirs
)

// markerRunLen is the exact number of identical marker characters git writes.
// A longer run is ordinary content: git emits exactly seven.
const markerRunLen = 7

func markerCharKind(c byte) MarkerKind {
	switch c {
	case '<':
		return MarkerOurs
	case '|':
		return MarkerBase
	case '=':
		return MarkerSeparator
	case '>':
		return MarkerTheirs
	}
	return MarkerNone
}

// ClassifyMarkerLine reports which conflict marker, if any, the given line IS.
// The line must be the content of one line WITHOUT its terminating newline; a
// single trailing CR is tolerated so a CRLF file classifies the same way.
//
// A marker line is EXACTLY seven identical marker characters at line start,
// followed by end-of-line or a space (git's label separator). A longer run, an
// indented run, and a mixed run are all ordinary content.
func ClassifyMarkerLine(line []byte) MarkerKind {
	line = bytes.TrimSuffix(line, []byte("\r"))
	if len(line) < markerRunLen {
		return MarkerNone
	}
	kind := markerCharKind(line[0])
	if kind == MarkerNone {
		return MarkerNone
	}
	for i := 1; i < markerRunLen; i++ {
		if line[i] != line[0] {
			return MarkerNone
		}
	}
	if len(line) == markerRunLen {
		return kind
	}
	if line[markerRunLen] == ' ' {
		return kind
	}
	return MarkerNone
}

// lineSpan locates one line inside a byte slice. End excludes the terminating
// newline; Next is the first byte of the following line (== End for an
// unterminated final line).
type lineSpan struct {
	Start int
	End   int
	Next  int
}

// eachLine walks b line by line, stopping early when fn returns false. A file
// whose last line is unterminated yields that line with End == Next == len(b);
// a file ending in a newline yields no trailing empty line.
func eachLine(b []byte, fn func(lineSpan) bool) {
	for i := 0; i < len(b); {
		sp := lineSpan{Start: i}
		if nl := bytes.IndexByte(b[i:], '\n'); nl < 0 {
			sp.End, sp.Next = len(b), len(b)
		} else {
			sp.End, sp.Next = i+nl, i+nl+1
		}
		if !fn(sp) {
			return
		}
		i = sp.Next
	}
}

// ContainsMarkerLine reports whether b carries any conflict-marker line. The
// runner uses it to tell a content conflict (git wrote markers) from a binary
// one (git left one side on disk with no markers at all).
func ContainsMarkerLine(b []byte) bool {
	found := false
	eachLine(b, func(sp lineSpan) bool {
		if ClassifyMarkerLine(b[sp.Start:sp.End]) != MarkerNone {
			found = true
			return false
		}
		return true
	})
	return found
}

// markerLineSpans returns every marker line in b, each spanning [Start, Next)
// — the terminating newline is deliberately part of the span, so a replacement
// region that supplies a marker line's newline still intersects it.
func markerLineSpans(b []byte) []lineSpan {
	var out []lineSpan
	eachLine(b, func(sp lineSpan) bool {
		if ClassifyMarkerLine(b[sp.Start:sp.End]) != MarkerNone {
			out = append(out, sp)
		}
		return true
	})
	return out
}
