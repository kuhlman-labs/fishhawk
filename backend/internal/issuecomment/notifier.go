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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/cost"
	"github.com/kuhlman-labs/fishhawk/backend/internal/drive"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/latency"
	"github.com/kuhlman-labs/fishhawk/backend/internal/operatorrole"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
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

// CategoryPRStatusCommentPosted records that Fishhawk created or edited the
// run's sticky PR status comment (E42.1 / #1784) — the PR-locus sibling of the
// living issue anchor. A SEPARATE category from CategoryStatusCommentPosted so
// the two comments never collide on comment-id lookup: the anchor lookup wants
// "the latest issue-comment id for this run", this one wants "the latest
// PR-comment id + body hash for this run". Each successful post/edit appends a
// fresh row carrying the canonical `github_comment_id` and a `body_hash` used
// for the identical-body edit skip.
const CategoryPRStatusCommentPosted = "pr_status_comment_posted"

// CategoryPRReviewPosted records that Fishhawk posted a terminal agent
// implement-review verdict as an advisory COMMENT-type GitHub PR review
// (E42.2 / #1785) — the PR merge-box locus of the verdict, distinct from the
// issue anchor and the audit chain. Dedup is per SOURCE implement_reviewed
// audit Sequence: each row's payload carries `source_sequence` (the dedup
// key) plus `reviewer_model`, `review_id`, and `event`, so a re-render of the
// run (which re-reads the whole chain) never double-posts a review for the
// same verdict, while a genuinely new post-fixup review round (a later
// Sequence) posts a NEW review.
const CategoryPRReviewPosted = "pr_review_posted"

// Sticky-comment marker loci (#1793). Each sticky comment carries a hidden
// HTML-comment marker keyed by locus + run id so the notifier can re-discover
// an orphaned comment (one whose GitHub id never made it into the audit chain
// because the post succeeded but the subsequent audit append failed) by LISTING
// the thread and matching the marker, rather than stacking a second comment.
//
//   - stickyLocusAnchor is shared by the issue-anchor renderers (RenderAnchorBody
//     and the CLI status-comment renderer RenderStatusBody, which edit the SAME
//     anchor comment in place — both must emit the identical marker or a CLI edit
//     would strip it from the live comment).
//   - stickyLocusPRStatus is the PR sticky status comment.
const (
	stickyLocusAnchor   = "anchor"
	stickyLocusPRStatus = "pr-status"
)

// stickyMarker returns the hidden HTML-comment marker embedded as the first
// section of a sticky comment body. Constant for a given (locus, runID) so it
// does not perturb body-hash dedup (hashPRStatusBody) across rebuilds. The
// marker is an inert HTML comment: invisible in rendered GitHub markdown, but
// greppable in the raw body for orphan re-discovery.
func stickyMarker(locus string, runID uuid.UUID) string {
	return fmt.Sprintf("<!-- fishhawk-sticky locus=%s run=%s -->", locus, runID)
}

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
	// KindPRStatusUpdate tags the sticky PR-status-comment audit row (E42.1 /
	// #1784). Lives on CategoryPRStatusCommentPosted, not
	// CategoryStatusCommentPosted. The payload carries github_comment_id (the
	// comment to edit next time) plus body_hash (the identical-body edit skip).
	KindPRStatusUpdate Kind = "pr_status_update"
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
//
// CreateReview posts an advisory COMMENT-type pull-request review of a
// terminal agent implement verdict (E42.2 / #1785), so the verdict lands in
// the PR merge box, not only in the issue anchor + audit chain.
type IssueCommenter interface {
	CreateIssueCommentScoped(ctx context.Context, scope forge.CredentialScope, repo githubclient.RepoRef, issueNumber int, body string) (*githubclient.IssueComment, error)
	UpdateIssueCommentScoped(ctx context.Context, scope forge.CredentialScope, repo githubclient.RepoRef, commentID int64, body string) (*githubclient.IssueComment, error)
	CreateReviewScoped(ctx context.Context, scope forge.CredentialScope, repo githubclient.RepoRef, prNumber int, params githubclient.CreateReviewParams) (*githubclient.CreateReviewResult, error)
	// ListIssueCommentsScoped lists an issue/PR comment thread so the sticky-comment
	// orphan-rediscovery fallback (#1793) can match a hidden marker to a comment
	// whose id was lost from the audit chain. Production *githubclient.Client
	// already implements it.
	ListIssueCommentsScoped(ctx context.Context, scope forge.CredentialScope, repo githubclient.RepoRef, number int) ([]githubclient.FetchedIssueComment, error)
}

// PlanArtifactLister is the narrow slice of artifact.Repository the
// anchor render needs: list a plan stage's artifacts so the living
// anchor (#1054) can project the current + superseded plan content
// (which lives in the artifact store, not the audit chain). Optional —
// when the Notifier is constructed without it, the anchor omits the plan
// sections and renders everything else.
type PlanArtifactLister interface {
	ListForStage(ctx context.Context, stageID uuid.UUID) ([]*artifact.Artifact, error)
}

// Notifier owns the comment-back I/O. Construct once with New and
// share — methods are safe for concurrent use (each post writes an
// independent audit entry, and the dedup check is read-then-write
// scoped to a single run).
//
// Notifier is the GitHub-comment Channel: the first (and, in v0, only)
// channel behind the Channel routing abstraction (ADR-015 #79 option B).
// Its method set IS the Channel surface, so it satisfies Channel
// unchanged (the compile-time assertion lives in channel.go) and the
// Router fans every Notify* call out to it. A future Slack adapter is a
// sibling Channel, not a change to this type.
type Notifier struct {
	github      IssueCommenter
	runs        run.Repository
	audit       audit.Repository
	artifacts   PlanArtifactLister
	externalURL string
	now         func() time.Time
}

// Deps groups the dependencies New needs.
type Deps struct {
	GitHub      IssueCommenter
	Runs        run.Repository
	Audit       audit.Repository
	ExternalURL string
	// Artifacts optionally loads plan artifacts so the living anchor
	// (#1054) can render the current + superseded plan content. When
	// nil the anchor omits the plan sections (graceful degradation).
	Artifacts PlanArtifactLister
	// Now is the clock used for audit timestamps; defaults to
	// time.Now. Overridable for deterministic tests.
	Now func() time.Time
}

// New returns a Notifier. Returns nil when the deps don't add up to
// a working notifier so callers can `notifier.NotifyPlanReady(...)`
// without nil-checking the receiver — the methods short-circuit on
// a nil receiver.
//
// The notifier constructs whenever GitHub / Runs / Audit are wired. An UNSET
// ExternalURL no longer suppresses comments (#1787): every renderer degrades a
// run reference to a plain, link-less short-id (and omits footer "view run"
// links) when the base URL is empty, so link-less comments still post — the
// nil-notifier dividing line is "no GitHub client", not "no base URL". This
// unblocks the dogfood posture, which leaves ExternalURL unset rather than
// pointing it at an operator-host-local address that would post dead links.
func New(d Deps) *Notifier {
	if d.GitHub == nil || d.Runs == nil || d.Audit == nil {
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
		artifacts:   d.Artifacts,
		externalURL: strings.TrimRight(d.ExternalURL, "/"),
		now:         now,
	}
}

// ArtifactListerWired reports whether the Notifier was constructed with
// a plan-artifact lister, i.e. whether the living anchor (#1054) will
// render its plan section in production. It exists only so
// constructor-wiring tests can assert the server.New → issuecomment.New
// seam carries cfg.ArtifactRepo through (the #1069 regression: the field
// is unexported, so neither the external notifier test nor the server
// test can read it directly). Nil-safe; not an issue-comment surface —
// it posts nothing.
func (n *Notifier) ArtifactListerWired() bool {
	return n != nil && n.artifacts != nil
}

// NotifyPlanReady fires the plan-ready hook after the plan stage
// transitions terminally. The living anchor (#1054) subsumes the old
// plan-on-issue full/summary comment surfaces: the plan is now projected
// into the run's single anchor comment (a collapsed <details> with the
// summary visible), so this entry point routes to the same anchor
// rebuild as every other transition. The exported signature is preserved
// so server/trace.go's call site is untouched — planStage/planArtifact
// are no longer needed (the anchor reloads the plan from the artifact
// store) but accepting them keeps the hook's contract stable. Skips on a
// nil receiver or nil plan stage (defensive; the trace handler only
// calls this once a plan artifact exists).
func (n *Notifier) NotifyPlanReady(ctx context.Context, runID uuid.UUID, planStage *run.Stage, planArtifact *plan.Plan) error {
	if n == nil || planStage == nil || planArtifact == nil {
		return nil
	}
	return n.NotifyStatusUpdateForRun(ctx, runID)
}

// MaxIssueCommentBodyBytes mirrors GitHub's per-comment body cap. Anchor
// output that exceeds this is collapsed by RenderAnchorBody's degradation
// ladder. Documented at https://docs.github.com/en/rest/issues/comments
// — the practical cap is 65,536 characters; we treat the limit as bytes
// since UTF-8 payloads can be longer than rune counts.
const MaxIssueCommentBodyBytes = 65_536

// decodeApprovalStatus decodes an `approval_submitted` audit payload
// into a planStatus: the decision, the provenance `approver` (the
// acting token subject), the resolved `approver_github_login` the
// MCP loop threads through (#751), and the `delegated` rule name the
// handler stamps on ADR-040 delegated approvals (#1026).
// ApproverGithubLogin is absent on SPA/CLI approvals, where `approver`
// is itself a GitHub login; Delegated is absent on human approvals.
// Returns nil when the payload is malformed so callers treat a corrupt
// row as "no status yet".
func decodeApprovalStatus(payload []byte) *planStatus {
	var p struct {
		Decision            string `json:"decision"`
		Approver            string `json:"approver"`
		ApproverGithubLogin string `json:"approver_github_login"`
		Delegated           string `json:"delegated"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil
	}
	return &planStatus{
		decision:    approval.Decision(p.Decision),
		approver:    p.Approver,
		githubLogin: p.ApproverGithubLogin,
		delegated:   p.Delegated,
	}
}

// PlanStatusFooterForAuditPayload renders the issue-thread plan-status
// footer from a raw `approval_submitted` audit payload — the same
// decode + render the live notifier applies when editing the
// plan-on-issue comment. Exported so a cross-package caller (the #751
// integration test in the server package) can assert the
// wire→handler→audit-payload→render seam end to end without
// reconstructing the full comment-post path. Returns "" when the
// payload doesn't decode or carries no terminal decision.
func PlanStatusFooterForAuditPayload(payload []byte) string {
	return renderPlanStatusFooter(decodeApprovalStatus(payload))
}

// planStatus carries the latest approval state for the plan stage,
// used by renderPlanStatusFooter / PlanStatusFooterForAuditPayload to
// render the `_Status:_` footer introduced in #377. Nil status renders
// as "awaiting approval"; approve / reject render named after the actor.
type planStatus struct {
	decision approval.Decision
	// approver is the provenance identity — the acting token subject
	// recorded server-side (e.g. brett@local-mcp for the MCP loop).
	approver string
	// githubLogin is the resolved GitHub login the MCP loop threads
	// through for `@`-mention rendering (#751). Preferred over approver
	// when it is a syntactically-valid login; empty on SPA/CLI
	// approvals, where approver itself is the GitHub login.
	githubLogin string
	// delegated names the ADR-040 delegation rule (e.g.
	// "clean_dual_approval") when the approval landed via the
	// delegated path (#1026); empty on every human approval.
	delegated string
}

// renderPlanStatusFooter returns the italic status line appended to
// the plan-on-issue comment, naming the actor that cleared or
// blocked the gate. Empty when no approval audit row exists yet
// (the comment first lands awaiting approval).
func renderPlanStatusFooter(s *planStatus) string {
	if s == nil {
		return ""
	}
	// Prefer the resolved GitHub login (#751) when present and valid —
	// a human MCP approval renders `@<login>` with no delegated clause.
	// Otherwise renderApproverIdentity picks the three-form identity
	// rendering for the acting subject (#1053).
	actor := renderApproverIdentity(s.approver, s.delegated, true)
	if validApproverLogin(s.githubLogin) {
		actor = renderApproverIdentity(s.githubLogin, "", true)
	}
	switch s.decision {
	case approval.DecisionApprove:
		return fmt.Sprintf("_Status: approved by %s · implementing now_", actor)
	case approval.DecisionReject:
		return fmt.Sprintf("_Status: rejected by %s_", actor)
	}
	return ""
}

// renderApproverIdentity picks the human-facing form of an approval
// audit row's subject (#1053). Three forms, in preference order:
//
//  1. A syntactically-valid GitHub login → `@<login>` in mention mode
//     (the #751 status-comment mention path), or a backtick-wrapped code
//     span (no @) in no-mention mode (the living anchor, which must never
//     @-mention an actor so a system/app actor can never ping an unrelated
//     GitHub user — #751/#755). Forms 2 and 3 below carry NO `@` in either
//     mode, so the coupled anchor render (#1788) stays @-free by delegating
//     its non-login cases straight to this helper.
//  2. An operator-agent token subject (operatorrole.IsTokenSubject,
//     ADR-040 / #1027) → "the operator agent (`<subject>`, delegated:
//     `<rule>`)", naming the delegation rule when the audit payload
//     recorded one (#1026); without a rule (or when the rule
//     sanitizes to empty) the parenthetical carries the subject
//     alone. The rule passes through the same sanitizer as the
//     subject and sits inside its own code span — it is read from
//     the audit payload, so it gets the identical no-markdown,
//     no-mention containment even though today's writer only ever
//     stamps a workflow-spec rule identifier.
//  3. Any other non-empty, non-"anonymous" subject → the subject
//     verbatim inside a markdown code span (sanitized; no `@` prefix,
//     so GitHub cannot ping a real user — the #751 stop-the-ping
//     guarantee holds).
//
// "an approver" is reserved strictly for the empty subject and the
// literal "anonymous" placeholder (matches the convention the retired
// renderPlanApprovedBody used so the issue thread never leaks
// `@anonymous`).
func renderApproverIdentity(subject, delegatedRule string, mention bool) string {
	if validApproverLogin(subject) {
		if mention {
			return "@" + subject
		}
		return "`" + subject + "`"
	}
	if operatorrole.IsTokenSubject(subject) {
		if rule := sanitizeSubjectForCodeSpan(delegatedRule); rule != "" {
			return fmt.Sprintf("the operator agent (`%s`, delegated: `%s`)", sanitizeSubjectForCodeSpan(subject), rule)
		}
		return fmt.Sprintf("the operator agent (`%s`)", sanitizeSubjectForCodeSpan(subject))
	}
	if subject == "" || subject == "anonymous" {
		return "an approver"
	}
	if s := sanitizeSubjectForCodeSpan(subject); s != "" {
		return "`" + s + "`"
	}
	return "an approver"
}

// maxRenderedSubjectRunes caps the verbatim-subject form so a
// pathological token subject can't balloon the issue comment.
const maxRenderedSubjectRunes = 64

// sanitizeSubjectForCodeSpan prepares a non-login subject (or the
// delegated rule name, which renderApproverIdentity contains the same
// way) for verbatim display inside a single-backtick markdown code
// span. Backticks are
// replaced (with "'") rather than stripped BEFORE wrapping, so no
// subject can close the span and re-enable markdown or an @-mention;
// control characters (including newlines, which also break a code
// span) are dropped; a leading "@" is stripped; length is capped at
// maxRenderedSubjectRunes. May return "" (e.g. a subject that is only
// control characters) — callers fall back to "an approver".
func sanitizeSubjectForCodeSpan(subject string) string {
	subject = strings.TrimPrefix(subject, "@")
	var b strings.Builder
	n := 0
	for _, r := range subject {
		if n >= maxRenderedSubjectRunes {
			break
		}
		switch {
		case r == '`':
			b.WriteByte('\'')
		case unicode.IsControl(r):
			continue
		default:
			b.WriteRune(r)
		}
		n++
	}
	return b.String()
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
	// When the base URL is unset (#1787), degrade the truncation marker to a
	// link-less form rather than emitting a dead relative /runs/<id>/stages path.
	var tail string
	if externalURL == "" {
		tail = "\n\n_…truncated._\n"
	} else {
		tail = fmt.Sprintf("\n\n_…truncated — [view full plan →](%s/runs/%s/stages/%s)_\n",
			externalURL, runID, stageID)
	}
	// Reserve room for the tail; trim conservatively.
	budget := MaxIssueCommentBodyBytes - len(tail)
	if budget < 0 {
		// Tail itself exceeds the cap (would be a very long URL).
		// Render only the bare run URL as a fallback so the
		// reviewer can navigate without leaving the comment; when the base URL
		// is unset there is no link to offer, so state the overflow plainly.
		if runURL == "" {
			return "_Plan exceeds GitHub's comment size._\n"
		}
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
		runURL:      runURLFor(n.externalURL, runID),
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
	if _, err := n.github.CreateIssueCommentScoped(ctx, forge.FromGitHubInstallationID(*ctxv.run.InstallationID), ctxv.repo, ctxv.issueNumber, body); err != nil {
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
// Returns posted=true ONLY when a comment was actually created on the
// issue; every silent-skip path returns posted=false (with a nil error)
// and a real failure returns (false, err). The caller uses the posted
// bit to decide whether to write the cross-run budget_alert_sent
// comment-dedup marker (#758): a suppressed emission writes no marker,
// so the next capable run still surfaces the comment for the period.
//
// Returns posted=false (and skips silently) when:
//   - The receiver is nil.
//   - The tier is empty.
//   - The run isn't issue-triggered (CLI / PR / local runner with no
//     installation_id), or its trigger ref / repo is unparseable —
//     contextForBudgetAlert validates this.
//   - A budget_alert comment with the SAME (period_start, tier) already
//     landed on this run (per-period/per-tier dedup; a re-evaluation in
//     the same period or a redelivered upload is absorbed, but the warn
//     tier and the later 100% tier each post once).
func (n *Notifier) NotifyBudgetAlert(ctx context.Context, runID uuid.UUID, p BudgetAlertPayload) (posted bool, err error) {
	if n == nil {
		return false, nil
	}
	if p.Tier == "" {
		return false, nil
	}
	ctxv, ok, err := n.contextForBudgetAlert(ctx, runID, p.PeriodStart, p.Tier)
	if err != nil || !ok {
		return false, err
	}
	body := renderBudgetAlertBody(ctxv, p, n.externalURL)
	if err := n.postBudgetAlert(ctx, ctxv, p, body); err != nil {
		return false, err
	}
	return true, nil
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
		runURL:      runURLFor(n.externalURL, runID),
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
	if _, err := n.github.CreateIssueCommentScoped(ctx, forge.FromGitHubInstallationID(*ctxv.run.InstallationID), ctxv.repo, ctxv.issueNumber, body); err != nil {
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
	if existingID == 0 {
		// The audit chain has no id. Either this is the genuine first post, or a
		// prior post landed on GitHub but its audit append failed and orphaned the
		// id (#1793). Re-discover by matching the hidden anchor marker on the
		// thread before creating a second comment; on no match / list error this
		// returns 0 and we fall through to create, preserving today's behavior.
		if recovered := n.rediscoverStickyComment(ctx, forge.FromGitHubInstallationID(*ctxv.run.InstallationID),
			ctxv.repo, ctxv.issueNumber, stickyMarker(stickyLocusAnchor, runID)); recovered > 0 {
			existingID = recovered
		}
	}

	if existingID > 0 {
		// Try to edit in place. If the comment was deleted, fall
		// through to create.
		got, updErr := n.github.UpdateIssueCommentScoped(ctx, forge.FromGitHubInstallationID(*ctxv.run.InstallationID),
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

	created, err := n.github.CreateIssueCommentScoped(ctx, forge.FromGitHubInstallationID(*ctxv.run.InstallationID),
		ctxv.repo, ctxv.issueNumber, body)
	if err != nil {
		return fmt.Errorf("issuecomment: create status comment: %w", err)
	}
	return n.appendStatusAudit(ctx, ctxv, created.ID)
}

// NotifyStatusUpdateForRun is the convenience entry point every
// transition-point caller uses (E20.4 / #330, anchor redrive #1054). It
// loads the run, its stages, the audit chain, and (when an artifact
// lister is wired) the plan content, rebuilds the living-anchor body via
// RenderAnchorBody, edits the anchor comment in place, and then fires any
// page-class pings the new audit state crossed. Returns nil silently for
// non-issue triggers so callers at every transition point don't need to
// branch on TriggerSource.
//
// Best-effort: load failures return wrapped errors the caller can log;
// the post itself follows NotifyStatusUpdate's own best-effort posture
// (operator-deleted comment → fresh create, idempotent on redelivery,
// etc.). A ping-post failure is returned but never unwinds the anchor
// edit that already landed.
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
	current, superseded := n.loadAnchorPlans(ctx, stages, entries)
	body := RenderAnchorBody(AnchorInput{
		Run:             runRow,
		Stages:          stages,
		Audit:           entries,
		CurrentPlan:     current,
		SupersededPlans: superseded,
		Economics:       BuildRunEconomics(runRow, entries),
		ExternalURL:     n.externalURL,
		Now:             n.now(),
	})
	if err := n.NotifyStatusUpdate(ctx, runID, body); err != nil {
		return err
	}
	// PR-locus sibling of the anchor (E42.1 / #1784): fold a best-effort PR
	// status comment into the same fan-out AFTER the anchor edit lands, so
	// every existing rebuild hook maintains it with no new call site. Fail-open
	// — the error is swallowed so a PR-comment failure never unwinds the
	// transition or blocks the page-class pings below.
	_ = n.maybeUpdatePRStatusComment(ctx, runRow, stages, entries)
	// Advisory COMMENT-type PR reviews for terminal agent implement verdicts
	// (E42.2 / #1785): fold into the same fan-out AFTER the anchor edit lands.
	// Fail-open — the error is swallowed so a review-post failure never unwinds
	// the transition or blocks the page-class pings below.
	_ = n.maybePostAgentReviewPRReviews(ctx, runRow, entries)
	// Page-class pings ride on the same audit chain we just projected;
	// fire them after the anchor edit lands. Resolve the comment context
	// fresh (NotifyStatusUpdate validated it but doesn't return it).
	ctxv, ok, err := n.contextForStatus(ctx, runID)
	if err != nil || !ok {
		return err
	}
	return n.firePings(ctx, ctxv, entries, stages, ctxv.runURL)
}

// NotifyPageClassForRun is the pings-only sibling of
// NotifyStatusUpdateForRun (#1786). It fires any page-class pings the
// current audit state crossed WITHOUT rebuilding the living anchor, the PR
// status comment, or the agent-review PR reviews. It is invoked at the four
// batched page-class append sites (plan-review reject, implement-review
// reject, scope-amendment request, paged acceptance triage) so a page posts
// within the event's own transaction window instead of riding the NEXT
// stage transition minutes later.
//
// Best-effort and idempotent: firePings dedups on the source audit
// Sequence, so invoking this in ADDITION to the per-transition
// NotifyStatusUpdateForRun cannot double-post. Returns nil silently for a
// nil receiver and for non-issue triggers (contextForStatus screens both),
// mirroring NotifyStatusUpdateForRun's posture so every call site is
// branch-free.
func (n *Notifier) NotifyPageClassForRun(ctx context.Context, runID uuid.UUID) error {
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
	ctxv, ok, err := n.contextForStatus(ctx, runID)
	if err != nil || !ok {
		return err
	}
	return n.firePings(ctx, ctxv, entries, stages, ctxv.runURL)
}

// loadAnchorPlans projects the run's plan artifacts into the anchor's
// current + superseded plan views. The latest plan artifact (by
// CreatedAt) across the run's plan stages is the current plan; any
// earlier ones are superseded, oldest-first, each annotated with the
// rejection reason that retired it (derived from the run's plan-gate
// reject decisions, chronologically aligned — see planRejectionReasons).
// Returns (nil, nil) when no artifact lister is wired (graceful
// degradation — the anchor omits the plan sections) or no plan artifact
// exists yet. Best-effort throughout: a load or decode failure for one
// stage is skipped, never fatal.
func (n *Notifier) loadAnchorPlans(ctx context.Context, stages []*run.Stage, entries []*audit.Entry) (*AnchorPlanView, []AnchorPlanView) {
	if n.artifacts == nil {
		return nil, nil
	}
	type dated struct {
		view AnchorPlanView
		at   time.Time
	}
	var views []dated
	for _, s := range stages {
		if s.Type != run.StageTypePlan {
			continue
		}
		arts, err := n.artifacts.ListForStage(ctx, s.ID)
		if err != nil {
			continue
		}
		for _, a := range arts {
			if a.Kind != artifact.KindPlan {
				continue
			}
			var p plan.Plan
			if json.Unmarshal(a.Content, &p) != nil {
				continue
			}
			av := AnchorPlanView{
				Summary:  p.Summary,
				Files:    p.Scope.Files,
				Approach: p.Approach,
			}
			// Surface the planner's implement-model recommendation (#1013)
			// so the anchor shows the suggestion the gate ratifies/overrides.
			if p.ModelRecommendation != nil {
				av.RecommendedModel = p.ModelRecommendation.ImplementModel
				av.RecommendationRationale = p.ModelRecommendation.Rationale
			}
			views = append(views, dated{view: av, at: a.CreatedAt})
		}
	}
	if len(views) == 0 {
		return nil, nil
	}
	// Oldest first so each superseded plan lines up with the rejection
	// (in chronological order) that retired it. The newest is current.
	sort.SliceStable(views, func(i, j int) bool { return views[i].at.Before(views[j].at) })
	current := views[len(views)-1].view
	reasons := planRejectionReasons(entries)
	superseded := make([]AnchorPlanView, 0, len(views)-1)
	for i := 0; i < len(views)-1; i++ {
		v := views[i].view
		if i < len(reasons) {
			v.RejectionReason = reasons[i]
		}
		superseded = append(superseded, v)
	}
	return &current, superseded
}

// BuildRunEconomics folds the run's audit chain into the economics rollups
// the living anchor renders (#1702): the cost + cache-efficiency aggregates
// over the cost_recorded ledger and the gate-latency rollup over the whole
// chain. It mirrors the derivation the /cost, /cache-efficiency, and /latency
// read surfaces perform (backend/internal/server), reusing the authoritative
// rolled cost total (runRow.CostUSDTotal) and the run-start bound
// (runRow.CreatedAt).
//
// Exported so the merge-time PR-body stamp (server.resolveReviewStageOnMerge)
// derives the SAME block the anchor shows from one fold, rather than
// re-implementing the mapping in the server package.
//
// Best-effort and pure over the loaded entries: an unparsable cost payload is
// skipped, and a chain with no economics signal yields an all-zero
// EconomicsInput that RenderEconomicsBlock renders as "" — the block is
// dropped from the anchor rather than shown empty. Always returns a non-nil
// input; emptiness is decided by the renderer.
func BuildRunEconomics(runRow *run.Run, entries []*audit.Entry) *EconomicsInput {
	costEntries := make([]cost.RunCostEntry, 0)
	cacheEntries := make([]cost.CacheEfficiencyEntry, 0)
	events := make([]latency.GateEvent, 0, len(entries))

	// runEnd is the newest terminal-marker timestamp (pr_merged /
	// post_merge_observed) when present, else the max timestamp in the chain —
	// "as far as the run has gotten" — mirroring server.runLatencySummary.
	runEnd := runRow.CreatedAt
	var terminal time.Time
	haveTerminal := false

	for _, e := range entries {
		if e.Timestamp.After(runEnd) {
			runEnd = e.Timestamp
		}
		if e.Category == "cost_recorded" {
			var p struct {
				Model            string  `json:"model"`
				USD              float64 `json:"usd"`
				Source           string  `json:"source"`
				InputTokens      int     `json:"input_tokens"`
				OutputTokens     int     `json:"output_tokens"`
				CacheReadTokens  int     `json:"cache_read_input_tokens"`
				CacheWriteTokens int     `json:"cache_write_input_tokens"`
			}
			if json.Unmarshal(e.Payload, &p) == nil {
				costEntries = append(costEntries, cost.RunCostEntry{Source: p.Source, USD: p.USD})
				cacheEntries = append(cacheEntries, cost.CacheEfficiencyEntry{
					Model:      p.Model,
					Source:     p.Source,
					FreshInput: p.InputTokens,
					CacheRead:  p.CacheReadTokens,
					CacheWrite: p.CacheWriteTokens,
					Output:     p.OutputTokens,
				})
			}
		}
		if cat, ok := latencyGateCategory(e.Category, e.Payload); ok {
			events = append(events, latency.GateEvent{Category: cat, Timestamp: e.Timestamp})
		}
		if e.Category == latency.CategoryPRMerged || e.Category == "post_merge_observed" {
			if !haveTerminal || e.Timestamp.After(terminal) {
				terminal = e.Timestamp
				haveTerminal = true
			}
		}
	}
	if haveTerminal {
		runEnd = terminal
	}

	costSummary := cost.AggregateRunCost(costEntries)
	// total_cost mirrors the /cost surface's authoritative figure — the run
	// record's rolled total, which the per-stage sum breaks down.
	costSummary.TotalUSD = runRow.CostUSDTotal

	return &EconomicsInput{
		Cost:    costSummary,
		Cache:   cost.AggregateCacheEfficiency(cacheEntries),
		Latency: latency.AggregateGateLatency(events, runRow.CreatedAt, runEnd),
	}
}

// latencyGateCategory maps an audit entry to the latency category the
// aggregator keys on, or reports ok=false to skip it. Most categories map to
// themselves; the synthetic `ci_green` boundary is derived from the
// `run_auto_advanced` entry whose payload rule is checks_green_awaiting_merge.
// It mirrors server.gateEventCategory — duplicated here because the server
// package imports issuecomment, so this package cannot import back the other
// way.
func latencyGateCategory(category string, payload json.RawMessage) (string, bool) {
	switch category {
	case latency.CategoryPlanGenerated,
		latency.CategoryApprovalSubmitted,
		latency.CategoryImplementReviewed,
		latency.CategoryAcceptanceDispatched,
		latency.CategoryPRMerged:
		return category, true
	case drive.Category: // run_auto_advanced
		var adv struct {
			Rule string `json:"rule"`
		}
		if err := json.Unmarshal(payload, &adv); err != nil {
			return "", false
		}
		if adv.Rule == string(drive.RuleChecksGreenAwaitingMerge) {
			return latency.CategoryCIGreen, true
		}
		return "", false
	default:
		return "", false
	}
}

// planRejectionReasons extracts the rejection comments from the run's
// plan-gate reject decisions (`approval_submitted` with
// decision=reject), ascending by audit sequence — so the Nth rejected
// plan version aligns with the Nth (oldest-first) superseded plan
// artifact. A reject with no recorded comment contributes an empty
// string (rendered without a "Rejected:" line). Used by loadAnchorPlans
// to annotate superseded plan views.
func planRejectionReasons(entries []*audit.Entry) []string {
	type seqReason struct {
		seq    int64
		reason string
	}
	var rs []seqReason
	for _, e := range entries {
		if e.Category != "approval_submitted" {
			continue
		}
		decision, reason := decodeRejectionComment(e.Payload)
		if decision != string(approval.DecisionReject) {
			continue
		}
		rs = append(rs, seqReason{seq: e.Sequence, reason: reason})
	}
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].seq < rs[j].seq })
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.reason)
	}
	return out
}

// decodeRejectionComment reads the decision + rejection_comment out of
// an approval_submitted payload (the reject path stamps the operator's
// reason there; see approvals.go). Returns ("", "") on any decode
// failure.
func decodeRejectionComment(payload []byte) (decision, comment string) {
	if len(payload) == 0 {
		return "", ""
	}
	var p struct {
		Decision         string `json:"decision"`
		RejectionComment string `json:"rejection_comment"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return "", ""
	}
	return p.Decision, p.RejectionComment
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
		runURL:      runURLFor(n.externalURL, runID),
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

// rediscoverStickyComment recovers the id of an orphaned sticky comment — one
// posted successfully but whose id never reached the audit chain because the
// subsequent audit append failed (#1793). It LISTS the issue/PR thread and
// returns the id of the first comment whose body contains marker, or 0 on no
// match. Fail-open: a list error (or an empty thread) returns 0, degrading to
// today's behavior (create a fresh comment) — the extra list call is off the
// hot path because it fires only when the audit lookup already returned 0.
func (n *Notifier) rediscoverStickyComment(ctx context.Context, scope forge.CredentialScope, repo githubclient.RepoRef, number int, marker string) int64 {
	comments, err := n.github.ListIssueCommentsScoped(ctx, scope, repo, number)
	if err != nil {
		// Fail-open: degrade to create, matching the surrounding best-effort
		// posture. The next successful audit append re-anchors the id.
		return 0
	}
	for _, c := range comments {
		if strings.Contains(c.Body, marker) {
			return c.ID
		}
	}
	return 0
}

// maybeUpdatePRStatusComment posts or edits the run's sticky PR status comment
// (E42.1 / #1784). Folded into NotifyStatusUpdateForRun so every rebuild hook
// maintains it. Best-effort / fail-open throughout: EVERY skip and EVERY
// GitHub / artifact-load / audit error returns nil so the PR comment never
// unwinds a transition. Maintained ONLY for runs that own the PR (non-empty
// pull_request_url + installation id), matching stampEconomicsIntoPRBody's
// owns-PR guard. An identical rendered body skips the GitHub edit (dedup via
// the body hash stored in the last pr_status_comment_posted row).
func (n *Notifier) maybeUpdatePRStatusComment(ctx context.Context, runRow *run.Run, stages []*run.Stage, entries []*audit.Entry) error {
	// Owns-PR guard — identical to stampEconomicsIntoPRBody.
	if runRow.InstallationID == nil || runRow.PullRequestURL == nil || *runRow.PullRequestURL == "" {
		return nil
	}
	prNumber := parsePRNumberFromURL(runRow.PullRequestURL)
	if prNumber <= 0 {
		return nil
	}
	repo, err := parseRepo(runRow.Repo)
	if err != nil {
		return nil
	}

	body := RenderPRStatusBody(PRStatusInput{
		Run:                runRow,
		Stages:             stages,
		Audit:              entries,
		AcceptanceArtifact: n.loadAcceptanceArtifact(ctx, stages, entries),
		ExternalURL:        n.externalURL,
		Now:                n.now(),
	})
	if body == "" {
		return nil
	}
	newHash := hashPRStatusBody(body)

	existingID, lastHash, err := n.findPRStatusComment(ctx, runRow.ID)
	if err != nil {
		// Fail-open: a bookkeeping read failure must not unwind the transition.
		return nil
	}
	if existingID == 0 {
		// The audit chain has no id — genuine first post, or a prior post landed
		// on GitHub but its audit append failed and orphaned the id (#1793).
		// Re-discover by matching the hidden pr-status marker on the PR thread
		// before creating a second comment; leave lastHash empty so the recovered
		// comment is edited in place (not dedup-skipped). No match / list error
		// returns 0 and we fall through to create, preserving today's behavior.
		if recovered := n.rediscoverStickyComment(ctx, forge.FromGitHubInstallationID(*runRow.InstallationID),
			repo, prNumber, stickyMarker(stickyLocusPRStatus, runRow.ID)); recovered > 0 {
			existingID = recovered
		}
	}
	if existingID > 0 && lastHash == newHash {
		// Identical body — skip the GitHub edit (mirrors the
		// stampEconomicsIntoPRBody no-op-on-redelivery posture) and avoid a
		// second GitHub read.
		return nil
	}

	var commentID int64
	switch {
	case existingID > 0:
		got, updErr := n.github.UpdateIssueCommentScoped(ctx, forge.FromGitHubInstallationID(*runRow.InstallationID), repo, existingID, body)
		switch {
		case updErr == nil:
			commentID = got.ID
		case errors.Is(updErr, githubclient.ErrNotFound):
			// Operator deleted the comment between updates — recreate it.
			created, cerr := n.github.CreateIssueCommentScoped(ctx, forge.FromGitHubInstallationID(*runRow.InstallationID), repo, prNumber, body)
			if cerr != nil {
				return nil
			}
			commentID = created.ID
		default:
			return nil
		}
	default:
		created, cerr := n.github.CreateIssueCommentScoped(ctx, forge.FromGitHubInstallationID(*runRow.InstallationID), repo, prNumber, body)
		if cerr != nil {
			return nil
		}
		commentID = created.ID
	}
	return n.appendPRStatusAudit(ctx, runRow, repo, prNumber, commentID, newHash)
}

// loadAcceptanceArtifact loads the KindAcceptance artifact body for the run's
// acceptance stage so the PR comment can render the per-criterion table (which
// the acceptance_outcome_recorded audit payload does NOT carry). Returns nil
// when no artifact lister is wired, no acceptance outcome has been recorded, or
// the matching artifact is unretrievable — the render then degrades to the
// tally line. Best-effort: any error is swallowed to nil.
func (n *Notifier) loadAcceptanceArtifact(ctx context.Context, stages []*run.Stage, entries []*audit.Entry) []byte {
	if n.artifacts == nil {
		return nil
	}
	var latest *audit.Entry
	for _, e := range entries {
		if e.Category == "acceptance_outcome_recorded" && (latest == nil || e.Sequence > latest.Sequence) {
			latest = e
		}
	}
	if latest == nil {
		return nil
	}
	stageIDStr, contentHash := decodeAcceptanceArtifactRef(latest.Payload)

	acceptanceStageID := uuid.Nil
	if stageIDStr != "" {
		if id, perr := uuid.Parse(stageIDStr); perr == nil {
			acceptanceStageID = id
		}
	}
	if acceptanceStageID == uuid.Nil {
		for _, s := range stages {
			if s.Type == run.StageTypeAcceptance {
				acceptanceStageID = s.ID
				break
			}
		}
	}
	if acceptanceStageID == uuid.Nil {
		return nil
	}
	arts, err := n.artifacts.ListForStage(ctx, acceptanceStageID)
	if err != nil {
		return nil
	}
	for _, a := range arts {
		if a.Kind != artifact.KindAcceptance {
			continue
		}
		// Prefer the artifact the recorded outcome referenced; a content-hash
		// mismatch means a newer record superseded this artifact.
		if contentHash != "" && a.ContentHash != contentHash {
			continue
		}
		return a.Content
	}
	return nil
}

// decodeAcceptanceArtifactRef reads stage_id + content_hash out of an
// acceptance_outcome_recorded payload (server/acceptance.go buildOutcomePayload).
// Empty strings on any decode failure.
func decodeAcceptanceArtifactRef(payload []byte) (stageID, contentHash string) {
	if len(payload) == 0 {
		return "", ""
	}
	var p struct {
		StageID     string `json:"stage_id"`
		ContentHash string `json:"content_hash"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return "", ""
	}
	return p.StageID, p.ContentHash
}

// findPRStatusComment returns the most-recent PR status comment id AND the body
// hash the audit log records for this run, or (0, "") when none exists. Errors
// propagate; corrupt payloads are treated as "no id" so the notifier falls back
// to creating a fresh comment.
func (n *Notifier) findPRStatusComment(ctx context.Context, runID uuid.UUID) (int64, string, error) {
	entries, err := n.audit.ListForRunByCategory(ctx, runID, CategoryPRStatusCommentPosted)
	if err != nil {
		return 0, "", err
	}
	for i := len(entries) - 1; i >= 0; i-- {
		if id, hash := extractPRStatusCommentID(entries[i].Payload); id > 0 {
			return id, hash, nil
		}
	}
	return 0, "", nil
}

// extractPRStatusCommentID pulls the comment id + body hash out of a
// pr_status_comment_posted audit payload. Returns (0, "") on parse failure or
// absent field — the caller treats 0 as "no prior id; create a new comment."
func extractPRStatusCommentID(payload []byte) (int64, string) {
	if len(payload) == 0 {
		return 0, ""
	}
	var p struct {
		GithubCommentID int64  `json:"github_comment_id"`
		BodyHash        string `json:"body_hash"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return 0, ""
	}
	return p.GithubCommentID, p.BodyHash
}

// appendPRStatusAudit records that the run's PR status comment is at commentID
// with the given body hash as of now. The body hash powers the identical-body
// edit skip on the next rebuild.
func (n *Notifier) appendPRStatusAudit(ctx context.Context, runRow *run.Run, repo githubclient.RepoRef, prNumber int, commentID int64, bodyHash string) error {
	systemKind := audit.ActorSystem
	payload, _ := json.Marshal(map[string]any{
		"kind":              string(KindPRStatusUpdate),
		"pr_number":         prNumber,
		"repo":              repo.String(),
		"github_comment_id": commentID,
		"body_hash":         bodyHash,
	})
	if _, err := n.audit.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runRow.ID,
		Timestamp: n.now().UTC(),
		Category:  CategoryPRStatusCommentPosted,
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		return fmt.Errorf("issuecomment: pr status audit append: %w", err)
	}
	return nil
}

// hashPRStatusBody returns a hex SHA-256 of the rendered PR-comment body, used
// to skip a redundant GitHub edit when the freshly rendered body is unchanged.
func hashPRStatusBody(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// maybePostAgentReviewPRReviews posts each terminal agent implement-review
// verdict as an advisory COMMENT-type GitHub PR review (E42.2 / #1785) so the
// verdict lands in the PR merge box, not only in the issue anchor + audit
// chain. Folded into NotifyStatusUpdateForRun alongside
// maybeUpdatePRStatusComment so every rebuild hook maintains it with no new
// call site.
//
// Best-effort / fail-open throughout: EVERY guard-skip and EVERY GitHub /
// audit error returns nil so a review-post failure never unwinds a transition
// or blocks the page-class pings. Maintained ONLY for runs that own the PR
// (non-empty pull_request_url + installation id), matching
// maybeUpdatePRStatusComment's owns-PR guard.
//
// Reviews are ALWAYS COMMENT-type (PRReviewEventComment): advisory reviewer
// verdicts must never hard-block merge under branch protection. Dedup is on
// the source implement_reviewed audit Sequence via a pr_review_posted row, so
// a round never double-posts while a post-fixup re-review (a later Sequence)
// posts a NEW review.
func (n *Notifier) maybePostAgentReviewPRReviews(ctx context.Context, runRow *run.Run, entries []*audit.Entry) error {
	// Owns-PR guard — identical to maybeUpdatePRStatusComment.
	if runRow.InstallationID == nil || runRow.PullRequestURL == nil || *runRow.PullRequestURL == "" {
		return nil
	}
	prNumber := parsePRNumberFromURL(runRow.PullRequestURL)
	if prNumber <= 0 {
		return nil
	}
	repo, err := parseRepo(runRow.Repo)
	if err != nil {
		return nil
	}

	posted, err := n.postedReviewSequences(ctx, runRow.ID)
	if err != nil {
		// Fail-open: a bookkeeping read failure must not unwind the transition.
		return nil
	}

	for _, e := range entries {
		if e.Category != "implement_reviewed" {
			continue
		}
		if _, done := posted[e.Sequence]; done {
			continue
		}
		body := RenderPRReviewBody(e, runRow, n.externalURL)
		if body == "" {
			// Undecodable / verdictless payload — nothing to post.
			continue
		}
		result, cerr := n.github.CreateReviewScoped(ctx, forge.FromGitHubInstallationID(*runRow.InstallationID), repo, prNumber, githubclient.CreateReviewParams{
			Body:  body,
			Event: PRReviewEventComment,
		})
		if cerr != nil {
			// Fail-open: log-and-continue. A CreateReview failure never unwinds
			// the notify path; no dedup row is written, so the next rebuild
			// retries this Sequence.
			continue
		}
		var reviewID int64
		if result != nil {
			reviewID = result.ID
		}
		_, model := decodeReviewerVerdict(e.Payload)
		if aerr := n.appendPRReviewAudit(ctx, runRow.ID, e.Sequence, model, reviewID); aerr != nil {
			// Fail-open: the review posted; a missing dedup row only risks a
			// re-post on the next rebuild, never a lost transition.
			continue
		}
	}
	return nil
}

// postedReviewSequences returns the set of source implement_reviewed audit
// sequences already posted as PR reviews for the run — the per-verdict dedup
// gate, mirroring pingedSequences.
func (n *Notifier) postedReviewSequences(ctx context.Context, runID uuid.UUID) (map[int64]struct{}, error) {
	entries, err := n.audit.ListForRunByCategory(ctx, runID, CategoryPRReviewPosted)
	if err != nil {
		return nil, err
	}
	seen := make(map[int64]struct{}, len(entries))
	for _, e := range entries {
		if seq := prReviewSourceSequence(e.Payload); seq > 0 {
			seen[seq] = struct{}{}
		}
	}
	return seen, nil
}

// appendPRReviewAudit records that the run's terminal implement verdict at
// sourceSequence was posted as PR review reviewID (COMMENT-type). The
// source_sequence powers the per-verdict dedup on the next rebuild.
func (n *Notifier) appendPRReviewAudit(ctx context.Context, runID uuid.UUID, sourceSequence int64, reviewerModel string, reviewID int64) error {
	systemKind := audit.ActorSystem
	payload, _ := json.Marshal(map[string]any{
		"source_sequence": sourceSequence,
		"reviewer_model":  reviewerModel,
		"review_id":       reviewID,
		"event":           PRReviewEventComment,
	})
	if _, err := n.audit.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		Timestamp: n.now().UTC(),
		Category:  CategoryPRReviewPosted,
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		return fmt.Errorf("issuecomment: pr review audit append: %w", err)
	}
	return nil
}

// prReviewSourceSequence pulls the source implement_reviewed audit Sequence out
// of a pr_review_posted payload. Returns 0 on parse failure or absent field.
func prReviewSourceSequence(payload []byte) int64 {
	if len(payload) == 0 {
		return 0
	}
	var p struct {
		SourceSequence int64 `json:"source_sequence"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return 0
	}
	return p.SourceSequence
}

// parsePRNumberFromURL extracts the integer PR number from a GitHub PR URL of
// the form https://github.com/{owner}/{repo}/pull/{n}. Returns 0 when the URL
// is nil/empty or carries no parseable trailing number, so callers treat it as
// "unknown" and skip the PR comment. Mirrors the server-package helper of the
// same name (server/lineage.go); duplicated because server imports this
// package, not the other way.
func parsePRNumberFromURL(url *string) int {
	if url == nil || *url == "" {
		return 0
	}
	idx := strings.LastIndex(*url, "/pull/")
	if idx < 0 {
		return 0
	}
	tail := (*url)[idx+len("/pull/"):]
	if cut := strings.IndexAny(tail, "/?#"); cut >= 0 {
		tail = tail[:cut]
	}
	n, err := strconv.Atoi(tail)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// renderCIRetryBody renders the CI-failure auto-retry comment.
// Names the failing check and the attempt budget so the labeler
// can predict whether a second failure will trigger another retry.
func renderCIRetryBody(c commentContext, parentRunID uuid.UUID, checkName string, attempt, max int, externalURL string) string {
	var b strings.Builder
	// Both run references degrade to plain backticked short-ids when the base
	// URL is unset (#1787) via runShortLink.
	fmt.Fprintf(&b, "CI check `%s` failed on Run %s — Fishhawk dispatched a retry as Run %s.\n\n",
		checkName,
		runShortLink(externalURL, parentRunID),
		runShortLink(externalURL, c.run.ID))
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
func renderBudgetAlertBody(c commentContext, p BudgetAlertPayload, externalURL string) string {
	var b strings.Builder
	headline := "approaching its"
	if p.Tier == "over" {
		headline = "has exhausted its"
	}
	// The run reference degrades to a plain backticked short-id when the base
	// URL is unset (#1787) via runShortLink.
	fmt.Fprintf(&b, "Workflow `%s` %s %s cost budget on Run %s.\n\n",
		p.WorkflowID, headline, p.Period, runShortLink(externalURL, c.run.ID))
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
	if _, err := n.github.CreateIssueCommentScoped(ctx, forge.FromGitHubInstallationID(p.InstallationID), repo, p.IssueNumber, p.Body); err != nil {
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
// Flat primitive params (repo, scope, issueNumber,
// workflowID, stageID) rather than a struct or a run UUID: the guard
// fires before any run row is minted — there is no run UUID to pass —
// and the signature matches the NotifyCIRetry / NotifyStatusUpdateForRun
// convention so the webhook package's IssueNotifier interface can name
// the method without importing this package's concrete types.
//
// Skips silently when the receiver is nil, issueNumber <= 0,
// scope is zero, or the repo is malformed (defense in depth;
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
func (n *Notifier) NotifyRunRejected(ctx context.Context, repo string, scope forge.CredentialScope, issueNumber int, workflowID, stageID string) error {
	if n == nil {
		return nil
	}
	if issueNumber <= 0 || scope.IsZero() {
		return nil
	}
	repoRef, err := parseRepo(repo)
	if err != nil {
		return nil
	}
	body := renderRunRejectedBody(workflowID, stageID)
	if _, err := n.github.CreateIssueCommentScoped(ctx, scope, repoRef, issueNumber, body); err != nil {
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
		runURL:      runURLFor(n.externalURL, runID),
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
	if _, err := n.github.CreateIssueCommentScoped(ctx, forge.FromGitHubInstallationID(*ctxv.run.InstallationID), ctxv.repo, ctxv.issueNumber, body); err != nil {
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

// githubLoginPattern matches a syntactically-valid GitHub login:
// alphanumeric with single internal hyphens, no leading/trailing
// hyphen, and at most 39 characters. Crucially it rejects any string
// containing '@' or '.', so a non-login token subject like
// brett@local-mcp can never be rendered as an `@`-mention (#751).
// Compiled once at package init. Reference: GitHub username rules
// (max 39 chars, alphanumeric + single hyphens).
var githubLoginPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)

// validApproverLogin returns true when `s` is a syntactically-valid
// GitHub login suitable for `@`-prefix display. Rejects the empty
// string and the literal "anonymous" placeholder the HTTP handler
// stamps when bearer auth didn't resolve an identity (so the SPA's
// "anonymous" fallback never leaks into the issue thread as
// @anonymous), and — as the stop-the-ping guarantee (#751) —
// anything that isn't a real login, such as the MCP loop's token
// subject brett@local-mcp. This defensive filter works independently
// of the gh-login resolution: even if no resolved login is threaded
// through, a non-login subject is never `@`-mentioned —
// renderApproverIdentity renders it as the operator-agent form or a
// verbatim code span instead (#1053), so it cannot ping an unrelated
// GitHub user.
func validApproverLogin(s string) bool {
	if s == "" || s == "anonymous" {
		return false
	}
	return githubLoginPattern.MatchString(s)
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
