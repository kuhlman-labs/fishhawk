package prompt

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// MaxFixupConcernBytes bounds the rendered concern LIST of each fix-up concern
// block (#4284): the item lines of the trusted "### Fix-up concerns" block, and
// separately the quote-prefixed lines inside the acceptance block's
// BEGIN/END UNTRUSTED ACCEPTANCE FAILURE envelope. The fixed heading, framing
// paragraph and envelope delimiters are not counted. 12000 matches the repo's
// other binding operator channels (MaxApprovalConditionBytes,
// MaxRevisionConstraintBytes, MaxClarificationAnswerBytes): a maximum-size
// operator_concern plus ordinary reviewer concerns fits without refusal.
//
// The cap is a BUDGET, not a tail drop: planFixupConcernItems never drops a
// list item. Every concern renders its id and summary line; only the note
// BODIES of non-operator concerns are trimmed, each with its own marker.
const MaxFixupConcernBytes = 12000

// MaxFixupConcernSummaryBytes caps a concern's SUMMARY — the first line of its
// rendered text (the server-built "[severity/category…]" label plus the note's
// first line), cut rune-safely. The summary always renders, so without a cap a
// long single-line note would be an uncuttable minimum and force refusals.
const MaxFixupConcernSummaryBytes = 400

// FixupConcernIDPlaceholder is the conservative stand-in id a route-time
// caller stamps on every non-acceptance concern before FixupConcernsFit
// (#4284). The operator concern's real id is minted only after the fix-up
// transition, so the check cannot know it; the rendered size is monotone
// non-decreasing in id length and a canonical uuid.UUID.String() is 36 bytes,
// so a set that fits with this placeholder also fits with its real ids.
const FixupConcernIDPlaceholder = "00000000-0000-0000-0000-000000000000"

// fixupConcernShrinkIterations bounds planFixupConcernItems' measure-and-shrink
// loop. Past it the allocator falls back to the minimum render, which is known
// to fit, so the bound costs body bytes, never a concern.
const fixupConcernShrinkIterations = 32

// FixupConcernFit is FixupConcernsFit's verdict for one concern set.
type FixupConcernFit struct {
	// Fits reports whether every block renders within MaxBytes.
	Fits bool
	// RequiredBytes is the rendered list size of the worse block. When Fits is
	// false it is the MINIMUM render (every summary line, operator concerns in
	// full), so OverflowBytes is how much must be removed.
	RequiredBytes int
	// MaxBytes is MaxFixupConcernBytes.
	MaxBytes int
	// OverflowBytes is RequiredBytes - MaxBytes when positive, else 0.
	OverflowBytes int
	// ConcernCount is len(concerns) as passed in.
	ConcernCount int
	// BodiesTruncated reports whether any concern's note body was trimmed.
	BodiesTruncated bool
	// TruncatedIndexes are the indexes INTO THE INPUT SLICE of the concerns
	// whose note body was trimmed, ascending, across both blocks.
	TruncatedIndexes []int
}

// FixupConcernsFit is the route-time twin of writeFixupConcerns (#4284). It
// renders each block's concern list with the SAME code the prompt uses and
// measures the bytes, so its verdict cannot drift from the real render. The
// trusted and acceptance blocks are evaluated independently, each against its
// own MaxFixupConcernBytes as the renderer does, and the worse block's figures
// are returned. A caller that wants a block exempted from a route-time refusal
// passes only the concerns it enforces on.
func FixupConcernsFit(concerns []FixupConcern) FixupConcernFit {
	trusted, trustedIdx, acceptance, acceptanceIdx := partitionFixupConcerns(concerns)
	trustedOut, trustedCut := renderFixupConcernItems(trusted, false)
	acceptanceOut, acceptanceCut := renderFixupConcernItems(acceptance, true)

	worse := len(trustedOut)
	if len(acceptanceOut) > worse {
		worse = len(acceptanceOut)
	}
	fit := FixupConcernFit{
		Fits:          worse <= MaxFixupConcernBytes,
		RequiredBytes: worse,
		MaxBytes:      MaxFixupConcernBytes,
		ConcernCount:  len(concerns),
	}
	if !fit.Fits {
		fit.OverflowBytes = worse - MaxFixupConcernBytes
	}
	for _, i := range trustedCut {
		fit.TruncatedIndexes = append(fit.TruncatedIndexes, trustedIdx[i])
	}
	for _, i := range acceptanceCut {
		fit.TruncatedIndexes = append(fit.TruncatedIndexes, acceptanceIdx[i])
	}
	sort.Ints(fit.TruncatedIndexes)
	fit.BodiesTruncated = len(fit.TruncatedIndexes) > 0
	return fit
}

// partitionFixupConcerns splits concerns by trust provenance and orders the
// trusted half operator-first (#4284 C1). The partition is STABLE and happens
// here, inside the renderer, after the caller paired ids: the caller's order
// (which concern_ids pairing depends on) is never touched. The returned index
// slices map each partitioned concern back to its input position.
func partitionFixupConcerns(concerns []FixupConcern) (trusted []FixupConcern, trustedIdx []int, acceptance []FixupConcern, acceptanceIdx []int) {
	var operator, reviewer []FixupConcern
	var operatorIdx, reviewerIdx []int
	for i, c := range concerns {
		switch {
		case c.AcceptanceDerived:
			acceptance = append(acceptance, c)
			acceptanceIdx = append(acceptanceIdx, i)
		case c.Operator:
			operator = append(operator, c)
			operatorIdx = append(operatorIdx, i)
		default:
			reviewer = append(reviewer, c)
			reviewerIdx = append(reviewerIdx, i)
		}
	}
	trusted = append(append(make([]FixupConcern, 0, len(concerns)), operator...), reviewer...)
	trustedIdx = append(append(make([]int, 0, len(concerns)), operatorIdx...), reviewerIdx...)
	return trusted, trustedIdx, acceptance, acceptanceIdx
}

// fixupConcernItem is one concern's split for the allocator: Text is exactly
// summary + body.
type fixupConcernItem struct {
	concern FixupConcern
	summary string
	body    string
	// full marks a concern whose body is never trimmed (an operator concern
	// in the trusted block).
	full bool
}

func splitFixupConcern(c FixupConcern, acceptance bool) fixupConcernItem {
	first := c.Text
	if i := strings.IndexByte(first, '\n'); i >= 0 {
		first = first[:i]
	}
	summary := cutRuneSafe(first, MaxFixupConcernSummaryBytes)
	return fixupConcernItem{
		concern: c,
		summary: summary,
		body:    c.Text[len(summary):],
		full:    c.Operator && !acceptance,
	}
}

// renderFixupConcernItems renders one block's concern list under the
// MaxFixupConcernBytes budget and reports which items (indexes into concerns)
// had their body trimmed. It NEVER drops an item (#4284). The allocator is
// deterministic, so the same input renders byte-identically on replay:
//
//  1. Every body in full. If that fits, done (and an id-less trusted item is
//     byte-identical to the pre-#4284 "- " + Text line).
//  2. The MINIMUM: every trimmable body cut to zero (summary + marker), every
//     operator concern in full. If even that overflows, it is returned as is —
//     over budget, every concern still named. The route-time check refuses
//     such sets, so only a pre-check trigger entry can reach this.
//  3. Otherwise the leftover budget is water-filled across the trimmable
//     bodies (shortest need first, each taking at most an equal share of what
//     remains). The candidate is RENDERED and MEASURED; an overshoot (marker
//     digits, a fence closer, acceptance sanitization) is taken off the
//     leftover (or the leftover halved, when the overshoot exceeds it) and the
//     fill repeated, falling back to the minimum after
//     fixupConcernShrinkIterations.
func renderFixupConcernItems(concerns []FixupConcern, acceptance bool) (string, []int) {
	if len(concerns) == 0 {
		return "", nil
	}
	items := make([]fixupConcernItem, len(concerns))
	full := make([]int, len(concerns))
	minimum := make([]int, len(concerns))
	for i, c := range concerns {
		items[i] = splitFixupConcern(c, acceptance)
		full[i] = len(items[i].body)
		if items[i].full {
			minimum[i] = full[i]
		}
	}
	if out, cut := renderFixupConcernAllotment(items, full, acceptance); len(out) <= MaxFixupConcernBytes {
		return out, cut
	}
	minOut, minCut := renderFixupConcernAllotment(items, minimum, acceptance)
	if len(minOut) > MaxFixupConcernBytes {
		return minOut, minCut
	}
	leftover := MaxFixupConcernBytes - len(minOut)
	for range fixupConcernShrinkIterations {
		allot := waterFillFixupBodies(items, minimum, leftover)
		out, cut := renderFixupConcernAllotment(items, allot, acceptance)
		if len(out) <= MaxFixupConcernBytes {
			return out, cut
		}
		next := leftover - (len(out) - MaxFixupConcernBytes)
		if next <= 0 {
			// The overshoot exceeds the whole leftover: sanitization more than
			// doubled the bodies (short or rule-shaped acceptance lines).
			// Halve instead, so such bodies still get a share.
			next = leftover / 2
		}
		if next <= 0 {
			break
		}
		leftover = next
	}
	return minOut, minCut
}

// waterFillFixupBodies hands leftover body bytes to the trimmable items in a
// fixed order (need ascending, then index), each taking min(need, an equal
// share of what remains). Operator items keep their full allotment from
// minimum.
func waterFillFixupBodies(items []fixupConcernItem, minimum []int, leftover int) []int {
	allot := append([]int(nil), minimum...)
	var order []int
	for i, it := range items {
		if !it.full && len(it.body) > 0 {
			order = append(order, i)
		}
	}
	sort.SliceStable(order, func(a, b int) bool {
		return len(items[order[a]].body) < len(items[order[b]].body)
	})
	for k, i := range order {
		share := leftover / (len(order) - k)
		give := min(len(items[i].body), share)
		allot[i] = give
		leftover -= give
	}
	return allot
}

// renderFixupConcernAllotment renders every item with allot[i] body bytes and
// returns the list text plus the indexes of items rendered trimmed.
func renderFixupConcernAllotment(items []fixupConcernItem, allot []int, acceptance bool) (string, []int) {
	var b strings.Builder
	var cut []int
	for i, it := range items {
		if allot[i] >= len(it.body) {
			writeFixupConcernItem(&b, it, it.body, false, acceptance)
			continue
		}
		shown := strings.TrimRight(cutRuneSafe(it.body, allot[i]), "\n")
		writeFixupConcernItem(&b, it, shown, true, acceptance)
		cut = append(cut, i)
	}
	return b.String(), cut
}

// writeFixupConcernItem writes one concern. Trusted: "- " + optional
// "(concern <id>) " + the kept text; a trimmed body closes any fence the cut
// left open, then states the cut on its own line. Acceptance: the kept text
// through sanitizeUntrustedComment (so every rendered byte is structure-
// neutralized), an optional quoted id line before it, and a quoted marker
// line after a trimmed body.
func writeFixupConcernItem(b *strings.Builder, it fixupConcernItem, body string, trimmed, acceptance bool) {
	kept := it.summary + body
	total := len(it.concern.Text)
	if acceptance {
		if it.concern.ID != "" {
			b.WriteString(sanitizeUntrustedComment("(concern " + it.concern.ID + ")"))
			b.WriteString("\n")
		}
		b.WriteString(sanitizeUntrustedComment(kept))
		b.WriteString("\n")
		if trimmed {
			fmt.Fprintf(b, "| …[failure text truncated: %d of %d bytes shown]\n", len(kept), total)
		}
		return
	}
	b.WriteString("- ")
	if it.concern.ID != "" {
		b.WriteString("(concern " + neutralizeLineStructure(it.concern.ID) + ") ")
	}
	b.WriteString(kept)
	if trimmed {
		if closer := openFenceCloser(kept); closer != "" {
			b.WriteString("\n" + closer)
		}
		fmt.Fprintf(b, "\n  …[note truncated: %d of %d bytes shown — this concern is still MANDATORY; "+
			"address it as summarized and name in your fix-up commit message body anything the summary left you "+
			"unable to resolve]", len(kept), total)
	}
	b.WriteString("\n")
}

// openFenceCloser returns the fence line that closes a ``` or ~~~ code fence
// left open at the end of s, or "" when every fence is closed. A fence opens
// on a line whose leading-whitespace-trimmed form starts with three or more
// backticks or tildes, and closes on a line of only that character, at least
// as long as the opener.
func openFenceCloser(s string) string {
	var open string
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimLeft(line, " \t")
		if open == "" {
			for _, ch := range []byte{'`', '~'} {
				if n := leadingRun(t, ch); n >= 3 {
					open = t[:n]
					break
				}
			}
			continue
		}
		t = strings.TrimRight(t, " \t")
		if n := leadingRun(t, open[0]); n >= len(open) && n == len(t) {
			open = ""
		}
	}
	return open
}

func leadingRun(s string, ch byte) int {
	n := 0
	for n < len(s) && s[n] == ch {
		n++
	}
	return n
}

// cutRuneSafe returns the longest prefix of s of at most n bytes that does not
// split a UTF-8 rune.
func cutRuneSafe(s string, n int) string {
	if n >= len(s) {
		return s
	}
	if n <= 0 {
		return ""
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
