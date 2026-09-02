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
//     terminal (succeeded/failed/cancelled/superseded) OR parked-for-operator
//     (awaiting_approval / awaiting_children / awaiting_input /
//     awaiting_scope_decision / awaiting_deploy_approval /
//     awaiting_host_dispatch). State carries the RAW backend state so a parked
//     awaiting_approval is never coerced to succeeded.
//   - "timeout"      — nothing settled within the window. The wait holds no
//     server state, so re-calling is a safe idempotent no-op.
//   - "run_terminal" — the run reached a terminal state while the stage was
//     still unsettled (ADR-036 backstop); a final read found it still
//     unsettled, so it most likely never will. Do not re-arm blindly.
//   - "amendment_pending" — the SECOND release condition (#2588): the awaited
//     stage filed a mid-stage scope amendment that is still pending. The stage
//     has NOT settled (it stays `running` — filing an amendment does not park
//     it), so Terminal is false and State carries the raw running state;
//     PendingAmendment carries the row to decide and NextStep the pre-filled
//     fishhawk_decide_scope_amendment call.
//
// Settledness WINS over amendment_pending, and the guarantee is enforced rather
// than asserted: the amendment probe re-reads the stage before resolving and
// prefers `settled` if the stage settled while the amendment list was in
// flight. See awaitStageAmendmentRelease.
//
// On the "settled" status the top-level Message is normally EMPTY. It is
// populated (E68.31 / #3081) when the settled stage is an IMPLEMENT stage whose
// LATEST fix-up pass failed and was recovered by the backend: State/Status then
// read `succeeded`, which is true of the stage and misleading about the fix-up,
// so StageWaitStatus.FixupRecovered carries the marker and Message repeats its
// advisory at the top level where an operator cannot miss it. See
// awaitStageSettled.
type AwaitStageOutput struct {
	Status string `json:"status" jsonschema:"one of settled, timeout, run_terminal, amendment_pending"`
	Stage  string `json:"stage" jsonschema:"the resolved stage type"`
	// StageID is the resolved stage UUID — the durable ADR-037 handle, echoed so
	// a resuming caller can re-issue against the same stage.
	StageID string `json:"stage_id" jsonschema:"the resolved stage UUID the wait was armed against (the durable ADR-037 handle, with run_id)"`
	// State is the RAW backend stage state, never coerced: a parked
	// awaiting_approval settles the wait but is reported as awaiting_approval,
	// NOT succeeded, so the operator can tell a completed stage from one parked
	// for their attention.
	State string `json:"state" jsonschema:"the RAW backend stage state (succeeded/failed/cancelled/superseded or a parked awaiting_* state); never coerced, so a parked settled stage is distinguishable from a succeeded one. superseded means a merge made the stage unreachable (#3083) — neither a pass nor an operator cancellation"`
	// Terminal is the endpoint's settledness flag (#1252): true when the stage
	// IsSettled — terminal OR parked. It is the authority for the settled
	// resolve; keying on terminality alone (succeeded/failed/cancelled/superseded) would
	// silently miss the parked half.
	Terminal            bool             `json:"terminal" jsonschema:"true when the stage has SETTLED — a terminal state OR a parked-for-operator state; the authoritative resolve signal"`
	FailureCategory     string           `json:"failure_category,omitempty" jsonschema:"the failed stage's category, when the settled state is failed"`
	FailureReason       string           `json:"failure_reason,omitempty" jsonschema:"the failed stage's reason, when the settled state is failed"`
	StageWaitStatus     *StageWaitStatus `json:"stage_wait_status,omitempty" jsonschema:"the classified execution wait status (same shape get_run_status / dispatch_stage carry), for continuity; the raw state + terminal fields are the authority. On a settled implement stage it may carry fixup_recovered (#3081) — the marker that the latest fix-up pass FAILED and was recovered, so no fix-up commit landed"`
	WaitedSeconds       float64          `json:"waited_seconds" jsonschema:"elapsed wall time spent waiting"`
	Message             string           `json:"message,omitempty" jsonschema:"actionable explanation on the timeout / run_terminal statuses, AND on a SETTLED status whose stage_wait_status carries fixup_recovered (#3081) — a fix-up pass that failed and was recovered, so the succeeded status is misleading about the fix-up"`
	PollIntervalSeconds int              `json:"poll_interval_seconds,omitempty" jsonschema:"server-suggested cadence (seconds) for switching to fishhawk_get_run_status polling; present only on the timeout status"`
	// Heartbeat reports whether the CLIENT supplied a progressToken and a
	// per-tick keep-alive was therefore emitted (#2490). Present on every return
	// path; a false value is the operator's evidence their client does not supply
	// one, so the raised cap is reachable only via long_wait.
	Heartbeat bool `json:"heartbeat" jsonschema:"true when your MCP client supplied a progressToken and a per-tick keep-alive was emitted; false means it did not (use long_wait to reach the raised timeout cap)"`
	// TimeoutCapSeconds is the timeout cap actually applied: 7200 when long_wait
	// or a progressToken was in effect, else 600 (#2490). Present on every path.
	TimeoutCapSeconds int `json:"timeout_cap_seconds" jsonschema:"the timeout cap actually applied to this call (600 by default, 7200 when long_wait or a progressToken was in effect)"`
	// PendingAmendment carries the WHOLE amendment row on the amendment_pending
	// status (#2588) — id, paths+operations, reason, requested_at and the #983
	// cap headroom — so the operator decides without a second
	// fishhawk_list_scope_amendments call. Reuses ScopeAmendmentItem; no new
	// exported type.
	PendingAmendment *ScopeAmendmentItem `json:"pending_amendment,omitempty" jsonschema:"the pending mid-stage scope amendment that released the wait (status amendment_pending): id, requested paths + operations, the agent's reason, and the #983 cap headroom. Decide it with fishhawk_decide_scope_amendment"`
	// NextStep is the pre-filled fishhawk_decide_scope_amendment call for
	// PendingAmendment (#2588), reusing the SuggestedAction shape from
	// next_actions.go so the operator acts in one hop.
	NextStep *SuggestedAction `json:"next_step,omitempty" jsonschema:"the single call to make on the amendment_pending status: fishhawk_decide_scope_amendment with run_id + amendment_id pre-filled"`
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

There are TWO release conditions: the stage SETTLES, or the awaited stage
files a mid-stage scope amendment that is still pending (#2588).

"Settled" is deliberately BROADER than "terminal": the wait resolves the
moment the stage reaches a terminal state (succeeded / failed / cancelled /
superseded) OR
a parked-for-operator state (awaiting_approval / awaiting_children /
awaiting_input / awaiting_scope_decision / awaiting_deploy_approval /
awaiting_host_dispatch) — the settledness a detached watcher actually wants
(release the moment the stage needs attention). The RAW backend state is
carried out on the state field, never coerced, so a parked awaiting_approval
is distinguishable from a succeeded stage. (A freshly dispatched local stage
can legitimately be awaiting_host_dispatch, which IS settled — so a wait
against a never-dispatched stage returns immediately, and the raw state says
why.)

A pending scope amendment does NOT park the stage: it stays 'running' — there
is no awaiting_scope_decision transition on this path, and that state now
carries a decomposition-parent meaning (#2548). So settledness alone would
never release, and the wait would hold past the agent's ` + amendmentPollWindowAdjective() + ` poll window.
The wait releases on the pending amendment instead; the stage state does not
change.

Statuses:
  - "settled"      — the stage settled; state carries the raw state and
                     terminal is true.
  - "amendment_pending" — the awaited stage filed a mid-stage scope amendment
                     that is still pending. The stage is STILL RUNNING
                     (terminal is false; state carries the raw running state).
                     pending_amendment carries the whole row — id, paths,
                     reason, cap headroom — and next_step is a pre-filled
                     fishhawk_decide_scope_amendment call, so no separate
                     fishhawk_list_scope_amendments lookup is needed. Decide
                     PROMPTLY: the agent polls ` + amendmentPollWindowText() + ` per request; if the
                     window elapses with no decision the request EXPIRES
                     UNDECIDED — it stays pending server-side and is NOT a
                     denial, so a late decision is still honored on a
                     fishhawk_retry_stage of the same stage. Settledness wins
                     the race — a stage that has settled resolves 'settled'
                     even with an amendment still pending.
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
		return nil, r.awaitStageSettled(ctx, runID, stageUUID, stageType, sw, start, heartbeat, capSeconds), nil
	}

	// Second release condition (#2588), probed on the FAST PATH before the first
	// tick: an amendment already pending when the wait is armed — the shape an
	// operator re-arming after a timeout hits — releases IMMEDIATELY rather than
	// after a poll interval. Evaluated AFTER the settled check above so a settled
	// stage can never surface as amendment_pending.
	if out, done := r.awaitStageAmendmentRelease(ctx, runID, stageUUID, stageType, sw.State, start, heartbeat, capSeconds); done {
		return nil, out, nil
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
				return nil, r.awaitStageSettled(pollCtx, runID, stageUUID, stageType, sw, start, heartbeat, capSeconds), nil
			}
			// Second release condition (#2588) on every tick, after the settled
			// check and before the ADR-036 backstop. pollCtx (not ctx) so a
			// deadline hit cancels the probe with everything else — an errored
			// probe yields nil and the loop's pollCtx.Done() arm produces the
			// resumable timeout.
			if out, done := r.awaitStageAmendmentRelease(pollCtx, runID, stageUUID, stageType, sw.State, start, heartbeat, capSeconds); done {
				return nil, out, nil
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
		return r.awaitStageSettled(ctx, runID, stageID, stageType, sw, start, heartbeat, capSeconds), true
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

// awaitStagePendingAmendment probes the run's mid-stage scope amendments for a
// PENDING one filed by the awaited stage (#2588), returning nil when there is
// none. It reuses the EXISTING apiClient.ListScopeAmendments read (the same
// GET /v0/runs/{run_id}/scope-amendments fishhawk_list_scope_amendments hits),
// so this adds no REST surface.
//
// BEST-EFFORT by construction, mirroring awaitStageRunTerminalBackstop: any
// list error returns nil, so the normal poll/timeout path stays in charge and
// the wait NEVER fails on this probe. Losing an advisory signal beats killing
// an hours-long wait on a transient list error.
//
// The match is STRICT on both fields. Status must be exactly "pending" (a
// decided amendment is not actionable and must not release a re-armed wait),
// and StageID must equal the awaited stage — the backend's amendment row
// carries a non-pointer, non-omitempty stage_id, so equality is a complete
// predicate rather than one that silently drops rows, and a sibling stage's
// request can never release this wait. stageID is a string because that is
// what ScopeAmendmentItem.StageID carries; awaitStageAmendmentPendingOutput
// takes the same type for the same reason.
//
// The endpoint returns items oldest-first, so the first match is the oldest
// pending request — the one closest to expiring undecided at the
// amendmentPollWindow cap.
func (r *runResolver) awaitStagePendingAmendment(ctx context.Context, runID uuid.UUID, stageID string) *ScopeAmendmentItem {
	items, err := r.api.ListScopeAmendments(ctx, runID)
	if err != nil {
		return nil
	}
	for i := range items {
		if items[i].Status == "pending" && items[i].StageID == stageID {
			return &items[i]
		}
	}
	return nil
}

// awaitStageAmendmentRelease is the #2588 second release condition, shaped like
// awaitStageRunTerminalBackstop: it returns (output, true) to resolve the wait
// and (zero, false) to keep polling.
//
// Settledness WINS, and the claim is MADE TRUE rather than asserted. The
// caller's stage read and this probe's amendment-list read are not atomic: the
// stage can settle between them, so an ordering that returned amendment_pending
// on the strength of the caller's now-stale read would report a running stage
// that has in fact settled. So after finding a pending amendment this re-reads
// the stage (wait=0) and prefers `settled`. The extra read is paid only on the
// rare tick that actually finds a pending amendment, never on the common path.
//
// The re-read is best-effort in the same direction as the probe: if it fails,
// the amendment release stands. That is the honest resolution — a pending
// amendment is known to exist, settledness merely could not be confirmed — and
// the operator's next re-arm reads the settled stage.
//
// state is the raw stage state from the caller's read, carried onto the
// amendment_pending output so Terminal/State stay the honest settledness
// signal. stageUUID is uuid.UUID only because the api-client reads require it;
// both helpers below key on its string form, matching ScopeAmendmentItem.
func (r *runResolver) awaitStageAmendmentRelease(ctx context.Context, runID, stageUUID uuid.UUID, stageType, state string, start time.Time, heartbeat bool, capSeconds int) (AwaitStageOutput, bool) {
	item := r.awaitStagePendingAmendment(ctx, runID, stageUUID.String())
	if item == nil {
		return AwaitStageOutput{}, false
	}
	if sw, err := r.api.GetRunStageWait(ctx, runID, stageUUID, 0); err == nil && sw != nil && sw.Terminal {
		return r.awaitStageSettled(ctx, runID, stageUUID, stageType, sw, start, heartbeat, capSeconds), true
	}
	return awaitStageAmendmentPendingOutput(stageType, stageUUID.String(), runID, state, item, start, heartbeat, capSeconds), true
}

// awaitStageAmendmentPendingOutput builds the amendment_pending response
// (#2588). Terminal is FALSE and State carries the raw (normally "running")
// stage state: the stage genuinely has not settled, and the flag stays the
// honest settledness signal rather than being borrowed to mean "the wait
// released". The row plus a pre-filled fishhawk_decide_scope_amendment
// next_step ride out so the operator decides in one hop.
func awaitStageAmendmentPendingOutput(stageType, stageID string, runID uuid.UUID, state string, item *ScopeAmendmentItem, start time.Time, heartbeat bool, capSeconds int) AwaitStageOutput {
	paths := make([]string, 0, len(item.Paths))
	for _, p := range item.Paths {
		paths = append(paths, fmt.Sprintf("%s (%s)", p.Path, p.Operation))
	}
	pathList := strings.Join(paths, ", ")
	if pathList == "" {
		pathList = "(no paths listed)"
	}
	return AwaitStageOutput{
		Status:            "amendment_pending",
		Stage:             stageType,
		StageID:           stageID,
		State:             state,
		Terminal:          false,
		WaitedSeconds:     time.Since(start).Seconds(),
		Heartbeat:         heartbeat,
		TimeoutCapSeconds: capSeconds,
		PendingAmendment:  item,
		NextStep: &SuggestedAction{
			Action: "fishhawk_decide_scope_amendment",
			Params: map[string]string{
				"run_id":       runID.String(),
				"amendment_id": item.ID,
			},
			Precondition: "the awaited stage filed this scope amendment and it is still pending",
			Consumes:     "none",
			Reason:       "the agent is blocked on this decision and its " + amendmentPollWindowText() + " poll window expires UNDECIDED (an expiry is not a denial — the request stays pending)",
		},
		Message: fmt.Sprintf("stage %q is still running and filed scope amendment %s for %s. Reason: %s. "+
			"Decide it now with fishhawk_decide_scope_amendment (run_id + amendment_id are pre-filled on next_step): "+
			"the agent polls %s per request and then the request EXPIRES UNDECIDED (it stays pending — an expiry is not a denial, so decide now).",
			stageType, item.ID, pathList, item.Reason, amendmentPollWindowText()),
	}
}

// awaitStageSettled is the settled-response builder every settled path goes
// through (E68.31 / #3081): it calls the pure awaitStageSettledOutput and then
// DECORATES the result with the fix-up-recovery marker when one applies.
//
// Without this, a fix-up that FAILED and was recovered by the backend settles
// as a bare `succeeded` / `terminal: true` — byte-identical to a fix-up that
// landed a commit — so an operator cannot tell the routed concerns were never
// addressed. The marker rides on StageWaitStatus (the block get_run_status and
// the dispatch verbs already carry) and its advisory is ALSO promoted to the
// top-level Message, where a caller reading only the settled summary sees it.
//
// The probe is gated on stageType == "implement": no other stage type can carry
// a fix-up, so an ordinary plan / review / acceptance wait pays nothing. It is
// BEST-EFFORT (fixupRecoveryFor returns nil on any audit read error), so a
// transient audit failure loses the advisory and returns the settled response
// INTACT rather than failing a wait that may have been running for hours.
func (r *runResolver) awaitStageSettled(ctx context.Context, runID, stageUUID uuid.UUID, stageType string, sw *RunStageWait, start time.Time, heartbeat bool, capSeconds int) AwaitStageOutput {
	out := awaitStageSettledOutput(stageType, sw, start, heartbeat, capSeconds)
	if stageType != "implement" {
		return out
	}
	return decorateSettledWithFixupRecovery(out, r.fixupRecoveryFor(ctx, runID, stageUUID))
}

// decorateSettledWithFixupRecovery attaches the #3081 marker to a settled
// response. The structured block and the top-level message are ONE advisory in
// two places, so they are attached TOGETHER or not at all: a response carrying
// the prose without the block would hand a caller a warning it cannot act on
// programmatically, and one carrying the block without the prose would let a
// caller reading only the summary miss it.
//
// The StageWaitStatus nil arm is unreachable on today's code —
// awaitStageSettledOutput always populates it from classifyStageWaitStatus,
// which returns a non-nil *StageWaitStatus on every path — so this is
// defence-in-depth against a future refactor that makes the field optional,
// not a live branch. It is a pure function precisely so the pairing stays
// falsifiable: the surrounding builder cannot produce a nil block, but a test
// can hand one straight to this.
func decorateSettledWithFixupRecovery(out AwaitStageOutput, rec *FixupRecovery) AwaitStageOutput {
	if rec == nil || out.StageWaitStatus == nil {
		return out
	}
	out.StageWaitStatus.FixupRecovered = rec
	out.Message = rec.Message
	return out
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
