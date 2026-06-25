package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// costAuditFake serves synthetic cost_recorded + pr_merged entries per run for
// the cost handler. It embeds audit.BaseFake so only ListForRunByCategory is
// overridden.
type costAuditFake struct {
	audit.BaseFake
	costByRun   map[uuid.UUID][]*audit.Entry
	mergedByRun map[uuid.UUID][]*audit.Entry
	err         error
}

func (f *costAuditFake) ListForRunByCategory(_ context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	if f.err != nil {
		return nil, f.err
	}
	switch category {
	case "cost_recorded":
		return f.costByRun[runID], nil
	case CategoryPRMerged:
		return f.mergedByRun[runID], nil
	default:
		return nil, nil
	}
}

// costRecordedEntry builds a cost_recorded audit entry payload carrying usd +
// optional source (an empty source is omitted, mirroring the runner
// stage-agent path).
func costRecordedEntry(usd float64, source string) *audit.Entry {
	m := map[string]any{"usd": usd}
	if source != "" {
		m["source"] = source
	}
	payload, _ := json.Marshal(m)
	return &audit.Entry{Category: "cost_recorded", Payload: payload}
}

// costRunRepo wraps approvalRunRepo to make ListRuns return a seeded slice,
// which the cost handler's merged-PR rollup needs (the base approvalRunRepo
// stubs ListRuns with an error). seedRun + GetRun are promoted from the embed.
type costRunRepo struct {
	*approvalRunRepo
	listRuns []*run.Run
	listErr  error
}

func (r *costRunRepo) ListRuns(_ context.Context, _ run.ListRunsFilter) ([]*run.Run, error) {
	if r.listErr != nil {
		return nil, r.listErr
	}
	return r.listRuns, nil
}

func newCostRunRepo() *costRunRepo {
	return &costRunRepo{approvalRunRepo: newApprovalRunRepo()}
}

func seedCostRun(rr *costRunRepo, id uuid.UUID, costUSD float64, prURL *string) {
	rr.seedRun(&run.Run{
		ID:             id,
		Repo:           "x/y",
		WorkflowID:     "feature_change",
		TriggerSource:  run.TriggerCLI,
		CostUSDTotal:   costUSD,
		PullRequestURL: prURL,
		CreatedAt:      time.Now().UTC(),
	})
}

func getCost(t *testing.T, s *Server, runID string) (int, map[string]json.RawMessage) {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v0/runs/"+runID+"/cost", nil)
	s.Handler().ServeHTTP(w, req)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode response (status %d): %v\nbody: %s", w.Code, err, w.Body.String())
	}
	return w.Code, raw
}

// TestGetRunCost_PerStageBreakdown (case a / c): a populated ledger with a PR
// URL but NO pr_merged audit row yields the per-stage breakdown summing the
// seeded usd values, total_cost_usd matching run.CostUSDTotal, and merged_pr
// omitted (the not-merged branch).
func TestGetRunCost_PerStageBreakdown(t *testing.T) {
	rr := newCostRunRepo()
	id := uuid.New()
	seedCostRun(rr, id, 6.50, ptr("https://github.com/x/y/pull/1"))
	af := &costAuditFake{costByRun: map[uuid.UUID][]*audit.Entry{
		id: {
			costRecordedEntry(4.00, ""), // runner stage-agent, no source -> agent
			costRecordedEntry(1.50, "plan_review"),
			costRecordedEntry(1.00, "implement_review"),
		},
	}}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: af})

	code, raw := getCost(t, s, id.String())
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	assertJSONFloat(t, raw, "total_cost_usd", 6.50)

	var stages []costStageResult
	if err := json.Unmarshal(raw["stages"], &stages); err != nil {
		t.Fatalf("decode stages: %v", err)
	}
	if len(stages) != 3 {
		t.Fatalf("want 3 stages, got %d: %+v", len(stages), stages)
	}
	// Sorted by source: agent, implement_review, plan_review.
	want := map[string]float64{"agent": 4.00, "implement_review": 1.00, "plan_review": 1.50}
	wantOrder := []string{"agent", "implement_review", "plan_review"}
	for i, src := range wantOrder {
		if stages[i].Source != src {
			t.Errorf("stages[%d].Source = %q, want %q", i, stages[i].Source, src)
		}
		if got := stages[i].CostUSD; got < want[src]-1e-9 || got > want[src]+1e-9 {
			t.Errorf("stages[%d] (%s) cost = %v, want %v", i, src, got, want[src])
		}
	}
	// Not merged (no pr_merged audit row) -> merged_pr omitted.
	if _, ok := raw["merged_pr"]; ok {
		t.Errorf("merged_pr must be omitted when the run has no pr_merged audit row; got %s", raw["merged_pr"])
	}
}

// TestGetRunCost_MergedPRRollup (case b): a run with a pr_merged audit row and
// a PR URL shared by two runs surfaces cost_per_merged_pr_usd = the sum of
// both runs' CostUSDTotal with run_count=2.
func TestGetRunCost_MergedPRRollup(t *testing.T) {
	rr := newCostRunRepo()
	url := "https://github.com/x/y/pull/7"
	id := uuid.New()
	siblingID := uuid.New()
	seedCostRun(rr, id, 5.00, ptr(url))
	seedCostRun(rr, siblingID, 3.00, ptr(url))
	// Both runs share the URL; the per-PR ListRuns returns both.
	rr.listRuns = []*run.Run{rr.runs[id], rr.runs[siblingID]}

	af := &costAuditFake{
		costByRun: map[uuid.UUID][]*audit.Entry{
			id: {costRecordedEntry(5.00, "")},
		},
		mergedByRun: map[uuid.UUID][]*audit.Entry{
			id: {{Category: CategoryPRMerged}},
		},
	}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: af})

	code, raw := getCost(t, s, id.String())
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	mergedRaw, ok := raw["merged_pr"]
	if !ok {
		t.Fatalf("merged_pr must be present for a merged run; body: %v", raw)
	}
	var merged mergedPRCost
	if err := json.Unmarshal(mergedRaw, &merged); err != nil {
		t.Fatalf("decode merged_pr: %v", err)
	}
	if merged.PullRequestURL != url {
		t.Errorf("merged_pr.pull_request_url = %q, want %q", merged.PullRequestURL, url)
	}
	if merged.RunCount != 2 {
		t.Errorf("merged_pr.run_count = %d, want 2", merged.RunCount)
	}
	if merged.CostPerMergedPRUSD < 8.00-1e-9 || merged.CostPerMergedPRUSD > 8.00+1e-9 {
		t.Errorf("merged_pr.cost_per_merged_pr_usd = %v, want 8.00 (5.00+3.00)", merged.CostPerMergedPRUSD)
	}
}

// TestGetRunCost_MergedAuditButNoPRURL: a pr_merged audit row with a nil
// PullRequestURL on the run does NOT produce a merged_pr rollup — the URL is
// required alongside the audit row.
func TestGetRunCost_MergedAuditButNoPRURL(t *testing.T) {
	rr := newCostRunRepo()
	id := uuid.New()
	seedCostRun(rr, id, 2.00, nil) // no PR URL
	af := &costAuditFake{
		costByRun:   map[uuid.UUID][]*audit.Entry{id: {costRecordedEntry(2.00, "")}},
		mergedByRun: map[uuid.UUID][]*audit.Entry{id: {{Category: CategoryPRMerged}}},
	}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: af})

	code, raw := getCost(t, s, id.String())
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if _, ok := raw["merged_pr"]; ok {
		t.Errorf("merged_pr must be omitted when the run has no PR URL; got %s", raw["merged_pr"])
	}
}

// TestGetRunCost_UnparsablePayloadSkipped: a malformed cost_recorded payload
// is skipped (best-effort); a run whose every entry is unparsable collapses to
// the no-data 200 empty object.
func TestGetRunCost_UnparsablePayloadSkipped(t *testing.T) {
	rr := newCostRunRepo()
	id := uuid.New()
	seedCostRun(rr, id, 0, nil)
	af := &costAuditFake{costByRun: map[uuid.UUID][]*audit.Entry{
		id: {{Category: "cost_recorded", Payload: []byte("not json")}},
	}}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: af})

	code, raw := getCost(t, s, id.String())
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(raw) != 0 {
		t.Errorf("all-unparsable response must be an empty object; got %v", raw)
	}
}

// TestGetRunCost_NoCostData_EmptyObject (case d): a run with no cost_recorded
// entries returns 200 with an empty object.
func TestGetRunCost_NoCostData_EmptyObject(t *testing.T) {
	rr := newCostRunRepo()
	id := uuid.New()
	seedCostRun(rr, id, 0, nil)
	af := &costAuditFake{costByRun: map[uuid.UUID][]*audit.Entry{}}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: af})

	code, raw := getCost(t, s, id.String())
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(raw) != 0 {
		t.Errorf("no-cost-data response must be an empty object; got %v", raw)
	}
}

// TestGetRunCost_AuditRepoNil_EmptyObject (case e): with no AuditRepo
// configured runCostSummary returns "nothing to report", so the handler
// renders an empty object (200).
func TestGetRunCost_AuditRepoNil_EmptyObject(t *testing.T) {
	rr := newCostRunRepo()
	id := uuid.New()
	seedCostRun(rr, id, 0, nil)
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr}) // no AuditRepo

	code, raw := getCost(t, s, id.String())
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(raw) != 0 {
		t.Errorf("nil-AuditRepo response must be an empty object; got %v", raw)
	}
}

// TestGetRunCost_AuditListError_500 (case f): a cost_recorded audit-list
// failure surfaces as 500 (the run existence check passes first).
func TestGetRunCost_AuditListError_500(t *testing.T) {
	rr := newCostRunRepo()
	id := uuid.New()
	seedCostRun(rr, id, 0, nil)
	af := &costAuditFake{costByRun: map[uuid.UUID][]*audit.Entry{}, err: errors.New("boom")}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: af})

	code, _ := getCost(t, s, id.String())
	if code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", code)
	}
}

// TestGetRunCost_ListRunsError_500: a ListRuns failure during the merged-PR
// rollup surfaces as 500 (the cost ledger read succeeds, then the per-PR
// lineage query fails).
func TestGetRunCost_ListRunsError_500(t *testing.T) {
	rr := newCostRunRepo()
	url := "https://github.com/x/y/pull/9"
	id := uuid.New()
	seedCostRun(rr, id, 4.00, ptr(url))
	rr.listErr = errors.New("list boom")
	af := &costAuditFake{
		costByRun:   map[uuid.UUID][]*audit.Entry{id: {costRecordedEntry(4.00, "")}},
		mergedByRun: map[uuid.UUID][]*audit.Entry{id: {{Category: CategoryPRMerged}}},
	}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: af})

	code, _ := getCost(t, s, id.String())
	if code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", code)
	}
}

// TestGetRunCost_BadUUID_400 (case g).
func TestGetRunCost_BadUUID_400(t *testing.T) {
	rr := newCostRunRepo()
	af := &costAuditFake{costByRun: map[uuid.UUID][]*audit.Entry{}}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: af})

	code, _ := getCost(t, s, "not-a-uuid")
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
}

// TestGetRunCost_MissingRun_404 (case h): an unknown run id is a 404, distinct
// from the no-cost-data 200 — the GetRun existence check.
func TestGetRunCost_MissingRun_404(t *testing.T) {
	rr := newCostRunRepo()
	af := &costAuditFake{costByRun: map[uuid.UUID][]*audit.Entry{}}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: af})

	code, raw := getCost(t, s, uuid.New().String())
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404\nbody: %v", code, raw)
	}
}
