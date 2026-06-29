// Package campaigndriver runs the server-side ticker that mechanically
// advances each running campaign (Track C of ADR-047 / #1444, E25.5). It
// reuses the established background-worker shape —
// backend/internal/deployreconciler.Ticker / backend/internal/childcompletion.
// Sweeper: an exported Tick(ctx) one-pass method driven by a Run(ctx)
// interval loop, an off-by-default --enable-campaign-driver flag, and a
// fail-closed switch in serve.go that refuses to start when a required
// dependency is unwired.
//
// Per tick, for each campaign in state running the ticker runs two passes:
//
//   - ADVANCE: each running item whose linked run has reached a terminal run
//     state is transitioned to the mapped terminal item-state
//     (succeeded/failed/cancelled), a campaign_issue_settled entry is emitted,
//     and the campaign state is re-derived from its items (campaign.DeriveState)
//     and transitioned with a campaign_advanced entry when it changes.
//   - START: items partitioned Eligible by the E25.3 pure engine
//     (campaign.NextEligible) are started — bounded by a concurrency cap
//     counting currently-running items — via the RunStarter seam, linked to
//     their item (SetCampaignItemRun), transitioned to running, and recorded
//     with a campaign_issue_started entry.
//
// ADVANCE runs BEFORE START within a tick and re-reads the items in between,
// so a predecessor that reached terminal this tick settles AND its now-eligible
// dependent starts in the SAME tick rather than waiting a full interval — the
// "a predecessor merging unblocks dependents" contract.
//
// On top of mechanical advancement, the driver AUTO-ACTS on each running
// run's gate under the operator_agent contract (E25.6 / ADR-047 Track C):
// during the ADVANCE pass, every running item whose linked run is NON-terminal
// is handed to the optional GateActor seam BEFORE terminal observation. The
// actor (server.Server.AutoDriveRunGate, bound in serve.go) re-evaluates the
// run's delegation in-process and, for a delegated knob whose condition is met
// AND whose real gate state matches, takes the gate action (approve/fixup/
// retry/merge) under the campaign operator identity, recording the run-level
// audit itself; the driver records a campaign-level campaign_gate_acted marker.
// The GateActor is OPTIONAL: a nil seam (auto-drive disabled / merge client
// unconfigured) leaves the run parked for the human operator-agent — the
// driver then advances campaigns mechanically and observes only. pause/page is
// E25.7; this child's actor only EMITS the campaign_gate_paged hand-off (on the
// run chain, written by the actor) and takes no action on a must_page_human
// gate. The driver consumes existing surfaces through narrow interfaces so it
// is independently unit-testable with the campaign.fake and recording fakes,
// plus a Postgres-backed end-to-end test over a 2-issue depends_on campaign.
//
// Per-item and per-campaign errors WARN-log and never abort the tick; a
// transient error leaves the work for the next tick. Mirrors the
// deployreconciler / childcompletion posture.
package campaigndriver

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// DefaultInterval is the tick period fishhawkd applies when the caller leaves
// Interval zero. Matches the other background workers' 60s cadence.
const DefaultInterval = 60 * time.Second

// DefaultMaxParallel bounds how many of a campaign's items may be running
// concurrently when no campaign-level cap is configured. The campaign model
// (E25.2) carries no max_parallel field yet, so the driver applies this
// package default; a campaign-level override lands with the campaign-config
// work (E25.4/E25.7). The effective budget per tick is this minus the count of
// items already running.
const DefaultMaxParallel = 4

// Audit categories the driver emits. Free-form strings (audit.Entry.Category
// is not a Go enum), documented as a surface in docs/issue-comment-surfaces.md.
// They ride the GLOBAL audit chain (AppendGlobalChained) because a campaign is
// not a run and the per-run chain is keyed to runs; the run linkage travels in
// the payload's run_id field.
const (
	categoryCampaignIssueStarted = "campaign_issue_started"
	categoryCampaignIssueSettled = "campaign_issue_settled"
	categoryCampaignAdvanced     = "campaign_advanced"
	// categoryCampaignGateActed is the campaign-level marker the driver
	// records on the GLOBAL chain when the GateActor took a delegated gate
	// action on a running run (E25.6 / ADR-047). The run-level audit of the
	// action itself (approval_submitted / stage_fixup_triggered / stage_retried
	// / pr_merged, stamped ActorAgent operator-agent/campaign) is written by the
	// actor on the run chain; this marker ties that action back to the campaign.
	categoryCampaignGateActed = "campaign_gate_acted"
	// categoryCampaignPaused is the campaign-level marker the driver records on
	// the GLOBAL chain when the GateActor REFUSED a must_page_human gate
	// (out.Paged) and the driver paused the affected item — and, under the
	// pause_campaign policy, the whole campaign — and fired the page (E25.7 /
	// ADR-047 Track C). The run-chained campaign_gate_paged hand-off entry the
	// actor wrote is the page trigger; this marker records the pause action on
	// the campaign side.
	categoryCampaignPaused = "campaign_paused"
)

// CampaignStore is the slice of campaign.Repository the driver uses: list the
// running campaigns and their items, link an item to its run, and apply
// item/campaign state transitions. Satisfied by campaign.Repository; extracted
// as an interface so unit tests substitute the campaign fake.
type CampaignStore interface {
	ListCampaigns(ctx context.Context, f campaign.ListCampaignsFilter) ([]*campaign.Campaign, error)
	ListCampaignItemsForCampaign(ctx context.Context, campaignID uuid.UUID) ([]*campaign.Item, error)
	SetCampaignItemRun(ctx context.Context, itemID uuid.UUID, runID *uuid.UUID) (*campaign.Item, error)
	TransitionCampaignItem(ctx context.Context, id uuid.UUID, to campaign.ItemState) (*campaign.Item, error)
	TransitionCampaign(ctx context.Context, id uuid.UUID, to campaign.State) (*campaign.Campaign, error)
	// PauseCampaignItem transitions a running item to paused, recording the
	// PauseReason (the page event + run/stage) under the same FOR UPDATE lock
	// as the other item transitions (E25.7). Used by the Paged branch when the
	// GateActor hands a must_page_human gate off to a human.
	PauseCampaignItem(ctx context.Context, id uuid.UUID, reason campaign.PauseReason) (*campaign.Item, error)
}

// RunReader reads a run row for terminal detection. A narrow capability —
// only GetRun — so the unit-test fake stays tiny rather than implementing the
// full run.Repository. Satisfied by run.Repository.
type RunReader interface {
	GetRun(ctx context.Context, id uuid.UUID) (*run.Run, error)
}

// RunStarter starts a run for an eligible campaign item. The single
// integrating seam for run creation: the serve.go adapter satisfies it over
// Server.StartRunForCampaignIssue (which routes through CreateRunForTrigger),
// while unit tests substitute a recording fake. Keeping it an interface
// defined HERE avoids an import cycle (the driver never imports server) and
// keeps the driver's mechanical logic decoupled from how a run is minted.
type RunStarter interface {
	StartCampaignRun(ctx context.Context, item *campaign.Item, c *campaign.Campaign) (*run.Run, error)
}

// AuditAppender records the driver's campaign-level audit entries on the
// global chain. Satisfied by audit.Repository.
type AuditAppender interface {
	AppendGlobalChained(ctx context.Context, p audit.GlobalChainAppendParams) (*audit.Entry, error)
}

// GateActionOutcome reports what the GateActor did at a run gate so the
// driver can record the campaign-level marker. It mirrors the fields of
// server.AutoDriveOutcome the driver needs; the serve.go adapter translates.
// On an observe-only outcome both Acted and Paged are false.
type GateActionOutcome struct {
	// Acted is true when the actor dispatched a delegated gate action;
	// Action then names the delegation verb taken (approve/route_fixup/
	// retry/merge). The driver records a campaign_gate_acted marker.
	Acted  bool
	Action string
	// Paged is true when the actor REFUSED a must_page_human condition and
	// emitted the campaign_gate_paged hand-off (on the run chain) itself;
	// PageEvent names the event. No gate action was taken and the driver
	// records nothing extra (the run-level page entry is the hand-off).
	Paged     bool
	PageEvent string
	// Note is a short human-readable summary for the driver log. Always set.
	Note string
}

// GateActor auto-acts on a single running run's gate under the operator_agent
// contract (E25.6 / ADR-047). The single seam between the campaign driver and
// the gate-action machinery: the serve.go adapter satisfies it over
// server.Server.AutoDriveRunGate (binding the campaign operator identity + the
// GitHub merge client), while unit tests substitute a recording fake. Defined
// HERE so the driver never imports server (avoiding an import cycle) and stays
// decoupled from how a gate action is taken. OPTIONAL on the Ticker: a nil
// GateActor disables auto-driving (the driver observes only).
type GateActor interface {
	DriveRunGate(ctx context.Context, runRow *run.Run) (GateActionOutcome, error)
}

// CampaignGateActor is the optional campaign-aware extension of GateActor
// (E25.12 / #1451): a GateActor that also accepts the owning campaign's
// operator_agent override bytes. driveGate prefers it when the bound actor
// implements it, threading c.OperatorAgent so the run resolves its delegation
// against the campaign block applied as the outermost rung of the delegation
// ladder (campaign > gate > workflow, wholesale); nil/empty leaves the run on
// its own workflow contract. Kept separate from GateActor so observe-only
// actors (and existing tests) that only implement the base seam still satisfy
// Ticker.GateActor without taking on the campaign-override parameter.
type CampaignGateActor interface {
	GateActor
	DriveRunGateWithCampaign(ctx context.Context, runRow *run.Run, campaignOverride []byte) (GateActionOutcome, error)
}

// Notifier fires the human page when the auto-driver hands a gate off (E25.7).
// The single seam between the campaign driver and the issue-comment notifier:
// serve.go binds it to the existing issuecomment notifier's
// NotifyStatusUpdateForRun, which re-renders the run's status anchor and posts
// the page-class campaign_gate_paged ping (issuecomment/ping.go) on the run's
// issue — where the operator is already watching. Defined HERE so the driver
// never imports the notifier package (avoiding an import cycle). OPTIONAL on
// the Ticker: a nil Notifier makes the Paged branch observe-only — the pause is
// still recorded but no page is fired.
type Notifier interface {
	NotifyStatusUpdateForRun(ctx context.Context, runID uuid.UUID) error
}

// Ticker mechanically advances running campaigns. Run() blocks until ctx is
// cancelled. All of Campaigns, Runs, Starter, and Audit are required; a nil
// one is a configuration error caught by Run() and is a logged no-op in Tick()
// (the fail-closed posture — the driver exists solely to advance campaigns and
// has nothing useful to do without any of them).
type Ticker struct {
	Campaigns CampaignStore
	Runs      RunReader
	Starter   RunStarter
	Audit     AuditAppender

	// GateActor auto-acts on each running run's gate under the operator_agent
	// contract (E25.6 / ADR-047). OPTIONAL: a nil actor disables auto-driving
	// and the ticker advances campaigns mechanically and observes only. Not
	// among the required dependencies Run()/Tick() guard on — the driver is
	// fully functional (mechanical advancement) without it.
	GateActor GateActor

	// Notifier fires the human page when the GateActor refuses a
	// must_page_human gate (E25.7). OPTIONAL: a nil Notifier makes the Paged
	// branch observe-only — the pause is still recorded but no page is posted.
	// Not among the required dependencies Run()/Tick() guard on.
	Notifier Notifier

	// Logger receives structured warnings about transient errors. nil →
	// slog.Default().
	Logger *slog.Logger

	// Interval is the tick period. Defaults to DefaultInterval when zero.
	// The ticker fires immediately on Run() start; the interval gates
	// subsequent ticks.
	Interval time.Duration

	// MaxParallel is the per-campaign running-item cap. Defaults to
	// DefaultMaxParallel when <= 0.
	MaxParallel int

	// Now sources the current time for audit timestamps. nil → time.Now.
	Now func() time.Time
}

// Run drives the ticker until ctx is cancelled. Per-campaign and per-item
// errors log but never abort the loop.
func (t *Ticker) Run(ctx context.Context) error {
	if t.Campaigns == nil || t.Runs == nil || t.Starter == nil || t.Audit == nil {
		return errors.New("campaigndriver: Campaigns, Runs, Starter, and Audit must all be set")
	}
	interval := t.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}

	// Fire once at startup so an instance that just restarted picks up any
	// campaign that became advanceable during the gap.
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

// Tick performs one pass over running campaigns. Exported so tests drive a
// single deterministic pass without spinning real timers. A nil required
// dependency makes Tick a logged no-op (it never panics and starts no run) —
// the fail-closed contract.
func (t *Ticker) Tick(ctx context.Context) {
	logger := t.logger()
	if t.Campaigns == nil || t.Runs == nil || t.Starter == nil || t.Audit == nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "campaigndriver: tick skipped; a required dependency is nil")
		return
	}

	campaigns, err := t.Campaigns.ListCampaigns(ctx, campaign.ListCampaignsFilter{
		State: string(campaign.StateRunning),
		Limit: 100,
	})
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "campaigndriver: list running campaigns failed",
			slog.String("error", err.Error()))
		return
	}
	for _, c := range campaigns {
		if c.State != campaign.StateRunning {
			// Defense-in-depth: the filter already constrains to running. A
			// fake or future listing returning a non-running campaign can't
			// reach the advance/start passes.
			continue
		}
		t.processCampaign(ctx, logger, c)
	}
}

// processCampaign runs the ADVANCE then START passes for one running
// campaign, re-reading items between them so a predecessor settled this tick
// unblocks its dependent in the same tick. Per-item errors WARN-log and never
// abort the campaign.
func (t *Ticker) processCampaign(ctx context.Context, logger *slog.Logger, c *campaign.Campaign) {
	items, err := t.Campaigns.ListCampaignItemsForCampaign(ctx, c.ID)
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "campaigndriver: list items failed",
			slog.String("campaign_id", c.ID.String()),
			slog.String("error", err.Error()))
		return
	}

	settledAny := t.advance(ctx, logger, c, items)

	if settledAny {
		// Re-read so the campaign re-derivation and the START pass see the
		// just-settled item states.
		refreshed, rerr := t.Campaigns.ListCampaignItemsForCampaign(ctx, c.ID)
		if rerr != nil {
			logger.LogAttrs(ctx, slog.LevelWarn, "campaigndriver: re-list items after settle failed",
				slog.String("campaign_id", c.ID.String()),
				slog.String("error", rerr.Error()))
			return
		}
		items = refreshed
		t.deriveAndTransition(ctx, logger, c, items)
	}

	// Skip the START pass once the campaign left the running state this tick —
	// a mid-tick pause (E25.7) or a derived terminal state must not dispatch new
	// runs. A paused campaign re-engages only on resume (paused->running), and a
	// terminal one has nothing to start.
	if c.State == campaign.StateRunning {
		t.start(ctx, logger, c, items)
	}
}

// advance settles every running item whose linked run has reached a terminal
// run state, emitting a campaign_issue_settled entry per settle. Returns
// whether any item was settled this pass (so the caller re-derives the
// campaign state). Idempotent: a non-running item, an item with no run, or an
// item whose run is still in flight is skipped, so a re-tick over an
// already-settled item does nothing.
func (t *Ticker) advance(ctx context.Context, logger *slog.Logger, c *campaign.Campaign, items []*campaign.Item) bool {
	settledAny := false
	for _, it := range items {
		if it.State != campaign.ItemStateRunning || it.RunID == nil {
			continue
		}
		runRow, err := t.Runs.GetRun(ctx, *it.RunID)
		if err != nil {
			logger.LogAttrs(ctx, slog.LevelWarn, "campaigndriver: get linked run failed",
				slog.String("campaign_id", c.ID.String()),
				slog.String("item_id", it.ID.String()),
				slog.String("run_id", it.RunID.String()),
				slog.String("error", err.Error()))
			continue
		}
		if !runRow.State.IsTerminal() {
			// E25.6: auto-act on the run's gate under the operator_agent
			// contract before terminal observation. A non-terminal run parked
			// at a gate is the actor's opportunity to approve/fixup/retry/merge;
			// any action it takes moves the run toward terminal, observed and
			// settled on a subsequent tick (the 60s-latency posture). Disabled
			// (nil actor) leaves the run parked for the human operator-agent.
			t.driveGate(ctx, logger, c, it, runRow)
			continue
		}
		target, ok := mapRunTerminalToItem(runRow.State)
		if !ok {
			// A terminal run state we don't map (defensive — the run enum's
			// terminal set is exactly succeeded/failed/cancelled). Leave the
			// item running rather than guess.
			logger.LogAttrs(ctx, slog.LevelWarn, "campaigndriver: unmapped terminal run state; item left running",
				slog.String("campaign_id", c.ID.String()),
				slog.String("item_id", it.ID.String()),
				slog.String("run_state", string(runRow.State)))
			continue
		}
		if !campaign.ValidCampaignItemTransition(it.State, target) {
			continue
		}
		if _, err := t.Campaigns.TransitionCampaignItem(ctx, it.ID, target); err != nil {
			logger.LogAttrs(ctx, slog.LevelWarn, "campaigndriver: settle item transition failed",
				slog.String("campaign_id", c.ID.String()),
				slog.String("item_id", it.ID.String()),
				slog.String("target", string(target)),
				slog.String("error", err.Error()))
			continue
		}
		settledAny = true
		t.emit(ctx, logger, categoryCampaignIssueSettled, map[string]any{
			"campaign_id": c.ID.String(),
			"issue_ref":   it.IssueRef,
			"run_id":      it.RunID.String(),
			"outcome":     string(target),
		})
	}
	return settledAny
}

// driveGate hands one running item's non-terminal run to the GateActor so it
// can auto-act on the run's gate under the operator_agent contract (E25.6 /
// ADR-047). A nil actor is observe-only — the run stays parked for the human
// operator-agent. On an action (out.Acted) the driver records the
// campaign-level campaign_gate_acted marker (the run-level audit of the action
// is the actor's responsibility); a refusal (out.Paged) is the actor's
// campaign_gate_paged hand-off on the run chain and the driver adds nothing.
// Best-effort: an actor error WARN-logs and leaves the gate for the next tick,
// never aborting the campaign (mirrors the per-item error posture).
func (t *Ticker) driveGate(ctx context.Context, logger *slog.Logger, c *campaign.Campaign, it *campaign.Item, runRow *run.Run) {
	if t.GateActor == nil {
		return
	}
	// Thread the campaign's operator_agent override bytes (E25.12 / #1451) so
	// the actor resolves this run's delegation against the campaign block
	// wholesale when present; nil/empty leaves the run on its workflow contract.
	// A base-only GateActor (observe-only/legacy) ignores the override.
	var (
		out GateActionOutcome
		err error
	)
	if ca, ok := t.GateActor.(CampaignGateActor); ok {
		out, err = ca.DriveRunGateWithCampaign(ctx, runRow, c.OperatorAgent)
	} else {
		out, err = t.GateActor.DriveRunGate(ctx, runRow)
	}
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "campaigndriver: auto-drive run gate failed; left for next tick",
			slog.String("campaign_id", c.ID.String()),
			slog.String("item_id", it.ID.String()),
			slog.String("run_id", runRow.ID.String()),
			slog.String("error", err.Error()))
		return
	}
	if out.Acted {
		t.emit(ctx, logger, categoryCampaignGateActed, map[string]any{
			"campaign_id": c.ID.String(),
			"issue_ref":   it.IssueRef,
			"run_id":      runRow.ID.String(),
			"action":      out.Action,
		})
		return
	}
	if out.Paged {
		t.pageGate(ctx, logger, c, it, runRow, out.PageEvent)
	}
}

// pageGate handles a must_page_human hand-off (out.Paged) the GateActor
// refused (E25.7 / ADR-047 Track C): it pauses the affected item — recording
// the PauseReason (the page event + run) — and, unless the campaign's
// PausePolicy is pause_item (continue-others), pauses the whole campaign; it
// records a campaign_paused marker on the global chain and fires the human page
// through the Notifier seam (which posts the campaign_gate_paged page-class
// ping on the run's issue). The actor already wrote the run-chained
// campaign_gate_paged hand-off entry — the page trigger — so this method only
// pauses and pings. Best-effort and ordered so the SAFE outcome (the pause)
// always lands: a PauseCampaignItem error aborts (no campaign pause, no page);
// a campaign-pause or page error after the item paused only WARN-logs, leaving
// the recorded pause intact. A nil Notifier records the pause and skips the
// page (observe-only).
func (t *Ticker) pageGate(ctx context.Context, logger *slog.Logger, c *campaign.Campaign, it *campaign.Item, runRow *run.Run, pageEvent string) {
	runID := runRow.ID
	reason := campaign.PauseReason{PageEvent: pageEvent, RunID: &runID}
	if _, err := t.Campaigns.PauseCampaignItem(ctx, it.ID, reason); err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "campaigndriver: pause item on gate hand-off failed; left for next tick",
			slog.String("campaign_id", c.ID.String()),
			slog.String("item_id", it.ID.String()),
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
		return
	}

	// pause_item (continue-others) pauses only the affected item; any other
	// policy (including the normalized default pause_campaign, and an unset
	// zero value defensively) blocks the whole campaign.
	if c.PausePolicy != campaign.PausePolicyPauseItem {
		if campaign.ValidCampaignTransition(c.State, campaign.StatePaused) {
			if _, err := t.Campaigns.TransitionCampaign(ctx, c.ID, campaign.StatePaused); err != nil {
				logger.LogAttrs(ctx, slog.LevelWarn, "campaigndriver: pause campaign on gate hand-off failed; item paused, campaign left for next tick",
					slog.String("campaign_id", c.ID.String()),
					slog.String("error", err.Error()))
			} else {
				// Reflect the pause on the in-memory campaign so the same tick's
				// deriveAndTransition sees it as already-paused (sticky) and the
				// START pass is skipped — the postgres TransitionCampaign returns
				// a fresh row and does not mutate c.
				c.State = campaign.StatePaused
			}
		}
	}

	t.emit(ctx, logger, categoryCampaignPaused, map[string]any{
		"campaign_id": c.ID.String(),
		"issue_ref":   it.IssueRef,
		"run_id":      runID.String(),
		"page_event":  pageEvent,
		"policy":      string(normalizedPolicy(c.PausePolicy)),
	})

	if t.Notifier == nil {
		return
	}
	if err := t.Notifier.NotifyStatusUpdateForRun(ctx, runID); err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "campaigndriver: fire page on gate hand-off failed; pause recorded",
			slog.String("campaign_id", c.ID.String()),
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
	}
}

// normalizedPolicy reports the effective pause policy for the audit marker,
// defaulting a zero value to pause_campaign (a persisted campaign is never
// empty, but the marker should record the effective policy even for a
// hand-built campaign).
func normalizedPolicy(p campaign.PausePolicy) campaign.PausePolicy {
	if p == campaign.PausePolicyPauseItem {
		return campaign.PausePolicyPauseItem
	}
	return campaign.PausePolicyPauseCampaign
}

// deriveAndTransition re-derives the campaign state from its items and, when
// it differs from the campaign's current state, transitions the campaign and
// emits a campaign_advanced entry. Idempotent: a no-change derivation emits
// nothing.
func (t *Ticker) deriveAndTransition(ctx context.Context, logger *slog.Logger, c *campaign.Campaign, items []*campaign.Item) {
	if c.State == campaign.StatePaused {
		// Sticky-paused (E25.7): a campaign the driver/operator paused must not
		// be auto-unpaused by a sibling settling this tick. paused->running is a
		// valid transition (the resume verb uses it), so without this guard an
		// un-paused re-derive could silently resume a paused campaign. Resuming
		// is an explicit operator action (POST /resume), never a derivation.
		return
	}
	newState := campaign.DeriveState(items)
	if newState == c.State {
		return
	}
	if !campaign.ValidCampaignTransition(c.State, newState) {
		// A derived state the transition table forbids from the current state
		// (defensive). Surface it rather than forcing an illegal transition.
		logger.LogAttrs(ctx, slog.LevelWarn, "campaigndriver: derived state not a valid transition; campaign left unchanged",
			slog.String("campaign_id", c.ID.String()),
			slog.String("from", string(c.State)),
			slog.String("to", string(newState)))
		return
	}
	if _, err := t.Campaigns.TransitionCampaign(ctx, c.ID, newState); err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "campaigndriver: campaign transition failed",
			slog.String("campaign_id", c.ID.String()),
			slog.String("from", string(c.State)),
			slog.String("to", string(newState)),
			slog.String("error", err.Error()))
		return
	}
	t.emit(ctx, logger, categoryCampaignAdvanced, map[string]any{
		"campaign_id": c.ID.String(),
		"from":        string(c.State),
		"to":          string(newState),
	})
}

// start dispatches eligible items up to the per-campaign concurrency budget
// (effective max minus currently-running items), starting each via the
// RunStarter seam, linking it to its item, transitioning it to running, and
// emitting a campaign_issue_started entry. Idempotent: an item that already
// has a run, or is no longer in a startable state, is skipped.
func (t *Ticker) start(ctx context.Context, logger *slog.Logger, c *campaign.Campaign, items []*campaign.Item) {
	elig := campaign.NextEligible(items)
	budget := t.effectiveMaxParallel() - len(elig.Running)
	if budget <= 0 || len(elig.Eligible) == 0 {
		return
	}

	byRef := make(map[string]*campaign.Item, len(items))
	for _, it := range items {
		byRef[it.IssueRef] = it
	}

	for _, ref := range elig.Eligible {
		if budget <= 0 {
			break
		}
		it, ok := byRef[ref]
		if !ok {
			continue
		}
		// Idempotency guard: only start an item with no run yet and a state
		// from which running is reachable. NextEligible already excludes
		// linked/terminal items, but re-check defensively.
		if it.RunID != nil || !campaign.ValidCampaignItemTransition(it.State, campaign.ItemStateRunning) {
			continue
		}

		runRow, err := t.Starter.StartCampaignRun(ctx, it, c)
		if err != nil {
			logger.LogAttrs(ctx, slog.LevelWarn, "campaigndriver: start run for item failed; will retry next tick",
				slog.String("campaign_id", c.ID.String()),
				slog.String("item_id", it.ID.String()),
				slog.String("issue_ref", it.IssueRef),
				slog.String("error", err.Error()))
			continue
		}
		if runRow == nil {
			logger.LogAttrs(ctx, slog.LevelWarn, "campaigndriver: starter returned nil run; item left un-started",
				slog.String("campaign_id", c.ID.String()),
				slog.String("item_id", it.ID.String()))
			continue
		}

		if _, err := t.Campaigns.SetCampaignItemRun(ctx, it.ID, &runRow.ID); err != nil {
			logger.LogAttrs(ctx, slog.LevelWarn, "campaigndriver: link item to run failed",
				slog.String("campaign_id", c.ID.String()),
				slog.String("item_id", it.ID.String()),
				slog.String("run_id", runRow.ID.String()),
				slog.String("error", err.Error()))
			continue
		}
		if _, err := t.Campaigns.TransitionCampaignItem(ctx, it.ID, campaign.ItemStateRunning); err != nil {
			logger.LogAttrs(ctx, slog.LevelWarn, "campaigndriver: transition item to running failed; unlinking run so the item retries next tick",
				slog.String("campaign_id", c.ID.String()),
				slog.String("item_id", it.ID.String()),
				slog.String("run_id", runRow.ID.String()),
				slog.String("error", err.Error()))
			// The link committed but the running transition did not, leaving the
			// item linked-but-not-running. NextEligible classifies that item as
			// Running (RunID set, non-terminal), so without a rollback it is
			// never settled (advance requires state running) nor re-dispatched
			// (start only acts on Eligible) — it is permanently stranded against
			// the contract that a transient per-item error stays retryable. Roll
			// the link back so the next tick re-partitions it as Eligible and
			// retries. Best-effort: an unlink failure is logged and the item
			// then waits for manual repair rather than being silently
			// half-started.
			if _, uerr := t.Campaigns.SetCampaignItemRun(ctx, it.ID, nil); uerr != nil {
				logger.LogAttrs(ctx, slog.LevelWarn, "campaigndriver: unlink after failed running transition also failed; item left linked-but-not-running",
					slog.String("campaign_id", c.ID.String()),
					slog.String("item_id", it.ID.String()),
					slog.String("run_id", runRow.ID.String()),
					slog.String("error", uerr.Error()))
			}
			continue
		}
		budget--
		t.emit(ctx, logger, categoryCampaignIssueStarted, map[string]any{
			"campaign_id": c.ID.String(),
			"issue_ref":   it.IssueRef,
			"run_id":      runRow.ID.String(),
		})
	}
}

// emit appends one campaign-level audit entry on the global chain.
// Best-effort: a marshal or append error WARN-logs and never unwinds the
// transition it records, mirroring the sweeper/reconciler audit posture.
func (t *Ticker) emit(ctx context.Context, logger *slog.Logger, category string, payload map[string]any) {
	body, err := json.Marshal(payload)
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "campaigndriver: marshal audit payload failed",
			slog.String("category", category),
			slog.String("error", err.Error()))
		return
	}
	systemKind := audit.ActorSystem
	if _, err := t.Audit.AppendGlobalChained(ctx, audit.GlobalChainAppendParams{
		Timestamp: t.now(),
		Category:  category,
		ActorKind: &systemKind,
		Payload:   body,
	}); err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "campaigndriver: append audit entry failed",
			slog.String("category", category),
			slog.String("error", err.Error()))
	}
}

// mapRunTerminalToItem maps a terminal run state to the campaign item's
// terminal state. ok=false for a non-terminal or unmapped state.
func mapRunTerminalToItem(s run.State) (campaign.ItemState, bool) {
	switch s {
	case run.StateSucceeded:
		return campaign.ItemStateSucceeded, true
	case run.StateFailed:
		return campaign.ItemStateFailed, true
	case run.StateCancelled:
		return campaign.ItemStateCancelled, true
	default:
		return "", false
	}
}

func (t *Ticker) effectiveMaxParallel() int {
	if t.MaxParallel > 0 {
		return t.MaxParallel
	}
	return DefaultMaxParallel
}

func (t *Ticker) now() time.Time {
	if t.Now != nil {
		return t.Now().UTC()
	}
	return time.Now().UTC()
}

func (t *Ticker) logger() *slog.Logger {
	if t.Logger != nil {
		return t.Logger
	}
	return slog.Default()
}
