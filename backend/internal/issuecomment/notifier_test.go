package issuecomment_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/issuecomment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// happyDeps wires a Notifier against an issue-triggered run with no
// prior comment audit entries. Returns the notifier and the fakes
// so tests can assert on emitted state.
func happyDeps(t *testing.T) (uuid.UUID, *fakeGitHub, *fakeAudit, *issuecomment.Notifier) {
	t.Helper()
	runID := uuid.New()
	triggerRef := "issue:42"
	repoRuns := &fakeRuns{
		runs: map[uuid.UUID]*run.Run{runID: {
			ID:             runID,
			Repo:           "x/y",
			WorkflowID:     "feature_change",
			TriggerSource:  run.TriggerGitHubIssue,
			TriggerRef:     &triggerRef,
			InstallationID: int64Ptr(99),
		}},
	}
	gh := &fakeGitHub{}
	au := &fakeAudit{}
	n := issuecomment.New(issuecomment.Deps{
		GitHub:      gh,
		Runs:        repoRuns,
		Audit:       au,
		ExternalURL: "https://app.fishhawk.example.com",
		Now:         func() time.Time { return time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC) },
	})
	if n == nil {
		t.Fatal("notifier nil")
	}
	return runID, gh, au, n
}

func TestNotifyPlanReady_GatedRun_LinksToApprovalSurface(t *testing.T) {
	runID, gh, _, n := happyDeps(t)
	planStage := &run.Stage{ID: uuid.New(), Type: run.StageTypePlan, RunID: runID, RequiresApproval: true}
	p := &plan.Plan{
		Summary: "Add a feature",
		Scope: plan.Scope{Files: []plan.ScopeFile{
			{Path: "x.go", Operation: plan.FileOpModify},
			{Path: "y.go", Operation: plan.FileOpCreate},
		}},
	}
	if err := n.NotifyPlanReady(context.Background(), runID, planStage, p); err != nil {
		t.Fatal(err)
	}
	if len(gh.calls) != 1 {
		t.Fatalf("expected 1 GitHub call; got %d", len(gh.calls))
	}
	body := gh.calls[0].body
	if !strings.Contains(body, "Plan ready") {
		t.Errorf("body should reference plan ready: %q", body)
	}
	if !strings.Contains(body, "Add a feature") {
		t.Errorf("body should include summary: %q", body)
	}
	if !strings.Contains(body, "`x.go`") || !strings.Contains(body, "`y.go`") {
		t.Errorf("body should include file paths: %q", body)
	}
	// The approval link must include the /runs/<run_id> prefix
	// before the /stages/<stage_id> segment — the SPA route is
	// /runs/:runId/stages/:stageId; pre-#273 this asserted only on
	// the trailing /stages/<id> shape, which pinned a broken URL
	// in place (every plan-ready comment 404'd).
	wantURL := "/runs/" + runID.String() + "/stages/" + planStage.ID.String()
	if !strings.Contains(body, wantURL) {
		t.Errorf("body should link to %q (run-scoped stage URL): %q", wantURL, body)
	}
	// The typed-reply / slash-reject discovery hint is plan-on-issue-
	// only (E17.5 / #373). The legacy summary path must not advertise
	// the approval reply tokens, or reviewers reading the summary
	// comment will try +1 there and get silence.
	for _, unwanted := range []string{"+1", "/fishhawk reject"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("legacy summary body should not contain %q: %q", unwanted, body)
		}
	}
}

func TestNotifyPlanReady_GatelessRun_LinksToRunPage(t *testing.T) {
	runID, gh, _, n := happyDeps(t)
	planStage := &run.Stage{ID: uuid.New(), Type: run.StageTypePlan, RunID: runID, RequiresApproval: false}
	p := &plan.Plan{Summary: "x", Scope: plan.Scope{Files: []plan.ScopeFile{{Path: "a.go", Operation: plan.FileOpModify}}}}
	if err := n.NotifyPlanReady(context.Background(), runID, planStage, p); err != nil {
		t.Fatal(err)
	}
	body := gh.calls[0].body
	if !strings.Contains(body, "/runs/"+runID.String()) {
		t.Errorf("body should link to run page for gateless run: %q", body)
	}
	if strings.Contains(body, "/stages/") {
		t.Errorf("body should not link to a stage page for gateless run: %q", body)
	}
}

func TestNotifyPlanReady_TruncatesLongSummaryAndFiles(t *testing.T) {
	runID, gh, _, n := happyDeps(t)
	planStage := &run.Stage{ID: uuid.New(), Type: run.StageTypePlan, RunID: runID, RequiresApproval: true}
	long := strings.Repeat("a", 400)
	files := make([]plan.ScopeFile, 25)
	for i := range files {
		files[i] = plan.ScopeFile{Path: "f" + strings.Repeat("x", i), Operation: plan.FileOpModify}
	}
	p := &plan.Plan{Summary: long, Scope: plan.Scope{Files: files}}
	if err := n.NotifyPlanReady(context.Background(), runID, planStage, p); err != nil {
		t.Fatal(err)
	}
	body := gh.calls[0].body
	if !strings.Contains(body, "...") {
		t.Errorf("expected summary ellipsis: %q", body)
	}
	if !strings.Contains(body, "and 15 more") {
		t.Errorf("expected '…and 15 more' file list footer: %q", body)
	}
}

// fullPlanSpecYAML returns a minimal but valid workflow spec where
// the plan stage's `produces.persistence` opts into the
// originating_issue / rendered_comment surface (E17.2 / #337). The
// `update_on_change` flag is toggled by the caller via a sprintf
// param so tests can flip behavior without forking the YAML.
func fullPlanSpecYAML(updateOnChange bool) []byte {
	return []byte(fmt.Sprintf(`version: "0.3"
roles:
  tech_lead:
    members: ["@org/leads"]
workflows:
  feature_change:
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
            persistence:
              - target: originating_issue
                mode: rendered_comment
                update_on_change: %t
        gates:
          - type: approval
            approvers:
              any_of: [tech_lead]
      - id: implement
        type: implement
        executor:
          agent: claude-code
`, updateOnChange))
}

// summaryOnlySpecYAML returns a workflow spec whose plan stage does
// NOT declare originating_issue persistence — the legacy summary
// path should fire.
func summaryOnlySpecYAML() []byte {
	return []byte(`version: "0.3"
workflows:
  feature_change:
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
              any_of: ["@org/leads"]
      - id: implement
        type: implement
        executor:
          agent: claude-code
`)
}

// fullPlanFixture returns a plan with every field populated so the
// rendered-comment body covers all sections.
func fullPlanFixture() *plan.Plan {
	return &plan.Plan{
		Summary: "Refactor the dispatcher to skip the watchdog timer in dry-run mode.",
		Scope: plan.Scope{
			Files: []plan.ScopeFile{
				{Path: "backend/internal/webhook/dispatcher.go", Operation: plan.FileOpModify},
				{Path: "backend/internal/webhook/dispatcher_test.go", Operation: plan.FileOpModify},
			},
		},
		Approach: []plan.ApproachStep{
			{Step: 1, Description: "Add a dryRun field to dispatchOptions."},
			{Step: 2, Description: "Skip the watchdog when dryRun is true."},
			{Step: 3, Description: "Add a unit test covering the new branch."},
		},
		Verification: plan.Verification{
			TestStrategy: "Run the dispatcher test suite plus the new dry-run case.",
			RollbackPlan: "Revert the PR; the dispatcher returns to its prior shape.",
		},
		RisksAndAssumptions: []string{
			"Operators set dryRun via a feature flag — no env var landed yet.",
		},
	}
}

func TestNotifyPlanReady_FullPlanSpec_PostsFullDocument(t *testing.T) {
	// Spec opts into originating_issue + rendered_comment; the
	// notifier renders the full plan doc and posts it. The legacy
	// "Plan ready" summary phrase should NOT appear.
	runID := uuid.New()
	gh := &fakeGitHub{}
	au := &fakeAudit{}
	runs := map[uuid.UUID]*run.Run{runID: {
		ID:             runID,
		Repo:           "x/y",
		WorkflowID:     "feature_change",
		TriggerSource:  run.TriggerGitHubIssue,
		TriggerRef:     ptrStr("issue:42"),
		InstallationID: int64Ptr(99),
		WorkflowSpec:   fullPlanSpecYAML(true),
	}}
	n := issuecomment.New(issuecomment.Deps{
		GitHub:      gh,
		Runs:        &fakeRuns{runs: runs},
		Audit:       au,
		ExternalURL: "https://app.fishhawk.example.com",
		Now:         func() time.Time { return time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC) },
	})

	planStage := &run.Stage{ID: uuid.New(), Type: run.StageTypePlan, RunID: runID, RequiresApproval: true}
	p := fullPlanFixture()

	if err := n.NotifyPlanReady(context.Background(), runID, planStage, p); err != nil {
		t.Fatalf("NotifyPlanReady: %v", err)
	}
	if len(gh.calls) != 1 {
		t.Fatalf("expected 1 create call; got %d", len(gh.calls))
	}
	body := gh.calls[0].body
	// The full-plan path doesn't use the "Plan ready" phrase — its
	// header is "Fishhawk plan for Run …".
	if strings.Contains(body, "Plan ready") {
		t.Errorf("body should not use the legacy summary phrase: %q", body)
	}
	for _, want := range []string{
		"Fishhawk plan",
		"feature_change",
		p.Summary,
		"Scope",
		"Approach",
		"Refactor the dispatcher",
		"Verification",
		"Test strategy",
		"Rollback plan",
		"Risks & assumptions",
		"Approve in the dashboard",
		"+1",
		"/fishhawk reject",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n---\n%s", want, body)
		}
	}

	// Audit row records the kind + comment id (the latter so the
	// next post can edit).
	if len(au.appended) != 1 {
		t.Fatalf("expected 1 audit row; got %d", len(au.appended))
	}
	var pl map[string]any
	_ = json.Unmarshal(au.appended[0].Payload, &pl)
	if pl["kind"] != string(issuecomment.KindPlanFull) {
		t.Errorf("audit kind = %v, want plan_full", pl["kind"])
	}
	if id, _ := pl["github_comment_id"].(float64); int64(id) != 1 {
		t.Errorf("audit github_comment_id = %v, want 1", pl["github_comment_id"])
	}
}

func TestNotifyPlanReady_FullPlanSpec_UpdateOnChange_EditsExistingComment(t *testing.T) {
	// Pre-seed a KindPlanFull audit row so the second post finds
	// the comment id and takes the edit-in-place path. update_on_
	// change=true is on in the spec.
	runID := uuid.New()
	gh := &fakeGitHub{}
	au := &fakeAudit{}
	au.preSeed(runID, issuecomment.CategoryIssueCommented, map[string]any{
		"kind":              string(issuecomment.KindPlanFull),
		"issue_number":      42,
		"repo":              "x/y",
		"github_comment_id": 7777,
	})
	runs := map[uuid.UUID]*run.Run{runID: {
		ID:             runID,
		Repo:           "x/y",
		WorkflowID:     "feature_change",
		TriggerSource:  run.TriggerGitHubIssue,
		TriggerRef:     ptrStr("issue:42"),
		InstallationID: int64Ptr(99),
		WorkflowSpec:   fullPlanSpecYAML(true),
	}}
	n := issuecomment.New(issuecomment.Deps{
		GitHub:      gh,
		Runs:        &fakeRuns{runs: runs},
		Audit:       au,
		ExternalURL: "https://app.fishhawk.example.com",
		Now:         func() time.Time { return time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC) },
	})

	planStage := &run.Stage{ID: uuid.New(), Type: run.StageTypePlan, RunID: runID, RequiresApproval: true}
	if err := n.NotifyPlanReady(context.Background(), runID, planStage, fullPlanFixture()); err != nil {
		t.Fatalf("NotifyPlanReady: %v", err)
	}
	if len(gh.calls) != 0 {
		t.Errorf("expected 0 create calls; got %d", len(gh.calls))
	}
	if len(gh.updateCalls) != 1 {
		t.Fatalf("expected 1 update call; got %d", len(gh.updateCalls))
	}
	if gh.updateCalls[0].commentID != 7777 {
		t.Errorf("edit commentID = %d, want 7777 (seeded)", gh.updateCalls[0].commentID)
	}
	// Fresh audit row for the update.
	if len(au.appended) != 1 {
		t.Fatalf("expected 1 new audit row; got %d", len(au.appended))
	}
	var pl map[string]any
	_ = json.Unmarshal(au.appended[0].Payload, &pl)
	if pl["kind"] != string(issuecomment.KindPlanUpdated) {
		t.Errorf("audit kind = %v, want plan_updated", pl["kind"])
	}
}

// TestNotifyPlanReady_FullPlanSpec_ApprovalFooter_Approve covers
// #377: re-fire after a plan-approve writes an `_Status: approved
// by @x · implementing now_` footer onto the edited plan comment,
// reading the approver from the audit chain's latest approval row.
func TestNotifyPlanReady_FullPlanSpec_ApprovalFooter_Approve(t *testing.T) {
	runID := uuid.New()
	planStageID := uuid.New()
	gh := &fakeGitHub{}
	au := &fakeAudit{}
	// Pre-seed the prior plan comment so notifyFullPlan takes the
	// edit-in-place path on this call.
	au.preSeed(runID, issuecomment.CategoryIssueCommented, map[string]any{
		"kind":              string(issuecomment.KindPlanFull),
		"issue_number":      42,
		"repo":              "x/y",
		"github_comment_id": 4242,
	})
	// Pre-seed the approval row on the plan stage. notifyFullPlan
	// reads this via latestPlanApproval and feeds it to the renderer.
	au.preSeedWithStage(runID, planStageID, "approval_submitted", map[string]any{
		"stage_id": planStageID.String(),
		"decision": "approve",
		"approver": "alice",
	})

	runs := map[uuid.UUID]*run.Run{runID: {
		ID: runID, Repo: "x/y", WorkflowID: "feature_change",
		TriggerSource:  run.TriggerGitHubIssue,
		TriggerRef:     ptrStr("issue:42"),
		InstallationID: int64Ptr(99),
		WorkflowSpec:   fullPlanSpecYAML(true),
	}}
	n := issuecomment.New(issuecomment.Deps{
		GitHub:      gh,
		Runs:        &fakeRuns{runs: runs},
		Audit:       au,
		ExternalURL: "https://app.fishhawk.example.com",
		Now:         func() time.Time { return time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC) },
	})
	planStage := &run.Stage{ID: planStageID, Type: run.StageTypePlan, RunID: runID, RequiresApproval: true}
	if err := n.NotifyPlanReady(context.Background(), runID, planStage, fullPlanFixture()); err != nil {
		t.Fatalf("NotifyPlanReady: %v", err)
	}
	if len(gh.calls) != 0 {
		t.Errorf("approval re-render should edit, not create: %d new comments", len(gh.calls))
	}
	if len(gh.updateCalls) != 1 {
		t.Fatalf("expected 1 edit; got %d", len(gh.updateCalls))
	}
	body := gh.updateCalls[0].body
	if !strings.Contains(body, "_Status: approved by @alice · implementing now_") {
		t.Errorf("edited body should carry the approval footer; got:\n%s", body)
	}
}

// TestNotifyPlanReady_FullPlanSpec_ApprovalFooter_Reject covers
// the reject side of #377: a plan-reject audit row produces the
// `_Status: rejected by @x_` footer on the next re-render. No
// reason rendered because v0 doesn't store one on the approval
// row.
func TestNotifyPlanReady_FullPlanSpec_ApprovalFooter_Reject(t *testing.T) {
	runID := uuid.New()
	planStageID := uuid.New()
	gh := &fakeGitHub{}
	au := &fakeAudit{}
	au.preSeed(runID, issuecomment.CategoryIssueCommented, map[string]any{
		"kind":              string(issuecomment.KindPlanFull),
		"issue_number":      42,
		"repo":              "x/y",
		"github_comment_id": 4243,
	})
	au.preSeedWithStage(runID, planStageID, "approval_submitted", map[string]any{
		"stage_id": planStageID.String(),
		"decision": "reject",
		"approver": "bob",
	})

	runs := map[uuid.UUID]*run.Run{runID: {
		ID: runID, Repo: "x/y", WorkflowID: "feature_change",
		TriggerSource:  run.TriggerGitHubIssue,
		TriggerRef:     ptrStr("issue:42"),
		InstallationID: int64Ptr(99),
		WorkflowSpec:   fullPlanSpecYAML(true),
	}}
	n := issuecomment.New(issuecomment.Deps{
		GitHub:      gh,
		Runs:        &fakeRuns{runs: runs},
		Audit:       au,
		ExternalURL: "https://app.fishhawk.example.com",
		Now:         func() time.Time { return time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC) },
	})
	planStage := &run.Stage{ID: planStageID, Type: run.StageTypePlan, RunID: runID, RequiresApproval: true}
	if err := n.NotifyPlanReady(context.Background(), runID, planStage, fullPlanFixture()); err != nil {
		t.Fatalf("NotifyPlanReady: %v", err)
	}
	if len(gh.updateCalls) != 1 {
		t.Fatalf("expected 1 edit; got %d", len(gh.updateCalls))
	}
	body := gh.updateCalls[0].body
	if !strings.Contains(body, "_Status: rejected by @bob_") {
		t.Errorf("edited body should carry the reject footer; got:\n%s", body)
	}
}

// TestPlanStatusFooterForAuditPayload_PrefersGithubLogin pins the
// #751 fix at the render seam: when the approval audit payload carries
// a resolved approver_github_login, the footer `@`-mentions THAT login
// even though the provenance `approver` is the raw MCP token subject
// (brett@local-mcp). Without the resolved login, the bare token
// subject falls back to "an approver" rather than `@`-mentioning an
// unrelated GitHub user.
func TestPlanStatusFooterForAuditPayload_PrefersGithubLogin(t *testing.T) {
	withLogin := mustJSON(t, map[string]any{
		"decision":              "approve",
		"approver":              "brett@local-mcp",
		"approver_github_login": "kuhlman-labs",
	})
	got := issuecomment.PlanStatusFooterForAuditPayload(withLogin)
	if got != "_Status: approved by @kuhlman-labs · implementing now_" {
		t.Errorf("with resolved login, footer = %q, want @kuhlman-labs mention", got)
	}

	bareSubject := mustJSON(t, map[string]any{
		"decision": "approve",
		"approver": "brett@local-mcp",
	})
	got = issuecomment.PlanStatusFooterForAuditPayload(bareSubject)
	if got != "_Status: approved by an approver · implementing now_" {
		t.Errorf("with bare token subject, footer = %q, want \"an approver\" (no ping)", got)
	}
}

// TestPlanStatusFooterForAuditPayload_LoginValidation exercises the
// tightened login validator (#751) through the exported render seam:
// a syntactically-valid GitHub login renders as an `@`-mention; any
// non-login string (containing '@'/'.', the "anonymous" placeholder,
// empty, leading/trailing hyphen, or over 39 chars) falls back to
// "an approver". The login is supplied as approver_github_login while
// `approver` is left empty so the fallback is unambiguously "an
// approver" on rejection.
func TestPlanStatusFooterForAuditPayload_LoginValidation(t *testing.T) {
	maxLogin := strings.Repeat("a", 39)
	overLogin := strings.Repeat("a", 40)
	cases := []struct {
		name  string
		login string
		valid bool
	}{
		{"plain login", "kuhlman-labs", true},
		{"single char", "a", true},
		{"39 chars max", maxLogin, true},
		{"internal hyphen", "a-b-c", true},
		{"mcp token subject", "brett@local-mcp", false},
		{"anonymous placeholder", "anonymous", false},
		{"empty", "", false},
		{"leading hyphen", "-foo", false},
		{"trailing hyphen", "foo-", false},
		{"40 chars over cap", overLogin, false},
		{"dotted", "foo.bar", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := mustJSON(t, map[string]any{
				"decision":              "approve",
				"approver":              "",
				"approver_github_login": tc.login,
			})
			got := issuecomment.PlanStatusFooterForAuditPayload(payload)
			if tc.valid {
				want := fmt.Sprintf("_Status: approved by @%s · implementing now_", tc.login)
				if got != want {
					t.Errorf("footer = %q, want %q", got, want)
				}
				return
			}
			if got != "_Status: approved by an approver · implementing now_" {
				t.Errorf("footer = %q, want fallback to \"an approver\"", got)
			}
		})
	}
}

// TestPlanStatusFooterForAuditPayload_NoDecisionAndMalformed confirms
// the exported render seam returns "" for a payload with no terminal
// decision and for a malformed payload (the awaiting-approval and
// corrupt-row cases the live notifier treats as no-status-yet).
func TestPlanStatusFooterForAuditPayload_NoDecisionAndMalformed(t *testing.T) {
	noDecision := mustJSON(t, map[string]any{"approver": "kuhlman-labs"})
	if got := issuecomment.PlanStatusFooterForAuditPayload(noDecision); got != "" {
		t.Errorf("no-decision footer = %q, want empty", got)
	}
	if got := issuecomment.PlanStatusFooterForAuditPayload([]byte("not json")); got != "" {
		t.Errorf("malformed footer = %q, want empty", got)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// TestNotifyPlanReady_FullPlanSpec_NoApprovalYet_NoFooter pins the
// awaiting-approval first-post case: no `approval_submitted` rows
// for the plan stage → no status footer in the body.
func TestNotifyPlanReady_FullPlanSpec_NoApprovalYet_NoFooter(t *testing.T) {
	runID := uuid.New()
	gh := &fakeGitHub{}
	au := &fakeAudit{}
	runs := map[uuid.UUID]*run.Run{runID: {
		ID: runID, Repo: "x/y", WorkflowID: "feature_change",
		TriggerSource:  run.TriggerGitHubIssue,
		TriggerRef:     ptrStr("issue:42"),
		InstallationID: int64Ptr(99),
		WorkflowSpec:   fullPlanSpecYAML(true),
	}}
	n := issuecomment.New(issuecomment.Deps{
		GitHub:      gh,
		Runs:        &fakeRuns{runs: runs},
		Audit:       au,
		ExternalURL: "https://app.fishhawk.example.com",
		Now:         func() time.Time { return time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC) },
	})
	planStage := &run.Stage{ID: uuid.New(), Type: run.StageTypePlan, RunID: runID, RequiresApproval: true}
	if err := n.NotifyPlanReady(context.Background(), runID, planStage, fullPlanFixture()); err != nil {
		t.Fatalf("NotifyPlanReady: %v", err)
	}
	if len(gh.calls) != 1 {
		t.Fatalf("expected 1 create call; got %d", len(gh.calls))
	}
	body := gh.calls[0].body
	if strings.Contains(body, "_Status:") {
		t.Errorf("body should NOT carry a status footer pre-approval; got:\n%s", body)
	}
}

func TestNotifyPlanReady_FullPlanSpec_NoUpdateOnChange_SkipsOnSecondCall(t *testing.T) {
	// update_on_change=false: the post is one-shot. A second call
	// after the first lands the audit row should silently skip.
	runID := uuid.New()
	gh := &fakeGitHub{}
	au := &fakeAudit{}
	au.preSeed(runID, issuecomment.CategoryIssueCommented, map[string]any{
		"kind":              string(issuecomment.KindPlanFull),
		"issue_number":      42,
		"repo":              "x/y",
		"github_comment_id": 5555,
	})
	runs := map[uuid.UUID]*run.Run{runID: {
		ID: runID, Repo: "x/y", WorkflowID: "feature_change",
		TriggerSource: run.TriggerGitHubIssue, TriggerRef: ptrStr("issue:42"),
		InstallationID: int64Ptr(99),
		WorkflowSpec:   fullPlanSpecYAML(false), // update_on_change=false
	}}
	n := issuecomment.New(issuecomment.Deps{
		GitHub: gh, Runs: &fakeRuns{runs: runs}, Audit: au,
		ExternalURL: "https://app.fishhawk.example.com",
	})

	planStage := &run.Stage{ID: uuid.New(), Type: run.StageTypePlan, RunID: runID, RequiresApproval: true}
	if err := n.NotifyPlanReady(context.Background(), runID, planStage, fullPlanFixture()); err != nil {
		t.Fatal(err)
	}
	if len(gh.calls)+len(gh.updateCalls) != 0 {
		t.Errorf("expected no GitHub calls when update_on_change=false; got %d creates + %d updates",
			len(gh.calls), len(gh.updateCalls))
	}
	if len(au.appended) != 0 {
		t.Errorf("no audit append expected on skip; got %d", len(au.appended))
	}
}

func TestNotifyPlanReady_SummaryOnlySpec_UsesLegacyPath(t *testing.T) {
	// Spec lacks originating_issue persistence; the legacy summary
	// path posts a KindPlan row with the "Plan ready" phrase.
	runID := uuid.New()
	gh := &fakeGitHub{}
	au := &fakeAudit{}
	runs := map[uuid.UUID]*run.Run{runID: {
		ID: runID, Repo: "x/y", WorkflowID: "feature_change",
		TriggerSource: run.TriggerGitHubIssue, TriggerRef: ptrStr("issue:42"),
		InstallationID: int64Ptr(99),
		WorkflowSpec:   summaryOnlySpecYAML(),
	}}
	n := issuecomment.New(issuecomment.Deps{
		GitHub: gh, Runs: &fakeRuns{runs: runs}, Audit: au,
		ExternalURL: "https://app.fishhawk.example.com",
	})

	planStage := &run.Stage{ID: uuid.New(), Type: run.StageTypePlan, RunID: runID, RequiresApproval: true}
	if err := n.NotifyPlanReady(context.Background(), runID, planStage, fullPlanFixture()); err != nil {
		t.Fatal(err)
	}
	if len(gh.calls) != 1 {
		t.Fatalf("expected 1 create call (legacy summary); got %d", len(gh.calls))
	}
	if !strings.Contains(gh.calls[0].body, "Plan ready") {
		t.Errorf("legacy path should use 'Plan ready' phrase: %q", gh.calls[0].body)
	}
	// Legacy audit row stays KindPlan, no github_comment_id.
	if len(au.appended) != 1 {
		t.Fatalf("expected 1 audit row; got %d", len(au.appended))
	}
	var pl map[string]any
	_ = json.Unmarshal(au.appended[0].Payload, &pl)
	if pl["kind"] != string(issuecomment.KindPlan) {
		t.Errorf("legacy audit kind = %v, want plan", pl["kind"])
	}
	if _, ok := pl["github_comment_id"]; ok {
		t.Errorf("legacy summary should not record github_comment_id: %+v", pl)
	}
}

func TestNotifyPlanReady_FullPlanSpec_OperatorDeletedComment_FallsBackToCreate(t *testing.T) {
	// Pre-seed a KindPlanFull row; GitHub returns ErrNotFound on
	// UpdateIssueComment (operator manually deleted the comment).
	// The notifier creates a fresh comment + appends a KindPlanFull
	// audit row carrying the new id.
	runID := uuid.New()
	gh := &fakeGitHub{updateErr: githubclient.ErrNotFound}
	au := &fakeAudit{}
	au.preSeed(runID, issuecomment.CategoryIssueCommented, map[string]any{
		"kind":              string(issuecomment.KindPlanFull),
		"issue_number":      42,
		"repo":              "x/y",
		"github_comment_id": 4242,
	})
	runs := map[uuid.UUID]*run.Run{runID: {
		ID: runID, Repo: "x/y", WorkflowID: "feature_change",
		TriggerSource: run.TriggerGitHubIssue, TriggerRef: ptrStr("issue:42"),
		InstallationID: int64Ptr(99),
		WorkflowSpec:   fullPlanSpecYAML(true),
	}}
	n := issuecomment.New(issuecomment.Deps{
		GitHub: gh, Runs: &fakeRuns{runs: runs}, Audit: au,
		ExternalURL: "https://app.fishhawk.example.com",
	})

	planStage := &run.Stage{ID: uuid.New(), Type: run.StageTypePlan, RunID: runID, RequiresApproval: true}
	if err := n.NotifyPlanReady(context.Background(), runID, planStage, fullPlanFixture()); err != nil {
		t.Fatal(err)
	}
	if len(gh.updateCalls) != 1 {
		t.Errorf("expected 1 update attempt (404'd); got %d", len(gh.updateCalls))
	}
	if len(gh.calls) != 1 {
		t.Fatalf("expected 1 fresh create after 404; got %d", len(gh.calls))
	}
}

func TestNotifyPlanReady_FullPlanSpec_TruncatesAtGitHubLimit(t *testing.T) {
	// Build a plan whose rendered body exceeds the 65,536-byte
	// cap; the renderer truncates with a "view full plan" link.
	runID := uuid.New()
	gh := &fakeGitHub{}
	au := &fakeAudit{}
	runs := map[uuid.UUID]*run.Run{runID: {
		ID: runID, Repo: "x/y", WorkflowID: "feature_change",
		TriggerSource: run.TriggerGitHubIssue, TriggerRef: ptrStr("issue:42"),
		InstallationID: int64Ptr(99),
		WorkflowSpec:   fullPlanSpecYAML(true),
	}}
	n := issuecomment.New(issuecomment.Deps{
		GitHub: gh, Runs: &fakeRuns{runs: runs}, Audit: au,
		ExternalURL: "https://app.fishhawk.example.com",
	})

	// One 100KB risk pushes the body well past the cap.
	huge := strings.Repeat("a", 100_000)
	p := &plan.Plan{
		Summary:             "x",
		RisksAndAssumptions: []string{huge},
	}
	planStage := &run.Stage{ID: uuid.New(), Type: run.StageTypePlan, RunID: runID, RequiresApproval: true}
	if err := n.NotifyPlanReady(context.Background(), runID, planStage, p); err != nil {
		t.Fatal(err)
	}
	if len(gh.calls) != 1 {
		t.Fatalf("expected 1 create call; got %d", len(gh.calls))
	}
	body := gh.calls[0].body
	if got := len(body); got > issuecomment.MaxIssueCommentBodyBytes {
		t.Errorf("body length %d exceeds cap %d", got, issuecomment.MaxIssueCommentBodyBytes)
	}
	if !strings.Contains(body, "truncated") {
		t.Errorf("body should carry truncation marker: %q", body[len(body)-200:])
	}
	if !strings.Contains(body, "view full plan") {
		t.Errorf("body should link to the SPA plan view: %q", body[len(body)-200:])
	}
}

func TestNotifyPlanReady_DedupsViaAuditLog(t *testing.T) {
	runID, gh, au, n := happyDeps(t)
	au.preSeed(runID, issuecomment.CategoryIssueCommented, map[string]any{"kind": "plan"})

	planStage := &run.Stage{ID: uuid.New(), Type: run.StageTypePlan, RunID: runID, RequiresApproval: true}
	p := &plan.Plan{Summary: "x"}
	if err := n.NotifyPlanReady(context.Background(), runID, planStage, p); err != nil {
		t.Fatal(err)
	}
	if len(gh.calls) != 0 {
		t.Errorf("expected 0 GitHub calls (deduped); got %d", len(gh.calls))
	}
}

func TestNotifyCIRetry_PostsCommentAndAuditEntry(t *testing.T) {
	runID, gh, au, n := happyDeps(t)
	parentID := uuid.New()
	if err := n.NotifyCIRetry(context.Background(), runID, parentID, "ci/build", 1, 1); err != nil {
		t.Fatalf("NotifyCIRetry: %v", err)
	}
	if len(gh.calls) != 1 {
		t.Fatalf("expected 1 GitHub call; got %d", len(gh.calls))
	}
	body := gh.calls[0].body
	if !strings.Contains(body, "ci/build") || !strings.Contains(body, "Retry attempt 1 of 1") {
		t.Errorf("body missing expected text: %q", body)
	}
	if !strings.Contains(body, parentID.String()[:8]) {
		t.Errorf("body should include parent short id: %q", body)
	}
	if len(au.appended) != 1 {
		t.Fatalf("expected 1 audit entry; got %d", len(au.appended))
	}
	var p map[string]any
	if err := json.Unmarshal(au.appended[0].Payload, &p); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if p["kind"] != "ci_retry" {
		t.Errorf("payload.kind = %v, want ci_retry", p["kind"])
	}
	if attempt, _ := p["retry_attempt"].(float64); int(attempt) != 1 {
		t.Errorf("payload.retry_attempt = %v, want 1", p["retry_attempt"])
	}
}

func TestNotifyCIRetry_PerAttemptDedup(t *testing.T) {
	runID, gh, au, n := happyDeps(t)
	// Pre-seed a ci_retry audit at retry_attempt=1: a second
	// NotifyCIRetry for the same attempt is the redelivery case
	// and should skip; an attempt=2 call (different run, but for
	// stub-test purposes) still posts because the dedup is
	// per-attempt.
	au.preSeed(runID, issuecomment.CategoryIssueCommented, map[string]any{
		"kind":          "ci_retry",
		"retry_attempt": 1,
	})

	if err := n.NotifyCIRetry(context.Background(), runID, uuid.New(), "ci/build", 1, 1); err != nil {
		t.Fatal(err)
	}
	if len(gh.calls) != 0 {
		t.Errorf("attempt=1 should dedup; got %d calls", len(gh.calls))
	}
	if err := n.NotifyCIRetry(context.Background(), runID, uuid.New(), "ci/build", 2, 2); err != nil {
		t.Fatal(err)
	}
	if len(gh.calls) != 1 {
		t.Errorf("attempt=2 should post; got %d calls", len(gh.calls))
	}
	if len(au.appended) != 1 {
		t.Errorf("expected 1 new audit entry; got %d", len(au.appended))
	}
}

func TestNotifyCIRetry_SkipsNonIssueTrigger(t *testing.T) {
	runID := uuid.New()
	cliRef := "cli:adhoc"
	repoRuns := &fakeRuns{
		runs: map[uuid.UUID]*run.Run{runID: {
			ID: runID, Repo: "x/y",
			TriggerSource:  run.TriggerCLI,
			TriggerRef:     &cliRef,
			InstallationID: int64Ptr(99),
		}},
	}
	gh := &fakeGitHub{}
	au := &fakeAudit{}
	n := issuecomment.New(issuecomment.Deps{
		GitHub: gh, Runs: repoRuns, Audit: au, ExternalURL: "https://app.fishhawk.example.com",
	})
	if err := n.NotifyCIRetry(context.Background(), runID, uuid.New(), "ci/build", 1, 1); err != nil {
		t.Fatal(err)
	}
	if len(gh.calls) != 0 || len(au.appended) != 0 {
		t.Errorf("expected no GitHub / audit activity; got %d / %d", len(gh.calls), len(au.appended))
	}
}

// --- NotifyStatusUpdate (E20.2 / #328) ---

func TestNotifyStatusUpdate_NilReceiver_NoOp(t *testing.T) {
	var n *issuecomment.Notifier
	if err := n.NotifyStatusUpdate(context.Background(), uuid.New(), "body"); err != nil {
		t.Errorf("nil receiver should return nil; got %v", err)
	}
}

func TestNotifyStatusUpdate_EmptyBody_NoOp(t *testing.T) {
	// Caller didn't render anything (no transition worth surfacing).
	// Skip without touching GitHub or the audit log.
	runID, gh, au, n := happyDeps(t)
	if err := n.NotifyStatusUpdate(context.Background(), runID, ""); err != nil {
		t.Fatal(err)
	}
	if len(gh.calls)+len(gh.updateCalls) != 0 {
		t.Errorf("expected no GitHub activity on empty body; got %d/%d", len(gh.calls), len(gh.updateCalls))
	}
	if len(au.appended) != 0 {
		t.Errorf("expected no audit activity on empty body; got %d", len(au.appended))
	}
}

func TestNotifyStatusUpdate_NonIssueTrigger_NoOp(t *testing.T) {
	// CLI / PR-triggered runs don't have an originating issue;
	// the status comment has nowhere to land.
	runID := uuid.New()
	cliRef := "cli:adhoc"
	repoRuns := &fakeRuns{
		runs: map[uuid.UUID]*run.Run{runID: {
			ID: runID, Repo: "x/y",
			TriggerSource:  run.TriggerCLI,
			TriggerRef:     &cliRef,
			InstallationID: int64Ptr(99),
		}},
	}
	gh := &fakeGitHub{}
	au := &fakeAudit{}
	n := issuecomment.New(issuecomment.Deps{
		GitHub: gh, Runs: repoRuns, Audit: au,
		ExternalURL: "https://app.fishhawk.example.com",
	})
	if err := n.NotifyStatusUpdate(context.Background(), runID, "body"); err != nil {
		t.Fatal(err)
	}
	if len(gh.calls)+len(gh.updateCalls) != 0 || len(au.appended) != 0 {
		t.Errorf("expected no activity for non-issue-trigger; got gh=%d/%d audit=%d",
			len(gh.calls), len(gh.updateCalls), len(au.appended))
	}
}

func TestNotifyStatusUpdate_FirstCall_CreatesCommentAndAuditRow(t *testing.T) {
	runID, gh, au, n := happyDeps(t)
	if err := n.NotifyStatusUpdate(context.Background(), runID, "status v1"); err != nil {
		t.Fatalf("NotifyStatusUpdate: %v", err)
	}
	if len(gh.calls) != 1 {
		t.Fatalf("expected 1 create call; got %d", len(gh.calls))
	}
	if gh.calls[0].body != "status v1" {
		t.Errorf("body = %q", gh.calls[0].body)
	}
	if len(gh.updateCalls) != 0 {
		t.Errorf("expected no update calls on first invocation; got %d", len(gh.updateCalls))
	}
	if len(au.appended) != 1 {
		t.Fatalf("expected 1 audit row; got %d", len(au.appended))
	}
	row := au.appended[0]
	if row.Category != issuecomment.CategoryStatusCommentPosted {
		t.Errorf("category = %q, want %q", row.Category, issuecomment.CategoryStatusCommentPosted)
	}
	var body map[string]any
	if err := json.Unmarshal(row.Payload, &body); err != nil {
		t.Fatalf("decode audit payload: %v", err)
	}
	if body["kind"] != string(issuecomment.KindStatusUpdate) {
		t.Errorf("payload.kind = %v, want status_update", body["kind"])
	}
	// fakeGitHub assigns id=1 to the first create.
	if id, _ := body["github_comment_id"].(float64); int64(id) != 1 {
		t.Errorf("payload.github_comment_id = %v, want 1", body["github_comment_id"])
	}
}

func TestNotifyStatusUpdate_SubsequentCall_EditsExistingComment(t *testing.T) {
	// Pre-seed an existing status comment audit row; the second
	// call should call UpdateIssueComment with the seeded id instead
	// of creating a new comment.
	runID, gh, au, n := happyDeps(t)
	au.preSeed(runID, issuecomment.CategoryStatusCommentPosted, map[string]any{
		"kind":              string(issuecomment.KindStatusUpdate),
		"issue_number":      42,
		"repo":              "x/y",
		"github_comment_id": 4242,
	})

	if err := n.NotifyStatusUpdate(context.Background(), runID, "status v2"); err != nil {
		t.Fatalf("NotifyStatusUpdate: %v", err)
	}
	if len(gh.calls) != 0 {
		t.Errorf("expected no create on edit path; got %d", len(gh.calls))
	}
	if len(gh.updateCalls) != 1 {
		t.Fatalf("expected 1 update call; got %d", len(gh.updateCalls))
	}
	upd := gh.updateCalls[0]
	if upd.commentID != 4242 {
		t.Errorf("update.commentID = %d, want 4242 (from preseed)", upd.commentID)
	}
	if upd.body != "status v2" {
		t.Errorf("update.body = %q", upd.body)
	}
	// Fresh audit row for the updated state.
	if len(au.appended) != 1 {
		t.Fatalf("expected 1 audit row; got %d", len(au.appended))
	}
}

func TestNotifyStatusUpdate_404OnUpdate_FallsBackToCreate(t *testing.T) {
	// Operator manually deleted the prior status comment. PATCH
	// returns 404 → ErrNotFound; notifier falls back to creating a
	// fresh comment and recording its id in a new audit row.
	runID, gh, au, n := happyDeps(t)
	au.preSeed(runID, issuecomment.CategoryStatusCommentPosted, map[string]any{
		"kind":              string(issuecomment.KindStatusUpdate),
		"github_comment_id": 9999,
	})
	gh.updateErr = githubclient.ErrNotFound

	if err := n.NotifyStatusUpdate(context.Background(), runID, "status after delete"); err != nil {
		t.Fatalf("NotifyStatusUpdate: %v", err)
	}
	if len(gh.updateCalls) != 1 {
		t.Errorf("expected 1 update attempt; got %d", len(gh.updateCalls))
	}
	if len(gh.calls) != 1 {
		t.Errorf("expected 1 create fallback; got %d", len(gh.calls))
	}
	if len(au.appended) != 1 {
		t.Fatalf("expected 1 audit row; got %d", len(au.appended))
	}
	var body map[string]any
	_ = json.Unmarshal(au.appended[0].Payload, &body)
	// New comment id from the fallback create (fakeGitHub's len-based
	// id assignment; first create returns id=1).
	if id, _ := body["github_comment_id"].(float64); int64(id) != 1 {
		t.Errorf("payload.github_comment_id = %v, want 1 (fresh id from fallback create)", body["github_comment_id"])
	}
}

func TestNotifyStatusUpdate_UpdateErrorOtherThan404_SurfacesError(t *testing.T) {
	// 403 / 500 / other errors should propagate, not fall through
	// to create. The caller decides whether to retry.
	runID, gh, au, n := happyDeps(t)
	au.preSeed(runID, issuecomment.CategoryStatusCommentPosted, map[string]any{
		"github_comment_id": 4242,
	})
	gh.updateErr = githubclient.ErrForbidden

	err := n.NotifyStatusUpdate(context.Background(), runID, "x")
	if err == nil {
		t.Fatalf("expected error on non-404 update failure")
	}
	if len(gh.calls) != 0 {
		t.Errorf("non-404 update failure should not fall back to create; got %d creates", len(gh.calls))
	}
	if len(au.appended) != 0 {
		t.Errorf("non-404 update failure should not append audit row; got %d", len(au.appended))
	}
}

// happyDepsWithStages returns the happyDeps fixtures plus a stage
// list so NotifyStatusUpdateForRun has something to render against.
func happyDepsWithStages(t *testing.T) (uuid.UUID, *fakeGitHub, *fakeAudit, *fakeRuns, *issuecomment.Notifier) {
	t.Helper()
	runID := uuid.New()
	triggerRef := "issue:42"
	stages := []*run.Stage{
		{ID: uuid.New(), RunID: runID, Sequence: 1, Type: run.StageTypePlan, State: run.StageStateSucceeded},
		{ID: uuid.New(), RunID: runID, Sequence: 2, Type: run.StageTypeImplement, State: run.StageStateRunning},
	}
	repoRuns := &fakeRuns{
		runs: map[uuid.UUID]*run.Run{runID: {
			ID:             runID,
			Repo:           "x/y",
			WorkflowID:     "feature_change",
			TriggerSource:  run.TriggerGitHubIssue,
			TriggerRef:     &triggerRef,
			InstallationID: int64Ptr(99),
			State:          run.StateRunning,
		}},
		stages: map[uuid.UUID][]*run.Stage{runID: stages},
	}
	gh := &fakeGitHub{}
	au := &fakeAudit{}
	n := issuecomment.New(issuecomment.Deps{
		GitHub:      gh,
		Runs:        repoRuns,
		Audit:       au,
		ExternalURL: "https://app.fishhawk.example.com",
		Now:         func() time.Time { return time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC) },
	})
	if n == nil {
		t.Fatal("notifier nil")
	}
	return runID, gh, au, repoRuns, n
}

func TestNotifyStatusUpdateForRun_FirstCall_CreatesAndRenders(t *testing.T) {
	runID, gh, au, _, n := happyDepsWithStages(t)
	if err := n.NotifyStatusUpdateForRun(context.Background(), runID); err != nil {
		t.Fatalf("NotifyStatusUpdateForRun: %v", err)
	}
	if len(gh.calls) != 1 {
		t.Fatalf("expected 1 create call; got %d", len(gh.calls))
	}
	body := gh.calls[0].body
	// Should carry the rendered header + stage list.
	for _, want := range []string{"Fishhawk run", "feature_change", "plan", "implement"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n---\n%s", want, body)
		}
	}
	if len(au.appended) != 1 {
		t.Fatalf("expected 1 audit row; got %d", len(au.appended))
	}
	if au.appended[0].Category != issuecomment.CategoryStatusCommentPosted {
		t.Errorf("audit category = %q", au.appended[0].Category)
	}
}

func TestNotifyStatusUpdateForRun_SubsequentCall_EditsSameComment(t *testing.T) {
	// Pre-seed an existing status comment audit row; the convenience
	// method should resolve to UpdateIssueComment with that id.
	runID, gh, au, _, n := happyDepsWithStages(t)
	au.preSeed(runID, issuecomment.CategoryStatusCommentPosted, map[string]any{
		"kind":              string(issuecomment.KindStatusUpdate),
		"issue_number":      42,
		"repo":              "x/y",
		"github_comment_id": 4242,
	})
	if err := n.NotifyStatusUpdateForRun(context.Background(), runID); err != nil {
		t.Fatalf("NotifyStatusUpdateForRun: %v", err)
	}
	if len(gh.calls) != 0 {
		t.Errorf("expected 0 create calls on edit path; got %d", len(gh.calls))
	}
	if len(gh.updateCalls) != 1 {
		t.Fatalf("expected 1 update call; got %d", len(gh.updateCalls))
	}
	if gh.updateCalls[0].commentID != 4242 {
		t.Errorf("update.commentID = %d, want 4242", gh.updateCalls[0].commentID)
	}
}

func TestNotifyStatusUpdateForRun_NonIssueTrigger_SkipsSilently(t *testing.T) {
	runID, gh, au, runs, n := happyDepsWithStages(t)
	runs.runs[runID].TriggerSource = run.TriggerCLI
	if err := n.NotifyStatusUpdateForRun(context.Background(), runID); err != nil {
		t.Fatalf("NotifyStatusUpdateForRun: %v", err)
	}
	if len(gh.calls)+len(gh.updateCalls) != 0 {
		t.Errorf("non-issue trigger should skip; got %d creates + %d updates",
			len(gh.calls), len(gh.updateCalls))
	}
	if len(au.appended) != 0 {
		t.Errorf("non-issue trigger should not append audit rows; got %d", len(au.appended))
	}
}

func TestNotifyStatusUpdateForRun_NilReceiver_NoOp(t *testing.T) {
	var n *issuecomment.Notifier
	if err := n.NotifyStatusUpdateForRun(context.Background(), uuid.New()); err != nil {
		t.Errorf("nil receiver should be a no-op; got %v", err)
	}
}

// TestStatusComment_Lifecycle drives the sticky-status comment through
// the operator-visible transitions of a representative run lifecycle
// (E20.5 / #331): dispatch seed → plan-ready → plan-approved →
// implementing → PR-open → merged. Each transition mutates the
// underlying run/stage state and fires NotifyStatusUpdateForRun; the
// test verifies that the notifier (1) creates exactly one comment,
// (2) edits the same comment id on every subsequent transition, and
// (3) renders the right state content at each step.
//
// This is the cross-cutting integration test promised by #331's
// acceptance criteria. The wiring of each handler is unit-tested per
// transition (dispatcher_test, trace_plannotify_test, issue_approval_test,
// pullrequest_review_events_test); this test sits one level up and
// verifies the audit-log-based edit-in-place loop survives a real
// sequence of transitions.
func TestStatusComment_Lifecycle(t *testing.T) {
	runID := uuid.New()
	triggerRef := "issue:42"
	r := &run.Run{
		ID:             runID,
		Repo:           "x/y",
		WorkflowID:     "feature_change",
		TriggerSource:  run.TriggerGitHubIssue,
		TriggerRef:     &triggerRef,
		InstallationID: int64Ptr(99),
		State:          run.StatePending,
	}
	planStage := &run.Stage{ID: uuid.New(), RunID: runID, Sequence: 1, Type: run.StageTypePlan, State: run.StageStatePending}
	implementStage := &run.Stage{ID: uuid.New(), RunID: runID, Sequence: 2, Type: run.StageTypeImplement, State: run.StageStatePending}
	reviewStage := &run.Stage{ID: uuid.New(), RunID: runID, Sequence: 3, Type: run.StageTypeReview, State: run.StageStatePending}
	stages := []*run.Stage{planStage, implementStage, reviewStage}

	repoRuns := &fakeRuns{
		runs:   map[uuid.UUID]*run.Run{runID: r},
		stages: map[uuid.UUID][]*run.Stage{runID: stages},
	}
	gh := &fakeGitHub{}
	au := &fakeAudit{}
	n := issuecomment.New(issuecomment.Deps{
		GitHub:      gh,
		Runs:        repoRuns,
		Audit:       au,
		ExternalURL: "https://app.fishhawk.example.com",
		Now:         func() time.Time { return time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC) },
	})

	ctx := context.Background()

	// Step 1: dispatcher seed. Run is pending, plan stage is
	// pending — the first transition fires after CreateRun. Expect
	// a create call landing comment id=1. This is now the
	// "Fishhawk picked it up" beat (#376 retired the standalone
	// pickup-broadcast).
	r.State = run.StateRunning
	planStage.State = run.StageStateDispatched
	if err := n.NotifyStatusUpdateForRun(ctx, runID); err != nil {
		t.Fatalf("step 1 dispatcher seed: %v", err)
	}
	if len(gh.calls) != 1 || len(gh.updateCalls) != 0 {
		t.Fatalf("step 1: expected 1 create + 0 updates; got %d + %d", len(gh.calls), len(gh.updateCalls))
	}

	// Step 2: plan terminal (trace handler). Plan stage moves to
	// awaiting_approval (the workflow requires approval). Expect an
	// edit on comment id=1.
	planStage.State = run.StageStateAwaitingApproval
	planStage.RequiresApproval = true
	if err := n.NotifyStatusUpdateForRun(ctx, runID); err != nil {
		t.Fatalf("step 2 plan terminal: %v", err)
	}
	if len(gh.calls) != 1 || len(gh.updateCalls) != 1 {
		t.Fatalf("step 2: expected 1 create + 1 update; got %d + %d", len(gh.calls), len(gh.updateCalls))
	}
	if gh.updateCalls[0].commentID != 1 {
		t.Errorf("step 2: edit commentID = %d, want 1", gh.updateCalls[0].commentID)
	}

	// Step 3: plan approved (approval handler). Plan succeeds, implement
	// dispatches.
	planStage.State = run.StageStateSucceeded
	implementStage.State = run.StageStateDispatched
	if err := n.NotifyStatusUpdateForRun(ctx, runID); err != nil {
		t.Fatalf("step 3 plan approved: %v", err)
	}
	if len(gh.calls) != 1 || len(gh.updateCalls) != 2 {
		t.Fatalf("step 3: expected 1 create + 2 updates; got %d + %d", len(gh.calls), len(gh.updateCalls))
	}

	// Step 4: implement terminal (trace handler). Implement succeeds.
	implementStage.State = run.StageStateSucceeded
	if err := n.NotifyStatusUpdateForRun(ctx, runID); err != nil {
		t.Fatalf("step 4 implement terminal: %v", err)
	}
	if len(gh.calls) != 1 || len(gh.updateCalls) != 3 {
		t.Fatalf("step 4: expected 1 create + 3 updates; got %d + %d", len(gh.calls), len(gh.updateCalls))
	}

	// Step 5: PR opened (pullrequest handler). PR URL is stamped on
	// the run; review stage moves to awaiting_approval.
	prURL := "https://github.com/x/y/pull/42"
	r.PullRequestURL = &prURL
	reviewStage.State = run.StageStateAwaitingApproval
	if err := n.NotifyStatusUpdateForRun(ctx, runID); err != nil {
		t.Fatalf("step 5 PR opened: %v", err)
	}
	if len(gh.calls) != 1 || len(gh.updateCalls) != 4 {
		t.Fatalf("step 5: expected 1 create + 4 updates; got %d + %d", len(gh.calls), len(gh.updateCalls))
	}
	if !strings.Contains(gh.updateCalls[3].body, prURL) {
		t.Errorf("step 5: comment body should contain PR URL: %q", gh.updateCalls[3].body)
	}

	// Step 6: PR merged (PR-events handler). Review stage succeeds,
	// run state moves to succeeded.
	reviewStage.State = run.StageStateSucceeded
	r.State = run.StateSucceeded
	if err := n.NotifyStatusUpdateForRun(ctx, runID); err != nil {
		t.Fatalf("step 6 PR merged: %v", err)
	}
	if len(gh.calls) != 1 || len(gh.updateCalls) != 5 {
		t.Fatalf("step 6: expected 1 create + 5 updates; got %d + %d", len(gh.calls), len(gh.updateCalls))
	}

	// All updates must target the same comment id — the test fails
	// loudly if the dedup ever races and creates a second comment.
	for i, upd := range gh.updateCalls {
		if upd.commentID != 1 {
			t.Errorf("update %d targeted comment id %d; expected stable id=1 across lifecycle",
				i, upd.commentID)
		}
	}

	// Final body should reflect the succeeded state and carry both
	// the run link and the PR link.
	finalBody := gh.updateCalls[len(gh.updateCalls)-1].body
	for _, want := range []string{"succeeded", "Pull request", prURL, "View run"} {
		if !strings.Contains(finalBody, want) {
			t.Errorf("final body missing %q\n---\n%s", want, finalBody)
		}
	}

	// Audit-log accounting: one status_comment_posted row per
	// transition (6 total). The chain is what production reads from
	// to find the comment id on subsequent calls, so the count is a
	// load-bearing invariant.
	statusRows := 0
	for _, p := range au.appended {
		if p.Category == issuecomment.CategoryStatusCommentPosted {
			statusRows++
		}
	}
	if statusRows != 6 {
		t.Errorf("expected 6 status_comment_posted audit rows (one per transition); got %d", statusRows)
	}
}

// TestStatusComment_ConcurrentUpdates_TargetSameComment locks in the
// dedup property under concurrent fire (E20.5 / #331 case 3): once a
// status comment exists, concurrent NotifyStatusUpdateForRun calls
// all resolve to UpdateIssueComment with the same id rather than
// racing into multiple CreateIssueComment calls. The production
// path's `ListForRunByCategory` query returns the existing row to
// every concurrent reader, so all callers find the seeded comment id
// and take the edit path.
//
// First-call concurrency (no prior comment) is NOT exercised by this
// test — that's a TOCTOU race the audit-log dedup cannot prevent on
// its own, and the dispatcher's seed step happens before any other
// transition so in practice the race window is bounded.
func TestStatusComment_ConcurrentUpdates_TargetSameComment(t *testing.T) {
	runID, gh, au, _, n := happyDepsWithStages(t)

	// Pre-seed an existing status comment so every concurrent caller
	// reads the same id from the dedup query.
	au.preSeed(runID, issuecomment.CategoryStatusCommentPosted, map[string]any{
		"kind":              string(issuecomment.KindStatusUpdate),
		"issue_number":      42,
		"repo":              "x/y",
		"github_comment_id": 4242,
	})

	const N = 16
	var wg sync.WaitGroup
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- n.NotifyStatusUpdateForRun(context.Background(), runID)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent NotifyStatusUpdateForRun: %v", err)
		}
	}

	if len(gh.calls) != 0 {
		t.Errorf("no concurrent caller should have taken the create path; got %d", len(gh.calls))
	}
	if len(gh.updateCalls) != N {
		t.Fatalf("expected %d update calls (one per caller); got %d", N, len(gh.updateCalls))
	}
	for i, upd := range gh.updateCalls {
		if upd.commentID != 4242 {
			t.Errorf("update %d targeted comment id %d; want 4242 (seeded)", i, upd.commentID)
		}
	}
}

func TestNew_NilDepsReturnsNilNotifier(t *testing.T) {
	cases := []struct {
		name string
		d    issuecomment.Deps
	}{
		{"no github", issuecomment.Deps{Runs: &fakeRuns{}, Audit: &fakeAudit{}, ExternalURL: "x"}},
		{"no runs", issuecomment.Deps{GitHub: &fakeGitHub{}, Audit: &fakeAudit{}, ExternalURL: "x"}},
		{"no audit", issuecomment.Deps{GitHub: &fakeGitHub{}, Runs: &fakeRuns{}, ExternalURL: "x"}},
		{"no external url", issuecomment.Deps{GitHub: &fakeGitHub{}, Runs: &fakeRuns{}, Audit: &fakeAudit{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if n := issuecomment.New(tc.d); n != nil {
				t.Errorf("expected nil; got %+v", n)
			}
		})
	}
}

func TestNotifySlashApprovalReply_PostsAndDoesNotDedup(t *testing.T) {
	_, _, _, n := happyDeps(t)
	gh := &fakeGitHub{}
	// Replace the inner GitHub fake by reflecting through New
	// again with a fresh notifier sharing the audit log so dedup
	// state would survive if we erroneously persisted it.
	n2 := issuecomment.New(issuecomment.Deps{
		GitHub:      gh,
		Runs:        &fakeRuns{},
		Audit:       &fakeAudit{},
		ExternalURL: "https://app.fishhawk.example.com",
	})
	_ = n // unused; happyDeps constructs one we don't need here

	if err := n2.NotifySlashApprovalReply(context.Background(), issuecomment.SlashApprovalReply{
		Repo: "x/y", InstallationID: 99, IssueNumber: 42, Body: "Approved by @alice.",
	}); err != nil {
		t.Fatal(err)
	}
	if len(gh.calls) != 1 {
		t.Fatalf("expected 1 call; got %d", len(gh.calls))
	}
	if gh.calls[0].body != "Approved by @alice." {
		t.Errorf("body = %q", gh.calls[0].body)
	}

	// Second call — replies are not deduped.
	if err := n2.NotifySlashApprovalReply(context.Background(), issuecomment.SlashApprovalReply{
		Repo: "x/y", InstallationID: 99, IssueNumber: 42, Body: "Cannot approve: ci_pass failing.",
	}); err != nil {
		t.Fatal(err)
	}
	if len(gh.calls) != 2 {
		t.Errorf("replies should not be deduped; got %d calls", len(gh.calls))
	}
}

func TestNotifySlashApprovalReply_SkipsBadParams(t *testing.T) {
	gh := &fakeGitHub{}
	n := issuecomment.New(issuecomment.Deps{
		GitHub:      gh,
		Runs:        &fakeRuns{},
		Audit:       &fakeAudit{},
		ExternalURL: "https://app.fishhawk.example.com",
	})
	cases := []struct {
		name string
		p    issuecomment.SlashApprovalReply
	}{
		{"zero issue", issuecomment.SlashApprovalReply{Repo: "x/y", InstallationID: 99, Body: "x"}},
		{"zero installation", issuecomment.SlashApprovalReply{Repo: "x/y", IssueNumber: 1, Body: "x"}},
		{"empty body", issuecomment.SlashApprovalReply{Repo: "x/y", InstallationID: 99, IssueNumber: 1}},
		{"malformed repo", issuecomment.SlashApprovalReply{Repo: "no-slash", InstallationID: 99, IssueNumber: 1, Body: "x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := n.NotifySlashApprovalReply(context.Background(), tc.p); err != nil {
				t.Errorf("expected nil; got %v", err)
			}
		})
	}
	if len(gh.calls) != 0 {
		t.Errorf("expected 0 calls; got %d", len(gh.calls))
	}
}

func TestNotify_NilReceiver_NoOp(t *testing.T) {
	var n *issuecomment.Notifier
	if err := n.NotifyPlanReady(context.Background(), uuid.New(),
		&run.Stage{ID: uuid.New(), Type: run.StageTypePlan}, &plan.Plan{}); err != nil {
		t.Errorf("nil plan should be a no-op; got %v", err)
	}
	if err := n.NotifySlashApprovalReply(context.Background(), issuecomment.SlashApprovalReply{
		Repo: "x/y", InstallationID: 1, IssueNumber: 1, Body: "x",
	}); err != nil {
		t.Errorf("nil reply should be a no-op; got %v", err)
	}
	if err := n.NotifyRunRejected(context.Background(), "x/y", 1, 1, "feature_change", "plan"); err != nil {
		t.Errorf("nil run-rejected should be a no-op; got %v", err)
	}
}

// TestNotifyRunRejected_PostsExplanation covers the #599 surface: the
// run-rejected comment names the offending workflow_id + stage and
// both fixes, posts no audit row (runless; canonical record is the
// dispatcher's global-chain entry), and is not deduped.
func TestNotifyRunRejected_PostsExplanation(t *testing.T) {
	gh := &fakeGitHub{}
	au := &fakeAudit{}
	n := issuecomment.New(issuecomment.Deps{
		GitHub:      gh,
		Runs:        &fakeRuns{},
		Audit:       au,
		ExternalURL: "https://app.fishhawk.example.com",
	})

	if err := n.NotifyRunRejected(context.Background(), "kuhlman-labs/fishhawk", 42, 1247,
		"feature_change", "plan"); err != nil {
		t.Fatal(err)
	}
	if len(gh.calls) != 1 {
		t.Fatalf("expected 1 comment; got %d", len(gh.calls))
	}
	call := gh.calls[0]
	if call.installationID != 42 {
		t.Errorf("installationID = %d, want 42", call.installationID)
	}
	if call.issueNumber != 1247 {
		t.Errorf("issueNumber = %d, want 1247", call.issueNumber)
	}
	for _, want := range []string{"feature_change", "plan", "FISHHAWKD_ANTHROPIC_API_KEY", "reviewers"} {
		if !strings.Contains(call.body, want) {
			t.Errorf("body missing %q:\n%s", want, call.body)
		}
	}
	// Runless surface: no notifier-level audit row (mirrors
	// NotifySlashApprovalReply).
	if len(au.appended) != 0 {
		t.Errorf("expected no audit rows; got %d", len(au.appended))
	}

	// Not deduped: a second refusal posts again.
	if err := n.NotifyRunRejected(context.Background(), "kuhlman-labs/fishhawk", 42, 1247,
		"feature_change", "plan"); err != nil {
		t.Fatal(err)
	}
	if len(gh.calls) != 2 {
		t.Errorf("run-rejected comments should not be deduped; got %d calls", len(gh.calls))
	}
}

// TestNotifyRunRejected_SkipsBadParams exercises the defensive skips:
// zero issue, zero installation, malformed repo all no-op without
// touching GitHub.
func TestNotifyRunRejected_SkipsBadParams(t *testing.T) {
	gh := &fakeGitHub{}
	n := issuecomment.New(issuecomment.Deps{
		GitHub:      gh,
		Runs:        &fakeRuns{},
		Audit:       &fakeAudit{},
		ExternalURL: "https://app.fishhawk.example.com",
	})
	cases := []struct {
		name           string
		repo           string
		installationID int64
		issueNumber    int
	}{
		{"zero issue", "x/y", 99, 0},
		{"zero installation", "x/y", 0, 1},
		{"malformed repo", "no-slash", 99, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := n.NotifyRunRejected(context.Background(), tc.repo, tc.installationID,
				tc.issueNumber, "feature_change", "plan"); err != nil {
				t.Errorf("expected nil; got %v", err)
			}
		})
	}
	if len(gh.calls) != 0 {
		t.Errorf("expected 0 calls; got %d", len(gh.calls))
	}
}

// --- helpers ---

func int64Ptr(v int64) *int64 { return &v }
func ptrStr(s string) *string { return &s }

// --- fakes ---

type ghCommentCall struct {
	installationID int64
	repo           githubclient.RepoRef
	issueNumber    int
	body           string
}

type ghUpdateCommentCall struct {
	installationID int64
	repo           githubclient.RepoRef
	commentID      int64
	body           string
}

type fakeGitHub struct {
	mu          sync.Mutex
	calls       []ghCommentCall
	updateCalls []ghUpdateCommentCall
	err         error
	updateErr   error
}

func (f *fakeGitHub) CreateIssueComment(_ context.Context, installationID int64, repo githubclient.RepoRef, issueNumber int, body string) (*githubclient.IssueComment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, ghCommentCall{installationID: installationID, repo: repo, issueNumber: issueNumber, body: body})
	if f.err != nil {
		return nil, f.err
	}
	// Synthesize an id deterministically from the call index so
	// status-comment tests can predict the comment id without
	// extra plumbing. The +1 keeps ids positive (1, 2, 3, …).
	id := int64(len(f.calls))
	return &githubclient.IssueComment{
		ID:      id,
		Body:    body,
		HTMLURL: fmt.Sprintf("https://github.com/%s/issues/%d#issuecomment-%d", repo.String(), issueNumber, id),
	}, nil
}

// UpdateIssueComment records the edit call alongside the existing
// create-call log. Status-comment tests assert on both surfaces.
func (f *fakeGitHub) UpdateIssueComment(_ context.Context, installationID int64, repo githubclient.RepoRef, commentID int64, body string) (*githubclient.IssueComment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updateCalls = append(f.updateCalls, ghUpdateCommentCall{installationID: installationID, repo: repo, commentID: commentID, body: body})
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	return &githubclient.IssueComment{
		ID:      commentID,
		Body:    body,
		HTMLURL: fmt.Sprintf("https://github.com/%s#issuecomment-%d", repo.String(), commentID),
	}, nil
}

type fakeRuns struct {
	run.Repository
	runs   map[uuid.UUID]*run.Run
	stages map[uuid.UUID][]*run.Stage
}

func (f *fakeRuns) GetRun(_ context.Context, id uuid.UUID) (*run.Run, error) {
	r, ok := f.runs[id]
	if !ok {
		return nil, run.ErrNotFound
	}
	return r, nil
}

func (f *fakeRuns) ListStagesForRun(_ context.Context, id uuid.UUID) ([]*run.Stage, error) {
	if f.stages == nil {
		return nil, nil
	}
	return f.stages[id], nil
}

type fakeAudit struct {
	audit.Repository
	mu       sync.Mutex
	appended []audit.ChainAppendParams
	preSeeds []*audit.Entry
}

func (f *fakeAudit) preSeed(runID uuid.UUID, category string, payload map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := json.Marshal(payload)
	r := runID
	f.preSeeds = append(f.preSeeds, &audit.Entry{
		ID: uuid.New(), RunID: &r, Category: category, Payload: body,
	})
}

// preSeedWithStage is the stage-scoped variant — needed for
// approval rows since the plan-status renderer reads stage_id.
func (f *fakeAudit) preSeedWithStage(runID, stageID uuid.UUID, category string, payload map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := json.Marshal(payload)
	r := runID
	s := stageID
	f.preSeeds = append(f.preSeeds, &audit.Entry{
		ID: uuid.New(), RunID: &r, StageID: &s, Category: category, Payload: body,
	})
}

func (f *fakeAudit) AppendChained(_ context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.appended = append(f.appended, p)
	r := p.RunID
	return &audit.Entry{ID: uuid.New(), RunID: &r}, nil
}

// appendedToEntries projects the recorded ChainAppendParams back to
// *audit.Entry rows so the lifecycle / lookup queries can see what the
// production code wrote. Sequence is the 1-indexed position; the
// caller's slice order is the canonical chronological order.
func (f *fakeAudit) appendedToEntries() []*audit.Entry {
	out := make([]*audit.Entry, 0, len(f.appended))
	for i, p := range f.appended {
		r := p.RunID
		out = append(out, &audit.Entry{
			ID:        uuid.New(),
			Sequence:  int64(i + 1),
			RunID:     &r,
			StageID:   p.StageID,
			Timestamp: p.Timestamp,
			Category:  p.Category,
			Payload:   p.Payload,
		})
	}
	return out
}

func (f *fakeAudit) ListForRunByCategory(_ context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []*audit.Entry{}
	for _, e := range f.preSeeds {
		if e.RunID != nil && *e.RunID == runID && e.Category == category {
			out = append(out, e)
		}
	}
	for _, e := range f.appendedToEntries() {
		if e.RunID != nil && *e.RunID == runID && e.Category == category {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeAudit) ListForRun(_ context.Context, runID uuid.UUID) ([]*audit.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []*audit.Entry{}
	for _, e := range f.preSeeds {
		if e.RunID != nil && *e.RunID == runID {
			out = append(out, e)
		}
	}
	for _, e := range f.appendedToEntries() {
		if e.RunID != nil && *e.RunID == runID {
			out = append(out, e)
		}
	}
	return out, nil
}
