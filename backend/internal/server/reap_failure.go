package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// errReapRepoNotCAS is the sentinel failStageForReap returns when the run
// repository does NOT implement run.StageCASTransitioner (#2672). The reap path
// hard-REQUIRES that capability: its only alternative, run.FailStage, re-anchors
// its walk through whatever live state a concurrent advance produced and would
// take the legal park → failed edge on the four non-children parks — exactly the
// live-park destruction GUARD 2 exists to prevent. Rather than degrade to that
// hazard, the reap path REFUSES loudly. handleReapStageFailure classifies this
// sentinel BEFORE its post-transition re-load so a concurrent park cannot mask a
// misconfiguration as the benign {transitioned:false} no-op. The condition is
// unreachable in a deployed daemon: postgresRepo implements the capability (the
// compile-time assertion in run/postgres.go guarantees it), and serve.go's boot
// check (runRepoCASWiringError) refuses startup for any non-nil RunRepo lacking
// it.
var errReapRepoNotCAS = errors.New("reap path requires a run repository implementing run.StageCASTransitioner")

// maxReapFailureBodyBytes caps the request body. The reap-failure report is a
// handful of small fields (category, reason, detail, exit_code), so 32 KB is
// well above any realistic payload and well below trace's 64 MiB cap.
const maxReapFailureBodyBytes = 32 * 1024

// reapProtectedParkStates is the set of NON-terminal stage states the reap
// handler must NEVER collapse to failed — the ONE shared predicate behind BOTH
// reap guards (#2630). Each is a LIVE park owned by a resolver OTHER than the
// reaper: a runner legitimately exits 0 into it, or a concurrent advance drives
// the stage into it. The reaper's authority is exactly {dispatched, running} —
// the states that mean a spawned runner exited WITHOUT settling the stage; every
// other non-terminal state is a park it must leave alone.
//
//   - awaiting_children       — decomposition fan-in park (#1891/#1903)
//   - awaiting_approval       — plan/gate park
//   - awaiting_input          — clarification park
//   - awaiting_scope_decision — scope-completeness park
//   - awaiting_host_dispatch  — local-spawn park (#1912)
//
// This GENERALIZES the pre-#2630 awaiting_children-only protection to all five
// through one predicate: a sixth park state is protected by being added HERE
// rather than by remembering to touch a fifth open-coded comparison. Keeping the
// set closed (an explicit allow-list of parks, not !IsTerminal) is deliberate —
// a future non-terminal state a runner exits into that is NOT a park (a new
// reapable phase) must fail CLOSED to reapable, not silently protected.
var reapProtectedParkStates = map[run.StageState]bool{
	run.StageStateAwaitingChildren:      true,
	run.StageStateAwaitingApproval:      true,
	run.StageStateAwaitingInput:         true,
	run.StageStateAwaitingScopeDecision: true,
	run.StageStateAwaitingHostDispatch:  true,
}

// isReapProtectedPark reports whether state is a live park the reap handler must
// never reap. It is the shared predicate for GUARD 1 (the load-time fast path)
// and GUARD 2 (the reap-scoped CAS re-anchor refusal in reapFailCAS).
func isReapProtectedPark(state run.StageState) bool {
	return reapProtectedParkStates[state]
}

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

	// Protected-park FAST PATH — GUARD 1 of 2 (#1891/#1903, generalized for the
	// #2630 stale-probe TOCTOU): a stage already in ANY protected park at load
	// time is a LIVE park owned by a resolver other than the reaper, not a stuck
	// spawn. Return the benign no-op WITHOUT reaching the transition: 200
	// {transitioned:false}, NO dispatch_reaper_failed audit entry, NO orchestrator
	// advance.
	//
	// This is the AUTHORITATIVE close of the introduced TOCTOU: the detached
	// reaper's zero-exit strand probe (mcpserver run_stage.go) is read BEFORE this
	// POST is sent, so the stage can move running/dispatched → a park in the
	// seconds between that probe and this handler processing the report. Each
	// retry is a FRESH POST re-checked here, so a park that arrives between report
	// attempts is caught too. The runner-side allow-list probe narrows but cannot
	// close that race — no re-probe is atomic with a transition happening on the
	// server; only this handler, re-reading the CURRENT state, is. So a stale
	// positive probe never reaps a park that is live when the report lands.
	//
	// GUARD 1 only catches a park VISIBLE at load. A park landing AFTER this
	// pre-check but before the transition (the mid-transition half of the TOCTOU)
	// is refused by GUARD 2 (failStageForReap), whose refusal surfaces in the
	// post-transition error branch below and re-loads to the same benign no-op.
	// Pre-#2630 only awaiting_children was protected here (and only it was refused
	// mid-flight by run.FailStage); the four other parks were reaped.
	if isReapProtectedPark(stage.State) {
		s.writeJSON(w, r, http.StatusOK, reapFailureResponse{
			Transitioned: false,
			StageState:   string(stage.State),
		})
		return
	}

	// Fail the stage → append the dispatch_reaper_failed audit entry → advance
	// the run, in the exact order and with the exact best-effort logging the
	// dispatch watchdog uses (dispatchwatchdog.go). failStageForReap walks the
	// canonical path from whichever reapable state the stage is in (e.g.
	// dispatched → running → failed), so the spawn-phase 'dispatched' case is
	// handled — but, UNLIKE run.FailStage, it refuses to re-anchor into any
	// protected park that lands mid-transition (GUARD 2, #2630).
	if _, err := failStageForReap(r.Context(), s.cfg.RunRepo, stageID, stage.State, cat, req.Reason); err != nil {
		// (d) WIRING FAULT — the run repository does not implement
		// run.StageCASTransitioner (#2672). This is classified FIRST, ahead of
		// the re-load below, and is load-bearing: the re-load's terminal-or-park
		// check would otherwise report a misconfiguration as the benign
		// {transitioned:false} 200 whenever a concurrent writer parked the stage,
		// collapsing an uncertain configuration fault into a definite no-op
		// answer. 503 matches the endpoint's existing wiring-fault class
		// (reap_failure_unconfigured); the distinct CODE is what pins this arm. No
		// audit entry and no orchestrator advance are written — the early return
		// precedes both. Unreachable in a booted daemon (postgres has the
		// capability and the boot check refuses a repo that does not).
		if errors.Is(err, errReapRepoNotCAS) {
			s.cfg.Logger.LogAttrs(r.Context(), slog.LevelError,
				"reap-failure: run repository does not implement run.StageCASTransitioner",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stageID.String()),
				slog.String("repo_type", fmt.Sprintf("%T", s.cfg.RunRepo)))
			s.writeError(w, r, http.StatusServiceUnavailable, "reap_failure_repo_not_cas",
				"reap-failure endpoint requires a run repository implementing run.StageCASTransitioner",
				map[string]any{"stage_id": stageID.String()})
			return
		}
		// This branch fires for a NARROW, well-classified set, because
		// failStageForReap's re-anchor loop ABSORBS every benign concurrent
		// ADVANCE to another still-live, REAPABLE state (e.g. a dispatched →
		// running flip landing mid-window): those no longer reach here as a
		// refusal — the loop re-anchors and lands failed, so this call returns
		// success. What still lands here is:
		//
		//   (a) A concurrent writer SETTLED the stage terminal (a double-report
		//       or a race with the dispatch watchdog / runner's own terminal
		//       report) — the typed StageStateChangedError is returned unchanged.
		//       Re-load: the stage is terminal, the winner did the work, so return
		//       the benign {transitioned:false} no-op — no audit entry, no advance.
		//   (b) A concurrent writer PARKED the stage in any protected park — the
		//       #1903 decomposed-parent race (awaiting_children), OR the #2630
		//       running/dispatched → awaiting_approval/input/scope_decision race
		//       (the mid-transition half of the stale-probe TOCTOU).
		//       failStageForReap REFUSES the park rather than taking the legal
		//       park → failed edge and destroying it — either up-front (park
		//       visible at its load) or via the row-locked CAS (park landing
		//       mid-flight OR after a re-anchor). Re-load: the stage is a live
		//       park, so return the same benign no-op. Never fail a live park
		//       (#1891/#1903/#2630).
		//   (c) Retry EXHAUSTION under pathological livelock, or a genuine repo
		//       error — the stage is still non-terminal and non-park, so the
		//       re-load falls through to the 500 below. That 500 is the DELIBERATE,
		//       retryable contract (#1907): the detached reaper may re-POST, and
		//       the ~1h dispatch watchdog is the eventual backstop for a genuinely
		//       stuck stage.
		//
		// The re-load's terminal-or-park check is exactly the benign set (a)+(b),
		// using the same shared isReapProtectedPark predicate GUARD 2 refuses on.
		if cur, gerr := s.cfg.RunRepo.GetStage(r.Context(), stageID); gerr == nil &&
			(cur.State.IsTerminal() || isReapProtectedPark(cur.State)) {
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

// reapFailMaxAttempts bounds reapFailCAS's re-anchor loop, mirroring run's
// failStageCASMaxAttempts: each attempt absorbs one benign concurrent advance to
// another reapable state; four is far beyond any realistic interleaving yet caps
// a pathological livelock so the loop terminates and the handler returns the
// documented 500-and-retry contract.
const reapFailMaxAttempts = 4

// failStageForReap is the reap-scoped compare-and-swap fail — GUARD 2 of 2
// (#2630). It is the reap path's replacement for run.FailStage. run.FailStage
// RE-ANCHORS its walk through whatever live state a concurrent advance produced
// and, for the four non-children parks (awaiting_approval / awaiting_input /
// awaiting_scope_decision / awaiting_host_dispatch), takes the legal park →
// failed edge — its reanchorTarget refuses ONLY awaiting_children, because those
// four ARE legitimately failable by their gate owners (the approval-SLA path, the
// deploy trigger, the trace-policy path). That absorption is correct for THOSE
// callers but WRONG for the reaper: a park landing between this handler's load
// and the transition is a LIVE park the reaper must not collapse (the
// mid-transition half of the #2630 stale-probe TOCTOU). So this walk refuses to
// re-anchor into ANY protected park (isReapProtectedPark) — awaiting_children as
// before, plus the four others — while still ABSORBING a benign advance to
// another reapable state (dispatched → running) exactly as FailStage does, so a
// genuinely-stuck stage is still reaped. A refusal surfaces the typed
// StageStateChangedError unchanged for the handler's post-transition re-load to
// classify as the benign no-op.
//
// run.FailStage is intentionally left UNCHANGED: reversing its #1907 absorption
// globally would break the callers that MUST fail an awaiting_approval /
// awaiting_input / awaiting_scope_decision stage (the approval-SLA, deploy, and
// trace paths). Only the reap path takes this stricter, park-refusing policy.
//
// It anchors the walk to `from` — the state the HANDLER already loaded (never a
// park: GUARD 1 short-circuits a park visible at that load, and the terminal
// pre-check a terminal state, so `from` is a reapable {pending, dispatched,
// running}). It deliberately does NOT re-read the stage: a park landing between
// the handler's load and the CAS is the mid-transition window GUARD 2 owns, and
// the CAS itself observes it (Actual=park → reapReanchor refuses). Re-reading
// here would instead make GUARD 1 redundant (a load-visible park would be caught
// twice) and leave GUARD 1 unable to serve as an independent, counterfactual-able
// guard.
//
// The walk no longer DEGRADES when the repo lacks the CAS capability. run.FailStage
// (the former fallback) re-anchors through whatever live state a concurrent advance
// produced and would take the legal park → failed edge on the four non-children
// parks (awaiting_approval / awaiting_input / awaiting_scope_decision /
// awaiting_host_dispatch) — i.e. exactly the live-park destruction GUARD 2 exists
// to prevent. So a repo without run.StageCASTransitioner makes the reap FAIL LOUDLY
// with errReapRepoNotCAS instead. run.FailStage is now UNREFERENCED from this file
// (the reap path can no longer call it), so the dangerous re-anchor-into-a-live-park
// edge no longer exists here rather than being merely unreached. The compile-time
// assertion in run/postgres.go plus serve.go's boot check (runRepoCASWiringError)
// make this refusal unreachable in a deployed daemon: production (postgresRepo)
// always has the capability.
func failStageForReap(ctx context.Context, repo run.Repository, stageID uuid.UUID, from run.StageState, cat run.FailureCategory, reason string) (*run.Stage, error) {
	cas, ok := repo.(run.StageCASTransitioner)
	if !ok {
		return nil, fmt.Errorf("%w (repo type %T)", errReapRepoNotCAS, repo)
	}
	return reapFailCAS(ctx, cas, stageID, from, cat, reason)
}

// reapFailCAS is failStageForReap's bounded re-anchor loop, mirroring run's
// failStageCAS but with a reap-scoped re-anchor guard (reapReanchor): it ABSORBS
// a benign advance to another reapable state and REFUSES any protected park.
// The dispatched → running → failed walk matches FailStage's so the spawn-phase
// 'dispatched' case and the #1907 benign dispatched → running absorption both
// hold; only the park-refusal diverges.
func reapFailCAS(ctx context.Context, cas run.StageCASTransitioner, stageID uuid.UUID, from run.StageState, cat run.FailureCategory, reason string) (*run.Stage, error) {
	var lastErr error
	for attempt := 0; attempt < reapFailMaxAttempts; attempt++ {
		if from == run.StageStateDispatched {
			running, err := cas.TransitionStageFrom(ctx, stageID, run.StageStateDispatched, run.StageStateRunning, nil)
			if err != nil {
				next, retry := reapReanchor(err)
				if !retry {
					return nil, err
				}
				from, lastErr = next, err
				continue
			}
			from = running.State
		}
		out, err := cas.TransitionStageFrom(ctx, stageID, from, run.StageStateFailed, &run.StageCompletion{
			FailureCategory: &cat,
			FailureReason:   &reason,
		})
		if err != nil {
			next, retry := reapReanchor(err)
			if !retry {
				return nil, err
			}
			from, lastErr = next, err
			continue
		}
		return out, nil
	}
	// Retry exhaustion under pathological livelock: surface the last typed
	// refusal so the handler's re-load yields the documented 500.
	return nil, lastErr
}

// reapReanchor classifies a CAS refusal for reapFailCAS. It returns
// (Actual, true) — re-anchor and retry — ONLY when the row-locked Actual state is
// still reapable (non-terminal AND not a protected park). A terminal state, or
// ANY protected park (isReapProtectedPark: awaiting_children as before, plus the
// four the #2630 concern adds), returns ("", false) so the error propagates
// unchanged for the handler to classify as the benign no-op. This single
// predicate swap — isReapProtectedPark in place of run.reanchorTarget's
// awaiting_children-only check — is the ONLY behavioral divergence from
// run.FailStage, and it is what refuses the four non-children parks the reaper
// must never collapse.
func reapReanchor(err error) (run.StageState, bool) {
	var sce run.StageStateChangedError
	if !errors.As(err, &sce) {
		return "", false
	}
	if sce.Actual.IsTerminal() || isReapProtectedPark(sce.Actual) {
		return "", false
	}
	return sce.Actual, true
}
