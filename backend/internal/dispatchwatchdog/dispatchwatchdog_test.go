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
	liveness       []run.DispatchedStageLiveness
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

// ListDispatchedStageLiveness satisfies run.DispatchLivenessLister — the signal
// the ticker actually consumes (#2744).
func (f *fakeRepo) ListDispatchedStageLiveness(_ context.Context) ([]run.DispatchedStageLiveness, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]run.DispatchedStageLiveness, len(f.liveness))
	copy(out, f.liveness)
	return out, nil
}

// seed registers a liveness row AND a matching dispatched stage so FailStage's
// GetStage/TransitionStage walk finds a row to fail.
func (f *fakeRepo) seed(l run.DispatchedStageLiveness) {
	f.liveness = append(f.liveness, l)
	f.stages = append(f.stages, &run.Stage{
		ID:    l.StageID,
		RunID: l.RunID,
		Type:  run.StageTypeImplement,
		State: run.StageStateDispatched,
	})
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
func (f *fakeRepo) RetryRun(context.Context, uuid.UUID, run.State) (*run.Run, error) {
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
func (f *fakeRepo) ListReviewStagesAwaitingApproval(context.Context) ([]*run.Stage, error) {
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

func (a *fakeAudit) ChainsByParent(_ context.Context, _ uuid.UUID, _ bool) ([]*audit.Entry, error) {
	return nil, nil
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
func (a *fakeAudit) ListGlobalByAccount(context.Context, *uuid.UUID) ([]*audit.Entry, error) {
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

// mkLiveness builds one DispatchedStageLiveness. dispatchedAgo sets the
// dedicated dispatch clock; updatedAgo sets the generic updated_at (which a
// heartbeat bumps independently); heartbeatAgo, when non-nil, sets a
// last-heartbeat time (the wedged_after_checkin signal). A nil heartbeatAgo
// leaves LastHeartbeatAt nil (never_checked_in).
func mkLiveness(dispatchedAgo, updatedAgo time.Duration, heartbeatAgo *time.Duration) run.DispatchedStageLiveness {
	now := time.Now().UTC()
	d := now.Add(-dispatchedAgo)
	l := run.DispatchedStageLiveness{
		StageID:      uuid.New(),
		RunID:        uuid.New(),
		DispatchedAt: &d,
		UpdatedAt:    now.Add(-updatedAgo),
	}
	if heartbeatAgo != nil {
		hb := now.Add(-*heartbeatAgo)
		l.LastHeartbeatAt = &hb
	}
	return l
}

func dur(d time.Duration) *time.Duration { return &d }

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

// M1: Run refuses (returns the named error, transitions nothing) when neither an
// explicit Liveness nor a capability-carrying Repo is wired. run.BaseFake is a
// full run.Repository that deliberately does NOT implement
// run.DispatchLivenessLister, so it stands in for a non-Postgres repo.
func TestRun_RefusesRepoWithoutLivenessCapability(t *testing.T) {
	tick := &Ticker{Repo: run.BaseFake{}, Audit: &fakeAudit{}, Timeout: 1 * time.Hour}
	err := tick.Run(context.Background())
	if err == nil {
		t.Fatal("Run returned nil; want a fail-closed capability error")
	}
	if !strings.Contains(err.Error(), "DispatchLivenessLister") {
		t.Errorf("Run error = %v, want it to name the missing DispatchLivenessLister capability", err)
	}
}

// M4: a wedged-but-heartbeating stage past the DISPATCH deadline IS failed, and
// both the reason and the audit payload carry mode=wedged_after_checkin plus the
// dispatched_at / last_heartbeat_at keys. Dispatched 2h ago (past the 1h
// deadline) but a heartbeat 15s ago keeps updated_at fresh — the exact signal a
// heartbeat used to forge. This is the primary counterfactual for the
// DispatchedAt-preferring deadline base (approach step 12a).
func TestTick_WedgedAfterCheckinModeRecorded(t *testing.T) {
	repo := &fakeRepo{}
	au := &fakeAudit{}
	l := mkLiveness(2*time.Hour, 15*time.Second, dur(15*time.Second))
	repo.seed(l)

	tick := &Ticker{Repo: repo, Audit: au, Timeout: 1 * time.Hour}
	tick.Tick(context.Background())

	if len(repo.transitionedTo) < 1 {
		t.Fatalf("no transitions recorded; a wedged-but-heartbeating stage past the dispatch deadline must fail")
	}
	got := repo.transitionedTo[len(repo.transitionedTo)-1]
	if got.State != run.StageStateFailed {
		t.Errorf("State = %s, want failed", got.State)
	}
	if got.FailureCategory == nil || *got.FailureCategory != run.FailureC {
		t.Errorf("FailureCategory = %v, want C", got.FailureCategory)
	}
	if got.FailureReason == nil || !strings.Contains(*got.FailureReason, "last heartbeat") {
		t.Errorf("FailureReason = %v, want it to name the last heartbeat (wedged_after_checkin)", got.FailureReason)
	}
	if len(au.appended) != 1 {
		t.Fatalf("audit appended %d, want 1", len(au.appended))
	}
	var payload map[string]any
	if err := json.Unmarshal(au.appended[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload["mode"] != "wedged_after_checkin" {
		t.Errorf("payload.mode = %v, want wedged_after_checkin", payload["mode"])
	}
	if payload["dispatched_at"] == nil {
		t.Errorf("payload.dispatched_at missing; want the dispatch timestamp")
	}
	if payload["last_heartbeat_at"] == nil {
		t.Errorf("payload.last_heartbeat_at missing; want the heartbeat timestamp")
	}
	if payload["failure_category"] != "C" {
		t.Errorf("payload.failure_category = %v, want C", payload["failure_category"])
	}
}

// M3: no heartbeat ever arrived → failed with mode=never_checked_in in BOTH the
// reason string and the audit payload, and last_heartbeat_at is null.
func TestTick_NeverCheckedInModeRecorded(t *testing.T) {
	repo := &fakeRepo{}
	au := &fakeAudit{}
	l := mkLiveness(2*time.Hour, 2*time.Hour, nil) // never checked in
	repo.seed(l)

	tick := &Ticker{Repo: repo, Audit: au, Timeout: 1 * time.Hour}
	tick.Tick(context.Background())

	if len(repo.transitionedTo) < 1 {
		t.Fatalf("no transitions recorded; a never-checked-in stage past the deadline must fail")
	}
	got := repo.transitionedTo[len(repo.transitionedTo)-1]
	if got.FailureReason == nil || !strings.Contains(*got.FailureReason, "no runner check-in") {
		t.Errorf("FailureReason = %v, want it to name the missing check-in (never_checked_in)", got.FailureReason)
	}
	if len(au.appended) != 1 {
		t.Fatalf("audit appended %d, want 1", len(au.appended))
	}
	var payload map[string]any
	if err := json.Unmarshal(au.appended[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload["mode"] != "never_checked_in" {
		t.Errorf("payload.mode = %v, want never_checked_in", payload["mode"])
	}
	if payload["last_heartbeat_at"] != nil {
		t.Errorf("payload.last_heartbeat_at = %v, want null for never_checked_in", payload["last_heartbeat_at"])
	}
}

// M5: a healthy stage still within its DISPATCH budget is NOT failed, even with
// fresh heartbeats. This is the false-positive guard that made the pre-#2744
// behaviour tolerable: a long implement pass that heartbeats must not trip.
func TestTick_HealthyStageWithinBudgetNotFailed(t *testing.T) {
	repo := &fakeRepo{}
	au := &fakeAudit{}
	// Dispatched 30m ago, heartbeat 5s ago, 1h budget → within window.
	l := mkLiveness(30*time.Minute, 5*time.Second, dur(5*time.Second))
	repo.seed(l)

	tick := &Ticker{Repo: repo, Audit: au, Timeout: 1 * time.Hour}
	tick.Tick(context.Background())

	if len(repo.transitionedTo) != 0 {
		t.Errorf("transitions = %d, want 0 (healthy long-running stage must not be failed)", len(repo.transitionedTo))
	}
	if len(au.appended) != 0 {
		t.Errorf("audit appended = %d, want 0", len(au.appended))
	}
}

// M2: a nil DispatchedAt (a legacy row that escaped the 0072 backfill) degrades
// to the updated_at fallback and still fails past the deadline.
func TestTick_NilDispatchedAtFallsBackToUpdatedAt(t *testing.T) {
	repo := &fakeRepo{}
	au := &fakeAudit{}
	l := mkLiveness(0, 2*time.Hour, nil)
	l.DispatchedAt = nil // legacy row: no dedicated clock
	repo.seed(l)

	tick := &Ticker{Repo: repo, Audit: au, Timeout: 1 * time.Hour}
	tick.Tick(context.Background())

	if len(repo.transitionedTo) < 1 {
		t.Fatalf("no transitions recorded; a nil-dispatched_at row past the updated_at deadline must still fail")
	}
	got := repo.transitionedTo[len(repo.transitionedTo)-1]
	if got.State != run.StageStateFailed {
		t.Errorf("State = %s, want failed", got.State)
	}
	if len(au.appended) != 1 {
		t.Fatalf("audit appended %d, want 1", len(au.appended))
	}
	var payload map[string]any
	if err := json.Unmarshal(au.appended[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload["dispatched_at"] != nil {
		t.Errorf("payload.dispatched_at = %v, want null for a legacy nil-dispatched_at row", payload["dispatched_at"])
	}
}

func TestTicker_DoesNotFailWithinTimeout(t *testing.T) {
	repo := &fakeRepo{}
	au := &fakeAudit{}

	// Dispatched 30m ago with a 1h timeout → still within window.
	repo.seed(mkLiveness(30*time.Minute, 30*time.Minute, nil))

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
	repo.seed(mkLiveness(48*time.Hour, 48*time.Hour, nil))

	tick := &Ticker{Repo: repo, Audit: &fakeAudit{}, Timeout: 0}
	tick.Tick(context.Background())

	if len(repo.transitionedTo) != 0 {
		t.Errorf("transitions = %d, want 0 (zero-timeout disables firing)", len(repo.transitionedTo))
	}
}

// M6: the lister errors → nothing transitions, nothing is audited.
func TestTick_ListErrorLogsAndSkips(t *testing.T) {
	repo := &fakeRepo{listErr: errors.New("db down")}
	au := &fakeAudit{}
	repo.seed(mkLiveness(2*time.Hour, 2*time.Hour, nil))

	tick := &Ticker{Repo: repo, Audit: au, Timeout: 1 * time.Hour}
	tick.Tick(context.Background())

	if len(repo.transitionedTo) != 0 {
		t.Errorf("transitions = %d, want 0 when the lister errors", len(repo.transitionedTo))
	}
	if len(au.appended) != 0 {
		t.Errorf("audit appended = %d, want 0 when the lister errors", len(au.appended))
	}
}

// M8: the transition succeeds but the audit append fails — we log loudly but do
// NOT roll back; the stage stays in its terminal failed state.
func TestTicker_AuditFailureLeavesStateChanged(t *testing.T) {
	repo := &fakeRepo{}
	au := &fakeAudit{appendErr: errors.New("db down")}
	repo.seed(mkLiveness(2*time.Hour, 15*time.Second, dur(15*time.Second)))

	tick := &Ticker{Repo: repo, Audit: au, Timeout: 1 * time.Hour}
	tick.Tick(context.Background())

	if len(repo.transitionedTo) == 0 {
		t.Fatalf("transition should still happen despite audit failure, got 0")
	}
	last := repo.transitionedTo[len(repo.transitionedTo)-1]
	if last.State != run.StageStateFailed {
		t.Errorf("last state = %s, want failed", last.State)
	}
}

// M7: if the transition fails (e.g. a concurrent writer already settled the
// stage) we must NOT append a misleading audit entry.
func TestTicker_TransitionFailureSkipsAudit(t *testing.T) {
	repo := &fakeRepo{transitionErr: errors.New("boom")}
	au := &fakeAudit{}
	repo.seed(mkLiveness(2*time.Hour, 2*time.Hour, nil))

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

// GetRunAccountID satisfies the REQUIRED run.AccountGetter portion of
// run.Repository (E44.11 / #2074). Untenanted: this fake's runs carry no
// tenant account, matching its pre-promotion effective behavior.
func (*fakeRepo) GetRunAccountID(_ context.Context, _ uuid.UUID) (string, error) {
	return "", nil
}
