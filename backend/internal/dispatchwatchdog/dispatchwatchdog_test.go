package dispatchwatchdog

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

// -----------------------------------------------------------------
// Fakes
// -----------------------------------------------------------------

type fakeRepo struct {
	mu sync.Mutex

	stages         []*run.Stage
	transitionedTo []*run.Stage
	listErr        error
	transitionErr  error
}

func (f *fakeRepo) ListStagesDispatched(_ context.Context) ([]*run.Stage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]*run.Stage, 0, len(f.stages))
	for _, s := range f.stages {
		if s.State == run.StageStateDispatched {
			out = append(out, s)
		}
	}
	return out, nil
}

func (f *fakeRepo) GetStage(_ context.Context, id uuid.UUID) (*run.Stage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.stages {
		if s.ID == id {
			return s, nil
		}
	}
	return nil, run.ErrNotFound
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

func (f *fakeRepo) RetryStage(context.Context, uuid.UUID, run.StageState) (*run.Stage, error) {
	return nil, errors.New("not used")
}

// Stub out the rest of run.Repository so fakeRepo satisfies the interface.
func (f *fakeRepo) CreateRun(context.Context, run.CreateRunParams) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (f *fakeRepo) GetRun(context.Context, uuid.UUID) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (f *fakeRepo) GetRunByIdempotencyKey(context.Context, string, string) (*run.Run, error) {
	return nil, run.ErrNotFound
}
func (f *fakeRepo) ListRuns(context.Context, run.ListRunsFilter) ([]*run.Run, error) {
	return nil, errors.New("not used")
}
func (f *fakeRepo) TransitionRun(context.Context, uuid.UUID, run.State) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (f *fakeRepo) SetRunPullRequestURL(context.Context, uuid.UUID, string) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (f *fakeRepo) CreateStage(context.Context, run.CreateStageParams) (*run.Stage, error) {
	return nil, errors.New("not used")
}
func (f *fakeRepo) ListStagesForRun(context.Context, uuid.UUID) ([]*run.Stage, error) {
	return nil, errors.New("not used")
}
func (f *fakeRepo) ListStagesAwaitingApproval(context.Context) ([]*run.Stage, error) {
	return nil, nil
}

func (f *fakeRepo) ListStagesAwaitingChildren(context.Context) ([]*run.Stage, error) {
	return nil, nil
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
func (a *fakeAudit) ListAll(context.Context, audit.ListAllParams) ([]*audit.Entry, error) {
	return nil, nil
}
func (a *fakeAudit) LastForRun(context.Context, uuid.UUID) (*audit.Entry, error) {
	return nil, audit.ErrNotFound
}
func (a *fakeAudit) ListForRunByCategory(context.Context, uuid.UUID, string) ([]*audit.Entry, error) {
	return nil, nil
}

// -----------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------

func mkDispatchedStage(updatedAgo time.Duration) *run.Stage {
	return &run.Stage{
		ID:        uuid.New(),
		RunID:     uuid.New(),
		Type:      run.StageTypeImplement,
		State:     run.StageStateDispatched,
		UpdatedAt: time.Now().UTC().Add(-updatedAgo),
	}
}

// -----------------------------------------------------------------
// Tests
// -----------------------------------------------------------------

func TestTicker_RequiresRepoAndAudit(t *testing.T) {
	if err := (&Ticker{Audit: &fakeAudit{}}).Run(context.Background()); err == nil {
		t.Error("missing Repo: Run returned nil error")
	}
	if err := (&Ticker{Repo: &fakeRepo{}}).Run(context.Background()); err == nil {
		t.Error("missing Audit: Run returned nil error")
	}
}

func TestTicker_FailsStuckDispatchedStage(t *testing.T) {
	repo := &fakeRepo{}
	au := &fakeAudit{}

	// Updated 2h ago with a 1h timeout → past deadline.
	s := mkDispatchedStage(2 * time.Hour)
	repo.stages = []*run.Stage{s}

	tick := &Ticker{
		Repo:    repo,
		Audit:   au,
		Timeout: 1 * time.Hour,
		Now:     func() time.Time { return time.Now().UTC() },
	}
	tick.Tick(context.Background())

	// FailStage walks dispatched → running → failed, so the fake
	// records two TransitionStage calls. The second is the one we
	// care about.
	if len(repo.transitionedTo) < 1 {
		t.Fatalf("no transitions recorded")
	}
	got := repo.transitionedTo[len(repo.transitionedTo)-1]
	if got.State != run.StageStateFailed {
		t.Errorf("State = %s, want failed", got.State)
	}
	if got.FailureCategory == nil || *got.FailureCategory != run.FailureC {
		t.Errorf("FailureCategory = %v, want C", got.FailureCategory)
	}
	if got.FailureReason == nil || !strings.Contains(*got.FailureReason, "dispatch_watchdog") {
		t.Errorf("FailureReason = %v", got.FailureReason)
	}

	if len(au.appended) != 1 {
		t.Fatalf("audit appended %d, want 1", len(au.appended))
	}
	if au.appended[0].Category != CategoryDispatchWatchdogElapsed {
		t.Errorf("audit category = %q", au.appended[0].Category)
	}
	var payload map[string]any
	if err := json.Unmarshal(au.appended[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload["failure_category"] != "C" {
		t.Errorf("payload.failure_category = %v, want C", payload["failure_category"])
	}
}

func TestTicker_DoesNotFailWithinTimeout(t *testing.T) {
	repo := &fakeRepo{}
	au := &fakeAudit{}

	// Updated 30m ago with a 1h timeout → still within window.
	s := mkDispatchedStage(30 * time.Minute)
	repo.stages = []*run.Stage{s}

	tick := &Ticker{Repo: repo, Audit: au, Timeout: 1 * time.Hour}
	tick.Tick(context.Background())

	if len(repo.transitionedTo) != 0 {
		t.Errorf("transitions = %d, want 0", len(repo.transitionedTo))
	}
	if len(au.appended) != 0 {
		t.Errorf("audit appended = %d, want 0", len(au.appended))
	}
}

func TestTicker_ZeroTimeoutNeverFires(t *testing.T) {
	repo := &fakeRepo{}
	// Even ancient stages don't transition when Timeout == 0; this
	// is the "watchdog enabled but deadline not yet chosen" mode.
	s := mkDispatchedStage(48 * time.Hour)
	repo.stages = []*run.Stage{s}

	tick := &Ticker{Repo: repo, Audit: &fakeAudit{}, Timeout: 0}
	tick.Tick(context.Background())

	if len(repo.transitionedTo) != 0 {
		t.Errorf("transitions = %d, want 0 (zero-timeout disables firing)", len(repo.transitionedTo))
	}
}

func TestTicker_IgnoresNonDispatchedStages(t *testing.T) {
	repo := &fakeRepo{}
	old := mkDispatchedStage(2 * time.Hour)
	old.State = run.StageStateAwaitingApproval // SLA's territory, not ours
	repo.stages = []*run.Stage{old}

	tick := &Ticker{Repo: repo, Audit: &fakeAudit{}, Timeout: 1 * time.Hour}
	tick.Tick(context.Background())

	if len(repo.transitionedTo) != 0 {
		t.Errorf("transitions = %d, want 0 (awaiting_approval is sla.Ticker's job)", len(repo.transitionedTo))
	}
}

func TestTicker_AuditFailureLeavesStateChanged(t *testing.T) {
	// Sanity check on the failure mode where the transition succeeds
	// but the audit append fails. We log loudly but don't roll back —
	// the stage is in the right terminal state, and re-running the
	// watchdog won't see it again.
	repo := &fakeRepo{}
	au := &fakeAudit{appendErr: errors.New("db down")}

	s := mkDispatchedStage(2 * time.Hour)
	repo.stages = []*run.Stage{s}

	tick := &Ticker{Repo: repo, Audit: au, Timeout: 1 * time.Hour}
	tick.Tick(context.Background())

	// FailStage walks dispatched → running → failed, so we expect
	// at least one transition; the last one should be the terminal
	// failed state.
	if len(repo.transitionedTo) == 0 {
		t.Fatalf("transition should still happen despite audit failure, got 0")
	}
	last := repo.transitionedTo[len(repo.transitionedTo)-1]
	if last.State != run.StageStateFailed {
		t.Errorf("last state = %s, want failed", last.State)
	}
}

func TestTicker_TransitionFailureSkipsAudit(t *testing.T) {
	// Conversely, if the transition fails (e.g. concurrent failure
	// elsewhere already moved the stage to a terminal state), we
	// must NOT append a misleading audit entry.
	repo := &fakeRepo{transitionErr: errors.New("boom")}
	au := &fakeAudit{}

	s := mkDispatchedStage(2 * time.Hour)
	repo.stages = []*run.Stage{s}

	tick := &Ticker{Repo: repo, Audit: au, Timeout: 1 * time.Hour}
	tick.Tick(context.Background())

	if len(au.appended) != 0 {
		t.Errorf("audit appended = %d, want 0 when transition errors", len(au.appended))
	}
}

func TestTicker_RunStopsOnContextCancel(t *testing.T) {
	repo := &fakeRepo{}
	au := &fakeAudit{}

	tick := &Ticker{
		Repo:     repo,
		Audit:    au,
		Interval: 10 * time.Millisecond,
		Timeout:  1 * time.Hour,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- tick.Run(ctx) }()
	time.Sleep(25 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v, want nil on ctx-cancel", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Run didn't return after ctx cancel")
	}
}
