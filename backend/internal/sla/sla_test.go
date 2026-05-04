package sla

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
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"4_hours", 4 * time.Hour, false},
		{"24_hours", 24 * time.Hour, false},
		{"1_hour", time.Hour, false},
		{"4_business_hours", 4 * time.Hour, false}, // v0 alias
		{"30_minutes", 30 * time.Minute, false},
		{"2_days", 48 * time.Hour, false},
		{"", 0, false},            // empty → zero, no error
		{"   ", 0, false},         // whitespace-only → zero, no error
		{"4hours", 0, true},       // missing underscore
		{"abc_hours", 0, true},    // non-numeric
		{"0_hours", 0, true},      // zero
		{"-1_hours", 0, true},     // negative — caught as bad number
		{"5_lightyears", 0, true}, // unknown unit
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := Parse(c.in)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, c.wantErr)
			}
			if got != c.want {
				t.Errorf("Parse(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestParse_UnknownUnitWraps(t *testing.T) {
	_, err := Parse("5_lightyears")
	if !errors.Is(err, ErrUnknownUnit) {
		t.Errorf("err = %v, want ErrUnknownUnit", err)
	}
}

// -----------------------------------------------------------------
// Ticker tests with a fake clock + in-memory repo.
// -----------------------------------------------------------------

type fakeRepo struct {
	mu     sync.Mutex
	stages []*run.Stage

	transitionedTo []*run.Stage
	transitionErr  error
	listErr        error
}

func (f *fakeRepo) ListStagesAwaitingApproval(_ context.Context) ([]*run.Stage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]*run.Stage, 0, len(f.stages))
	for _, s := range f.stages {
		if s.State == run.StageStateAwaitingApproval {
			out = append(out, s)
		}
	}
	return out, nil
}

func (f *fakeRepo) TransitionStage(_ context.Context, id uuid.UUID, to run.StageState, c *run.StageCompletion) (*run.Stage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.transitionErr != nil {
		return nil, f.transitionErr
	}
	for _, s := range f.stages {
		if s.ID == id {
			s.State = to
			if c != nil {
				s.FailureCategory = c.FailureCategory
				s.FailureReason = c.FailureReason
			}
			f.transitionedTo = append(f.transitionedTo, s)
			return s, nil
		}
	}
	return nil, run.ErrNotFound
}

// Stub the rest of run.Repository so fakeRepo satisfies the interface.
func (f *fakeRepo) CreateRun(context.Context, run.CreateRunParams) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (f *fakeRepo) GetRun(context.Context, uuid.UUID) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (f *fakeRepo) ListRuns(context.Context, run.ListRunsFilter) ([]*run.Run, error) {
	return nil, errors.New("not used")
}
func (f *fakeRepo) TransitionRun(context.Context, uuid.UUID, run.State) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (f *fakeRepo) CreateStage(context.Context, run.CreateStageParams) (*run.Stage, error) {
	return nil, errors.New("not used")
}
func (f *fakeRepo) GetStage(context.Context, uuid.UUID) (*run.Stage, error) {
	return nil, errors.New("not used")
}
func (f *fakeRepo) ListStagesForRun(context.Context, uuid.UUID) ([]*run.Stage, error) {
	return nil, errors.New("not used")
}

type fakeAudit struct {
	mu        sync.Mutex
	appended  []audit.ChainAppendParams
	appendErr error
}

func (a *fakeAudit) Append(context.Context, audit.AppendParams) (*audit.Entry, error) {
	return nil, errors.New("not used")
}
func (a *fakeAudit) AppendChained(_ context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.appendErr != nil {
		return nil, a.appendErr
	}
	a.appended = append(a.appended, p)
	rid := p.RunID
	return &audit.Entry{ID: uuid.New(), RunID: &rid}, nil
}
func (a *fakeAudit) AppendGlobalChained(context.Context, audit.GlobalChainAppendParams) (*audit.Entry, error) {
	return nil, errors.New("not used")
}
func (a *fakeAudit) Get(context.Context, uuid.UUID) (*audit.Entry, error) {
	return nil, audit.ErrNotFound
}
func (a *fakeAudit) ListForRun(context.Context, uuid.UUID) ([]*audit.Entry, error) {
	return nil, nil
}
func (a *fakeAudit) ListGlobal(context.Context) ([]*audit.Entry, error) {
	return nil, nil
}
func (a *fakeAudit) LastForRun(context.Context, uuid.UUID) (*audit.Entry, error) {
	return nil, audit.ErrNotFound
}
func (a *fakeAudit) ListForRunByCategory(context.Context, uuid.UUID, string) ([]*audit.Entry, error) {
	return nil, nil
}

func ptrStr(s string) *string { return &s }

func mkStage(updatedAgo time.Duration, sla *string) *run.Stage {
	return &run.Stage{
		ID:        uuid.New(),
		RunID:     uuid.New(),
		Type:      run.StageTypeImplement,
		State:     run.StageStateAwaitingApproval,
		GateSLA:   sla,
		UpdatedAt: time.Now().UTC().Add(-updatedAgo),
	}
}

func TestTicker_TimesOutPastSLA(t *testing.T) {
	repo := &fakeRepo{}
	au := &fakeAudit{}

	// Updated 5h ago with a 4h SLA → past deadline.
	s := mkStage(5*time.Hour, ptrStr("4_hours"))
	repo.stages = []*run.Stage{s}

	ticker := &Ticker{
		Repo:  repo,
		Audit: au,
		Now:   func() time.Time { return time.Now().UTC() },
	}
	ticker.Tick(context.Background())

	if len(repo.transitionedTo) != 1 {
		t.Fatalf("transitions = %d, want 1", len(repo.transitionedTo))
	}
	got := repo.transitionedTo[0]
	if got.State != run.StageStateFailed {
		t.Errorf("State = %s, want failed", got.State)
	}
	if got.FailureCategory == nil || *got.FailureCategory != run.FailureD {
		t.Errorf("FailureCategory = %v, want D", got.FailureCategory)
	}
	if got.FailureReason == nil || !strings.Contains(*got.FailureReason, "sla_timeout") {
		t.Errorf("FailureReason = %v", got.FailureReason)
	}

	if len(au.appended) != 1 {
		t.Fatalf("audit appended %d, want 1", len(au.appended))
	}
	if au.appended[0].Category != CategoryApprovalSLAElapsed {
		t.Errorf("audit category = %q", au.appended[0].Category)
	}
	var payload map[string]any
	if err := json.Unmarshal(au.appended[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload["failure_category"] != "D" {
		t.Errorf("payload.failure_category = %v", payload["failure_category"])
	}
}

func TestTicker_DoesNotTimeOutWithinSLA(t *testing.T) {
	repo := &fakeRepo{}
	au := &fakeAudit{}

	// Updated 1h ago with a 4h SLA → still in the window.
	s := mkStage(1*time.Hour, ptrStr("4_hours"))
	repo.stages = []*run.Stage{s}

	ticker := &Ticker{Repo: repo, Audit: au}
	ticker.Tick(context.Background())

	if len(repo.transitionedTo) != 0 {
		t.Errorf("expected no transitions, got %d", len(repo.transitionedTo))
	}
	if len(au.appended) != 0 {
		t.Errorf("expected no audit entries, got %d", len(au.appended))
	}
}

func TestTicker_NilSLASkipped(t *testing.T) {
	repo := &fakeRepo{}
	au := &fakeAudit{}

	s := mkStage(1*time.Hour, nil) // no SLA
	repo.stages = []*run.Stage{s}

	ticker := &Ticker{Repo: repo, Audit: au}
	ticker.Tick(context.Background())

	if len(repo.transitionedTo) != 0 {
		t.Errorf("nil SLA should skip, got %d transitions", len(repo.transitionedTo))
	}
}

func TestTicker_BadSLALogsButContinues(t *testing.T) {
	repo := &fakeRepo{}
	au := &fakeAudit{}

	bad := mkStage(5*time.Hour, ptrStr("not_a_valid_sla"))
	good := mkStage(5*time.Hour, ptrStr("4_hours"))
	repo.stages = []*run.Stage{bad, good}

	ticker := &Ticker{Repo: repo, Audit: au}
	ticker.Tick(context.Background())

	// Bad row is skipped; good row times out.
	if len(repo.transitionedTo) != 1 {
		t.Fatalf("transitions = %d, want 1 (only the good row)", len(repo.transitionedTo))
	}
	if repo.transitionedTo[0].ID != good.ID {
		t.Errorf("wrong stage transitioned")
	}
}

func TestTicker_TerminalStageNoOp(t *testing.T) {
	repo := &fakeRepo{}
	au := &fakeAudit{}

	s := mkStage(5*time.Hour, ptrStr("4_hours"))
	s.State = run.StageStateSucceeded // ListStagesAwaitingApproval would filter, but defense in depth
	repo.stages = []*run.Stage{s}

	ticker := &Ticker{Repo: repo, Audit: au}
	ticker.Tick(context.Background())

	// Our fakeRepo's ListStagesAwaitingApproval filters out non-awaiting
	// rows, so the ticker never sees this stage. Reproduces the prod
	// invariant: terminal stages are excluded by the SQL.
	if len(repo.transitionedTo) != 0 {
		t.Errorf("terminal stage should be filtered, got %d transitions", len(repo.transitionedTo))
	}
}

func TestTicker_TransitionFailureLogsButContinues(t *testing.T) {
	repo := &fakeRepo{transitionErr: errors.New("db down")}
	au := &fakeAudit{}

	s := mkStage(5*time.Hour, ptrStr("4_hours"))
	repo.stages = []*run.Stage{s}

	ticker := &Ticker{Repo: repo, Audit: au}
	ticker.Tick(context.Background()) // should not panic

	// No audit entry written when transition failed (we only audit
	// after a successful state change).
	if len(au.appended) != 0 {
		t.Errorf("audit should not record a failed-to-transition stage, got %d", len(au.appended))
	}
}

func TestTicker_AuditFailureSurfacesButLeavesStateChanged(t *testing.T) {
	repo := &fakeRepo{}
	au := &fakeAudit{appendErr: errors.New("audit append failed")}

	s := mkStage(5*time.Hour, ptrStr("4_hours"))
	repo.stages = []*run.Stage{s}

	ticker := &Ticker{Repo: repo, Audit: au}
	ticker.Tick(context.Background()) // should not panic

	// Stage was already transitioned even though audit failed —
	// see the comment in handleStage for the rationale.
	if len(repo.transitionedTo) != 1 {
		t.Errorf("transition should have happened despite audit failure, got %d", len(repo.transitionedTo))
	}
}

func TestTicker_ListErrorAborts(t *testing.T) {
	repo := &fakeRepo{listErr: errors.New("db down")}
	au := &fakeAudit{}
	ticker := &Ticker{Repo: repo, Audit: au}
	ticker.Tick(context.Background()) // logs + returns
	if len(au.appended) != 0 || len(repo.transitionedTo) != 0 {
		t.Errorf("list error should short-circuit cleanly")
	}
}

func TestTicker_RunCancellable(t *testing.T) {
	repo := &fakeRepo{}
	au := &fakeAudit{}
	ticker := &Ticker{
		Repo:     repo,
		Audit:    au,
		Interval: 10 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = ticker.Run(ctx)
		close(done)
	}()
	// Let it tick once or twice.
	time.Sleep(25 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestTicker_RunRequiresRepo(t *testing.T) {
	ticker := &Ticker{Audit: &fakeAudit{}}
	err := ticker.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Repo") {
		t.Errorf("err = %v, want Repo error", err)
	}
}

func TestTicker_RunRequiresAudit(t *testing.T) {
	ticker := &Ticker{Repo: &fakeRepo{}}
	err := ticker.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Audit") {
		t.Errorf("err = %v, want Audit error", err)
	}
}

func TestTicker_FixedClockProducesDeterministicDeadline(t *testing.T) {
	repo := &fakeRepo{}
	au := &fakeAudit{}

	// Construct a stage with a known UpdatedAt and a fixed Now.
	updated := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	s := &run.Stage{
		ID:        uuid.New(),
		RunID:     uuid.New(),
		State:     run.StageStateAwaitingApproval,
		GateSLA:   ptrStr("2_hours"),
		UpdatedAt: updated,
	}
	repo.stages = []*run.Stage{s}

	// Now is one second past the 2h deadline.
	ticker := &Ticker{
		Repo:  repo,
		Audit: au,
		Now:   func() time.Time { return updated.Add(2*time.Hour + time.Second) },
	}
	ticker.Tick(context.Background())

	if len(repo.transitionedTo) != 1 {
		t.Fatalf("transitions = %d, want 1", len(repo.transitionedTo))
	}
}
