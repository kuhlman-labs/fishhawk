package issuecomment

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// PRReviewEventComment is the ONLY GitHub pull-request-review event Fishhawk
// ever posts from the agent-review surface (E42.2 / #1785). Agent reviewer
// verdicts are ADVISORY — the operator arbitrates the gate — so they post as
// COMMENT-type reviews, which do NOT participate in branch protection.
// APPROVE / REQUEST_CHANGES are the branch-protection-relevant blocking
// events; posting one from a bot would silently hard-block (or unblock) merge
// under branch protection, changing gate semantics. This constant is the
// load-bearing safety property: the COMMENT-type invariant is pinned by a
// dedicated test asserting it can never be "APPROVE" or "REQUEST_CHANGES".
const PRReviewEventComment = "COMMENT"

// RenderPRReviewBody renders ONE reviewer's implement_reviewed verdict as the
// body of an advisory COMMENT-type PR review (E42.2 / #1785). It decodes the
// entry's planreview.ImplementReviewedPayload and renders: a header line
// naming the verdict token (with a severity-bucketed concern-count suffix), a
// severity-bucketed concern list reusing the anchor's
// `- **<severity>** (<category>): <note>` shape, the free_form block, a
// concern-resolutions arc when the reviewer judged prior concerns (a
// post-fixup re-review round), and an attribution line naming the reviewer
// model plus a link back to the run. Pure (no I/O).
//
// Returns "" when the entry is nil, carries no payload, does not decode, or
// carries no verdict — the caller then posts nothing.
func RenderPRReviewBody(entry *audit.Entry, runRow *run.Run, externalURL string) string {
	if entry == nil || len(entry.Payload) == 0 {
		return ""
	}
	var p planreview.ImplementReviewedPayload
	if err := json.Unmarshal(entry.Payload, &p); err != nil {
		return ""
	}
	if p.Verdict == "" {
		// A payload with no verdict is degenerate — nothing meaningful to post.
		return ""
	}

	// Reuse the anchor's concern shape + severity ordering so the PR review
	// reads identically to the issue anchor's per-reviewer <details>.
	concerns := make([]anchorReviewConcern, 0, len(p.Concerns))
	for _, c := range p.Concerns {
		concerns = append(concerns, anchorReviewConcern{
			severity: string(c.Severity),
			category: c.Category,
			note:     c.Note,
		})
	}

	var b strings.Builder
	b.WriteString("### 🤖 Agent implement review\n\n")

	header := fmt.Sprintf("**Verdict:** `%s`", p.Verdict)
	if suffix := severityCountSuffix(concerns); suffix != "" {
		header += " " + suffix
	}
	b.WriteString(header + "\n")

	if len(concerns) > 0 {
		b.WriteString("\n")
		for _, c := range concerns {
			sev := c.severity
			if sev == "" {
				sev = "concern"
			}
			cat := c.category
			if cat == "" {
				cat = "general"
			}
			fmt.Fprintf(&b, "- **%s** (%s): %s\n", sev, cat, c.note)
		}
	}

	if strings.TrimSpace(p.FreeForm) != "" {
		fmt.Fprintf(&b, "\n%s\n", strings.TrimRight(p.FreeForm, "\n"))
	}

	// Concern-resolutions arc: the reviewer's delta verdicts on prior concerns
	// threaded into a re-review prompt (a post-fixup round). Rendered only when
	// present so a first review carries no arc.
	if len(p.ConcernResolutions) > 0 {
		b.WriteString("\n**Prior concerns**\n\n")
		for _, r := range p.ConcernResolutions {
			res := r.Resolution
			if res == "" {
				res = "unknown"
			}
			line := fmt.Sprintf("- `%s` → **%s**", shortConcernID(r.ID), res)
			if strings.TrimSpace(r.Note) != "" {
				line += ": " + oneLine(r.Note)
			}
			b.WriteString(line + "\n")
		}
	}

	// Attribution line: reviewer model + a link back to the run, so the PR
	// review makes clear which reviewer (and which run) produced the verdict.
	model := p.ReviewerModel
	if model == "" {
		model = "agent reviewer"
	}
	fmt.Fprintf(&b, "\n_Advisory review by `%s`", model)
	if runRow != nil {
		runURL := strings.TrimRight(externalURL, "/") + "/runs/" + runRow.ID.String()
		fmt.Fprintf(&b, " · [view run →](%s)", runURL)
	}
	b.WriteString("._\n")

	return strings.TrimRight(b.String(), "\n")
}

// shortConcernID renders a concern's UUID compactly for the resolutions arc —
// the first segment of a hyphenated UUID, or the whole id when it carries no
// hyphen (or is empty, rendered as "concern").
func shortConcernID(id string) string {
	if id == "" {
		return "concern"
	}
	if i := strings.IndexByte(id, '-'); i > 0 {
		return id[:i]
	}
	return id
}
