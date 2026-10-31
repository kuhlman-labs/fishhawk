package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fishhawk_withdraw_merge_intent (ADR-112 R3, E87.17 / #4685) is the
// operator-reachable wrapper over POST /v0/runs/{run_id}/merge-queue/withdraw:
// it withdraws the merge intent of ONE run's YIELDED merge-queue entry by
// settling that entry dropped intent_withdrawn. Every refusal, the lock and
// the merge_queue_intent_withdrawn audit row live server-side
// (backend/internal/server/merge_queue_withdraw.go; store contract:
// backend/internal/mergequeue/README.md § "Withdraw intent"); this tool
// validates the run id and the reason, passes the result through and names
// the follow-up inline merge.
//
// The I/O types are unexported on purpose (the package's exported surface is
// pinned by export_surface_test.go), as fishhawk_get_merge_queue's are.

// withdrawMergeIntentInput is the tool's input schema.
type withdrawMergeIntentInput struct {
	RunID  string `json:"run_id" jsonschema:"the UUID of a run whose merge-queue entry has YIELDED (state held with a yield_reason on fishhawk_get_merge_queue) and carries merge intent"`
	Reason string `json:"reason" jsonschema:"required: why this run's merge should leave the merge queue and be merged inline. Recorded on the merge_queue_intent_withdrawn audit row; at most 1000 bytes after trimming"`
}

// withdrawMergeIntentResult mirrors the 200 body of POST
// /v0/runs/{run_id}/merge-queue/withdraw
// (backend/internal/server/merge_queue_withdraw.go::mergeQueueWithdrawResponse).
// Every json tag MUST byte-match or the field silently decodes to its zero
// value (the #371 trap).
type withdrawMergeIntentResult struct {
	RunID          string `json:"run_id" jsonschema:"the run whose merge intent was withdrawn"`
	EntryID        string `json:"entry_id" jsonschema:"the run's merge-queue entry, now settled"`
	Repo           string `json:"repo" jsonschema:"the entry's repository as owner/name"`
	BaseRef        string `json:"base_ref" jsonschema:"the base ref whose line the entry waited in"`
	Withdrawn      bool   `json:"withdrawn" jsonschema:"true: the entry settled and the queue no longer owns the run's merge"`
	SettledState   string `json:"settled_state" jsonschema:"the entry's terminal state (dropped)"`
	SettleReason   string `json:"settle_reason" jsonschema:"the entry's drop reason (intent_withdrawn)"`
	IntentSequence int64  `json:"intent_sequence,omitempty" jsonschema:"the withdrawn intent's merge_queue_entered audit sequence; absent when unrecorded"`
	YieldReason    string `json:"yield_reason" jsonschema:"the yield the entry carried when withdrawn"`
	LiveRunID      string `json:"live_run_id,omitempty" jsonschema:"the run holding the base's line (untouched); absent when the line is free"`
	AuditSequence  int64  `json:"audit_sequence" jsonschema:"the sequence of the merge_queue_intent_withdrawn audit row"`
}

// withdrawMergeIntentOutput is the tool response.
type withdrawMergeIntentOutput struct {
	Result     withdrawMergeIntentResult `json:"result"`
	Message    string                    `json:"message" jsonschema:"what changed and what to do next"`
	NextAction *SuggestedAction          `json:"next_action,omitempty" jsonschema:"the follow-up: fishhawk_merge_run on the run (the inline merge)"`
}

// WithdrawMergeIntent withdraws one run's yielded merge-queue entry's merge
// intent via POST /v0/runs/{run_id}/merge-queue/withdraw (E87.17 / #4685)
// with body {"reason"}. 4xx/5xx surface as *apiError:
//   - 400 validation_failed (bad UUID, body or reason)
//   - 401 authentication_required
//   - 403 run_token_forbidden, insufficient_scope or account_forbidden
//   - 404 run_not_found
//   - 409 merge_queue_not_queued, merge_queue_entry_live,
//     merge_queue_no_intent, merge_queue_entry_not_yielded or
//     merge_dispatch_claimed (details.claim_state / next_step)
//   - 500 merge_queue_withdraw_failed (rolled back, nothing changed)
//   - 503 merge_queue_unconfigured or merge_lock_unavailable
func (c *apiClient) WithdrawMergeIntent(ctx context.Context, runID uuid.UUID, reason string) (*withdrawMergeIntentResult, error) {
	body, err := json.Marshal(struct {
		Reason string `json:"reason"`
	}{Reason: reason})
	if err != nil {
		return nil, fmt.Errorf("marshal merge-queue withdraw body: %w", err)
	}
	var res withdrawMergeIntentResult
	if err := c.do(ctx, http.MethodPost, "/v0/runs/"+runID.String()+"/merge-queue/withdraw", body, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// registerWithdrawMergeIntent wires the fishhawk_withdraw_merge_intent tool
// (E87.17 / #4685).
//
// Auth: operator write tool. The endpoint requires write:runs and refuses a
// run-bound agent token; a delegated operator-agent token is admitted (the
// withdrawal merges nothing, and the inline merge it unblocks keeps every
// gate).
func registerWithdrawMergeIntent(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_withdraw_merge_intent",
		Description: strings.TrimSpace(`
Use this when a run's MERGE-QUEUE entry has YIELDED its place (yield_reason on
fishhawk_get_merge_queue, e.g. forge_approval_absent) and the operator wants
to take the run's merge OUT of the queue and merge it inline with
fishhawk_merge_run instead (ADR-112 R3, #4685). While the yielded entry holds
merge intent, an inline merge is refused by the queue.

ELIGIBILITY (each refusal is decided before anything is written):
  - the run's entry must be HELD and YIELDED with merge intent. A LIVE entry
    is refused (merge_queue_entry_live): its pass is in flight and is never
    withdrawn mid-pass. A held entry that has NOT yielded is refused
    (merge_queue_entry_not_yielded): it is progressing to the queue's own
    merge; cancel the run to stop it. An entry without merge intent is
    refused (merge_queue_no_intent); a run with no active entry is refused
    (merge_queue_not_queued)
  - the run must hold NO merge-dispatch claim (ADR-109), dispatched or
    dispatch_failed: it is refused (merge_dispatch_claimed, with
    details.claim_state and details.next_step). The claim is never cleared
    here; a dispatch_failed claim needs reconciliation or the operator re-arm
An operator action: the backend requires write:runs and refuses a run-bound
agent token (run_token_forbidden).

What it does: under the run's merge-intent lock, the entry settles dropped
with reason intent_withdrawn and one merge_queue_intent_withdrawn audit row
(entry, run, repo, base, head, intent sequence, yield reason, reason, actor)
is appended in the same transaction, so a failed append rolls the withdrawal
back. The queue then no longer owns the run's merge. Nothing is merged: the
next step is fishhawk_merge_run, which still applies every merge gate and
re-records intent on a FRESH entry if its merge-candidate gate routes the
run back to the queue.

Input:
  - run_id (required) — the run whose yielded entry's intent to withdraw.
  - reason (required) — why; recorded on the audit row.

Response: {result{run_id, entry_id, repo, base_ref, withdrawn,
settled_state, settle_reason, intent_sequence, yield_reason, live_run_id,
audit_sequence}, message, next_action}.

Tool errors (the backend refusal verbatim, plus a remedy hint):
  - invalid UUID or blank reason (caught before the HTTP hop)
  - run_not_found (404)
  - run_token_forbidden / insufficient_scope (403)
  - merge_queue_not_queued / merge_queue_entry_live / merge_queue_no_intent /
    merge_queue_entry_not_yielded / merge_dispatch_claimed (409)
  - merge_queue_withdraw_failed (500 — rolled back, nothing changed)
  - merge_queue_unconfigured / merge_lock_unavailable (503)
`),
	}, resolver.withdrawMergeIntent)
}

// withdrawMergeIntent is the tool handler.
func (r *runResolver) withdrawMergeIntent(ctx context.Context, _ *mcp.CallToolRequest, in withdrawMergeIntentInput) (*mcp.CallToolResult, withdrawMergeIntentOutput, error) {
	runUUID, err := uuid.Parse(strings.TrimSpace(in.RunID))
	if err != nil {
		return nil, withdrawMergeIntentOutput{}, fmt.Errorf("run_id %q is not a valid UUID: %w", in.RunID, err)
	}
	reason := strings.TrimSpace(in.Reason)
	if reason == "" {
		return nil, withdrawMergeIntentOutput{}, errors.New("reason is required: say why this run's merge should leave the merge queue (it is recorded on the merge_queue_intent_withdrawn audit row)")
	}
	res, err := r.api.WithdrawMergeIntent(ctx, runUUID, reason)
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) {
			if hint := withdrawMergeIntentRefusalHint(ae.Code); hint != "" {
				return nil, withdrawMergeIntentOutput{}, fmt.Errorf("withdraw merge intent: %w — %s", err, hint)
			}
		}
		return nil, withdrawMergeIntentOutput{}, fmt.Errorf("withdraw merge intent: %w", err)
	}
	if res.RunID == "" {
		res.RunID = runUUID.String()
	}
	out := withdrawMergeIntentOutput{Result: *res}
	out.Message = fmt.Sprintf("withdrew run %s's merge intent: its merge-queue entry on %s %s (yield %s) settled %s %s (merge_queue_intent_withdrawn audit sequence %d). "+
		"The queue no longer owns the run's merge and nothing was merged; merge it inline with fishhawk_merge_run, which applies every merge gate.",
		res.RunID, res.Repo, res.BaseRef, res.YieldReason, res.SettledState, res.SettleReason, res.AuditSequence)
	out.NextAction = &SuggestedAction{
		Action:       "fishhawk_merge_run",
		Params:       map[string]string{"run_id": res.RunID},
		Precondition: "the run's PR is approved and its merge gates pass",
		Consumes:     consumesNone,
		Reason:       "the withdrawal only takes the merge out of the queue; the inline merge records your verdict and dispatches it (or re-queues the run on a FRESH entry if the merge-candidate gate routes it back)",
	}
	return nil, out, nil
}

// withdrawMergeIntentRefusalHint names the move that resolves each withdraw
// refusal, so the tool error tells the operator what to do instead of only
// what failed. Empty for a code with no specific remedy (the error is passed
// through verbatim). Pure so a table test pins it.
func withdrawMergeIntentRefusalHint(code string) string {
	switch code {
	case "merge_queue_entry_live":
		return "the entry holds its base's line and its pass is in flight; a live entry is never withdrawn mid-pass — watch it with fishhawk_get_merge_queue"
	case "merge_queue_not_queued":
		return "the run has no active merge-queue entry (never queued, settled, or already withdrawn); if it is not queued an inline fishhawk_merge_run is not blocked by the queue"
	case "merge_queue_no_intent":
		return "the entry carries no merge intent, so the queue does not own the run's merge; fishhawk_merge_run is not blocked by it"
	case "merge_queue_entry_not_yielded":
		return "only a YIELDED entry's intent can be withdrawn; this entry is progressing to the queue's own merge — let it, or cancel the run to stop it"
	case "merge_dispatch_claimed":
		return "the run already holds a merge-dispatch claim (ADR-109) and the withdrawal never clears it; take details.next_step (the operator re-arm for dispatch_failed, fishhawk_get_merge_queue for dispatched)"
	case "merge_lock_unavailable":
		return "the run's merge-intent lock was busy (a merge of this run holds it); nothing was written — re-invoke to retry"
	case "run_token_forbidden":
		return "a run-bound agent token cannot withdraw merge intent; withdraw from the operator session"
	case "insufficient_scope":
		return "the token lacks write:runs; withdraw with an operator token that carries write:runs"
	}
	return ""
}
