package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/issuecomment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Verifies that the trace handler's plan-ready hook fires the
// issuecomment notifier when the plan stage transitions terminally
// for an issue-triggered run (#234). Unit-level rule coverage lives
// in internal/issuecomment; this only asserts the trace-handler →
// notifier wiring.

func TestNotifyPlanReady_FiresNotifierOnPlanStageTransition(t *testing.T) {
	rr := newOrchestratorRepo()
	r := rr.seedRun()
	r.TriggerSource = run.TriggerGitHubIssue
	triggerRef := "issue:42"
	r.TriggerRef = &triggerRef
	r.InstallationID = ptrInt64(99)
	r.Repo = "x/y"
	r.WorkflowID = "feature_change"

	planStage := rr.seedStage(r.ID, 0, run.StageStateAwaitingApproval)
	planStage.Type = run.StageTypePlan
	planStage.RequiresApproval = true

	// Plan artifact attached to the plan stage.
	arts := newFakeArtifactRepo()
	v := "standard_v1"
	arts.all = append(arts.all, &artifact.Artifact{
		ID: uuid.New(), StageID: planStage.ID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &v,
		Content:       json.RawMessage(`{"summary":"add a feature","scope":{"files":[{"path":"x.go","operation":"modify"}]}}`),
		CreatedAt:     time.Now().UTC(),
	})

	au := newPlanReadyAuditFake()
	gh := newCommentRecorder()

	s := New(Config{
		Addr: "127.0.0.1:0", RunRepo: rr,
		AuditRepo: au, ArtifactRepo: arts,
		ExternalURL: "https://app.fishhawk.example.com",
	})
	// Wire a real issuecomment.Notifier with the fake GitHub.
	s.issueNotifier = issuecomment.New(issuecomment.Deps{
		GitHub:      gh,
		Runs:        rr,
		Audit:       au,
		ExternalURL: "https://app.fishhawk.example.com",
	})

	// Drive notifyPlanReady directly; the trace handler calls it
	// after advanceStageAfterTrace on plan stages, so this exercises
	// the same code path without the full multipart upload setup.
	req := httptest.NewRequest("POST", "/", nil)
	s.notifyPlanReady(req.Context(), r.ID, planStage)

	if got := gh.calls(); len(got) != 1 {
		t.Fatalf("expected 1 GitHub call; got %d", len(got))
	}
	c := gh.calls()[0]
	if c.issueNumber != 42 {
		t.Errorf("issueNumber = %d", c.issueNumber)
	}
	if !strings.Contains(c.body, "Plan ready") {
		t.Errorf("body should reference plan ready: %q", c.body)
	}
	if !strings.Contains(c.body, "/stages/"+planStage.ID.String()) {
		t.Errorf("body should link to approval surface: %q", c.body)
	}
	if !strings.Contains(c.body, "x.go") {
		t.Errorf("body should include scope file: %q", c.body)
	}
}

func TestNotifyPlanReady_NoNotifier_NoOp(t *testing.T) {
	rr := newOrchestratorRepo()
	r := rr.seedRun()
	planStage := rr.seedStage(r.ID, 0, run.StageStateAwaitingApproval)
	planStage.Type = run.StageTypePlan

	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr})
	if s.issueNotifier != nil {
		t.Fatal("notifier should be nil without GitHub + ExternalURL")
	}
	// Should not panic, should be a no-op.
	req := httptest.NewRequest("POST", "/", nil)
	s.notifyPlanReady(req.Context(), r.ID, planStage)
}

// TestNotifyPlanReady_RealRunnerOrder_TracePrecedesPlan covers the
// race that #245 fixed: the runner ships trace bundles BEFORE the
// plan artifact, so the trace-handler's notify hook runs before the
// plan exists and silently skips. The plan-upload handler fires the
// hook again; the audit-log dedup ensures only one comment lands.
func TestNotifyPlanReady_RealRunnerOrder_TracePrecedesPlan(t *testing.T) {
	rr := newOrchestratorRepo()
	r := rr.seedRun()
	r.TriggerSource = run.TriggerGitHubIssue
	triggerRef := "issue:42"
	r.TriggerRef = &triggerRef
	r.InstallationID = ptrInt64(99)
	r.Repo = "x/y"
	r.WorkflowID = "feature_change"

	planStage := rr.seedStage(r.ID, 0, run.StageStateAwaitingApproval)
	planStage.Type = run.StageTypePlan
	planStage.RequiresApproval = true

	arts := newFakeArtifactRepo()
	au := newPlanReadyAuditFake()
	gh := newCommentRecorder()

	s := New(Config{
		Addr: "127.0.0.1:0", RunRepo: rr,
		AuditRepo: au, ArtifactRepo: arts,
		ExternalURL: "https://app.fishhawk.example.com",
	})
	s.issueNotifier = issuecomment.New(issuecomment.Deps{
		GitHub: gh, Runs: rr, Audit: au,
		ExternalURL: "https://app.fishhawk.example.com",
	})
	req := httptest.NewRequest("POST", "/", nil)

	// Step 1: trace lands first (the runner's actual order). The
	// trace-handler's notify hook fires; no plan artifact exists
	// yet; we should silently skip without commenting.
	s.notifyPlanReady(req.Context(), r.ID, planStage)
	if got := gh.calls(); len(got) != 0 {
		t.Fatalf("trace-handler hook commented before plan artifact existed; got %d calls", len(got))
	}

	// Step 2: plan upload lands. The plan-upload handler's hook
	// fires; the artifact is now there; the comment lands.
	v := "standard_v1"
	arts.all = append(arts.all, &artifact.Artifact{
		ID: uuid.New(), StageID: planStage.ID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &v,
		Content:       json.RawMessage(`{"summary":"add a feature","scope":{"files":[{"path":"x.go","operation":"modify"}]}}`),
		CreatedAt:     time.Now().UTC(),
	})
	s.notifyPlanReadyIfReady(req, r.ID, planStage)
	if got := gh.calls(); len(got) != 1 {
		t.Fatalf("plan-upload hook should have posted one comment; got %d calls", len(got))
	}
	if !strings.Contains(gh.calls()[0].body, "Plan ready") {
		t.Errorf("comment body should reference plan-ready: %q", gh.calls()[0].body)
	}

	// Step 3: re-fire (e.g. runner retries the plan upload, hits
	// the idempotent path). Audit-log dedup should keep this a
	// no-op so the issue doesn't get the same comment twice.
	s.notifyPlanReadyIfReady(req, r.ID, planStage)
	if got := gh.calls(); len(got) != 1 {
		t.Errorf("dedup failed — re-firing the plan-upload hook produced %d calls; want 1", len(got))
	}
}

// TestNotifyPlanReadyIfReady_SkipsWhenStagePending guards the
// future-runner case where a plan upload arrives before the trace
// (so the stage is still 'dispatched'/'running'). Commenting at
// that point would be premature; the notify must wait for the
// terminal transition.
func TestNotifyPlanReadyIfReady_SkipsWhenStagePending(t *testing.T) {
	rr := newOrchestratorRepo()
	r := rr.seedRun()
	r.TriggerSource = run.TriggerGitHubIssue
	triggerRef := "issue:42"
	r.TriggerRef = &triggerRef
	r.InstallationID = ptrInt64(99)
	r.Repo = "x/y"

	planStage := rr.seedStage(r.ID, 0, run.StageStateRunning) // not yet terminal
	planStage.Type = run.StageTypePlan

	arts := newFakeArtifactRepo()
	v := "standard_v1"
	arts.all = append(arts.all, &artifact.Artifact{
		ID: uuid.New(), StageID: planStage.ID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &v,
		Content:       json.RawMessage(`{"summary":"x","scope":{"files":[]}}`),
		CreatedAt:     time.Now().UTC(),
	})
	au := newPlanReadyAuditFake()
	gh := newCommentRecorder()

	s := New(Config{
		Addr: "127.0.0.1:0", RunRepo: rr,
		AuditRepo: au, ArtifactRepo: arts,
		ExternalURL: "https://app.fishhawk.example.com",
	})
	s.issueNotifier = issuecomment.New(issuecomment.Deps{
		GitHub: gh, Runs: rr, Audit: au,
		ExternalURL: "https://app.fishhawk.example.com",
	})
	req := httptest.NewRequest("POST", "/", nil)
	s.notifyPlanReadyIfReady(req, r.ID, planStage)
	if got := gh.calls(); len(got) != 0 {
		t.Errorf("plan-upload hook commented while stage was non-terminal; got %d calls", len(got))
	}
}

func TestNotifyPlanReadyIfReady_SkipsForNonPlanStage(t *testing.T) {
	rr := newOrchestratorRepo()
	r := rr.seedRun()
	implementStage := rr.seedStage(r.ID, 1, run.StageStateAwaitingApproval)
	implementStage.Type = run.StageTypeImplement

	gh := newCommentRecorder()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr})
	s.issueNotifier = issuecomment.New(issuecomment.Deps{
		GitHub: gh, Runs: rr, Audit: newPlanReadyAuditFake(),
		ExternalURL: "https://app.fishhawk.example.com",
	})
	req := httptest.NewRequest("POST", "/", nil)
	s.notifyPlanReadyIfReady(req, r.ID, implementStage)
	if got := gh.calls(); len(got) != 0 {
		t.Errorf("plan-upload hook commented on a non-plan stage; got %d calls", len(got))
	}
}

func TestNotifyPlanReady_NoPlanArtifact_SkipsCleanly(t *testing.T) {
	rr := newOrchestratorRepo()
	r := rr.seedRun()
	r.TriggerSource = run.TriggerGitHubIssue
	triggerRef := "issue:42"
	r.TriggerRef = &triggerRef
	r.InstallationID = ptrInt64(99)
	r.Repo = "x/y"

	planStage := rr.seedStage(r.ID, 0, run.StageStateAwaitingApproval)
	planStage.Type = run.StageTypePlan

	au := newPlanReadyAuditFake()
	gh := newCommentRecorder()

	s := New(Config{
		Addr: "127.0.0.1:0", RunRepo: rr,
		AuditRepo: au, ArtifactRepo: newFakeArtifactRepo(),
		ExternalURL: "https://app.fishhawk.example.com",
	})
	s.issueNotifier = issuecomment.New(issuecomment.Deps{
		GitHub: gh, Runs: rr, Audit: au,
		ExternalURL: "https://app.fishhawk.example.com",
	})

	req := httptest.NewRequest("POST", "/", nil)
	s.notifyPlanReady(req.Context(), r.ID, planStage)

	if got := gh.calls(); len(got) != 0 {
		t.Errorf("expected 0 GitHub calls when no plan artifact attached; got %d", len(got))
	}
}

// --- helpers ---

type commentCall struct {
	installationID int64
	repo           githubclient.RepoRef
	issueNumber    int
	body           string
}

type commentRecorder struct {
	mu     sync.Mutex
	stored []commentCall
}

func newCommentRecorder() *commentRecorder { return &commentRecorder{} }

func (c *commentRecorder) calls() []commentCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]commentCall, len(c.stored))
	copy(out, c.stored)
	return out
}

func (c *commentRecorder) CreateIssueComment(_ context.Context, installationID int64, repo githubclient.RepoRef, issueNumber int, body string) (*githubclient.IssueComment, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stored = append(c.stored, commentCall{installationID: installationID, repo: repo, issueNumber: issueNumber, body: body})
	return &githubclient.IssueComment{ID: int64(len(c.stored)), Body: body}, nil
}

// UpdateIssueComment is a stub for the IssueCommenter interface
// extension landed in #328. trace_plannotify tests don't exercise
// the update path; returning a happy response keeps the interface
// satisfied without changing the test surface.
func (c *commentRecorder) UpdateIssueComment(_ context.Context, _ int64, _ githubclient.RepoRef, commentID int64, body string) (*githubclient.IssueComment, error) {
	return &githubclient.IssueComment{ID: commentID, Body: body}, nil
}

// planReadyAuditFake is a tiny in-memory audit.Repository that the
// notifier's dedup query and AppendChained can both run against.
// Mirrors the in-memory fakes elsewhere in the package.
type planReadyAuditFake struct {
	mu       sync.Mutex
	entries  []*audit.Entry
	appended []audit.ChainAppendParams
}

func newPlanReadyAuditFake() *planReadyAuditFake {
	return &planReadyAuditFake{}
}

func (f *planReadyAuditFake) Append(context.Context, audit.AppendParams) (*audit.Entry, error) {
	return nil, errPlanReadyFakeNotImpl
}

func (f *planReadyAuditFake) ChainsByParent(_ context.Context, _ uuid.UUID, _ bool) ([]*audit.Entry, error) {
	return nil, nil
}
func (f *planReadyAuditFake) AppendChained(_ context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.appended = append(f.appended, p)
	r := p.RunID
	entry := &audit.Entry{
		ID: uuid.New(), RunID: &r, Category: p.Category, Payload: p.Payload, Timestamp: p.Timestamp,
	}
	f.entries = append(f.entries, entry)
	return entry, nil
}

func (f *planReadyAuditFake) AppendGlobalChained(context.Context, audit.GlobalChainAppendParams) (*audit.Entry, error) {
	return nil, errPlanReadyFakeNotImpl
}

func (f *planReadyAuditFake) ListGlobal(context.Context) ([]*audit.Entry, error) {
	return nil, nil
}
func (f *planReadyAuditFake) ListAll(context.Context, audit.ListAllParams) ([]*audit.Entry, error) {
	return nil, nil
}
func (f *planReadyAuditFake) Get(context.Context, uuid.UUID) (*audit.Entry, error) {
	return nil, errPlanReadyFakeNotImpl
}
func (f *planReadyAuditFake) ListForRun(context.Context, uuid.UUID) ([]*audit.Entry, error) {
	return nil, errPlanReadyFakeNotImpl
}
func (f *planReadyAuditFake) LastForRun(context.Context, uuid.UUID) (*audit.Entry, error) {
	return nil, errPlanReadyFakeNotImpl
}
func (f *planReadyAuditFake) ListForRunByCategory(_ context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []*audit.Entry{}
	for _, e := range f.entries {
		if e.RunID != nil && *e.RunID == runID && e.Category == category {
			out = append(out, e)
		}
	}
	return out, nil
}

var errPlanReadyFakeNotImpl = newPlanReadyFakeNotImpl()

func newPlanReadyFakeNotImpl() error {
	return planReadyFakeError("plan-ready audit fake: method not implemented")
}

type planReadyFakeError string

func (e planReadyFakeError) Error() string { return string(e) }
