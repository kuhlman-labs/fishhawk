package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ResumeRunInput is the fishhawk_resume_run tool's input schema
// (E22.X / #978). parent_run_id is the failed run; the backend mints
// a NEW plan-stage-less child run executing against the parent's
// approved plan.
type ResumeRunInput struct {
	ParentRunID string `json:"parent_run_id" jsonschema:"UUID of the failed run to recover: a top-level category-B-failed run (mints a new plan-stage-less child against its approved plan) OR a failed decomposition CHILD (re-drives that child in place on the shared parent branch)"`
	// AddScopeFiles are operator-named paths folded into the recovery
	// run's effective scope as a pre-approved scope amendment.
	AddScopeFiles []RecoverScopePath `json:"add_scope_files,omitempty" jsonschema:"paths to fold into the recovery run's effective scope; each entry is {path, operation} with operation 'modify' (default) or 'create' for net-new files the #818 gate would otherwise fail"`
	// ExemptScopeFiles are operator-justified-unchanged paths — the inverse
	// of AddScopeFiles. Each marks a DECLARED scope.files path the runner's
	// #1151 shortfall gate should subtract, so a category-B run that failed
	// SOLELY on a declared-but-unchanged file recovers in ONE resume call
	// with no replan. Unlike AddScopeFiles, an exemption does NOT widen
	// scope (no amendment row) — it subtracts from the gate.
	ExemptScopeFiles []RecoverExemptPath `json:"exempt_scope_files,omitempty" jsonschema:"DECLARED scope.files paths to mark operator-justified-unchanged so the runner's shortfall gate subtracts them; each entry is {path, reason} and both are required. The inverse of add_scope_files: it subtracts from the gate, it does NOT widen scope"`
	Reason           string              `json:"reason,omitempty" jsonschema:"why the recovery (and each added path) is needed; recorded on the amendment row and the plan_reused_from audit entry, and injected into the recovery agent's binding conditions"`
	// BudgetOverride forces the recovery past a blocking periodic cost
	// budget that is over its limit for the current period.
	BudgetOverride bool   `json:"budget_override,omitempty" jsonschema:"force the recovery past a blocking periodic cost budget that is over its limit; ignored when no blocking budget is over"`
	IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"idempotency token; a second call with the same (repo, key) returns the existing recovery run with Idempotent=true instead of fresh-creating"`
}

// ResumeRunOutput mirrors StartRunOutput: the canonical child Run row
// plus the idempotent-replay flag.
type ResumeRunOutput struct {
	Run        Run  `json:"run"`
	Idempotent bool `json:"idempotent" jsonschema:"true when this call replayed against an existing recovery run via idempotency_key; false on fresh create"`
}

// registerResumeRun wires the fishhawk_resume_run tool (E22.X / #978).
//
// Auth: a write tool — operator-side fhk_* tokens with scope
// `write:runs`, same path as fishhawk_start_run.
func registerResumeRun(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_resume_run",
		Description: strings.TrimSpace(`
Recover a category-B-failed run without replanning. Use this when a
run's implement stage failed category-B (scope/constraint violation)
after its plan was approved, optionally folding operator-named
add_scope_files into the effective scope as a pre-approved scope
amendment — the recovery counterpart to fishhawk_retry_stage (which
refuses category B) and fishhawk_start_run (which replans from scratch).

Two target shapes, auto-detected from parent_run_id:

  - A top-level failed run: mints a NEW plan-stage-less child run that
    re-executes against the parent's approved plan. Eligibility: the
    parent's plan stage SUCCEEDED and its implement stage FAILED
    category-B. The child carries parent_run_id (provenance + plan
    resolution via the parent walk) and the parent's retry_attempt
    UNCHANGED (the on_ci_failure auto-retry budget is not consumed).

  - A failed DECOMPOSITION CHILD (a run minted by a parent fan-out):
    re-drives THAT child IN PLACE on the shared parent branch — the same
    run id, NOT a freshly minted run — so the parked parent fan-out can
    still consolidate. Eligibility: the child's own implement stage
    FAILED category-B and its plan resolves via the parent walk. Point
    parent_run_id at the failed child's own id (next_actions surfaces
    it); pointing it at the parent run replans from scratch instead.

Either way an ineligible target returns a recovery_not_eligible error
naming which leg failed the gate, and a plan_reused_from audit entry
records the recovery. Parents without a cached workflow spec (legacy
rows) cannot recover — start a fresh run instead. Drive the recovered
run like any local run: fishhawk_run_stage executes the implement stage
directly — no plan stage exists, no plan approval is needed.

Idempotency: pass idempotency_key to make re-calls safe after a
network hiccup; a replay returns the existing recovery run with
Idempotent=true.
`),
	}, resolver.resumeRun)
}

// resumeRun is the tool handler.
func (r *runResolver) resumeRun(ctx context.Context, _ *mcp.CallToolRequest, in ResumeRunInput) (*mcp.CallToolResult, ResumeRunOutput, error) {
	parentID, err := uuid.Parse(in.ParentRunID)
	if err != nil {
		return nil, ResumeRunOutput{}, fmt.Errorf("parent_run_id %q is not a valid UUID: %w", in.ParentRunID, err)
	}

	created, idempotent, err := r.api.RecoverRun(ctx, RecoverRunParams{
		ParentRunID:      parentID,
		AddScopeFiles:    in.AddScopeFiles,
		ExemptScopeFiles: in.ExemptScopeFiles,
		Reason:           in.Reason,
		BudgetOverride:   in.BudgetOverride,
		IdempotencyKey:   in.IdempotencyKey,
	})
	if err != nil {
		// Map the backend's gate codes onto operator-actionable tool
		// errors rather than a generic wrap.
		var ae *apiError
		if errors.As(err, &ae) {
			switch ae.Code {
			case "run_not_found":
				return nil, ResumeRunOutput{}, fmt.Errorf(
					"run_not_found: no run with id %s — pass the FAILED run's id as parent_run_id (fishhawk_list_runs to find it)", parentID)
			case "recovery_not_eligible":
				return nil, ResumeRunOutput{}, fmt.Errorf(
					"recovery_not_eligible: %s (plan_state=%v implement_state=%v failure_category=%v plan_resolved=%v). A top-level recovery requires the run's plan stage SUCCEEDED and its implement stage FAILED category-B; an in-place decomposition-child recovery requires the CHILD's own implement stage FAILED category-B and a plan resolvable via the parent walk. For category A/C/D use fishhawk_retry_stage instead",
					ae.Message, ae.Details["plan_state"], ae.Details["implement_state"], ae.Details["failure_category"], ae.Details["plan_resolved"])
			case "recovery_unsupported":
				return nil, ResumeRunOutput{}, fmt.Errorf(
					"recovery_unsupported: %s — start a fresh run with fishhawk_start_run", ae.Message)
			}
		}
		return nil, ResumeRunOutput{}, fmt.Errorf("recover run: %w", err)
	}
	return nil, ResumeRunOutput{Run: *created, Idempotent: idempotent}, nil
}
