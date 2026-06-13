package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/issuecomment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// statusCommentRunRepo provides GetRun + ListStagesForRun for status-comment tests.
// All other methods are stubs that return errors or no-ops.
type statusCommentRunRepo struct {
	stored *run.Run
	stages []*run.Stage
	getErr error
}

func (r *statusCommentRunRepo) GetRun(_ context.Context, id uuid.UUID) (*run.Run, error) {
	if r.getErr != nil {
		return nil, r.getErr
	}
	if r.stored == nil || r.stored.ID != id {
		return nil, run.ErrNotFound
	}
	return r.stored, nil
}

func (r *statusCommentRunRepo) ListStagesForRun(_ context.Context, _ uuid.UUID) ([]*run.Stage, error) {
	return r.stages, nil
}

func (r *statusCommentRunRepo) CreateRun(context.Context, run.CreateRunParams) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *statusCommentRunRepo) GetRunByIdempotencyKey(context.Context, string, string) (*run.Run, error) {
	return nil, run.ErrNotFound
}
func (r *statusCommentRunRepo) ListRuns(context.Context, run.ListRunsFilter) ([]*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *statusCommentRunRepo) TransitionRun(context.Context, uuid.UUID, run.State) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *statusCommentRunRepo) RetryRun(context.Context, uuid.UUID, run.State) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *statusCommentRunRepo) SetRunPullRequestURL(context.Context, uuid.UUID, string) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *statusCommentRunRepo) CreateStage(context.Context, run.CreateStageParams) (*run.Stage, error) {
	return nil, errors.New("not used")
}
func (r *statusCommentRunRepo) GetStage(context.Context, uuid.UUID) (*run.Stage, error) {
	return nil, errors.New("not used")
}
func (r *statusCommentRunRepo) ListStagesAwaitingApproval(context.Context) ([]*run.Stage, error) {
	return nil, errors.New("not used")
}
func (r *statusCommentRunRepo) ListReviewStagesAwaitingApproval(context.Context) ([]*run.Stage, error) {
	return nil, errors.New("not used")
}

func (r *statusCommentRunRepo) ListStagesAwaitingChildren(context.Context) ([]*run.Stage, error) {
	return nil, errors.New("not used")
}
func (r *statusCommentRunRepo) ListStagesDispatched(context.Context) ([]*run.Stage, error) {
	return nil, nil
}
func (r *statusCommentRunRepo) TransitionStage(context.Context, uuid.UUID, run.StageState, *run.StageCompletion) (*run.Stage, error) {
	return nil, errors.New("not used")
}
func (r *statusCommentRunRepo) RetryStage(context.Context, uuid.UUID, run.StageState) (*run.Stage, error) {
	return nil, errors.New("not used")
}

// scAuditFake is a minimal audit.Repository fake for status-comment tests.
// Embeds BaseFake and overrides only the methods the handlers exercise.
type scAuditFake struct {
	audit.BaseFake
	allEntries     []*audit.Entry
	statusEntries  []*audit.Entry
	appendedParams []audit.ChainAppendParams
	appendErr      error
}

func (a *scAuditFake) ListForRun(_ context.Context, _ uuid.UUID) ([]*audit.Entry, error) {
	return a.allEntries, nil
}

func (a *scAuditFake) ListForRunByCategory(_ context.Context, _ uuid.UUID, _ string) ([]*audit.Entry, error) {
	return a.statusEntries, nil
}

func (a *scAuditFake) AppendChained(_ context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	if a.appendErr != nil {
		return nil, a.appendErr
	}
	a.appendedParams = append(a.appendedParams, p)
	return &audit.Entry{
		ID:       uuid.New(),
		Sequence: int64(len(a.appendedParams)),
		RunID:    &p.RunID,
		Category: p.Category,
		Payload:  p.Payload,
	}, nil
}

func newSCServer(t *testing.T, stored *run.Run, stages []*run.Stage, af *scAuditFake) *Server {
	t.Helper()
	repo := &statusCommentRunRepo{stored: stored, stages: stages}
	return New(Config{
		Addr:        "127.0.0.1:0",
		RunRepo:     repo,
		AuditRepo:   af,
		ExternalURL: "http://localhost:8080",
	})
}

// --- GET tests ---

func TestGetStatusComment_HappyPath_NoExistingComment(t *testing.T) {
	runID := uuid.New()
	runRow := &run.Run{
		ID:           runID,
		Repo:         "x/y",
		WorkflowID:   "feature",
		State:        run.StatePending,
		IssueContext: &run.IssueContext{Number: 42, Title: "t", Body: "b", URL: "https://github.com/x/y/issues/42"},
	}
	s := newSCServer(t, runRow, nil, &scAuditFake{})

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/runs/%s/status-comment", runID), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	var resp statusCommentResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.GithubCommentID != 0 {
		t.Errorf("github_comment_id = %d, want 0 (no prior entry)", resp.GithubCommentID)
	}
	if resp.IssueNumber != 42 {
		t.Errorf("issue_number = %d, want 42", resp.IssueNumber)
	}
	if resp.Repo != "x/y" {
		t.Errorf("repo = %q, want x/y", resp.Repo)
	}
	if !strings.Contains(resp.Body, "Fishhawk run") {
		t.Errorf("body should contain rendered status header; got: %s", resp.Body)
	}
}

func TestGetStatusComment_ReturnsStoredCommentID(t *testing.T) {
	runID := uuid.New()
	runRow := &run.Run{
		ID:           runID,
		Repo:         "x/y",
		WorkflowID:   "feature",
		State:        run.StateRunning,
		IssueContext: &run.IssueContext{Number: 7},
	}
	payload, _ := json.Marshal(map[string]any{
		"kind": "status_update", "issue_number": 7, "repo": "x/y", "github_comment_id": int64(99),
	})
	af := &scAuditFake{
		statusEntries: []*audit.Entry{
			{ID: uuid.New(), Sequence: 1, Payload: payload, Category: issuecomment.CategoryStatusCommentPosted},
		},
	}
	s := newSCServer(t, runRow, nil, af)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/runs/%s/status-comment", runID), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	var resp statusCommentResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.GithubCommentID != 99 {
		t.Errorf("github_comment_id = %d, want 99", resp.GithubCommentID)
	}
}

func TestGetStatusComment_NotFound(t *testing.T) {
	s := newSCServer(t, nil, nil, &scAuditFake{})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/runs/%s/status-comment", uuid.New()), nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestGetStatusComment_NilRunRepo(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: &scAuditFake{}})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/runs/%s/status-comment", uuid.New()), nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestGetStatusComment_NilAuditRepo(t *testing.T) {
	runID := uuid.New()
	runRow := &run.Run{ID: runID, Repo: "x/y", WorkflowID: "f", State: run.StatePending}
	repo := &statusCommentRunRepo{stored: runRow}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/runs/%s/status-comment", runID), nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

// --- POST tests ---

func TestPostStatusComment_HappyPath(t *testing.T) {
	runID := uuid.New()
	runRow := &run.Run{
		ID:           runID,
		Repo:         "x/y",
		WorkflowID:   "feature",
		State:        run.StatePending,
		IssueContext: &run.IssueContext{Number: 42},
	}
	af := &scAuditFake{}
	s := newSCServer(t, runRow, nil, af)

	body := `{"github_comment_id": 12345}`
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/v0/runs/%s/status-comment", runID), strings.NewReader(body)))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	if len(af.appendedParams) != 1 {
		t.Fatalf("expected 1 audit append; got %d", len(af.appendedParams))
	}
	p := af.appendedParams[0]
	if p.Category != issuecomment.CategoryStatusCommentPosted {
		t.Errorf("category = %q, want %q", p.Category, issuecomment.CategoryStatusCommentPosted)
	}
	var pl map[string]any
	if err := json.Unmarshal(p.Payload, &pl); err != nil {
		t.Fatalf("decode audit payload: %v", err)
	}
	// json.Unmarshal decodes integers as float64
	if pl["github_comment_id"].(float64) != 12345 {
		t.Errorf("payload github_comment_id = %v, want 12345", pl["github_comment_id"])
	}
	if pl["repo"].(string) != "x/y" {
		t.Errorf("payload repo = %v, want x/y", pl["repo"])
	}
}

func TestPostStatusComment_SubsequentGETReadsBackPostedID(t *testing.T) {
	runID := uuid.New()
	runRow := &run.Run{
		ID:           runID,
		Repo:         "x/y",
		WorkflowID:   "feature",
		State:        run.StatePending,
		IssueContext: &run.IssueContext{Number: 5},
	}
	af := &scAuditFake{}
	s := newSCServer(t, runRow, nil, af)

	// POST the comment id.
	postBody := `{"github_comment_id": 777}`
	w1 := httptest.NewRecorder()
	s.Handler().ServeHTTP(w1, httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/v0/runs/%s/status-comment", runID), strings.NewReader(postBody)))
	if w1.Code != http.StatusCreated {
		t.Fatalf("POST status = %d", w1.Code)
	}

	// Simulate subsequent GET by seeding the appended entry into statusEntries.
	payload, _ := json.Marshal(map[string]any{
		"kind": "status_update", "issue_number": 5, "repo": "x/y", "github_comment_id": int64(777),
	})
	af.statusEntries = []*audit.Entry{
		{ID: uuid.New(), Sequence: 1, Payload: payload, Category: issuecomment.CategoryStatusCommentPosted},
	}

	w2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(w2, httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/runs/%s/status-comment", runID), nil))
	if w2.Code != http.StatusOK {
		t.Fatalf("GET status = %d:\n%s", w2.Code, w2.Body.String())
	}
	var resp statusCommentResponse
	_ = json.Unmarshal(w2.Body.Bytes(), &resp)
	if resp.GithubCommentID != 777 {
		t.Errorf("github_comment_id = %d, want 777", resp.GithubCommentID)
	}
}

func TestPostStatusComment_NotFound(t *testing.T) {
	s := newSCServer(t, nil, nil, &scAuditFake{})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/v0/runs/%s/status-comment", uuid.New()),
		strings.NewReader(`{"github_comment_id": 1}`)))
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestPostStatusComment_InvalidCommentID(t *testing.T) {
	runID := uuid.New()
	runRow := &run.Run{ID: runID, Repo: "x/y", WorkflowID: "f", State: run.StatePending}
	s := newSCServer(t, runRow, nil, &scAuditFake{})

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/v0/runs/%s/status-comment", runID),
		strings.NewReader(`{"github_comment_id": 0}`)))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "github_comment_id") {
		t.Errorf("body should mention github_comment_id: %s", w.Body.String())
	}
}

func TestPostStatusComment_BadJSON(t *testing.T) {
	runID := uuid.New()
	runRow := &run.Run{ID: runID, Repo: "x/y", WorkflowID: "f", State: run.StatePending}
	s := newSCServer(t, runRow, nil, &scAuditFake{})

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/v0/runs/%s/status-comment", runID),
		strings.NewReader("not json")))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestPostStatusComment_AuditAppendFails(t *testing.T) {
	runID := uuid.New()
	runRow := &run.Run{ID: runID, Repo: "x/y", WorkflowID: "f", State: run.StatePending}
	af := &scAuditFake{appendErr: errors.New("db down")}
	s := newSCServer(t, runRow, nil, af)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/v0/runs/%s/status-comment", runID),
		strings.NewReader(`{"github_comment_id": 1}`)))
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestPostStatusComment_NilRunRepo(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: &scAuditFake{}})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/v0/runs/%s/status-comment", uuid.New()),
		strings.NewReader(`{"github_comment_id": 1}`)))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestPostStatusComment_NilAuditRepo(t *testing.T) {
	runID := uuid.New()
	runRow := &run.Run{ID: runID, Repo: "x/y", WorkflowID: "f", State: run.StatePending}
	repo := &statusCommentRunRepo{stored: runRow}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/v0/runs/%s/status-comment", runID),
		strings.NewReader(`{"github_comment_id": 1}`)))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

// --- Anchor lifecycle (cross-boundary) ---

// TestAnchor_OneCommentIDAcrossLifecycle is the slice-3 load-bearing
// end-to-end test (#1054): it drives one simulated feature_change run
// through plan upload → two reviewer verdicts → human approval →
// implement → reviewer reject → fixup → plan re-upload (replan), refreshing
// the living anchor at each transition through the real
// issuecomment.Notifier + RenderAnchorBody projection (fake IssueCommenter
// + audit + artifact repos). It asserts:
//
//   - exactly ONE comment is ever created; every later refresh edits that
//     same comment id in place (the one-comment-id invariant);
//   - the awaiting-approval anchor still advertises reply-to-approve, so
//     the typed-reply / reaction gate path is not regressed by the surface
//     change (asserted, not assumed);
//   - after the replan, the current plan renders as Plan v2 and the earlier
//     plan renders collapsed as "Plan v1 (superseded)", with the reviewer
//     reject verdict surfaced;
//   - a second projection with no new chain entries is a no-op-equivalent
//     rebuild — same body, no new comment (the idempotency claim).
func TestAnchor_OneCommentIDAcrossLifecycle(t *testing.T) {
	runID := uuid.New()
	planStageID := uuid.New()
	implStageID := uuid.New()
	installID := int64(99)
	triggerRef := "issue:42"
	runRow := &run.Run{
		ID: runID, Repo: "x/y", WorkflowID: "feature_change",
		State:          run.StateRunning,
		TriggerSource:  run.TriggerGitHubIssue,
		TriggerRef:     &triggerRef,
		InstallationID: &installID,
		IssueContext:   &run.IssueContext{Number: 42},
	}
	planStage := &run.Stage{ID: planStageID, RunID: runID, Type: run.StageTypePlan, RequiresApproval: true, State: run.StageStatePending}
	implStage := &run.Stage{ID: implStageID, RunID: runID, Type: run.StageTypeImplement, State: run.StageStatePending}
	repo := &statusCommentRunRepo{stored: runRow, stages: []*run.Stage{planStage, implStage}}

	arts := newFakeArtifactRepo()
	aud := &lifecycleAudit{}
	gh := &lifecycleCommenter{t: t, nextID: 555}

	now := time.Unix(1_700_000_000, 0).UTC()
	n := issuecomment.New(issuecomment.Deps{
		GitHub: gh, Runs: repo, Audit: aud, Artifacts: arts,
		ExternalURL: "https://app.fishhawk.example",
		Now:         func() time.Time { return now },
	})
	if n == nil {
		t.Fatal("notifier should be non-nil")
	}
	ctx := context.Background()

	seq := int64(0)
	add := func(cat string, stageID *uuid.UUID, payload map[string]any) {
		seq++
		b, _ := json.Marshal(payload)
		aud.chain = append(aud.chain, &audit.Entry{
			ID: uuid.New(), Sequence: seq, RunID: &runID, StageID: stageID,
			Category: cat, Payload: b, Timestamp: now,
		})
	}
	schema := "standard_v1"
	addPlan := func(content string, at time.Time) {
		arts.all = append(arts.all, &artifact.Artifact{
			ID: uuid.New(), StageID: planStageID, Kind: artifact.KindPlan,
			SchemaVersion: &schema, Content: json.RawMessage(content), CreatedAt: at,
		})
	}
	refresh := func() {
		t.Helper()
		if err := n.NotifyStatusUpdateForRun(ctx, runID); err != nil {
			t.Fatalf("NotifyStatusUpdateForRun: %v", err)
		}
	}

	// 1) Run dispatched (no plan yet).
	add("run_dispatched", nil, map[string]any{})
	refresh()

	// 2) Plan v1 generated; plan stage parks awaiting approval.
	addPlan(`{"summary":"Implement feature X","scope":{"files":[{"path":"a.go","operation":"modify"}]},"approach":[{"step":1,"description":"do a"}]}`, now)
	planStage.State = run.StageStateAwaitingApproval
	add("plan_generated", &planStageID, map[string]any{})
	refresh()
	if !strings.Contains(gh.lastBody, "reply `+1`") {
		t.Errorf("awaiting-approval anchor must advertise reply-to-approve; got:\n%s", gh.lastBody)
	}

	// 3) Two reviewer verdicts land.
	add("plan_review_started", &planStageID, map[string]any{"configured_agents": 2})
	add("plan_reviewed", &planStageID, map[string]any{"reviewer_model": "opus-4-8", "verdict": "approve"})
	add("plan_reviewed", &planStageID, map[string]any{
		"reviewer_model": "gpt-5.5", "verdict": "approve_with_concerns",
		"concerns": []map[string]any{{"severity": "medium"}},
	})
	refresh()

	// 4) Human approval clears the gate; implement starts.
	add("approval_submitted", &planStageID, map[string]any{"decision": "approve", "approver": "kuhlman-labs"})
	planStage.State = run.StageStateSucceeded
	implStage.State = run.StageStateRunning
	refresh()

	// 5) Implement reviewer rejects.
	add("implement_review_started", &implStageID, map[string]any{"configured_agents": 1})
	add("implement_reviewed", &implStageID, map[string]any{
		"reviewer_model": "gpt-5.5", "verdict": "reject",
		"concerns": []map[string]any{{"severity": "high"}}, "free_form": "needs a regression test",
	})
	refresh()

	// 6) Fixup + replan: plan v2 re-uploaded to the plan stage.
	addPlan(`{"summary":"Implement feature X, take two","scope":{"files":[{"path":"a.go","operation":"modify"}]}}`, now.Add(time.Hour))
	add("plan_generated", &planStageID, map[string]any{})
	refresh()

	// One comment id throughout.
	if gh.creates != 1 {
		t.Fatalf("expected exactly 1 CreateIssueComment across the lifecycle; got %d", gh.creates)
	}
	if len(gh.updateIDs) == 0 {
		t.Fatalf("expected edit-in-place refreshes; got 0 updates")
	}
	for i, id := range gh.updateIDs {
		if id != gh.nextID {
			t.Errorf("update %d targeted comment id %d, want %d (edit-in-place)", i, id, gh.nextID)
		}
	}
	// Current + superseded plan rendering and the reviewer reject verdict.
	if !strings.Contains(gh.lastBody, "Plan v2") {
		t.Errorf("current plan v2 missing from anchor:\n%s", gh.lastBody)
	}
	if !strings.Contains(gh.lastBody, "Plan v1 (superseded)") {
		t.Errorf("superseded plan v1 missing from anchor:\n%s", gh.lastBody)
	}
	if !strings.Contains(gh.lastBody, "rejected") {
		t.Errorf("reviewer reject verdict missing from anchor:\n%s", gh.lastBody)
	}

	// Idempotent rebuild: no new chain entries → identical body, no new comment.
	prevBody, prevCreates, prevUpdates := gh.lastBody, gh.creates, len(gh.updateIDs)
	refresh()
	if gh.creates != prevCreates {
		t.Errorf("idempotent rebuild created a new comment; creates %d → %d", prevCreates, gh.creates)
	}
	if len(gh.updateIDs) != prevUpdates+1 {
		t.Errorf("idempotent rebuild should be one more in-place edit; updates %d → %d", prevUpdates, len(gh.updateIDs))
	}
	if gh.lastBody != prevBody {
		t.Errorf("idempotent rebuild produced a different body:\n--- prev ---\n%s\n--- now ---\n%s", prevBody, gh.lastBody)
	}
}

// lifecycleCommenter is a fake IssueCommenter that records create/update
// calls so the anchor lifecycle test can assert the one-comment-id
// invariant. It fails the test if a second comment is ever created.
type lifecycleCommenter struct {
	t         *testing.T
	nextID    int64
	creates   int
	updateIDs []int64
	lastBody  string
}

func (c *lifecycleCommenter) CreateIssueComment(_ context.Context, _ int64, _ githubclient.RepoRef, _ int, body string) (*githubclient.IssueComment, error) {
	c.creates++
	if c.creates > 1 {
		c.t.Fatalf("anchor created a second comment (want edit-in-place); body:\n%s", body)
	}
	c.lastBody = body
	return &githubclient.IssueComment{ID: c.nextID, Body: body}, nil
}

func (c *lifecycleCommenter) UpdateIssueComment(_ context.Context, _ int64, _ githubclient.RepoRef, commentID int64, body string) (*githubclient.IssueComment, error) {
	c.updateIDs = append(c.updateIDs, commentID)
	c.lastBody = body
	return &githubclient.IssueComment{ID: commentID, Body: body}, nil
}

// lifecycleAudit is an in-memory audit.Repository for the anchor lifecycle
// test: ListForRun returns the growing run chain, AppendChained records the
// notifier's status_comment_posted rows so the next refresh resolves the
// same comment id, and ListForRunByCategory serves both surfaces.
type lifecycleAudit struct {
	audit.BaseFake
	chain      []*audit.Entry
	statusRows []*audit.Entry
	seq        int64
}

func (a *lifecycleAudit) ListForRun(_ context.Context, _ uuid.UUID) ([]*audit.Entry, error) {
	return a.chain, nil
}

func (a *lifecycleAudit) ListForRunByCategory(_ context.Context, _ uuid.UUID, cat string) ([]*audit.Entry, error) {
	if cat == issuecomment.CategoryStatusCommentPosted {
		return a.statusRows, nil
	}
	out := []*audit.Entry{}
	for _, e := range a.chain {
		if e.Category == cat {
			out = append(out, e)
		}
	}
	return out, nil
}

func (a *lifecycleAudit) AppendChained(_ context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	a.seq++
	r := p.RunID
	e := &audit.Entry{ID: uuid.New(), Sequence: a.seq, RunID: &r, Category: p.Category, Payload: p.Payload, Timestamp: p.Timestamp}
	if p.Category == issuecomment.CategoryStatusCommentPosted {
		a.statusRows = append(a.statusRows, e)
	}
	return e, nil
}
