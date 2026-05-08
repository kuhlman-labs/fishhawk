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
// chained audit entry with `payload.kind ∈ {pickup, plan}`. Before
// each post we read back the run's audit log and skip if a matching
// row already exists. Audit-log dedup matches the integrity story —
// "we said we did it" lives next to "we did it" — and survives
// process restarts.
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
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// CategoryIssueCommented is the audit-log category the notifier writes
// after a successful post. Static so the dedup query
// (`ListForRunByCategory`) can index on it.
const CategoryIssueCommented = "issue_commented"

// Kind enumerates which moment a comment recorded. Stored in the
// audit entry's payload so a single category covers both moments
// while staying queryable per kind.
type Kind string

// Kind values.
const (
	KindPickup Kind = "pickup"
	KindPlan   Kind = "plan"
)

// IssueCommenter is the slice of githubclient.Client this package
// needs. Defining it as an interface keeps the unit tests free of a
// fake api.github.com and lets the dispatcher's existing GitHubAPI
// shape stay focused.
type IssueCommenter interface {
	CreateIssueComment(ctx context.Context, installationID int64, repo githubclient.RepoRef, issueNumber int, body string) error
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
// a working notifier so callers can `notifier.NotifyPickup(...)`
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

// NotifyPickup posts the pickup-acknowledgment comment for an issue-
// triggered run. Best-effort: returns errors so callers can log
// them, but a comment failure should NOT unwind the dispatch — the
// run is in the DB and the user can navigate without the comment.
//
// Skips silently when:
//   - The receiver is nil.
//   - The run isn't issue-triggered (CLI / PR / etc.).
//   - The run is missing installation_id, parseable repo, or a
//     decodable issue number.
//   - A pickup comment already landed for this run (audit-log
//     dedup).
//
// `senderLogin` is the GitHub login of the user who fired the
// trigger (labeled the issue, etc.). Empty is fine — the comment
// just won't include the "Triggered by @x" line.
func (n *Notifier) NotifyPickup(ctx context.Context, runID uuid.UUID, senderLogin string) error {
	if n == nil {
		return nil
	}
	ctxv, ok, err := n.contextFor(ctx, runID, KindPickup)
	if err != nil || !ok {
		return err
	}
	body := renderPickupBody(ctxv, senderLogin)
	return n.post(ctx, ctxv, KindPickup, body)
}

// NotifyPlanReady posts the plan-ready comment after the plan stage
// transitions terminally. `planStage` is the plan stage row;
// `planArtifact` is the typed `*plan.Plan` from its standard_v1
// artifact. Both are required — if either is nil the call skips.
//
// The comment routes to the approval-surface URL when the plan
// stage requires approval (v0's typical workflow); to the run page
// otherwise (`routine_change`-style flows).
func (n *Notifier) NotifyPlanReady(ctx context.Context, runID uuid.UUID, planStage *run.Stage, planArtifact *plan.Plan) error {
	if n == nil || planStage == nil || planArtifact == nil {
		return nil
	}
	ctxv, ok, err := n.contextFor(ctx, runID, KindPlan)
	if err != nil || !ok {
		return err
	}
	body := renderPlanBody(ctxv, planStage, planArtifact, n.externalURL)
	return n.post(ctx, ctxv, KindPlan, body)
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
// NotifyPickup / NotifyPlanReady, replies are NOT deduped — every
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
	if err := n.github.CreateIssueComment(ctx, p.InstallationID, repo, p.IssueNumber, p.Body); err != nil {
		return fmt.Errorf("issuecomment: create reply: %w", err)
	}
	return nil
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
	if err := n.github.CreateIssueComment(ctx, *ctxv.run.InstallationID, ctxv.repo, ctxv.issueNumber, body); err != nil {
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

func renderPickupBody(c commentContext, senderLogin string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Fishhawk picked this up — Run [`%s`](%s) started.\n\n",
		shortID(c.run.ID), c.runURL)
	fmt.Fprintf(&b, "Workflow: `%s`", c.run.WorkflowID)
	if senderLogin != "" {
		fmt.Fprintf(&b, " · Triggered by `@%s`", senderLogin)
	}
	b.WriteString("\n")
	return b.String()
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
		fmt.Fprintf(&b, "\n[Approve in the dashboard →](%s/stages/%s)\n",
			externalURL, planStage.ID.String())
	} else {
		fmt.Fprintf(&b, "\n[View run →](%s)\n", c.runURL)
	}
	return b.String()
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
