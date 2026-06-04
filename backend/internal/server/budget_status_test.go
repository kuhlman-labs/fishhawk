package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// budgetStatusSpec builds a one-workflow manifest carrying a single
// weekly periodic budget. enforcement is omitted from the YAML when
// empty (the default-advisory zero-value case, concern 1); warnAt is
// omitted when nil.
func budgetStatusSpec(limitUSD float64, enforcement string, warnAt *float64) []byte {
	var b string
	b = fmt.Sprintf(`
version: "0.4"
workflows:
  feature_change:
    description: "x"
    budgets:
      - period: weekly
        limit_usd: %g
`, limitUSD)
	if enforcement != "" {
		b += fmt.Sprintf("        enforcement: %s\n", enforcement)
	}
	if warnAt != nil {
		b += fmt.Sprintf("        warn_at: %g\n", *warnAt)
	}
	b += `    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`
	return []byte(b)
}

// seedBudgetRun stands up a run keyed by a fresh id carrying the given
// workflow spec and in-period spend, and returns the id. The run's own
// CostUSDTotal is what SumWorkflowCostInRange totals for the current
// period (it matches repo/workflow and CreatedAt is now).
func seedBudgetStatusRun(rr *approvalRunRepo, spec []byte, spentUSD float64) uuid.UUID {
	id := uuid.New()
	rr.seedRun(&run.Run{
		ID:            id,
		Repo:          "x/y",
		WorkflowID:    "feature_change",
		TriggerSource: run.TriggerCLI,
		CostUSDTotal:  spentUSD,
		WorkflowSpec:  spec,
		CreatedAt:     time.Now().UTC(),
	})
	return id
}

func getRunBudget(t *testing.T, s *Server, runID string) (int, map[string]json.RawMessage) {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v0/runs/%s/budget", runID), nil)
	s.Handler().ServeHTTP(w, req)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode response (status %d): %v\nbody: %s", w.Code, err, w.Body.String())
	}
	return w.Code, raw
}

func TestGetRunBudget_NoBudget_EmptyObjectNoKey(t *testing.T) {
	rr := newApprovalRunRepo()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr})

	// A workflow with no budgets at all.
	spec := []byte(`
version: "0.4"
workflows:
  feature_change:
    description: "x"
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`)
	id := seedBudgetStatusRun(rr, spec, 10)

	code, raw := getRunBudget(t, s, id.String())
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if _, ok := raw["period"]; ok {
		t.Errorf("no-budget response must not carry a period key; got %v", raw)
	}
	if _, ok := raw["tier"]; ok {
		t.Errorf("no-budget response must not carry a tier key; got %v", raw)
	}
}

func TestGetRunBudget_AdvisoryUnderWarn_TierOK(t *testing.T) {
	rr := newApprovalRunRepo()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr})

	warn := 0.8
	id := seedBudgetStatusRun(rr, budgetStatusSpec(100, "advisory", &warn), 10)

	code, raw := getRunBudget(t, s, id.String())
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	assertJSONString(t, raw, "tier", "ok")
	assertJSONString(t, raw, "enforcement", "advisory")
	assertJSONFloat(t, raw, "fraction", 0.1)
	assertJSONFloat(t, raw, "spent_usd", 10)
	assertJSONFloat(t, raw, "limit_usd", 100)
	if _, ok := raw["warn_at"]; !ok {
		t.Error("expected warn_at present when configured")
	}
	if _, ok := raw["period_start"]; !ok {
		t.Error("expected period_start present")
	}
}

func TestGetRunBudget_WarnCrossed_TierWarn(t *testing.T) {
	rr := newApprovalRunRepo()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr})

	warn := 0.5
	id := seedBudgetStatusRun(rr, budgetStatusSpec(100, "advisory", &warn), 60)

	code, raw := getRunBudget(t, s, id.String())
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	assertJSONString(t, raw, "tier", "warn")
	assertJSONFloat(t, raw, "fraction", 0.6)
}

// TestGetRunBudget_Over_TierOver mirrors the #693 evidence: $165.86 spent
// against a $50 limit — fraction > 1, tier over.
func TestGetRunBudget_Over_TierOver(t *testing.T) {
	rr := newApprovalRunRepo()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr})

	warn := 0.8
	id := seedBudgetStatusRun(rr, budgetStatusSpec(50, "advisory", &warn), 165.86)

	code, raw := getRunBudget(t, s, id.String())
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	assertJSONString(t, raw, "tier", "over")
	var frac float64
	if err := json.Unmarshal(raw["fraction"], &frac); err != nil {
		t.Fatalf("decode fraction: %v", err)
	}
	if frac <= 1 {
		t.Errorf("fraction = %g, want > 1", frac)
	}
}

// TestGetRunBudget_DefaultAdvisory_NormalizesEnforcement is concern 1:
// a spec budget with no enforcement key surfaces enforcement:"advisory"
// on the wire — never the empty string.
func TestGetRunBudget_DefaultAdvisory_NormalizesEnforcement(t *testing.T) {
	rr := newApprovalRunRepo()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr})

	id := seedBudgetStatusRun(rr, budgetStatusSpec(100, "", nil), 10)

	code, raw := getRunBudget(t, s, id.String())
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	assertJSONString(t, raw, "enforcement", "advisory")
	assertJSONString(t, raw, "tier", "ok")
	// warn_at omitted when unconfigured.
	if _, ok := raw["warn_at"]; ok {
		t.Errorf("warn_at must be omitted when not configured; got %v", raw["warn_at"])
	}
}

func TestGetRunBudget_BadUUID_400(t *testing.T) {
	rr := newApprovalRunRepo()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr})

	code, _ := getRunBudget(t, s, "not-a-uuid")
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
}

func TestGetRunBudget_MissingRun_404(t *testing.T) {
	rr := newApprovalRunRepo()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr})

	code, raw := getRunBudget(t, s, uuid.New().String())
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404\nbody: %v", code, raw)
	}
}

func assertJSONString(t *testing.T, raw map[string]json.RawMessage, key, want string) {
	t.Helper()
	v, ok := raw[key]
	if !ok {
		t.Fatalf("response missing %q key; got %v", key, raw)
	}
	var got string
	if err := json.Unmarshal(v, &got); err != nil {
		t.Fatalf("decode %q: %v", key, err)
	}
	if got != want {
		t.Errorf("%s = %q, want %q", key, got, want)
	}
}

func assertJSONFloat(t *testing.T, raw map[string]json.RawMessage, key string, want float64) {
	t.Helper()
	v, ok := raw[key]
	if !ok {
		t.Fatalf("response missing %q key; got %v", key, raw)
	}
	var got float64
	if err := json.Unmarshal(v, &got); err != nil {
		t.Fatalf("decode %q: %v", key, err)
	}
	if got != want {
		t.Errorf("%s = %g, want %g", key, got, want)
	}
}
