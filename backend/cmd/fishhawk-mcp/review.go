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
type ReviewStatus struct {
	Stage   string       `json:"stage" jsonschema:"the reviewed stage type: 'plan' or 'implement'"`
	Status  string       `json:"status" jsonschema:"one of none, pending, complete, skipped, failed"`
	Reviews []PlanReview `json:"reviews,omitempty" jsonschema:"decoded verdicts when status=complete; synthesized skipped verdict(s) when status=skipped; synthesized failure reason when status=failed; empty for none/pending"`
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
func (r *runResolver) decodeReviewVerdicts(ctx context.Context, runID uuid.UUID, category string) ([]PlanReview, error) {
	entries, _, err := r.api.ListRunAudit(ctx, runID, ListRunAuditFilter{
		Category: category,
		Limit:    reviewAuditQueryLimit,
	})
	if err != nil {
		return nil, err
	}
	var reviews []PlanReview
	for _, e := range entries {
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
func (r *runResolver) decodeSkippedReviews(ctx context.Context, runID uuid.UUID, category string) ([]PlanReview, error) {
	entries, _, err := r.api.ListRunAudit(ctx, runID, ListRunAuditFilter{
		Category: category,
		Limit:    reviewAuditQueryLimit,
	})
	if err != nil {
		return nil, err
	}
	var reviews []PlanReview
	for _, e := range entries {
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
func (r *runResolver) decodeFailedReviews(ctx context.Context, runID uuid.UUID, category string) ([]PlanReview, error) {
	entries, _, err := r.api.ListRunAudit(ctx, runID, ListRunAuditFilter{
		Category: category,
		Limit:    reviewAuditQueryLimit,
	})
	if err != nil {
		return nil, err
	}
	var reviews []PlanReview
	for _, e := range entries {
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
// trail (#600, #664). Precedence: a terminal *_reviewed entry wins
// (=> complete, with decoded verdicts); else a *_review_skipped entry
// (=> skipped); else a terminal *_review_failed entry (=> failed, with the
// synthesized failure reason); else a *_review_started entry (=> pending);
// else none. The *_review_failed branch (#664) resolves what used to fall
// through to an ambiguous 'pending' — a reviewer that errored or timed out
// now writes a terminal entry, so the await/status surface reports a
// definite 'failed' instead of a still-waiting 'pending'.
func (r *runResolver) reviewStatusFor(ctx context.Context, runID uuid.UUID, stage string) (*ReviewStatus, error) {
	cats, err := categoriesForStage(stage)
	if err != nil {
		return nil, err
	}

	reviewed, err := r.decodeReviewVerdicts(ctx, runID, cats.reviewed)
	if err != nil {
		return nil, err
	}
	if len(reviewed) > 0 {
		return &ReviewStatus{Stage: stage, Status: "complete", Reviews: reviewed}, nil
	}

	skipped, err := r.decodeSkippedReviews(ctx, runID, cats.skipped)
	if err != nil {
		return nil, err
	}
	if len(skipped) > 0 {
		return &ReviewStatus{Stage: stage, Status: "skipped", Reviews: skipped}, nil
	}

	failed, err := r.decodeFailedReviews(ctx, runID, cats.failed)
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
		return &ReviewStatus{Stage: stage, Status: "pending"}, nil
	}

	return &ReviewStatus{Stage: stage, Status: "none"}, nil
}

// defaultReviewPollInterval is the fallback poll cadence for
// fishhawk_await_review when the resolver's reviewPollInterval is unset.
// Tests inject a sub-millisecond interval so the poll loop runs without
// wall-clock sleeps.
const defaultReviewPollInterval = 3 * time.Second

// awaitReviewTimeout bounds. Default 120s matches the backend's plan-
// review budget order-of-magnitude; the 600s cap keeps a runaway input
// from holding the MCP session open indefinitely.
const (
	awaitReviewTimeoutDefault = 120
	awaitReviewTimeoutMax     = 600
)

// AwaitReviewInput is the fishhawk_await_review tool's input schema (#600).
type AwaitReviewInput struct {
	RunID          string `json:"run_id" jsonschema:"the Fishhawk run UUID"`
	Stage          string `json:"stage" jsonschema:"which review to wait on: 'plan' or 'implement'"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"how long to wait before returning 'pending' (default 120, capped at 600)"`
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
}

// clampAwaitTimeout applies the default + cap. Non-positive falls back to
// the default; values over the cap clamp down.
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
Block until a Fishhawk stage's agent review reaches a terminal state.

The ergonomic replacement for curl-polling /v0/runs/{id}/audit for a
plan_reviewed / implement_reviewed entry. Resolves the review_status from
the audit trail and:

  - Returns immediately when the review is already "complete", "skipped",
    "failed", or "none" (no review configured).
  - On "pending" (a review was dispatched but no terminal entry has landed)
    polls the audit endpoint until a terminal entry lands or the timeout
    elapses.

Inputs:
  - run_id          (required) — Fishhawk run UUID.
  - stage           (required) — "plan" or "implement".
  - timeout_seconds — default 120, capped at 600.

Response: {stage, status, reviews[], waited_seconds, message}. A "failed"
status is a definite terminal state: the reviewer errored or timed out (e.g.
it hit FISHHAWKD_PLAN_REVIEW_TIMEOUT) and a terminal *_review_failed audit
entry was written — reviews[] carries the failure reason. A "pending" status
after the timeout means the review is genuinely STILL RUNNING (no terminal
entry yet); re-wait or check the fishhawkd logs.
`),
	}, resolver.awaitReview)
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

	// Pending: poll until a terminal entry lands or the deadline fires.
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

// awaitPendingTimeoutOutput builds the actionable pending-after-timeout
// response. Since #664 a reviewer that errors or times out writes a terminal
// *_review_failed entry that resolves to a definite 'failed' status, so a
// lingering 'pending' now means the review is genuinely still in flight —
// the message points the operator at that distinction rather than framing
// the timeout as an ambiguous silent failure.
func (*runResolver) awaitPendingTimeoutOutput(stage string, timeout int, start time.Time) AwaitReviewOutput {
	return AwaitReviewOutput{
		Stage:         stage,
		Status:        "pending",
		WaitedSeconds: time.Since(start).Seconds(),
		Message: fmt.Sprintf("%s review still pending after %ds: the review is genuinely still running — no terminal "+
			"audit entry has landed yet. A reviewer that errored or timed out (e.g. hit FISHHAWKD_PLAN_REVIEW_TIMEOUT) "+
			"would have resolved to a definite 'failed' status instead. Re-wait, or check the fishhawkd logs if this "+
			"persists.", stage, timeout),
	}
}
