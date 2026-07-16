// Package reactionpoller runs the background ticker that polls
// reactions on Fishhawk-authored plan comments and forwards
// approval-shaped reactions (👍 / ❤️ / 🎉 / 🚀) into the same
// approval-ingestion path the reply-comment matcher uses
// (E17.4 / #339).
//
// Why polling: GitHub doesn't deliver webhook events for reactions
// on issue comments (verified at
// https://docs.github.com/en/webhooks/webhook-events-and-payloads —
// the documented `issue_comment` event covers the comment lifecycle,
// not reactions on it). E17 / ADR-020 pivoted to typed reply
// patterns (`+1` / `lgtm`) as the primary lightweight approval
// surface; this worker is the catch-net for the operator who clicks
// the 👍 reaction without typing.
//
// Adaptive cadence keeps the rate-limit cost bounded:
//
//   - Fast tier (~30s) for plan comments < 10 minutes old —
//     reactions usually land within minutes of plan posting.
//   - Slow tier (~5 min) for plan comments ≥ 10 minutes old that
//     are still awaiting approval — covers the long-tail case
//     where a reviewer comes back hours later.
//
// At 100 simultaneous awaiting plans on the slow tier, the worker
// makes ~1,200 API calls per hour — well under the 5,000 / hour
// per-installation budget.
//
// The worker is OFF BY DEFAULT in fishhawkd (per --enable-reaction-
// poller) so a v0 deployment that doesn't need it doesn't pay the
// poll cost.
package reactionpoller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/issuecomment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
)

// CategoryPlanReactionObserved is the audit-log category for the
// chained entry the worker writes when it surfaces a new reaction
// on a plan comment. Stable so log scrapers and compliance exports
// can index on it. One row per (run, reaction id) pair — the
// per-reaction-id dedup happens against this category.
const CategoryPlanReactionObserved = "plan_reaction_observed"

// approvalReactions is the closed set of reaction kinds the worker
// forwards to the approval handler as MatchActionApprove. Mirrors
// the conventions developers already use on GitHub PRs and issues:
// 👍 / ❤️ / 🎉 / 🚀 all read as "I approve this." 👀 / 😄 / 😕 /
// 👎 are explicitly NOT approvals — 👎 in particular is a "block"
// signal but v0 doesn't have a reject-by-reaction surface (the
// typed `/fishhawk reject <reason>` slash path is the rationale-
// bearing reject surface).
var approvalReactions = map[githubclient.IssueCommentReactKind]struct{}{
	githubclient.ReactPlusOne: {},
	githubclient.ReactHeart:   {},
	githubclient.ReactHooray:  {},
	githubclient.ReactRocket:  {},
}

// Defaults for the cadence knobs. The Ticker fields are
// configurable; these are the values fishhawkd applies when the
// caller leaves them zero.
const (
	DefaultFastInterval  = 30 * time.Second
	DefaultSlowInterval  = 5 * time.Minute
	DefaultAgeThreshold  = 10 * time.Minute
	DefaultPollerTimeout = 60 * time.Second
)

// ReactionLister is the slice of githubclient.Client the worker
// uses. Tests inject a stub that returns canned reaction lists.
type ReactionLister interface {
	ListIssueCommentReactionsScoped(ctx context.Context, scope forge.CredentialScope, repo githubclient.RepoRef, commentID int64) ([]githubclient.IssueCommentReaction, error)
}

// Ticker scans the runs that have a plan comment awaiting approval
// and polls each for new reactions. Forwards approval-shaped
// reactions to the approval handler; appends a per-reaction audit
// row to the dedup chain.
//
// Mirrors the dispatchwatchdog / sla.Ticker pattern: Run() blocks
// until ctx is cancelled; for production wiring start it on its own
// goroutine via the server config.
type Ticker struct {
	// Runs lists stages and reads the run row (for trigger
	// metadata + required-checks snapshot). Required.
	Runs run.Repository

	// Audit reads prior `issue_commented` rows (to find the plan
	// comment id) and appends `plan_reaction_observed` rows.
	// Required.
	Audit audit.Repository

	// Reactions is the GitHub-side surface. Required.
	Reactions ReactionLister

	// Approvals forwards approval-shaped reactions into the same
	// pipeline the reply-comment matcher uses. The handler is
	// expected to skip silently when no awaiting plan stage
	// matches (`ApprovalSourceReactionEmoji` follows the
	// `ApprovalSourceReplyComment` posture). Required.
	Approvals webhook.ApprovalCommandHandler

	// Logger receives structured warnings about transient errors
	// + per-tick observability for rate-limit accounting. nil →
	// slog.Default().
	Logger *slog.Logger

	// FastInterval is the cadence applied when the plan comment is
	// younger than AgeThreshold. Defaults to DefaultFastInterval
	// when zero.
	FastInterval time.Duration

	// SlowInterval is the cadence applied when the plan comment is
	// at least AgeThreshold old. Defaults to DefaultSlowInterval
	// when zero.
	SlowInterval time.Duration

	// AgeThreshold is the boundary between fast- and slow-tier
	// cadence. Defaults to DefaultAgeThreshold when zero.
	AgeThreshold time.Duration

	// Now is the clock used for cadence accounting. Tests inject
	// a fake clock; production leaves it nil for time.Now.
	Now func() time.Time

	// lastPolledAt records the time of the last successful poll
	// per stage id so the cadence check can skip stages within
	// the current tier's interval. In-memory; lost on restart
	// (acceptable — we just re-poll everything once).
	mu           sync.Mutex
	lastPolledAt map[uuid.UUID]time.Time
}

// Run drives the ticker until ctx is cancelled. Each tick fires
// at FastInterval; per-stage cadence is gated internally so slow-
// tier stages skip until SlowInterval has elapsed since their last
// poll. Per-stage errors log but don't abort the loop.
func (t *Ticker) Run(ctx context.Context) error {
	if t.Runs == nil {
		return errors.New("reactionpoller: ticker requires Runs")
	}
	if t.Audit == nil {
		return errors.New("reactionpoller: ticker requires Audit")
	}
	if t.Reactions == nil {
		return errors.New("reactionpoller: ticker requires Reactions")
	}
	if t.Approvals == nil {
		return errors.New("reactionpoller: ticker requires Approvals")
	}

	interval := t.FastInterval
	if interval <= 0 {
		interval = DefaultFastInterval
	}

	t.Tick(ctx)

	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			t.Tick(ctx)
		}
	}
}

// Tick performs one pass over the awaiting plan stages. Exposed
// for tests so a fake clock can step the loop without spinning
// real timers.
func (t *Ticker) Tick(ctx context.Context) {
	logger := t.Logger
	if logger == nil {
		logger = slog.Default()
	}

	now := time.Now().UTC()
	if t.Now != nil {
		now = t.Now().UTC()
	}

	stages, err := t.Runs.ListStagesAwaitingApproval(ctx)
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "reactionpoller: list awaiting stages failed",
			slog.String("error", err.Error()))
		return
	}

	polled := 0
	skipped := 0
	for _, s := range stages {
		if s.Type != run.StageTypePlan {
			continue
		}
		if !t.pollStage(ctx, logger, now, s) {
			skipped++
			continue
		}
		polled++
	}

	logger.LogAttrs(ctx, slog.LevelDebug, "reactionpoller: tick complete",
		slog.Int("polled", polled),
		slog.Int("skipped", skipped),
		slog.Int("awaiting_plan_stages", polled+skipped),
	)
}

// pollStage walks one awaiting plan stage. Returns true when the
// poller actually issued a reactions request (counts toward the
// per-tick observability metric); false when the cadence gate
// skipped it or the stage had no plan comment to poll.
func (t *Ticker) pollStage(ctx context.Context, logger *slog.Logger, now time.Time, s *run.Stage) bool {
	commentID, planExistsAt, ok := t.planApprovalMeta(ctx, s.RunID)
	if !ok {
		return false
	}

	// Cadence ages off plan EXISTENCE, not the anchor comment's
	// posted-at (#1054): the anchor is created at run dispatch, before
	// any plan, so the comment's own age would force every run onto the
	// slow tier immediately. Plan-existence age preserves the
	// "reactions land within minutes of plan posting → poll fast" intent.
	cadence := t.cadenceFor(now, planExistsAt)
	if !t.shouldPoll(s.ID, now, cadence) {
		return false
	}

	runRow, err := t.Runs.GetRun(ctx, s.RunID)
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "reactionpoller: get run failed",
			slog.String("run_id", s.RunID.String()),
			slog.String("error", err.Error()))
		return false
	}
	if runRow.InstallationID == nil {
		// No installation_id → no GitHub creds to poll with.
		// Same skip-clean posture as the issuecomment notifier.
		return false
	}
	repo, err := splitRepoFullName(runRow.Repo)
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "reactionpoller: malformed repo",
			slog.String("run_id", s.RunID.String()),
			slog.String("repo", runRow.Repo),
			slog.String("error", err.Error()))
		return false
	}
	issueNumber, err := parseIssueNumber(runRow.TriggerRef)
	if err != nil {
		// Not issue-triggered (CLI / PR / etc.) — no reactions
		// to poll. issuecomment's contextFor skips these too.
		return false
	}

	reactions, err := t.Reactions.ListIssueCommentReactionsScoped(ctx, forge.FromGitHubInstallationID(*runRow.InstallationID), repo, commentID)
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "reactionpoller: list reactions failed",
			slog.String("run_id", s.RunID.String()),
			slog.Int64("comment_id", commentID),
			slog.String("error", err.Error()))
		return false
	}
	t.recordLastPoll(s.ID, now)

	seen, err := t.observedReactionIDs(ctx, s.RunID)
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "reactionpoller: load observed reactions failed",
			slog.String("run_id", s.RunID.String()),
			slog.String("error", err.Error()))
		return true // still counts toward "polled" — we did make the API call
	}

	for _, r := range reactions {
		if _, already := seen[r.ID]; already {
			continue
		}
		t.handleNewReaction(ctx, logger, runRow, s, commentID, issueNumber, planExistsAt, r)
	}

	return true
}

// handleNewReaction appends the audit row + forwards to the
// approval handler when the reaction is approval-shaped. Errors
// log but don't unwind — the audit chain is the canonical record;
// a re-deliver of the same poll absorbs gracefully via dedup.
func (t *Ticker) handleNewReaction(
	ctx context.Context,
	logger *slog.Logger,
	runRow *run.Run,
	stage *run.Stage,
	commentID int64,
	issueNumber int,
	planExistsAt time.Time,
	r githubclient.IssueCommentReaction,
) {
	stageID := stage.ID
	systemKind := audit.ActorSystem
	payload, _ := json.Marshal(map[string]any{
		"reaction_id": r.ID,
		"content":     string(r.Content),
		"user_login":  r.User.Login,
		"comment_id":  commentID,
	})
	if _, err := t.Audit.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runRow.ID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  CategoryPlanReactionObserved,
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "reactionpoller: audit append failed",
			slog.String("run_id", runRow.ID.String()),
			slog.Int64("reaction_id", r.ID),
			slog.String("error", err.Error()))
		// On audit failure, bail before forwarding — without
		// the dedup row a retry would re-forward the same
		// reaction. Better to surface the gap than double-fire.
		return
	}

	if _, isApproval := approvalReactions[r.Content]; !isApproval {
		return
	}

	// Plan-existence cutoff (#1054, binding condition 2): the anchor
	// comment exists from run creation, so a reaction placed BEFORE the
	// plan was generated cannot be a plan approval — admitting it would
	// let a pre-plan thumbs-up clear the gate the moment the plan lands.
	// Drop any approval-shaped reaction whose placement time precedes
	// plan existence. A zero CreatedAt (GitHub omitted the field, or a
	// legacy fixture) is admitted — absence of a timestamp is not
	// evidence the reaction predates the plan, and dropping a real
	// approval on a parse quirk is the worse failure. The dedup audit
	// row was already written above, so a dropped reaction is not
	// re-evaluated.
	if !r.CreatedAt.IsZero() && r.CreatedAt.Before(planExistsAt) {
		logger.LogAttrs(ctx, slog.LevelInfo, "reactionpoller: reaction predates plan; not a plan approval",
			slog.String("run_id", runRow.ID.String()),
			slog.Int64("reaction_id", r.ID),
			slog.Time("reaction_created_at", r.CreatedAt),
			slog.Time("plan_exists_at", planExistsAt))
		return
	}

	if err := t.Approvals.HandleApprovalCommand(ctx, webhook.ApprovalCommandParams{
		Repo:           runRow.Repo,
		IssueNumber:    issueNumber,
		InstallationID: derefInt64(runRow.InstallationID),
		SenderLogin:    r.User.Login,
		Decision:       webhook.MatchActionApprove,
		Source:         webhook.ApprovalSourceReactionEmoji,
	}); err != nil {
		// The handler is "best-effort companion to the SPA
		// flow" per its docstring — log and carry on.
		logger.LogAttrs(ctx, slog.LevelWarn, "reactionpoller: forward to approval handler failed",
			slog.String("run_id", runRow.ID.String()),
			slog.Int64("reaction_id", r.ID),
			slog.String("error", err.Error()))
	}
}

// planApprovalMeta resolves the two facts the poller needs to decide
// whether (and how fast) to poll a plan stage for approval reactions
// (#1054):
//
//   - The anchor comment id: the run's living anchor (#1054) is the
//     canonical comment reactions land on. Its id is recorded on the
//     `status_comment_posted` category, latest-row-wins (the
//     edit-in-place anchor keeps the same id across its lifetime).
//     This supersedes the pre-#1054 `plan_full`/`plan_updated` lookup;
//     those plan-on-issue comments no longer exist.
//   - The plan-existence instant: the first `plan_generated` audit
//     timestamp for the run. Used both as the cadence age origin and
//     as the approval cutoff (binding condition 2) — a reaction placed
//     before this instant is not a plan approval.
//
// Returns ok=false when either fact is missing: no anchor comment yet
// (run just created, comment not posted), or no plan generated yet (no
// approvable plan, so nothing to poll for).
func (t *Ticker) planApprovalMeta(ctx context.Context, runID uuid.UUID) (commentID int64, planExistsAt time.Time, ok bool) {
	statusEntries, err := t.Audit.ListForRunByCategory(ctx, runID, issuecomment.CategoryStatusCommentPosted)
	if err != nil {
		return 0, time.Time{}, false
	}
	// Latest-write wins for the anchor comment id.
	for i := len(statusEntries) - 1; i >= 0; i-- {
		if id := extractCommentID(statusEntries[i].Payload); id > 0 {
			commentID = id
			break
		}
	}
	if commentID == 0 {
		return 0, time.Time{}, false
	}

	planEntries, err := t.Audit.ListForRunByCategory(ctx, runID, planGeneratedCategory)
	if err != nil || len(planEntries) == 0 {
		return 0, time.Time{}, false
	}
	// First-write wins for plan existence: the earliest plan_generated
	// is when a plan first existed for the run.
	planExistsAt = planEntries[0].Timestamp
	for _, e := range planEntries {
		if e.Timestamp.Before(planExistsAt) {
			planExistsAt = e.Timestamp
		}
	}
	return commentID, planExistsAt, true
}

// planGeneratedCategory is the audit category the plan-upload handler
// appends when a standard_v1 plan artifact lands (server/plan.go). The
// first such entry's timestamp is the plan-existence cutoff (#1054).
const planGeneratedCategory = "plan_generated"

// observedReactionIDs reads the `plan_reaction_observed` rows for
// the run and returns the set of GitHub reaction IDs already
// recorded. The poller's dedup gate.
func (t *Ticker) observedReactionIDs(ctx context.Context, runID uuid.UUID) (map[int64]struct{}, error) {
	entries, err := t.Audit.ListForRunByCategory(ctx, runID, CategoryPlanReactionObserved)
	if err != nil {
		return nil, err
	}
	seen := make(map[int64]struct{}, len(entries))
	for _, e := range entries {
		if id := extractReactionID(e.Payload); id > 0 {
			seen[id] = struct{}{}
		}
	}
	return seen, nil
}

// cadenceFor returns the polling interval that applies given the
// comment's age at `now`.
func (t *Ticker) cadenceFor(now, commentPostedAt time.Time) time.Duration {
	threshold := t.AgeThreshold
	if threshold <= 0 {
		threshold = DefaultAgeThreshold
	}
	fast := t.FastInterval
	if fast <= 0 {
		fast = DefaultFastInterval
	}
	slow := t.SlowInterval
	if slow <= 0 {
		slow = DefaultSlowInterval
	}
	if now.Sub(commentPostedAt) < threshold {
		return fast
	}
	return slow
}

// shouldPoll reads the in-memory last-polled map and returns true
// when the per-stage cadence interval has elapsed (or no prior
// poll has happened in this process).
func (t *Ticker) shouldPoll(stageID uuid.UUID, now time.Time, cadence time.Duration) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	last, ok := t.lastPolledAt[stageID]
	if !ok {
		return true
	}
	return now.Sub(last) >= cadence
}

// recordLastPoll updates the in-memory last-poll timestamp for the
// stage. Lazily allocates the map.
func (t *Ticker) recordLastPoll(stageID uuid.UUID, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.lastPolledAt == nil {
		t.lastPolledAt = make(map[uuid.UUID]time.Time, 8)
	}
	t.lastPolledAt[stageID] = now
}

// extractCommentID reads `github_comment_id` from the
// `issue_commented` audit payload.
func extractCommentID(raw json.RawMessage) int64 {
	var p struct {
		ID int64 `json:"github_comment_id"`
	}
	_ = json.Unmarshal(raw, &p)
	return p.ID
}

// extractReactionID reads `reaction_id` from a
// `plan_reaction_observed` audit payload.
func extractReactionID(raw json.RawMessage) int64 {
	var p struct {
		ID int64 `json:"reaction_id"`
	}
	_ = json.Unmarshal(raw, &p)
	return p.ID
}

// splitRepoFullName turns "owner/name" into RepoRef. Returns an
// error for malformed inputs so the caller can skip cleanly.
func splitRepoFullName(s string) (githubclient.RepoRef, error) {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return githubclient.RepoRef{}, fmt.Errorf("invalid repo %q (want owner/name)", s)
	}
	return githubclient.RepoRef{Owner: parts[0], Name: parts[1]}, nil
}

// parseIssueNumber turns "issue:42" into 42. Returns an error for
// non-issue triggers (PR-, CLI-, or webhook-dispatch-triggered
// runs don't reach this path normally — the stages we care about
// were filtered to plan-stage awaiting-approval, which only fire
// after a plan comment was posted, which only happens for issue-
// triggered runs — but the helper validates as defense in depth).
func parseIssueNumber(triggerRef *string) (int, error) {
	if triggerRef == nil {
		return 0, errors.New("trigger_ref is nil")
	}
	const prefix = "issue:"
	if !strings.HasPrefix(*triggerRef, prefix) {
		return 0, fmt.Errorf("trigger_ref %q is not issue-shaped", *triggerRef)
	}
	n, err := parseInt(strings.TrimPrefix(*triggerRef, prefix))
	if err != nil {
		return 0, fmt.Errorf("parse issue number from %q: %w", *triggerRef, err)
	}
	return n, nil
}

func parseInt(s string) (int, error) {
	if s == "" {
		return 0, errors.New("empty")
	}
	var n int
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return 0, fmt.Errorf("non-digit %q", ch)
		}
		n = n*10 + int(ch-'0')
	}
	return n, nil
}

func derefInt64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}
