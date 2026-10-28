package orchestrator

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// #4785: a workflow that orders acceptance BEFORE review (feature_change)
// reaches a decomposed parent's acceptance stage first, so the ADR-032
// review-gate tail (consolidated PR, fan-in verify, consolidated review) must
// run ahead of acceptance. Every fake and fixture for these tests lives in this
// file (approval condition C6).

// fanInVerifyCall is one recorded FanInVerifySettled call, with the state the
// hook OBSERVED at call time so a test can assert ordering (PR opened, and
// acceptance still pending, BEFORE the hook is consulted).
type fanInVerifyCall struct {
	runID           uuid.UUID
	base, branch    string
	prURLAtCall     string
	acceptanceState run.StageState
}

// recordingFanInVerify is a FanInVerifyGate fake answering a scripted
// sequence of verdicts (the last one repeats) and recording each call.
type recordingFanInVerify struct {
	mu      sync.Mutex
	answers []bool
	err     error
	calls   []fanInVerifyCall

	// observe, when set, snapshots the repo state at call time.
	runs       *stubRuns
	acceptance *run.Stage
}

func (f *recordingFanInVerify) FanInVerifySettled(_ context.Context, runID uuid.UUID, base, branch string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := fanInVerifyCall{runID: runID, base: base, branch: branch}
	if f.runs != nil {
		f.runs.mu.Lock()
		if r := f.runs.runs[runID]; r != nil && r.PullRequestURL != nil {
			c.prURLAtCall = *r.PullRequestURL
		}
		f.runs.mu.Unlock()
	}
	if f.acceptance != nil {
		c.acceptanceState = f.acceptance.State
	}
	f.calls = append(f.calls, c)
	if f.err != nil {
		return false, f.err
	}
	if len(f.answers) == 0 {
		return true, nil
	}
	i := len(f.calls) - 1
	if i >= len(f.answers) {
		i = len(f.answers) - 1
	}
	return f.answers[i], nil
}

func (f *recordingFanInVerify) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// acceptanceAheadParent holds the stages of a feature_change-shaped decomposed
// parent: implement (seq 0), acceptance (seq 1), review (seq 2).
type acceptanceAheadParent struct {
	run        *run.Run
	implement  *run.Stage
	acceptance *run.Stage
	review     *run.Stage
}

// seedAcceptanceAheadParent seeds a runner_kind-local decomposed parent whose
// implement stage is in implState, with acceptance and review both pending
// and one succeeded child. The PR URL is left EMPTY, so the acceptance arm
// must open the consolidated PR itself.
func seedAcceptanceAheadParent(t *testing.T, rs *stubRuns, implState run.StageState) acceptanceAheadParent {
	t.Helper()
	parent, stages := rs.seed(t, "kuhlman-labs/fishhawk", int64Ptr(55), []stageSeed{
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, State: implState},
		{Type: run.StageTypeAcceptance, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStatePending},
		{Type: run.StageTypeReview, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStatePending},
	})
	parent.RunnerKind = run.RunnerKindLocal
	parent.RunnerKindResolved = true
	seedSucceededSlice(t, rs, parent.ID, int64Ptr(55), 0)
	return acceptanceAheadParent{run: parent, implement: stages[0], acceptance: stages[1], review: stages[2]}
}

// newFanInAcceptanceOrchestrator wires the orchestrator with the recording
// audit, consolidated-review dispatcher and FanInVerifyGate fakes.
func newFanInAcceptanceOrchestrator(t *testing.T, answers ...bool) (*Orchestrator, *stubRuns, *stubGitHub, *recordingAudit, *recordingConsolidatedReview, *recordingFanInVerify) {
	t.Helper()
	o, rs, gh := newOrchestrator(t)
	o.DefaultRef = "main"
	au := &recordingAudit{}
	o.Audit = au
	rec := &recordingConsolidatedReview{}
	o.ConsolidatedReview = rec
	hook := &recordingFanInVerify{answers: answers, runs: rs}
	o.FanInVerify = hook
	return o, rs, gh, au, rec, hook
}

func prURL(rs *stubRuns, id uuid.UUID) string {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if r := rs.runs[id]; r != nil && r.PullRequestURL != nil {
		return *r.PullRequestURL
	}
	return ""
}

// RELEASE case (hook reports SETTLED): the consolidated PR is opened BEFORE
// acceptance leaves pending, acceptance is then dispatched (parked at
// awaiting_host_dispatch on the local runner) and the consolidated review
// fires.
func TestAdvance_DecomposedParentAcceptance_OpensPRBeforeDispatch(t *testing.T) {
	o, rs, gh, au, rec, hook := newFanInAcceptanceOrchestrator(t, true)
	p := seedAcceptanceAheadParent(t, rs, run.StageStateSucceeded)
	hook.acceptance = p.acceptance

	out, err := o.Advance(context.Background(), p.run.ID)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if out != OutcomeDispatched {
		t.Errorf("Outcome = %q, want dispatched", out)
	}
	if len(gh.createPRCalls) != 1 {
		t.Fatalf("CreatePullRequest calls = %d, want 1", len(gh.createPRCalls))
	}
	if want := consolidatedBranch(p.run.ID); gh.createPRCalls[0].Head != want {
		t.Errorf("PR head = %q, want %q", gh.createPRCalls[0].Head, want)
	}
	if prURL(rs, p.run.ID) == "" {
		t.Error("parent pull_request_url not stamped")
	}
	if !auditHasCategory(au, "consolidated_pr_opened") {
		t.Errorf("audit categories = %v, want consolidated_pr_opened", au.appended)
	}
	for _, e := range au.appended {
		if e.Category == "consolidated_pr_opened" && (e.StageID == nil || *e.StageID != p.review.ID) {
			t.Errorf("consolidated_pr_opened stage anchor = %v, want the review stage %s", e.StageID, p.review.ID)
		}
	}
	if len(hook.calls) != 1 {
		t.Fatalf("FanInVerifySettled calls = %d, want 1", len(hook.calls))
	}
	c := hook.calls[0]
	if c.base != "main" || c.branch != consolidatedBranch(p.run.ID) {
		t.Errorf("hook called with (%q, %q), want (main, %q)", c.base, c.branch, consolidatedBranch(p.run.ID))
	}
	if c.prURLAtCall == "" {
		t.Error("hook consulted before the consolidated PR was opened")
	}
	if c.acceptanceState != run.StageStatePending {
		t.Errorf("acceptance state at hook call = %q, want pending (consulted before dispatch)", c.acceptanceState)
	}
	if p.acceptance.State != run.StageStateAwaitingHostDispatch {
		t.Errorf("acceptance state = %q, want awaiting_host_dispatch (released on the local runner)", p.acceptance.State)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("DispatchConsolidatedReview calls = %d, want 1", len(rec.calls))
	}
	if rec.calls[0].base != "main" || rec.calls[0].head != consolidatedBranch(p.run.ID) {
		t.Errorf("DispatchConsolidatedReview(%q, %q), want (main, consolidated branch)", rec.calls[0].base, rec.calls[0].head)
	}
	if p.review.State != run.StageStatePending {
		t.Errorf("review state = %q, want pending (acceptance runs first)", p.review.State)
	}
}

// HOLD case (hook reports NOT settled): acceptance stays pending, the PR is
// opened exactly once (the fixture leaves the URL empty), and no review round
// runs. The ONE DispatchConsolidatedReview call is the verify-pass start: the
// server-side D5 hold starts the verify-only pass and holds the round
// (TestFanInAcceptanceHold_EndToEnd pins zero implement_review_started).
func TestAdvance_DecomposedParentAcceptance_HeldWhileFanInUnsettled(t *testing.T) {
	for _, implState := range []run.StageState{run.StageStateSucceeded, run.StageStateRunning} {
		t.Run(string(implState), func(t *testing.T) {
			o, rs, gh, _, rec, hook := newFanInAcceptanceOrchestrator(t, false)
			p := seedAcceptanceAheadParent(t, rs, implState)

			out, err := o.Advance(context.Background(), p.run.ID)
			if err != nil {
				t.Fatalf("Advance: %v", err)
			}
			if out != OutcomeNoOp {
				t.Errorf("Outcome = %q, want no-op (held)", out)
			}
			if p.acceptance.State != run.StageStatePending {
				t.Errorf("acceptance state = %q, want pending (held until the fan-in verify settles)", p.acceptance.State)
			}
			if len(gh.createPRCalls) != 1 {
				t.Errorf("CreatePullRequest calls = %d, want exactly 1", len(gh.createPRCalls))
			}
			if hook.callCount() != 1 {
				t.Errorf("FanInVerifySettled calls = %d, want 1", hook.callCount())
			}
			if len(rec.calls) != 1 {
				t.Errorf("DispatchConsolidatedReview calls = %d, want 1 (the verify-pass start the D5 hold holds)", len(rec.calls))
			}
			if p.review.State != run.StageStatePending {
				t.Errorf("review state = %q, want pending", p.review.State)
			}
			if p.implement.State != implState {
				t.Errorf("implement state = %q, want unchanged %q", p.implement.State, implState)
			}
			if n := o.fanInHolds[p.run.ID]; n != 1 {
				t.Errorf("hold attempt count = %d, want 1", n)
			}
		})
	}
}

// A hook error fails closed: Advance returns it and acceptance stays pending.
func TestAdvance_DecomposedParentAcceptance_HookErrorHolds(t *testing.T) {
	o, rs, _, _, rec, hook := newFanInAcceptanceOrchestrator(t)
	hook.err = errors.New("compare boom")
	p := seedAcceptanceAheadParent(t, rs, run.StageStateSucceeded)

	if _, err := o.Advance(context.Background(), p.run.ID); err == nil {
		t.Fatal("Advance returned nil error on a hook error")
	}
	if p.acceptance.State != run.StageStatePending {
		t.Errorf("acceptance state = %q, want pending", p.acceptance.State)
	}
	if len(rec.calls) != 0 {
		t.Errorf("DispatchConsolidatedReview calls = %d, want 0 on a hook error", len(rec.calls))
	}
}

// A PR-open error is returned so the next Advance retries; acceptance is not
// dispatched and the hook is not consulted.
func TestAdvance_DecomposedParentAcceptance_PROpenErrorHolds(t *testing.T) {
	o, rs, gh, _, rec, hook := newFanInAcceptanceOrchestrator(t, true)
	gh.createPRErr = errors.New("forge boom")
	p := seedAcceptanceAheadParent(t, rs, run.StageStateSucceeded)

	if _, err := o.Advance(context.Background(), p.run.ID); err == nil {
		t.Fatal("Advance returned nil error on a PR-open error")
	}
	if p.acceptance.State != run.StageStatePending {
		t.Errorf("acceptance state = %q, want pending", p.acceptance.State)
	}
	if hook.callCount() != 0 || len(rec.calls) != 0 {
		t.Errorf("hook calls = %d, review fires = %d, want 0/0 on a PR-open error", hook.callCount(), len(rec.calls))
	}
}

// A children-list error is returned before anything is opened or dispatched.
func TestAdvance_DecomposedParentAcceptance_ListChildrenErrorHolds(t *testing.T) {
	o, rs, gh, _, _, hook := newFanInAcceptanceOrchestrator(t, true)
	p := seedAcceptanceAheadParent(t, rs, run.StageStateSucceeded)
	rs.listRunsErr = errors.New("list boom")

	if _, err := o.Advance(context.Background(), p.run.ID); err == nil {
		t.Fatal("Advance returned nil error on a children-list error")
	}
	if p.acceptance.State != run.StageStatePending {
		t.Errorf("acceptance state = %q, want pending", p.acceptance.State)
	}
	if len(gh.createPRCalls) != 0 || hook.callCount() != 0 {
		t.Errorf("creates = %d, hook calls = %d, want 0/0", len(gh.createPRCalls), hook.callCount())
	}
}

// The hold releases on the Advance after the hook turns settled (the
// merge_candidate_verified report's Advance in production): acceptance is
// dispatched, the PR is not re-opened, and the hold counter counted the
// repeated hold and is cleared on release.
func TestAdvance_DecomposedParentAcceptance_ReleaseAfterSettled(t *testing.T) {
	o, rs, gh, _, rec, _ := newFanInAcceptanceOrchestrator(t, false, false, true)
	p := seedAcceptanceAheadParent(t, rs, run.StageStateSucceeded)

	for i := 0; i < 2; i++ {
		if _, err := o.Advance(context.Background(), p.run.ID); err != nil {
			t.Fatalf("Advance #%d: %v", i+1, err)
		}
		if p.acceptance.State != run.StageStatePending {
			t.Fatalf("after held Advance #%d: acceptance state = %q, want pending", i+1, p.acceptance.State)
		}
	}
	if n := o.fanInHolds[p.run.ID]; n != 2 {
		t.Errorf("hold attempt count after two holds = %d, want 2 (a repeated hold is distinguishable)", n)
	}

	out, err := o.Advance(context.Background(), p.run.ID)
	if err != nil {
		t.Fatalf("release Advance: %v", err)
	}
	if out != OutcomeDispatched {
		t.Errorf("release Outcome = %q, want dispatched", out)
	}
	if p.acceptance.State != run.StageStateAwaitingHostDispatch {
		t.Errorf("acceptance state = %q, want awaiting_host_dispatch after release", p.acceptance.State)
	}
	if len(gh.createPRCalls) != 1 {
		t.Errorf("CreatePullRequest calls = %d, want still 1", len(gh.createPRCalls))
	}
	if len(rec.calls) != 3 {
		t.Errorf("DispatchConsolidatedReview calls = %d, want 3 (two verify-pass holds + the release fire)", len(rec.calls))
	}
	if _, ok := o.fanInHolds[p.run.ID]; ok {
		t.Error("hold attempt count not cleared on release")
	}
}

// A nil hook (CLI/dev posture) still opens the consolidated PR before
// acceptance, dispatches acceptance and fires the consolidated review.
func TestAdvance_DecomposedParentAcceptance_NilHook_DispatchesWithPR(t *testing.T) {
	o, rs, gh, _, rec, _ := newFanInAcceptanceOrchestrator(t)
	o.FanInVerify = nil
	p := seedAcceptanceAheadParent(t, rs, run.StageStateSucceeded)

	if _, err := o.Advance(context.Background(), p.run.ID); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if len(gh.createPRCalls) != 1 {
		t.Errorf("CreatePullRequest calls = %d, want 1", len(gh.createPRCalls))
	}
	if p.acceptance.State != run.StageStateAwaitingHostDispatch {
		t.Errorf("acceptance state = %q, want awaiting_host_dispatch", p.acceptance.State)
	}
	if len(rec.calls) != 1 {
		t.Errorf("DispatchConsolidatedReview calls = %d, want 1", len(rec.calls))
	}
}

// An ordinary (non-decomposed) run's acceptance is untouched: no PR, no hook,
// no consolidated review, acceptance dispatched as before.
func TestAdvance_DecomposedParentAcceptance_NonDecomposedRunUntouched(t *testing.T) {
	o, rs, gh, _, rec, hook := newFanInAcceptanceOrchestrator(t, false)
	r, stages := rs.seed(t, "kuhlman-labs/fishhawk", int64Ptr(55), []stageSeed{
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, State: run.StageStateSucceeded},
		{Type: run.StageTypeAcceptance, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStatePending},
		{Type: run.StageTypeReview, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStatePending},
	})
	r.RunnerKind = run.RunnerKindLocal
	r.RunnerKindResolved = true

	if _, err := o.Advance(context.Background(), r.ID); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if len(gh.createPRCalls) != 0 {
		t.Errorf("CreatePullRequest calls = %d, want 0", len(gh.createPRCalls))
	}
	if hook.callCount() != 0 {
		t.Errorf("FanInVerifySettled calls = %d, want 0", hook.callCount())
	}
	if len(rec.calls) != 0 {
		t.Errorf("DispatchConsolidatedReview calls = %d, want 0", len(rec.calls))
	}
	if stages[1].State != run.StageStateAwaitingHostDispatch {
		t.Errorf("acceptance state = %q, want awaiting_host_dispatch (dispatched as before)", stages[1].State)
	}
}

// A run with no implement stage can never be a decomposed parent (only an
// implement stage fans out), so the arm does not even list children: a
// ListRuns failure cannot block its acceptance dispatch.
func TestAdvance_DecomposedParentAcceptance_NoImplementStageSkipsChildLookup(t *testing.T) {
	o, rs, gh, _, rec, hook := newFanInAcceptanceOrchestrator(t, false)
	r, stages := rs.seed(t, "kuhlman-labs/fishhawk", int64Ptr(55), []stageSeed{
		{Type: run.StageTypeAcceptance, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStatePending},
	})
	r.RunnerKind = run.RunnerKindLocal
	r.RunnerKindResolved = true
	rs.listRunsErr = errors.New("ListRuns must not be called")

	if _, err := o.Advance(context.Background(), r.ID); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if stages[0].State != run.StageStateAwaitingHostDispatch {
		t.Errorf("acceptance state = %q, want awaiting_host_dispatch", stages[0].State)
	}
	if len(gh.createPRCalls) != 0 || hook.callCount() != 0 || len(rec.calls) != 0 {
		t.Errorf("creates = %d, hook calls = %d, review fires = %d, want 0/0/0", len(gh.createPRCalls), hook.callCount(), len(rec.calls))
	}
}

// The issue's exact sequence (#4785): an awaiting_children parent with
// acceptance ahead of review, a recoverable failed child that parks it, then
// the re-driven child succeeds. The settle's Advance must open the PR and
// consult the hook BEFORE acceptance leaves pending.
func TestMaybeAdvanceDecomposedParent_RedriveSettle_OpensPRBeforeAcceptance(t *testing.T) {
	o, rs, gh, au, rec, hook := newFanInAcceptanceOrchestrator(t, true)
	gh.branchSHAs = map[string]string{"main": "basesha"} // fan-in creates the consolidated branch
	parent, stages := rs.seed(t, "kuhlman-labs/fishhawk", int64Ptr(55), []stageSeed{
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, State: run.StageStateAwaitingChildren},
		{Type: run.StageTypeAcceptance, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStatePending},
		{Type: run.StageTypeReview, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStatePending},
	})
	parent.RunnerKind = run.RunnerKindLocal
	parent.RunnerKindResolved = true
	hook.acceptance = stages[1]

	cat := run.FailureC
	reason := "runner OOM"
	child, childStages := rs.seed(t, "kuhlman-labs/fishhawk", int64Ptr(55), []stageSeed{
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, State: run.StageStateFailed},
	})
	child.DecomposedFrom = &parent.ID
	idx := 0
	child.SliceIndex = &idx
	child.State = run.StateFailed
	childStages[0].FailureCategory = &cat
	childStages[0].FailureReason = &reason

	o.maybeAdvanceDecomposedParent(context.Background(), parent.ID)
	if stages[0].State != run.StageStateAwaitingChildren {
		t.Fatalf("parent implement = %q, want awaiting_children (parked awaiting re-drive)", stages[0].State)
	}
	if !auditHasCategory(au, "parent_awaiting_redrive") {
		t.Fatal("want parent_awaiting_redrive after the recoverable child failure")
	}

	// The re-drive succeeds the child.
	childStages[0].State = run.StageStateSucceeded
	childStages[0].FailureCategory = nil
	childStages[0].FailureReason = nil
	child.State = run.StateSucceeded

	o.maybeAdvanceDecomposedParent(context.Background(), parent.ID)

	if stages[0].State != run.StageStateSucceeded {
		t.Fatalf("parent implement = %q, want succeeded", stages[0].State)
	}
	if len(gh.createPRCalls) != 1 {
		t.Fatalf("CreatePullRequest calls = %d, want 1", len(gh.createPRCalls))
	}
	if len(hook.calls) != 1 {
		t.Fatalf("FanInVerifySettled calls = %d, want 1", len(hook.calls))
	}
	if hook.calls[0].prURLAtCall == "" {
		t.Error("hook consulted before the consolidated PR was opened")
	}
	if hook.calls[0].acceptanceState != run.StageStatePending {
		t.Errorf("acceptance state at hook call = %q, want pending", hook.calls[0].acceptanceState)
	}
	if stages[1].State != run.StageStateAwaitingHostDispatch {
		t.Errorf("acceptance state = %q, want awaiting_host_dispatch after the settled hook", stages[1].State)
	}
	if len(rec.calls) != 1 {
		t.Errorf("DispatchConsolidatedReview calls = %d, want 1", len(rec.calls))
	}
}
