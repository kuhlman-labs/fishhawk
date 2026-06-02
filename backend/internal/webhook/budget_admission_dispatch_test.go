package webhook

import (
	"context"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
)

// budgetStubRuns adds the SumWorkflowCostInRange capability
// (webhook.CostSummer) to the in-memory stubRuns so the dispatcher's
// blocking-budget admission gate has a cost source. spent is returned
// for every range query.
type budgetStubRuns struct {
	*stubRuns
	spent float64
}

func (s *budgetStubRuns) SumWorkflowCostInRange(_ context.Context, _, _ string, _, _ time.Time) (float64, error) {
	return s.spent, nil
}

// blockingBudgetSpec is validSpec's feature_change workflow at v0.4
// with one weekly blocking budget at the given limit.
func blockingBudgetDispatchSpec(limitUSD string) string {
	return `version: "0.4"
roles:
  tech_lead:
    members: ["@kuhlman-labs"]
workflows:
  feature_change:
    description: Test workflow
    budgets:
      - period: weekly
        limit_usd: ` + limitUSD + `
        enforcement: blocking
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        inputs:
          - source: github_issue
            required: true
        produces:
          - artifact: plan
            schema: standard_v1
        gates:
          - type: approval
            approvers:
              any_of: [tech_lead]
            sla: 4_business_hours
`
}

// newBudgetDispatcher builds a dispatcher whose run repo sums to spent,
// with a protected branch and the blocking-budget spec wired.
func newBudgetDispatcher(t *testing.T, specYAML string, spent float64) (*Dispatcher, *stubGitHub, *budgetStubRuns, *stubAudit) {
	t.Helper()
	gh := &stubGitHub{
		specContent: []byte(specYAML),
		specSHA:     "feedf00d",
		branchProtection: &githubclient.BranchProtection{
			RequiredStatusCheckContexts: []string{"ci/build"},
		},
	}
	runs := &budgetStubRuns{stubRuns: &stubRuns{}, spent: spent}
	au := &stubAudit{}
	d := &Dispatcher{
		GitHub: gh,
		Runs:   runs,
		Audit:  au,
		Now:    func() time.Time { return time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC) },
	}
	return d, gh, runs, au
}

func countDispatchGlobalAudits(au *stubAudit, category string) int {
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

// TestHandle_BlockingBudget_Exhausted_RefusesNewRun is the webhook
// admission seam: a NEW run whose workflow has crossed a blocking
// budget is refused before CreateRun — no run row, no dispatch, and a
// run_rejected_budget global audit entry.
func TestHandle_BlockingBudget_Exhausted_RefusesNewRun(t *testing.T) {
	d, gh, runs, au := newBudgetDispatcher(t, blockingBudgetDispatchSpec("50"), 100)

	if err := d.Handle(context.Background(), issueLabeledEvent(t)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(runs.created) != 0 {
		t.Errorf("runs created = %d, want 0 (refused before CreateRun)", len(runs.created))
	}
	if gh.dispatchCalls != 0 {
		t.Errorf("dispatch calls = %d, want 0 (no run to dispatch)", gh.dispatchCalls)
	}
	if n := countDispatchGlobalAudits(au, "run_rejected_budget"); n != 1 {
		t.Errorf("run_rejected_budget audits = %d, want 1", n)
	}
}

// TestHandle_BlockingBudget_UnderLimit_CreatesRun: spend under the
// limit dispatches as usual.
func TestHandle_BlockingBudget_UnderLimit_CreatesRun(t *testing.T) {
	d, gh, runs, au := newBudgetDispatcher(t, blockingBudgetDispatchSpec("50"), 10)

	if err := d.Handle(context.Background(), issueLabeledEvent(t)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(runs.created) != 1 {
		t.Fatalf("runs created = %d, want 1 (under limit admits)", len(runs.created))
	}
	if gh.dispatchCalls != 1 {
		t.Errorf("dispatch calls = %d, want 1", gh.dispatchCalls)
	}
	if n := countDispatchGlobalAudits(au, "run_rejected_budget"); n != 0 {
		t.Errorf("run_rejected_budget audits = %d, want 0 under limit", n)
	}
}

// ciBudgetSpec is the 2-stage CI-retry spec with a blocking budget so
// the continuation-not-gated regression can assert an Over budget does
// not block child creation.
const ciBudgetSpec = `version: "0.4"
roles:
  tech_lead:
    members: ["@kuhlman-labs"]
workflows:
  feature_change:
    description: Test workflow with retries
    budgets:
      - period: weekly
        limit_usd: 50
        enforcement: blocking
    on_ci_failure:
      max_retries: 1
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
        gates:
          - type: approval
            approvers:
              any_of: [tech_lead]
            sla: 4_business_hours
      - id: implement
        type: implement
        executor:
          agent: claude-code
`

// TestHandle_BlockingBudget_CIRetryChild_NotGated is the regression
// the re-plan requires: a CI-failure retry continues an already-
// admitted in-flight parent (ADR-030: in-flight runs finish), so the
// child create path must NOT be gated even when the parent workflow's
// blocking budget is well over its limit.
func TestHandle_BlockingBudget_CIRetryChild_NotGated(t *testing.T) {
	d, _, runs, au := newBudgetDispatcher(t, ciBudgetSpec, 999) // budget grossly over
	d.Artifacts = &stubArtifacts{}
	d.IssueNotifier = &stubIssueNotifier{}
	parent := seedParentRunForRetry(t, runs.stubRuns, "kuhlman-labs/fishhawk", ciBudgetSpec, 0)

	if err := d.Handle(context.Background(), checkRunFailedEvent(t)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(runs.created) != 2 {
		t.Fatalf("runs.created = %d, want 2 (parent + child; retry NOT gated by budget)", len(runs.created))
	}
	child := runs.created[1]
	if child.ParentRunID == nil || *child.ParentRunID != parent.ID {
		t.Errorf("child.ParentRunID = %v, want %s (continuation of in-flight parent)", child.ParentRunID, parent.ID)
	}
	if n := countDispatchGlobalAudits(au, "run_rejected_budget"); n != 0 {
		t.Errorf("run_rejected_budget audits = %d, want 0 (continuation never gated)", n)
	}
}

// TestHandle_BlockingBudget_CapabilityAbsent_Admits: a run repo
// without the cost-summer capability admits the run (capability-absent
// skip). The plain stubRuns has no SumWorkflowCostInRange, so the
// existing happy-path coverage already proves this — this test pins it
// explicitly against a blocking-budget spec.
func TestHandle_BlockingBudget_CapabilityAbsent_Admits(t *testing.T) {
	d, gh, runs, _ := newDispatcherWithStubs(t)
	gh.specContent = []byte(blockingBudgetDispatchSpec("1")) // limit so low any spend would be over

	if err := d.Handle(context.Background(), issueLabeledEvent(t)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(runs.created) != 1 {
		t.Fatalf("runs created = %d, want 1 (capability-absent admits)", len(runs.created))
	}
}
