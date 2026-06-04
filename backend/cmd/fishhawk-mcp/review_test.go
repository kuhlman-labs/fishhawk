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
	if got := clampAwaitTimeout(0); got != awaitReviewTimeoutDefault {
		t.Errorf("clampAwaitTimeout(0) = %d, want %d", got, awaitReviewTimeoutDefault)
	}
	if got := clampAwaitTimeout(99999); got != awaitReviewTimeoutMax {
		t.Errorf("clampAwaitTimeout(99999) = %d, want %d", got, awaitReviewTimeoutMax)
	}
	if got := clampAwaitTimeout(45); got != 45 {
		t.Errorf("clampAwaitTimeout(45) = %d, want 45", got)
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
