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

	// stagesByRun tracks CreateStage calls so #411 tests can assert
	// the API runs handler actually persists stages from the spec.
	// Ordered by spec sequence (the slice index).
	stagesByRun map[uuid.UUID][]*run.Stage

	// createStageErr lets a test fail the create-stage path so the
	// rollback / 500 surface is reachable.
	createStageErr error

	// createErr / getErr / transitionErr / listErr let tests inject
	// failures without instrumenting fakeRepo's internals.
	createErr     error
	getErr        error
	transitionErr error
	listErr       error

	// lastListFilter captures the most-recent ListRuns filter for
	// tests that need to assert filter-forwarding (runner_kind,
	// pull_request_url, etc.).
	lastListFilter run.ListRunsFilter

	// lastCreateRunParams captures the most-recent CreateRun call so
	// tests that need to assert workflow-spec persistence
	// (WorkflowSpec bytes, MaxRetriesSnapshot) can inspect the
	// params the handler built.
	lastCreateRunParams run.CreateRunParams
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		runs:        map[uuid.UUID]*run.Run{},
		stagesByRun: map[uuid.UUID][]*run.Stage{},
	}
}

func (f *fakeRepo) CreateRun(_ context.Context, p run.CreateRunParams) (*run.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastCreateRunParams = p
	if f.createErr != nil {
		return nil, f.createErr
	}
	now := time.Now().UTC()
	// Mirror the postgres adapter's empty → github_actions default
	// so the fake's behavior tracks production.
	runnerKind := p.RunnerKind
	if runnerKind == "" {
		runnerKind = run.RunnerKindGitHubActions
	}
	r := &run.Run{
		ID:                 uuid.New(),
		Repo:               p.Repo,
		WorkflowID:         p.WorkflowID,
		WorkflowSHA:        p.WorkflowSHA,
		TriggerSource:      p.TriggerSource,
		TriggerRef:         p.TriggerRef,
		IdempotencyKey:     p.IdempotencyKey,
		RunnerKind:         runnerKind,
		WorkflowSpec:       p.WorkflowSpec,
		MaxRetriesSnapshot: p.MaxRetriesSnapshot,
		State:              run.StatePending,
		CreatedAt:          now,
		UpdatedAt:          now,
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
	f.lastListFilter = fil
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
		if fil.PullRequestURL != nil {
			if r.PullRequestURL == nil || *r.PullRequestURL != *fil.PullRequestURL {
				continue
			}
		}
		if fil.TriggerRef != nil {
			if r.TriggerRef == nil || *r.TriggerRef != *fil.TriggerRef {
				continue
			}
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
func (f *fakeRepo) SetRunPullRequestURL(_ context.Context, id uuid.UUID, url string) (*run.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.runs[id]
	if !ok {
		return nil, run.ErrNotFound
	}
	u := url
	r.PullRequestURL = &u
	r.UpdatedAt = time.Now().UTC()
	return r, nil
}

func (f *fakeRepo) CreateStage(_ context.Context, p run.CreateStageParams) (*run.Stage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createStageErr != nil {
		return nil, f.createStageErr
	}
	now := time.Now().UTC()
	stage := &run.Stage{
		ID:               uuid.New(),
		RunID:            p.RunID,
		Sequence:         p.Sequence,
		Type:             p.Type,
		ExecutorKind:     p.ExecutorKind,
		ExecutorRef:      p.ExecutorRef,
		State:            run.StageStatePending,
		GateSLA:          p.GateSLA,
		RequiresApproval: p.RequiresApproval,
		Gate:             p.Gate,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	f.stagesByRun[p.RunID] = append(f.stagesByRun[p.RunID], stage)
	return stage, nil
}

// stagesFor returns the stage slice the handler created for the
// given run, in spec order. Tests use this to assert one Stage row
// per stage definition lands when workflow_spec is present.
func (f *fakeRepo) stagesFor(runID uuid.UUID) []*run.Stage {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*run.Stage, len(f.stagesByRun[runID]))
	copy(out, f.stagesByRun[runID])
	return out
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

func TestListRuns_PullRequestURLFilter(t *testing.T) {
	// Threaded-runs view (#216) filters by pull_request_url to find
	// every run on a PR.
	repo := newFakeRepo()
	t0 := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	target := "https://github.com/x/y/pull/42"
	r1 := seedRun(repo, "x/y", "w", run.StateRunning, t0)
	r1.PullRequestURL = strPtr(target)
	r2 := seedRun(repo, "x/y", "w", run.StateSucceeded, t0.Add(time.Minute))
	r2.PullRequestURL = strPtr(target)
	other := seedRun(repo, "x/y", "w", run.StateRunning, t0.Add(2*time.Minute))
	other.PullRequestURL = strPtr("https://github.com/x/y/pull/99")

	s := newServer(t, repo)

	req := httptest.NewRequest(http.MethodGet, "/v0/runs?pull_request_url="+target, nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var got struct {
		Items []runResponse `json:"items"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Items) != 2 {
		t.Fatalf("filter returned %d items, want 2; items=%+v", len(got.Items), got.Items)
	}
	for _, it := range got.Items {
		if it.PullRequestURL == nil || *it.PullRequestURL != target {
			t.Errorf("filtered row has PullRequestURL = %v, want %s", it.PullRequestURL, target)
		}
	}
}

func TestListRuns_TriggerRefFilter(t *testing.T) {
	// Threaded-runs view (#216) also filters by trigger_ref so the
	// dispatcher's parent-finder + the SPA's "all runs on this
	// issue" view share the same query path.
	repo := newFakeRepo()
	t0 := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	r1 := seedRun(repo, "x/y", "w", run.StateRunning, t0)
	r1.TriggerRef = strPtr("issue:42")
	r2 := seedRun(repo, "x/y", "w", run.StateSucceeded, t0.Add(time.Minute))
	r2.TriggerRef = strPtr("issue:99")

	s := newServer(t, repo)
	req := httptest.NewRequest(http.MethodGet, "/v0/runs?trigger_ref=issue:42", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var got struct {
		Items []runResponse `json:"items"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Items) != 1 {
		t.Fatalf("filter returned %d items, want 1; items=%+v", len(got.Items), got.Items)
	}
	if got.Items[0].TriggerRef == nil || *got.Items[0].TriggerRef != "issue:42" {
		t.Errorf("filtered row TriggerRef = %v", got.Items[0].TriggerRef)
	}
}

func strPtr(s string) *string { return &s }

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

// --- runner_kind (E22.7 / #404) ---

func TestCreateRun_RunnerKind_DefaultsGitHubActions(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)
	body := `{
		"repo": "x/y",
		"workflow_id": "feature_change",
		"workflow_sha": "abc",
		"trigger_source": "cli"
	}`
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d", w.Code)
	}
	var got runResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.RunnerKind != run.RunnerKindGitHubActions {
		t.Errorf("RunnerKind = %q, want github_actions", got.RunnerKind)
	}
}

func TestCreateRun_RunnerKind_AcceptsLocal(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)
	body := `{
		"repo": "x/y",
		"workflow_id": "feature_change",
		"workflow_sha": "abc",
		"trigger_source": "cli",
		"runner_kind": "local"
	}`
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	var got runResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.RunnerKind != run.RunnerKindLocal {
		t.Errorf("RunnerKind = %q, want local", got.RunnerKind)
	}
}

func TestCreateRun_RunnerKind_RejectsUnknown(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)
	body := `{
		"repo": "x/y",
		"workflow_id": "feature_change",
		"workflow_sha": "abc",
		"trigger_source": "cli",
		"runner_kind": "k8s"
	}`
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "runner_kind") {
		t.Errorf("body should reference runner_kind: %s", w.Body.String())
	}
}

func TestListRuns_RunnerKindFilter_Forwards(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)
	req := httptest.NewRequest(http.MethodGet, "/v0/runs?runner_kind=github_actions", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	if repo.lastListFilter.RunnerKind == nil {
		t.Fatal("RunnerKind filter not forwarded to repo")
	}
	if *repo.lastListFilter.RunnerKind != run.RunnerKindGitHubActions {
		t.Errorf("RunnerKind filter = %q, want github_actions", *repo.lastListFilter.RunnerKind)
	}
}

func TestListRuns_RunnerKindFilter_RejectsUnknown(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)
	req := httptest.NewRequest(http.MethodGet, "/v0/runs?runner_kind=k8s", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// minimalSpecYAML is the smallest valid workflow spec for the
// workflow_spec tests below: one workflow with one implement stage,
// no gates. Mirrors backend/internal/spec/testdata/valid/minimal.yaml.
const minimalSpecYAML = `version: "0.3"
workflows:
  trivial:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`

// gatedSpecYAML carries a plan stage with an approval gate so tests
// can assert that gate metadata (sla, requires_approval) lands on
// the corresponding Stage row.
const gatedSpecYAML = `version: "0.3"
roles:
  tech_lead:
    members: ["@org/tech-leads"]
workflows:
  feature_change:
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
        produces:
          - artifact: pull_request
`

// TestCreateRun_WorkflowSpec_CreatesStages is the headline #411
// behavior: posting a workflow_spec inline lands one Stage row per
// stage in the spec, in spec order, with the right type +
// executor.
func TestCreateRun_WorkflowSpec_CreatesStages(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)

	body, _ := json.Marshal(map[string]any{
		"repo":           "x/y",
		"workflow_id":    "feature_change",
		"workflow_sha":   "abc",
		"trigger_source": "cli",
		"runner_kind":    "local",
		"workflow_spec":  gatedSpecYAML,
	})
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	var got runResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	stages := repo.stagesFor(got.ID)
	if len(stages) != 2 {
		t.Fatalf("len(stages) = %d, want 2: %#v", len(stages), stages)
	}
	if stages[0].Type != run.StageTypePlan {
		t.Errorf("stage[0].Type = %q, want plan", stages[0].Type)
	}
	if stages[1].Type != run.StageTypeImplement {
		t.Errorf("stage[1].Type = %q, want implement", stages[1].Type)
	}
	// Plan stage carries an approval gate → RequiresApproval true,
	// GateSLA populated from the spec verbatim.
	if !stages[0].RequiresApproval {
		t.Error("stage[0].RequiresApproval = false, want true (plan has approval gate)")
	}
	if stages[0].GateSLA == nil || *stages[0].GateSLA != "4_business_hours" {
		t.Errorf("stage[0].GateSLA = %v, want 4_business_hours", stages[0].GateSLA)
	}
	// Implement has no gate → RequiresApproval false, GateSLA nil.
	if stages[1].RequiresApproval {
		t.Error("stage[1].RequiresApproval = true, want false")
	}
	if stages[1].GateSLA != nil {
		t.Errorf("stage[1].GateSLA = %v, want nil", stages[1].GateSLA)
	}
}

// TestCreateRun_WorkflowSpec_PersistsBytesAndMaxRetries asserts the
// spec bytes are cached on the run row (so the trace handler's
// policy re-evaluation can read constraints without refetching)
// and that MaxRetriesSnapshot is populated from the parsed spec.
// Matches the dispatcher's cache behavior (#280, #283).
func TestCreateRun_WorkflowSpec_PersistsBytesAndMaxRetries(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)

	body, _ := json.Marshal(map[string]any{
		"repo":           "x/y",
		"workflow_id":    "trivial",
		"workflow_sha":   "abc",
		"trigger_source": "cli",
		"workflow_spec":  minimalSpecYAML,
	})
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	if string(repo.lastCreateRunParams.WorkflowSpec) != minimalSpecYAML {
		t.Errorf("WorkflowSpec bytes not cached on row; got len=%d, want len=%d",
			len(repo.lastCreateRunParams.WorkflowSpec), len(minimalSpecYAML))
	}
	// The minimal spec has no on_ci_failure → default applies. The
	// spec package's DefaultMaxRetries is exposed via
	// webhook.WorkflowMaxRetries which the handler calls; assert
	// it's a non-zero default rather than coupling the test to the
	// exact constant.
	if repo.lastCreateRunParams.MaxRetriesSnapshot == 0 {
		t.Error("MaxRetriesSnapshot = 0, want default-from-spec")
	}
}

// TestCreateRun_WorkflowSpec_MalformedYAML rejects the request
// with 400 before any DB write — the run row should NOT exist after
// a parse failure.
func TestCreateRun_WorkflowSpec_MalformedYAML(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)

	body, _ := json.Marshal(map[string]any{
		"repo":           "x/y",
		"workflow_id":    "trivial",
		"workflow_sha":   "abc",
		"trigger_source": "cli",
		"workflow_spec":  "this: is: not: valid: yaml: ::",
	})
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "workflow_spec") {
		t.Errorf("body should mention workflow_spec: %s", w.Body.String())
	}
	if len(repo.runs) != 0 {
		t.Errorf("expected zero runs created, got %d", len(repo.runs))
	}
}

// TestCreateRun_WorkflowSpec_UnknownWorkflowID rejects when the
// requested workflow_id isn't defined in the supplied spec — same
// 400 the dispatcher would emit on the GHA path.
func TestCreateRun_WorkflowSpec_UnknownWorkflowID(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)

	body, _ := json.Marshal(map[string]any{
		"repo":           "x/y",
		"workflow_id":    "does_not_exist",
		"workflow_sha":   "abc",
		"trigger_source": "cli",
		"workflow_spec":  minimalSpecYAML,
	})
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "workflow_id") {
		t.Errorf("body should reference workflow_id: %s", w.Body.String())
	}
	if len(repo.runs) != 0 {
		t.Errorf("expected zero runs created, got %d", len(repo.runs))
	}
}

// TestCreateRun_NoWorkflowSpec_LegacyPath documents the legacy
// shape: when workflow_spec is absent, the handler creates a run
// row with no stages (the pre-#411 behavior). Kept so integration
// tests and existing scripts that POST without a spec keep
// working.
func TestCreateRun_NoWorkflowSpec_LegacyPath(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)

	body := `{
		"repo": "x/y",
		"workflow_id": "trivial",
		"workflow_sha": "abc",
		"trigger_source": "cli"
	}`
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	var got runResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if len(repo.stagesFor(got.ID)) != 0 {
		t.Errorf("legacy path should not create stages; got %d", len(repo.stagesFor(got.ID)))
	}
	if len(repo.lastCreateRunParams.WorkflowSpec) != 0 {
		t.Error("legacy path should NOT cache workflow spec bytes on the row")
	}
}

// TestCreateRun_WorkflowSpec_StageCreateFails_Returns500 covers
// the unhappy persistence path: parse + spec validation pass, the
// run row inserts, then CreateStage errors. Server returns 500 and
// the run row is left behind (orphan) — the dispatcher's behavior
// on the same failure shape. v0.x can wrap this in a transaction
// once we have a use case that demands strict atomicity.
func TestCreateRun_WorkflowSpec_StageCreateFails_Returns500(t *testing.T) {
	repo := newFakeRepo()
	repo.createStageErr = errors.New("disk full")
	s := newServer(t, repo)

	body, _ := json.Marshal(map[string]any{
		"repo":           "x/y",
		"workflow_id":    "trivial",
		"workflow_sha":   "abc",
		"trigger_source": "cli",
		"workflow_spec":  minimalSpecYAML,
	})
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "create stages failed") {
		t.Errorf("body missing diagnostic: %s", w.Body.String())
	}
}
