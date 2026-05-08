package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/stagecheck"
)

// stageCheckResponse mirrors what the SPA's BlockingChecksPanel
// expects: a small `{name, state, …}` shape with the SPA's enum
// rather than raw GitHub fields. Detail fields (conclusion,
// head_sha, github_check_run_id) are forwarded for forensic /
// audit-export use.
type stageCheckResponse struct {
	Name             string    `json:"name"`
	State            string    `json:"state"`
	Status           string    `json:"status,omitempty"`
	Conclusion       *string   `json:"conclusion,omitempty"`
	HeadSHA          string    `json:"head_sha,omitempty"`
	GitHubCheckRunID *int64    `json:"github_check_run_id,omitempty"`
	Timestamp        time.Time `json:"ts,omitempty"`
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
	s.writeJSON(w, r, http.StatusOK, stageChecksListResponse{
		Declared: declared,
		Items:    items,
	})
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
