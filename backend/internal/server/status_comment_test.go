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

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
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
