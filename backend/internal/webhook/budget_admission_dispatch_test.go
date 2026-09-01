package webhook

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// budgetStubRuns adds the SumWorkflowCostInRange capability
// (webhook.CostSummer) to the in-memory stubRuns so the dispatcher's
// blocking-budget admission gate has a cost source. spent is returned
// for every range query.
type budgetStubRuns struct {
	*stubRuns
	spent float64

	// sumMu guards sumCalls, which counts every SumWorkflowCostInRange
	// invocation. It is the sharpest assertion available to the ADR-030
	// exemption tests below: a retry path that never consults the CostSummer
	// cannot have been gated by it, whatever the run rows look like
	// afterwards. Mutex-guarded because Handle may be driven concurrently and
	// the package runs under -race.
	sumMu    sync.Mutex
	sumCalls int
}

func (s *budgetStubRuns) SumWorkflowCostInRange(_ context.Context, _, _ string, _, _ time.Time) (float64, error) {
	s.sumMu.Lock()
	s.sumCalls++
	s.sumMu.Unlock()
	return s.spent, nil
}

func (s *budgetStubRuns) sumCallCount() int {
	s.sumMu.Lock()
	defer s.sumMu.Unlock()
	return s.sumCalls
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

// TestHandle_BlockingBudget_GitLabTrigger_RefusesNewRun pins that the GitLab
// creation path (E45.22 / #2043) runs the SAME blocking-budget admission gate
// the GitHub path does. The gate is not forge-aware, so a GitLab trigger over
// an exhausted budget must be refused before CreateRun with the same
// run_rejected_budget entry — not admitted through a forge-specific bypass.
func TestHandle_BlockingBudget_GitLabTrigger_RefusesNewRun(t *testing.T) {
	d, _, runs, au := newBudgetDispatcher(t, blockingBudgetDispatchSpec("50"), 100)
	d.GitLabFiles = &stubFileFetcher{
		content: []byte(blockingBudgetDispatchSpec("50")),
		sha:     "g1t1absha",
	}
	d.GitLabProjects = registeredGitLabProject()

	if err := d.Handle(context.Background(), gitlabIssueTriggerEvent()); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(runs.created) != 0 {
		t.Errorf("runs created = %d, want 0 (refused before CreateRun)", len(runs.created))
	}
	if n := countDispatchGlobalAudits(au, "run_rejected_budget"); n != 1 {
		t.Errorf("run_rejected_budget audits = %d, want 1", n)
	}
}

// TestHandle_BlockingBudget_GitLabTrigger_UnderLimit_CreatesRun is the paired
// control: the same GitLab trigger under the limit creates the gitlab_ci run,
// so the refusal above cannot be satisfied by a path that never creates runs.
func TestHandle_BlockingBudget_GitLabTrigger_UnderLimit_CreatesRun(t *testing.T) {
	d, _, runs, au := newBudgetDispatcher(t, blockingBudgetDispatchSpec("50"), 10)
	d.GitLabFiles = &stubFileFetcher{
		content: []byte(blockingBudgetDispatchSpec("50")),
		sha:     "g1t1absha",
	}
	d.GitLabProjects = registeredGitLabProject()

	if err := d.Handle(context.Background(), gitlabIssueTriggerEvent()); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(runs.created) != 1 {
		t.Fatalf("runs created = %d, want 1 (under limit admits)", len(runs.created))
	}
	if n := countDispatchGlobalAudits(au, "run_rejected_budget"); n != 0 {
		t.Errorf("run_rejected_budget audits = %d, want 0 under limit", n)
	}
}

// --- ADR-030 continuation exemption, pinned per forge (E45.27 / #2878) ----
//
// The two tests below pin a DOCUMENTED DECISION rather than a control: neither
// CI-failure retry handler runs blocking-budget admission, because a retry
// continues an already-admitted lineage (ADR-030: in-flight work finishes) and
// its spend is capped per lineage by the parent-derived retry_attempt plus
// runs_retry_child_once_idx. The reasoning lives in README.md under "Why the
// CI-retry paths are exempt"; CHANGING THIS BEHAVIOUR MEANS CHANGING THAT
// DOCUMENT IN THE SAME COMMIT — inserting a refusedByBlockingBudget call into
// either handler turns the matching test below red first.
//
// Each test proves its OWN over-budget precondition by driving the CREATE seam
// with the SAME spec and the SAME spend and observing the refusal, so neither
// can pass because the budget was never actually over. sumCallCount() == 0 is
// the sharpest assertion: the CostSummer is not consulted at all on a retry.

// TestCIFailureRetry_BlockingBudgetExhausted_StillRetries_ADR030Exemption is
// the GitHub half.
func TestCIFailureRetry_BlockingBudgetExhausted_StillRetries_ADR030Exemption(t *testing.T) {
	const spent = 999.0 // ciBudgetSpec's weekly blocking limit is 50

	// PRECONDITION, OBSERVED NOT BORROWED: the same spec at the same spend
	// refuses on the CREATE seam. Without this, a retry that "was not gated"
	// could simply be a budget that was never over.
	t.Run("precondition_same_spec_same_spend_refuses_on_create", func(t *testing.T) {
		d, _, runs, au := newBudgetDispatcher(t, ciBudgetSpec, spent)
		if err := d.Handle(context.Background(), issueLabeledEvent(t)); err != nil {
			t.Fatalf("Handle: %v", err)
		}
		if len(runs.created) != 0 {
			t.Fatalf("runs created = %d, want 0 — the fixture's budget is NOT over, so the retry assertions below would be vacuous", len(runs.created))
		}
		if n := countDispatchGlobalAudits(au, "run_rejected_budget"); n != 1 {
			t.Fatalf("run_rejected_budget audits = %d, want 1 on the create seam", n)
		}
		if runs.sumCallCount() == 0 {
			t.Fatal("CostSummer never consulted on the CREATE seam — the fixture does not reach the budget gate at all")
		}
	})

	d, gh, runs, au := newBudgetDispatcher(t, ciBudgetSpec, spent)
	d.Artifacts = &stubArtifacts{}
	d.IssueNotifier = &stubIssueNotifier{}
	parent := seedParentRunForRetry(t, runs.stubRuns, "kuhlman-labs/fishhawk", ciBudgetSpec, 0)

	if err := d.Handle(context.Background(), checkRunFailedEvent(t)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// (1) the retry child exists at parent.RetryAttempt+1.
	if len(runs.created) != 2 {
		t.Fatalf("runs.created = %d, want 2 (parent + retry child)", len(runs.created))
	}
	child := runs.created[1]
	if child.ParentRunID == nil || *child.ParentRunID != parent.ID {
		t.Fatalf("child.ParentRunID = %v, want %s", child.ParentRunID, parent.ID)
	}
	if child.RetryAttempt != parent.RetryAttempt+1 {
		t.Errorf("child.RetryAttempt = %d, want %d (parent-derived)", child.RetryAttempt, parent.RetryAttempt+1)
	}
	// (2) it actually dispatched.
	if gh.dispatchCalls != 1 {
		t.Errorf("dispatch calls = %d, want 1 (the retry stage really fired)", gh.dispatchCalls)
	}
	// (3) no budget refusal was recorded.
	if n := countDispatchGlobalAudits(au, "run_rejected_budget"); n != 0 {
		t.Errorf("run_rejected_budget audits = %d, want 0 (a continuation is never gated)", n)
	}
	// (4) the dispatch audit says dispatched.
	if got := ciRetryAuditOutcome(t, au); got != "dispatched" {
		t.Errorf("ci_failure_retry_dispatched outcome = %q, want %q", got, "dispatched")
	}
	// (5) the sharpest pin: the CostSummer was never consulted on this path.
	if n := runs.sumCallCount(); n != 0 {
		t.Errorf("SumWorkflowCostInRange calls = %d, want 0 — the CI-retry path must not reach the budget gate at all (ADR-030 / #2878)", n)
	}
}

// TestGitLabCIRetry_BlockingBudgetExhausted_StillRetries_ADR030Exemption is
// the GitLab half. Parity with the GitHub test above is the point: the
// exemption must not be forge-specific in either direction.
func TestGitLabCIRetry_BlockingBudgetExhausted_StillRetries_ADR030Exemption(t *testing.T) {
	const spent = 999.0 // ciBudgetSpec's weekly blocking limit is 50

	t.Run("precondition_same_spec_same_spend_refuses_on_create", func(t *testing.T) {
		d, _, runs, au := newBudgetDispatcher(t, ciBudgetSpec, spent)
		d.GitLabFiles = &stubFileFetcher{content: []byte(ciBudgetSpec), sha: "g1t1absha"}
		d.GitLabProjects = registeredGitLabProject()
		if err := d.Handle(context.Background(), gitlabIssueTriggerEvent()); err != nil {
			t.Fatalf("Handle: %v", err)
		}
		if len(runs.created) != 0 {
			t.Fatalf("runs created = %d, want 0 — the fixture's budget is NOT over, so the retry assertions below would be vacuous", len(runs.created))
		}
		if n := countDispatchGlobalAudits(au, "run_rejected_budget"); n != 1 {
			t.Fatalf("run_rejected_budget audits = %d, want 1 on the GitLab create seam", n)
		}
		if runs.sumCallCount() == 0 {
			t.Fatal("CostSummer never consulted on the GitLab CREATE seam — the fixture does not reach the budget gate at all")
		}
	})

	d, _, runs, au := newBudgetDispatcher(t, ciBudgetSpec, spent)
	arts := &stubArtifacts{}
	d.Artifacts = arts
	d.GitLabFiles = &stubFileFetcher{content: []byte(ciBudgetSpec), sha: "g1t1absha"}
	d.GitLabProjects = registeredGitLabProject()
	trigger := &stubPipelineTrigger{}
	d.GitLabTrigger = trigger

	parent := seedGitLabRun(t, runs.stubRuns, arts, "acme/widgets", "deadbeef", 0, 0)
	// seedGitLabRun hardcodes ciRetrySpec (no budgets); the budget must be in
	// the PARENT's cached spec because that is what resolveRetryPolicy reads.
	parent.WorkflowSpec = []byte(ciBudgetSpec)
	// OBSERVE that the overwrite reaches the handler, rather than asserting it
	// from the stub's pointer semantics. Nothing else in this test would catch
	// a non-propagating overwrite: the precondition sub-test drives a
	// DIFFERENT dispatcher through the CREATE seam (whose spec comes from
	// d.GitLabFiles, not the parent row), and if the handler saw ciRetrySpec
	// it would find no budgets, never consult the CostSummer, and mint and
	// dispatch the child — so (1)-(5) below would all pass GREEN for a fixture
	// that never exercised the budget question at all. The GitHub twin needs
	// no equivalent because seedParentRunForRetry takes the spec as a
	// parameter.
	assertParentSpecCarriesBudget(t, d, parent)

	ev := gitlabPipelineEvent("failed", gitLabRunBranch(parent), "deadbeef", 9001, 0)
	if err := d.Handle(context.Background(), ev); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// (1) the retry child exists at parent.RetryAttempt+1.
	children := retryChildren(runs.stubRuns, 1)
	if len(children) != 1 {
		t.Fatalf("retry children = %d, want 1 (retry NOT gated by an exhausted budget)", len(children))
	}
	child := children[0]
	if child.ParentRunID == nil || *child.ParentRunID != parent.ID {
		t.Fatalf("child.ParentRunID = %v, want %s", child.ParentRunID, parent.ID)
	}
	if child.RetryAttempt != parent.RetryAttempt+1 {
		t.Errorf("child.RetryAttempt = %d, want %d (parent-derived)", child.RetryAttempt, parent.RetryAttempt+1)
	}
	// (2) a pipeline was actually triggered.
	if n := trigger.callCount(); n != 1 {
		t.Errorf("pipeline trigger calls = %d, want 1 (the retry really fired)", n)
	}
	// (3) no budget refusal was recorded.
	if n := countDispatchGlobalAudits(au, "run_rejected_budget"); n != 0 {
		t.Errorf("run_rejected_budget audits = %d, want 0 (a continuation is never gated)", n)
	}
	// (4) the dispatch audit says dispatched.
	if got := ciRetryAuditOutcome(t, au); got != "dispatched" {
		t.Errorf("ci_failure_retry_dispatched outcome = %q, want %q", got, "dispatched")
	}
	// (5) the sharpest pin: the CostSummer was never consulted on this path.
	if n := runs.sumCallCount(); n != 0 {
		t.Errorf("SumWorkflowCostInRange calls = %d, want 0 — the GitLab CI-retry path must not reach the budget gate at all (ADR-030 / #2878)", n)
	}
}

// assertParentSpecCarriesBudget re-reads the parent through the SAME store
// call the GitLab retry path uses to find its candidates
// (gitLabRetryCandidates -> Runs.ListRuns) and fails unless the row the
// handler will hand to resolveRetryPolicy carries a workflow with at least one
// budget. This is what makes the GitLab exemption test self-proving: its
// fixture sets the budgeted spec by overwriting a field on the seeded run, and
// whether the handler sees that depends on store semantics the test would
// otherwise be assuming rather than observing.
func assertParentSpecCarriesBudget(t *testing.T, d *Dispatcher, parent *run.Run) {
	t.Helper()
	kind := run.RunnerKindGitLabCI
	rows, err := d.Runs.ListRuns(context.Background(), run.ListRunsFilter{
		Repo:       parent.Repo,
		RunnerKind: &kind,
		Limit:      gitLabRetryCandidateLimit,
	})
	if err != nil {
		t.Fatalf("ListRuns (the handler's candidate lookup): %v", err)
	}
	for _, r := range rows {
		if r.ID != parent.ID {
			continue
		}
		parsed, err := spec.ParseBytes(r.WorkflowSpec)
		if err != nil {
			t.Fatalf("parent spec as the handler sees it does not parse: %v", err)
		}
		wf, ok := parsed.Workflows[r.WorkflowID]
		if !ok {
			t.Fatalf("parent spec as the handler sees it declares no workflow %q", r.WorkflowID)
		}
		if len(wf.Budgets) == 0 {
			t.Fatalf("parent workflow %q as the handler sees it declares 0 budgets — the budgeted-spec overwrite did NOT reach the handler, so the retry assertions would be vacuous", r.WorkflowID)
		}
		return
	}
	t.Fatalf("parent run %s is not among the %d candidates the handler's lookup returns", parent.ID, len(rows))
}

// ciRetryAuditOutcome returns the `outcome` recorded on the single
// ci_failure_retry_dispatched chained audit row, failing if there is not
// exactly one. Both exemption tests read it so a retry that was minted but
// audited as dispatch_failed cannot pass as a healthy continuation.
func ciRetryAuditOutcome(t *testing.T, au *stubAudit) string {
	t.Helper()
	au.mu.Lock()
	defer au.mu.Unlock()
	found := 0
	outcome := ""
	for _, a := range au.appended {
		if a.Category != "ci_failure_retry_dispatched" {
			continue
		}
		found++
		var payload map[string]any
		if err := json.Unmarshal(a.Payload, &payload); err != nil {
			t.Fatalf("unmarshal ci_failure_retry_dispatched payload: %v", err)
		}
		outcome, _ = payload["outcome"].(string)
	}
	if found != 1 {
		t.Fatalf("ci_failure_retry_dispatched audit rows = %d, want 1", found)
	}
	return outcome
}
