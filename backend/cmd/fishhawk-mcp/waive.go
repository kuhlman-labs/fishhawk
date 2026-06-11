package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// WaiveConcernInput is the fishhawk_waive_concern tool's input schema
// (E22.X / #984). Mirrors `POST /v0/concerns/{concern_id}/waive`. Both
// fields are required: the concern's stable UUID (from
// fishhawk_get_run_status's run.concerns.items[].id) and the operator's
// rationale, which is recorded on the concern_waived audit entry and as
// the concern's state_reason.
type WaiveConcernInput struct {
	ConcernID string `json:"concern_id" jsonschema:"the stable concern UUID to waive (from fishhawk_get_run_status's run.concerns.items[].id)"`
	Reason    string `json:"reason" jsonschema:"REQUIRED operator rationale; recorded on the concern_waived audit entry, stored as the concern's state_reason, and shown verbatim to later re-reviews as the not-re-litigable waive context"`
}

// WaiveConcernOutput surfaces the updated concern row: state waived,
// state_reason carrying the operator's rationale.
type WaiveConcernOutput struct {
	Concern WaivedConcern `json:"concern"`
}

// registerWaiveConcern wires the fishhawk_waive_concern tool (E22.X /
// #984): the operator verb that resolves a review concern WITHOUT
// routing it back to the agent — the audited "this does not block"
// judgment, as distinct from fishhawk_fixup_stage (route the concern
// back for a bounded fix-up pass).
//
// Auth: write tool. Same scope pair as fix-up (write:stages or
// write:fixups); a run-bound MCP token may waive only its own run's
// concerns.
func registerWaiveConcern(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_waive_concern",
		Description: strings.TrimSpace(`
Waive one open review concern with a required, audited reason.

Use this when a recorded concern does NOT warrant a change — a false
positive, an accepted trade-off, or a deliberate deferral — instead of
routing it back to the agent with fishhawk_fixup_stage or leaving it to
clutter every later re-review. The waive:

  - transitions the concern to the terminal waived state (it stops
    appearing in fishhawk_get_run_status's run.concerns open block and
    can no longer be routed into a fix-up);
  - records your reason FIRST as a concern_waived audit entry on the
    concern's run/stage (durable before the state change), and stores it
    as the concern's state_reason;
  - shows the waived concern to later re-reviews of the stage as
    context that must NOT be re-litigated absent new evidence — your
    reason is rendered verbatim, so make it self-contained.

Applies to any concern in an OPEN state: raised, addressed_pending, or
reopened. Plan-stage and implement-stage concerns can both be waived.
The waive is terminal — there is no un-waive; if the concern turns out
to matter after all, a NEW concern from a later review is the path back.

Inputs:
  - concern_id : the stable concern UUID, from fishhawk_get_run_status's
    run.concerns.items[].id.
  - reason     : REQUIRED operator rationale (audited, shown to later
    re-reviews).

Returns the updated concern row (state waived, state_reason set).
Returns a tool error on:
  - invalid UUID or empty reason (caught before the HTTP hop)
  - concern_not_found (404)
  - cross_run_waive (a run-bound token reaching another run's concern, 403)
  - concern_waive_conflict (the concern is not open — already waived,
    superseded, or addressed; 422)
  - concern_store_unconfigured (503)
`),
	}, resolver.waiveConcern)
}

// waiveConcern is the tool handler. Thin wrapper over the client's
// WaiveConcern; the audit-before-mutation ordering, the state-machine
// check, and the subject-binding guard all live server-side in
// server/waive.go.
func (r *runResolver) waiveConcern(ctx context.Context, _ *mcp.CallToolRequest, in WaiveConcernInput) (*mcp.CallToolResult, WaiveConcernOutput, error) {
	concernID, err := uuid.Parse(in.ConcernID)
	if err != nil {
		return nil, WaiveConcernOutput{}, fmt.Errorf("concern_id %q is not a valid UUID: %w", in.ConcernID, err)
	}
	if strings.TrimSpace(in.Reason) == "" {
		return nil, WaiveConcernOutput{}, fmt.Errorf("reason is required: the waive rationale is audited and shown to later re-reviews")
	}
	waived, err := r.api.WaiveConcern(ctx, concernID, in.Reason)
	if err != nil {
		return nil, WaiveConcernOutput{}, fmt.Errorf("waive concern: %w", err)
	}
	return nil, WaiveConcernOutput{Concern: *waived}, nil
}
