package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/policy"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// categoryPlanScopePrecheck is the audit-log category for the entry
// runScopePrecheck writes when it evaluates a plan's scope.files against
// the run's implement-stage path constraints (#658). The entry is the
// MCP-readable proxy for "the plan gate ran the scope pre-check": it is
// written even on a clean scope (empty violations) so a reader can
// distinguish "checked and clean" from "never checked" (an older run
// predating this feature).
const categoryPlanScopePrecheck = "plan_scope_precheck"

// ScopePrecheckPayload is the audit-payload shape for a
// plan_scope_precheck entry (#658). Violations reuses policy.Violation
// so the plan-gate pre-check payload matches the post-implement
// policy_evaluated gate's shape exactly — the MCP read side decodes the
// same JSON contract. ScannedFiles is the count of scope.files the
// pre-check evaluated toward the max_files_changed cap — the exempted
// count from policy.CountedFileCount, so generated/vendored paths
// (sqlc */db/*.go, vendor/) are excluded exactly as they are at the
// post-implement policy gate (a db-only scope reports scanned_files ==
// 0). MaxFilesChanged is the resolved implement-stage
// cap (#983; 0 = no cap configured) so downstream surfaces can render
// headroom (scanned_files vs cap) even when violations is empty — the
// 29/30 near-miss a violations-only payload makes invisible.
type ScopePrecheckPayload struct {
	WorkflowID       string             `json:"workflow_id"`
	ImplementStageID string             `json:"implement_stage_id"`
	Violations       []policy.Violation `json:"violations"`
	ScannedFiles     int                `json:"scanned_files"`
	MaxFilesChanged  int                `json:"max_files_changed"`
}

// runScopePrecheck evaluates an uploaded plan's scope.files against the
// run's implement-stage path constraints and records the result as an
// advisory plan_scope_precheck audit entry (#658). It shifts a
// deterministic implement-stage category-B path failure left to the plan
// gate: e.g. a feature_change plan listing .github/workflows/** (which
// feature_change forbids by design) is flagged before the human approves,
// so the operator sees "this plan's scope hits forbidden_paths — wrong
// workflow?" rather than discovering it after the implement stage runs.
//
// It reuses backend/internal/policy — the same matcher the post-implement
// gate runs — so the plan-time verdict is identical to the verdict the
// implement stage would produce for the same files.
//
// Advisory-only and fail-open: a missing/unparseable workflow spec or a
// run whose workflow has no implement stage produces NO entry and NO
// block (the upload is never unwound). Only the path/scope-knowable
// constraints are evaluated — forbidden_paths, allowed_paths, and
// max_files_changed; required_outcomes is deliberately excluded (see
// resolveImplementConstraints) because tests_added_or_updated and
// ci_green have no reliable signal from scope.files at plan time.
//
// Best-effort throughout: every failure path WARN-logs and returns
// without unwinding the upload, matching runPlanReviews' degradation
// contract.
//
// Returns the computed result payload so handleShipPlan can thread it
// into the plan-review prompt's gate-evidence section (#963); nil on
// every fail-open path (no result was computed). An audit-append failure
// still returns the computed result — the entry is observability, the
// evaluation itself succeeded.
func (s *Server) runScopePrecheck(ctx context.Context, runID, stageID uuid.UUID, planBody []byte) *ScopePrecheckPayload {
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		return nil
	}

	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "scope precheck: get run failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
		return nil
	}

	constraints, implStageID, ok := s.resolveImplementConstraints(ctx, runRow)
	if !ok {
		// Fail-open: no spec, unparseable spec, or no implement stage —
		// nothing to check against, so write nothing and never block.
		return nil
	}

	// Validation already passed in handleShipPlan; a parse failure here
	// is an internal inconsistency — log and skip rather than block.
	parsedPlan, err := plan.Parse(planBody)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "scope precheck: parse plan failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
		return nil
	}

	diff := policy.Diff{}
	for _, f := range parsedPlan.Scope.Files {
		diff.ChangedFiles = append(diff.ChangedFiles, policy.ChangedFile{
			Path:   f.Path,
			Status: mapFileOperation(f.Operation),
		})
	}

	violations := policy.Evaluate(diff, constraints)
	if violations == nil {
		// Marshal an empty array rather than null so the audit payload's
		// "checked and clean" state is explicit (a missing entry means
		// "never checked").
		violations = []policy.Violation{}
	}

	result := &ScopePrecheckPayload{
		WorkflowID:       runRow.WorkflowID,
		ImplementStageID: implStageID,
		Violations:       violations,
		ScannedFiles:     policy.CountedFileCount(diff),
		MaxFilesChanged:  constraints.MaxFilesChanged,
	}
	payload, _ := json.Marshal(result)
	systemKind := audit.ActorKind("system")
	if _, aerr := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  categoryPlanScopePrecheck,
		ActorKind: &systemKind,
		Payload:   payload,
	}); aerr != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "scope precheck: append audit entry failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", aerr.Error()),
		)
	}
	return result
}

// resolveImplementConstraints reads the run's workflow spec, finds the
// first implement stage in the active workflow, and flattens its
// []spec.Constraint into a single policy.Constraints. It mirrors
// resolveStageReviewers' spec-read contract: returns ok=false (fail-open)
// when the spec is absent, unparseable, the workflow is missing, or it has
// no implement stage.
//
// Only the path/scope-knowable constraints survive the flatten:
// forbidden_paths, allowed_paths, and max_files_changed. RequiredOutcomes
// is deliberately dropped — at plan time tests_added_or_updated run
// against scope.files would flag any plan that doesn't enumerate a test
// file (the implement agent routinely adds tests beyond scope), and
// ci_green has no signal before the implement stage runs. Both would
// produce noisy plan-gate advisories that muddy the clear "scope hits
// forbidden_paths — wrong workflow?" signal this pre-check exists to give.
func (s *Server) resolveImplementConstraints(ctx context.Context, runRow *run.Run) (policy.Constraints, string, bool) {
	if runRow.WorkflowSpec == nil {
		return policy.Constraints{}, "", false
	}
	parsed, err := spec.ParseBytes(runRow.WorkflowSpec)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "scope precheck: parse workflow spec failed",
			slog.String("run_id", runRow.ID.String()),
			slog.String("error", err.Error()),
		)
		return policy.Constraints{}, "", false
	}
	wf, ok := parsed.Workflows[runRow.WorkflowID]
	if !ok {
		return policy.Constraints{}, "", false
	}
	for _, st := range wf.Stages {
		if st.Type == spec.StageTypeImplement {
			return flattenPathConstraints(st.Constraints), st.ID, true
		}
	}
	return policy.Constraints{}, "", false
}

// flattenPathConstraints collapses the implement stage's []spec.Constraint
// (each carries exactly one non-zero field per the schema's
// maxProperties:1) into a single policy.Constraints, keeping only the
// constraints determinable from scope.files at plan time. RequiredOutcomes
// is intentionally not copied (see resolveImplementConstraints).
func flattenPathConstraints(cs []spec.Constraint) policy.Constraints {
	var out policy.Constraints
	for _, c := range cs {
		if len(c.ForbiddenPaths) > 0 {
			out.ForbiddenPaths = append(out.ForbiddenPaths, c.ForbiddenPaths...)
		}
		if len(c.AllowedPaths) > 0 {
			out.AllowedPaths = append(out.AllowedPaths, c.AllowedPaths...)
		}
		// Min-wins when more than one constraint sets the cap, matching
		// the post-implement gate's mergeConstraints (trace.go) so the
		// plan-time verdict equals the verdict the implement stage
		// produces for the same spec.
		if c.MaxFilesChanged > 0 {
			if out.MaxFilesChanged == 0 || c.MaxFilesChanged < out.MaxFilesChanged {
				out.MaxFilesChanged = c.MaxFilesChanged
			}
		}
		// c.RequiredOutcomes deliberately dropped — see the doc comment
		// on resolveImplementConstraints.
	}
	return out
}

// mapFileOperation maps a plan FileOperation to the policy.Status the
// post-implement gate uses. The mapping is for fidelity only: policy's
// path checks (forbidden_paths / allowed_paths / max_files_changed) match
// on ChangedFile.Path and ignore Status, so the value is not load-bearing
// for the pre-check. plan.FileOperation has only create/modify/delete —
// there is no rename — so the closed set maps to A/M/D with a defensive
// modified fallback.
func mapFileOperation(op plan.FileOperation) policy.Status {
	switch op {
	case plan.FileOpCreate:
		return policy.StatusAdded
	case plan.FileOpDelete:
		return policy.StatusDeleted
	default:
		return policy.StatusModified
	}
}
