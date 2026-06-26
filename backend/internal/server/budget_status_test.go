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

// TestGetRunBudget_Over_TierOver exercises the 'over' band specifically:
// spend between 1x and the 2x ack rung (here $75 against a $50 limit →
// 1.5x). The #693 saturating evidence ($165.86 vs $50 ≈ 3.3x) now escalates
// past 'over' to 'page' under the #1371 ladder — that band is covered by
// TestGetRunBudget_Page_TierAndFlag.
func TestGetRunBudget_Over_TierOver(t *testing.T) {
	rr := newApprovalRunRepo()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr})

	warn := 0.8
	id := seedBudgetStatusRun(rr, budgetStatusSpec(50, "advisory", &warn), 75)

	code, raw := getRunBudget(t, s, id.String())
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	assertJSONString(t, raw, "tier", "over")
	assertJSONBool(t, raw, "ack_required", false)
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

// assertJSONBool asserts a boolean JSON field equals want.
func assertJSONBool(t *testing.T, raw map[string]json.RawMessage, key string, want bool) {
	t.Helper()
	v, ok := raw[key]
	if !ok {
		if !want {
			return // omitempty drops a false ack_required — absence == false
		}
		t.Fatalf("response missing %q key; got %v", key, raw)
	}
	var got bool
	if err := json.Unmarshal(v, &got); err != nil {
		t.Fatalf("decode %q: %v", key, err)
	}
	if got != want {
		t.Errorf("%s = %v, want %v", key, got, want)
	}
}

// TestGetRunBudget_AckRequired_TierAndFlag is the #1371 escalation band:
// spend at the 2x default ack multiple surfaces tier=ack_required and
// ack_required=true (no override / multiples configured → budget.Tier's
// 2x/3x defaults apply).
func TestGetRunBudget_AckRequired_TierAndFlag(t *testing.T) {
	rr := newApprovalRunRepo()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr})

	warn := 0.8
	// limit 100, spend 250 → fraction 2.5 → ack_required (>= 2x, < 3x).
	id := seedBudgetStatusRun(rr, budgetStatusSpec(100, "advisory", &warn), 250)

	code, raw := getRunBudget(t, s, id.String())
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	assertJSONString(t, raw, "tier", "ack_required")
	assertJSONBool(t, raw, "ack_required", true)
	assertJSONFloat(t, raw, "fraction", 2.5)
}

// TestGetRunBudget_Page_TierAndFlag: spend at the 3x default page multiple
// surfaces tier=page and ack_required=true.
func TestGetRunBudget_Page_TierAndFlag(t *testing.T) {
	rr := newApprovalRunRepo()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr})

	warn := 0.8
	// limit 100, spend 350 → fraction 3.5 → page (>= 3x).
	id := seedBudgetStatusRun(rr, budgetStatusSpec(100, "advisory", &warn), 350)

	code, raw := getRunBudget(t, s, id.String())
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	assertJSONString(t, raw, "tier", "page")
	assertJSONBool(t, raw, "ack_required", true)
}

// TestGetRunBudget_BelowAck_NoAckRequired confirms the ok/warn/over bands
// carry ack_required=false (omitted on the wire by omitempty).
func TestGetRunBudget_BelowAck_NoAckRequired(t *testing.T) {
	rr := newApprovalRunRepo()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr})

	warn := 0.8
	// limit 100, spend 150 → fraction 1.5 → over, NOT ack_required.
	id := seedBudgetStatusRun(rr, budgetStatusSpec(100, "advisory", &warn), 150)

	code, raw := getRunBudget(t, s, id.String())
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	assertJSONString(t, raw, "tier", "over")
	assertJSONBool(t, raw, "ack_required", false)
}

// TestGetRunBudget_LimitOverride_ChangesLimitFractionTier is the #1371
// calibration seam: BudgetLimitOverrideUSD > 0 replaces the spec limit, so
// limit_usd, fraction, and tier all report against the effective limit.
// Same seeded spend ($150) reads 'over' against the $100 spec limit but
// only 'warn' against a $300 calibrated override.
func TestGetRunBudget_LimitOverride_ChangesLimitFractionTier(t *testing.T) {
	rr := newApprovalRunRepo()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, BudgetLimitOverrideUSD: 300})

	warn := 0.8
	// spec limit 100, override 300, spend 150 → fraction 0.5 against the
	// effective $300 limit → tier ok (0.5 < warn 0.8).
	id := seedBudgetStatusRun(rr, budgetStatusSpec(100, "advisory", &warn), 150)

	code, raw := getRunBudget(t, s, id.String())
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	assertJSONFloat(t, raw, "limit_usd", 300)
	assertJSONFloat(t, raw, "fraction", 0.5)
	assertJSONString(t, raw, "tier", "ok")
}

// TestGetRunBudget_ZeroValueMultiples_FallBackToDefaults pins the defensive
// fallback at the response seam: a Server built with no BudgetAckMultiple /
// BudgetPageMultiple (both zero) must NOT collapse an over-limit fraction
// into 'page'. Spend 1.5x reads 'over', not 'page'.
func TestGetRunBudget_ZeroValueMultiples_FallBackToDefaults(t *testing.T) {
	rr := newApprovalRunRepo()
	// Note: no BudgetAckMultiple/BudgetPageMultiple set — both default 0.
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr})

	warn := 0.8
	id := seedBudgetStatusRun(rr, budgetStatusSpec(100, "advisory", &warn), 150)

	code, raw := getRunBudget(t, s, id.String())
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	// With the bad zero pair honored, 1.5 >= 0 would be 'page'; the
	// fallback to 2x/3x keeps it 'over'.
	assertJSONString(t, raw, "tier", "over")
	assertJSONBool(t, raw, "ack_required", false)
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
