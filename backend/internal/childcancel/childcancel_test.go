package childcancel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// memRuns is an in-memory RunStore over run.BaseFake. ListRuns and GetRun
// return COPIES, like the Postgres repository, so a caller's snapshot never
// aliases the stored row.
type memRuns struct {
	run.BaseFake
	mu            sync.Mutex
	runs          map[uuid.UUID]*run.Run
	stages        map[uuid.UUID][]*run.Stage
	stageErr      map[uuid.UUID]error
	transitionErr map[uuid.UUID]error
	getErr        map[uuid.UUID]error
	listErr       error
	getCalls      map[uuid.UUID]int
	transitions   int
	seq           int
}

func newMemRuns() *memRuns {
	return &memRuns{
		runs: map[uuid.UUID]*run.Run{}, stages: map[uuid.UUID][]*run.Stage{}, stageErr: map[uuid.UUID]error{},
		transitionErr: map[uuid.UUID]error{}, getErr: map[uuid.UUID]error{}, getCalls: map[uuid.UUID]int{},
	}
}

func (m *memRuns) add(state run.State, parent *uuid.UUID) *run.Run {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	r := &run.Run{ID: uuid.New(), State: state, DecomposedFrom: parent, CreatedAt: time.Unix(int64(m.seq), 0)}
	m.runs[r.ID] = r
	c := *r
	return &c
}

func (m *memRuns) addImplement(runID uuid.UUID, state run.StageState) uuid.UUID {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := uuid.New()
	m.stages[runID] = append(m.stages[runID],
		&run.Stage{ID: uuid.New(), RunID: runID, Type: run.StageTypePlan, State: run.StageStateSucceeded},
		&run.Stage{ID: id, RunID: runID, Type: run.StageTypeImplement, State: state})
	return id
}

func (m *memRuns) state(id uuid.UUID) run.State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runs[id].State
}

func (m *memRuns) GetRun(_ context.Context, id uuid.UUID) (*run.Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.getCalls[id]++
	if err := m.getErr[id]; err != nil {
		return nil, err
	}
	r, ok := m.runs[id]
	if !ok {
		return nil, run.ErrNotFound
	}
	c := *r
	return &c, nil
}

func (m *memRuns) ListRuns(_ context.Context, f run.ListRunsFilter) ([]*run.Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listErr != nil {
		return nil, m.listErr
	}
	var matched []*run.Run
	for _, r := range m.runs {
		if f.State != "" && string(r.State) != f.State {
			continue
		}
		if f.DecomposedFrom != nil && (r.DecomposedFrom == nil || *r.DecomposedFrom != *f.DecomposedFrom) {
			continue
		}
		c := *r
		matched = append(matched, &c)
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].CreatedAt.After(matched[j].CreatedAt) })
	if f.Offset >= len(matched) {
		return nil, nil
	}
	end := f.Offset + f.Limit
	if end > len(matched) {
		end = len(matched)
	}
	return matched[f.Offset:end], nil
}

func (m *memRuns) ListStagesForRun(_ context.Context, runID uuid.UUID) ([]*run.Stage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.stageErr[runID]; err != nil {
		return nil, err
	}
	return m.stages[runID], nil
}

// TransitionRun mirrors postgresRepo: same-state is a successful no-op,
// otherwise run.ValidRunTransition gates the move.
func (m *memRuns) TransitionRun(_ context.Context, id uuid.UUID, to run.State) (*run.Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.transitions++
	if err := m.transitionErr[id]; err != nil {
		return nil, err
	}
	r, ok := m.runs[id]
	if !ok {
		return nil, run.ErrNotFound
	}
	if r.State != to {
		if !run.ValidRunTransition(r.State, to) {
			return nil, run.InvalidTransitionError{Kind: "run", From: string(r.State), To: string(to)}
		}
		r.State = to
	}
	c := *r
	return &c, nil
}

// memAudit is an in-memory AuditStore WITHOUT the deduped capability, so it
// drives the fallback list-then-append leg.
type memAudit struct {
	mu        sync.Mutex
	rows      []audit.ChainAppendParams
	appendErr error
	listErr   error
	listCalls int
}

func (a *memAudit) AppendChained(_ context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.appendErr != nil {
		return nil, a.appendErr
	}
	a.rows = append(a.rows, p)
	rid := p.RunID
	return &audit.Entry{RunID: &rid, Category: p.Category, Payload: p.Payload}, nil
}

func (a *memAudit) ListForRunByCategory(_ context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.listCalls++
	if a.listErr != nil {
		return nil, a.listErr
	}
	var out []*audit.Entry
	for _, p := range a.rows {
		if p.RunID == runID && p.Category == category {
			rid := p.RunID
			out = append(out, &audit.Entry{RunID: &rid, Category: p.Category, Payload: p.Payload})
		}
	}
	return out, nil
}

// rowsFor decodes every Category row on runID's chain.
func (a *memAudit) rowsFor(t *testing.T, runID uuid.UUID) []map[string]any {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []map[string]any
	for _, p := range a.rows {
		if p.RunID != runID || p.Category != Category {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(p.Payload, &m); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		if p.ActorKind == nil || *p.ActorKind != audit.ActorSystem {
			t.Errorf("row actor = %v, want system", p.ActorKind)
		}
		out = append(out, m)
	}
	return out
}

// seedPrior appends one prior Category row naming parentID on childID's chain.
func (a *memAudit) seedPrior(childID, parentID uuid.UUID) {
	payload, _ := json.Marshal(map[string]any{"parent_run_id": parentID.String()})
	actor := audit.ActorSystem
	a.rows = append(a.rows, audit.ChainAppendParams{RunID: childID, Category: Category, ActorKind: &actor, Payload: payload})
}

// dedupAudit adds the audit.DedupedChainAppender capability with a real scan.
type dedupAudit struct {
	*memAudit
	dedupErr error
	calls    int
}

func (d *dedupAudit) AppendChainedDeduped(ctx context.Context, p audit.ChainAppendParams, spec audit.DedupeSpec) (*audit.Entry, error) {
	d.mu.Lock()
	d.calls++
	if d.dedupErr != nil {
		d.mu.Unlock()
		return nil, d.dedupErr
	}
	for _, r := range d.rows {
		if r.RunID != p.RunID || r.Category != p.Category {
			continue
		}
		var m map[string]any
		if json.Unmarshal(r.Payload, &m) == nil && m[spec.PayloadKey] == spec.PayloadValue {
			d.mu.Unlock()
			return nil, &audit.DedupedDuplicateError{Existing: &audit.Entry{Sequence: 1}}
		}
	}
	d.mu.Unlock()
	return d.AppendChained(ctx, p)
}

var _ audit.DedupedChainAppender = (*dedupAudit)(nil)

// TestCancelChild_SkipsTerminalSnapshot: a terminal snapshot is skipped with
// NO TransitionRun call and no row (a same-state call would succeed and
// write a spurious row).
func TestCancelChild_SkipsTerminalSnapshot(t *testing.T) {
	for _, st := range []run.State{run.StateCancelled, run.StateSucceeded, run.StateFailed} {
		t.Run(string(st), func(t *testing.T) {
			m, a := newMemRuns(), &memAudit{}
			parent := m.add(run.StateCancelled, nil)
			child := m.add(st, &parent.ID)
			res := CancelChild(context.Background(), m, a, child, parent.ID, parent.State, ReasonParentCancelled, "operator_cancel")
			if res.Outcome != OutcomeSkippedTerminal || res.Appended || res.Err != nil {
				t.Errorf("result = %+v, want skipped_terminal, no append, no error", res)
			}
			if m.transitions != 0 {
				t.Errorf("TransitionRun calls = %d, want 0", m.transitions)
			}
			if len(a.rows) != 0 {
				t.Errorf("rows = %d, want 0", len(a.rows))
			}
		})
	}
}

// TestCancelChild_TransitionsAndAppends: the happy path — a running child with
// a dispatched implement stage is cancelled with one fully-populated row.
func TestCancelChild_TransitionsAndAppends(t *testing.T) {
	m, a := newMemRuns(), &memAudit{}
	parent := m.add(run.StateCancelled, nil)
	child := m.add(run.StateRunning, &parent.ID)
	implID := m.addImplement(child.ID, run.StageStateDispatched)

	res := CancelChild(context.Background(), m, a, child, parent.ID, parent.State, ReasonParentCancelled, "operator_cancel")
	if res.Outcome != OutcomeCancelled || !res.Appended || res.Err != nil || !res.LiveStage ||
		res.ImplementStageID == nil || *res.ImplementStageID != implID || res.ImplementStageState != run.StageStateDispatched ||
		res.FromState != run.StateRunning || res.ChildRunID != child.ID || res.ParentRunID != parent.ID {
		t.Fatalf("result = %+v", res)
	}
	if got := m.state(child.ID); got != run.StateCancelled {
		t.Errorf("child state = %q, want cancelled", got)
	}
	rows := a.rowsFor(t, child.ID)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	want := map[string]any{
		"parent_run_id": parent.ID.String(), "parent_state": "cancelled", "reason": ReasonParentCancelled,
		"cancel_source": "operator_cancel", "from_state": "running", "implement_stage_id": implID.String(),
		"implement_stage_state": "dispatched", "live_stage": true,
	}
	for k, v := range want {
		if rows[0][k] != v {
			t.Errorf("payload[%s] = %v, want %v", k, rows[0][k], v)
		}
	}
}

// TestCancelChild_LiveStageOnlyForSpawnedImplement: dispatched and running are
// live; every other implement state is not.
func TestCancelChild_LiveStageOnlyForSpawnedImplement(t *testing.T) {
	for st, want := range map[run.StageState]bool{
		run.StageStatePending: false, run.StageStateAwaitingHostDispatch: false,
		run.StageStateDispatched: true, run.StageStateRunning: true, run.StageStateAwaitingChildren: false,
	} {
		m, a := newMemRuns(), &memAudit{}
		parent := m.add(run.StateCancelled, nil)
		child := m.add(run.StateRunning, &parent.ID)
		m.addImplement(child.ID, st)
		if res := CancelChild(context.Background(), m, a, child, parent.ID, parent.State, ReasonParentCancelled, "s"); res.LiveStage != want {
			t.Errorf("%s: LiveStage = %v, want %v", st, res.LiveStage, want)
		}
	}
}

// TestCancelChild_StageReadErrorStillCancels: a stage read failure records an
// empty stage (null id, "" state, live false) and still cancels.
func TestCancelChild_StageReadErrorStillCancels(t *testing.T) {
	m, a := newMemRuns(), &memAudit{}
	parent := m.add(run.StateCancelled, nil)
	child := m.add(run.StatePending, &parent.ID)
	m.addImplement(child.ID, run.StageStateRunning)
	m.stageErr[child.ID] = errors.New("injected stage read failure")

	res := CancelChild(context.Background(), m, a, child, parent.ID, parent.State, ReasonParentCancelled, "s")
	if res.Outcome != OutcomeCancelled || res.ImplementStageID != nil || res.ImplementStageState != "" || res.LiveStage {
		t.Fatalf("result = %+v, want cancelled with an empty stage", res)
	}
	rows := a.rowsFor(t, child.ID)
	if len(rows) != 1 || rows[0]["implement_stage_id"] != nil || rows[0]["implement_stage_state"] != "" {
		t.Errorf("rows = %+v, want one row with a null stage id", rows)
	}
}

// TestCancelChild_RacedInvalidTransitionSkipped: the state machine refusing
// the move (the child raced to succeeded) is skipped_terminal with no row.
func TestCancelChild_RacedInvalidTransitionSkipped(t *testing.T) {
	m, a := newMemRuns(), &memAudit{}
	parent := m.add(run.StateCancelled, nil)
	child := m.add(run.StateRunning, &parent.ID)
	m.mu.Lock()
	m.runs[child.ID].State = run.StateSucceeded // the stored row moved on; the snapshot is stale
	m.mu.Unlock()

	res := CancelChild(context.Background(), m, a, child, parent.ID, parent.State, ReasonParentCancelled, "s")
	if res.Outcome != OutcomeSkippedTerminal || res.Err != nil || res.Appended {
		t.Errorf("result = %+v, want skipped_terminal", res)
	}
	if len(a.rows) != 0 {
		t.Errorf("rows = %d, want 0", len(a.rows))
	}
}

// TestCancelChild_TransitionErrorFailed: any other transition error is
// OutcomeFailed carrying the error, with no row.
func TestCancelChild_TransitionErrorFailed(t *testing.T) {
	m, a := newMemRuns(), &memAudit{}
	parent := m.add(run.StateCancelled, nil)
	child := m.add(run.StateRunning, &parent.ID)
	boom := errors.New("injected transition failure")
	m.transitionErr[child.ID] = boom

	res := CancelChild(context.Background(), m, a, child, parent.ID, parent.State, ReasonParentCancelled, "s")
	if res.Outcome != OutcomeFailed || !errors.Is(res.Err, boom) || res.Appended {
		t.Errorf("result = %+v, want failed carrying the error", res)
	}
	if len(a.rows) != 0 {
		t.Errorf("rows = %d, want 0", len(a.rows))
	}
}

// TestCancelChild_AppendErrorStillCancelled: the state change landed, so the
// outcome stays cancelled while Err carries the append failure.
func TestCancelChild_AppendErrorStillCancelled(t *testing.T) {
	for name, a := range map[string]AuditStore{
		"fallback":   &memAudit{appendErr: errors.New("injected append failure")},
		"capability": &dedupAudit{memAudit: &memAudit{}, dedupErr: errors.New("injected append failure")},
	} {
		t.Run(name, func(t *testing.T) {
			m := newMemRuns()
			parent := m.add(run.StateCancelled, nil)
			child := m.add(run.StateRunning, &parent.ID)
			res := CancelChild(context.Background(), m, a, child, parent.ID, parent.State, ReasonParentCancelled, "s")
			if res.Outcome != OutcomeCancelled || res.Err == nil || res.Appended {
				t.Errorf("result = %+v, want cancelled with an append error", res)
			}
			if got := m.state(child.ID); got != run.StateCancelled {
				t.Errorf("child state = %q, want cancelled", got)
			}
		})
	}
}

// TestCancelChild_FallbackDedup pins the non-capable leg. The fixture already
// holds ONE row for the parent (approval condition C6), so mutating the scan
// to always-false yields two rows — that is counterfactual (4)'s fallback arm.
// A prior row for a DIFFERENT parent does not dedup (the key is
// parent_run_id), and a list error proceeds without the guard.
func TestCancelChild_FallbackDedup(t *testing.T) {
	t.Run("same parent deduped", func(t *testing.T) {
		m, a := newMemRuns(), &memAudit{}
		parent := m.add(run.StateCancelled, nil)
		child := m.add(run.StateRunning, &parent.ID)
		a.seedPrior(child.ID, parent.ID)
		res := CancelChild(context.Background(), m, a, child, parent.ID, parent.State, ReasonParentCancelled, "s")
		if res.Outcome != OutcomeCancelled || res.Appended || res.Err != nil {
			t.Errorf("result = %+v, want cancelled, not appended", res)
		}
		if n := len(a.rowsFor(t, child.ID)); n != 1 {
			t.Errorf("rows = %d, want 1 (the prior row only)", n)
		}
	})
	t.Run("other parent appends", func(t *testing.T) {
		m, a := newMemRuns(), &memAudit{}
		parent := m.add(run.StateCancelled, nil)
		child := m.add(run.StateRunning, &parent.ID)
		a.seedPrior(child.ID, uuid.New())
		if res := CancelChild(context.Background(), m, a, child, parent.ID, parent.State, ReasonParentCancelled, "s"); !res.Appended {
			t.Errorf("result = %+v, want appended", res)
		}
	})
	t.Run("list error proceeds", func(t *testing.T) {
		m, a := newMemRuns(), &memAudit{listErr: errors.New("injected list failure")}
		parent := m.add(run.StateCancelled, nil)
		child := m.add(run.StateRunning, &parent.ID)
		res := CancelChild(context.Background(), m, a, child, parent.ID, parent.State, ReasonParentCancelled, "s")
		if !res.Appended || res.Err != nil || a.listCalls != 1 {
			t.Errorf("result = %+v listCalls = %d, want appended after one failed list", res, a.listCalls)
		}
	})
}

// TestCancelChild_CapabilityDedup: on a capable store the deduped append is
// used (never the fallback list), and a duplicate is not an error. A stale
// running snapshot replays the cancel, so the IsTerminal skip cannot fire
// and only the dedup prevents a second row (counterfactual (4)).
func TestCancelChild_CapabilityDedup(t *testing.T) {
	m := newMemRuns()
	a := &dedupAudit{memAudit: &memAudit{}}
	parent := m.add(run.StateCancelled, nil)
	child := m.add(run.StateRunning, &parent.ID)
	first := CancelChild(context.Background(), m, a, child, parent.ID, parent.State, ReasonParentCancelled, "s")
	second := CancelChild(context.Background(), m, a, child, parent.ID, parent.State, ReasonParentCancelled, "s")
	if !first.Appended || second.Appended || second.Err != nil || second.Outcome != OutcomeCancelled {
		t.Errorf("first = %+v, second = %+v; want one append then a benign duplicate", first, second)
	}
	if n := len(a.rowsFor(t, child.ID)); n != 1 {
		t.Errorf("rows = %d, want 1", n)
	}
	if a.calls != 2 || a.listCalls != 0 {
		t.Errorf("deduped calls = %d, fallback lists = %d; want 2 / 0", a.calls, a.listCalls)
	}
}

// TestCascadeFromParent_ListErrorTouchesNothing: the child list fails closed
// before any child is touched.
func TestCascadeFromParent_ListErrorTouchesNothing(t *testing.T) {
	m, a := newMemRuns(), &memAudit{}
	parent := m.add(run.StateCancelled, nil)
	child := m.add(run.StateRunning, &parent.ID)
	m.listErr = errors.New("injected list failure")
	res, err := CascadeFromParent(context.Background(), m, a, parent, "s")
	if err == nil || res != nil {
		t.Fatalf("CascadeFromParent = (%v, %v), want an error and no results", res, err)
	}
	if m.transitions != 0 || m.state(child.ID) != run.StateRunning {
		t.Errorf("transitions = %d, child = %q; want 0 / running", m.transitions, m.state(child.ID))
	}
}

// TestCascade_PagesPast100: a fan-out past one ListRuns page is cancelled in
// full, and only the parent's children are touched.
func TestCascade_PagesPast100(t *testing.T) {
	m, a := newMemRuns(), &memAudit{}
	parent := m.add(run.StateCancelled, nil)
	const n = 2*pageSize + 7
	for i := 0; i < n; i++ {
		m.add(run.StateRunning, &parent.ID)
	}
	unrelated := m.add(run.StateRunning, nil)
	res, err := CascadeFromParent(context.Background(), m, a, parent, "s")
	if err != nil {
		t.Fatalf("CascadeFromParent: %v", err)
	}
	if len(res) != n {
		t.Fatalf("results = %d, want %d", len(res), n)
	}
	for _, r := range res {
		if r.Outcome != OutcomeCancelled || m.state(r.ChildRunID) != run.StateCancelled {
			t.Fatalf("child %s: %+v, state %q", r.ChildRunID, r, m.state(r.ChildRunID))
		}
	}
	if len(a.rows) != n {
		t.Errorf("rows = %d, want %d", len(a.rows), n)
	}
	if m.state(unrelated.ID) != run.StateRunning {
		t.Error("an unrelated run was cancelled")
	}
}

// TestFindOrphans_ParentStateFilter: only children of cancelled and succeeded
// parents are orphans. Counterfactual (5): accepting any terminal parent lets
// the FAILED parent's child in; dropping the filter lets the RUNNING parent's
// child in (approval condition C6) — both are seeded here. Each parent is read
// once even with two children.
func TestFindOrphans_ParentStateFilter(t *testing.T) {
	m := newMemRuns()
	parents := map[run.State]*run.Run{}
	children := map[run.State]*run.Run{}
	for _, st := range []run.State{run.StateCancelled, run.StateSucceeded, run.StateFailed, run.StateRunning, run.StatePending} {
		p := m.add(st, nil)
		parents[st] = p
		children[st] = m.add(run.StateRunning, &p.ID)
	}
	second := m.add(run.StatePending, &parents[run.StateCancelled].ID)
	m.add(run.StateCancelled, &parents[run.StateCancelled].ID) // terminal child: not a candidate
	m.add(run.StateRunning, nil)                               // non-child

	orphans, err := FindOrphans(context.Background(), m)
	if err != nil {
		t.Fatalf("FindOrphans: %v", err)
	}
	got := map[uuid.UUID]run.State{}
	for _, o := range orphans {
		got[o.Child.ID] = o.ParentState
	}
	want := map[uuid.UUID]run.State{
		children[run.StateCancelled].ID: run.StateCancelled,
		second.ID:                       run.StateCancelled,
		children[run.StateSucceeded].ID: run.StateSucceeded,
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("orphans = %v, want %v", got, want)
	}
	if c := m.getCalls[parents[run.StateCancelled].ID]; c != 1 {
		t.Errorf("cancelled parent GetRun calls = %d, want 1 (cached)", c)
	}
}

// TestFindOrphans_ParentReadErrorFailsClosed: one unreadable parent fails the
// whole scan rather than silently dropping its children.
func TestFindOrphans_ParentReadErrorFailsClosed(t *testing.T) {
	m := newMemRuns()
	ok := m.add(run.StateCancelled, nil)
	m.add(run.StateRunning, &ok.ID)
	bad := m.add(run.StateCancelled, nil)
	m.add(run.StateRunning, &bad.ID)
	m.getErr[bad.ID] = errors.New("injected parent read failure")
	if orphans, err := FindOrphans(context.Background(), m); err == nil || orphans != nil {
		t.Errorf("FindOrphans = (%v, %v), want an error and no orphans", orphans, err)
	}
}

// TestFindOrphans_ListErrorFailsClosed: a candidate-list failure fails the
// scan.
func TestFindOrphans_ListErrorFailsClosed(t *testing.T) {
	m := newMemRuns()
	m.listErr = errors.New("injected list failure")
	if orphans, err := FindOrphans(context.Background(), m); err == nil || orphans != nil {
		t.Errorf("FindOrphans = (%v, %v), want an error", orphans, err)
	}
}

// TestFindOrphans_PagesPast100: the candidate scan walks every page of both
// non-terminal states.
func TestFindOrphans_PagesPast100(t *testing.T) {
	m := newMemRuns()
	p := m.add(run.StateSucceeded, nil)
	for i := 0; i < pageSize+3; i++ {
		m.add(run.StatePending, &p.ID)
		m.add(run.StateRunning, &p.ID)
	}
	orphans, err := FindOrphans(context.Background(), m)
	if err != nil {
		t.Fatalf("FindOrphans: %v", err)
	}
	if len(orphans) != 2*(pageSize+3) {
		t.Errorf("orphans = %d, want %d", len(orphans), 2*(pageSize+3))
	}
}

// TestReconcile_StampsBackfillReasonAndSource: the backfill cancels each orphan
// with parent_terminal / orphan_backfill and the parent's own state.
func TestReconcile_StampsBackfillReasonAndSource(t *testing.T) {
	m, a := newMemRuns(), &memAudit{}
	p := m.add(run.StateSucceeded, nil)
	c := m.add(run.StatePending, &p.ID)
	orphans, err := FindOrphans(context.Background(), m)
	if err != nil || len(orphans) != 1 {
		t.Fatalf("FindOrphans = (%v, %v), want one orphan", orphans, err)
	}
	res := Reconcile(context.Background(), m, a, orphans)
	if len(res) != 1 || res[0].Outcome != OutcomeCancelled || res[0].ParentRunID != p.ID {
		t.Fatalf("Reconcile = %+v", res)
	}
	rows := a.rowsFor(t, c.ID)
	if len(rows) != 1 || rows[0]["reason"] != ReasonParentTerminal || rows[0]["cancel_source"] != SourceBackfill ||
		rows[0]["parent_state"] != "succeeded" || rows[0]["from_state"] != "pending" {
		t.Errorf("rows = %+v", rows)
	}
}
