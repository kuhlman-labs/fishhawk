package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/bundle"
	"github.com/kuhlman-labs/fishhawk/backend/internal/issuecomment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/pricing"
)

// budgetAlertSpec builds a minimal workflow-v0.4 spec whose
// feature_change workflow carries one weekly budget.
func budgetAlertSpec(limitUSD float64, enforcement string, warnAt float64) []byte {
	return []byte(fmt.Sprintf(`
version: "0.4"
workflows:
  feature_change:
    description: "x"
    budgets:
      - period: weekly
        limit_usd: %g
        enforcement: %s
        warn_at: %g
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`, limitUSD, enforcement, warnAt))
}

// countBudgetAlerts returns how many appended budget_alert audit entries
// carry the given tier.
func countBudgetAlerts(au *auditFake, tier string) int {
	au.mu.Lock()
	defer au.mu.Unlock()
	n := 0
	for i := range au.appended {
		if au.appended[i].Category != "budget_alert" {
			continue
		}
		var p struct {
			Tier string `json:"tier"`
		}
		if err := json.Unmarshal(au.appended[i].Payload, &p); err != nil {
			continue
		}
		if p.Tier == tier {
			n++
		}
	}
	return n
}

// budgetCommentBodies returns the bodies of budget-alert issue comments
// the recorder captured (every NotifyBudgetAlert comment names "cost
// budget").
func budgetCommentBodies(gh *slashGitHubRecorder) []string {
	var out []string
	for _, c := range gh.calls() {
		if strings.Contains(c.body, "cost budget") {
			out = append(out, c.body)
		}
	}
	return out
}

// TestRecordCost_AdvisoryBudget_WarnThenOver exercises the full
// advisory periodic-budget seam (#688): manifest parse -> AddRunCost ->
// SumWorkflowCostInRange -> budget.Evaluate -> budget_alert audit +
// NotifyBudgetAlert. It drives recordCost three times in the same
// calendar period and asserts the warn comment fires once, the 100%
// comment fires once, and a third pass re-emits neither. A prior run in
// an earlier period must not count toward this period's spend.
func TestRecordCost_AdvisoryBudget_WarnThenOver(t *testing.T) {
	au := newAuditFake()
	rr := newApprovalRunRepo()
	gh := newSlashGitHubRecorder()
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, RunRepo: rr})
	s.issueNotifier = issuecomment.New(issuecomment.Deps{
		GitHub: gh, Runs: rr, Audit: au,
		ExternalURL: "https://app.fishhawk.example.com",
	})

	const model = "claude-opus-4-8"
	const inTok, outTok = 1000, 2000
	x, ok := pricing.Cost(model, inTok, outTok)
	if !ok || x <= 0 {
		t.Fatalf("pricing.Cost(%q) ok=%v usd=%v — fixture model must be priced", model, ok, x)
	}
	// limit = 1.5x, warn_at = 0.5:
	//   pass 1 → sum x   → fraction 0.667 → warn, not over
	//   pass 2 → sum 2x  → fraction 1.33  → over
	spec := budgetAlertSpec(1.5*x, "advisory", 0.5)

	stage := rr.seedStage(run.StageStateRunning)
	runID := stage.RunID
	triggerRef := "issue:42"
	rr.seedRun(&run.Run{
		ID: runID, Repo: "x/y", WorkflowID: "feature_change",
		TriggerSource: run.TriggerGitHubIssue, TriggerRef: &triggerRef,
		InstallationID: ptrInt64(99),
		WorkflowSpec:   spec,
		CreatedAt:      time.Now().UTC(),
	})
	// A prior run from ~30 days ago: its spend must be excluded from
	// this calendar week's period sum (straddles the boundary).
	rr.seedRun(&run.Run{
		ID: uuid.New(), Repo: "x/y", WorkflowID: "feature_change",
		TriggerSource: run.TriggerGitHubIssue,
		CostUSDTotal:  1000,
		CreatedAt:     time.Now().UTC().AddDate(0, 0, -30),
	})

	bundleBytes := packManifestBundle(t, bundle.Manifest{
		BundleSchema: "trace-bundle-v0",
		RunID:        runID.String(),
		StageID:      stage.ID.String(),
		Agent:        "claude-code",
		Model:        model,
		InputTokens:  inTok,
		OutputTokens: outTok,
	})

	ctx := context.Background()

	// Pass 1 → warn tier.
	s.recordCost(ctx, runID, stage.ID, bundleBytes)
	if got := countBudgetAlerts(au, "warn"); got != 1 {
		t.Fatalf("after pass 1: warn budget_alert count = %d, want 1", got)
	}
	if got := countBudgetAlerts(au, "over"); got != 0 {
		t.Fatalf("after pass 1: over budget_alert count = %d, want 0", got)
	}
	if got := len(budgetCommentBodies(gh)); got != 1 {
		t.Fatalf("after pass 1: budget comments = %d, want 1", got)
	}

	// Pass 2 → over tier (warn must not re-emit).
	s.recordCost(ctx, runID, stage.ID, bundleBytes)
	if got := countBudgetAlerts(au, "warn"); got != 1 {
		t.Errorf("after pass 2: warn budget_alert count = %d, want 1 (no re-emit)", got)
	}
	if got := countBudgetAlerts(au, "over"); got != 1 {
		t.Fatalf("after pass 2: over budget_alert count = %d, want 1", got)
	}
	bodies := budgetCommentBodies(gh)
	if len(bodies) != 2 {
		t.Fatalf("after pass 2: budget comments = %d, want 2", len(bodies))
	}
	if !strings.Contains(bodies[0], "approaching") {
		t.Errorf("first comment should be the warn tier: %q", bodies[0])
	}
	if !strings.Contains(bodies[1], "has exhausted") {
		t.Errorf("second comment should be the over tier: %q", bodies[1])
	}

	// Pass 3 in the same period → no new audit entry, no new comment.
	s.recordCost(ctx, runID, stage.ID, bundleBytes)
	if got := countBudgetAlerts(au, "warn") + countBudgetAlerts(au, "over"); got != 2 {
		t.Errorf("after pass 3: total budget_alerts = %d, want 2 (deduped)", got)
	}
	if got := len(budgetCommentBodies(gh)); got != 2 {
		t.Errorf("after pass 3: budget comments = %d, want 2 (deduped)", got)
	}
}

// TestRecordCost_AdvisoryBudget_HealsAfterCommentlessFirstEmission is the
// #758 regression: a comment-less first emission (a run that structurally
// can't comment — nil installation_id) must NOT poison the audit-keyed
// dedup for the whole period. It seeds run A with InstallationID=nil
// (comment suppressed) and run B with InstallationID set, both
// feature_change in the same calendar period over the limit. Driving
// recordCost on A writes the budget_alert crossing record but posts no
// comment; the later capable run B must NOT re-emit the crossing record
// yet must still surface the advisory comment exactly once.
func TestRecordCost_AdvisoryBudget_HealsAfterCommentlessFirstEmission(t *testing.T) {
	au := newAuditFake()
	rr := newApprovalRunRepo()
	gh := newSlashGitHubRecorder()
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, RunRepo: rr})
	s.issueNotifier = issuecomment.New(issuecomment.Deps{
		GitHub: gh, Runs: rr, Audit: au,
		ExternalURL: "https://app.fishhawk.example.com",
	})

	const model = "claude-opus-4-8"
	const inTok, outTok = 1000, 2000
	x, ok := pricing.Cost(model, inTok, outTok)
	if !ok || x <= 0 {
		t.Fatalf("pricing.Cost(%q) ok=%v usd=%v — fixture model must be priced", model, ok, x)
	}
	// limit = x/10, warn_at = 0.5: a single bundle's cost (x) already
	// drives the period over 100%, so both runs cross the "over" tier.
	spec := budgetAlertSpec(x/10, "advisory", 0.5)
	triggerRef := "issue:42"

	// Run A: no installation_id — the visible comment is structurally
	// suppressed (contextForBudgetAlert returns ok=false).
	stageA := rr.seedStage(run.StageStateRunning)
	runA := stageA.RunID
	rr.seedRun(&run.Run{
		ID: runA, Repo: "x/y", WorkflowID: "feature_change",
		TriggerSource: run.TriggerGitHubIssue, TriggerRef: &triggerRef,
		InstallationID: nil,
		WorkflowSpec:   spec,
		CreatedAt:      time.Now().UTC(),
	})

	// Run B: installation-bearing — the comment can land.
	stageB := rr.seedStage(run.StageStateRunning)
	runB := stageB.RunID
	rr.seedRun(&run.Run{
		ID: runB, Repo: "x/y", WorkflowID: "feature_change",
		TriggerSource: run.TriggerGitHubIssue, TriggerRef: &triggerRef,
		InstallationID: ptrInt64(99),
		WorkflowSpec:   spec,
		CreatedAt:      time.Now().UTC(),
	})

	bundleA := packManifestBundle(t, bundle.Manifest{
		BundleSchema: "trace-bundle-v0", RunID: runA.String(), StageID: stageA.ID.String(),
		Agent: "claude-code", Model: model, InputTokens: inTok, OutputTokens: outTok,
	})
	bundleB := packManifestBundle(t, bundle.Manifest{
		BundleSchema: "trace-bundle-v0", RunID: runB.String(), StageID: stageB.ID.String(),
		Agent: "claude-code", Model: model, InputTokens: inTok, OutputTokens: outTok,
	})

	ctx := context.Background()

	// Run A first: crossing record written, comment suppressed.
	s.recordCost(ctx, runA, stageA.ID, bundleA)
	if got := countBudgetAlerts(au, "over"); got != 1 {
		t.Fatalf("after run A: over budget_alert count = %d, want 1 (crossing recorded)", got)
	}
	if got := len(budgetCommentBodies(gh)); got != 0 {
		t.Fatalf("after run A: budget comments = %d, want 0 (nil installation suppresses comment)", got)
	}

	// Run B next: no re-emit of the crossing record, but the comment
	// now surfaces — the comment-less first emission did not poison it.
	s.recordCost(ctx, runB, stageB.ID, bundleB)
	if got := countBudgetAlerts(au, "over"); got != 1 {
		t.Errorf("after run B: over budget_alert count = %d, want 1 (no re-emit)", got)
	}
	if got := len(budgetCommentBodies(gh)); got != 1 {
		t.Fatalf("after run B: budget comments = %d, want 1 (comment heals on the capable run)", got)
	}
}

// TestRecordCost_BlockingBudget_NoAdvisoryEmission confirms a blocking
// budget never surfaces through the warn path — blocking enforcement is
// an admission-time gate (scope item 4), out of scope here.
func TestRecordCost_BlockingBudget_NoAdvisoryEmission(t *testing.T) {
	au := newAuditFake()
	rr := newApprovalRunRepo()
	gh := newSlashGitHubRecorder()
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, RunRepo: rr})
	s.issueNotifier = issuecomment.New(issuecomment.Deps{
		GitHub: gh, Runs: rr, Audit: au,
		ExternalURL: "https://app.fishhawk.example.com",
	})

	const model = "claude-opus-4-8"
	const inTok, outTok = 1000, 2000
	x, ok := pricing.Cost(model, inTok, outTok)
	if !ok || x <= 0 {
		t.Fatalf("pricing.Cost(%q) ok=%v usd=%v", model, ok, x)
	}
	// limit well below x so the period is "over" — a blocking budget
	// would gate at admission, but this warn path must stay silent.
	spec := budgetAlertSpec(x/10, "blocking", 0.5)

	stage := rr.seedStage(run.StageStateRunning)
	runID := stage.RunID
	triggerRef := "issue:42"
	rr.seedRun(&run.Run{
		ID: runID, Repo: "x/y", WorkflowID: "feature_change",
		TriggerSource: run.TriggerGitHubIssue, TriggerRef: &triggerRef,
		InstallationID: ptrInt64(99),
		WorkflowSpec:   spec,
		CreatedAt:      time.Now().UTC(),
	})

	bundleBytes := packManifestBundle(t, bundle.Manifest{
		BundleSchema: "trace-bundle-v0",
		RunID:        runID.String(),
		StageID:      stage.ID.String(),
		Agent:        "claude-code",
		Model:        model,
		InputTokens:  inTok,
		OutputTokens: outTok,
	})

	s.recordCost(context.Background(), runID, stage.ID, bundleBytes)

	if got := countBudgetAlerts(au, "warn") + countBudgetAlerts(au, "over"); got != 0 {
		t.Errorf("blocking budget emitted %d budget_alert entries via the warn path, want 0", got)
	}
	if got := len(budgetCommentBodies(gh)); got != 0 {
		t.Errorf("blocking budget posted %d advisory comments, want 0", got)
	}
}
