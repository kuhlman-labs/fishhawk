package stalesweep

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/childcancel"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

var (
	now0   = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	stale  = now0.Add(-30 * 24 * time.Hour)
	recent = now0.Add(-24 * time.Hour)
)

func testOpts() Options {
	return Options{Threshold: 14 * 24 * time.Hour, Now: func() time.Time { return now0 }}
}

// memRuns is an in-memory RunStore over run.BaseFake. Like Postgres it copies
// on read, TransitionRun succeeds on a same-state call, and
// TransitionStageFrom refuses with StageStateChangedError when the pinned
// from-state differs.
type memRuns struct {
	run.BaseFake
	mu             sync.Mutex
	runs           map[uuid.UUID]*run.Run
	stages         map[uuid.UUID][]*run.Stage
	stageErr       map[uuid.UUID]error
	getErr         map[uuid.UUID]error
	transitionErr  map[uuid.UUID]error
	failOnTarget   map[uuid.UUID]run.State
	listErr        error
	cascadeListErr error
	// cascadeListOK is how many DecomposedFrom ListRuns calls succeed before
	// cascadeListErr applies (0: it applies to the first).
	cascadeListOK    int
	decomposedListed int
	seq              int
}

func newMemRuns() *memRuns {
	return &memRuns{
		runs: map[uuid.UUID]*run.Run{}, stages: map[uuid.UUID][]*run.Stage{}, stageErr: map[uuid.UUID]error{},
		getErr: map[uuid.UUID]error{}, transitionErr: map[uuid.UUID]error{}, failOnTarget: map[uuid.UUID]run.State{},
	}
}

func (m *memRuns) addRun(state run.State, updated time.Time, parent *uuid.UUID, prURL string) *run.Run {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	r := &run.Run{ID: uuid.New(), State: state, DecomposedFrom: parent, CreatedAt: time.Unix(int64(m.seq), 0), UpdatedAt: updated}
	if prURL != "" {
		u := prURL
		r.PullRequestURL = &u
	}
	m.runs[r.ID] = r
	c := *r
	return &c
}

func (m *memRuns) addStage(runID uuid.UUID, typ run.StageType, state run.StageState, updated time.Time) *run.Stage {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := &run.Stage{ID: uuid.New(), RunID: runID, Sequence: len(m.stages[runID]), Type: typ, State: state, UpdatedAt: updated}
	m.stages[runID] = append(m.stages[runID], s)
	return s
}

func (m *memRuns) state(id uuid.UUID) run.State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runs[id].State
}

func (m *memRuns) stageState(runID, stageID uuid.UUID) run.StageState {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.stages[runID] {
		if s.ID == stageID {
			return s.State
		}
	}
	return ""
}

// bumpStage moves the updated_at of runID's stage at index i.
func (m *memRuns) bumpStage(runID uuid.UUID, i int, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stages[runID][i].UpdatedAt = at
}

func (m *memRuns) mutate(id uuid.UUID, f func(r *run.Run)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f(m.runs[id])
}

func (m *memRuns) GetRun(_ context.Context, id uuid.UUID) (*run.Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
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
	if f.DecomposedFrom != nil && m.cascadeListErr != nil {
		if m.decomposedListed >= m.cascadeListOK {
			return nil, m.cascadeListErr
		}
		m.decomposedListed++
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
	var out []*run.Stage
	for _, s := range m.stages[runID] {
		c := *s
		out = append(out, &c)
	}
	return out, nil
}

func (m *memRuns) TransitionRun(_ context.Context, id uuid.UUID, to run.State) (*run.Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.transitionErr[id]; err != nil {
		return nil, err
	}
	if t, ok := m.failOnTarget[id]; ok && t == to {
		return nil, fmt.Errorf("injected failure on %s", to)
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

func (m *memRuns) TransitionStageFrom(_ context.Context, id uuid.UUID, from, to run.StageState, _ *run.StageCompletion) (*run.Stage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ss := range m.stages {
		for _, s := range ss {
			if s.ID != id {
				continue
			}
			if s.State != from {
				return nil, run.StageStateChangedError{StageID: id, Expected: from, Actual: s.State}
			}
			s.State = to
			c := *s
			return &c, nil
		}
	}
	return nil, run.ErrNotFound
}

// memAudit is an in-memory AuditStore WITHOUT the deduped capability, so it
// drives the fallback list-then-append leg.
type memAudit struct {
	mu        sync.Mutex
	rows      []*audit.Entry
	appendErr error
	listErr   map[uuid.UUID]error
}

func newMemAudit() *memAudit { return &memAudit{listErr: map[uuid.UUID]error{}} }

func (a *memAudit) seed(runID uuid.UUID, category string, ts time.Time, payload map[string]any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	b, _ := json.Marshal(payload)
	rid := runID
	a.rows = append(a.rows, &audit.Entry{RunID: &rid, Category: category, Timestamp: ts, Payload: b})
}

func (a *memAudit) AppendChained(_ context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.appendErr != nil {
		return nil, a.appendErr
	}
	rid := p.RunID
	e := &audit.Entry{RunID: &rid, Category: p.Category, Timestamp: p.Timestamp, ActorKind: p.ActorKind, Payload: p.Payload}
	a.rows = append(a.rows, e)
	return e, nil
}

func (a *memAudit) ListForRun(_ context.Context, runID uuid.UUID) ([]*audit.Entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.listErr[runID]; err != nil {
		return nil, err
	}
	var out []*audit.Entry
	for _, e := range a.rows {
		if *e.RunID == runID {
			out = append(out, e)
		}
	}
	return out, nil
}

func (a *memAudit) ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	all, err := a.ListForRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	var out []*audit.Entry
	for _, e := range all {
		if e.Category == category {
			out = append(out, e)
		}
	}
	return out, nil
}

// swept decodes every stale_run_swept row on runID's chain.
func (a *memAudit) swept(t *testing.T, runID uuid.UUID) []map[string]any {
	t.Helper()
	es, _ := a.ListForRunByCategory(context.Background(), runID, Category)
	var out []map[string]any
	for _, e := range es {
		var m map[string]any
		if err := json.Unmarshal(e.Payload, &m); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		out = append(out, m)
	}
	return out
}

// dedupAudit adds the audit.DedupedChainAppender capability with a real scan.
type dedupAudit struct{ *memAudit }

func (d dedupAudit) AppendChainedDeduped(ctx context.Context, p audit.ChainAppendParams, spec audit.DedupeSpec) (*audit.Entry, error) {
	es, _ := d.ListForRunByCategory(ctx, p.RunID, p.Category)
	for _, e := range es {
		var m map[string]any
		if json.Unmarshal(e.Payload, &m) == nil && m[spec.PayloadKey] == spec.PayloadValue {
			return nil, &audit.DedupedDuplicateError{Existing: e}
		}
	}
	return d.AppendChained(ctx, p)
}

var _ audit.DedupedChainAppender = dedupAudit{}

type fakeProbe struct {
	rep ProbeReport
	err error
}

func (f fakeProbe) LiveRunners(context.Context) (ProbeReport, error) { return f.rep, f.err }

var noRunners = fakeProbe{rep: ProbeReport{RunIDs: map[uuid.UUID]bool{}}}

// sweep runs Find then Apply and fails the test on a scan error.
func sweep(t *testing.T, m *memRuns, a AuditStore, probe RunnerProbe, o Options) ([]Candidate, []Result) {
	t.Helper()
	cands, _, err := Find(context.Background(), m, a, probe, o)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	return cands, Apply(context.Background(), m, a, cands, o)
}

func candidateFor(t *testing.T, cands []Candidate, id uuid.UUID) Candidate {
	t.Helper()
	for _, c := range cands {
		if c.Run.ID == id {
			return c
		}
	}
	t.Fatalf("no candidate for run %s", id)
	return Candidate{}
}

func resultFor(t *testing.T, res []Result, id uuid.UUID) Result {
	t.Helper()
	for _, r := range res {
		if r.RunID == id {
			return r
		}
	}
	t.Fatalf("no result for run %s", id)
	return Result{}
}

// staleRunning seeds a running run whose every activity signal is 30 days old,
// with one pending implement stage (not settled) and no PR evidence: left
// alone, it classifies abandoned and Apply cancels it.
func staleRunning(m *memRuns, a *memAudit) *run.Run {
	r := m.addRun(run.StateRunning, stale, nil, "")
	m.addStage(r.ID, run.StageTypeImplement, run.StageStatePending, stale)
	a.seed(r.ID, "run_created", stale, nil)
	return r
}

// assertUntouched: the run kept its state and carries no stale_run_swept row.
func assertUntouched(t *testing.T, m *memRuns, a *memAudit, id uuid.UUID, want run.State) {
	t.Helper()
	if got := m.state(id); got != want {
		t.Errorf("run %s state = %s, want %s (untouched)", id, got, want)
	}
	if n := len(a.swept(t, id)); n != 0 {
		t.Errorf("run %s carries %d stale_run_swept rows, want 0", id, n)
	}
}

// TestFind_FreshBy: each fixture has exactly ONE activity signal inside the
// threshold, so dropping that leg from lastActivity's max() makes it abandoned
// and cancelled.
func TestFind_FreshBy(t *testing.T) {
	for name, bump := range map[string]func(m *memRuns, a *memAudit, r *run.Run){
		"RunUpdatedAt": func(m *memRuns, _ *memAudit, r *run.Run) { m.mutate(r.ID, func(x *run.Run) { x.UpdatedAt = recent }) },
		"StageUpdatedAt": func(m *memRuns, _ *memAudit, r *run.Run) {
			m.addStage(r.ID, run.StageTypeReview, run.StageStatePending, recent)
		},
		"AuditTimestamp": func(_ *memRuns, a *memAudit, r *run.Run) { a.seed(r.ID, "stage_heartbeat", recent, nil) },
	} {
		t.Run(name, func(t *testing.T) {
			m, a := newMemRuns(), newMemAudit()
			r := staleRunning(m, a)
			bump(m, a, r)
			cands, res := sweep(t, m, a, noRunners, testOpts())
			if c := candidateFor(t, cands, r.ID); c.Class != ClassFresh || c.Action != ActionSkip {
				t.Errorf("class/action = %s/%s, want fresh/skip", c.Class, c.Action)
			}
			if len(res) != 0 {
				t.Errorf("Apply results = %+v, want none", res)
			}
			assertUntouched(t, m, a, r.ID, run.StateRunning)
		})
	}
}

func TestFind_LiveRunnerSkipped(t *testing.T) {
	m, a := newMemRuns(), newMemAudit()
	r := staleRunning(m, a)
	probe := fakeProbe{rep: ProbeReport{RunIDs: map[uuid.UUID]bool{r.ID: true}}}
	cands, _ := sweep(t, m, a, probe, testOpts())
	if c := candidateFor(t, cands, r.ID); c.Class != ClassLiveRunner || c.Action != ActionSkip {
		t.Errorf("class/action = %s/%s, want live_runner/skip", c.Class, c.Action)
	}
	assertUntouched(t, m, a, r.ID, run.StateRunning)
}

// TestFind_ProbeErrorFolded: a failed (or absent) probe does not fail the
// scan; it lands in ProbeReport.Err and no run is classified live_runner.
func TestFind_ProbeErrorFolded(t *testing.T) {
	for name, probe := range map[string]RunnerProbe{
		"probe error": fakeProbe{err: errors.New("ps: not found")},
		"nil probe":   nil,
	} {
		t.Run(name, func(t *testing.T) {
			m, a := newMemRuns(), newMemAudit()
			r := staleRunning(m, a)
			cands, rep, err := Find(context.Background(), m, a, probe, testOpts())
			if err != nil || rep.Err == nil || len(rep.RunIDs) != 0 {
				t.Fatalf("Find err = %v, report = %+v; want nil scan error and a probe error", err, rep)
			}
			if c := candidateFor(t, cands, r.ID); c.Class != ClassAbandoned {
				t.Errorf("class = %s, want abandoned", c.Class)
			}
		})
	}
}

func TestFind_DecompositionChildExcluded(t *testing.T) {
	m, a := newMemRuns(), newMemAudit()
	parent := uuid.New() // not a pending/running run, so no cascade can reach the child
	child := m.addRun(run.StateRunning, stale, &parent, "")
	m.addStage(child.ID, run.StageTypeImplement, run.StageStatePending, stale)
	cands, res := sweep(t, m, a, noRunners, testOpts())
	if len(cands) != 0 || len(res) != 0 {
		t.Errorf("candidates = %d, results = %d; want the decomposition child excluded", len(cands), len(res))
	}
	assertUntouched(t, m, a, child.ID, run.StateRunning)
}

// TestFind_ChildActivityKeepsParent: a decomposed parent parked at
// awaiting_children has a quiet chain of its own while its children progress,
// and a child's runner carries the CHILD's --run-id. Each fixture's only
// non-stale signal (or only live runner) is on the child, so dropping the
// child half of readEvidence/classify makes the parent abandoned, and the
// cancel cascade then cancels the busy child.
func TestFind_ChildActivityKeepsParent(t *testing.T) {
	for name, tc := range map[string]struct {
		seed  func(m *memRuns, a *memAudit, child *run.Run)
		live  bool
		class Class
	}{
		"child run updated_at": {seed: func(m *memRuns, _ *memAudit, ch *run.Run) {
			m.mutate(ch.ID, func(x *run.Run) { x.UpdatedAt = recent })
		}, class: ClassFresh},
		"child stage updated_at": {seed: func(m *memRuns, _ *memAudit, ch *run.Run) {
			m.addStage(ch.ID, run.StageTypeImplement, run.StageStateRunning, recent)
		}, class: ClassFresh},
		"child audit entry": {seed: func(_ *memRuns, a *memAudit, ch *run.Run) {
			a.seed(ch.ID, "stage_heartbeat", recent, nil)
		}, class: ClassFresh},
		"child live runner": {live: true, class: ClassLiveRunner},
	} {
		t.Run(name, func(t *testing.T) {
			m, a := newMemRuns(), newMemAudit()
			parent := m.addRun(run.StateRunning, stale, nil, "")
			m.addStage(parent.ID, run.StageTypeImplement, run.StageStateAwaitingChildren, stale)
			a.seed(parent.ID, "run_created", stale, nil)
			child := m.addRun(run.StateRunning, stale, &parent.ID, "")
			if tc.seed != nil {
				tc.seed(m, a, child)
			}
			probe := noRunners
			if tc.live {
				probe = fakeProbe{rep: ProbeReport{RunIDs: map[uuid.UUID]bool{child.ID: true}}}
			}
			cands, res := sweep(t, m, a, probe, testOpts())
			if c := candidateFor(t, cands, parent.ID); c.Class != tc.class || c.Action != ActionSkip {
				t.Errorf("parent class/action = %s/%s, want %s/skip", c.Class, c.Action, tc.class)
			}
			if len(res) != 0 {
				t.Errorf("Apply results = %+v, want none", res)
			}
			assertUntouched(t, m, a, parent.ID, run.StateRunning)
			if s := m.state(child.ID); s != run.StateRunning {
				t.Errorf("child state = %s, want running (untouched)", s)
			}
			if es, _ := a.ListForRunByCategory(context.Background(), child.ID, childcancel.Category); len(es) != 0 {
				t.Errorf("child carries %d %s rows, want 0", len(es), childcancel.Category)
			}
		})
	}
}

func TestFind_StagesSettled(t *testing.T) {
	type stg struct {
		typ   run.StageType
		state run.StageState
	}
	plan, impl, review := run.StageTypePlan, run.StageTypeImplement, run.StageTypeReview
	for name, tc := range map[string]struct {
		from     run.State
		stages   []stg
		evidence string // audit category seeded (stale), or ""
		prURL    string
		class    Class
		action   Action
		want     run.State
		path     []string
	}{
		"pending all succeeded": {run.StatePending, []stg{{plan, run.StageStateSucceeded}, {impl, run.StageStateSucceeded}, {review, run.StageStateSucceeded}},
			"", "", ClassStagesSettled, ActionReconcileSucceeded, run.StateSucceeded, []string{"running", "succeeded"}},
		"running implement failed review pending": {run.StateRunning, []stg{{plan, run.StageStateSucceeded}, {impl, run.StageStateFailed}, {review, run.StageStatePending}},
			"", "", ClassStagesSettled, ActionReconcileFailed, run.StateFailed, []string{"failed"}},
		"pending plan failed rest pending": {run.StatePending, []stg{{plan, run.StageStateFailed}, {impl, run.StageStatePending}, {review, run.StageStatePending}},
			"", "", ClassStagesSettled, ActionReconcileFailed, run.StateFailed, []string{"failed"}},
		"cancelled stage": {run.StateRunning, []stg{{plan, run.StageStateSucceeded}, {impl, run.StageStateCancelled}, {review, run.StageStatePending}},
			"", "", ClassStagesSettled, ActionReconcileCancelled, run.StateCancelled, []string{"cancelled"}},
		"gated stage before a failed one (Advance walks past it)": {run.StateRunning, []stg{{impl, run.StageStateAwaitingApproval}, {review, run.StageStateFailed}},
			"", "", ClassStagesSettled, ActionReconcileFailed, run.StateFailed, []string{"failed"}},
		"not settled: review awaiting_approval": {run.StateRunning, []stg{{plan, run.StageStateSucceeded}, {impl, run.StageStateSucceeded}, {review, run.StageStateAwaitingApproval}},
			"", "", ClassAbandoned, ActionCancel, run.StateCancelled, []string{"cancelled"}},
		"no stages is not settled": {run.StateRunning, nil,
			"", "", ClassAbandoned, ActionCancel, run.StateCancelled, []string{"cancelled"}},
		// C5: a succeeded target never overrides closed-unmerged or
		// unobserved PR evidence.
		"all succeeded but PR closed unmerged": {run.StateRunning, []stg{{plan, run.StageStateSucceeded}, {impl, run.StageStateSucceeded}},
			CategoryPRClosedWithoutMerge, "", ClassPRClosed, ActionCancel, run.StateCancelled, []string{"cancelled"}},
		"all succeeded but PR opened unobserved": {run.StateRunning, []stg{{plan, run.StageStateSucceeded}, {impl, run.StageStateSucceeded}},
			"", "https://github.com/x/y/pull/1", ClassPRUnobserved, ActionSkip, run.StateRunning, nil},
		"all succeeded and merged stays settled": {run.StateRunning, []stg{{plan, run.StageStateSucceeded}, {impl, run.StageStateSucceeded}},
			CategoryPRMerged, "", ClassStagesSettled, ActionReconcileSucceeded, run.StateSucceeded, []string{"succeeded"}},
		"failed target wins over closed PR": {run.StateRunning, []stg{{plan, run.StageStateSucceeded}, {impl, run.StageStateFailed}},
			CategoryPRClosedWithoutMerge, "", ClassStagesSettled, ActionReconcileFailed, run.StateFailed, []string{"failed"}},
	} {
		t.Run(name, func(t *testing.T) {
			m, a := newMemRuns(), newMemAudit()
			r := m.addRun(tc.from, stale, nil, tc.prURL)
			for _, s := range tc.stages {
				m.addStage(r.ID, s.typ, s.state, stale)
			}
			if tc.evidence != "" {
				a.seed(r.ID, tc.evidence, stale, nil)
			}
			cands, res := sweep(t, m, a, noRunners, testOpts())
			c := candidateFor(t, cands, r.ID)
			if c.Class != tc.class || c.Action != tc.action {
				t.Fatalf("class/action = %s/%s, want %s/%s", c.Class, c.Action, tc.class, tc.action)
			}
			if got := m.state(r.ID); got != tc.want {
				t.Errorf("state after Apply = %s, want %s", got, tc.want)
			}
			if tc.path == nil {
				if len(res) != 0 || len(a.swept(t, r.ID)) != 0 {
					t.Errorf("results = %+v; want none and no row", res)
				}
				return
			}
			rows := a.swept(t, r.ID)
			if len(rows) != 1 {
				t.Fatalf("rows = %d, want 1", len(rows))
			}
			if got := fmt.Sprint(rows[0]["transition_path"]); got != fmt.Sprint(tc.path) {
				t.Errorf("transition_path = %s, want %v", got, tc.path)
			}
			if rows[0]["class"] != string(tc.class) || rows[0]["from_state"] != string(tc.from) || rows[0]["to_state"] != string(tc.want) {
				t.Errorf("row = %v", rows[0])
			}
		})
	}
}

func TestFind_MergedDelegated(t *testing.T) {
	for _, cat := range []string{CategoryPRMerged, CategoryPostMergeObserved, CategoryMergeObservationRecorded} {
		t.Run(cat, func(t *testing.T) {
			m, a := newMemRuns(), newMemAudit()
			r := staleRunning(m, a)
			a.seed(r.ID, CategoryPullRequestOpened, stale, nil)
			a.seed(r.ID, cat, stale, nil)
			cands, res := sweep(t, m, a, noRunners, testOpts())
			c := candidateFor(t, cands, r.ID)
			if c.Class != ClassMerged || c.Action != ActionDelegateReconcileMerge || c.PREvidence != PREvidenceMerged || c.Target != "" {
				t.Errorf("candidate = %s/%s/%s/%q, want merged/delegate_reconcile_merge/merged/\"\"", c.Class, c.Action, c.PREvidence, c.Target)
			}
			if len(res) != 0 {
				t.Errorf("results = %+v, want none", res)
			}
			assertUntouched(t, m, a, r.ID, run.StateRunning)
		})
	}
}

func TestFind_PRClosedCancelled(t *testing.T) {
	m, a := newMemRuns(), newMemAudit()
	r := staleRunning(m, a)
	a.seed(r.ID, CategoryPRClosedWithoutMerge, stale, nil)
	cands, _ := sweep(t, m, a, noRunners, testOpts())
	if c := candidateFor(t, cands, r.ID); c.Class != ClassPRClosed || c.Reason != ReasonPRClosedWithoutMerge {
		t.Errorf("class/reason = %s/%s, want pr_closed/pr_closed_without_merge", c.Class, c.Reason)
	}
	if got := m.state(r.ID); got != run.StateCancelled {
		t.Errorf("state = %s, want cancelled", got)
	}
	if rows := a.swept(t, r.ID); len(rows) != 1 || rows[0]["reason"] != ReasonPRClosedWithoutMerge || rows[0]["pr_evidence"] != "closed" {
		t.Errorf("rows = %v", rows)
	}
}

func TestFind_PRUnobserved(t *testing.T) {
	seeds := map[string]func(m *memRuns, a *memAudit) *run.Run{
		"pull_request_url": func(m *memRuns, a *memAudit) *run.Run {
			r := m.addRun(run.StateRunning, stale, nil, "https://github.com/x/y/pull/7")
			m.addStage(r.ID, run.StageTypeReview, run.StageStateAwaitingApproval, stale)
			return r
		},
		"pull_request_opened row": func(m *memRuns, a *memAudit) *run.Run {
			r := staleRunning(m, a)
			a.seed(r.ID, CategoryPullRequestOpened, stale, nil)
			return r
		},
	}
	for name, seed := range seeds {
		t.Run(name+"/default skips", func(t *testing.T) {
			m, a := newMemRuns(), newMemAudit()
			r := seed(m, a)
			cands, res := sweep(t, m, a, noRunners, testOpts())
			if c := candidateFor(t, cands, r.ID); c.Class != ClassPRUnobserved || c.Action != ActionSkip || c.PREvidence != PREvidenceOpened {
				t.Errorf("candidate = %s/%s/%s, want pr_unobserved/skip/opened", c.Class, c.Action, c.PREvidence)
			}
			if len(res) != 0 {
				t.Errorf("results = %+v, want none", res)
			}
			assertUntouched(t, m, a, r.ID, run.StateRunning)
		})
		t.Run(name+"/--cancel-unobserved-pr cancels", func(t *testing.T) {
			m, a := newMemRuns(), newMemAudit()
			r := seed(m, a)
			o := testOpts()
			o.CancelUnobservedPR = true
			cands, _ := sweep(t, m, a, noRunners, o)
			if c := candidateFor(t, cands, r.ID); c.Class != ClassPRUnobserved || c.Action != ActionCancel || c.Reason != ReasonPRUnobserved {
				t.Errorf("candidate = %s/%s/%s", c.Class, c.Action, c.Reason)
			}
			if got := m.state(r.ID); got != run.StateCancelled {
				t.Errorf("state = %s, want cancelled", got)
			}
			rows := a.swept(t, r.ID)
			if len(rows) != 1 || rows[0]["reason"] != ReasonPRUnobserved {
				t.Fatalf("rows = %v", rows)
			}
			if r.PullRequestURL != nil && rows[0]["pull_request_url"] != *r.PullRequestURL {
				t.Errorf("pull_request_url = %v, want %s", rows[0]["pull_request_url"], *r.PullRequestURL)
			}
		})
	}
}

// TestFind_FailsClosed: every read error fails the whole scan with zero
// candidates, so Apply (driven on whatever Find returned) transitions nothing.
func TestFind_FailsClosed(t *testing.T) {
	for name, inject := range map[string]func(m *memRuns, a *memAudit, r *run.Run){
		"ListRunsError":  func(m *memRuns, _ *memAudit, _ *run.Run) { m.listErr = errors.New("list boom") },
		"StageReadError": func(m *memRuns, _ *memAudit, r *run.Run) { m.stageErr[r.ID] = errors.New("stage boom") },
		"AuditReadError": func(_ *memRuns, a *memAudit, r *run.Run) { a.listErr[r.ID] = errors.New("audit boom") },
		"ChildListError": func(m *memRuns, _ *memAudit, _ *run.Run) { m.cascadeListErr = errors.New("children boom") },
		"ChildStageReadError": func(m *memRuns, _ *memAudit, r *run.Run) {
			ch := m.addRun(run.StateRunning, stale, &r.ID, "")
			m.stageErr[ch.ID] = errors.New("child stage boom")
		},
		"ChildAuditReadError": func(m *memRuns, a *memAudit, r *run.Run) {
			ch := m.addRun(run.StateRunning, stale, &r.ID, "")
			a.listErr[ch.ID] = errors.New("child audit boom")
		},
	} {
		t.Run(name, func(t *testing.T) {
			m, a := newMemRuns(), newMemAudit()
			other := staleRunning(m, a)
			r := staleRunning(m, a)
			inject(m, a, r)
			cands, _, err := Find(context.Background(), m, a, noRunners, testOpts())
			if err == nil || !strings.Contains(err.Error(), "boom") || len(cands) != 0 {
				t.Fatalf("Find = %d candidates, err %v; want 0 and the read error", len(cands), err)
			}
			Apply(context.Background(), m, a, cands, testOpts())
			for _, id := range []uuid.UUID{r.ID, other.ID} {
				assertUntouched(t, m, a, id, run.StateRunning)
			}
		})
	}
}

func TestFind_PagesPast100(t *testing.T) {
	m, a := newMemRuns(), newMemAudit()
	for i := 0; i < 150; i++ {
		staleRunning(m, a)
	}
	for i := 0; i < 101; i++ {
		r := m.addRun(run.StatePending, stale, nil, "")
		m.addStage(r.ID, run.StageTypePlan, run.StageStatePending, stale)
	}
	cands, _, err := Find(context.Background(), m, a, noRunners, testOpts())
	if err != nil || len(cands) != 251 {
		t.Fatalf("candidates = %d (err %v), want 251", len(cands), err)
	}
	seen := map[uuid.UUID]bool{}
	for _, c := range cands {
		seen[c.Run.ID] = true
	}
	if len(seen) != 251 {
		t.Errorf("distinct candidates = %d, want 251", len(seen))
	}
}

// TestApply_ReReadTerminalSkips: the snapshot says running but the run went
// cancelled since the scan. Without the re-read's terminal check the
// same-state TransitionRun would succeed and append a row.
func TestApply_ReReadTerminalSkips(t *testing.T) {
	m, a := newMemRuns(), newMemAudit()
	r := staleRunning(m, a)
	cands, _, _ := Find(context.Background(), m, a, noRunners, testOpts())
	m.mutate(r.ID, func(x *run.Run) { x.State = run.StateCancelled })
	res := Apply(context.Background(), m, a, cands, testOpts())
	if got := resultFor(t, res, r.ID); got.Outcome != OutcomeSkippedTerminal || got.Appended || got.Err != nil || len(got.TransitionPath) != 0 {
		t.Errorf("result = %+v, want skipped_terminal with no transition and no row", got)
	}
	if n := len(a.swept(t, r.ID)); n != 0 {
		t.Errorf("rows = %d, want 0", n)
	}
}

func TestApply_ChangedSinceScanSkips(t *testing.T) {
	// The run's updated_at starts and ends BELOW the stage/audit signals, so
	// only the run-updated_at leg sees the move.
	t.Run("updated_at moved but still stale", func(t *testing.T) {
		m, a := newMemRuns(), newMemAudit()
		r := staleRunning(m, a)
		m.mutate(r.ID, func(x *run.Run) { x.UpdatedAt = stale.Add(-48 * time.Hour) })
		cands, _, _ := Find(context.Background(), m, a, noRunners, testOpts())
		m.mutate(r.ID, func(x *run.Run) { x.UpdatedAt = stale.Add(-24 * time.Hour) })
		res := Apply(context.Background(), m, a, cands, testOpts())
		if got := resultFor(t, res, r.ID); got.Outcome != OutcomeSkippedChanged {
			t.Errorf("outcome = %s, want skipped_changed", got.Outcome)
		}
		assertUntouched(t, m, a, r.ID, run.StateRunning)
	})
	t.Run("updated_at now inside the threshold", func(t *testing.T) {
		m, a := newMemRuns(), newMemAudit()
		r := staleRunning(m, a)
		cands, _, _ := Find(context.Background(), m, a, noRunners, testOpts())
		m.mutate(r.ID, func(x *run.Run) { x.UpdatedAt = now0 })
		res := Apply(context.Background(), m, a, cands, testOpts())
		if got := resultFor(t, res, r.ID); got.Outcome != OutcomeSkippedChanged {
			t.Errorf("outcome = %s, want skipped_changed", got.Outcome)
		}
		assertUntouched(t, m, a, r.ID, run.StateRunning)
	})
	t.Run("hand-built fresh snapshot (threshold leg alone)", func(t *testing.T) {
		m, a := newMemRuns(), newMemAudit()
		r := m.addRun(run.StateRunning, recent, nil, "")
		c := Candidate{Run: r, Class: ClassAbandoned, Action: ActionCancel, Target: run.StateCancelled, Reason: ReasonStaleSweep, LastActivity: recent}
		res := Apply(context.Background(), m, a, []Candidate{c}, testOpts())
		if got := resultFor(t, res, r.ID); got.Outcome != OutcomeSkippedChanged {
			t.Errorf("outcome = %s, want skipped_changed", got.Outcome)
		}
		assertUntouched(t, m, a, r.ID, run.StateRunning)
	})
	// Activity that lands on a stage, the chain or a child between Find and
	// Apply, with run.updated_at untouched: only the evidence re-read sees it.
	for name, bump := range map[string]func(m *memRuns, a *memAudit, r *run.Run, child *run.Run){
		"stage updated_at moved but still stale": func(m *memRuns, _ *memAudit, r *run.Run, _ *run.Run) {
			m.bumpStage(r.ID, 0, stale.Add(time.Hour))
		},
		"stage heartbeat inside the threshold": func(m *memRuns, _ *memAudit, r *run.Run, _ *run.Run) {
			m.bumpStage(r.ID, 0, recent)
		},
		"audit entry after the scan": func(_ *memRuns, a *memAudit, r *run.Run, _ *run.Run) {
			a.seed(r.ID, "stage_heartbeat", stale.Add(time.Hour), nil)
		},
		"child activity after the scan": func(m *memRuns, _ *memAudit, _ *run.Run, child *run.Run) {
			m.mutate(child.ID, func(x *run.Run) { x.UpdatedAt = stale.Add(time.Hour) })
		},
	} {
		t.Run(name, func(t *testing.T) {
			m, a := newMemRuns(), newMemAudit()
			r := staleRunning(m, a)
			child := m.addRun(run.StateRunning, stale, &r.ID, "")
			cands, _, _ := Find(context.Background(), m, a, noRunners, testOpts())
			if c := candidateFor(t, cands, r.ID); c.Class != ClassAbandoned {
				t.Fatalf("scan class = %s, want abandoned", c.Class)
			}
			bump(m, a, r, child)
			res := Apply(context.Background(), m, a, cands, testOpts())
			if got := resultFor(t, res, r.ID); got.Outcome != OutcomeSkippedChanged {
				t.Errorf("outcome = %s, want skipped_changed", got.Outcome)
			}
			assertUntouched(t, m, a, r.ID, run.StateRunning)
			if s := m.state(child.ID); s != run.StateRunning {
				t.Errorf("child state = %s, want running", s)
			}
		})
	}
}

func TestApply_ReReadError(t *testing.T) {
	for name, inject := range map[string]func(m *memRuns, r *run.Run){
		"get boom":   func(m *memRuns, r *run.Run) { m.getErr[r.ID] = errors.New("get boom") },
		"stage boom": func(m *memRuns, r *run.Run) { m.stageErr[r.ID] = errors.New("stage boom") },
	} {
		t.Run(name, func(t *testing.T) {
			m, a := newMemRuns(), newMemAudit()
			r := staleRunning(m, a)
			cands, _, _ := Find(context.Background(), m, a, noRunners, testOpts())
			inject(m, r)
			got := resultFor(t, Apply(context.Background(), m, a, cands, testOpts()), r.ID)
			if got.Outcome != OutcomeFailed || got.Err == nil || !strings.Contains(got.Err.Error(), name) {
				t.Errorf("result = %+v, want failed carrying the read error", got)
			}
			assertUntouched(t, m, a, r.ID, run.StateRunning)
		})
	}
}

func TestApply_TransitionError(t *testing.T) {
	m, a := newMemRuns(), newMemAudit()
	r := staleRunning(m, a)
	m.transitionErr[r.ID] = errors.New("db down")
	_, res := sweep(t, m, a, noRunners, testOpts())
	got := resultFor(t, res, r.ID)
	if got.Outcome != OutcomeFailed || got.Err == nil || !strings.Contains(got.Err.Error(), "db down") {
		t.Errorf("result = %+v, want failed with the transition error", got)
	}
	assertUntouched(t, m, a, r.ID, run.StateRunning)
}

func TestApply_InvalidTransitionRace(t *testing.T) {
	m, a := newMemRuns(), newMemAudit()
	r := staleRunning(m, a)
	m.transitionErr[r.ID] = run.InvalidTransitionError{Kind: "run", From: "succeeded", To: "cancelled"}
	_, res := sweep(t, m, a, noRunners, testOpts())
	if got := resultFor(t, res, r.ID); got.Outcome != OutcomeSkippedTerminal || got.Err != nil || got.Appended {
		t.Errorf("result = %+v, want skipped_terminal, no error, no row", got)
	}
	assertUntouched(t, m, a, r.ID, run.StateRunning)
}

func TestApply_AppendError(t *testing.T) {
	m, a := newMemRuns(), newMemAudit()
	r := staleRunning(m, a)
	a.appendErr = errors.New("append boom")
	_, res := sweep(t, m, a, noRunners, testOpts())
	got := resultFor(t, res, r.ID)
	if got.Outcome != OutcomeTransitioned || got.Appended || got.Err == nil || !strings.Contains(got.Err.Error(), "append boom") {
		t.Errorf("result = %+v, want transitioned with the append error", got)
	}
	if s := m.state(r.ID); s != run.StateCancelled {
		t.Errorf("state = %s, want cancelled (the transition landed)", s)
	}
}

func TestApply_Dedup(t *testing.T) {
	for name, wrap := range map[string]func(*memAudit) AuditStore{
		"Deduped":  func(a *memAudit) AuditStore { return dedupAudit{a} },
		"Fallback": func(a *memAudit) AuditStore { return a },
	} {
		t.Run(name, func(t *testing.T) {
			m, a := newMemRuns(), newMemAudit()
			r := staleRunning(m, a)
			a.seed(r.ID, Category, stale, map[string]any{"source": Source})
			_, res := sweep(t, m, wrap(a), noRunners, testOpts())
			got := resultFor(t, res, r.ID)
			if got.Outcome != OutcomeTransitioned || got.Appended || got.Err != nil {
				t.Errorf("result = %+v, want transitioned, not appended, no error", got)
			}
			if n := len(a.swept(t, r.ID)); n != 1 {
				t.Errorf("rows = %d, want 1 (the pre-existing one)", n)
			}
		})
	}
	t.Run("Deduped/first append lands", func(t *testing.T) {
		m, a := newMemRuns(), newMemAudit()
		r := staleRunning(m, a)
		_, res := sweep(t, m, dedupAudit{a}, noRunners, testOpts())
		if got := resultFor(t, res, r.ID); !got.Appended || got.Err != nil {
			t.Errorf("result = %+v, want appended", got)
		}
	})
	t.Run("Deduped/append error", func(t *testing.T) {
		m, a := newMemRuns(), newMemAudit()
		r := staleRunning(m, a)
		a.appendErr = errors.New("deduped boom")
		_, res := sweep(t, m, dedupAudit{a}, noRunners, testOpts())
		if got := resultFor(t, res, r.ID); got.Appended || got.Err == nil || !strings.Contains(got.Err.Error(), "deduped boom") {
			t.Errorf("result = %+v, want the deduped append error", got)
		}
	})
}

// TestApply_CancelsParkedStages: a cancelled run's non-terminal stages are
// CAS-cancelled and recorded; a stage whose CAS is refused (it moved since the
// scan) is left alone and OMITTED from the record.
func TestApply_CancelsParkedStages(t *testing.T) {
	m, a := newMemRuns(), newMemAudit()
	r := m.addRun(run.StateRunning, stale, nil, "")
	m.addStage(r.ID, run.StageTypePlan, run.StageStateSucceeded, stale)
	impl := m.addStage(r.ID, run.StageTypeImplement, run.StageStatePending, stale)
	review := m.addStage(r.ID, run.StageTypeReview, run.StageStateAwaitingApproval, stale)
	moved := m.addStage(r.ID, run.StageTypeAcceptance, run.StageStatePending, stale)
	cands, _, err := Find(context.Background(), m, a, noRunners, testOpts())
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	m.mu.Lock()
	m.stages[r.ID][3].State = run.StageStateRunning // moved since the scan
	m.mu.Unlock()
	got := resultFor(t, Apply(context.Background(), m, a, cands, testOpts()), r.ID)
	if got.Outcome != OutcomeTransitioned || got.Err != nil {
		t.Fatalf("result = %+v", got)
	}
	for _, s := range []*run.Stage{impl, review} {
		if st := m.stageState(r.ID, s.ID); st != run.StageStateCancelled {
			t.Errorf("stage %s state = %s, want cancelled", s.Type, st)
		}
	}
	if st := m.stageState(r.ID, moved.ID); st != run.StageStateRunning {
		t.Errorf("moved stage state = %s, want running (CAS refused)", st)
	}
	rows := a.swept(t, r.ID)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	listed, _ := rows[0]["stages_cancelled"].([]any)
	if len(listed) != 2 {
		t.Fatalf("stages_cancelled = %v, want the two cancelled stages only", rows[0]["stages_cancelled"])
	}
	want := map[string]string{impl.ID.String(): "pending", review.ID.String(): "awaiting_approval"}
	for _, e := range listed {
		entry, _ := e.(map[string]any)
		id, _ := entry["stage_id"].(string)
		if want[id] == "" || entry["from_state"] != want[id] {
			t.Errorf("stages_cancelled entry = %v", entry)
		}
	}
}

func TestApply_CascadesToChildren(t *testing.T) {
	m, a := newMemRuns(), newMemAudit()
	parent := m.addRun(run.StateRunning, stale, nil, "")
	m.addStage(parent.ID, run.StageTypeImplement, run.StageStateAwaitingChildren, stale)
	child := m.addRun(run.StateRunning, stale, &parent.ID, "")
	_, res := sweep(t, m, a, noRunners, testOpts())
	got := resultFor(t, res, parent.ID)
	if got.Outcome != OutcomeTransitioned || got.ChildrenCancelled != 1 || got.Err != nil {
		t.Errorf("result = %+v, want transitioned with 1 child cancelled", got)
	}
	if s := m.state(child.ID); s != run.StateCancelled {
		t.Errorf("child state = %s, want cancelled", s)
	}
	es, _ := a.ListForRunByCategory(context.Background(), child.ID, childcancel.Category)
	if len(es) != 1 {
		t.Fatalf("child %s rows = %d, want 1", childcancel.Category, len(es))
	}
	var p map[string]any
	_ = json.Unmarshal(es[0].Payload, &p)
	if p["cancel_source"] != Source || p["parent_run_id"] != parent.ID.String() {
		t.Errorf("child row = %v, want cancel_source %s", p, Source)
	}
	if rows := a.swept(t, parent.ID); len(rows) != 1 || rows[0]["children_cancelled"] != float64(1) {
		t.Errorf("parent rows = %v, want children_cancelled 1", rows)
	}
}

func TestApply_CascadeErrorsRecorded(t *testing.T) {
	t.Run("list error", func(t *testing.T) {
		m, a := newMemRuns(), newMemAudit()
		r := staleRunning(m, a)
		m.cascadeListErr = errors.New("children boom")
		m.cascadeListOK = 2 // Find's and Apply's evidence reads succeed; the cascade's list fails
		got := resultFor(t, func() []Result { _, res := sweep(t, m, a, noRunners, testOpts()); return res }(), r.ID)
		if got.Outcome != OutcomeTransitioned || got.Err == nil || !strings.Contains(got.Err.Error(), "cascade: ") {
			t.Errorf("result = %+v, want transitioned carrying the cascade error", got)
		}
		if s := m.state(r.ID); s != run.StateCancelled {
			t.Errorf("state = %s, want cancelled (not undone)", s)
		}
		if n := len(a.swept(t, r.ID)); n != 1 {
			t.Errorf("rows = %d, want 1", n)
		}
	})
	t.Run("child failure", func(t *testing.T) {
		m, a := newMemRuns(), newMemAudit()
		r := staleRunning(m, a)
		child := m.addRun(run.StateRunning, stale, &r.ID, "")
		m.transitionErr[child.ID] = errors.New("child boom")
		_, res := sweep(t, m, a, noRunners, testOpts())
		got := resultFor(t, res, r.ID)
		if got.ChildrenCancelled != 0 || got.Err == nil || !strings.Contains(got.Err.Error(), "cascade child "+child.ID.String()) {
			t.Errorf("result = %+v, want the child failure recorded", got)
		}
	})
}

// TestApply_TwoStepPendingToSucceeded: pending→succeeded walks via running. A
// failure on the second step leaves the run running with a one-step path and
// no row; a re-run classifies it again and finishes it.
func TestApply_TwoStepPendingToSucceeded(t *testing.T) {
	m, a := newMemRuns(), newMemAudit()
	r := m.addRun(run.StatePending, stale, nil, "")
	m.addStage(r.ID, run.StageTypePlan, run.StageStateSucceeded, stale)
	m.failOnTarget[r.ID] = run.StateSucceeded
	_, res := sweep(t, m, a, noRunners, testOpts())
	got := resultFor(t, res, r.ID)
	if got.Outcome != OutcomeFailed || fmt.Sprint(got.TransitionPath) != "[running]" || m.state(r.ID) != run.StateRunning || len(a.swept(t, r.ID)) != 0 {
		t.Fatalf("first apply = %+v, state %s; want failed after [running], no row", got, m.state(r.ID))
	}

	delete(m.failOnTarget, r.ID)
	_, res = sweep(t, m, a, noRunners, testOpts())
	got = resultFor(t, res, r.ID)
	if got.Outcome != OutcomeTransitioned || fmt.Sprint(got.TransitionPath) != "[succeeded]" || m.state(r.ID) != run.StateSucceeded {
		t.Fatalf("re-run = %+v, want transitioned via [succeeded]", got)
	}

	m2, a2 := newMemRuns(), newMemAudit()
	r2 := m2.addRun(run.StatePending, stale, nil, "")
	m2.addStage(r2.ID, run.StageTypePlan, run.StageStateSucceeded, stale)
	sweep(t, m2, a2, noRunners, testOpts())
	rows := a2.swept(t, r2.ID)
	if len(rows) != 1 || fmt.Sprint(rows[0]["transition_path"]) != "[running succeeded]" || rows[0]["from_state"] != "pending" ||
		rows[0]["to_state"] != "succeeded" || rows[0]["threshold_days"] != float64(14) || rows[0]["source"] != Source ||
		rows[0]["pull_request_url"] != nil || rows[0]["last_activity_at"] != stale.Format(time.RFC3339Nano) {
		t.Errorf("row = %v", rows)
	}
}

func TestAction_Transitions(t *testing.T) {
	for a, want := range map[Action]bool{
		ActionSkip: false, ActionDelegateReconcileMerge: false, ActionCancel: true,
		ActionReconcileSucceeded: true, ActionReconcileFailed: true, ActionReconcileCancelled: true,
	} {
		if got := a.Transitions(); got != want {
			t.Errorf("%s.Transitions() = %v, want %v", a, got, want)
		}
	}
}

func TestOptions_DefaultClock(t *testing.T) {
	before := time.Now()
	if got := (Options{}).now(); got.Before(before) {
		t.Errorf("default clock = %v, want >= %v", got, before)
	}
}
