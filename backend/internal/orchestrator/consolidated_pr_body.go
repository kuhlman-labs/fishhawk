package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// This file holds two related things.
//
// 1. The repository's ONE definition of an active GitHub closing reference
//    (#2570, #4085): NeutralizeClosingReferences, HasClosingReference,
//    StripCodeContexts and CodeSpanElision, plus their fence and inline-code
//    helpers. They moved here from backend/internal/server (#5192) so the
//    consolidated PR body applies exactly the rewrite a single-run PR gets; the
//    server keeps one-line delegates (partial_delivery_pr.go, heldcommitpr.go)
//    and its tests pin this code through them.
//
// 2. The decomposed parent's consolidated-PR slice sections (#5192): the
//    pr_body a child's child_pushed audit row carries, and the pipeline that
//    splices each slice's `## Test plan` / `## Notes` into the consolidated
//    body (consolidatedSliceTexts, renderSliceSections).

// NeutralizeClosingReferences rewrites every ACTIVE GitHub closing reference to
// issue n in body — a closing keyword (close/closes/closed, fix/fixes/fixed,
// resolve/resolves/resolved), an optional `:`, whitespace, then `#n` with a real
// word boundary — so it reads `Refs #n`, and returns the rewritten body plus the
// number of references rewritten.
//
// "Active" is EXACTLY HasClosingReference's definition (#2570): the scan runs
// over a code-masked view built with the same fence rules as StripCodeContexts
// (fenceDelimiter; liberal open, strict close) and the same backtick-run rule as
// stripInlineCode, so text inside an inline code span or a fenced block is
// byte-preserved and `#n1` / `#nfoo` are untouched. The view keeps every byte
// offset of body (a masked byte becomes a space in a fence, NUL in a code span —
// the same regex behaviour as StripCodeContexts' empty line and single NUL), so
// a match position in the view IS the position in body.
//
// Only the keyword (and its optional `:`) is replaced, so nothing outside the
// directive moves: "Closes #7" → "Refs #7", "Fixes: #7" → "Refs #7". When the
// span between the keyword and the colon crosses masked text, only the keyword
// itself is replaced. Invariant: HasClosingReference(result, n) is false for
// every input.
func NeutralizeClosingReferences(body string, n int) (string, int) {
	if n <= 0 || body == "" {
		return body, 0
	}
	re := regexp.MustCompile(`(?i)\b((close[sd]?|fix(?:e[sd])?|resolve[sd]?)(?:\s*:)?)\s+#` + fmt.Sprint(n) + `\b`)
	return rewriteClosingKeywords(body, re)
}

// rewriteClosingKeywords replaces group 1 (keyword plus optional colon) — or,
// when that span crosses masked text, group 2 (the bare keyword) — of every
// match of re over the code-masked view of body with "Refs". It is
// NeutralizeClosingReferences' rewrite loop, shared with the consolidated
// path's repository-qualified pre-pass so both read code contexts identically.
func rewriteClosingKeywords(body string, re *regexp.Regexp) (string, int) {
	view := maskCodeContexts(body)
	// `(?:\s*:)?\s+` matches exactly what HasClosingReference's `\s*:?\s+` does;
	// it is spelled this way so group 1 swallows whitespace only up to a colon,
	// and the rewrite never joins the lines a bare keyword was split across.
	matches := re.FindAllStringSubmatchIndex(view, -1)
	if len(matches) == 0 {
		return body, 0
	}
	var out strings.Builder
	last := 0
	for _, m := range matches {
		// m[2:4] is the keyword plus optional colon; m[4:6] the bare keyword.
		start, end := m[2], m[3]
		if view[start:end] != body[start:end] {
			start, end = m[4], m[5]
		}
		out.WriteString(body[last:start])
		out.WriteString("Refs")
		last = end
	}
	out.WriteString(body[last:])
	return out.String(), len(matches)
}

// maskCodeContexts is the offset-preserving sibling of StripCodeContexts: it
// returns a string the same length as body in which every byte of a fence line
// and of a fenced block's content becomes a space, and every byte of an inline
// code span (backticks included) becomes CodeSpanElision. Newlines are kept.
// The fence and span recognition rules are StripCodeContexts' and
// stripInlineCode's verbatim; only the elision preserves length so a regex match
// over the view maps straight back onto body.
func maskCodeContexts(body string) string {
	var out strings.Builder
	out.Grow(len(body))
	inFence := false
	var fenceChar byte
	fenceLen := 0
	for i, line := range strings.Split(body, "\n") {
		if i > 0 {
			out.WriteByte('\n')
		}
		trimmed := strings.TrimLeft(line, " \t")
		if c, n, rest, ok := fenceDelimiter(trimmed); ok {
			if !inFence {
				inFence, fenceChar, fenceLen = true, c, n
				out.WriteString(strings.Repeat(" ", len(line)))
				continue
			}
			if c == fenceChar && n >= fenceLen && strings.TrimSpace(rest) == "" {
				inFence, fenceChar, fenceLen = false, 0, 0
				out.WriteString(strings.Repeat(" ", len(line)))
				continue
			}
		}
		if inFence {
			out.WriteString(strings.Repeat(" ", len(line)))
			continue
		}
		out.WriteString(maskInlineCode(line))
	}
	return out.String()
}

// maskInlineCode is stripInlineCode with a length-preserving elision: a run of
// N backticks opens a span that the next run of EXACTLY N backticks closes, and
// every byte of the span becomes CodeSpanElision. An unmatched run is literal.
func maskInlineCode(line string) string {
	var out strings.Builder
	i := 0
	for i < len(line) {
		if line[i] != '`' {
			out.WriteByte(line[i])
			i++
			continue
		}
		j := i
		for j < len(line) && line[j] == '`' {
			j++
		}
		n := j - i
		closed := false
		for k := j; k < len(line); {
			if line[k] != '`' {
				k++
				continue
			}
			m := k
			for m < len(line) && line[m] == '`' {
				m++
			}
			if m-k == n {
				out.WriteString(strings.Repeat(CodeSpanElision, m-i))
				i, closed = m, true
				break
			}
			k = m
		}
		if !closed {
			out.WriteString(line[i:j])
			i = j
		}
	}
	return out.String()
}

// HasClosingReference reports whether body already carries an ACTIVE GitHub
// closing reference to issue n — a closing keyword immediately preceding `#n`,
// outside any code span or fenced code block.
//
// PRECISION IN BOTH DIRECTIONS IS THE POINT (#2570). A false NEGATIVE only costs
// a duplicated `Closes #N` line. A false POSITIVE suppresses the ONLY auto-close
// directive and the issue silently stays open on merge — the exact outcome
// #2570 exists to prevent. So three things are deliberately NOT treated as a
// closing reference:
//
//   - A non-closing MENTION: "Related to #2570", "See #2570", or a bare "#2570"
//     do not auto-close anything, so the directive is still appended.
//   - A number that merely STARTS with n: `#2570foo` and `#25701` are different
//     references (or none at all) and will not close #2570, so `\b` after the
//     digits is required — a "any non-digit terminates it" boundary would let
//     `Closes #2570foo` suppress the real append.
//   - Closing-looking text inside INLINE CODE or a FENCED CODE BLOCK: a body that
//     merely documents the syntax (“Closes #2570“) is not a directive. GitHub
//     does not act on it, so neither do we. The elision that implements this must
//     itself be false-positive-free in two ways the first cut was not: an elided
//     inline span leaves a BOUNDARY behind rather than joining its neighbours
//     ("Closes `note` #2570" is not a directive and must not collapse into one),
//     and only a VALID closing fence closes a block, so an inner `~~~`, a shorter
//     backtick run, or an info-string line cannot expose the rest of the block.
func HasClosingReference(body string, n int) bool {
	if n <= 0 {
		return false
	}
	// close/closes/closed, fix/fixes/fixed, resolve/resolves/resolved — GitHub's
	// closing keywords — then optional `:`, whitespace, and `#n` terminated by a
	// real word boundary.
	re := regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s*:?\s+#` + fmt.Sprint(n) + `\b`)
	return re.MatchString(StripCodeContexts(body))
}

// StripCodeContexts blanks out fenced code blocks and inline code spans so a
// scan for directives reads only the body's ACTIVE markdown (#2570). Lines are
// preserved (as empty lines) rather than removed, and an elided inline span
// leaves a BOUNDARY behind (see CodeSpanElision), so nothing on either side of
// an elision is accidentally joined into a match that the source text does not
// contain.
//
// An unterminated fence swallows the rest of the body, matching CommonMark and
// erring toward "no closing reference found" — the safe direction, since the
// cost is a duplicate line rather than a silently un-closed issue. Fence
// RECOGNITION is asymmetric for the same reason: opening is liberal (an
// over-eager open only elides more text) while CLOSING is strict, because a line
// wrongly read as a closer exposes the rest of the block and can turn documented
// syntax into an apparently active directive.
func StripCodeContexts(body string) string {
	var out strings.Builder
	inFence := false
	var fenceChar byte
	fenceLen := 0
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimLeft(line, " \t")
		if c, n, rest, ok := fenceDelimiter(trimmed); ok {
			if !inFence {
				inFence, fenceChar, fenceLen = true, c, n
				out.WriteByte('\n')
				continue
			}
			// CommonMark §4.5: a CLOSING fence uses the same character, is AT
			// LEAST as long as the opener, and carries no info string. A run that
			// fails any of those is fence CONTENT — a `~~~` inside a ``` block, a
			// three-backtick line inside a four-backtick block, or an info-string
			// line like "```go". Accepting one as a closer would expose every
			// following line of the block to the directive scan.
			if c == fenceChar && n >= fenceLen && strings.TrimSpace(rest) == "" {
				inFence, fenceChar, fenceLen = false, 0, 0
				out.WriteByte('\n')
				continue
			}
		}
		if inFence {
			out.WriteByte('\n')
			continue
		}
		out.WriteString(stripInlineCode(line))
		out.WriteByte('\n')
	}
	return out.String()
}

// fenceDelimiter reports the code-fence run a line opens or closes with — a run
// of three or more backticks or tildes at the start of the (leading-space
// trimmed) line — returning the fence character, the run's LENGTH and the text
// that follows it. Callers need all three: the length and the trailing text are
// what distinguish a valid closing fence from ordinary fence content.
func fenceDelimiter(trimmed string) (marker byte, length int, rest string, ok bool) {
	for _, c := range []byte{'`', '~'} {
		n := 0
		for n < len(trimmed) && trimmed[n] == c {
			n++
		}
		if n >= 3 {
			return c, n, trimmed[n:], true
		}
	}
	return 0, 0, "", false
}

// CodeSpanElision is what an elided inline code span leaves behind. Eliding to
// NOTHING is a false-positive generator: "Closes `note` #2570" would collapse to
// "Closes  #2570" and read as an active closing directive, suppressing the
// required append even though GitHub closes nothing for that text. The
// replacement must therefore be a single character that is neither whitespace
// (so it breaks the `keyword <space> #n` shape) nor a word character (so a
// keyword abutting a span still terminates for `\b`). NUL satisfies both and
// cannot occur in a real markdown body.
const CodeSpanElision = "\x00"

// stripInlineCode replaces inline code spans in one line with CodeSpanElision: a
// run of N backticks opens a span that the next run of EXACTLY N backticks
// closes (CommonMark's rule, which is what lets a span contain a literal
// backtick). A run with no matching closer is not a span — its backticks are
// emitted literally and the scan continues past them.
func stripInlineCode(line string) string {
	var out strings.Builder
	i := 0
	for i < len(line) {
		if line[i] != '`' {
			out.WriteByte(line[i])
			i++
			continue
		}
		j := i
		for j < len(line) && line[j] == '`' {
			j++
		}
		n := j - i
		closed := false
		for k := j; k < len(line); {
			if line[k] != '`' {
				k++
				continue
			}
			m := k
			for m < len(line) && line[m] == '`' {
				m++
			}
			if m-k == n {
				out.WriteString(CodeSpanElision)
				i, closed = m, true
				break
			}
			k = m
		}
		if !closed {
			out.WriteString(line[i:j])
			i = j
		}
	}
	return out.String()
}

// --- Consolidated-PR slice sections (#5192) ---

// ChildPushedPRBodyKey is the child_pushed audit payload key carrying the slice
// agent's footer-free PR description. The server writes it (succeedChildPushStage)
// and consolidatedSliceTexts reads it, through this one constant.
const ChildPushedPRBodyKey = "pr_body"

// MaxChildPushedPRBodyBytes bounds the pr_body persisted on a child_pushed row
// (the runner applies the same 8 KiB bound before it sends the text).
const MaxChildPushedPRBodyBytes = 8 * 1024

// childPushedPRBodyTruncationMarker ends a pr_body ClampChildPushedPRBody cut.
const childPushedPRBodyTruncationMarker = "\n\n_[truncated by Fishhawk to fit the child_pushed audit bound]_"

// consolidatedSliceTextBudget bounds the total slice text spliced into one
// consolidated PR body, keeping it well inside GitHub's 65,536-character body
// limit alongside the summary, bullets and footer.
const consolidatedSliceTextBudget = 40 * 1024

// safeCutTail is how far back a size cut looks for a newline to end on.
const safeCutTail = 512

// trailingIssueRefRe matches a trailing `#` plus digit run.
var trailingIssueRefRe = regexp.MustCompile(`#[0-9]+$`)

// safeCut bounds s to at most max BYTES for a size cut this change adds
// (ClampChildPushedPRBody, the consolidated budget cut). It cuts on a rune
// boundary, then backs the cut off to the last newline when one lies within the
// final safeCutTail bytes; otherwise it backs off past any trailing `#` plus
// digit run. Either way a cut can never turn `Fixes #5121` into `Fixes #51`, a
// closing reference to a DIFFERENT issue.
func safeCut(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 0 {
		return ""
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	t := s[:cut]
	if i := strings.LastIndexByte(t, '\n'); i >= 0 && len(t)-i <= safeCutTail {
		return t[:i]
	}
	return trailingIssueRefRe.ReplaceAllString(t, "")
}

// ClampChildPushedPRBody bounds a slice agent's PR body to
// MaxChildPushedPRBodyBytes for the child_pushed row. Text already within the
// bound is returned unchanged; longer text is cut (safeCut: rune boundary, no
// minted issue reference) and ends with a truncation marker, the total staying
// at or under the bound.
func ClampChildPushedPRBody(s string) string {
	if len(s) <= MaxChildPushedPRBodyBytes {
		return s
	}
	return safeCut(s, MaxChildPushedPRBodyBytes-len(childPushedPRBodyTruncationMarker)) + childPushedPRBodyTruncationMarker
}

// ChildPushedPRBody decodes the pr_body a child_pushed payload carries. It
// returns "" when the key is absent or the payload is undecodable.
func ChildPushedPRBody(payload []byte) string {
	var p map[string]json.RawMessage
	if err := json.Unmarshal(payload, &p); err != nil {
		return ""
	}
	raw, ok := p[ChildPushedPRBodyKey]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// sliceText is one succeeded slice's PR text, extracted from its newest
// child_pushed row's pr_body.
type sliceText struct {
	Index      int
	Title      string
	ChildRunID uuid.UUID
	TestPlan   string
	Notes      string
}

// consolidatedSliceTexts reads each succeeded slice's PR text for the
// consolidated body: State == succeeded children with a non-nil SliceIndex,
// in ascending SliceIndex order (the set and order integrateSlices merges).
// Each slice's text is the NEWEST child_pushed row's pr_body (audit entries
// list oldest-first), so a re-run's latest push wins; a newest row without
// pr_body omits the slice rather than falling back to superseded text. Every
// failure (nil Audit, list error, no row, absent or undecodable pr_body)
// WARN-logs and omits the slice; none fails the open.
func (o *Orchestrator) consolidatedSliceTexts(ctx context.Context, r *run.Run, p *plan.Plan, children []*run.Run) []sliceText {
	if o.Audit == nil {
		if len(children) > 0 {
			o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: Audit not configured; consolidated PR body omits slice text",
				slog.String("run_id", r.ID.String()))
		}
		return nil
	}
	succeeded := make([]*run.Run, 0, len(children))
	for _, c := range children {
		if c.State == run.StateSucceeded && c.SliceIndex != nil {
			succeeded = append(succeeded, c)
		}
	}
	sort.SliceStable(succeeded, func(i, j int) bool {
		return *succeeded[i].SliceIndex < *succeeded[j].SliceIndex
	})
	var out []sliceText
	for _, c := range succeeded {
		idx := *c.SliceIndex
		warn := func(reason string, extra ...slog.Attr) {
			attrs := append([]slog.Attr{
				slog.String("run_id", r.ID.String()),
				slog.String("child_run_id", c.ID.String()),
				slog.Int("slice_index", idx),
				slog.String("reason", reason),
			}, extra...)
			o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: consolidated PR body omits slice text", attrs...)
		}
		entries, err := o.Audit.ListForRunByCategory(ctx, c.ID, "child_pushed")
		if err != nil {
			warn("list child_pushed failed", slog.String("error", err.Error()))
			continue
		}
		if len(entries) == 0 {
			warn("no child_pushed row")
			continue
		}
		body := ChildPushedPRBody(entries[len(entries)-1].Payload)
		if body == "" {
			warn("newest child_pushed row carries no decodable pr_body")
			continue
		}
		st := sliceText{Index: idx, ChildRunID: c.ID}
		if p != nil && p.Decomposition != nil && idx >= 0 && idx < len(p.Decomposition.SubPlans) {
			st.Title = p.Decomposition.SubPlans[idx].Title
		}
		st.TestPlan, st.Notes = extractPRSections(body)
		out = append(out, st)
	}
	return out
}

// extractPRSections returns the `## Test plan` and `## Notes` sections of a PR
// body. A section runs from its `## ` heading line to the next `## ` heading;
// heading names match case-insensitively after trimming, ignoring a trailing
// colon. A `## ` line inside a ``` or ~~~ fence (CommonMark close rule: same
// character, at least as long, no info string) is fence content, never a
// heading. `## Summary` and every other section are discarded.
func extractPRSections(body string) (testPlan, notes string) {
	var tp, nt []string
	var cur *[]string
	inFence := false
	var fenceChar byte
	fenceLen := 0
	for _, line := range strings.Split(body, "\n") {
		if c, n, rest, ok := fenceDelimiter(strings.TrimLeft(line, " \t")); ok {
			switch {
			case !inFence:
				inFence, fenceChar, fenceLen = true, c, n
			case c == fenceChar && n >= fenceLen && strings.TrimSpace(rest) == "":
				inFence, fenceChar, fenceLen = false, 0, 0
			}
		} else if !inFence && strings.HasPrefix(line, "## ") {
			name := strings.ToLower(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line[3:]), ":")))
			switch name {
			case "test plan":
				cur = &tp
			case "notes":
				cur = &nt
			default:
				cur = nil
			}
			continue
		}
		if cur != nil {
			*cur = append(*cur, line)
		}
	}
	return strings.Join(tp, "\n"), strings.Join(nt, "\n")
}

// closeOpenFence appends a matching closing fence line when s ends inside an
// open ``` or ~~~ fence, so a slice's unterminated (or cut) fence cannot
// swallow the later slices, the parent's closing line and the footer.
func closeOpenFence(s string) string {
	inFence := false
	var fenceChar byte
	fenceLen := 0
	for _, line := range strings.Split(s, "\n") {
		c, n, rest, ok := fenceDelimiter(strings.TrimLeft(line, " \t"))
		if !ok {
			continue
		}
		switch {
		case !inFence:
			inFence, fenceChar, fenceLen = true, c, n
		case c == fenceChar && n >= fenceLen && strings.TrimSpace(rest) == "":
			inFence, fenceChar, fenceLen = false, 0, 0
		}
	}
	if !inFence {
		return s
	}
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	return s + strings.Repeat(string(fenceChar), fenceLen)
}

// closingKeywords is GitHub's closing keyword set as a bare regexp
// alternation; closingKeywordAlt is the same set as one non-capturing group.
const (
	closingKeywords   = `close[sd]?|fix(?:e[sd])?|resolve[sd]?`
	closingKeywordAlt = `(?:` + closingKeywords + `)`
)

// stripOwnIssueClosingLines drops, on a FULL-delivery consolidated body, a
// slice line that is EXACTLY a bare closing directive for the run's own issue n
// — the parent writes the one `Closes #n` itself. After trimming whitespace,
// list markers (`-`, `*`, `+`, `1.`, `1)`), blockquote `>` markers and
// emphasis delimiters around the keyword or the whole directive, the line must
// be one keyword, an optional colon, whitespace and `#n` or `<repo>#n` (repo
// compared case-insensitively) with nothing after it. Every other line survives
// byte-for-byte: another issue's line, `#n0`, trailing punctuation or prose, a
// reference list, `Refs #n` and inline prose. Fenced lines are kept. With
// n <= 0 nothing is stripped.
func stripOwnIssueClosingLines(s string, n int, repo string) string {
	if n <= 0 {
		return s
	}
	ref := `#` + fmt.Sprint(n)
	if repo != "" {
		ref = `(?:` + regexp.QuoteMeta(repo) + `)?` + ref
	}
	kw := `(?:\*\*` + closingKeywordAlt + `(?:\s*:)?\*\*|__` + closingKeywordAlt + `(?:\s*:)?__|\*` + closingKeywordAlt + `(?:\s*:)?\*|_` + closingKeywordAlt + `(?:\s*:)?_|` + closingKeywordAlt + `)`
	re := regexp.MustCompile(`(?i)^` + kw + `\s*:?\s+` + ref + `$`)
	lines := strings.Split(s, "\n")
	kept := lines[:0:0]
	inFence := false
	var fenceChar byte
	fenceLen := 0
	for _, line := range lines {
		if c, fn, rest, ok := fenceDelimiter(strings.TrimLeft(line, " \t")); ok {
			switch {
			case !inFence:
				inFence, fenceChar, fenceLen = true, c, fn
			case c == fenceChar && fn >= fenceLen && strings.TrimSpace(rest) == "":
				inFence, fenceChar, fenceLen = false, 0, 0
			}
			kept = append(kept, line)
			continue
		}
		if !inFence && re.MatchString(normalizeDirectiveLine(line)) {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

var (
	listMarkerRe = regexp.MustCompile(`^(?:[-*+]|[0-9]+[.)])\s+`)
	quoteRe      = regexp.MustCompile(`^>\s*`)
)

// normalizeDirectiveLine trims whitespace, then repeatedly removes a leading
// list marker or blockquote `>` marker, then one pair of emphasis delimiters
// wrapping the whole remaining text.
func normalizeDirectiveLine(line string) string {
	t := strings.TrimSpace(line)
	for {
		u := strings.TrimSpace(quoteRe.ReplaceAllString(listMarkerRe.ReplaceAllString(t, ""), ""))
		if u == t {
			break
		}
		t = u
	}
	for _, d := range []string{"**", "__", "*", "_"} {
		if len(t) > 2*len(d) && strings.HasPrefix(t, d) && strings.HasSuffix(t, d) {
			return strings.TrimSpace(t[len(d) : len(t)-len(d)])
		}
	}
	return t
}

// neutralizeQualifiedOwnIssueRefs rewrites every ACTIVE closing reference to
// this repository's qualified own-issue form (`<repo>#n`, repo matched
// case-insensitively) as `Refs <repo>#n`, reading code contexts exactly as
// NeutralizeClosingReferences does. It is a consolidated-path-only pre-pass:
// the shared helper matches only the bare `#n` form.
func neutralizeQualifiedOwnIssueRefs(body string, n int, repo string) string {
	if n <= 0 || repo == "" || body == "" {
		return body
	}
	re := regexp.MustCompile(`(?i)\b((` + closingKeywords + `)(?:\s*:)?)\s+` + regexp.QuoteMeta(repo) + `#` + fmt.Sprint(n) + `\b`)
	out, _ := rewriteClosingKeywords(body, re)
	return out
}

// trimBlankLines drops leading whitespace-only lines and trailing whitespace,
// keeping the first content line's indentation.
func trimBlankLines(s string) string {
	s = strings.TrimRight(s, " \t\r\n")
	for {
		i := strings.IndexByte(s, '\n')
		if i < 0 || strings.TrimSpace(s[:i]) != "" {
			break
		}
		s = s[i+1:]
	}
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return s
}

// renderSliceSections renders the consolidated body's `## Test plan` and
// `## Notes` sections from the slices' extracted text. Each slice text goes
// through, IN THIS ORDER: (1) extraction (done by consolidatedSliceTexts);
// (2) on a FULL delivery with issue > 0, stripOwnIssueClosingLines; (3) trim,
// omitting a text left empty (no heading, no placeholder); (4) the
// consolidatedSliceTextBudget cut — the slice that crosses it is cut (safeCut)
// and it and every later slice render a pointer to the child's audit log;
// (5) closeOpenFence; (6) LAST, only on a partial-delivery parent with
// issue > 0, the qualified own-issue pre-pass and then
// NeutralizeClosingReferences, so no later step can cut the text and re-expose
// a reference the rewrite skipped. A section heading is written only when at
// least one slice contributes; nil or empty slices render "".
func renderSliceSections(slices []sliceText, baseURL string, issue int, partial bool, repo string) string {
	type entry struct {
		s         sliceText
		text      string
		truncated bool
	}
	sections := [2][]entry{}
	for _, s := range slices {
		for i, raw := range [2]string{s.TestPlan, s.Notes} {
			text := raw
			if !partial && issue > 0 {
				text = stripOwnIssueClosingLines(text, issue, repo)
			}
			if text = trimBlankLines(text); text != "" {
				sections[i] = append(sections[i], entry{s: s, text: text})
			}
		}
	}
	used := 0
	for i := range sections {
		for j := range sections[i] {
			e := &sections[i][j]
			switch {
			case used >= consolidatedSliceTextBudget:
				e.text, e.truncated = "", true
			case used+len(e.text) > consolidatedSliceTextBudget:
				e.text = trimBlankLines(safeCut(e.text, consolidatedSliceTextBudget-used))
				e.truncated = true
				used = consolidatedSliceTextBudget
			default:
				used += len(e.text)
			}
			e.text = closeOpenFence(e.text)
			if partial && issue > 0 {
				e.text = neutralizeQualifiedOwnIssueRefs(e.text, issue, repo)
				e.text, _ = NeutralizeClosingReferences(e.text, issue)
			}
		}
	}
	var b strings.Builder
	for i, heading := range [2]string{"Test plan", "Notes"} {
		if len(sections[i]) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n\n## %s\n", heading)
		for _, e := range sections[i] {
			if e.s.Title != "" {
				fmt.Fprintf(&b, "\n### Slice %d: %s (run %s)\n\n", e.s.Index, e.s.Title, shortRunID(e.s.ChildRunID))
			} else {
				fmt.Fprintf(&b, "\n### Slice %d (run %s)\n\n", e.s.Index, shortRunID(e.s.ChildRunID))
			}
			text := e.text
			if e.truncated {
				pointer := fmt.Sprintf("_Slice text truncated to fit the consolidated PR; the full text is on the child_pushed entry at `%s/v0/runs/%s/audit`._",
					strings.TrimRight(baseURL, "/"), e.s.ChildRunID)
				if text != "" {
					text += "\n\n" + pointer
				} else {
					text = pointer
				}
			}
			b.WriteString(text)
			b.WriteString("\n")
		}
	}
	return b.String()
}
