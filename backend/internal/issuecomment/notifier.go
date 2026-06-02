// Package issuecomment posts back to GitHub-issue-triggered runs in
// the place the user already lives — the issue conversation (#234).
//
// Two moments matter for the v0 demo loop:
//   - Pickup: the dispatcher accepted the trigger and created a Run.
//     Without this, the user labels an issue and Fishhawk vanishes.
//   - Plan ready: the plan stage produced a `standard_v1` artifact
//     and either parked at awaiting_approval (gated workflow) or
//     succeeded (gateless). Without this, the user has to alt-tab to
//     the SPA to see what was proposed.
//
// Why a separate package: same shape as `auditcheckpublisher`. The
// dispatcher and the trace handler both live in different layers,
// both want the same I/O + dedup + body-template logic, and a thin
// helper avoids spreading that code through both call sites.
//
// Idempotency: every successful post writes an `issue_commented`
// chained audit entry with `payload.kind` naming the kind (plan,
// plan_approved, etc.). Before each post we read back the run's
// audit log and skip if a matching row already exists. Audit-log
// dedup matches the integrity story — "we said we did it" lives
// next to "we did it" — and survives process restarts.
//
// What this package does NOT do:
//   - Comment on PR-triggered or CLI-triggered runs. The trigger
//     surface is different (PR conversation, terminal). Out of
//     scope per #234.
//   - Comment on run completion. Worth doing eventually; not in v0.
//   - Honor customizable comment templates from the workflow spec.
package issuecomment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// CategoryIssueCommented is the audit-log category the notifier writes
// after a successful post. Static so the dedup query
// (`ListForRunByCategory`) can index on it.
const CategoryIssueCommented = "issue_commented"

// CategoryStatusCommentPosted records that Fishhawk created or
// edited the run's sticky status comment (E20 / #326). Distinct
// from CategoryIssueCommented because the lookup pattern is
// different — for the status comment we want "the latest comment
// id for this run" (so we know what to edit next time); for the
// other notifications we want "did we already post this kind"
// (one-shot dedup). A separate category makes the lookup query
// cleaner and lets compliance consumers index on intent.
//
// Each transition that triggers a status update appends a fresh
// row; the most-recent row carries the canonical `github_comment_id`.
// The audit log therefore records the timeline of state changes
// (one row per transition) AND the comment id (same id across all
// rows for a run, until the operator deletes it manually and the
// notifier falls back to creating a new one).
const CategoryStatusCommentPosted = "status_comment_posted"

// Kind enumerates which moment a comment recorded. Stored in the
// audit entry's payload so a single category covers both moments
// while staying queryable per kind.
type Kind string

// Kind values.
const (
	KindPlan Kind = "plan"
	// KindCIRetry tags a comment posted when the dispatcher fires a
	// CI-failure auto-retry (#279 / E16). Dedup is per-attempt: the
	// payload also carries `retry_attempt`, and the dedup query
	// matches both kind and retry_attempt so re-deliveries of the
	// same check_run.completed event don't double-post but a fresh
	// retry round (attempt N → N+1) still announces itself.
	KindCIRetry Kind = "ci_retry"
	// KindStatusUpdate tags the sticky-status-comment audit row
	// (E20.2 / #328). Lives on CategoryStatusCommentPosted, not
	// CategoryIssueCommented. Distinct enum lets future analytics
	// answer "how many status updates fired per run" without
	// scanning payload kinds.
	KindStatusUpdate Kind = "status_update"
	// KindPlanFull tags the full-plan-document post (E17.2 / #337).
	// Distinct from KindPlan (the legacy summary post) because the
	// payload carries github_comment_id and the comment is editable
	// via UpdateIssueComment on subsequent plan re-uploads when the
	// spec opts in to `update_on_change`. Audit-log dedup uses this
	// kind plus KindPlanUpdated to find the most-recent comment id
	// for a run.
	KindPlanFull Kind = "plan_full"
	// KindPlanUpdated tags an edit-in-place of the full-plan
	// comment. Each re-upload that lands a UpdateIssueComment call
	// appends one row carrying the (unchanged) github_comment_id so
	// the audit chain records every revision.
	KindPlanUpdated Kind = "plan_updated"
	// KindBudgetAlert tags an advisory periodic-budget warning comment
	// (ADR-030 / #688). Non-sticky and append-only like KindCIRetry; the
	// payload also carries `period_start` + `budget_tier` so the dedup
	// query fires the warn-tier comment and the 100% comment at most once
	// each per calendar period while a re-evaluation in the same period
	// (or a redelivered upload) is absorbed.
	KindBudgetAlert Kind = "budget_alert"
)

// IssueCommenter is the slice of githubclient.Client this package
// needs. Defining it as an interface keeps the unit tests free of a
// fake api.github.com and lets the dispatcher's existing GitHubAPI
// shape stay focused.
//
// CreateIssueComment returns the created IssueComment so the
// sticky-status-comment flow (E20.2 / #328) and the plan
// `update_on_change` flow (E17.2 / #337) can persist the comment
// id for later edits via UpdateIssueComment.
type IssueCommenter interface {
	CreateIssueComment(ctx context.Context, installationID int64, repo githubclient.RepoRef, issueNumber int, body string) (*githubclient.IssueComment, error)
	UpdateIssueComment(ctx context.Context, installationID int64, repo githubclient.RepoRef, commentID int64, body string) (*githubclient.IssueComment, error)
}

// Notifier owns the comment-back I/O. Construct once with New and
// share — methods are safe for concurrent use (each post writes an
// independent audit entry, and the dedup check is read-then-write
// scoped to a single run).
type Notifier struct {
	github      IssueCommenter
	runs        run.Repository
	audit       audit.Repository
	externalURL string
	now         func() time.Time
}

// Deps groups the dependencies New needs.
type Deps struct {
	GitHub      IssueCommenter
	Runs        run.Repository
	Audit       audit.Repository
	ExternalURL string
	// Now is the clock used for audit timestamps; defaults to
	// time.Now. Overridable for deterministic tests.
	Now func() time.Time
}

// New returns a Notifier. Returns nil when the deps don't add up to
// a working notifier so callers can `notifier.NotifyPlanReady(...)`
// without nil-checking the receiver — the methods short-circuit on
// a nil receiver.
//
// We bail on missing ExternalURL because every comment carries at
// least one Fishhawk URL; without ExternalURL the comment would
// link nowhere useful.
func New(d Deps) *Notifier {
	if d.GitHub == nil || d.Runs == nil || d.Audit == nil || d.ExternalURL == "" {
		return nil
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	return &Notifier{
		github:      d.GitHub,
		runs:        d.Runs,
		audit:       d.Audit,
		externalURL: strings.TrimRight(d.ExternalURL, "/"),
		now:         now,
	}
}

// NotifyPlanReady posts the plan-ready comment after the plan stage
// transitions terminally. `planStage` is the plan stage row;
// `planArtifact` is the typed `*plan.Plan` from its standard_v1
// artifact. Both are required — if either is nil the call skips.
//
// Two surfaces today (E17.2 / #337):
//
//   - **Full-plan-as-comment**: when the workflow spec declares the
//     plan stage's `produces.persistence` with
//     `target: originating_issue, mode: rendered_comment`, the call
//     renders the whole standard_v1 plan as a markdown document and
//     posts it on the issue. If `update_on_change: true` is also set
//     and a prior full-plan comment exists for the run, the call
//     edits the existing comment in place via UpdateIssueComment.
//     Per ADR-020 / #321 this is the canonical plan-review surface.
//
//   - **Summary-only**: when the spec opts out (no
//     originating_issue persistence), the legacy path posts a short
//     summary comment that links to the SPA's plan-document page.
//     Behavior unchanged from #234.
//
// The comment routes to the approval-surface URL when the plan
// stage requires approval (v0's typical workflow); to the run page
// otherwise (`routine_change`-style flows).
func (n *Notifier) NotifyPlanReady(ctx context.Context, runID uuid.UUID, planStage *run.Stage, planArtifact *plan.Plan) error {
	if n == nil || planStage == nil || planArtifact == nil {
		return nil
	}
	runRow, err := n.runs.GetRun(ctx, runID)
	if err != nil {
		return fmt.Errorf("issuecomment: get run: %w", err)
	}
	// Per-stage persistence config lives on the cached workflow
	// spec (the dispatcher snapshotted it onto the run row per
	// #283). Read once; nil means "use the legacy summary path".
	persistence := planOriginatingIssuePersistence(runRow.WorkflowSpec, runRow.WorkflowID, planStage)
	if persistence != nil && persistence.Mode == spec.ModeRenderedComment {
		return n.notifyFullPlan(ctx, runID, planStage, planArtifact, *persistence)
	}
	// Legacy path: summary post with audit-log dedup.
	ctxv, ok, err := n.contextFor(ctx, runID, KindPlan)
	if err != nil || !ok {
		return err
	}
	body := renderPlanBody(ctxv, planStage, planArtifact, n.externalURL)
	return n.post(ctx, ctxv, KindPlan, body)
}

// MaxIssueCommentBodyBytes mirrors GitHub's per-comment body cap.
// Render output that exceeds this is truncated with a "View full
// plan →" link to the SPA's plan-document page. Documented at
// https://docs.github.com/en/rest/issues/comments — the practical
// cap is 65,536 characters; we treat the limit as bytes since UTF-8
// payloads can be longer than rune counts.
const MaxIssueCommentBodyBytes = 65_536

// notifyFullPlan is the post-or-edit path for the full-plan-on-issue
// surface (E17.2 / #337). Resolves the plan-comment id from the
// audit log; CreateIssueComment when none exists, UpdateIssueComment
// when one does AND update_on_change is set, no-op when one exists
// but update_on_change is unset (the post is one-shot in that mode).
func (n *Notifier) notifyFullPlan(ctx context.Context, runID uuid.UUID, planStage *run.Stage, planArtifact *plan.Plan, persistence spec.Persistence) error {
	ctxv, ok, err := n.contextForStatus(ctx, runID)
	if err != nil || !ok {
		return err
	}

	existingID, err := n.findPlanCommentID(ctx, runID)
	if err != nil {
		return fmt.Errorf("issuecomment: lookup plan comment: %w", err)
	}

	// Plan-status footer (#377): drives the `_Status: approved by
	// @x · implementing now_` / rejected line at the bottom of the
	// comment so subscribers see the approval result without a
	// second broadcast. Nil status = no approval row yet (the
	// awaiting-approval first post).
	status, err := n.latestPlanApproval(ctx, runID, planStage.ID)
	if err != nil {
		return fmt.Errorf("issuecomment: lookup plan approval: %w", err)
	}

	body := renderFullPlanBody(ctxv, planStage, planArtifact, n.externalURL, status)

	if existingID > 0 {
		if !persistence.UpdateOnChange {
			// First plan post is the final post for this surface;
			// re-fires shouldn't re-edit. Skip silently.
			return nil
		}
		got, updErr := n.github.UpdateIssueComment(ctx, *ctxv.run.InstallationID,
			ctxv.repo, existingID, body)
		switch {
		case updErr == nil:
			return n.appendPlanCommentAudit(ctx, ctxv, got.ID, KindPlanUpdated)
		case errors.Is(updErr, githubclient.ErrNotFound):
			// Operator deleted the comment; fall through to create.
		default:
			return fmt.Errorf("issuecomment: update plan comment: %w", updErr)
		}
	}

	created, err := n.github.CreateIssueComment(ctx, *ctxv.run.InstallationID,
		ctxv.repo, ctxv.issueNumber, body)
	if err != nil {
		return fmt.Errorf("issuecomment: create plan comment: %w", err)
	}
	return n.appendPlanCommentAudit(ctx, ctxv, created.ID, KindPlanFull)
}

// latestPlanApproval walks the run's `approval_submitted` audit
// rows newest-first and returns the latest one scoped to the plan
// stage. Returns (nil, nil) when no row exists yet (the
// awaiting-approval first post). #377.
func (n *Notifier) latestPlanApproval(ctx context.Context, runID, planStageID uuid.UUID) (*planStatus, error) {
	entries, err := n.audit.ListForRunByCategory(ctx, runID, "approval_submitted")
	if err != nil {
		return nil, err
	}
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e.StageID == nil || *e.StageID != planStageID {
			continue
		}
		var p struct {
			Decision string `json:"decision"`
			Approver string `json:"approver"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			// Defensive: a malformed row shouldn't block the
			// comment render. Treat as no-status-yet.
			continue
		}
		return &planStatus{decision: approval.Decision(p.Decision), approver: p.Approver}, nil
	}
	return nil, nil
}

// findPlanCommentID returns the most-recent github_comment_id from
// the run's audit log across KindPlanFull and KindPlanUpdated rows.
// Returns 0 when none exists (the first-post case).
func (n *Notifier) findPlanCommentID(ctx context.Context, runID uuid.UUID) (int64, error) {
	entries, err := n.audit.ListForRunByCategory(ctx, runID, CategoryIssueCommented)
	if err != nil {
		return 0, err
	}
	// ListForRunByCategory returns ascending-by-sequence; walk from
	// the end so the latest comment id wins.
	for i := len(entries) - 1; i >= 0; i-- {
		k := extractKind(entries[i].Payload)
		if k != KindPlanFull && k != KindPlanUpdated {
			continue
		}
		if id := extractGithubCommentID(entries[i].Payload); id > 0 {
			return id, nil
		}
	}
	return 0, nil
}

// appendPlanCommentAudit records a full-plan create/update on the
// issue_commented category. Payload carries the kind + comment id +
// issue/repo for compliance consumers that index on intent.
func (n *Notifier) appendPlanCommentAudit(ctx context.Context, ctxv commentContext, commentID int64, kind Kind) error {
	systemKind := audit.ActorSystem
	payload, _ := json.Marshal(map[string]any{
		"kind":              string(kind),
		"issue_number":      ctxv.issueNumber,
		"repo":              ctxv.repo.String(),
		"github_comment_id": commentID,
	})
	if _, err := n.audit.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     ctxv.run.ID,
		Timestamp: n.now().UTC(),
		Category:  CategoryIssueCommented,
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		return fmt.Errorf("issuecomment: audit append: %w", err)
	}
	return nil
}

// planOriginatingIssuePersistence pulls the plan stage's
// originating_issue persistence config out of the cached workflow
// spec. Returns nil when the spec opts out — that's the signal for
// NotifyPlanReady to use the legacy summary-post path.
//
// The lookup walks the workflow named by `workflowID` (the run's
// WorkflowID column, which is the dispatcher-resolved key into the
// spec's Workflows map) and finds the first stage of type plan.
// v0 workflows have at most one plan stage per workflow.
//
// Best-effort: a parse failure falls through to the legacy path.
// The spec was validated at dispatch time per #283, so this is
// defensive against future shape drift.
func planOriginatingIssuePersistence(workflowSpec []byte, workflowID string, planStage *run.Stage) *spec.Persistence {
	if len(workflowSpec) == 0 || planStage == nil || workflowID == "" {
		return nil
	}
	parsed, err := spec.ParseBytes(workflowSpec)
	if err != nil {
		return nil
	}
	wf, ok := parsed.Workflows[workflowID]
	if !ok {
		return nil
	}
	for i := range wf.Stages {
		if string(wf.Stages[i].Type) != string(planStage.Type) {
			continue
		}
		for _, p := range wf.Stages[i].Produces {
			if p.Artifact != spec.ArtifactPlan {
				continue
			}
			for _, ps := range p.Persistence {
				if ps.Target == spec.PersistenceOriginatingIssue {
					pp := ps
					return &pp
				}
			}
		}
		// First plan stage wins; later plan stages (if a future
		// workflow shape allows them) would need a more specific
		// match — file a follow-up if that happens.
		return nil
	}
	return nil
}

// planStatus carries the latest approval state for the plan stage,
// used by renderFullPlanBody to render the `_Status:_` footer
// introduced in #377. Nil status renders as "awaiting approval";
// approve / reject render named after the actor.
type planStatus struct {
	decision approval.Decision
	approver string
}

// renderFullPlanBody renders the entire standard_v1 plan as a
// markdown document for the issue thread (E17.2 / #337). Sections:
//
//	**Fishhawk plan** (header + run + workflow link)
//	**Summary**: <plan.Summary>
//	**Scope**: bullet list of plan.Scope.Files
//	**Approach**:
//	  1. step 1
//	  2. step 2
//	**Verification**:
//	  Test strategy: ...
//	  Rollback plan: ...
//	**Risks & assumptions**: bullets
//	[Approve in the dashboard →](url) | [Reject →](url)
//	_Status: …_  (when status is non-nil; #377)
//
// When the rendered body exceeds MaxIssueCommentBodyBytes the
// renderer truncates at a safe rune boundary and appends a
// "View full plan →" link to the SPA's plan-document page so the
// reviewer can see the untruncated body.
func renderFullPlanBody(c commentContext, planStage *run.Stage, p *plan.Plan, externalURL string, status *planStatus) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**Fishhawk plan** for Run [`%s`](%s)\n\n", shortID(c.run.ID), c.runURL)
	fmt.Fprintf(&b, "Workflow: `%s`\n\n", c.run.WorkflowID)

	if p.Summary != "" {
		fmt.Fprintf(&b, "**Summary**\n\n%s\n\n", p.Summary)
	}
	if files := renderFileList(p.Scope.Files, len(p.Scope.Files)); files != "" {
		b.WriteString("**Scope**\n\n")
		b.WriteString(files)
		b.WriteString("\n")
	}
	if len(p.Approach) > 0 {
		b.WriteString("**Approach**\n\n")
		for _, s := range p.Approach {
			fmt.Fprintf(&b, "%d. %s\n", s.Step, s.Description)
		}
		b.WriteString("\n")
	}
	if p.Verification.TestStrategy != "" || p.Verification.RollbackPlan != "" {
		b.WriteString("**Verification**\n\n")
		if p.Verification.TestStrategy != "" {
			fmt.Fprintf(&b, "- **Test strategy**: %s\n", p.Verification.TestStrategy)
		}
		if p.Verification.RollbackPlan != "" {
			fmt.Fprintf(&b, "- **Rollback plan**: %s\n", p.Verification.RollbackPlan)
		}
		b.WriteString("\n")
	}
	if len(p.RisksAndAssumptions) > 0 {
		b.WriteString("**Risks & assumptions**\n\n")
		for _, r := range p.RisksAndAssumptions {
			fmt.Fprintf(&b, "- %s\n", r)
		}
		b.WriteString("\n")
	}

	if planStage.RequiresApproval {
		fmt.Fprintf(&b, "[Approve in the dashboard →](%s/runs/%s/stages/%s)\n",
			externalURL, c.run.ID.String(), planStage.ID.String())
	} else {
		fmt.Fprintf(&b, "[View run →](%s)\n", c.runURL)
	}
	b.WriteString("\n_Or approve from this thread by replying " + "`+1`" + " / " + "`lgtm`" + "; reply " + "`/fishhawk reject <reason>`" + " to block with a rationale._\n")
	if footer := renderPlanStatusFooter(status); footer != "" {
		b.WriteString("\n")
		b.WriteString(footer)
		b.WriteString("\n")
	}
	return truncateForGitHubComment(b.String(), c.runURL, planStage.ID.String(), externalURL, c.run.ID.String())
}

// renderPlanStatusFooter returns the italic status line appended to
// the plan-on-issue comment, naming the actor that cleared or
// blocked the gate. Empty when no approval audit row exists yet
// (the comment first lands awaiting approval).
func renderPlanStatusFooter(s *planStatus) string {
	if s == nil {
		return ""
	}
	actor := renderApproverHandle(s.approver)
	switch s.decision {
	case approval.DecisionApprove:
		return fmt.Sprintf("_Status: approved by %s · implementing now_", actor)
	case approval.DecisionReject:
		return fmt.Sprintf("_Status: rejected by %s_", actor)
	}
	return ""
}

// renderApproverHandle picks the human-facing form of the audit
// row's subject: `@<login>` for real GitHub logins, "an approver"
// for the literal "anonymous" or empty (matches the convention the
// retired renderPlanApprovedBody used so the issue thread never
// leaks `@anonymous`).
func renderApproverHandle(subject string) string {
	if validApproverLogin(subject) {
		return "@" + subject
	}
	return "an approver"
}

// truncateForGitHubComment caps body at MaxIssueCommentBodyBytes,
// dropping bytes from the end and appending a "View full plan →"
// link to the SPA so the reviewer can see the rest. Pure function;
// safe to call when the body is already short — returns body
// unchanged.
func truncateForGitHubComment(body, runURL, stageID, externalURL, runID string) string {
	if len(body) <= MaxIssueCommentBodyBytes {
		return body
	}
	tail := fmt.Sprintf("\n\n_…truncated — [view full plan →](%s/runs/%s/stages/%s)_\n",
		externalURL, runID, stageID)
	// Reserve room for the tail; trim conservatively.
	budget := MaxIssueCommentBodyBytes - len(tail)
	if budget < 0 {
		// Tail itself exceeds the cap (would be a very long URL).
		// Render only the bare run URL as a fallback so the
		// reviewer can navigate without leaving the comment.
		return fmt.Sprintf("_Plan exceeds GitHub's comment size; view at %s_\n", runURL)
	}
	cut := budget
	// Back off any UTF-8 continuation bytes so we land on a rune
	// boundary — same defense the summary path's truncate uses.
	for cut > 0 && (body[cut]&0xC0) == 0x80 {
		cut--
	}
	return strings.TrimRight(body[:cut], " \n") + tail
}

// NotifyCIRetry posts the CI-failure auto-retry comment for an
// issue-triggered run (#279 / E16). Best-effort: returns errors so
// callers can log them, but a comment failure does NOT unwind the
// retry dispatch — the child run is already in the DB with its own
// audit entries, and the SPA's threaded-runs view (#216) renders
// the lineage without the comment.
//
// Skips silently when:
//   - The receiver is nil.
//   - The CHILD run isn't issue-triggered (CLI / PR / etc.) — we
//     read the child's TriggerSource here, not the parent's,
//     because the comment routes to the child's run page and the
//     contextFor helper validates the child.
//   - The child run is missing installation_id or a decodable issue
//     number.
//   - A ci_retry comment with the SAME retry_attempt already landed
//     on this run (per-attempt dedup; redeliveries of the same
//     check_run.completed are absorbed, but a fresh attempt N+1
//     still posts).
//
// `attempt` is the child run's RetryAttempt (1 for the first retry);
// `max` is the workflow's on_ci_failure.max_retries cap. Both render
// into the comment body so the labeler knows whether they have
// budget for another auto-retry if this one also fails.
func (n *Notifier) NotifyCIRetry(ctx context.Context, runID uuid.UUID, parentRunID uuid.UUID, checkName string, attempt, max int) error {
	if n == nil {
		return nil
	}
	if attempt <= 0 {
		// 0 = original run; never the child of a CI-retry path.
		// Negative is nonsense from a bad caller. Either way, skip.
		return nil
	}
	ctxv, ok, err := n.contextForCIRetry(ctx, runID, attempt)
	if err != nil || !ok {
		return err
	}
	body := renderCIRetryBody(ctxv, parentRunID, checkName, attempt, max, n.externalURL)
	return n.postCIRetry(ctx, ctxv, attempt, body)
}

// contextForCIRetry mirrors contextFor but uses the per-attempt
// dedup query — `alreadyPosted(KindCIRetry, …)` would falsely
// suppress attempt 2 after attempt 1 posted.
func (n *Notifier) contextForCIRetry(ctx context.Context, runID uuid.UUID, attempt int) (commentContext, bool, error) {
	runRow, err := n.runs.GetRun(ctx, runID)
	if err != nil {
		return commentContext{}, false, fmt.Errorf("issuecomment: get run: %w", err)
	}
	if runRow.TriggerSource != run.TriggerGitHubIssue {
		return commentContext{}, false, nil
	}
	if runRow.InstallationID == nil || runRow.TriggerRef == nil {
		return commentContext{}, false, nil
	}
	number, ok := parseIssueRef(*runRow.TriggerRef)
	if !ok {
		return commentContext{}, false, nil
	}
	repo, err := parseRepo(runRow.Repo)
	if err != nil {
		return commentContext{}, false, nil
	}
	already, err := n.alreadyPostedAttempt(ctx, runID, attempt)
	if err != nil {
		return commentContext{}, false, fmt.Errorf("issuecomment: dedup check: %w", err)
	}
	if already {
		return commentContext{}, false, nil
	}
	return commentContext{
		run:         runRow,
		repo:        repo,
		issueNumber: number,
		runURL:      n.externalURL + "/runs/" + runID.String(),
	}, true, nil
}

// alreadyPostedAttempt returns true when a ci_retry audit entry on
// this run already records the same retry_attempt. Different from
// alreadyPosted (which matches on kind alone) because a child run
// only ever sees one retry_attempt — but a future change that
// reuses one run row across multiple attempts would still need this
// per-attempt scoping.
func (n *Notifier) alreadyPostedAttempt(ctx context.Context, runID uuid.UUID, attempt int) (bool, error) {
	entries, err := n.audit.ListForRunByCategory(ctx, runID, CategoryIssueCommented)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if extractKind(e.Payload) != KindCIRetry {
			continue
		}
		if extractRetryAttempt(e.Payload) == attempt {
			return true, nil
		}
	}
	return false, nil
}

// postCIRetry fires the comment and writes the audit row. Mirrors
// post() but stamps retry_attempt into the payload so dedup can
// scope per-attempt.
func (n *Notifier) postCIRetry(ctx context.Context, ctxv commentContext, attempt int, body string) error {
	if _, err := n.github.CreateIssueComment(ctx, *ctxv.run.InstallationID, ctxv.repo, ctxv.issueNumber, body); err != nil {
		return fmt.Errorf("issuecomment: create comment: %w", err)
	}
	systemKind := audit.ActorSystem
	payload, _ := json.Marshal(map[string]any{
		"kind":          string(KindCIRetry),
		"issue_number":  ctxv.issueNumber,
		"repo":          ctxv.repo.String(),
		"retry_attempt": attempt,
	})
	if _, err := n.audit.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     ctxv.run.ID,
		Timestamp: n.now().UTC(),
		Category:  CategoryIssueCommented,
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		return fmt.Errorf("issuecomment: audit append: %w", err)
	}
	return nil
}

// BudgetAlertPayload is the rendered-comment input the trace handler
// hands NotifyBudgetAlert for one crossed (advisory budget, tier). The
// figures are echoed from budget.Decision; the caller has already
// evaluated the threshold and decided the comment should fire.
//
// PeriodStart is the RFC3339-formatted calendar period start (in the
// backend's budget timezone) — the same string the budget_alert audit
// payload carries — so the per-period/per-tier dedup keys identically on
// both surfaces. Tier is "warn" or "over".
type BudgetAlertPayload struct {
	WorkflowID  string
	Period      string
	PeriodStart string
	Spent       float64
	Limit       float64
	Fraction    float64
	WarnAt      *float64
	Tier        string
}

// NotifyBudgetAlert posts the advisory periodic-budget warning comment
// for an issue-triggered run (ADR-030 / #688). Best-effort: returns
// errors so callers can log them, but a comment failure does NOT unwind
// the cost recording — the budget_alert audit entry is the canonical
// signal and the SPA renders period spend without the comment.
//
// Skips silently when:
//   - The receiver is nil.
//   - The run isn't issue-triggered (CLI / PR / local runner with no
//     installation_id) — contextForBudgetAlert validates this.
//   - A budget_alert comment with the SAME (period_start, tier) already
//     landed on this run (per-period/per-tier dedup; a re-evaluation in
//     the same period or a redelivered upload is absorbed, but the warn
//     tier and the later 100% tier each post once).
func (n *Notifier) NotifyBudgetAlert(ctx context.Context, runID uuid.UUID, p BudgetAlertPayload) error {
	if n == nil {
		return nil
	}
	if p.Tier == "" {
		return nil
	}
	ctxv, ok, err := n.contextForBudgetAlert(ctx, runID, p.PeriodStart, p.Tier)
	if err != nil || !ok {
		return err
	}
	body := renderBudgetAlertBody(ctxv, p)
	return n.postBudgetAlert(ctx, ctxv, p, body)
}

// contextForBudgetAlert mirrors contextFor but uses the per-period/
// per-tier dedup query — alreadyPosted(KindBudgetAlert) would falsely
// suppress the 100% comment after the warn comment posted, and would
// suppress next period's warn after this period's.
func (n *Notifier) contextForBudgetAlert(ctx context.Context, runID uuid.UUID, periodStart, tier string) (commentContext, bool, error) {
	runRow, err := n.runs.GetRun(ctx, runID)
	if err != nil {
		return commentContext{}, false, fmt.Errorf("issuecomment: get run: %w", err)
	}
	if runRow.TriggerSource != run.TriggerGitHubIssue {
		return commentContext{}, false, nil
	}
	if runRow.InstallationID == nil || runRow.TriggerRef == nil {
		return commentContext{}, false, nil
	}
	number, ok := parseIssueRef(*runRow.TriggerRef)
	if !ok {
		return commentContext{}, false, nil
	}
	repo, err := parseRepo(runRow.Repo)
	if err != nil {
		return commentContext{}, false, nil
	}
	already, err := n.alreadyPostedBudgetTier(ctx, runID, periodStart, tier)
	if err != nil {
		return commentContext{}, false, fmt.Errorf("issuecomment: dedup check: %w", err)
	}
	if already {
		return commentContext{}, false, nil
	}
	return commentContext{
		run:         runRow,
		repo:        repo,
		issueNumber: number,
		runURL:      n.externalURL + "/runs/" + runID.String(),
	}, true, nil
}

// alreadyPostedBudgetTier returns true when a budget_alert comment on
// this run already records the same (period_start, tier).
func (n *Notifier) alreadyPostedBudgetTier(ctx context.Context, runID uuid.UUID, periodStart, tier string) (bool, error) {
	entries, err := n.audit.ListForRunByCategory(ctx, runID, CategoryIssueCommented)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if extractKind(e.Payload) != KindBudgetAlert {
			continue
		}
		ps, t := extractBudgetPeriodTier(e.Payload)
		if ps == periodStart && t == tier {
			return true, nil
		}
	}
	return false, nil
}

// postBudgetAlert fires the comment and writes the audit row, stamping
// period_start + budget_tier so the dedup can scope per-period/per-tier.
func (n *Notifier) postBudgetAlert(ctx context.Context, ctxv commentContext, p BudgetAlertPayload, body string) error {
	if _, err := n.github.CreateIssueComment(ctx, *ctxv.run.InstallationID, ctxv.repo, ctxv.issueNumber, body); err != nil {
		return fmt.Errorf("issuecomment: create comment: %w", err)
	}
	systemKind := audit.ActorSystem
	payload, _ := json.Marshal(map[string]any{
		"kind":         string(KindBudgetAlert),
		"issue_number": ctxv.issueNumber,
		"repo":         ctxv.repo.String(),
		"period_start": p.PeriodStart,
		"budget_tier":  p.Tier,
	})
	if _, err := n.audit.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     ctxv.run.ID,
		Timestamp: n.now().UTC(),
		Category:  CategoryIssueCommented,
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		return fmt.Errorf("issuecomment: audit append: %w", err)
	}
	return nil
}

// NotifyStatusUpdate creates or edits the run's sticky status
// comment per E20 / #326. The status comment is a single comment
// per run that reflects the run's current stage + state; rather
// than firing a new comment on every transition, the notifier
// finds the existing comment via audit-log lookup and edits it in
// place. Operators watching the issue thread see live state
// without leaving GitHub — the framing of ADR-019 / #320.
//
// Caller provides the rendered body (template is E20.3 / #329).
// Caller decides when to call (every meaningful transition,
// wired in E20.4 / #330).
//
// Best-effort throughout:
//   - Nil receiver / non-issue-trigger / missing run / empty body
//     return nil; the caller doesn't need to branch.
//   - GitHub create / edit failures are logged via the wrapped
//     error but don't unwind the underlying transition.
//   - Audit-append failures after a successful edit log but don't
//     fail the call — the next status update would re-record the
//     comment id from the GitHub response.
//   - 404 on update (operator manually deleted the comment) falls
//     back to creating a fresh one + appending a new audit row.
//
// The audit row's payload carries `kind: status_update`,
// `issue_number`, `repo`, and `github_comment_id`. Subsequent
// reads use the most-recent row's comment id.
func (n *Notifier) NotifyStatusUpdate(ctx context.Context, runID uuid.UUID, body string) error {
	if n == nil {
		return nil
	}
	if body == "" {
		// Caller decided there's nothing to render. Skip without
		// touching GitHub or the audit log.
		return nil
	}
	ctxv, ok, err := n.contextForStatus(ctx, runID)
	if err != nil || !ok {
		return err
	}

	// Look up the run's existing status comment id, if any.
	existingID, err := n.findStatusCommentID(ctx, runID)
	if err != nil {
		return fmt.Errorf("issuecomment: lookup status comment: %w", err)
	}

	if existingID > 0 {
		// Try to edit in place. If the comment was deleted, fall
		// through to create.
		got, updErr := n.github.UpdateIssueComment(ctx, *ctxv.run.InstallationID,
			ctxv.repo, existingID, body)
		switch {
		case updErr == nil:
			return n.appendStatusAudit(ctx, ctxv, got.ID)
		case errors.Is(updErr, githubclient.ErrNotFound):
			// Operator deleted the comment between updates. Fall
			// through to create a fresh one; the next call will
			// edit that one.
		default:
			return fmt.Errorf("issuecomment: update status comment: %w", updErr)
		}
	}

	created, err := n.github.CreateIssueComment(ctx, *ctxv.run.InstallationID,
		ctxv.repo, ctxv.issueNumber, body)
	if err != nil {
		return fmt.Errorf("issuecomment: create status comment: %w", err)
	}
	return n.appendStatusAudit(ctx, ctxv, created.ID)
}

// NotifyStatusUpdateForRun is the convenience entry point transition-
// point callers use (E20.4 / #330). It loads the run, its stages, and
// the audit chain for the run, renders the body via RenderStatusBody,
// and dispatches to NotifyStatusUpdate. Returns nil silently for
// non-issue triggers so callers at every transition point don't need
// to branch on TriggerSource.
//
// Best-effort: load failures return wrapped errors the caller can
// log; the post itself follows NotifyStatusUpdate's own best-effort
// posture (operator-deleted comment → fresh create, idempotent on
// redelivery, etc.).
func (n *Notifier) NotifyStatusUpdateForRun(ctx context.Context, runID uuid.UUID) error {
	if n == nil {
		return nil
	}
	runRow, err := n.runs.GetRun(ctx, runID)
	if err != nil {
		return fmt.Errorf("issuecomment: get run: %w", err)
	}
	if runRow.TriggerSource != run.TriggerGitHubIssue {
		return nil
	}
	stages, err := n.runs.ListStagesForRun(ctx, runID)
	if err != nil {
		return fmt.Errorf("issuecomment: list stages: %w", err)
	}
	entries, err := n.audit.ListForRun(ctx, runID)
	if err != nil {
		return fmt.Errorf("issuecomment: list audit: %w", err)
	}
	body := RenderStatusBody(runRow, stages, entries, n.externalURL, n.now())
	return n.NotifyStatusUpdate(ctx, runID, body)
}

// contextForStatus is the status-comment variant of contextFor — it
// resolves run + repo + issue without the per-kind dedup check that
// contextFor enforces. The status comment's "dedup" is "use the
// most-recent comment id from the audit log"; that lookup happens
// in findStatusCommentID instead.
func (n *Notifier) contextForStatus(ctx context.Context, runID uuid.UUID) (commentContext, bool, error) {
	runRow, err := n.runs.GetRun(ctx, runID)
	if err != nil {
		return commentContext{}, false, fmt.Errorf("issuecomment: get run: %w", err)
	}
	if runRow.TriggerSource != run.TriggerGitHubIssue {
		return commentContext{}, false, nil
	}
	// Local-runner runs (#416) carry no installation_id by design:
	// the backend can't mint an App token for the operator's repo,
	// and the operator's own `gh` does the posting from the CLI
	// side. The nil-installation_id branch below silently skips —
	// the GHA flow keeps working unchanged, and the CLI's
	// ghcomment package handles the local case directly.
	if runRow.InstallationID == nil || runRow.TriggerRef == nil {
		return commentContext{}, false, nil
	}
	number, ok := parseIssueRef(*runRow.TriggerRef)
	if !ok {
		return commentContext{}, false, nil
	}
	repo, err := parseRepo(runRow.Repo)
	if err != nil {
		return commentContext{}, false, nil
	}
	return commentContext{
		run:         runRow,
		repo:        repo,
		issueNumber: number,
		runURL:      n.externalURL + "/runs/" + runID.String(),
	}, true, nil
}

// findStatusCommentID returns the most-recent status comment id
// the audit log records for this run, or 0 when none exists.
// Errors propagate; corrupt payloads are treated as "no id" so
// the notifier falls back to creating a fresh comment.
func (n *Notifier) findStatusCommentID(ctx context.Context, runID uuid.UUID) (int64, error) {
	entries, err := n.audit.ListForRunByCategory(ctx, runID, CategoryStatusCommentPosted)
	if err != nil {
		return 0, err
	}
	// ListForRunByCategory returns ascending-by-sequence; the
	// most-recent row carries the canonical id. Walk from the end.
	for i := len(entries) - 1; i >= 0; i-- {
		if id := extractGithubCommentID(entries[i].Payload); id > 0 {
			return id, nil
		}
	}
	return 0, nil
}

// appendStatusAudit records that the run's status comment is at
// `commentID` as of now. Called from both the edit-in-place and
// fresh-create paths.
func (n *Notifier) appendStatusAudit(ctx context.Context, ctxv commentContext, commentID int64) error {
	systemKind := audit.ActorSystem
	payload, _ := json.Marshal(map[string]any{
		"kind":              string(KindStatusUpdate),
		"issue_number":      ctxv.issueNumber,
		"repo":              ctxv.repo.String(),
		"github_comment_id": commentID,
	})
	if _, err := n.audit.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     ctxv.run.ID,
		Timestamp: n.now().UTC(),
		Category:  CategoryStatusCommentPosted,
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		return fmt.Errorf("issuecomment: audit append: %w", err)
	}
	return nil
}

// extractGithubCommentID pulls the integer comment id out of a
// status_comment_posted audit payload. Returns 0 on parse failure
// or absent field — the caller treats 0 as "no prior id; create a
// new comment."
func extractGithubCommentID(payload []byte) int64 {
	if len(payload) == 0 {
		return 0
	}
	var p struct {
		GithubCommentID int64 `json:"github_comment_id"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return 0
	}
	return p.GithubCommentID
}

// renderCIRetryBody renders the CI-failure auto-retry comment.
// Names the failing check and the attempt budget so the labeler
// can predict whether a second failure will trigger another retry.
func renderCIRetryBody(c commentContext, parentRunID uuid.UUID, checkName string, attempt, max int, externalURL string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CI check `%s` failed on Run [`%s`](%s/runs/%s) — Fishhawk dispatched a retry as Run [`%s`](%s).\n\n",
		checkName,
		shortID(parentRunID), externalURL, parentRunID.String(),
		shortID(c.run.ID), c.runURL)
	fmt.Fprintf(&b, "Retry attempt %d of %d.\n", attempt, max)
	return b.String()
}

// extractRetryAttempt reads `retry_attempt` out of an
// issue_commented payload. Returns 0 on any decode failure or
// absent field; the caller treats 0 as "not the attempt we're
// checking" since a real ci_retry payload always carries >=1.
func extractRetryAttempt(payload []byte) int {
	if len(payload) == 0 {
		return 0
	}
	var p struct {
		RetryAttempt int `json:"retry_attempt"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return 0
	}
	return p.RetryAttempt
}

// extractBudgetPeriodTier reads `period_start` + `budget_tier` out of a
// budget_alert issue_commented payload for the per-period/per-tier dedup.
// Returns empty strings on any decode failure or absent fields.
func extractBudgetPeriodTier(payload []byte) (periodStart, tier string) {
	if len(payload) == 0 {
		return "", ""
	}
	var p struct {
		PeriodStart string `json:"period_start"`
		BudgetTier  string `json:"budget_tier"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return "", ""
	}
	return p.PeriodStart, p.BudgetTier
}

// renderBudgetAlertBody renders the advisory periodic-budget warning
// comment. Names the workflow, the calendar period, the spend-vs-limit
// figures, and whether the warn threshold or the full ceiling was
// crossed. Closes with the estimate caveat: period spend is summed from
// the per-run cost rollup, which undercounts invocations a backend
// reported no tokens for (known_usage=false, #685), so the real spend is
// at least the figure shown.
func renderBudgetAlertBody(c commentContext, p BudgetAlertPayload) string {
	var b strings.Builder
	headline := "approaching its"
	if p.Tier == "over" {
		headline = "has exhausted its"
	}
	fmt.Fprintf(&b, "Workflow `%s` %s %s cost budget on Run [`%s`](%s).\n\n",
		p.WorkflowID, headline, p.Period, shortID(c.run.ID), c.runURL)
	fmt.Fprintf(&b, "- **Period spend**: $%.2f of $%.2f (%.0f%%)\n",
		p.Spent, p.Limit, p.Fraction*100)
	if p.Tier == "warn" && p.WarnAt != nil {
		fmt.Fprintf(&b, "- **Warn threshold**: %.0f%%\n", *p.WarnAt*100)
	}
	b.WriteString("\n")
	b.WriteString("This is an advisory budget — runs are not blocked. ")
	b.WriteString("Period spend is an estimate summed from per-run cost rollups; ")
	b.WriteString("invocations whose backend reported no token usage are undercounted, ")
	b.WriteString("so actual spend is at least this figure.\n")
	return b.String()
}

// SlashApprovalReply is the params for NotifySlashApprovalReply
// (#238). Carries the issue coordinates explicitly because the
// reply path doesn't have a run UUID handy yet — the slash-command
// handler may post a reply before resolving (or while failing to
// resolve) the corresponding run.
type SlashApprovalReply struct {
	Repo           string
	InstallationID int64
	IssueNumber    int
	Body           string
}

// NotifySlashApprovalReply posts a reply comment to a /fishhawk
// approve or /fishhawk reject command (#238). Unlike
// NotifyPlanReady, replies are NOT deduped — every
// command attempt should produce its own reply, even if the
// reviewer fires the same command twice. The reply is fire-and-
// forget for the slash-command handler: a failure here logs but
// doesn't unwind the gate decision that's already recorded.
//
// Skips silently when:
//   - The receiver is nil.
//   - Repo is malformed (the slash-command handler should have
//     short-circuited before getting here, but defense in depth).
//   - InstallationID is zero (same).
func (n *Notifier) NotifySlashApprovalReply(ctx context.Context, p SlashApprovalReply) error {
	if n == nil {
		return nil
	}
	if p.IssueNumber <= 0 || p.InstallationID == 0 || p.Body == "" {
		return nil
	}
	repo, err := parseRepo(p.Repo)
	if err != nil {
		return nil
	}
	if _, err := n.github.CreateIssueComment(ctx, p.InstallationID, repo, p.IssueNumber, p.Body); err != nil {
		return fmt.Errorf("issuecomment: create reply: %w", err)
	}
	return nil
}

// NotifyRunRejected posts a comment on the triggering issue when the
// dispatcher refuses a GitHub-triggered run because the workflow's
// plan stage declares an agent-gated review (reviewers.agent > 0,
// human == 0) but the server has no plan reviewer wired (#577 / #599).
// Without this the refusal is silent to the customer: the webhook
// returns 202, no run appears, and the only trail is the operator-side
// global-chain run_rejected_misconfigured audit entry + a WARN log,
// neither of which the customer can see.
//
// Flat primitive params (repo, installationID, issueNumber,
// workflowID, stageID) rather than a struct or a run UUID: the guard
// fires before any run row is minted — there is no run UUID to pass —
// and the signature matches the NotifyCIRetry / NotifyStatusUpdateForRun
// convention so the webhook package's IssueNotifier interface can name
// the method without importing this package's concrete types.
//
// Skips silently when the receiver is nil, issueNumber <= 0,
// installationID == 0, or the repo is malformed (defense in depth;
// the dispatcher guard should only call this with valid coordinates).
//
// Dedup posture (deliberately none at this layer, mirroring
// NotifySlashApprovalReply): the per-run audit-log dedup the other
// surfaces use requires a run row that does not exist here, and the
// webhook receipt layer already dedups deliveries before
// Dispatcher.Handle is invoked (Handle's contract: "called once dedup
// has passed"), so same-delivery redeliveries cannot double-post.
// Distinct relabel / re-comment events are legitimately distinct
// refusals and should each get a reply. No notifier-level audit row is
// written — the canonical machine record stays the dispatcher's
// existing run_rejected_misconfigured global-chain entry.
//
// Best-effort: a post failure returns a wrapped error the dispatcher
// logs at WARN; it does not change the refusal outcome.
func (n *Notifier) NotifyRunRejected(ctx context.Context, repo string, installationID int64, issueNumber int, workflowID, stageID string) error {
	if n == nil {
		return nil
	}
	if issueNumber <= 0 || installationID == 0 {
		return nil
	}
	repoRef, err := parseRepo(repo)
	if err != nil {
		return nil
	}
	body := renderRunRejectedBody(workflowID, stageID)
	if _, err := n.github.CreateIssueComment(ctx, installationID, repoRef, issueNumber, body); err != nil {
		return fmt.Errorf("issuecomment: create run-rejected comment: %w", err)
	}
	return nil
}

// renderRunRejectedBody renders the fixed explanatory comment posted
// when the dispatcher refuses a run for a missing plan reviewer (#599).
// Names the offending workflow_id + stage and both fixes per the
// issue: (a) configure the server-side reviewer, or (b) change the
// stage's `reviewers`. Fixed short template — far under GitHub's
// MaxIssueCommentBodyBytes cap, so no truncation needed.
func renderRunRejectedBody(workflowID, stageID string) string {
	var b strings.Builder
	b.WriteString("**Fishhawk did not start a run.**\n\n")
	fmt.Fprintf(&b, "Workflow `%s`, stage `%s` declares an agent-gated plan review "+
		"(`reviewers.agent > 0`, `human == 0`), but this Fishhawk server has no plan "+
		"reviewer configured. An agent gate that can never be satisfied would leave the "+
		"run stuck forever, so the run was refused before it started.\n\n",
		workflowID, stageID)
	b.WriteString("Fix it one of two ways:\n\n")
	b.WriteString("- **Configure the server-side reviewer**: set `FISHHAWKD_ANTHROPIC_API_KEY` " +
		"(or otherwise enable the local plan reviewer) so the agent gate can be satisfied.\n")
	fmt.Fprintf(&b, "- **Change the stage's `reviewers`** for `%s`: add a human approver "+
		"(advisory mode, `human > 0`) so the human gate stays authoritative, or remove the "+
		"agent gate entirely.\n", stageID)
	return b.String()
}

// commentContext bundles the per-run inputs the post-helpers need:
// the run row (for installation_id), the parsed repo, the issue
// number, and the pre-rendered run URL. Built once per call.
type commentContext struct {
	run         *run.Run
	repo        githubclient.RepoRef
	issueNumber int
	runURL      string
}

// contextFor returns (ctx, true) when the run is eligible for a
// comment of `kind`, or (zero, false) when it should be skipped.
// The error return is non-nil only on transient I/O failures the
// caller should retry; logical "skip" cases return (zero, false,
// nil).
func (n *Notifier) contextFor(ctx context.Context, runID uuid.UUID, kind Kind) (commentContext, bool, error) {
	runRow, err := n.runs.GetRun(ctx, runID)
	if err != nil {
		return commentContext{}, false, fmt.Errorf("issuecomment: get run: %w", err)
	}
	if runRow.TriggerSource != run.TriggerGitHubIssue {
		return commentContext{}, false, nil
	}
	if runRow.InstallationID == nil {
		return commentContext{}, false, nil
	}
	if runRow.TriggerRef == nil {
		return commentContext{}, false, nil
	}
	number, ok := parseIssueRef(*runRow.TriggerRef)
	if !ok {
		return commentContext{}, false, nil
	}
	repo, err := parseRepo(runRow.Repo)
	if err != nil {
		return commentContext{}, false, nil
	}

	already, err := n.alreadyPosted(ctx, runID, kind)
	if err != nil {
		return commentContext{}, false, fmt.Errorf("issuecomment: dedup check: %w", err)
	}
	if already {
		return commentContext{}, false, nil
	}

	return commentContext{
		run:         runRow,
		repo:        repo,
		issueNumber: number,
		runURL:      n.externalURL + "/runs/" + runID.String(),
	}, true, nil
}

// alreadyPosted returns true when an issue_commented audit entry
// for this run already records a post of the given kind. Reads
// the run's audit log filtered to the category — the kind lives
// inside the JSON payload because two posts share one category.
func (n *Notifier) alreadyPosted(ctx context.Context, runID uuid.UUID, kind Kind) (bool, error) {
	entries, err := n.audit.ListForRunByCategory(ctx, runID, CategoryIssueCommented)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if extractKind(e.Payload) == kind {
			return true, nil
		}
	}
	return false, nil
}

// post fires the GitHub call and writes the audit entry. The order
// matters: comment first, audit entry second. If the comment fails
// we never write the audit entry, so a retry will try again. If the
// audit append fails after a successful comment, we log but treat
// the comment as posted — the next NotifyXxx call would re-post
// (rare; the audit log is highly available).
func (n *Notifier) post(ctx context.Context, ctxv commentContext, kind Kind, body string) error {
	if _, err := n.github.CreateIssueComment(ctx, *ctxv.run.InstallationID, ctxv.repo, ctxv.issueNumber, body); err != nil {
		return fmt.Errorf("issuecomment: create comment: %w", err)
	}
	systemKind := audit.ActorSystem
	payload, _ := json.Marshal(map[string]any{
		"kind":         string(kind),
		"issue_number": ctxv.issueNumber,
		"repo":         ctxv.repo.String(),
	})
	if _, err := n.audit.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     ctxv.run.ID,
		Timestamp: n.now().UTC(),
		Category:  CategoryIssueCommented,
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		return fmt.Errorf("issuecomment: audit append: %w", err)
	}
	return nil
}

func renderPlanBody(c commentContext, planStage *run.Stage, p *plan.Plan, externalURL string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Plan ready for Run [`%s`](%s):\n\n", shortID(c.run.ID), c.runURL)

	summary := truncate(p.Summary, 280)
	if summary != "" {
		fmt.Fprintf(&b, "**Summary**: %s\n\n", summary)
	}

	if files := renderFileList(p.Scope.Files, 10); files != "" {
		fmt.Fprintf(&b, "**Files in scope**:\n%s\n", files)
	}

	if planStage.RequiresApproval {
		// SPA route is /runs/:runId/stages/:stageId — there's no
		// top-level /stages/<id> route, so pre-#273 this URL 404'd
		// for every plan-ready comment that landed (#273).
		fmt.Fprintf(&b, "\n[Approve in the dashboard →](%s/runs/%s/stages/%s)\n",
			externalURL, c.run.ID.String(), planStage.ID.String())
	} else {
		fmt.Fprintf(&b, "\n[View run →](%s)\n", c.runURL)
	}
	return b.String()
}

// validApproverLogin returns true when `s` is a real GitHub login
// suitable for `@`-prefix display. Rejects the empty string and
// the literal "anonymous" placeholder the HTTP handler stamps when
// bearer auth didn't resolve an identity (so the SPA's "anonymous"
// fallback never leaks into the issue thread as @anonymous).
func validApproverLogin(s string) bool {
	return s != "" && s != "anonymous"
}

// renderFileList renders Plan.Scope.Files as a markdown bullet list,
// capped at `limit` rows with "and N more" when truncated. Returns
// "" when the list is empty.
func renderFileList(files []plan.ScopeFile, limit int) string {
	if len(files) == 0 {
		return ""
	}
	shown := limit
	if len(files) < shown {
		shown = len(files)
	}
	var b strings.Builder
	for _, f := range files[:shown] {
		fmt.Fprintf(&b, "- `%s` (%s)\n", f.Path, f.Operation)
	}
	if remaining := len(files) - shown; remaining > 0 {
		fmt.Fprintf(&b, "- _…and %d more_\n", remaining)
	}
	return b.String()
}

// truncate snips s at a rune boundary near `max` bytes and tacks on
// "...". Returns s unchanged when shorter. The 3-byte ASCII ellipsis
// is intentional over the 1-rune Unicode one — renders more
// consistently across the monospace terminals operators paste from.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		// Walk back off any UTF-8 continuation byte so we land on a
		// rune boundary.
		cut--
	}
	return strings.TrimRight(s[:cut], " ") + "..."
}

func shortID(id uuid.UUID) string {
	s := id.String()
	if len(s) >= 8 {
		return s[:8]
	}
	return s
}

// parseRepo splits "owner/name" into a RepoRef. Mirrors the
// server-package helper of the same name.
func parseRepo(s string) (githubclient.RepoRef, error) {
	parts := strings.SplitN(s, "/", 3)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return githubclient.RepoRef{}, fmt.Errorf("repo must be owner/name, got %q", s)
	}
	return githubclient.RepoRef{Owner: parts[0], Name: parts[1]}, nil
}

// parseIssueRef pulls the numeric issue number out of "issue:42"
// — the TriggerRef shape the dispatcher writes for issue triggers.
// Returns (0, false) on any other shape.
func parseIssueRef(ref string) (int, bool) {
	rest, ok := strings.CutPrefix(ref, "issue:")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// extractKind reads `kind` out of an issue_commented audit entry's
// payload. Returns "" on any decode failure or absent field.
func extractKind(payload []byte) Kind {
	if len(payload) == 0 {
		return ""
	}
	var p struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return ""
	}
	return Kind(p.Kind)
}
