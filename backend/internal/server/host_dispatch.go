package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/runnerbackend"
)

// hostDispatchAnchorSource is the acceptance_dispatched payload discriminator
// identifying the host-dispatch marker as the emit site, distinguishing it from
// orchestrator.emitAcceptanceDispatched's backend-triggered entry (which carries
// no `source` field).
const hostDispatchAnchorSource = "host_dispatch"

// hostDispatchResponse is the 200 body of the host-dispatch marker endpoint
// (#1912). Transitioned is true when this call drove the stage
// pending|awaiting_host_dispatch → dispatched (the spawn marker), false on the
// idempotent no-op path (the stage was already 'dispatched' — a legal manual
// re-dispatch of a stage whose spawned runner died). StageState is the stage's
// state after the call.
//
// BaseBranch (#2363) is the branch a DECOMPOSED FAN-OUT CHILD must spawn
// against: the parent's consolidated branch, returned only once every
// dependency slice this child declares is provably merged onto it. It is
// omitted (empty) for every other caller — a non-decomposed run, the parent's
// own re-dispatch, and a wave-0 child declaring no dependencies — and those
// callers keep whatever base they had. The server is the authority on the
// per-wave re-base; the client derives nothing.
type hostDispatchResponse struct {
	Transitioned bool   `json:"transitioned"`
	StageState   string `json:"stage_state"`
	BaseBranch   string `json:"base_branch,omitempty"`
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

	// Fence against a concurrent acceptance-admission short-circuit walk (#1936).
	// When an orchestrator is wired, acquire the SAME per-stage admission lock
	// TryShortCircuitAcceptance holds across its read -> admissibility-check -> walk,
	// and hold it here across the stage load -> eligibility checks -> CAS. This
	// closes the mid-walk window: the marker can no longer observe a walk-intermediate
	// 'dispatched' and return the idempotent {transitioned:false} proceed while the
	// walk continues to succeeded. It instead either waits for the walk to complete
	// (then observes the settled stage and 409s dispatch_not_admissible — the MCP
	// verb's fail-closed marker handling spawns nothing) or wins the CAS first (then
	// the late admission re-reads 'dispatched' under the lock and no-ops, since
	// 'dispatched' is non-admissible post-#1936). When Orchestrator is nil the
	// admission endpoint never walks, so no lock is taken and behavior is unchanged.
	// Held via defer through the response write — the response touches no stage
	// state, so the extra hold is harmless, while defer guarantees no early-return
	// path leaks the lock and wedges the stage forever.
	if s.cfg.Orchestrator != nil {
		unlock := s.cfg.Orchestrator.LockStageAdmission(stageID)
		defer unlock()
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
	// Admit a host spawn only when the resolved kind is KNOWN and host-dispatched
	// (local). This site rejects unknown resolved kinds too — a runner_kind
	// fishhawkd doesn't recognize is never a legitimate LOCAL host spawn — the
	// opposite unknown-kind posture from the MCP guardHostDispatch, which the
	// KindHostDispatched (hostDispatched, known) two-value shape keeps explicit.
	if runRow.RunnerKindResolved {
		if hostDispatched, known := runnerbackend.KindHostDispatched(runRow.RunnerKind); !known || !hostDispatched {
			s.writeError(w, r, http.StatusConflict, "dispatch_not_admissible",
				"run is locked to a non-local runner_kind; host-dispatch marks a LOCAL host spawn",
				map[string]any{"run_id": runID.String(), "runner_kind": runRow.RunnerKind})
			return
		}
	}
	if stage.ExecutorKind != run.ExecutorAgent || isAutoMergeReviewStage(stage) {
		s.writeError(w, r, http.StatusConflict, "dispatch_not_admissible",
			"stage is not an agent-executed host-spawn target (human or auto-merge review-gate stages are never host-spawned)",
			map[string]any{"stage_id": stageID.String(),
				"executor_kind": string(stage.ExecutorKind), "stage_type": string(stage.Type)})
		return
	}

	// Decomposition wave-order guard (#2546). For a fan-out child carrying
	// decomposed_from + slice_index this resolves the child's declared
	// dependencies from the parent's approved plan and refuses when a
	// dependency slice has not reached run state 'succeeded'. It sits AFTER the
	// eligibility checks and BEFORE the state switch/CAS below, inside the
	// held stage-admission lock, so a refusal commits NO state and the stage
	// stays parked at awaiting_host_dispatch. Non-child runs (the parent's own
	// re-dispatch, every non-decomposed run) take the inert admit path with no
	// extra reads. Fail-OPEN on ABSENT dependency data (no plan / no
	// decomposition / out-of-range slice — refusing there would wedge every
	// legitimate dispatch behind an unresolvable plan) but fail-CLOSED on an
	// ERRORED read (retryable 500, never a silent admit).
	//
	// Do NOT infer atomicity from the held stage-admission lock: that lock is
	// keyed to THIS stage and serializes no sibling-RUN write, so the guard's
	// sibling snapshot is not atomic with the CAS below. What makes the window
	// safe is that run state 'succeeded' is absorbing — see the "Atomicity of
	// the predicate vs the CAS (#2586)" section on guardDecompositionWaveOrder,
	// including the plan-revision window that argument does NOT cover.
	if depErr, derr := s.guardDecompositionWaveOrder(r.Context(), runRow); derr != nil {
		s.writeError(w, r, http.StatusInternalServerError, "dependency_check_failed",
			"could not resolve the run's decomposition dependencies to validate wave order",
			map[string]any{"run_id": runID.String(), "error": derr.Error()})
		return
	} else if depErr != nil {
		s.writeError(w, r, http.StatusConflict, "dependency_not_satisfied",
			depErr.message(), depErr.details())
		return
	}

	// Per-wave RE-BASE + integration guard (#2363). The wave-order guard above
	// proves every dependency slice SUCCEEDED; this proves they are MERGED. Run
	// state flips to succeeded before the between-wave integration merges the
	// slice branches, so a child admitted on state alone would spawn on a base
	// missing its predecessors' symbols (#1302). Admitting returns the parent's
	// consolidated branch as base_branch — the server, not the client, is the
	// authority on the re-base — and refusing 409 wave_not_integrated happens
	// HERE, before the state switch/CAS below, so a refusal commits NO state and
	// the child stays cleanly re-dispatchable once the sweeper integrates.
	//
	// A NON-EMPTY consolidated branch is deliberately not sufficient: the
	// coverage predicate is keyed to the DEPENDENCY SET, so a stale integration
	// (an earlier wave merged, this child's newly-succeeded dependency not) is
	// refused too. See resolveDependentChildBase.
	baseBranch, waveErr, werr := s.resolveDependentChildBase(r.Context(), runRow)
	if werr != nil {
		s.writeError(w, r, http.StatusInternalServerError, "dependency_check_failed",
			"could not resolve the parent's slice integration state to derive this child's base branch",
			map[string]any{"run_id": runID.String(), "error": werr.Error()})
		return
	}
	if waveErr != nil {
		s.writeError(w, r, http.StatusConflict, "wave_not_integrated",
			waveErr.message(), waveErr.details())
		return
	}

	switch stage.State {
	case run.StageStateDispatched:
		// Idempotent no-op: a spawn attempt already exists. The manual
		// dead-runner re-dispatch lands here; the caller proceeds and re-spawns
		// — so it needs the derived base too, exactly as the transitioned arm.
		s.writeJSON(w, r, http.StatusOK, hostDispatchResponse{
			Transitioned: false,
			StageState:   string(stage.State),
			BaseBranch:   baseBranch,
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
						BaseBranch:   baseBranch,
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

	// Acceptance spawn anchor (E64.53 / #3174). THIS is the site that marks a
	// real local spawn, so it is where the acceptance_dispatched anchor belongs
	// for a host-dispatched run. Before this, the only emit site was
	// orchestrator.Advance, which for a locked-local run fires at PARK time —
	// and the fix-up re-open path (reopenAcceptanceOnFixupPush →
	// run.ReopenAcceptanceStage) never calls Advance at all, so the acceptance
	// RE-dispatch wrote no new anchor and latestAcceptanceDispatchSeq stayed
	// pinned at the ORIGINAL dispatch. The verdict then bound to the PRE-fix-up
	// head, because acceptanceValidatedHeadSHA's `e.Sequence <= dispatchSeq`
	// bound filtered out the later fixup_pushed head. Emitting here makes every
	// local dispatch that TRANSITIONS the stage — first spawn, re-open spawn,
	// fix-up re-dispatch — advance the anchor.
	//
	// Deliberately NOT emitted on either idempotent {transitioned:false} arm
	// (the early already-dispatched arm above and the post-CAS
	// StageStateChangedError re-load arm): an anchor already exists from the
	// first mark, so that is not an anchorless spawn, and a fix-up re-opens the
	// stage to 'pending' — which makes the NEXT mark a real transition that DOES
	// emit.
	//
	// Runs INSIDE the held stage-admission lock (deferred through this response
	// write), so no concurrent admission walk interleaves between the CAS and
	// the anchor.
	if stage.Type == run.StageTypeAcceptance {
		s.emitHostDispatchAcceptanceAnchor(r.Context(), runID, stage)
	}

	s.writeJSON(w, r, http.StatusOK, hostDispatchResponse{
		Transitioned: true,
		StageState:   string(updated.State),
		BaseBranch:   baseBranch,
	})
}

// emitHostDispatchAcceptanceAnchor writes the acceptance_dispatched audit entry
// that anchors a LOCAL acceptance dispatch to the moment of the spawn (E64.53 /
// #3174). The payload is compatible-plus-discriminator with
// orchestrator.emitAcceptanceDispatched's — the same stage_id/sequence/executor
// keys, plus a `source: "host_dispatch"` field so the two emit sites are
// distinguishable in the ledger.
//
// Best-effort, mirroring the orchestrator's posture: a nil AuditRepo skips with
// a WARN, and a failed append is logged at WARN and NEVER unwinds the CAS or
// changes the 200 response — the stage is genuinely dispatched, and refusing
// here would wedge it. A FIRST dispatch whose append fails leaves the stage
// anchorless, so the #3091 head_unresolved clamp records `undecidable`
// (fail-closed). A RE-dispatch whose append fails leaves the PREVIOUS episode's
// anchor on the chain, but that stale anchor is NO LONGER trusted: the resolver's
// episode-restart staleness guard (acceptanceValidatedHeadSHA, E64.54 / #3176)
// treats an anchor at or below the newest acceptance_reopened marker as
// unresolvable and clamps to undecidable. The remaining residual, narrower than
// #3174's, is an episode whose acceptance_reopened append ALSO failed (no restart
// marker on the chain) — unclosable by any write-based fix, since a write is what
// is failing.
func (s *Server) emitHostDispatchAcceptanceAnchor(ctx context.Context, runID uuid.UUID, stage *run.Stage) {
	if s.cfg.AuditRepo == nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"host-dispatch: AuditRepo not configured; skipping acceptance_dispatched anchor",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stage.ID.String()))
		return
	}
	payload, err := json.Marshal(map[string]any{
		"stage_id": stage.ID.String(),
		"sequence": stage.Sequence,
		"executor": string(stage.ExecutorKind),
		"source":   hostDispatchAnchorSource,
	})
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"host-dispatch: marshal acceptance_dispatched payload failed; anchor not written",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stage.ID.String()),
			slog.String("error", err.Error()))
		return
	}
	systemKind := audit.ActorSystem
	stageID := stage.ID
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  CategoryAcceptanceDispatched,
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"host-dispatch: append acceptance_dispatched anchor failed; the acceptance verdict may bind to a stale or unresolvable head",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stage.ID.String()),
			slog.String("error", err.Error()))
	}
}

// isAutoMergeReviewStage mirrors orchestrator.isAutoMergeStage (unexported in
// that package): a review stage carrying a check-only gate is queued for GitHub
// auto-merge and walked straight to succeeded, never host-spawned. Replicated
// here so the host-dispatch endpoint refuses to mark such a stage 'dispatched'.
func isAutoMergeReviewStage(s *run.Stage) bool {
	return s.Type == run.StageTypeReview && s.Gate != nil && s.Gate.Kind == run.GateKindCheck
}
