package childcompletion

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// fakeRunRepo is a minimal run.Repository for sweeper tests. Only
// the methods Tick actually calls are wired with behaviour; the rest
// return "not used" errors so accidental drift fails loudly.
type fakeRunRepo struct {
	mu sync.Mutex

	awaitingChildren []*run.Stage
	childrenByParent map[uuid.UUID][]*run.Run
	stagesByRun      map[uuid.UUID][]*run.Stage
	transitions      []transitionCall
	transitionErr    error
}

type transitionCall struct {
	StageID  uuid.UUID
	To       run.StageState
	Failure  *run.FailureCategory
	Reason   *string
	HasError bool
}

func (f *fakeRunRepo) ListStagesAwaitingChildren(context.Context) ([]*run.Stage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.awaitingChildren, nil
}
func (f *fakeRunRepo) ListRuns(_ context.Context, fl run.ListRunsFilter) ([]*run.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if fl.DecomposedFrom == nil {
		return nil, nil
	}
	return f.childrenByParent[*fl.DecomposedFrom], nil
}
func (f *fakeRunRepo) TransitionStage(_ context.Context, id uuid.UUID, to run.StageState, c *run.StageCompletion) (*run.Stage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.transitionErr != nil {
		return nil, f.transitionErr
	}
	call := transitionCall{StageID: id, To: to}
	if c != nil {
		call.Failure = c.FailureCategory
		call.Reason = c.FailureReason
	}
	f.transitions = append(f.transitions, call)
	return &run.Stage{ID: id, State: to}, nil
}

// Remaining run.Repository methods: not used by the sweeper, return
// not-used errors to surface accidental drift.
func (f *fakeRunRepo) CreateRun(context.Context, run.CreateRunParams) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (f *fakeRunRepo) GetRun(context.Context, uuid.UUID) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (f *fakeRunRepo) GetRunByIdempotencyKey(context.Context, string, string) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (f *fakeRunRepo) TransitionRun(context.Context, uuid.UUID, run.State) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (f *fakeRunRepo) RetryRun(context.Context, uuid.UUID, run.State) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (f *fakeRunRepo) SetRunPullRequestURL(context.Context, uuid.UUID, string) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (f *fakeRunRepo) CreateStage(context.Context, run.CreateStageParams) (*run.Stage, error) {
	return nil, errors.New("not used")
}
func (f *fakeRunRepo) GetStage(context.Context, uuid.UUID) (*run.Stage, error) {
	return nil, errors.New("not used")
}
func (f *fakeRunRepo) ListStagesForRun(_ context.Context, runID uuid.UUID) ([]*run.Stage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stagesByRun == nil {
		return nil, nil
	}
	return f.stagesByRun[runID], nil
}
func (f *fakeRunRepo) ListStagesAwaitingApproval(context.Context) ([]*run.Stage, error) {
	return nil, errors.New("not used")
}
func (f *fakeRunRepo) ListReviewStagesAwaitingApproval(context.Context) ([]*run.Stage, error) {
	return nil, errors.New("not used")
}

func (f *fakeRunRepo) ListStagesDispatched(context.Context) ([]*run.Stage, error) {
	return nil, errors.New("not used")
}
func (f *fakeRunRepo) RetryStage(context.Context, uuid.UUID, run.StageState) (*run.Stage, error) {
	return nil, errors.New("not used")
}

// fakeAudit records AppendChained calls.
type fakeAudit struct {
	mu       sync.Mutex
	appended []audit.ChainAppendParams
}

func (f *fakeAudit) ChainsByParent(_ context.Context, _ uuid.UUID, _ bool) ([]*audit.Entry, error) {
	return nil, nil
}
func (f *fakeAudit) AppendChained(_ context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.appended = append(f.appended, p)
	return &audit.Entry{ID: uuid.New()}, nil
}
func (f *fakeAudit) Append(context.Context, audit.AppendParams) (*audit.Entry, error) {
	return nil, nil
}
func (f *fakeAudit) AppendGlobalChained(context.Context, audit.GlobalChainAppendParams) (*audit.Entry, error) {
	return nil, nil
}
func (f *fakeAudit) Get(context.Context, uuid.UUID) (*audit.Entry, error) {
	return nil, audit.ErrNotFound
}
func (f *fakeAudit) ListForRun(context.Context, uuid.UUID) ([]*audit.Entry, error) {
	return nil, nil
}
func (f *fakeAudit) ListGlobal(context.Context) ([]*audit.Entry, error) { return nil, nil }
func (f *fakeAudit) LastForRun(context.Context, uuid.UUID) (*audit.Entry, error) {
	return nil, audit.ErrNotFound
}
func (f *fakeAudit) ListForRunByCategory(context.Context, uuid.UUID, string) ([]*audit.Entry, error) {
	return nil, nil
}
func (f *fakeAudit) ListAll(context.Context, audit.ListAllParams) ([]*audit.Entry, error) {
	return nil, nil
}

type recordingAdvancer struct {
	mu        sync.Mutex
	advanced  []uuid.UUID
	returnErr error
}

func (a *recordingAdvancer) Advance(_ context.Context, runID uuid.UUID) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.advanced = append(a.advanced, runID)
	return a.returnErr
}

func mkChild(id uuid.UUID, state run.State) *run.Run {
	return &run.Run{ID: id, State: state, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
}

// mkFailedImplement builds a failed implement stage carrying the given
// failure category + reason, for driving the #698 retryability check.
func mkFailedImplement(runID uuid.UUID, cat run.FailureCategory, reason string) *run.Stage {
	c := cat
	r := reason
	return &run.Stage{
		ID:              uuid.New(),
		RunID:           runID,
		Type:            run.StageTypeImplement,
		State:           run.StageStateFailed,
		FailureCategory: &c,
		FailureReason:   &r,
	}
}

func TestTick_AllChildrenSucceed_TransitionsParentToSucceeded(t *testing.T) {
	parentRun := uuid.New()
	parentStage := &run.Stage{ID: uuid.New(), RunID: parentRun, State: run.StageStateAwaitingChildren}

	rs := &fakeRunRepo{
		awaitingChildren: []*run.Stage{parentStage},
		childrenByParent: map[uuid.UUID][]*run.Run{
			parentRun: {
				mkChild(uuid.New(), run.StateSucceeded),
				mkChild(uuid.New(), run.StateSucceeded),
			},
		},
	}
	au := &fakeAudit{}
	ad := &recordingAdvancer{}
	s := &Sweeper{Runs: rs, Audit: au, Advance: ad, Logger: slog.Default()}

	if err := s.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	rs.mu.Lock()
	defer rs.mu.Unlock()
	if len(rs.transitions) != 1 {
		t.Fatalf("transitions = %d, want 1", len(rs.transitions))
	}
	tr := rs.transitions[0]
	if tr.To != run.StageStateSucceeded {
		t.Errorf("transition target = %q, want succeeded", tr.To)
	}
	if tr.Failure != nil {
		t.Errorf("succeeded transition has FailureCategory: %v", *tr.Failure)
	}

	au.mu.Lock()
	defer au.mu.Unlock()
	if len(au.appended) != 1 || au.appended[0].Category != "children_settled" {
		t.Errorf("audit appended = %v", au.appended)
	}

	if len(ad.advanced) != 1 || ad.advanced[0] != parentRun {
		t.Errorf("Advance calls = %v, want [%s]", ad.advanced, parentRun)
	}
}

func TestTick_OneChildFails_TransitionsParentToFailedC(t *testing.T) {
	parentRun := uuid.New()
	parentStage := &run.Stage{ID: uuid.New(), RunID: parentRun, State: run.StageStateAwaitingChildren}

	rs := &fakeRunRepo{
		awaitingChildren: []*run.Stage{parentStage},
		childrenByParent: map[uuid.UUID][]*run.Run{
			parentRun: {
				mkChild(uuid.New(), run.StateSucceeded),
				mkChild(uuid.New(), run.StateFailed),
			},
		},
	}
	s := &Sweeper{Runs: rs, Audit: &fakeAudit{}, Advance: &recordingAdvancer{}, Logger: slog.Default()}

	if err := s.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if len(rs.transitions) != 1 {
		t.Fatalf("transitions = %d, want 1", len(rs.transitions))
	}
	tr := rs.transitions[0]
	if tr.To != run.StageStateFailed {
		t.Errorf("target = %q, want failed", tr.To)
	}
	if tr.Failure == nil || *tr.Failure != run.FailureC {
		t.Errorf("FailureCategory = %v, want C", tr.Failure)
	}
}

func TestTick_AllFailedChildrenRetryable_ParksParent(t *testing.T) {
	parentRun := uuid.New()
	parentStage := &run.Stage{ID: uuid.New(), RunID: parentRun, State: run.StageStateAwaitingChildren}
	failedChild := uuid.New()

	rs := &fakeRunRepo{
		awaitingChildren: []*run.Stage{parentStage},
		childrenByParent: map[uuid.UUID][]*run.Run{
			parentRun: {
				mkChild(uuid.New(), run.StateSucceeded),
				mkChild(failedChild, run.StateFailed),
			},
		},
		stagesByRun: map[uuid.UUID][]*run.Stage{
			// Category C (infrastructure) is retryable: the parent
			// should park awaiting re-drive, not resolve to failed-C.
			failedChild: {mkFailedImplement(failedChild, run.FailureC, "runner OOM")},
		},
	}
	au := &fakeAudit{}
	ad := &recordingAdvancer{}
	s := &Sweeper{Runs: rs, Audit: au, Advance: ad, Logger: slog.Default()}

	if err := s.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if len(rs.transitions) != 0 {
		t.Errorf("transitions = %d, want 0 (parent should park awaiting re-drive)", len(rs.transitions))
	}
	if len(ad.advanced) != 0 {
		t.Errorf("Advance calls = %v, want none (parent parked)", ad.advanced)
	}
	// The sweeper does not audit on park — discoverability comes from
	// the orchestrator path's one-time parent_awaiting_redrive entry.
	if len(au.appended) != 0 {
		t.Errorf("audit appended = %v, want none (sweeper park is silent)", au.appended)
	}
}

func TestTick_FailedChildCategoryB_ResolvesFailedC(t *testing.T) {
	parentRun := uuid.New()
	parentStage := &run.Stage{ID: uuid.New(), RunID: parentRun, State: run.StageStateAwaitingChildren}
	failedChild := uuid.New()

	rs := &fakeRunRepo{
		awaitingChildren: []*run.Stage{parentStage},
		childrenByParent: map[uuid.UUID][]*run.Run{
			parentRun: {
				mkChild(failedChild, run.StateFailed),
			},
		},
		stagesByRun: map[uuid.UUID][]*run.Stage{
			// Category B (constraint/policy) is NOT retryable: the
			// parent must resolve to failed-C.
			failedChild: {mkFailedImplement(failedChild, run.FailureB, "scope violation")},
		},
	}
	s := &Sweeper{Runs: rs, Audit: &fakeAudit{}, Advance: &recordingAdvancer{}, Logger: slog.Default()}

	if err := s.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if len(rs.transitions) != 1 {
		t.Fatalf("transitions = %d, want 1 (non-retryable B resolves failed-C)", len(rs.transitions))
	}
	tr := rs.transitions[0]
	if tr.To != run.StageStateFailed {
		t.Errorf("target = %q, want failed", tr.To)
	}
	if tr.Failure == nil || *tr.Failure != run.FailureC {
		t.Errorf("FailureCategory = %v, want C", tr.Failure)
	}
}

func TestTick_AnyChildStillRunning_NoTransition(t *testing.T) {
	parentRun := uuid.New()
	parentStage := &run.Stage{ID: uuid.New(), RunID: parentRun, State: run.StageStateAwaitingChildren}

	rs := &fakeRunRepo{
		awaitingChildren: []*run.Stage{parentStage},
		childrenByParent: map[uuid.UUID][]*run.Run{
			parentRun: {
				mkChild(uuid.New(), run.StateSucceeded),
				mkChild(uuid.New(), run.StateRunning),
			},
		},
	}
	s := &Sweeper{Runs: rs, Audit: &fakeAudit{}, Advance: &recordingAdvancer{}, Logger: slog.Default()}

	if err := s.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if len(rs.transitions) != 0 {
		t.Errorf("transitions = %d, want 0 (one child still running)", len(rs.transitions))
	}
}

func TestTick_NoChildren_NoOp(t *testing.T) {
	parentRun := uuid.New()
	parentStage := &run.Stage{ID: uuid.New(), RunID: parentRun, State: run.StageStateAwaitingChildren}

	rs := &fakeRunRepo{
		awaitingChildren: []*run.Stage{parentStage},
		childrenByParent: map[uuid.UUID][]*run.Run{},
	}
	s := &Sweeper{Runs: rs, Audit: &fakeAudit{}, Advance: &recordingAdvancer{}, Logger: slog.Default()}

	if err := s.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if len(rs.transitions) != 0 {
		t.Errorf("transitions = %d, want 0 (no children yet)", len(rs.transitions))
	}
}

func TestRun_RequiresAllDeps(t *testing.T) {
	s := &Sweeper{}
	err := s.Run(context.Background())
	if err == nil || !errIsMissingDeps(err) {
		t.Errorf("Run with no deps returned %v, want missing-deps error", err)
	}
}

func errIsMissingDeps(err error) bool {
	return err != nil && err.Error() == "childcompletion: Runs, Audit, and Advance must all be set"
}
