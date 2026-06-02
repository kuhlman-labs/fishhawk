package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/pricing"
)

// reviewerCostPayload decodes the cost_recorded audit payload written for an
// advisory reviewer invocation (#681). The source field distinguishes a
// reviewer entry (plan_review / implement_review) from a runner stage-agent
// entry (no source), and known_usage is the graceful-degradation marker.
type reviewerCostPayload struct {
	Model        string  `json:"model"`
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	USD          float64 `json:"usd"`
	KnownModel   bool    `json:"known_model"`
	KnownUsage   bool    `json:"known_usage"`
	Source       string  `json:"source"`
}

// usageReviewer is a fake PlanReviewer backend that reports token usage
// through the planreview.ReviewVerdict contract (#681). It is the seam-test
// stand-in for the two real adapters (anthropic SDK, claudecode subprocess):
// it exercises the server's price → audit → rollup path independent of which
// backend produced the usage, proving the capture is backend-agnostic.
type usageReviewer struct {
	verdict *planreview.ReviewVerdict
	model   string
}

func (u *usageReviewer) Review(_ context.Context, _ string) (*planreview.ReviewVerdict, string, error) {
	return u.verdict, u.model, nil
}

// findReviewerCostEntry returns the cost_recorded audit entry whose payload
// carries the given source, or nil. It filters on source so a runner-style
// cost_recorded entry (written by recordCost on the raw bundle, with no
// source) does not satisfy the reviewer-cost assertion.
func findReviewerCostEntry(t *testing.T, au *auditFake, source string, stageID uuid.UUID) *reviewerCostPayload {
	t.Helper()
	au.mu.Lock()
	defer au.mu.Unlock()
	for i := range au.appended {
		e := au.appended[i]
		if e.Category != "cost_recorded" {
			continue
		}
		var p reviewerCostPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			continue
		}
		if p.Source != source {
			continue
		}
		if e.StageID == nil || *e.StageID != stageID {
			t.Errorf("reviewer cost_recorded StageID = %v, want %s", e.StageID, stageID)
		}
		return &p
	}
	return nil
}

// TestPlanReview_RecordsReviewerCost is the cross-boundary seam test for the
// plan-review arm of #681. A fake reviewer backend reports usage through the
// planreview.ReviewVerdict contract; driven through the full handleShipPlan
// → runPlanReviews → runPlanReviewLoop path under gating authority, it must
// produce BOTH (i) a cost_recorded audit entry sourced plan_review against
// the plan stage, priced from the reviewer model + reported usage, AND (ii) a
// per-run cost rollup folded in via a real AddRunCost call (non-zero delta).
func TestPlanReview_RecordsReviewerCost(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	const model = "claude-opus-4-8"
	const inTok, outTok = 1000, 2000
	wantUSD, ok := pricing.Cost(model, inTok, outTok)
	if !ok {
		t.Fatalf("pricing.Cost ok=false for %q — fixture model must be priced", model)
	}

	reviewer := &usageReviewer{
		verdict: &planreview.ReviewVerdict{
			Verdict: planreview.VerdictApprove,
			Usage:   planreview.Usage{InputTokens: inTok, OutputTokens: outTok, Known: true},
		},
		model: model,
	}
	s, sf, _, au, rr := newPlanServerWithReviewer(t, runID, stageID, reviewer, specGatingReviewers)
	priv, _ := sf.issue(t, runID)

	w := shipPlanRequest(t, s, runID, stageID, priv, validPlanBytes(t), "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}

	// (i) cost_recorded audit entry sourced plan_review against the stage.
	got := findReviewerCostEntry(t, au, "plan_review", stageID)
	if got == nil {
		t.Fatal("no cost_recorded entry sourced plan_review")
	}
	if got.Model != model || got.InputTokens != inTok || got.OutputTokens != outTok {
		t.Errorf("plan_review cost payload mismatch: %+v", got)
	}
	if got.USD != wantUSD {
		t.Errorf("plan_review usd = %v, want %v (pricing.Cost)", got.USD, wantUSD)
	}
	if !got.KnownModel || !got.KnownUsage {
		t.Errorf("plan_review known_model=%v known_usage=%v, want both true", got.KnownModel, got.KnownUsage)
	}

	// (ii) the per-run rollup folded it in via a real AddRunCost call.
	var sawNonZero bool
	for _, d := range rr.addRunCostDeltas {
		if d > 0 {
			sawNonZero = true
		}
	}
	if !sawNonZero {
		t.Fatalf("AddRunCost was not called with a non-zero delta; deltas = %v", rr.addRunCostDeltas)
	}
	gotRun, err := rr.GetRun(t.Context(), runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if gotRun.CostUSDTotal != wantUSD {
		t.Errorf("run.CostUSDTotal = %v, want %v", gotRun.CostUSDTotal, wantUSD)
	}
	if gotRun.ResolvedModel != model {
		t.Errorf("run.ResolvedModel = %q, want %q", gotRun.ResolvedModel, model)
	}
}

// TestImplementReview_RecordsReviewerCost is the cross-boundary seam test for
// the implement-review arm of #681. It mirrors the plan-review seam test but
// drives the full handleShipTrace → runImplementReviews → runImplementReviewLoop
// path under gating authority, asserting (i) a cost_recorded entry sourced
// implement_review against the implement stage and (ii) the per-run rollup
// via a real AddRunCost call on the orchestratorRepo (extended to satisfy
// runCostRecorder — the binding #647-fixture trap).
func TestImplementReview_RecordsReviewerCost(t *testing.T) {
	const model = "claude-opus-4-8"
	const inTok, outTok = 1500, 3000
	wantUSD, ok := pricing.Cost(model, inTok, outTok)
	if !ok {
		t.Fatalf("pricing.Cost ok=false for %q — fixture model must be priced", model)
	}

	reviewer := &usageReviewer{
		verdict: &planreview.ReviewVerdict{
			Verdict: planreview.VerdictApprove,
			Usage:   planreview.Usage{InputTokens: inTok, OutputTokens: outTok, Known: true},
		},
		model: model,
	}
	s, sf, au, rr, runRow, implStage := newImplementReviewServer(t, reviewer, specImplementGatingReviewers)
	priv, _ := sf.issue(t, runRow.ID)

	bundleBytes := implementDiffBundle(t, []map[string]string{
		{"path": "backend/internal/foo/foo.go", "status": "M"},
	})
	w := shipRequest(t, s, runRow.ID, implStage.ID, "raw", priv, bundleBytes, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}

	// (i) cost_recorded audit entry sourced implement_review against the stage.
	got := findReviewerCostEntry(t, au, "implement_review", implStage.ID)
	if got == nil {
		t.Fatal("no cost_recorded entry sourced implement_review")
	}
	if got.Model != model || got.InputTokens != inTok || got.OutputTokens != outTok {
		t.Errorf("implement_review cost payload mismatch: %+v", got)
	}
	if got.USD != wantUSD {
		t.Errorf("implement_review usd = %v, want %v (pricing.Cost)", got.USD, wantUSD)
	}
	if !got.KnownModel || !got.KnownUsage {
		t.Errorf("implement_review known_model=%v known_usage=%v, want both true", got.KnownModel, got.KnownUsage)
	}

	// (ii) the per-run rollup folded it in via a real AddRunCost call.
	rr.mu.Lock()
	deltas := append([]float64(nil), rr.addRunCostDeltas...)
	rr.mu.Unlock()
	var sawNonZero bool
	for _, d := range deltas {
		if d > 0 {
			sawNonZero = true
		}
	}
	if !sawNonZero {
		t.Fatalf("AddRunCost was not called with a non-zero delta; deltas = %v", deltas)
	}
	gotRun, err := rr.GetRun(t.Context(), runRow.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if gotRun.CostUSDTotal < wantUSD {
		t.Errorf("run.CostUSDTotal = %v, want >= %v", gotRun.CostUSDTotal, wantUSD)
	}
	if gotRun.ResolvedModel != model {
		t.Errorf("run.ResolvedModel = %q, want %q", gotRun.ResolvedModel, model)
	}
}

// TestRecordReviewerCost_UnknownUsageDegrades asserts the graceful-degradation
// arm (#681): a reviewer backend that cannot report usage (Usage.Known=false)
// yields a cost_recorded entry at usd=0 with known_usage=false rather than a
// guessed figure — mirroring the unknown-model contract — and the rollup folds
// in a zero delta without faulting.
func TestRecordReviewerCost_UnknownUsageDegrades(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	reviewer := &usageReviewer{
		verdict: &planreview.ReviewVerdict{
			Verdict: planreview.VerdictApprove,
			// Known=false: the backend could not report usage.
			Usage: planreview.Usage{Known: false},
		},
		model: "claude-opus-4-8",
	}
	s, sf, _, au, rr := newPlanServerWithReviewer(t, runID, stageID, reviewer, specGatingReviewers)
	priv, _ := sf.issue(t, runID)

	w := shipPlanRequest(t, s, runID, stageID, priv, validPlanBytes(t), "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}

	got := findReviewerCostEntry(t, au, "plan_review", stageID)
	if got == nil {
		t.Fatal("no cost_recorded entry sourced plan_review")
	}
	if got.KnownUsage {
		t.Error("known_usage = true, want false for a backend that cannot report usage")
	}
	if got.USD != 0 {
		t.Errorf("degraded usd = %v, want 0 (must not guess)", got.USD)
	}
	gotRun, err := rr.GetRun(t.Context(), runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if gotRun.CostUSDTotal != 0 {
		t.Errorf("run.CostUSDTotal = %v, want 0 for degraded usage", gotRun.CostUSDTotal)
	}
}
