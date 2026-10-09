package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
)

// Partial-delivery ship-time guard (E83.52 / #4085).
//
// A plan may declare `delivery: partial` (plan.(*Plan).IsPartialDelivery): the
// run deliberately ships only a slice of its triggering issue, so its PR must
// reference the issue with `Refs #N` rather than close it. The implement prompt
// tells the agent so, but an instruction is not a control: an agent that still
// writes `Closes #N` would silently close an issue whose remaining scope is
// still owed. neutralizePartialDeliveryClosingRef is the deterministic backstop
// — on the implement ship it re-reads the LIVE PR body and rewrites every
// active closing reference to the issue as `Refs #N`, recording a
// partial_delivery_closing_reference_neutralized audit row.
//
// Named residuals (the guard does NOT cover them):
//
//   - A later fix-up push, or an operator's hand edit of the PR body, that
//     reintroduces `Closes #N` is not re-checked before merge — the guard runs
//     once, on the implement ship.
//   - The guard is GitHub-only (it needs the GitHub client and an installation
//     id). On GitLab nothing rewrites the body, while the merge-time
//     remaining-scope comment (partial_delivery_merge.go) still posts — which
//     is why that comment is worded to stay true even when the issue closed
//     anyway.
//   - A closing keyword in an agent-authored COMMIT message is not rewritten
//     (the prompt forbids it); after this guard the PR body carries Refs, so a
//     squash merge that copies the body is covered.

// categoryPartialDeliveryClosingReferenceNeutralized is the audit category the
// ship-time guard appends after a successful PR-body rewrite. Registered in
// audit.KnownCategories and admitted to issuecomment.activityCategories.
const categoryPartialDeliveryClosingReferenceNeutralized = "partial_delivery_closing_reference_neutralized"

// neutralizeClosingReferences rewrites every ACTIVE GitHub closing reference to
// issue n in body — a closing keyword (close/closes/closed, fix/fixes/fixed,
// resolve/resolves/resolved), an optional `:`, whitespace, then `#n` with a real
// word boundary — so it reads `Refs #n`, and returns the rewritten body plus the
// number of references rewritten.
//
// "Active" is EXACTLY hasClosingReference's definition (#2570): the scan runs
// over a code-masked view built with the same fence rules as stripCodeContexts
// (fenceDelimiter; liberal open, strict close) and the same backtick-run rule as
// stripInlineCode, so text inside an inline code span or a fenced block is
// byte-preserved and `#n1` / `#nfoo` are untouched. The view keeps every byte
// offset of body (a masked byte becomes a space in a fence, NUL in a code span —
// the same regex behaviour as stripCodeContexts' empty line and single NUL), so
// a match position in the view IS the position in body.
//
// Only the keyword (and its optional `:`) is replaced, so nothing outside the
// directive moves: "Closes #7" → "Refs #7", "Fixes: #7" → "Refs #7". When the
// span between the keyword and the colon crosses masked text, only the keyword
// itself is replaced. Invariant: hasClosingReference(result, n) is false for
// every input.
func neutralizeClosingReferences(body string, n int) (string, int) {
	if n <= 0 || body == "" {
		return body, 0
	}
	re := regexp.MustCompile(`(?i)\b((close[sd]?|fix(?:e[sd])?|resolve[sd]?)(?:\s*:)?)\s+#` + fmt.Sprint(n) + `\b`)
	view := maskCodeContexts(body)
	// `(?:\s*:)?\s+` matches exactly what hasClosingReference's `\s*:?\s+` does;
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

// maskCodeContexts is the offset-preserving sibling of stripCodeContexts: it
// returns a string the same length as body in which every byte of a fence line
// and of a fenced block's content becomes a space, and every byte of an inline
// code span (backticks included) becomes codeSpanElision. Newlines are kept.
// The fence and span recognition rules are stripCodeContexts' and
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
// every byte of the span becomes codeSpanElision. An unmatched run is literal.
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
				out.WriteString(strings.Repeat(codeSpanElision, m-i))
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

// partialDeliveryPlan loads the run's approved plan (loadApprovedPlanForRun,
// which walks a retry chain to its planning ancestor) and reports whether it
// declares a PARTIAL delivery through plan.(*Plan).IsPartialDelivery — the one
// shared definition. A load error WARN-logs and reports false, as does a run
// with no plan (nil). Shared by the ship-time guard and the merge-time
// remaining-scope comment.
func (s *Server) partialDeliveryPlan(ctx context.Context, runID uuid.UUID) (*plan.Plan, bool) {
	p, err := s.loadApprovedPlanForRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"partial delivery: load approved plan failed; treating as full delivery",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
		return nil, false
	}
	return p, p.IsPartialDelivery()
}

// neutralizePartialDeliveryClosingRef is the ship-time guard (see the file
// comment). handleShipPullRequest calls it on EVERY successful implement ship —
// after the branch-lineage guard passes and OUTSIDE the running-stage terminal
// drive, so the non-gated flow whose stage the trace handler already advanced is
// covered too — and never on the child-push, fix-up, gating-reject or
// lineage-failure returns, nor on the idempotent replay.
//
// The success path of the ship handler never loads the run row, so this loads it
// itself. Each precondition returns before any forge call: the run must load,
// carry a triggering issue (IssueContext.Number > 0, checked before the plan is
// loaded), and its approved plan must declare a PARTIAL delivery; the GitHub
// client and an installation id must be present (GitLab / no installation is an
// INFO skip — the named residual). It then reads the LIVE PR body, so an agent
// body is judged as GitHub holds it. Zero rewrites → no edit and no audit. The
// audit row is appended ONLY after EditPullRequest succeeds, so the chain never
// claims a rewrite the forge did not take. Every failure WARN-logs and never
// unwinds the ship response.
func (s *Server) neutralizePartialDeliveryClosingRef(ctx context.Context, runID, stageID uuid.UUID, prNumber int) {
	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil || runRow == nil {
		attrs := []slog.Attr{slog.String("run_id", runID.String())}
		if err != nil {
			attrs = append(attrs, slog.String("error", err.Error()))
		}
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"partial delivery guard: load run failed; skipping closing-reference check", attrs...)
		return
	}
	if runRow.IssueContext == nil || runRow.IssueContext.Number <= 0 {
		return
	}
	issueNumber := runRow.IssueContext.Number
	if _, partial := s.partialDeliveryPlan(ctx, runID); !partial {
		return
	}
	if s.cfg.GitHub == nil || runRow.InstallationID == nil || *runRow.InstallationID == 0 {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
			"partial delivery guard: no GitHub client or installation; PR body not checked (GitLab residual)",
			slog.String("run_id", runID.String()),
			slog.Int("issue_number", issueNumber))
		return
	}
	repo, err := parseRepoOwnerName(runRow.Repo)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"partial delivery guard: unparseable repo; skipping closing-reference check",
			slog.String("run_id", runID.String()),
			slog.String("repo", runRow.Repo),
			slog.String("error", err.Error()))
		return
	}
	scope := forge.FromGitHubInstallationID(*runRow.InstallationID)
	// A non-positive prNumber needs no guard of its own: GetPullRequest refuses
	// it before any request, which lands on the WARN-and-skip below.
	pr, err := s.cfg.GitHub.GetPullRequest(ctx, scope, repo, prNumber)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"partial delivery guard: get pr failed; closing reference not checked",
			slog.String("run_id", runID.String()),
			slog.Int("pr_number", prNumber),
			slog.String("error", err.Error()))
		return
	}
	updated, rewritten := neutralizeClosingReferences(pr.Body, issueNumber)
	if rewritten == 0 {
		return
	}
	if err := s.cfg.GitHub.EditPullRequest(ctx, scope, repo, prNumber, updated); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"partial delivery guard: edit pr body failed; the PR still closes the issue on merge",
			slog.String("run_id", runID.String()),
			slog.Int("pr_number", prNumber),
			slog.Int("issue_number", issueNumber),
			slog.String("error", err.Error()))
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"issue_number": issueNumber,
		"pr_number":    prNumber,
		"rewritten":    rewritten,
	})
	systemKind := audit.ActorSystem
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  categoryPartialDeliveryClosingReferenceNeutralized,
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"partial delivery guard: append audit entry failed after the PR body was rewritten",
			slog.String("run_id", runID.String()),
			slog.Int("pr_number", prNumber),
			slog.String("error", err.Error()))
		return
	}
	s.notifyOperatorVisible(ctx, runID, categoryPartialDeliveryClosingReferenceNeutralized)
}
