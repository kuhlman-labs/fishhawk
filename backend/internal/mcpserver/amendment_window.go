package mcpserver

import (
	"fmt"
	"regexp"
	"strings"
)

// amendmentPollWindowMinutes is this package's SINGLE numeric statement of the
// mid-stage scope-amendment poll window — the length of time the implement
// agent keeps polling a filed amendment before it gives up, at which point the
// request EXPIRES UNDECIDED (it stays pending server-side; an expiry is NOT a
// denial, and a late decision is still honored on a fishhawk_retry_stage).
//
// It is deliberately a var, not a const, for exactly one reason: two
// discrimination tests reassign it to a different value and assert the rendered
// text follows, which proves those sites RENDER the figure from here rather than
// hardcoding it (a hardcoded literal would still say 15 and fail once the var
// moved). Each test reaches a distinct surface:
//
//   - TestAwaitStageAmendmentMessageRendersWindowConstant — the RUNTIME-rendered
//     amendment_pending Message and next_step Reason (built per response in
//     awaitStageAmendmentPendingOutput).
//   - TestToolDescriptionsRenderWindowConstant — the three REGISTER-TIME tool
//     descriptions that state the figure (fishhawk_await_stage,
//     fishhawk_dispatch_stage, fishhawk_decide_scope_amendment), plus a generic
//     sweep asserting NO registered description carries the stale figure after a
//     mutation. Those descriptions are built INSIDE registerAwaitStage /
//     registerDispatchStage / registerDecideScopeAmendment by concatenating
//     amendmentPollWindowAdjective() / amendmentPollWindowText() — evaluated on
//     every registration call, NOT at package init, which is what makes them
//     reachable by a var mutation (correcting #2627's package-init premise).
//
// Production code never mutates it.
//
// SCOPE OF THIS CONSTANT — deliberately NOT a single source of truth. It
// unifies ONLY the Go-rendered mcpserver surfaces (await_stage.go,
// dispatch_stage.go, scope_amendment.go). Two other surfaces keep their own
// statement of the figure and cannot render from a Go var:
//
//   - backend/internal/prompt/prompt.go's writeScopeAmendments — the text the
//     agent actually follows; this constant MIRRORS that figure.
//   - backend/internal/mcpserver/README.md — operator prose stating the number.
//
// Those surfaces are kept in sync by the doc cross-references between them plus
// staleAmendmentWordingFindings below, which is a TRIPWIRE for the known retired
// phrasings, NOT a single source: the next correction to the number is three
// edits (this constant, the prompt, the README), not one.
//
// mcpserver deliberately does NOT import backend/internal/scopeamendment to
// share this int: that package links pgx, and importing it would drag a
// persistence dependency into the fishhawk-mcp binary purely to share a number.
var amendmentPollWindowMinutes = 15

// amendmentPollWindowText renders the "~15 minutes" phrasing from the constant.
func amendmentPollWindowText() string {
	return fmt.Sprintf("~%d minutes", amendmentPollWindowMinutes)
}

// amendmentPollWindowAdjective renders the "~15-minute" attributive phrasing.
func amendmentPollWindowAdjective() string {
	return fmt.Sprintf("~%d-minute", amendmentPollWindowMinutes)
}

// Retired amendment phrasings this package must never reintroduce on an
// operator-facing surface (#2601 / #2617). Both describe behaviour the product
// no longer has: a lapsed poll window is an EXPIRY — the absence of a decision —
// not a refusal.
const (
	// findingProceedAsDenied names the retired "proceeds as if denied" /
	// "proceed-as-denied" framing.
	findingProceedAsDenied = "retired proceed-as-denied amendment wording"
	// findingStaleFigure names the stale ~5-minute window figure (the real
	// window is ~15 minutes).
	findingStaleFigure = "stale ~5-minute amendment window figure"
)

// staleAmendmentFigureRE matches a stale ~5-minute window figure. It matches a
// bare "5 minute(s)" or "5-minute(s)" with or without a leading tilde/space, but
// NOT a figure whose 5 is preceded by a digit or a dot. Go's regexp has no
// lookbehind, so requiring a non-digit-non-dot char (or start-of-line)
// immediately before the 5 is what keeps "~15 minutes" (5 after "1") and
// stage_wait.go's "~5.5 minutes" (5 after ".") from tripping while a bare
// "5 minute" with no tilde still does.
var staleAmendmentFigureRE = regexp.MustCompile(`(^|[^0-9.])~? ?5[- ]minutes?|five[- ]minutes?`)

// historicalAmendmentMarkers are the words that, TOGETHER with a #2601
// reference on the same line, mark it as a deliberate historical citation of
// the retired wording — a correction record that must survive the guard.
var historicalAmendmentMarkers = []string{"retired", "formerly", "previously", "used to", "understated"}

// staleAmendmentWordingFindings returns the names of the retired amendment
// phrasings a single source line contains: the retired proceed-as-denied
// wording and the stale ~5-minute figure. It returns nil for a line that is a
// deliberate historical citation — one carrying BOTH a #2601 reference AND one
// of historicalAmendmentMarkers — so a correction record such as
// scope_amendment.go's "this doc said ~5 minutes and understated the window
// threefold, #2601" is not a finding, while a bare stale line, or one citing
// #2601 with no historical marker, still is. Pure and line-level so the
// exemption's narrowness is unit-testable.
func staleAmendmentWordingFindings(line string) []string {
	if lineIsHistoricalAmendmentCitation(line) {
		return nil
	}
	lower := strings.ToLower(line)
	var findings []string
	// The BARE "as denied" literal is a third disjunct, not a substring of the
	// other two: "as if denied" does not contain "as denied" and vice versa. Its
	// absence is what let backend/internal/mcpserver/README.md keep a
	// "proceeds as denied" sentence through #2601 and #2617 (E50.13 / #2363).
	if strings.Contains(lower, "as if denied") || strings.Contains(lower, "proceed-as-denied") ||
		strings.Contains(lower, "as denied") {
		findings = append(findings, findingProceedAsDenied)
	}
	if staleAmendmentFigureRE.MatchString(line) {
		findings = append(findings, findingStaleFigure)
	}
	return findings
}

// lineIsHistoricalAmendmentCitation reports whether a line is a deliberate
// historical citation: it must carry BOTH a #2601 reference AND a historical
// marker. A #2601 reference alone is NOT sufficient — that keeps the exemption
// narrow enough that a reintroduced stale line cannot be silenced by merely
// citing the issue.
func lineIsHistoricalAmendmentCitation(line string) bool {
	if !strings.Contains(line, "#2601") {
		return false
	}
	lower := strings.ToLower(line)
	for _, m := range historicalAmendmentMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}
