package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fishhawk_resume_split_filing (#3925, E50.25) is the operator wrapper over
// POST /v0/runs/{run_id}/split-filing/resume (route contract:
// backend/internal/server/docs/review.md § "Detached split filing and the
// resume verb").
//
// The on-approval split-child filing runs DETACHED from the approval request
// under a phase-count-scaled budget. A filing that stopped part-way (budget
// expiry, a provider or audit error, a process exit) leaves durable per-phase
// work_item_filed markers and NO split_children_filed completion marker. This
// verb re-enters the same hook, which files only the phases with no durable
// marker, and only from the run's OWN approved plan artifact.

// resumeSplitFilingHandFiledWarning is the hand-filed warning, carried
// verbatim in the tool description, the README and the run_terminal hint.
const resumeSplitFilingHandFiledWarning = "Children filed by hand carry no marker and will be filed again."

// ResumeSplitFilingInput is the tool's input schema.
type ResumeSplitFilingInput struct {
	RunID string `json:"run_id" jsonschema:"the Fishhawk run UUID whose approved split_proposal filing should be resumed"`
}

// ResumeSplitFilingOutput is the tool response.
type ResumeSplitFilingOutput struct {
	RunID         string `json:"run_id"`
	StageID       string `json:"stage_id" jsonschema:"the run's own plan stage the filing is bound to"`
	State         string `json:"state" jsonschema:"resuming (a filing was launched for the phases with no durable marker) or completed (a split_children_filed completion marker already exists; nothing was launched)"`
	PhaseCount    int    `json:"phase_count" jsonschema:"how many phases the approved split_proposal declares"`
	AlreadyFiled  int    `json:"already_filed" jsonschema:"how many of those phases already carry a durable work_item_filed marker and will not be filed again"`
	BudgetSeconds int    `json:"budget_seconds,omitempty" jsonschema:"set only when state is resuming: the longest the launched filing can run. A missing split_children_filed is not proof of progress; wait at most this long, then probe with this tool again"`
	Message       string `json:"message" jsonschema:"what happened and how to observe completion"`
}

// registerResumeSplitFiling wires the fishhawk_resume_split_filing tool.
//
// Auth: operator write tool. The endpoint requires write:approvals (the scope
// plan approval needs); a run-bound agent token is refused 403
// run_token_forbidden and a token without the scope 403 insufficient_scope.
func registerResumeSplitFiling(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_resume_split_filing",
		Description: strings.TrimSpace(`
Use this when a run's plan stage was approved and has succeeded, its plan
carries a split_proposal, fishhawk_get_plan shows no split_filing, and the
on-approval filing has stopped. The on-approval filing runs detached from the
approval request under a budget; one that stopped part-way (budget expiry, a
provider or audit error, a fishhawkd restart) leaves per-phase work_item_filed
markers and no split_children_filed completion marker. A detached filing that
aborts writes no terminal marker, so a missing split_children_filed is NOT
proof of progress.

What it does. It re-enters the same filing hook, which is idempotent: it files
only the phases with no durable marker. It files only from the run's OWN
approved plan artifact, never a parent run's plan.
Children filed by hand carry no marker and will be filed again.

What it refuses (each is a tool error carrying the backend code verbatim):
  - run_terminal (409) — the run is failed or cancelled; start a new run for
    the remaining phases.
  - plan_changed_since_approval (409) — the plan stage's newest plan artifact
    was created after the plan gate closed; start a new run, because the
    artifact changed after the plan gate closed.
  - plan_not_approved (409, details.reason) — the run's own plan stage is not
    succeeded, has no approval gate, or carries no approve decision.
  - no_split_proposal (409, details.reason) — the run's own plan artifact is
    absent (no_plan_artifact), not valid plan JSON (plan_artifact_undecodable)
    or carries no split (no_split_proposal).
  - split_filing_in_progress (409) — a filing for this run is still running.
  - split_filing_refused (409) — the filing was refused (an over-cap phase).
  - split_filing_no_parent_issue / split_filing_repo_malformed (409).
  - run_token_forbidden / insufficient_scope (403 — needs write:approvals).

How to observe completion. On state resuming, await split_filing on
fishhawk_get_plan, or fishhawk_await_audit on split_children_filed. Wait at
most budget_seconds (the longest the filing can run). The probe is this call
itself: 409 split_filing_in_progress means the filing is still running, and a
later call files any phase that is still missing. state completed means the
completion marker already exists and nothing was launched.

Input:
  - run_id (required) — the Fishhawk run UUID.

Response: {run_id, stage_id, state, phase_count, already_filed,
budget_seconds, message}.
`),
	}, resolver.resumeSplitFiling)
}

// resumeSplitFiling is the tool handler.
func (r *runResolver) resumeSplitFiling(ctx context.Context, _ *mcp.CallToolRequest, in ResumeSplitFilingInput) (*mcp.CallToolResult, ResumeSplitFilingOutput, error) {
	runUUID, err := uuid.Parse(strings.TrimSpace(in.RunID))
	if err != nil {
		return nil, ResumeSplitFilingOutput{}, fmt.Errorf("run_id %q is not a valid UUID: %w", in.RunID, err)
	}
	res, err := r.api.ResumeSplitFiling(ctx, runUUID)
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) {
			if hint := resumeSplitFilingRefusalHint(ae.Code); hint != "" {
				return nil, ResumeSplitFilingOutput{}, fmt.Errorf("resume split filing: %w — %s", err, hint)
			}
		}
		return nil, ResumeSplitFilingOutput{}, fmt.Errorf("resume split filing: %w", err)
	}
	out := ResumeSplitFilingOutput{
		RunID:         res.RunID,
		StageID:       res.StageID,
		State:         res.State,
		PhaseCount:    res.PhaseCount,
		AlreadyFiled:  res.AlreadyFiled,
		BudgetSeconds: res.BudgetSeconds,
	}
	if out.RunID == "" {
		out.RunID = runUUID.String()
	}
	out.Message = resumeSplitFilingMessage(out)
	return nil, out, nil
}

// resumeSplitFilingMessage renders the operator-facing summary. Pure so a
// table test pins it.
func resumeSplitFilingMessage(out ResumeSplitFilingOutput) string {
	if out.State == "completed" {
		return fmt.Sprintf(
			"the split filing for run %s already completed (split_children_filed is recorded for %d phases); nothing was launched. Read the filed children with fishhawk_get_plan.",
			out.RunID, out.PhaseCount)
	}
	return fmt.Sprintf(
		"resumed the split filing for run %s: %d of %d phases already carry a durable marker and are not filed again; the remaining %d are being filed in the background. "+
			"Await split_filing on fishhawk_get_plan, or fishhawk_await_audit on split_children_filed. "+
			"A missing split_children_filed is not proof of progress: wait at most budget_seconds (%d), then call fishhawk_resume_split_filing again as the probe — 409 split_filing_in_progress means it is still running.",
		out.RunID, out.AlreadyFiled, out.PhaseCount, max(out.PhaseCount-out.AlreadyFiled, 0), out.BudgetSeconds)
}

// resumeSplitFilingRefusalHint names the remedy for each backend refusal.
// The backend code and message pass through verbatim in the wrapped error;
// this only appends the next step.
func resumeSplitFilingRefusalHint(code string) string {
	switch code {
	case "run_terminal":
		return "the run is failed or cancelled; start a new run for the remaining phases, or file them by hand. " + resumeSplitFilingHandFiledWarning
	case "plan_changed_since_approval":
		return "start a new run, because the artifact changed after the plan gate closed"
	case "plan_not_approved":
		return "approve the run's own plan stage at the plan gate first (details.reason names which rung refused)"
	case "no_split_proposal":
		return "this run's own plan stage carries no approved split_proposal; a parent run's plan is never filed from here"
	case "split_filing_in_progress":
		return "a filing for this run is still running; await split_filing on fishhawk_get_plan or a split_children_filed entry, waiting at most budget_seconds, then call this tool again"
	case "split_filing_refused":
		return "the filing was refused because a phase's own scope exceeds the implement cap; re-plan with in-cap phases in a new run"
	case "split_filing_no_parent_issue":
		return "the run has no originating issue trigger_ref, so split children have no parent to reference"
	case "split_filing_repo_malformed":
		return "the run's repo is not owner/name, so no child can be filed"
	case "run_token_forbidden":
		return "a run-bound agent token cannot resume a split filing; call this from the operator session"
	case "insufficient_scope":
		return "the token lacks write:approvals; use an operator token that carries write:approvals"
	case "split_filing_resume_failed":
		return "nothing was filed; retry the call, and if it keeps failing read the backend log for the error_ref"
	case "split_filing_resume_unconfigured":
		return "the backend has no run, audit, artifact or approval repository wired; fix the fishhawkd configuration"
	case "run_not_found":
		return "no run has that id; find it with fishhawk_list_runs"
	}
	return ""
}
