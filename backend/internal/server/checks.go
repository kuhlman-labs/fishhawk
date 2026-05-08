package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcomplete"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/stagecheck"
)

// stageCheckResponse mirrors what the SPA's BlockingChecksPanel
// expects: a small `{name, state, …}` shape with the SPA's enum
// rather than raw GitHub fields. Detail fields (conclusion,
// head_sha, github_check_run_id) are forwarded for forensic /
// audit-export use. `missing` is populated only for self-derived
// checks like fishhawk_audit_complete (#229) where the failure
// reason is structured rather than a raw GitHub conclusion.
type stageCheckResponse struct {
	Name             string                      `json:"name"`
	State            string                      `json:"state"`
	Status           string                      `json:"status,omitempty"`
	Conclusion       *string                     `json:"conclusion,omitempty"`
	HeadSHA          string                      `json:"head_sha,omitempty"`
	GitHubCheckRunID *int64                      `json:"github_check_run_id,omitempty"`
	Timestamp        time.Time                   `json:"ts,omitempty"`
	Missing          []auditcomplete.MissingItem `json:"missing,omitempty"`
}

// stageChecksListResponse is the envelope for GET /v0/stages/{id}/checks.
// `declared` is the gate's blocking_checks list as written in the
// workflow spec; `items` is the latest observed state per check
// name. Declared-but-not-observed checks render in the SPA as
// `not_tracked`; the response itself only carries observed rows
// since the SPA already knows the declared list from the stage.
type stageChecksListResponse struct {
	Declared []string             `json:"declared"`
	Items    []stageCheckResponse `json:"items"`
}

// handleListStageChecks implements GET /v0/stages/{stage_id}/checks (#228).
//
// Returns the most-recent observed state per blocking check name on
// the stage, plus the gate's declared list so the SPA doesn't need
// to re-derive it from the Stage response. Declared-but-not-observed
// checks are reported in `declared` but absent from `items`; the
// SPA fills with `not_tracked`.
func (s *Server) handleListStageChecks(w http.ResponseWriter, r *http.Request) {
	if s.cfg.RunRepo == nil || s.cfg.StageCheckRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "stage_checks_unconfigured",
			"stage checks endpoint requires run and stage-check repos to be configured", nil)
		return
	}
	stageID, err := uuid.Parse(r.PathValue("stage_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"stage_id must be a valid UUID",
			map[string]any{"field": "stage_id", "got": r.PathValue("stage_id")})
		return
	}

	stage, err := s.cfg.RunRepo.GetStage(r.Context(), stageID)
	if err != nil {
		if errors.Is(err, run.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "stage_not_found",
				"no stage with that id", map[string]any{"stage_id": stageID.String()})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get stage failed", map[string]any{"error": err.Error()})
		return
	}

	checks, err := s.cfg.StageCheckRepo.LatestForStage(r.Context(), stageID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"list stage checks failed", map[string]any{"error": err.Error()})
		return
	}

	declared := []string{}
	if stage.Gate != nil {
		declared = stage.Gate.BlockingChecks
	}
	items := make([]stageCheckResponse, 0, len(checks))
	for _, c := range checks {
		items = append(items, toStageCheckResponse(c))
	}

	// Inject the self-derived fishhawk_audit_complete row when the
	// gate declares it (#229). Computed live from the run's
	// artifact + audit-log presence; carries the structured
	// `missing` list so the SPA can show "fail because: plan
	// missing, redacted trace missing on stage X" without a
	// secondary call.
	if containsString(declared, AuditCompleteCheckName) && s.cfg.ArtifactRepo != nil && s.cfg.AuditRepo != nil {
		state, missing, err := auditcomplete.Compute(r.Context(), stage.RunID, auditcomplete.Deps{
			Runs:      s.cfg.RunRepo,
			Artifacts: s.cfg.ArtifactRepo,
			Audit:     s.cfg.AuditRepo,
		})
		if err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"derive audit-complete state failed",
				map[string]any{"stage_id": stageID.String(), "error": err.Error()})
			return
		}
		items = append(items, stageCheckResponse{
			Name:      AuditCompleteCheckName,
			State:     string(state),
			Timestamp: time.Now().UTC(),
			Missing:   missing,
		})

		// Publish the same state to GitHub as a Check Run (#231).
		// Best-effort: a failure logs but doesn't fail the read —
		// the in-Fishhawk gate enforcement still works without
		// the GitHub publish.
		s.publishAuditCheck(r.Context(), stage.RunID, state, missing)
	}

	s.writeJSON(w, r, http.StatusOK, stageChecksListResponse{
		Declared: declared,
		Items:    items,
	})
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// publishAuditCheck is the small adapter between the server's
// compute paths and the auditcheckpublisher. Best-effort: a
// publish failure logs at WARN and returns; the in-Fishhawk gate
// enforcement still proceeds so a GitHub outage doesn't black-
// hole approvals. Nil-safe — the publisher is nil when
// ExternalURL or GitHub aren't wired (legacy / dev posture), and
// Publish returns immediately in that case.
func (s *Server) publishAuditCheck(ctx context.Context, runID uuid.UUID, state stagecheck.State, missing []auditcomplete.MissingItem) {
	if s.auditCheckPublisher == nil {
		return
	}
	published, err := s.auditCheckPublisher.Publish(ctx, runID, state, missing)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"audit-complete check-run publish failed",
			slog.String("run_id", runID.String()),
			slog.String("state", string(state)),
			slog.String("error", err.Error()),
		)
		return
	}
	if published {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelDebug,
			"audit-complete check-run published",
			slog.String("run_id", runID.String()),
			slog.String("state", string(state)),
		)
	}
}

func toStageCheckResponse(c *stagecheck.Check) stageCheckResponse {
	return stageCheckResponse{
		Name:             c.Name,
		State:            string(c.State),
		Status:           c.Status,
		Conclusion:       c.Conclusion,
		HeadSHA:          c.HeadSHA,
		GitHubCheckRunID: c.GitHubCheckRunID,
		Timestamp:        c.Timestamp,
	}
}
