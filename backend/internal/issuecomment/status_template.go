package issuecomment

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// RenderStatusBody returns the markdown body for the sticky status
// comment (E20.3 / #329). Pure — no IO, no time.Now() — so the
// caller (`NotifyStatusUpdate` / the transition wiring in E20.4)
// has full control over what gets surfaced.
//
// Composition:
//
//  1. Header: "Fishhawk run [<short-id>](<run-url>) — <workflow_id> · <state>"
//  2. Stage list: one row per stage with a state icon, type name,
//     and the bare state text. Ordered by sequence.
//  3. "Latest activity" subsection: up to 3 audit rows rendered as
//     verb + actor + relative time. Unknown / noisy categories
//     (status_comment_posted, trace_uploaded, installation_token_issued)
//     are filtered out so the user-facing surface stays readable.
//  4. Footer: "View run" link + optional "Pull request" link when
//     `run.PullRequestURL` is stamped.
//
// `recentAudit` is the caller's slice of recent entries (any
// ordering). The renderer picks the most-recent N (sequence desc)
// from the interesting subset. `now` is the reference point for
// the "ago" timestamps; passing the notifier's own `now()` keeps
// rendering deterministic under test.
func RenderStatusBody(runRow *run.Run, stages []*run.Stage, recentAudit []*audit.Entry, externalURL string, now time.Time) string {
	if runRow == nil {
		return ""
	}
	var b strings.Builder
	writeHeader(&b, runRow, externalURL)
	b.WriteString("\n")
	writeStages(&b, stages)
	if activity := pickActivity(recentAudit, 3); len(activity) > 0 {
		b.WriteString("\n_Latest activity:_\n")
		for _, e := range activity {
			fmt.Fprintf(&b, "- %s · %s\n", renderActivityLine(e), relativeAge(e.Timestamp, now))
		}
	}
	b.WriteString("\n")
	writeFooter(&b, runRow, externalURL)
	return b.String()
}

func writeHeader(b *strings.Builder, r *run.Run, externalURL string) {
	short := shortID(r.ID)
	runURL := externalURL + "/runs/" + r.ID.String()
	fmt.Fprintf(b, "**Fishhawk run [`%s`](%s)** — `%s` · %s\n",
		short, runURL, r.WorkflowID, runStateIcon(r.State)+" "+string(r.State))
}

func writeStages(b *strings.Builder, stages []*run.Stage) {
	if len(stages) == 0 {
		b.WriteString("\n_No stages yet._\n")
		return
	}
	// Stages come back in sequence order from ListStagesForRun; we
	// re-sort defensively in case the caller hands a different
	// order, but the typical input is already in the right shape.
	b.WriteString("\n")
	for _, s := range stages {
		fmt.Fprintf(b, "- %s `%s` · %s\n",
			stageStateIcon(s.State), s.Type, string(s.State))
	}
}

func writeFooter(b *strings.Builder, r *run.Run, externalURL string) {
	runURL := externalURL + "/runs/" + r.ID.String()
	fmt.Fprintf(b, "[View run →](%s)", runURL)
	if r.PullRequestURL != nil && *r.PullRequestURL != "" {
		fmt.Fprintf(b, " · [Pull request →](%s)", *r.PullRequestURL)
	}
	b.WriteString("\n")
}

// pickActivity returns up to `limit` interesting audit entries
// ordered most-recent-first. Categories outside the user-readable
// set are filtered. Status-comment-posted rows are always filtered
// (recursive — every transition writes one, but reporting "the
// status comment was updated" inside the status comment is noise).
func pickActivity(entries []*audit.Entry, limit int) []*audit.Entry {
	if limit <= 0 || len(entries) == 0 {
		return nil
	}
	out := make([]*audit.Entry, 0, len(entries))
	for _, e := range entries {
		if _, ok := activityCategories[e.Category]; !ok {
			continue
		}
		out = append(out, e)
	}
	// Most-recent first.
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].Sequence > out[i].Sequence {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// activityCategories is the closed set of audit categories the
// status comment surfaces. Anything else (status_comment_posted
// itself, trace_uploaded, installation_token_issued, policy_evaluated,
// etc.) is filtered as system noise. The set tracks the audit-row
// vocabulary in `backend/internal/audit/categories.md` (conceptual
// — we don't have a doc, but the source-of-truth list lives in
// the comments throughout the codebase).
var activityCategories = map[string]struct{}{
	"run_dispatched":              {},
	"plan_generated":              {},
	"approval_submitted":          {},
	"plan_approved_via_reaction":  {}, // future, post-E17.4
	"ci_failure_retry_dispatched": {},
	"ci_retry_exhausted":          {},
	"pr_approved_on_github":       {},
	"pr_review_submitted":         {},
	"pr_merged":                   {},
	"pr_closed_without_merge":     {},
	"stage_retried":               {},
	// Fan-in outcome of a decomposed parent (E24.7 / #1147). These are
	// system-actor audit kinds (ADR-041 / #1142) with no dedicated Notifier
	// method; they render data-drivenly through this activity set so the
	// living-anchor timeline reflects whether the slices integrated cleanly.
	"slices_integrated":          {},
	"slice_integration_conflict": {},
	// Implement-model resolution at the plan gate (#1013). The operator-gate
	// slice emits this when a plan-stage approve resolves the implement model;
	// surfacing it lets the issue thread show which model (and which rung)
	// will drive the implement stage.
	"model_resolved": {},
}

// renderActivityLine returns a user-readable verb-phrase for an
// audit entry. Falls back to the bare category name + actor when
// the category has no template — keeps the output stable if the
// audit vocabulary grows.
func renderActivityLine(e *audit.Entry) string {
	actor := actorMention(e.ActorSubject)
	switch e.Category {
	case "run_dispatched":
		return "Fishhawk run dispatched"
	case "plan_generated":
		return "Plan posted"
	case "approval_submitted":
		// Prefer the resolved GitHub login (#751); never @-mention the raw
		// token subject (#755) — approverMention renders the three-form
		// identity convention (#1053) instead.
		return fmt.Sprintf("%s %s the plan", approverMention(e), approvalDecisionVerb(e.Payload))
	case "plan_approved_via_reaction":
		return fmt.Sprintf("%s approved on GitHub (reaction)", actor)
	case "ci_failure_retry_dispatched":
		return fmt.Sprintf("CI failed; retry dispatched%s", retryAttemptSuffix(e.Payload))
	case "ci_retry_exhausted":
		return "Retry cap reached"
	case "pr_approved_on_github":
		return fmt.Sprintf("%s approved on GitHub", actor)
	case "pr_review_submitted":
		return fmt.Sprintf("%s left a review", actor)
	case "pr_merged":
		return fmt.Sprintf("%s merged the PR", actor)
	case "pr_closed_without_merge":
		return fmt.Sprintf("%s closed the PR without merging", actor)
	case "stage_retried":
		return "Stage retried"
	case "slices_integrated":
		return "Slices integrated"
	case "slice_integration_conflict":
		return "Slice integration conflict"
	case "model_resolved":
		return renderModelResolvedLine(e.Payload)
	default:
		if actor == "" {
			return e.Category
		}
		return fmt.Sprintf("%s · %s", actor, e.Category)
	}
}

// actorMention renders an audit actor as a GitHub `@`-mention ONLY when
// the subject is a syntactically valid GitHub login (validApproverLogin,
// notifier.go — same package). A non-login subject (e.g. the MCP token
// subject "brett@local-mcp", or the "anonymous"/"system"/"github-webhook"
// sentinels) returns "" so it never produces a bogus mention that pings an
// unrelated GitHub user (#755). Webhook-sourced rows carry real logins and
// are unaffected.
func actorMention(actor *string) string {
	if actor == nil || !validApproverLogin(*actor) {
		return ""
	}
	return "@" + *actor
}

// approverMention renders the approver for an approval_submitted activity
// row, preferring the resolved GitHub login the MCP loop threads through
// (#751, approver_github_login); otherwise the acting subject (the
// payload's `approver`, falling back to the row's actor_subject) goes
// through the shared renderApproverIdentity three-form convention
// (notifier.go, #1053): login mention / operator-agent role + delegation
// rule / verbatim code span — so a non-login token subject is never
// `@`-mentioned (#755) yet keeps its identity. Mirrors the plan-status
// footer exactly.
func approverMention(e *audit.Entry) string {
	id := decodeApproverIdentity(e.Payload)
	if validApproverLogin(id.githubLogin) {
		return "@" + id.githubLogin
	}
	subject := id.approver
	if subject == "" && e.ActorSubject != nil {
		subject = *e.ActorSubject
	}
	return renderApproverIdentity(subject, id.delegated)
}

// approverIdentity carries the identity fields of an approval_submitted
// payload that approverMention renders.
type approverIdentity struct {
	approver    string
	githubLogin string
	delegated   string
}

// decodeApproverIdentity extracts the acting subject, the resolved
// GitHub login (#751), and the ADR-040 delegation rule (#1026) from an
// approval_submitted payload; zero value when absent or unparseable.
func decodeApproverIdentity(payload json.RawMessage) approverIdentity {
	if len(payload) == 0 {
		return approverIdentity{}
	}
	var p struct {
		Approver            string `json:"approver"`
		ApproverGithubLogin string `json:"approver_github_login"`
		Delegated           string `json:"delegated"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return approverIdentity{}
	}
	return approverIdentity{
		approver:    p.Approver,
		githubLogin: p.ApproverGithubLogin,
		delegated:   p.Delegated,
	}
}

func approvalDecisionVerb(payload json.RawMessage) string {
	if len(payload) == 0 {
		return "acted on"
	}
	var p struct {
		Decision string `json:"decision"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return "acted on"
	}
	switch p.Decision {
	case "approve":
		return "approved"
	case "reject":
		return "rejected"
	}
	return "acted on"
}

// renderModelResolvedLine renders a model_resolved activity row (#1013):
// "Implement model resolved: `<model>` (source: <rung>)". An empty model is
// the deliberate default spawn — rendered as "(adapter default)" so the
// timeline reads honestly rather than showing an empty code span. Falls back
// to a bare verb on an unparseable payload.
func renderModelResolvedLine(payload json.RawMessage) string {
	model, source := decodeModelResolved(payload)
	if model == "" {
		return "Implement model resolved: adapter default"
	}
	if source == "" {
		return fmt.Sprintf("Implement model resolved: `%s`", model)
	}
	return fmt.Sprintf("Implement model resolved: `%s` (source: %s)", model, source)
}

// decodeModelResolved reads the {model, model_source} payload the approval
// gate stamps on a model_resolved entry (ResolvedModel's json tags). Returns
// ("", "") on any decode failure.
func decodeModelResolved(payload json.RawMessage) (model, source string) {
	if len(payload) == 0 {
		return "", ""
	}
	var p struct {
		Model       string `json:"model"`
		ModelSource string `json:"model_source"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return "", ""
	}
	return p.Model, p.ModelSource
}

func retryAttemptSuffix(payload json.RawMessage) string {
	if len(payload) == 0 {
		return ""
	}
	var p struct {
		RetryAttempt int `json:"retry_attempt"`
		MaxRetries   int `json:"max_retries"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return ""
	}
	if p.RetryAttempt <= 0 {
		return ""
	}
	if p.MaxRetries > 0 {
		return fmt.Sprintf(" (attempt %d/%d)", p.RetryAttempt, p.MaxRetries)
	}
	return fmt.Sprintf(" (attempt %d)", p.RetryAttempt)
}

// stageStateIcon maps a stage state to its glyph. Closed set: a
// future state machine extension must add its icon here or land as
// the fallback question mark.
func stageStateIcon(s run.StageState) string {
	switch s {
	case run.StageStatePending:
		return "⏳"
	case run.StageStateDispatched:
		return "🚀"
	case run.StageStateRunning:
		return "🔄"
	case run.StageStateAwaitingApproval:
		return "👋"
	case run.StageStateSucceeded:
		return "✅"
	case run.StageStateFailed:
		return "❌"
	case run.StageStateCancelled:
		return "🚫"
	}
	return "❓"
}

// runStateIcon mirrors stageStateIcon for run-level state. Runs
// don't have `dispatched` / `awaiting_approval` (those are stage-
// only) so the set is narrower.
func runStateIcon(s run.State) string {
	switch s {
	case run.StatePending:
		return "⏳"
	case run.StateRunning:
		return "🔄"
	case run.StateSucceeded:
		return "✅"
	case run.StateFailed:
		return "❌"
	case run.StateCancelled:
		return "🚫"
	}
	return "❓"
}

// relativeAge returns a short "Xm ago" / "Xh ago" / "Xd ago" /
// absolute-date string. The reference point `now` lets callers
// pass deterministic clocks under test.
func relativeAge(ts, now time.Time) string {
	d := now.Sub(ts)
	switch {
	case d < 0:
		// Clock skew or pre-set timestamp. Render as "just now"
		// rather than "in 3m" — the comment is about the past.
		return "just now"
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
	return ts.Format("Jan 2")
}

// shortID lives in notifier.go; reused here for the comment header.
