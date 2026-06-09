package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ReviewStatus is the lifecycle summary the MCP surface derives from the
// review audit trail for one stage (#600, #664). Status is one of:
//
//   - "none"     — no review was configured (no *_review_started entry).
//   - "pending"  — a review was dispatched (a *_review_started entry exists)
//     but no terminal entry has landed yet. The review is still running.
//     A reviewer that errors or times out now writes a terminal
//     *_review_failed entry (#664), so "pending" no longer subsumes a
//     silent failure — it means genuinely still-in-flight.
//   - "complete" — at least one *_reviewed verdict landed; Reviews carries
//     the decoded verdicts.
//   - "skipped"  — a *_review_skipped entry exists (configured agent layer
//     not wired); Reviews carries the synthesized skipped
//     verdict(s).
//   - "failed"   — a terminal *_review_failed entry exists (#664): the
//     reviewer errored or hit FISHHAWKD_PLAN_REVIEW_TIMEOUT; Reviews
//     carries the synthesized failure reason. A definite terminal state,
//     not a bare 'pending'.
//
// Reviews is populated for the complete + skipped + failed states and empty
// for none + pending.
//
// PollIntervalSeconds is a server-suggested poll cadence (#879): it is
// populated ONLY on the 'pending' status — the one state where a polling
// agent should keep calling fishhawk_get_run_status until a terminal status
// lands — and omitted (zero) on every terminal/none status. Polling
// get_run_status on this cadence is the authoritative way to reach a
// terminal review status; fishhawk_await_review is an optional convenience
// block over the same poll.
type ReviewStatus struct {
	Stage               string       `json:"stage" jsonschema:"the reviewed stage type: 'plan' or 'implement'"`
	Status              string       `json:"status" jsonschema:"one of none, pending, complete, skipped, failed"`
	Reviews             []PlanReview `json:"reviews,omitempty" jsonschema:"decoded verdicts when status=complete; synthesized skipped verdict(s) when status=skipped; synthesized failure reason when status=failed; empty for none/pending"`
	PollIntervalSeconds int          `json:"poll_interval_seconds,omitempty" jsonschema:"server-suggested cadence (seconds) for re-polling fishhawk_get_run_status while status=pending; present only on pending, omitted on terminal/none. Poll get_run_status on this cadence as the authoritative path to a terminal status"`
}

// reviewCategories names the three audit categories that describe a stage's
// review lifecycle. The MCP review_status + await semantics derive entirely
// from these — no workflow-spec read is needed because the started entry is
// the backend-emitted proxy for "agent>0 was configured" (#600).
type reviewCategories struct {
	reviewed string
	skipped  string
	started  string
	failed   string
}

// categoriesForStage maps a stage label to its review audit categories.
// Returns an error for any value other than "plan" / "implement" so a bad
// tool input surfaces a clean error before any backend round-trip.
func categoriesForStage(stage string) (reviewCategories, error) {
	switch stage {
	case "plan":
		return reviewCategories{
			reviewed: "plan_reviewed",
			skipped:  "plan_review_skipped",
			started:  "plan_review_started",
			failed:   "plan_review_failed",
		}, nil
	case "implement":
		return reviewCategories{
			reviewed: "implement_reviewed",
			skipped:  "implement_review_skipped",
			started:  "implement_review_started",
			failed:   "implement_review_failed",
		}, nil
	default:
		return reviewCategories{}, fmt.Errorf("stage %q is not one of plan, implement", stage)
	}
}

// reviewAuditQueryLimit caps how many audit entries the review queries
// pull per category. A handful of agents per stage is the realistic
// ceiling; 50 leaves an order-of-magnitude headroom.
const reviewAuditQueryLimit = 50

// decodeReviewVerdicts queries the given *_reviewed category for the run
// and decodes each payload into a PlanReview (the verdict shape is
// identical across plan and implement review, ADR-027). Entries whose
// payload is absent or malformed are silently skipped — a corrupt payload
// is not a reason to fail the whole fetch. Returns nil when no entries
// exist. Shared by loadPlanReviews, loadImplementReviews, and
// reviewStatusFor so the decode lives in one place.
//
// sinceSeq is a fix-up-boundary floor (#894): entries whose audit
// Sequence is <= sinceSeq are dropped before decoding, so a stale
// pre-fix-up verdict is not counted after a fix-up re-opens the stage.
// Callers that want every entry (the plan-reviews / implement_reviews
// listing surfaces) pass sinceSeq == 0; since real and fake audit
// sequences are >= 1, a 0 floor is a no-op and the listing semantics are
// unchanged. Only reviewStatusFor passes a non-zero floor.
func (r *runResolver) decodeReviewVerdicts(ctx context.Context, runID uuid.UUID, category string, sinceSeq int64) ([]PlanReview, error) {
	entries, _, err := r.api.ListRunAudit(ctx, runID, ListRunAuditFilter{
		Category: category,
		Limit:    reviewAuditQueryLimit,
	})
	if err != nil {
		return nil, err
	}
	var reviews []PlanReview
	for _, e := range entries {
		if e.Sequence <= sinceSeq {
			continue
		}
		if e.Payload == nil {
			continue
		}
		raw, merr := json.Marshal(e.Payload)
		if merr != nil {
			continue
		}
		var review PlanReview
		if uerr := json.Unmarshal(raw, &review); uerr != nil {
			continue
		}
		reviews = append(reviews, review)
	}
	return reviews, nil
}

// decodeSkippedReviews queries the given *_review_skipped category and
// synthesizes a PlanReview with verdict "skipped" for each entry (#574).
// Each surfaces the recorded reason/authority so an agent can tell a
// degraded gate from a real verdict without a separate audit query.
//
// sinceSeq is the same fix-up-boundary floor as decodeReviewVerdicts
// (#894): entries with Sequence <= sinceSeq are dropped. The listing
// surfaces pass 0 (no-op); only reviewStatusFor floors to the latest
// fix-up.
func (r *runResolver) decodeSkippedReviews(ctx context.Context, runID uuid.UUID, category string, sinceSeq int64) ([]PlanReview, error) {
	entries, _, err := r.api.ListRunAudit(ctx, runID, ListRunAuditFilter{
		Category: category,
		Limit:    reviewAuditQueryLimit,
	})
	if err != nil {
		return nil, err
	}
	var reviews []PlanReview
	for _, e := range entries {
		if e.Sequence <= sinceSeq {
			continue
		}
		if e.Payload == nil {
			continue
		}
		raw, merr := json.Marshal(e.Payload)
		if merr != nil {
			continue
		}
		var p struct {
			Reason    string `json:"reason"`
			Authority string `json:"authority"`
		}
		if uerr := json.Unmarshal(raw, &p); uerr != nil {
			continue
		}
		reviews = append(reviews, PlanReview{
			ReviewerKind: "agent",
			Authority:    p.Authority,
			Verdict:      "skipped",
			Reason:       p.Reason,
		})
	}
	return reviews, nil
}

// decodeFailedReviews queries the given *_review_failed category and
// synthesizes a PlanReview with verdict "failed" for each entry (#664). A
// failed entry is the terminal record of a reviewer that errored or timed
// out; surfacing it as a definite verdict lets an agent distinguish a real
// failure from a still-running review (which stays 'pending'). Mirrors
// decodeSkippedReviews — same reason/authority projection.
//
// sinceSeq is the same fix-up-boundary floor as decodeReviewVerdicts
// (#894): entries with Sequence <= sinceSeq are dropped, so a pre-fix-up
// failure is not treated as the current round's terminal state.
func (r *runResolver) decodeFailedReviews(ctx context.Context, runID uuid.UUID, category string, sinceSeq int64) ([]PlanReview, error) {
	entries, _, err := r.api.ListRunAudit(ctx, runID, ListRunAuditFilter{
		Category: category,
		Limit:    reviewAuditQueryLimit,
	})
	if err != nil {
		return nil, err
	}
	var reviews []PlanReview
	for _, e := range entries {
		if e.Sequence <= sinceSeq {
			continue
		}
		if e.Payload == nil {
			continue
		}
		raw, merr := json.Marshal(e.Payload)
		if merr != nil {
			continue
		}
		var p struct {
			Reason        string `json:"reason"`
			ReviewerModel string `json:"reviewer_model"`
			Authority     string `json:"authority"`
		}
		if uerr := json.Unmarshal(raw, &p); uerr != nil {
			continue
		}
		reviews = append(reviews, PlanReview{
			ReviewerKind:  "agent",
			ReviewerModel: p.ReviewerModel,
			Authority:     p.Authority,
			Verdict:       "failed",
			Reason:        p.Reason,
		})
	}
	return reviews, nil
}

// hasAuditCategory returns whether at least one audit entry of the given
// category exists for the run. Used to detect the *_review_started proxy
// without decoding its payload — a single entry is enough to flip a
// not-yet-terminal review to 'pending'.
func (r *runResolver) hasAuditCategory(ctx context.Context, runID uuid.UUID, category string) (bool, error) {
	entries, _, err := r.api.ListRunAudit(ctx, runID, ListRunAuditFilter{
		Category: category,
		Limit:    1,
	})
	if err != nil {
		return false, err
	}
	return len(entries) > 0, nil
}

// reviewStatusFor derives the ReviewStatus for one stage from the audit
// trail (#600, #664, #894). Precedence: a terminal *_reviewed entry wins
// (=> complete, with decoded verdicts); else a *_review_skipped entry
// (=> skipped); else a terminal *_review_failed entry (=> failed, with the
// synthesized failure reason); else a *_review_started entry (=> pending);
// else none. The *_review_failed branch (#664) resolves what used to fall
// through to an ambiguous 'pending' — a reviewer that errored or timed out
// now writes a terminal entry, so the await/status surface reports a
// definite 'failed' instead of a still-waiting 'pending'.
//
// Fix-up boundary (#894): the three TERMINAL-verdict reads (reviewed /
// skipped / failed) are floored to entries that landed AFTER the latest
// stage_fixup_triggered audit sequence, so once a fix-up re-opens the
// implement stage the stale pre-fix-up verdict no longer reads as terminal.
// The *_review_started proxy check stays UNFLOORED on purpose: the round-1
// started entry (at a sequence below the fix-up boundary) is still present,
// so 'started exists' remains true and the precedence falls through to
// 'pending' in the window between the fix-up and the re-review's terminal
// entry — which is exactly what fishhawk_await_review must report while the
// re-review of the fix-up head is in flight, the analogous sibling to the
// #870 stale-input fix. sinceSeq is 0 for the plan stage (plan stages never
// fix-up, so the extra audit query is skipped) and for an implement stage
// with no prior fix-up; a 0 floor is a no-op (sequences are >= 1), so both
// the plan path and the no-fix-up implement path are byte-for-byte unchanged.
func (r *runResolver) reviewStatusFor(ctx context.Context, runID uuid.UUID, stage string) (*ReviewStatus, error) {
	cats, err := categoriesForStage(stage)
	if err != nil {
		return nil, err
	}

	// Only the implement stage can be re-opened by a fix-up; for the plan
	// stage keep sinceSeq at 0 and avoid the extra audit query.
	var sinceSeq int64
	if stage == "implement" {
		sinceSeq, err = r.latestImplementFixupSeq(ctx, runID)
		if err != nil {
			return nil, err
		}
	}

	reviewed, err := r.decodeReviewVerdicts(ctx, runID, cats.reviewed, sinceSeq)
	if err != nil {
		return nil, err
	}
	if len(reviewed) > 0 {
		return &ReviewStatus{Stage: stage, Status: "complete", Reviews: reviewed}, nil
	}

	skipped, err := r.decodeSkippedReviews(ctx, runID, cats.skipped, sinceSeq)
	if err != nil {
		return nil, err
	}
	if len(skipped) > 0 {
		return &ReviewStatus{Stage: stage, Status: "skipped", Reviews: skipped}, nil
	}

	failed, err := r.decodeFailedReviews(ctx, runID, cats.failed, sinceSeq)
	if err != nil {
		return nil, err
	}
	if len(failed) > 0 {
		return &ReviewStatus{Stage: stage, Status: "failed", Reviews: failed}, nil
	}

	started, err := r.hasAuditCategory(ctx, runID, cats.started)
	if err != nil {
		return nil, err
	}
	if started {
		// 'pending' is the one state where a polling agent should keep
		// calling — advertise the server-suggested poll cadence (#879).
		return &ReviewStatus{Stage: stage, Status: "pending", PollIntervalSeconds: suggestedReviewPollIntervalSeconds}, nil
	}

	return &ReviewStatus{Stage: stage, Status: "none"}, nil
}

// latestImplementFixupSeq returns the MAX audit Sequence among the run's
// stage_fixup_triggered entries (0 when none exist), the fix-up boundary
// reviewStatusFor floors the implement stage's terminal-verdict reads to
// (#894). It is RUN-scoped, not stage-scoped, to match reviewStatusFor's
// existing run-scoped audit reads (decodeReviewVerdicts filters by
// runID+category only, with no stage_id); a decomposition run with multiple
// implement stages is out of scope here and unchanged from today's
// run-scoped behavior. Reuses categoryStageFixupTriggered from
// review_action_hint.go.
func (r *runResolver) latestImplementFixupSeq(ctx context.Context, runID uuid.UUID) (int64, error) {
	entries, _, err := r.api.ListRunAudit(ctx, runID, ListRunAuditFilter{
		Category: categoryStageFixupTriggered,
		Limit:    reviewAuditQueryLimit,
	})
	if err != nil {
		return 0, err
	}
	var latestSeq int64
	for _, e := range entries {
		if e.Sequence > latestSeq {
			latestSeq = e.Sequence
		}
	}
	return latestSeq, nil
}

// defaultReviewPollInterval is the fallback poll cadence for
// fishhawk_await_review when the resolver's reviewPollInterval is unset.
// Tests inject a sub-millisecond interval so the poll loop runs without
// wall-clock sleeps.
const defaultReviewPollInterval = 3 * time.Second

// suggestedReviewPollIntervalSeconds is the server-suggested cadence a
// polling agent should use to re-poll fishhawk_get_run_status while a
// review is 'pending' (#879). Advertised on ReviewStatus.PollIntervalSeconds
// (pending only) and on the await tool's pending-after-timeout output so a
// resuming caller stops guessing sleep durations.
const suggestedReviewPollIntervalSeconds = 15

// awaitReviewTimeout bounds. The default is sized to the measured review
// latency (#878): real reviews complete in 3.5–4.5min (4m33s=273s worst
// case across the four cited runs) and the reviewer's own budget
// (FISHHAWKD_PLAN_REVIEW_TIMEOUT) is 300s, so a 360s default exceeds both —
// leaving ~60s headroom for a terminal *_review_failed entry to land within
// the await window. The 600s cap keeps a runaway input from holding the MCP
// session open indefinitely. poll-the-handle (fishhawk_get_run_status) is
// the blessed authoritative path; await is a best-effort, idempotent,
// resumable convenience over it (#879).
const (
	awaitReviewTimeoutDefault = 360
	awaitReviewTimeoutMax     = 600
)

// AwaitReviewInput is the fishhawk_await_review tool's input schema (#600).
type AwaitReviewInput struct {
	RunID          string `json:"run_id" jsonschema:"the Fishhawk run UUID"`
	Stage          string `json:"stage" jsonschema:"which review to wait on: 'plan' or 'implement'"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"how long to wait before returning 'pending' (default 360, capped at 600). On timeout the call returns pending + poll_interval_seconds; re-call to resume the wait"`
}

// AwaitReviewOutput is the fishhawk_await_review response. Status mirrors
// ReviewStatus.Status. WaitedSeconds reports the elapsed wall time so the
// caller can see whether it returned immediately or polled. Message is
// populated only on a pending-after-timeout result and names the
// actionable next step.
type AwaitReviewOutput struct {
	Stage         string       `json:"stage"`
	Status        string       `json:"status" jsonschema:"one of none, pending, complete, skipped, failed"`
	Reviews       []PlanReview `json:"reviews,omitempty" jsonschema:"decoded verdicts when status=complete; synthesized skipped verdict(s) when status=skipped; synthesized failure reason when status=failed"`
	WaitedSeconds float64      `json:"waited_seconds" jsonschema:"elapsed wall time spent waiting"`
	Message       string       `json:"message,omitempty" jsonschema:"actionable explanation when status=pending after the timeout"`
	// PollIntervalSeconds carries the server-suggested poll cadence (#879)
	// on a pending-after-timeout result so a resuming/idempotent re-caller
	// (or an agent switching to fishhawk_get_run_status polling) uses the
	// server cadence rather than guessing. Omitted on a terminal result.
	PollIntervalSeconds int `json:"poll_interval_seconds,omitempty" jsonschema:"server-suggested cadence (seconds) for the resumable re-call or for switching to fishhawk_get_run_status polling; present only on a pending-after-timeout result"`
}

// clampAwaitTimeout applies the default + cap. Non-positive falls back to
// the default (360s — sized to the measured 3.5–4.5min review latency and
// the 300s reviewer budget, #878); values over the cap (600s) clamp down.
func clampAwaitTimeout(n int) int {
	if n <= 0 {
		return awaitReviewTimeoutDefault
	}
	if n > awaitReviewTimeoutMax {
		return awaitReviewTimeoutMax
	}
	return n
}

// registerAwaitReview wires the fishhawk_await_review tool (#600). It is
// the ergonomic replacement for curl-polling GET /v0/runs/{id}/audit for a
// plan_reviewed / implement_reviewed entry: the tool blocks until the
// verdict lands (or the review is skipped / was never configured) or the
// timeout elapses. Read-only per ADR-021 — it only polls the audit
// endpoint, server-side, on an injectable interval.
func registerAwaitReview(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_await_review",
		Description: strings.TrimSpace(`
OPTIONAL convenience block over polling. fishhawk_get_run_status is the
AUTHORITATIVE source of truth for a review's terminal status — reach for it
FIRST and re-poll on the poll_interval_seconds it advertises while a review
is "pending". This tool just blocks that poll for you when you would rather
wait synchronously than loop yourself.

Resolves the review_status from the audit trail and:

  - Returns immediately when the review is already "complete", "skipped",
    "failed", or "none" (no review configured).
  - On "pending" (a review was dispatched but no terminal entry has landed)
    polls the audit endpoint until a terminal entry lands, the run itself
    reaches a terminal state (the review can no longer progress — it never
    strands, ADR-036 #874), or the timeout elapses.

Idempotent / resumable: a timeout returns status "pending" plus
poll_interval_seconds; the wait holds nothing — re-call to resume it, or
switch to fishhawk_get_run_status polling. Because the default is a long
(360s) synchronous call with no progress keep-alive, a client/transport
per-call timeout may still cut it short; that is fine here precisely because
poll-the-handle is the blessed primary path and a cut-short await is a
no-op you can re-issue.

Inputs:
  - run_id          (required) — Fishhawk run UUID.
  - stage           (required) — "plan" or "implement".
  - timeout_seconds — default 360, capped at 600.

Response: {stage, status, reviews[], waited_seconds, message,
poll_interval_seconds}. A "failed" status is a definite terminal state: the
reviewer errored or timed out (e.g. it hit FISHHAWKD_PLAN_REVIEW_TIMEOUT)
and a terminal *_review_failed audit entry was written — reviews[] carries
the failure reason. A "pending" status after the timeout means the review is
genuinely STILL RUNNING (no terminal entry yet); re-call to resume, switch
to fishhawk_get_run_status polling on poll_interval_seconds, or check the
fishhawkd logs.
`),
	}, resolver.awaitReview)
}

// runStateIsTerminal reports whether a run's state is one past which a
// review can no longer make progress (ADR-036 #874). The terminal set —
// succeeded / failed / cancelled — is compared INLINE here against the
// fishhawk-mcp-local Run.State string (client.go); the backend's run.State
// type and its IsTerminal() method are deliberately NOT imported, as they
// are not available in this package.
func runStateIsTerminal(state string) bool {
	switch state {
	case "succeeded", "failed", "cancelled":
		return true
	default:
		return false
	}
}

// awaitRunTerminalBackstop resolves the wait when the run itself has reached
// a terminal state while the review is still pending (ADR-036 #874): the
// review can never land a verdict, so holding the session open to the
// deadline would strand the caller. Returns (output, true) to resolve the
// wait; (zero, false) to keep polling. Best-effort — a GetRun error or a
// non-terminal run leaves the normal poll/timeout path in charge.
func (r *runResolver) awaitRunTerminalBackstop(ctx context.Context, runID uuid.UUID, stage string, st *ReviewStatus, start time.Time) (AwaitReviewOutput, bool) {
	runRow, err := r.api.GetRun(ctx, runID)
	if err != nil || runRow == nil {
		return AwaitReviewOutput{}, false
	}
	if !runStateIsTerminal(runRow.State) {
		return AwaitReviewOutput{}, false
	}
	return AwaitReviewOutput{
		Stage:         stage,
		Status:        st.Status,
		Reviews:       st.Reviews,
		WaitedSeconds: time.Since(start).Seconds(),
		Message: fmt.Sprintf("%s review is still %q, but run %s has reached terminal state %q — "+
			"the review can no longer progress, so the wait resolved instead of holding the "+
			"session open. Poll fishhawk_get_run_status for the final run state.",
			stage, st.Status, runID, runRow.State),
	}, true
}

// awaitReview is the tool handler.
func (r *runResolver) awaitReview(ctx context.Context, _ *mcp.CallToolRequest, in AwaitReviewInput) (*mcp.CallToolResult, AwaitReviewOutput, error) {
	runID, err := uuid.Parse(in.RunID)
	if err != nil {
		return nil, AwaitReviewOutput{}, fmt.Errorf("run_id %q is not a valid UUID: %w", in.RunID, err)
	}
	if _, err := categoriesForStage(in.Stage); err != nil {
		return nil, AwaitReviewOutput{}, err
	}
	timeout := clampAwaitTimeout(in.TimeoutSeconds)
	start := time.Now()

	// Fast path: terminal / none returns immediately without polling.
	st, err := r.reviewStatusFor(ctx, runID, in.Stage)
	if err != nil {
		return nil, AwaitReviewOutput{}, fmt.Errorf("review status: %w", err)
	}
	if st.Status != "pending" {
		return nil, r.awaitTerminalOutput(in.Stage, st, start), nil
	}

	// Pending: poll until a terminal entry lands, the run itself goes
	// terminal (the ADR-036 #874 non-stranding backstop), or the deadline
	// fires. Check the run-terminal backstop once before the loop so a run
	// that is already terminal at call time resolves without a poll tick.
	if out, done := r.awaitRunTerminalBackstop(ctx, runID, in.Stage, st, start); done {
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

	for {
		select {
		case <-pollCtx.Done():
			return nil, r.awaitPendingTimeoutOutput(in.Stage, timeout, start), nil
		case <-ticker.C:
			st, err := r.reviewStatusFor(pollCtx, runID, in.Stage)
			if err != nil {
				// A deadline hit mid-poll cancels the in-flight request;
				// that is a timeout, not a transport failure — return
				// pending rather than surfacing the cancellation as an error.
				if pollCtx.Err() != nil {
					return nil, r.awaitPendingTimeoutOutput(in.Stage, timeout, start), nil
				}
				return nil, AwaitReviewOutput{}, fmt.Errorf("poll review status: %w", err)
			}
			if st.Status != "pending" {
				return nil, r.awaitTerminalOutput(in.Stage, st, start), nil
			}
			// Still pending: the review hasn't landed a verdict. If the run
			// itself has gone terminal the review never will — resolve now
			// rather than spinning to the deadline (ADR-036 #874).
			if out, done := r.awaitRunTerminalBackstop(pollCtx, runID, in.Stage, st, start); done {
				return nil, out, nil
			}
		}
	}
}

// awaitTerminalOutput builds the response for a resolved (non-pending)
// review status.
func (*runResolver) awaitTerminalOutput(stage string, st *ReviewStatus, start time.Time) AwaitReviewOutput {
	return AwaitReviewOutput{
		Stage:         stage,
		Status:        st.Status,
		Reviews:       st.Reviews,
		WaitedSeconds: time.Since(start).Seconds(),
	}
}

// awaitPendingTimeoutOutput builds the resumable pending-after-timeout
// response (#879). The wait holds no state, so a timeout is a documented,
// idempotent checkpoint — not an error: the message frames the re-call (or a
// switch to fishhawk_get_run_status polling) as the next step and carries
// the server-suggested poll cadence. Since #664 a reviewer that errors or
// times out writes a terminal *_review_failed entry that resolves to a
// definite 'failed' status, so a lingering 'pending' still means the review
// is genuinely in flight.
func (*runResolver) awaitPendingTimeoutOutput(stage string, timeout int, start time.Time) AwaitReviewOutput {
	return AwaitReviewOutput{
		Stage:               stage,
		Status:              "pending",
		WaitedSeconds:       time.Since(start).Seconds(),
		PollIntervalSeconds: suggestedReviewPollIntervalSeconds,
		Message: fmt.Sprintf("%s review still pending after %ds — the review is genuinely still running (no terminal "+
			"audit entry yet; a reviewer that errored or hit FISHHAWKD_PLAN_REVIEW_TIMEOUT would have resolved to a "+
			"definite 'failed' status). The wait holds nothing: re-call fishhawk_await_review to resume it, or poll "+
			"fishhawk_get_run_status every %ds (the authoritative path). Check the fishhawkd logs if this persists.",
			stage, timeout, suggestedReviewPollIntervalSeconds),
	}
}
