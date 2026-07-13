package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// CategoryDispatchReaperFailed is the audit-log category for the chained entry
// the reap-failure endpoint writes when the MCP host's detached reaper reports
// a spawn-phase runner failure (#1747). Defined LOCALLY here rather than in the
// audit package (per the operator's binding approval condition): audit.category
// is a free-form TEXT column with no enum/CHECK and there is no central
// category registry, so a new category needs no registry coupling. Mirrors the
// dispatchwatchdog.CategoryDispatchWatchdogElapsed precedent — a stable string
// so log scrapers can index on it.
const CategoryDispatchReaperFailed = "dispatch_reaper_failed"

// maxReapFailureBodyBytes caps the request body. The reap-failure report is a
// handful of small fields (category, reason, detail, exit_code), so 32 KB is
// well above any realistic payload and well below trace's 64 MiB cap.
const maxReapFailureBodyBytes = 32 * 1024

// reapFailureRequest is the wire shape the MCP host's detached reaper POSTs
// (#1747). category is exactly "B" or "C" (mirroring pullrequest.go's
// failed-outcome validation); reason is required; detail and exit_code are
// optional diagnostics carrying the parsed runner_failed line and the child's
// process exit code.
type reapFailureRequest struct {
	Category string `json:"category"`
	Reason   string `json:"reason"`
	Detail   string `json:"detail,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
}

// reapFailureResponse is the 200 body. Transitioned is false on the idempotent
// no-op path (the stage was already terminal — a double-report or a race with
// the dispatch watchdog), true when this call drove the stage to failed.
type reapFailureResponse struct {
	Transitioned bool   `json:"transitioned"`
	StageState   string `json:"stage_state"`
}

// handleReapStageFailure implements
// POST /v0/runs/{run_id}/stages/{stage_id}/reap-failure (#1747).
//
// The detached fishhawk_dispatch_stage reaper (backend/cmd/fishhawk-mcp
// run_stage.go::spawnRunnerStageDetached) calls this over HTTP when a spawned
// runner exits non-zero BEFORE reporting a terminal stage state (e.g. an
// acceptance pre-flight provision failure). Without it the stage stays
// 'dispatched' forever: retry_stage 422s and no audit entry is written. This is
// the eager, event-driven complement to the off-by-default ~1h dispatch
// watchdog, and it mirrors that watchdog's contract exactly: run.FailStage
// (category C is the retryable infrastructure class) -> AppendChained the
// dispatch_reaper_failed audit entry -> orchestrator.Advance, with the same
// best-effort logging order.
//
// Idempotent: a report against an already-terminal stage is a benign no-op
// (200 {transitioned:false}) with NO audit entry and NO advance. A report
// against an awaiting_children stage is the same benign no-op (#1891): that
// state is a live decomposition park owned by its children, and failing it
// would destroy the fan-in park a doomed mis-dispatched runner never owned.
func (s *Server) handleReapStageFailure(w http.ResponseWriter, r *http.Request) {
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "reap_failure_unconfigured",
			"reap-failure endpoint requires run and audit repositories", nil)
		return
	}

	// Auth: an authenticated identity carrying write:runs. Mirrors the
	// consolidate handler's operator-write gate — anonymous → 401; an
	// authenticated token without write:runs → 403. A cookie session with an
	// empty TokenID is not scope-gated (matching the sibling write handlers'
	// bypass); the operator/MCP token that drives dispatch already carries
	// write:runs, so the impact inventory is empty.
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

	body, err := io.ReadAll(io.LimitReader(r.Body, maxReapFailureBodyBytes+1))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"could not read request body", map[string]any{"error": err.Error()})
		return
	}
	if len(body) > maxReapFailureBodyBytes {
		s.writeError(w, r, http.StatusRequestEntityTooLarge, "body_too_large",
			"reap-failure body exceeds size cap",
			map[string]any{"limit_bytes": maxReapFailureBodyBytes})
		return
	}

	var req reapFailureRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"reap-failure body could not be decoded",
			map[string]any{"error": err.Error()})
		return
	}

	// Category is exactly B or C (mirroring pullrequest.go's failed-outcome
	// validation). C is the retryable infrastructure class the reaper always
	// sends for a process-level non-zero exit; B is accepted for completeness.
	// A or an empty/unknown value is a 400.
	var cat run.FailureCategory
	switch req.Category {
	case "B":
		cat = run.FailureB
	case "C":
		cat = run.FailureC
	default:
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			`category must be "B" or "C"`,
			map[string]any{"field": "category", "got": req.Category})
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"reason is required", map[string]any{"field": "reason"})
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

	// Idempotent no-op: a stage that already reached a terminal state (a
	// double-report, or a race with the dispatch watchdog reaping the same
	// stuck stage) needs no transition, no audit entry, and no advance. Return
	// 200 {transitioned:false} so the reaper treats a duplicate as benign. This
	// is the pre-check the plan calls for — reaching FailStage on a terminal
	// stage would return a transition error, which we would otherwise have to
	// classify here.
	if stage.State.IsTerminal() {
		s.writeJSON(w, r, http.StatusOK, reapFailureResponse{
			Transitioned: false,
			StageState:   string(stage.State),
		})
		return
	}

	// Protected-park FAST PATH (#1891): a stage already in awaiting_children
	// at load time is a LIVE decomposition park owned by its child slices,
	// not a stuck spawn. A spawn-phase failure report can reach here when a
	// runner is (mis-)dispatched against a decomposed parent's implement
	// stage — the doomed runner 409s on prompt fetch and its detached reaper
	// reports the exit. Return the benign no-op WITHOUT reaching FailStage:
	// 200 {transitioned:false}, NO dispatch_reaper_failed audit entry, NO
	// orchestrator advance.
	//
	// This pre-check is the fast path, NOT the sole guarantor of the
	// park-protection invariant — it only catches a park already visible at
	// load. The invariant is actually held by run.FailStage itself: it
	// REFUSES an awaiting_children stage up-front (ErrStageParked) and drives
	// its transitions through a row-locked compare-and-swap, so a park that
	// lands AFTER this pre-check (the residual TOCTOU, #1903) is refused
	// there rather than destroyed. Both refusal shapes surface in the
	// post-FailStage error branch below, where the re-load classifies them as
	// the same benign no-op. The pre-check remains as the cheap common case
	// and as the fail-closed backstop even if the MCP admission guard
	// (guardSiblingStageInFlight) is skipped.
	if stage.State == run.StageStateAwaitingChildren {
		s.writeJSON(w, r, http.StatusOK, reapFailureResponse{
			Transitioned: false,
			StageState:   string(stage.State),
		})
		return
	}

	// Fail the stage → append the dispatch_reaper_failed audit entry → advance
	// the run, in the exact order and with the exact best-effort logging the
	// dispatch watchdog uses (dispatchwatchdog.go). FailStage walks the canonical
	// path from whichever non-terminal state the stage is in (e.g. dispatched →
	// running → failed), so the spawn-phase 'dispatched' case is handled.
	if _, err := run.FailStage(r.Context(), s.cfg.RunRepo, stageID, cat, req.Reason); err != nil {
		// This branch now fires for a NARROW, well-classified set, because
		// FailStage's #1907 re-anchor loop ABSORBS every benign concurrent
		// ADVANCE to a still-live, legally-failable state (e.g. a
		// dispatched → running or running → awaiting_approval flip landing
		// mid-window): those no longer reach here as a refusal — FailStage
		// re-anchors and lands failed, so this call returns success. What
		// still lands here is:
		//
		//   (a) A concurrent writer SETTLED the stage terminal (a double-report
		//       or a race with the dispatch watchdog / runner's own terminal
		//       report) — FailStage returns the typed StageStateChangedError
		//       unchanged. Re-load: the stage is terminal, the winner did the
		//       work, so return the benign {transitioned:false} no-op — no audit
		//       entry, no advance.
		//   (b) A concurrent fanout PARKED the stage awaiting_children (the
		//       decomposed-parent race, #1903). FailStage REFUSES that park
		//       rather than taking the legal awaiting_children → failed edge and
		//       destroying it — either up-front (ErrStageParked, park visible at
		//       FailStage's load) or via the row-locked CAS
		//       (StageStateChangedError, park landing mid-flight OR after a
		//       re-anchor). Re-load: the stage is a live park, so return the same
		//       benign no-op. Never fail a restored/live park (#1891/#1903).
		//   (c) FailStage retry EXHAUSTION under pathological livelock, or a
		//       genuine repo error — the stage is still non-terminal and
		//       non-park, so the re-load falls through to the 500 below. That
		//       500 is the DELIBERATE, retryable contract (#1907): the detached
		//       reaper may re-POST, and the ~1h dispatch watchdog is the eventual
		//       backstop for a genuinely stuck stage.
		//
		// The re-load's terminal-or-park check is exactly the benign set (a)+(b);
		// no classification code changed for the #1907 semantics.
		if cur, gerr := s.cfg.RunRepo.GetStage(r.Context(), stageID); gerr == nil &&
			(cur.State.IsTerminal() || cur.State == run.StageStateAwaitingChildren) {
			s.writeJSON(w, r, http.StatusOK, reapFailureResponse{
				Transitioned: false,
				StageState:   string(cur.State),
			})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"could not transition the stage to failed",
			map[string]any{"stage_id": stageID.String(), "state": string(stage.State), "error": err.Error()})
		return
	}

	stageIDCopy := stageID
	systemKind := audit.ActorSystem
	auditPayload, _ := json.Marshal(map[string]any{
		"run_id":           runID.String(),
		"stage_id":         stageID.String(),
		"failure_category": string(cat),
		"reason":           req.Reason,
		"detail":           req.Detail,
		"exit_code":        req.ExitCode,
		"reported_at":      time.Now().UTC().Format(time.RFC3339Nano),
		"auth_method":      "bearer",
	})
	if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageIDCopy,
		Timestamp: time.Now().UTC(),
		Category:  CategoryDispatchReaperFailed,
		ActorKind: &systemKind,
		Payload:   auditPayload,
	}); err != nil {
		// State is already failed; surface the audit gap loudly but do NOT
		// unwind the transition — mirrors the watchdog's chain-integrity posture.
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelError,
			"reap-failure: append audit entry failed (state changed without entry)",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()))
	}

	// Walk the run's state machine so a run whose only dispatched stage is now
	// failed doesn't sit in pending/running forever. Best-effort, like the
	// watchdog.
	if s.cfg.Orchestrator != nil {
		if _, err := s.cfg.Orchestrator.Advance(r.Context(), runID); err != nil {
			s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
				"reap-failure: orchestrator advance failed",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stageID.String()),
				slog.String("error", err.Error()))
		}
	}

	s.writeJSON(w, r, http.StatusOK, reapFailureResponse{
		Transitioned: true,
		StageState:   string(run.StageStateFailed),
	})
}
