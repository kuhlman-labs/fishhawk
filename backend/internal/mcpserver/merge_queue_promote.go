package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fishhawk_promote_merge_queue_entry (ADR-093 D2/D7, E87.6 / #4297) is the
// operator-reachable wrapper over POST /v0/runs/{run_id}/merge-queue/promote:
// it moves ONE run's unmarked HELD merge-queue entry to the head of its base's
// line. Every refusal, the lock order and the merge_queue_promoted audit row
// live server-side (backend/internal/server/merge_queue_promote.go, contract:
// backend/internal/mergequeue/README.md § "Promote"); this tool validates the
// run id and the reason, passes the result through and names the follow-up
// read.
//
// It never acts on a stage concurrency group: that waiter's promote is
// fishhawk_promote_queued_stage (#4191). The two queues differ in key, lock
// domain and refusal set, so they are two distinctly named verbs.
//
// The I/O types are unexported on purpose (the package's exported surface is
// pinned by export_surface_test.go), as fishhawk_get_merge_queue's are.

// promoteMergeQueueEntryInput is the tool's input schema.
type promoteMergeQueueEntryInput struct {
	RunID  string `json:"run_id" jsonschema:"the UUID of a run whose merge-queue entry is HELD (state held, no yield_reason) on fishhawk_get_merge_queue"`
	Reason string `json:"reason" jsonschema:"required: why this run should be admitted to the merge line ahead of the entries queued before it. Recorded on the merge_queue_promoted audit row; at most 1000 bytes after trimming"`
}

// promoteMergeQueueEntryResult mirrors the 200 body of POST
// /v0/runs/{run_id}/merge-queue/promote
// (backend/internal/server/merge_queue_promote.go::mergeQueuePromoteResponse).
// Every json tag MUST byte-match or the field silently decodes to its zero
// value (the #371 trap).
type promoteMergeQueueEntryResult struct {
	RunID         string `json:"run_id" jsonschema:"the promoted run"`
	EntryID       string `json:"entry_id" jsonschema:"the run's held merge-queue entry"`
	Repo          string `json:"repo" jsonschema:"the entry's repository as owner/name"`
	BaseRef       string `json:"base_ref" jsonschema:"the base ref whose line the entry waits in"`
	Promoted      bool   `json:"promoted" jsonschema:"true when the order changed; false when the entry was already first among the base's held entries (nothing was written or recorded)"`
	OldPosition   int    `json:"old_position" jsonschema:"the entry's 1-based place among the base's unmarked held entries before the promote"`
	NewPosition   int    `json:"new_position" jsonschema:"the entry's 1-based place after the promote (1)"`
	LiveRunID     string `json:"live_run_id,omitempty" jsonschema:"the run holding the base's line (never preempted); absent when the line is free"`
	AuditSequence *int64 `json:"audit_sequence,omitempty" jsonschema:"the sequence of the merge_queue_promoted audit row; absent on a no-op"`
}

// promoteMergeQueueEntryOutput is the tool response.
type promoteMergeQueueEntryOutput struct {
	Result     promoteMergeQueueEntryResult `json:"result"`
	Message    string                       `json:"message" jsonschema:"what changed and what to do next"`
	NextAction *SuggestedAction             `json:"next_action,omitempty" jsonschema:"the follow-up read: fishhawk_get_merge_queue on the entry's repository and base"`
}

// PromoteMergeQueueEntry promotes one run's held merge-queue entry via POST
// /v0/runs/{run_id}/merge-queue/promote (E87.6 / #4297) with body
// {"reason"}. 4xx/5xx surface as *apiError:
//   - 400 validation_failed (bad UUID, body or reason)
//   - 401 authentication_required
//   - 403 run_token_forbidden, insufficient_scope or account_forbidden
//   - 404 run_not_found
//   - 409 merge_queue_not_queued, merge_queue_entry_live or
//     merge_queue_entry_yielded (details.next_step)
//   - 500 merge_queue_promote_failed (rolled back, order unchanged)
//   - 503 merge_queue_unconfigured
func (c *apiClient) PromoteMergeQueueEntry(ctx context.Context, runID uuid.UUID, reason string) (*promoteMergeQueueEntryResult, error) {
	body, err := json.Marshal(struct {
		Reason string `json:"reason"`
	}{Reason: reason})
	if err != nil {
		return nil, fmt.Errorf("marshal merge-queue promote body: %w", err)
	}
	var res promoteMergeQueueEntryResult
	if err := c.do(ctx, http.MethodPost, "/v0/runs/"+runID.String()+"/merge-queue/promote", body, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// registerPromoteMergeQueueEntry wires the fishhawk_promote_merge_queue_entry
// tool (E87.6 / #4297).
//
// Auth: operator write tool. The endpoint requires write:runs and refuses a
// run-bound agent token; a delegated operator-agent token is admitted (an
// ordering control, not an approval).
func registerPromoteMergeQueueEntry(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_promote_merge_queue_entry",
		Description: strings.TrimSpace(`
Use this when a run's MERGE-QUEUE entry is held behind others on its base and
should be admitted to the merge line NEXT — typically a fix the runs queued
ahead of it need. The signature is a held entry at position > 1 on
fishhawk_get_merge_queue (or fishhawk_merge_run answering status=queued).

ELIGIBILITY (each refusal is decided before anything is written):
  - it acts ONLY on an ADR-093 merge-queue entry. A stage QUEUED for a local
    concurrency slot (concurrency.status awaiting_concurrency_slot) is NOT
    such an entry: promote that waiter with fishhawk_promote_queued_stage;
    this tool never touches a concurrency group
  - the run's entry must be HELD: a LIVE entry is refused
    (merge_queue_entry_live) because the live entry is never preempted; a run
    with no held entry is refused (merge_queue_not_queued) — queue it with
    fishhawk_merge_run first
  - the entry must not have YIELDED its place (yield_reason on the queue
    read): it is refused (merge_queue_entry_yielded) naming the yield's
    next_step, because admission skips it until the yield clears
An operator action: the backend requires write:runs and refuses a run-bound
agent token (run_token_forbidden).

What it does: the entry is ordered ahead of every other unmarked held entry
on its base (the newest promote wins if several are promoted). It does not
change enqueued_at, never touches the live entry, and appends one
merge_queue_promoted audit row (entry, run, repo, base, old and new position,
reason, actor) in the same transaction, so a failed append rolls the
promotion back. The promotion lasts for the entry's episode; an ejected entry
re-enters at the tail. An entry already first answers promoted:false and
records nothing. Admission is still automatic when the line frees: read the
line with fishhawk_get_merge_queue.

Input:
  - run_id (required) — the run whose held entry to promote.
  - reason (required) — why it should go first; recorded on the audit row.

Response: {result{run_id, entry_id, repo, base_ref, promoted, old_position,
new_position, live_run_id, audit_sequence}, message, next_action}.

Tool errors (the backend refusal verbatim, plus a remedy hint):
  - invalid UUID or blank reason (caught before the HTTP hop)
  - run_not_found (404)
  - run_token_forbidden / insufficient_scope (403)
  - merge_queue_not_queued / merge_queue_entry_live /
    merge_queue_entry_yielded (409)
  - merge_queue_promote_failed (500 — rolled back, the order is unchanged)
  - merge_queue_unconfigured (503)
`),
	}, resolver.promoteMergeQueueEntry)
}

// promoteMergeQueueEntry is the tool handler.
func (r *runResolver) promoteMergeQueueEntry(ctx context.Context, _ *mcp.CallToolRequest, in promoteMergeQueueEntryInput) (*mcp.CallToolResult, promoteMergeQueueEntryOutput, error) {
	runUUID, err := uuid.Parse(strings.TrimSpace(in.RunID))
	if err != nil {
		return nil, promoteMergeQueueEntryOutput{}, fmt.Errorf("run_id %q is not a valid UUID: %w", in.RunID, err)
	}
	reason := strings.TrimSpace(in.Reason)
	if reason == "" {
		return nil, promoteMergeQueueEntryOutput{}, errors.New("reason is required: say why this run should be admitted to the merge line ahead of the others (it is recorded on the merge_queue_promoted audit row)")
	}
	res, err := r.api.PromoteMergeQueueEntry(ctx, runUUID, reason)
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) {
			if hint := promoteMergeQueueEntryRefusalHint(ae.Code); hint != "" {
				return nil, promoteMergeQueueEntryOutput{}, fmt.Errorf("promote merge-queue entry: %w — %s", err, hint)
			}
		}
		return nil, promoteMergeQueueEntryOutput{}, fmt.Errorf("promote merge-queue entry: %w", err)
	}
	if res.RunID == "" {
		res.RunID = runUUID.String()
	}
	out := promoteMergeQueueEntryOutput{Result: *res}
	if res.Promoted {
		seq := "unreported"
		if res.AuditSequence != nil {
			seq = strconv.FormatInt(*res.AuditSequence, 10)
		}
		out.Message = fmt.Sprintf("promoted run %s's merge-queue entry on %s %s from position %d to %d (merge_queue_promoted audit sequence %s). "+
			"It is admitted automatically when the line frees; the live entry was not preempted. Read the line with fishhawk_get_merge_queue.",
			res.RunID, res.Repo, res.BaseRef, res.OldPosition, res.NewPosition, seq)
	} else {
		out.Message = fmt.Sprintf("run %s's merge-queue entry is already first among the held entries on %s %s (position %d); nothing was changed or recorded. Read the line with fishhawk_get_merge_queue.",
			res.RunID, res.Repo, res.BaseRef, res.NewPosition)
	}
	if res.Repo != "" {
		params := map[string]string{"repo": res.Repo}
		if res.BaseRef != "" {
			params["base_ref"] = res.BaseRef
		}
		out.NextAction = &SuggestedAction{
			Action:       "fishhawk_get_merge_queue",
			Params:       params,
			Precondition: "the run's merge-queue entry is held on this base",
			Consumes:     consumesNone,
			Reason:       "admission is automatic when the line frees; read the queue to see the promoted entry's place and the live entry ahead of it",
		}
	}
	return nil, out, nil
}

// promoteMergeQueueEntryRefusalHint names the move that resolves each
// promote refusal, so the tool error tells the operator what to do instead of
// only what failed. Empty for a code with no specific remedy (the error is
// passed through verbatim). Pure so a table test pins it.
func promoteMergeQueueEntryRefusalHint(code string) string {
	switch code {
	case "merge_queue_entry_live":
		return "the entry already holds its base's line and the live entry is never preempted; there is nothing to promote — read it with fishhawk_get_merge_queue"
	case "merge_queue_not_queued":
		return "only a run with a held merge-queue entry can be promoted; queue it with fishhawk_merge_run. A stage queued for a local concurrency slot is not a merge-queue entry — use fishhawk_promote_queued_stage"
	case "merge_queue_entry_yielded":
		return "the entry yielded its place; take the refusal's details.next_step (e.g. approve_pr) so the yield clears, then promote again"
	case "run_token_forbidden":
		return "a run-bound agent token cannot reorder the merge queue; promote from the operator session"
	case "insufficient_scope":
		return "the token lacks write:runs; promote with an operator token that carries write:runs"
	}
	return ""
}
