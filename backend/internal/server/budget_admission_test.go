package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// blockingBudgetSpec builds a workflow-v0.4 spec whose feature_change
// workflow carries one weekly blocking budget (no warn_at — blocking
// gates at 100%, not a warn fraction).
func blockingBudgetSpec(limitUSD float64) string {
	return fmt.Sprintf(`version: "0.4"
workflows:
  feature_change:
    description: "x"
    budgets:
      - period: weekly
        limit_usd: %g
        enforcement: blocking
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`, limitUSD)
}

// advisoryBudgetSpec is the same shape with enforcement: advisory, so
// the gate must never fire even when spend is over 100%.
func advisoryBudgetSpec(limitUSD float64) string {
	return fmt.Sprintf(`version: "0.4"
workflows:
  feature_change:
    description: "x"
    budgets:
      - period: weekly
        limit_usd: %g
        enforcement: advisory
        warn_at: 0.5
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`, limitUSD)
}

// budgetRunRepo embeds the in-memory fakeRepo and adds the
// SumWorkflowCostInRange capability (webhook.CostSummer) so the
// admission gate has a cost source to evaluate. spent is returned for
// every range query; sumErr forces the fail-open path.
type budgetRunRepo struct {
	*fakeRepo
	spent  float64
	sumErr error
}

func newBudgetRunRepo() *budgetRunRepo {
	return &budgetRunRepo{fakeRepo: newFakeRepo()}
}

func (r *budgetRunRepo) SumWorkflowCostInRange(_ context.Context, _, _ string, _, _ time.Time) (float64, error) {
	if r.sumErr != nil {
		return 0, r.sumErr
	}
	return r.spent, nil
}

// postCreateRun drives handleCreateRun via httptest and returns the
// recorder. budgetOverride threads the budget_override request field.
func postCreateRun(t *testing.T, s *Server, specYAML string, budgetOverride bool) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"repo":            "x/y",
		"workflow_id":     "feature_change",
		"workflow_sha":    "abc",
		"trigger_source":  "cli",
		"workflow_spec":   specYAML,
		"budget_override": budgetOverride,
	})
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleCreateRun(w, withAuth(req))
	return w
}

func countGlobalAudits(au *auditFake, category string) int {
	au.mu.Lock()
	defer au.mu.Unlock()
	n := 0
	for _, p := range au.globalAppended {
		if p.Category == category {
			n++
		}
	}
	return n
}

func runRowCount(r *fakeRepo) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.runs)
}

// TestCreateRun_BlockingBudget_Exhausted_Refused is the headline
// admission gate: a blocking budget whose period spend has crossed the
// limit refuses a NEW run with 402 budget_exhausted, writes a
// run_rejected_budget audit, and creates NO run row.
func TestCreateRun_BlockingBudget_Exhausted_Refused(t *testing.T) {
	au := newAuditFake()
	rr := newBudgetRunRepo()
	rr.spent = 100 // over the 50 limit
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, RunRepo: rr})

	w := postCreateRun(t, s, blockingBudgetSpec(50), false)
	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402:\n%s", w.Code, w.Body.String())
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if env.Error.Code != "budget_exhausted" {
		t.Errorf("error code = %q, want budget_exhausted", env.Error.Code)
	}
	if n := countGlobalAudits(au, "run_rejected_budget"); n != 1 {
		t.Errorf("run_rejected_budget audits = %d, want 1", n)
	}
	if n := runRowCount(rr.fakeRepo); n != 0 {
		t.Errorf("run rows created = %d, want 0 (refused before CreateRun)", n)
	}
}

// TestCreateRun_BlockingBudget_Override_Admitted: with
// budget_override the same exhausted budget admits the run (201) and
// records a run_admitted_budget_override audit instead.
func TestCreateRun_BlockingBudget_Override_Admitted(t *testing.T) {
	au := newAuditFake()
	rr := newBudgetRunRepo()
	rr.spent = 100
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, RunRepo: rr})

	w := postCreateRun(t, s, blockingBudgetSpec(50), true)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	if n := countGlobalAudits(au, "run_admitted_budget_override"); n != 1 {
		t.Errorf("run_admitted_budget_override audits = %d, want 1", n)
	}
	if n := countGlobalAudits(au, "run_rejected_budget"); n != 0 {
		t.Errorf("run_rejected_budget audits = %d, want 0 on override", n)
	}
	if n := runRowCount(rr.fakeRepo); n != 1 {
		t.Errorf("run rows created = %d, want 1 (override admits)", n)
	}
}

// TestCreateRun_BlockingBudget_UnderLimit_Admitted: spend under the
// limit admits with no budget audit.
func TestCreateRun_BlockingBudget_UnderLimit_Admitted(t *testing.T) {
	au := newAuditFake()
	rr := newBudgetRunRepo()
	rr.spent = 10
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, RunRepo: rr})

	w := postCreateRun(t, s, blockingBudgetSpec(50), false)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	if n := countGlobalAudits(au, "run_rejected_budget"); n != 0 {
		t.Errorf("run_rejected_budget audits = %d, want 0 under limit", n)
	}
}

// TestCreateRun_AdvisoryBudget_OverLimit_NeverGated: an advisory
// budget over 100% never blocks admission.
func TestCreateRun_AdvisoryBudget_OverLimit_NeverGated(t *testing.T) {
	au := newAuditFake()
	rr := newBudgetRunRepo()
	rr.spent = 999
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, RunRepo: rr})

	w := postCreateRun(t, s, advisoryBudgetSpec(50), false)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (advisory never gates):\n%s", w.Code, w.Body.String())
	}
	if n := countGlobalAudits(au, "run_rejected_budget"); n != 0 {
		t.Errorf("run_rejected_budget audits = %d, want 0 for advisory", n)
	}
}

// TestCreateRun_BlockingBudget_CapabilityAbsent_Admitted: a RunRepo
// that doesn't implement CostSummer admits the run (capability-absent
// skip).
func TestCreateRun_BlockingBudget_CapabilityAbsent_Admitted(t *testing.T) {
	au := newAuditFake()
	rr := newFakeRepo() // no SumWorkflowCostInRange
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, RunRepo: rr})

	w := postCreateRun(t, s, blockingBudgetSpec(50), false)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (capability-absent admits):\n%s", w.Code, w.Body.String())
	}
}

// TestCreateRun_BlockingBudget_SumError_FailOpen: a sum error admits
// the run (fail-open) with no budget audit.
func TestCreateRun_BlockingBudget_SumError_FailOpen(t *testing.T) {
	au := newAuditFake()
	rr := newBudgetRunRepo()
	rr.sumErr = errors.New("db down")
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, RunRepo: rr})

	w := postCreateRun(t, s, blockingBudgetSpec(50), false)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (fail-open on sum error):\n%s", w.Code, w.Body.String())
	}
	if n := countGlobalAudits(au, "run_rejected_budget"); n != 0 {
		t.Errorf("run_rejected_budget audits = %d, want 0 on fail-open", n)
	}
}

// Compile-time assertion that the embedded fake plus the cost-summer
// method still satisfies run.Repository — guards against signature drift.
var _ run.Repository = (*budgetRunRepo)(nil)
