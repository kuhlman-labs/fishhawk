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
	"sort"
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

	// createErr / getErr / transitionErr / listErr let tests inject
	// failures without instrumenting fakeRepo's internals.
	createErr     error
	getErr        error
	transitionErr error
	listErr       error
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
		ID:             uuid.New(),
		Repo:           p.Repo,
		WorkflowID:     p.WorkflowID,
		WorkflowSHA:    p.WorkflowSHA,
		TriggerSource:  p.TriggerSource,
		TriggerRef:     p.TriggerRef,
		IdempotencyKey: p.IdempotencyKey,
		State:          run.StatePending,
		CreatedAt:      now,
		UpdatedAt:      now,
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

// transitionErr lets tests inject specific errors out of TransitionRun
// so the cancel handler's branches (404, 409, 500) are reachable.
// listErr does the same for ListRuns.
//
// The remaining stage methods aren't exercised by these handler tests
// but must exist so fakeRepo satisfies run.Repository.

func (f *fakeRepo) GetRunByIdempotencyKey(_ context.Context, repo, key string) (*run.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.runs {
		if r.Repo == repo && r.IdempotencyKey != nil && *r.IdempotencyKey == key {
			return r, nil
		}
	}
	return nil, run.ErrNotFound
}

func (f *fakeRepo) TransitionRun(_ context.Context, id uuid.UUID, to run.State) (*run.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.transitionErr != nil {
		return nil, f.transitionErr
	}
	r, ok := f.runs[id]
	if !ok {
		return nil, run.ErrNotFound
	}
	// Match the postgres adapter's transition rules: same-state is
	// idempotent, terminal states reject further transitions.
	if r.State == to {
		return r, nil
	}
	if r.State == run.StateSucceeded || r.State == run.StateFailed || r.State == run.StateCancelled {
		return nil, run.InvalidTransitionError{Kind: "run", From: string(r.State), To: string(to)}
	}
	r.State = to
	r.UpdatedAt = time.Now().UTC()
	return r, nil
}

func (f *fakeRepo) ListRuns(_ context.Context, fil run.ListRunsFilter) ([]*run.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	var matched []*run.Run
	for _, r := range f.runs {
		if fil.Repo != "" && r.Repo != fil.Repo {
			continue
		}
		if fil.WorkflowID != "" && r.WorkflowID != fil.WorkflowID {
			continue
		}
		if fil.State != "" && string(r.State) != fil.State {
			continue
		}
		matched = append(matched, r)
	}
	// Order matches the SQL: created_at DESC, id DESC.
	sort.Slice(matched, func(i, j int) bool {
		if !matched[i].CreatedAt.Equal(matched[j].CreatedAt) {
			return matched[i].CreatedAt.After(matched[j].CreatedAt)
		}
		return matched[i].ID.String() > matched[j].ID.String()
	})
	if fil.Offset >= len(matched) {
		return nil, nil
	}
	end := fil.Offset + fil.Limit
	if end > len(matched) {
		end = len(matched)
	}
	return matched[fil.Offset:end], nil
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
func (f *fakeRepo) ListStagesAwaitingApproval(context.Context) ([]*run.Stage, error) {
	return nil, errors.New("fakeRepo: ListStagesAwaitingApproval not implemented")
}

func (f *fakeRepo) ListStagesDispatched(context.Context) ([]*run.Stage, error) {
	return nil, nil
}

func (f *fakeRepo) RetryStage(context.Context, uuid.UUID, run.StageState) (*run.Stage, error) {
	return nil, errors.New("not used")
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

// -------- Idempotency-Key tests (E8.2) --------

func TestCreateRun_IdempotencyKey_Replay_Returns200(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)
	body := `{
		"repo": "x/y",
		"workflow_id": "w",
		"workflow_sha": "s",
		"trigger_source": "cli"
	}`

	req1 := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader(body))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("Idempotency-Key", "abc123")
	w1 := httptest.NewRecorder()
	s.Handler().ServeHTTP(w1, req1)
	if w1.Code != http.StatusCreated {
		t.Fatalf("first status = %d, want 201:\n%s", w1.Code, w1.Body.String())
	}
	var first runResponse
	_ = json.Unmarshal(w1.Body.Bytes(), &first)

	// Replay: same key, same body → 200 with the prior run.
	req2 := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Idempotency-Key", "abc123")
	w2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("replay status = %d, want 200:\n%s", w2.Code, w2.Body.String())
	}
	var second runResponse
	_ = json.Unmarshal(w2.Body.Bytes(), &second)
	if second.ID != first.ID {
		t.Errorf("replay returned a different run: first=%s second=%s", first.ID, second.ID)
	}
	if len(repo.runs) != 1 {
		t.Errorf("repo has %d runs, want 1 (replay must not insert)", len(repo.runs))
	}
}

func TestCreateRun_IdempotencyKey_DifferentRepo_CreatesSeparateRun(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)
	body := func(r string) string {
		return `{"repo":"` + r + `","workflow_id":"w","workflow_sha":"s","trigger_source":"cli"}`
	}

	req1 := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader(body("a/x")))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("Idempotency-Key", "shared")
	w1 := httptest.NewRecorder()
	s.Handler().ServeHTTP(w1, req1)
	if w1.Code != http.StatusCreated {
		t.Fatalf("first status = %d", w1.Code)
	}

	// Same key, different repo → separate run.
	req2 := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader(body("b/y")))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Idempotency-Key", "shared")
	w2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(w2, req2)
	if w2.Code != http.StatusCreated {
		t.Fatalf("second status = %d, want 201 (different repo, no collision)", w2.Code)
	}
	if len(repo.runs) != 2 {
		t.Errorf("repo has %d runs, want 2", len(repo.runs))
	}
}

func TestCreateRun_IdempotencyKey_DifferentKey_CreatesSeparateRun(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)
	body := `{"repo":"x/y","workflow_id":"w","workflow_sha":"s","trigger_source":"cli"}`

	for _, key := range []string{"k1", "k2"} {
		req := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("status = %d for key=%s", w.Code, key)
		}
	}
	if len(repo.runs) != 2 {
		t.Errorf("repo has %d runs, want 2", len(repo.runs))
	}
}

func TestCreateRun_NoIdempotencyKey_AlwaysCreates(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)
	body := `{"repo":"x/y","workflow_id":"w","workflow_sha":"s","trigger_source":"cli"}`

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("iter %d status = %d", i, w.Code)
		}
	}
	if len(repo.runs) != 3 {
		t.Errorf("repo has %d runs, want 3 (no key = always create)", len(repo.runs))
	}
}

func TestCreateRun_IdempotencyKey_Whitespace_Trimmed(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)
	body := `{"repo":"x/y","workflow_id":"w","workflow_sha":"s","trigger_source":"cli"}`

	req1 := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader(body))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("Idempotency-Key", "abc")
	w1 := httptest.NewRecorder()
	s.Handler().ServeHTTP(w1, req1)

	// Header with surrounding whitespace should match the original.
	req2 := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Idempotency-Key", "  abc  ")
	w2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Errorf("whitespace-padded key didn't match original: status = %d", w2.Code)
	}
	_ = w1
	if len(repo.runs) != 1 {
		t.Errorf("repo has %d runs, want 1", len(repo.runs))
	}
}

func TestCreateRun_IdempotencyKey_LookupErrorBubbles(t *testing.T) {
	// Use a repo whose GetRunByIdempotencyKey returns an
	// unexpected error (not ErrNotFound). The handler should 500
	// rather than silently fall through to create.
	repo := &errIdempotencyRepo{}
	s := newServer(t, repo)
	body := `{"repo":"x/y","workflow_id":"w","workflow_sha":"s","trigger_source":"cli"}`
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "abc")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

// errIdempotencyRepo wraps fakeRepo to inject a non-ErrNotFound
// error from GetRunByIdempotencyKey while behaving normally for
// every other method. Used to exercise the handler's "unexpected
// error" path.
type errIdempotencyRepo struct {
	fakeRepo
}

func (e *errIdempotencyRepo) GetRunByIdempotencyKey(context.Context, string, string) (*run.Run, error) {
	return nil, errors.New("simulated lookup error")
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

// seedRun inserts a run with controlled fields directly into the
// fake's map so list/cancel tests don't depend on POST /v0/runs.
func seedRun(repo *fakeRepo, repoName, workflowID string, state run.State, createdAt time.Time) *run.Run {
	r := &run.Run{
		ID:            uuid.New(),
		Repo:          repoName,
		WorkflowID:    workflowID,
		WorkflowSHA:   "sha-" + string(state),
		TriggerSource: run.TriggerCLI,
		State:         state,
		CreatedAt:     createdAt,
		UpdatedAt:     createdAt,
	}
	repo.mu.Lock()
	repo.runs[r.ID] = r
	repo.mu.Unlock()
	return r
}

func TestListRuns_HappyPath(t *testing.T) {
	repo := newFakeRepo()
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	seedRun(repo, "x/y", "feature_change", run.StatePending, t0)
	seedRun(repo, "x/y", "feature_change", run.StateRunning, t0.Add(time.Second))
	seedRun(repo, "a/b", "hotfix", run.StateSucceeded, t0.Add(2*time.Second))
	s := newServer(t, repo)

	req := httptest.NewRequest(http.MethodGet, "/v0/runs", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	var got struct {
		Items      []runResponse `json:"items"`
		NextCursor string        `json:"next_cursor"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 3 {
		t.Errorf("items = %d, want 3", len(got.Items))
	}
	// created_at DESC: most-recently created comes first.
	if got.Items[0].State != string(run.StateSucceeded) {
		t.Errorf("first state = %q, want succeeded", got.Items[0].State)
	}
	if got.NextCursor != "" {
		t.Errorf("next_cursor = %q, want empty", got.NextCursor)
	}
}

func TestListRuns_RepoFilter(t *testing.T) {
	repo := newFakeRepo()
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	seedRun(repo, "x/y", "w", run.StatePending, t0)
	seedRun(repo, "a/b", "w", run.StatePending, t0)
	s := newServer(t, repo)

	req := httptest.NewRequest(http.MethodGet, "/v0/runs?repo=x/y", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var got struct {
		Items []runResponse `json:"items"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Items) != 1 || got.Items[0].Repo != "x/y" {
		t.Errorf("repo filter broken: %+v", got.Items)
	}
}

func TestListRuns_StateFilter_BadValue(t *testing.T) {
	s := newServer(t, newFakeRepo())
	req := httptest.NewRequest(http.MethodGet, "/v0/runs?state=fake", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"validation_failed"`) {
		t.Errorf("body missing validation_failed: %s", w.Body.String())
	}
}

func TestListRuns_Pagination(t *testing.T) {
	repo := newFakeRepo()
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		seedRun(repo, "x/y", "w", run.StatePending, t0.Add(time.Duration(i)*time.Second))
	}
	s := newServer(t, repo)

	// Page 1: limit=2.
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v0/runs?limit=2", nil))
	var page1 struct {
		Items      []runResponse `json:"items"`
		NextCursor string        `json:"next_cursor"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &page1)
	if len(page1.Items) != 2 {
		t.Errorf("page1 size = %d, want 2", len(page1.Items))
	}
	if page1.NextCursor == "" {
		t.Fatal("page1 next_cursor empty")
	}

	// Follow cursor — page 2.
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v0/runs?limit=2&cursor="+page1.NextCursor, nil))
	var page2 struct {
		Items      []runResponse `json:"items"`
		NextCursor string        `json:"next_cursor"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &page2)
	if len(page2.Items) != 2 {
		t.Errorf("page2 size = %d, want 2", len(page2.Items))
	}
	if page2.NextCursor == "" {
		t.Fatal("page2 next_cursor empty")
	}

	// Page 3 — last item, empty cursor.
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v0/runs?limit=2&cursor="+page2.NextCursor, nil))
	var page3 struct {
		Items      []runResponse `json:"items"`
		NextCursor string        `json:"next_cursor"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &page3)
	if len(page3.Items) != 1 {
		t.Errorf("page3 size = %d, want 1", len(page3.Items))
	}
	if page3.NextCursor != "" {
		t.Errorf("page3 cursor = %q, want empty", page3.NextCursor)
	}
}

func TestListRuns_RepoError(t *testing.T) {
	repo := newFakeRepo()
	repo.listErr = errors.New("db down")
	s := newServer(t, repo)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v0/runs", nil))
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestListRuns_NilRepo(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v0/runs", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestCancelRun_HappyPath(t *testing.T) {
	repo := newFakeRepo()
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	r := seedRun(repo, "x/y", "w", run.StatePending, t0)
	s := newServer(t, repo)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/v0/runs/%s/cancel", r.ID), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	var got runResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.State != string(run.StateCancelled) {
		t.Errorf("State = %q, want cancelled", got.State)
	}
}

func TestCancelRun_Idempotent(t *testing.T) {
	repo := newFakeRepo()
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	r := seedRun(repo, "x/y", "w", run.StateCancelled, t0)
	s := newServer(t, repo)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/v0/runs/%s/cancel", r.ID), nil))
	if w.Code != http.StatusOK {
		t.Errorf("idempotent cancel status = %d, want 200", w.Code)
	}
}

func TestCancelRun_TerminalStateConflict(t *testing.T) {
	repo := newFakeRepo()
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	r := seedRun(repo, "x/y", "w", run.StateSucceeded, t0)
	s := newServer(t, repo)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/v0/runs/%s/cancel", r.ID), nil))
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"invalid_state_transition"`) {
		t.Errorf("body missing invalid_state_transition: %s", w.Body.String())
	}
}

func TestCancelRun_NotFound(t *testing.T) {
	s := newServer(t, newFakeRepo())
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/v0/runs/%s/cancel", uuid.New()), nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestCancelRun_BadUUID(t *testing.T) {
	s := newServer(t, newFakeRepo())
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost,
		"/v0/runs/not-a-uuid/cancel", nil))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestCancelRun_RepoError(t *testing.T) {
	repo := newFakeRepo()
	repo.transitionErr = errors.New("db down")
	s := newServer(t, repo)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/v0/runs/%s/cancel", uuid.New()), nil))
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestCancelRun_NilRepo(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/v0/runs/%s/cancel", uuid.New()), nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
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
