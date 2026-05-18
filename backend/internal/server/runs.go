package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// runResponse is the JSON shape POST /v0/runs and GET /v0/runs/{id}
// return. Field names + types match docs/api/v0.openapi.yaml's
// `Run` schema exactly so there's never a translation step between
// the OpenAPI doc and the wire format.
type runResponse struct {
	ID                 uuid.UUID  `json:"id"`
	Repo               string     `json:"repo"`
	WorkflowID         string     `json:"workflow_id"`
	WorkflowSHA        string     `json:"workflow_sha"`
	TriggerSource      string     `json:"trigger_source"`
	TriggerRef         *string    `json:"trigger_ref"`
	State              string     `json:"state"`
	ParentRunID        *uuid.UUID `json:"parent_run_id,omitempty"`
	PullRequestURL     *string    `json:"pull_request_url,omitempty"`
	RetryAttempt       int        `json:"retry_attempt"`
	MaxRetriesSnapshot int        `json:"max_retries_snapshot"`
	RunnerKind         string     `json:"runner_kind"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

func toRunResponse(r *run.Run) runResponse {
	return runResponse{
		ID:                 r.ID,
		Repo:               r.Repo,
		WorkflowID:         r.WorkflowID,
		WorkflowSHA:        r.WorkflowSHA,
		TriggerSource:      string(r.TriggerSource),
		TriggerRef:         r.TriggerRef,
		State:              string(r.State),
		ParentRunID:        r.ParentRunID,
		PullRequestURL:     r.PullRequestURL,
		RetryAttempt:       r.RetryAttempt,
		MaxRetriesSnapshot: r.MaxRetriesSnapshot,
		RunnerKind:         r.RunnerKind,
		CreatedAt:          r.CreatedAt,
		UpdatedAt:          r.UpdatedAt,
	}
}

// createRunRequest mirrors POST /v0/runs's request body in
// v0.openapi.yaml. All four required fields must be present and
// non-empty; trigger_ref and runner_kind are optional.
type createRunRequest struct {
	Repo          string  `json:"repo"`
	WorkflowID    string  `json:"workflow_id"`
	WorkflowSHA   string  `json:"workflow_sha"`
	TriggerSource string  `json:"trigger_source"`
	TriggerRef    *string `json:"trigger_ref,omitempty"`
	// RunnerKind tags the execution backend per ADR-022 / #388.
	// Optional; defaults to github_actions when omitted (the v0
	// dominant case). The local-runner CLI (Phase C of E22 / #389)
	// passes `local`. Validated against `run.ValidRunnerKinds` at
	// the handler.
	RunnerKind string `json:"runner_kind,omitempty"`
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
	if req.RunnerKind != "" {
		if _, ok := run.ValidRunnerKinds[req.RunnerKind]; !ok {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"runner_kind must be one of github_actions, local",
				map[string]any{"field": "runner_kind", "got": req.RunnerKind})
			return
		}
	}

	// Idempotency-Key (E8.2 / #40). When set, a previously-created
	// run with the same (repo, key) is returned 200 instead of
	// fresh-creating + dispatching a duplicate. Empty header is
	// equivalent to "not idempotent" — every call mints a new run.
	idempKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempKey != "" {
		existing, err := s.cfg.RunRepo.GetRunByIdempotencyKey(r.Context(), req.Repo, idempKey)
		switch {
		case err == nil:
			// Replay: return the prior run with 200 (not 201).
			// 200 is the idempotency convention — clients that
			// react to "201 Created" by, e.g., posting a Slack
			// notification get a chance to no-op on the replay.
			s.writeJSON(w, r, http.StatusOK, toRunResponse(existing))
			return
		case errors.Is(err, run.ErrNotFound):
			// First call with this key — fall through to create.
		default:
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"idempotency lookup failed", map[string]any{"error": err.Error()})
			return
		}
	}

	createParams := run.CreateRunParams{
		Repo:          req.Repo,
		WorkflowID:    req.WorkflowID,
		WorkflowSHA:   req.WorkflowSHA,
		TriggerSource: run.TriggerSource(req.TriggerSource),
		TriggerRef:    req.TriggerRef,
		// Empty req.RunnerKind → repo layer applies the default
		// (RunnerKindGitHubActions). Explicit values are validated
		// above; only known-good kinds reach the repo.
		RunnerKind: req.RunnerKind,
	}
	if idempKey != "" {
		k := idempKey
		createParams.IdempotencyKey = &k
	}

	created, err := s.cfg.RunRepo.CreateRun(r.Context(), createParams)
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

const (
	runsDefaultLimit = 50
	runsMaxLimit     = 200
)

// handleListRuns implements GET /v0/runs. Cursor-paginated by
// created_at DESC; filter params (repo, workflow_id, state) are
// additive — multiple filters AND together. Cursor encoding is
// shared with the audit endpoint via pageOffset / encodeOffsetCursor.
func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	if s.cfg.RunRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "run_repo_unconfigured",
			"runs endpoint requires a configured run repository", nil)
		return
	}
	q := r.URL.Query()
	limit, err := parseLimit(q.Get("limit"), runsDefaultLimit, runsMaxLimit)
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			err.Error(), map[string]any{"field": "limit"})
		return
	}
	offset, err := decodeOffsetCursor(q.Get("cursor"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "cursor_invalid",
			err.Error(), nil)
		return
	}
	stateFilter := q.Get("state")
	if stateFilter != "" {
		if _, ok := validRunStates[stateFilter]; !ok {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"state must be one of pending, running, succeeded, failed, cancelled",
				map[string]any{"field": "state", "got": stateFilter})
			return
		}
	}

	// pull_request_url and trigger_ref are optional equality
	// filters introduced in #216 for the threaded-runs view.
	// Empty value = no constraint (matches the SQL convention);
	// any non-empty value is passed verbatim.
	var prURLFilter *string
	if v := q.Get("pull_request_url"); v != "" {
		prURLFilter = &v
	}
	var triggerRefFilter *string
	if v := q.Get("trigger_ref"); v != "" {
		triggerRefFilter = &v
	}
	// runner_kind is the ADR-022 / #388 filter — compliance
	// consumers project to `github_actions` only to reproduce the
	// pre-pluggable-backends view. Validated against the closed
	// set so bad values surface as a clean 400 (not a silent
	// no-results page).
	var runnerKindFilter *string
	if v := q.Get("runner_kind"); v != "" {
		if _, ok := run.ValidRunnerKinds[v]; !ok {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"runner_kind must be one of github_actions, local",
				map[string]any{"field": "runner_kind", "got": v})
			return
		}
		runnerKindFilter = &v
	}

	// Fetch one extra row so we can tell whether there's a next
	// page without a separate COUNT query. The trick: ask for
	// limit+1, drop the extra in the response if present.
	rows, err := s.cfg.RunRepo.ListRuns(r.Context(), run.ListRunsFilter{
		Repo:           q.Get("repo"),
		WorkflowID:     q.Get("workflow_id"),
		State:          stateFilter,
		PullRequestURL: prURLFilter,
		TriggerRef:     triggerRefFilter,
		RunnerKind:     runnerKindFilter,
		Limit:          limit + 1,
		Offset:         offset,
	})
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"list runs failed", map[string]any{"error": err.Error()})
		return
	}

	var nextCursor string
	if len(rows) > limit {
		nextCursor = encodeOffsetCursor(offset + limit)
		rows = rows[:limit]
	}
	items := make([]runResponse, 0, len(rows))
	for _, ru := range rows {
		items = append(items, toRunResponse(ru))
	}
	s.writeJSON(w, r, http.StatusOK, map[string]any{
		"items":       items,
		"next_cursor": nextCursor,
	})
}

// validRunStates pins the closed set per docs/api/v0.openapi.yaml.
// Schema constraint mirrors backend/internal/postgres/migrations/0001
// CHECK; defense-in-depth at the handler keeps a typo from
// reaching the DB layer.
var validRunStates = map[string]struct{}{
	string(run.StatePending):   {},
	string(run.StateRunning):   {},
	string(run.StateSucceeded): {},
	string(run.StateFailed):    {},
	string(run.StateCancelled): {},
}

// handleCancelRun implements POST /v0/runs/{run_id}/cancel.
// Idempotent: cancelling an already-cancelled run returns 200 with
// the same body as a fresh cancel. Cancelling a terminally-completed
// run (succeeded / failed) returns 409 because the state machine
// rejects the transition.
func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
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

	got, err := s.cfg.RunRepo.TransitionRun(r.Context(), runID, run.StateCancelled)
	if err != nil {
		if errors.Is(err, run.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "run_not_found",
				"no run with that id", map[string]any{"run_id": runID.String()})
			return
		}
		var inv run.InvalidTransitionError
		if errors.As(err, &inv) {
			s.writeError(w, r, http.StatusConflict, "invalid_state_transition",
				err.Error(),
				map[string]any{"run_id": runID.String(), "from": inv.From, "to": inv.To})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"cancel run failed", map[string]any{"error": err.Error()})
		return
	}

	s.writeJSON(w, r, http.StatusOK, toRunResponse(got))
}
