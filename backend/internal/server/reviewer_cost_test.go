package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
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
	Model                 string  `json:"model"`
	InputTokens           int     `json:"input_tokens"`
	OutputTokens          int     `json:"output_tokens"`
	CachedInputTokens     int     `json:"cached_input_tokens"`
	CacheReadInputTokens  int     `json:"cache_read_input_tokens"`
	CacheWriteInputTokens int     `json:"cache_write_input_tokens"`
	TotalInputTokens      int     `json:"total_input_tokens"`
	Turns                 int     `json:"turns"`
	USD                   float64 `json:"usd"`
	KnownModel            bool    `json:"known_model"`
	KnownUsage            bool    `json:"known_usage"`
	Source                string  `json:"source"`
}

// reviewedTokenFields decodes only the #995 token members of a persisted
// plan_reviewed / implement_reviewed audit payload, pinning the wire field
// names independently of the planreview payload structs.
type reviewedTokenFields struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// findReviewedTokens returns the token fields of the single audit entry with
// the given category (plan_reviewed / implement_reviewed), failing the test
// when none exists.
func findReviewedTokens(t *testing.T, au *auditFake, category string) reviewedTokenFields {
	t.Helper()
	au.mu.Lock()
	defer au.mu.Unlock()
	for i := range au.appended {
		e := au.appended[i]
		if e.Category != category {
			continue
		}
		var p reviewedTokenFields
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode %s payload: %v", category, err)
		}
		return p
	}
	t.Fatalf("no %s audit entry found", category)
	return reviewedTokenFields{}
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
	// The stage agent (which ships the trace before the advisory review runs)
	// has already pinned runs.resolved_model to its own model — the G6
	// reproducibility pin. The advisory reviewer runs under a DIFFERENT model
	// and must NOT clobber that pin (#684).
	const stageAgentModel = "claude-opus-4-8"
	const reviewerModel = "claude-sonnet-4-6"
	// Distinct cache read vs write counts so the cache-aware USD prices reads
	// at the discount (sonnet 0.3/1M) and writes at the premium (3.75/1M),
	// exercising the read/write-aware path end to end (#1343).
	const inTok, outTok, cacheReadTok, cacheWriteTok, turns = 1000, 2000, 400, 250, 3
	const cachedTok = cacheReadTok + cacheWriteTok
	wantUSD, ok := pricing.CostWithCache(reviewerModel, inTok, cacheReadTok, cacheWriteTok, outTok)
	if !ok {
		t.Fatalf("pricing.CostWithCache ok=false for %q — fixture model must be priced", reviewerModel)
	}
	// The cache-aware figure must exceed the fresh-input-only figure, proving
	// the cached tokens are actually priced in (not dropped).
	if plainUSD, _ := pricing.Cost(reviewerModel, inTok, outTok); wantUSD <= plainUSD {
		t.Fatalf("cache-aware USD %v not > fresh-only USD %v — cache pricing not exercised", wantUSD, plainUSD)
	}

	reviewer := &usageReviewer{
		verdict: &planreview.ReviewVerdict{
			Verdict: planreview.VerdictApprove,
			// Fully populated usage (#995/#1343): the cache read/write split and
			// turn count must cross the adapter-contract → server-record →
			// audit-payload seam end to end, alongside the priced token counts.
			Usage: planreview.Usage{InputTokens: inTok, OutputTokens: outTok, CacheReadInputTokens: cacheReadTok, CacheWriteInputTokens: cacheWriteTok, Turns: turns, Known: true},
		},
		model: reviewerModel,
	}
	s, sf, _, au, rr := newPlanServerWithReviewer(t, runID, stageID, reviewer, specGatingReviewers)
	// Seed the stage-agent pin that the trace-ship path would have set before
	// the advisory review runs, so we can prove the reviewer leaves it intact.
	rr.getRuns[runID].ResolvedModel = stageAgentModel
	priv, _ := sf.issue(t, runID)

	w := shipPlanRequest(t, s, runID, stageID, priv, validPlanBytes(t), "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}

	// (i) cost_recorded audit entry sourced plan_review against the stage,
	// priced from the REVIEWER model (not the stage-agent model).
	got := findReviewerCostEntry(t, au, "plan_review", stageID)
	if got == nil {
		t.Fatal("no cost_recorded entry sourced plan_review")
	}
	if got.Model != reviewerModel || got.InputTokens != inTok || got.OutputTokens != outTok {
		t.Errorf("plan_review cost payload mismatch: %+v", got)
	}
	if got.CachedInputTokens != cachedTok || got.Turns != turns {
		t.Errorf("plan_review cost payload cached_input_tokens=%d turns=%d, want %d/%d (#995)", got.CachedInputTokens, got.Turns, cachedTok, turns)
	}
	// The additive read/write split rides alongside the back-compat summed
	// total (#1343): cached_input_tokens == read + write.
	if got.CacheReadInputTokens != cacheReadTok || got.CacheWriteInputTokens != cacheWriteTok {
		t.Errorf("plan_review cache split = read %d / write %d, want %d / %d (#1343)", got.CacheReadInputTokens, got.CacheWriteInputTokens, cacheReadTok, cacheWriteTok)
	}
	if got.CachedInputTokens != got.CacheReadInputTokens+got.CacheWriteInputTokens {
		t.Errorf("plan_review cached_input_tokens=%d != read+write=%d (back-compat sum, #1343)", got.CachedInputTokens, got.CacheReadInputTokens+got.CacheWriteInputTokens)
	}
	if got.TotalInputTokens != inTok+cachedTok {
		t.Errorf("plan_review cost payload total_input_tokens=%d, want %d (fresh + cached, #1010)", got.TotalInputTokens, inTok+cachedTok)
	}
	if got.USD != wantUSD {
		t.Errorf("plan_review usd = %v, want %v (cache-aware: read at discount, write at premium, #1343)", got.USD, wantUSD)
	}
	if !got.KnownModel || !got.KnownUsage {
		t.Errorf("plan_review known_model=%v known_usage=%v, want both true", got.KnownModel, got.KnownUsage)
	}

	// The persisted plan_reviewed audit payload carries the invocation's token
	// usage on the review surface itself (#995).
	if rv := findReviewedTokens(t, au, "plan_reviewed"); rv.InputTokens != inTok || rv.OutputTokens != outTok {
		t.Errorf("plan_reviewed payload tokens = %+v, want input_tokens=%d output_tokens=%d", rv, inTok, outTok)
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
	// (iii) the G6 pin survives: reviewer cost folded into the total but
	// resolved_model is STILL the stage-agent model, not the reviewer model
	// (#684). This assertion fails on the pre-fix code that passed rec.Model.
	if gotRun.ResolvedModel != stageAgentModel {
		t.Errorf("run.ResolvedModel = %q, want %q (stage-agent pin must survive the reviewer)", gotRun.ResolvedModel, stageAgentModel)
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
	// As in the plan-review arm: the implement stage agent has already pinned
	// runs.resolved_model to its own model before the advisory reviewer (a
	// DIFFERENT model) runs. The reviewer must not clobber that pin (#684).
	const stageAgentModel = "claude-opus-4-8"
	const reviewerModel = "claude-sonnet-4-6"
	// Distinct read vs write so the cache-aware USD prices reads at the
	// discount and writes at the premium (#1343), mirroring the plan-side test.
	const inTok, outTok, cacheReadTok, cacheWriteTok, turns = 1500, 3000, 250, 180, 2
	const cachedTok = cacheReadTok + cacheWriteTok
	wantUSD, ok := pricing.CostWithCache(reviewerModel, inTok, cacheReadTok, cacheWriteTok, outTok)
	if !ok {
		t.Fatalf("pricing.CostWithCache ok=false for %q — fixture model must be priced", reviewerModel)
	}
	if plainUSD, _ := pricing.Cost(reviewerModel, inTok, outTok); wantUSD <= plainUSD {
		t.Fatalf("cache-aware USD %v not > fresh-only USD %v — cache pricing not exercised", wantUSD, plainUSD)
	}

	reviewer := &usageReviewer{
		verdict: &planreview.ReviewVerdict{
			Verdict: planreview.VerdictApprove,
			// Fully populated usage (#995/#1343), mirroring the plan-side seam test.
			Usage: planreview.Usage{InputTokens: inTok, OutputTokens: outTok, CacheReadInputTokens: cacheReadTok, CacheWriteInputTokens: cacheWriteTok, Turns: turns, Known: true},
		},
		model: reviewerModel,
	}
	s, sf, au, rr, runRow, implStage := newImplementReviewServer(t, reviewer, specImplementGatingReviewers)
	// Seed the stage-agent pin the trace-ship path would have set. The implement
	// trace bundle carries no manifest model, so recordCost leaves resolved_model
	// untouched — this seed is the only stage-agent pin in play.
	runRow.ResolvedModel = stageAgentModel
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
	if got.Model != reviewerModel || got.InputTokens != inTok || got.OutputTokens != outTok {
		t.Errorf("implement_review cost payload mismatch: %+v", got)
	}
	if got.CachedInputTokens != cachedTok || got.Turns != turns {
		t.Errorf("implement_review cost payload cached_input_tokens=%d turns=%d, want %d/%d (#995)", got.CachedInputTokens, got.Turns, cachedTok, turns)
	}
	if got.CacheReadInputTokens != cacheReadTok || got.CacheWriteInputTokens != cacheWriteTok {
		t.Errorf("implement_review cache split = read %d / write %d, want %d / %d (#1343)", got.CacheReadInputTokens, got.CacheWriteInputTokens, cacheReadTok, cacheWriteTok)
	}
	if got.CachedInputTokens != got.CacheReadInputTokens+got.CacheWriteInputTokens {
		t.Errorf("implement_review cached_input_tokens=%d != read+write=%d (back-compat sum, #1343)", got.CachedInputTokens, got.CacheReadInputTokens+got.CacheWriteInputTokens)
	}
	if got.TotalInputTokens != inTok+cachedTok {
		t.Errorf("implement_review cost payload total_input_tokens=%d, want %d (fresh + cached, #1010)", got.TotalInputTokens, inTok+cachedTok)
	}
	if got.USD != wantUSD {
		t.Errorf("implement_review usd = %v, want %v (cache-aware: read at discount, write at premium, #1343)", got.USD, wantUSD)
	}
	if !got.KnownModel || !got.KnownUsage {
		t.Errorf("implement_review known_model=%v known_usage=%v, want both true", got.KnownModel, got.KnownUsage)
	}

	// The persisted implement_reviewed audit payload carries the invocation's
	// token usage on the review surface itself (#995), mirroring the plan side.
	if rv := findReviewedTokens(t, au, "implement_reviewed"); rv.InputTokens != inTok || rv.OutputTokens != outTok {
		t.Errorf("implement_reviewed payload tokens = %+v, want input_tokens=%d output_tokens=%d", rv, inTok, outTok)
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
	// The G6 pin survives: reviewer cost folded into the total but
	// resolved_model is STILL the stage-agent model, not the reviewer model
	// (#684). This assertion fails on the pre-fix code that passed rec.Model.
	if gotRun.ResolvedModel != stageAgentModel {
		t.Errorf("run.ResolvedModel = %q, want %q (stage-agent pin must survive the reviewer)", gotRun.ResolvedModel, stageAgentModel)
	}
}

// TestRecordReviewerCost_WarnCeiling asserts the two advisory tripwires
// (#995/#1010): recordReviewerCost WARN-logs when a reviewer invocation's
// KNOWN FRESH (cache-exclusive) input tokens exceed
// reviewerInputTokenWarnCeiling, separately WARN-logs when the TOTAL
// input-side count (fresh + cached) exceeds the higher
// reviewerTotalInputTokenWarnCeiling — the codex-shaped heavy-cache case,
// like the observed 689k-total/572k-cached review, trips the total ceiling
// while the fresh ceiling stays silent — and stays silent at or below both
// ceilings and for unknown usage (whose token counts are zero-value by
// contract and must not trip a misleading warning).
func TestRecordReviewerCost_WarnCeiling(t *testing.T) {
	// The two WARN messages are distinguishable by these substrings; the
	// fresh message is NOT a substring of the total message and vice versa.
	const freshWarnMsg = "input tokens exceed warn ceiling — possible context-assembly blowup"
	const totalWarnMsg = "exceed warn ceiling — runaway total context"
	tests := []struct {
		name          string
		usage         planreview.Usage
		wantFreshWarn bool
		wantTotalWarn bool
	}{
		{
			name:          "above fresh ceiling warns",
			usage:         planreview.Usage{InputTokens: reviewerInputTokenWarnCeiling + 1, OutputTokens: 900, Turns: 12, Known: true},
			wantFreshWarn: true,
		},
		{
			name:  "at fresh ceiling stays silent",
			usage: planreview.Usage{InputTokens: reviewerInputTokenWarnCeiling, OutputTokens: 900, Turns: 1, Known: true},
		},
		{
			name:  "typical review stays silent",
			usage: planreview.Usage{InputTokens: 4053, OutputTokens: 900, Turns: 1, Known: true},
		},
		{
			// The observed codex shape (run 0a0765ff scaled to fresh < 100k):
			// heavy caching keeps fresh under the fresh ceiling, but the total
			// context is a runaway — only the total ceiling fires.
			name:          "heavy-cache total blowup trips total ceiling only",
			usage:         planreview.Usage{InputTokens: 90_000, CacheReadInputTokens: 560_000, OutputTokens: 900, Turns: 21, Known: true},
			wantTotalWarn: true,
		},
		{
			name:  "cached review under both ceilings stays silent",
			usage: planreview.Usage{InputTokens: 4000, CacheReadInputTokens: 120_000, OutputTokens: 900, Turns: 3, Known: true},
		},
		{
			name:  "unknown usage never warns",
			usage: planreview.Usage{Known: false},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runID, stageID := uuid.New(), uuid.New()
			var buf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&buf, nil))
			s := New(Config{
				Addr:      "127.0.0.1:0",
				AuditRepo: newAuditFake(),
				RunRepo:   newPromptRunRepo(),
				Logger:    logger,
			})

			s.recordReviewerCost(context.Background(), runID, stageID, "gpt-5.5", tt.usage, "plan_review")

			logs := buf.String()
			if gotFresh := strings.Contains(logs, freshWarnMsg); gotFresh != tt.wantFreshWarn {
				t.Fatalf("fresh warn logged = %v, want %v; logs:\n%s", gotFresh, tt.wantFreshWarn, logs)
			}
			if gotTotal := strings.Contains(logs, totalWarnMsg); gotTotal != tt.wantTotalWarn {
				t.Fatalf("total warn logged = %v, want %v; logs:\n%s", gotTotal, tt.wantTotalWarn, logs)
			}
			if tt.wantFreshWarn || tt.wantTotalWarn {
				// The warning carries the locating attrs an operator needs.
				for _, want := range []string{
					runID.String(), stageID.String(), `"source":"plan_review"`,
					`"model":"gpt-5.5"`, fmt.Sprintf(`"turns":%d`, tt.usage.Turns),
				} {
					if !strings.Contains(logs, want) {
						t.Errorf("warn log missing %s; logs:\n%s", want, logs)
					}
				}
			}
			if tt.wantTotalWarn {
				// The total warn carries the fresh/cached split alongside the sum.
				total := tt.usage.InputTokens + tt.usage.CachedInputTokens()
				for _, want := range []string{
					fmt.Sprintf(`"total_input_tokens":%d`, total),
					fmt.Sprintf(`"cached_input_tokens":%d`, tt.usage.CachedInputTokens()),
				} {
					if !strings.Contains(logs, want) {
						t.Errorf("total warn log missing %s; logs:\n%s", want, logs)
					}
				}
			}
		})
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
