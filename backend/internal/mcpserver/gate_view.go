package mcpserver

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
                                 reviewer_role, severity, category, state,
                                 state_reason, has_suggested_patch, fixups[],
                                 resolutions[]. reviewer_role names the persona
                                 (or 'standard') that raised it, absent for a
                                 legacy unattributed row; quote_unverified marks
                                 a quoted document passage the server could not
                                 find in the injected text (demoted to low);
                                 severity_clamped_from is the reviewer's
                                 original severity when ingest lowered it.
  - settled[]                  — waived/deferred/addressed/superseded rows with
                                 state_reason (same reviewer_role /
                                 quote_unverified / severity_clamped_from).
  - suppressed_relitigations[] — settled concerns a reviewer tried to re-raise.
  - history_incomplete + history_gaps[] — set when an audit-derived join could
    not be built (the concerns stay intact; only cross-references may be
    missing). Degradation is visible, never silent.
  - review_diff_truncated — present when an implement review ran on a TRUNCATED
    diff (the runner cut the patch at 256 KiB, or the forge capped a compare, so
    the reviewer saw only a prefix). Carries reason, changed_file_count,
    omitted_file_count, the COMPLETE omitted_files list (unlike the reviewer
    prompt, which is capped — omitted_files_residual counts what the prompt
    dropped), delta_re_review, and best_effort (true for a forge truncation whose
    file inventory is itself capped, so the omitted set may itself be
    incomplete). Omitted when no review ran on a truncated diff. A HIGH finding
    that a control is "missing" against a truncated review deserves scrutiny.
  - review_head_mismatch — present when the open implement-review round judged
    a tree the PR does NOT carry (e.g. a base-rebase re-invoke shipped a new
    tree after the review round was dispatched from the first attempt's
    bundle). Carries stage_id, reviewed_tree_sha, pushed_tree_sha,
    review_round_sequence, reviewed_head_sha and pushed_head_sha. The open
    verdicts describe a stale tree: force a fresh round (fishhawk_fixup_stage)
    before merging. Omitted when no mismatch was recorded, or once a newer
    same-stage review round superseded it.
  - precedent — present when a HUMAN gate is open on the run and the
    repository has indexed prior decisions of that gate's class (E75.4): how
    this kind of gate was decided before. Carries decision_class
    (plan_approval | concern_waive | merge_verdict | scope_amendment),
    stage_id, index_version, fingerprint, up to 3 cited items (each with its
    source_sequence + source_entry_hash citation, outcome, explained score,
    matched keys and a capped reason_excerpt), the aggregate summary (modal
    outcome, agreement ratio), truncated, degraded[], and a full_query pointer
    (fishhawk_precedent, plus alternate_decision_class — concern_defer beside
    concern_waive). DISPLAY-ONLY: never authority, never a gate input, never an
    agent input — the decision is still yours. What was surfaced is recorded
    once on the chain as precedent_surfaced. Omitted when no human gate is
    open, no precedent is indexed, or the backend's index is unwired.
  - consults[] — the crew consults this run's plan or review stages SENT
    (E77.8), each with sent_sequence, stage_id, stage_kind, sender_role,
    recipient_role, state (open | accepted | rejected | expired), answered, an
    excerpted question and answer_summary, cited_entry_refs[] (for the
    historian, the audit sequences of the prior decisions it cited) and
    asked_at. ALWAYS PRESENT — an empty array when the run asked nothing, so
    absence is never ambiguous. An EXPIRED consult is "no answer arrived in
    time", NEVER the role having refused. A historian answer is structured
    fields only and carries no reason prose from another run.
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
