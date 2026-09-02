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
)

// CategoryStageSupersededByMerge is the audit-log category for the chained
// entry the merge-supersede sweep writes per stage it terminalized (E64.2 /
// #3083). The payload names the stage, its type, the state it was parked in and
// the reason the sweep ran, so the run record says WHY a run completed around a
// stage that never executed.
//
// It is the durable half of the honesty this issue is about: the pre-existing
// escape hatches record a lie (reap_stage writes `failed` for work that was
// never attempted, cancel_run writes `cancelled` for a change that shipped), and
// the state alone does not say what dissolved the stage. This entry does.
//
// Open-set string — audit_entries.category has no CHECK, so it needs no
// migration; it IS registered in audit.KnownCategories so an operator can arm
// fishhawk_await_audit on it. Internal audit kind projected through the
// living-anchor timeline — NOT a new issue-comment surface.
const CategoryStageSupersededByMerge = "stage_superseded_by_merge"

// The three reasons a supersession can carry. They are recorded, never
// interpreted by a gate: an operator reading the chain needs to know whether the
// merge itself swept the stage, an operator invoked the recovery verb, or the
// entry is a late repair of a sweep whose audit append failed after its
// transition already committed.
const (
	// supersedeReasonMergeObserved — the merge-observation path swept the stage
	// on the same pass that resolved the review stage.
	supersedeReasonMergeObserved = "merge_observed"
	// supersedeReasonOperatorReconcile — an operator invoked
	// POST /v0/runs/{run_id}/reconcile-merge on an already-merged run.
	supersedeReasonOperatorReconcile = "operator_reconcile"
	// supersedeReasonRepair — the stage was ALREADY `superseded` but carried no
	// audit row (a sweep whose transition committed and whose append then
	// failed). The repair transitions nothing; it only restores the record.
	supersedeReasonRepair = "repair"
)

// supersededStage is one stage the sweep actually moved (or repaired): the
// identity an operator and the response need. Returned only for stages whose
// compare-and-swap SUCCEEDED — a refused CAS contributes nothing, which is what
// makes "a missing row, never a false one" observable in the return value too.
type supersededStage struct {
	StageID   uuid.UUID `json:"stage_id"`
	StageType string    `json:"stage_type"`
	FromState string    `json:"from_state"`
	Reason    string    `json:"reason"`
}

// supersedeParkedStagesOnMerge is THE shared merge-supersede sweep — the one
// primitive both the automatic merged-path invocation and the operator recovery
// endpoint call, so the two can never disagree about which stages a merge may
// terminalize.
//
// For every stage of the run OTHER than skipStageID whose (Type, State) the
// DEFAULT-DENY run.MergeSupersedable pair table admits, it applies the move via
// the run.StageCASTransitioner compare-and-swap, pinned to the state the sweep
// classified. The CAS is not a convenience: it closes the classify→transition
// race atomically under the stage row lock, so a concurrent writer that re-parks
// or fails the stage in that window is refused with a typed
// run.StageStateChangedError instead of having its state destroyed.
//
// ORDER IS TRANSITION-FIRST, THEN AUDIT, and it is load-bearing. An audit row
// appended before a CAS that then refuses would be an IMMUTABLE record of a
// supersession that never happened — the chain is append-only, so there is no
// unwinding it. The failure mode must be a MISSING row, never a false one; the
// reconcile endpoint's repair scan exists precisely to close the missing-row
// window from the other side.
//
// The repository capability is REQUIRED, not optional: a repo that does not
// implement run.StageCASTransitioner sweeps NOTHING (warn-logged) rather than
// degrading to the non-CAS run.TransitionStage, which would apply the move on a
// stale premise and could destroy a live park. This mirrors the reap path's
// refusal (reap_failure.go, #2672).
//
// Best-effort throughout: every failure warn-logs and the sweep continues to the
// next stage. It never returns an error, because BOTH callers are best-effort
// tails that must not unwind a merge resolution that has already committed.
func (s *Server) supersedeParkedStagesOnMerge(ctx context.Context, runID uuid.UUID, skipStageID *uuid.UUID, reason string) []supersededStage {
	if s.cfg.RunRepo == nil {
		return nil
	}
	cas, ok := s.cfg.RunRepo.(run.StageCASTransitioner)
	if !ok {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge supersede: run repository does not implement run.StageCASTransitioner; sweeping nothing",
			slog.String("run_id", runID.String()))
		return nil
	}
	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge supersede: list stages failed; sweeping nothing",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
		return nil
	}

	var moved []supersededStage
	for _, st := range stages {
		if st == nil {
			continue
		}
		// The caller's own stage is never swept. The merged path resolves the
		// review stage itself and passes its id here, so the sweep can never
		// race the transition that path owns.
		if skipStageID != nil && st.ID == *skipStageID {
			continue
		}
		// DEFAULT DENY. Only the (stage_type, state) pairs the table admits.
		// A `pending` plan stage or a `running` implement stage is NOT
		// supersedable — sweeping one would let completeRun stamp the run
		// succeeded around work never done, defeating the #968 guard.
		if !run.MergeSupersedable(st.Type, st.State) {
			continue
		}
		from := st.State
		if _, terr := cas.TransitionStageFrom(ctx, st.ID, from, run.StageStateSuperseded, nil); terr != nil {
			var sce run.StageStateChangedError
			if errors.As(terr, &sce) {
				// A concurrent writer moved the stage between the classify and
				// the CAS. Refusing is correct; NO audit row is written.
				s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
					"merge supersede: stage state changed under the sweep; refused (no audit row)",
					slog.String("run_id", runID.String()),
					slog.String("stage_id", st.ID.String()),
					slog.String("expected", string(sce.Expected)),
					slog.String("actual", string(sce.Actual)))
				continue
			}
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"merge supersede: stage transition failed (no audit row)",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", st.ID.String()),
				slog.String("error", terr.Error()))
			continue
		}
		rec := supersededStage{
			StageID:   st.ID,
			StageType: string(st.Type),
			FromState: string(from),
			Reason:    reason,
		}
		s.appendStageSupersededAudit(ctx, runID, rec)
		moved = append(moved, rec)
	}
	return moved
}

// appendStageSupersededAudit writes the one chained stage_superseded_by_merge
// entry for a supersession that has ALREADY committed. Best-effort by design:
// the transition is durable and must not be unwound by an audit failure, so a
// failed append warn-logs and leaves a missing row the reconcile endpoint's
// repair scan can restore later.
func (s *Server) appendStageSupersededAudit(ctx context.Context, runID uuid.UUID, rec supersededStage) {
	if s.cfg.AuditRepo == nil {
		return
	}
	stageID := rec.StageID
	payload, _ := json.Marshal(map[string]any{
		"run_id":     runID.String(),
		"stage_id":   rec.StageID.String(),
		"stage_type": rec.StageType,
		"from_state": rec.FromState,
		"reason":     rec.Reason,
	})
	actorKind := audit.ActorSystem
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  CategoryStageSupersededByMerge,
		ActorKind: &actorKind,
		Payload:   payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge supersede: append stage_superseded_by_merge audit entry failed; row is MISSING (repairable via reconcile-merge)",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", rec.StageID.String()),
			slog.String("error", err.Error()))
	}
}

// reconcileMergeResponse reports what the operator recovery verb did.
// Superseded lists the stages this invocation MOVED; Repaired lists stages that
// were already `superseded` but carried no audit row and got one back. RunState
// is the run's state after the completion re-evaluation, so an operator sees
// whether the reconcile actually settled the run.
type reconcileMergeResponse struct {
	RunID      string            `json:"run_id"`
	Superseded []supersededStage `json:"superseded"`
	Repaired   []supersededStage `json:"repaired"`
	RunState   string            `json:"run_state"`
}

// handleReconcileMerge implements POST /v0/runs/{run_id}/reconcile-merge
// (E64.2 / #3083) — the operator recovery for a run whose PR merged while a
// stage stayed parked, leaving Orchestrator.completeRun's #968 guard correctly
// refusing to complete it and every existing escape hatch recording something
// untrue.
//
// It supersedes exactly the pair-table-admissible parked stages, re-runs
// completion, and returns what moved.
//
// Refusals, ALL evaluated before any write so a refused reconcile leaves ZERO
// rows and moves ZERO stages:
//  1. 400 validation_failed — a non-UUID run_id;
//  2. 503 reconcile_merge_unconfigured — the run/audit repositories are unwired;
//  3. 404 run_not_found;
//  4. 409 reconcile_merge_pr_not_merged — the run's PR is not OBSERVABLY merged
//     (no pr_merged / post_merge_observed entry on its chain). This is the guard
//     that stops the verb manufacturing a `succeeded` run for an unmerged
//     change: without it, an operator could settle a run whose work never
//     shipped. A chain-read failure is a 500, never a write — fail closed;
//  5. 409 reconcile_merge_not_applicable — the run holds no pair-table-
//     admissible parked stage AND no already-superseded stage to repair, so
//     there is nothing for this verb to do.
//
// IDEMPOTENT: a second POST finds the stage already `superseded` (not admissible
// — `superseded` is not a pair-table state), moves nothing, finds its audit row
// present, repairs nothing, and returns 200 with two empty lists. The repair
// scan EXCLUDES the stages this same invocation moved, so exactly one row per
// swept stage exists no matter how many times the verb is called.
func (s *Server) handleReconcileMerge(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "reconcile_merge_unconfigured",
			"merge reconciliation requires run + audit repositories", nil)
		return
	}
	if _, gerr := s.cfg.RunRepo.GetRun(r.Context(), runID); gerr != nil {
		if errors.Is(gerr, run.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "run_not_found",
				"no run with that id", map[string]any{"run_id": runID.String()})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get run failed", map[string]any{"error": gerr.Error()})
		return
	}

	// Guard 4 — the merged-PR precondition. Fail CLOSED on an unreadable chain:
	// a verb that can complete a run must never run on unknown evidence.
	merged, merr := s.runPRObservablyMerged(r.Context(), runID)
	if merr != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"read merge observation failed", map[string]any{"error": merr.Error()})
		return
	}
	if !merged {
		s.writeError(w, r, http.StatusConflict, "reconcile_merge_pr_not_merged",
			"this run's pull request is not observably merged; reconcile-merge would manufacture a succeeded run for a change that never shipped",
			map[string]any{"run_id": runID.String()})
		return
	}

	stages, serr := s.cfg.RunRepo.ListStagesForRun(r.Context(), runID)
	if serr != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"list stages failed", map[string]any{"error": serr.Error()})
		return
	}
	var admissible, alreadySuperseded int
	for _, st := range stages {
		if st == nil {
			continue
		}
		if st.State == run.StageStateSuperseded {
			alreadySuperseded++
			continue
		}
		if run.MergeSupersedable(st.Type, st.State) {
			admissible++
		}
	}
	// Guard 5. `alreadySuperseded` keeps a repeat POST on the idempotent 200
	// path rather than a confusing 409 — and it is what makes the repair scan
	// reachable at all on a run whose sweep already moved everything.
	if admissible == 0 && alreadySuperseded == 0 {
		s.writeError(w, r, http.StatusConflict, "reconcile_merge_not_applicable",
			"this run holds no merge-supersedable parked stage and no superseded stage to repair; a stage in any other non-terminal state must run, settle or be cancelled",
			map[string]any{"run_id": runID.String()})
		return
	}

	// Read the EXISTING supersede rows BEFORE the sweep. The repair scan
	// compares against this pre-sweep snapshot, so the rows this invocation is
	// about to write are NOT in it — which is exactly why the
	// moved-this-invocation exclusion below is load-bearing rather than
	// decorative.
	priorRows, perr := s.supersededStageIDsWithAuditRow(r.Context(), runID)
	if perr != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"read prior supersede audit rows failed", map[string]any{"error": perr.Error()})
		return
	}

	moved := s.supersedeParkedStagesOnMerge(r.Context(), runID, nil, supersedeReasonOperatorReconcile)
	movedIDs := make(map[uuid.UUID]struct{}, len(moved))
	for _, m := range moved {
		movedIDs[m.StageID] = struct{}{}
	}

	// Repair scan: a stage that is already `superseded` but has no audit row is
	// the residue of a sweep whose CAS committed and whose append then failed.
	// Re-append with reason `repair`; transition NOTHING. Read the stages back
	// so a supersession landed by a concurrent path is repairable too.
	repaired := s.repairMissingSupersedeRows(r.Context(), runID, movedIDs, priorRows)

	// Re-run completion. The sweep made the parked stages terminal, so the
	// orchestrator's Advance can now route the all-terminal stage set through
	// completeRun. Best-effort, exactly as the merged path's advance is.
	s.advanceRunAfterReviewResolve(r.Context(), runID)

	state := ""
	if after, aerr := s.cfg.RunRepo.GetRun(r.Context(), runID); aerr == nil {
		state = string(after.State)
	}
	if moved == nil {
		moved = []supersededStage{}
	}
	if repaired == nil {
		repaired = []supersededStage{}
	}
	s.writeJSON(w, r, http.StatusOK, reconcileMergeResponse{
		RunID:      runID.String(),
		Superseded: moved,
		Repaired:   repaired,
		RunState:   state,
	})
}

// repairMissingSupersedeRows re-appends a stage_superseded_by_merge entry for
// every stage that is `superseded` on the CURRENT stage rows but carries no row
// in priorRows — the missing-row residue the transition-first ordering
// deliberately allows.
//
// movedThisInvocation is the LOAD-BEARING exclusion. priorRows is a PRE-sweep
// snapshot, so a stage this invocation just moved is superseded in the re-read
// and absent from that snapshot; without the exclusion it would draw a SECOND
// row for the same supersession, and "exactly one row per swept stage" would
// become "one or two, depending on whether an operator ran the verb".
func (s *Server) repairMissingSupersedeRows(ctx context.Context, runID uuid.UUID, movedThisInvocation map[uuid.UUID]struct{}, priorRows map[uuid.UUID]struct{}) []supersededStage {
	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge supersede: re-list stages for repair scan failed; repairing nothing",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
		return nil
	}
	var repaired []supersededStage
	for _, st := range stages {
		if st == nil || st.State != run.StageStateSuperseded {
			continue
		}
		if _, justMoved := movedThisInvocation[st.ID]; justMoved {
			continue
		}
		if _, has := priorRows[st.ID]; has {
			continue
		}
		rec := supersededStage{
			StageID:   st.ID,
			StageType: string(st.Type),
			FromState: string(run.StageStateSuperseded),
			Reason:    supersedeReasonRepair,
		}
		s.appendStageSupersededAudit(ctx, runID, rec)
		repaired = append(repaired, rec)
	}
	return repaired
}

// supersededStageIDsWithAuditRow returns the set of stage ids that already carry
// a stage_superseded_by_merge audit row. A read failure is propagated, never
// swallowed: the repair scan's whole job is to decide whether a row is MISSING,
// and treating an unreadable chain as "no rows" would duplicate every row.
func (s *Server) supersededStageIDsWithAuditRow(ctx context.Context, runID uuid.UUID) (map[uuid.UUID]struct{}, error) {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryStageSupersededByMerge)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]struct{}, len(entries))
	for _, e := range entries {
		if e == nil {
			continue
		}
		if e.StageID != nil {
			out[*e.StageID] = struct{}{}
			continue
		}
		var p struct {
			StageID string `json:"stage_id"`
		}
		if json.Unmarshal(e.Payload, &p) == nil {
			if id, perr := uuid.Parse(p.StageID); perr == nil {
				out[id] = struct{}{}
			}
		}
	}
	return out, nil
}

// runPRObservablyMerged reports whether the run's chain carries a merge
// observation — a pr_merged or post_merge_observed entry. It is the
// evidence the reconcile verb and the completion_blocked recovery
// discrimination both key on, so the two can never disagree about whether a
// reconcile applies.
func (s *Server) runPRObservablyMerged(ctx context.Context, runID uuid.UUID) (bool, error) {
	for _, category := range []string{CategoryPRMerged, CategoryPostMergeObserved} {
		entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, category)
		if err != nil {
			return false, err
		}
		if len(entries) > 0 {
			return true, nil
		}
	}
	return false, nil
}
