package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// RevisePlanInput is the fishhawk_revise_plan tool's input schema
// (E22.X / #1099). Mirrors `fishhawk plan revise <run-id>
// --constraint …` — takes a run id; the tool resolves the plan stage
// internally.
type RevisePlanInput struct {
	RunID      string `json:"run_id" jsonschema:"the Fishhawk run UUID whose plan stage is being revised"`
	Constraint string `json:"constraint" jsonschema:"REQUIRED binding design constraint the planner must revise the prior plan to satisfy. Injected into the re-dispatched plan prompt as a binding 'Revision constraint' section (the #558 channel), with the prior plan as the revision base. Use this when the plan's direction is sound but needs a design change, rather than rejecting it to a fresh-run replan"`
	// ForceAdditionalPass is the bounded operator override.
	ForceAdditionalPass bool `json:"force_additional_pass,omitempty" jsonschema:"bounded operator override: set true to grant ONE revise pass BEYOND the normal budget when it is already spent (you got revise_budget_exhausted) but the plan still needs a design tweak. Hard-capped at 3 total passes per stage; the forced pass is audited. At the ceiling the tool returns revise_ceiling_reached and the override no longer helps. Default false."`
}

// RevisePlanOutput surfaces the re-opened Stage row plus the resolved
// plan-stage id (the caller passed a run id, not a stage id, so the
// response makes the resolution visible for audit clarity).
type RevisePlanOutput struct {
	Stage   Stage  `json:"stage"`
	StageID string `json:"stage_id" jsonschema:"the resolved plan-stage UUID the revise was posted to"`
}

// registerRevisePlan wires the fishhawk_revise_plan tool (E22.X / #1099).
//
// The plan-gate revise verdict is the third option alongside
// approve/reject. A `revise` re-plans IN PLACE in the same run: it
// re-opens the parked plan stage from awaiting_approval back to pending,
// re-dispatches the plan stage once with the operator's binding design
// constraint injected (the #558 binding-conditions channel, via a
// dedicated "Revision constraint" prompt section) and the prior plan
// carried as the revision base, then re-enters the normal review →
// approve gate. It is the plan-stage analogue of fishhawk_fixup_stage:
// a bounded, operator-gated re-open of a HEALTHY gate.
//
// Auth: write tool. Operator-side fhk_* tokens with `write:approvals`
// scope succeed; runner-side fhm_* tokens surface 403.
func registerRevisePlan(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_revise_plan",
		Description: strings.TrimSpace(`
Re-plan a run's plan IN PLACE against a binding operator design
constraint — the third plan-gate verdict alongside
fishhawk_approve_plan and fishhawk_reject_plan. Use this when the plan's
direction is sound but needs a specific design change you want the agent
to apply, rather than rejecting it (which throws the plan away and starts
a fresh run) or approving it as-is.

When to reach for revise vs the alternatives:
  - approve : the plan is correct as written.
  - revise  : the plan is on the right track but a design constraint must
              change first — cheaper than a reject → fresh-run replan,
              because the prior plan is carried as the revision base and
              only the constrained parts change.
  - reject  : the plan takes a wrong fork no constraint can amend.

Mirrors the CLI's "fishhawk plan revise <run-id> --constraint …" verb.
Takes a run id; the tool resolves the plan stage internally by listing
the run's stages and finding the one with type=plan.

What it does:
  - re-opens the plan stage from awaiting_approval back to pending;
  - re-dispatches the plan stage ONCE with your constraint injected as a
    binding "Revision constraint" section (MANDATORY, wins on conflict —
    the #558 framing) and the prior plan as the revision base;
  - the run re-enters the normal plan review → approve gate.

Bounded + operator-gated: the NORMAL bound defaults to ONE revise pass
per stage, enforced server-side by counting prior plan_revised audit
entries. A second attempt once the budget is spent returns
revise_budget_exhausted — but the operator may grant ONE more pass with
force_additional_pass=true, hard-capped at 3 TOTAL passes per stage. Once
that hard ceiling is reached the tool returns the DISTINCT
revise_ceiling_reached error (the override no longer helps — reject and
start a fresh run). A run-bound token may revise only its own run's
stages.

Inputs:
  - run_id     : the run whose plan stage to revise.
  - constraint : REQUIRED binding design constraint to revise the plan
    against.
  - force_additional_pass : bounded operator override (see above).

Returns the re-opened Stage row (pending → dispatched) and the resolved
plan-stage UUID. Returns a tool error on:
  - "no plan stage" (the run has no plan stage)
  - validation_failed (empty constraint, 400)
  - cross_run_revise (a run-bound token reaching another run's stage, 403)
  - stage_not_found (404)
  - revise_not_applicable (the stage is not a plan stage parked at
    awaiting_approval, 409)
  - revise_budget_exhausted (the NORMAL bounded pass count is spent, 409;
    one more pass is available via force_additional_pass=true)
  - revise_ceiling_reached (the hard ceiling of 3 total passes is reached,
    409; a hard stop — reject and start a fresh run)
`),
	}, resolver.revisePlan)
}

// revisePlan is the tool handler. Resolves the plan stage from the run
// id, then posts the constraint to the /v0/stages/{id}/revise endpoint.
func (r *runResolver) revisePlan(ctx context.Context, _ *mcp.CallToolRequest, in RevisePlanInput) (*mcp.CallToolResult, RevisePlanOutput, error) {
	if strings.TrimSpace(in.Constraint) == "" {
		return nil, RevisePlanOutput{}, fmt.Errorf("constraint is required: provide the binding design constraint the planner must revise the plan to satisfy")
	}
	planStage, err := r.resolvePlanStage(ctx, in.RunID)
	if err != nil {
		return nil, RevisePlanOutput{}, err
	}
	stageID, err := uuid.Parse(planStage.ID)
	if err != nil {
		return nil, RevisePlanOutput{}, fmt.Errorf("resolved plan stage has invalid id %q: %w", planStage.ID, err)
	}
	updated, err := r.api.SubmitRevise(ctx, stageID, in.Constraint, in.ForceAdditionalPass)
	if err != nil {
		return nil, RevisePlanOutput{}, fmt.Errorf("submit revise: %w", err)
	}
	return nil, RevisePlanOutput{Stage: *updated, StageID: planStage.ID}, nil
}
