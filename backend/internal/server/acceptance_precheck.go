package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// categoryPlanAcceptancePrecheck is the audit-log category for the entry
// runAcceptancePrecheck writes when it evaluates a plan's
// verification.acceptance_criteria against the run's configured acceptance
// stage (#1533, ADR-049 decision #4). Like plan_scope_precheck, the entry
// is the MCP-readable proxy for "the plan gate ran the acceptance
// pre-check": it is written even on a clean criteria set (empty findings)
// so a reader can distinguish "checked and clean" from "never checked" (a
// run whose workflow has no acceptance stage, or one predating this
// feature).
const categoryPlanAcceptancePrecheck = "plan_acceptance_precheck"

// AcceptancePrecheckPayload is the audit-payload shape for a
// plan_acceptance_precheck entry (#1533). Findings enumerates the
// deterministic acceptance-criteria defects the pre-check found;
// CriteriaCount/BlockingCount/OutOfScopeCount carry the counts so a
// downstream surface can render coverage headroom even when Findings is
// empty. Findings marshals as [] (not null) on a clean-and-checked plan,
// matching ScopePrecheckPayload's contract.
type AcceptancePrecheckPayload struct {
	WorkflowID        string              `json:"workflow_id"`
	AcceptanceStageID string              `json:"acceptance_stage_id"`
	Findings          []AcceptanceFinding `json:"findings"`
	CriteriaCount     int                 `json:"criteria_count"`
	BlockingCount     int                 `json:"blocking_count"`
	OutOfScopeCount   int                 `json:"out_of_scope_count"`
}

// AcceptanceFinding is one deterministic defect the acceptance pre-check
// flagged. It is a type ALIAS of plan.AcceptanceFinding (the rule set moved to
// the plan package in #1596 so intake and the plan gate share ONE evaluator),
// so AcceptancePrecheckPayload's JSON shape and every existing reference in
// plan.go / plan_test.go / acceptance_integration_test.go compiles and
// marshals unchanged.
type AcceptanceFinding = plan.AcceptanceFinding

// Acceptance pre-check finding rules referenced by the server and its tests.
// These alias the canonical plan-package constants (the single source), so the
// server's callers keep their identifiers while the rule strings live in
// exactly one place. The remaining rules (missing_rationale, empty_id) are
// referenced only through plan.Rule* directly.
const (
	acceptanceRuleNoBlockingCriterion = plan.RuleNoBlockingCriterion
	acceptanceRuleMissingSourceRef    = plan.RuleMissingSourceRef
	acceptanceRuleDuplicateID         = plan.RuleDuplicateID
)

// runAcceptancePrecheck evaluates an uploaded plan's
// verification.acceptance_criteria against the run's configured acceptance
// stage and records the result as an advisory plan_acceptance_precheck
// audit entry (#1533, ADR-049 decision #4). It is the acceptance-criteria
// sibling of runScopePrecheck, shifting a deterministic acceptance-quality
// gap left to the plan gate: a plan with no blocking criterion (and no
// out_of_scope justification), an explicit criterion missing its
// source_ref, an inferred criterion missing its rationale, or a
// duplicate/empty id is flagged before the human approves.
//
// Stage-conditional: the pre-check runs ONLY when the run's workflow
// configures an acceptance stage (resolveAcceptanceStage returns ok). A
// workflow with no acceptance stage means acceptance criteria are not part
// of that run's contract — so there is NO entry and NO finding, ever. This
// is the issue's off-switch: enforcement follows the workflow's own shape.
//
// Advisory-only and fail-open throughout: a nil RunRepo/AuditRepo, a
// GetRun error, a missing/unparseable workflow spec, a workflow with no
// acceptance stage, or a raw-body unmarshal error each produces NO block
// (the upload is never unwound) and, on the fail-open paths, no entry.
// Every failure WARN-logs and returns, matching runScopePrecheck's
// degradation contract.
//
// The criteria are decoded from the RAW plan body with json.Unmarshal —
// deliberately NOT plan.Parse. plan.Parse runs semanticCheck, which
// rejects duplicate acceptance_criteria ids
// (backend/internal/plan/validate.go), so a duplicate-id plan would
// fail-open out of the pre-check and never be flagged BY the pre-check as
// the duplicate_id rule requires. handleShipPlan has already
// schema-validated the body, so the unmarshal is shape-safe.
//
// Returns the computed result so handleShipPlan can thread it into the
// plan-review prompt's gate-evidence section; nil on every fail-open path.
// An audit-append failure still returns the computed result — the entry is
// observability, the evaluation itself succeeded.
func (s *Server) runAcceptancePrecheck(ctx context.Context, runID, stageID uuid.UUID, planBody []byte) *AcceptancePrecheckPayload {
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		return nil
	}

	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "acceptance precheck: get run failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
		return nil
	}

	acceptanceStageID, ok := s.resolveAcceptanceStage(ctx, runRow)
	if !ok {
		// Fail-open + stage-conditional: no spec, unparseable spec, missing
		// workflow, or no acceptance stage — acceptance criteria are not part
		// of this run's contract, so write nothing and never block.
		return nil
	}

	// Decode the criteria from the RAW body (NOT plan.Parse — see the doc
	// comment). handleShipPlan already schema-validated the body, so the
	// unmarshal is shape-safe; a decode error here is an internal
	// inconsistency — WARN-log and fail-open.
	var decoded struct {
		Verification plan.Verification `json:"verification"`
	}
	if err := json.Unmarshal(planBody, &decoded); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "acceptance precheck: unmarshal verification failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
		return nil
	}

	v := decoded.Verification
	findings := plan.EvaluateAcceptanceCriteria(v)

	blockingCount := 0
	for _, c := range v.AcceptanceCriteria {
		if plan.CriterionBlocking(c) {
			blockingCount++
		}
	}

	result := &AcceptancePrecheckPayload{
		WorkflowID:        runRow.WorkflowID,
		AcceptanceStageID: acceptanceStageID,
		Findings:          findings,
		CriteriaCount:     len(v.AcceptanceCriteria),
		BlockingCount:     blockingCount,
		OutOfScopeCount:   len(v.OutOfScope),
	}
	payload, _ := json.Marshal(result)
	systemKind := audit.ActorKind("system")
	if _, aerr := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  categoryPlanAcceptancePrecheck,
		ActorKind: &systemKind,
		Payload:   payload,
	}); aerr != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "acceptance precheck: append audit entry failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", aerr.Error()),
		)
	}
	return result
}

// resolveAcceptanceStage reads the run's workflow spec, looks up the active
// workflow, and returns the ID of its first acceptance stage. It mirrors
// resolveImplementConstraints' spec-read contract: returns ok=false
// (fail-open) when the spec is absent, unparseable, the workflow is
// missing, or it has no acceptance stage. This is the stage-conditional
// off-switch — a run whose workflow configures no acceptance stage yields
// ok=false, so runAcceptancePrecheck writes no entry and never blocks.
func (s *Server) resolveAcceptanceStage(ctx context.Context, runRow *run.Run) (string, bool) {
	st, ok := s.resolveAcceptanceStageSpec(ctx, runRow)
	if !ok {
		return "", false
	}
	return st.ID, true
}

// resolveAcceptanceStageSpec is resolveAcceptanceStage's stage-returning
// core: the same fail-open spec read, yielding the full spec.Stage so
// consumers that need more than the ID (the E31.4 egress allowance read in
// resolveAcceptanceTargetURL) share one spec-read contract.
func (s *Server) resolveAcceptanceStageSpec(ctx context.Context, runRow *run.Run) (spec.Stage, bool) {
	if runRow.WorkflowSpec == nil {
		return spec.Stage{}, false
	}
	parsed, err := spec.ParseBytes(runRow.WorkflowSpec)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "acceptance precheck: parse workflow spec failed",
			slog.String("run_id", runRow.ID.String()),
			slog.String("error", err.Error()),
		)
		return spec.Stage{}, false
	}
	wf, ok := parsed.Workflows[runRow.WorkflowID]
	if !ok {
		return spec.Stage{}, false
	}
	for _, st := range wf.Stages {
		if st.Type == spec.StageTypeAcceptance {
			return st, true
		}
	}
	return spec.Stage{}, false
}
