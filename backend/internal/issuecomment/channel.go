package issuecomment

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Channel is the notification-delivery seam introduced by ADR-015 (#79)
// option B: the externally-invoked notification surface, abstracted away
// from any single delivery medium. The method set is exactly the public
// comment-back surface the server and webhook dispatcher invoke today —
// every live surface in docs/issue-comment-surfaces.md (sticky status,
// plan-on-issue, CI-retry, budget alert, slash-command replies,
// run-rejected) plus the nil-safe ArtifactListerWired wiring introspector.
//
// The (audit category, kind) taxonomy documented in
// docs/issue-comment-surfaces.md is the routing key: a channel decides
// what to deliver (and how to dedup) from that taxonomy. In v0 the only
// channel is the GitHub-comment channel — *Notifier — so every Notify*
// call reaches the identical code path it did before the abstraction
// existed. A future Slack adapter (v0.x) is a new Channel appended to the
// Router with no change to this core (ADR-015 done-means #2).
type Channel interface {
	// NotifyStatusUpdateForRun rebuilds and edits the run's living-anchor
	// status comment and fires any page-class pings the new audit state
	// crossed (E20.4 / #330, anchor redrive #1054).
	NotifyStatusUpdateForRun(ctx context.Context, runID uuid.UUID) error
	// NotifyPageClassForRun fires any page-class pings the current audit
	// state crossed WITHOUT rebuilding the anchor — the pings-only immediate
	// sibling invoked at each batched page-class append site so a page posts
	// within the event's own window instead of the next transition (#1786).
	NotifyPageClassForRun(ctx context.Context, runID uuid.UUID) error
	// NotifyPlanReady fires the plan-ready hook after the plan stage
	// transitions terminally (#234); in the living-anchor world it routes
	// to the same anchor rebuild as every other transition.
	NotifyPlanReady(ctx context.Context, runID uuid.UUID, planStage *run.Stage, planArtifact *plan.Plan) error
	// NotifyCIRetry posts the CI-failure auto-retry comment (#279 / E16).
	NotifyCIRetry(ctx context.Context, runID uuid.UUID, parentRunID uuid.UUID, checkName string, attempt, max int) error
	// NotifyBudgetAlert posts the advisory periodic-budget warning comment
	// (ADR-030 / #688). posted reports whether a comment actually landed,
	// driving the cross-run budget_alert_sent dedup marker (#758).
	NotifyBudgetAlert(ctx context.Context, runID uuid.UUID, p BudgetAlertPayload) (posted bool, err error)
	// NotifySlashApprovalReply posts a reply to a /fishhawk approve|reject
	// command (#238).
	NotifySlashApprovalReply(ctx context.Context, p SlashApprovalReply) error
	// NotifyRunRejected posts the missing-plan-reviewer refusal comment
	// (#577 / #599) before any run row exists.
	NotifyRunRejected(ctx context.Context, repo string, installationID int64, issueNumber int, workflowID, stageID string) error
	// ArtifactListerWired reports whether this channel renders the living
	// anchor's plan section (the #1069 constructor-seam introspector). It
	// posts nothing; non-GitHub channels return false.
	ArtifactListerWired() bool
}

// Router is the notification core: it holds a set of channels and
// implements Channel itself by fanning every call out to each registered
// channel. It is the single point a future Slack adapter plugs into — add
// the adapter to the channel set and every Notify* surface reaches it with
// no change to the call sites or to the existing GitHub-comment channel
// (ADR-015 #79 option B).
//
// In v0 the Router wraps exactly one channel — the GitHub-comment
// *Notifier — so fan-out is a pass-through and there is no behavior
// change relative to calling the Notifier directly.
//
// Nil-safe to match the existing Notifier posture: a nil *Router and any
// nil channel entry are skipped, so call sites need no nil checks.
type Router struct {
	channels []Channel
}

// NewRouter returns a Router fanning out to the given channels (in order).
// nil entries are retained but skipped at dispatch time, so callers can
// pass issuecomment.New(...)'s result without nil-checking it.
func NewRouter(channels ...Channel) *Router {
	return &Router{channels: channels}
}

// each calls fn for every non-nil channel, joining the returned errors.
// Nil receiver / nil entries are skipped so the Router degrades to a
// no-op exactly like a nil *Notifier would.
func (r *Router) each(fn func(Channel) error) error {
	if r == nil {
		return nil
	}
	var errs []error
	for _, c := range r.channels {
		if c == nil {
			continue
		}
		if err := fn(c); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// NotifyStatusUpdateForRun fans the anchor rebuild out to every channel.
func (r *Router) NotifyStatusUpdateForRun(ctx context.Context, runID uuid.UUID) error {
	return r.each(func(c Channel) error { return c.NotifyStatusUpdateForRun(ctx, runID) })
}

// NotifyPageClassForRun fans the pings-only immediate hook out to every channel.
func (r *Router) NotifyPageClassForRun(ctx context.Context, runID uuid.UUID) error {
	return r.each(func(c Channel) error { return c.NotifyPageClassForRun(ctx, runID) })
}

// NotifyPlanReady fans the plan-ready hook out to every channel.
func (r *Router) NotifyPlanReady(ctx context.Context, runID uuid.UUID, planStage *run.Stage, planArtifact *plan.Plan) error {
	return r.each(func(c Channel) error { return c.NotifyPlanReady(ctx, runID, planStage, planArtifact) })
}

// NotifyCIRetry fans the CI-retry comment out to every channel.
func (r *Router) NotifyCIRetry(ctx context.Context, runID uuid.UUID, parentRunID uuid.UUID, checkName string, attempt, max int) error {
	return r.each(func(c Channel) error { return c.NotifyCIRetry(ctx, runID, parentRunID, checkName, attempt, max) })
}

// NotifyBudgetAlert fans the budget-alert comment out to every channel.
// posted is the OR across channels (true if ANY channel posted) and the
// error is the joined per-channel error. For v0's single GitHub channel
// the OR is the channel's own value, so the #758 cross-run dedup marker
// keys identically to the pre-abstraction path. Per-channel dedup for
// multiple channels (so a Slack post doesn't suppress a GitHub post, and
// vice versa) is a deferred v0.x concern to design when the Slack adapter
// lands — see ADR-015 (#79).
func (r *Router) NotifyBudgetAlert(ctx context.Context, runID uuid.UUID, p BudgetAlertPayload) (posted bool, err error) {
	if r == nil {
		return false, nil
	}
	var errs []error
	for _, c := range r.channels {
		if c == nil {
			continue
		}
		cposted, perr := c.NotifyBudgetAlert(ctx, runID, p)
		if cposted {
			posted = true
		}
		if perr != nil {
			errs = append(errs, perr)
		}
	}
	return posted, errors.Join(errs...)
}

// NotifySlashApprovalReply fans the slash-command reply out to every channel.
func (r *Router) NotifySlashApprovalReply(ctx context.Context, p SlashApprovalReply) error {
	return r.each(func(c Channel) error { return c.NotifySlashApprovalReply(ctx, p) })
}

// NotifyRunRejected fans the run-rejected refusal comment out to every channel.
func (r *Router) NotifyRunRejected(ctx context.Context, repo string, installationID int64, issueNumber int, workflowID, stageID string) error {
	return r.each(func(c Channel) error {
		return c.NotifyRunRejected(ctx, repo, installationID, issueNumber, workflowID, stageID)
	})
}

// ArtifactListerWired reports whether ANY channel renders the living
// anchor's plan section (OR across channels). Posts nothing.
func (r *Router) ArtifactListerWired() bool {
	if r == nil {
		return false
	}
	for _, c := range r.channels {
		if c == nil {
			continue
		}
		if c.ArtifactListerWired() {
			return true
		}
	}
	return false
}

// Compile-time assertions: both the GitHub-comment channel and the Router
// satisfy Channel.
var (
	_ Channel = (*Notifier)(nil)
	_ Channel = (*Router)(nil)
)
