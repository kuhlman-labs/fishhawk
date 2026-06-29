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
// This child implements ONLY mechanical advancement: acting on each run's
// gates is E25.6 and pause/page is E25.7, so until those land the started runs
// park at their plan/review gates for the operator-agent. The driver consumes
// existing surfaces through narrow interfaces so it is independently
// unit-testable with the campaign.fake and recording fakes, plus a
// Postgres-backed end-to-end test over a 2-issue depends_on campaign.
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

	t.start(ctx, logger, c, items)
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

// deriveAndTransition re-derives the campaign state from its items and, when
// it differs from the campaign's current state, transitions the campaign and
// emits a campaign_advanced entry. Idempotent: a no-change derivation emits
// nothing.
func (t *Ticker) deriveAndTransition(ctx context.Context, logger *slog.Logger, c *campaign.Campaign, items []*campaign.Item) {
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
