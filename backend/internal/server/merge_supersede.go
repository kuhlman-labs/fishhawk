package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// supersedeRepairMu serializes the missing-audit-row repair in
// repairMissingSupersedeRows (E64.2 / #3083 fix-up). The repair is a
// read-then-append (is a row present? if not, write one), which two concurrent
// reconcile-merge requests would otherwise both pass — each writing a row for
// the same stage and breaking the documented exactly-one-row guarantee. Holding
// this lock across the fresh re-read AND the append makes the pair atomic within
// the process: the second request's re-read observes the first's committed row
// and skips it.
//
// It is a package-level lock (not a Server field) so the whole change stays
// within the files this fix-up owns; there is one fishhawkd Server per process,
// so this is equivalent to a per-Server lock in practice. It serializes repair
// across ALL runs (a single lock, not keyed by run) — acceptable because
// reconcile-merge is a rare operator recovery path — and does NOT serialize
// across separate fishhawkd replicas, which would need a DB-level guard
// (advisory lock or a per-stage unique constraint) tracked separately.
var supersedeRepairMu sync.Mutex

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
		// The CAS already committed, so this stage IS moved regardless of
		// whether the audit append lands — a swallowed append failure leaves a
		// MISSING row the reconcile repair scan restores later, which is why
		// the error is intentionally discarded here (unlike the repair path).
		_ = s.appendStageSupersededAudit(ctx, runID, rec)
		moved = append(moved, rec)
	}
	return moved
}

// appendStageSupersededAudit writes the one chained stage_superseded_by_merge
// entry for a supersession that has ALREADY committed. Best-effort by design:
// the transition is durable and must not be unwound by an audit failure, so a
// failed append warn-logs and leaves a missing row the reconcile endpoint's
// repair scan can restore later.
//
// It returns the append error (nil on success, and nil when no audit repository
// is wired so a sweep whose transition committed still counts as moved). The
// repair scan reads this return to decide whether a row was DURABLY restored: a
// swallowed failure must not let the reconcile response claim a repair that
// never persisted (#3083 fix-up — the Repaired list is confirmed durable
// repairs only).
func (s *Server) appendStageSupersededAudit(ctx context.Context, runID uuid.UUID, rec supersededStage) error {
	if s.cfg.AuditRepo == nil {
		return nil
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
		return err
	}
	return nil
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
//     (no pr_merged / post_merge_observed / merge_observation_recorded entry on
//     its chain — the third is the row POST /v0/runs/{run_id}/record-merge-observation
//     appends after a live merged=true forge read, E64.32 / #3136). This is the guard
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
// yet — the missing-row residue the transition-first ordering deliberately
// allows.
//
// movedThisInvocation is the LOAD-BEARING exclusion. A stage this invocation
// just moved is superseded in the re-read but its row (if any) was written by
// this same invocation's sweep; without the exclusion it would draw a SECOND
// row for the same supersession, and "exactly one row per swept stage" would
// become "one or two, depending on whether an operator ran the verb".
//
// priorRows is the PRE-sweep snapshot the handler read; it is one of the two
// membership guards (a stage recorded before the sweep is never repaired).
//
// ATOMICITY (#3083 fix-up). The check-then-append is serialized under
// s.supersedeRepairMu AND re-reads the CURRENT audit rows inside the lock, so
// two concurrent reconcile-merge requests can no longer both observe a missing
// row and both append: the second acquires the lock after the first commits,
// its fresh read sees the row, and it skips the stage. The pre-sweep priorRows
// snapshot alone could not close this — both requests captured it before either
// wrote — so the fresh under-lock read is what makes the guarantee hold.
//
// DURABILITY (#3083 fix-up). A stage is added to the returned Repaired list
// ONLY when its audit append SUCCEEDS. appendStageSupersededAudit swallows the
// failure (best-effort), so reporting a repair before checking its return would
// let the response claim a row was restored when none persisted — the exact
// contract violation this fix closes.
func (s *Server) repairMissingSupersedeRows(ctx context.Context, runID uuid.UUID, movedThisInvocation map[uuid.UUID]struct{}, priorRows map[uuid.UUID]struct{}) []supersededStage {
	supersedeRepairMu.Lock()
	defer supersedeRepairMu.Unlock()

	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge supersede: re-list stages for repair scan failed; repairing nothing",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
		return nil
	}
	// Fresh read UNDER THE LOCK: this observes any row a concurrent reconcile
	// committed before we acquired the mutex, which is what makes the repair
	// idempotent against a racing request rather than only a sequential one.
	current, cerr := s.supersededStageIDsWithAuditRow(ctx, runID)
	if cerr != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge supersede: re-read audit rows for repair scan failed; repairing nothing",
			slog.String("run_id", runID.String()),
			slog.String("error", cerr.Error()))
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
		if _, has := current[st.ID]; has {
			continue
		}
		rec := supersededStage{
			StageID:   st.ID,
			StageType: string(st.Type),
			FromState: string(run.StageStateSuperseded),
			Reason:    supersedeReasonRepair,
		}
		if aerr := s.appendStageSupersededAudit(ctx, runID, rec); aerr != nil {
			// The append failed and swallowed the error; the row is NOT
			// durable, so it must not be reported as repaired. Leave it for a
			// later reconcile to retry.
			continue
		}
		// Record it in the fresh set so a duplicate stage id in the same scan
		// (there should be none, but the guard is cheap) cannot draw a second
		// row within this invocation.
		current[st.ID] = struct{}{}
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

// runHasSupersededStage reports whether the run currently holds at least one
// stage in the `superseded` terminal state. The no-review-stage merged path
// (pullrequest_review_events.go) uses it to decide whether to re-drive the run
// to completion on a merge redelivery even when THIS pass's sweep moved nothing
// (#3083 fix-up): an earlier invocation may have committed the
// awaiting_host_dispatch → superseded transition and then stopped before
// advancing the run — a crash, or an Advance error — leaving the run stranded
// in `running`. A later merge observation must re-evaluate it to completion
// rather than skip the advance again. Best-effort: a list failure logs and
// returns false, so a transient read error degrades to the prior (advance-only-
// when-moved) behavior rather than a spurious advance.
func (s *Server) runHasSupersededStage(ctx context.Context, runID uuid.UUID) bool {
	if s.cfg.RunRepo == nil {
		return false
	}
	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge supersede: list stages for superseded-stage check failed; assuming none",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
		return false
	}
	for _, st := range stages {
		if st != nil && st.State == run.StageStateSuperseded {
			return true
		}
	}
	return false
}

// runPRObservablyMerged reports whether the run's chain carries a merge
// observation — a pr_merged, post_merge_observed or merge_observation_recorded
// entry. It is the evidence the reconcile verb and the completion_blocked
// recovery discrimination both key on, so the two can never disagree about
// whether a reconcile applies.
//
// merge_observation_recorded (E64.32 / #3136) is the THIRD category, and adding
// it is NOT a loosening of the #3083 gate. Evidence is still REQUIRED, it is
// still read from the run's OWN chain, and this settling path still NEVER
// re-observes the forge itself. The new category is a durable,
// operator-attributed, distinctly-labelled record of a forge read that a
// DIFFERENT verb (POST /v0/runs/{run_id}/record-merge-observation) performed
// and audited, and that verb writes it only on a live merged=true answer
// carrying a merge commit SHA and a merge timestamp. What changes is that the
// previously UNREACHABLE shape — a run whose merge genuinely happened and was
// never recorded — now has a way onto the chain; what does not change is that
// an unmerged run can never acquire one.
func (s *Server) runPRObservablyMerged(ctx context.Context, runID uuid.UUID) (bool, error) {
	for _, category := range []string{CategoryPRMerged, CategoryPostMergeObserved, CategoryMergeObservationRecorded} {
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
