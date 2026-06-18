package childcompletion

import (
	"context"
	"encoding/json"
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

func TestTick_FailedChildCategoryB_ParksParent(t *testing.T) {
	// #1081: a category-B (constraint/policy) child is now recoverable
	// in decomposition (re-driven in place via the recover path), so the
	// parent parks awaiting re-drive instead of resolving to failed-C.
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
			failedChild: {mkFailedImplement(failedChild, run.FailureB, "scope violation")},
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
		t.Errorf("transitions = %d, want 0 (category-B child should park awaiting re-drive)", len(rs.transitions))
	}
	if len(ad.advanced) != 0 {
		t.Errorf("Advance calls = %v, want none (parent parked)", ad.advanced)
	}
	if len(au.appended) != 0 {
		t.Errorf("audit appended = %v, want none (sweeper park is silent)", au.appended)
	}
}

func TestTick_FailedChildNonRecoverable_ResolvesFailedC(t *testing.T) {
	// A D-rejection child (approver said no) stays non-recoverable: the
	// parent must resolve to failed-C rather than park indefinitely.
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
			failedChild: {mkFailedImplement(failedChild, run.FailureD, "gate rejected by approver")},
		},
	}
	s := &Sweeper{Runs: rs, Audit: &fakeAudit{}, Advance: &recordingAdvancer{}, Logger: slog.Default()}

	if err := s.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if len(rs.transitions) != 1 {
		t.Fatalf("transitions = %d, want 1 (non-recoverable D-rejection resolves failed-C)", len(rs.transitions))
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

// recordingIntegrator stubs childcompletion.Integrator: it records the
// parent ids it was asked to integrate and returns a programmed conflict
// / error.
type recordingIntegrator struct {
	mu        sync.Mutex
	called    []uuid.UUID
	conflict  *SliceConflict
	returnErr error
}

func (i *recordingIntegrator) IntegrateSlices(_ context.Context, parentRunID uuid.UUID) (*SliceConflict, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.called = append(i.called, parentRunID)
	return i.conflict, i.returnErr
}

func TestTick_Integrate_CleanIntegrationResolvesSucceeded(t *testing.T) {
	parentRun := uuid.New()
	parentStage := &run.Stage{ID: uuid.New(), RunID: parentRun, State: run.StageStateAwaitingChildren}
	rs := &fakeRunRepo{
		awaitingChildren: []*run.Stage{parentStage},
		childrenByParent: map[uuid.UUID][]*run.Run{
			parentRun: {mkChild(uuid.New(), run.StateSucceeded), mkChild(uuid.New(), run.StateSucceeded)},
		},
	}
	au := &fakeAudit{}
	ad := &recordingAdvancer{}
	integ := &recordingIntegrator{} // nil conflict, nil err → clean
	s := &Sweeper{Runs: rs, Audit: au, Advance: ad, Integrate: integ, Logger: slog.Default()}

	if err := s.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	integ.mu.Lock()
	if len(integ.called) != 1 || integ.called[0] != parentRun {
		t.Errorf("IntegrateSlices called = %v, want [%s]", integ.called, parentRun)
	}
	integ.mu.Unlock()

	rs.mu.Lock()
	defer rs.mu.Unlock()
	if len(rs.transitions) != 1 || rs.transitions[0].To != run.StageStateSucceeded {
		t.Fatalf("transitions = %v, want one to succeeded", rs.transitions)
	}
	if len(ad.advanced) != 1 || ad.advanced[0] != parentRun {
		t.Errorf("Advance calls = %v, want [%s] after clean integration", ad.advanced, parentRun)
	}
}

func TestTick_Integrate_ConflictFailsParentBNoAdvance(t *testing.T) {
	parentRun := uuid.New()
	parentStage := &run.Stage{ID: uuid.New(), RunID: parentRun, State: run.StageStateAwaitingChildren}
	conflictChild := uuid.New()
	rs := &fakeRunRepo{
		awaitingChildren: []*run.Stage{parentStage},
		childrenByParent: map[uuid.UUID][]*run.Run{
			parentRun: {mkChild(uuid.New(), run.StateSucceeded)},
		},
	}
	au := &fakeAudit{}
	ad := &recordingAdvancer{}
	integ := &recordingIntegrator{
		conflict: &SliceConflict{SliceIndex: 2, ChildRunID: conflictChild, Detail: "slice integration conflict: slice 2"},
	}
	s := &Sweeper{Runs: rs, Audit: au, Advance: ad, Integrate: integ, Logger: slog.Default()}

	if err := s.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	rs.mu.Lock()
	if len(rs.transitions) != 1 || rs.transitions[0].To != run.StageStateFailed {
		t.Fatalf("transitions = %v, want one to failed", rs.transitions)
	}
	if rs.transitions[0].Failure == nil || *rs.transitions[0].Failure != run.FailureB {
		t.Errorf("FailureCategory = %v, want B", rs.transitions[0].Failure)
	}
	rs.mu.Unlock()

	// No Advance on a conflict — the parent stays failed-B (recoverable).
	if len(ad.advanced) != 0 {
		t.Errorf("Advance calls = %v, want none on conflict", ad.advanced)
	}

	// slice_integration_conflict carries the structured provenance.
	au.mu.Lock()
	defer au.mu.Unlock()
	var found *audit.ChainAppendParams
	for i := range au.appended {
		if au.appended[i].Category == "slice_integration_conflict" {
			found = &au.appended[i]
		}
	}
	if found == nil {
		t.Fatalf("no slice_integration_conflict audit; have %v", au.appended)
	}
	var p map[string]any
	if err := json.Unmarshal(found.Payload, &p); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if got, _ := p["conflicting_slice_index"].(float64); int(got) != 2 {
		t.Errorf("conflicting_slice_index = %v, want 2", p["conflicting_slice_index"])
	}
	if p["conflicting_child_run_id"] != conflictChild.String() {
		t.Errorf("conflicting_child_run_id = %v, want %q", p["conflicting_child_run_id"], conflictChild.String())
	}
}

func TestTick_Integrate_ErrorParksParentNoTransition(t *testing.T) {
	// A non-conflict integration error must leave the stage parked (it
	// surfaces as a tick error) — never resolve the parent succeeded.
	parentRun := uuid.New()
	parentStage := &run.Stage{ID: uuid.New(), RunID: parentRun, State: run.StageStateAwaitingChildren}
	rs := &fakeRunRepo{
		awaitingChildren: []*run.Stage{parentStage},
		childrenByParent: map[uuid.UUID][]*run.Run{
			parentRun: {mkChild(uuid.New(), run.StateSucceeded)},
		},
	}
	ad := &recordingAdvancer{}
	integ := &recordingIntegrator{returnErr: errors.New("github down")}
	s := &Sweeper{Runs: rs, Audit: &fakeAudit{}, Advance: ad, Integrate: integ, Logger: slog.Default()}

	// Tick swallows per-parent resolve errors (logged), so Tick returns nil
	// but the stage must NOT have transitioned and Advance must NOT fire.
	if err := s.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if len(rs.transitions) != 0 {
		t.Errorf("transitions = %v, want none on integration error (parent parked)", rs.transitions)
	}
	if len(ad.advanced) != 0 {
		t.Errorf("Advance calls = %v, want none on integration error", ad.advanced)
	}
}

func TestTick_NilIntegrate_PreservesPreFanInBehavior(t *testing.T) {
	// A nil Integrate (dev posture / pre-#1142) skips integration entirely:
	// all-succeeded children resolve the parent succeeded exactly as before.
	parentRun := uuid.New()
	parentStage := &run.Stage{ID: uuid.New(), RunID: parentRun, State: run.StageStateAwaitingChildren}
	rs := &fakeRunRepo{
		awaitingChildren: []*run.Stage{parentStage},
		childrenByParent: map[uuid.UUID][]*run.Run{
			parentRun: {mkChild(uuid.New(), run.StateSucceeded)},
		},
	}
	ad := &recordingAdvancer{}
	s := &Sweeper{Runs: rs, Audit: &fakeAudit{}, Advance: ad, Logger: slog.Default()} // Integrate nil

	if err := s.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if len(rs.transitions) != 1 || rs.transitions[0].To != run.StageStateSucceeded {
		t.Errorf("transitions = %v, want one to succeeded with nil Integrate", rs.transitions)
	}
	if len(ad.advanced) != 1 {
		t.Errorf("Advance calls = %v, want one with nil Integrate", ad.advanced)
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

// recordingDispatcher records DispatchChildren calls for the backstop tests.
type recordingDispatcher struct {
	mu        sync.Mutex
	calls     []uuid.UUID
	returnN   int
	returnErr error
}

func (d *recordingDispatcher) DispatchChildren(_ context.Context, parentRunID uuid.UUID) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, parentRunID)
	return d.returnN, d.returnErr
}

// TestResolveParent_BackstopDispatchesWhenNotAllTerminal asserts the
// fail-closed backstop (#1143): when a parent's children are not all
// terminal, the sweeper re-tops-up the concurrent dispatch via the wired
// Dispatcher before returning (the parent stays parked, no transition).
func TestResolveParent_BackstopDispatchesWhenNotAllTerminal(t *testing.T) {
	parentRun := uuid.New()
	parentStage := &run.Stage{ID: uuid.New(), RunID: parentRun, State: run.StageStateAwaitingChildren}
	rs := &fakeRunRepo{
		awaitingChildren: []*run.Stage{parentStage},
		childrenByParent: map[uuid.UUID][]*run.Run{
			parentRun: {
				mkChild(uuid.New(), run.StateRunning),
				mkChild(uuid.New(), run.StatePending),
			},
		},
	}
	disp := &recordingDispatcher{returnN: 1}
	s := &Sweeper{Runs: rs, Audit: &fakeAudit{}, Advance: &recordingAdvancer{}, Dispatch: disp, Logger: slog.Default()}

	if err := s.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	disp.mu.Lock()
	defer disp.mu.Unlock()
	if len(disp.calls) != 1 || disp.calls[0] != parentRun {
		t.Errorf("DispatchChildren calls = %v, want one for parent %s", disp.calls, parentRun)
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if len(rs.transitions) != 0 {
		t.Errorf("transitions = %d, want 0 (parent stays parked)", len(rs.transitions))
	}
}

// TestResolveParent_BackstopDispatchErrorIsBestEffort asserts a backstop
// DispatchChildren error is WARN-logged and swallowed: the tick returns nil
// and the parent stays parked (no transition), so a transient dispatch
// failure never wedges the sweep.
func TestResolveParent_BackstopDispatchErrorIsBestEffort(t *testing.T) {
	parentRun := uuid.New()
	parentStage := &run.Stage{ID: uuid.New(), RunID: parentRun, State: run.StageStateAwaitingChildren}
	rs := &fakeRunRepo{
		awaitingChildren: []*run.Stage{parentStage},
		childrenByParent: map[uuid.UUID][]*run.Run{
			parentRun: {
				mkChild(uuid.New(), run.StateRunning),
				mkChild(uuid.New(), run.StatePending),
			},
		},
	}
	disp := &recordingDispatcher{returnErr: errors.New("boom")}
	s := &Sweeper{Runs: rs, Audit: &fakeAudit{}, Advance: &recordingAdvancer{}, Dispatch: disp, Logger: slog.Default()}

	if err := s.Tick(context.Background()); err != nil {
		t.Fatalf("Tick should swallow the backstop error, got: %v", err)
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if len(rs.transitions) != 0 {
		t.Errorf("transitions = %d, want 0 (parent stays parked)", len(rs.transitions))
	}
}

// TestResolveParent_NilDispatchIsNoOp asserts a nil Dispatch disables the
// backstop entirely (pre-#1143 posture): the not-all-terminal branch still
// returns cleanly with no transition and no panic.
func TestResolveParent_NilDispatchIsNoOp(t *testing.T) {
	parentRun := uuid.New()
	parentStage := &run.Stage{ID: uuid.New(), RunID: parentRun, State: run.StageStateAwaitingChildren}
	rs := &fakeRunRepo{
		awaitingChildren: []*run.Stage{parentStage},
		childrenByParent: map[uuid.UUID][]*run.Run{
			parentRun: {
				mkChild(uuid.New(), run.StateRunning),
				mkChild(uuid.New(), run.StatePending),
			},
		},
	}
	s := &Sweeper{Runs: rs, Audit: &fakeAudit{}, Advance: &recordingAdvancer{}, Logger: slog.Default()} // Dispatch nil

	if err := s.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if len(rs.transitions) != 0 {
		t.Errorf("transitions = %d, want 0 (parent stays parked, no backstop)", len(rs.transitions))
	}
}
