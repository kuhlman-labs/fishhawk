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

// cacheEffAuditFake serves synthetic cost_recorded entries per run for the
// cache-efficiency handler. It embeds audit.BaseFake so only the one method
// the handler exercises is overridden.
type cacheEffAuditFake struct {
	audit.BaseFake
	byRun map[uuid.UUID][]*audit.Entry
	err   error
}

func (f *cacheEffAuditFake) ListForRunByCategory(_ context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	if f.err != nil {
		return nil, f.err
	}
	if category != "cost_recorded" {
		return nil, nil
	}
	return f.byRun[runID], nil
}

// costEntry builds a cost_recorded audit entry payload. An empty source is
// omitted from the payload, mirroring the runner stage-agent path.
func costEntry(model, source string, fresh, read, write, output int) *audit.Entry {
	m := map[string]any{
		"model":                    model,
		"input_tokens":             fresh,
		"output_tokens":            output,
		"cache_read_input_tokens":  read,
		"cache_write_input_tokens": write,
	}
	if source != "" {
		m["source"] = source
	}
	payload, _ := json.Marshal(m)
	return &audit.Entry{Category: "cost_recorded", Payload: payload}
}

func getCacheEfficiency(t *testing.T, s *Server, runID string) (int, map[string]json.RawMessage) {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v0/runs/"+runID+"/cache-efficiency", nil)
	s.Handler().ServeHTTP(w, req)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode response (status %d): %v\nbody: %s", w.Code, err, w.Body.String())
	}
	return w.Code, raw
}

func seedCacheEffRun(rr *approvalRunRepo, id uuid.UUID) {
	rr.seedRun(&run.Run{
		ID:            id,
		Repo:          "x/y",
		WorkflowID:    "feature_change",
		TriggerSource: run.TriggerCLI,
		CreatedAt:     time.Now().UTC(),
	})
}

// TestGetRunCacheEfficiency_MultiModelMultiStage drives the full
// handler → runCacheEfficiency → cost.AggregateCacheEfficiency path and
// asserts the per-run ratios/savings and per-stage breakdown. The figures
// mirror the cost-package values test (opus + gpt-5.5 across
// plan_review / implement_review / agent).
func TestGetRunCacheEfficiency_MultiModelMultiStage(t *testing.T) {
	const M = 1_000_000
	rr := newApprovalRunRepo()
	id := uuid.New()
	seedCacheEffRun(rr, id)
	af := &cacheEffAuditFake{byRun: map[uuid.UUID][]*audit.Entry{
		id: {
			costEntry("claude-opus-4-8", "plan_review", 1*M, 3*M, 1*M, 0),
			costEntry("gpt-5.5-2026", "implement_review", 1*M, 1*M, 1*M, 0),
			costEntry("claude-opus-4-8", "", 2*M, 0, 0, 0), // runner stage-agent, no source
		},
	}}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: af})

	code, raw := getCacheEfficiency(t, s, id.String())
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	assertJSONFloat(t, raw, "cache_read_ratio", 0.5)
	assertJSONFloat(t, raw, "reuse_factor", 2.0)
	assertJSONFloat(t, raw, "gross_read_savings_usd", 18.0)
	assertJSONFloat(t, raw, "write_penalty_usd", 1.25)
	assertJSONFloat(t, raw, "net_savings_usd", 16.75)

	var stages []cacheEfficiencyStageResult
	if err := json.Unmarshal(raw["stages"], &stages); err != nil {
		t.Fatalf("decode stages: %v", err)
	}
	if len(stages) != 3 {
		t.Fatalf("want 3 stages, got %d: %+v", len(stages), stages)
	}
	// Sorted by source: agent, implement_review, plan_review.
	wantSources := []string{"agent", "implement_review", "plan_review"}
	for i, src := range wantSources {
		if stages[i].Source != src {
			t.Errorf("stages[%d].Source = %q, want %q", i, stages[i].Source, src)
		}
	}
	if got := stages[2].NetSavingsUSD; got < 12.25-1e-9 || got > 12.25+1e-9 {
		t.Errorf("plan_review net = %v, want ~12.25", got)
	}
}

// TestGetRunCacheEfficiency_NoCostData_EmptyObject: a run with no
// cost_recorded entries returns 200 with an empty object (no keys), so the
// MCP client collapses it to nil.
func TestGetRunCacheEfficiency_NoCostData_EmptyObject(t *testing.T) {
	rr := newApprovalRunRepo()
	id := uuid.New()
	seedCacheEffRun(rr, id)
	af := &cacheEffAuditFake{byRun: map[uuid.UUID][]*audit.Entry{}}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: af})

	code, raw := getCacheEfficiency(t, s, id.String())
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(raw) != 0 {
		t.Errorf("no-cost-data response must be an empty object; got %v", raw)
	}
}

// TestGetRunCacheEfficiency_AuditRepoNil_EmptyObject: with no AuditRepo
// configured runCacheEfficiency returns "nothing to report", so the handler
// renders an empty object (200) rather than erroring.
func TestGetRunCacheEfficiency_AuditRepoNil_EmptyObject(t *testing.T) {
	rr := newApprovalRunRepo()
	id := uuid.New()
	seedCacheEffRun(rr, id)
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr}) // no AuditRepo

	code, raw := getCacheEfficiency(t, s, id.String())
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(raw) != 0 {
		t.Errorf("nil-AuditRepo response must be an empty object; got %v", raw)
	}
}

// TestGetRunCacheEfficiency_AuditListError_500: an audit-list failure is
// surfaced as 500 (the run existence check passes first, so this is the
// ledger-read failure, distinct from a missing run's 404).
func TestGetRunCacheEfficiency_AuditListError_500(t *testing.T) {
	rr := newApprovalRunRepo()
	id := uuid.New()
	seedCacheEffRun(rr, id)
	af := &cacheEffAuditFake{byRun: map[uuid.UUID][]*audit.Entry{}, err: errors.New("boom")}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: af})

	code, _ := getCacheEfficiency(t, s, id.String())
	if code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", code)
	}
}

// TestGetRunCacheEfficiency_BadUUID_400.
func TestGetRunCacheEfficiency_BadUUID_400(t *testing.T) {
	rr := newApprovalRunRepo()
	af := &cacheEffAuditFake{byRun: map[uuid.UUID][]*audit.Entry{}}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: af})

	code, _ := getCacheEfficiency(t, s, "not-a-uuid")
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
}

// TestGetRunCacheEfficiency_MissingRun_404: an unknown run id is a 404,
// distinct from the no-cost-data 200 — the GetRun existence check.
func TestGetRunCacheEfficiency_MissingRun_404(t *testing.T) {
	rr := newApprovalRunRepo()
	af := &cacheEffAuditFake{byRun: map[uuid.UUID][]*audit.Entry{}}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: af})

	code, raw := getCacheEfficiency(t, s, uuid.New().String())
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404\nbody: %v", code, raw)
	}
}
