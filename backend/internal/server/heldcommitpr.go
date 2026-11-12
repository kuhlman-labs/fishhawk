package server

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// conventionalCommitHeaderRe matches a Conventional Commits v1.0.0 header: a
// lowercase type from the allowed set, an optional lowercase scope in parens, an
// optional breaking-change `!`, then `: ` and a non-empty description. Used by
// heldCommitPRTitleBody to decide whether the issue title is already a
// conventional header (used verbatim) or needs the `chore: ` prefix.
//
// THIRD COPY of the pattern. The other two are
// backend/internal/orchestrator/orchestrator.go's conventionalCommitHeaderRe
// (#1774, SAME MODULE — TestConventionalCommitHeaderRe_MatchesOrchestratorSource
// pins this copy against that file's source literal, so a drift fails the suite)
// and runner/cmd/fishhawk-runner/main.go's (a separate Go module, so it can only
// be comment-guarded). Keep all three in sync.
var conventionalCommitHeaderRe = regexp.MustCompile(`^(feat|fix|docs|refactor|test|chore|perf|build)(\([a-z0-9/._-]+\))?!?: .+$`)

// heldCommitPRTitleBody resolves the pull-request title + body a held-commit
// RESUME opens its PR with (#2570).
//
// WHY THIS EXISTS. A resumed implement stage — an exempt-resolved
// scope-completeness park (#1231) or a PR-open checkpoint resume (#2169) — takes
// the runner's pre-agent short-circuit, so no agent runs and no PR-description
// handoff is written. Before this, that path always fell through to
// `chore: fishhawk implement stage <id>` with a two-line body: no summary, no
// test plan, and crucially no `Closes #N`, so merging never auto-closed the
// trigger issue and main's history recorded a meaningless commit subject.
//
// BRANCH TABLE. Each field is resolved INDEPENDENTLY, so a recovered title is
// never discarded because the body was empty (or the reverse):
//
//	recovered title | recovered body | issue context | title            | body
//	----------------+----------------+---------------+------------------+---------------------------
//	present         | present        | any           | recovered        | recovered + Closes #N¹
//	present         | empty          | present       | recovered        | synthesized fallback
//	present         | empty          | absent        | recovered        | "" (runner placeholder body)
//	empty           | present        | present       | synthesized      | recovered + Closes #N¹
//	empty           | present        | absent        | "" (placeholder) | recovered
//	empty           | empty          | present       | synthesized      | synthesized fallback
//	empty           | empty          | absent        | ""               | "" (runner placeholder)
//
// ¹ appended only when the recovered body carries no CLOSING reference to this
// issue already — see hasClosingReference.
//
// PARTIAL DELIVERY (E83.52 / #4085). When partial is true — the run's approved
// plan declares `delivery: partial` (plan.(*Plan).IsPartialDelivery, resolved by
// the caller through partialDeliveryPlan) — the resumed PR must NOT close the
// issue, so `Closes #N` in the table above becomes `Refs #N`: the recovered or
// synthesized body is first passed through neutralizeClosingReferences (every
// active closing reference to the issue becomes `Refs #N`), and `Refs #N` is
// appended unless the body already carries an active `Refs #N`. The ship-time
// guard (neutralizePartialDeliveryClosingRef) still re-checks the live body when
// the resumed runner ships the PR; this keeps the served text honest on its own.
// partial=false is byte-identical to the pre-#4085 behavior.
//
// An empty return for either field is not a failure: the runner falls back to
// its own placeholder half, i.e. exactly today's behavior. Recovered text is a
// QUALITY input, never a safety precondition — nothing here can withhold a
// resume.
//
// The returned body is FOOTER-FREE. The runner that opens the pull request
// appends the Fishhawk attribution footer exactly once at open time, so the
// footer is stamped with the branch and audit URL of the run that actually
// opened the PR and can never be doubled.
func heldCommitPRTitleBody(runRow *run.Run, recoveredTitle, recoveredBody string, partial bool) (title, body string) {
	title = recoveredTitle
	body = recoveredBody

	issueTitle, issueNumber := issueContextOf(runRow)

	if title == "" && issueTitle != "" {
		// Conventional-Commits title, modelled on the decomposed parent's
		// consolidatedPRTitleBody (#1774): an issue title already matching the
		// header shape is used verbatim (no double prefix); anything else gets
		// the `chore: ` prefix.
		if conventionalCommitHeaderRe.MatchString(issueTitle) {
			title = issueTitle
		} else {
			title = "chore: " + issueTitle
		}
	}

	if body == "" {
		if issueTitle == "" && issueNumber <= 0 {
			// Nothing recovered and no issue context: return empty so the runner
			// keeps today's placeholder verbatim. A resume never starts failing
			// because there was nothing to synthesize from.
			return title, ""
		}
		var b strings.Builder
		b.WriteString("## Summary\n\n")
		if issueTitle != "" {
			fmt.Fprintf(&b, "Implements %s.", issueTitle)
		} else {
			fmt.Fprintf(&b, "Implements issue #%d.", issueNumber)
		}
		b.WriteString("\n\nThis pull request was opened by a Fishhawk RESUME of an" +
			" already-verified commit, so the implement agent did not run again and" +
			" its own change description was not available. The commit and its" +
			" verify gates are unchanged; only this description is synthesized from" +
			" the issue.")
		body = b.String()
	}

	if partial && issueNumber > 0 {
		// A partial delivery references the issue without closing it: rewrite
		// any active closing reference the recovered text carries, then append
		// `Refs #N` unless an active one is already there (the rewrite itself
		// produces one).
		body, _ = neutralizeClosingReferences(body, issueNumber)
		if !hasRefsReference(body, issueNumber) {
			body += fmt.Sprintf("\n\nRefs #%d", issueNumber)
		}
		return title, body
	}

	// `Closes #N` is the issue's blocking requirement: merging the resumed PR
	// must auto-close the trigger issue. Append it unless the body ALREADY
	// carries a real closing reference to this exact issue.
	if issueNumber > 0 && !hasClosingReference(body, issueNumber) {
		body += fmt.Sprintf("\n\nCloses #%d", issueNumber)
	}
	return title, body
}

// hasRefsReference reports whether body already carries an ACTIVE `Refs #n`
// (case-insensitive, `#n` terminated by a real word boundary) outside any code
// span or fenced block — the same code-context rules as hasClosingReference, so
// a `Refs #n` quoted in code does not suppress the partial-delivery append.
func hasRefsReference(body string, n int) bool {
	if n <= 0 {
		return false
	}
	re := regexp.MustCompile(`(?i)\brefs\s+#` + fmt.Sprint(n) + `\b`)
	return re.MatchString(stripCodeContexts(body))
}

// issueContextOf reads the run's cached triggering-issue title + number,
// tolerating a nil run or a nil IssueContext (both are ordinary states for a
// non-issue-triggered run).
func issueContextOf(runRow *run.Run) (title string, number int) {
	if runRow == nil || runRow.IssueContext == nil {
		return "", 0
	}
	return runRow.IssueContext.Title, runRow.IssueContext.Number
}

// hasClosingReference reports whether body already carries an ACTIVE GitHub
// closing reference to issue n (a closing keyword immediately preceding `#n`,
// outside any code span or fenced code block). It delegates to the shared
// definition in orchestrator.HasClosingReference (#2570, moved by #5192),
// which documents why precision in both directions matters.
func hasClosingReference(body string, n int) bool {
	return orchestrator.HasClosingReference(body, n)
}

// stripCodeContexts blanks out fenced code blocks and inline code spans so a
// scan for directives reads only the body's ACTIVE markdown. It delegates to
// orchestrator.StripCodeContexts (#2570, moved by #5192).
func stripCodeContexts(body string) string {
	return orchestrator.StripCodeContexts(body)
}

// codeSpanElision is what an elided inline code span leaves behind; see
// orchestrator.CodeSpanElision.
const codeSpanElision = orchestrator.CodeSpanElision
