package server

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// acceptanceRollbackOffer is the rollback_offer object a rollback_offered
// acceptance_triage_decided payload carries (E35.3 / #1600, ADR-053): the
// deploy stage the failed post-deploy verdict verified, the deployment
// artifact its stored rollback_handle was read from, and the handle itself.
// The two artifact-derived fields are empty when the deploy recorded no
// forward deployment record / no handle, or when the handle read failed — the
// offer still stands, and the rollback endpoint re-reads (failing closed
// itself). backend/internal/mcpserver/next_actions.go mirrors these JSON keys
// (it cannot import this package, #875); agreement is pinned by both
// packages' tests using the identical literal keys.
type acceptanceRollbackOffer struct {
	DeployStageID        string `json:"deploy_stage_id"`
	DeploymentArtifactID string `json:"deployment_artifact_id"`
	RollbackHandle       string `json:"rollback_handle"`
}

// postDeployRollbackTarget resolves, from a run's stage rows, the deploy stage
// a post-deploy acceptance verified and the deploy stage the run-scoped
// rollback endpoint would revert.
//
//   - verified is the NEAREST deploy-typed stage sequenced before acceptance
//     (the just-verified deploy); nil when no deploy precedes it — the
//     feature_change shape, which keeps every existing disposition.
//   - runScoped is the LOWEST-sequence deploy-typed stage: the row
//     deployStageForRun returns for POST /v0/runs/{run_id}/deployment/rollback
//     (it takes the first deploy of ListStagesForRun, which production orders
//     by sequence ASC). It is resolved by sequence here rather than by list
//     position so the comparison never depends on a repository's ordering; the
//     two are compared to detect the #2642 multi-deploy mismatch.
//   - hasImplement reports whether the run carries ANY implement stage. The
//     rollback offer fires only when it does not (approval condition 1 on
//     #1600): with an implement stage the existing class-1 fix-up route
//     (routeAcceptanceClass1, which scans every stage) stays authoritative, so
//     a mixed [implement, deploy, acceptance] workflow is unchanged; without one
//     — the release shape — class 1 would degrade to fixup_unavailable_paged,
//     so the offer strictly improves the outcome.
func postDeployRollbackTarget(stages []*run.Stage, acceptance *run.Stage) (verified, runScoped *run.Stage, hasImplement bool) {
	for _, st := range stages {
		if st == nil {
			continue
		}
		switch st.Type {
		case run.StageTypeImplement:
			hasImplement = true
		case run.StageTypeDeploy:
			if runScoped == nil || st.Sequence < runScoped.Sequence {
				runScoped = st
			}
			if acceptance != nil && st.ID != acceptance.ID && st.Sequence < acceptance.Sequence &&
				(verified == nil || st.Sequence > verified.Sequence) {
				verified = st
			}
		}
	}
	return verified, runScoped, hasImplement
}

// decidePostDeployRollback is the post-deploy branch of triageAcceptanceFailure
// (E35.3 / #1600). handled=false means "not a post-deploy rollback shape": the
// caller runs its EXISTING routing unchanged. handled=true returns the
// disposition (rollback_offered, or paged on the #2642 multi-deploy mismatch),
// the audit reason, and — for rollback_offered only — the offer to record.
//
// It is pure decision + reads: it NEVER calls dispatchRollback, the
// orchestrator, or any stage/run transition. The offer is operator-gated by
// construction; the rollback fires only on an explicit
// POST /v0/runs/{run_id}/deployment/rollback.
//
// Only classes 1 (the code objectively fails) and 4 (unitemized / works-as-
// planned dispute) are offered; class 2 keeps its re-run, class 3 its page and
// class 5 its terminal page. A stage-list error falls through (handled=false)
// to the existing routing, WARN-logged. A stored-handle read error still
// offers — with an empty handle and a reason naming the failure — because the
// offer has no side effect and the rollback endpoint re-reads and fails closed
// itself.
func (s *Server) decidePostDeployRollback(ctx context.Context, runID uuid.UUID, stage *run.Stage, class string) (offer *acceptanceRollbackOffer, disposition, reason string, handled bool) {
	if class != acceptanceClass1 && class != acceptanceClass4 {
		return nil, "", "", false
	}
	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"acceptance triage: list stages for post-deploy rollback check failed; routing as before",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
		return nil, "", "", false
	}
	verified, runScoped, hasImplement := postDeployRollbackTarget(stages, stage)
	if verified == nil || hasImplement {
		return nil, "", "", false
	}
	if runScoped == nil || runScoped.ID != verified.ID {
		runScopedID := ""
		if runScoped != nil {
			runScopedID = runScoped.ID.String()
		}
		return nil, acceptanceDispositionPaged, fmt.Sprintf(
			"class-%s failed post-deploy verdict on deploy stage %s; not offering a rollback because the run-scoped rollback endpoint resolves a different deploy stage (%s) on this multi-deploy run (#2642); paging",
			class, verified.ID, runScopedID), true
	}

	offer = &acceptanceRollbackOffer{DeployStageID: verified.ID.String()}
	stored, herr := s.storedRollbackHandleFor(ctx, verified.ID)
	if herr != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"acceptance triage: read stored rollback_handle failed; offering the rollback without a handle",
			slog.String("run_id", runID.String()),
			slog.String("deploy_stage_id", verified.ID.String()),
			slog.String("error", herr.Error()))
		return offer, acceptanceDispositionRollbackOffered, fmt.Sprintf(
			"class-%s failed post-deploy verdict on deploy stage %s; offering an operator-gated rollback (never auto-fired) — the stored rollback_handle could not be read, the rollback endpoint re-reads it and fails closed",
			class, verified.ID), true
	}
	offer.DeploymentArtifactID = stored.artifactIDString()
	offer.RollbackHandle = stored.Handle
	return offer, acceptanceDispositionRollbackOffered, fmt.Sprintf(
		"class-%s failed post-deploy verdict on deploy stage %s; offering an operator-gated rollback (never auto-fired) — roll back via POST /v0/runs/{run_id}/deployment/rollback or arbitrate to keep the deploy",
		class, verified.ID), true
}
