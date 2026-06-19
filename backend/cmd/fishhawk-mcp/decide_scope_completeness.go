package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fishhawk_decide_scope_completeness (E22.X / #1231) is the operator
// decision verb for the zero-re-run scope-completeness PARK.
//
// When an implement stage's ONLY committed-tree gate failure is the #1151
// scope-completeness "missing declared scope file(s)" check and the tree
// otherwise passed verify, the runner does NOT fail category-B. Instead it
// pushes the verified commit to the run branch (no PR) and parks the stage
// in awaiting_scope_decision, carrying the held commit SHA, run branch,
// verified tree SHA, and the missing declared paths. This verb resolves the
// park in-band:
//
//   - decision=exempt: the backend opens the PR from the EXACT held commit
//     with NO agent re-invocation (zero re-run), accepting the
//     already-committed tree. Supersedes #1229's one-re-run exempt lever
//     for the missing-declared-scope-file class specifically.
//   - decision=fail: the stage falls through to today's category-B
//     fail-and-restore path.
//
// Operator-only: the backend rejects run-bound agent tokens, so the agent
// whose stage parked can never decide its own park (mirrors
// fishhawk_decide_scope_amendment's self_decision rejection).

// DecideScopeCompletenessInput is the fishhawk_decide_scope_completeness
// tool's input schema.
type DecideScopeCompletenessInput struct {
	RunID    string `json:"run_id" jsonschema:"the Fishhawk run UUID whose implement stage is parked in awaiting_scope_decision"`
	Decision string `json:"decision" jsonschema:"exempt (accept the held commit and open the PR with no agent re-run) or fail (fall through to category-B)"`
	Reason   string `json:"reason" jsonschema:"operator rationale; required and non-empty. Recorded on the scope_completeness_exempted / scope_completeness_failed audit entry"`
}

// DecideScopeCompletenessOutput surfaces the decided park record.
type DecideScopeCompletenessOutput struct {
	Result ScopeCompletenessDecisionResult `json:"result"`
}

// registerDecideScopeCompleteness wires the fishhawk_decide_scope_completeness
// tool (E22.X / #1231).
//
// Auth: operator-only write tool — the backend requires write:stages and
// rejects any run-bound agent token outright (run_token_forbidden), so the
// agent whose stage parked can never decide its own park.
func registerDecideScopeCompleteness(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_decide_scope_completeness",
		Description: strings.TrimSpace(`
Resolve an implement stage parked in awaiting_scope_decision (#1231):
exempt the already-committed tree, or fail it to category-B.

The runner parks the stage HERE — instead of failing category-B — only
when the SOLE committed-tree gate failure is the #1151 scope-completeness
"missing declared scope file(s)" check and verify otherwise passed. It has
already pushed the gate-verified commit to the run branch (no PR opened).

On decision=exempt the backend opens the PR from the EXACT held commit
with NO agent re-invocation (zero re-run): the held tree is accepted as-is
and the implement-review gate proceeds. On decision=fail the stage falls
through to today's category-B fail-and-restore.

Operator-only: the backend rejects run-bound agent tokens
(run_token_forbidden), so the agent whose stage parked can never decide its
own park. Read the parked record first (fishhawk_get_run_status surfaces
the awaiting_scope_decision next action, or fishhawk_list_audit on category
scope_completeness_parked carries the missing paths + held SHA).

Returns the decided park record. Tool errors:
  - invalid run_id UUID (caught before the HTTP hop)
  - decision not exempt/fail (caught before the HTTP hop)
  - empty reason (caught before the HTTP hop)
  - validation_failed (400)
  - run_token_forbidden (a run-bound agent token attempted the decision, 403)
  - insufficient_scope (token lacks write:stages, 403)
  - run_not_found (404)
  - scope_completeness_not_parked (the stage is not parked in
    awaiting_scope_decision, 409)
`),
	}, resolver.decideScopeCompleteness)
}

func (r *runResolver) decideScopeCompleteness(ctx context.Context, _ *mcp.CallToolRequest, in DecideScopeCompletenessInput) (*mcp.CallToolResult, DecideScopeCompletenessOutput, error) {
	runID, err := uuid.Parse(in.RunID)
	if err != nil {
		return nil, DecideScopeCompletenessOutput{}, fmt.Errorf("run_id %q is not a valid UUID: %w", in.RunID, err)
	}
	if in.Decision != "exempt" && in.Decision != "fail" {
		return nil, DecideScopeCompletenessOutput{}, fmt.Errorf("decision must be \"exempt\" or \"fail\", got %q", in.Decision)
	}
	if strings.TrimSpace(in.Reason) == "" {
		return nil, DecideScopeCompletenessOutput{}, fmt.Errorf("reason is required and must be non-empty")
	}
	result, err := r.api.DecideScopeCompleteness(ctx, runID, in.Decision, in.Reason)
	if err != nil {
		return nil, DecideScopeCompletenessOutput{}, fmt.Errorf("decide scope completeness: %w", err)
	}
	return nil, DecideScopeCompletenessOutput{Result: *result}, nil
}
