package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ArbitrateAcceptanceInput is the fishhawk_arbitrate_acceptance tool's input
// schema (E66.37 / #2474). Mirrors
// `POST /v0/runs/{run_id}/acceptance-arbitration`. reason is required — the
// arbitration is an audited operator declaration overriding a FAILED verdict.
type ArbitrateAcceptanceInput struct {
	RunID  string `json:"run_id" jsonschema:"the Fishhawk run UUID whose acceptance triage is parked at a paged disposition; resolved like the other run-keyed verbs"`
	Reason string `json:"reason" jsonschema:"required operator rationale — why the change is acceptable despite the failed acceptance verdict; recorded verbatim on the acceptance_triage_arbitrated audit entry"`
	// AcknowledgeFailedCriteria carries the issue's "deliberate, separately-
	// stated decision". It is REQUIRED (the backend 409s without it) only when
	// the discharged verdict carries genuinely FAILED criteria; a class-5
	// all-skip verdict, where nothing failed, is arbitrable on the reason alone.
	AcknowledgeFailedCriteria bool `json:"acknowledge_failed_criteria,omitempty" jsonschema:"set true to state deliberately that you are merging despite genuinely FAILED acceptance criteria. Required (409 acceptance_arbitration_requires_acknowledgement otherwise) when the verdict carries criteria_failed > 0; unnecessary for an all-skip class-5 verdict"`
}

// ArbitrateAcceptanceOutput surfaces the recorded discharge.
type ArbitrateAcceptanceOutput struct {
	Result ArbitrateAcceptanceResult `json:"result"`
}

// registerArbitrateAcceptance wires the fishhawk_arbitrate_acceptance tool
// (E66.37 / #2474).
//
// Auth: operator-only write tool — the backend requires write:approvals and
// rejects any run-bound agent token outright (403 run_token_forbidden), because
// the arbitration admits the same merge fishhawk_merge_run does.
func registerArbitrateAcceptance(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_arbitrate_acceptance",
		Description: strings.TrimSpace(`
Operator-gated discharge of a PAGED acceptance triage: record an audited
arbitration so a run whose acceptance verdict FAILED but whose triage paged the
human can be merged through the ordinary loop instead of by hand (#2474).

Use this when fishhawk_merge_run refuses with 409 acceptance_gate_not_passed on
an acceptance_triage / acceptance_triage_paged run — typically a class-5
externally_unvalidatable_paged verdict (every criterion skipped because the
default-deny sandbox cannot produce the trigger), a class-3 bad/ambiguous
criterion, or a class-1/2 whose fix-up or retry route was unavailable and paged
instead. It writes an acceptance_triage_arbitrated audit entry carrying your
reason and BOUND by sequence to the acceptance_outcome_recorded verdict it
discharges, after which the acceptance gate reads acceptance_arbitrated and
fishhawk_merge_run works — so your merge verdict still lands on the chain.

What it is NOT:
  - NOT a way past an AUTO-ROUTED disposition. A class-1/2 verdict that routed
    to fixup_dispatched / retry_dispatched keeps its automatic route and is
    refused (409 acceptance_arbitration_not_applicable) — drive the re-opened
    stage instead.
  - NOT a re-run. It records a decision; it does not re-validate anything. To
    re-run acceptance, dispatch the acceptance stage.
  - NOT a pass. The gate state is acceptance_arbitrated, deliberately distinct
    from acceptance_passed; say in your merge verdict that you merged on an
    arbitrated acceptance failure.

Invalidated by construction: a later acceptance re-run records a NEW verdict at
a higher sequence that this arbitration does not name, so the gate returns to
acceptance_triage and a fresh arbitration is required.

Inputs:
  - run_id                     : the parked run.
  - reason                     : required operator rationale, recorded verbatim.
  - acknowledge_failed_criteria: required true when the verdict carries
                                 genuinely FAILED criteria.

Returns the recorded discharge (run_id, acceptance_gate_state, outcome_sequence,
arbitration_sequence, already_recorded). Tool errors:
  - invalid UUID (caught before the HTTP hop)
  - validation_failed (empty reason, 400)
  - run_token_forbidden (a run-bound agent token attempted the arbitration, 403)
  - insufficient_scope (token lacks write:approvals, 403)
  - run_not_found (404)
  - acceptance_arbitration_not_applicable (the gate is not parked at
    acceptance_triage, or the correlated triage disposition did not page, 409)
  - acceptance_arbitration_requires_acknowledgement (failed criteria without
    acknowledge_failed_criteria, 409)
  - acceptance_outcome_superseded (a newer acceptance verdict landed while the
    arbitration was being evaluated, 409)
  - acceptance_arbitration_unconfigured (run/audit repositories not wired, 503)
`),
	}, resolver.arbitrateAcceptance)
}

// arbitrateAcceptance is the tool handler. It validates run_id/reason locally (a
// fast fail before the HTTP hop) and delegates the auth ladder, every gate guard
// and the audit append to the backend (server/acceptance_arbitration.go) — the
// authoritative surface, so the tool can never admit something the gate refuses.
func (r *runResolver) arbitrateAcceptance(ctx context.Context, _ *mcp.CallToolRequest, in ArbitrateAcceptanceInput) (*mcp.CallToolResult, ArbitrateAcceptanceOutput, error) {
	runID, err := uuid.Parse(in.RunID)
	if err != nil {
		return nil, ArbitrateAcceptanceOutput{}, fmt.Errorf("run_id %q is not a valid UUID: %w", in.RunID, err)
	}
	if strings.TrimSpace(in.Reason) == "" {
		return nil, ArbitrateAcceptanceOutput{}, fmt.Errorf("reason is required: the arbitration is an audited operator declaration overriding a failed acceptance verdict")
	}
	res, err := r.api.ArbitrateAcceptance(ctx, runID, strings.TrimSpace(in.Reason), in.AcknowledgeFailedCriteria)
	if err != nil {
		return nil, ArbitrateAcceptanceOutput{}, fmt.Errorf("arbitrate acceptance: %w", err)
	}
	return nil, ArbitrateAcceptanceOutput{Result: *res}, nil
}
