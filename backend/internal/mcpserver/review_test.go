package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
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
	// #2494: reviews[] has ONE shape — present and EMPTY, never nil/absent.
	if st.Reviews == nil {
		t.Error("Reviews is nil for none; it must be a present, empty slice")
	}
	if len(st.Reviews) != 0 {
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
	// #2494: reviews[] has ONE shape — present and EMPTY, never nil/absent.
	if st.Reviews == nil {
		t.Error("Reviews is nil for pending; it must be a present, empty slice")
	}
	if len(st.Reviews) != 0 {
		t.Errorf("Reviews should be empty for pending; got %+v", st.Reviews)
	}
}

// TestReviewStatus_ReviewsAlwaysPresent pins the ONE-shape contract (#2494):
// a none/pending ReviewStatus marshals with `"reviews":[]` — present and
// empty, never absent and never null — while the terminal statuses still carry
// their rows. This is the assertion that fails if `omitempty` is left on the
// field or a construction path leaves the slice nil, so it covers both halves
// of the change.
func TestReviewStatus_ReviewsAlwaysPresent(t *testing.T) {
	t.Run("none and pending marshal to an empty array", func(t *testing.T) {
		fb, srv := newFakeBackend(t)
		r := newResolver(srv, nil)

		noneRun := uuid.New()
		pendingRun := uuid.New()
		seedReviewStartedAudit(fb, pendingRun, "plan_review_started", 1, "advisory")

		for _, tc := range []struct {
			name       string
			runID      uuid.UUID
			wantStatus string
		}{
			{"none", noneRun, "none"},
			{"pending", pendingRun, "pending"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				st, err := r.reviewStatusFor(context.Background(), tc.runID, "plan")
				if err != nil {
					t.Fatalf("reviewStatusFor: %v", err)
				}
				if st.Status != tc.wantStatus {
					t.Fatalf("Status = %q, want %q", st.Status, tc.wantStatus)
				}
				b, err := json.Marshal(st)
				if err != nil {
					t.Fatalf("marshal ReviewStatus: %v", err)
				}
				if !strings.Contains(string(b), `"reviews":[]`) {
					t.Errorf("marshalled %s ReviewStatus = %s, want it to carry \"reviews\":[]", tc.wantStatus, b)
				}
			})
		}
	})

	t.Run("complete still carries its rows", func(t *testing.T) {
		fb, srv := newFakeBackend(t)
		runID := uuid.New()
		seedReviewStartedAudit(fb, runID, "plan_review_started", 1, "advisory")
		seedPlanReviewAudit(fb, runID, PlanReview{
			ReviewerKind: "agent",
			Authority:    "advisory",
			Verdict:      "approve",
		})
		r := newResolver(srv, nil)

		st, err := r.reviewStatusFor(context.Background(), runID, "plan")
		if err != nil {
			t.Fatalf("reviewStatusFor: %v", err)
		}
		if st.Status != "complete" || len(st.Reviews) != 1 {
			t.Fatalf("status/reviews = %q/%d, want complete/1", st.Status, len(st.Reviews))
		}
		b, err := json.Marshal(st)
		if err != nil {
			t.Fatalf("marshal ReviewStatus: %v", err)
		}
		if strings.Contains(string(b), `"reviews":[]`) {
			t.Errorf("marshalled complete ReviewStatus dropped its rows: %s", b)
		}
	})
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

// TestReviewStatusFor_Fallback_FailedOnly_NoStartedEntry is the #1472
// end-to-end surfacing test for the plan-decomposition parse failure. The
// server's runPlanReviews emits a plan_review_failed entry at the plan.Parse
// guard — BEFORE any plan_review_started entry (started is emitted later, only
// when a reviewer actually dispatches). So a single plan_review_failed entry
// with NO started entry must resolve to 'failed' via reviewStatusFallback
// (review.go:421-428, the any-failed->failed path), NOT the count-gated default
// at review.go:411 (which is unreachable here because configured<=0 routes to
// the fallback first; were the gate reached with configured>=2 it would strand
// on 'pending'). This pins that the operator-visible status cannot regress to
// 'none' or 'pending' for the silently-hung-review case the PR fixes.
func TestReviewStatusFor_Fallback_FailedOnly_NoStartedEntry(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	// Only a plan_review_failed entry — no plan_review_started (the parse
	// failure fires before the started proxy is emitted), so configured<=0 and
	// the fallback predicate owns the resolution.
	seedReviewFailedAudit(fb, runID, "plan_review_failed",
		"decomposition.sub_plans: file backend/internal/server/server.go is scoped by multiple slices (\"slice 1\", \"slice 2\"); keep all edits to one file in a single slice or re-slice along file boundaries",
		"", "gating")
	r := newResolver(srv, nil)

	st, err := r.reviewStatusFor(context.Background(), runID, "plan")
	if err != nil {
		t.Fatalf("reviewStatusFor: %v", err)
	}
	if st.Status != "failed" {
		t.Errorf("Status = %q, want failed (any-failed->failed via reviewStatusFallback, not none/pending)", st.Status)
	}
	if len(st.Reviews) != 1 || st.Reviews[0].Verdict != "failed" {
		t.Fatalf("Reviews = %+v, want one failed verdict", st.Reviews)
	}
	if !strings.Contains(st.Reviews[0].Reason, "scoped by multiple slices") ||
		!strings.Contains(st.Reviews[0].Reason, "re-slice along file boundaries") {
		t.Errorf("Reason = %q, want the validator's re-slice message surfaced to the operator", st.Reviews[0].Reason)
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

// TestClampAwaitTimeoutHeartbeat pins the token-OR-long_wait cap (#1963, #2490)
// across all five enumerated regimes (m1–m5). The raised 7200s cap applies when
// EITHER a progressToken heartbeat is present OR long_wait is set; neither keeps
// the byte-identical 360 default / 600 cap that fishhawk_merge_run's
// clampAwaitTimeout call sites depend on. The non-positive -> 360 default is
// unchanged in every regime.
func TestClampAwaitTimeoutHeartbeat(t *testing.T) {
	if awaitHeartbeatTimeoutMax != 7200 {
		t.Fatalf("awaitHeartbeatTimeoutMax = %d, want 7200", awaitHeartbeatTimeoutMax)
	}
	cases := []struct {
		name      string
		n         int
		heartbeat bool
		longWait  bool
		want      int
	}{
		// (m1) neither opt-in: byte-identical to clampAwaitTimeout (600 cap).
		{"m1 no-hb no-lw over-cap clamps to 600", 9999, false, false, 600},
		{"neither passthrough", 45, false, false, 45},
		// (m2) long_wait alone raises the cap to 7200 (the #2490 reachable knob).
		{"m2 lw-only over-cap clamps to 7200", 9999, false, true, 7200},
		{"m2 lw-only passthrough above old cap", 601, false, true, 601},
		// (m3) progressToken alone raises the cap to 7200.
		{"m3 hb-only over-cap clamps to 7200", 9999, true, false, 7200},
		// (m4) both set: no double-widening — still 7200, no interaction bug.
		{"m4 both over-cap clamps to 7200", 9999, true, true, 7200},
		{"m4 both at new cap", 7200, true, true, 7200},
		// (m5) non-positive -> 360 default in EACH of the four regimes.
		{"m5 default neither", 0, false, false, 360},
		{"m5 default lw-only", 0, false, true, 360},
		{"m5 default hb-only", 0, true, false, 360},
		{"m5 default both", -1, true, true, 360},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampAwaitTimeoutHeartbeat(tc.n, tc.heartbeat, tc.longWait); got != tc.want {
				t.Errorf("clampAwaitTimeoutHeartbeat(%d, %v, %v) = %d, want %d", tc.n, tc.heartbeat, tc.longWait, got, tc.want)
			}
			// The neither-opt-in branch must exactly mirror clampAwaitTimeout so
			// the token-less / merge_run cap can never silently diverge.
			if !tc.heartbeat && !tc.longWait {
				if got, want := clampAwaitTimeoutHeartbeat(tc.n, false, false), clampAwaitTimeout(tc.n); got != want {
					t.Errorf("neither-opt-in diverged from clampAwaitTimeout for %d: %d vs %d", tc.n, got, want)
				}
			}
		})
	}
}

// TestEffectiveAwaitCap pins the cap disjunction directly (#2490): 7200 when
// either signal is set, 600 when neither. This is the seam counterfactual c2
// deletes (the `|| longWait` disjunct).
func TestEffectiveAwaitCap(t *testing.T) {
	cases := []struct {
		heartbeat bool
		longWait  bool
		want      int
	}{
		{false, false, awaitReviewTimeoutMax},   // 600
		{false, true, awaitHeartbeatTimeoutMax}, // 7200 — the reachable knob
		{true, false, awaitHeartbeatTimeoutMax}, // 7200
		{true, true, awaitHeartbeatTimeoutMax},  // 7200
	}
	for _, tc := range cases {
		if got := effectiveAwaitCap(tc.heartbeat, tc.longWait); got != tc.want {
			t.Errorf("effectiveAwaitCap(%v, %v) = %d, want %d", tc.heartbeat, tc.longWait, got, tc.want)
		}
	}
}

// awaitReviewHeartbeatFake seeds a pending implement review and flips it to
// complete when the started-category query count reaches settleAt — the pass
// with started-query count settleAt+1 (the NEXT reviewStatusFor pass) then
// observes the reviewed entry and resolves. Since the poll loop emits exactly
// one heartbeat per tick and resolves on the settleAt-th tick, this yields
// EXACTLY settleAt heartbeats. Returns (fb, resolver, runID).
func awaitReviewHeartbeatFake(t *testing.T, settleAt int64) (*fakeBackend, *runResolver, uuid.UUID) {
	t.Helper()
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	seedReviewStartedAudit(fb, runID, "implement_review_started", 1, "advisory")

	var startedQueries atomic.Int64
	fb.reviewFlip = func(category string) {
		// Mutates perRunAuditByRun under fb.mu (the handler holds it).
		if category == "implement_review_started" && startedQueries.Add(1) == settleAt {
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
	return fb, r, runID
}

// TestAwaitReview_ProgressHeartbeat_RealMCPBoundary is the mode-1 done-means
// test (#1963): a client holding one long fishhawk_await_review call open with a
// progressToken receives a keep-alive heartbeat once per poll tick, each echoing
// the request token and carrying awaitReviewProgressMessage content. Exercises
// the REAL MCP boundary — in-memory client/server transports + a
// ProgressNotificationHandler — mirroring TestDriveRun_ProgressHeartbeat.
func TestAwaitReview_ProgressHeartbeat_RealMCPBoundary(t *testing.T) {
	ctx := context.Background()
	const wantHeartbeats = 3
	_, r, runID := awaitReviewHeartbeatFake(t, wantHeartbeats)

	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "0"}, nil)
	registerAwaitReview(server, r)

	var mu sync.Mutex
	var notes []*mcp.ProgressNotificationParams
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
			mu.Lock()
			notes = append(notes, req.Params)
			mu.Unlock()
		},
	})
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer serverSession.Close()
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer clientSession.Close()

	params := &mcp.CallToolParams{
		Name:      "fishhawk_await_review",
		Arguments: map[string]any{"run_id": runID.String(), "stage": "implement", "timeout_seconds": 5},
	}
	params.SetProgressToken("review-tok-1")
	res, err := clientSession.CallTool(ctx, params)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("CallTool returned IsError; content: %+v", res.Content)
	}
	// The supplied-progressToken result must report heartbeat=true and the
	// raised 7200s cap (#2490).
	if raw, merr := json.Marshal(res.StructuredContent); merr != nil {
		t.Fatalf("marshal StructuredContent: %v", merr)
	} else {
		if !strings.Contains(string(raw), `"heartbeat":true`) {
			t.Errorf("supplied-token result should report heartbeat=true; got %s", raw)
		}
		if !strings.Contains(string(raw), `"timeout_cap_seconds":7200`) {
			t.Errorf("supplied-token result should report timeout_cap_seconds=7200; got %s", raw)
		}
	}

	// Notifications are delivered async; wait for all wantHeartbeats to flush.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(notes)
		mu.Unlock()
		if n >= wantHeartbeats {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(notes) != wantHeartbeats {
		t.Fatalf("received %d progress notifications at the real MCP boundary; want exactly %d (one per poll tick)", len(notes), wantHeartbeats)
	}
	for i, n := range notes {
		if n.ProgressToken != "review-tok-1" {
			t.Errorf("notification[%d] progressToken = %v, want the request token review-tok-1", i, n.ProgressToken)
		}
		if !strings.HasPrefix(n.Message, "await_review: implement review pending") {
			t.Errorf("notification[%d] message = %q, want awaitReviewProgressMessage content", i, n.Message)
		}
		if n.Progress != float64(i+1) {
			t.Errorf("notification[%d] progress = %v, want %d (one increment per poll tick)", i, n.Progress, i+1)
		}
	}
}

// TestAwaitReview_ProgressHeartbeat_NoToken_NoEmission is the mode-2 opt-in
// proof (#1963): a real CallTool that supplies NO progressToken receives ZERO
// progress notifications and still resolves to the normal complete result.
func TestAwaitReview_ProgressHeartbeat_NoToken_NoEmission(t *testing.T) {
	ctx := context.Background()
	_, r, runID := awaitReviewHeartbeatFake(t, 2)

	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "0"}, nil)
	registerAwaitReview(server, r)

	var mu sync.Mutex
	var notes int
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, _ *mcp.ProgressNotificationClientRequest) {
			mu.Lock()
			notes++
			mu.Unlock()
		},
	})
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer serverSession.Close()
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer clientSession.Close()

	// No SetProgressToken: the opt-in is not exercised.
	res, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name:      "fishhawk_await_review",
		Arguments: map[string]any{"run_id": runID.String(), "stage": "implement", "timeout_seconds": 5},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("CallTool returned IsError; content: %+v", res.Content)
	}
	// The result must still be the normal complete verdict.
	raw, merr := json.Marshal(res.StructuredContent)
	if merr != nil {
		t.Fatalf("marshal StructuredContent: %v", merr)
	}
	if !strings.Contains(string(raw), `"status":"complete"`) {
		t.Errorf("no-token result should still resolve complete; got %s", raw)
	}
	// No token and no long_wait: heartbeat=false and the unchanged 600s cap (#2490).
	if !strings.Contains(string(raw), `"heartbeat":false`) {
		t.Errorf("no-token result should report heartbeat=false; got %s", raw)
	}
	if !strings.Contains(string(raw), `"timeout_cap_seconds":600`) {
		t.Errorf("no-token result should report timeout_cap_seconds=600; got %s", raw)
	}
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if notes != 0 {
		t.Errorf("received %d progress notifications with no progressToken; want 0 (opt-in)", notes)
	}
}

// TestAwaitReview_LongWait_NoToken_RaisesCapNoEmission is the #2490 reachability
// proof at the real MCP boundary: a CallTool that sets long_wait:true but
// supplies NO progressToken reports the raised 7200s cap with heartbeat=false
// and receives ZERO progress notifications — long_wait widens the cap without
// ever emitting against a token that does not exist. This is the counterfactual
// vehicle for c2 (the `|| longWait` disjunct) and c4 (the progToken != nil
// emission guard).
func TestAwaitReview_LongWait_NoToken_RaisesCapNoEmission(t *testing.T) {
	ctx := context.Background()
	// Timeout is small so the fast path resolves complete quickly; the cap
	// reported is effectiveAwaitCap, independent of the elapsed wait.
	_, r, runID := awaitReviewHeartbeatFake(t, 2)

	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "0"}, nil)
	registerAwaitReview(server, r)

	var mu sync.Mutex
	var notes int
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, _ *mcp.ProgressNotificationClientRequest) {
			mu.Lock()
			notes++
			mu.Unlock()
		},
	})
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer serverSession.Close()
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer clientSession.Close()

	// long_wait:true, NO SetProgressToken.
	res, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name:      "fishhawk_await_review",
		Arguments: map[string]any{"run_id": runID.String(), "stage": "implement", "timeout_seconds": 5, "long_wait": true},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("CallTool returned IsError; content: %+v", res.Content)
	}
	raw, merr := json.Marshal(res.StructuredContent)
	if merr != nil {
		t.Fatalf("marshal StructuredContent: %v", merr)
	}
	if !strings.Contains(string(raw), `"timeout_cap_seconds":7200`) {
		t.Errorf("long_wait result should report the raised timeout_cap_seconds=7200; got %s", raw)
	}
	if !strings.Contains(string(raw), `"heartbeat":false`) {
		t.Errorf("long_wait-without-token result should report heartbeat=false; got %s", raw)
	}
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if notes != 0 {
		t.Errorf("received %d progress notifications with long_wait and no progressToken; want 0 (long_wait must never emit against a token that does not exist)", notes)
	}
}

// errNotifySession stands up a real-but-closed MCP server session whose
// NotifyProgress errors on every call, so the heartbeat emission's error-swallow
// resilience (binding condition) can be pinned deterministically.
func errNotifySession(t *testing.T) *mcp.ServerSession {
	t.Helper()
	ctx := context.Background()
	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "0"}, nil)
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	// Tear both sides down so any subsequent NotifyProgress write errors.
	_ = clientSession.Close()
	_ = serverSession.Close()
	return serverSession
}

// TestAwaitReview_ProgressHeartbeat_NotifyErrorDoesNotFailWait pins the binding
// condition (#1963): a heartbeat emission whose NotifyProgress FAILS (here, a
// closed session/transport that errors on every notification) must NOT terminate
// or fail the await — the wait still reaches its terminal complete result.
// Without this test the non-authoritative-heartbeat behavior is unpinned and a
// future refactor could silently make a notify error fatal.
func TestAwaitReview_ProgressHeartbeat_NotifyErrorDoesNotFailWait(t *testing.T) {
	_, r, runID := awaitReviewHeartbeatFake(t, 2)

	sess := errNotifySession(t)
	// A request carrying a progressToken AND the closed session: every tick
	// attempts a NotifyProgress that errors. The swallowed error must not break
	// the wait.
	req := &mcp.CallToolRequest{
		Session: sess,
		Params:  &mcp.CallToolParamsRaw{Name: "fishhawk_await_review"},
	}
	req.Params.SetProgressToken("err-tok")

	_, out, err := r.awaitReview(context.Background(), req, AwaitReviewInput{
		RunID:          runID.String(),
		Stage:          "implement",
		TimeoutSeconds: 5,
	})
	if err != nil {
		t.Fatalf("awaitReview returned error despite a swallowed notify failure: %v", err)
	}
	if out.Status != "complete" {
		t.Fatalf("Status = %q, want complete — the wait must reach its terminal result despite notify errors", out.Status)
	}
	if len(out.Reviews) != 1 || out.Reviews[0].Verdict != "approve" {
		t.Errorf("Reviews = %+v, want one approve verdict", out.Reviews)
	}
}

// TestAwaitReviewProgressMessage pins the pure heartbeat-message helper,
// including the #1915 terminalInFlight note.
func TestAwaitReviewProgressMessage(t *testing.T) {
	base := awaitReviewProgressMessage("implement", 12*time.Second, false)
	if base != "await_review: implement review pending; elapsed 12s" {
		t.Errorf("base message = %q", base)
	}
	tif := awaitReviewProgressMessage("plan", 3*time.Second, true)
	if !strings.HasPrefix(tif, "await_review: plan review pending; elapsed 3s") ||
		!strings.Contains(tif, "run terminal with review still in flight") {
		t.Errorf("terminalInFlight message = %q", tif)
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

// TestAwaitRunTerminalBackstop_NoReviewInFlight_ResolvesEarly unit-pins the
// #874 early-resolve arm as refined by #1915: on a terminal run with NO
// dispatched review in flight (a non-pending status), the backstop resolves
// the wait immediately and reports terminalInFlight=false. The message names
// the non-progress reason. This is the defensive branch the caller cannot
// reach through the normal 'pending'-only invocation, so it is exercised
// directly.
func TestAwaitRunTerminalBackstop_NoReviewInFlight_ResolvesEarly(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), State: "failed"}
	r := newResolver(srv, nil)

	st := &ReviewStatus{Stage: "plan", Status: "none"}
	out, done, tif := r.awaitRunTerminalBackstop(context.Background(), runID, "plan", st, time.Now(), false, awaitReviewTimeoutMax)
	if !done {
		t.Fatal("backstop should resolve early on a terminal run with no review in flight")
	}
	if tif {
		t.Error("terminalInFlight should be false when no review is in flight")
	}
	if !strings.Contains(out.Message, "can no longer progress") {
		t.Errorf("early-resolve message should explain the review can no longer progress: %q", out.Message)
	}
	// The observed regime is carried on the early-resolve path too (#2490).
	if out.Heartbeat {
		t.Error("Heartbeat should be false (no token passed)")
	}
	if out.TimeoutCapSeconds != awaitReviewTimeoutMax {
		t.Errorf("TimeoutCapSeconds = %d, want %d", out.TimeoutCapSeconds, awaitReviewTimeoutMax)
	}
}

// TestAwaitRunTerminalBackstop_InFlightReview_KeepsPolling unit-pins the #1915
// keep-polling arm: on a terminal run with a review STILL in flight (a pending
// status), the backstop does NOT resolve — it reports done=false with
// terminalInFlight=true so the caller keeps polling (the verdict is recorded
// unguarded and WILL land) and a later timeout can name fishhawk_revive_run.
func TestAwaitRunTerminalBackstop_InFlightReview_KeepsPolling(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), State: "failed"}
	r := newResolver(srv, nil)

	st := &ReviewStatus{Stage: "implement", Status: "pending"}
	_, done, tif := r.awaitRunTerminalBackstop(context.Background(), runID, "implement", st, time.Now(), false, awaitReviewTimeoutMax)
	if done {
		t.Fatal("backstop must NOT resolve early while a review is in flight on a terminal run (#1915)")
	}
	if !tif {
		t.Error("terminalInFlight should be true (terminal run + in-flight review)")
	}
}

// TestAwaitReview_TerminalRun_InFlightReview_KeepsPollingThenResolves is the
// #1915 m1 behavioral proof: a run already terminal-failed at call time with a
// review still in flight (a plan_review_started marker, no verdict) does NOT
// short-circuit to 'can no longer progress' anymore — the wait keeps polling
// and returns the verdict once the fake backend lands it mid-wait. This is the
// operator-visible never-freeze behavior: a sibling stage's failure flipping
// the run terminal no longer abandons a healthy stage's in-flight review.
func TestAwaitReview_TerminalRun_InFlightReview_KeepsPollingThenResolves(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	// Terminal-failed run, review dispatched (=> pending) but no verdict yet.
	fb.getRunByID[runID] = Run{ID: runID.String(), State: "failed"}
	seedReviewStartedAudit(fb, runID, "implement_review_started", 1, "advisory")

	flipped := false
	fb.reviewFlip = func(category string) {
		// Land the verdict on the started-category query (the last query of a
		// reviewStatusFor pass), flipping the NEXT pass to complete. Mutates
		// under fb.mu (the handler holds it).
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

	// Large timeout: if the wait resolved early on the terminal run the verdict
	// would never be observed; a prompt 'complete' is proof it kept polling.
	_, out, err := r.awaitReview(context.Background(), nil, AwaitReviewInput{
		RunID:          runID.String(),
		Stage:          "implement",
		TimeoutSeconds: 5,
	})
	if err != nil {
		t.Fatalf("awaitReview: %v", err)
	}
	if out.Status != "complete" {
		t.Fatalf("Status = %q, want complete (terminal run must keep polling while the review is in flight)", out.Status)
	}
	if len(out.Reviews) != 1 || out.Reviews[0].Verdict != "approve" {
		t.Errorf("Reviews = %+v, want the landed approve verdict", out.Reviews)
	}
}

// TestAwaitReview_TerminalRun_InFlightReview_TimeoutNamesRevive is the #1915 m2
// proof: when the run is terminal, the review stays in flight, and the deadline
// elapses, the wait returns status 'pending' with a message that explains the
// run is terminal but the review is still in flight and names
// fishhawk_revive_run for re-admitting the run.
func TestAwaitReview_TerminalRun_InFlightReview_TimeoutNamesRevive(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), State: "failed"}
	seedReviewStartedAudit(fb, runID, "plan_review_started", 1, "gating")

	r := newResolver(srv, nil)
	r.reviewPollInterval = 100 * time.Microsecond

	// Drive the deadline deterministically (#729): terminalInFlight is captured
	// by the pre-loop backstop (run already terminal + pending), then cancel on
	// the 2nd started query so the loop times out without a wall-clock wait.
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
	if !strings.Contains(out.Message, "fishhawk_revive_run") {
		t.Errorf("timeout message should name fishhawk_revive_run for a terminal run with an in-flight review: %q", out.Message)
	}
	if !strings.Contains(out.Message, "terminal") {
		t.Errorf("timeout message should explain the run is terminal: %q", out.Message)
	}
}

// TestAwaitReview_TerminalRunMidLoop_KeepsPolling pins the IN-LOOP #1915 arm:
// the run is non-terminal at call time (the pre-loop backstop sees "running"
// and the loop is entered), then transitions to terminal mid-loop while the
// review is still pending. The in-loop backstop must set terminalInFlight and
// KEEP polling rather than resolve — proven by the verdict landing afterward
// and resolving to 'complete'. A regression that resolved early in-loop would
// return 'pending' (or the stale non-progress message) here.
func TestAwaitReview_TerminalRunMidLoop_KeepsPolling(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), State: "running"}
	seedReviewStartedAudit(fb, runID, "implement_review_started", 1, "advisory")

	// started query #1 is the fast path (run "running"); #2 is the first poll
	// tick — flip the run terminal then, AFTER the pre-loop backstop already
	// observed "running". Land the verdict on query #3 so the in-loop backstop
	// runs at least once on the terminal run before the wait resolves.
	var startedQueries atomic.Int64
	fb.reviewFlip = func(category string) {
		if category != "implement_review_started" {
			return
		}
		switch startedQueries.Add(1) {
		case 2:
			fb.getRunByID[runID] = Run{ID: runID.String(), State: "failed"}
		case 3:
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
		t.Fatalf("Status = %q, want complete (in-loop terminal transition must keep polling)", out.Status)
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

// TestAwaitReview_CarriesConcernEvidence is the done-means for the surface
// #2353 was reported from: fishhawk_await_review is what the operator reads
// when a reviewer rejects, and it rendered `note` alone — so a rejection whose
// substance sat in new_evidence read as an unsupported assertion.
//
// The payload is seeded with RAW WIRE KEYS, so this exercises the actual decode
// (a json-tag typo goes RED here). It also proves the RETROACTIVE reach of this
// leg: nothing is persisted, the fields are read straight off the
// implement_reviewed audit payload, so runs that predate migration 0069 gain
// the evidence on this surface too.
func TestAwaitReview_CarriesConcernEvidence(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	const evidence = "run 90e0ea6a's gate view showed note-only; the verdict payload carried 340 bytes of new_evidence"
	settledRef := uuid.New().String()
	seedRawReviewAudit(fb, runID, "implement_reviewed",
		concernEvidenceWirePayload("reject", evidence, settledRef))
	r := newResolver(srv, nil)

	_, out, err := r.awaitReview(context.Background(), nil, AwaitReviewInput{RunID: runID.String(), Stage: "implement"})
	if err != nil {
		t.Fatalf("awaitReview: %v", err)
	}
	if out.Status != "complete" {
		t.Fatalf("Status = %q, want complete", out.Status)
	}
	assertConcernEvidenceDecoded(t, "await_review", out.Reviews, evidence, settledRef)
}

// --- #2712: restart-strand probe ---

// strandBootA / strandBootB are two fishhawkd boot instants with a review
// dispatch between them: a round started at strandDispatch belongs to the
// process that booted at A, and is ORPHANED once the daemon has restarted and
// reports B.
var (
	strandBootA    = time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)
	strandDispatch = strandBootA.Add(10 * time.Minute)
	strandBootB    = strandBootA.Add(30 * time.Minute)
)

// seedReviewStartedAuditAt is seedReviewStartedAudit with an explicit dispatch
// TIMESTAMP — the field the restart-strand probe compares against the daemon's
// boot instant.
func seedReviewStartedAuditAt(fb *fakeBackend, runID uuid.UUID, category string, configuredAgents int, authority string, ts time.Time) {
	payload, _ := json.Marshal(map[string]any{
		"configured_agents": configuredAgents,
		"authority":         authority,
	})
	var decoded any
	_ = json.Unmarshal(payload, &decoded)
	fb.mu.Lock()
	fb.perRunAuditByRun[runID] = append(fb.perRunAuditByRun[runID], AuditEntry{
		ID:        uuid.New().String(),
		Sequence:  int64(len(fb.perRunAuditByRun[runID]) + 1),
		RunID:     runID.String(),
		Timestamp: ts,
		Category:  category,
		Payload:   decoded,
	})
	fb.mu.Unlock()
}

// TestReviewRoundStrand_Branches pins ONE behavioral assertion per named
// branch of reviewRoundStrand (#2712). The undecidable cases are the
// fail-open direction: an unreadable boundary must never manufacture a strand.
func TestReviewRoundStrand_Branches(t *testing.T) {
	cases := []struct {
		name            string
		seed            func(fb *fakeBackend, runID uuid.UUID)
		healthzStart    string
		healthzStatus   int
		healthzBody     string
		wantStranded    bool
		wantUndecidable bool
	}{
		{
			name:         "no started entry is not stranded",
			seed:         func(*fakeBackend, uuid.UUID) {},
			healthzStart: strandBootB.Format(time.RFC3339Nano),
		},
		{
			name: "zero configured agents is not stranded",
			seed: func(fb *fakeBackend, runID uuid.UUID) {
				seedReviewStartedAuditAt(fb, runID, "plan_review_started", 0, "advisory", strandDispatch)
			},
			healthzStart: strandBootB.Format(time.RFC3339Nano),
		},
		{
			name: "fully landed round is not stranded",
			seed: func(fb *fakeBackend, runID uuid.UUID) {
				seedReviewStartedAuditAt(fb, runID, "plan_review_started", 1, "advisory", strandDispatch)
				seedPlanReviewAudit(fb, runID, PlanReview{ReviewerKind: "agent", ReviewerModel: "claude-fable-5", Verdict: "approve"})
			},
			healthzStart: strandBootB.Format(time.RFC3339Nano),
		},
		{
			name: "unrestarted daemon is not stranded",
			seed: func(fb *fakeBackend, runID uuid.UUID) {
				seedReviewStartedAuditAt(fb, runID, "plan_review_started", 2, "advisory", strandDispatch)
				seedPlanReviewAudit(fb, runID, PlanReview{ReviewerKind: "agent", ReviewerModel: "claude-fable-5", Verdict: "approve_with_concerns"})
			},
			// Boot BEFORE the dispatch: the process serving is the one that
			// dispatched the round, so its reviewers are genuinely running.
			healthzStart: strandBootA.Format(time.RFC3339Nano),
		},
		{
			name: "healthz transport error is undecidable",
			seed: func(fb *fakeBackend, runID uuid.UUID) {
				seedReviewStartedAuditAt(fb, runID, "plan_review_started", 2, "advisory", strandDispatch)
				seedPlanReviewAudit(fb, runID, PlanReview{ReviewerKind: "agent", ReviewerModel: "claude-fable-5", Verdict: "approve"})
			},
			healthzStatus:   http.StatusInternalServerError,
			wantUndecidable: true,
		},
		{
			name: "healthz without process_start is undecidable",
			seed: func(fb *fakeBackend, runID uuid.UUID) {
				seedReviewStartedAuditAt(fb, runID, "plan_review_started", 2, "advisory", strandDispatch)
				seedPlanReviewAudit(fb, runID, PlanReview{ReviewerKind: "agent", ReviewerModel: "claude-fable-5", Verdict: "approve"})
			},
			healthzStart:    "",
			wantUndecidable: true,
		},
		{
			name: "unparseable process_start is undecidable",
			seed: func(fb *fakeBackend, runID uuid.UUID) {
				seedReviewStartedAuditAt(fb, runID, "plan_review_started", 2, "advisory", strandDispatch)
				seedPlanReviewAudit(fb, runID, PlanReview{ReviewerKind: "agent", ReviewerModel: "claude-fable-5", Verdict: "approve"})
			},
			healthzBody:     `{"status":"ok","process_start":"not-a-timestamp"}`,
			wantUndecidable: true,
		},
		{
			name: "restarted daemon with a partial landing is stranded",
			seed: func(fb *fakeBackend, runID uuid.UUID) {
				seedReviewStartedAuditAt(fb, runID, "plan_review_started", 2, "advisory", strandDispatch)
				seedPlanReviewAudit(fb, runID, PlanReview{ReviewerKind: "agent", ReviewerModel: "claude-fable-5", Verdict: "approve_with_concerns"})
			},
			healthzStart: strandBootB.Format(time.RFC3339Nano),
			wantStranded: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fb, srv := newFakeBackend(t)
			runID := uuid.New()
			fb.healthzProcessStart = tc.healthzStart
			fb.healthzBody = tc.healthzBody
			if tc.healthzStatus != 0 {
				fb.healthzStatus = tc.healthzStatus
			}
			tc.seed(fb, runID)

			r := newResolver(srv, nil)
			boundary := r.probeHealthBoundary(context.Background(), time.Now())
			got, err := r.reviewRoundStrand(context.Background(), runID, "plan", boundary)
			if err != nil {
				t.Fatalf("reviewRoundStrand: %v", err)
			}
			if got.Stranded != tc.wantStranded {
				t.Errorf("Stranded = %v, want %v (reason %q)", got.Stranded, tc.wantStranded, got.Reason)
			}
			if got.Undecidable != tc.wantUndecidable {
				t.Errorf("Undecidable = %v, want %v (reason %q)", got.Undecidable, tc.wantUndecidable, got.Reason)
			}
			if got.Undecidable && got.Reason == "" {
				t.Error("an undecidable verdict must carry a reason")
			}
			if got.Undecidable && !got.DaemonProcessStart.IsZero() {
				t.Errorf("an undecidable verdict must not publish a boundary; got %v", got.DaemonProcessStart)
			}
		})
	}
}

// TestReviewRoundStrand_FullyLandedNotStranded is the dedicated counterfactual
// vehicle for the landed<configured conjunct (control (b)): a round whose
// reviewers ALL landed is not stranded even under a restarted daemon —
// deleting that conjunct would report a settled round as stranded.
func TestReviewRoundStrand_FullyLandedNotStranded(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.healthzProcessStart = strandBootB.Format(time.RFC3339Nano)
	seedReviewStartedAuditAt(fb, runID, "plan_review_started", 2, "advisory", strandDispatch)
	seedPlanReviewAudit(fb, runID, PlanReview{ReviewerKind: "agent", ReviewerModel: "claude-fable-5", Verdict: "approve"})
	seedPlanReviewAudit(fb, runID, PlanReview{ReviewerKind: "agent", ReviewerModel: "gpt-5.6-sol", Verdict: "approve"})

	r := newResolver(srv, nil)
	got, err := r.reviewRoundStrand(context.Background(), runID, "plan",
		r.probeHealthBoundary(context.Background(), time.Now()))
	if err != nil {
		t.Fatalf("reviewRoundStrand: %v", err)
	}
	if got.Stranded {
		t.Fatalf("Stranded = true for a fully-landed round (%d of %d); want false",
			got.LandedTerminal, got.ConfiguredAgents)
	}
}

// TestReviewRoundStrand_UnrestartedDaemonNotStranded is acceptance criterion
// 4's control (counterfactual (c)): a genuinely in-flight review under a daemon
// that has NOT restarted must never be reported as stranded. Deleting the
// startedAt.Before(processStart) conjunct turns this RED.
func TestReviewRoundStrand_UnrestartedDaemonNotStranded(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	// The serving daemon booted BEFORE the review was dispatched.
	fb.healthzProcessStart = strandBootA.Format(time.RFC3339Nano)
	seedReviewStartedAuditAt(fb, runID, "plan_review_started", 2, "advisory", strandDispatch)
	seedPlanReviewAudit(fb, runID, PlanReview{ReviewerKind: "agent", ReviewerModel: "claude-fable-5", Verdict: "approve_with_concerns"})

	r := newResolver(srv, nil)
	got, err := r.reviewRoundStrand(context.Background(), runID, "plan",
		r.probeHealthBoundary(context.Background(), time.Now()))
	if err != nil {
		t.Fatalf("reviewRoundStrand: %v", err)
	}
	if got.Stranded {
		t.Fatal("Stranded = true for a review dispatched by the SERVING daemon; the reviewer is alive, not orphaned")
	}
	if got.Undecidable {
		t.Fatalf("Undecidable = true; want a positive not-stranded verdict (reason %q)", got.Reason)
	}
}

// TestReviewRoundStrand_HealthzUnreachableIsUndecidable is counterfactual (d)'s
// first vehicle: an unreachable /healthz must be UNDECIDABLE, never a zero
// boundary — a zero time.Time compares as before every audit entry and would
// convert every pending review into a false strand.
func TestReviewRoundStrand_HealthzUnreachableIsUndecidable(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.healthzStatus = http.StatusServiceUnavailable
	seedReviewStartedAuditAt(fb, runID, "plan_review_started", 2, "advisory", strandDispatch)
	seedPlanReviewAudit(fb, runID, PlanReview{ReviewerKind: "agent", ReviewerModel: "claude-fable-5", Verdict: "approve"})

	r := newResolver(srv, nil)
	got, err := r.reviewRoundStrand(context.Background(), runID, "plan",
		r.probeHealthBoundary(context.Background(), time.Now()))
	if err != nil {
		t.Fatalf("reviewRoundStrand: %v", err)
	}
	if got.Stranded {
		t.Fatal("Stranded = true with an UNREADABLE restart boundary; the probe fabricated a strand")
	}
	if !got.Undecidable {
		t.Fatal("Undecidable = false with an unreachable /healthz; want the fail-open degrade")
	}
}

// TestAwaitReview_UnreachableHealthz_StillWaits is counterfactual (d)'s second
// vehicle at the await surface: with the boundary unreadable the wait behaves
// exactly as it did before this diagnostic existed — it keeps polling and
// times out 'pending', never resolving 'stranded'.
func TestAwaitReview_UnreachableHealthz_StillWaits(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.healthzStatus = http.StatusServiceUnavailable
	seedReviewStartedAuditAt(fb, runID, "plan_review_started", 2, "advisory", strandDispatch)
	seedPlanReviewAudit(fb, runID, PlanReview{ReviewerKind: "agent", ReviewerModel: "claude-fable-5", Verdict: "approve_with_concerns"})

	r := newResolver(srv, nil)
	r.reviewPollInterval = 100 * time.Microsecond
	r.strandProbeTTL = time.Nanosecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var startedQueries atomic.Int64
	fb.reviewFlip = func(category string) {
		if category == "plan_review_started" && startedQueries.Add(1) >= 4 {
			cancel()
		}
	}

	_, out, err := r.awaitReview(ctx, nil, AwaitReviewInput{
		RunID: runID.String(), Stage: "plan", TimeoutSeconds: 600,
	})
	if err != nil {
		t.Fatalf("awaitReview: %v", err)
	}
	if out.Status != "pending" {
		t.Fatalf("Status = %q, want pending — an unverifiable boundary must not resolve the wait", out.Status)
	}
	if out.Stranded {
		t.Error("Stranded = true with an unreachable /healthz")
	}
	if !out.Undecidable {
		t.Error("Undecidable = false; the timeout output must report that the daemon could not be verified")
	}
	if strings.Contains(out.Message, "genuinely still running") {
		t.Errorf("the undecidable timeout message must NOT assert the reviewer is alive: %q", out.Message)
	}
	if !strings.Contains(out.Message, "fishhawk_reconcile_reviews") {
		t.Errorf("the undecidable timeout message should name the recovery verb: %q", out.Message)
	}
}

// TestAwaitReview_StrandedResolvesImmediately covers the first-round-trip
// resolution: an ALREADY-stranded round returns 'stranded' without burning the
// timeout, and the message names the shortfall in concrete terms.
func TestAwaitReview_StrandedResolvesImmediately(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.healthzProcessStart = strandBootB.Format(time.RFC3339Nano)
	seedReviewStartedAuditAt(fb, runID, "plan_review_started", 2, "advisory", strandDispatch)
	seedPlanReviewAudit(fb, runID, PlanReview{ReviewerKind: "agent", ReviewerModel: "claude-fable-5", Verdict: "approve_with_concerns"})

	r := newResolver(srv, nil)
	r.reviewPollInterval = 10 * time.Millisecond

	start := time.Now()
	_, out, err := r.awaitReview(context.Background(), nil, AwaitReviewInput{
		RunID: runID.String(), Stage: "plan", TimeoutSeconds: 600,
	})
	if err != nil {
		t.Fatalf("awaitReview: %v", err)
	}
	if out.Status != "stranded" || !out.Stranded {
		t.Fatalf("Status = %q / Stranded = %v, want stranded", out.Status, out.Stranded)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("await took %v, want an immediate resolution rather than the full timeout", elapsed)
	}
	if out.LandedTerminal != 1 || out.ConfiguredAgents != 2 {
		t.Errorf("landed/configured = %d/%d, want 1/2", out.LandedTerminal, out.ConfiguredAgents)
	}
	if out.DaemonRestartedAt == "" {
		t.Error("a stranded result must publish the daemon's boot instant")
	}
	// The shortfall must be CONCRETE — the silent two-reviewers-to-one
	// degradation is half of what makes this failure dangerous.
	for _, want := range []string{"1 of 2 configured reviewers", "claude-fable-5", "fishhawk_reconcile_reviews", "preserved"} {
		if !strings.Contains(out.Message, want) {
			t.Errorf("stranded message missing %q: %q", want, out.Message)
		}
	}
	if strings.Contains(out.Message, "genuinely still running") {
		t.Errorf("a stranded result must not claim the reviewer is running: %q", out.Message)
	}
}

// TestAwaitReview_BoundaryChangesMidWait_SameCallDetectsStrand is BINDING
// CONDITION 1's acceptance test. The wait begins under boundary A — the daemon
// that dispatched the round, so the round is NOT stranded and polling starts —
// and the boundary CHANGES to B mid-wait (the operator lands a sibling campaign
// item with `scripts/dev post-merge` while this review is in flight). The SAME
// in-flight call must detect the strand and return promptly; a boundary sampled
// once at call start and pinned for the call could never see it.
func TestAwaitReview_BoundaryChangesMidWait_SameCallDetectsStrand(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	// Boundary A: booted BEFORE the dispatch — reviewers alive, wait proceeds.
	fb.healthzProcessStart = strandBootA.Format(time.RFC3339Nano)
	seedReviewStartedAuditAt(fb, runID, "plan_review_started", 2, "advisory", strandDispatch)
	seedPlanReviewAudit(fb, runID, PlanReview{ReviewerKind: "agent", ReviewerModel: "claude-fable-5", Verdict: "approve_with_concerns"})

	r := newResolver(srv, nil)
	r.reviewPollInterval = 200 * time.Microsecond
	// A sub-millisecond TTL so the boundary is re-probed on the next tick.
	r.strandProbeTTL = time.Nanosecond

	// Flip the boundary to B (the RESTART) after the wait is already polling.
	// The hook runs under fb.mu, which the /healthz handler also takes, so the
	// change is observed by the next probe.
	var startedQueries atomic.Int64
	fb.reviewFlip = func(category string) {
		if category == "plan_review_started" && startedQueries.Add(1) == 3 {
			fb.healthzProcessStart = strandBootB.Format(time.RFC3339Nano)
		}
	}

	start := time.Now()
	_, out, err := r.awaitReview(context.Background(), nil, AwaitReviewInput{
		RunID: runID.String(), Stage: "plan", TimeoutSeconds: 600,
	})
	if err != nil {
		t.Fatalf("awaitReview: %v", err)
	}
	if out.Status != "stranded" {
		t.Fatalf("Status = %q, want stranded — the SAME call must see a restart that happens DURING the wait", out.Status)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Errorf("await took %v; the mid-wait strand must resolve promptly, not at the timeout", elapsed)
	}
	if out.WaitedSeconds <= 0 {
		t.Error("waited_seconds should record the polling that happened before the restart was seen")
	}
	if startedQueries.Load() < 3 {
		t.Fatalf("the wait resolved before the boundary changed (started queries = %d); the test did not exercise the mid-wait case",
			startedQueries.Load())
	}
}

// TestAwaitPendingTimeoutOutput_NoLivenessClaim pins the rewritten
// pending-after-timeout message on both probe branches. The old unconditional
// "the review is genuinely still running" assertion is the exact regression
// #2712 reports: it was simply FALSE for an orphaned round.
func TestAwaitPendingTimeoutOutput_NoLivenessClaim(t *testing.T) {
	r := &runResolver{}
	start := time.Now()

	// The 'verified' fixture carries a REAL boundary: DaemonProcessStart is set
	// only after reviewRoundStrandFrom reached and passed the /healthz
	// comparison, so it is the evidence the claim is gated on.
	verified := r.awaitPendingTimeoutOutput("plan", 360, start, false, false, 600, &reviewStrand{
		LandedTerminal: 1, ConfiguredAgents: 2,
		StartedAt:          start.Add(-10 * time.Minute),
		DaemonProcessStart: start.Add(-30 * time.Minute),
	})
	if !strings.Contains(verified.Message, "verified") {
		t.Errorf("a positively not-stranded timeout should say the dispatching daemon was verified: %q", verified.Message)
	}
	if verified.Undecidable {
		t.Error("Undecidable = true on a decided verdict")
	}

	// A strand from one of reviewRoundStrandFrom's early returns — here the
	// ConfiguredAgents <= 0 legacy round, which is reachable at the timeout
	// path via reviewStatusFallback — carries the SAME !Stranded &&
	// !Undecidable shape while having never consulted /healthz at all (zero
	// DaemonProcessStart). Claiming "verified" there asserts a check that was
	// never performed, under a daemon that may well have restarted. It must
	// fall back to the neutral pre-#2712 wording.
	unprobed := r.awaitPendingTimeoutOutput("plan", 360, start, false, false, 600, &reviewStrand{
		Reason: "the review round records no configured agent count",
	})
	if strings.Contains(unprobed.Message, "verified") {
		t.Errorf("a strand that never reached the boundary check must NOT claim verification: %q", unprobed.Message)
	}
	if strings.Contains(unprobed.Message, "orphaned by a restart") {
		t.Errorf("un-probed timeout message leaked the boundary-comparison wording: %q", unprobed.Message)
	}
	if !strings.Contains(unprobed.Message, "no terminal audit entry yet") {
		t.Errorf("un-probed timeout should keep the neutral pre-#2712 wording: %q", unprobed.Message)
	}
	if unprobed.Undecidable {
		t.Error("Undecidable = true on an early-return verdict")
	}

	undecidable := r.awaitPendingTimeoutOutput("plan", 360, start, false, false, 600, &reviewStrand{
		Undecidable: true, Reason: "/healthz was unreachable (dial tcp: connection refused)",
		LandedTerminal: 1, ConfiguredAgents: 2,
	})
	if strings.Contains(undecidable.Message, "genuinely still running") {
		t.Errorf("the undecidable branch must NOT assert the reviewer is alive: %q", undecidable.Message)
	}
	for _, want := range []string{"could NOT be verified", "connection refused", "fishhawk_reconcile_reviews", "1 of 2"} {
		if !strings.Contains(undecidable.Message, want) {
			t.Errorf("undecidable timeout message missing %q: %q", want, undecidable.Message)
		}
	}
	if !undecidable.Undecidable {
		t.Error("Undecidable = false on an undecidable verdict")
	}

	// No probe verdict at all (the probe itself errored): the message stays
	// on the pre-#2712 wording rather than claiming either way.
	none := r.awaitPendingTimeoutOutput("plan", 360, start, false, false, 600, nil)
	if none.Status != "pending" {
		t.Errorf("Status = %q, want pending", none.Status)
	}
	if none.Undecidable {
		t.Error("Undecidable = true with no probe verdict")
	}
}

// TestAwaitReview_StrandedThenReconciled_EndToEnd is BINDING CONDITION 2's
// cross-boundary control. Nothing here is hand-authored: the REAL MCP api
// client issues real HTTP to the REAL registered fishhawkd handlers
// (server.Handler(), including its bearerAuth + adminWrite wrappers), backed by
// REAL PERSISTED audit rows in Postgres. A divergence between what the handler
// emits and what the client decodes — the failure this change is most exposed
// to, since it adds a new endpoint AND a new tool at once — turns it RED, which
// a fake backend serving a body the test itself wrote could never catch.
//
// The scenario is the observed one (#2712): 2 configured plan reviewers, ONE
// landed verdict, the dispatching daemon since restarted.
func TestAwaitReview_StrandedThenReconciled_EndToEnd(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	runRepo := runpkg.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)

	r0, err := runRepo.CreateRun(ctx, runpkg.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef",
		TriggerSource: runpkg.TriggerCLI,
		RunnerKind:    runpkg.RunnerKindLocal,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	planStage, err := runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID: r0.ID, Sequence: 0, Type: runpkg.StageTypePlan,
		ExecutorKind: runpkg.ExecutorAgent, ExecutorRef: "claude-code",
	})
	if err != nil {
		t.Fatalf("CreateStage: %v", err)
	}

	// The daemon serving this request booted AFTER the round was dispatched:
	// the restart that orphaned the reviewers.
	boot := time.Now().UTC()
	dispatched := boot.Add(-15 * time.Minute)
	appendE2EAudit(t, auditRepo, r0.ID, &planStage.ID, dispatched, "plan_review_started",
		map[string]any{"configured_agents": 2, "authority": "advisory"})
	appendE2EAudit(t, auditRepo, r0.ID, &planStage.ID, dispatched.Add(time.Minute), "plan_reviewed",
		map[string]any{
			"reviewer_kind": "agent", "reviewer_model": "claude-fable-5",
			"authority": "advisory", "verdict": "approve_with_concerns",
			"free_form": "the boot marker comparison needs a test",
		})

	const bearer = "fhk_reconcile_e2e"
	tokRepo := &stubMCPAPITokens{tok: &apitoken.Token{
		ID: uuid.New(), Subject: "github:op", Scopes: []string{"write:runs", "read:runs"}, PlainText: bearer,
	}}
	srv := server.New(server.Config{
		RunRepo: runRepo, AuditRepo: auditRepo, APITokenRepo: tokRepo, ProcessStart: boot,
	})
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)

	r := &runResolver{api: newAPIClient(config{backendURL: httpSrv.URL, apiToken: bearer})}
	r.reviewPollInterval = 10 * time.Millisecond

	// (a) The wait resolves 'stranded' on the FIRST round-trip, well under the
	// configured timeout, and never claims the dead reviewer is alive.
	_, out, err := r.awaitReview(ctx, nil, AwaitReviewInput{
		RunID: r0.ID.String(), Stage: "plan", TimeoutSeconds: 600,
	})
	if err != nil {
		t.Fatalf("awaitReview: %v", err)
	}
	if out.Status != "stranded" {
		t.Fatalf("Status = %q, want stranded (message %q)", out.Status, out.Message)
	}
	if out.WaitedSeconds > 30 {
		t.Errorf("waited_seconds = %v, want well under the 600s timeout", out.WaitedSeconds)
	}
	if out.LandedTerminal != 1 || out.ConfiguredAgents != 2 {
		t.Errorf("landed/configured = %d/%d, want 1/2", out.LandedTerminal, out.ConfiguredAgents)
	}
	if strings.Contains(out.Message, "genuinely still running") {
		t.Errorf("stranded message must not claim the reviewer is alive: %q", out.Message)
	}
	if !strings.Contains(out.Message, "1 of 2 configured reviewers") {
		t.Errorf("stranded message must name the shortfall concretely: %q", out.Message)
	}

	// (b) The recovery tool, through the REAL endpoint, synthesizes exactly
	// the ONE missing terminal entry.
	_, rec, err := r.reconcileReviews(ctx, nil, ReconcileReviewsInput{RunID: r0.ID.String()})
	if err != nil {
		t.Fatalf("reconcileReviews: %v", err)
	}
	if !rec.Terminated {
		t.Fatalf("terminated = false, want true; %+v", rec)
	}
	var planRow *ReconcileReviewsStage
	for i := range rec.Stages {
		if rec.Stages[i].Stage == "plan" {
			planRow = &rec.Stages[i]
		}
	}
	if planRow == nil {
		t.Fatalf("no plan stage row in %+v", rec.Stages)
	}
	if planRow.Synthesized != 1 || planRow.LandedBefore != 1 || planRow.ConfiguredAgents != 2 {
		t.Fatalf("plan row = %+v, want synthesized 1 / landed_before 1 / configured 2", *planRow)
	}

	// (c) The review now resolves terminal AND the original verdict survives
	// verbatim — the no-re-pay assertion, read back through the same client.
	st, err := r.reviewStatusFor(ctx, r0.ID, "plan")
	if err != nil {
		t.Fatalf("reviewStatusFor: %v", err)
	}
	if st.Status != "complete" {
		t.Fatalf("review status = %q, want complete once the round settled", st.Status)
	}
	var sawOriginal bool
	for _, rev := range st.Reviews {
		if rev.ReviewerModel == "claude-fable-5" && rev.Verdict == "approve_with_concerns" &&
			rev.FreeForm == "the boot marker comparison needs a test" {
			sawOriginal = true
		}
	}
	if !sawOriginal {
		t.Fatalf("the landed claude-fable-5 approve_with_concerns verdict was not preserved verbatim: %+v", st.Reviews)
	}
	if len(st.Reviews) != 2 {
		t.Errorf("reviews = %d rows, want 2 (the real verdict + one synthesized terminal)", len(st.Reviews))
	}
}

// appendE2EAudit appends one real chained audit entry for the end-to-end test.
func appendE2EAudit(t *testing.T, repo audit.Repository, runID uuid.UUID, stageID *uuid.UUID, ts time.Time, category string, payload any) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal %s: %v", category, err)
	}
	kind := audit.ActorKind("system")
	if _, err := repo.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: runID, StageID: stageID, Timestamp: ts, Category: category,
		ActorKind: &kind, Payload: raw,
	}); err != nil {
		t.Fatalf("AppendChained %s: %v", category, err)
	}
}
