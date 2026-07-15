package server

import (
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// hostDispatchResponse is the 200 body of the host-dispatch marker endpoint
// (#1912). Transitioned is true when this call drove the stage
// pending|awaiting_host_dispatch → dispatched (the spawn marker), false on the
// idempotent no-op path (the stage was already 'dispatched' — a legal manual
// re-dispatch of a stage whose spawned runner died). StageState is the stage's
// state after the call.
type hostDispatchResponse struct {
	Transitioned bool   `json:"transitioned"`
	StageState   string `json:"stage_state"`
}

// handleHostDispatchStage implements
// POST /v0/runs/{run_id}/stages/{stage_id}/host-dispatch (#1912).
//
// It is the SPAWN MARKER for a runner_kind-locked-local run: the backend cannot
// spawn the host-local runner (ADR-024), so orchestrator.dispatchStage parks an
// agent stage at 'awaiting_host_dispatch' rather than 'dispatched'. The MCP
// host-spawn verbs (fishhawk_run_stage, fishhawk_dispatch_stage,
// fishhawk_drive_run) call this endpoint fail-closed IMMEDIATELY BEFORE spawning
// the runner, so post-#1912 'dispatched' unambiguously means "a spawn attempt
// exists". It CAS-transitions {pending, awaiting_host_dispatch} → dispatched:
//
//   - awaiting_host_dispatch → dispatched: the parked-local common case.
//   - pending → dispatched: the first plan-stage spawn, which today sits at
//     'pending' until trace time (the local first-stage semantics, #1030) —
//     marking it here stamps the spawn signal at spawn time.
//
// Idempotent: a stage already 'dispatched' returns 200 {transitioned:false} — a
// spawned runner died and the operator is re-dispatching, which the caller
// proceeds on. A running/terminal/awaiting_* gate state returns 409
// dispatch_not_admissible so a live or settled stage can never be re-marked.
//
// Eligibility (the #1912 fix-up): before the state CAS the endpoint validates
// that the target is a legitimate host-spawn — the marker's 'dispatched'
// meaning is only sound for a stage the backend would PARK for a host spawn.
// A run LOCKED to a non-local runner_kind (github_actions), a human-executed
// stage, and an auto-merge check-gate review stage each return 409
// dispatch_not_admissible: none is ever host-spawned, so marking it 'dispatched'
// would misrepresent state and could wedge the stage.
//
// Auth mirrors the reap-failure endpoint: an authenticated identity carrying
// write:runs. Anonymous → 401; an authenticated token without write:runs → 403;
// a cookie session with an empty TokenID is not scope-gated (matching the
// sibling write handlers). The operator/MCP token that drives dispatch already
// carries write:runs, so the auth-change impact inventory is empty.
func (s *Server) handleHostDispatchStage(w http.ResponseWriter, r *http.Request) {
	// Auth ladder BEFORE the nil-dependency guard (the #1915 revive convention)
	// so an anonymous caller gets 401 rather than a 503 that would leak
	// configuration state before authentication.
	id := IdentityFrom(r.Context())
	if id.IsAnonymous() {
		s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
			"an authenticated token is required", nil)
		return
	}
	if id.TokenID != "" && !hasScope(id, "write:runs") {
		s.writeError(w, r, http.StatusForbidden, "insufficient_scope",
			"token is missing required scope: write:runs",
			map[string]any{"required_scope": "write:runs"})
		return
	}

	if s.cfg.RunRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "host_dispatch_unconfigured",
			"host-dispatch endpoint requires a configured run repository", nil)
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

	// Load the stage and validate the (run_id, stage_id) handle: a stage whose
	// run_id differs from the path does not exist AT THIS PATH → 404.
	stage, err := s.cfg.RunRepo.GetStage(r.Context(), stageID)
	if err != nil {
		s.writeError(w, r, http.StatusNotFound, "stage_not_found",
			"stage does not exist", map[string]any{"stage_id": stageID.String()})
		return
	}
	if stage.RunID != runID {
		s.writeError(w, r, http.StatusNotFound, "stage_not_found",
			"stage does not belong to the supplied run",
			map[string]any{"stage_id": stageID.String(), "run_id": runID.String()})
		return
	}

	// Host-dispatch eligibility (#1912 fix-up): the marker stamps 'dispatched'
	// to mean "a host spawn attempt exists", which is only meaningful for a stage
	// the backend actually PARKS for a host-side spawn — an agent-executed stage
	// on a run destined for the LOCAL runner. Without these checks a write:runs
	// caller could mark a GitHub Actions stage, or a human/auto-merge review-gate
	// stage, 'dispatched' with no host spawn — breaking the meaning of
	// 'dispatched' and potentially wedging that stage. The two guards below
	// mirror the admission surface EXACTLY: orchestrator.dispatchStage's
	// park predicate (agent executor, not an auto-merge check-gate review) and
	// the MCP guardHostDispatch runner_kind posture (reject a run LOCKED to a
	// non-local kind; allow an un-resolved run, whose first host dispatch
	// auto-resolves it to local, and a locked-local run).
	runRow, err := s.cfg.RunRepo.GetRun(r.Context(), runID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"could not load the stage's run to validate host-dispatch eligibility",
			map[string]any{"run_id": runID.String(), "error": err.Error()})
		return
	}
	if runRow.RunnerKindResolved && runRow.RunnerKind != run.RunnerKindLocal {
		s.writeError(w, r, http.StatusConflict, "dispatch_not_admissible",
			"run is locked to a non-local runner_kind; host-dispatch marks a LOCAL host spawn",
			map[string]any{"run_id": runID.String(), "runner_kind": runRow.RunnerKind})
		return
	}
	if stage.ExecutorKind != run.ExecutorAgent || isAutoMergeReviewStage(stage) {
		s.writeError(w, r, http.StatusConflict, "dispatch_not_admissible",
			"stage is not an agent-executed host-spawn target (human or auto-merge review-gate stages are never host-spawned)",
			map[string]any{"stage_id": stageID.String(),
				"executor_kind": string(stage.ExecutorKind), "stage_type": string(stage.Type)})
		return
	}

	switch stage.State {
	case run.StageStateDispatched:
		// Idempotent no-op: a spawn attempt already exists. The manual
		// dead-runner re-dispatch lands here; the caller proceeds and re-spawns.
		s.writeJSON(w, r, http.StatusOK, hostDispatchResponse{
			Transitioned: false,
			StageState:   string(stage.State),
		})
		return
	case run.StageStatePending, run.StageStateAwaitingHostDispatch:
		// Admissible: mark the spawn. Fall through to the CAS below.
	default:
		// running / any terminal / any awaiting_* gate state: a live or settled
		// stage can never be re-marked as a fresh spawn.
		s.writeError(w, r, http.StatusConflict, "dispatch_not_admissible",
			"stage is not in a host-dispatchable state",
			map[string]any{"stage_id": stageID.String(), "state": string(stage.State)})
		return
	}

	// CAS the observed state → dispatched under the row lock (production
	// postgresRepo). A concurrent writer that flipped the stage between the load
	// and this call refuses atomically with StageStateChangedError rather than
	// being stomped; we re-classify below. In-memory fakes without the
	// capability fall back to the plain table-validated TransitionStage.
	from := stage.State
	var updated *run.Stage
	if cas, ok := s.cfg.RunRepo.(run.StageCASTransitioner); ok {
		updated, err = cas.TransitionStageFrom(r.Context(), stageID, from, run.StageStateDispatched, nil)
	} else {
		updated, err = s.cfg.RunRepo.TransitionStage(r.Context(), stageID, run.StageStateDispatched, nil)
	}
	if err != nil {
		// A concurrent writer changed the state under us. Re-load and honour the
		// same idempotency contract: if the winner already marked the spawn
		// (dispatched), return the benign no-op; otherwise the stage moved to a
		// non-admissible state → 409.
		var sce run.StageStateChangedError
		if errors.As(err, &sce) {
			if cur, gerr := s.cfg.RunRepo.GetStage(r.Context(), stageID); gerr == nil {
				if cur.State == run.StageStateDispatched {
					s.writeJSON(w, r, http.StatusOK, hostDispatchResponse{
						Transitioned: false,
						StageState:   string(cur.State),
					})
					return
				}
				s.writeError(w, r, http.StatusConflict, "dispatch_not_admissible",
					"stage is not in a host-dispatchable state",
					map[string]any{"stage_id": stageID.String(), "state": string(cur.State)})
				return
			}
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"could not mark the stage dispatched",
			map[string]any{"stage_id": stageID.String(), "state": string(from), "error": err.Error()})
		return
	}

	s.writeJSON(w, r, http.StatusOK, hostDispatchResponse{
		Transitioned: true,
		StageState:   string(updated.State),
	})
}

// isAutoMergeReviewStage mirrors orchestrator.isAutoMergeStage (unexported in
// that package): a review stage carrying a check-only gate is queued for GitHub
// auto-merge and walked straight to succeeded, never host-spawned. Replicated
// here so the host-dispatch endpoint refuses to mark such a stage 'dispatched'.
func isAutoMergeReviewStage(s *run.Stage) bool {
	return s.Type == run.StageTypeReview && s.Gate != nil && s.Gate.Kind == run.GateKindCheck
}
