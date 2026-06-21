package server

import (
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
	"github.com/kuhlman-labs/fishhawk/backend/internal/drive"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// CategoryPlanRevised is the audit-log category for the entry the
// revise handler writes when a plan stage is re-opened to re-plan in
// place against a binding operator design constraint (E22.X / #1099).
// The payload carries the rendered operator constraint (so the plan
// prompt renderer delivers it as a binding "Revision constraint"
// section on the re-dispatch) plus the bounded-pass receipt fields.
// This entry IS the durable record of the revise bound: the handler
// counts prior entries of this category for the stage to enforce the
// configured max-passes ceiling — there is no dedicated column, exactly
// as fixup counts stage_fixup_triggered.
const CategoryPlanRevised = "plan_revised"

// defaultMaxRevisePasses bounds the number of revise passes a single
// plan stage may consume. Default 1 — a revise is a bounded,
// operator-gated single pass, never an unbounded auto-loop. Mirrors
// defaultMaxFixupPasses.
const defaultMaxRevisePasses = 1

// defaultReviseCeiling is the absolute hard cap on total revise passes a
// single plan stage may consume, INCLUDING any operator-forced override
// pass. The normal budget is defaultMaxRevisePasses (1); once spent, an
// operator may grant ONE additional pass via force_additional_pass, but
// never past this ceiling. At the ceiling the handler refuses with the
// distinct revise_ceiling_reached error (a hard stop: reject → fresh-run
// replan), not revise_budget_exhausted. Mirrors defaultFixupCeiling.
const defaultReviseCeiling = 3

// reviseRequest is the JSON body of POST /v0/stages/{stage_id}/revise.
// Constraint is the operator's binding design constraint the planner
// must revise the prior plan to satisfy — REQUIRED. ForceAdditionalPass
// is the bounded operator override: when true it grants ONE revise pass
// beyond the normal budget (defaultMaxRevisePasses), hard-capped at
// defaultReviseCeiling total passes per stage.
type reviseRequest struct {
	Constraint          string `json:"constraint"`
	ForceAdditionalPass bool   `json:"force_additional_pass"`
}

// maxRevisionConstraintBytes caps the operator constraint stored on the
// plan_revised audit entry, matching the 4000-byte cap the prompt
// renderer applies to the other resume channels (clarification answers,
// approval conditions, prior-rejection feedback).
const maxRevisionConstraintBytes = 4000

// handleRevisePlan implements POST /v0/stages/{stage_id}/revise.
//
// The plan-gate revise verdict (E22.X / #1099) is the third plan-gate
// option alongside approve/reject. A `revise` re-plans IN PLACE in the
// same run: it re-opens the parked plan stage from awaiting_approval back
// to pending, re-dispatches the plan stage once with the operator's
// binding design constraint injected (the #558 binding-conditions
// channel, via a DEDICATED "Revision constraint" prompt section) and the
// prior plan carried as the revision base, then re-enters the normal
// review → approve gate. It is the plan-stage analogue of
// handleFixupStage (a bounded, operator-gated re-open of a HEALTHY gate)
// and a generalization of handleAnswerClarification's re-open-and-inject
// machinery.
//
// write:approvals is the correct scope: this is the #558 binding-
// conditions / gate-answer family — the operator answering a parked gate.
//
// Failure modes:
//   - non-plan stage, or a plan stage not at awaiting_approval → 409
//     revise_not_applicable
//   - revise budget already spent (no override)                 → 409
//     revise_budget_exhausted
//   - hard ceiling reached (override cannot push past)           → 409
//     revise_ceiling_reached
//   - empty constraint                                           → 400
//     validation_failed
//
// Distinct from POST /v0/stages/{stage_id}/retry: retry re-opens a FAILED
// stage; revise re-opens a HEALTHY plan gate and is bounded
// (defaultMaxRevisePasses) by counting prior plan_revised audit entries.
func (s *Server) handleRevisePlan(w http.ResponseWriter, r *http.Request) {
	if !s.requireWriteScope(w, r, "write:approvals") {
		return
	}
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "revise_unconfigured",
			"revise endpoint requires run and audit repositories", nil)
		return
	}

	stageID, err := uuid.Parse(r.PathValue("stage_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"stage_id must be a valid UUID",
			map[string]any{"field": "stage_id", "got": r.PathValue("stage_id")})
		return
	}

	var reqBody reviseRequest
	if r.Body != nil {
		if decErr := json.NewDecoder(r.Body).Decode(&reqBody); decErr != nil && !errors.Is(decErr, io.EOF) {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"request body must be valid JSON {constraint, force_additional_pass}",
				map[string]any{"error": decErr.Error()})
			return
		}
	}
	constraint := strings.TrimSpace(reqBody.Constraint)
	if constraint == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"constraint must be a non-empty binding design constraint for the planner to revise the plan against",
			map[string]any{"field": "constraint"})
		return
	}
	if len(constraint) > maxRevisionConstraintBytes {
		constraint = constraint[:maxRevisionConstraintBytes]
	}

	// Pre-fetch the stage for the subject-binding guard and the audit
	// lookups below.
	stage, err := s.cfg.RunRepo.GetStage(r.Context(), stageID)
	if err != nil {
		if errors.Is(err, run.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "stage_not_found",
				"no stage with that id", nil)
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get stage failed", map[string]any{"error": err.Error()})
		return
	}

	// Subject-binding guard: an MCP run-bound token may only revise stages
	// within its own run. Subject format is "mcp:run:<uuid>". Mirrors the
	// fixup handler.
	if strings.HasPrefix(IdentityFrom(r.Context()).Subject, "mcp:run:") {
		runIDStr := strings.TrimPrefix(IdentityFrom(r.Context()).Subject, "mcp:run:")
		subjectRunID, parseErr := uuid.Parse(runIDStr)
		if parseErr != nil {
			s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
				"mcp token subject is malformed", nil)
			return
		}
		if subjectRunID != stage.RunID {
			s.writeError(w, r, http.StatusForbidden, "cross_run_revise",
				"mcp token may only revise stages within its own run",
				map[string]any{
					"token_run_id": subjectRunID.String(),
					"stage_run_id": stage.RunID.String(),
				})
			return
		}
	}

	// Count prior revise passes for this stage to enforce the bound — the
	// durable record is the plan_revised audit entry (no dedicated column),
	// exactly as fixup counts stage_fixup_triggered.
	priorPasses, err := s.countRevisePasses(r.Context(), stage.RunID, stageID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"count prior revise passes failed", map[string]any{"error": err.Error()})
		return
	}

	// Discount the NORMAL revise budget by the number of prior passes that
	// dropped previously-scoped files (#1257): a regressing revise pass
	// refunds the budget so the operator gets a free recovery pass, while
	// PriorPassCount keeps the hard ceiling counting all passes (bounded).
	regressedPasses, err := s.countRegressedRevisePasses(r.Context(), stage.RunID, stageID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"count regressed revise passes failed", map[string]any{"error": err.Error()})
		return
	}
	budgetPasses := priorPasses - regressedPasses
	if budgetPasses < 0 {
		budgetPasses = 0
	}

	// Record the plan_revised audit entry (carrying the binding constraint
	// the plan prompt renderer reads back) via the OnAdmit hook, which fires
	// after the gate + budget decision but BEFORE the stage is re-opened.
	// This is the #1099 fix-up ordering: the prompt path loads the constraint
	// EXCLUSIVELY from this entry, so were the transition to land first and
	// the append to fail best-effort, the orchestrator would re-dispatch a
	// constraint-less plan. A failed append aborts the revise with the plan
	// stage left at its approval gate (errReviseAuditAppendFailed → 500).
	dec, err := run.RevisePlanStage(r.Context(), s.cfg.RunRepo, stageID, run.ReviseOptions{
		PriorPassCount:      priorPasses,
		BudgetPassCount:     budgetPasses,
		MaxPasses:           defaultMaxRevisePasses,
		ForceAdditionalPass: reqBody.ForceAdditionalPass,
		HardCeiling:         defaultReviseCeiling,
		OnAdmit: func(d *run.ReviseDecision) error {
			return s.writeReviseAudit(r, d, constraint, priorPasses)
		},
	})
	if err != nil {
		switch {
		case errors.Is(err, run.ErrNotFound):
			s.writeError(w, r, http.StatusNotFound, "stage_not_found",
				"no stage with that id", nil)
			return
		case errors.Is(err, run.ErrReviseCeilingReached):
			// Placed BEFORE the budget-exhausted arm so the distinct
			// hard-stop error is not masked (the override cannot push past
			// this).
			s.writeError(w, r, http.StatusConflict, "revise_ceiling_reached",
				err.Error(), map[string]any{"ceiling": defaultReviseCeiling, "used": priorPasses})
			return
		case errors.Is(err, run.ErrReviseBudgetExhausted):
			s.writeError(w, r, http.StatusConflict, "revise_budget_exhausted",
				err.Error(), map[string]any{"max_passes": defaultMaxRevisePasses, "used": priorPasses})
			return
		case errors.Is(err, run.ErrReviseNotApplicable):
			s.writeError(w, r, http.StatusConflict, "revise_not_applicable",
				err.Error(),
				map[string]any{"stage_type": string(stage.Type), "stage_state": string(stage.State)})
			return
		case errors.Is(err, errReviseAuditAppendFailed):
			// The pre-transition audit append failed, so the stage was NEVER
			// re-opened — the gate is intact and the operator can retry. Fail
			// loud rather than re-dispatch a constraint-less plan (#1099).
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"append plan_revised audit entry failed; revise aborted with the plan stage left at its approval gate",
				map[string]any{"error": err.Error()})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"revise failed", map[string]any{"error": err.Error()})
		return
	}

	// The revise re-open lands the stage in pending; hand off to the
	// orchestrator to walk pending → dispatched and fire workflow_dispatch.
	// Orchestrator failures are logged but don't fail the request: the
	// audit row recorded the intent and the stage is in pending, so an
	// operator can re-fire Advance manually (a local-runner run re-runs via
	// the operator's next fishhawk_run_stage plan).
	if dec.Stage.State == run.StageStatePending && s.cfg.Orchestrator != nil {
		if _, err := s.cfg.Orchestrator.Advance(r.Context(), dec.Stage.RunID); err != nil {
			s.cfg.Logger.LogAttrs(r.Context(), slog.LevelError,
				"orchestrator advance failed for revise",
				slog.String("run_id", dec.Stage.RunID.String()),
				slog.String("stage_id", dec.Stage.ID.String()),
				slog.String("error", err.Error()))
		}
		if updated, err := s.cfg.RunRepo.GetStage(r.Context(), dec.Stage.ID); err == nil {
			dec.Stage = updated
		}
		// Drive (#1256): the revise re-open is the revise_replan transition
		// point — the orchestrator handoff above IS the auto-advance
		// (workflow_dispatch for runner_kind github_actions), so stamp the
		// run_auto_advanced entry that surfaces the required next action on
		// the authoritative REST run resource; runner_kind local parks with a
		// host-side run_plan_stage next action instead (ADR-024: the runner
		// is host-spawned, the backend has no execution channel to it).
		// Mirrors recordDrivePlanApproved on the approve path; placed after
		// the Advance attempt so the entry documents an advance actually
		// attempted, and the re-open-to-pending guard ensures it fires only
		// when the revise genuinely re-opened the stage.
		s.recordDriveReviseReplan(r.Context(), dec.Stage)
	}

	// Sticky status comment (E20.4 / #330): a revise flips the plan stage
	// back to pending / dispatched; the status comment should reflect that.
	s.notifyStatusUpdate(r.Context(), dec.Stage.RunID, "plan_revised")

	s.writeJSON(w, r, http.StatusOK, toStageResponse(dec.Stage))
}

// recordDriveReviseReplan stamps the drive engine's revise_replan rule
// (#1256) after a plan-gate revise re-opens the plan stage. No-ops for
// non-drive runs, when no engine is wired, or on a run read failure
// (best-effort: the revise already landed; a missing stamp degrades
// attribution, never the run). For runner_kind github_actions the entry
// records the advance the orchestrator's workflow_dispatch fired; for
// runner_kind local it records the park (Parked=true) with the
// run_plan_stage next action that surfaces on the REST run resource and
// MCP get_run_status. The entry is keyed to the re-opened plan stage.
func (s *Server) recordDriveReviseReplan(ctx context.Context, stage *run.Stage) {
	if s.drive == nil || s.cfg.RunRepo == nil {
		return
	}
	runRow, err := s.cfg.RunRepo.GetRun(ctx, stage.RunID)
	if err != nil || !runRow.Drive {
		return
	}
	out := drive.EvaluateReviseReplan(runRow.RunnerKind)
	adv := drive.Advance{
		Rule: drive.RuleReviseReplan,
		From: "plan:revised",
	}
	if out.Advance {
		adv.To = "plan:dispatched"
		adv.Event = "plan gate revised; orchestrator re-dispatched the plan stage via workflow_dispatch"
	} else {
		adv.To = "plan:ready"
		adv.Event = "plan gate revised; runner_kind local parks for a host-side re-dispatch"
		adv.Parked = true
		adv.NextAction = out.NextAction
	}
	s.drive.Record(ctx, stage.RunID, &stage.ID, adv)
}

// countRevisePasses returns the number of prior plan_revised audit
// entries recorded for the stage — the durable revise-pass counter the
// bound is enforced against. Mirrors countFixupPasses.
func (s *Server) countRevisePasses(ctx context.Context, runID, stageID uuid.UUID) (int, error) {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryPlanRevised)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if e.StageID != nil && *e.StageID == stageID {
			n++
		}
	}
	return n, nil
}

// countRegressedRevisePasses returns the number of prior plan_scope_regression
// audit entries for the stage whose Regressed flag is true — the count of
// revise passes that DROPPED previously-scoped files (#1257). The revise
// handler subtracts this from the prior-pass count to discount the NORMAL
// budget (a regressing pass refunds the budget), while the hard ceiling
// keeps counting all passes via PriorPassCount. Mirrors countRevisePasses'
// stage-filtered shape; a malformed payload is skipped (not counted).
func (s *Server) countRegressedRevisePasses(ctx context.Context, runID, stageID uuid.UUID) (int, error) {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, categoryPlanScopeRegression)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if e.StageID == nil || *e.StageID != stageID {
			continue
		}
		var p ScopeRegressionPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			continue
		}
		if p.Regressed {
			n++
		}
	}
	return n, nil
}

// errReviseAuditAppendFailed wraps an AppendChained failure from
// writeReviseAudit so handleRevisePlan can distinguish the
// must-not-proceed audit outage (the #1099 ordering hazard) from the
// other RevisePlanStage error classes and surface a precise 500. It is
// returned through ReviseOptions.OnAdmit, which fires BEFORE the
// transition, so the revise aborts with the plan stage still parked at
// its approval gate.
var errReviseAuditAppendFailed = errors.New("append plan_revised audit entry failed")

// writeReviseAudit appends a plan_revised entry capturing the rendered
// operator constraint (so the plan prompt renderer delivers it as a
// binding "Revision constraint" section on the re-dispatch), the
// bounded-pass receipt fields, and whether the pass was operator-forced.
// The `conditions` key carries the constraint blob the plan-stage prompt
// builder reads back (loadRevisionConstraint), mirroring how
// clarification_answered carries the resumed plan's answers.
//
// It is wired as RevisePlanStage's OnAdmit hook so it runs BEFORE the
// awaiting_approval → pending transition: the plan prompt path loads the
// constraint EXCLUSIVELY from this entry, so an append failure MUST abort
// the revise rather than let the orchestrator re-dispatch a constraint-less
// plan (#1099). On failure it logs and returns errReviseAuditAppendFailed,
// which RevisePlanStage propagates to leave the stage at its gate.
func (s *Server) writeReviseAudit(r *http.Request, dec *run.ReviseDecision, constraint string, priorPasses int) error {
	id := IdentityFrom(r.Context())
	subject := id.Subject
	if subject == "" {
		subject = "anonymous"
	}
	actorKind := actorKindForSubject(subject)

	passOrdinal := priorPasses + 1
	fields := map[string]any{
		"stage_id":         dec.Stage.ID.String(),
		"prior_state":      string(dec.PriorState),
		"conditions":       constraint,
		"pass_ordinal":     passOrdinal,
		"max_passes":       defaultMaxRevisePasses,
		"hard_ceiling":     defaultReviseCeiling,
		"remaining_budget": dec.RemainingBudget,
		"forced":           dec.Forced,
		"actor":            subject,
	}
	payload, _ := json.Marshal(fields)

	if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:        dec.Stage.RunID,
		StageID:      &dec.Stage.ID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryPlanRevised,
		ActorKind:    &actorKind,
		ActorSubject: &subject,
		Payload:      payload,
	}); err != nil {
		s.cfg.Logger.Error("audit append failed for revise",
			"run_id", dec.Stage.RunID,
			"stage_id", dec.Stage.ID,
			"error", err.Error(),
		)
		return fmt.Errorf("%w: %v", errReviseAuditAppendFailed, err)
	}
	return nil
}
