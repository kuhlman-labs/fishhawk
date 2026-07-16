package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// reviveNextStepHint is the constant next-step guidance surfaced on every
// successful revive (#1915): revive re-parks the failed stages WITHOUT
// dispatching, so the operator must dispatch each re-parked stage at its
// proper gate turn via the existing verbs. Named so the tool test can assert
// the hint ships.
const reviveNextStepHint = "revive re-parked the failed stages WITHOUT dispatching — no runner was spawned. Dispatch happens at each stage's proper gate turn via the existing verbs (fishhawk_dispatch_stage / fishhawk_run_stage on the local runner). Poll fishhawk_get_run_status and follow next_actions for the re-parked stages."

// ReviveRunInput is the fishhawk_revive_run tool's input schema (#1915).
// Mirrors `POST /v0/runs/{run_id}/revive` — takes a run id; the backend
// re-parks every failed stage internally.
type ReviveRunInput struct {
	RunID string `json:"run_id" jsonschema:"the Fishhawk run UUID to revive; must be a terminal-FAILED run whose every failed stage is retryable"`
}

// ReviveRunOutput surfaces the re-opened run (now running), the per-stage
// re-park summary, and the constant no-dispatch next-step hint.
type ReviveRunOutput struct {
	Run            Run                   `json:"run" jsonschema:"the re-opened run row, now in state running"`
	RestoredStages []ReviveRestoredStage `json:"restored_stages" jsonschema:"each re-parked stage's id/type/prior failure category+reason/restored pre-dispatch state"`
	NextStep       string                `json:"next_step" jsonschema:"the no-dispatch next-step guidance: revive re-parks only, so dispatch each stage at its proper gate turn via the existing verbs"`
	// AuditWarning is a plain string (per the #371 reflection rule) so the SDK's
	// response-schema reflection emits type string. Set ONLY when the backend
	// committed the revive but failed to append the run_revived chained
	// provenance record (#1943) — the revive succeeded; investigate the audit
	// store. Omitted on a clean revive.
	AuditWarning string `json:"audit_warning,omitempty" jsonschema:"set only when the revive succeeded but the backend failed to append the run_revived chained provenance record — the run IS revived; investigate the audit store. Omitted on a clean revive"`
}

// registerReviveRun wires the fishhawk_revive_run tool (#1915): the single
// operator verb that re-admits a terminal-FAILED run, replacing the
// retry-without-dispatch dance.
//
// Auth: operator-only write tool. The backend requires write:stages or
// write:retries and rejects any run-bound agent (mcp) token outright (403
// agent_token_forbidden) — re-opening a terminal run is never an
// agent-permitted action. The MCP tool is a thin wrapper; per-category re-park
// semantics live in `run.ReviveRun`.
func registerReviveRun(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_revive_run",
		Description: strings.TrimSpace(`
Re-admit a terminal-FAILED run for another turn with ONE operator verb — the
single-step replacement for the old retry-without-dispatch dance.

Use this when a run has flipped failed (a failed stage fails the whole run) and
you want every retryable failed stage re-opened at once — especially when a
sibling stage's failure flipped the run terminal while a healthy stage's review
is still settling. The backend pre-validates that EVERY failed stage is
retryable, then re-parks each in its correct gate-ordered pre-dispatch state
(A/C → pending, D SLA-timeout → awaiting_approval, decomposed-parent implement →
awaiting_children) and flips the run failed → running.

CRUCIAL semantic difference from fishhawk_retry_stage: revive RE-PARKS ONLY — it
performs NO orchestrator handoff and never dispatches. A re-parked stage sits in
its pre-dispatch state until you dispatch it at its proper gate turn via the
existing verbs (so the #1700 wrong-order re-dispatch corruption is structurally
impossible). fishhawk_retry_stage, by contrast, re-opens ONE stage and
auto-dispatches it. Reach for revive when reviews on sibling stages are still
settling and you want a safe batch re-park; reach for retry when you want one
stage re-run immediately.

Each re-park consumes that stage's per-stage retry budget exactly like
fishhawk_retry_stage — revive is a batch retry-shaped re-open, not a budget
bypass.

Input:
  - run_id : the terminal-FAILED run to revive.

Returns the re-opened run (now running), the per-stage re-park summary
(restored_stages), and a next_step hint. On a resumed revive (see below)
restored_stages is empty and the response carries resumed:true. If audit_warning
is present the revive succeeded but the backend failed to append the run_revived
chained provenance record — the run IS revived; investigate the audit store.

Resumable partial state (#1942): the re-park batch and run reopen are NOT one
transaction, so a mid-batch failure (an infra error or a concurrent transition)
can leave the run partially re-parked and surface a 500. Simply RETRY the verb
— a second revive resumes: it re-parks the remaining failed stages, and if every
failed stage was already re-parked it completes the interrupted reopen (returning
resumed:true with an empty restored_stages, consuming no retry budget a second
time).

Tool errors:
  - invalid UUID (caught before the HTTP hop)
  - agent_token_forbidden (a run-bound agent/mcp token attempted revive, 403)
  - insufficient_scope (token lacks write:stages or write:retries, 403)
  - run_not_found (404)
  - revive_not_applicable (the run is not failed, or has no failed stage AND no
    stage in a re-parked pre-dispatch state — the interrupted-revive shape, which
    instead SUCCEEDS with an empty restored_stages — or a failed stage is
    non-retryable — category-B, D-rejected, or no recorded category; the message
    names the blocking stage. No partial mutation from the pre-validation, 422)
  - internal_error (a post-validation re-park/reopen failure left the run
    partially re-parked; retry the verb to resume, 500)
  - revive_unconfigured (run/audit repositories not wired, 503)
`),
	}, resolver.reviveRun)
}

// reviveRun is the tool handler. It validates run_id locally (a fast fail
// before the HTTP hop) and delegates the auth + pre-validation + batch re-park
// + audit append to the backend (server/revive.go → run.ReviveRun).
func (r *runResolver) reviveRun(ctx context.Context, _ *mcp.CallToolRequest, in ReviveRunInput) (*mcp.CallToolResult, ReviveRunOutput, error) {
	runID, err := uuid.Parse(in.RunID)
	if err != nil {
		return nil, ReviveRunOutput{}, fmt.Errorf("run_id %q is not a valid UUID: %w", in.RunID, err)
	}
	res, err := r.api.ReviveRun(ctx, runID)
	if err != nil {
		return nil, ReviveRunOutput{}, fmt.Errorf("revive run: %w", err)
	}
	return nil, ReviveRunOutput{
		Run:            res.Run,
		RestoredStages: res.RestoredStages,
		NextStep:       reviveNextStepHint,
		AuditWarning:   res.AuditWarning,
	}, nil
}
