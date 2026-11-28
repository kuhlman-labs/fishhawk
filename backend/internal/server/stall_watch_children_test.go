package server

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concurrency"
	"github.com/kuhlman-labs/fishhawk/backend/internal/issuecomment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// ---------------------------------------------------------------------
// Fakes (V5f: the child-scanner fakes live in this file; the run, audit and
// notifier fakes are stall_watch_test.go's).
// ---------------------------------------------------------------------

// childStallHold is a HostAdmissionHold the test flips.
type childStallHold struct{ held bool }

func (h *childStallHold) Held(time.Time) (bool, string) { return h.held, "memory pressure critical" }

func (h *childStallHold) Host() string { return "" }

// childStallConcurrency answers StatusForStages from a map; every other
// concurrency.Store method panics through the nil embedded interface.
type childStallConcurrency struct {
	concurrency.Store
	mu       sync.Mutex
	statuses map[uuid.UUID]concurrency.Status
	err      error
}

func (c *childStallConcurrency) StatusForStages(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]concurrency.Status, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return nil, c.err
	}
	out := map[uuid.UUID]concurrency.Status{}
	for _, id := range ids {
		if st, ok := c.statuses[id]; ok {
			out[id] = st
		}
	}
	return out, nil
}

// childStallWriteTrap wraps the stall run repo and records every run/stage
// write the Repository surface offers, so a test can pin that the scan's
// guards stay read-only.
type childStallWriteTrap struct {
	*stallRunRepo
	mu     sync.Mutex
	writes []string
}

func (w *childStallWriteTrap) record(m string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes = append(w.writes, m)
}

func (w *childStallWriteTrap) CreateRun(context.Context, run.CreateRunParams) (*run.Run, error) {
	w.record("CreateRun")
	return nil, run.ErrNotFound
}
func (w *childStallWriteTrap) TransitionRun(context.Context, uuid.UUID, run.State) (*run.Run, error) {
	w.record("TransitionRun")
	return nil, run.ErrNotFound
}
func (w *childStallWriteTrap) RetryRun(context.Context, uuid.UUID, run.State) (*run.Run, error) {
	w.record("RetryRun")
	return nil, run.ErrNotFound
}
func (w *childStallWriteTrap) SetRunPullRequestURL(context.Context, uuid.UUID, string) (*run.Run, error) {
	w.record("SetRunPullRequestURL")
	return nil, run.ErrNotFound
}
func (w *childStallWriteTrap) CreateStage(context.Context, run.CreateStageParams) (*run.Stage, error) {
	w.record("CreateStage")
	return nil, run.ErrNotFound
}
func (w *childStallWriteTrap) TransitionStage(context.Context, uuid.UUID, run.StageState, *run.StageCompletion) (*run.Stage, error) {
	w.record("TransitionStage")
	return nil, run.ErrNotFound
}
func (w *childStallWriteTrap) RetryStage(context.Context, uuid.UUID, run.StageState) (*run.Stage, error) {
	w.record("RetryStage")
	return nil, run.ErrNotFound
}

// ---------------------------------------------------------------------
// Fixture.
// ---------------------------------------------------------------------

type childStallFixture struct {
	*stallFixture
	art    *fakeArtifactRepo
	hold   *childStallHold
	conc   *childStallConcurrency
	parent *run.Run
}

// newChildStallFixture seeds a running decomposed parent whose approved plan
// declares subDeps (one entry per slice) and whose plan_decomposed row
// carries effective_max_parallel capN. Every parent-side clock is 72h old.
func newChildStallFixture(t *testing.T, subDeps [][]int, capN int) *childStallFixture {
	t.Helper()
	base := newStallFixture(t)
	f := &childStallFixture{stallFixture: base, art: newFakeArtifactRepo(), hold: &childStallHold{},
		conc: &childStallConcurrency{statuses: map[uuid.UUID]concurrency.Status{}}}
	f.s = New(Config{Addr: "127.0.0.1:0", RunRepo: f.runs, AuditRepo: f.au, ArtifactRepo: f.art,
		HostAdmissionHold: f.hold, Concurrency: f.conc})
	f.s.issueNotifier = f.ntf
	f.parent = f.addRun(run.StateRunning, stallNow.Add(-72*time.Hour), func(r *run.Run) { r.RunnerKind = run.RunnerKindLocal })
	planStage := f.addStage(f.parent.ID, 0, run.StageTypePlan, run.StageStateSucceeded, nil)
	subs := make([]plan.SubPlanSummary, len(subDeps))
	for i, deps := range subDeps {
		subs[i] = plan.SubPlanSummary{Title: fmt.Sprintf("slice %d", i), ScopeHint: "x", DependsOn: deps}
	}
	seedPlanArtifactContent(t, f.art, planStage.ID, &plan.Plan{
		PlanVersion: "standard_v1", Summary: "s",
		Verification:  plan.Verification{TestStrategy: "t", RollbackPlan: "r"},
		Decomposition: &plan.Decomposition{Rationale: "split", SubPlans: subs},
	})
	f.au.seed(f.parent.ID, nil, "plan_decomposed", audit.ActorSystem, stallNow.Add(-72*time.Hour),
		map[string]any{"effective_max_parallel": capN})
	return f
}

// addChild mints a decomposition child at sliceIdx with one agent-executed
// implement stage, its runner_kind the un-resolved local hint it inherits.
// Its run is created, and its stage last updated, updatedAgo before stallNow.
func (f *childStallFixture) addChild(sliceIdx int, state run.State, stageState run.StageState, updatedAgo time.Duration) (*run.Run, *run.Stage) {
	pid := f.parent.ID
	idx := sliceIdx
	at := stallNow.Add(-updatedAgo)
	c := f.addRun(state, at, func(r *run.Run) {
		r.DecomposedFrom = &pid
		r.SliceIndex = &idx
		r.AutoDispatch = true
		r.RunnerKind = run.RunnerKindLocal
		r.UpdatedAt = at
	})
	st := f.addStage(c.ID, 0, run.StageTypeImplement, stageState, func(st *run.Stage) {
		st.UpdatedAt = at
		st.ExecutorKind = run.ExecutorAgent
	})
	return c, st
}

// integrate seeds a slices_integrated row on the parent covering runIDs.
func (f *childStallFixture) integrate(ago time.Duration, runIDs ...uuid.UUID) {
	ids := make([]string, 0, len(runIDs))
	for _, id := range runIDs {
		ids = append(ids, id.String())
	}
	f.au.seed(f.parent.ID, nil, "slices_integrated", audit.ActorSystem, stallNow.Add(-ago),
		map[string]any{"consolidated_branch": "fishhawk/run-consolidated", "child_run_ids": ids})
}

func (f *childStallFixture) liveWaiter(stageID uuid.UUID) {
	f.conc.mu.Lock()
	defer f.conc.mu.Unlock()
	f.conc.statuses[stageID] = concurrency.Status{State: concurrency.SlotQueued, WaiterLive: true}
}

func (f *childStallFixture) scanChildren(t *testing.T, now time.Time) StallScanReport {
	t.Helper()
	rep, err := f.s.ScanUndispatchedChildStalls(context.Background(), stallParams(now))
	if err != nil {
		t.Fatalf("ScanUndispatchedChildStalls: %v", err)
	}
	return rep
}

func (f *childStallFixture) childRows(t *testing.T) []map[string]any {
	t.Helper()
	return f.au.rows(t, CategoryChildDispatchAwaitingOperator)
}

func (f *childStallFixture) wantNoChildPage(t *testing.T) {
	t.Helper()
	if n := f.au.count(CategoryChildDispatchAwaitingOperator); n != 0 {
		t.Fatalf("%s rows = %d, want 0: %v", CategoryChildDispatchAwaitingOperator, n, f.childRows(t))
	}
	if n := f.ntf.count(); n != 0 {
		t.Fatalf("notifyPageClass calls = %d, want 0", n)
	}
}

// dispatchableWave1 is the canonical page-eligible fixture: slice 0
// succeeded 2h ago and was integrated integratedAgo before stallNow; slice 1
// depends on it and sits awaiting_host_dispatch, its clocks 2h old.
func (f *childStallFixture) dispatchableWave1(integratedAgo time.Duration) (*run.Run, *run.Stage) {
	dep, _ := f.addChild(0, run.StateSucceeded, run.StageStateSucceeded, 2*time.Hour)
	f.integrate(integratedAgo, dep.ID)
	return f.addChild(1, run.StateRunning, run.StageStateAwaitingHostDispatch, 2*time.Hour)
}

// ---------------------------------------------------------------------
// Tests.
// ---------------------------------------------------------------------

func TestScanUndispatchedChildStalls_DispatchablePastThresholdPagesOnce(t *testing.T) {
	f := newChildStallFixture(t, [][]int{nil, {0}}, 0)
	child, st := f.dispatchableWave1(31 * time.Minute)

	first := f.scanChildren(t, stallNow)
	second := f.scanChildren(t, stallNow.Add(5*time.Minute))
	if first.Paged != 1 || second.Paged != 0 {
		t.Fatalf("paged = %d then %d, want 1 then 0", first.Paged, second.Paged)
	}
	rows := f.childRows(t)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	got := rows[0]
	want := map[string]any{
		"stage_id": st.ID.String(), "child_run_id": child.ID.String(), "parent_run_id": f.parent.ID.String(),
		"next_action": StallNextActionRunChildren, "next_action_target": f.parent.ID.String(),
		"stage_state": string(run.StageStateAwaitingHostDispatch), "auto_dispatch": true,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("row[%s] = %v, want %v", k, got[k], v)
		}
	}
	if got["slice_index"] != float64(1) || got["effective_max_parallel"] != float64(0) || got["in_flight_siblings"] != float64(0) {
		t.Errorf("row counts = slice %v cap %v in-flight %v, want 1/0/0", got["slice_index"], got["effective_max_parallel"], got["in_flight_siblings"])
	}
	for _, e := range f.chain(t, child.ID) {
		if e.Category == CategoryChildDispatchAwaitingOperator && (e.StageID == nil || *e.StageID != st.ID) {
			t.Errorf("row stage_id = %v, want the implement stage %s", e.StageID, st.ID)
		}
	}
	if n := f.au.count(CategoryChildDispatchAwaitingOperator); n != 1 {
		t.Errorf("row not on the child chain only: count %d", n)
	}
	for _, e := range f.chain(t, f.parent.ID) {
		if e.Category == CategoryChildDispatchAwaitingOperator {
			t.Error("child page written on the parent chain")
		}
	}
	if n := f.ntf.count(); n != 1 {
		t.Errorf("notifyPageClass calls = %d, want 1", n)
	}
	if st2 := f.liveStages(t, child.ID)[0]; st2.State != run.StageStateAwaitingHostDispatch {
		t.Errorf("stage state = %s, want unchanged awaiting_host_dispatch", st2.State)
	}
}

// Concern 76a08b07: a child whose dispatch does not route through the
// host-dispatch marker cannot take fishhawk_run_children (a local host
// spawn), so it never pages. Each arm is otherwise the page-eligible
// dispatchableWave1 fixture, which pages with the local hint
// (_DispatchablePastThresholdPagesOnce).
func TestScanUndispatchedChildStalls_NonHostDispatchedRunnerKindNoPage(t *testing.T) {
	for _, tc := range []struct {
		name          string
		childKind     string
		childResolved bool
		parentKind    string
		parentLocked  bool
	}{
		{"child locked github_actions", run.RunnerKindGitHubActions, true, run.RunnerKindGitHubActions, true},
		{"child locked gitlab_ci", run.RunnerKindGitLabCI, true, run.RunnerKindGitLabCI, true},
		{"child hint github_actions, un-resolved", run.RunnerKindGitHubActions, false, run.RunnerKindGitHubActions, false},
		{"child hint local, parent locked github_actions", run.RunnerKindLocal, false, run.RunnerKindGitHubActions, true},
		{"child locked to an unknown kind", "nomad", true, run.RunnerKindLocal, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newChildStallFixture(t, [][]int{nil, {0}}, 0)
			f.parent.RunnerKind, f.parent.RunnerKindResolved = tc.parentKind, tc.parentLocked
			child, _ := f.dispatchableWave1(2 * time.Hour)
			child.RunnerKind, child.RunnerKindResolved = tc.childKind, tc.childResolved
			f.scanChildren(t, stallNow)
			f.wantNoChildPage(t)
		})
	}
}

// Concern 76a08b07: the marker admits only an agent-executed stage.
func TestScanUndispatchedChildStalls_NonAgentExecutorNoPage(t *testing.T) {
	f := newChildStallFixture(t, [][]int{nil, {0}}, 0)
	child, st := f.dispatchableWave1(2 * time.Hour)
	f.mutateStage(child.ID, st.ID, func(s *run.Stage) { s.ExecutorKind = run.ExecutorHuman })
	f.scanChildren(t, stallNow)
	f.wantNoChildPage(t)
}

// Concern 8b18a4a6: a terminal parent's fan-out can never dispatch the child
// (restart_blockers.go's exclusion), so the child, still pending under it,
// never pages. The running-parent control is _DispatchablePastThresholdPagesOnce.
func TestScanUndispatchedChildStalls_TerminalParentNoPage(t *testing.T) {
	for _, state := range []run.State{run.StateFailed, run.StateSucceeded, run.StateCancelled} {
		t.Run(string(state), func(t *testing.T) {
			f := newChildStallFixture(t, [][]int{nil, {0}}, 0)
			f.dispatchableWave1(2 * time.Hour)
			f.parent.State = state
			f.scanChildren(t, stallNow)
			f.wantNoChildPage(t)
		})
	}
}

func TestScanUndispatchedChildStalls_BelowThresholdNoPage(t *testing.T) {
	f := newChildStallFixture(t, [][]int{nil, {0}}, 0)
	f.dispatchableWave1(29 * time.Minute)
	f.scanChildren(t, stallNow)
	f.wantNoChildPage(t)
}

func TestScanUndispatchedChildStalls_WaveOrderBlockedNoPage(t *testing.T) {
	f := newChildStallFixture(t, [][]int{nil, {0}}, 0)
	dep, _ := f.addChild(0, run.StateRunning, run.StageStateRunning, 2*time.Hour)
	// The integration names the dependency so coverage alone would admit:
	// only the wave-order guard (dependency not succeeded) withholds.
	f.integrate(2*time.Hour, dep.ID)
	f.addChild(1, run.StateRunning, run.StageStateAwaitingHostDispatch, 2*time.Hour)
	f.scanChildren(t, stallNow)
	f.wantNoChildPage(t)
}

func TestScanUndispatchedChildStalls_UncoveredDependencyNoPage(t *testing.T) {
	f := newChildStallFixture(t, [][]int{nil, {0}}, 0)
	f.addChild(0, run.StateSucceeded, run.StageStateSucceeded, 2*time.Hour)
	f.integrate(2 * time.Hour) // a stale integration that does not cover slice 0
	f.addChild(1, run.StateRunning, run.StageStateAwaitingHostDispatch, 2*time.Hour)
	f.scanChildren(t, stallNow)
	f.wantNoChildPage(t)
}

func TestScanUndispatchedChildStalls_LiveSlotWaiterNoPage(t *testing.T) {
	f := newChildStallFixture(t, [][]int{nil, {0}}, 0)
	_, st := f.dispatchableWave1(2 * time.Hour)
	f.liveWaiter(st.ID)
	f.scanChildren(t, stallNow)
	f.wantNoChildPage(t)
}

func TestScanUndispatchedChildStalls_StaleSlotWaiterStillPages(t *testing.T) {
	f := newChildStallFixture(t, [][]int{nil, {0}}, 0)
	_, st := f.dispatchableWave1(2 * time.Hour)
	f.conc.statuses[st.ID] = concurrency.Status{State: concurrency.SlotQueued, WaiterLive: false}
	if rep := f.scanChildren(t, stallNow); rep.Paged != 1 {
		t.Fatalf("paged = %d, want 1 (a stale waiter spawns nothing)", rep.Paged)
	}
}

func TestScanUndispatchedChildStalls_DispatchedOrRunningNoPage(t *testing.T) {
	for _, state := range []run.StageState{run.StageStateDispatched, run.StageStateRunning, run.StageStateFailed, run.StageStateSucceeded} {
		t.Run(string(state), func(t *testing.T) {
			f := newChildStallFixture(t, [][]int{nil}, 0)
			f.addChild(0, run.StateRunning, state, 2*time.Hour)
			f.scanChildren(t, stallNow)
			f.wantNoChildPage(t)
		})
	}
}

func TestScanUndispatchedChildStalls_HostAdmissionHoldNoPage(t *testing.T) {
	f := newChildStallFixture(t, [][]int{nil, {0}}, 0)
	f.dispatchableWave1(2 * time.Hour)
	f.hold.held = true
	f.scanChildren(t, stallNow)
	f.wantNoChildPage(t)
	f.hold.held = false
	if rep := f.scanChildren(t, stallNow); rep.Paged != 1 {
		t.Fatalf("paged after the hold clears = %d, want 1", rep.Paged)
	}
}

// capFixture: one sibling (slice 1) running its implement stage with every
// clock 2h old, and a wave-0 target (slice 0) pending, created 2h ago.
func capFixture(t *testing.T, capN int) *childStallFixture {
	f := newChildStallFixture(t, [][]int{nil, nil}, capN)
	f.addChild(1, run.StateRunning, run.StageStateRunning, 2*time.Hour)
	f.addChild(0, run.StateRunning, run.StageStatePending, 2*time.Hour)
	return f
}

func TestScanUndispatchedChildStalls_CapDeferredWithSiblingsInFlightNoPage(t *testing.T) {
	f := capFixture(t, 1)
	f.scanChildren(t, stallNow)
	f.wantNoChildPage(t)
}

func TestScanUndispatchedChildStalls_CapHeadroomPages(t *testing.T) {
	for _, capN := range []int{2, 0} {
		t.Run(fmt.Sprintf("cap_%d", capN), func(t *testing.T) {
			f := capFixture(t, capN)
			if rep := f.scanChildren(t, stallNow); rep.Paged != 1 {
				t.Fatalf("paged = %d, want 1", rep.Paged)
			}
			if rows := f.childRows(t); rows[0]["in_flight_siblings"] != float64(1) {
				t.Errorf("in_flight_siblings = %v, want 1", rows[0]["in_flight_siblings"])
			}
		})
	}
}

func TestScanUndispatchedChildStalls_UndispatchedSiblingsDoNotCountAsInFlight(t *testing.T) {
	f := newChildStallFixture(t, [][]int{nil, nil}, 1)
	f.addChild(0, run.StateRunning, run.StageStateAwaitingHostDispatch, 2*time.Hour)
	f.addChild(1, run.StateRunning, run.StageStateAwaitingHostDispatch, 2*time.Hour)
	if rep := f.scanChildren(t, stallNow); rep.Paged != 2 {
		t.Fatalf("paged = %d, want 2 (mutually stuck children both page)", rep.Paged)
	}
}

// M3: a sibling queued for a slot with a live waiter occupies the cap.
func TestScanUndispatchedChildStalls_LiveQueuedSiblingCountsAsInFlight(t *testing.T) {
	f := newChildStallFixture(t, [][]int{nil, nil}, 1)
	_, sibStage := f.addChild(1, run.StateRunning, run.StageStatePending, 2*time.Hour)
	f.liveWaiter(sibStage.ID)
	f.addChild(0, run.StateRunning, run.StageStatePending, 2*time.Hour)
	f.scanChildren(t, stallNow)
	f.wantNoChildPage(t)
}

func TestScanUndispatchedChildStalls_SiblingSettledRestartsClock(t *testing.T) {
	f := newChildStallFixture(t, [][]int{nil, nil}, 0)
	f.addChild(1, run.StateSucceeded, run.StageStateSucceeded, 10*time.Minute)
	f.addChild(0, run.StateRunning, run.StageStatePending, 2*time.Hour)
	f.scanChildren(t, stallNow)
	f.wantNoChildPage(t)
}

func TestScanUndispatchedChildStalls_ParentActRestartsClock(t *testing.T) {
	f := newChildStallFixture(t, [][]int{nil}, 0)
	f.addChild(0, run.StateRunning, run.StageStatePending, 2*time.Hour)
	f.au.seed(f.parent.ID, nil, "operator_note", audit.ActorUser, stallNow.Add(-10*time.Minute), map[string]any{})
	f.scanChildren(t, stallNow)
	f.wantNoChildPage(t)
}

func TestScanUndispatchedChildStalls_ChildActRestartsClock(t *testing.T) {
	f := newChildStallFixture(t, [][]int{nil}, 0)
	c, _ := f.addChild(0, run.StateRunning, run.StageStatePending, 2*time.Hour)
	f.au.seed(c.ID, nil, "operator_note", audit.ActorAgent, stallNow.Add(-10*time.Minute), map[string]any{})
	f.scanChildren(t, stallNow)
	f.wantNoChildPage(t)
}

func TestScanUndispatchedChildStalls_NewEpisodeAfterRetry(t *testing.T) {
	f := newChildStallFixture(t, [][]int{nil}, 0)
	c, st := f.addChild(0, run.StateRunning, run.StageStatePending, 2*time.Hour)
	f.scanChildren(t, stallNow)
	later := stallNow.Add(3 * time.Hour)
	f.mutateStage(c.ID, st.ID, func(s *run.Stage) {
		s.SelfRetryCount++
		s.UpdatedAt = later.Add(-31 * time.Minute)
	})
	f.scanChildren(t, later)
	rows := f.childRows(t)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[0]["episode_key"] == rows[1]["episode_key"] {
		t.Errorf("both rows carry episode %v; a retry bump is a new episode", rows[0]["episode_key"])
	}
}

// V3: a row written while the notifier fails, then a dispatch that returns
// to pending without a retry bump, leaves no stale page to post.
func TestScanUndispatchedChildStalls_ReEntryAfterRedispatchNoStalePage(t *testing.T) {
	f := newChildStallFixture(t, [][]int{nil}, 0)
	f.ntf.err = errors.New("forge down")
	c, st := f.addChild(0, run.StateRunning, run.StageStatePending, 2*time.Hour)
	f.scanChildren(t, stallNow)
	if n := f.au.count(CategoryChildDispatchAwaitingOperator); n != 1 {
		t.Fatalf("precondition rows = %d, want 1", n)
	}
	f.mutateStage(c.ID, st.ID, func(s *run.Stage) {
		d := stallNow.Add(-20 * time.Minute)
		s.DispatchedAt = &d
		s.State = run.StageStatePending
		s.UpdatedAt = stallNow.Add(-5 * time.Minute)
	})
	f.scanChildren(t, stallNow)
	if n := f.au.count(CategoryChildDispatchAwaitingOperator); n != 1 {
		t.Errorf("rows after re-entry = %d, want 1 (no new page below threshold)", n)
	}
	if kinds := issuecomment.UnresolvedPageClassEventKinds(f.chain(t, c.ID), f.liveStages(t, c.ID)); len(kinds) != 0 {
		t.Errorf("unresolved kinds = %v, want none (the old episode ended)", kinds)
	}
}

func TestScanUndispatchedChildStalls_ReadErrorSkipsOnlyThatChild(t *testing.T) {
	f := newChildStallFixture(t, [][]int{nil, nil}, 0)
	bad, _ := f.addChild(0, run.StateRunning, run.StageStatePending, 2*time.Hour)
	good, _ := f.addChild(1, run.StateRunning, run.StageStatePending, 2*time.Hour)
	f.au.failListFor = map[uuid.UUID]bool{bad.ID: true}
	rep := f.scanChildren(t, stallNow)
	if rep.Paged != 1 || rep.Skipped != 1 {
		t.Fatalf("report = %+v, want 1 paged and 1 skipped", rep)
	}
	rows := f.childRows(t)
	if len(rows) != 1 || rows[0]["child_run_id"] != good.ID.String() {
		t.Errorf("rows = %v, want one for the good child", rows)
	}
}

func TestScanUndispatchedChildStalls_ConcurrencyReadErrorSkips(t *testing.T) {
	f := newChildStallFixture(t, [][]int{nil}, 0)
	f.addChild(0, run.StateRunning, run.StageStatePending, 2*time.Hour)
	f.conc.err = errors.New("slot store down")
	if rep := f.scanChildren(t, stallNow); rep.Skipped != 1 {
		t.Fatalf("skipped = %d, want 1", rep.Skipped)
	}
	f.wantNoChildPage(t)
}

func TestScanUndispatchedChildStalls_RefusesUnwiredAndDegenerate(t *testing.T) {
	f := newChildStallFixture(t, [][]int{nil}, 0)
	bad := StallScanParams{Threshold: 0, Lookback: time.Hour, Now: stallNow}
	if _, err := f.s.ScanUndispatchedChildStalls(context.Background(), bad); err == nil {
		t.Error("degenerate params accepted")
	}
	unwired := New(Config{Addr: "127.0.0.1:0"})
	if _, err := unwired.ScanUndispatchedChildStalls(context.Background(), stallParams(stallNow)); !errors.Is(err, errStallWatchUnwired) {
		t.Errorf("unwired err = %v, want errStallWatchUnwired", err)
	}
	f.runs.listErr = errors.New("db down")
	if _, err := f.s.ScanUndispatchedChildStalls(context.Background(), stallParams(stallNow)); err == nil {
		t.Error("ListRuns error did not fail the scan")
	}
}

// L(c): the two marker guards the scan reuses stay read-only. A scan over a
// blocked, an uncovered and a dispatchable child writes no run or stage row
// and appends nothing but the one page.
func TestScanUndispatchedChildStalls_GuardsStayReadOnly(t *testing.T) {
	f := newChildStallFixture(t, [][]int{nil, {0}, {0}}, 0)
	trap := &childStallWriteTrap{stallRunRepo: f.runs}
	f.s = New(Config{Addr: "127.0.0.1:0", RunRepo: trap, AuditRepo: f.au, ArtifactRepo: f.art,
		HostAdmissionHold: f.hold, Concurrency: f.conc})
	f.s.issueNotifier = f.ntf
	dep, _ := f.addChild(0, run.StateSucceeded, run.StageStateSucceeded, 2*time.Hour)
	blocked, _ := f.addChild(1, run.StateRunning, run.StageStatePending, 2*time.Hour)
	f.integrate(2 * time.Hour) // uncovered
	before := len(f.chain(t, f.parent.ID))
	ctx := context.Background()
	if _, err := f.s.guardDecompositionWaveOrder(ctx, blocked); err != nil {
		t.Fatalf("wave-order guard: %v", err)
	}
	if _, _, err := f.s.resolveDependentChildBase(ctx, blocked); err != nil {
		t.Fatalf("coverage guard: %v", err)
	}
	f.integrate(2*time.Hour, dep.ID)
	before++
	f.scanChildren(t, stallNow)
	if len(trap.writes) != 0 {
		t.Errorf("run/stage writes = %v, want none", trap.writes)
	}
	if after := len(f.chain(t, f.parent.ID)); after != before {
		t.Errorf("parent chain grew %d -> %d", before, after)
	}
}

// Cross-boundary (persistence -> server -> projection).
func TestStallWatchChildren_PageRowProjectsToPageKind(t *testing.T) {
	f := newChildStallFixture(t, [][]int{nil, {0}}, 0)
	child, st := f.dispatchableWave1(31 * time.Minute)
	f.scanChildren(t, stallNow)
	entries := f.chain(t, child.ID)
	stages := f.liveStages(t, child.ID)
	if kinds := issuecomment.PageClassEventKinds(entries, stages); len(kinds) != 1 || kinds[0] != "child_dispatch_stalled" {
		t.Fatalf("projected kinds = %v, want [child_dispatch_stalled]", kinds)
	}
	if kinds := issuecomment.UnresolvedPageClassEventKinds(entries, stages); len(kinds) != 1 || kinds[0] != "child_dispatch_stalled" {
		t.Fatalf("unresolved kinds = %v, want [child_dispatch_stalled]", kinds)
	}
	want, ok := issuecomment.ChildDispatchStallEpisode(st)
	if !ok {
		t.Fatal("ChildDispatchStallEpisode(!ok) on the awaiting fixture stage")
	}
	if rows := f.childRows(t); len(rows) != 1 || rows[0]["episode_key"] != want {
		t.Errorf("row episode_key = %v, want %q", rows, want)
	}
}
