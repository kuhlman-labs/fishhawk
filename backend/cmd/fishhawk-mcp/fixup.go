package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// FixupStageInput is the fishhawk_fixup_stage tool's input schema
// (E22.X / #762). Mirrors `POST /v0/stages/{stage_id}/fixup`. Concerns
// selects which recorded implement-review concerns (by their index in
// the stage's resolved concern set, as surfaced in the implement_reviewed
// audit entry) to route back to the agent; it must be non-empty. Reason
// is an optional operator note recorded on the fix-up audit entry.
type FixupStageInput struct {
	StageID  string `json:"stage_id" jsonschema:"the Fishhawk implement stage UUID to fix up (the stage parked at the implement-review gate)"`
	Concerns []int  `json:"concerns" jsonschema:"indices of the recorded implement-review concerns to route back to the agent; at least one required"`
	Reason   string `json:"reason,omitempty" jsonschema:"optional operator rationale, recorded on the stage_fixup_triggered audit entry"`
}

// FixupStageOutput surfaces the re-opened Stage row. A successful fix-up
// flips the implement stage awaiting_approval → pending (the orchestrator
// advances it to dispatched before the response returns, re-firing
// workflow_dispatch). Refusals — empty concerns, an out-of-range index,
// no recorded approve_with_concerns verdict, the bounded pass count
// already spent, or a cross-run token — surface as a tool error carrying
// the backend's error code (e.g. fixup_budget_exhausted).
type FixupStageOutput struct {
	Stage Stage `json:"stage"`
}

// registerFixupStage wires the fishhawk_fixup_stage tool (E22.X / #762).
//
// The implement-review fix-up routes one or more advisory implement-
// review concerns (ADR-027 approve_with_concerns) back to the implement
// agent for a bounded, operator-gated single pass, instead of the
// operator hand-editing the PR branch. The selected concerns are
// delivered to the agent as binding instructions (the #558 condition-
// delivery framing), the fix-up is committed onto the SAME PR branch
// (no new diff, no new PR — distinct from fishhawk_retry_stage), and the
// implement review re-runs on the result. The operator still owns the
// merge.
//
// Operator-gated + bounded: the bound defaults to one pass per stage,
// enforced server-side by counting prior stage_fixup_triggered audit
// entries; a second attempt returns a fixup_budget_exhausted tool error.
// A run-bound MCP token may fix up only stages within its own run.
//
// Auth: write tool. Operator-side fhk_* tokens with `write:stages` (or
// the dedicated `write:fixups`) scope succeed; a token without either
// surfaces 403.
func registerFixupStage(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_fixup_stage",
		Description: strings.TrimSpace(`
Route advisory implement-review concerns back to the implement agent for
a bounded, operator-gated fix-up pass.

Use this when the advisory implement review returned approve_with_concerns
and you want the agent to address one or more concerns, rather than hand-
editing the PR branch yourself. The fix-up:

  - re-invokes the implement agent with the selected concerns delivered as
    binding instructions (MANDATORY, win on conflict — the #558 framing);
  - commits onto the SAME PR branch and UPDATES the existing PR (it does
    NOT regenerate a fresh diff or open a new PR — distinct from
    fishhawk_retry_stage);
  - re-runs the implement review on the result.

Inputs:
  - stage_id : the implement stage parked at the review gate.
  - concerns : indices of the recorded implement-review concerns to route
    back (at least one). The indices address the concern set in the
    stage's implement_reviewed audit entry; inspect it via
    fishhawk_list_audit.
  - reason   : optional operator note, recorded on the audit entry.

Bounded + operator-gated: the bound defaults to ONE pass per stage. The
budget is the number of remaining passes (max − fix-ups already
triggered, observable on the stage_fixup_triggered audit entry's
remaining_budget field). A second attempt once the bound is spent returns
a fixup_budget_exhausted tool error. A run-bound token may fix up only its
own run's stages. The operator still owns the merge.

Returns the re-opened Stage row (pending → dispatched) on success.
Returns a tool error on:
  - invalid UUID (caught before the HTTP hop)
  - validation_failed (empty concerns / out-of-range index, 400)
  - cross_run_fixup (a run-bound token reaching another run's stage, 403)
  - stage_not_found (404)
  - fixup_not_applicable (no recorded approve_with_concerns verdict, 422)
  - fixup_budget_exhausted (the bounded pass count is spent, 422)
`),
	}, resolver.fixupStage)
}

// fixupStage is the tool handler. Thin wrapper over the client's
// FixupStage; per-pass bounding, concern resolution, and the subject-
// binding guard all live server-side in server/fixup.go.
func (r *runResolver) fixupStage(ctx context.Context, _ *mcp.CallToolRequest, in FixupStageInput) (*mcp.CallToolResult, FixupStageOutput, error) {
	stageID, err := uuid.Parse(in.StageID)
	if err != nil {
		return nil, FixupStageOutput{}, fmt.Errorf("stage_id %q is not a valid UUID: %w", in.StageID, err)
	}
	if len(in.Concerns) == 0 {
		return nil, FixupStageOutput{}, fmt.Errorf("concerns must select at least one recorded implement-review concern")
	}
	fixed, err := r.api.FixupStage(ctx, stageID, in.Concerns, in.Reason)
	if err != nil {
		return nil, FixupStageOutput{}, fmt.Errorf("fixup stage: %w", err)
	}
	return nil, FixupStageOutput{Stage: *fixed}, nil
}
