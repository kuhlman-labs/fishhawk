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
// flagged. Rule is the machine-readable classifier (no_blocking_criterion,
// missing_source_ref, missing_rationale, empty_id, duplicate_id).
// CriterionID names the offending criterion; it is empty for the
// plan-level no_blocking_criterion presence finding, which has no single
// criterion to point at. Detail is a short human-readable explanation.
type AcceptanceFinding struct {
	Rule        string `json:"rule"`
	CriterionID string `json:"criterion_id,omitempty"`
	Detail      string `json:"detail"`
}

// Acceptance pre-check finding rules.
const (
	acceptanceRuleNoBlockingCriterion = "no_blocking_criterion"
	acceptanceRuleMissingSourceRef    = "missing_source_ref"
	acceptanceRuleMissingRationale    = "missing_rationale"
	acceptanceRuleEmptyID             = "empty_id"
	acceptanceRuleDuplicateID         = "duplicate_id"
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
	findings := evaluateAcceptanceCriteria(v)

	blockingCount := 0
	for _, c := range v.AcceptanceCriteria {
		if criterionBlocking(c) {
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

// evaluateAcceptanceCriteria runs the deterministic acceptance-criteria
// rules over a decoded Verification and returns the findings. It always
// returns a non-nil slice so the audit payload records [] (not null) on a
// clean-and-checked plan — the "checked and clean" contract shared with
// the scope pre-check.
//
// Rules:
//   - no_blocking_criterion — no criterion is effectively blocking AND
//     out_of_scope is empty. A non-empty out_of_scope is the justified
//     escape hatch: it declares what the change deliberately does not
//     cover, so an absent blocking criterion is not necessarily a gap.
//   - missing_source_ref — an explicit criterion with no source_ref.
//   - missing_rationale — an inferred criterion with no rationale
//     (defense-in-depth: the schema conditional normally rejects this
//     upstream, but the pre-check stays order-independent).
//   - empty_id / duplicate_id — id integrity for the join key.
func evaluateAcceptanceCriteria(v plan.Verification) []AcceptanceFinding {
	findings := []AcceptanceFinding{}

	hasBlocking := false
	seen := make(map[string]struct{}, len(v.AcceptanceCriteria))
	for _, c := range v.AcceptanceCriteria {
		if criterionBlocking(c) {
			hasBlocking = true
		}
		if c.ID == "" {
			findings = append(findings, AcceptanceFinding{
				Rule:   acceptanceRuleEmptyID,
				Detail: "acceptance criterion has an empty id (ids are the join key across execution, evidence, and feedback)",
			})
		} else if _, dup := seen[c.ID]; dup {
			findings = append(findings, AcceptanceFinding{
				Rule:        acceptanceRuleDuplicateID,
				CriterionID: c.ID,
				Detail:      "duplicate acceptance criterion id (ids must be unique within a plan)",
			})
		} else {
			seen[c.ID] = struct{}{}
		}
		if c.Source == plan.CriterionSourceExplicit && c.SourceRef == "" {
			findings = append(findings, AcceptanceFinding{
				Rule:        acceptanceRuleMissingSourceRef,
				CriterionID: c.ID,
				Detail:      "explicit criterion is missing source_ref (an explicit criterion must cite where the ticket/spec states it)",
			})
		}
		if c.Source == plan.CriterionSourceInferred && c.Rationale == "" {
			findings = append(findings, AcceptanceFinding{
				Rule:        acceptanceRuleMissingRationale,
				CriterionID: c.ID,
				Detail:      "inferred criterion is missing rationale (an inferred criterion must justify why it was derived)",
			})
		}
	}

	if !hasBlocking && len(v.OutOfScope) == 0 {
		findings = append(findings, AcceptanceFinding{
			Rule:   acceptanceRuleNoBlockingCriterion,
			Detail: "no blocking acceptance criterion and no verification.out_of_scope justification (a plan must carry at least one blocking criterion or declare what is deliberately out of scope)",
		})
	}

	return findings
}

// criterionBlocking applies the schema's blocking default: an omitted
// (nil) blocking is true, matching the plan.AcceptanceCriterion.Blocking
// pointer contract (backend/internal/plan/plan.go).
func criterionBlocking(c plan.AcceptanceCriterion) bool {
	return c.Blocking == nil || *c.Blocking
}

// resolveAcceptanceStage reads the run's workflow spec, looks up the active
// workflow, and returns the ID of its first acceptance stage. It mirrors
// resolveImplementConstraints' spec-read contract: returns ok=false
// (fail-open) when the spec is absent, unparseable, the workflow is
// missing, or it has no acceptance stage. This is the stage-conditional
// off-switch — a run whose workflow configures no acceptance stage yields
// ok=false, so runAcceptancePrecheck writes no entry and never blocks.
func (s *Server) resolveAcceptanceStage(ctx context.Context, runRow *run.Run) (string, bool) {
	if runRow.WorkflowSpec == nil {
		return "", false
	}
	parsed, err := spec.ParseBytes(runRow.WorkflowSpec)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "acceptance precheck: parse workflow spec failed",
			slog.String("run_id", runRow.ID.String()),
			slog.String("error", err.Error()),
		)
		return "", false
	}
	wf, ok := parsed.Workflows[runRow.WorkflowID]
	if !ok {
		return "", false
	}
	for _, st := range wf.Stages {
		if st.Type == spec.StageTypeAcceptance {
			return st.ID, true
		}
	}
	return "", false
}
