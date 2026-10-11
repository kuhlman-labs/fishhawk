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

// fishhawk_promote_queued_stage (#4191) is the operator-reachable wrapper over
// POST /v0/stages/{stage_id}/concurrency/promote: it moves ONE queued waiter in
// a LOCAL stage concurrency group (ADR-087 stage_concurrency_slots) ahead of
// every other fresh waiter in its group, so a stage that unblocks everyone
// else is admitted next. Every refusal, the lock order and the
// stage_concurrency_promoted audit row live server-side
// (backend/internal/server/stage_concurrency_promote.go, contract:
// backend/internal/concurrency/README.md § "Promote"); this tool validates the
// stage id and the reason, passes the result through and names the follow-up
// wait.
//
// It never acts on the ADR-092/093 merge-candidate queue. That queue's
// promote is #4297's own verb (ADR-093 D7's merge_queue_promoted): the two
// queues differ in key, lock domain and refusal set, so they are two
// distinctly named verbs (the #4191 binding choice, option 2).

// PromoteQueuedStageInput is the fishhawk_promote_queued_stage input schema.
type PromoteQueuedStageInput struct {
	StageID string `json:"stage_id" jsonschema:"the UUID of a stage QUEUED for a local concurrency slot (concurrency.status awaiting_concurrency_slot on fishhawk_get_run_status, with waiter_live true)"`
	Reason  string `json:"reason" jsonschema:"required: why this waiter should be admitted ahead of the others (e.g. it fixes the defect the other queued stages fail on). Recorded on the stage_concurrency_promoted audit row; at most 1000 bytes after trimming"`
}

// PromoteQueuedStageResult mirrors the 200 body of POST
// /v0/stages/{stage_id}/concurrency/promote
// (backend/internal/server/stage_concurrency_promote.go::promoteQueuedStageResponse).
// Every json tag MUST byte-match or the field silently decodes to its zero
// value (the #371 trap).
type PromoteQueuedStageResult struct {
	StageID       string              `json:"stage_id" jsonschema:"the promoted stage"`
	RunID         string              `json:"run_id" jsonschema:"the promoted stage's run"`
	Group         string              `json:"group" jsonschema:"the concurrency group the stage waits in, e.g. local-implement:<host>"`
	Limit         int                 `json:"limit" jsonschema:"how many stages the group admits at once"`
	Promoted      bool                `json:"promoted" jsonschema:"true when the order changed; false when the stage was already first (nothing was written or recorded)"`
	OldPosition   int                 `json:"old_position" jsonschema:"the stage's 1-based queue position before the promote"`
	NewPosition   int                 `json:"new_position" jsonschema:"the stage's 1-based queue position after the promote (1)"`
	Holders       []ConcurrencyHolder `json:"holders" jsonschema:"the group's live slot holders; a promote never preempts one"`
	AuditSequence *int64              `json:"audit_sequence,omitempty" jsonschema:"the sequence of the stage_concurrency_promoted audit row; absent on a no-op"`
}

// PromoteQueuedStageOutput is the tool response.
type PromoteQueuedStageOutput struct {
	Result     PromoteQueuedStageResult `json:"result"`
	Message    string                   `json:"message" jsonschema:"what changed and what to do next"`
	NextAction *SuggestedAction         `json:"next_action,omitempty" jsonschema:"the follow-up wait: fishhawk_await_stage on the promoted stage, which its live slot waiter spawns when the next slot frees"`
}

// PromoteQueuedStage promotes one queued concurrency-group waiter via POST
// /v0/stages/{stage_id}/concurrency/promote (#4191) with body {"reason"}.
// 4xx/5xx surface as *apiError:
//   - 400 validation_failed (bad UUID, body or reason)
//   - 401 authentication_required
//   - 403 run_token_forbidden, insufficient_scope or account_forbidden
//   - 404 stage_not_found
//   - 409 concurrency_slot_held, concurrency_slot_not_queued,
//     concurrency_waiter_not_live or concurrency_slot_changed
//   - 500 concurrency_promote_failed (rolled back, nothing changed)
//   - 503 concurrency_promote_unconfigured
func (c *apiClient) PromoteQueuedStage(ctx context.Context, stageID uuid.UUID, reason string) (*PromoteQueuedStageResult, error) {
	body, err := json.Marshal(struct {
		Reason string `json:"reason"`
	}{Reason: reason})
	if err != nil {
		return nil, fmt.Errorf("marshal promote body: %w", err)
	}
	var res PromoteQueuedStageResult
	if err := c.do(ctx, http.MethodPost, "/v0/stages/"+stageID.String()+"/concurrency/promote", body, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// registerPromoteQueuedStage wires the fishhawk_promote_queued_stage tool
// (#4191).
//
// Auth: operator write tool. The endpoint requires write:runs and refuses a
// run-bound agent token; a delegated operator-agent token is admitted (an
// operational ordering control like dispatch and retry, not an approval).
func registerPromoteQueuedStage(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_promote_queued_stage",
		Description: strings.TrimSpace(`
Use this when a stage is QUEUED for a LOCAL concurrency slot and should be
admitted NEXT, ahead of the waiters queued before it — typically a fix that
unblocks the other queued stages. The signature is concurrency.status
awaiting_concurrency_slot (position > 1) on fishhawk_get_run_status or
fishhawk_await_stage, or a fishhawk_dispatch_stage that answered queued.

ELIGIBILITY (each refusal is decided before anything is written):
  - it acts ONLY on a waiter in a local stage concurrency group (ADR-087:
    local-implement:<host>, local-verify:<host>, spec:<repo>:<name>). A held
    merge-candidate pass or a merge-queue entry is NOT such a waiter: that
    queue has its own promote verb (#4297's merge-queue verb), and this tool
    never touches it
  - the stage must be queued (pending / awaiting_host_dispatch with a queued
    slot row): a HOLDING stage is refused (concurrency_slot_held) because
    holders are never preempted; a stage with no queued slot row is refused
    (concurrency_slot_not_queued)
  - its waiter must be LIVE (concurrency.waiter_live true). A stale waiter is
    refused (concurrency_waiter_not_live): it restarts at the tail on its next
    poll and would lose the promotion. Re-dispatch it with
    fishhawk_dispatch_stage first (it re-queues at the tail with a live
    waiter), then promote it
An operator action: the backend requires write:runs and refuses a run-bound
agent token (run_token_forbidden).

What it does: the stage is ordered ahead of every other fresh waiter in its
group (the newest promote wins if several are promoted). It does not change
enqueued_at or waited_seconds, never writes a holder's row, and appends one
stage_concurrency_promoted audit row (stage, run, group, limit, old and new
position, reason, actor) in the same transaction. The promotion lasts for the
waiter's current queue episode. A stage already first answers promoted:false
and records nothing. Admission still happens through the stage's own slot
waiter when the next slot frees: await it with fishhawk_await_stage.

Input:
  - stage_id (required) — the queued stage UUID.
  - reason (required) — why it should go first; recorded on the audit row.

Response: {result{stage_id, run_id, group, limit, promoted, old_position,
new_position, holders, audit_sequence}, message, next_action}.

Tool errors (the backend refusal verbatim, plus a remedy hint):
  - invalid UUID or blank reason (caught before the HTTP hop)
  - stage_not_found (404)
  - run_token_forbidden / insufficient_scope (403)
  - concurrency_slot_held / concurrency_slot_not_queued /
    concurrency_waiter_not_live / concurrency_slot_changed (409)
  - concurrency_promote_failed (500 — rolled back, the order is unchanged)
`),
	}, resolver.promoteQueuedStage)
}

// promoteQueuedStage is the tool handler.
func (r *runResolver) promoteQueuedStage(ctx context.Context, _ *mcp.CallToolRequest, in PromoteQueuedStageInput) (*mcp.CallToolResult, PromoteQueuedStageOutput, error) {
	stageUUID, err := uuid.Parse(in.StageID)
	if err != nil {
		return nil, PromoteQueuedStageOutput{}, fmt.Errorf("stage_id %q is not a valid UUID: %w", in.StageID, err)
	}
	reason := strings.TrimSpace(in.Reason)
	if reason == "" {
		return nil, PromoteQueuedStageOutput{}, errors.New("reason is required: say why this waiter should be admitted ahead of the others (it is recorded on the stage_concurrency_promoted audit row)")
	}
	res, err := r.api.PromoteQueuedStage(ctx, stageUUID, reason)
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) {
			if hint := promoteQueuedStageRefusalHint(ae.Code); hint != "" {
				return nil, PromoteQueuedStageOutput{}, fmt.Errorf("promote queued stage: %w — %s", err, hint)
			}
		}
		return nil, PromoteQueuedStageOutput{}, fmt.Errorf("promote queued stage: %w", err)
	}
	if res.StageID == "" {
		res.StageID = stageUUID.String()
	}
	out := PromoteQueuedStageOutput{Result: *res}
	if res.Promoted {
		seq := "unreported"
		if res.AuditSequence != nil {
			seq = strconv.FormatInt(*res.AuditSequence, 10)
		}
		out.Message = fmt.Sprintf("promoted stage %s in concurrency group %q from position %d to %d (stage_concurrency_promoted audit sequence %s). "+
			"It is admitted when the next of the group's %d slot(s) frees, through its own slot waiter; holders were not preempted. Await it with fishhawk_await_stage.",
			res.StageID, res.Group, res.OldPosition, res.NewPosition, seq, res.Limit)
	} else {
		out.Message = fmt.Sprintf("stage %s is already first in concurrency group %q (position %d); nothing was changed or recorded. Await it with fishhawk_await_stage.",
			res.StageID, res.Group, res.NewPosition)
	}
	if res.RunID != "" {
		out.NextAction = r.promoteNextAction(ctx, res)
	}
	return nil, out, nil
}

// promoteNextAction names fishhawk_await_stage on the promoted stage. The
// await verb resolves an explicit stage_id against the stage TYPE (default
// implement), so the type is read best-effort from the run's stages; on a
// failed read the param is omitted and the reason says so.
func (r *runResolver) promoteNextAction(ctx context.Context, res *PromoteQueuedStageResult) *SuggestedAction {
	params := map[string]string{"run_id": res.RunID, "stage_id": res.StageID}
	reason := "the promoted stage's live slot waiter spawns it when the next slot frees; await that admission"
	if runUUID, err := uuid.Parse(res.RunID); err == nil {
		if stages, err := r.api.ListRunStages(ctx, runUUID); err == nil {
			for _, s := range stages {
				if s.ID == res.StageID && s.Type != "" {
					params["stage"] = s.Type
					break
				}
			}
		}
	}
	if _, ok := params["stage"]; !ok {
		reason += "; the stage type could not be read, so pass stage=<the promoted stage's type> when it is not implement"
	}
	return &SuggestedAction{
		Action:       "fishhawk_await_stage",
		Params:       params,
		Precondition: "the stage is queued for a local concurrency slot with a live waiter",
		Consumes:     consumesNone,
		Reason:       reason,
	}
}

// promoteQueuedStageRefusalHint names the move that resolves each promote
// refusal, so the tool error tells the operator what to do instead of only
// what failed. Empty for a code with no specific remedy (the error is passed
// through verbatim). Pure so a table test pins it.
func promoteQueuedStageRefusalHint(code string) string {
	switch code {
	case "concurrency_slot_held":
		return "the stage already holds its concurrency slot and holders are never preempted; there is nothing to promote — await it with fishhawk_await_stage"
	case "concurrency_slot_not_queued":
		return "only a stage queued for a local concurrency slot (concurrency.status awaiting_concurrency_slot) can be promoted; read it with fishhawk_get_run_status. A held merge-candidate pass or merge-queue entry is not a concurrency waiter — use the merge-queue promote verb (#4297)"
	case "concurrency_waiter_not_live":
		return "re-dispatch the stage with fishhawk_dispatch_stage (it re-queues at the tail with a live waiter), then call fishhawk_promote_queued_stage again"
	case "concurrency_slot_changed":
		return "the slot row moved to another group during the promote; re-read the stage's concurrency block with fishhawk_get_run_status and retry"
	case "run_token_forbidden":
		return "a run-bound agent token cannot reorder a concurrency queue; promote from the operator session"
	case "insufficient_scope":
		return "the token lacks write:runs; promote with an operator token that carries write:runs"
	}
	return ""
}
