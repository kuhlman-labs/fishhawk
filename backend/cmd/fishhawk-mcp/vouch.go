package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// VouchCommitInput is the fishhawk_vouch_commit tool's input schema
// (ADR-035 remediation, #1044). Mirrors
// `POST /v0/runs/{run_id}/vouch-commit`. Both sha and reason are required
// — the vouch is an audited operator declaration.
type VouchCommitInput struct {
	RunID  string `json:"run_id" jsonschema:"the Fishhawk run UUID whose branch carries the commit being vouched; resolved like the other run-keyed verbs"`
	SHA    string `json:"sha" jsonschema:"the full commit SHA on the run branch to declare as run-authored lineage"`
	Reason string `json:"reason" jsonschema:"required operator rationale — why this foreign commit is legitimate run-authored lineage; recorded verbatim on the operator_commit_vouched audit entry"`
}

// VouchCommitOutput surfaces the recorded declaration.
type VouchCommitOutput struct {
	Result VouchCommitResult `json:"result"`
}

// registerVouchCommit wires the fishhawk_vouch_commit tool (ADR-035
// remediation, #1044).
//
// Auth: operator-only write tool — the backend requires write:stages and
// rejects any run-bound agent token outright (403 run_token_forbidden),
// preserving the ADR-035 sole-writer invariant.
func registerVouchCommit(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_vouch_commit",
		Description: strings.TrimSpace(`
Operator-gated ADR-035 provenance path: declare a foreign commit on a run
branch to be run-authored lineage so the branch-lineage check stops
wedging the run it fixed (#1044).

Use this when an operator's mechanical remediation commit (e.g. a
sync-schemas / formatter output) was pushed onto a run branch — typically a
decomposition fan-out branch whose children are all terminal with zero open
concerns — so no loop-native remediation (fix-up, child redrive) can route
it, and the lineage check is therefore flagging it as foreign and blocking
the merge reconciler. The vouch:

  - records an operator_commit_vouched audit entry (operator-authored,
    distinct from agent pushes);
  - unions the vouched SHA into the run's reported-head ledger on the run's
    own chain AND its decomposition children, so the commit attributes
    cleanly and the merge reconciler can resolve the run.

Distinct from fishhawk_reset_run_branch: reset DROPS an on-top foreign
commit; vouch KEEPS the operator commit and attributes it. Use vouch when
the commit is a legitimate remediation you want to retain.

Fail-closed is preserved: vouching records your declaration verbatim
without verifying the SHA, so an UN-vouched foreign commit still fails
category-B at the report boundary and still blocks merge resolution.

Inputs:
  - run_id : the run whose branch carries the commit.
  - sha    : the commit SHA to vouch.
  - reason : required operator rationale, recorded on the audit entry.

Returns the recorded declaration (run_id, vouched_sha, reason). Tool errors:
  - invalid UUID (caught before the HTTP hop)
  - validation_failed (empty sha or reason, 400)
  - run_token_forbidden (a run-bound agent token attempted the vouch, 403)
  - insufficient_scope (token lacks write:stages, 403)
  - run_not_found (404)
  - vouch_unconfigured (run/audit repositories not wired, 503)
`),
	}, resolver.vouchCommit)
}

// vouchCommit is the tool handler. It validates run_id/sha/reason locally
// (a fast fail before the HTTP hop) and delegates the auth + audit append
// + ledger union to the backend (server/vouch.go).
func (r *runResolver) vouchCommit(ctx context.Context, _ *mcp.CallToolRequest, in VouchCommitInput) (*mcp.CallToolResult, VouchCommitOutput, error) {
	runID, err := uuid.Parse(in.RunID)
	if err != nil {
		return nil, VouchCommitOutput{}, fmt.Errorf("run_id %q is not a valid UUID: %w", in.RunID, err)
	}
	if strings.TrimSpace(in.SHA) == "" {
		return nil, VouchCommitOutput{}, fmt.Errorf("sha is required: name the commit to vouch as run-authored lineage")
	}
	if strings.TrimSpace(in.Reason) == "" {
		return nil, VouchCommitOutput{}, fmt.Errorf("reason is required: the vouch is an audited operator declaration")
	}
	res, err := r.api.VouchCommit(ctx, runID, strings.TrimSpace(in.SHA), strings.TrimSpace(in.Reason))
	if err != nil {
		return nil, VouchCommitOutput{}, fmt.Errorf("vouch commit: %w", err)
	}
	return nil, VouchCommitOutput{Result: *res}, nil
}
