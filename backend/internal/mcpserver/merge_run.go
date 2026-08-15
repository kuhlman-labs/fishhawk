package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mergeRunCategories are the audit categories the tool's post-POST poll
// resolves on: the backend lifecycle signals that the run's PR merge has
// settled. pr_merged lands when the merge webhook resolves; the
// post_merge_observed backstop covers the lifecycle post-merge tail
// (#1370). There is NO persisted 'merged' run state — terminal-on-merge is
// StateSucceeded — so the await keys on these categories plus the
// run-terminal backstop, never on a state string.
var mergeRunCategories = []string{"pr_merged", "post_merge_observed"}

// MergeRunInput is the fishhawk_merge_run tool's input schema (E48.7 /
// #1954). run_id + verdict are required; timeout_seconds bounds the
// terminal await (clampAwaitTimeout: default 360, cap 600).
type MergeRunInput struct {
	RunID   string `json:"run_id" jsonschema:"the Fishhawk run UUID whose gate-approved PR to merge; resolved like the other run-keyed verbs"`
	Verdict string `json:"verdict" jsonschema:"required operator merge verdict — recorded verbatim on the chained merge_verdict_recorded audit entry as the audited decision to ship"`
	// TimeoutSeconds bounds the post-POST terminal await. The wait holds no
	// server state, so a timeout is a resumable checkpoint (re-invoke to
	// resume — the endpoint's idempotence means the re-POST records no
	// duplicate verdict row).
	TimeoutSeconds int `json:"timeout_seconds,omitempty" jsonschema:"how long to await the terminal merge (default 360, capped at 600). On timeout the tool returns status=timeout (resumable) — re-invoke to resume; the endpoint is idempotent so the re-POST records no duplicate verdict row"`
}

// MergeRunOutput is the fishhawk_merge_run response. Status is one of:
//
//   - "merged"        — a pr_merged / post_merge_observed entry landed past
//     the verdict anchor: the PR is merged and the run resolved.
//   - "timeout"       — nothing landed within the window. Resumable: re-invoke
//     the tool to resume (the endpoint is idempotent, so the re-POST records
//     no duplicate verdict row and re-dispatches the queued merge).
//   - "run_terminal"  — the run reached failed/cancelled while the wait was
//     pending (ADR-036 backstop) — the merge will most likely never settle.
//   - "checks_pending" — the pull request's required checks have not all passed
//     (GitHub reports it in unstable status), so GitHub would not queue the
//     merge within the wall-clock budget (E67.56 / #2717). The verdict row is
//     recorded and durable; the merge itself is NOT queued (merge_queued:false).
//     Resumable: re-invoke once the checks complete — but if a required check
//     has already FAILED the merge will never queue, so inspect the PR instead
//     of waiting. See the per-field notes below for what the flags mean here.
type MergeRunOutput struct {
	Status string `json:"status" jsonschema:"one of merged, timeout, run_terminal, checks_pending"`
	// RunState is the run's lifecycle state at resolution (succeeded on a
	// settled merge; failed/cancelled on the run_terminal backstop).
	RunState string `json:"run_state,omitempty" jsonschema:"the run's lifecycle state at resolution"`
	// MergeQueued mirrors the endpoint's merge_queued: the merge was
	// dispatched through the shared GitHubMerger seam. FALSE on checks_pending —
	// GitHub refused to queue the merge because the required checks have not all
	// passed, so nothing is queued yet.
	MergeQueued bool `json:"merge_queued" jsonschema:"true when the endpoint dispatched the squash merge through the shared merger seam; false on checks_pending (GitHub would not queue it — the required checks have not all passed)"`
	// VerdictRecorded is true when THIS call appended the merge_verdict_recorded
	// row; AlreadyRecorded is true when the endpoint found an existing row and
	// skipped the duplicate append (still re-dispatching the merge). Exactly one
	// is true per SUCCESSFUL POST. On checks_pending BOTH are false: the merge
	// was never queued, and the response does not positively distinguish a
	// pre-existing verdict row from one this invocation created (E67.56 / #2717
	// binding condition 4), so provenance is not inferred — the Message carries
	// the durability claim instead.
	VerdictRecorded bool    `json:"verdict_recorded" jsonschema:"true when this call appended the chained merge_verdict_recorded audit entry; false on checks_pending (provenance is not inferred there)"`
	AlreadyRecorded bool    `json:"already_recorded" jsonschema:"true when the endpoint found an existing merge_verdict_recorded row (idempotent resume) and re-dispatched the merge without a duplicate append; false on checks_pending"`
	VerdictSequence int64   `json:"verdict_sequence,omitempty" jsonschema:"the merge_verdict_recorded row's audit sequence — the anchor the terminal await polls past; on checks_pending it is the durable verdict row's sequence when the server surfaced it"`
	PRURL           string  `json:"pr_url,omitempty" jsonschema:"the merged pull request URL"`
	WaitedSeconds   float64 `json:"waited_seconds" jsonschema:"elapsed wall time spent awaiting the terminal merge"`
	// NextAction surfaces the operator post-merge dev-host step (the reused
	// postMergeStep) on status=merged. Per ADR-038 the MCP surface never
	// mutates the host, so this is SURFACED, not invoked.
	NextAction *SuggestedAction `json:"next_action,omitempty" jsonschema:"on status=merged, the operator post-merge dev-host step (scripts/dev post-merge) — surfaced for you to run, never invoked by the tool (ADR-038)"`
	Message    string           `json:"message,omitempty" jsonschema:"actionable explanation on the timeout / run_terminal / checks_pending statuses"`
	// Note restates the split-identity contract: the PR-approval review stays a
	// gh step under the operator's OWN GitHub identity (option a, App-identity
	// approval deferred to E39). Queueing the merge before that approval is
	// safe — GitHub fires the merge once branch protection is satisfied.
	Note string `json:"note" jsonschema:"the PR-approval review stays a gh step under your own GitHub identity; queueing the merge before approval is safe — GitHub fires it once branch protection is satisfied"`
}

// mergeRunNote is the split-identity reminder surfaced on every response.
const mergeRunNote = "The PR-approval review (gh pr review --approve) stays a step under your own GitHub identity — App-identity approval is deferred to E39. Queueing the merge before approval is safe: GitHub fires the squash merge once branch protection (required review + the fishhawk_audit_complete check) is satisfied."

// registerMergeRun wires the fishhawk_merge_run tool (E48.7 / #1954): the
// one operator verb that takes a gate-approved run from verdict to
// merged+terminal, replacing the bare merge_pr + post_merge hand ceremony.
//
// Auth: operator-only write tool — the backend requires write:approvals and
// rejects a run-bound agent token (403 run_token_forbidden).
func registerMergeRun(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_merge_run",
		Description: strings.TrimSpace(`
Use this AFTER a run's PR gate is settled and you have approved the PR (gh
pr review --approve under your own identity): fishhawk_merge_run records
your operator merge verdict, queues the squash merge through the same
GitHubMerger seam the delegated drive_run may_merge arm uses, awaits the
webhook-settled terminal run state, and surfaces the operator post-merge
dev-host step (E48.7 / #1954). It replaces the four-step hand ceremony
(approve → merge → post-merge) with one verb.

Records a chained merge_verdict_recorded audit entry (your verdict verbatim)
and dispatches the merge. The endpoint is IDEMPOTENT: a repeated POST finds
the existing verdict row, appends no duplicate (already_recorded:true), and
STILL re-dispatches the merge — so a timed-out re-invoke or a 502 retry
re-queues the merge with no duplicate verdict row. The tool always re-POSTs
on resume with NO client-side skip.

The PR-approval review itself STAYS a gh step under your own GitHub identity
(App-identity approval is deferred to E39); queueing the merge before that
approval is safe — GitHub fires the merge once branch protection (required
review + the fishhawk_audit_complete check) is satisfied.

There is no persisted 'merged' run state — terminal-on-merge is succeeded —
so the await keys on the pr_merged / post_merge_observed audit categories
plus the ADR-036 run-terminal backstop, never on a state string.

Statuses:
  - "merged"        — the merge settled; next_action carries the operator
                     post-merge dev-host step (surfaced, not invoked —
                     ADR-038 keeps host mutation out of the MCP surface).
  - "timeout"       — nothing settled within the window; resumable — re-invoke
                     to resume (idempotent, no duplicate verdict row).
  - "run_terminal"  — the run reached failed/cancelled while waiting; the
                     merge will most likely never settle — check
                     fishhawk_get_run_status.
  - "checks_pending" — the pull request's required checks have not all passed
                     (GitHub reports it in unstable status), so GitHub would not
                     queue the merge within the wall-clock budget. The verdict is
                     recorded and durable but the merge is NOT queued. Resumable:
                     re-invoke once the checks complete — but if a required check
                     has already FAILED the merge will never queue, so inspect the
                     PR instead of waiting. This is a STATUS, not a tool error: an
                     immediate re-POST cannot succeed, so the tool waits within its
                     timeout budget rather than surfacing a remedy that would fail.

Inputs:
  - run_id          (required) — the gate-approved run's UUID; it must carry
                    a PR URL and must not be failed/cancelled (the backend
                    re-validates authoritatively, including the acceptance
                    gate).
  - verdict         (required) — your operator merge verdict, recorded
                    verbatim on the audit entry.
  - timeout_seconds — default 360, capped at 600.

Tool errors:
  - invalid UUID (caught before the HTTP hop)
  - the run has no PR URL, or the run is failed/cancelled (fast local
    refusal before the POST)
  - the backend's authoritative surfaces: validation_failed (400),
    run_token_forbidden / insufficient_scope (403), run_not_found (404),
    run_not_mergeable / acceptance_gate_not_passed (409),
    merge_dispatch_failed (502 — the verdict row is durable and the merge is
    retryable; re-invoke), merge_unconfigured (503)

The backend's 409 merge_checks_pending is NOT surfaced as a tool error: it is
the checks-not-all-passed precondition the tool WAITS on (re-POSTing on the poll
tick within the shared timeout budget), returning status=checks_pending on
budget exhaustion rather than an error whose remedy is guaranteed to fail.
`),
	}, resolver.mergeRun)
}

// isChecksPending reports whether err is the backend's 409 merge_checks_pending
// classification (E67.56 / #2717) — the checks-not-all-passed precondition the
// tool WAITS on rather than surfacing as an error. Returns the *apiError so the
// caller can read details.verdict_sequence.
func isChecksPending(err error) (*apiError, bool) {
	var ae *apiError
	if errors.As(err, &ae) && ae.Code == "merge_checks_pending" {
		return ae, true
	}
	return nil, false
}

// mergeVerdictSequenceFrom best-effort reads details.verdict_sequence (the audit
// sequence of the durable merge_verdict_recorded row) from a merge_checks_pending
// error's details. Returns 0 when absent or untyped. It is REPORTED in
// VerdictSequence but is NOT treated as provenance evidence (binding condition
// 4): the sequence's presence does not distinguish a pre-existing row from one
// this invocation created.
func mergeVerdictSequenceFrom(details map[string]any) int64 {
	if details == nil {
		return 0
	}
	switch n := details["verdict_sequence"].(type) {
	case float64:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	case int64:
		return n
	}
	return 0
}

// checksPendingOutput builds the resumable checks_pending checkpoint returned
// when the shared deadline expires while GitHub still refuses to queue the merge
// (E67.56 / #2717). Per binding condition 4 it does NOT infer provenance the
// response cannot establish: VerdictRecorded and AlreadyRecorded are BOTH false
// (details.verdict_sequence is not evidence of which invocation created the row),
// and MergeQueued is false (nothing was queued). VerdictSequence carries the
// sequence when the server surfaced it, and the Message carries the durability
// claim plus the honest wording condition 1 requires — the checks have not all
// passed, an immediate retry cannot succeed, and a check that has already FAILED
// means inspecting the PR rather than waiting.
func checksPendingOutput(seq int64, start time.Time) MergeRunOutput {
	return MergeRunOutput{
		Status:          "checks_pending",
		MergeQueued:     false,
		VerdictRecorded: false,
		AlreadyRecorded: false,
		VerdictSequence: seq,
		WaitedSeconds:   time.Since(start).Seconds(),
		Note:            mergeRunNote,
		Message:         "the merge verdict is recorded and durable, but GitHub will not queue the squash merge because the pull request's required checks have not all passed (GitHub reports the pull request in unstable status). An immediate retry cannot succeed. If the checks are still pending, re-invoke fishhawk_merge_run once they complete; if a required check has already FAILED, the merge will never queue — inspect the pull request rather than waiting.",
	}
}

// mergeRun is the tool handler. It validates locally, refuses fast when the run
// cannot merge, ALWAYS re-POSTs the verdict (endpoint-side idempotence per #1954
// binding condition 1 — no client-side skip), WAITS through a checks-not-all-passed
// (409 merge_checks_pending) refusal within a single shared deadline (E67.56 /
// #2717) rather than surfacing it as an error whose remedy would fail, then
// awaits the terminal merge with the remaining budget via the await_audit poll
// idiom.
func (r *runResolver) mergeRun(ctx context.Context, _ *mcp.CallToolRequest, in MergeRunInput) (*mcp.CallToolResult, MergeRunOutput, error) {
	runID, err := uuid.Parse(in.RunID)
	if err != nil {
		return nil, MergeRunOutput{}, fmt.Errorf("run_id %q is not a valid UUID: %w", in.RunID, err)
	}
	verdict := strings.TrimSpace(in.Verdict)
	if verdict == "" {
		return nil, MergeRunOutput{}, fmt.Errorf("verdict is required: the merge records an audited operator decision to ship")
	}

	// Pre-flight fast refusal (the backend re-validates authoritatively). A
	// run with no PR to merge, or one already failed/cancelled, can never
	// merge — refuse before the POST so the operator gets a clear local error
	// rather than a 409 round-trip.
	run, err := r.api.GetRun(ctx, runID)
	if err != nil {
		return nil, MergeRunOutput{}, fmt.Errorf("get run: %w", err)
	}
	if run.PullRequestURL == nil || *run.PullRequestURL == "" {
		return nil, MergeRunOutput{}, fmt.Errorf("run %s has no pull request URL — there is nothing to merge (dispatch and review the implement stage first)", runID)
	}
	if run.State == "failed" || run.State == "cancelled" {
		return nil, MergeRunOutput{}, fmt.Errorf("run %s is %s — a terminal-failed run cannot be merged; recover or start a fresh run", runID, run.State)
	}

	// One shared deadline bounds BOTH the checks-pending retry loop and the
	// terminal await (E67.56 / #2717), so the tool's overall wall-clock contract
	// is unchanged: a merge held back by pending checks cannot make the tool wait
	// longer than it does today.
	timeout := clampAwaitTimeout(in.TimeoutSeconds)
	start := time.Now()
	deadline := start.Add(time.Duration(timeout) * time.Second)

	interval := r.reviewPollInterval
	if interval <= 0 {
		interval = defaultReviewPollInterval
	}

	// Record the verdict + queue the merge. ALWAYS POST on resume (no
	// client-side skip): the endpoint is idempotent (#1954 condition 1), so a
	// re-invoke records no duplicate verdict row and still re-dispatches the
	// merge. A 409 merge_checks_pending is NOT an error — GitHub reports the PR's
	// required checks have not all passed and will not queue the merge, so the
	// tool re-POSTs on the poll tick until the endpoint accepts the merge or the
	// shared deadline expires (then returns the resumable checks_pending status).
	// A 502 (merge_dispatch_failed) and every OTHER error surface here verbatim —
	// the verdict row is durable, so a re-invoke re-queues the merge.
	var res *MergeRunResult
	var checksSeq int64
	for {
		var merr error
		res, merr = r.api.MergeRun(ctx, runID, verdict)
		if merr == nil {
			break
		}
		ae, pending := isChecksPending(merr)
		if !pending {
			// A ctx cancellation mid-retry AFTER a checks-pending hit is the
			// bounded wait expiring, not a genuine dispatch failure — return the
			// resumable checkpoint rather than a spurious tool error.
			if checksSeq != 0 && ctx.Err() != nil {
				return nil, checksPendingOutput(checksSeq, start), nil
			}
			return nil, MergeRunOutput{}, fmt.Errorf("merge run: %w", merr)
		}
		if seq := mergeVerdictSequenceFrom(ae.Details); seq != 0 {
			checksSeq = seq
		}
		// Wait one poll tick, bounded by the shared deadline and ctx. On
		// exhaustion return the resumable checks_pending checkpoint.
		if !time.Now().Before(deadline) {
			return nil, checksPendingOutput(checksSeq, start), nil
		}
		wait := interval
		if d := time.Until(deadline); d < wait {
			wait = d
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, checksPendingOutput(checksSeq, start), nil
		case <-timer.C:
		}
	}

	out := MergeRunOutput{
		MergeQueued:     res.MergeQueued,
		VerdictRecorded: !res.AlreadyRecorded,
		AlreadyRecorded: res.AlreadyRecorded,
		VerdictSequence: res.VerdictSequence,
		PRURL:           res.PRURL,
		Note:            mergeRunNote,
	}

	// Await the terminal merge with the REMAINING budget (the shared deadline),
	// so the checks-pending retry loop + the terminal await never exceed the one
	// clamped timeout. Anchored past the verdict row so a stale pr_merged from an
	// earlier attempt cannot resolve the wait.
	remaining := int(time.Until(deadline).Seconds())
	if remaining < 0 {
		remaining = 0
	}
	status, runState, waited := r.awaitMergeTerminal(ctx, runID, res.VerdictSequence, remaining, start)
	out.Status = status
	out.RunState = runState
	out.WaitedSeconds = waited

	switch status {
	case "merged":
		step := postMergeStep(run)
		out.NextAction = &step
	case "timeout":
		out.Message = fmt.Sprintf("no pr_merged / post_merge_observed entry landed within %ds. The merge is queued; re-invoke fishhawk_merge_run to resume the wait (the endpoint is idempotent — the re-POST records no duplicate verdict row), or poll fishhawk_get_run_status.", clampAwaitTimeout(in.TimeoutSeconds))
	case "run_terminal":
		out.Message = fmt.Sprintf("run %s reached terminal state %q while awaiting the merge and no pr_merged / post_merge_observed entry landed — the merge will most likely never settle. Check fishhawk_get_run_status before re-invoking.", runID, runState)
	}
	return nil, out, nil
}

// awaitMergeTerminal polls the run audit for the first pr_merged /
// post_merge_observed entry past the verdict anchor, mirroring the
// fishhawk_await_audit idiom (a since-anchored fast read, then a poll on the
// injectable reviewPollInterval under a clamped deadline, with the ADR-036
// run-terminal backstop checked once before the loop and on each still-empty
// tick). Returns (status, runState, waitedSeconds) where status is one of
// merged / timeout / run_terminal.
func (r *runResolver) awaitMergeTerminal(ctx context.Context, runID uuid.UUID, sinceSeq int64, timeout int, start time.Time) (string, string, float64) {
	// Fast path: the merge may already have settled (a resume after the
	// webhook landed).
	if entry, err := r.nextAuditEntry(ctx, runID, mergeRunCategories, sinceSeq, false); err == nil && entry != nil {
		return "merged", r.mergeRunState(ctx, runID), time.Since(start).Seconds()
	}

	// Nothing yet: the ADR-036 run-terminal backstop resolves a run that is
	// already terminal at call time before a poll tick.
	if status, runState, done := r.mergeTerminalBackstop(ctx, runID, sinceSeq); done {
		return status, runState, time.Since(start).Seconds()
	}

	interval := r.reviewPollInterval
	if interval <= 0 {
		interval = defaultReviewPollInterval
	}
	pollCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-pollCtx.Done():
			return "timeout", "", time.Since(start).Seconds()
		case <-ticker.C:
			entry, err := r.nextAuditEntry(pollCtx, runID, mergeRunCategories, sinceSeq, false)
			if err != nil {
				// A deadline hit mid-poll cancels the in-flight request; that is
				// a timeout, not a transport failure.
				if pollCtx.Err() != nil {
					return "timeout", "", time.Since(start).Seconds()
				}
				// A transient transport error is not fatal — keep polling to the
				// bounded deadline rather than aborting the merge await.
				continue
			}
			if entry != nil {
				return "merged", r.mergeRunState(pollCtx, runID), time.Since(start).Seconds()
			}
			if status, runState, done := r.mergeTerminalBackstop(pollCtx, runID, sinceSeq); done {
				return status, runState, time.Since(start).Seconds()
			}
		}
	}
}

// mergeTerminalBackstop resolves the await ONLY when the run has reached a
// FAILED / CANCELLED terminal state while the merge entry is still pending
// (ADR-036) — states past which the queued merge will most likely never
// settle. It deliberately does NOT arm on 'succeeded': feature_change is
// terminal-on-succeeded, so a succeeded_pr_open run whose squash merge is
// still queued (no pr_merged / post_merge_observed entry yet) is the NORMAL
// happy path, and treating it as run_terminal would abandon the await the
// instant it started rather than letting the merge settle (or time out
// resumably). It does ONE final since-anchored read first — a pr_merged /
// post_merge_observed that landed at/after the terminal transition still
// resolves as merged and wins over the backstop. A failed/cancelled run with
// no such entry resolves run_terminal. Best-effort: a GetRun error or a
// non-failed/cancelled run keeps the poll/timeout path in charge.
func (r *runResolver) mergeTerminalBackstop(ctx context.Context, runID uuid.UUID, sinceSeq int64) (string, string, bool) {
	run, err := r.api.GetRun(ctx, runID)
	if err != nil || run == nil {
		return "", "", false
	}
	// Only failed / cancelled arm the backstop. 'succeeded' is the expected
	// terminal-on-merge state, not a stranded-merge signal (see the concern
	// #1954 fix-up: a normal succeeded run must await its queued merge).
	if run.State != "failed" && run.State != "cancelled" {
		return "", "", false
	}
	// Final read: an entry that landed at/after the terminal transition still
	// resolves as merged.
	if entry, rerr := r.nextAuditEntry(ctx, runID, mergeRunCategories, sinceSeq, false); rerr == nil && entry != nil {
		return "merged", run.State, true
	}
	return "run_terminal", run.State, true
}

// mergeRunState reads the run's current lifecycle state for the resolved
// output. Best-effort: a read error yields "" rather than failing the
// already-resolved merge await.
func (r *runResolver) mergeRunState(ctx context.Context, runID uuid.UUID) string {
	run, err := r.api.GetRun(ctx, runID)
	if err != nil || run == nil {
		return ""
	}
	return run.State
}
