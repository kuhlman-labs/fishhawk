package server

// campaign_item_host_dispatcher.go classifies a host_dispatcher awaiting block
// from the dispatchers' server heartbeats (#5489, ADR-104 Amendment A2).
//
// The status read loads the heartbeat rows visible to the item run's account
// once per status call (hostDispatcherHeartbeatLoader) and hands them to the
// pure classifyHostDispatcherHeartbeat. Per #5402 ruling R1 the verdict is
// dispatcher-wide first: health (paused, a tick failure) and the daily cap
// apply to every host_dispatcher item, and the per-run lookup reads the
// heartbeat's run_refusals entry, never a run row.
//
// The reasons reuse the MCP status overlay's vocabulary byte-for-byte
// (backend/internal/mcpserver/host_dispatch_overlay.go hostDispatchReason*),
// so a tool keyed on reason sees no new vocabulary. Every next_step is
// server-constant (hostDispatcherNextSteps); no dispatcher-authored text ever
// reaches it. The dispatcher's refusal detail is carried, already bounded at
// ingest, only in the structured host_dispatcher observation.

import (
	"context"
	"sync"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/dispatcherheartbeat"
	"github.com/kuhlman-labs/fishhawk/backend/internal/hostdispatcher"
)

// Heartbeat-classified awaiting reasons, byte-identical to the MCP overlay's.
// Each pairs with kind human.
const (
	awaitingReasonHostDispatchPaused        = "host_dispatch_paused"
	awaitingReasonHostDispatchCapped        = "host_dispatch_capped"
	awaitingReasonHostDispatchRefused       = "host_dispatch_refused"
	awaitingReasonHostDispatchBudgetBlocked = "host_dispatch_budget_blocked"
	awaitingReasonHostDispatcherAbsent      = "host_dispatcher_absent"
)

// host_dispatcher.state values, the MCP overlay's hostDispatchState* set
// (unverified excepted: the server never reads status.json).
const (
	hostDispatcherStateLive     = "live"
	hostDispatcherStateWaiting  = "waiting"
	hostDispatcherStateInFlight = "in_flight"
	hostDispatcherStateHeld     = "held"
	hostDispatcherStateRefusing = "refusing"
	hostDispatcherStateCapped   = "capped"
	hostDispatcherStatePaused   = "paused"
	hostDispatcherStateStale    = "stale"
	hostDispatcherStateAbsent   = "absent"
)

// hostDispatcherHeartbeatListLimit bounds the one List read per account per
// status call.
const hostDispatcherHeartbeatListLimit = 32

// hostDispatcherObservation is the structured host_dispatcher object on an
// awaiting block, mirroring docs/api/v0.openapi.yaml's
// `CampaignItemAwaitingHostDispatcher` schema. Every field but state is
// omitted when empty or zero.
type hostDispatcherObservation struct {
	State           string    `json:"state"`
	Host            string    `json:"host,omitempty"`
	LastHeartbeatAt time.Time `json:"last_heartbeat_at,omitzero"`
	RefusalCode     string    `json:"refusal_code,omitempty"`
	RefusalDetail   string    `json:"refusal_detail,omitempty"`
	RefusalFirstAt  time.Time `json:"refusal_first_at,omitzero"`
	TickFailureCode string    `json:"tick_failure_code,omitempty"`
	DailyCap        int       `json:"daily_cap,omitempty"`
	SpawnsToday     int       `json:"spawns_today,omitempty"`
	CapResetsAt     time.Time `json:"cap_resets_at,omitzero"`
	BudgetSource    string    `json:"budget_source,omitempty"`
}

// hostDispatcherVerdict is the classifier's result: the block's kind, reason
// and next_step plus the observation.
type hostDispatcherVerdict struct {
	Kind        string
	Reason      string
	NextStep    string
	Observation hostDispatcherObservation
}

// hostDispatcherTextKey keys a state-dependent next_step. BudgetSource is set
// only for the live_spec budget text; every other budget source shares the
// empty key (Captain ruling D4 on #5473).
type hostDispatcherTextKey struct {
	Reason       string
	State        string
	BudgetSource string
}

// hostDispatcherNextSteps is the server-constant next_step per (reason,
// state, budget_source). Each names its remedy and points at the
// host_dispatcher fields.
var hostDispatcherNextSteps = map[hostDispatcherTextKey]string{
	{awaitingReasonHostDispatchPaused, hostDispatcherStatePaused, ""}:                                       "The host dispatcher named in host_dispatcher.host is paused and spawns nothing. Resume it on that host with `fishhawk-mcp host-dispatch --resume --state-dir <dir>`, or dispatch the stage by hand with fishhawk_dispatch_stage.",
	{awaitingReasonHostDispatchCapped, hostDispatcherStateCapped, ""}:                                       "The host dispatcher reached its daily spawn cap (host_dispatcher.daily_cap) and resumes at host_dispatcher.cap_resets_at. Wait for the reset, raise the cap on that host, or dispatch the stage by hand with fishhawk_dispatch_stage.",
	{awaitingReasonHostDispatchRefused, hostDispatcherStateHeld, ""}:                                        "The host dispatcher holds this run (host_dispatcher.refusal_code): the campaign or run is held or not running. Clear the hold or resume the campaign; the dispatcher spawns on its next tick after that.",
	{awaitingReasonHostDispatchRefused, hostDispatcherStateRefusing, ""}:                                    "The host dispatcher refuses to spawn this stage (host_dispatcher.refusal_code, or tick_failure_code for a failing tick). Fix the named cause on that host, read `fishhawk-mcp host-dispatch --status --state-dir <dir>` there, or dispatch the stage by hand with fishhawk_dispatch_stage.",
	{awaitingReasonHostDispatchBudgetBlocked, hostDispatcherStateHeld, hostdispatcher.BudgetSourceLiveSpec}: "The host dispatcher refuses this run on its live workflow budget (host_dispatcher.budget_source live_spec). Raise limit_usd or set `enforcement: advisory` on the default branch and the dispatcher spawns on its next tick. A hand fishhawk_dispatch_stage is not budget-gated.",
	{awaitingReasonHostDispatchBudgetBlocked, hostDispatcherStateHeld, ""}:                                  "The host dispatcher refuses this run on the budget it was admitted under (host_dispatcher.budget_source), which stays fixed for this run. Wait for the budget period to roll over, or start a new run with budget_override. A hand fishhawk_dispatch_stage is not budget-gated.",
	{awaitingReasonHostDispatcherAbsent, hostDispatcherStateAbsent, ""}:                                     "No host dispatcher has sent a heartbeat this run's account can see. Start `fishhawk-mcp host-dispatch` on the host (an older dispatcher binary does not publish; rebuild it), or dispatch the stage by hand with fishhawk_dispatch_stage.",
	{awaitingReasonHostDispatcherAbsent, hostDispatcherStateStale, ""}:                                      "The host dispatcher's last heartbeat (host_dispatcher.last_heartbeat_at from host_dispatcher.host) is stale: it is stopped, hung or cannot reach the server. Check `fishhawk-mcp host-dispatch --status --state-dir <dir>` on that host and restart it, or dispatch the stage by hand with fishhawk_dispatch_stage.",
}

// Ranks across fresh records, best first (#5489 step 5d).
const (
	hostDispatcherRankInFlight = iota
	hostDispatcherRankRunHold
	hostDispatcherRankLive
	hostDispatcherRankWaiting
	hostDispatcherRankHostBlock
)

// classifyHostDispatcherHeartbeat is the pure heartbeat classifier for one
// host_dispatcher item. records are already filtered to the item run's
// account by the store. A record is fresh when it was received within
// hostdispatcher.StaleAfter of now and no more than FutureSkew ahead of it.
// With no record the dispatcher is absent; with no fresh record it is stale.
// Each fresh record gets its own verdict (recordVerdict) and the best rank
// wins; within a rank the newest received_at, then the lower host label.
func classifyHostDispatcherHeartbeat(records []dispatcherheartbeat.Record, runID, stageID string, now time.Time) hostDispatcherVerdict {
	if len(records) == 0 {
		return hostDispatcherHuman(awaitingReasonHostDispatcherAbsent, "", hostDispatcherObservation{State: hostDispatcherStateAbsent})
	}
	var (
		best     hostDispatcherVerdict
		bestRank int
		bestRec  *dispatcherheartbeat.Record
		newest   *dispatcherheartbeat.Record
	)
	for i := range records {
		rec := &records[i]
		if newest == nil || rec.ReceivedAt.After(newest.ReceivedAt) ||
			(rec.ReceivedAt.Equal(newest.ReceivedAt) && rec.HostLabel < newest.HostLabel) {
			newest = rec
		}
		if !hostDispatcherRecordFresh(*rec, now) {
			continue
		}
		v, rank := hostDispatcherRecordVerdict(*rec, runID, stageID, now)
		if bestRec == nil || rank < bestRank ||
			(rank == bestRank && (rec.ReceivedAt.After(bestRec.ReceivedAt) ||
				(rec.ReceivedAt.Equal(bestRec.ReceivedAt) && rec.HostLabel < bestRec.HostLabel))) {
			best, bestRank, bestRec = v, rank, rec
		}
	}
	if bestRec == nil {
		return hostDispatcherHuman(awaitingReasonHostDispatcherAbsent, "", hostDispatcherObservation{
			State: hostDispatcherStateStale, Host: newest.HostLabel, LastHeartbeatAt: newest.ReceivedAt,
		})
	}
	return best
}

// hostDispatcherRecordFresh reports whether rec was received within its
// clamped staleness window and not implausibly far in the future.
func hostDispatcherRecordFresh(rec dispatcherheartbeat.Record, now time.Time) bool {
	if rec.ReceivedAt.After(now.Add(hostdispatcher.FutureSkew)) {
		return false
	}
	return now.Sub(rec.ReceivedAt) <= hostdispatcher.StaleAfter(rec.Heartbeat.IntervalSeconds)
}

// hostDispatcherRecordVerdict is one fresh record's verdict and rank. Health
// comes first (paused, then a tick failure), then the run's entry (an entry
// naming a different stage is discarded first), then the daily cap; otherwise
// the dispatcher is live.
func hostDispatcherRecordVerdict(rec dispatcherheartbeat.Record, runID, stageID string, now time.Time) (hostDispatcherVerdict, int) {
	hb := rec.Heartbeat
	obs := hostDispatcherObservation{Host: rec.HostLabel, LastHeartbeatAt: rec.ReceivedAt, DailyCap: hb.DailyCap, SpawnsToday: hb.SpawnsToday}
	if hb.Paused {
		obs.State = hostDispatcherStatePaused
		return hostDispatcherHuman(awaitingReasonHostDispatchPaused, "", obs), hostDispatcherRankHostBlock
	}
	if hb.TickFailureCode != "" {
		obs.State = hostDispatcherStateRefusing
		obs.TickFailureCode = hb.TickFailureCode
		return hostDispatcherHuman(awaitingReasonHostDispatchRefused, "", obs), hostDispatcherRankHostBlock
	}
	if ref := hostDispatcherRunEntry(hb, runID, stageID); ref != nil {
		obs.RefusalCode, obs.RefusalFirstAt = ref.Code, ref.FirstAt
		switch ref.Disposition {
		case hostdispatcher.DispositionWaiting:
			obs.State = hostDispatcherStateWaiting
			return hostDispatcherVerdict{Kind: awaitingKindHostDispatcher, Reason: awaitingReasonHostDispatch,
				NextStep: awaitingNextSteps[awaitingReasonHostDispatch], Observation: obs}, hostDispatcherRankWaiting
		case hostdispatcher.DispositionInFlight:
			obs.State = hostDispatcherStateInFlight
			return hostDispatcherVerdict{Kind: awaitingKindSystem, Reason: awaitingReasonInFlight,
				NextStep: awaitingNextSteps[awaitingReasonInFlight], Observation: obs}, hostDispatcherRankInFlight
		case hostdispatcher.DispositionHeld:
			obs.State = hostDispatcherStateHeld
			return hostDispatcherHuman(awaitingReasonHostDispatchRefused, "", obs), hostDispatcherRankRunHold
		case hostdispatcher.DispositionBudgetBlocked:
			obs.State = hostDispatcherStateHeld
			obs.BudgetSource = ref.BudgetSource
			return hostDispatcherHuman(awaitingReasonHostDispatchBudgetBlocked, ref.BudgetSource, obs), hostDispatcherRankRunHold
		case hostdispatcher.DispositionPaused:
			obs.State = hostDispatcherStatePaused
			return hostDispatcherHuman(awaitingReasonHostDispatchPaused, "", obs), hostDispatcherRankHostBlock
		case hostdispatcher.DispositionCapped:
			obs.State = hostDispatcherStateCapped
			obs.CapResetsAt = hostDispatcherNextUTCMidnight(now)
			return hostDispatcherHuman(awaitingReasonHostDispatchCapped, "", obs), hostDispatcherRankHostBlock
		default:
			// refusing (and, defensively, anything ingest let through).
			obs.State = hostDispatcherStateRefusing
			obs.RefusalDetail = ref.Detail
			return hostDispatcherHuman(awaitingReasonHostDispatchRefused, "", obs), hostDispatcherRankHostBlock
		}
	}
	if hb.DailyCap > 0 && hb.SpawnsToday >= hb.DailyCap && hb.SpawnsDay == now.UTC().Format(time.DateOnly) {
		obs.State = hostDispatcherStateCapped
		obs.CapResetsAt = hostDispatcherNextUTCMidnight(now)
		return hostDispatcherHuman(awaitingReasonHostDispatchCapped, "", obs), hostDispatcherRankHostBlock
	}
	obs.State = hostDispatcherStateLive
	return hostDispatcherVerdict{Kind: awaitingKindHostDispatcher, Reason: awaitingReasonHostDispatch,
		NextStep: awaitingNextSteps[awaitingReasonHostDispatch], Observation: obs}, hostDispatcherRankLive
}

// hostDispatcherRunEntry returns hb's run_refusals entry for runID, or nil.
// An entry whose non-empty stage_id names a different stage than the item's
// is discarded: it can never decide or select a record.
func hostDispatcherRunEntry(hb hostdispatcher.Heartbeat, runID, stageID string) *hostdispatcher.RunRefusal {
	for i := range hb.RunRefusals {
		ref := &hb.RunRefusals[i]
		if ref.RunID != runID {
			continue
		}
		if ref.StageID != "" && ref.StageID != stageID {
			return nil
		}
		return ref
	}
	return nil
}

// hostDispatcherHuman builds a human verdict with its state-keyed text.
// budgetSource selects the live_spec budget text; any other value shares the
// admission text.
func hostDispatcherHuman(reason, budgetSource string, obs hostDispatcherObservation) hostDispatcherVerdict {
	key := hostDispatcherTextKey{Reason: reason, State: obs.State}
	if reason == awaitingReasonHostDispatchBudgetBlocked && budgetSource == hostdispatcher.BudgetSourceLiveSpec {
		key.BudgetSource = hostdispatcher.BudgetSourceLiveSpec
	}
	return hostDispatcherVerdict{Kind: awaitingKindHuman, Reason: reason, NextStep: hostDispatcherNextSteps[key], Observation: obs}
}

func hostDispatcherNextUTCMidnight(now time.Time) time.Time {
	y, m, d := now.UTC().Date()
	return time.Date(y, m, d+1, 0, 0, 0, 0, time.UTC)
}

// hostDispatcherHeartbeatLoader reads the heartbeat rows visible to an item
// run's account. A nil loader means the heartbeat store is unwired.
type hostDispatcherHeartbeatLoader func(accountID string) ([]dispatcherheartbeat.Record, error)

// hostDispatcherHeartbeatLoaderForStatus builds one status call's loader: nil
// when the store is unwired, else a lazy loader memoised per account, so a
// status call lists once per distinct item-run account and a campaign with
// no host_dispatcher item lists nothing. An error is memoised too.
func (s *Server) hostDispatcherHeartbeatLoaderForStatus(ctx context.Context) hostDispatcherHeartbeatLoader {
	repo := s.cfg.HostDispatcherHeartbeatRepo
	if repo == nil {
		return nil
	}
	type result struct {
		recs []dispatcherheartbeat.Record
		err  error
	}
	var mu sync.Mutex
	memo := map[string]result{}
	return func(accountID string) ([]dispatcherheartbeat.Record, error) {
		mu.Lock()
		defer mu.Unlock()
		if res, ok := memo[accountID]; ok {
			return res.recs, res.err
		}
		recs, err := repo.List(ctx, accountID, hostDispatcherHeartbeatListLimit)
		memo[accountID] = result{recs, err}
		return recs, err
	}
}

// applyHostDispatcherHeartbeat classifies a block that still reads
// host_dispatcher/host_dispatch (after the operator dispatch hold) from the
// heartbeats visible to the run's account. A nil loader (unwired) leaves the
// block unchanged; a read error leaves it unchanged with decision_unavailable.
func applyHostDispatcherHeartbeat(blk campaignItemAwaitingPayload, accountID string, load hostDispatcherHeartbeatLoader, now time.Time) (campaignItemAwaitingPayload, error) {
	if load == nil || blk.Kind != awaitingKindHostDispatcher || blk.Reason != awaitingReasonHostDispatch {
		return blk, nil
	}
	recs, err := load(accountID)
	if err != nil {
		blk.DecisionUnavailable = true
		return blk, err
	}
	v := classifyHostDispatcherHeartbeat(recs, blk.RunID, blk.StageID, now)
	blk.Kind, blk.Reason, blk.NextStep = v.Kind, v.Reason, v.NextStep
	obs := v.Observation
	blk.HostDispatcher = &obs
	return blk, nil
}
