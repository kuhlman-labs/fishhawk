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

// Unused methods on run.Repository — the handler doesn't touch them.
func (r *stagesRunRepo) CreateRun(context.Context, run.CreateRunParams) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *stagesRunRepo) GetRun(context.Context, uuid.UUID) (*run.Run, error) {
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
	out := make([]*audit.Entry, n)
	for i := range out {
		out[i] = &audit.Entry{
			ID:        uuid.New(),
			Sequence:  int64(i + 1),
			RunID:     uuid.MustParse("11111111-2222-3333-4444-555555555555"),
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
