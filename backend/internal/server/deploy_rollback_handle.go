package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// rollbackHandleDispatchInput is the github_actions workflow_dispatch input
// that carries the stored deployment rollback_handle to the rollback
// re-dispatch (E35.3 / #1600). It is sent ONLY when a stored handle exists:
// GitHub's workflow_dispatch endpoint refuses an input the workflow does not
// declare (422 "Unexpected inputs provided"), so a pipeline that never returns
// a rollback_handle never sees it, and a pipeline that does return one must
// declare it under on.workflow_dispatch.inputs.
const rollbackHandleDispatchInput = "fishhawk_rollback_handle"

// rollbackHandleWebhookVariable is the webhook trigger body's
// variables.<name> entry carrying the stored rollback_handle on a rollback
// re-dispatch (E35.3 / #1600). It rides the already-reserved `variables`
// object, so spec.WebhookReservedBodyKeys is unchanged; like the dispatch
// input it is set only when a stored handle exists.
const rollbackHandleWebhookVariable = "FISHHAWK_ROLLBACK_HANDLE"

// storedRollbackHandle is the rollback reference a deploy stage's stored
// deployment artifacts resolve to: the artifact the handle was read from and
// the opaque handle itself. The zero value means "no forward deployment
// record"; a non-zero ArtifactID with an empty Handle means "a forward record
// exists but no forward record carried a rollback_handle".
type storedRollbackHandle struct {
	ArtifactID uuid.UUID
	Handle     string
}

// artifactIDString renders ArtifactID for a wire payload, empty when no
// forward deployment record was found.
func (h storedRollbackHandle) artifactIDString() string {
	if h.ArtifactID == uuid.Nil {
		return ""
	}
	return h.ArtifactID.String()
}

// storedRollbackHandleFor resolves the rollback_handle the external pipeline
// returned for deployStageID's deploy (E35.3 / #1600, ADR-053).
//
// Selection rule: among the stage's deployment artifacts, ignore rollback
// sub-action records (a non-empty rollback_action, or outcome rolled_back) —
// they describe the revert, not the deploy to revert — and return the NEWEST
// FORWARD record that carries a non-empty rollback_handle. A handle-bearing
// pipeline callback therefore wins over a NEWER handle-less record such as
// the github_actions reconciler's poll-state artifact, which can never know
// a handle. When no forward record carries a handle, the newest forward
// record's ArtifactID is returned with an empty Handle; with no forward record
// at all, the zero value.
//
// The artifacts are sorted by CreatedAt here rather than trusting
// ListForStage's documented ascending order, so the "newest" decision never
// depends on a repository implementation detail. A row whose content does not
// decode is skipped with a WARN (content was validated at ingest, so this is
// corruption, not a caller error). A ListForStage error is returned wrapped:
// the rollback endpoint fails closed on it. A nil ArtifactRepo returns the
// zero value and no error — the unconfigured posture, in which the rollback
// dispatches exactly as it did before the handle was wired.
func (s *Server) storedRollbackHandleFor(ctx context.Context, deployStageID uuid.UUID) (storedRollbackHandle, error) {
	if s.cfg.ArtifactRepo == nil {
		return storedRollbackHandle{}, nil
	}
	all, err := s.cfg.ArtifactRepo.ListForStage(ctx, deployStageID)
	if err != nil {
		return storedRollbackHandle{}, fmt.Errorf("list deployment artifacts for stage %s: %w", deployStageID, err)
	}
	deployments := make([]*artifact.Artifact, 0, len(all))
	for _, a := range all {
		if a.Kind == artifact.KindDeployment {
			deployments = append(deployments, a)
		}
	}
	sort.SliceStable(deployments, func(i, j int) bool {
		return deployments[i].CreatedAt.Before(deployments[j].CreatedAt)
	})

	var newestForward uuid.UUID
	for i := len(deployments) - 1; i >= 0; i-- {
		a := deployments[i]
		var body deploymentBody
		if err := json.Unmarshal(a.Content, &body); err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"deploy rollback: stored deployment artifact does not decode; skipped for rollback_handle selection",
				slog.String("stage_id", deployStageID.String()),
				slog.String("artifact_id", a.ID.String()),
				slog.String("error", err.Error()))
			continue
		}
		if body.RollbackAction != "" || body.Outcome == string(run.DeployOutcomeRolledBack) {
			continue
		}
		if newestForward == uuid.Nil {
			newestForward = a.ID
		}
		if body.RollbackHandle != "" {
			return storedRollbackHandle{ArtifactID: a.ID, Handle: body.RollbackHandle}, nil
		}
	}
	return storedRollbackHandle{ArtifactID: newestForward}, nil
}
