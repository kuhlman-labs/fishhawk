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
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"how long to wait before returning 'timeout' (default 600). Cap is 600 by default, raised to 7200 when long_wait=true OR your MCP client supplied a progressToken. So an omitted value with neither opt-in waits the full 600s cap. On timeout, re-call to resume — the wait holds no server state"`
	LongWait       bool   `json:"long_wait,omitempty" jsonschema:"unlock the 7200s timeout cap WITHOUT a progressToken (default false = 600s cap). The wait holds no state and is resumable, so a client-cut-short call is a safe no-op to re-issue"`
}

// AwaitChildrenOutput is the fishhawk_await_children response. Status is one of:
//
//   - "amendment_pending"     — some child filed a mid-stage scope amendment that
//     is still pending. Decide it, then re-invoke.
//   - "children_dispatchable" — some child's implement stage is awaiting a host
//     dispatch AND its dependency slices are provably merged onto the parent's
//     consolidated branch, so fishhawk_run_children will actually spawn it.
//   - "integration_failed"    — the parent's newest fan-in FAILURE
//     (slice_head_missing, slice_integration_conflict or
//     slice_integration_failed) is newer than its newest clean integration
//     (#4080). Released even while children are in flight: a between-wave
//     failure blocks the dependent wave indefinitely.
//   - "child_failed"          — some child is terminal-failed (failed or
//     cancelled) AND every non-terminal child is undispatched and transitively
//     depends on a failed child (#4178): no child can make progress until the
//     failed slice recovers, so waiting for every child to settle would block
//     until the timeout.
//   - "children_failed"       — every child is terminal and some did not
//     succeed (#4080).
//   - "integration_pending"   — every child succeeded but the newest
//     slices_integrated entry does not cover every child (#4080): the
//     consolidated branch lacks those slices, so acceptance and review wait.
//     When the server HAS slice-integration authority and the parent's
//     implement stage already succeeded, the uncovered slices mean a lost
//     best-effort slices_integrated append — NO record at all (#4165), or only
//     an earlier between-wave record surviving a lost FINAL append in a
//     multi-wave fan-out (#4221) — and the release names the integrate-wave
//     recovery instead of re-driving the already-succeeded children.
//   - "children_settled"      — every child succeeded AND the newest
//     slices_integrated entry covers every child (#4080; before, any terminal
//     fan-out released this). On a deployment with no slice-integration
//     authority — capabilities.slice_integration.available false, the server
//     gate's own predicate (#4165); on an older backend inferred from no record
//     and a succeeded parent implement stage — it falls back to the pre-#4080
//     all-terminal release (approval condition C3). A FAILED read of the
//     parent keeps the wait instead (#4220); it is never the absent key.
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
	Status string `json:"status" jsonschema:"one of amendment_pending, children_dispatchable, integration_failed, child_failed, children_failed, integration_pending, children_settled, timeout"`
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
	// UnintegratedChildRunIDs names the succeeded children the newest
	// slices_integrated entry does not cover (status integration_pending).
	UnintegratedChildRunIDs []string `json:"unintegrated_child_run_ids,omitempty" jsonschema:"the succeeded children whose slices are NOT on the consolidated branch yet, in slice order (status integration_pending)"`
	// IntegrationFailure names the fan-in failure that released the wait
	// (status integration_failed).
	IntegrationFailure *integrationFailure `json:"integration_failure,omitempty" jsonschema:"the fan-in failure that released the wait (status integration_failed): its cause (audit category), the failing child and slice, and the detail"`
	// FailedChildRunIDs names the terminal children that did not succeed
	// (status children_failed or child_failed), in slice order.
	FailedChildRunIDs []string `json:"failed_child_run_ids,omitempty" jsonschema:"the terminal children that did not succeed, in slice order (status children_failed or child_failed)"`
	// BlockedChildRunIDs names the non-terminal children parked behind a failed
	// child (status child_failed, #4178), in slice order.
	BlockedChildRunIDs []string `json:"blocked_child_run_ids,omitempty" jsonschema:"the non-terminal children that cannot progress because they are undispatched and transitively depend on a failed child, in slice order (status child_failed)"`
	// NextStep is the single pre-filled call to make on this release.
	NextStep            *SuggestedAction `json:"next_step,omitempty" jsonschema:"the single call to make on this release: decide the amendment, re-invoke run_children, read the failure's audit, recover the failed child (retry_stage, resume_run or its status), read the failed child's status, consolidate, or re-arm the wait"`
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
  - "integration_failed"    — the parent's newest fan-in FAILURE
                              (slice_head_missing, slice_integration_conflict
                              or slice_integration_failed) is newer than its
                              newest clean integration. Released even while
                              children are in flight. integration_failure names
                              the cause, child, slice and detail; next_step is
                              fishhawk_list_audit on that category. Do NOT
                              dispatch acceptance or approve the review.
  - "child_failed"          — some child is terminal-failed (run failed or
                              cancelled) AND no other child can progress: at
                              least one child is non-terminal and EVERY
                              non-terminal child is undispatched (implement
                              stage pending/awaiting_host_dispatch) and
                              transitively depends on a failed child through
                              the plan's slice depends_on. An independent
                              sibling still in flight (or waiting only on the
                              between-wave integration) keeps the wait armed;
                              an unknown-state child or a not-minted dependency
                              fails closed to the wait. failed_child_run_ids
                              and blocked_child_run_ids name both sides.
                              next_step recovers the lowest-slice failed child
                              from its implement failure category:
                              fishhawk_retry_stage {stage_id} for A/C/D,
                              fishhawk_resume_run {parent_run_id: <child>}
                              (in-place re-drive) for B, and
                              fishhawk_get_run_status on the child when it was
                              cancelled or its stage is unreadable. Do NOT
                              cancel the dependents.
  - "children_failed"       — every child is terminal and some did not
                              succeed. next_step is fishhawk_get_run_status on
                              the lowest-slice failed child, whose next_actions
                              own its recovery.
  - "integration_pending"   — every child succeeded but the newest
                              slices_integrated record does not cover every
                              child: the consolidated branch lacks those
                              slices (unintegrated_child_run_ids), so
                              acceptance and review must wait. next_step is
                              fishhawk_consolidate_slices while the parent's
                              implement stage is awaiting_children; once it has
                              advanced (consolidate would answer 409
                              not_awaiting_children) the message names the real
                              recovery — re-drive or resume the uncovered child.
                              When the server HAS slice-integration authority
                              and the parent's implement stage succeeded, the
                              uncovered slices mean the record was lost — no
                              slices_integrated record at all, or only an
                              earlier between-wave record because the FINAL
                              append of a multi-wave fan-out was lost: the
                              server refuses acceptance 409
                              acceptance_integration_incomplete, next_step is
                              fishhawk_get_run_status on the parent and the
                              message names the recovery, POST
                              /v0/runs/{run_id}/integrate-wave (do NOT re-drive
                              the uncovered children; they already succeeded).
  - "children_settled"      — every child succeeded AND the newest
                              slices_integrated record covers every child.
                              next_step is fishhawk_consolidate_slices. On a
                              deployment with no slice-integration authority
                              (the run's capabilities.slice_integration says
                              available false — the server gate's own
                              predicate; an older backend without it is
                              inferred from no record + a succeeded parent
                              implement stage) it falls back to releasing on
                              an all-succeeded fan-out, with next_step
                              fishhawk_consolidate_slices only while the
                              parent's implement stage is awaiting_children and
                              fishhawk_get_run_status on the parent otherwise.
                              A FAILED read of the parent's capabilities keeps
                              waiting and never takes this fallback.
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
releases with another status. Because every child's
window runs concurrently while your session is free, serial decisions still land
inside their individual windows — which is exactly the property this change
exists to create.

Inputs:
  - run_id          (required) — the DECOMPOSED PARENT run UUID.
  - long_wait       — unlock the 7200s cap from a tool call (default false).
  - timeout_seconds — default 600; cap 600, or 7200 when long_wait=true OR a
                      progressToken is present (so an omitted value with neither
                      opt-in waits the full 600s cap).
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
	// #2695 item 2: await_children reconciles with #2363's approved 600s default
	// via its OWN default (clampAwaitChildrenTimeout), NOT the shared 360s
	// clampAwaitTimeoutHeartbeat. With neither long_wait nor a progressToken the
	// default now equals the 600s cap BY CONSTRUCTION, which is the approved
	// contract.
	timeout := clampAwaitChildrenTimeout(in.TimeoutSeconds, heartbeat, in.LongWait)
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
			return nil, r.awaitChildrenTimeout(ctx, parentUUID, timeout, start, heartbeat, capSeconds), nil
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
					return nil, r.awaitChildrenTimeout(ctx, parentUUID, timeout, start, heartbeat, capSeconds), nil
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
// the absolute release conditions in their fixed order: amendment_pending,
// children_dispatchable, integration_failed, child_failed (#4178), then the
// all-terminal arm (children_failed / integration_pending / children_settled). It returns
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

	// (3) integration_failed — a fan-in failure newer than the newest clean
	// integration. Released even mid-fan-out (#4080): a between-wave
	// slice_head_missing or conflict blocks the dependent wave indefinitely, so
	// waiting for every child to settle first would hide it.
	if cs.IntegrationFailure != nil {
		return awaitChildrenIntegrationFailedOutput(base, parentUUID, cs.IntegrationFailure), true, nil
	}

	// (3b) child_failed (#4178) — a terminal-failed child while every
	// non-terminal child is undispatched and transitively depends on a failed
	// one: no child can progress, so waiting for (4) would block until the
	// timeout. Placed after (2) and (3) so a progressable sibling or a fan-in
	// failure keeps precedence, and before (4), whose all-terminal territory it
	// never enters (it requires a non-terminal child).
	if failed, blocked := awaitChildrenFailureBlocked(cs); len(failed) > 0 {
		return awaitChildrenChildFailedOutput(base, parentUUID, cs, failed, blocked), true, nil
	}

	// (4) every child terminal: children_failed, integration_pending or
	// children_settled — settled ONLY on full coverage (#4080).
	if !awaitChildrenAllSettled(cs) {
		return AwaitChildrenOutput{}, false, nil
	}
	if failed := awaitChildrenNonSucceeded(cs); len(failed) > 0 {
		out := base
		out.Status = "children_failed"
		out.FailedChildRunIDs = failed
		out.NextStep = &SuggestedAction{
			Action:       "fishhawk_get_run_status",
			Params:       map[string]string{"run_id": failed[0]},
			Precondition: "every decomposed child is terminal and this one (the lowest slice) did not succeed",
			Consumes:     "none",
			Reason:       "the failed child's next_actions own its recovery by failure category; the parent cannot integrate, review or accept until every child succeeds",
		}
		out.Message = fmt.Sprintf(
			"all %d children are terminal but %d did not succeed (%s). The parent cannot integrate, so do NOT consolidate, approve the review or dispatch acceptance. "+
				"Read fishhawk_get_run_status on %s (next_step) for its recovery, then re-invoke fishhawk_await_children.",
			cs.Total, len(failed), strings.Join(failed, ", "), failed[0])
		return out, true, nil
	}
	if cs.IntegrationPhase != integrationPhaseIntegrated {
		parentImplementState := r.parentImplementStageState(ctx, parentUUID)
		// The server's own slice-integration predicate (#4165), read ONLY here
		// so every other release pays zero extra reads. Three states (#4220): a
		// present key is authoritative; an ABSENT key on a successful read (an
		// older backend) is undecidable → the C3 inference fallback; a FAILED
		// read is distinct from an absent key and fails CLOSED to the wait — no
		// release on this poll, the next tick re-reads, a persistent failure
		// surfaces as the resumable timeout. Releasing children_settled on the
		// inference there would hand the exact #4165 lost-record wedge a green
		// light. Same precedent as the child_failed arm, which fails closed to
		// the wait on a child whose GetRun failed.
		authority, aerr := r.parentSliceIntegration(ctx, parentUUID)
		if aerr != nil {
			return AwaitChildrenOutput{}, false, nil
		}
		if integrationAuthorityAbsent(cs, parentImplementState, authority) {
			// C3: the server has no slice-integration authority, so its
			// acceptance gate stands down and no coverage is required. Fall
			// back to the pre-#4080 release.
			return awaitChildrenNoAuthorityOutput(base, parentUUID, cs, parentImplementState, authority), true, nil
		}
		if fanInRecordLost(cs, parentImplementState, authority) {
			// #4165/#4221: authority present, the parent advanced, and the
			// fan-in record covering every child was lost (no record, or only
			// an earlier between-wave one) — the server refuses acceptance and
			// nothing re-integrates on its own. Hold, and name the recovery.
			return awaitChildrenFanInRecordLostOutput(base, parentUUID, cs), true, nil
		}
		return awaitChildrenIntegrationPendingOutput(base, parentUUID, cs, parentImplementState), true, nil
	}
	return awaitChildrenSettledOutput(base, parentUUID, cs), true, nil
}

// awaitChildrenNoAuthorityOutput builds the C3 children_settled release for a
// deployment with no slice-integration authority. next_step is CONDITIONAL on
// the parent's implement stage (run 671e7f41 low (a)):
// fishhawk_consolidate_slices only while it is awaiting_children (where it
// graceful-skips the fan-in and resolves the stage); once the stage has left
// awaiting_children consolidate would answer 409 not_awaiting_children, so the
// step is fishhawk_get_run_status on the PARENT, whose next_actions own what
// comes next. The message names the server's own reason when the backend
// surfaced it (#4165), otherwise the inference it fell back to.
func awaitChildrenNoAuthorityOutput(base AwaitChildrenOutput, parentUUID uuid.UUID, cs *ChildrenStatus, parentImplementState string, authority *runSliceIntegration) AwaitChildrenOutput {
	out := awaitChildrenSettledOutput(base, parentUUID, cs)
	if parentImplementState == "awaiting_children" {
		out.NextStep = &SuggestedAction{
			Action:       "fishhawk_consolidate_slices",
			Params:       map[string]string{"run_id": parentUUID.String()},
			Precondition: "every decomposed child succeeded, the server has no slice-integration authority, and the parent's implement stage is awaiting_children",
			Consumes:     "none",
			Reason:       "the fan-in graceful-skips without slice-integration authority, and consolidate resolves the parent's awaiting_children implement stage",
		}
		out.Message = fmt.Sprintf("all %d children succeeded. Consolidate with fishhawk_consolidate_slices (next_step).", cs.Total)
	} else {
		out.NextStep = &SuggestedAction{
			Action:       "fishhawk_get_run_status",
			Params:       map[string]string{"run_id": parentUUID.String()},
			Precondition: "every decomposed child succeeded, the server has no slice-integration authority, and the parent's implement stage is no longer awaiting_children",
			Consumes:     "none",
			Reason:       "fishhawk_consolidate_slices would answer 409 not_awaiting_children on an advanced parent; the parent's next_actions name its next move",
		}
		out.Message = fmt.Sprintf("all %d children succeeded. Read fishhawk_get_run_status on the parent (next_step) for its next move.", cs.Total)
	}
	if authority != nil {
		reason := authority.Reason
		if reason == "" {
			reason = "unspecified"
		}
		out.Message += fmt.Sprintf(" The server reports no slice-integration authority for this parent (%s), so its acceptance gate stands down and no slices_integrated coverage is required.", reason)
		return out
	}
	out.Message += " No slices_integrated record exists although the parent's implement stage already succeeded: this deployment has no slice-integration authority (no GitHub client or no installation id), so no coverage is required."
	return out
}

// awaitChildrenFanInRecordLostOutput builds the integration_pending release for
// the lost-record wedge (fanInRecordLost, #4165/#4221): every child succeeded,
// the server HAS slice-integration authority, the parent's implement stage
// already succeeded, and the newest slices_integrated record (if any) does not
// cover every child. It holds — agreeing with the server's 409
// acceptance_integration_incomplete — and names the recovery, POST
// /v0/runs/{run_id}/integrate-wave, never a re-drive of the uncovered
// children (they already succeeded). next_step is fishhawk_get_run_status on
// the PARENT (its next_actions carry the same hold and recovery).
func awaitChildrenFanInRecordLostOutput(base AwaitChildrenOutput, parentUUID uuid.UUID, cs *ChildrenStatus) AwaitChildrenOutput {
	out := base
	out.Status = "integration_pending"
	out.UnintegratedChildRunIDs = cs.UnintegratedChildRunIDs
	out.NextStep = &SuggestedAction{
		Action:       "fishhawk_get_run_status",
		Params:       map[string]string{"run_id": parentUUID.String()},
		Precondition: "every child succeeded and the parent's implement stage succeeded, but the newest slices_integrated record (if any) does not cover every child although the server has slice-integration authority",
		Consumes:     "none",
		Reason:       "the fan-in record covering every child was lost; the server refuses acceptance 409 acceptance_integration_incomplete until POST /v0/runs/" + parentUUID.String() + "/integrate-wave rewrites it",
	}
	out.Message = fmt.Sprintf("all %d children succeeded but %s, then re-invoke fishhawk_await_children.",
		cs.Total, fanInRecordLostRecovery(parentUUID.String(), cs))
	return out
}

// awaitChildrenSettledOutput builds the children_settled release.
func awaitChildrenSettledOutput(base AwaitChildrenOutput, parentUUID uuid.UUID, cs *ChildrenStatus) AwaitChildrenOutput {
	out := base
	out.Status = "children_settled"
	out.NextStep = &SuggestedAction{
		Action:       "fishhawk_consolidate_slices",
		Params:       map[string]string{"run_id": parentUUID.String()},
		Precondition: "every decomposed child succeeded and the newest slices_integrated record covers every child",
		Consumes:     "none",
		Reason:       "the fan-out is complete; consolidate the slices into the parent's consolidated branch and PR",
	}
	out.Message = fmt.Sprintf(
		"all %d children succeeded and the consolidated branch carries every slice. Consolidate with fishhawk_consolidate_slices.",
		cs.Total)
	return out
}

// awaitChildrenIntegrationFailedOutput builds the integration_failed release
// (#4080). next_step is fishhawk_list_audit on the failure's own category —
// always legal and read-only — and the message carries the per-cause remedy.
func awaitChildrenIntegrationFailedOutput(base AwaitChildrenOutput, parentUUID uuid.UUID, f *integrationFailure) AwaitChildrenOutput {
	out := base
	out.Status = "integration_failed"
	out.IntegrationFailure = f
	out.NextStep = &SuggestedAction{
		Action:       "fishhawk_list_audit",
		Params:       map[string]string{"run_id": parentUUID.String(), "category": f.Cause},
		Precondition: "the parent's newest fan-in failure is newer than its newest clean slices_integrated record",
		Consumes:     "none",
		Reason:       "read the failure record; the consolidated branch does not carry every slice, so acceptance and review must wait",
	}
	slice := "unknown"
	if f.SliceIndex != nil {
		slice = fmt.Sprintf("%d", *f.SliceIndex)
	}
	var what, remedy string
	switch f.Cause {
	case auditCategorySliceHeadMissing:
		what = fmt.Sprintf("the slice branch %q of child %s (slice %s) is missing: %s", f.Branch, f.ChildRunID, slice, f.Detail)
		remedy = "push the missing slice branch, or resume the child's push (fishhawk_get_run_status on the child names how); the server re-integrates once the branch exists"
	case auditCategorySliceIntegrationConflict:
		what = fmt.Sprintf("child %s (slice %s) could not merge onto the consolidated branch (merge conflict)", f.ChildRunID, slice)
		remedy = "re-drive the conflicting child onto the current consolidated branch (fishhawk_get_run_status on the parent names the conflict-resolution move)"
	default:
		what = fmt.Sprintf("the fan-in gave up: %s", f.Detail)
		remedy = "read fishhawk_get_run_status on the parent for the category-B decomposed-parent recovery"
	}
	out.Message = fmt.Sprintf(
		"slice integration failed for parent %s (%s, audit sequence %d): %s. Do NOT dispatch acceptance or approve the review — the consolidated branch does not carry every slice. Remedy: %s; then re-invoke fishhawk_await_children.",
		parentUUID, f.Cause, f.Sequence, what, remedy)
	return out
}

// awaitChildrenIntegrationPendingOutput builds the integration_pending release
// (#4080): every child succeeded but the newest slices_integrated does not
// cover every child. next_step is CONDITIONAL on the parent's implement stage
// (approval condition C4): fishhawk_consolidate_slices only answers while that
// stage is awaiting_children — once it has advanced it answers 409
// not_awaiting_children — so an advanced parent is pointed at the lowest-slice
// uncovered child instead, and the message names the real recovery.
func awaitChildrenIntegrationPendingOutput(base AwaitChildrenOutput, parentUUID uuid.UUID, cs *ChildrenStatus, parentImplementState string) AwaitChildrenOutput {
	out := base
	out.Status = "integration_pending"
	out.UnintegratedChildRunIDs = cs.UnintegratedChildRunIDs
	uncovered := strings.Join(cs.UnintegratedChildRunIDs, ", ")
	if parentImplementState == "awaiting_children" {
		out.NextStep = &SuggestedAction{
			Action:       "fishhawk_consolidate_slices",
			Params:       map[string]string{"run_id": parentUUID.String()},
			Precondition: "every child succeeded, the newest slices_integrated record does not cover every child, and the parent's implement stage is awaiting_children",
			Consumes:     "none",
			Reason:       "run the fan-in on demand; it surfaces any integration error instead of waiting for the sweeper",
		}
		out.Message = fmt.Sprintf(
			"all %d children succeeded but the consolidated branch lacks the slices of %s (not covered by the newest slices_integrated record). Acceptance and review must wait. "+
				"Run the fan-in now with fishhawk_consolidate_slices (next_step), then re-invoke fishhawk_await_children.",
			cs.Total, uncovered)
		return out
	}
	target := parentUUID.String()
	if len(cs.UnintegratedChildRunIDs) > 0 {
		target = cs.UnintegratedChildRunIDs[0]
	}
	out.NextStep = &SuggestedAction{
		Action:       "fishhawk_get_run_status",
		Params:       map[string]string{"run_id": target},
		Precondition: "every child succeeded but the newest slices_integrated record does not cover every child, and the parent's implement stage is no longer awaiting_children",
		Consumes:     "none",
		Reason:       "fishhawk_consolidate_slices would answer 409 not_awaiting_children here; the uncovered child's slice must be re-driven or resumed",
	}
	stateText := parentImplementState
	if stateText == "" {
		stateText = "unreadable"
	}
	out.Message = fmt.Sprintf(
		"all %d children succeeded but the consolidated branch lacks the slices of %s (not covered by the newest slices_integrated record). Acceptance and review must wait. "+
			"The parent's implement stage is %s, not awaiting_children, so fishhawk_consolidate_slices would answer 409 not_awaiting_children. "+
			"Recovery: re-drive or resume the uncovered child (fishhawk_get_run_status on it, next_step, names the move) so its slice reaches the consolidated branch, then re-invoke fishhawk_await_children.",
		cs.Total, uncovered, stateText)
	return out
}

// parentImplementStageState reads the parent's implement stage state for the
// integration_pending / C3 arms. It selects the NEWEST implement stage by
// Sequence (newestStageByType) rather than the ambiguity-erroring
// resolveStage, so a parent with a retried implement stage is not misread as
// unreadable (#4165). The read is bounded like resolveStage's (5s).
// Best-effort: "" when the stages cannot be read or there is no implement
// stage, which both arms treat as NOT awaiting_children and NOT succeeded — so
// an unreadable parent is never read as an authority-less deployment.
func (r *runResolver) parentImplementStageState(ctx context.Context, parentUUID uuid.UUID) string {
	fetchCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	stages, err := r.api.ListRunStages(fetchCtx, parentUUID)
	if err != nil {
		return ""
	}
	if impl := newestStageByType(stages, "implement"); impl != nil {
		return impl.State
	}
	return ""
}

// parentSliceIntegration reads the parent's capabilities.slice_integration —
// the server gate's own slice-integration predicate (#4165) — with one bounded
// (5s) single-run GetRun. Three distinct states: a non-nil authority is
// AUTHORITATIVE; a nil authority with a nil error means the key is ABSENT on a
// SUCCESSFUL read (an older backend), which is UNDECIDABLE and takes the C3
// inference fallback; a non-nil error is a READ FAILURE, which is never the
// absent key (#4220) — the caller must not fall back to the inference on it.
func (r *runResolver) parentSliceIntegration(ctx context.Context, parentUUID uuid.UUID) (*runSliceIntegration, error) {
	fetchCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	run, err := r.api.GetRun(fetchCtx, parentUUID)
	if err != nil {
		return nil, err
	}
	return sliceIntegrationOf(run), nil
}

// childrenStatusForAwait assembles the parent's ChildrenStatus snapshot for the
// await verb: fanInChildrenStatus (the same block fishhawk_get_run_status
// carries, built from the FULL paginated fan-in history rather than the bounded
// recent window) enriched with each child's implement-stage state.
func (r *runResolver) childrenStatusForAwait(ctx context.Context, parentUUID uuid.UUID) (*ChildrenStatus, error) {
	cs, err := r.fanInChildrenStatus(ctx, parentUUID)
	if err != nil || cs == nil {
		return cs, err
	}
	// Enrich each child with its IMPLEMENT-stage state so the dispatchable
	// predicate keys on the stage state (#1237), not the run state. A parked
	// local child has run state 'running' while its implement stage waits at
	// awaiting_host_dispatch; keying on run state would skip it. Best-effort in
	// the same direction as the amendment arm's probe: a child whose stage
	// cannot be resolved keeps an empty ImplementStageState (not dispatchable),
	// never failing the whole snapshot.
	for i := range cs.Children {
		childUUID, perr := uuid.Parse(cs.Children[i].RunID)
		if perr != nil {
			continue
		}
		if stage, serr := r.resolveStage(ctx, childUUID, "implement", ""); serr == nil {
			cs.Children[i].ImplementStageState = stage.State
			// The failure category and the stage itself come from this SAME read
			// (#4178), so the child_failed arm's next_step targets exactly the
			// stage its release predicate saw.
			if stage.FailureCategory != nil {
				cs.Children[i].ImplementFailureCategory = *stage.FailureCategory
			}
			if cs.implementStages == nil {
				cs.implementStages = make(map[string]Stage, len(cs.Children))
			}
			cs.implementStages[cs.Children[i].RunID] = stage
		}
	}
	return cs, nil
}

// fanInChildrenStatus is the FRESH decomposed-parent snapshot (#4080) shared by
// fishhawk_await_children and the next_actions acceptance hold. It probes
// LatestPlanDecomposed FIRST and returns (nil, nil) for a non-decomposed run
// BEFORE any category-paginated audit walk (approval condition C1), so an
// ordinary run pays one plan_decomposed read and zero fan-in reads. For a
// decomposed parent it walks each fan-in category to its last page
// (latestFanInAudit) and classifies off that, never off the bounded recent
// window, where an aged-out slices_integrated would mis-classify the parent.
func (r *runResolver) fanInChildrenStatus(ctx context.Context, parentUUID uuid.UUID) (*ChildrenStatus, error) {
	pd, err := r.api.LatestPlanDecomposed(ctx, parentUUID)
	if err != nil {
		return nil, err
	}
	if pd == nil {
		return nil, nil
	}
	entries, err := r.latestFanInAudit(ctx, parentUUID)
	if err != nil {
		// latestFanInAudit already wraps as "read parent audit: …" — the
		// UNCHANGED contract awaitChildrenEvaluate reads, so a read failure
		// (including cap exhaustion / a non-progressing cursor) surfaces as an
		// error rather than a silently stale snapshot.
		return nil, err
	}
	return r.childrenStatusFromDecomposition(ctx, pd, entries), nil
}

// awaitChildrenAuditLimit is the PER-PAGE size for the category-filtered fan-in
// reads (#2695). It takes the endpoint's per-request cap of 500. A SINGLE window
// no longer suffices and the old comment claiming it did was the bug: the fan-in
// kinds (fanInCategories) land LATE in a
// decomposed parent's audit, so on a parent with a longer history the newest
// marker sits beyond the first 500-entry page — an unfiltered single read then
// silently omits it and every await times out. latestFanInAudit instead
// category-filters each kind and walks it to its LAST page (the endpoint returns
// entries ASCENDING, so the max-Sequence entry childrenStatusFor keeps is always
// on the last page).
const awaitChildrenAuditLimit = 500

// awaitChildrenAuditMaxPages bounds the per-category pagination walk so a
// pathological or looping cursor cannot spin forever (binding condition 1). At
// 500 entries/page this admits a fan-in history far larger than any real
// decomposition — a parent emits at most one fan-in entry per wave — so a walk
// that reaches it is either a genuinely pathological history or a broken cursor;
// either way exhaustion is a READ FAILURE, never a silently-truncated snapshot.
const awaitChildrenAuditMaxPages = 256

// fanInCategories is the EXACTLY-FOUR audit categories childrenStatusFor consumes
// from the window it is handed (children_status.go switches on only these four
// and keeps the max-Sequence entry per kind): the clean slices_integrated plus
// the three fan-in FAILURE kinds — slice_integration_conflict,
// slice_head_missing (#4079) and slice_integration_failed (#1243) — added in
// #4080 so a failure newer than the newest clean integration releases
// integration_failed instead of hiding behind it. Feeding it category-filtered
// pages of just these is semantically identical to an unfiltered window on a
// short history and strictly correct on a long one; the cost is one bounded
// paginated read per kind per poll.
var fanInCategories = []string{
	auditCategorySlicesIntegrated,
	auditCategorySliceIntegrationConflict,
	auditCategorySliceHeadMissing,
	auditCategorySliceIntegrationFailed,
}

// latestFanInAudit assembles the fan-in markers for the await snapshot by
// paginating EACH fan-in category to its last page and concatenating those final
// pages (#2695). It replaces childrenStatusForAwait's old single unfiltered
// 500-entry read, which dropped the newest marker on any parent with a longer
// audit history and timed out every await. A read error is wrapped as
// "read parent audit: …" — the UNCHANGED contract awaitChildrenEvaluate reads.
func (r *runResolver) latestFanInAudit(ctx context.Context, parentUUID uuid.UUID) ([]AuditEntry, error) {
	var out []AuditEntry
	for _, cat := range fanInCategories {
		page, err := r.lastFanInAuditPage(ctx, parentUUID, cat)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
	}
	return out, nil
}

// lastFanInAuditPage walks one category's audit pages to the LAST page, mirroring
// verify_run.go's cursor loop, and returns that final page's entries. Because the
// endpoint returns entries sequence-ASCENDING, the newest entry of the kind — the
// only one childrenStatusFor keeps — is on the last page.
//
// Two DISTINCT terminating faults each surface as their OWN wrapped
// "read parent audit" error (binding condition 1), so awaitChildrenEvaluate sees a
// read FAILURE rather than a silently stale last-fetched page — and the diagnosis
// is not collapsed:
//
//   - CAP EXHAUSTION — the walk reached awaitChildrenAuditMaxPages without the
//     endpoint reporting the end. A legitimately long (or pathological) history:
//     failing loud is strictly better than confidently returning a possibly-stale
//     page, which would reintroduce the exact stale-marker miss this change fixes.
//   - NON-PROGRESSING CURSOR — the endpoint handed back the SAME cursor it was
//     given, which would otherwise spin forever. A different fault (a looping or
//     broken cursor, not merely a long history), named as its own cause.
func (r *runResolver) lastFanInAuditPage(ctx context.Context, parentUUID uuid.UUID, category string) ([]AuditEntry, error) {
	cursor := ""
	var last []AuditEntry
	for page := 0; page < awaitChildrenAuditMaxPages; page++ {
		items, next, err := r.api.ListRunAudit(ctx, parentUUID, ListRunAuditFilter{
			Category: category,
			Limit:    awaitChildrenAuditLimit,
			Cursor:   cursor,
		})
		if err != nil {
			return nil, fmt.Errorf("read parent audit: %w", err)
		}
		last = items
		if next == "" {
			return last, nil
		}
		if next == cursor {
			return nil, fmt.Errorf(
				"read parent audit: %s pagination cursor did not advance (stuck at %q) — aborting the walk rather than spinning on a looping cursor", category, next)
		}
		cursor = next
	}
	return nil, fmt.Errorf(
		"read parent audit: %s fan-in history exceeds the %d-page walk cap (%d entries/page) — refusing to use a possibly-stale page, so the fan-in snapshot could not be determined",
		category, awaitChildrenAuditMaxPages, awaitChildrenAuditLimit)
}

// awaitChildrenTimeoutDefault is the omitted-timeout default for
// fishhawk_await_children (#2695 item 2). It reconciles with #2363's approved
// 600s contract: the shared clampAwaitTimeoutHeartbeat delegates a non-positive
// input to the 360s review default, which CONTRADICTS that contract. So
// await_children carries its own 600s default while leaving the cap ladder
// (effectiveAwaitCap: 600s, raised to 7200s under long_wait or a progressToken)
// untouched — and no other await verb is affected.
const awaitChildrenTimeoutDefault = 600

// clampAwaitChildrenTimeout is await_children's timeout clamp. A non-positive
// input returns the 600s default (NOT the shared 360s review default); a positive
// input clamps against the SHARED effectiveAwaitCap, so long_wait / a
// progressToken still unlock the 7200s cap exactly as elsewhere. By construction,
// an omitted timeout with neither opt-in resolves to 600 — equal to the cap — so
// one call waits the full approved window.
func clampAwaitChildrenTimeout(n int, heartbeat, longWait bool) int {
	if n <= 0 {
		return awaitChildrenTimeoutDefault
	}
	capSeconds := effectiveAwaitCap(heartbeat, longWait)
	if n > capSeconds {
		return capSeconds
	}
	return n
}

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
// The predicate is deliberately in two halves. (a) The child's IMPLEMENT STAGE
// awaits a host dispatch — keyed on ImplementStageState via
// implementStageDispatchable, the SAME {pending, awaiting_host_dispatch}
// partition fishhawk_run_children's own dispatch loop uses (#1237). It is NOT
// keyed on the run-level State: a decomposed child parked by RuleChildrenDispatch
// has run state 'running' while its implement stage sits at
// awaiting_host_dispatch, so a run-state predicate would skip the entire
// primary locked-local parked population — announcing nothing until the sweeper
// integrated. A 'dispatched'/'running' stage already has a runner in flight and
// is not re-dispatched (#1912). (b) Its dependency slices are COVERED per
// wavecoverage.Covered — the identical function the sweeper's short-circuit and
// the host-dispatch marker's admission use. Reconstructing (b) from
// ChildStatus.Blocked would be wrong in a way invisible to this package's own
// tests: Blocked keys on predecessor run STATE, which flips to succeeded BEFORE
// the integration runs.
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
		if !implementStageDispatchable(c.ImplementStageState) {
			continue
		}
		if covered, _ := wavecoverage.Covered(c.DependsOn, sliceRunID, cs.IntegratedChildRunIDs); covered {
			out = append(out, c.RunID)
		}
	}
	return out
}

// awaitChildrenNonSucceeded returns the run ids of every child not in run state
// succeeded, in slice order (status children_failed).
func awaitChildrenNonSucceeded(cs *ChildrenStatus) []string {
	var out []string
	for _, c := range childrenInSliceOrder(cs.Children) {
		if c.State != "succeeded" {
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

// awaitChildrenFailureBlocked is the child_failed release predicate (#4178).
// Pure — no I/O — so every branch is unit-testable like
// awaitChildrenDispatchable.
//
// F is every child whose run state is terminal and not succeeded (failed or
// cancelled — the same non-succeeded terminal set awaitChildrenNonSucceeded and
// the childcompletion sweeper use). N is every child whose run state is NOT
// terminal, "unknown" included. It returns (F, N), each in ascending slice
// order, ONLY when F and N are both non-empty AND every child in N is
//
//   - undispatched: implementStageDispatchable(ImplementStageState), the same
//     {pending, awaiting_host_dispatch} partition fishhawk_run_children uses. A
//     dispatched/running stage is progress, and an unresolved "" stage is
//     unknown — neither releases; and
//   - failure-blocked: some DependsOn slice resolves BY SLICE INDEX to a minted
//     sibling in F, or to a non-terminal sibling that is itself failure-blocked
//     (transitive).
//
// Otherwise it returns (nil, nil). Every uncertain case fails CLOSED to the
// existing wait: a child whose GetRun failed (State "unknown") counts as NOT
// failure-blocked whatever its readable implement stage says (approval
// condition C3 — the next poll re-reads it); a not-minted dependency
// contributes false; a dependency cycle resolves false. The len(N) > 0
// requirement keeps the all-terminal arm (4) unchanged: an all-terminal
// fan-out never releases here.
func awaitChildrenFailureBlocked(cs *ChildrenStatus) (failed, blocked []string) {
	ordered := childrenInSliceOrder(cs.Children)
	bySlice := make(map[int]ChildStatus, len(ordered))
	for _, c := range ordered {
		bySlice[c.SliceIndex] = c
	}
	isFailed := func(c ChildStatus) bool {
		return runStateIsTerminal(c.State) && c.State != "succeeded"
	}
	for _, c := range ordered {
		if isFailed(c) {
			failed = append(failed, c.RunID)
		} else if !runStateIsTerminal(c.State) {
			blocked = append(blocked, c.RunID)
		}
	}
	if len(failed) == 0 || len(blocked) == 0 {
		return nil, nil
	}

	// Memoized DFS over the slice depends_on graph. A slice still being
	// visited resolves false, so a cycle fails closed.
	const (
		visiting = iota + 1
		resolvedTrue
		resolvedFalse
	)
	memo := make(map[int]int, len(ordered))
	var blockedByFailure func(c ChildStatus) bool
	blockedByFailure = func(c ChildStatus) bool {
		if c.State == "unknown" {
			return false
		}
		switch memo[c.SliceIndex] {
		case visiting, resolvedFalse:
			return false
		case resolvedTrue:
			return true
		}
		memo[c.SliceIndex] = visiting
		result := false
		for _, depIdx := range c.DependsOn {
			dep, minted := bySlice[depIdx]
			if !minted {
				continue
			}
			if isFailed(dep) || (!runStateIsTerminal(dep.State) && blockedByFailure(dep)) {
				result = true
				break
			}
		}
		if result {
			memo[c.SliceIndex] = resolvedTrue
		} else {
			memo[c.SliceIndex] = resolvedFalse
		}
		return result
	}

	for _, c := range ordered {
		if runStateIsTerminal(c.State) {
			continue
		}
		if !implementStageDispatchable(c.ImplementStageState) || !blockedByFailure(c) {
			return nil, nil
		}
	}
	return failed, blocked
}

// awaitChildFailedAcceptedStates is the allow-list of implementFailedNextActions
// states whose first action the child_failed arm may surface verbatim
// (approval condition C1): an in-place retry (A, and the C/D default arm) or
// the decomposition child's in-place resume (B). Every other state —
// slices_integration_conflict (a field-path-pointer param meant for a PARENT),
// implement_failed_category_b_decomposed_parent (a fishhawk_start_run restart),
// or any arm added later — falls back to fishhawk_get_run_status on the child.
var awaitChildFailedAcceptedStates = map[string]bool{
	"implement_failed_category_a":                     true,
	"implement_failed":                                true,
	"implement_failed_category_b_decomposition_child": true,
}

// awaitChildrenChildFailedNextStep derives the child_failed next_step for the
// lowest-slice failed child from the implement stage read the release
// predicate was decided on (ChildrenStatus.implementStages).
//
//   - A CANCELLED child run cannot be retried, so it NEVER maps to
//     fishhawk_retry_stage or fishhawk_fixup_stage (approval condition C2): it
//     gets fishhawk_get_run_status with a reason saying the parent must be
//     re-planned or the item restarted.
//   - A failed implement stage is routed through the existing
//     implementFailedNextActions table, with a Run built from the snapshot's
//     own facts: membership in the parent's plan_decomposed IS the
//     decomposition-child fact that table's category-B arm detects (a
//     parent_run_id and no plan/review stage), so B maps to the in-place
//     fishhawk_resume_run {parent_run_id: <child>}. Its first action is taken
//     ONLY for an awaitChildFailedAcceptedStates state (C1).
//   - Anything else (stage unreadable, not in state failed, or a state off the
//     allow-list) falls back to fishhawk_get_run_status on the child, whose
//     next_actions own the recovery.
func awaitChildrenChildFailedNextStep(parentUUID uuid.UUID, cs *ChildrenStatus, child ChildStatus) *SuggestedAction {
	if child.State == "cancelled" {
		return &SuggestedAction{
			Action:       "fishhawk_get_run_status",
			Params:       map[string]string{"run_id": child.RunID},
			Precondition: "this decomposed child's run was cancelled while its dependents are parked behind it",
			Consumes:     "none",
			Reason:       "the child run was cancelled, so fishhawk_retry_stage and fishhawk_fixup_stage cannot apply to it; its dependents can never dispatch, so the parent must be re-planned or the item restarted",
		}
	}
	if stage, ok := cs.implementStages[child.RunID]; ok && stage.State == "failed" {
		parentID := parentUUID.String()
		na := implementFailedNextActions(&Run{ID: child.RunID, ParentRunID: &parentID}, nil, nil, &stage)
		if na != nil && len(na.Actions) > 0 && awaitChildFailedAcceptedStates[na.State] {
			action := na.Actions[0]
			return &action
		}
	}
	return &SuggestedAction{
		Action:       "fishhawk_get_run_status",
		Params:       map[string]string{"run_id": child.RunID},
		Precondition: "this decomposed child is terminal-failed and its implement stage's recovery could not be derived from the snapshot (stage unreadable, not failed, or a failure shape this wait does not route)",
		Consumes:     "none",
		Reason:       "the failed child's next_actions own its recovery by failure category; its dependents cannot dispatch until it recovers",
	}
}

// awaitChildrenChildFailedOutput builds the child_failed release (#4178).
// failed and blocked come from awaitChildrenFailureBlocked, in slice order.
func awaitChildrenChildFailedOutput(base AwaitChildrenOutput, parentUUID uuid.UUID, cs *ChildrenStatus, failed, blocked []string) AwaitChildrenOutput {
	byRunID := make(map[string]ChildStatus, len(cs.Children))
	for _, c := range cs.Children {
		byRunID[c.RunID] = c
	}
	out := base
	out.Status = "child_failed"
	out.FailedChildRunIDs = failed
	out.BlockedChildRunIDs = blocked
	out.NextStep = awaitChildrenChildFailedNextStep(parentUUID, cs, byRunID[failed[0]])

	described := make([]string, 0, len(failed))
	for _, id := range failed {
		c := byRunID[id]
		// failureClass is the run failure class (A–D), not an audit category;
		// the name keeps the audit-category registry sweep from reading this
		// placeholder literal as an emitted category.
		failureClass := c.ImplementFailureCategory
		if failureClass == "" {
			failureClass = "unknown"
		}
		described = append(described, fmt.Sprintf("%s (slice %d, run %s, implement failure category %s)", id, c.SliceIndex, c.State, failureClass))
	}
	out.Message = fmt.Sprintf(
		"%d child(ren) failed: %s. Every remaining child (%s) is undispatched and depends, directly or transitively, on a failed slice, so none can ever dispatch until the failed slice recovers — waiting longer only runs out the timeout. "+
			"Do NOT cancel the dependents, and do NOT consolidate, approve the review or dispatch acceptance. "+
			"Apply next_step to recover %s, then re-invoke fishhawk_await_children (a retried local slice parks at awaiting_host_dispatch, which then releases children_dispatchable).",
		len(failed), strings.Join(described, "; "), strings.Join(blocked, ", "), failed[0])
	return out
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

// awaitChildrenTimeout builds the resumable timeout response, best-effort
// enriched with the current ChildrenStatus snapshot so a timeout release carries
// the same per-child block EVERY other release does (the contract requires it on
// every release, timeout included). The snapshot read uses the caller's ctx —
// NOT the expired poll ctx — and degrades to no snapshot on a read failure
// rather than turning a resumable timeout into an error. The re-arm NextStep is
// attached unconditionally by awaitChildrenTimeoutOutput.
func (r *runResolver) awaitChildrenTimeout(ctx context.Context, parentUUID uuid.UUID, timeout int, start time.Time, heartbeat bool, capSeconds int) AwaitChildrenOutput {
	out := awaitChildrenTimeoutOutput(parentUUID.String(), timeout, start, heartbeat, capSeconds)
	if cs, err := r.childrenStatusForAwait(ctx, parentUUID); err == nil && cs != nil {
		out.Children = cs
	}
	return out
}

// awaitChildrenTimeoutOutput builds the resumable timeout response. The wait
// holds no server state, so a timeout is an idempotent checkpoint, not an error.
// It carries a re-arm NextStep (fishhawk_await_children) so the contract's
// "every release returns a next_step" holds on the timeout path too — a timeout
// is a checkpoint to resume, not a terminal state.
func awaitChildrenTimeoutOutput(runID string, timeout int, start time.Time, heartbeat bool, capSeconds int) AwaitChildrenOutput {
	return AwaitChildrenOutput{
		Status:              "timeout",
		RunID:               runID,
		WaitedSeconds:       time.Since(start).Seconds(),
		PollIntervalSeconds: suggestedStageWaitPollIntervalSeconds,
		Heartbeat:           heartbeat,
		TimeoutCapSeconds:   capSeconds,
		NextStep: &SuggestedAction{
			Action:       "fishhawk_await_children",
			Params:       map[string]string{"run_id": runID},
			Precondition: "the wait timed out with no release; it holds no server state",
			Consumes:     "none",
			Reason:       "re-arm the in-band wait — a timeout is a resumable idempotent checkpoint, not a terminal state",
		},
		Message: fmt.Sprintf("no child of run %s filed an amendment, became dispatchable, hit a fan-in failure, had a failed child blocking every remaining child, or settled within %ds. "+
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
			"scope amendment (decidable IN BAND), another child becomes dispatchable, the fan-in fails, a failed child blocks every remaining child, or every child settles",
	}
}
