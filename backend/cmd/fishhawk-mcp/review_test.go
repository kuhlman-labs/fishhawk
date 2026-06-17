package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// seedReviewStartedAudit appends a *_review_started audit entry to the
// fake's per-run audit map — the #600 proxy that marks a dispatched-but-
// not-yet-terminal review as 'pending'.
func seedReviewStartedAudit(fb *fakeBackend, runID uuid.UUID, category string, configuredAgents int, authority string) {
	payload, _ := json.Marshal(map[string]any{
		"configured_agents": configuredAgents,
		"authority":         authority,
	})
	var decoded any
	_ = json.Unmarshal(payload, &decoded)
	fb.mu.Lock()
	fb.perRunAuditByRun[runID] = append(fb.perRunAuditByRun[runID], AuditEntry{
		ID:       uuid.New().String(),
		Sequence: int64(len(fb.perRunAuditByRun[runID]) + 1),
		RunID:    runID.String(),
		Category: category,
		Payload:  decoded,
	})
	fb.mu.Unlock()
}

// seedReviewSkippedAudit appends a *_review_skipped audit entry.
func seedReviewSkippedAudit(fb *fakeBackend, runID uuid.UUID, category, reason, authority string) {
	payload, _ := json.Marshal(map[string]any{
		"reason":            reason,
		"configured_agents": 1,
		"authority":         authority,
	})
	var decoded any
	_ = json.Unmarshal(payload, &decoded)
	fb.mu.Lock()
	fb.perRunAuditByRun[runID] = append(fb.perRunAuditByRun[runID], AuditEntry{
		ID:       uuid.New().String(),
		Sequence: int64(len(fb.perRunAuditByRun[runID]) + 1),
		RunID:    runID.String(),
		Category: category,
		Payload:  decoded,
	})
	fb.mu.Unlock()
}

// seedReviewFailedAudit appends a terminal *_review_failed audit entry
// (#664) — the producer's ReviewFailedPayload shape — so the consumer-side
// resolution can be pinned against the exact category + payload the server
// writes.
func seedReviewFailedAudit(fb *fakeBackend, runID uuid.UUID, category, reason, model, authority string) {
	payload, _ := json.Marshal(map[string]any{
		"reason":         reason,
		"reviewer_model": model,
		"authority":      authority,
	})
	var decoded any
	_ = json.Unmarshal(payload, &decoded)
	fb.mu.Lock()
	fb.perRunAuditByRun[runID] = append(fb.perRunAuditByRun[runID], AuditEntry{
		ID:       uuid.New().String(),
		Sequence: int64(len(fb.perRunAuditByRun[runID]) + 1),
		RunID:    runID.String(),
		Category: category,
		Payload:  decoded,
	})
	fb.mu.Unlock()
}

// --- reviewStatusFor precedence (#600) ---

func TestReviewStatusFor_None_NoEntries(t *testing.T) {
	_, srv := newFakeBackend(t)
	runID := uuid.New()
	r := newResolver(srv, nil)

	st, err := r.reviewStatusFor(context.Background(), runID, "plan")
	if err != nil {
		t.Fatalf("reviewStatusFor: %v", err)
	}
	if st.Status != "none" {
		t.Errorf("Status = %q, want none", st.Status)
	}
	if st.Reviews != nil {
		t.Errorf("Reviews should be empty for none; got %+v", st.Reviews)
	}
}

func TestReviewStatusFor_Pending_StartedOnly(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	seedReviewStartedAudit(fb, runID, "plan_review_started", 1, "advisory")
	r := newResolver(srv, nil)

	st, err := r.reviewStatusFor(context.Background(), runID, "plan")
	if err != nil {
		t.Fatalf("reviewStatusFor: %v", err)
	}
	if st.Status != "pending" {
		t.Errorf("Status = %q, want pending", st.Status)
	}
	if st.Reviews != nil {
		t.Errorf("Reviews should be empty for pending; got %+v", st.Reviews)
	}
}

func TestReviewStatusFor_Complete_ReviewedWinsOverStarted(t *testing.T) {
	// Both a started and a terminal reviewed entry exist (the normal
	// happy path). Precedence: reviewed → complete, with verdicts.
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	seedReviewStartedAudit(fb, runID, "plan_review_started", 1, "gating")
	seedPlanReviewAudit(fb, runID, PlanReview{
		ReviewerKind: "agent",
		Authority:    "gating",
		Verdict:      "approve",
	})
	r := newResolver(srv, nil)

	st, err := r.reviewStatusFor(context.Background(), runID, "plan")
	if err != nil {
		t.Fatalf("reviewStatusFor: %v", err)
	}
	if st.Status != "complete" {
		t.Errorf("Status = %q, want complete", st.Status)
	}
	if len(st.Reviews) != 1 || st.Reviews[0].Verdict != "approve" {
		t.Errorf("Reviews = %+v, want one approve verdict", st.Reviews)
	}
}

func TestReviewStatusFor_Skipped_WinsOverStarted(t *testing.T) {
	// A skipped entry takes precedence over a started one (a degraded
	// gate is terminal — no verdict will ever land).
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	seedReviewStartedAudit(fb, runID, "implement_review_started", 1, "gating")
	seedReviewSkippedAudit(fb, runID, "implement_review_skipped", "reviewer_not_configured", "gating")
	r := newResolver(srv, nil)

	st, err := r.reviewStatusFor(context.Background(), runID, "implement")
	if err != nil {
		t.Fatalf("reviewStatusFor: %v", err)
	}
	if st.Status != "skipped" {
		t.Errorf("Status = %q, want skipped", st.Status)
	}
	if len(st.Reviews) != 1 || st.Reviews[0].Verdict != "skipped" {
		t.Errorf("Reviews = %+v, want one skipped verdict", st.Reviews)
	}
	if st.Reviews[0].Reason != "reviewer_not_configured" {
		t.Errorf("Reason = %q, want reviewer_not_configured", st.Reviews[0].Reason)
	}
}

// TestReviewStatusFor_Failed_WinsOverStarted is the #664 consumer-contract
// test: a terminal plan_review_failed entry resolves the status to a definite
// 'failed' (not the old ambiguous 'pending'), carrying the synthesized
// failure reason. It is pinned against the same category string +
// ReviewFailedPayload shape the server producer test writes, so a rename or
// field drift on either side trips a test.
func TestReviewStatusFor_Failed_WinsOverStarted(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	seedReviewStartedAudit(fb, runID, "plan_review_started", 1, "advisory")
	seedReviewFailedAudit(fb, runID, "plan_review_failed",
		"review timed out: context deadline exceeded", "claude-sonnet-4-6", "advisory")
	r := newResolver(srv, nil)

	st, err := r.reviewStatusFor(context.Background(), runID, "plan")
	if err != nil {
		t.Fatalf("reviewStatusFor: %v", err)
	}
	if st.Status != "failed" {
		t.Errorf("Status = %q, want failed", st.Status)
	}
	if len(st.Reviews) != 1 || st.Reviews[0].Verdict != "failed" {
		t.Fatalf("Reviews = %+v, want one failed verdict", st.Reviews)
	}
	if st.Reviews[0].Reason != "review timed out: context deadline exceeded" {
		t.Errorf("Reason = %q, want the synthesized failure reason", st.Reviews[0].Reason)
	}
}

// TestReviewStatusFor_Complete_WinsOverFailed pins the precedence ordering
// complete > failed: when a real verdict AND a failed entry both exist (e.g.
// a multi-agent stage where one reviewer succeeded and another timed out),
// 'complete' wins so a landed verdict is never masked by a sibling failure.
func TestReviewStatusFor_Complete_WinsOverFailed(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	seedReviewFailedAudit(fb, runID, "plan_review_failed", "timed out", "claude-sonnet-4-6", "advisory")
	seedPlanReviewAudit(fb, runID, PlanReview{ReviewerKind: "agent", Authority: "advisory", Verdict: "approve"})
	r := newResolver(srv, nil)

	st, err := r.reviewStatusFor(context.Background(), runID, "plan")
	if err != nil {
		t.Fatalf("reviewStatusFor: %v", err)
	}
	if st.Status != "complete" {
		t.Errorf("Status = %q, want complete (a landed verdict outranks a sibling failure)", st.Status)
	}
}

// TestAwaitReview_ReturnsImmediately_Failed confirms the await tool resolves
// a terminal failed entry on the fast path (no polling) to status 'failed'
// with the failure reason surfaced.
func TestAwaitReview_ReturnsImmediately_Failed(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	seedReviewFailedAudit(fb, runID, "implement_review_failed",
		"review timed out: context deadline exceeded", "claude-sonnet-4-6", "gating")
	r := newResolver(srv, nil)

	_, out, err := r.awaitReview(context.Background(), nil, AwaitReviewInput{RunID: runID.String(), Stage: "implement"})
	if err != nil {
		t.Fatalf("awaitReview: %v", err)
	}
	if out.Status != "failed" {
		t.Errorf("Status = %q, want failed", out.Status)
	}
	if len(out.Reviews) != 1 || out.Reviews[0].Reason != "review timed out: context deadline exceeded" {
		t.Errorf("Reviews = %+v, want one failed verdict with the timeout reason", out.Reviews)
	}
}

func TestReviewStatusFor_RejectsBadStage(t *testing.T) {
	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	_, err := r.reviewStatusFor(context.Background(), uuid.New(), "review")
	if err == nil {
		t.Fatal("expected error on unknown stage")
	}
	if !strings.Contains(err.Error(), "plan, implement") {
		t.Errorf("error wording: %v", err)
	}
}

// --- get_run_status / get_plan review_status field population (#600) ---

func TestGetRunStatus_ReviewStatusFields_Populated(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", State: "running"}
	// Plan review complete; implement review pending.
	seedPlanReviewAudit(fb, runID, PlanReview{ReviewerKind: "agent", Authority: "advisory", Verdict: "approve"})
	seedReviewStartedAudit(fb, runID, "implement_review_started", 1, "advisory")

	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if out.PlanReviewStatus == nil || out.PlanReviewStatus.Status != "complete" {
		t.Errorf("PlanReviewStatus = %+v, want complete", out.PlanReviewStatus)
	}
	if out.ImplementReviewStatus == nil || out.ImplementReviewStatus.Status != "pending" {
		t.Errorf("ImplementReviewStatus = %+v, want pending", out.ImplementReviewStatus)
	}
	// #879 poll-cadence seam: the hint set ONCE in reviewStatusFor must reach
	// the getRunStatus output through the shared ReviewStatus — present on the
	// pending status, omitted (zero) on the terminal one. This asserts the
	// cross-tool seam, not a second computation in getRunStatus.
	if out.ImplementReviewStatus.PollIntervalSeconds != suggestedReviewPollIntervalSeconds {
		t.Errorf("pending ImplementReviewStatus.PollIntervalSeconds = %d, want %d", out.ImplementReviewStatus.PollIntervalSeconds, suggestedReviewPollIntervalSeconds)
	}
	if out.PlanReviewStatus.PollIntervalSeconds != 0 {
		t.Errorf("complete PlanReviewStatus.PollIntervalSeconds = %d, want 0 (omitted on terminal)", out.PlanReviewStatus.PollIntervalSeconds)
	}
	// Existing ImplementReviews[] driver field must remain unpopulated
	// here (no implement_reviewed entries) — no regression.
	if out.ImplementReviews != nil {
		t.Errorf("ImplementReviews should be nil with no implement_reviewed entries; got %+v", out.ImplementReviews)
	}
}

func TestGetRunStatus_ReviewStatus_NoneWhenNoEntries(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String()}

	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if out.PlanReviewStatus == nil || out.PlanReviewStatus.Status != "none" {
		t.Errorf("PlanReviewStatus = %+v, want none", out.PlanReviewStatus)
	}
	if out.ImplementReviewStatus == nil || out.ImplementReviewStatus.Status != "none" {
		t.Errorf("ImplementReviewStatus = %+v, want none", out.ImplementReviewStatus)
	}
}

func TestGetPlan_PlanReviewStatus_Populated(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, planStageID, samplePlanContent(), time.Hour)
	seedReviewStartedAudit(fb, runID, "plan_review_started", 1, "advisory")

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.PlanReviewStatus == nil || out.PlanReviewStatus.Status != "pending" {
		t.Errorf("PlanReviewStatus = %+v, want pending", out.PlanReviewStatus)
	}
}

// --- fishhawk_await_review (#600) ---

func TestAwaitReview_RejectsBadInput(t *testing.T) {
	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	if _, _, err := r.awaitReview(context.Background(), nil, AwaitReviewInput{RunID: "nope", Stage: "plan"}); err == nil {
		t.Error("expected error on bad run_id")
	}
	if _, _, err := r.awaitReview(context.Background(), nil, AwaitReviewInput{RunID: uuid.NewString(), Stage: "review"}); err == nil {
		t.Error("expected error on bad stage")
	}
}

func TestAwaitReview_ReturnsImmediately_Complete(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	seedPlanReviewAudit(fb, runID, PlanReview{ReviewerKind: "agent", Authority: "gating", Verdict: "approve"})
	r := newResolver(srv, nil)
	// A real poll interval would never fire because the fast path returns
	// first; assert that by leaving it at the production default.

	_, out, err := r.awaitReview(context.Background(), nil, AwaitReviewInput{RunID: runID.String(), Stage: "plan"})
	if err != nil {
		t.Fatalf("awaitReview: %v", err)
	}
	if out.Status != "complete" {
		t.Errorf("Status = %q, want complete", out.Status)
	}
	if len(out.Reviews) != 1 {
		t.Errorf("Reviews = %+v, want 1", out.Reviews)
	}
}

func TestAwaitReview_ReturnsImmediately_SkippedAndNone(t *testing.T) {
	fb, srv := newFakeBackend(t)
	skippedRun := uuid.New()
	noneRun := uuid.New()
	seedReviewSkippedAudit(fb, skippedRun, "plan_review_skipped", "reviewer_not_configured", "gating")
	r := newResolver(srv, nil)

	_, out, err := r.awaitReview(context.Background(), nil, AwaitReviewInput{RunID: skippedRun.String(), Stage: "plan"})
	if err != nil {
		t.Fatalf("awaitReview skipped: %v", err)
	}
	if out.Status != "skipped" {
		t.Errorf("Status = %q, want skipped", out.Status)
	}

	_, out, err = r.awaitReview(context.Background(), nil, AwaitReviewInput{RunID: noneRun.String(), Stage: "plan"})
	if err != nil {
		t.Fatalf("awaitReview none: %v", err)
	}
	if out.Status != "none" {
		t.Errorf("Status = %q, want none", out.Status)
	}
}

func TestAwaitReview_PollsThenResolves(t *testing.T) {
	// Start pending (only a started entry); flip to complete on the first
	// per-run audit poll. The injected sub-millisecond interval keeps the
	// loop fast and sleep-free.
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	seedReviewStartedAudit(fb, runID, "implement_review_started", 1, "advisory")

	flipped := false
	fb.reviewFlip = func(category string) {
		// The started-category query is the last one reviewStatusFor makes
		// on a pending resolution; appending the reviewed entry here flips
		// the NEXT reviewStatusFor to complete. Mutates under fb.mu (the
		// handler holds it), so no re-lock.
		if category == "implement_review_started" && !flipped {
			flipped = true
			payload, _ := json.Marshal(PlanReview{ReviewerKind: "agent", Authority: "advisory", Verdict: "approve"})
			var decoded any
			_ = json.Unmarshal(payload, &decoded)
			fb.perRunAuditByRun[runID] = append(fb.perRunAuditByRun[runID], AuditEntry{
				ID:       uuid.New().String(),
				Sequence: int64(len(fb.perRunAuditByRun[runID]) + 1),
				RunID:    runID.String(),
				Category: "implement_reviewed",
				Payload:  decoded,
			})
		}
	}

	r := newResolver(srv, nil)
	r.reviewPollInterval = 100 * time.Microsecond

	_, out, err := r.awaitReview(context.Background(), nil, AwaitReviewInput{
		RunID:          runID.String(),
		Stage:          "implement",
		TimeoutSeconds: 5,
	})
	if err != nil {
		t.Fatalf("awaitReview: %v", err)
	}
	if out.Status != "complete" {
		t.Errorf("Status = %q, want complete after poll-resolve", out.Status)
	}
	if len(out.Reviews) != 1 || out.Reviews[0].Verdict != "approve" {
		t.Errorf("Reviews = %+v, want one approve verdict", out.Reviews)
	}
}

func TestAwaitReview_PendingOnTimeout(t *testing.T) {
	// Review stays pending forever; the await loop must return 'pending'
	// with the actionable message rather than hanging.
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	seedReviewStartedAudit(fb, runID, "plan_review_started", 1, "gating")

	r := newResolver(srv, nil)
	r.reviewPollInterval = 100 * time.Microsecond

	// Drive the deadline deterministically instead of racing a tiny
	// wall-clock parent deadline (#729). A 5ms context.WithTimeout could
	// elapse before the handler's goroutine was scheduled under CI -race, so
	// the fast-path reviewStatusFor (review.go:355) returned context.Canceled
	// and the loop was never entered. Use a cancellable context and cancel it
	// from the audit hook only AFTER the fast path has resolved to 'pending'
	// and the poll loop has begun. reviewStatusFor queries the started
	// category exactly once per pass, as its final lookup (review.go:248): the
	// fast path is pass #1 (count == 1), the first poll iteration is pass #2
	// (count == 2). Cancelling on the 2nd started query guarantees the fast
	// path completes (returns 'pending', review.go:359-361) and the loop is
	// entered (review.go:373) before the cancellation is observed — the next
	// reviewStatusFor(pollCtx) / pollCtx.Done() then yields the pending-timeout
	// output deterministically.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var startedQueries atomic.Int64
	fb.reviewFlip = func(category string) {
		if category == "plan_review_started" && startedQueries.Add(1) == 2 {
			cancel()
		}
	}

	_, out, err := r.awaitReview(ctx, nil, AwaitReviewInput{
		RunID:          runID.String(),
		Stage:          "plan",
		TimeoutSeconds: 600,
	})
	if err != nil {
		t.Fatalf("awaitReview: %v", err)
	}
	if out.Status != "pending" {
		t.Fatalf("Status = %q, want pending on timeout", out.Status)
	}
	if !strings.Contains(out.Message, "FISHHAWK_PLAN_REVIEW_TIMEOUT") && !strings.Contains(out.Message, "FISHHAWKD_PLAN_REVIEW_TIMEOUT") {
		t.Errorf("pending-timeout message should name the timeout env var: %q", out.Message)
	}
	if !strings.Contains(out.Message, "failed") {
		t.Errorf("pending-timeout message should explain the failed/timed-out case: %q", out.Message)
	}
}

func TestAwaitReview_BoundedPolls_DoesNotHammerBackend(t *testing.T) {
	// A bounded poll loop must terminate within a small number of audit
	// requests, not spin unbounded. Drive a deterministic number of poll
	// iterations via the started-query counter rather than a wall-clock
	// context window that races the fast path (#729), then assert the
	// per-run audit endpoint was polled a bounded, small number of times.
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	seedReviewStartedAudit(fb, runID, "plan_review_started", 1, "gating")

	r := newResolver(srv, nil)
	r.reviewPollInterval = 100 * time.Microsecond

	// Started-category queries count reviewStatusFor passes: pass #1 is the
	// fast path, each subsequent pass is one poll iteration (review.go:248).
	// Cancel after a small number so the loop exits promptly with 'pending'.
	const cancelAfterStartedQueries = 3
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var startedQueries atomic.Int64
	fb.reviewFlip = func(category string) {
		if category == "plan_review_started" && startedQueries.Add(1) == cancelAfterStartedQueries {
			cancel()
		}
	}

	_, out, err := r.awaitReview(ctx, nil, AwaitReviewInput{
		RunID:          runID.String(),
		Stage:          "plan",
		TimeoutSeconds: 600,
	})
	if err != nil {
		t.Fatalf("awaitReview: %v", err)
	}
	if out.Status != "pending" {
		t.Errorf("Status = %q, want pending", out.Status)
	}
	// reviewStatusFor issues exactly one started-category query per pass,
	// and the loop must exit on the first pass that observes cancellation —
	// so the observed count equals the cancel threshold EXACTLY. This pins a
	// real property of the loop (one query per poll iteration, prompt exit
	// on cancel); a regression that issued multiple queries per iteration,
	// or kept polling after cancellation, would push the count past the
	// threshold (or hang) and fail here — unlike a `got < N` upper bound,
	// which the deterministic cancel makes vacuously true by construction.
	if got := startedQueries.Load(); got != cancelAfterStartedQueries {
		t.Errorf("started-category audit queries = %d, want exactly %d (one query per pass, prompt exit on cancel)", got, cancelAfterStartedQueries)
	}
}

func TestAwaitReview_TimeoutClamped(t *testing.T) {
	// The default was recalibrated 120→360 (#878/#879) to exceed the measured
	// 3.5–4.5min review latency and the 300s reviewer budget. Pin the literal
	// so a stray re-bump trips here, not just via the const.
	if awaitReviewTimeoutDefault != 360 {
		t.Fatalf("awaitReviewTimeoutDefault = %d, want 360", awaitReviewTimeoutDefault)
	}
	if got := clampAwaitTimeout(0); got != 360 {
		t.Errorf("clampAwaitTimeout(0) = %d, want 360", got)
	}
	if got := clampAwaitTimeout(99999); got != awaitReviewTimeoutMax {
		t.Errorf("clampAwaitTimeout(99999) = %d, want %d", got, awaitReviewTimeoutMax)
	}
	if got := clampAwaitTimeout(45); got != 45 {
		t.Errorf("clampAwaitTimeout(45) = %d, want 45", got)
	}
}

// TestReviewStatusFor_PollIntervalHint_PendingOnly pins the #879 contract:
// reviewStatusFor advertises the server-suggested poll cadence ONLY on the
// 'pending' status (the one state where an agent should keep polling) and
// omits it (zero) on every terminal/none status.
func TestReviewStatusFor_PollIntervalHint_PendingOnly(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	pendingRun := uuid.New()
	seedReviewStartedAudit(fb, pendingRun, "plan_review_started", 1, "advisory")
	st, err := r.reviewStatusFor(context.Background(), pendingRun, "plan")
	if err != nil {
		t.Fatalf("reviewStatusFor pending: %v", err)
	}
	if st.Status != "pending" || st.PollIntervalSeconds != suggestedReviewPollIntervalSeconds {
		t.Errorf("pending: Status=%q PollIntervalSeconds=%d, want pending/%d", st.Status, st.PollIntervalSeconds, suggestedReviewPollIntervalSeconds)
	}

	completeRun := uuid.New()
	seedPlanReviewAudit(fb, completeRun, PlanReview{ReviewerKind: "agent", Authority: "advisory", Verdict: "approve"})
	st, err = r.reviewStatusFor(context.Background(), completeRun, "plan")
	if err != nil {
		t.Fatalf("reviewStatusFor complete: %v", err)
	}
	if st.PollIntervalSeconds != 0 {
		t.Errorf("complete: PollIntervalSeconds = %d, want 0 (omitted on terminal)", st.PollIntervalSeconds)
	}

	skippedRun := uuid.New()
	seedReviewSkippedAudit(fb, skippedRun, "plan_review_skipped", "reviewer_not_configured", "gating")
	st, err = r.reviewStatusFor(context.Background(), skippedRun, "plan")
	if err != nil {
		t.Fatalf("reviewStatusFor skipped: %v", err)
	}
	if st.PollIntervalSeconds != 0 {
		t.Errorf("skipped: PollIntervalSeconds = %d, want 0", st.PollIntervalSeconds)
	}

	st, err = r.reviewStatusFor(context.Background(), uuid.New(), "plan")
	if err != nil {
		t.Fatalf("reviewStatusFor none: %v", err)
	}
	if st.PollIntervalSeconds != 0 {
		t.Errorf("none: PollIntervalSeconds = %d, want 0", st.PollIntervalSeconds)
	}
}

// TestAwaitReview_RunTerminalBackstop pins the ADR-036 #874 non-stranding
// backstop: when the run itself reaches a terminal state while the review is
// still pending, awaitReview resolves immediately rather than spinning to the
// deadline — the review can no longer land a verdict.
func TestAwaitReview_RunTerminalBackstop(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	// Review dispatched (=> pending) but the run has failed.
	fb.getRunByID[runID] = Run{ID: runID.String(), State: "failed"}
	seedReviewStartedAudit(fb, runID, "implement_review_started", 1, "gating")

	r := newResolver(srv, nil)
	r.reviewPollInterval = 100 * time.Microsecond

	// A large timeout: if the backstop did NOT fire the test would hang on the
	// deadline rather than returning, so a prompt return is itself the proof.
	_, out, err := r.awaitReview(context.Background(), nil, AwaitReviewInput{
		RunID:          runID.String(),
		Stage:          "implement",
		TimeoutSeconds: 600,
	})
	if err != nil {
		t.Fatalf("awaitReview: %v", err)
	}
	if out.Status != "pending" {
		t.Errorf("Status = %q, want pending (backstop preserves the in-flight review status)", out.Status)
	}
	// Assert a fragment UNIQUE to the backstop arm. "terminal" alone also
	// appears in awaitPendingTimeoutOutput's "no terminal audit entry yet", so
	// it would pass on the deadline path too and fail to distinguish backstop
	// from a 600s timeout (caught only by a test-suite hang, not a clean
	// assertion). "can no longer progress" appears only on the backstop arm.
	if !strings.Contains(out.Message, "can no longer progress") {
		t.Errorf("backstop message should explain the review can no longer progress: %q", out.Message)
	}
}

// TestAwaitReview_RunTerminalBackstop_InLoop pins the IN-LOOP arm of the
// ADR-036 #874 backstop, distinct from TestAwaitReview_RunTerminalBackstop
// (which seeds the run terminal before the call, so only the pre-loop check
// fires and the loop never runs). Here the run is non-terminal at call time —
// the pre-loop backstop sees "running" and the poll loop IS entered — then it
// transitions to terminal mid-loop. The second awaitRunTerminalBackstop call,
// inside the select, must resolve the wait. A regression in that in-loop arm
// is invisible to the pre-loop test.
func TestAwaitReview_RunTerminalBackstop_InLoop(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	// Non-terminal at call time: the pre-loop backstop observes "running", so
	// the loop is entered. The review stays pending throughout.
	fb.getRunByID[runID] = Run{ID: runID.String(), State: "running"}
	seedReviewStartedAudit(fb, runID, "implement_review_started", 1, "gating")

	r := newResolver(srv, nil)
	r.reviewPollInterval = 100 * time.Microsecond

	// reviewStatusFor issues exactly one started-category query per pass: pass
	// #1 is the fast path (run still "running"); pass #2 is the first poll
	// tick. Flip the run to terminal on the 2nd started query so the
	// transition lands AFTER the pre-loop backstop check has already passed —
	// guaranteeing the IN-LOOP arm (not the pre-loop check) is what resolves
	// the wait. reviewFlip runs under fb.mu, the same lock the GetRun handler
	// takes, so mutating getRunByID here is race-free.
	var startedQueries atomic.Int64
	fb.reviewFlip = func(category string) {
		if category == "implement_review_started" && startedQueries.Add(1) == 2 {
			fb.getRunByID[runID] = Run{ID: runID.String(), State: "failed"}
		}
	}

	// Large timeout: if the in-loop backstop did NOT fire the test would hang
	// on the deadline, so a prompt return is itself proof the in-loop arm
	// resolved it.
	_, out, err := r.awaitReview(context.Background(), nil, AwaitReviewInput{
		RunID:          runID.String(),
		Stage:          "implement",
		TimeoutSeconds: 600,
	})
	if err != nil {
		t.Fatalf("awaitReview: %v", err)
	}
	if out.Status != "pending" {
		t.Errorf("Status = %q, want pending (backstop preserves the in-flight review status)", out.Status)
	}
	if !strings.Contains(out.Message, "can no longer progress") {
		t.Errorf("in-loop backstop message should explain the review can no longer progress: %q", out.Message)
	}
}

// TestAwaitReview_PendingTimeout_CarriesHint_AndIdempotent pins the #879
// resumable contract: a pending-after-timeout result carries the
// poll_interval_seconds hint, the wait holds no state, and a second await
// call resolves cleanly once the verdict lands.
func TestAwaitReview_PendingTimeout_CarriesHint_AndIdempotent(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	seedReviewStartedAudit(fb, runID, "plan_review_started", 1, "gating")

	r := newResolver(srv, nil)
	r.reviewPollInterval = 100 * time.Microsecond

	// Drive the deadline deterministically (no wall-clock sleep): cancel on
	// the 2nd started-category query, after the fast path resolved to pending
	// and the poll loop began (same pattern as TestAwaitReview_PendingOnTimeout).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var startedQueries atomic.Int64
	fb.reviewFlip = func(category string) {
		if category == "plan_review_started" && startedQueries.Add(1) == 2 {
			cancel()
		}
	}

	_, out, err := r.awaitReview(ctx, nil, AwaitReviewInput{RunID: runID.String(), Stage: "plan", TimeoutSeconds: 600})
	if err != nil {
		t.Fatalf("awaitReview: %v", err)
	}
	if out.Status != "pending" {
		t.Fatalf("Status = %q, want pending on timeout", out.Status)
	}
	if out.PollIntervalSeconds != suggestedReviewPollIntervalSeconds {
		t.Errorf("pending-timeout PollIntervalSeconds = %d, want %d", out.PollIntervalSeconds, suggestedReviewPollIntervalSeconds)
	}

	// Idempotent re-call: the wait held nothing. Land the verdict and re-wait;
	// it resolves cleanly on the fast path. Cancelling the first call's request
	// can leave an httptest handler in flight reading the fake's maps under
	// fb.mu, so mutate them under the same lock to stay race-free.
	payload, _ := json.Marshal(PlanReview{ReviewerKind: "agent", Authority: "gating", Verdict: "approve"})
	var decoded any
	_ = json.Unmarshal(payload, &decoded)
	fb.mu.Lock()
	fb.reviewFlip = nil
	fb.perRunAuditByRun[runID] = append(fb.perRunAuditByRun[runID], AuditEntry{
		ID:       uuid.New().String(),
		Sequence: int64(len(fb.perRunAuditByRun[runID]) + 1),
		RunID:    runID.String(),
		Category: "plan_reviewed",
		Payload:  decoded,
	})
	fb.mu.Unlock()
	_, out2, err := r.awaitReview(context.Background(), nil, AwaitReviewInput{RunID: runID.String(), Stage: "plan"})
	if err != nil {
		t.Fatalf("re-wait awaitReview: %v", err)
	}
	if out2.Status != "complete" {
		t.Errorf("re-wait Status = %q, want complete", out2.Status)
	}
}

// seedRunFixupTriggeredAudit appends a RUN-scoped stage_fixup_triggered
// audit entry (no StageID) — the fix-up boundary reviewStatusFor floors the
// implement stage's terminal-verdict reads to (#894). It mirrors the
// stage-keyed seedFixupTriggeredAudit in review_action_hint_test.go, but
// leaves StageID nil because latestImplementFixupSeq reads run-scoped (it
// takes the MAX Sequence across the run's entries regardless of stage). The
// entry's Sequence lands after every previously seeded entry, so a fix-up
// seeded after a round-1 implement_reviewed correctly floors that verdict out.
func seedRunFixupTriggeredAudit(fb *fakeBackend, runID uuid.UUID) {
	fb.mu.Lock()
	fb.perRunAuditByRun[runID] = append(fb.perRunAuditByRun[runID], AuditEntry{
		ID:       uuid.New().String(),
		Sequence: int64(len(fb.perRunAuditByRun[runID]) + 1),
		RunID:    runID.String(),
		Category: categoryStageFixupTriggered,
	})
	fb.mu.Unlock()
}

// --- fix-up-boundary flooring (#894) ---

// TestReviewStatusFor_Implement_PendingAfterFixup is the #894 regression:
// after a fix-up re-opens the implement stage, the stale round-1
// implement_reviewed verdict must NOT read as terminal 'complete'. The
// terminal-verdict reads are floored to entries after the latest
// stage_fixup_triggered, but the round-1 *_review_started proxy stays
// unfloored, so with no re-review yet the status resolves to 'pending' — the
// in-flight re-review window. Before the fix this returned 'complete' with
// the stale verdict, instantly resolving fishhawk_await_review.
func TestReviewStatusFor_Implement_PendingAfterFixup(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	// Round 1: a review was dispatched and landed an approve_with_concerns
	// verdict, then a fix-up was triggered to route the concerns back.
	seedReviewStartedAudit(fb, runID, "implement_review_started", 1, "advisory")
	seedImplementReviewAudit(fb, runID, withConcerns(2))
	seedRunFixupTriggeredAudit(fb, runID)
	r := newResolver(srv, nil)

	st, err := r.reviewStatusFor(context.Background(), runID, "implement")
	if err != nil {
		t.Fatalf("reviewStatusFor: %v", err)
	}
	if st.Status != "pending" {
		t.Errorf("Status = %q, want pending (the stale pre-fix-up verdict must not read complete)", st.Status)
	}
	if len(st.Reviews) != 0 {
		t.Errorf("Reviews = %+v, want empty while the re-review is in flight", st.Reviews)
	}
}

// TestReviewStatusFor_Implement_CompleteWithRound2Verdict pins that once the
// re-review of the fix-up head lands, the status resolves to 'complete'
// carrying ONLY the round-2 verdict — the floored-out round-1 verdict does
// not leak into Reviews.
func TestReviewStatusFor_Implement_CompleteWithRound2Verdict(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	seedReviewStartedAudit(fb, runID, "implement_review_started", 1, "advisory")
	seedImplementReviewAudit(fb, runID, withConcerns(2)) // round 1: stale
	seedRunFixupTriggeredAudit(fb, runID)
	// Round 2: the re-review of the fix-up head lands a clean approve.
	seedImplementReviewAudit(fb, runID, PlanReview{ReviewerKind: "agent", Authority: "advisory", Verdict: "approve"})
	r := newResolver(srv, nil)

	st, err := r.reviewStatusFor(context.Background(), runID, "implement")
	if err != nil {
		t.Fatalf("reviewStatusFor: %v", err)
	}
	if st.Status != "complete" {
		t.Fatalf("Status = %q, want complete once the re-review lands", st.Status)
	}
	if len(st.Reviews) != 1 {
		t.Fatalf("Reviews = %+v, want only the round-2 verdict", st.Reviews)
	}
	if st.Reviews[0].Verdict != "approve" {
		t.Errorf("Reviews[0].Verdict = %q, want approve (round-2); the round-1 approve_with_concerns must be floored out", st.Reviews[0].Verdict)
	}
}

// TestReviewStatusFor_Plan_FloorExempt confirms the stage_fixup_triggered
// floor applies ONLY to the implement stage: a plan stage with a landed verdict
// resolves to 'complete' even when an unrelated stage_fixup_triggered entry (a
// sequence above the plan verdict) exists. The plan stage floors to plan_revised
// (#1201), NOT stage_fixup_triggered, and no plan_revised entry exists here, so
// the plan floor is 0 and the behavior is byte-for-byte unchanged.
func TestReviewStatusFor_Plan_FloorExempt(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	seedPlanReviewAudit(fb, runID, PlanReview{ReviewerKind: "agent", Authority: "advisory", Verdict: "approve"})
	seedRunFixupTriggeredAudit(fb, runID) // unrelated, higher sequence
	r := newResolver(srv, nil)

	st, err := r.reviewStatusFor(context.Background(), runID, "plan")
	if err != nil {
		t.Fatalf("reviewStatusFor: %v", err)
	}
	if st.Status != "complete" {
		t.Errorf("Status = %q, want complete (the fix-up floor must not touch the plan stage)", st.Status)
	}
}

// TestReviewStatusFor_Implement_SkippedOrFailedAfterFixup confirms the floor
// applies to all three terminal reads: a round-1 implement_reviewed is
// floored out, and a post-fix-up skipped / failed re-review resolves to the
// matching terminal status rather than the stale 'complete'.
func TestReviewStatusFor_Implement_SkippedOrFailedAfterFixup(t *testing.T) {
	t.Run("skipped", func(t *testing.T) {
		fb, srv := newFakeBackend(t)
		runID := uuid.New()
		seedReviewStartedAudit(fb, runID, "implement_review_started", 1, "gating")
		seedImplementReviewAudit(fb, runID, withConcerns(1)) // round 1: stale
		seedRunFixupTriggeredAudit(fb, runID)
		seedReviewSkippedAudit(fb, runID, "implement_review_skipped", "reviewer_not_configured", "gating")
		r := newResolver(srv, nil)

		st, err := r.reviewStatusFor(context.Background(), runID, "implement")
		if err != nil {
			t.Fatalf("reviewStatusFor: %v", err)
		}
		if st.Status != "skipped" {
			t.Errorf("Status = %q, want skipped (round-1 verdict floored out)", st.Status)
		}
	})
	t.Run("failed", func(t *testing.T) {
		fb, srv := newFakeBackend(t)
		runID := uuid.New()
		seedReviewStartedAudit(fb, runID, "implement_review_started", 1, "gating")
		seedImplementReviewAudit(fb, runID, withConcerns(1)) // round 1: stale
		seedRunFixupTriggeredAudit(fb, runID)
		seedReviewFailedAudit(fb, runID, "implement_review_failed",
			"review timed out", "claude-sonnet-4-6", "gating")
		r := newResolver(srv, nil)

		st, err := r.reviewStatusFor(context.Background(), runID, "implement")
		if err != nil {
			t.Fatalf("reviewStatusFor: %v", err)
		}
		if st.Status != "failed" {
			t.Errorf("Status = %q, want failed (round-1 verdict floored out)", st.Status)
		}
	})
}

// --- plan-revision-boundary flooring (#1201) ---

// seedRunPlanRevisedAudit appends a RUN-scoped plan_revised audit entry — the
// plan-revision boundary reviewStatusFor / loadPlanReviews floor the plan
// stage's terminal-verdict reads to (#1201). Mirrors seedRunFixupTriggeredAudit:
// the entry's Sequence lands after every previously seeded entry, so a revise
// seeded after a round-1 plan_reviewed correctly floors that verdict out. It is
// the plan-stage analog of stage_fixup_triggered.
func seedRunPlanRevisedAudit(fb *fakeBackend, runID uuid.UUID) {
	fb.mu.Lock()
	fb.perRunAuditByRun[runID] = append(fb.perRunAuditByRun[runID], AuditEntry{
		ID:       uuid.New().String(),
		Sequence: int64(len(fb.perRunAuditByRun[runID]) + 1),
		RunID:    runID.String(),
		Category: categoryPlanRevised,
	})
	fb.mu.Unlock()
}

// TestReviewStatusFor_Plan_PendingAfterRevise is the #1201 regression (the
// plan-stage analog of TestReviewStatusFor_Implement_PendingAfterFixup): after
// a fishhawk_revise_plan re-opens the plan gate, the stale round-1 plan_reviewed
// verdict must NOT read as terminal 'complete'. The terminal reads floor to the
// latest plan_revised, but the round-1 plan_review_started proxy stays unfloored,
// so with no re-review yet the status resolves to 'pending'. Before the fix this
// returned the stale round-1 verdict.
func TestReviewStatusFor_Plan_PendingAfterRevise(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	// Round 1: a review was dispatched and landed a reject verdict, then the
	// operator revised the plan to re-open the gate.
	seedReviewStartedAudit(fb, runID, "plan_review_started", 1, "advisory")
	seedPlanReviewAudit(fb, runID, PlanReview{ReviewerKind: "agent", Authority: "advisory", Verdict: "reject"})
	seedRunPlanRevisedAudit(fb, runID)
	r := newResolver(srv, nil)

	st, err := r.reviewStatusFor(context.Background(), runID, "plan")
	if err != nil {
		t.Fatalf("reviewStatusFor: %v", err)
	}
	if st.Status != "pending" {
		t.Errorf("Status = %q, want pending (the stale pre-revision verdict must not read complete)", st.Status)
	}
	if len(st.Reviews) != 0 {
		t.Errorf("Reviews = %+v, want empty while the re-review is in flight", st.Reviews)
	}
}

// TestReviewStatusFor_Plan_CompleteWithRound2Verdict pins that once the
// re-review of the revised plan lands, the status resolves to 'complete'
// carrying ONLY the round-2 verdict — the floored-out round-1 reject does not
// leak into Reviews.
func TestReviewStatusFor_Plan_CompleteWithRound2Verdict(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	seedReviewStartedAudit(fb, runID, "plan_review_started", 1, "advisory")
	seedPlanReviewAudit(fb, runID, PlanReview{ReviewerKind: "agent", Authority: "advisory", Verdict: "reject"}) // round 1: stale
	seedRunPlanRevisedAudit(fb, runID)
	// Round 2: the re-review of the revised plan lands a clean approve, with a
	// fresh started entry carrying the current round's configured count.
	seedReviewStartedAudit(fb, runID, "plan_review_started", 1, "advisory")
	seedPlanReviewAudit(fb, runID, PlanReview{ReviewerKind: "agent", Authority: "advisory", Verdict: "approve"})
	r := newResolver(srv, nil)

	st, err := r.reviewStatusFor(context.Background(), runID, "plan")
	if err != nil {
		t.Fatalf("reviewStatusFor: %v", err)
	}
	if st.Status != "complete" {
		t.Fatalf("Status = %q, want complete once the re-review lands", st.Status)
	}
	if len(st.Reviews) != 1 {
		t.Fatalf("Reviews = %+v, want only the round-2 verdict", st.Reviews)
	}
	if st.Reviews[0].Verdict != "approve" {
		t.Errorf("Reviews[0].Verdict = %q, want approve (round-2); the round-1 reject must be floored out", st.Reviews[0].Verdict)
	}
}

// --- count-based completeness (#1127) ---
//
// In the heterogeneous topology the reviewers run sequentially in one loop and
// each invocation takes minutes, so a poll can catch the window after the
// first reviewer's *_reviewed entry but before the second finishes. Before
// #1127 reviewStatusFor returned 'complete' on that first verdict, dropping the
// slower reviewer's verdict from the surface. The fix gates 'complete' on
// landed_terminal >= configured_agents.

// TestReviewStatusFor_Heterogeneous_PartialIsPending pins the core fix: with
// configured_agents=2 and only ONE verdict landed, the status is 'pending'
// (NOT complete) — the partial-landing window keeps polling.
func TestReviewStatusFor_Heterogeneous_PartialIsPending(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	seedReviewStartedAudit(fb, runID, "plan_review_started", 2, "advisory")
	seedPlanReviewAudit(fb, runID, PlanReview{
		ReviewerKind:  "agent",
		ReviewerModel: "claude-opus-4-8",
		Authority:     "advisory",
		Verdict:       "approve_with_concerns",
		Concerns:      []PlanReviewConcern{{Severity: "medium", Category: "scope", Note: "x"}},
	})
	r := newResolver(srv, nil)

	st, err := r.reviewStatusFor(context.Background(), runID, "plan")
	if err != nil {
		t.Fatalf("reviewStatusFor: %v", err)
	}
	if st.Status != "pending" {
		t.Errorf("Status = %q, want pending (1 of 2 configured reviewers landed)", st.Status)
	}
	if len(st.Reviews) != 0 {
		t.Errorf("Reviews = %+v, want empty while the round is in flight", st.Reviews)
	}
	if st.PollIntervalSeconds != suggestedReviewPollIntervalSeconds {
		t.Errorf("PollIntervalSeconds = %d, want %d on pending", st.PollIntervalSeconds, suggestedReviewPollIntervalSeconds)
	}
}

// TestReviewStatusFor_Heterogeneous_FullRoundCompletesWithBothVerdicts pins
// the full round: with configured_agents=2 and BOTH verdicts landed, the
// status is 'complete' carrying both rows verbatim — approve_with_concerns is
// NOT collapsed to a bare approve and the reject is present.
func TestReviewStatusFor_Heterogeneous_FullRoundCompletesWithBothVerdicts(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	seedReviewStartedAudit(fb, runID, "plan_review_started", 2, "advisory")
	seedPlanReviewAudit(fb, runID, PlanReview{
		ReviewerKind:  "agent",
		ReviewerModel: "claude-opus-4-8",
		Authority:     "advisory",
		Verdict:       "approve_with_concerns",
		Concerns:      []PlanReviewConcern{{Severity: "medium", Category: "scope", Note: "x"}},
	})
	seedPlanReviewAudit(fb, runID, PlanReview{
		ReviewerKind:  "agent",
		ReviewerModel: "gpt-5.5",
		Authority:     "advisory",
		Verdict:       "reject",
	})
	r := newResolver(srv, nil)

	st, err := r.reviewStatusFor(context.Background(), runID, "plan")
	if err != nil {
		t.Fatalf("reviewStatusFor: %v", err)
	}
	if st.Status != "complete" {
		t.Fatalf("Status = %q, want complete once both configured reviewers landed", st.Status)
	}
	if len(st.Reviews) != 2 {
		t.Fatalf("Reviews = %+v, want both reviewer rows", st.Reviews)
	}
	var sawOpusConcerns, sawCodexReject bool
	for _, rev := range st.Reviews {
		if rev.ReviewerModel == "claude-opus-4-8" && rev.Verdict == "approve_with_concerns" {
			sawOpusConcerns = true
		}
		if rev.ReviewerModel == "gpt-5.5" && rev.Verdict == "reject" {
			sawCodexReject = true
		}
	}
	if !sawOpusConcerns {
		t.Errorf("opus approve_with_concerns missing/collapsed; Reviews = %+v", st.Reviews)
	}
	if !sawCodexReject {
		t.Errorf("gpt-5.5 reject missing; Reviews = %+v", st.Reviews)
	}
}

// TestReviewStatusFor_MixedTerminal_CompleteWithReviewedAndFailedRows pins that
// ANY terminal kind counts toward the round: configured_agents=2 with one
// implement_reviewed approve + one implement_review_failed reaches the
// threshold and resolves 'complete' (a real verdict exists), with BOTH a
// reviewed row and a synthesized failed row in the union.
func TestReviewStatusFor_MixedTerminal_CompleteWithReviewedAndFailedRows(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	seedReviewStartedAudit(fb, runID, "implement_review_started", 2, "advisory")
	seedImplementReviewAudit(fb, runID, PlanReview{
		ReviewerKind:  "agent",
		ReviewerModel: "claude-opus-4-8",
		Authority:     "advisory",
		Verdict:       "approve",
	})
	seedReviewFailedAudit(fb, runID, "implement_review_failed",
		"review timed out: context deadline exceeded", "gpt-5.5", "advisory")
	r := newResolver(srv, nil)

	st, err := r.reviewStatusFor(context.Background(), runID, "implement")
	if err != nil {
		t.Fatalf("reviewStatusFor: %v", err)
	}
	if st.Status != "complete" {
		t.Fatalf("Status = %q, want complete (a real verdict exists alongside a failure)", st.Status)
	}
	if len(st.Reviews) != 2 {
		t.Fatalf("Reviews = %+v, want a reviewed row + a synthesized failed row", st.Reviews)
	}
	var sawApprove, sawFailed bool
	for _, rev := range st.Reviews {
		if rev.Verdict == "approve" {
			sawApprove = true
		}
		if rev.Verdict == "failed" && rev.Reason == "review timed out: context deadline exceeded" {
			sawFailed = true
		}
	}
	if !sawApprove || !sawFailed {
		t.Errorf("union must carry both the reviewed approve and the synthesized failed row; Reviews = %+v", st.Reviews)
	}
}

// TestReviewStatusFor_SingleReviewer_ResolvesImmediately is the homogeneous
// regression guard: configured_agents=1 with one reviewed entry still resolves
// 'complete' immediately (so a single-reviewer run never polls forever).
func TestReviewStatusFor_SingleReviewer_ResolvesImmediately(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	seedReviewStartedAudit(fb, runID, "plan_review_started", 1, "gating")
	seedPlanReviewAudit(fb, runID, PlanReview{ReviewerKind: "agent", Authority: "gating", Verdict: "approve"})
	r := newResolver(srv, nil)

	st, err := r.reviewStatusFor(context.Background(), runID, "plan")
	if err != nil {
		t.Fatalf("reviewStatusFor: %v", err)
	}
	if st.Status != "complete" {
		t.Errorf("Status = %q, want complete (1 of 1 configured reviewer landed)", st.Status)
	}
	if len(st.Reviews) != 1 || st.Reviews[0].Verdict != "approve" {
		t.Errorf("Reviews = %+v, want one approve verdict", st.Reviews)
	}
}

// TestReviewStatusFor_Fallback_NoStartedEntry pins the fail-safe (step 3): a
// reviewed entry with NO *_review_started entry (an old-run / malformed-payload
// path where ConfiguredAgents is absent or <=0) degrades to the prior
// complete-on-first-verdict predicate rather than stranding on 'pending'.
func TestReviewStatusFor_Fallback_NoStartedEntry(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	// No started entry — configured count is absent.
	seedPlanReviewAudit(fb, runID, PlanReview{ReviewerKind: "agent", Authority: "advisory", Verdict: "approve"})
	r := newResolver(srv, nil)

	st, err := r.reviewStatusFor(context.Background(), runID, "plan")
	if err != nil {
		t.Fatalf("reviewStatusFor: %v", err)
	}
	if st.Status != "complete" {
		t.Errorf("Status = %q, want complete (fallback predicate, no configured count)", st.Status)
	}
	if len(st.Reviews) != 1 {
		t.Errorf("Reviews = %+v, want the single decoded verdict", st.Reviews)
	}
}

// TestReviewStatusFor_Fallback_ZeroConfigured pins that a started entry with a
// non-positive configured_agents (a malformed/old payload) also degrades to
// the fallback predicate — never stranding on 'pending'.
func TestReviewStatusFor_Fallback_ZeroConfigured(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	seedReviewStartedAudit(fb, runID, "plan_review_started", 0, "advisory")
	seedPlanReviewAudit(fb, runID, PlanReview{ReviewerKind: "agent", Authority: "advisory", Verdict: "approve"})
	r := newResolver(srv, nil)

	st, err := r.reviewStatusFor(context.Background(), runID, "plan")
	if err != nil {
		t.Fatalf("reviewStatusFor: %v", err)
	}
	if st.Status != "complete" {
		t.Errorf("Status = %q, want complete (zero configured => fallback)", st.Status)
	}
}

// TestReviewStatusFor_Implement_PartialAfterFixupReadsRound2Count pins the
// fix-up-round interaction: the count gate must read the LATEST
// implement_review_started entry's ConfiguredAgents, so a re-review round with
// 2 configured reviewers stays 'pending' on a partial landing even though the
// round-1 started entry (also configured) sits below the fix-up floor.
func TestReviewStatusFor_Implement_PartialAfterFixupReadsRound2Count(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	// Round 1: a single-reviewer round completed, then a fix-up re-opened the
	// stage and dispatched a 2-reviewer re-review round.
	seedReviewStartedAudit(fb, runID, "implement_review_started", 1, "advisory")
	seedImplementReviewAudit(fb, runID, PlanReview{ReviewerKind: "agent", Authority: "advisory", Verdict: "approve"})
	seedRunFixupTriggeredAudit(fb, runID)
	seedReviewStartedAudit(fb, runID, "implement_review_started", 2, "advisory")
	// Only ONE of the re-review round's two reviewers has landed.
	seedImplementReviewAudit(fb, runID, PlanReview{ReviewerKind: "agent", Authority: "advisory", Verdict: "approve_with_concerns"})
	r := newResolver(srv, nil)

	st, err := r.reviewStatusFor(context.Background(), runID, "implement")
	if err != nil {
		t.Fatalf("reviewStatusFor: %v", err)
	}
	if st.Status != "pending" {
		t.Errorf("Status = %q, want pending (re-review round needs 2, only 1 landed)", st.Status)
	}
}

// TestRegisterTools_RegistersAwaitReview is a smoke test that the new tool
// registers without panicking and the SDK accepts its output schema (the
// harness rejects unrepresentable types, so this also exercises ReviewStatus
// + AwaitReviewOutput reflection).
func TestRegisterTools_RegistersAwaitReview(t *testing.T) {
	cfg := config{backendURL: "http://localhost:8080", apiToken: "tok"}
	srv := buildServer(cfg)
	// Reaching here without panic means AddTool accepted the await_review
	// (and review_status) output schemas — the SDK rejects unrepresentable
	// types at registration.
	registerTools(srv, &runResolver{api: newAPIClient(cfg), getenv: envFuncFromMap(nil)})
}
