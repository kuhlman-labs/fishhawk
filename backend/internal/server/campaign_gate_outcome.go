package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaigndriver"
	"github.com/kuhlman-labs/fishhawk/backend/internal/issuecomment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// CategoryCampaignDriverActAwaitingOperator is the #5562 page-class row
// DriveCampaignRunGate appends when a campaign-driver act parks a stage at
// awaiting_host_dispatch on a run no dispatcher will spawn.
// issuecomment.pageClassEvents projects it to the
// driver_act_awaiting_operator_dispatch page (pings-only).
const CategoryCampaignDriverActAwaitingOperator = "campaign_driver_act_awaiting_operator"

// campaignDriverActorSubject is the system actor stamped on the #5562 row.
const campaignDriverActorSubject = "system:campaign-driver"

// driverActPageReason is the server-constant reason on the #5562 row.
const driverActPageReason = "a campaign-driver act parked this stage at awaiting_host_dispatch and no dispatcher will spawn it on this run; an operator must dispatch it"

// DriveCampaignRunGate is the campaign driver's gate actor (E88.4 / #4528). It
// calls AutoDriveRunGate unchanged and translates its outcome into the
// driver's campaigndriver.GateActionOutcome, carrying DecisionRequired,
// DecisionState and ConcernIDs (set only on fixup_no_changes_unaddressed,
// #5390) so the driver can record a campaign_gate_decision_required marker. The serve.go campaignGateActor adapter delegates here, so the
// translation lives in one place the server tests reach.
//
// After the gate evaluation it reads the run's stages once and reports the
// parked plan/implement gate (gatedReviewStage, called, not changed) and every
// parked plan, implement or review stage, with GateKnown=true. The driver
// clears a recorded decision only on that positive read (approval condition
// C1): AutoDriveRunGate answers observe-only with a nil error on its own
// fail-closed reads, which is not evidence the gate moved. GateKnown stays
// false, with no read, when the evaluation errored, when RunRepo is unwired
// (C4), or when the outcome is not a decision and the driver's
// campaigndriver.DecisionKeyHeld hint says it holds no key for the run
// (nothing to clear, C4). A failed read WARN-logs and also leaves GateKnown
// false; the outcome and error are otherwise unchanged.
//
// A review-type gate is never returned by gatedReviewStage and never yields
// DecisionRequired, so it never gets a marker; the campaign status awaiting
// block shows a parked review stage from stage state instead.
//
// #5562: on a local run no dispatcher will spawn, it also snapshots the parked
// stages before the act and, after an acted outcome, pages each stage the act
// itself parked at awaiting_host_dispatch (pageDriverActAwaitingOperator). The
// page is best-effort and never changes the outcome or error.
func (s *Server) DriveCampaignRunGate(ctx context.Context, runRow *run.Run, id Identity, merger GitHubMerger, campaignOverride []byte) (campaigndriver.GateActionOutcome, error) {
	pre := s.snapshotHostDispatchParks(ctx, runRow)
	out, err := s.AutoDriveRunGate(ctx, runRow, id, merger, campaignOverride)
	res := campaigndriver.GateActionOutcome{
		Acted:            out.Acted,
		Action:           out.Action,
		Paged:            out.Paged,
		PageEvent:        out.PageEvent,
		Note:             out.Note,
		DecisionRequired: out.DecisionRequired,
		DecisionState:    out.DecisionState,
		ConcernIDs:       out.ConcernIDs,
	}
	if err != nil {
		// The driver discards an errored outcome before any marker logic.
		return res, err
	}
	if out.Acted {
		s.pageDriverActAwaitingOperator(ctx, runRow, out, pre)
	}
	if s.cfg.RunRepo == nil {
		return res, nil
	}
	if !out.DecisionRequired {
		if held, ok := campaigndriver.DecisionKeyHeld(ctx); ok && !held {
			return res, nil
		}
	}
	stages, rerr := s.cfg.RunRepo.ListStagesForRun(ctx, runRow.ID)
	if rerr != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "campaign gate: list stages failed; gate unknown",
			slog.String("run_id", runRow.ID.String()), slog.String("error", rerr.Error()))
		return res, nil
	}
	res.GateKnown = true
	if st := gatedReviewStage(stages); st != nil {
		stageID := st.ID
		res.GateStageID = &stageID
	}
	res.ParkedGateStageIDs = parkedGateStageIDs(stages)
	return res, nil
}

// parkedGateStageIDs returns every plan, implement or review stage in
// awaiting_approval, in the order read.
func parkedGateStageIDs(stages []*run.Stage) []uuid.UUID {
	var ids []uuid.UUID
	for _, st := range stages {
		if st.State != run.StageStateAwaitingApproval {
			continue
		}
		switch st.Type {
		case run.StageTypePlan, run.StageTypeImplement, run.StageTypeReview:
			ids = append(ids, st.ID)
		}
	}
	return ids
}

// driverActLeavesOperatorDispatch reports whether a stage parked on r waits on
// an operator dispatch rather than the host dispatcher (#5562). It is the same
// hostDispatchKind classification the campaign awaiting block and the #5308
// acceptance page use, so the three cannot disagree.
func driverActLeavesOperatorDispatch(r *run.Run) bool {
	kind, _ := hostDispatchKind(r)
	return kind != awaitingKindHostDispatcher
}

// hostDispatchParkSnapshot is the pre-act read of a run's awaiting_host_dispatch
// parks (#5562): stage id → issuecomment.HostDispatchParkEpisode key. known is
// false when no snapshot was taken (nothing to page) or the read failed.
type hostDispatchParkSnapshot struct {
	known    bool
	episodes map[uuid.UUID]string
}

// snapshotHostDispatchParks reads the run's stages once BEFORE the gate act so
// the post-act page names only the stages the act itself parked (#5562
// condition P2). It reads nothing, and returns an unknown snapshot, when
// RunRepo or AuditRepo is unwired, when the run is not local (only the local
// backend parks at awaiting_host_dispatch, runnerbackend.Local.HostDispatched),
// or when the host dispatcher spawns the run's stages
// (driverActLeavesOperatorDispatch). A read error WARN-logs and also returns
// an unknown snapshot, which fails toward NO page: without the pre-act state
// there is no basis to say which park the act made.
func (s *Server) snapshotHostDispatchParks(ctx context.Context, runRow *run.Run) hostDispatchParkSnapshot {
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		return hostDispatchParkSnapshot{}
	}
	if runRow.RunnerKind != run.RunnerKindLocal || !driverActLeavesOperatorDispatch(runRow) {
		return hostDispatchParkSnapshot{}
	}
	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, runRow.ID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "campaign driver act page: pre-act stage read failed; no page this tick",
			slog.String("run_id", runRow.ID.String()), slog.String("error", err.Error()))
		return hostDispatchParkSnapshot{}
	}
	snap := hostDispatchParkSnapshot{known: true, episodes: map[uuid.UUID]string{}}
	for _, st := range stages {
		if key, ok := issuecomment.HostDispatchParkEpisode(st); ok {
			snap.episodes[st.ID] = key
		}
	}
	return snap
}

// pageDriverActAwaitingOperator pages the operator when a campaign-driver act
// (route_fixup, approve or retry) parked a stage at awaiting_host_dispatch on a
// run no dispatcher will spawn (#5562). Without it the re-opened stage sits idle
// with no page: the host dispatcher spawns only an auto_dispatch-marked local
// run (ADR-104 Q3), and an operator-started campaign item is unmarked unless
// started with auto_dispatch: true.
//
// It pages only the stages the act itself parked: a stage parked in the same
// episode before the act (pre) is skipped, and so is a stage whose current park
// episode already carries a row (dedup per stage and park episode, keyed by
// issuecomment.HostDispatchParkEpisode). For each remaining stage it appends one
// stage-scoped CategoryCampaignDriverActAwaitingOperator row, then fires
// notifyPageClass ONCE after the appends. issuecomment.pageClassEvents projects
// the row to the driver_act_awaiting_operator_dispatch page, which resolves when
// the stage leaves that park episode.
//
// It sits in this campaign-only seam rather than AutoDriveRunGate because
// drive_run, the other caller, dispatches its own stages. Best-effort: a failed
// read or append WARN-logs and loses the page permanently; the fallback is the
// campaign status awaiting block (human/operator_dispatch, next_step
// fishhawk_dispatch_stage). It never changes the returned outcome or error.
func (s *Server) pageDriverActAwaitingOperator(ctx context.Context, runRow *run.Run, out AutoDriveOutcome, pre hostDispatchParkSnapshot) {
	// An unknown snapshot covers every no-page run: unwired repositories, a
	// non-local or host-dispatched run, and a failed pre-act read.
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil || !pre.known {
		return
	}
	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, runRow.ID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "campaign driver act page: list stages failed; skipping page",
			slog.String("run_id", runRow.ID.String()), slog.String("error", err.Error()))
		return
	}
	var paged map[string]struct{}
	appended := 0
	for _, st := range stages {
		if st.State != run.StageStateAwaitingHostDispatch {
			continue
		}
		key, _ := issuecomment.HostDispatchParkEpisode(st)
		if prior, ok := pre.episodes[st.ID]; ok && prior == key {
			continue // parked before the act: not this act's park
		}
		if paged == nil {
			if paged, err = s.pagedDriverActEpisodes(ctx, runRow.ID); err != nil {
				s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "campaign driver act page: read prior pages failed; skipping page",
					slog.String("run_id", runRow.ID.String()), slog.String("error", err.Error()))
				return
			}
		}
		if _, done := paged[key]; done {
			continue // this park episode is already paged
		}
		stageID := st.ID
		subject := campaignDriverActorSubject
		actorKind := audit.ActorSystem
		payload, _ := json.Marshal(map[string]any{
			"stage_id":      stageID.String(),
			"stage_type":    string(st.Type),
			"stage_state":   string(st.State),
			"episode_key":   key,
			"action":        out.Action,
			"runner_kind":   runRow.RunnerKind,
			"auto_dispatch": runRow.AutoDispatch,
			"next_action":   "fishhawk_dispatch_stage",
			"reason":        driverActPageReason,
		})
		if _, aerr := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
			RunID:        runRow.ID,
			StageID:      &stageID,
			Timestamp:    time.Now().UTC(),
			Category:     CategoryCampaignDriverActAwaitingOperator,
			ActorKind:    &actorKind,
			ActorSubject: &subject,
			Payload:      payload,
		}); aerr != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "campaign driver act page: append failed; skipping stage",
				slog.String("run_id", runRow.ID.String()), slog.String("stage_id", stageID.String()),
				slog.String("error", aerr.Error()))
			continue
		}
		paged[key] = struct{}{}
		appended++
	}
	if appended > 0 {
		s.notifyPageClass(ctx, runRow.ID, "campaign_driver_act")
	}
}

// pagedDriverActEpisodes returns the park-episode keys the run's existing
// CategoryCampaignDriverActAwaitingOperator rows already paged (#5562 dedup).
func (s *Server) pagedDriverActEpisodes(ctx context.Context, runID uuid.UUID) (map[string]struct{}, error) {
	rows, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryCampaignDriverActAwaitingOperator)
	if err != nil {
		return nil, err
	}
	keys := make(map[string]struct{}, len(rows))
	for _, e := range rows {
		var p struct {
			EpisodeKey string `json:"episode_key"`
		}
		if json.Unmarshal(e.Payload, &p) == nil && p.EpisodeKey != "" {
			keys[p.EpisodeKey] = struct{}{}
		}
	}
	return keys, nil
}
