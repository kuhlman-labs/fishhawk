package decisionrecord

import (
	"bytes"
	"regexp"
)

// DecisionExcerptRendering is the repodoc rendering name of a record shown as
// its Decision-section excerpt (#4489): the document's content hash and
// OriginalBytes stay the fetched file's, and the audit row and the prompt's
// Source line name this rendering.
const DecisionExcerptRendering = "adr-decision-section-v1"

// sectionBoundary matches a line that starts a section: a level-1 or level-2
// ATX heading at column 0 (an empty heading included). A level-3+ heading
// never starts or ends a section.
var sectionBoundary = regexp.MustCompile(`^#{1,2}(?:[ \t]|\r?\n?$)`)

// decisionHeading matches a section boundary that opens a Decision section: a
// level-2 heading whose text begins with the word Decision or Decisions, in
// any case ("## Decision", "## Decisions", "## Decision (2026-05-21)",
// "## Decision addendum ..."). "## Decisionmaking" and
// "## Recommendation / Decision" do not match.
var decisionHeading = regexp.MustCompile(`(?i)^##[ \t]+decisions?\b`)

// DecisionExcerpt returns the record's Decision section(s), verbatim and in
// file order, and whether it has any. A section runs from its heading line up
// to, not including, the next section boundary (a level-1 or level-2 ATX
// heading at column 0) or the end of the file; every section whose level-2
// heading begins with the word Decision is kept, every other section dropped.
// Line terminators, CRLF included, are kept as written.
//
// A heading-shaped line inside a fenced code block (CommonMark 0.31.2 §4.5)
// is code, not a heading: it neither ends nor starts a section, and an
// unclosed fence runs to the end of the file.
//
// Limits, both deliberate narrowings of CommonMark ATX headings (§4.2): a
// heading indented by one to three spaces is not recognized, and neither is a
// setext heading (text underlined with = or -). A record whose Decision
// heading takes either form has no excerpt.
func DecisionExcerpt(content []byte) ([]byte, bool) {
	var out []byte
	inDecision := false
	fence := ""
	for _, line := range bytes.SplitAfter(content, []byte("\n")) {
		if fence == "" && sectionBoundary.Match(line) {
			inDecision = decisionHeading.Match(line)
		}
		fence = nextFence(fence, line)
		if inDecision {
			out = append(out, line...)
		}
	}
	return out, len(out) > 0
}

// nextFence returns the fenced-code state after line, per CommonMark 0.31.2
// §4.5. The state is the open fence's marker run ("" outside a fence).
//
// Outside a fence, an opening fence is up to three spaces of indentation and
// then a run of at least three backticks or at least three tildes; a backtick
// fence's info string may not itself contain a backtick. Inside a fence, a
// closing fence is up to three spaces of indentation, a run of the SAME
// character at least as long as the opening run, and then only spaces or
// tabs; any other line leaves the fence open.
func nextFence(fence string, line []byte) string {
	line = bytes.TrimRight(line, "\r\n")
	indent := 0
	for indent < len(line) && indent < 4 && line[indent] == ' ' {
		indent++
	}
	if indent > 3 || indent == len(line) {
		return fence
	}
	c := line[indent]
	if c != '`' && c != '~' {
		return fence
	}
	run := indent
	for run < len(line) && line[run] == c {
		run++
	}
	marker, rest := line[indent:run], line[run:]
	if fence != "" {
		if c == fence[0] && len(marker) >= len(fence) && len(bytes.Trim(rest, " \t")) == 0 {
			return ""
		}
		return fence
	}
	if len(marker) < 3 || (c == '`' && bytes.IndexByte(rest, '`') >= 0) {
		return fence
	}
	return string(marker)
}
