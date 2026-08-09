package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// AwaitStageInput is the fishhawk_await_stage tool's input schema (#2491). It
// gives fishhawk_dispatch_stage something to await: dispatch returns a durable
// (run_id, stage_id) handle and a log_path but nothing to block on, so an
// operator had to hand-roll a poll loop. This verb blocks — one call a client
// backgrounds exactly as it backgrounds fishhawk_run_stage — until the stage
// SETTLES (terminal OR parked-for-operator), reusing the #1252 ?wait long-poll.
type AwaitStageInput struct {
	RunID string `json:"run_id" jsonschema:"the Fishhawk run UUID"`
	// Stage defaults to implement — the stage type an operator dispatches and
	// then wants to await. resolveStage matches it against the run's stages, so
	// an unknown/ambiguous type surfaces the same byte-identical error text
	// fishhawk_run_stage / fishhawk_dispatch_stage produce.
	Stage string `json:"stage,omitempty" jsonschema:"stage type to await: plan | implement | review | acceptance (default implement, the stage an operator dispatches)"`
	// StageID is the optional explicit UUID; omit it to auto-resolve from
	// (run_id, stage type) exactly as the dispatch verbs do.
	StageID        string `json:"stage_id,omitempty" jsonschema:"stage UUID inside the run; optional — auto-resolved from (run_id, stage type) when omitted, same as fishhawk_dispatch_stage"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"how long to wait before returning 'timeout' (default 360). Cap is 600 by default, raised to 7200 when long_wait=true OR your MCP client supplied a progressToken. On timeout, re-call to resume — the wait holds no server state"`
	// LongWait makes the raised 7200s timeout cap REACHABLE from a tool call
	// (#2490): the progressToken path is client-supplied MCP request metadata an
	// agent cannot set, so this boolean is the caller-settable knob that unlocks
	// the same cap.
	LongWait bool `json:"long_wait,omitempty" jsonschema:"unlock the 7200s timeout cap WITHOUT a progressToken (default false = 600s cap). There is no keep-alive on this path, so your MCP client's own idle timeout may still cut the call short — but the wait holds no state and is resumable, so a cut-short call is a safe no-op to re-issue"`
}

// AwaitStageOutput is the fishhawk_await_stage response. Status is one of:
//
//   - "settled"      — the stage reached a SETTLED state (run.StageState.IsSettled):
//     terminal (succeeded/failed/cancelled) OR parked-for-operator
//     (awaiting_approval / awaiting_children / awaiting_input /
//     awaiting_scope_decision / awaiting_deploy_approval /
//     awaiting_host_dispatch). State carries the RAW backend state so a parked
//     awaiting_approval is never coerced to succeeded.
//   - "timeout"      — nothing settled within the window. The wait holds no
//     server state, so re-calling is a safe idempotent no-op.
//   - "run_terminal" — the run reached a terminal state while the stage was
//     still unsettled (ADR-036 backstop); a final read found it still
//     unsettled, so it most likely never will. Do not re-arm blindly.
type AwaitStageOutput struct {
	Status string `json:"status" jsonschema:"one of settled, timeout, run_terminal"`
	Stage  string `json:"stage" jsonschema:"the resolved stage type"`
	// StageID is the resolved stage UUID — the durable ADR-037 handle, echoed so
	// a resuming caller can re-issue against the same stage.
	StageID string `json:"stage_id" jsonschema:"the resolved stage UUID the wait was armed against (the durable ADR-037 handle, with run_id)"`
	// State is the RAW backend stage state, never coerced: a parked
	// awaiting_approval settles the wait but is reported as awaiting_approval,
	// NOT succeeded, so the operator can tell a completed stage from one parked
	// for their attention.
	State string `json:"state" jsonschema:"the RAW backend stage state (succeeded/failed/cancelled or a parked awaiting_* state); never coerced, so a parked settled stage is distinguishable from a succeeded one"`
	// Terminal is the endpoint's settledness flag (#1252): true when the stage
	// IsSettled — terminal OR parked. It is the authority for the settled
	// resolve; keying on terminality alone (succeeded/failed/cancelled) would
	// silently miss the parked half.
	Terminal            bool             `json:"terminal" jsonschema:"true when the stage has SETTLED — a terminal state OR a parked-for-operator state; the authoritative resolve signal"`
	FailureCategory     string           `json:"failure_category,omitempty" jsonschema:"the failed stage's category, when the settled state is failed"`
	FailureReason       string           `json:"failure_reason,omitempty" jsonschema:"the failed stage's reason, when the settled state is failed"`
	StageWaitStatus     *StageWaitStatus `json:"stage_wait_status,omitempty" jsonschema:"the classified execution wait status (same shape get_run_status / dispatch_stage carry), for continuity; the raw state + terminal fields are the authority"`
	WaitedSeconds       float64          `json:"waited_seconds" jsonschema:"elapsed wall time spent waiting"`
	Message             string           `json:"message,omitempty" jsonschema:"actionable explanation on the timeout / run_terminal statuses"`
	PollIntervalSeconds int              `json:"poll_interval_seconds,omitempty" jsonschema:"server-suggested cadence (seconds) for switching to fishhawk_get_run_status polling; present only on the timeout status"`
	// Heartbeat reports whether the CLIENT supplied a progressToken and a
	// per-tick keep-alive was therefore emitted (#2490). Present on every return
	// path; a false value is the operator's evidence their client does not supply
	// one, so the raised cap is reachable only via long_wait.
	Heartbeat bool `json:"heartbeat" jsonschema:"true when your MCP client supplied a progressToken and a per-tick keep-alive was emitted; false means it did not (use long_wait to reach the raised timeout cap)"`
	// TimeoutCapSeconds is the timeout cap actually applied: 7200 when long_wait
	// or a progressToken was in effect, else 600 (#2490). Present on every path.
	TimeoutCapSeconds int `json:"timeout_cap_seconds" jsonschema:"the timeout cap actually applied to this call (600 by default, 7200 when long_wait or a progressToken was in effect)"`
}

// awaitStageDefaultStageType is the stage type awaited when the caller omits
// one — the stage an operator dispatches with fishhawk_dispatch_stage and then
// wants to block on.
const awaitStageDefaultStageType = "implement"

// registerAwaitStage wires the fishhawk_await_stage tool (#2491): the terminal
// wait for the durable handle fishhawk_dispatch_stage hands back. Read-only per
// ADR-021 — it only polls the #1252 stage-wait endpoint, server-side, on an
// injectable interval. Registered between registerAwaitAudit and
// registerAwaitReview so source order (the stable ListTools order) keeps the
// three await verbs adjacent.
func registerAwaitStage(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_await_stage",
		Description: strings.TrimSpace(`
Block until a stage SETTLES and return its state. This is the terminal wait
for the durable (run_id, stage_id) handle fishhawk_dispatch_stage hands back
(#2491): dispatch returns a log_path and nothing to await, so without this an
operator had to hand-roll a poll loop. Reuses the #1252 ?wait long-poll — a
single call your MCP client backgrounds exactly as it backgrounds
fishhawk_run_stage, with no polling.

"Settled" is deliberately BROADER than "terminal": the wait resolves the
moment the stage reaches a terminal state (succeeded / failed / cancelled) OR
a parked-for-operator state (awaiting_approval / awaiting_children /
awaiting_input / awaiting_scope_decision / awaiting_deploy_approval /
awaiting_host_dispatch) — the settledness a detached watcher actually wants
(release the moment the stage needs attention). The RAW backend state is
carried out on the state field, never coerced, so a parked awaiting_approval
is distinguishable from a succeeded stage. (A freshly dispatched local stage
can legitimately be awaiting_host_dispatch, which IS settled — so a wait
against a never-dispatched stage returns immediately, and the raw state says
why.)

Statuses:
  - "settled"      — the stage settled; state carries the raw state and
                     terminal is true.
  - "timeout"      — nothing settled within the window. The wait holds no
                     server state, so a cut-short call is a safe no-op to
                     re-issue; poll_interval_seconds names the fallback
                     get_run_status cadence.
  - "run_terminal" — the run reached succeeded/failed/cancelled while the
                     stage was still unsettled (the ADR-036 non-stranding
                     backstop); a final read found it still unsettled, so it
                     most likely never will. Do not re-arm blindly — check
                     fishhawk_get_run_status first.

Inputs:
  - run_id          (required) — Fishhawk run UUID.
  - stage           — stage type to await (default implement, the stage an
                      operator dispatches). Auto-resolved to a stage_id from
                      (run_id, stage type) unless stage_id is given.
  - stage_id        — optional explicit stage UUID.
  - long_wait       — unlock the 7200s cap from a tool call (default false).
  - timeout_seconds — default 360; cap 600, or 7200 when long_wait=true OR a
                      progressToken is present.

Raising the timeout cap (#2490): the default cap is 600s; it rises to 7200s
(2h) so one call can wait out a full implement pass. There are two ways to
reach it, and only one is settable from a tool call:

  - long_wait:true — the REACHABLE knob. Set it plus timeout_seconds up to 7200.
    No keep-alive on this path, so your MCP client's own idle timeout may still
    cut the call short — a safe no-op to re-issue, since the wait holds no state.
  - a progressToken — MCP request metadata supplied by your client, not a tool
    input; you cannot set it from a tool call. When present it also drives an MCP
    notifications/progress keep-alive once per poll tick that resets the client's
    idle clock. The response's heartbeat field reports whether yours did.

Re-polling fishhawk_get_run_status remains the authoritative fallback path
(ADR-037); this tool blocks that poll for you when you would rather wait
synchronously than loop yourself.
`),
	}, resolver.awaitStage)
}

// awaitStage is the tool handler. Mirrors awaitAudit's structure: a fast-path
// read, then a poll on the injectable reviewPollInterval under a clamped
// deadline, with the per-call ?wait long-poll driving settlement detection and
// the ADR-036 run-terminal backstop checked once before the loop and on each
// still-unsettled tick.
func (r *runResolver) awaitStage(ctx context.Context, req *mcp.CallToolRequest, in AwaitStageInput) (*mcp.CallToolResult, AwaitStageOutput, error) {
	runID, err := uuid.Parse(in.RunID)
	if err != nil {
		return nil, AwaitStageOutput{}, fmt.Errorf("run_id %q is not a valid UUID: %w", in.RunID, err)
	}
	stageType := strings.TrimSpace(in.Stage)
	if stageType == "" {
		stageType = awaitStageDefaultStageType
	}

	// Resolve the stage via the SHARED resolver so the zero-match / ambiguous /
	// explicit-disagrees error text is byte-identical to run_stage and
	// dispatch_stage (no second resolver).
	stage, err := r.resolveStage(ctx, runID, stageType, strings.TrimSpace(in.StageID))
	if err != nil {
		return nil, AwaitStageOutput{}, err
	}
	stageUUID, err := uuid.Parse(stage.ID)
	if err != nil {
		return nil, AwaitStageOutput{}, fmt.Errorf("resolved stage_id %q is not a valid UUID: %w", stage.ID, err)
	}

	// Progress heartbeat (#2490): capture the client-supplied progressToken
	// exactly as awaitAudit does (nil-guarding req and req.Params). A token
	// unlocks the 7200s cap AND a per-tick keep-alive; long_wait unlocks the same
	// cap from a tool call WITHOUT emitting; neither changes the byte-identical
	// 360/600 contract. Opt-in per the MCP spec: no token (or no session) => no
	// emission — long_wait must NOT cause an emission.
	var progToken any
	if req != nil && req.Params != nil {
		progToken = req.Params.GetProgressToken()
	}
	heartbeat := progToken != nil
	capSeconds := effectiveAwaitCap(heartbeat, in.LongWait)
	timeout := clampAwaitTimeoutHeartbeat(in.TimeoutSeconds, heartbeat, in.LongWait)
	start := time.Now()

	// Fast path: the stage may already be settled. A wait=0 read returns
	// immediately with the envelope's terminal (IsSettled) flag.
	sw, err := r.api.GetRunStageWait(ctx, runID, stageUUID, 0)
	if err != nil {
		return nil, AwaitStageOutput{}, fmt.Errorf("read stage wait: %w", err)
	}
	if sw.Terminal {
		return nil, awaitStageSettledOutput(stageType, sw, start, heartbeat, capSeconds), nil
	}

	// Nothing settled yet: check the run-terminal backstop once before the loop
	// so a run already terminal at call time resolves without a poll tick.
	if out, done := r.awaitStageRunTerminalBackstop(ctx, runID, stageUUID, stageType, start, heartbeat, capSeconds); done {
		return nil, out, nil
	}

	interval := r.reviewPollInterval
	if interval <= 0 {
		interval = defaultReviewPollInterval
	}
	pollCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var progress float64
	for {
		select {
		case <-pollCtx.Done():
			return nil, awaitStageTimeoutOutput(stageType, stageUUID, timeout, start, heartbeat, capSeconds), nil
		case <-ticker.C:
			// Best-effort progress heartbeat once per tick (opt-in): keeps a long
			// wait from being aborted by the client's idle timeout. Emitted only
			// when the caller supplied a progressToken AND the request carries a
			// live session. A failed notify is SWALLOWED — the await is
			// authoritative, the heartbeat advisory.
			if progToken != nil && req != nil && req.Session != nil {
				progress++
				_ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
					ProgressToken: progToken,
					Progress:      progress,
					Message:       awaitStageProgressMessage(stageType, time.Since(start)),
				})
			}
			// Per-call ?wait long-poll: the backend holds the connection up to
			// awaitStagePerCallWaitSeconds (clamped in GetRunStageWait) so
			// settlement is observed within ~500ms of happening, not on a poll
			// tick.
			sw, err := r.api.GetRunStageWait(pollCtx, runID, stageUUID, awaitStagePerCallWaitSeconds)
			if err != nil {
				// A deadline hit mid-poll cancels the in-flight request; that is a
				// timeout, not a transport failure — return the resumable timeout.
				if pollCtx.Err() != nil {
					return nil, awaitStageTimeoutOutput(stageType, stageUUID, timeout, start, heartbeat, capSeconds), nil
				}
				return nil, AwaitStageOutput{}, fmt.Errorf("poll stage wait: %w", err)
			}
			if sw.Terminal {
				return nil, awaitStageSettledOutput(stageType, sw, start, heartbeat, capSeconds), nil
			}
			if out, done := r.awaitStageRunTerminalBackstop(pollCtx, runID, stageUUID, stageType, start, heartbeat, capSeconds); done {
				return nil, out, nil
			}
		}
	}
}

// awaitStageRunTerminalBackstop resolves the wait when the run itself has
// reached a terminal state while the stage is still unsettled (ADR-036): the
// stage most likely never settles, so holding the session to the deadline
// would strand the caller. It performs ONE final stage read before resolving —
// a settlement landing at/after the run transition still wins as status=settled
// (final-read-wins). Returns (output, true) to resolve the wait; (zero, false)
// to keep polling. Best-effort — a GetRun error or a non-terminal run leaves
// the normal poll/timeout path in charge, never spinning or failing the wait.
func (r *runResolver) awaitStageRunTerminalBackstop(ctx context.Context, runID, stageID uuid.UUID, stageType string, start time.Time, heartbeat bool, capSeconds int) (AwaitStageOutput, bool) {
	runRow, err := r.api.GetRun(ctx, runID)
	if err != nil || runRow == nil {
		return AwaitStageOutput{}, false
	}
	if !runStateIsTerminal(runRow.State) {
		return AwaitStageOutput{}, false
	}
	// Final read: a settlement landing at/after the terminal transition still
	// resolves as settled and beats the backstop.
	sw, ferr := r.api.GetRunStageWait(ctx, runID, stageID, 0)
	if ferr == nil && sw != nil && sw.Terminal {
		return awaitStageSettledOutput(stageType, sw, start, heartbeat, capSeconds), true
	}
	return AwaitStageOutput{
		Status:            "run_terminal",
		Stage:             stageType,
		StageID:           stageID.String(),
		WaitedSeconds:     time.Since(start).Seconds(),
		Heartbeat:         heartbeat,
		TimeoutCapSeconds: capSeconds,
		Message: fmt.Sprintf("stage %q of run %s is still unsettled and the run has reached terminal state %q — "+
			"the stage most likely never settles, so the wait resolved instead of holding the session open. "+
			"Do not re-arm blindly: check fishhawk_get_run_status for the final run state first.",
			stageType, runID, runRow.State),
	}, true
}

// awaitStageSettledOutput builds the resolved response for a settled stage. It
// carries the RAW envelope state (never coerced) plus the classified
// StageWaitStatus for continuity with get_run_status / dispatch_stage.
func awaitStageSettledOutput(stageType string, sw *RunStageWait, start time.Time, heartbeat bool, capSeconds int) AwaitStageOutput {
	out := AwaitStageOutput{
		Status:            "settled",
		Stage:             stageType,
		StageID:           sw.ID,
		State:             sw.State,
		Terminal:          sw.Terminal,
		StageWaitStatus:   classifyStageWaitStatus(stageType, sw.State, "", sw.StartedAt, 0, time.Now().UTC()),
		WaitedSeconds:     time.Since(start).Seconds(),
		Heartbeat:         heartbeat,
		TimeoutCapSeconds: capSeconds,
	}
	if sw.FailureCategory != nil {
		out.FailureCategory = *sw.FailureCategory
	}
	if sw.FailureReason != nil {
		out.FailureReason = *sw.FailureReason
	}
	return out
}

// awaitStageTimeoutOutput builds the resumable timeout response. The wait holds
// no server state, so a timeout is an idempotent checkpoint, not an error.
func awaitStageTimeoutOutput(stageType string, stageID uuid.UUID, timeout int, start time.Time, heartbeat bool, capSeconds int) AwaitStageOutput {
	return AwaitStageOutput{
		Status:              "timeout",
		Stage:               stageType,
		StageID:             stageID.String(),
		WaitedSeconds:       time.Since(start).Seconds(),
		PollIntervalSeconds: suggestedStageWaitPollIntervalSeconds,
		Heartbeat:           heartbeat,
		TimeoutCapSeconds:   capSeconds,
		Message: fmt.Sprintf("stage %q did not settle within %ds. The wait holds nothing: re-call fishhawk_await_stage "+
			"to resume it (a safe idempotent no-op), or poll fishhawk_get_run_status every %ds (the authoritative path).",
			stageType, timeout, suggestedStageWaitPollIntervalSeconds),
	}
}

// awaitStageProgressMessage builds the per-tick heartbeat message for a pending
// stage wait (#2490): the awaited stage type and the elapsed wall-clock
// seconds. Pure (no I/O) so a table test pins it.
func awaitStageProgressMessage(stageType string, elapsed time.Duration) string {
	return fmt.Sprintf("await_stage: waiting for stage %q to settle; elapsed %ds", stageType, int(elapsed.Seconds()))
}
