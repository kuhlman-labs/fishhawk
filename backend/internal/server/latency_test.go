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
	"github.com/kuhlman-labs/fishhawk/backend/internal/drive"
	"github.com/kuhlman-labs/fishhawk/backend/internal/latency"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// latencyAuditFake serves a synthetic full audit chain per run for the latency
// handler. It embeds audit.BaseFake so only ListForRun is overridden — the
// latency rollup pairs multiple categories, so it reads the WHOLE chain.
type latencyAuditFake struct {
	audit.BaseFake
	chainByRun map[uuid.UUID][]*audit.Entry
	err        error
}

func (f *latencyAuditFake) ListForRun(_ context.Context, runID uuid.UUID) ([]*audit.Entry, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.chainByRun[runID], nil
}

// latencyBase is a fixed run-start instant so the tests are deterministic.
var latencyBase = time.Date(2026, 7, 8, 9, 0, 0, 0, time.UTC)

// entryAt builds an audit entry of the given category at latencyBase+d.
func entryAt(category string, d time.Duration) *audit.Entry {
	return &audit.Entry{Category: category, Timestamp: latencyBase.Add(d)}
}

// checksGreenEntryAt builds the run_auto_advanced entry the server maps to the
// synthetic ci_green boundary (payload rule = checks_green_awaiting_merge).
func checksGreenEntryAt(d time.Duration) *audit.Entry {
	payload, _ := json.Marshal(map[string]any{"rule": string(drive.RuleChecksGreenAwaitingMerge)})
	return &audit.Entry{Category: drive.Category, Timestamp: latencyBase.Add(d), Payload: payload}
}

func newLatencyRunRepo(id uuid.UUID) *approvalRunRepo {
	rr := newApprovalRunRepo()
	rr.seedRun(&run.Run{
		ID:            id,
		Repo:          "x/y",
		WorkflowID:    "feature_change",
		TriggerSource: run.TriggerCLI,
		CreatedAt:     latencyBase, // runStart
	})
	return rr
}

func getLatency(t *testing.T, s *Server, runID string) (int, map[string]json.RawMessage) {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v0/runs/"+runID+"/latency", nil)
	s.Handler().ServeHTTP(w, req)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode response (status %d): %v\nbody: %s", w.Code, err, w.Body.String())
	}
	return w.Code, raw
}

// TestGetRunLatency_FullyGatedChain: a complete chain resolves all three gates,
// the JSON shape carries each gate's wait, the total is their sum, the wall
// clock is CreatedAt→pr_merged, and each gate reconciles with the seeded
// timestamps.
func TestGetRunLatency_FullyGatedChain(t *testing.T) {
	id := uuid.New()
	rr := newLatencyRunRepo(id)
	af := &latencyAuditFake{chainByRun: map[uuid.UUID][]*audit.Entry{
		id: {
			entryAt(latency.CategoryPlanGenerated, 1*time.Minute),
			entryAt(latency.CategoryApprovalSubmitted, 6*time.Minute), // plan_approval = 300s
			entryAt(latency.CategoryImplementReviewed, 20*time.Minute),
			entryAt(latency.CategoryImplementReviewed, 22*time.Minute),    // terminal review
			entryAt(latency.CategoryAcceptanceDispatched, 25*time.Minute), // review→dispatch = 180s
			checksGreenEntryAt(30 * time.Minute),
			entryAt(latency.CategoryPRMerged, 40*time.Minute), // checks→merge = 600s
		},
	}}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: af})

	code, raw := getLatency(t, s, id.String())
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	assertJSONFloat(t, raw, "total_wait_on_human_seconds", 1080)
	assertJSONFloat(t, raw, "wall_clock_seconds", 2400) // 40 minutes

	var gates []latencyGateResult
	if err := json.Unmarshal(raw["gates"], &gates); err != nil {
		t.Fatalf("decode gates: %v", err)
	}
	if len(gates) != 3 {
		t.Fatalf("want 3 gates, got %d: %+v", len(gates), gates)
	}
	wantWait := map[string]float64{
		latency.GatePlanApproval:              300,
		latency.GateImplementReviewToDispatch: 180,
		latency.GateChecksGreenToMerge:        600,
	}
	for _, g := range gates {
		if wantWait[g.Gate] != g.WaitSeconds {
			t.Errorf("%s wait = %g, want %g", g.Gate, g.WaitSeconds, wantWait[g.Gate])
		}
		// Reconcile: reported wait == audit-timestamp delta.
		if delta := g.ClosedAt.Sub(g.OpenedAt).Seconds(); delta != g.WaitSeconds {
			t.Errorf("%s ClosedAt−OpenedAt = %g, but WaitSeconds = %g", g.Gate, delta, g.WaitSeconds)
		}
	}
}

// TestGetRunLatency_ChecksGreenFromAutoAdvance isolates the synthetic ci_green
// derivation: a run_auto_advanced entry whose rule is checks_green_awaiting_merge
// opens the checks→merge gate, while a run_auto_advanced entry with a DIFFERENT
// rule does NOT.
func TestGetRunLatency_ChecksGreenFromAutoAdvance(t *testing.T) {
	id := uuid.New()
	rr := newLatencyRunRepo(id)
	otherRule, _ := json.Marshal(map[string]any{"rule": string(drive.RulePlanApprovedDispatch)})
	af := &latencyAuditFake{chainByRun: map[uuid.UUID][]*audit.Entry{
		id: {
			// A non-checks-green auto-advance must be ignored as a boundary.
			{Category: drive.Category, Timestamp: latencyBase.Add(5 * time.Minute), Payload: otherRule},
			checksGreenEntryAt(10 * time.Minute),
			entryAt(latency.CategoryPRMerged, 12*time.Minute), // checks→merge = 120s
		},
	}}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: af})

	code, raw := getLatency(t, s, id.String())
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	var gates []latencyGateResult
	if err := json.Unmarshal(raw["gates"], &gates); err != nil {
		t.Fatalf("decode gates: %v", err)
	}
	if len(gates) != 1 || gates[0].Gate != latency.GateChecksGreenToMerge {
		t.Fatalf("want a single checks_green_to_merge gate, got %+v", gates)
	}
	if gates[0].WaitSeconds != 120 {
		t.Errorf("checks→merge wait = %g, want 120", gates[0].WaitSeconds)
	}
}

// TestGetRunLatency_MalformedAutoAdvancePayloadSkipped proves the
// gateEventCategory unmarshal-error branch is best-effort: a run_auto_advanced
// entry with a non-JSON payload is skipped (not a boundary, no panic), so the
// checks→merge gate never resolves and the response is the empty object.
func TestGetRunLatency_MalformedAutoAdvancePayloadSkipped(t *testing.T) {
	id := uuid.New()
	rr := newLatencyRunRepo(id)
	af := &latencyAuditFake{chainByRun: map[uuid.UUID][]*audit.Entry{
		id: {
			{Category: drive.Category, Timestamp: latencyBase.Add(5 * time.Minute), Payload: json.RawMessage("not-json")},
			entryAt(latency.CategoryPRMerged, 12*time.Minute),
		},
	}}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: af})

	code, raw := getLatency(t, s, id.String())
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(raw) != 0 {
		t.Errorf("malformed auto-advance payload must not resolve a gate; got %v", raw)
	}
}

// TestGetRunLatency_NoGatesEmptyObject: a chain that never reaches a gate
// boundary returns 200 with an empty object (no gates key), so callers branch
// on presence like /cost.
func TestGetRunLatency_NoGatesEmptyObject(t *testing.T) {
	id := uuid.New()
	rr := newLatencyRunRepo(id)
	af := &latencyAuditFake{chainByRun: map[uuid.UUID][]*audit.Entry{
		id: {
			entryAt("run_started", 0),
			entryAt(latency.CategoryPlanGenerated, 1*time.Minute), // opening only, no approval
		},
	}}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: af})

	code, raw := getLatency(t, s, id.String())
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(raw) != 0 {
		t.Errorf("want empty object for a run with no resolved gates, got %v", raw)
	}
}

// TestGetRunLatency_EmptyChainEmptyObject: a run with no audit entries at all
// returns 200 with an empty object.
func TestGetRunLatency_EmptyChainEmptyObject(t *testing.T) {
	id := uuid.New()
	rr := newLatencyRunRepo(id)
	af := &latencyAuditFake{chainByRun: map[uuid.UUID][]*audit.Entry{}}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: af})

	code, raw := getLatency(t, s, id.String())
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(raw) != 0 {
		t.Errorf("want empty object for a run with no audit entries, got %v", raw)
	}
}

// TestGetRunLatency_AuditRepoUnconfigured: with a RunRepo but no AuditRepo the
// handler returns 200 + empty object (the runLatencySummary nil-AuditRepo arm),
// never a 500.
func TestGetRunLatency_AuditRepoUnconfigured(t *testing.T) {
	id := uuid.New()
	rr := newLatencyRunRepo(id)
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr}) // AuditRepo nil

	code, raw := getLatency(t, s, id.String())
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(raw) != 0 {
		t.Errorf("want empty object when AuditRepo is unconfigured, got %v", raw)
	}
}

// TestGetRunLatency_UnknownRun404: an id that GetRun can't resolve returns 404.
func TestGetRunLatency_UnknownRun404(t *testing.T) {
	rr := newApprovalRunRepo() // no seeded run
	af := &latencyAuditFake{chainByRun: map[uuid.UUID][]*audit.Entry{}}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: af})

	code, raw := getLatency(t, s, uuid.New().String())
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
	if _, ok := raw["error"]; !ok {
		t.Errorf("want an error body, got %v", raw)
	}
}

// TestGetRunLatency_BadUUID400: a non-UUID path segment returns 400.
func TestGetRunLatency_BadUUID400(t *testing.T) {
	rr := newApprovalRunRepo()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr})

	code, _ := getLatency(t, s, "not-a-uuid")
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
}

// TestGetRunLatency_AuditListError500: a failing audit list surfaces as 500.
func TestGetRunLatency_AuditListError500(t *testing.T) {
	id := uuid.New()
	rr := newLatencyRunRepo(id)
	af := &latencyAuditFake{err: errors.New("boom")}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: af})

	code, _ := getLatency(t, s, id.String())
	if code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", code)
	}
}
