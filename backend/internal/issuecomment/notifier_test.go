package issuecomment_test

import (
	"context"
	"encoding/json"
	"errors"
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

func TestNotifyPickup_PostsCommentAndAuditEntry(t *testing.T) {
	runID, gh, au, n := happyDeps(t)
	if err := n.NotifyPickup(context.Background(), runID, "alice"); err != nil {
		t.Fatalf("NotifyPickup: %v", err)
	}
	if len(gh.calls) != 1 {
		t.Fatalf("expected 1 GitHub call; got %d", len(gh.calls))
	}
	c := gh.calls[0]
	if c.repo.Owner != "x" || c.repo.Name != "y" {
		t.Errorf("repo = %+v", c.repo)
	}
	if c.issueNumber != 42 {
		t.Errorf("issueNumber = %d", c.issueNumber)
	}
	if !strings.Contains(c.body, "Fishhawk picked this up") {
		t.Errorf("body should reference pickup: %q", c.body)
	}
	if !strings.Contains(c.body, "feature_change") {
		t.Errorf("body should reference workflow_id: %q", c.body)
	}
	if !strings.Contains(c.body, "@alice") {
		t.Errorf("body should reference triggering user: %q", c.body)
	}
	if !strings.Contains(c.body, runID.String()[:8]) {
		t.Errorf("body should include short run id: %q", c.body)
	}

	if len(au.appended) != 1 {
		t.Fatalf("expected 1 audit append; got %d", len(au.appended))
	}
	if au.appended[0].Category != issuecomment.CategoryIssueCommented {
		t.Errorf("audit category = %q", au.appended[0].Category)
	}
	var p map[string]any
	if err := json.Unmarshal(au.appended[0].Payload, &p); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if p["kind"] != "pickup" {
		t.Errorf("payload.kind = %v", p["kind"])
	}
}

func TestNotifyPickup_DedupsViaAuditLog(t *testing.T) {
	runID, gh, au, n := happyDeps(t)

	// Pre-seed an existing pickup audit entry — the notifier must
	// short-circuit without posting a second time.
	au.preSeed(runID, issuecomment.CategoryIssueCommented, map[string]any{"kind": "pickup"})

	if err := n.NotifyPickup(context.Background(), runID, "alice"); err != nil {
		t.Fatalf("NotifyPickup: %v", err)
	}
	if len(gh.calls) != 0 {
		t.Errorf("expected 0 GitHub calls (deduped); got %d", len(gh.calls))
	}
}

func TestNotifyPickup_SkipsNonIssueTrigger(t *testing.T) {
	runID := uuid.New()
	repoRuns := &fakeRuns{
		runs: map[uuid.UUID]*run.Run{runID: {
			ID: runID, Repo: "x/y",
			TriggerSource:  run.TriggerCLI, // not github_issue
			InstallationID: int64Ptr(99),
		}},
	}
	gh := &fakeGitHub{}
	au := &fakeAudit{}
	n := issuecomment.New(issuecomment.Deps{
		GitHub: gh, Runs: repoRuns, Audit: au,
		ExternalURL: "https://app.fishhawk.example.com",
	})

	if err := n.NotifyPickup(context.Background(), runID, "alice"); err != nil {
		t.Fatal(err)
	}
	if len(gh.calls) != 0 {
		t.Errorf("expected no calls for CLI-triggered run")
	}
}

func TestNotifyPickup_SkipsMalformedTriggerRef(t *testing.T) {
	runID := uuid.New()
	bad := "not-an-issue-ref"
	repoRuns := &fakeRuns{
		runs: map[uuid.UUID]*run.Run{runID: {
			ID: runID, Repo: "x/y",
			TriggerSource:  run.TriggerGitHubIssue,
			TriggerRef:     &bad,
			InstallationID: int64Ptr(99),
		}},
	}
	gh := &fakeGitHub{}
	au := &fakeAudit{}
	n := issuecomment.New(issuecomment.Deps{
		GitHub: gh, Runs: repoRuns, Audit: au,
		ExternalURL: "https://app.fishhawk.example.com",
	})
	if err := n.NotifyPickup(context.Background(), runID, "alice"); err != nil {
		t.Fatal(err)
	}
	if len(gh.calls) != 0 {
		t.Errorf("expected no calls when trigger_ref is malformed")
	}
}

func TestNotifyPickup_GitHubErrorReturned_NoAuditEntry(t *testing.T) {
	runID, gh, au, n := happyDeps(t)
	gh.err = errors.New("403 forbidden")

	err := n.NotifyPickup(context.Background(), runID, "alice")
	if err == nil {
		t.Fatal("expected error")
	}
	if len(au.appended) != 0 {
		t.Errorf("audit append should not happen on comment failure; got %d", len(au.appended))
	}
}

func TestNotifyPickup_NoSenderRendersWithoutTriggeredBy(t *testing.T) {
	runID, gh, _, n := happyDeps(t)
	if err := n.NotifyPickup(context.Background(), runID, ""); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(gh.calls[0].body, "Triggered by") {
		t.Errorf("body should not reference triggering user when sender is empty: %q", gh.calls[0].body)
	}
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
	if !strings.Contains(body, "/stages/"+planStage.ID.String()) {
		t.Errorf("body should link to approval surface for gated run: %q", body)
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

func TestNotifyPickup_AndPlan_ShareCategoryButDistinctKinds(t *testing.T) {
	runID, gh, au, n := happyDeps(t)
	if err := n.NotifyPickup(context.Background(), runID, "alice"); err != nil {
		t.Fatal(err)
	}
	planStage := &run.Stage{ID: uuid.New(), Type: run.StageTypePlan, RunID: runID, RequiresApproval: true}
	p := &plan.Plan{Summary: "x"}
	if err := n.NotifyPlanReady(context.Background(), runID, planStage, p); err != nil {
		t.Fatal(err)
	}
	if len(gh.calls) != 2 {
		t.Errorf("expected 2 GitHub calls; got %d", len(gh.calls))
	}
	if len(au.appended) != 2 {
		t.Errorf("expected 2 audit entries; got %d", len(au.appended))
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

func TestNotify_NilReceiver_NoOp(t *testing.T) {
	var n *issuecomment.Notifier
	if err := n.NotifyPickup(context.Background(), uuid.New(), "x"); err != nil {
		t.Errorf("nil pickup should be a no-op; got %v", err)
	}
	if err := n.NotifyPlanReady(context.Background(), uuid.New(),
		&run.Stage{ID: uuid.New(), Type: run.StageTypePlan}, &plan.Plan{}); err != nil {
		t.Errorf("nil plan should be a no-op; got %v", err)
	}
}

// --- helpers ---

func int64Ptr(v int64) *int64 { return &v }

// --- fakes ---

type ghCommentCall struct {
	installationID int64
	repo           githubclient.RepoRef
	issueNumber    int
	body           string
}

type fakeGitHub struct {
	mu    sync.Mutex
	calls []ghCommentCall
	err   error
}

func (f *fakeGitHub) CreateIssueComment(_ context.Context, installationID int64, repo githubclient.RepoRef, issueNumber int, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, ghCommentCall{installationID: installationID, repo: repo, issueNumber: issueNumber, body: body})
	return f.err
}

type fakeRuns struct {
	run.Repository
	runs map[uuid.UUID]*run.Run
}

func (f *fakeRuns) GetRun(_ context.Context, id uuid.UUID) (*run.Run, error) {
	r, ok := f.runs[id]
	if !ok {
		return nil, run.ErrNotFound
	}
	return r, nil
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

func (f *fakeAudit) AppendChained(_ context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.appended = append(f.appended, p)
	r := p.RunID
	return &audit.Entry{ID: uuid.New(), RunID: &r}, nil
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
	return out, nil
}
