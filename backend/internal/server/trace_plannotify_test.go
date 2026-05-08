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
	s.notifyPlanReady(req, r.ID, planStage)

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
	s.notifyPlanReady(req, r.ID, planStage)
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
	s.notifyPlanReady(req, r.ID, planStage)

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

func (c *commentRecorder) CreateIssueComment(_ context.Context, installationID int64, repo githubclient.RepoRef, issueNumber int, body string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stored = append(c.stored, commentCall{installationID: installationID, repo: repo, issueNumber: issueNumber, body: body})
	return nil
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
