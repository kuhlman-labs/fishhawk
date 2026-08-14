package server

import (
	"fmt"
	"regexp"
	"strings"

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
// An empty return for either field is not a failure: the runner falls back to
// its own placeholder half, i.e. exactly today's behavior. Recovered text is a
// QUALITY input, never a safety precondition — nothing here can withhold a
// resume.
//
// The returned body is FOOTER-FREE. The runner that opens the pull request
// appends the Fishhawk attribution footer exactly once at open time, so the
// footer is stamped with the branch and audit URL of the run that actually
// opened the PR and can never be doubled.
func heldCommitPRTitleBody(runRow *run.Run, recoveredTitle, recoveredBody string) (title, body string) {
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

	// `Closes #N` is the issue's blocking requirement: merging the resumed PR
	// must auto-close the trigger issue. Append it unless the body ALREADY
	// carries a real closing reference to this exact issue.
	if issueNumber > 0 && !hasClosingReference(body, issueNumber) {
		body += fmt.Sprintf("\n\nCloses #%d", issueNumber)
	}
	return title, body
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
//     does not act on it, so neither do we.
func hasClosingReference(body string, n int) bool {
	if n <= 0 {
		return false
	}
	// close/closes/closed, fix/fixes/fixed, resolve/resolves/resolved — GitHub's
	// closing keywords — then optional `:`, whitespace, and `#n` terminated by a
	// real word boundary.
	re := regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s*:?\s+#` + fmt.Sprint(n) + `\b`)
	return re.MatchString(stripCodeContexts(body))
}

// stripCodeContexts blanks out fenced code blocks and inline code spans so a
// scan for directives reads only the body's ACTIVE markdown (#2570). Lines are
// preserved (as empty lines) rather than removed so nothing on either side of an
// elision is accidentally joined into a match that the source text does not
// contain.
//
// An unterminated fence swallows the rest of the body, matching CommonMark and
// erring toward "no closing reference found" — the safe direction, since the
// cost is a duplicate line rather than a silently un-closed issue.
func stripCodeContexts(body string) string {
	var out strings.Builder
	inFence := false
	fenceMarker := ""
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimLeft(line, " \t")
		if marker, ok := fenceDelimiter(trimmed); ok {
			if !inFence {
				inFence, fenceMarker = true, marker
				out.WriteByte('\n')
				continue
			}
			if marker == fenceMarker {
				inFence, fenceMarker = false, ""
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

// fenceDelimiter reports the code-fence delimiter a line opens or closes with —
// a run of three or more backticks or tildes at the start of the (leading-space
// trimmed) line.
func fenceDelimiter(trimmed string) (string, bool) {
	for _, c := range []byte{'`', '~'} {
		n := 0
		for n < len(trimmed) && trimmed[n] == c {
			n++
		}
		if n >= 3 {
			return string(c), true
		}
	}
	return "", false
}

// stripInlineCode removes inline code spans from one line: a run of N backticks
// opens a span that the next run of EXACTLY N backticks closes (CommonMark's
// rule, which is what lets a span contain a literal backtick). A run with no
// matching closer is not a span — its backticks are emitted literally and the
// scan continues past them.
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
