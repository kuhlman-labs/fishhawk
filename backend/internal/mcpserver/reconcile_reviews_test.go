package mcpserver

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestReconcileReviews_HealsAndReportsCounts covers the happy path: the tool
// forwards the run id to the endpoint and reports the per-stage counts,
// naming the preserved landed verdicts in the message.
func TestReconcileReviews_HealsAndReportsCounts(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.reconcileRespByRun[runID] = ReconcileReviewsResult{
		RunID:      runID.String(),
		Terminated: true,
		Stages: []ReconciledReviewStage{
			{Stage: "plan", ConfiguredAgents: 2, LandedBefore: 1, Synthesized: 1},
			{Stage: "implement", Skipped: true, SkipReason: "no_review_started_entry"},
		},
	}

	r := newResolver(srv, nil)
	_, out, err := r.reconcileReviews(context.Background(), nil, ReconcileReviewsInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("reconcileReviews: %v", err)
	}
	if !out.Terminated {
		t.Fatalf("terminated = false, want true")
	}
	if len(out.Stages) != 2 {
		t.Fatalf("stages = %d, want 2", len(out.Stages))
	}
	if out.Stages[0].Synthesized != 1 || out.Stages[0].LandedBefore != 1 {
		t.Errorf("plan row = %+v, want synthesized 1 / landed_before 1", out.Stages[0])
	}
	for _, want := range []string{"synthesized 1 terminal entry", "preserved", "no_review_started_entry"} {
		if !strings.Contains(out.Message, want) {
			t.Errorf("message missing %q: %q", want, out.Message)
		}
	}
	if got := fb.reconcileCallsByRun[runID]; got != 1 {
		t.Errorf("endpoint calls = %d, want exactly 1", got)
	}
}

// TestReconcileReviews_NothingHealed reports the reason rather than a silent
// no-op — including the live-review refusal, which is the control that stops
// this verb terminating a review the daemon still has in flight.
func TestReconcileReviews_NothingHealed(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.reconcileRespByRun[runID] = ReconcileReviewsResult{
		RunID: runID.String(),
		Stages: []ReconciledReviewStage{
			{Stage: "plan", ConfiguredAgents: 2, Skipped: true, SkipReason: "review_dispatched_by_this_process"},
			{Stage: "implement", Skipped: true, SkipReason: "round_already_settled"},
		},
	}

	r := newResolver(srv, nil)
	_, out, err := r.reconcileReviews(context.Background(), nil, ReconcileReviewsInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("reconcileReviews: %v", err)
	}
	if out.Terminated {
		t.Error("terminated = true, want false")
	}
	for _, want := range []string{"no orphaned review round was terminated", "review_dispatched_by_this_process", "round_already_settled"} {
		if !strings.Contains(out.Message, want) {
			t.Errorf("message missing %q: %q", want, out.Message)
		}
	}
}

// TestReconcileReviews_BadUUIDRefusedLocally covers the pre-HTTP guard: an
// invalid run id never reaches the backend.
func TestReconcileReviews_BadUUIDRefusedLocally(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.reconcileReviews(context.Background(), nil, ReconcileReviewsInput{RunID: "not-a-uuid"})
	if err == nil {
		t.Fatal("want an error for a malformed run_id")
	}
	if !strings.Contains(err.Error(), "not a valid UUID") {
		t.Errorf("error = %v, want the UUID refusal", err)
	}
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if len(fb.reconcileCallsByRun) != 0 {
		t.Errorf("the malformed id reached the backend: %v", fb.reconcileCallsByRun)
	}
}

// TestReconcileReviews_BackendErrorSurfaces covers the endpoint-refusal path:
// a 404 reaches the operator rather than being swallowed into a false success.
func TestReconcileReviews_BackendErrorSurfaces(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.reconcileStatus = http.StatusNotFound
	runID := uuid.New()

	r := newResolver(srv, nil)
	_, _, err := r.reconcileReviews(context.Background(), nil, ReconcileReviewsInput{RunID: runID.String()})
	if err == nil {
		t.Fatal("want an error when the endpoint refuses")
	}
	if !strings.Contains(err.Error(), "run_not_found") {
		t.Errorf("error = %v, want the backend refusal verbatim", err)
	}
}

// TestReconcileReviewsMessage covers the pure renderer's branches, including
// the singular/plural entry wording.
func TestReconcileReviewsMessage(t *testing.T) {
	one := reconcileReviewsMessage(ReconcileReviewsOutput{
		Stages: []ReconcileReviewsStage{{Stage: "plan", ConfiguredAgents: 2, LandedBefore: 1, Synthesized: 1}},
	})
	if !strings.Contains(one, "1 terminal entry") {
		t.Errorf("singular wording missing: %q", one)
	}
	many := reconcileReviewsMessage(ReconcileReviewsOutput{
		Stages: []ReconcileReviewsStage{{Stage: "plan", ConfiguredAgents: 3, LandedBefore: 1, Synthesized: 2}},
	})
	if !strings.Contains(many, "2 terminal entries") {
		t.Errorf("plural wording missing: %q", many)
	}
	empty := reconcileReviewsMessage(ReconcileReviewsOutput{
		Stages: []ReconcileReviewsStage{{Stage: "plan", Skipped: true}},
	})
	if !strings.Contains(empty, "nothing to heal") {
		t.Errorf("a skipped row with no reason should still render a reason: %q", empty)
	}
}
