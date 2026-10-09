package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// retriggerCIInput is the fishhawk_retrigger_ci tool's input schema
// (E83.49 / #4082). Mirrors `POST /v0/runs/{run_id}/retrigger-ci`.
type retriggerCIInput struct {
	RunID string `json:"run_id" jsonschema:"the Fishhawk run UUID whose pull request's failed CI should be re-run at the current head"`
}

// retriggerCIOutput surfaces the re-trigger outcome.
type retriggerCIOutput struct {
	Result retriggerCIResult `json:"result"`
}

// registerRetriggerCI wires the fishhawk_retrigger_ci tool (E83.49 / #4082).
//
// Auth: operator-only write tool — the backend requires write:stages and
// rejects any run-bound agent token outright (403 run_token_forbidden).
func registerRetriggerCI(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_retrigger_ci",
		Description: strings.TrimSpace(`
WHEN: a run's pull request has failed (or flaky) CI and you want it run
again at the CURRENT head. ELIGIBILITY: an operator token carrying
write:stages; a run-bound agent token is refused (run_token_forbidden).

NEVER close and reopen a run's PR to re-trigger CI: closing a run's PR
CANCELS the run. (A reopen within 10 minutes of the close, with the head
unchanged, revives the run to its review gate — but this verb is the safe
path and changes no PR state.)

What it does: re-runs, through the GitHub Actions re-run API, every
COMPLETED, NON-SUCCESSFUL workflow run at the PR's current head whose event
is pull_request, pull_request_target or push. It makes no pull-request
write, no ref write and no commit. workflow_dispatch runs (Fishhawk's own
runner dispatches) are never re-run; in-progress and successful runs are
reported as skipped with a reason. One ci_retriggered audit entry is
recorded when at least one re-run was accepted.

Inputs:
  - run_id : the run whose PR's CI to re-trigger.

Returns run_id, pr_url, head_sha and three lists: rerun (accepted),
skipped (each with a reason: event_excluded, not_completed, succeeded) and
failed (each with GitHub's error).

Tool errors:
  - invalid UUID (caught before the HTTP hop)
  - run_token_forbidden / insufficient_scope (403)
  - run_not_found (404)
  - run_has_no_pull_request (409)
  - pull_request_not_open (409): the PR is closed — see the warning above
  - no_ci_runs_at_head (409): no CI run exists at the head; push a commit to
    the PR branch and record it with fishhawk_vouch_commit
  - retrigger_unsupported_forge (422): not a GitHub run
  - forge_error (502): the PR or its workflow runs could not be read
  - retrigger_failed (502): every re-run request failed; nothing recorded
  - retrigger_unconfigured (503)
`),
	}, resolver.retriggerCI)
}

// retriggerCI is the tool handler. It validates run_id locally (a fast fail
// before the HTTP hop) and delegates auth, selection and the re-runs to the
// backend (server/retrigger_ci.go); a backend error code is surfaced verbatim.
func (r *runResolver) retriggerCI(ctx context.Context, _ *mcp.CallToolRequest, in retriggerCIInput) (*mcp.CallToolResult, retriggerCIOutput, error) {
	runID, err := uuid.Parse(strings.TrimSpace(in.RunID))
	if err != nil {
		return nil, retriggerCIOutput{}, fmt.Errorf("run_id %q is not a valid UUID: %w", in.RunID, err)
	}
	res, err := r.api.RetriggerCI(ctx, runID)
	if err != nil {
		return nil, retriggerCIOutput{}, fmt.Errorf("retrigger ci: %w", err)
	}
	return nil, retriggerCIOutput{Result: *res}, nil
}
