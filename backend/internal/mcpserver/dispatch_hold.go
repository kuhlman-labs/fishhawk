package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fishhawk_hold_dispatch (E110.50 / #5486) is the operator wrapper over POST
// /v0/runs/{run_id}/dispatch-hold: it sets or clears a per-run DISPATCH HOLD.
// The backend records the hold on the run's own audit chain
// (run_dispatch_hold_set / run_dispatch_hold_cleared, latest row wins) and
// enforces it at the host-dispatch marker for the run and for every run below
// it in its decomposed_from / parent_run_id ancestry (at most 8 hops), so it
// binds every spawner: the host dispatcher, a queued slot waiter re-POSTing
// the marker, run_children, drive_run, run_stage and a hand
// fishhawk_dispatch_stage. Every refusal and the audit row live server-side
// (backend/internal/server/run_dispatch_hold.go); this tool validates the
// input before any HTTP, passes the result through and names the follow-up.
//
// The hold reason is operator free text. It rides only the reason data
// fields; no next_step or message embeds it.

// The hold verb's actions and its reason bound (the backend's
// promoteReasonMax, re-checked server-side).
const (
	holdDispatchActionSet   = "set"
	holdDispatchActionClear = "clear"
	holdDispatchReasonMax   = 1000
)

// HoldDispatchInput is the fishhawk_hold_dispatch input schema.
type HoldDispatchInput struct {
	RunID  string `json:"run_id" jsonschema:"the UUID of the run to hold or release. A hold on a decomposed parent also parks every child run's later waves"`
	Action string `json:"action,omitempty" jsonschema:"set (default) parks every new host dispatch of the run and its decomposition descendants; clear releases the run's own hold"`
	Reason string `json:"reason" jsonschema:"required: why the run's dispatches are held or released. Recorded on the audit row; at most 1000 bytes after trimming"`
}

// HoldDispatchOutput is the tool response: the backend's 200 body plus the
// follow-up.
type HoldDispatchOutput struct {
	RunID                   string           `json:"run_id" jsonschema:"the run the verb acted on"`
	Action                  string           `json:"action" jsonschema:"set or clear"`
	Changed                 bool             `json:"changed" jsonschema:"true when a row was recorded; false on a no-op (set while already held, or clear while this run carries no hold of its own)"`
	DispatchHold            *RunDispatchHold `json:"dispatch_hold,omitempty" jsonschema:"the run's EFFECTIVE hold after the act (its own, or an ancestor's with inherited true); omitted when the run is no longer held"`
	DispatchHoldUnavailable bool             `json:"dispatch_hold_unavailable,omitempty" jsonschema:"true when the act was recorded but the effective hold after it could not be read; re-read it with fishhawk_get_run_status"`
	AuditSequence           int64            `json:"audit_sequence,omitempty" jsonschema:"the sequence of the recorded run_dispatch_hold_set / run_dispatch_hold_cleared row; absent on a no-op"`
	NextStep                string           `json:"next_step" jsonschema:"what happens next and what to do"`
}

// registerHoldDispatch wires the fishhawk_hold_dispatch tool (E110.50 /
// #5486), beside the queue controls.
//
// Auth: operator write tool. The endpoint requires write:runs and refuses a
// run-bound agent token; a delegated operator-agent token is admitted, since
// a hold only stops or resumes host dispatch and grants no gate act.
func registerHoldDispatch(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_hold_dispatch",
		Description: strings.TrimSpace(`
Use this when a run's NEXT dispatch must wait for the operator — typically a
decomposition wave that must not spawn until you have reviewed or merged its
predecessor, or a queued stage that should leave its concurrency queue. The
signature is a run whose next stage (or next child wave) is parked at
awaiting_host_dispatch and would be spawned by the host dispatcher on its
next tick.

What the hold binds: the backend enforces it at the host-dispatch marker, so
it binds EVERY spawner — the host dispatcher, a queued concurrency-slot waiter
re-posting the marker, fishhawk_run_children, fishhawk_drive_run,
fishhawk_run_stage and a hand fishhawk_dispatch_stage. It covers the run and
every decomposition descendant (a hold on a decomposed parent parks each
child's later waves). It never stops an in-flight stage: a running or
dispatched stage continues. The marker answers 409 run_dispatch_held; the host
dispatcher refuses run_dispatch_held before any reservation, act, marker or
spawn. This verb is the explicit operator override: there is no per-dispatch
bypass, so a held run dispatches again only after action clear.

ELIGIBILITY (each refusal is decided before anything is written):
  - an operator action: the backend requires write:runs and refuses a
    run-bound agent token (run_token_forbidden)
  - action set on a terminal run is refused (run_terminal); clear is always
    allowed
  - a set while already held, or a clear while the run carries no hold of its
    own, is a no-op (changed false) that records nothing
  - clear releases only the run's OWN hold: when the run still inherits an
    ancestor's hold, dispatch_hold names that ancestor (held_run_id); clear it
    there

Input:
  - run_id (required) — the run UUID.
  - action (optional) — set (default) or clear.
  - reason (required) — why; recorded on the audit row (at most 1000 bytes).

Response: {run_id, action, changed, dispatch_hold, dispatch_hold_unavailable,
audit_sequence, next_step}. dispatch_hold is the run's EFFECTIVE hold after
the act. After a clear that leaves no hold, the host dispatcher spawns the
parked stage on its next tick.

Tool errors (the backend refusal verbatim, plus a remedy hint):
  - invalid UUID, unknown action, or a blank or over-long reason (caught
    before the HTTP hop)
  - run_not_found (404)
  - run_token_forbidden / insufficient_scope (403)
  - run_terminal (409)
  - dispatch_hold_failed (500 — nothing recorded)
  - dispatch_hold_unavailable (503 — the backend has no run or audit store)
`),
	}, resolver.holdDispatch)
}

// validateHoldDispatchInput checks the input before any HTTP: the run UUID,
// the action (default set) and the trimmed reason (1..1000 bytes). It returns
// the parsed run id, the resolved action and the trimmed reason.
func validateHoldDispatchInput(in HoldDispatchInput) (uuid.UUID, string, string, error) {
	runUUID, err := uuid.Parse(in.RunID)
	if err != nil {
		return uuid.Nil, "", "", fmt.Errorf("run_id %q is not a valid UUID: %w", in.RunID, err)
	}
	action := strings.TrimSpace(in.Action)
	if action == "" {
		action = holdDispatchActionSet
	}
	if action != holdDispatchActionSet && action != holdDispatchActionClear {
		return uuid.Nil, "", "", fmt.Errorf("action %q is not valid: use set (park the run's new dispatches) or clear (release its own hold)", in.Action)
	}
	reason := strings.TrimSpace(in.Reason)
	if reason == "" {
		return uuid.Nil, "", "", errors.New("reason is required: say why this run's dispatches are held or released (it is recorded on the audit row)")
	}
	if len(reason) > holdDispatchReasonMax {
		return uuid.Nil, "", "", fmt.Errorf("reason is %d bytes after trimming; it must be at most %d", len(reason), holdDispatchReasonMax)
	}
	return runUUID, action, reason, nil
}

// holdDispatch is the tool handler.
func (r *runResolver) holdDispatch(ctx context.Context, _ *mcp.CallToolRequest, in HoldDispatchInput) (*mcp.CallToolResult, HoldDispatchOutput, error) {
	runUUID, action, reason, err := validateHoldDispatchInput(in)
	if err != nil {
		return nil, HoldDispatchOutput{}, err
	}
	res, err := r.api.SetRunDispatchHold(ctx, runUUID, action, reason)
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) {
			if hint := holdDispatchRefusalHint(ae.Code); hint != "" {
				return nil, HoldDispatchOutput{}, fmt.Errorf("hold dispatch: %w — %s", err, hint)
			}
		}
		return nil, HoldDispatchOutput{}, fmt.Errorf("hold dispatch: %w", err)
	}
	out := HoldDispatchOutput{
		RunID:                   res.RunID,
		Action:                  res.Action,
		Changed:                 res.Changed,
		DispatchHold:            res.DispatchHold,
		DispatchHoldUnavailable: res.DispatchHoldUnavailable,
		AuditSequence:           res.AuditSequence,
	}
	if out.RunID == "" {
		out.RunID = runUUID.String()
	}
	if out.Action == "" {
		out.Action = action
	}
	out.NextStep = holdDispatchNextStep(out)
	return nil, out, nil
}

// holdDispatchNextStep names what happens next. It is built from ids, flags
// and sequences only: the operator's reason never enters it. Pure so a table
// test pins it.
func holdDispatchNextStep(out HoldDispatchOutput) string {
	if out.DispatchHoldUnavailable {
		done := "Nothing was recorded"
		if out.Changed {
			done = fmt.Sprintf("The %s was recorded (audit sequence %d)", out.Action, out.AuditSequence)
		}
		return fmt.Sprintf("%s, but run %s's effective dispatch hold could not be read afterwards; re-read it with fishhawk_get_run_status (run.dispatch_hold) before relying on it. The dispatch marker and the host dispatcher both refuse while the hold is unreadable.",
			done, out.RunID)
	}
	hold := out.DispatchHold
	if out.Action == holdDispatchActionSet {
		if out.Changed {
			return fmt.Sprintf("Dispatch hold set on run %s (audit sequence %d). Every new host dispatch of this run and its decomposition descendants is refused with run_dispatch_held; in-flight stages continue. Release it with fishhawk_hold_dispatch action clear.",
				out.RunID, out.AuditSequence)
		}
		return fmt.Sprintf("Run %s already carries its own dispatch hold; nothing was recorded. Release it with fishhawk_hold_dispatch action clear.", out.RunID)
	}
	if hold != nil && hold.Inherited {
		prefix := fmt.Sprintf("Run %s carried no dispatch hold of its own; nothing was recorded.", out.RunID)
		if out.Changed {
			prefix = fmt.Sprintf("Run %s's own dispatch hold is cleared (audit sequence %d).", out.RunID, out.AuditSequence)
		}
		return fmt.Sprintf("%s It still inherits the hold on ancestor run %s (held_run_id), so its dispatches stay parked; clear that hold with fishhawk_hold_dispatch run_id %s action clear.",
			prefix, hold.HeldRunID, hold.HeldRunID)
	}
	if hold != nil {
		return fmt.Sprintf("Run %s is still held (held_run_id %s); re-read it with fishhawk_get_run_status before clearing again.", out.RunID, hold.HeldRunID)
	}
	if out.Changed {
		return fmt.Sprintf("Dispatch hold cleared on run %s (audit sequence %d). The host dispatcher spawns the run's parked stage on its next tick; no further action is needed.",
			out.RunID, out.AuditSequence)
	}
	return fmt.Sprintf("Run %s carried no dispatch hold; nothing was recorded. Its dispatches are not held.", out.RunID)
}

// holdDispatchRefusalHint names the move that resolves each backend refusal,
// so the tool error says what to do instead of only what failed. Empty for a
// code with no specific remedy (the error passes through verbatim). Pure so a
// table test pins it.
func holdDispatchRefusalHint(code string) string {
	switch code {
	case "run_not_found":
		return "no run has that id; find it with fishhawk_list_runs"
	case "run_terminal":
		return "a terminal run dispatches nothing, so it needs no hold; clear is still allowed"
	case "run_token_forbidden":
		return "a run-bound agent token cannot hold a run's dispatches; set or clear the hold from the operator session"
	case "insufficient_scope":
		return "the token lacks write:runs; use an operator token that carries write:runs"
	case "validation_failed":
		return "action must be set or clear and reason must be non-empty and at most 1000 bytes after trimming"
	case "dispatch_hold_failed":
		return "nothing was recorded; retry the call, and if it keeps failing read the backend log for the error_ref"
	case "dispatch_hold_unavailable":
		return "the backend has no run or audit repository wired, so no hold can be recorded; fix the fishhawkd configuration"
	}
	return ""
}
