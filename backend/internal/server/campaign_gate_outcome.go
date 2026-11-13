package server

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/campaigndriver"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// DriveCampaignRunGate is the campaign driver's gate actor (E88.4 / #4528). It
// calls AutoDriveRunGate unchanged and translates its outcome into the
// driver's campaigndriver.GateActionOutcome, carrying DecisionRequired and
// DecisionState so the driver can record a campaign_gate_decision_required
// marker. The serve.go campaignGateActor adapter delegates here, so the
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
func (s *Server) DriveCampaignRunGate(ctx context.Context, runRow *run.Run, id Identity, merger GitHubMerger, campaignOverride []byte) (campaigndriver.GateActionOutcome, error) {
	out, err := s.AutoDriveRunGate(ctx, runRow, id, merger, campaignOverride)
	res := campaigndriver.GateActionOutcome{
		Acted:            out.Acted,
		Action:           out.Action,
		Paged:            out.Paged,
		PageEvent:        out.PageEvent,
		Note:             out.Note,
		DecisionRequired: out.DecisionRequired,
		DecisionState:    out.DecisionState,
	}
	if err != nil {
		// The driver discards an errored outcome before any marker logic.
		return res, err
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
