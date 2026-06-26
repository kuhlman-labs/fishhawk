package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// budgetTierNoBudgetSpec is a one-workflow manifest with NO budgets at
// all — the "nothing to gate on" path for checkPeriodicBudgetTier.
func budgetTierNoBudgetSpec() []byte {
	return []byte(`
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
}

// seedTierRun seeds a plan stage plus its run carrying the given spec and
// in-period spend (the run's own CostUSDTotal is what
// SumWorkflowCostInRange totals for the current period), and returns the
// stage so a test can drive checkPeriodicBudgetTier against it.
func seedTierRun(rr *approvalRunRepo, specYAML []byte, spentUSD float64) *run.Stage {
	stage := rr.seedStage(run.StageStateAwaitingApproval)
	rr.seedRun(&run.Run{
		ID:            stage.RunID,
		Repo:          "x/y",
		WorkflowID:    "feature_change",
		TriggerSource: run.TriggerCLI,
		CostUSDTotal:  spentUSD,
		WorkflowSpec:  specYAML,
		CreatedAt:     time.Now().UTC(),
	})
	return stage
}

// countTierAudits returns how many appended audit entries carry category.
func countTierAudits(au *approvalAuditFake, category string) int {
	au.mu.Lock()
	defer au.mu.Unlock()
	n := 0
	for _, e := range au.appended {
		if e.Category == category {
			n++
		}
	}
	return n
}

// erroringSummerRepo wraps approvalRunRepo and forces SumWorkflowCostInRange
// to fail, exercising checkPeriodicBudgetTier's fail-open summer-error branch.
type erroringSummerRepo struct {
	*approvalRunRepo
	err error
}

func (r *erroringSummerRepo) SumWorkflowCostInRange(context.Context, string, string, time.Time, time.Time) (float64, error) {
	return 0, r.err
}

// callTierGate drives s.checkPeriodicBudgetTier against the stage with the
// given comment and returns (proceed, statusCode).
func callTierGate(s *Server, stage *run.Stage, comment string) (bool, int) {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	proceed := s.checkPeriodicBudgetTier(w, req, stage, comment)
	return proceed, w.Code
}

// TestCheckPeriodicBudgetTier_NoAdvisoryBudget_Proceeds: a workflow that
// declares no advisory budget has nothing to gate on (#1371).
func TestCheckPeriodicBudgetTier_NoAdvisoryBudget_Proceeds(t *testing.T) {
	au := newApprovalAuditFake()
	rr := newApprovalRunRepo()
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, RunRepo: rr})

	stage := seedTierRun(rr, budgetTierNoBudgetSpec(), 1000)
	proceed, code := callTierGate(s, stage, "")
	if !proceed {
		t.Errorf("no advisory budget must proceed; got refuse (code %d)", code)
	}
}

// TestCheckPeriodicBudgetTier_NonSummerRepo_Proceeds: a RunRepo that does
// not implement runCostSummer has no period-sum source — fail-open.
func TestCheckPeriodicBudgetTier_NonSummerRepo_Proceeds(t *testing.T) {
	au := newApprovalAuditFake()
	rr := newFakeRepo() // no SumWorkflowCostInRange
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, RunRepo: rr})

	stage := &run.Stage{ID: uuid.New(), RunID: uuid.New()}
	proceed, code := callTierGate(s, stage, "")
	if !proceed {
		t.Errorf("non-summer RunRepo must fail open and proceed; got refuse (code %d)", code)
	}
}

// TestCheckPeriodicBudgetTier_SummerError_Proceeds: a period-sum query
// failure fails open rather than bricking the gate.
func TestCheckPeriodicBudgetTier_SummerError_Proceeds(t *testing.T) {
	au := newApprovalAuditFake()
	base := newApprovalRunRepo()
	rr := &erroringSummerRepo{approvalRunRepo: base, err: errors.New("boom")}
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, RunRepo: rr})

	warn := 0.5
	stage := seedTierRun(base, budgetStatusSpec(100, "advisory", &warn), 250)
	proceed, code := callTierGate(s, stage, "")
	if !proceed {
		t.Errorf("summer error must fail open and proceed; got refuse (code %d)", code)
	}
}

// TestCheckPeriodicBudgetTier_BelowAck_Proceeds: spend below the 2x default
// ack multiple (here over the limit but only 1.5x) stays out of the gate.
func TestCheckPeriodicBudgetTier_BelowAck_Proceeds(t *testing.T) {
	au := newApprovalAuditFake()
	rr := newApprovalRunRepo()
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, RunRepo: rr})

	warn := 0.5
	// limit 100, spend 150 → fraction 1.5 → 'over', below the 2x ack rung.
	stage := seedTierRun(rr, budgetStatusSpec(100, "advisory", &warn), 150)
	proceed, code := callTierGate(s, stage, "")
	if !proceed {
		t.Errorf("below-ack spend must proceed; got refuse (code %d)", code)
	}
	if n := countTierAudits(au, "plan_violates_periodic_budget"); n != 0 {
		t.Errorf("below-ack must not record a violation; got %d", n)
	}
}

// TestCheckPeriodicBudgetTier_AckRequiredNoFlag_Refuses: at the 2x ack rung
// without --ack-budget, the gate refuses 422 and records the violation.
func TestCheckPeriodicBudgetTier_AckRequiredNoFlag_Refuses(t *testing.T) {
	au := newApprovalAuditFake()
	rr := newApprovalRunRepo()
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, RunRepo: rr})

	warn := 0.5
	// limit 100, spend 250 → fraction 2.5 → 'ack_required'.
	stage := seedTierRun(rr, budgetStatusSpec(100, "advisory", &warn), 250)
	proceed, code := callTierGate(s, stage, "")
	if proceed {
		t.Error("ack_required without --ack-budget must refuse")
	}
	if code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", code)
	}
	if n := countTierAudits(au, "plan_violates_periodic_budget"); n != 1 {
		t.Errorf("plan_violates_periodic_budget count = %d, want 1", n)
	}
	if n := countTierAudits(au, "plan_periodic_budget_tier_acknowledged"); n != 0 {
		t.Errorf("must not record an ack on a refusal; got %d", n)
	}
}

// TestCheckPeriodicBudgetTier_AckRequiredWithFlag_Proceeds: --ack-budget at
// the ack rung proceeds and records the acknowledgment.
func TestCheckPeriodicBudgetTier_AckRequiredWithFlag_Proceeds(t *testing.T) {
	au := newApprovalAuditFake()
	rr := newApprovalRunRepo()
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, RunRepo: rr})

	warn := 0.5
	stage := seedTierRun(rr, budgetStatusSpec(100, "advisory", &warn), 250)
	proceed, code := callTierGate(s, stage, "approved --ack-budget")
	if !proceed {
		t.Errorf("ack_required with --ack-budget must proceed; got refuse (code %d)", code)
	}
	if n := countTierAudits(au, "plan_periodic_budget_tier_acknowledged"); n != 1 {
		t.Errorf("plan_periodic_budget_tier_acknowledged count = %d, want 1", n)
	}
	if n := countTierAudits(au, "plan_violates_periodic_budget"); n != 0 {
		t.Errorf("must not record a violation when acknowledged; got %d", n)
	}
}

// TestCheckPeriodicBudgetTier_PageNoFlag_Refuses: the highest 'page' rung
// (3x default) without --ack-budget also refuses 422.
func TestCheckPeriodicBudgetTier_PageNoFlag_Refuses(t *testing.T) {
	au := newApprovalAuditFake()
	rr := newApprovalRunRepo()
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, RunRepo: rr})

	warn := 0.5
	// limit 100, spend 350 → fraction 3.5 → 'page'.
	stage := seedTierRun(rr, budgetStatusSpec(100, "advisory", &warn), 350)
	proceed, code := callTierGate(s, stage, "")
	if proceed {
		t.Error("page tier without --ack-budget must refuse")
	}
	if code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", code)
	}
	if n := countTierAudits(au, "plan_violates_periodic_budget"); n != 1 {
		t.Errorf("plan_violates_periodic_budget count = %d, want 1", n)
	}
}

// TestCheckPeriodicBudgetTier_PageWithFlag_Proceeds: --ack-budget at the
// page rung proceeds and records the acknowledgment.
func TestCheckPeriodicBudgetTier_PageWithFlag_Proceeds(t *testing.T) {
	au := newApprovalAuditFake()
	rr := newApprovalRunRepo()
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, RunRepo: rr})

	warn := 0.5
	stage := seedTierRun(rr, budgetStatusSpec(100, "advisory", &warn), 350)
	proceed, code := callTierGate(s, stage, "approved --ack-budget")
	if !proceed {
		t.Errorf("page with --ack-budget must proceed; got refuse (code %d)", code)
	}
	if n := countTierAudits(au, "plan_periodic_budget_tier_acknowledged"); n != 1 {
		t.Errorf("plan_periodic_budget_tier_acknowledged count = %d, want 1", n)
	}
}
