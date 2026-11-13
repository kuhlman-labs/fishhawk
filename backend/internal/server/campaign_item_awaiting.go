package server

// campaign_item_awaiting.go is the per-item awaiting block on GET
// /v0/campaigns/{campaign_id}/status (E88.4 / #4528): for every running
// campaign item with a non-terminal linked run, what the item is waiting on.
//
// Two layers, deliberately separate (R5):
//
//   - The STAGE-STATE classification (kind, reason, run_id, stage_*,
//     next_step) is derived from the linked run's own stage rows alone and is
//     the same for every caller. It is intentionally UNGATED: the item's run_id
//     is already in the item payload, and the stage state adds nothing a
//     caller who can read the campaign could not read off that run.
//   - The DECISION SOURCES (the campaign driver's decision marker on the run
//     chain and the campaign's pending scope amendments) are gated by
//     campaignAmendmentsReadable, the predicate the pending_amendments block
//     uses. An unreadable caller gets decision_withheld on every block and the
//     unchanged stage-state classification; no marker or amendment read
//     happens for it, so the flag depends only on the caller and never reveals
//     whether a decision exists.
//
// The decision marker pair (campaign_gate_decision_required /
// campaign_gate_decision_cleared) is written by the campaign driver
// (backend/internal/campaigndriver) as an INTERIM registration under ADR-106's
// existing audit-category mechanism; it needs no new ADR and takes the
// mandatory run-bound flag via #4599 / E89 #4537. Its payload carries no
// cross-run identifiers. The driver's once-per-occurrence dedupe assumes a
// single campaign driver per deployment.

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaigndriver"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Awaiting kinds: who must act next.
const (
	awaitingKindHostDispatcher = "host_dispatcher"
	awaitingKindHuman          = "human"
	awaitingKindSystem         = "system"
)

// Awaiting reasons. host_dispatch pairs with host_dispatcher; in_flight,
// children and no_active_stage with system; the rest with human.
const (
	awaitingReasonHostDispatch          = "host_dispatch"
	awaitingReasonOperatorDispatch      = "operator_dispatch"
	awaitingReasonGateApproval          = "gate_approval"
	awaitingReasonDeployApproval        = "deploy_approval"
	awaitingReasonInput                 = "input"
	awaitingReasonScopeDecision         = "scope_decision"
	awaitingReasonScopeAmendmentPending = "scope_amendment_pending"
	awaitingReasonRunDecision           = "run_decision"
	awaitingReasonInFlight              = "in_flight"
	awaitingReasonChildren              = "children"
	awaitingReasonNoActiveStage         = "no_active_stage"
)

// awaitingNextSteps is the server-constant next_step text per reason.
var awaitingNextSteps = map[string]string{
	awaitingReasonHostDispatch:          "The host dispatcher spawns this stage on its next tick; no operator action is needed.",
	awaitingReasonOperatorDispatch:      "Dispatch the stage with fishhawk_dispatch_stage; this run is not host-dispatched.",
	awaitingReasonGateApproval:          "The gate needs a human decision; read fishhawk_get_run_status next_actions for the verb.",
	awaitingReasonDeployApproval:        "Approve or reject the deploy with fishhawk_approve_deploy or fishhawk_reject_deploy.",
	awaitingReasonInput:                 "The stage asked a clarification; answer it with fishhawk_answer_clarification.",
	awaitingReasonScopeDecision:         "Decide the scope-completeness park with fishhawk_decide_scope_completeness.",
	awaitingReasonScopeAmendmentPending: "Decide the pending scope amendment with fishhawk_decide_scope_amendment; it is listed under pending_amendments.",
	awaitingReasonRunDecision:           "The run needs a human decision; read fishhawk_get_run_status next_actions for the verb.",
	awaitingReasonInFlight:              "The stage is executing; no action is needed until it settles.",
	awaitingReasonChildren:              "The stage waits on its child runs; drive them with fishhawk_run_children or fishhawk_await_children.",
	awaitingReasonNoActiveStage:         "No stage is active; read fishhawk_get_run_status next_actions.",
}

// campaignItemAwaitingPayload mirrors docs/api/v0.openapi.yaml's
// `CampaignItemAwaiting` schema. stage_* are omitted for no_active_stage.
// decision_withheld and decision_unavailable are omitempty: absent means
// false.
type campaignItemAwaitingPayload struct {
	Kind                string                        `json:"kind"`
	Reason              string                        `json:"reason"`
	RunID               string                        `json:"run_id"`
	StageID             string                        `json:"stage_id,omitempty"`
	StageType           string                        `json:"stage_type,omitempty"`
	StageState          string                        `json:"stage_state,omitempty"`
	NextStep            string                        `json:"next_step"`
	Decision            *campaignItemAwaitingDecision `json:"decision,omitempty"`
	DecisionWithheld    bool                          `json:"decision_withheld,omitempty"`
	DecisionUnavailable bool                          `json:"decision_unavailable,omitempty"`
}

// campaignItemAwaitingDecision is the current (uncleared) decision marker the
// campaign driver recorded for the item's run. Gate is a stage id or
// campaigndriver.GateRunLevel.
type campaignItemAwaitingDecision struct {
	DecisionState string    `json:"decision_state"`
	Action        string    `json:"action"`
	Gate          string    `json:"gate"`
	RecordedAt    time.Time `json:"recorded_at"`
}

// awaitingMarker is a decoded decision marker row.
type awaitingMarker struct {
	Sequence   int64
	Gate       string
	State      string
	Action     string
	RecordedAt time.Time
}

// classifyCampaignItemAwaiting is the pure stage-state classifier. It takes
// the earliest stage by sequence whose state is not terminal; with none, the
// non-terminal run is system/no_active_stage with no stage fields.
func classifyCampaignItemAwaiting(r *run.Run, stages []*run.Stage) campaignItemAwaitingPayload {
	ordered := append([]*run.Stage(nil), stages...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Sequence < ordered[j].Sequence })
	var active *run.Stage
	for _, st := range ordered {
		if st != nil && !st.State.IsTerminal() {
			active = st
			break
		}
	}
	blk := campaignItemAwaitingPayload{RunID: r.ID.String()}
	if active == nil {
		setAwaitingReason(&blk, awaitingKindSystem, awaitingReasonNoActiveStage)
		return blk
	}
	blk.StageID = active.ID.String()
	blk.StageType = string(active.Type)
	blk.StageState = string(active.State)
	switch active.State {
	case run.StageStatePending, run.StageStateAwaitingHostDispatch:
		kind, reason := hostDispatchKind(r)
		setAwaitingReason(&blk, kind, reason)
	case run.StageStateAwaitingApproval:
		// Whatever the stage type: a parked review stage reads
		// human/gate_approval with stage_type review.
		setAwaitingReason(&blk, awaitingKindHuman, awaitingReasonGateApproval)
	case run.StageStateAwaitingDeployApproval:
		setAwaitingReason(&blk, awaitingKindHuman, awaitingReasonDeployApproval)
	case run.StageStateAwaitingInput:
		setAwaitingReason(&blk, awaitingKindHuman, awaitingReasonInput)
	case run.StageStateAwaitingScopeDecision:
		setAwaitingReason(&blk, awaitingKindHuman, awaitingReasonScopeDecision)
	case run.StageStateAwaitingChildren:
		setAwaitingReason(&blk, awaitingKindSystem, awaitingReasonChildren)
	default:
		// dispatched, running, awaiting_deployment.
		setAwaitingReason(&blk, awaitingKindSystem, awaitingReasonInFlight)
	}
	return blk
}

// hostDispatchKind classifies a pending / awaiting_host_dispatch stage: the
// host dispatcher spawns it only for an auto_dispatch-marked run whose runner
// is local (ADR-104 Q3); any other run waits on an operator dispatch.
func hostDispatchKind(r *run.Run) (string, string) {
	if !r.AutoDispatch {
		return awaitingKindHuman, awaitingReasonOperatorDispatch
	}
	if r.RunnerKind != run.RunnerKindLocal {
		return awaitingKindHuman, awaitingReasonOperatorDispatch
	}
	return awaitingKindHostDispatcher, awaitingReasonHostDispatch
}

func setAwaitingReason(blk *campaignItemAwaitingPayload, kind, reason string) {
	blk.Kind = kind
	blk.Reason = reason
	blk.NextStep = awaitingNextSteps[reason]
}

// overlayCampaignItemAwaiting applies the gated decision sources to a
// classified block. An unreadable caller gets decision_withheld and the
// unchanged classification, and loadMarker is never called for it. A
// readable caller gets (a) the live-amendment overlay from the handler's
// already-gated pending_amendments block, then (b) the current decision
// marker, which loadMarker reads from the item's own run chain.
func overlayCampaignItemAwaiting(blk campaignItemAwaitingPayload, readable bool, pending *campaignPendingAmendmentsPayload, loadMarker func() (*awaitingMarker, error)) campaignItemAwaitingPayload {
	if !readable {
		blk.DecisionWithheld = true
		return blk
	}
	// (a) Amendments. A mid-implement amendment leaves the stage running, so
	// stage state alone cannot express it; a decomposed parent at
	// awaiting_children whose child filed one is parked on it too (C3).
	if live, known := liveAmendmentForRun(pending, blk.RunID); !known {
		blk.DecisionUnavailable = true
	} else if live && blk.Kind == awaitingKindSystem &&
		(blk.Reason == awaitingReasonInFlight || blk.Reason == awaitingReasonChildren) {
		setAwaitingReason(&blk, awaitingKindHuman, awaitingReasonScopeAmendmentPending)
	}
	// (b) The current decision marker.
	m, err := loadMarker()
	if err != nil {
		blk.DecisionUnavailable = true
		return blk
	}
	if m == nil {
		return blk
	}
	if m.Gate != campaigndriver.GateRunLevel && m.Gate != blk.StageID {
		// A foreign stage's marker: omitted.
		return blk
	}
	if m.Gate != campaigndriver.GateRunLevel && blk.StageState != string(run.StageStateAwaitingApproval) {
		// The keyed stage left awaiting_approval (a fix-up reopens it to
		// pending, transition.go) before the driver cleared the marker: the
		// decision is no longer current (C2).
		return blk
	}
	if m.Gate == campaigndriver.GateRunLevel && blk.Kind != awaitingKindHuman {
		// A run-level decision parks the run whatever its stage state (R4).
		setAwaitingReason(&blk, awaitingKindHuman, awaitingReasonRunDecision)
	}
	blk.Decision = &campaignItemAwaitingDecision{
		DecisionState: m.State,
		Action:        m.Action,
		Gate:          m.Gate,
		RecordedAt:    m.RecordedAt,
	}
	return blk
}

// liveAmendmentForRun reports whether pending lists a live amendment for the
// item run runID. known is false when the block is nil (unwired or the read
// failed), or truncated without a live entry for this run: absence is then not
// evidence.
func liveAmendmentForRun(pending *campaignPendingAmendmentsPayload, runID string) (live, known bool) {
	if pending == nil {
		return false, false
	}
	for _, p := range pending.Items {
		if p.ItemRunID == runID && p.DecisionLive {
			return true, true
		}
	}
	if pending.Truncated {
		return false, false
	}
	return false, true
}

// currentAwaitingMarker picks the current marker: the latest
// decision_required row with no later decision_cleared row. Nil when there is
// none, or the latest is unreadable (it is then not shown).
func currentAwaitingMarker(markers, clears []*audit.Entry) *awaitingMarker {
	marker := latestAuditEntry(markers)
	if marker == nil {
		return nil
	}
	cleared := latestAuditEntry(clears)
	if cleared != nil && cleared.Sequence > marker.Sequence {
		return nil
	}
	var p struct {
		Gate          string `json:"gate"`
		DecisionState string `json:"decision_state"`
		Action        string `json:"action"`
	}
	if err := json.Unmarshal(marker.Payload, &p); err != nil || p.Gate == "" {
		return nil
	}
	return &awaitingMarker{
		Sequence: marker.Sequence, Gate: p.Gate, State: p.DecisionState,
		Action: p.Action, RecordedAt: marker.Timestamp,
	}
}

func latestAuditEntry(entries []*audit.Entry) *audit.Entry {
	var latest *audit.Entry
	for _, e := range entries {
		if e != nil && (latest == nil || e.Sequence > latest.Sequence) {
			latest = e
		}
	}
	return latest
}

// campaignItemAwaitingForStatus builds one item's awaiting block, or nil when
// the item is not running, has no linked run, its run is terminal, or a run
// or stage read failed (WARN-logged; the status read never fails on it).
func (s *Server) campaignItemAwaitingForStatus(ctx context.Context, c *campaign.Campaign, it *campaign.Item, readable bool, pending *campaignPendingAmendmentsPayload) *campaignItemAwaitingPayload {
	if s.cfg.RunRepo == nil || it.State != campaign.ItemStateRunning || it.RunID == nil {
		return nil
	}
	runID := *it.RunID
	r, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.Warn("campaign status: awaiting run read failed; omitting awaiting",
			"campaign_id", c.ID.String(), "run_id", runID.String(), "error", err.Error())
		return nil
	}
	if r.State.IsTerminal() {
		return nil
	}
	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.Warn("campaign status: awaiting stage read failed; omitting awaiting",
			"campaign_id", c.ID.String(), "run_id", runID.String(), "error", err.Error())
		return nil
	}
	blk := overlayCampaignItemAwaiting(classifyCampaignItemAwaiting(r, stages), readable, pending, func() (*awaitingMarker, error) {
		return s.awaitingMarkerForRun(ctx, c, runID)
	})
	return &blk
}

// awaitingMarkerForRun reads the decision marker rows of runID. runID is
// ALWAYS the item's own linked run, from the campaign whose repo visibility
// the handler already enforced: the read never reaches another run's chain.
func (s *Server) awaitingMarkerForRun(ctx context.Context, c *campaign.Campaign, runID uuid.UUID) (*awaitingMarker, error) {
	if s.cfg.AuditRepo == nil {
		return nil, errAwaitingAuditUnwired
	}
	markers, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, campaigndriver.CategoryCampaignGateDecisionRequired)
	if err == nil {
		var clears []*audit.Entry
		clears, err = s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, campaigndriver.CategoryCampaignGateDecisionCleared)
		if err == nil {
			return currentAwaitingMarker(markers, clears), nil
		}
	}
	s.cfg.Logger.Warn("campaign status: decision marker read failed; awaiting decision unavailable",
		"campaign_id", c.ID.String(), "run_id", runID.String(), "error", err.Error())
	return nil, err
}

// errAwaitingAuditUnwired marks a status read with no audit repository: the
// decision source is unavailable, not absent.
var errAwaitingAuditUnwired = errors.New("audit repository unwired")
