package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

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

// postCreateRunWithKey is postCreateRun carrying an Idempotency-Key header,
// for the create + replay pairs that pin the gate's post-replay position
// (#2366).
func postCreateRunWithKey(t *testing.T, s *Server, specYAML string, budgetOverride bool, key string) *httptest.ResponseRecorder {
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
	req.Header.Set("Idempotency-Key", key)
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
	// ADR-030 Amendment A1: the grant is bound to the CREATED run's own
	// chain, read back from the audit store rather than inferred.
	var created runResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	rows, _ := au.ListForRunByCategory(context.Background(), created.ID, CategoryRunBudgetOverrideBound)
	if len(rows) != 1 {
		t.Fatalf("%s rows on the created run = %d, want 1", CategoryRunBudgetOverrideBound, len(rows))
	}
	var p budgetOverrideBoundPayload
	if err := json.Unmarshal(rows[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.Period != "weekly" || p.LimitUSD != 50 || p.Spent != 100 || p.Repo != "x/y" || p.WorkflowID != "feature_change" || p.AdmittedVia != "create" {
		t.Errorf("binding payload = %+v", p)
	}
	if created.BudgetOverrideBinding == nil || !created.BudgetOverrideBinding.Recorded {
		t.Errorf("201 budget_override_binding = %+v, want recorded:true", created.BudgetOverrideBinding)
	}
}

// TestCreateRun_BlockingBudget_Override_BindingAppendFails_Surfaced (C5a): a
// failed binding append keeps the 201 and surfaces recorded:false with the
// hand-dispatch remedy, WARN-logged with the run id.
func TestCreateRun_BlockingBudget_Override_BindingAppendFails_Surfaced(t *testing.T) {
	au := newAuditFake()
	au.appendErrCategory = CategoryRunBudgetOverrideBound
	rr := newBudgetRunRepo()
	rr.spent = 100
	var logBuf bytes.Buffer
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, RunRepo: rr, Logger: slog.New(slog.NewTextHandler(&logBuf, nil))})

	w := postCreateRun(t, s, blockingBudgetSpec(50), true)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	var created runResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	b := created.BudgetOverrideBinding
	if b == nil || b.Recorded || !strings.Contains(b.Remedy, "fishhawk_dispatch_stage") {
		t.Fatalf("budget_override_binding = %+v, want recorded:false with a fishhawk_dispatch_stage remedy:\n%s", b, w.Body.String())
	}
	if !strings.Contains(logBuf.String(), "run_budget_override_bound failed") || !strings.Contains(logBuf.String(), created.ID.String()) {
		t.Errorf("WARN log missing the run id:\n%s", logBuf.String())
	}
}

// TestCreateRun_CannotNominateParentForBudgetExemption (C6) pins an EXISTING
// property, not a new control: createRunRequest has no lineage field and its
// decoder uses DisallowUnknownFields, so a plain create cannot name a parent
// to inherit an override exemption through.
func TestCreateRun_CannotNominateParentForBudgetExemption(t *testing.T) {
	au := newAuditFake()
	rr := newBudgetRunRepo()
	rr.spent = 10
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, RunRepo: rr})
	bound := uuid.New()
	for _, field := range []string{"decomposed_from", "parent_run_id"} {
		body, _ := json.Marshal(map[string]any{
			"repo": "x/y", "workflow_id": "feature_change", "workflow_sha": "abc", "trigger_source": "cli",
			"workflow_spec": blockingBudgetSpec(50), field: bound.String(),
		})
		req := httptest.NewRequest(http.MethodPost, "/v0/runs", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.handleCreateRun(w, withAuth(req))
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"validation_failed"`) {
			t.Fatalf("%s: status = %d, want 400 validation_failed:\n%s", field, w.Code, w.Body.String())
		}
	}
	if n := runRowCount(rr.fakeRepo); n != 0 {
		t.Fatalf("run rows = %d, want 0", n)
	}
	// A plain create admitted under the limit reads blocked once spend is over.
	w := postCreateRun(t, s, blockingBudgetSpec(50), false)
	if w.Code != http.StatusCreated {
		t.Fatalf("plain create status = %d", w.Code)
	}
	var created runResponse
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	rr.spent = 100
	got, _ := rr.GetRun(context.Background(), created.ID)
	v, err := s.dispatchBudgetFor(context.Background(), got, time.Now())
	if err != nil || v.Status != dispatchBudgetBlocked {
		t.Fatalf("verdict = %+v, %v; want blocked", v, err)
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
	var created runResponse
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	if rows, _ := au.ListForRunByCategory(context.Background(), created.ID, CategoryRunBudgetOverrideBound); len(rows) != 0 {
		t.Errorf("%s rows = %d, want 0 under limit", CategoryRunBudgetOverrideBound, len(rows))
	}
	if created.BudgetOverrideBinding != nil {
		t.Errorf("budget_override_binding = %+v, want absent", created.BudgetOverrideBinding)
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

// --- #2366: the Idempotency-Key replay lookup precedes this gate ---

// TestCreateRun_Replay_SkipsBlockingBudgetGate: the period is exhausted
// BETWEEN the create and its replay. The replay returns the run it already
// created rather than 402 budget_exhausted, and appends no run_rejected_budget
// — a replay is a redelivery of one run, not new spend, so the gate must not
// see it at all. Sibling of handleRecoverRun's "replay is not new spend".
func TestCreateRun_Replay_SkipsBlockingBudgetGate(t *testing.T) {
	au := newAuditFake()
	rr := newBudgetRunRepo()
	rr.spent = 10 // under the 50 limit
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, RunRepo: rr})

	w1 := postCreateRunWithKey(t, s, blockingBudgetSpec(50), false, "k-budget")
	if w1.Code != http.StatusCreated {
		t.Fatalf("first status = %d, want 201:\n%s", w1.Code, w1.Body.String())
	}
	var first runResponse
	if err := json.Unmarshal(w1.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}

	rr.spent = 100 // the budget trips between the two calls

	w2 := postCreateRunWithKey(t, s, blockingBudgetSpec(50), false, "k-budget")
	if w2.Code != http.StatusOK {
		t.Fatalf("replay status = %d, want 200 with the existing run, not 402:\n%s", w2.Code, w2.Body.String())
	}
	var second runResponse
	if err := json.Unmarshal(w2.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Errorf("replay returned run %s, want the existing %s", second.ID, first.ID)
	}
	if n := countGlobalAudits(au, "run_rejected_budget"); n != 0 {
		t.Errorf("run_rejected_budget audits = %d, want 0 — the gate ran on a replay", n)
	}
	if n := runRowCount(rr.fakeRepo); n != 1 {
		t.Errorf("run rows = %d, want 1 (the replay must not insert)", n)
	}
}

// TestCreateRun_Replay_DoesNotReappendBudgetOverride: the override grant is a
// governance record like the applies_to one, so a create + replay pair leaves
// exactly ONE run_admitted_budget_override on the global chain.
func TestCreateRun_Replay_DoesNotReappendBudgetOverride(t *testing.T) {
	au := newAuditFake()
	rr := newBudgetRunRepo()
	rr.spent = 100 // over the 50 limit — the override is what admits it
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, RunRepo: rr})

	w1 := postCreateRunWithKey(t, s, blockingBudgetSpec(50), true, "k-budget-override")
	if w1.Code != http.StatusCreated {
		t.Fatalf("first status = %d, want 201:\n%s", w1.Code, w1.Body.String())
	}
	w2 := postCreateRunWithKey(t, s, blockingBudgetSpec(50), true, "k-budget-override")
	if w2.Code != http.StatusOK {
		t.Fatalf("replay status = %d, want 200:\n%s", w2.Code, w2.Body.String())
	}
	if n := countGlobalAudits(au, "run_admitted_budget_override"); n != 1 {
		t.Errorf("run_admitted_budget_override audits = %d, want 1 across create+replay", n)
	}
	var first runResponse
	_ = json.Unmarshal(w1.Body.Bytes(), &first)
	if rows, _ := au.ListForRunByCategory(context.Background(), first.ID, CategoryRunBudgetOverrideBound); len(rows) != 1 {
		t.Errorf("%s rows across create+replay = %d, want 1", CategoryRunBudgetOverrideBound, len(rows))
	}
	if strings.Contains(w2.Body.String(), "budget_override_binding") {
		t.Errorf("replay carried budget_override_binding:\n%s", w2.Body.String())
	}
	if n := runRowCount(rr.fakeRepo); n != 1 {
		t.Errorf("run rows = %d, want 1", n)
	}
}

// Compile-time assertion that the embedded fake plus the cost-summer
// method still satisfies run.Repository — guards against signature drift.
var _ run.Repository = (*budgetRunRepo)(nil)
