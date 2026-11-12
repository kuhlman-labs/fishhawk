package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/defaultbranch"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// casRaceRuns wraps stubRuns with the row-locked TransitionStageFrom
// (StageCASTransitioner) production postgres carries, plus an optional
// one-shot "concurrent winner" hook (#4610): on the FIRST transition attempt of
// hookStageID — through EITHER TransitionStage or TransitionStageFrom — it
// first sets that stage to winnerState (what a concurrent settler committed),
// then evaluates the attempt. The plain TransitionStage stays stubRuns', which
// accepts a same-state move as success exactly as postgres transitionStageTx
// does, so a settler that bypasses the CAS is observably a silent loser.
type casRaceRuns struct {
	*stubRuns

	hookStageID uuid.UUID
	winnerState run.StageState
	fired       bool
}

// fireWinnerLocked applies the one-shot winner hook. Caller holds c.mu.
func (c *casRaceRuns) fireWinnerLocked(id uuid.UUID) {
	if c.fired || c.hookStageID == uuid.Nil || id != c.hookStageID {
		return
	}
	c.fired = true
	for _, list := range c.stages {
		for _, st := range list {
			if st.ID == id {
				st.State = c.winnerState
			}
		}
	}
}

func (c *casRaceRuns) TransitionStage(ctx context.Context, id uuid.UUID, to run.StageState, completion *run.StageCompletion) (*run.Stage, error) {
	c.mu.Lock()
	c.fireWinnerLocked(id)
	c.mu.Unlock()
	return c.stubRuns.TransitionStage(ctx, id, to, completion)
}

func (c *casRaceRuns) TransitionStageFrom(_ context.Context, id uuid.UUID, from, to run.StageState, completion *run.StageCompletion) (*run.Stage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fireWinnerLocked(id)
	for _, list := range c.stages {
		for _, st := range list {
			if st.ID != id {
				continue
			}
			if st.State != from {
				return nil, run.StageStateChangedError{StageID: id, Expected: from, Actual: st.State}
			}
			if !run.ValidStageTransition(st.State, to) {
				return nil, run.InvalidTransitionError{Kind: "stage", From: string(st.State), To: string(to)}
			}
			st.State = to
			if completion != nil {
				st.FailureCategory = completion.FailureCategory
				st.FailureReason = completion.FailureReason
			}
			c.stageTransitions = append(c.stageTransitions, stageTransition{StageID: id, To: to})
			return st, nil
		}
	}
	return nil, run.ErrNotFound
}

var _ run.StageCASTransitioner = (*casRaceRuns)(nil)

// raceGitHub wraps stubGitHub with an entry signal on the fan-in's first forge
// write (CreateRef / MergeBranch), sent BEFORE stubGitHub takes its mutex, and
// an optional hook run at the top of CreatePullRequest — a call reached only
// from Advance's consolidated-PR open.
type raceGitHub struct {
	*stubGitHub
	entered  chan struct{}
	onOpenPR func()
}

func (g *raceGitHub) signalEntry() {
	if g.entered == nil {
		return
	}
	select {
	case g.entered <- struct{}{}:
	default:
	}
}

func (g *raceGitHub) CreateRef(ctx context.Context, scope forge.CredentialScope, repo forge.RepoRef, branch, sha string) error {
	g.signalEntry()
	return g.stubGitHub.CreateRef(ctx, scope, repo, branch, sha)
}

func (g *raceGitHub) MergeBranch(ctx context.Context, scope forge.CredentialScope, repo forge.RepoRef, base, head, msg string) (string, error) {
	g.signalEntry()
	return g.stubGitHub.MergeBranch(ctx, scope, repo, base, head, msg)
}

func (g *raceGitHub) CreatePullRequest(ctx context.Context, scope forge.CredentialScope, repo forge.RepoRef, head, base, title, body string) (*forge.PullRequest, error) {
	if g.onOpenPR != nil {
		g.onOpenPR()
	}
	return g.stubGitHub.CreatePullRequest(ctx, scope, repo, head, base, title, body)
}

// settleRaceFixture is a decomposed parent (implement awaiting_children,
// review pending) on a CAS-capable stub with a winner hook armed on the
// parent's implement stage.
type settleRaceFixture struct {
	o      *Orchestrator
	rs     *stubRuns
	runs   *casRaceRuns
	gh     *raceGitHub
	au     *recordingAudit
	parent *run.Run
	stages []*run.Stage
}

func newSettleRaceFixture(t *testing.T) *settleRaceFixture {
	t.Helper()
	rs := newStubRuns()
	runs := &casRaceRuns{stubRuns: rs}
	gh := &raceGitHub{stubGitHub: &stubGitHub{branchSHAs: map[string]string{"main": "basesha"}}}
	au := &recordingAudit{}
	o := &Orchestrator{Runs: runs, GitHub: gh, Audit: au, DefaultBranch: defaultbranch.Static("main")}
	parent, stages := seedFanInParent(t, rs, int64Ptr(55))
	return &settleRaceFixture{o: o, rs: rs, runs: runs, gh: gh, au: au, parent: parent, stages: stages}
}

// armWinner arms the one-shot concurrent-winner hook on the parent implement
// stage.
func (f *settleRaceFixture) armWinner(state run.StageState) {
	f.runs.hookStageID = f.stages[0].ID
	f.runs.winnerState = state
}

func (f *settleRaceFixture) createPRCount() int {
	f.gh.mu.Lock()
	defer f.gh.mu.Unlock()
	return len(f.gh.createPRCalls)
}

func (f *settleRaceFixture) forgeWriteCount() int {
	f.gh.mu.Lock()
	defer f.gh.mu.Unlock()
	return len(f.gh.createRefCalls) + len(f.gh.mergeCalls)
}

func (f *settleRaceFixture) stageState(i int) run.StageState {
	f.rs.mu.Lock()
	defer f.rs.mu.Unlock()
	return f.stages[i].State
}

// TestMaybeAdvanceDecomposedParent_LosesSettleRace_NoDuplicateSettle is the
// clean-integration CAS arm (#4610): a concurrent settler committed succeeded
// while this call integrated, so the shared settle transition's CAS refuses and
// this call writes no children_settled row and never Advances. With the CAS
// removed (helper or call site), the plain TransitionStage sees
// succeeded->succeeded, which ValidStageTransition accepts as a same-state
// success exactly as postgres does, so the loser writes children_settled and
// Advance opens the consolidated PR.
func TestMaybeAdvanceDecomposedParent_LosesSettleRace_NoDuplicateSettle(t *testing.T) {
	f := newSettleRaceFixture(t)
	seedSucceededSlice(t, f.rs, f.parent.ID, int64Ptr(55), 0)
	seedSucceededSlice(t, f.rs, f.parent.ID, int64Ptr(55), 1)
	f.armWinner(run.StageStateSucceeded)

	f.o.maybeAdvanceDecomposedParent(context.Background(), f.parent.ID)

	if !f.runs.fired {
		t.Fatal("the concurrent-winner hook never fired; the settle transition was not attempted")
	}
	if n := countAuditCategory(f.au, "children_settled"); n != 0 {
		t.Errorf("children_settled rows = %d, want 0 (the race loser must not write the winner's settle row)", n)
	}
	if n := f.createPRCount(); n != 0 {
		t.Errorf("CreatePullRequest calls = %d, want 0 (the race loser must not Advance)", n)
	}
	if st := f.stageState(1); st != run.StageStatePending {
		t.Errorf("review stage = %q, want pending (Advance must not run on the loser)", st)
	}
}

// TestMaybeAdvanceDecomposedParent_ConflictLosesSettleRace_NoConflictRow is the
// slice-conflict CAS arm: the concurrent winner committed failed, so the
// failed-B CAS refuses and no slice_integration_conflict row is written.
func TestMaybeAdvanceDecomposedParent_ConflictLosesSettleRace_NoConflictRow(t *testing.T) {
	f := newSettleRaceFixture(t)
	seedSucceededSlice(t, f.rs, f.parent.ID, int64Ptr(55), 0)
	seedSucceededSlice(t, f.rs, f.parent.ID, int64Ptr(55), 1)
	f.gh.mergeErrByHead = map[string]error{sliceBranch(f.parent.ID, 1): forge.ErrMergeConflict}
	f.armWinner(run.StageStateFailed)

	f.o.maybeAdvanceDecomposedParent(context.Background(), f.parent.ID)

	if !f.runs.fired {
		t.Fatal("the concurrent-winner hook never fired; the failed-B transition was not attempted")
	}
	if n := countAuditCategory(f.au, "slice_integration_conflict"); n != 0 {
		t.Errorf("slice_integration_conflict rows = %d, want 0 (the race loser must not write the winner's conflict row)", n)
	}
}

// TestMaybeAdvanceDecomposedParent_FailedChildLosesSettleRace_NoDuplicateSettle
// is the anyFailed CAS arm: a NON-recoverable failed child (implement failed
// with no category, so the parent is not parked) resolves the parent failed-C;
// the concurrent winner already committed failed, so the CAS refuses — no
// children_settled row and no run completion.
func TestMaybeAdvanceDecomposedParent_FailedChildLosesSettleRace_NoDuplicateSettle(t *testing.T) {
	f := newSettleRaceFixture(t)
	seedSucceededSlice(t, f.rs, f.parent.ID, int64Ptr(55), 0)
	failed, _ := f.rs.seed(t, "kuhlman-labs/fishhawk", int64Ptr(55), []stageSeed{
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, State: run.StageStateFailed},
	})
	pid := f.parent.ID
	idx := 1
	failed.DecomposedFrom = &pid
	failed.SliceIndex = &idx
	failed.State = run.StateFailed
	f.armWinner(run.StageStateFailed)

	f.o.maybeAdvanceDecomposedParent(context.Background(), f.parent.ID)

	if !f.runs.fired {
		t.Fatal("the concurrent-winner hook never fired; the failed-C transition was not attempted (was the parent parked?)")
	}
	if n := countAuditCategory(f.au, "children_settled"); n != 0 {
		t.Errorf("children_settled rows = %d, want 0 (the race loser must not write the winner's settle row)", n)
	}
	f.rs.mu.Lock()
	defer f.rs.mu.Unlock()
	for _, tr := range f.rs.runTransitions {
		if tr.RunID == f.parent.ID {
			t.Errorf("parent run transitioned to %q; the race loser must not Advance the parent to completion", tr.To)
		}
	}
}

// TestMaybeAdvanceDecomposedParent_WaitsOnFanInSettleLock pins the settle lock
// and the in-lock re-read (#4610): while another settler holds
// LockFanInSettle(parent), the event-driven settle must not integrate; when the
// holder commits succeeded and unlocks, the waiter re-reads the stage, finds it
// settled, and writes nothing. With the lock acquisition replaced by a no-op,
// the goroutine's ListStagesForRun sees awaiting_children before the flip and
// integrates (an entry signal arrives within the wait).
func TestMaybeAdvanceDecomposedParent_WaitsOnFanInSettleLock(t *testing.T) {
	f := newSettleRaceFixture(t)
	seedSucceededSlice(t, f.rs, f.parent.ID, int64Ptr(55), 0)
	f.gh.entered = make(chan struct{}, 1)

	unlock := f.o.LockFanInSettle(f.parent.ID)
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.o.maybeAdvanceDecomposedParent(context.Background(), f.parent.ID)
	}()

	select {
	case <-f.gh.entered:
		t.Error("the fan-in reached the forge while another settler held the settle lock")
	case <-time.After(200 * time.Millisecond):
	}

	// What the holder's settle would have committed.
	f.rs.mu.Lock()
	f.stages[0].State = run.StageStateSucceeded
	f.rs.mu.Unlock()
	unlock()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("maybeAdvanceDecomposedParent did not return after the settle lock was released")
	}
	if n := f.forgeWriteCount(); n != 0 {
		t.Errorf("CreateRef+MergeBranch calls = %d, want 0 (the waiter must re-read the settled stage and skip the fan-in)", n)
	}
	if n := countAuditCategory(f.au, "slices_integrated"); n != 0 {
		t.Errorf("slices_integrated rows = %d, want 0", n)
	}
	if n := countAuditCategory(f.au, "children_settled"); n != 0 {
		t.Errorf("children_settled rows = %d, want 0", n)
	}
}

// TestMaybeAdvanceDecomposedParent_SettleLockNotHeldAcrossAdvance pins the lock
// order (#4610, approval condition C2): the fan-in settle lock is NEVER held
// across Advance, so it never nests with the consolidated-PR-open locks
// Advance takes. The forge CreatePullRequest fake — reached only from
// Advance's consolidated-PR open — tries LockFanInSettle(parent) with a bounded
// wait. Holding the lock across Advance makes that wait time out (RED) without
// deadlocking the suite: the probe goroutine acquires and releases once the
// settle returns.
func TestMaybeAdvanceDecomposedParent_SettleLockNotHeldAcrossAdvance(t *testing.T) {
	f := newSettleRaceFixture(t)
	seedSucceededSlice(t, f.rs, f.parent.ID, int64Ptr(55), 0)

	var probed, acquired bool
	probeDone := make(chan struct{})
	f.gh.onOpenPR = func() {
		probed = true
		got := make(chan struct{})
		go func() {
			defer close(probeDone)
			u := f.o.LockFanInSettle(f.parent.ID)
			close(got)
			u()
		}()
		select {
		case <-got:
			acquired = true
		case <-time.After(time.Second):
		}
	}

	f.o.maybeAdvanceDecomposedParent(context.Background(), f.parent.ID)

	if !probed {
		t.Fatal("CreatePullRequest was never reached; Advance did not open the consolidated PR")
	}
	if !acquired {
		t.Error("LockFanInSettle(parent) blocked inside Advance's CreatePullRequest; the settle lock is held across Advance")
	}
	select {
	case <-probeDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the lock probe never acquired after the settle returned")
	}
	if st := f.stageState(0); st != run.StageStateSucceeded {
		t.Errorf("implement stage = %q, want succeeded", st)
	}
	if n := countAuditCategory(f.au, "children_settled"); n != 1 {
		t.Errorf("children_settled rows = %d, want 1", n)
	}
}
