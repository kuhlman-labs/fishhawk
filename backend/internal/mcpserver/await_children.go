package mcpserver

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/wavecoverage"
)

// AwaitChildrenInput is the fishhawk_await_children tool's input schema (E50.13
// / #2363). run_id is the DECOMPOSED PARENT whose fan-out is being watched.
type AwaitChildrenInput struct {
	RunID          string `json:"run_id" jsonschema:"the DECOMPOSED PARENT run UUID whose children this call watches"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"how long to wait before returning 'timeout' (default 360). Cap is 600 by default, raised to 7200 when long_wait=true OR your MCP client supplied a progressToken. On timeout, re-call to resume — the wait holds no server state"`
	LongWait       bool   `json:"long_wait,omitempty" jsonschema:"unlock the 7200s timeout cap WITHOUT a progressToken (default false = 600s cap). The wait holds no state and is resumable, so a client-cut-short call is a safe no-op to re-issue"`
}

// AwaitChildrenOutput is the fishhawk_await_children response. Status is one of:
//
//   - "amendment_pending"     — some child filed a mid-stage scope amendment that
//     is still pending. Decide it, then re-invoke.
//   - "children_dispatchable" — some child's implement stage is awaiting a host
//     dispatch AND its dependency slices are provably merged onto the parent's
//     consolidated branch, so fishhawk_run_children will actually spawn it.
//   - "children_settled"      — every child reached a terminal run state.
//   - "timeout"               — none of the above within the window. The wait
//     holds no server state, so re-calling is a safe no-op.
//
// EVERY release condition is an ABSOLUTE PROPERTY OF THE CURRENT SNAPSHOT,
// evaluated on EVERY poll INCLUDING THE FIRST. There is no first-poll baseline,
// no remembered previous snapshot, and no transition detection anywhere in this
// verb. That is deliberate and load-bearing: a transition-keyed release can
// neither fire before the integration it is waiting for, nor — when the
// interesting change already happened before the call — ever fire at all.
type AwaitChildrenOutput struct {
	Status string `json:"status" jsonschema:"one of amendment_pending, children_dispatchable, children_settled, timeout"`
	RunID  string `json:"run_id" jsonschema:"the decomposed parent run UUID the wait was armed against"`
	// Children is the SAME ChildrenStatus snapshot fishhawk_get_run_status
	// carries, returned on every release so the operator sees the whole fan-out
	// in one hop.
	Children      *ChildrenStatus `json:"children,omitempty" jsonschema:"the parent's per-child + integration-phase snapshot at release, the same block fishhawk_get_run_status carries"`
	WaitedSeconds float64         `json:"waited_seconds" jsonschema:"elapsed wall time spent waiting"`
	// PendingAmendmentChildRunID names the child whose amendment released the
	// wait (status amendment_pending).
	PendingAmendmentChildRunID string `json:"pending_amendment_child_run_id,omitempty" jsonschema:"the child run whose pending scope amendment released the wait"`
	// PendingAmendment carries the WHOLE amendment row, reusing
	// ScopeAmendmentItem exactly as fishhawk_await_stage does.
	PendingAmendment *ScopeAmendmentItem `json:"pending_amendment,omitempty" jsonschema:"the pending mid-stage scope amendment that released the wait (status amendment_pending): id, requested paths + operations, the agent's reason, and the #983 cap headroom"`
	// DispatchableChildRunIDs names every child that is dispatchable NOW, in
	// ascending slice order (status children_dispatchable).
	DispatchableChildRunIDs []string `json:"dispatchable_child_run_ids,omitempty" jsonschema:"the children whose implement stage awaits a host dispatch AND whose dependency slices are already merged onto the consolidated branch, in ascending slice order"`
	// NextStep is the single pre-filled call to make on this release.
	NextStep            *SuggestedAction `json:"next_step,omitempty" jsonschema:"the single call to make on this release: decide the amendment, re-invoke run_children, or consolidate"`
	Message             string           `json:"message,omitempty" jsonschema:"actionable explanation of the release"`
	PollIntervalSeconds int              `json:"poll_interval_seconds,omitempty" jsonschema:"server-suggested cadence (seconds) for switching to fishhawk_get_run_status polling; present only on the timeout status"`
	Heartbeat           bool             `json:"heartbeat" jsonschema:"true when your MCP client supplied a progressToken and a per-tick keep-alive was emitted"`
	TimeoutCapSeconds   int              `json:"timeout_cap_seconds" jsonschema:"the timeout cap actually applied to this call (600 by default, 7200 when long_wait or a progressToken was in effect)"`
}

// registerAwaitChildren wires the fishhawk_await_children tool (E50.13 /
// #2363): the read-only in-band wait that replaced fishhawk_run_children's
// blocking await-all. Read-only per ADR-021 — it polls server state and
// nothing else: it spawns nothing, cancels nothing, holds no handle. Because it
// owns no result, a return can never race a write.
func registerAwaitChildren(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_await_children",
		Description: strings.TrimSpace(`
Block until a decomposed parent's fan-out needs you, then return the whole
per-child snapshot. This is the in-band wait for fishhawk_run_children, which
now dispatches children DETACHED and returns immediately (#2363).

It is READ-ONLY: it polls server state, spawns nothing, cancels nothing and
holds no handle.

Release conditions, checked in this order on EVERY poll including the FIRST:

  - "amendment_pending"     — some child filed a mid-stage scope amendment that
                              is still pending. Checked FIRST because an open
                              window is the time-critical condition: the agent
                              polls ` + amendmentPollWindowText() + ` per request and then the request
                              EXPIRES UNDECIDED (it stays pending — an expiry is
                              NOT a denial). pending_amendment carries the whole
                              row and next_step is a pre-filled
                              fishhawk_decide_scope_amendment call.
  - "children_dispatchable" — some child's implement stage awaits a host
                              dispatch AND its dependency slices are provably
                              MERGED onto the parent's consolidated branch — the
                              same coverage predicate the server admits on. Not
                              merely "unblocked": predecessor run state flips to
                              succeeded BEFORE the between-wave integration runs,
                              so a state-keyed release would announce a dispatch
                              the server then refuses 409 wave_not_integrated.
                              next_step re-invokes fishhawk_run_children.
  - "children_settled"      — every child reached a terminal run state.
                              next_step is fishhawk_consolidate_slices.
  - "timeout"               — none of the above within the window. The wait
                              holds no server state, so re-calling is a safe
                              idempotent no-op.

Every condition is an ABSOLUTE property of the current snapshot — there is no
baseline and no transition detection. So calling this right after a 409
wave_not_integrated correctly does NOT release until the server's between-wave
integration makes the coverage predicate true, and calling it when a
budget-deferred child is already dispatchable releases on the FIRST poll rather
than waiting for a transition that will never occur.

CONCURRENT AMENDMENTS ARE SERIAL BY CONTRACT. Several children can have open
windows at once, and this verb surfaces exactly ONE amendment per release,
chosen deterministically: the LOWEST SLICE INDEX first (ties broken by run id so
the order is total), and within that child the OLDEST pending request — the one
closest to expiring. The loop is: await releases on one amendment; decide it;
re-invoke await, which releases on the next pending one; repeat until it
releases with children_dispatchable or children_settled. Because every child's
window runs concurrently while your session is free, serial decisions still land
inside their individual windows — which is exactly the property this change
exists to create.

Inputs:
  - run_id          (required) — the DECOMPOSED PARENT run UUID.
  - long_wait       — unlock the 7200s cap from a tool call (default false).
  - timeout_seconds — default 360; cap 600, or 7200 when long_wait=true OR a
                      progressToken is present.
`),
	}, resolver.awaitChildren)
}

// awaitChildren is the tool handler. Structurally a sibling of awaitStage: a
// fast-path evaluation, then a poll on the injectable reviewPollInterval under a
// clamped deadline. Unlike awaitStage there is no long-poll endpoint to lean on
// — the snapshot is assembled from existing reads — so every tick re-evaluates
// the same absolute predicate the fast path did.
func (r *runResolver) awaitChildren(ctx context.Context, req *mcp.CallToolRequest, in AwaitChildrenInput) (*mcp.CallToolResult, AwaitChildrenOutput, error) {
	parentUUID, err := uuid.Parse(in.RunID)
	if err != nil {
		return nil, AwaitChildrenOutput{}, fmt.Errorf("run_id %q is not a valid UUID: %w", in.RunID, err)
	}

	var progToken any
	if req != nil && req.Params != nil {
		progToken = req.Params.GetProgressToken()
	}
	heartbeat := progToken != nil
	capSeconds := effectiveAwaitCap(heartbeat, in.LongWait)
	timeout := clampAwaitTimeoutHeartbeat(in.TimeoutSeconds, heartbeat, in.LongWait)
	start := time.Now()

	// Fast path: evaluate the absolute predicate BEFORE the first tick. This is
	// what makes "the interesting thing already happened" releasable.
	if out, done, ferr := r.awaitChildrenEvaluate(ctx, parentUUID, start, heartbeat, capSeconds); ferr != nil {
		return nil, AwaitChildrenOutput{}, ferr
	} else if done {
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
			return nil, awaitChildrenTimeoutOutput(parentUUID.String(), timeout, start, heartbeat, capSeconds), nil
		case <-ticker.C:
			// Best-effort progress heartbeat once per tick (opt-in), mirroring
			// await_stage: emitted only when the caller supplied a progressToken
			// AND the request carries a live session. A failed notify is
			// SWALLOWED — the await is authoritative, the heartbeat advisory.
			if progToken != nil && req != nil && req.Session != nil {
				progress++
				_ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
					ProgressToken: progToken,
					Progress:      progress,
					Message: fmt.Sprintf("await_children: watching run %s fan-out; elapsed %ds",
						parentUUID, int(time.Since(start).Seconds())),
				})
			}
			out, done, eerr := r.awaitChildrenEvaluate(pollCtx, parentUUID, start, heartbeat, capSeconds)
			if eerr != nil {
				// A deadline hit mid-poll cancels the in-flight request; that is a
				// timeout, not a transport failure.
				if pollCtx.Err() != nil {
					return nil, awaitChildrenTimeoutOutput(parentUUID.String(), timeout, start, heartbeat, capSeconds), nil
				}
				return nil, AwaitChildrenOutput{}, eerr
			}
			if done {
				return nil, out, nil
			}
		}
	}
}

// awaitChildrenEvaluate reads ONE snapshot of the parent's fan-out and applies
// the three absolute release conditions in their fixed order. It returns
// (output, true, nil) to release, (zero, false, nil) to keep polling, and a
// non-nil error only for a failure that makes the snapshot unreadable at all
// (an unreadable parent is a caller error, not something to spin on).
func (r *runResolver) awaitChildrenEvaluate(ctx context.Context, parentUUID uuid.UUID, start time.Time, heartbeat bool, capSeconds int) (AwaitChildrenOutput, bool, error) {
	cs, err := r.childrenStatusForAwait(ctx, parentUUID)
	if err != nil {
		return AwaitChildrenOutput{}, false, err
	}
	if cs == nil {
		return AwaitChildrenOutput{}, false, fmt.Errorf(
			"run %s has no plan_decomposed audit entry; it is not a decomposed parent (nothing to await)", parentUUID)
	}

	base := AwaitChildrenOutput{
		RunID:             parentUUID.String(),
		Children:          cs,
		WaitedSeconds:     time.Since(start).Seconds(),
		Heartbeat:         heartbeat,
		TimeoutCapSeconds: capSeconds,
	}

	// (1) amendment_pending — checked FIRST because an open decision window is
	// the time-critical condition.
	if childID, item := r.awaitChildrenPendingAmendment(ctx, cs); item != nil {
		out := base
		out.Status = "amendment_pending"
		out.PendingAmendmentChildRunID = childID
		out.PendingAmendment = item
		out.NextStep = &SuggestedAction{
			Action: "fishhawk_decide_scope_amendment",
			Params: map[string]string{
				"run_id":       childID,
				"amendment_id": item.ID,
			},
			Precondition: "this fan-out child filed the scope amendment and it is still pending",
			Consumes:     "none",
			Reason: "the child agent is blocked on this decision and its " + amendmentPollWindowText() +
				" poll window expires UNDECIDED (an expiry is not a denial — the request stays pending)",
		}
		out.Message = fmt.Sprintf(
			"child %s filed scope amendment %s for %s. Reason: %s. Decide it now with fishhawk_decide_scope_amendment "+
				"(run_id + amendment_id are pre-filled on next_step): the agent polls %s per request and then the request "+
				"EXPIRES UNDECIDED. Amendments are surfaced ONE at a time in ascending slice order — decide this one, then "+
				"re-invoke fishhawk_await_children for the next.",
			childID, item.ID, amendmentPathList(item), item.Reason, amendmentPollWindowText())
		return out, true, nil
	}

	// (2) children_dispatchable — keyed on the SHARED coverage predicate, not on
	// ChildStatus.Blocked. Blocked keys on predecessor run STATE, which flips to
	// succeeded BEFORE the between-wave integration runs, so a Blocked-keyed
	// release would announce dispatchability the server refuses 409
	// wave_not_integrated.
	if ids := awaitChildrenDispatchable(cs); len(ids) > 0 {
		out := base
		out.Status = "children_dispatchable"
		out.DispatchableChildRunIDs = ids
		out.NextStep = &SuggestedAction{
			Action:       "fishhawk_run_children",
			Params:       map[string]string{"run_id": parentUUID.String()},
			Precondition: "at least one child awaits a host dispatch and its dependency slices are merged onto the consolidated branch",
			Consumes:     "none",
			Reason:       "the server will admit these children now; fishhawk_run_children spawns them detached and returns immediately",
		}
		out.Message = fmt.Sprintf(
			"%d child(ren) are dispatchable now (%s): their implement stage awaits a host dispatch and every dependency slice is merged onto the parent's consolidated branch. Re-invoke fishhawk_run_children.",
			len(ids), strings.Join(ids, ", "))
		return out, true, nil
	}

	// (3) children_settled — every child terminal.
	if awaitChildrenAllSettled(cs) {
		out := base
		out.Status = "children_settled"
		out.NextStep = &SuggestedAction{
			Action:       "fishhawk_consolidate_slices",
			Params:       map[string]string{"run_id": parentUUID.String()},
			Precondition: "every decomposed child reached a terminal run state",
			Consumes:     "none",
			Reason:       "the fan-out is complete; consolidate the slices into the parent's consolidated branch and PR",
		}
		out.Message = fmt.Sprintf(
			"all %d children are terminal (%d succeeded, %d failed). Consolidate with fishhawk_consolidate_slices.",
			cs.Total, cs.Succeeded, cs.Failed)
		return out, true, nil
	}

	return AwaitChildrenOutput{}, false, nil
}

// childrenStatusForAwait assembles the parent's ChildrenStatus snapshot for the
// await verb. It reuses childrenStatusFor VERBATIM — the same block
// fishhawk_get_run_status carries — feeding it the parent's recent audit window
// so the integration phase, the consolidated branch and (E50.13 / #2363) the
// integrated child run ids are decoded from the same entries.
func (r *runResolver) childrenStatusForAwait(ctx context.Context, parentUUID uuid.UUID) (*ChildrenStatus, error) {
	entries, _, err := r.api.ListRunAudit(ctx, parentUUID, ListRunAuditFilter{Limit: awaitChildrenAuditLimit})
	if err != nil {
		return nil, fmt.Errorf("read parent audit: %w", err)
	}
	return r.childrenStatusFor(ctx, parentUUID, entries)
}

// awaitChildrenAuditLimit bounds the parent-audit window the snapshot decodes
// the fan-in markers from. The fan-in kinds (slices_integrated /
// slice_integration_conflict) land late in a decomposed parent's audit, and the
// endpoint returns the window ASCENDING with a per-request cap of 500, so this
// takes the endpoint's cap.
const awaitChildrenAuditLimit = 500

// awaitChildrenPendingAmendment finds the ONE amendment to surface, applying
// the deterministic selection rule the tool description and the README both
// state: children are examined in ASCENDING SLICE INDEX with ties broken by run
// id (so the order is total and stable even though minting forbids a duplicate
// slice index), and within a child awaitStagePendingAmendment returns the OLDEST
// pending request because the endpoint returns items oldest-first.
//
// The per-child predicate is #2588's awaitStagePendingAmendment reused VERBATIM
// against the child's run id and implement stage id: status exactly "pending"
// AND stage_id equal to the child's implement stage. Nothing about it is
// parent/child-aware, which is why it is reusable rather than re-derived — a
// re-derivation that loosened either half is the drift the two negative pins
// exist to catch.
//
// Best-effort in the same direction as awaitStage's probe: a child whose stage
// cannot be resolved is skipped rather than failing the whole wait.
func (r *runResolver) awaitChildrenPendingAmendment(ctx context.Context, cs *ChildrenStatus) (string, *ScopeAmendmentItem) {
	ordered := make([]ChildStatus, len(cs.Children))
	copy(ordered, cs.Children)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].SliceIndex != ordered[j].SliceIndex {
			return ordered[i].SliceIndex < ordered[j].SliceIndex
		}
		return ordered[i].RunID < ordered[j].RunID
	})
	for _, c := range ordered {
		childUUID, perr := uuid.Parse(c.RunID)
		if perr != nil {
			continue
		}
		stage, serr := r.resolveStage(ctx, childUUID, "implement", "")
		if serr != nil {
			continue
		}
		if item := r.awaitStagePendingAmendment(ctx, childUUID, stage.ID); item != nil {
			return c.RunID, item
		}
	}
	return "", nil
}

// awaitChildrenDispatchable returns the run ids of every child the SERVER would
// admit right now, in ascending slice order.
//
// The predicate is deliberately in two halves. (a) The child's implement stage
// awaits a host dispatch — modelled here by the child's RUN state, because a
// decomposed child parked by RuleChildrenDispatch has run state 'pending' until
// a host dispatch drives it; a 'running' child already has a runner. (b) Its
// dependency slices are COVERED per wavecoverage.Covered — the identical
// function the sweeper's short-circuit and the host-dispatch marker's admission
// use. Reconstructing (b) from ChildStatus.Blocked would be wrong in a way
// invisible to this package's own tests: Blocked keys on predecessor run STATE,
// which flips to succeeded BEFORE the integration runs.
func awaitChildrenDispatchable(cs *ChildrenStatus) []string {
	sliceRunID := make(map[int]string, len(cs.Children))
	for _, c := range cs.Children {
		sliceRunID[c.SliceIndex] = c.RunID
	}
	ordered := make([]ChildStatus, len(cs.Children))
	copy(ordered, cs.Children)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].SliceIndex != ordered[j].SliceIndex {
			return ordered[i].SliceIndex < ordered[j].SliceIndex
		}
		return ordered[i].RunID < ordered[j].RunID
	})
	var out []string
	for _, c := range ordered {
		if c.State != "pending" {
			continue
		}
		if covered, _ := wavecoverage.Covered(c.DependsOn, sliceRunID, cs.IntegratedChildRunIDs); covered {
			out = append(out, c.RunID)
		}
	}
	return out
}

// awaitChildrenAllSettled reports whether every discovered child reached a
// terminal run state. An "unknown" child (its per-child GetRun failed) is NOT
// terminal, so a read failure can never fake a settled fan-out.
func awaitChildrenAllSettled(cs *ChildrenStatus) bool {
	if len(cs.Children) == 0 {
		return false
	}
	for _, c := range cs.Children {
		if !runStateIsTerminal(c.State) {
			return false
		}
	}
	return true
}

// amendmentPathList renders an amendment's requested paths for an operator
// message, matching awaitStageAmendmentPendingOutput's phrasing.
func amendmentPathList(item *ScopeAmendmentItem) string {
	paths := make([]string, 0, len(item.Paths))
	for _, p := range item.Paths {
		paths = append(paths, fmt.Sprintf("%s (%s)", p.Path, p.Operation))
	}
	if len(paths) == 0 {
		return "(no paths listed)"
	}
	return strings.Join(paths, ", ")
}

// awaitChildrenTimeoutOutput builds the resumable timeout response. The wait
// holds no server state, so a timeout is an idempotent checkpoint, not an error.
func awaitChildrenTimeoutOutput(runID string, timeout int, start time.Time, heartbeat bool, capSeconds int) AwaitChildrenOutput {
	return AwaitChildrenOutput{
		Status:              "timeout",
		RunID:               runID,
		WaitedSeconds:       time.Since(start).Seconds(),
		PollIntervalSeconds: suggestedStageWaitPollIntervalSeconds,
		Heartbeat:           heartbeat,
		TimeoutCapSeconds:   capSeconds,
		Message: fmt.Sprintf("no child of run %s filed an amendment, became dispatchable, or settled within %ds. "+
			"The wait holds nothing: re-call fishhawk_await_children to resume it (a safe idempotent no-op), "+
			"or poll fishhawk_get_run_status every %ds (the authoritative path).",
			runID, timeout, suggestedStageWaitPollIntervalSeconds),
	}
}

// awaitChildrenNextStep builds the fishhawk_await_children pointer a successful
// fishhawk_run_children dispatch returns: the in-band wait that replaced the old
// blocking await-all. Reuses the existing SuggestedAction shape.
func awaitChildrenNextStep(parentRunID string) *SuggestedAction {
	return &SuggestedAction{
		Action:       "fishhawk_await_children",
		Params:       map[string]string{"run_id": parentRunID},
		Precondition: "children were dispatched detached on this parent",
		Consumes:     "none",
		Reason: "the dispatch is detached and this session is free — block here until a child files a mid-stage " +
			"scope amendment (decidable IN BAND), another child becomes dispatchable, or every child settles",
	}
}
