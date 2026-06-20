package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/drive"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// maxRunStageWaitSeconds caps the opt-in server-side long-poll on
// GET /v0/runs/{run_id}/stages/{stage_id} (?wait=<seconds>, #1252). A
// single ?wait holds the connection at most this long before returning
// the stage's current (still-unsettled) state; the watcher re-issues to
// extend toward its own total budget. Deliberately modest and
// forward-safe — same posture and value as the scope-amendment ?wait
// (maxScopeAmendmentWaitSeconds): the fishhawkd http.Server sets only
// ReadHeaderTimeout (no WriteTimeout), so a held wait well under any
// future WriteTimeout stays correct.
const maxRunStageWaitSeconds = 30

// runStageWaitPollInterval is how often the ?wait long-poll re-reads the
// stage to detect settlement. A package var (not const) so tests can
// shorten it; production keeps the modest default.
var runStageWaitPollInterval = 500 * time.Millisecond

// runStageWaitResponse is the 200 body for GET
// /v0/runs/{run_id}/stages/{stage_id}. It embeds the canonical Stage
// shape and adds the wait envelope: the echoed state, a terminal flag
// (true when the stage IsSettled — terminal OR parked, the signal a
// detached watcher waits for), and the run's distilled next operator
// action (best-effort, omitted for non-drive or terminal runs).
type runStageWaitResponse struct {
	stageResponse
	// State echoes the stage state at the top level so a watcher polling
	// for settlement doesn't have to reach into the embedded stage shape.
	State string `json:"state"`
	// Terminal is true when the stage has settled (IsSettled): a terminal
	// state OR a parked-for-operator state. It is the one authoritative
	// signal the detached watcher blocks on.
	Terminal bool `json:"terminal"`
	// NextAction is the run's distilled next operator step, reusing the
	// same drive-surface distillation GET /v0/runs/{run_id} applies.
	// Best-effort: omitted for non-drive runs, terminal runs, and on any
	// read failure — never fabricated.
	NextAction *runNextActionPayload `json:"next_action,omitempty"`
}

// handleGetRunStage implements GET /v0/runs/{run_id}/stages/{stage_id}.
//
// It resolves a stage by the durable ADR-037 (run_id, stage_id) handle
// and, when ?wait=<seconds> is given, blocks server-side up to the cap
// until the stage settles (terminal OR parked) — the REST analogue of
// the scope-amendment decider's ?wait long-poll, applied to stage
// settledness so a detached operator-side watcher has ONE authoritative
// completion signal (#1252). Composes existing repository reads only; no
// orchestration, runner, or MCP-tool contract changes.
//
// Auth mirrors handleListScopeAmendments: anonymous => 401; a run-bound
// fhm_ token must carry mcp:read (else 403 insufficient_scope) AND match
// the path run_id (else 403 cross_run_stage); operator bearers/sessions
// pass. The (run_id, stage_id) handle must be consistent: a stage whose
// RunID != the path run_id is 404 stage_not_found.
func (s *Server) handleGetRunStage(w http.ResponseWriter, r *http.Request) {
	if s.cfg.RunRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "run_repo_unconfigured",
			"stage endpoint requires a configured run repository", nil)
		return
	}

	id := IdentityFrom(r.Context())
	if id.IsAnonymous() {
		s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
			"an authenticated token is required", nil)
		return
	}

	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}
	stageID, err := uuid.Parse(r.PathValue("stage_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"stage_id must be a valid UUID",
			map[string]any{"field": "stage_id", "got": r.PathValue("stage_id")})
		return
	}

	// A run-bound MCP token may only read its own run's stages, and only
	// with mcp:read; operator bearers/sessions (not run-bound) pass.
	if tokenRunID, runBound := runBoundTokenRunID(id); runBound {
		if !hasScope(id, "mcp:read") {
			s.writeError(w, r, http.StatusForbidden, "insufficient_scope",
				"token is missing required scope: mcp:read",
				map[string]any{"required_scope": "mcp:read"})
			return
		}
		if tokenRunID != runID {
			s.writeError(w, r, http.StatusForbidden, "cross_run_stage",
				"mcp token may only read stages for its own run",
				map[string]any{
					"token_run_id": tokenRunID.String(),
					"path_run_id":  runID.String(),
				})
			return
		}
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
	// The (run_id, stage_id) handle must be consistent: a stage that
	// exists but belongs to a different run reads as not-found under this
	// run (it does not exist at this path).
	if stage.RunID != runID {
		s.writeError(w, r, http.StatusNotFound, "stage_not_found",
			"no stage with that id for this run",
			map[string]any{"stage_id": stageID.String(), "run_id": runID.String()})
		return
	}

	// Opt-in, bounded server-side long-poll: ?wait=<seconds> holds the
	// connection until the stage settles (terminal OR parked) or the wait
	// cap elapses. wait<=0/absent and an already-settled stage both return
	// immediately.
	if wait := parseRunStageWaitSeconds(r); wait > 0 && !stage.State.IsSettled() {
		stage = s.awaitStageSettled(r, stageID, wait, stage)
	}

	resp := runStageWaitResponse{
		stageResponse: toStageResponse(stage),
		State:         string(stage.State),
		Terminal:      stage.State.IsSettled(),
		NextAction:    s.stageNextAction(r.Context(), runID),
	}
	s.writeJSON(w, r, http.StatusOK, resp)
}

// parseRunStageWaitSeconds reads and clamps the optional ?wait query
// param to [0, maxRunStageWaitSeconds]. A missing, non-integer, or
// non-positive value reads as 0 (no wait) so the param is purely
// additive — no new error code, unchanged non-wait envelope.
func parseRunStageWaitSeconds(r *http.Request) int {
	raw := strings.TrimSpace(r.URL.Query().Get("wait"))
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0
	}
	if n > maxRunStageWaitSeconds {
		return maxRunStageWaitSeconds
	}
	return n
}

// awaitStageSettled is the ?wait poll loop, modeled on
// awaitScopeAmendmentDecision: it re-reads the stage on
// runStageWaitPollInterval and returns the moment GetStage reports a
// settled (terminal OR parked) state, or when the wait cap elapses
// (returning the last-read still-unsettled stage so the watcher
// re-issues), or when the request context is canceled (client
// disconnect). On a transient re-read error it returns the last-good
// stage — best-effort, never regressing the wait to a 500.
func (s *Server) awaitStageSettled(r *http.Request, stageID uuid.UUID, waitSeconds int, current *run.Stage) *run.Stage {
	deadline := time.After(time.Duration(waitSeconds) * time.Second)
	ticker := time.NewTicker(runStageWaitPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return current
		case <-deadline:
			return current
		case <-ticker.C:
			got, err := s.cfg.RunRepo.GetStage(r.Context(), stageID)
			if err != nil {
				return current
			}
			current = got
			if got.State.IsSettled() {
				return current
			}
		}
	}
}

// stageNextAction distills the run's latest operator next-action from
// its run_auto_advanced (drive.Category) audit entries, reusing the
// applyDriveSurfaces distillation GET /v0/runs/{run_id} applies. Returns
// nil — the field is omitted — for non-drive runs, when the audit repo
// is not wired, and best-effort on any read failure (so a stage read
// never fails because the next-action could not be derived). Terminal
// runs naturally yield nil because applyDriveSurfaces suppresses
// next_action once the run completes.
func (s *Server) stageNextAction(ctx context.Context, runID uuid.UUID) *runNextActionPayload {
	got, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil {
		return nil
	}
	if !got.Drive || s.cfg.AuditRepo == nil {
		return nil
	}
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, drive.Category)
	if err != nil {
		return nil
	}
	var resp runResponse
	applyDriveSurfaces(&resp, got, entries)
	return resp.NextAction
}
