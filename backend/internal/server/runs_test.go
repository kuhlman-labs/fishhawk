package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// fakeRepo is a minimum in-memory implementation of run.Repository
// for handler tests. The Postgres adapter is exercised separately
// in backend/internal/run/postgres_test.go via testcontainers, so
// this fake only needs to satisfy the contract well enough that
// the handler logic gets coverage.
type fakeRepo struct {
	mu   sync.Mutex
	runs map[uuid.UUID]*run.Run

	// createErr / getErr let tests inject failures without
	// instrumenting fakeRepo's internals.
	createErr error
	getErr    error
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{runs: map[uuid.UUID]*run.Run{}}
}

func (f *fakeRepo) CreateRun(_ context.Context, p run.CreateRunParams) (*run.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return nil, f.createErr
	}
	now := time.Now().UTC()
	r := &run.Run{
		ID:            uuid.New(),
		Repo:          p.Repo,
		WorkflowID:    p.WorkflowID,
		WorkflowSHA:   p.WorkflowSHA,
		TriggerSource: p.TriggerSource,
		TriggerRef:    p.TriggerRef,
		State:         run.StatePending,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	f.runs[r.ID] = r
	return r, nil
}

func (f *fakeRepo) GetRun(_ context.Context, id uuid.UUID) (*run.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	r, ok := f.runs[id]
	if !ok {
		return nil, run.ErrNotFound
	}
	return r, nil
}

// The remaining methods aren't exercised by these handler tests but
// must exist so fakeRepo satisfies run.Repository. Returning
// "not implemented" errors makes any accidental call obvious.
func (f *fakeRepo) TransitionRun(_ context.Context, _ uuid.UUID, _ run.State) (*run.Run, error) {
	return nil, errors.New("fakeRepo: TransitionRun not implemented")
}
func (f *fakeRepo) CreateStage(_ context.Context, _ run.CreateStageParams) (*run.Stage, error) {
	return nil, errors.New("fakeRepo: CreateStage not implemented")
}
func (f *fakeRepo) GetStage(_ context.Context, _ uuid.UUID) (*run.Stage, error) {
	return nil, errors.New("fakeRepo: GetStage not implemented")
}
func (f *fakeRepo) ListStagesForRun(_ context.Context, _ uuid.UUID) ([]*run.Stage, error) {
	return nil, errors.New("fakeRepo: ListStagesForRun not implemented")
}
func (f *fakeRepo) TransitionStage(_ context.Context, _ uuid.UUID, _ run.StageState, _ *run.StageCompletion) (*run.Stage, error) {
	return nil, errors.New("fakeRepo: TransitionStage not implemented")
}

// newServer builds a Server wired to repo, with the Handler() exposed
// so tests can call it via httptest.NewRecorder.
func newServer(t *testing.T, repo run.Repository) *Server {
	t.Helper()
	return New(Config{Addr: "127.0.0.1:0", RunRepo: repo})
}

func TestCreateRun_HappyPath(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)

	body := `{
		"repo": "kuhlman-labs/fishhawk",
		"workflow_id": "feature_change",
		"workflow_sha": "abc123",
		"trigger_source": "cli"
	}`
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}

	var got runResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got.Repo != "kuhlman-labs/fishhawk" {
		t.Errorf("Repo = %q", got.Repo)
	}
	if got.State != string(run.StatePending) {
		t.Errorf("State = %q, want pending", got.State)
	}
	if got.TriggerSource != "cli" {
		t.Errorf("TriggerSource = %q", got.TriggerSource)
	}
	if got.ID == uuid.Nil {
		t.Error("ID is zero")
	}
}

func TestCreateRun_OptionalTriggerRef(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)

	body := `{
		"repo": "x/y",
		"workflow_id": "w",
		"workflow_sha": "abc",
		"trigger_source": "github_issue",
		"trigger_ref": "issue:1247"
	}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader(body))
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	var got runResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.TriggerRef == nil || *got.TriggerRef != "issue:1247" {
		t.Errorf("TriggerRef = %v, want issue:1247", got.TriggerRef)
	}
}

func TestCreateRun_BadJSON(t *testing.T) {
	s := newServer(t, newFakeRepo())
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader("{not json"))
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"validation_failed"`) {
		t.Errorf("body missing code: %s", w.Body.String())
	}
}

func TestCreateRun_UnknownField(t *testing.T) {
	s := newServer(t, newFakeRepo())
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v0/runs",
		strings.NewReader(`{"repo":"r","workflow_id":"w","workflow_sha":"s","trigger_source":"cli","extra":"x"}`))
	s.Handler().ServeHTTP(w, req)
	// DisallowUnknownFields → 400.
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 on unknown field", w.Code)
	}
}

func TestCreateRun_MissingRequiredFields(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantField string
	}{
		{"no repo", `{"workflow_id":"w","workflow_sha":"s","trigger_source":"cli"}`, "repo"},
		{"no workflow_id", `{"repo":"r","workflow_sha":"s","trigger_source":"cli"}`, "workflow_id"},
		{"no workflow_sha", `{"repo":"r","workflow_id":"w","trigger_source":"cli"}`, "workflow_sha"},
	}
	s := newServer(t, newFakeRepo())
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader(tc.body))
			s.Handler().ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", w.Code)
			}
			if !strings.Contains(w.Body.String(), tc.wantField) {
				t.Errorf("body missing field name %q: %s", tc.wantField, w.Body.String())
			}
		})
	}
}

func TestCreateRun_BadTriggerSource(t *testing.T) {
	s := newServer(t, newFakeRepo())
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v0/runs",
		strings.NewReader(`{"repo":"r","workflow_id":"w","workflow_sha":"s","trigger_source":"bogus"}`))
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestCreateRun_RepoError(t *testing.T) {
	repo := newFakeRepo()
	repo.createErr = errors.New("disk full")
	s := newServer(t, repo)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v0/runs",
		strings.NewReader(`{"repo":"r","workflow_id":"w","workflow_sha":"s","trigger_source":"cli"}`))
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"internal_error"`) {
		t.Errorf("body missing internal_error code: %s", w.Body.String())
	}
}

func TestCreateRun_NilRepoConfigured(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"}) // no RunRepo
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v0/runs",
		strings.NewReader(`{"repo":"r","workflow_id":"w","workflow_sha":"s","trigger_source":"cli"}`))
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestGetRun_HappyPath(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)

	// Pre-seed a run by calling the repo directly.
	got, _ := repo.CreateRun(context.Background(), run.CreateRunParams{
		Repo: "x/y", WorkflowID: "w", WorkflowSHA: "s",
		TriggerSource: run.TriggerCLI,
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v0/runs/%s", got.ID), nil)
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp runResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.ID != got.ID {
		t.Errorf("ID = %s, want %s", resp.ID, got.ID)
	}
}

func TestGetRun_NotFound(t *testing.T) {
	s := newServer(t, newFakeRepo())
	id := uuid.New()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v0/runs/%s", id), nil)
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"run_not_found"`) {
		t.Errorf("body missing run_not_found code: %s", w.Body.String())
	}
}

func TestGetRun_BadUUID(t *testing.T) {
	s := newServer(t, newFakeRepo())
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v0/runs/not-a-uuid", nil)
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestGetRun_RepoError(t *testing.T) {
	repo := newFakeRepo()
	repo.getErr = errors.New("connection lost")
	s := newServer(t, repo)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v0/runs/%s", uuid.New()), nil)
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestGetRun_NilRepoConfigured(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v0/runs/%s", uuid.New()), nil)
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestErrorEnvelope_Shape(t *testing.T) {
	// Decoding a known 400 confirms the envelope matches OpenAPI's
	// error schema verbatim. If the field names drift, clients
	// switching on `error.code` break.
	s := newServer(t, newFakeRepo())
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader("{not json"))
	s.Handler().ServeHTTP(w, req)
	body, _ := io.ReadAll(w.Body)
	var env errorEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if env.Error.Code == "" || env.Error.Message == "" {
		t.Errorf("error envelope missing code/message: %+v", env)
	}
}

// requestPath is a tiny helper for round-tripping a raw body through
// the server and asserting status + decoded JSON.
func requestPath(t *testing.T, s *Server, method, path string, body any) (*httptest.ResponseRecorder, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	s.Handler().ServeHTTP(w, req)
	return w, w.Body.Bytes()
}

func TestRoundTrip_CreateThenGet(t *testing.T) {
	s := newServer(t, newFakeRepo())

	w, body := requestPath(t, s, http.MethodPost, "/v0/runs", map[string]any{
		"repo":           "x/y",
		"workflow_id":    "w",
		"workflow_sha":   "abc",
		"trigger_source": "ui",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("create status = %d:\n%s", w.Code, body)
	}
	var created runResponse
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}

	w2, body2 := requestPath(t, s, http.MethodGet, "/v0/runs/"+created.ID.String(), nil)
	if w2.Code != http.StatusOK {
		t.Fatalf("get status = %d:\n%s", w2.Code, body2)
	}
	var fetched runResponse
	if err := json.Unmarshal(body2, &fetched); err != nil {
		t.Fatal(err)
	}
	if fetched.ID != created.ID {
		t.Errorf("ID round-trip mismatch: %s vs %s", fetched.ID, created.ID)
	}
}
