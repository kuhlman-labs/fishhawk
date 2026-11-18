package server

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// A review-first human workflow has no runner entry to initialize its gate.
// Only its uninitialized first stage is eligible: re-entering Advance after
// that stage parks could dispatch a later stage ahead of the human decision.
func humanReviewFirstUninitialized(st *run.Stage) bool {
	return st != nil && st.Sequence == 0 && st.Type == run.StageTypeReview &&
		st.ExecutorKind == run.ExecutorHuman &&
		(st.State == run.StageStatePending || st.State == run.StageStateDispatched || st.State == run.StageStateRunning)
}

func (s *Server) initializeHumanReviewFirst(ctx context.Context, row *run.Run, stages []*run.Stage) error {
	if row.State.IsTerminal() || len(stages) == 0 || !humanReviewFirstUninitialized(stages[0]) {
		return nil
	}
	reason := reviewGateAdmissionForRun(stages[0], row)
	if reason != reviewGateAdmitOK && reason != reviewGateAdmitPullRequestManaged {
		return fmt.Errorf("human review initialization: %s", reason.String())
	}
	if s.cfg.Orchestrator == nil {
		return fmt.Errorf("human review initialization: orchestrator unavailable")
	}

	_, err := s.cfg.Orchestrator.InitializeHumanReviewFirst(ctx, row.ID)
	return err
}

// Called only after the upload's governance record and branch-lineage check.
// Replays take the same path so a failed URL backfill or Advance can heal
// without another PR/artifact or an implicit approval.
func (s *Server) initializeHumanReviewAfterPR(ctx context.Context, runID uuid.UUID, stage *run.Stage, prURL string) error {
	if !humanReviewFirstUninitialized(stage) {
		return nil
	}
	if prURL == "" {
		return fmt.Errorf("human review PR initialization: missing pull request URL")
	}
	row, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, runID)
	if err != nil {
		return err
	}
	if row.State.IsTerminal() || len(stages) == 0 || stages[0].ID != stage.ID || !humanReviewFirstUninitialized(stages[0]) {
		return nil
	}
	if reason := reviewGateAdmissionForRun(stages[0], row); reason != reviewGateAdmitPullRequestManaged {
		return fmt.Errorf("human review PR initialization: %s", reason.String())
	}
	if row.PullRequestURL == nil || *row.PullRequestURL != prURL {
		row, err = s.cfg.RunRepo.SetRunPullRequestURL(ctx, runID, prURL)
		if err != nil {
			return err
		}
	}
	return s.initializeHumanReviewFirst(ctx, row, stages)
}
