package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// GetGateViewInput is the fishhawk_get_gate_view tool's input schema
// (E48.13 / #1960). run_id is required; stage_kind narrows the view to one
// stage's concerns.
type GetGateViewInput struct {
	RunID     string `json:"run_id" jsonschema:"the Fishhawk run UUID"`
	StageKind string `json:"stage_kind,omitempty" jsonschema:"optional filter: 'plan' or 'implement'; scopes open+settled concerns to that stage. Omit for the whole run"`
}

// GetGateViewOutput is the gate-view payload, passed through from the backend
// verbatim. None of compact.go's levers (stripReviewProse,
// auditPayloadStringCap) are applied — the concern notes carry FULL prose.
type GetGateViewOutput struct {
	GateView *GateView `json:"gate_view"`
}

// registerGetGateView wires the fishhawk_get_gate_view tool (#1960): the
// one-call review-gate decision read. It replaces the get_run_status +
// list_audit stitching an operator otherwise runs to answer "what is still
// open at this gate and why". Read-only per ADR-021.
func registerGetGateView(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_get_gate_view",
		Description: strings.TrimSpace(`
Read a run's review-gate decision view in ONE call: every OPEN concern with
its FULL note prose, the per-concern cross-round history (fix-up routing
claims + re-review confirmations/reopens), the settled ledger, and the run's
suppressed relitigations.

Use this at a review or fix-up gate instead of stitching fishhawk_get_run_status
(which elides concern notes) with fishhawk_list_audit (which the compaction
levers further strip). This surface deliberately applies NONE of those levers,
so the concern notes arrive complete.

Inputs:
  - run_id     (required) — Fishhawk run UUID.
  - stage_kind — 'plan' or 'implement' to scope the concerns to one stage.

Response (gate_view):
  - open[]                     — open concerns, each with note (full), round,
                                 origin_review_sequence, reviewer_model,
                                 severity, category, state, state_reason,
                                 has_suggested_patch, fixups[], resolutions[].
  - settled[]                  — waived/deferred/addressed/superseded rows with
                                 state_reason.
  - suppressed_relitigations[] — settled concerns a reviewer tried to re-raise.
  - history_incomplete + history_gaps[] — set when an audit-derived join could
    not be built (the concerns stay intact; only cross-references may be
    missing). Degradation is visible, never silent.
`),
	}, resolver.getGateView)
}

// getGateView is the tool handler. It validates the run_id locally, forwards
// the optional stage_kind, and returns the backend payload verbatim.
func (r *runResolver) getGateView(ctx context.Context, _ *mcp.CallToolRequest, in GetGateViewInput) (*mcp.CallToolResult, GetGateViewOutput, error) {
	runID, err := uuid.Parse(in.RunID)
	if err != nil {
		return nil, GetGateViewOutput{}, fmt.Errorf("run_id %q is not a valid UUID: %w", in.RunID, err)
	}
	// Validate stage_kind locally so a malformed value surfaces as a clean
	// tool error rather than a generic backend 400.
	if in.StageKind != "" && in.StageKind != "plan" && in.StageKind != "implement" {
		return nil, GetGateViewOutput{}, fmt.Errorf("stage_kind %q must be 'plan' or 'implement'", in.StageKind)
	}
	gv, err := r.api.GetGateView(ctx, runID, in.StageKind)
	if err != nil {
		return nil, GetGateViewOutput{}, fmt.Errorf("get gate view: %w", err)
	}
	return nil, GetGateViewOutput{GateView: gv}, nil
}
