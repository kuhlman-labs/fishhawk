package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// runResponse is the JSON shape POST /v0/runs and GET /v0/runs/{id}
// return. Field names + types match docs/api/v0.openapi.yaml's
// `Run` schema exactly so there's never a translation step between
// the OpenAPI doc and the wire format.
type runResponse struct {
	ID            uuid.UUID `json:"id"`
	Repo          string    `json:"repo"`
	WorkflowID    string    `json:"workflow_id"`
	WorkflowSHA   string    `json:"workflow_sha"`
	TriggerSource string    `json:"trigger_source"`
	TriggerRef    *string   `json:"trigger_ref"`
	State         string    `json:"state"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func toRunResponse(r *run.Run) runResponse {
	return runResponse{
		ID:            r.ID,
		Repo:          r.Repo,
		WorkflowID:    r.WorkflowID,
		WorkflowSHA:   r.WorkflowSHA,
		TriggerSource: string(r.TriggerSource),
		TriggerRef:    r.TriggerRef,
		State:         string(r.State),
		CreatedAt:     r.CreatedAt,
		UpdatedAt:     r.UpdatedAt,
	}
}

// createRunRequest mirrors POST /v0/runs's request body in
// v0.openapi.yaml. All four required fields must be present and
// non-empty; trigger_ref is optional.
type createRunRequest struct {
	Repo          string  `json:"repo"`
	WorkflowID    string  `json:"workflow_id"`
	WorkflowSHA   string  `json:"workflow_sha"`
	TriggerSource string  `json:"trigger_source"`
	TriggerRef    *string `json:"trigger_ref,omitempty"`
}

// validTriggerSources is the closed set per the workflow-spec and
// OpenAPI surface. New sources land in v0.x and require an explicit
// schema bump (see MVP_SPEC §7.1).
var validTriggerSources = map[string]struct{}{
	string(run.TriggerGitHubIssue): {},
	string(run.TriggerCLI):         {},
	string(run.TriggerUI):          {},
}

// handleCreateRun implements POST /v0/runs. Validates the request
// body, calls into the run repository, and returns the canonical
// Run JSON. The state machine starts every new run in
// run.StatePending.
func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	if s.cfg.RunRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "run_repo_unconfigured",
			"runs endpoint requires a configured run repository", nil)
		return
	}

	var req createRunRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"request body is not valid JSON or contains unknown fields",
			map[string]any{"error": err.Error()})
		return
	}

	if req.Repo == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"repo is required", map[string]any{"field": "repo"})
		return
	}
	if req.WorkflowID == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"workflow_id is required", map[string]any{"field": "workflow_id"})
		return
	}
	if req.WorkflowSHA == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"workflow_sha is required", map[string]any{"field": "workflow_sha"})
		return
	}
	if _, ok := validTriggerSources[req.TriggerSource]; !ok {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"trigger_source must be one of github_issue, cli, ui",
			map[string]any{"field": "trigger_source", "got": req.TriggerSource})
		return
	}

	created, err := s.cfg.RunRepo.CreateRun(r.Context(), run.CreateRunParams{
		Repo:          req.Repo,
		WorkflowID:    req.WorkflowID,
		WorkflowSHA:   req.WorkflowSHA,
		TriggerSource: run.TriggerSource(req.TriggerSource),
		TriggerRef:    req.TriggerRef,
	})
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"create run failed", map[string]any{"error": err.Error()})
		return
	}

	s.writeJSON(w, r, http.StatusCreated, toRunResponse(created))
}

// handleGetRun implements GET /v0/runs/{run_id}. Returns 404 with
// the run_not_found code if the ID doesn't resolve, and 400 if the
// path parameter isn't a valid UUID.
func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	if s.cfg.RunRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "run_repo_unconfigured",
			"runs endpoint requires a configured run repository", nil)
		return
	}

	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}

	got, err := s.cfg.RunRepo.GetRun(r.Context(), runID)
	if err != nil {
		if errors.Is(err, run.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "run_not_found",
				"no run with that id", map[string]any{"run_id": runID.String()})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get run failed", map[string]any{"error": err.Error()})
		return
	}

	s.writeJSON(w, r, http.StatusOK, toRunResponse(got))
}
