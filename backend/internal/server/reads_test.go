package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// stagesRunRepo is the read-side fake for the stages handler. It
// surfaces the methods the handler actually calls and panics on
// the others so an accidental call is loud.
type stagesRunRepo struct {
	stages   map[uuid.UUID][]*run.Stage
	listErr  error
	notFound bool
}

func newStagesRunRepo() *stagesRunRepo {
	return &stagesRunRepo{stages: map[uuid.UUID][]*run.Stage{}}
}

func (r *stagesRunRepo) ListStagesForRun(_ context.Context, runID uuid.UUID) ([]*run.Stage, error) {
	if r.listErr != nil {
		return nil, r.listErr
	}
	if r.notFound {
		return nil, run.ErrNotFound
	}
	return r.stages[runID], nil
}

func (r *stagesRunRepo) ListStagesAwaitingApproval(context.Context) ([]*run.Stage, error) {
	return nil, errors.New("not used")
}

func (r *stagesRunRepo) ListStagesDispatched(context.Context) ([]*run.Stage, error) {
	return nil, nil
}

func (r *stagesRunRepo) RetryStage(context.Context, uuid.UUID, run.StageState) (*run.Stage, error) {
	return nil, errors.New("not used")
}

// Unused methods on run.Repository — the handler doesn't touch them.
func (r *stagesRunRepo) CreateRun(context.Context, run.CreateRunParams) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *stagesRunRepo) GetRun(context.Context, uuid.UUID) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *stagesRunRepo) GetRunByIdempotencyKey(context.Context, string, string) (*run.Run, error) {
	return nil, run.ErrNotFound
}
func (r *stagesRunRepo) ListRuns(context.Context, run.ListRunsFilter) ([]*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *stagesRunRepo) TransitionRun(context.Context, uuid.UUID, run.State) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *stagesRunRepo) CreateStage(context.Context, run.CreateStageParams) (*run.Stage, error) {
	return nil, errors.New("not used")
}
func (r *stagesRunRepo) GetStage(context.Context, uuid.UUID) (*run.Stage, error) {
	return nil, errors.New("not used")
}
func (r *stagesRunRepo) TransitionStage(context.Context, uuid.UUID, run.StageState, *run.StageCompletion) (*run.Stage, error) {
	return nil, errors.New("not used")
}

func TestListRunStages_HappyPath(t *testing.T) {
	repo := newStagesRunRepo()
	runID := uuid.New()
	now := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	repo.stages[runID] = []*run.Stage{
		{ID: uuid.New(), RunID: runID, Sequence: 0, Type: run.StageTypePlan,
			ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
			State: run.StageStateSucceeded, CreatedAt: now, UpdatedAt: now},
		{ID: uuid.New(), RunID: runID, Sequence: 1, Type: run.StageTypeImplement,
			ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
			State: run.StageStateRunning, CreatedAt: now, UpdatedAt: now},
	}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo})

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v0/runs/%s/stages", runID), nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	var got struct {
		Items []stageResponse `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 2 {
		t.Fatalf("got %d stages, want 2", len(got.Items))
	}
	if got.Items[0].Sequence != 0 || got.Items[1].Sequence != 1 {
		t.Errorf("sequence order broken: %v", []int{got.Items[0].Sequence, got.Items[1].Sequence})
	}
	if got.Items[0].Executor.Kind != "agent" || got.Items[0].Executor.Ref != "claude-code" {
		t.Errorf("executor mapping wrong: %+v", got.Items[0].Executor)
	}
}

func TestListRunStages_NotFound(t *testing.T) {
	repo := newStagesRunRepo()
	repo.notFound = true
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo})
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/runs/%s/stages", uuid.New()), nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestListRunStages_BadUUID(t *testing.T) {
	repo := newStagesRunRepo()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo})
	req := httptest.NewRequest(http.MethodGet, "/v0/runs/not-a-uuid/stages", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestListRunStages_NilRepo(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/runs/%s/stages", uuid.New()), nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestListRunStages_RepoError(t *testing.T) {
	repo := newStagesRunRepo()
	repo.listErr = errors.New("db down")
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo})
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/runs/%s/stages", uuid.New()), nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

// --- Stage detail handler ---

func TestGetStage_HappyPath(t *testing.T) {
	repo := newStagesRunRepo()
	runID := uuid.New()
	now := time.Date(2026, 5, 3, 10, 0, 0, 0, time.UTC)
	stageID := uuid.New()
	repo.stages[runID] = []*run.Stage{{
		ID: stageID, RunID: runID, Sequence: 0, Type: run.StageTypePlan,
		ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
		State: run.StageStateAwaitingApproval, CreatedAt: now, UpdatedAt: now,
	}}
	// stagesRunRepo's GetStage isn't implemented; extend the
	// fake by direct map lookup. Build a small adapter.
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: &stageGetRepo{stagesRunRepo: repo}})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/stages/%s", stageID), nil)
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var got stageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != stageID {
		t.Errorf("ID = %s", got.ID)
	}
	if got.State != string(run.StageStateAwaitingApproval) {
		t.Errorf("State = %q", got.State)
	}
}

// stageGetRepo extends stagesRunRepo with a working GetStage so the
// handler test can resolve stages.
type stageGetRepo struct {
	*stagesRunRepo
}

func (r *stageGetRepo) GetStage(_ context.Context, id uuid.UUID) (*run.Stage, error) {
	for _, list := range r.stages {
		for _, st := range list {
			if st.ID == id {
				return st, nil
			}
		}
	}
	return nil, run.ErrNotFound
}

func TestGetStage_NotFound(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: &stageGetRepo{stagesRunRepo: newStagesRunRepo()}})
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/stages/%s", uuid.New()), nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestGetStage_BadUUID(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: &stageGetRepo{stagesRunRepo: newStagesRunRepo()}})
	req := httptest.NewRequest(http.MethodGet, "/v0/stages/not-a-uuid", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestGetStage_NilRepo(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/stages/%s", uuid.New()), nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

// --- Artifact handlers ---

// fakeArtifactRepo is the in-memory artifact.Repository for handler
// tests. The other Repository methods (Create, GetByHash) aren't
// touched by the read handlers, so they return "not used" errors.
type fakeArtifactRepo struct {
	mu       sync.Mutex
	all      []*artifact.Artifact
	listErr  error
	getErr   error
	notFound bool
}

func newFakeArtifactRepo() *fakeArtifactRepo {
	return &fakeArtifactRepo{}
}

func (f *fakeArtifactRepo) Create(_ context.Context, _ artifact.CreateParams) (*artifact.Artifact, error) {
	return nil, errors.New("not used")
}

func (f *fakeArtifactRepo) Get(_ context.Context, id uuid.UUID) (*artifact.Artifact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.notFound {
		return nil, artifact.ErrNotFound
	}
	for _, a := range f.all {
		if a.ID == id {
			return a, nil
		}
	}
	return nil, artifact.ErrNotFound
}

func (f *fakeArtifactRepo) ListForStage(_ context.Context, stageID uuid.UUID) ([]*artifact.Artifact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []*artifact.Artifact
	for _, a := range f.all {
		if a.StageID == stageID {
			out = append(out, a)
		}
	}
	return out, nil
}

func (f *fakeArtifactRepo) GetByHash(_ context.Context, _ uuid.UUID, _ string) (*artifact.Artifact, error) {
	return nil, errors.New("not used")
}

func samplePlanArtifact(stageID uuid.UUID) *artifact.Artifact {
	v := "standard_v1"
	return &artifact.Artifact{
		ID:            uuid.New(),
		StageID:       stageID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &v,
		Content:       json.RawMessage(`{"plan_version":"standard_v1"}`),
		ContentHash:   "abc123",
		CreatedAt:     time.Now().UTC(),
	}
}

func TestListStageArtifacts_HappyPath(t *testing.T) {
	repo := newFakeArtifactRepo()
	stageID := uuid.New()
	repo.all = []*artifact.Artifact{samplePlanArtifact(stageID), samplePlanArtifact(stageID)}
	s := New(Config{Addr: "127.0.0.1:0", ArtifactRepo: repo})

	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/stages/%s/artifacts", stageID), nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var got struct {
		Items []artifactResponse `json:"items"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Items) != 2 {
		t.Errorf("items = %d, want 2", len(got.Items))
	}
}

func TestListStageArtifacts_EmptyForUnknownStage(t *testing.T) {
	// We don't 404 — empty list is the honest answer (stage might
	// exist but have produced nothing yet).
	repo := newFakeArtifactRepo()
	s := New(Config{Addr: "127.0.0.1:0", ArtifactRepo: repo})
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/stages/%s/artifacts", uuid.New()), nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

func TestListStageArtifacts_BadUUID(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0", ArtifactRepo: newFakeArtifactRepo()})
	req := httptest.NewRequest(http.MethodGet, "/v0/stages/not-a-uuid/artifacts", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestListStageArtifacts_NilRepo(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/stages/%s/artifacts", uuid.New()), nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestListStageArtifacts_RepoError(t *testing.T) {
	repo := newFakeArtifactRepo()
	repo.listErr = errors.New("db down")
	s := New(Config{Addr: "127.0.0.1:0", ArtifactRepo: repo})
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/stages/%s/artifacts", uuid.New()), nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestGetArtifact_HappyPath(t *testing.T) {
	repo := newFakeArtifactRepo()
	a := samplePlanArtifact(uuid.New())
	repo.all = []*artifact.Artifact{a}
	s := New(Config{Addr: "127.0.0.1:0", ArtifactRepo: repo})

	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/artifacts/%s", a.ID), nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	var got artifactResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != a.ID {
		t.Errorf("ID = %s", got.ID)
	}
	if got.Kind != string(artifact.KindPlan) {
		t.Errorf("Kind = %q", got.Kind)
	}
	if got.SchemaVersion == nil || *got.SchemaVersion != "standard_v1" {
		t.Errorf("SchemaVersion = %v", got.SchemaVersion)
	}
	if !strings.Contains(string(got.Content), "standard_v1") {
		t.Errorf("Content not preserved: %s", got.Content)
	}
}

func TestGetArtifact_NotFound(t *testing.T) {
	repo := newFakeArtifactRepo()
	repo.notFound = true
	s := New(Config{Addr: "127.0.0.1:0", ArtifactRepo: repo})
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/artifacts/%s", uuid.New()), nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"artifact_not_found"`) {
		t.Errorf("body missing artifact_not_found: %s", w.Body.String())
	}
}

func TestGetArtifact_BadUUID(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0", ArtifactRepo: newFakeArtifactRepo()})
	req := httptest.NewRequest(http.MethodGet, "/v0/artifacts/not-a-uuid", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestGetArtifact_NilRepo(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/artifacts/%s", uuid.New()), nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestGetArtifact_RepoError(t *testing.T) {
	repo := newFakeArtifactRepo()
	repo.getErr = errors.New("db down")
	s := New(Config{Addr: "127.0.0.1:0", ArtifactRepo: repo})
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/artifacts/%s", uuid.New()), nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

// auditReadFake is the read-side audit.Repository fake.
type auditReadFake struct {
	mu      sync.Mutex
	all     []*audit.Entry
	byCat   map[string][]*audit.Entry
	listErr error
}

func newAuditReadFake() *auditReadFake {
	return &auditReadFake{byCat: map[string][]*audit.Entry{}}
}

func (a *auditReadFake) Append(context.Context, audit.AppendParams) (*audit.Entry, error) {
	return nil, errors.New("not used")
}
func (a *auditReadFake) AppendChained(context.Context, audit.ChainAppendParams) (*audit.Entry, error) {
	return nil, errors.New("not used")
}
func (a *auditReadFake) AppendGlobalChained(context.Context, audit.GlobalChainAppendParams) (*audit.Entry, error) {
	return nil, errors.New("not used")
}
func (a *auditReadFake) ListGlobal(context.Context) ([]*audit.Entry, error) {
	return nil, errors.New("not used")
}
func (a *auditReadFake) Get(context.Context, uuid.UUID) (*audit.Entry, error) {
	return nil, errors.New("not used")
}
func (a *auditReadFake) ListForRun(_ context.Context, _ uuid.UUID) ([]*audit.Entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.listErr != nil {
		return nil, a.listErr
	}
	return a.all, nil
}
func (a *auditReadFake) LastForRun(context.Context, uuid.UUID) (*audit.Entry, error) {
	return nil, errors.New("not used")
}
func (a *auditReadFake) ListForRunByCategory(_ context.Context, _ uuid.UUID, cat string) ([]*audit.Entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.listErr != nil {
		return nil, a.listErr
	}
	return a.byCat[cat], nil
}

func makeAuditEntries(n int) []*audit.Entry {
	rid := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	out := make([]*audit.Entry, n)
	for i := range out {
		out[i] = &audit.Entry{
			ID:        uuid.New(),
			Sequence:  int64(i + 1),
			RunID:     &rid,
			Timestamp: time.Date(2026, 5, 2, 12, 0, i, 0, time.UTC),
			Category:  "trace_uploaded",
			Payload:   json.RawMessage(`{}`),
			EntryHash: fmt.Sprintf("hash-%d", i),
		}
	}
	return out
}

func TestListRunAudit_HappyPath(t *testing.T) {
	a := newAuditReadFake()
	a.all = makeAuditEntries(3)
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: a})

	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/runs/%s/audit", uuid.New()), nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	var got struct {
		Items      []auditEntryResponse `json:"items"`
		NextCursor string               `json:"next_cursor"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 3 {
		t.Errorf("items = %d, want 3", len(got.Items))
	}
	if got.NextCursor != "" {
		t.Errorf("next_cursor = %q, want empty (all in one page)", got.NextCursor)
	}
}

func TestListRunAudit_PaginationCursor(t *testing.T) {
	a := newAuditReadFake()
	a.all = makeAuditEntries(5)
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: a})

	// First page: limit=2 → entries 1,2; next_cursor non-empty.
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/runs/%s/audit?limit=2", uuid.New()), nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var page1 struct {
		Items      []auditEntryResponse `json:"items"`
		NextCursor string               `json:"next_cursor"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &page1)
	if len(page1.Items) != 2 {
		t.Errorf("page1 size = %d, want 2", len(page1.Items))
	}
	if page1.NextCursor == "" {
		t.Fatal("page1 next_cursor empty; expected a cursor")
	}

	// Follow the cursor → entries 3,4; still has more.
	req = httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/runs/%s/audit?limit=2&cursor=%s", uuid.New(), page1.NextCursor), nil)
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	var page2 struct {
		Items      []auditEntryResponse `json:"items"`
		NextCursor string               `json:"next_cursor"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &page2)
	if len(page2.Items) != 2 {
		t.Errorf("page2 size = %d, want 2", len(page2.Items))
	}
	if page2.NextCursor == "" {
		t.Fatal("page2 next_cursor empty; expected a cursor")
	}
	if page2.Items[0].Sequence != 3 {
		t.Errorf("page2 first sequence = %d, want 3", page2.Items[0].Sequence)
	}

	// Last page: 1 item, empty next_cursor.
	req = httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/runs/%s/audit?limit=2&cursor=%s", uuid.New(), page2.NextCursor), nil)
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	var page3 struct {
		Items      []auditEntryResponse `json:"items"`
		NextCursor string               `json:"next_cursor"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &page3)
	if len(page3.Items) != 1 {
		t.Errorf("page3 size = %d, want 1", len(page3.Items))
	}
	if page3.NextCursor != "" {
		t.Errorf("page3 next_cursor = %q, want empty (end of stream)", page3.NextCursor)
	}
}

func TestListRunAudit_CategoryFilter(t *testing.T) {
	a := newAuditReadFake()
	a.byCat["plan_generated"] = makeAuditEntries(2)
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: a})

	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/runs/%s/audit?category=plan_generated", uuid.New()), nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var got struct {
		Items []auditEntryResponse `json:"items"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Items) != 2 {
		t.Errorf("items = %d, want 2", len(got.Items))
	}
}

func TestListRunAudit_BadLimit(t *testing.T) {
	a := newAuditReadFake()
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: a})
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/runs/%s/audit?limit=999999", uuid.New()), nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestListRunAudit_BadCursor(t *testing.T) {
	a := newAuditReadFake()
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: a})
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/runs/%s/audit?cursor=not-base64!!", uuid.New()), nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"cursor_invalid"`) {
		t.Errorf("body missing cursor_invalid: %s", w.Body.String())
	}
}

func TestListRunAudit_NilRepo(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/runs/%s/audit", uuid.New()), nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestListRunAudit_RepoError(t *testing.T) {
	a := newAuditReadFake()
	a.listErr = errors.New("db down")
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: a})
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/runs/%s/audit", uuid.New()), nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestListRunAudit_BadUUID(t *testing.T) {
	a := newAuditReadFake()
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: a})
	req := httptest.NewRequest(http.MethodGet, "/v0/runs/not-a-uuid/audit", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestEncodeDecodeOffsetCursor(t *testing.T) {
	for _, n := range []int{0, 1, 100, 99999} {
		c := encodeOffsetCursor(n)
		got, err := decodeOffsetCursor(c)
		if err != nil {
			t.Errorf("decode(%q): %v", c, err)
		}
		if got != n {
			t.Errorf("round-trip: %d → %q → %d", n, c, got)
		}
	}
}

func TestPageOffset_OutOfRange(t *testing.T) {
	items := []int{1, 2, 3}
	page, next := pageOffset(items, 10, 5)
	if page != nil || next != "" {
		t.Errorf("got (%v, %q), want (nil, '')", page, next)
	}
}

func TestParseLimit(t *testing.T) {
	cases := []struct {
		raw     string
		want    int
		wantErr bool
	}{
		{"", 100, false},
		{"50", 50, false},
		{"500", 500, false},
		{"0", 0, true},
		{"-1", 0, true},
		{"501", 0, true},
		{"abc", 0, true},
	}
	for _, c := range cases {
		got, err := parseLimit(c.raw, 100, 500)
		if (err != nil) != c.wantErr {
			t.Errorf("parseLimit(%q): err = %v, wantErr %v", c.raw, err, c.wantErr)
		}
		if !c.wantErr && got != c.want {
			t.Errorf("parseLimit(%q) = %d, want %d", c.raw, got, c.want)
		}
	}
}
