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
//     with a campaign_issue_started entry. A RunStarter that returns a
//     *StartRefusal (#4525) ends the campaign's START pass for the tick: no
//     item of it starts, and one campaign_auto_start_refused entry is recorded
//     per (campaign, reason).
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
// actor (server.Server.DriveCampaignRunGate over AutoDriveRunGate, bound in
// serve.go) re-evaluates the
// run's delegation in-process and, for a delegated knob whose condition is met
// AND whose real gate state matches, takes the gate action (approve/fixup/
// retry/merge) under the campaign operator identity, recording the run-level
// audit itself; the driver records a campaign-level campaign_gate_acted marker.
// The GateActor is OPTIONAL: a nil seam (auto-drive disabled / merge client
// unconfigured) leaves the run parked for the human operator-agent — the
// driver then advances campaigns mechanically and observes only. pause/page is
// E25.7; this child's actor only EMITS the campaign_gate_paged hand-off (on the
// run chain, written by the actor) and takes no action on a must_page_human
// gate. A DecisionRequired outcome (a gate that needs a human decision) is
// recorded once per occurrence as a run-chained
// campaign_gate_decision_required marker and cleared with a
// campaign_gate_decision_cleared row (E88.4 / #4528; README). The driver consumes existing surfaces through narrow interfaces so it
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
	"sync"
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
	// categoryCampaignAutoStartRefused is the campaign-level marker the
	// driver records on the GLOBAL chain when the RunStarter returned a
	// *StartRefusal (#4525): the campaign cannot be auto-started (for example
	// it resolves to a local run with no working_dir binding), so the driver
	// starts no item of it. Recorded once per (campaign, reason) while the
	// campaign stays running, not every tick.
	categoryCampaignAutoStartRefused = "campaign_auto_start_refused"
)

// Decision-required marker categories and the run-level gate token (E88.4 /
// #4528). Exported so the server's campaign status read names them without a
// string copy. Unlike the campaign-level markers above, both ride the RUN chain
// (AppendChained, RunID = the item's linked run, actor system): a parked
// decision belongs to one run, and the status read looks it up by that run.
//
// The marker/clear pair is an INTERIM registration under ADR-106's existing
// audit-category mechanism (audit.KnownCategories); it needs no new ADR and
// takes the mandatory run-bound flag when #4599 / E89 #4537 lands. The payload
// carries no cross-run identifiers: only this run's own campaign_id, issue_ref,
// run_id, gate stage and closed-set tokens (never the actor's Note, which can
// name other runs). The once-per-occurrence dedupe assumes a single campaign
// driver per deployment; two concurrent drivers could double-append.
const (
	// CategoryCampaignGateDecisionRequired records that the GateActor returned
	// a DecisionRequired outcome: the gate needs a human decision. Appended
	// once per occurrence key (run, decision_state, action, gate).
	CategoryCampaignGateDecisionRequired = "campaign_gate_decision_required"
	// CategoryCampaignGateDecisionCleared records that a previously recorded
	// decision stopped being current: a later non-decision outcome whose stage
	// read positively showed the keyed gate no longer parked.
	CategoryCampaignGateDecisionCleared = "campaign_gate_decision_cleared"
	// GateRunLevel is the gate value of a decision with no parked plan or
	// implement stage (for example fixup_budget_exhausted on a run whose
	// implement stage already succeeded).
	GateRunLevel = "run"
)

// Values of the cleared row's cleared_by field: the outcome kind that cleared
// the decision.
const (
	clearedByActed      = "acted"
	clearedByPaged      = "paged"
	clearedByNoDecision = "no_decision"
)

// StartRefusal is the error a RunStarter returns when a CAMPAIGN cannot be
// auto-started at all (#4525), as opposed to a transient per-item failure.
// On a StartRefusal the driver ends the campaign's START pass for the tick, so
// no item of that campaign is started, linked or transitioned, and records one
// campaign_auto_start_refused entry per (campaign, reason). Every field is a
// server constant or enum value: the serve.go adapter fills them from
// server.ResolveCampaignAutoStart, which never echoes the campaign's
// working_dir or any issue or campaign text.
type StartRefusal struct {
	// Reason is a stable machine-readable token (working_dir_unbound,
	// working_dir_not_absolute, campaign_runner_kind_invalid).
	Reason string
	// RunnerKind is the resolved runner kind, or "" when the campaign's
	// recorded kind is invalid.
	RunnerKind string
	// RunnerKindSource names the ladder rung that chose the kind (campaign or
	// auto_start_flag).
	RunnerKindSource string
	// Detail is a short human-readable explanation.
	Detail string
}

// Error implements error.
func (e *StartRefusal) Error() string {
	return "campaign auto-start refused: " + e.Reason
}

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
// global chain, and the decision-required marker and clear rows on the run
// chain (E88.4 / #4528), which it reads back once per run per process to
// hydrate the dedupe key. Satisfied by audit.Repository.
type AuditAppender interface {
	AppendGlobalChained(ctx context.Context, p audit.GlobalChainAppendParams) (*audit.Entry, error)
	AppendChained(ctx context.Context, p audit.ChainAppendParams) (*audit.Entry, error)
	ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error)
}

// GateActionOutcome reports what the GateActor did at a run gate so the
// driver can record the campaign-level marker. It mirrors the fields of
// server.AutoDriveOutcome the driver needs; server.Server.DriveCampaignRunGate
// translates (the serve.go adapter delegates to it) and adds the stage read
// behind GateKnown, GateStageID and ParkedGateStageIDs.
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
	// Never recorded in an audit payload: it can name other runs.
	Note string
	// DecisionRequired is true when the actor could not auto-act on an
	// expected gate state and hands the gate to a human (E88.4 / #4528);
	// DecisionState names the state (human_quorum_required,
	// fixup_budget_exhausted, ...) and Action the delegated verb whose arm
	// surfaced it. Set with Acted=false and Paged=false. The driver records one
	// campaign_gate_decision_required marker per occurrence.
	DecisionRequired bool
	DecisionState    string
	// GateKnown is true when the adapter read the run's stages after the gate
	// evaluation. False when the read failed, the run repository is unwired,
	// or the read was skipped because the driver holds no key and the outcome
	// is not a decision. An unknown gate never appends a marker and never
	// clears one.
	GateKnown bool
	// GateStageID is the parked plan/implement awaiting_approval stage (the
	// earliest by sequence), or nil when none is parked. Meaningful only when
	// GateKnown. A nil stage on a decision keys it at GateRunLevel.
	GateStageID *uuid.UUID
	// ParkedGateStageIDs lists every plan, implement or review stage in
	// awaiting_approval in the same stage read. Meaningful only when
	// GateKnown. A recorded decision clears only when this read positively
	// shows its gate no longer parked: a stage key clears when its stage is
	// absent here, a run-level key only when the list is empty.
	ParkedGateStageIDs []uuid.UUID
}

// decisionKeyHeldCtxKey carries the driver's hint to the GateActor of whether
// it may hold a decision key for the run being driven.
type decisionKeyHeldCtxKey struct{}

// WithDecisionKeyHeld returns ctx carrying the driver's hint for the run being
// driven: held=false means the driver hydrated the run and holds no decision
// key, so an adapter may skip its post-evaluation stage read for a
// non-decision outcome (nothing could be cleared). driveGate sets it on every
// actor call.
func WithDecisionKeyHeld(ctx context.Context, held bool) context.Context {
	return context.WithValue(ctx, decisionKeyHeldCtxKey{}, held)
}

// DecisionKeyHeld reads the hint WithDecisionKeyHeld set. ok=false when no
// hint is present; an adapter must then read the stages (the safe default).
func DecisionKeyHeld(ctx context.Context) (held, ok bool) {
	held, ok = ctx.Value(decisionKeyHeldCtxKey{}).(bool)
	return held, ok
}

// GateActor auto-acts on a single running run's gate under the operator_agent
// contract (E25.6 / ADR-047). The single seam between the campaign driver and
// the gate-action machinery: the serve.go adapter satisfies it over
// server.Server.DriveCampaignRunGate (binding the campaign operator identity + the
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

	// refusedMu guards refused: the (campaign, reason) pairs whose
	// campaign_auto_start_refused entry was recorded (#4525). A key is set only
	// after the append succeeds, cleared when a later start for the campaign
	// succeeds, and pruned for every campaign no longer listed running, so the
	// map is bounded by the running-campaign set. In-memory per process: a
	// restart records each standing refusal once more.
	refusedMu sync.Mutex
	refused   map[uuid.UUID]map[string]struct{}

	// decisionMu guards decisions and decisionsHydrated (E88.4 / #4528).
	// decisions is the last recorded, uncleared decision key per run;
	// decisionsHydrated marks the runs whose key was loaded from the run chain
	// this process. Both are pruned to the runs driven in the latest tick
	// (forgetUndrivenDecisions); a forgotten run re-hydrates when driven again.
	decisionMu        sync.Mutex
	decisions         map[uuid.UUID]decisionKey
	decisionsHydrated map[uuid.UUID]struct{}
}

// decisionKey is one decision occurrence for a run (R3): the run is the map
// key, and Gate is the parked stage id or GateRunLevel. It deliberately omits
// the stage's updated_at, so a stage that re-parks with the same decision and
// no non-decision tick in between is the same occurrence.
type decisionKey struct {
	DecisionState string
	Action        string
	Gate          string
}

// stageID returns the gate's stage id, or nil for a run-level gate.
func (k decisionKey) stageID() *uuid.UUID {
	if k.Gate == GateRunLevel {
		return nil
	}
	id, err := uuid.Parse(k.Gate)
	if err != nil {
		return nil
	}
	return &id
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
	t.pruneStartRefusals(campaigns)
	// driven collects the runs handed to the GateActor this tick, so the
	// decision keys of every other run (settled, unlinked, or no longer in a
	// running campaign) are dropped at the end of the tick.
	driven := map[uuid.UUID]struct{}{}
	for _, c := range campaigns {
		if c.State != campaign.StateRunning {
			// Defense-in-depth: the filter already constrains to running. A
			// fake or future listing returning a non-running campaign can't
			// reach the advance/start passes.
			continue
		}
		t.processCampaign(ctx, logger, c, driven)
	}
	t.forgetUndrivenDecisions(driven)
}

// processCampaign runs the ADVANCE then START passes for one running
// campaign, re-reading items between them so a predecessor settled this tick
// unblocks its dependent in the same tick. Per-item errors WARN-log and never
// abort the campaign.
func (t *Ticker) processCampaign(ctx context.Context, logger *slog.Logger, c *campaign.Campaign, driven map[uuid.UUID]struct{}) {
	items, err := t.Campaigns.ListCampaignItemsForCampaign(ctx, c.ID)
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "campaigndriver: list items failed",
			slog.String("campaign_id", c.ID.String()),
			slog.String("error", err.Error()))
		return
	}

	settledAny := t.advance(ctx, logger, c, items, driven)

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
func (t *Ticker) advance(ctx context.Context, logger *slog.Logger, c *campaign.Campaign, items []*campaign.Item, driven map[uuid.UUID]struct{}) bool {
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
			if t.GateActor != nil {
				driven[runRow.ID] = struct{}{}
			}
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
		t.emit(ctx, logger, c.ID, categoryCampaignIssueSettled, map[string]any{
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
	actx := WithDecisionKeyHeld(ctx, t.decisionKeyMayBeHeld(runRow.ID))
	if ca, ok := t.GateActor.(CampaignGateActor); ok {
		out, err = ca.DriveRunGateWithCampaign(actx, runRow, c.OperatorAgent)
	} else {
		out, err = t.GateActor.DriveRunGate(actx, runRow)
	}
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "campaigndriver: auto-drive run gate failed; left for next tick",
			slog.String("campaign_id", c.ID.String()),
			slog.String("item_id", it.ID.String()),
			slog.String("run_id", runRow.ID.String()),
			slog.String("error", err.Error()))
		return
	}
	// Record or clear the decision-required marker (E88.4 / #4528). After the
	// error return, so an actor error never appends and never clears.
	t.trackDecision(ctx, logger, c, it, runRow, out)
	if out.Acted {
		t.emit(ctx, logger, c.ID, categoryCampaignGateActed, map[string]any{
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

	t.emit(ctx, logger, c.ID, categoryCampaignPaused, map[string]any{
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

// trackDecision records the decision-required marker for a DecisionRequired
// outcome, once per occurrence key, and the cleared row when a later
// non-decision outcome positively shows the recorded gate no longer parked
// (E88.4 / #4528, R3, approval condition C1). Every step that cannot decide
// defers: nothing is appended or cleared, and the next tick retries.
func (t *Ticker) trackDecision(ctx context.Context, logger *slog.Logger, c *campaign.Campaign, it *campaign.Item, runRow *run.Run, out GateActionOutcome) {
	if !t.decisionHydrated(ctx, logger, runRow.ID) {
		return
	}
	if !out.DecisionRequired {
		t.clearDecision(ctx, logger, c, it, runRow, out)
		return
	}
	if !out.GateKnown {
		// The gate stage is unknown (stage read failed or unwired): defer the
		// occurrence rather than re-key it to the run level.
		logger.LogAttrs(ctx, slog.LevelWarn, "campaigndriver: decision gate unknown; marker deferred",
			slog.String("campaign_id", c.ID.String()),
			slog.String("run_id", runRow.ID.String()),
			slog.String("decision_state", out.DecisionState))
		return
	}
	gate := GateRunLevel
	if out.GateStageID != nil {
		gate = out.GateStageID.String()
	}
	key := decisionKey{DecisionState: out.DecisionState, Action: out.Action, Gate: gate}
	if t.decisionUnchanged(runRow.ID, key) {
		return
	}
	t.recordDecision(ctx, logger, c, it, runRow, key)
}

// recordDecision appends one campaign_gate_decision_required row on the run
// chain and stores the key only after the append succeeds, so a failed append
// is retried on the next tick.
func (t *Ticker) recordDecision(ctx context.Context, logger *slog.Logger, c *campaign.Campaign, it *campaign.Item, runRow *run.Run, key decisionKey) {
	payload := decisionPayload(c, it, runRow, key)
	if _, err := t.appendRunChained(ctx, runRow.ID, key.stageID(), CategoryCampaignGateDecisionRequired, payload); err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "campaigndriver: decision marker append failed",
			slog.String("campaign_id", c.ID.String()),
			slog.String("run_id", runRow.ID.String()),
			slog.String("error", err.Error()))
		return
	}
	t.storeDecision(runRow.ID, key)
}

// clearDecision appends one campaign_gate_decision_cleared row for the run's
// stored key when the outcome's own stage read positively shows the keyed gate
// no longer parked (C1), and deletes the key only after the append succeeds.
// A no-op without a stored key. An indeterminate outcome (GateKnown=false)
// never clears: AutoDriveRunGate answers observe-only with a nil error on its
// fail-closed reads, which is not evidence the gate moved.
func (t *Ticker) clearDecision(ctx context.Context, logger *slog.Logger, c *campaign.Campaign, it *campaign.Item, runRow *run.Run, out GateActionOutcome) {
	key, ok := t.storedDecision(runRow.ID)
	if !ok {
		return
	}
	if !gateReleased(key, out) {
		return
	}
	clearedBy := clearedByNoDecision
	switch {
	case out.Acted:
		clearedBy = clearedByActed
	case out.Paged:
		clearedBy = clearedByPaged
	}
	payload := decisionPayload(c, it, runRow, key)
	payload["cleared_by"] = clearedBy
	if _, err := t.appendRunChained(ctx, runRow.ID, key.stageID(), CategoryCampaignGateDecisionCleared, payload); err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "campaigndriver: decision clear append failed",
			slog.String("campaign_id", c.ID.String()),
			slog.String("run_id", runRow.ID.String()),
			slog.String("error", err.Error()))
		return
	}
	t.deleteDecision(runRow.ID, key)
}

// gateReleased reports whether out's stage read positively shows key's gate no
// longer parked (C1). A stage key is released when its stage is absent from
// the parked set (it left awaiting_approval); a run-level key only when no
// plan, implement or review stage is parked.
func gateReleased(key decisionKey, out GateActionOutcome) bool {
	if !out.GateKnown {
		return false
	}
	if key.Gate == GateRunLevel {
		return len(out.ParkedGateStageIDs) == 0
	}
	for _, id := range out.ParkedGateStageIDs {
		if id.String() == key.Gate {
			return false
		}
	}
	return true
}

// decisionPayload is the exact marker payload: this run's own campaign_id,
// issue_ref, run_id, gate and closed-set tokens. out.Note is never included.
func decisionPayload(c *campaign.Campaign, it *campaign.Item, runRow *run.Run, key decisionKey) map[string]any {
	return map[string]any{
		"campaign_id":    c.ID.String(),
		"issue_ref":      it.IssueRef,
		"run_id":         runRow.ID.String(),
		"gate":           key.Gate,
		"decision_state": key.DecisionState,
		"action":         key.Action,
	}
}

// appendRunChained appends one system-actor entry on runID's chain.
func (t *Ticker) appendRunChained(ctx context.Context, runID uuid.UUID, stageID *uuid.UUID, category string, payload map[string]any) (*audit.Entry, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	systemKind := audit.ActorSystem
	return t.Audit.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   stageID,
		Timestamp: t.now(),
		Category:  category,
		ActorKind: &systemKind,
		Payload:   body,
	})
}

// decisionHydrated loads the run's current decision key from its chain once
// per run per process: when the latest decision_required row is newer
// (higher Sequence) than the latest decision_cleared row, its payload seeds
// the key. Reports false on a read error (WARN-logged), leaving the run
// unhydrated so this tick neither appends nor clears and the next tick
// retries.
func (t *Ticker) decisionHydrated(ctx context.Context, logger *slog.Logger, runID uuid.UUID) bool {
	t.decisionMu.Lock()
	_, done := t.decisionsHydrated[runID]
	t.decisionMu.Unlock()
	if done {
		return true
	}
	marker, cleared, err := t.latestDecisionRows(ctx, runID)
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "campaigndriver: decision chain read failed; marker tracking deferred",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
		return false
	}
	var (
		key  decisionKey
		seed bool
	)
	if marker != nil && (cleared == nil || marker.Sequence > cleared.Sequence) {
		var p struct {
			Gate          string `json:"gate"`
			DecisionState string `json:"decision_state"`
			Action        string `json:"action"`
		}
		if uerr := json.Unmarshal(marker.Payload, &p); uerr != nil {
			// A malformed marker seeds no key: the next decision records a fresh
			// marker that supersedes it.
			logger.LogAttrs(ctx, slog.LevelWarn, "campaigndriver: decision marker payload unreadable; not seeded",
				slog.String("run_id", runID.String()),
				slog.String("error", uerr.Error()))
		} else {
			key = decisionKey{DecisionState: p.DecisionState, Action: p.Action, Gate: p.Gate}
			seed = true
		}
	}
	t.decisionMu.Lock()
	defer t.decisionMu.Unlock()
	if t.decisionsHydrated == nil {
		t.decisionsHydrated = map[uuid.UUID]struct{}{}
	}
	t.decisionsHydrated[runID] = struct{}{}
	if seed {
		if t.decisions == nil {
			t.decisions = map[uuid.UUID]decisionKey{}
		}
		t.decisions[runID] = key
	}
	return true
}

// latestDecisionRows returns the highest-Sequence decision_required and
// decision_cleared rows on runID's chain (nil when none).
func (t *Ticker) latestDecisionRows(ctx context.Context, runID uuid.UUID) (marker, cleared *audit.Entry, err error) {
	markers, err := t.Audit.ListForRunByCategory(ctx, runID, CategoryCampaignGateDecisionRequired)
	if err != nil {
		return nil, nil, err
	}
	clears, err := t.Audit.ListForRunByCategory(ctx, runID, CategoryCampaignGateDecisionCleared)
	if err != nil {
		return nil, nil, err
	}
	return latestEntry(markers), latestEntry(clears), nil
}

// latestEntry returns the highest-Sequence entry, or nil.
func latestEntry(entries []*audit.Entry) *audit.Entry {
	var latest *audit.Entry
	for _, e := range entries {
		if e != nil && (latest == nil || e.Sequence > latest.Sequence) {
			latest = e
		}
	}
	return latest
}

// decisionKeyMayBeHeld reports the hint driveGate passes the actor: false only
// when the run is hydrated and no key is stored (nothing could be cleared).
func (t *Ticker) decisionKeyMayBeHeld(runID uuid.UUID) bool {
	t.decisionMu.Lock()
	defer t.decisionMu.Unlock()
	if _, ok := t.decisionsHydrated[runID]; !ok {
		return true
	}
	_, held := t.decisions[runID]
	return held
}

// decisionUnchanged reports whether key equals the run's stored key.
func (t *Ticker) decisionUnchanged(runID uuid.UUID, key decisionKey) bool {
	t.decisionMu.Lock()
	defer t.decisionMu.Unlock()
	stored, ok := t.decisions[runID]
	return ok && stored == key
}

// storedDecision returns the run's stored key.
func (t *Ticker) storedDecision(runID uuid.UUID) (decisionKey, bool) {
	t.decisionMu.Lock()
	defer t.decisionMu.Unlock()
	k, ok := t.decisions[runID]
	return k, ok
}

// storeDecision records key as the run's current decision.
func (t *Ticker) storeDecision(runID uuid.UUID, key decisionKey) {
	t.decisionMu.Lock()
	defer t.decisionMu.Unlock()
	if t.decisions == nil {
		t.decisions = map[uuid.UUID]decisionKey{}
	}
	t.decisions[runID] = key
}

// deleteDecision forgets the run's key when it still equals key.
func (t *Ticker) deleteDecision(runID uuid.UUID, key decisionKey) {
	t.decisionMu.Lock()
	defer t.decisionMu.Unlock()
	if t.decisions[runID] == key {
		delete(t.decisions, runID)
	}
}

// forgetUndrivenDecisions drops the stored key and hydration flag of every run
// not driven this tick (a run that settled terminal, was unlinked, or whose
// campaign left running). No cleared row is written: the status read ignores
// terminal runs, and a run driven again re-hydrates from its chain.
func (t *Ticker) forgetUndrivenDecisions(driven map[uuid.UUID]struct{}) {
	t.decisionMu.Lock()
	defer t.decisionMu.Unlock()
	for id := range t.decisions {
		if _, ok := driven[id]; !ok {
			delete(t.decisions, id)
		}
	}
	for id := range t.decisionsHydrated {
		if _, ok := driven[id]; !ok {
			delete(t.decisionsHydrated, id)
		}
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
	t.emit(ctx, logger, c.ID, categoryCampaignAdvanced, map[string]any{
		"campaign_id": c.ID.String(),
		"from":        string(c.State),
		"to":          string(newState),
	})
}

// start dispatches eligible items up to the per-campaign concurrency budget
// (effective max minus currently-running items), starting each via the
// RunStarter seam, linking it to its item, transitioning it to running, and
// emitting a campaign_issue_started entry. Idempotent: an item that already
// has a run, or is no longer in a startable state, is skipped. A *StartRefusal
// from the starter ends the pass for the whole campaign (recordStartRefusal);
// any other starter error is per-item and the next eligible item is tried.
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
			// A campaign-level refusal (#4525) is not transient and applies to
			// every item of the campaign: record it (once per campaign+reason)
			// and end the START pass so no further item is attempted.
			var refusal *StartRefusal
			if errors.As(err, &refusal) {
				t.recordStartRefusal(ctx, logger, c, it, refusal)
				return
			}
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
		// The campaign starts again, so a later refusal is news: forget the
		// recorded ones.
		t.clearStartRefusals(c.ID)

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
		t.emit(ctx, logger, c.ID, categoryCampaignIssueStarted, map[string]any{
			"campaign_id": c.ID.String(),
			"issue_ref":   it.IssueRef,
			"run_id":      runRow.ID.String(),
		})
	}
}

// recordStartRefusal records a campaign-level StartRefusal (#4525) as one
// campaign_auto_start_refused entry per (campaign, reason). The dedup key is
// marked only after the append succeeds, so a failed append is retried on the
// next tick rather than silently lost; the WARN log fires only when recording.
func (t *Ticker) recordStartRefusal(ctx context.Context, logger *slog.Logger, c *campaign.Campaign, it *campaign.Item, refusal *StartRefusal) {
	if t.startRefusalRecorded(c.ID, refusal.Reason) {
		return
	}
	logger.LogAttrs(ctx, slog.LevelWarn, "campaigndriver: campaign auto-start refused; no item of the campaign is started",
		slog.String("campaign_id", c.ID.String()),
		slog.String("reason", refusal.Reason),
		slog.String("runner_kind", refusal.RunnerKind),
		slog.String("runner_kind_source", refusal.RunnerKindSource))
	if !t.emit(ctx, logger, c.ID, categoryCampaignAutoStartRefused, map[string]any{
		"campaign_id":        c.ID.String(),
		"issue_ref":          it.IssueRef,
		"reason":             refusal.Reason,
		"runner_kind":        refusal.RunnerKind,
		"runner_kind_source": refusal.RunnerKindSource,
		"detail":             refusal.Detail,
	}) {
		return
	}
	t.refusedMu.Lock()
	defer t.refusedMu.Unlock()
	if t.refused == nil {
		t.refused = map[uuid.UUID]map[string]struct{}{}
	}
	if t.refused[c.ID] == nil {
		t.refused[c.ID] = map[string]struct{}{}
	}
	t.refused[c.ID][refusal.Reason] = struct{}{}
}

// startRefusalRecorded reports whether (campaignID, reason) was recorded.
func (t *Ticker) startRefusalRecorded(campaignID uuid.UUID, reason string) bool {
	t.refusedMu.Lock()
	defer t.refusedMu.Unlock()
	_, ok := t.refused[campaignID][reason]
	return ok
}

// clearStartRefusals forgets every recorded refusal for campaignID.
func (t *Ticker) clearStartRefusals(campaignID uuid.UUID) {
	t.refusedMu.Lock()
	defer t.refusedMu.Unlock()
	delete(t.refused, campaignID)
}

// pruneStartRefusals forgets the recorded refusals of every campaign not in
// the running listing, so a campaign that left running and later resumes
// records a standing refusal again, and the map never outgrows the running set.
func (t *Ticker) pruneStartRefusals(running []*campaign.Campaign) {
	t.refusedMu.Lock()
	defer t.refusedMu.Unlock()
	if len(t.refused) == 0 {
		return
	}
	keep := make(map[uuid.UUID]struct{}, len(running))
	for _, c := range running {
		if c.State == campaign.StateRunning {
			keep[c.ID] = struct{}{}
		}
	}
	for id := range t.refused {
		if _, ok := keep[id]; !ok {
			delete(t.refused, id)
		}
	}
}

// emit appends one campaign-level audit entry on the global chain, stamped
// with the owning campaign's tenant account partition (ADR-057 / #1828).
// Best-effort: a marshal or append error WARN-logs and never unwinds the
// transition it records, mirroring the sweeper/reconciler audit posture. It
// reports whether the entry was appended, for a caller (recordStartRefusal)
// that must not mark an unrecorded entry as recorded.
func (t *Ticker) emit(ctx context.Context, logger *slog.Logger, campaignID uuid.UUID, category string, payload map[string]any) bool {
	body, err := json.Marshal(payload)
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "campaigndriver: marshal audit payload failed",
			slog.String("category", category),
			slog.String("error", err.Error()))
		return false
	}
	systemKind := audit.ActorSystem
	if _, err := t.Audit.AppendGlobalChained(ctx, audit.GlobalChainAppendParams{
		Timestamp: t.now(),
		Category:  category,
		ActorKind: &systemKind,
		Payload:   body,
		AccountID: t.campaignAccountID(ctx, logger, campaignID),
	}); err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "campaigndriver: append audit entry failed",
			slog.String("category", category),
			slog.String("error", err.Error()))
		return false
	}
	return true
}

// campaignAccountID resolves the campaign's owning tenant account — the
// run-less audit chain partition its driver-emitted entries chain within
// (ADR-057 / #1828) — via the OPTIONAL campaign.AccountGetter capability on
// the bound store (the production postgres repo carries it; the CampaignStore
// interface deliberately doesn't, mirroring how the server type-asserts the
// same capability). Best-effort like the emit it feeds: a store without the
// capability, a not-found row (the campaign.BaseFake stub posture), an
// untenanted ("") row, or an unparsable value degrade silently to nil — the
// untenanted NULL partition — and any other lookup error degrades to nil
// with a WARN log rather than suppressing the audit entry.
func (t *Ticker) campaignAccountID(ctx context.Context, logger *slog.Logger, campaignID uuid.UUID) *uuid.UUID {
	getter, ok := t.Campaigns.(campaign.AccountGetter)
	if !ok {
		return nil
	}
	acct, err := getter.GetCampaignAccountID(ctx, campaignID)
	if errors.Is(err, campaign.ErrNotFound) {
		return nil
	}
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "campaigndriver: resolve campaign account failed; stamping audit entry untenanted",
			slog.String("campaign_id", campaignID.String()),
			slog.String("error", err.Error()))
		return nil
	}
	if acct == "" {
		return nil
	}
	u, err := uuid.Parse(acct)
	if err != nil {
		return nil
	}
	return &u
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
