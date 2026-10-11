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
// SINCE E64.29 / #3133 THIS IS A SAME-PROCESS FAST PATH, NOT THE GUARANTEE.
// Process memory cannot arbitrate between PROCESSES, so two fishhawkd instances
// (two replicas, or a rolling restart's overlap) could each hold their own copy
// of this mutex, each observe the row missing and each append. The durable,
// cross-process control is now migration 0081's partial unique index
// audit_entries_stage_superseded_by_merge_once_idx on (run_id, stage_id) WHERE
// category = 'stage_superseded_by_merge': the DATABASE refuses the second
// append, and appendStageSupersededAudit recognizes that collision as the benign
// already-recorded outcome via audit.IsStageSupersededByMergeDuplicate. This
// mutex is RETAINED because it still spares the common same-process race a
// round-trip that ends in a constraint violation, and because it keeps the
// in-process guarantee intact if the index is ever rolled back.
//
// It is a package-level lock (not a Server field) so the whole change stays
// within the files this fix-up owns; there is one fishhawkd Server per process,
// so this is equivalent to a per-Server lock in practice. It serializes repair
// across ALL runs (a single lock, not keyed by run) — acceptable because
// reconcile-merge is a rare operator recovery path.
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

// The four reasons a supersession can carry. They are recorded, never
// interpreted by a gate: an operator reading the chain needs to know whether the
// merge itself swept the stage, an operator invoked the recovery verb (and
// whether it opted into retiring a stranded stage), or the entry is a late
// repair of a sweep whose audit append failed after its transition already
// committed.
const (
	// supersedeReasonMergeObserved — the merge-observation path swept the stage
	// on the same pass that resolved the review stage.
	supersedeReasonMergeObserved = "merge_observed"
	// supersedeReasonOperatorReconcile — an operator invoked
	// POST /v0/runs/{run_id}/reconcile-merge on an already-merged run.
	supersedeReasonOperatorReconcile = "operator_reconcile"
	// supersedeReasonOperatorReconcileStranded — an operator invoked
	// reconcile-merge with {"supersede_stranded": true} and the stage was one
	// stranded in flight (dispatched/running) or a never-opened gate (pending)
	// on the already-merged run (#4222). Its row also carries last_activity_at
	// and idle_threshold_seconds.
	supersedeReasonOperatorReconcileStranded = "operator_reconcile_stranded"
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

	// lastActivityAt and idleThreshold are set ONLY for a stranded
	// supersession (reason operator_reconcile_stranded, #4222) and are written
	// into its audit payload as last_activity_at / idle_threshold_seconds, so the
	// chain records the liveness evidence the retirement rested on. Unexported:
	// they are audit-only and never reach the response body.
	lastActivityAt *time.Time
	idleThreshold  time.Duration
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
//
// BENIGN DUPLICATE (E64.29 / #3133). Migration 0081's partial unique index makes
// the database refuse a SECOND row for the same (run_id, stage_id) in this
// category, which is what makes exactly-one hold across PROCESSES and not merely
// within one. That collision is not a failure: the row is durable, it just was
// not written by THIS invocation, so it logs at INFO rather than the WARN that
// (correctly, for every other error) claims the row is MISSING and repairable.
// The error is returned UNCHANGED rather than converted to nil, because the
// return value is load-bearing for the Repaired list — "the row exists" and
// "this invocation restored it" are different claims, and only the second
// belongs in the response.
func (s *Server) appendStageSupersededAudit(ctx context.Context, runID uuid.UUID, rec supersededStage) error {
	if s.cfg.AuditRepo == nil {
		return nil
	}
	stageID := rec.StageID
	fields := map[string]any{
		"run_id":     runID.String(),
		"stage_id":   rec.StageID.String(),
		"stage_type": rec.StageType,
		"from_state": rec.FromState,
		"reason":     rec.Reason,
	}
	if rec.idleThreshold > 0 {
		fields["idle_threshold_seconds"] = int64(rec.idleThreshold / time.Second)
	}
	if rec.lastActivityAt != nil {
		fields["last_activity_at"] = rec.lastActivityAt.UTC().Format(time.RFC3339Nano)
	}
	payload, _ := json.Marshal(fields)
	actorKind := audit.ActorSystem
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  CategoryStageSupersededByMerge,
		ActorKind: &actorKind,
		Payload:   payload,
	}); err != nil {
		if audit.IsStageSupersededByMergeDuplicate(err) {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
				"merge supersede: stage_superseded_by_merge row already recorded by a concurrent writer or replica; no second row appended",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", rec.StageID.String()))
			return err
		}
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

// reconcileMergeRequest is the OPTIONAL body of
// POST /v0/runs/{run_id}/reconcile-merge (#4222). An absent or empty body
// decodes to the zero value, so a caller that sends nothing gets exactly the
// pre-#4222 verb.
type reconcileMergeRequest struct {
	// SupersedeStranded opts into the stranded arm: on a run whose chain
	// already carries merge evidence, ALSO retire the stages
	// run.StrandedMergeSupersedable admits — implement/review/acceptance
	// stranded in dispatched or running, and review/acceptance gates still
	// pending — subject to the idle-threshold liveness gate.
	SupersedeStranded bool `json:"supersede_stranded"`
}

// maxReconcileMergeBodyBytes caps the optional body. Its only field is a
// boolean, so a body anywhere near the cap is malformed by construction.
const maxReconcileMergeBodyBytes = 4 << 10

// strandedStageIdleThreshold is how long a dispatched/running stranded
// candidate must have shown NO database-stamped activity before the stranded
// arm may retire it (#4222).
//
// Why 24h: every runner heartbeat UPDATEs the stage row (RecordStageProgress),
// and migration 0001's stages_set_updated_at BEFORE UPDATE trigger bumps
// updated_at on every such write and on every transition. The phases that send
// no heartbeat (the verify gate, the acceptance zero-credential posture) are
// bounded by agent and gate timeouts measured in minutes to an hour, and every
// re-dispatch resets dispatched_at and updated_at, so no live single attempt is
// silent for a day. A stage idle that long on a run whose PR already merged is
// held by no runner.
//
// Clock domains: the cutoff is derived from the fishhawkd process clock
// (s.nowFunc) and compared with Postgres-stamped columns. Skew between the two
// is unbounded in principle (AGENTS.md, #3048) but only matters at the 24h
// boundary, and the row-locked re-check in SupersedeStrandedStageOnMerge
// compares the SAME cutoff against the SAME column, so the gate and the write
// cannot disagree about one row.
const strandedStageIdleThreshold = 24 * time.Hour

// strandedCandidate is one stage the stranded arm classified for retirement,
// pinned to the state it was classified in.
type strandedCandidate struct {
	stage        *run.Stage
	from         run.StageState
	lastActivity time.Time
}

// strandedLastActivity is a stage's latest DATABASE-stamped activity: the max
// of updated_at (stages_set_updated_at trigger, bumped by every transition and
// every heartbeat) and dispatched_at (migration 0072's trigger, stamped on every
// transition into dispatched). started_at is deliberately NOT consulted: it is
// stamped from the Go process clock by the same UPDATE that bumps updated_at, so
// updated_at dominates it, and mixing it in would put a second clock domain
// into the comparison.
func strandedLastActivity(st *run.Stage) time.Time {
	last := st.UpdatedAt
	if st.DispatchedAt != nil && st.DispatchedAt.After(last) {
		last = *st.DispatchedAt
	}
	return last
}

// strandedCandidateInFlight reports whether a stranded candidate state is one a
// runner may still hold (dispatched or running) and is therefore subject to the
// liveness gate. A pending gate is held by no runner.
func strandedCandidateInFlight(state run.StageState) bool {
	return state == run.StageStateDispatched || state == run.StageStateRunning
}

// decodeReconcileMergeRequest reads the optional body. A nil, empty or
// whitespace-only body is the zero request; anything else must be exactly one
// JSON object carrying only known fields (the reap_failure.go precedent), with
// no trailing data.
func decodeReconcileMergeRequest(r *http.Request) (reconcileMergeRequest, error) {
	var req reconcileMergeRequest
	if r.Body == nil {
		return req, nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxReconcileMergeBodyBytes+1))
	if err != nil {
		return req, fmt.Errorf("read body: %w", err)
	}
	if len(body) > maxReconcileMergeBodyBytes {
		return req, fmt.Errorf("body exceeds %d bytes", maxReconcileMergeBodyBytes)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return req, nil
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return reconcileMergeRequest{}, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return reconcileMergeRequest{}, errors.New("trailing data after the JSON object")
	}
	return req, nil
}

// strandedLiveDetail renders one stage the liveness gate (or the row-locked
// re-check) found active, for the 409 reconcile_merge_stage_live details.
func strandedLiveDetail(stageID uuid.UUID, stageType string, state run.StageState, lastActivity time.Time) map[string]any {
	return map[string]any{
		"stage_id":         stageID.String(),
		"stage_type":       stageType,
		"state":            string(state),
		"last_activity_at": lastActivity.UTC().Format(time.RFC3339Nano),
	}
}

// The three row-locked refusals that STOP the stranded sweep, as recorded in
// the 409 reconcile_merge_stage_live live_stages[].refusal discriminator. Each
// is evidence that something touched the stage after the handler's read.
const (
	strandedRefusalRecentlyActive = "recently_active" // a heartbeat or transition after the idle cutoff
	strandedRefusalStateChanged   = "state_changed"   // the stage moved since it was classified
	strandedRefusalAttemptChanged = "attempt_changed" // the stage was re-dispatched since it was classified
)

// strandedRefusal is the stage whose row-locked refusal stopped the stranded
// sweep.
type strandedRefusal struct {
	candidate    strandedCandidate
	lastActivity time.Time
	reason       string
}

func (r *strandedRefusal) detail() map[string]any {
	d := strandedLiveDetail(r.candidate.stage.ID, string(r.candidate.stage.Type), r.candidate.from, r.lastActivity)
	d["refusal"] = r.reason
	return d
}

// handleReconcileMerge implements POST /v0/runs/{run_id}/reconcile-merge
// (E64.2 / #3083) — the operator recovery for a run whose PR merged while a
// stage stayed parked, leaving Orchestrator.completeRun's #968 guard correctly
// refusing to complete it and every existing escape hatch recording something
// untrue.
//
// It supersedes exactly the pair-table-admissible parked stages, re-runs
// completion, and returns what moved. Two further arms (#4222):
//
//   - STRANDED (opt-in, body {"supersede_stranded": true}): also retire the
//     stages run.StrandedMergeSupersedable admits — a legacy run whose PR
//     merged while implement stayed `running` beside a never-opened review gate
//     can otherwise never settle. Each move goes through the row-locked,
//     state- and attempt-pinned run.StrandedStageMergeSuperseder capability,
//     which is deliberately outside the ordinary transition union. Without the
//     flag such a stage is left alone and the verb is byte-identical to before.
//   - SETTLE-ONLY: a merged run whose stages ALL already succeeded but whose
//     run row is still pending/running (a legacy run whose completion never
//     re-ran) moves nothing and re-runs completion, instead of answering 409.
//
// Refusals, ALL evaluated before any write so a refused reconcile leaves ZERO
// rows and moves ZERO stages:
//  0. 401 authentication_required — no authenticated identity at all; and
//     403 insufficient_scope (details.required_scope = write:runs) for a bearer
//     that authenticates but does not hold write:runs. It is rung ZERO
//     deliberately — ahead of the run_id parse, the unconfigured check and the
//     run lookup — so a refused caller learns nothing about whether the run
//     exists. Exactly two shapes are admitted: a bearer holding write:runs, and
//     a COOKIE SESSION (TokenID == ""), which requireWriteScope exempts by its
//     documented contract (middleware.go) — a signed-in operator session carries
//     no explicit scope list and stays bounded by requireRunAccount's ownership
//     and role-bounding. write:runs is the scope every sibling run-lifecycle
//     recovery verb already enforces (consolidate.go, reap_failure.go,
//     reset_branch.go, recover.go), and the OBSERVE half
//     (handleRecordMergeObservation) enforces the IDENTICAL rung — the
//     observe/settle pair must not diverge in who may call it (E45.95 / #3635);
//  1. 400 validation_failed — a non-UUID run_id (field run_id), or a body that
//     is neither empty nor exactly {"supersede_stranded": <bool>} (field body:
//     a wrong type, an unknown field, trailing data, over the size cap);
//  2. 503 reconcile_merge_unconfigured — the run/audit repositories are
//     unwired, or supersede_stranded was requested and the run repository does
//     not implement run.StrandedStageMergeSuperseder (details
//     missing_capability) — the arm refuses rather than degrading to an
//     ordinary transition that would apply the move on a stale premise;
//  3. 404 run_not_found;
//  4. 409 reconcile_merge_pr_not_merged — the run's PR is not OBSERVABLY merged
//     (no pr_merged / post_merge_observed / merge_observation_recorded entry on
//     its chain — the third is the row POST /v0/runs/{run_id}/record-merge-observation
//     appends after a live merged=true forge read, E64.32 / #3136). This is the guard
//     that stops the verb manufacturing a `succeeded` run for an unmerged
//     change: without it, an operator could settle a run whose work never
//     shipped. It applies to every arm, the stranded and settle-only arms
//     included. A chain-read failure is a 500, never a write — fail closed;
//  5. 409 reconcile_merge_not_applicable — the run holds no pair-table-
//     admissible parked stage, no already-superseded stage to repair, no
//     stranded candidate (only counted when supersede_stranded is set) and is
//     not in the settle-only shape (>= 1 stage, every stage succeeded, run not
//     terminal), so there is nothing for this verb to do;
//  6. 409 reconcile_merge_stage_live — supersede_stranded is set and at least
//     one dispatched/running candidate shows database-stamped activity
//     (strandedLastActivity) after now - strandedStageIdleThreshold. The WHOLE
//     call is refused: details.live_stages names each live stage with its
//     last_activity_at, plus idle_threshold_seconds. Pending candidates need no
//     liveness check.
//
// Rung 6 has a row-locked twin. The handler's idle cutoff is passed into
// SupersedeStrandedStageOnMerge, which re-reads the stage under its row lock
// and refuses when a heartbeat landed between this gate and the write
// (run.StageRecentlyActiveError: updated_at newer than the cutoff), or the
// stage moved (run.StageStateChangedError) or was re-dispatched
// (run.StageAttemptChangedError) since the classification. These are the only
// refusals that can follow a write: the parked sweep and any stranded move
// earlier in stage-sequence order have committed with their audit rows, so the
// handler stops the stranded sweep, skips the repair scan and the completion
// re-run (whatever touched the stage owns its next step) and answers the same
// 409 reconcile_merge_stage_live, with live_stages[].refusal naming which
// re-check fired and details.superseded listing what this call already moved.
//
// IDEMPOTENT: a second POST finds the stage already `superseded` (not admissible
// — `superseded` is in neither pair table), moves nothing, finds its audit row
// present, repairs nothing, and returns 200 with two empty lists. The repair
// scan EXCLUDES the stages this same invocation moved — parked AND stranded —
// so exactly one row per swept stage exists no matter how many times the verb
// is called.
func (s *Server) handleReconcileMerge(w http.ResponseWriter, r *http.Request) {
	// Rung 0. FIRST, before the id parse / unconfigured check / run lookup: a
	// refused caller must learn nothing about the run.
	if !s.requireWriteScope(w, r, "write:runs") {
		return
	}
	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}
	req, berr := decodeReconcileMergeRequest(r)
	if berr != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			`reconcile-merge body must be empty or {"supersede_stranded": <bool>}`,
			map[string]any{"field": "body", "error": berr.Error()})
		return
	}
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "reconcile_merge_unconfigured",
			"merge reconciliation requires run + audit repositories", nil)
		return
	}
	var strander run.StrandedStageMergeSuperseder
	if req.SupersedeStranded {
		sm, ok := s.cfg.RunRepo.(run.StrandedStageMergeSuperseder)
		if !ok {
			s.writeError(w, r, http.StatusServiceUnavailable, "reconcile_merge_unconfigured",
				"supersede_stranded requires a run repository implementing run.StrandedStageMergeSuperseder; this deployment's run repository does not, so no stranded stage can be retired",
				map[string]any{"missing_capability": "run.StrandedStageMergeSuperseder"})
			return
		}
		strander = sm
	}
	runRow, gerr := s.cfg.RunRepo.GetRun(r.Context(), runID)
	if gerr != nil {
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
	var admissible, alreadySuperseded, succeeded, total int
	var stranded []strandedCandidate
	for _, st := range stages {
		if st == nil {
			continue
		}
		total++
		if st.State == run.StageStateSucceeded {
			succeeded++
		}
		switch {
		case st.State == run.StageStateSuperseded:
			alreadySuperseded++
		case run.MergeSupersedable(st.Type, st.State):
			admissible++
		case req.SupersedeStranded && run.StrandedMergeSupersedable(st.Type, st.State):
			// Classified ONLY under the opt-in. Without it a stranded stage
			// is left exactly where it is (#968), as before #4222.
			stranded = append(stranded, strandedCandidate{
				stage: st, from: st.State, lastActivity: strandedLastActivity(st),
			})
		}
	}
	// The settle-only shape: every stage already succeeded but the run row was
	// never completed. Guard 4 above already proved the merge, and only
	// succeeded stages qualify, so this can never settle a failed or cancelled
	// run.
	settleOnly := total > 0 && succeeded == total && !runRow.State.IsTerminal()

	// Guard 5. `alreadySuperseded` keeps a repeat POST on the idempotent 200
	// path rather than a confusing 409 — and it is what makes the repair scan
	// reachable at all on a run whose sweep already moved everything.
	if admissible == 0 && alreadySuperseded == 0 && len(stranded) == 0 && !settleOnly {
		s.writeError(w, r, http.StatusConflict, "reconcile_merge_not_applicable",
			`this run holds no merge-supersedable parked stage and no superseded stage to repair; a stage in any other non-terminal state must run, settle or be cancelled — or, for an implement/review/acceptance stage stranded in flight on this merged run, retry with {"supersede_stranded": true}`,
			map[string]any{"run_id": runID.String(), "supersede_stranded": req.SupersedeStranded})
		return
	}

	// Guard 6 — the liveness gate, evaluated for the WHOLE candidate set before
	// any write. One live in-flight candidate refuses the call.
	var idleCutoff time.Time
	if len(stranded) > 0 {
		idleCutoff = s.nowFunc().Add(-strandedStageIdleThreshold)
		var live []map[string]any
		for _, c := range stranded {
			if !strandedCandidateInFlight(c.from) {
				continue
			}
			if c.lastActivity.After(idleCutoff) {
				live = append(live, strandedLiveDetail(c.stage.ID, string(c.stage.Type), c.from, c.lastActivity))
			}
		}
		if len(live) > 0 {
			s.writeError(w, r, http.StatusConflict, "reconcile_merge_stage_live",
				"a stranded stage showed activity inside the idle threshold, so a runner may still hold it; let it settle, or retry once it has been idle for the full threshold",
				map[string]any{
					"run_id":                 runID.String(),
					"live_stages":            live,
					"idle_threshold_seconds": int64(strandedStageIdleThreshold / time.Second),
				})
			return
		}
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
	movedIDs := make(map[uuid.UUID]struct{}, len(moved)+len(stranded))
	for _, m := range moved {
		movedIDs[m.StageID] = struct{}{}
	}

	if len(stranded) > 0 {
		strandedMoved, refusal := s.supersedeStrandedStagesOnMerge(r.Context(), runID, strander, stranded, idleCutoff)
		for _, m := range strandedMoved {
			// The stranded sweep's rows are not in the pre-sweep snapshot
			// either, so these ids MUST join the exclusion set or the repair
			// scan below would draw a second row for each.
			movedIDs[m.StageID] = struct{}{}
		}
		moved = append(moved, strandedMoved...)
		if refusal != nil {
			if moved == nil {
				moved = []supersededStage{}
			}
			s.writeError(w, r, http.StatusConflict, "reconcile_merge_stage_live",
				"a stranded stage showed activity, moved or was re-dispatched after the liveness check, so a runner may still hold it; it was not retired and no later stranded stage was either — the stages under details.superseded were retired by this call before the refusal",
				map[string]any{
					"run_id":                 runID.String(),
					"live_stages":            []map[string]any{refusal.detail()},
					"idle_threshold_seconds": int64(strandedStageIdleThreshold / time.Second),
					"superseded":             moved,
				})
			return
		}
	}

	// Repair scan: a stage that is already `superseded` but has no audit row is
	// the residue of a sweep whose CAS committed and whose append then failed.
	// Re-append with reason `repair`; transition NOTHING. Read the stages back
	// so a supersession landed by a concurrent path is repairable too.
	repaired := s.repairMissingSupersedeRows(r.Context(), runID, movedIDs, priorRows)

	// Re-run completion. The sweep made the parked stages terminal, so the
	// orchestrator's Advance can now route the all-terminal stage set through
	// completeRun (walking a still-pending run to running first). Best-effort,
	// exactly as the merged path's advance is. The settle-only arm reaches
	// here having moved nothing.
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

// supersedeStrandedStagesOnMerge is the stranded sweep of reconcile-merge's
// opt-in arm (#4222). The handler has already proved merge evidence and passed
// the liveness gate; this applies each move through the row-locked
// run.StrandedStageMergeSuperseder, pinned to the state the handler classified,
// to the attempt (StageAttemptToken of the classified dispatched_at, so a
// re-dispatch since the read is refused) and to the handler's idleCutoff (so a
// heartbeat since the read is refused).
//
// TRANSITION FIRST, THEN AUDIT, exactly as supersedeParkedStagesOnMerge: a row
// appended before a refused move would be an immutable record of a retirement
// that never happened. A failed append leaves a missing row the repair scan
// restores on a later call; a duplicate-index collision is the benign
// already-recorded outcome inside appendStageSupersededAudit.
//
// A typed row-locked refusal — run.StageRecentlyActiveError,
// run.StageStateChangedError or run.StageAttemptChangedError — writes no row
// and STOPS the sweep, returned with the stages already moved: each says the
// stage was touched after the handler's read, so a runner may hold it, and no
// later candidate (in particular a pending gate sequenced after it) is retired
// on that stale premise. Candidates are processed in stage-sequence order
// (ListStagesForRun's order), so an in-flight implement precedes the gates
// downstream of it. Any OTHER error is logged, writes no row and the sweep
// continues, as in supersedeParkedStagesOnMerge; the stage stays where it was
// for a later call.
func (s *Server) supersedeStrandedStagesOnMerge(ctx context.Context, runID uuid.UUID, strander run.StrandedStageMergeSuperseder, candidates []strandedCandidate, idleCutoff time.Time) ([]supersededStage, *strandedRefusal) {
	var moved []supersededStage
	for _, c := range candidates {
		if _, err := strander.SupersedeStrandedStageOnMerge(ctx, c.stage.ID, c.from, run.StageAttemptToken(c.stage.DispatchedAt), idleCutoff); err != nil {
			if refusal := classifyStrandedRefusal(c, err); refusal != nil {
				s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
					"merge supersede: stranded stage touched after the liveness check; refused (no audit row), stopping the stranded sweep",
					slog.String("run_id", runID.String()),
					slog.String("stage_id", c.stage.ID.String()),
					slog.String("refusal", refusal.reason),
					slog.String("idle_cutoff", idleCutoff.UTC().Format(time.RFC3339Nano)),
					slog.String("error", err.Error()))
				return moved, refusal
			}
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"merge supersede: stranded stage transition failed (no audit row)",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", c.stage.ID.String()),
				slog.String("error", err.Error()))
			continue
		}
		last := c.lastActivity
		rec := supersededStage{
			StageID:        c.stage.ID,
			StageType:      string(c.stage.Type),
			FromState:      string(c.from),
			Reason:         supersedeReasonOperatorReconcileStranded,
			lastActivityAt: &last,
			idleThreshold:  strandedStageIdleThreshold,
		}
		// The move committed; a swallowed append failure leaves a MISSING row
		// the repair scan restores on a later call, never a false one.
		_ = s.appendStageSupersededAudit(ctx, runID, rec)
		moved = append(moved, rec)
	}
	return moved, nil
}

// classifyStrandedRefusal maps a typed row-locked refusal to the
// strandedRefusal that stops the sweep, or nil for any other error.
func classifyStrandedRefusal(c strandedCandidate, err error) *strandedRefusal {
	var active run.StageRecentlyActiveError
	if errors.As(err, &active) {
		return &strandedRefusal{candidate: c, lastActivity: active.LastActivity, reason: strandedRefusalRecentlyActive}
	}
	var sce run.StageStateChangedError
	if errors.As(err, &sce) {
		return &strandedRefusal{candidate: c, lastActivity: c.lastActivity, reason: strandedRefusalStateChanged}
	}
	var ace run.StageAttemptChangedError
	if errors.As(err, &ace) {
		return &strandedRefusal{candidate: c, lastActivity: c.lastActivity, reason: strandedRefusalAttemptChanged}
	}
	return nil
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
// ATOMICITY (#3083 fix-up, hardened by E64.29 / #3133). The check-then-append is
// serialized under supersedeRepairMu AND re-reads the CURRENT audit rows inside
// the lock, so two concurrent reconcile-merge requests in ONE PROCESS can no
// longer both observe a missing row and both append: the second acquires the
// lock after the first commits, its fresh read sees the row, and it skips the
// stage. The pre-sweep priorRows snapshot alone could not close this — both
// requests captured it before either wrote — so the fresh under-lock read is
// what makes the same-process guarantee hold.
//
// ACROSS PROCESSES the mutex is no guarantee at all (each fishhawkd holds its
// own), so the durable control is migration 0081's partial unique index on
// (run_id, stage_id): the losing append is REFUSED by the database and its error
// satisfies audit.IsStageSupersededByMergeDuplicate. That loser records the
// stage as present and omits it from the returned Repaired list — the row is
// durable, but this invocation did not restore it.
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
			if audit.IsStageSupersededByMergeDuplicate(aerr) {
				// Migration 0081's index refused the second row: a concurrent
				// writer — in this process or another fishhawkd — already
				// recorded this supersession. The row demonstrably EXISTS, so
				// record it in the in-scan set (nothing later in this scan may
				// try again), but it is NOT reported as repaired: Repaired
				// means "this invocation restored the row", and this invocation
				// wrote nothing.
				current[st.ID] = struct{}{}
				continue
			}
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
