package mcpserver

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

// WaiveConcernsInput is the fishhawk_waive_concerns (plural) tool's input
// schema (E64.77 / #3318). Mirrors
// `POST /v0/runs/{run_id}/concerns/waive`: a list of concern ids and ONE
// reason covering the whole batch.
type WaiveConcernsInput struct {
	RunID      string   `json:"run_id" jsonschema:"the run whose concerns to waive; every id must belong to THIS run"`
	ConcernIDs []string `json:"concern_ids" jsonschema:"the stable concern UUIDs to waive (from fishhawk_get_run_status's run.concerns.items[].id or fishhawk_get_gate_view). At most 50 per batch; duplicates are rejected"`
	Reason     string   `json:"reason" jsonschema:"REQUIRED operator rationale, recorded on EVERY concern_waived audit entry in the batch and stored as each concern's state_reason. One reason covers the batch — make it self-contained, since later re-reviews read it verbatim"`
	Delegated  bool     `json:"delegated,omitempty" jsonschema:"opt the batch into the ADR-040 delegated-action path; the may_waive condition is evaluated ONCE for the run before anything is appended"`
}

// WaiveConcernsOutput surfaces the per-item outcome list plus the counts.
type WaiveConcernsOutput struct {
	Result BulkWaiveResult `json:"result"`
}

// registerWaiveConcerns wires the fishhawk_waive_concerns tool (E64.77 /
// #3318): the BULK sibling of fishhawk_waive_concern, for the merge-gate case
// where a run carries a dozen open concerns the operator has already judged
// non-blocking and each one today costs its own round trip.
//
// Auth: write tool, same scope pair as the single waive; a run-bound MCP token
// may only bulk-waive its own run.
func registerWaiveConcerns(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_waive_concerns",
		Description: strings.TrimSpace(`
Waive SEVERAL of one run's open concerns in a single call, with one
required, audited reason.

Use this at a merge gate when a batch of recorded concerns does NOT
warrant a change and they share one rationale — the bulk sibling of
fishhawk_waive_concern, which you should still prefer when the concerns
need DIFFERENT reasons (the reason is what later re-reviews read, so a
reason that only fits some of the batch is worse than N calls).

Eligibility: every id must be a concern of THIS run in an OPEN state
(raised, addressed_pending, reopened). Plan-stage and implement-stage
concerns can both be waived. Waiving is terminal — there is no un-waive.

Before reaching for this on plan-stage concerns, consider
fishhawk_approve_plan's claims_all_open_plan_concerns instead: a concern
your binding approval condition already answers self-settles to
addressed_by_condition at the confirming implement review and should not
be waived at all.

The two halves have deliberately different atomicity:

  - PRE-VALIDATION is all-or-nothing and mutates NOTHING. The first
    violation refuses the WHOLE batch, naming the offending id.
  - the APPLY loop is per-item. A concurrent transition that raced the
    validation fails ONE concern; the rest still apply. Read results[]
    (request order) rather than assuming all-or-nothing here.

Returns {waived, failed, results[]}, where waived + failed == the number
of ids and each result carries concern_id, applied, and either
state/state_reason or error_code/error (error_code is the same
vocabulary the single waive uses: concern_waive_conflict,
audit_append_failed, internal_error).

Returns a tool error on:
  - invalid run_id UUID, empty concern_ids, or empty reason (caught
    before the HTTP hop)
  - validation_failed (400: over the 50-id cap, a duplicate id, a
    non-UUID id, or an id belonging to another run — details.rule
    concern_run_mismatch)
  - concern_not_found (404: an id with no row)
  - concern_waive_conflict (422: an id is not open — the WHOLE batch is
    refused and nothing is waived)
  - cross_run_waive (403: a run-bound token reaching another run)
  - concern_store_unconfigured (503)
`),
	}, resolver.waiveConcerns)
}

// waiveConcerns is the tool handler. Thin wrapper over the client's
// BulkWaiveConcerns; the pre-validation ladder, the per-item audit-before-
// mutation ordering, and the subject-binding guard all live server-side in
// server/bulk_waive.go.
func (r *runResolver) waiveConcerns(ctx context.Context, _ *mcp.CallToolRequest, in WaiveConcernsInput) (*mcp.CallToolResult, WaiveConcernsOutput, error) {
	runID, err := uuid.Parse(in.RunID)
	if err != nil {
		return nil, WaiveConcernsOutput{}, fmt.Errorf("run_id %q is not a valid UUID: %w", in.RunID, err)
	}
	if len(in.ConcernIDs) == 0 {
		return nil, WaiveConcernsOutput{}, fmt.Errorf("concern_ids must name at least one concern")
	}
	if strings.TrimSpace(in.Reason) == "" {
		return nil, WaiveConcernsOutput{}, fmt.Errorf("reason is required: the waive rationale is audited on every concern in the batch and shown to later re-reviews")
	}
	res, err := r.api.BulkWaiveConcerns(ctx, runID, in.ConcernIDs, in.Reason, in.Delegated)
	if err != nil {
		return nil, WaiveConcernsOutput{}, fmt.Errorf("bulk waive concerns: %w", err)
	}
	return nil, WaiveConcernsOutput{Result: *res}, nil
}
