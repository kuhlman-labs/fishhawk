package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// FixupStageInput is the fishhawk_fixup_stage tool's input schema
// (E22.X / #762). Mirrors `POST /v0/stages/{stage_id}/fixup`.
// ConcernIDs is the PRIMARY addressing scheme (#964): stable concern
// UUIDs surfaced by fishhawk_get_run_status's run.concerns block.
// Concerns (positional indices into the stage's flattened resolved
// concern set) is DEPRECATED — ambiguous once multiple heterogeneous
// review entries exist per stage — and only valid when ConcernIDs is
// absent; supplying both is rejected. Reason is an optional operator
// note recorded on the fix-up audit entry.
type FixupStageInput struct {
	StageID    string   `json:"stage_id" jsonschema:"the Fishhawk implement stage UUID to fix up (parked at the implement-review gate, or succeeded with the run's review gate still open)"`
	ConcernIDs []string `json:"concern_ids,omitempty" jsonschema:"PRIMARY addressing: stable concern UUIDs to route back to the agent (from fishhawk_get_run_status's run.concerns.items[].id). At least one of concern_ids/concerns required; supplying both is rejected"`
	Concerns   []int    `json:"concerns,omitempty" jsonschema:"DEPRECATED positional fallback: indices into the stage's flattened implement-review concern set. Ambiguous when multiple review entries exist per stage — prefer concern_ids. Only valid when concern_ids is absent"`
	Reason     string   `json:"reason,omitempty" jsonschema:"optional operator rationale, recorded on the stage_fixup_triggered audit entry and as the routed concerns' state_reason"`
	// AllowCreate declares net-new files this fix-up will create (#823).
	AllowCreate []string `json:"allow_create,omitempty" jsonschema:"optional repo-relative paths the fix-up will CREATE; folded into the effective scope.files for THIS pass only (bounded, explicit, operator-authorized) so the runner stages them instead of failing category-B created-out-of-scope. Any created file NOT declared here still fails category-B."`
	// ForceAdditionalPass is the bounded operator override (#860).
	ForceAdditionalPass bool `json:"force_additional_pass,omitempty" jsonschema:"bounded operator override: set true to grant ONE fix-up pass BEYOND the normal budget when it is already spent (you got fixup_budget_exhausted) but a concern still needs the agent. Hard-capped at 3 total passes per stage; the forced pass is audited (forced flag + your reason). At the ceiling the tool returns fixup_ceiling_reached and the override no longer helps. Default false."`
}

// FixupStageOutput surfaces the re-opened Stage row. A successful fix-up
// flips the implement stage awaiting_approval → pending (the orchestrator
// advances it to dispatched before the response returns, re-firing
// workflow_dispatch). Refusals — empty concerns, an out-of-range index,
// no recorded approve_with_concerns verdict, the bounded pass count
// already spent, the hard ceiling reached, or a cross-run token — surface
// as a tool error carrying the backend's error code (e.g.
// fixup_budget_exhausted or fixup_ceiling_reached).
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

Applies to an implement stage in either flow:
  - the implement stage is itself parked at the review gate
    (awaiting_approval), OR
  - the implement stage has succeeded (it opened the PR) and the run's
    SEPARATE review stage is still parked at awaiting_approval — the
    push_and_open_pr flow. The review stage is re-parked alongside the
    re-open so the fix-up flows back into a fresh review. Refused once
    the review gate has merged/resolved.

Inputs:
  - stage_id : the implement stage (parked at the review gate, or
    succeeded with the run's review gate still open).
  - concern_ids : PRIMARY addressing (#964) — stable concern UUIDs to
    route back (at least one). Read them from fishhawk_get_run_status's
    run.concerns.items[].id (open implement-stage concerns only; a
    plan-stage or already-resolved ID is rejected). Routed concerns are
    marked addressed_pending in the durable concern store.
  - concerns : DEPRECATED positional fallback — indices into the stage's
    flattened implement_reviewed concern set. Ambiguous once multiple
    heterogeneous review entries exist per stage; prefer concern_ids.
    Only valid when concern_ids is absent (supplying both is rejected).
  - reason   : optional operator note, recorded on the audit entry and
    as the routed concerns' state_reason.
  - allow_create : optional repo-relative paths the fix-up will CREATE.
    Each declared path is folded into the effective scope.files for THIS
    pass only (bounded, explicit, operator-authorized), so the runner
    stages the new file instead of failing the #818 created-out-of-scope
    gate. Use this when a concern requires a NET-NEW file. Any created
    file NOT declared here still fails category-B (the #818 fail-loud
    contract is preserved). Entries must be repo-relative (no absolute
    paths, no '..'); a bad entry returns validation_failed.
  - force_additional_pass : bounded operator override (#860). When the
    NORMAL budget is spent (you got fixup_budget_exhausted) but a concern
    still needs the agent, set this true to grant ONE pass beyond the
    budget. Hard-capped at 3 total passes per stage; the forced pass is
    audited (a 'forced' flag plus your reason on the stage_fixup_triggered
    entry). Default false.

Bounded + operator-gated: the NORMAL bound defaults to ONE pass per stage.
The budget is the number of remaining passes (max − fix-ups already
triggered, observable on the stage_fixup_triggered audit entry's
remaining_budget field). A second attempt once the budget is spent returns
fixup_budget_exhausted — but the operator may grant ONE more pass with
force_additional_pass=true, hard-capped at 3 TOTAL passes per stage. Once
that hard ceiling is reached the tool returns the DISTINCT
fixup_ceiling_reached error (the override no longer helps — file a
follow-up and merge, or start a fresh run). A run-bound token may fix up
only its own run's stages. The operator still owns the merge.

No-change refund (#967): a pass whose re-dispatch produced NO commit
(fishhawk_run_stage returned fixup_no_changes:true; a fixup_no_changes
audit entry exists for the stage) is REFUNDED against the normal budget —
the next trigger is admitted without force_additional_pass. The refund
never extends the absolute 3-pass ceiling, which counts every triggered
pass including refunded ones.

Returns the re-opened Stage row (pending → dispatched) on success.
Returns a tool error on:
  - invalid UUID (caught before the HTTP hop)
  - validation_failed (no concern selection / both concern_ids and
    indices supplied / out-of-range index / unknown, foreign, plan-stage,
    or non-open concern_id, 400)
  - cross_run_fixup (a run-bound token reaching another run's stage, 403)
  - stage_not_found (404)
  - fixup_not_applicable (no recorded approve_with_concerns verdict, or
    the stage is not at the gate / its review gate already resolved, 422)
  - fixup_budget_exhausted (the NORMAL bounded pass count is spent, 422;
    one more pass is available via force_additional_pass=true)
  - fixup_ceiling_reached (the hard ceiling of 3 total passes is reached,
    422; a hard stop — the override cannot push past it. File a follow-up
    and merge, or start a fresh run)
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
	if len(in.ConcernIDs) > 0 && len(in.Concerns) > 0 {
		return nil, FixupStageOutput{}, fmt.Errorf("supply concern_ids (stable concern UUIDs — the primary scheme) OR the deprecated positional concerns indices, not both")
	}
	if len(in.ConcernIDs) == 0 && len(in.Concerns) == 0 {
		return nil, FixupStageOutput{}, fmt.Errorf("concern_ids must select at least one recorded implement-review concern (stable UUIDs from fishhawk_get_run_status's run.concerns block; the positional concerns field is a deprecated fallback)")
	}
	fixed, err := r.api.FixupStage(ctx, stageID, in.ConcernIDs, in.Concerns, in.Reason, in.AllowCreate, in.ForceAdditionalPass)
	if err != nil {
		return nil, FixupStageOutput{}, fmt.Errorf("fixup stage: %w", err)
	}
	return nil, FixupStageOutput{Stage: *fixed}, nil
}
