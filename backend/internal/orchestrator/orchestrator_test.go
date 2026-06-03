package orchestrator

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// stubRuns is a minimal in-memory run.Repository covering the
// methods Advance touches: GetRun, ListStagesForRun, TransitionRun,
// TransitionStage. Other methods return "not used" errors so
// accidental calls are loud.
type stubRuns struct {
	mu sync.Mutex

	runs   map[uuid.UUID]*run.Run
	stages map[uuid.UUID][]*run.Stage

	getRunErr        error
	listStagesErr    error
	transitionRunErr error
	transitionErr    error

	stageTransitions []stageTransition
	runTransitions   []runTransition
}

type stageTransition struct {
	StageID uuid.UUID
	To      run.StageState
}

type runTransition struct {
	RunID uuid.UUID
	To    run.State
}

func newStubRuns() *stubRuns {
	return &stubRuns{
		runs:   map[uuid.UUID]*run.Run{},
		stages: map[uuid.UUID][]*run.Stage{},
	}
}

// seedRun inserts a run + stages into the stub. Stages are added
// in spec order; the caller chooses each stage's executor + state.
type stageSeed struct {
	Type         run.StageType
	ExecutorKind run.ExecutorKind
	ExecutorRef  string
	State        run.StageState
}

func (s *stubRuns) seed(t *testing.T, repo string, installationID *int64, stages []stageSeed) (*run.Run, []*run.Stage) {
	t.Helper()
	r := &run.Run{
		ID:             uuid.New(),
		Repo:           repo,
		WorkflowID:     "feature_change",
		WorkflowSHA:    "sha",
		TriggerSource:  run.TriggerGitHubIssue,
		InstallationID: installationID,
		State:          run.StateRunning,
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
	}
	stagesOut := make([]*run.Stage, 0, len(stages))
	for i, ss := range stages {
		st := &run.Stage{
			ID:           uuid.New(),
			RunID:        r.ID,
			Sequence:     i,
			Type:         ss.Type,
			ExecutorKind: ss.ExecutorKind,
			ExecutorRef:  ss.ExecutorRef,
			State:        ss.State,
			CreatedAt:    time.Now().UTC(),
			UpdatedAt:    time.Now().UTC(),
		}
		stagesOut = append(stagesOut, st)
	}
	s.mu.Lock()
	s.runs[r.ID] = r
	s.stages[r.ID] = stagesOut
	s.mu.Unlock()
	return r, stagesOut
}

func (s *stubRuns) GetRun(_ context.Context, id uuid.UUID) (*run.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getRunErr != nil {
		return nil, s.getRunErr
	}
	r, ok := s.runs[id]
	if !ok {
		return nil, run.ErrNotFound
	}
	return r, nil
}

func (s *stubRuns) GetRunByIdempotencyKey(context.Context, string, string) (*run.Run, error) {
	return nil, run.ErrNotFound
}

func (s *stubRuns) ListStagesForRun(_ context.Context, runID uuid.UUID) ([]*run.Stage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listStagesErr != nil {
		return nil, s.listStagesErr
	}
	return s.stages[runID], nil
}

func (s *stubRuns) ListStagesAwaitingApproval(context.Context) ([]*run.Stage, error) {
	return nil, nil
}

func (s *stubRuns) ListStagesAwaitingChildren(context.Context) ([]*run.Stage, error) {
	return nil, nil
}

func (s *stubRuns) ListStagesDispatched(context.Context) ([]*run.Stage, error) {
	return nil, nil
}

func (s *stubRuns) RetryStage(context.Context, uuid.UUID, run.StageState) (*run.Stage, error) {
	return nil, errors.New("not used")
}

func (s *stubRuns) TransitionRun(_ context.Context, id uuid.UUID, to run.State) (*run.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.transitionRunErr != nil {
		return nil, s.transitionRunErr
	}
	r := s.runs[id]
	if r == nil {
		return nil, run.ErrNotFound
	}
	if !run.ValidRunTransition(r.State, to) {
		return nil, run.InvalidTransitionError{Kind: "run", From: string(r.State), To: string(to)}
	}
	r.State = to
	s.runTransitions = append(s.runTransitions, runTransition{RunID: id, To: to})
	return r, nil
}

func (s *stubRuns) TransitionStage(_ context.Context, id uuid.UUID, to run.StageState, _ *run.StageCompletion) (*run.Stage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.transitionErr != nil {
		return nil, s.transitionErr
	}
	for _, list := range s.stages {
		for _, st := range list {
			if st.ID == id {
				if !run.ValidStageTransition(st.State, to) {
					return nil, run.InvalidTransitionError{
						Kind: "stage", From: string(st.State), To: string(to),
					}
				}
				st.State = to
				s.stageTransitions = append(s.stageTransitions, stageTransition{StageID: id, To: to})
				return st, nil
			}
		}
	}
	return nil, run.ErrNotFound
}

// Unused methods.
func (s *stubRuns) CreateRun(context.Context, run.CreateRunParams) (*run.Run, error) {
	return nil, errors.New("not used")
}

func (s *stubRuns) ListRuns(_ context.Context, f run.ListRunsFilter) ([]*run.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f.DecomposedFrom == nil {
		return nil, errors.New("not used")
	}
	var out []*run.Run
	for _, r := range s.runs {
		if r.DecomposedFrom != nil && *r.DecomposedFrom == *f.DecomposedFrom {
			out = append(out, r)
		}
	}
	return out, nil
}
func (s *stubRuns) SetRunPullRequestURL(context.Context, uuid.UUID, string) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (s *stubRuns) CreateStage(context.Context, run.CreateStageParams) (*run.Stage, error) {
	return nil, errors.New("not used")
}
func (s *stubRuns) GetStage(context.Context, uuid.UUID) (*run.Stage, error) {
	return nil, errors.New("not used")
}

// stubGitHub records DispatchWorkflow + EnableAutoMerge calls
// without making network requests.
type stubGitHub struct {
	mu             sync.Mutex
	calls          []dispatchCall
	dispatchErr    error
	autoMergeCalls []autoMergeCall
	autoMergeErr   error
}

type dispatchCall struct {
	InstallationID int64
	Repo           githubclient.RepoRef
	WorkflowFile   string
	Ref            string
	Inputs         githubclient.DispatchInputs
}

type autoMergeCall struct {
	InstallationID int64
	Repo           githubclient.RepoRef
	PRNumber       int
	Method         githubclient.MergeMethod
}

func (g *stubGitHub) DispatchWorkflow(_ context.Context, installationID int64,
	repo githubclient.RepoRef, file, ref string, inputs githubclient.DispatchInputs) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.dispatchErr != nil {
		return g.dispatchErr
	}
	g.calls = append(g.calls, dispatchCall{
		InstallationID: installationID, Repo: repo,
		WorkflowFile: file, Ref: ref, Inputs: inputs,
	})
	return nil
}

func (g *stubGitHub) EnableAutoMerge(_ context.Context, installationID int64,
	repo githubclient.RepoRef, prNumber int, method githubclient.MergeMethod) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.autoMergeErr != nil {
		return g.autoMergeErr
	}
	g.autoMergeCalls = append(g.autoMergeCalls, autoMergeCall{
		InstallationID: installationID, Repo: repo,
		PRNumber: prNumber, Method: method,
	})
	return nil
}

func newOrchestrator(t *testing.T) (*Orchestrator, *stubRuns, *stubGitHub) {
	t.Helper()
	rs := newStubRuns()
	gh := &stubGitHub{}
	return &Orchestrator{Runs: rs, GitHub: gh}, rs, gh
}

func int64Ptr(v int64) *int64 { return &v }

func TestAdvance_DispatchesNextAgentStage(t *testing.T) {
	o, rs, gh := newOrchestrator(t)
	r, stages := rs.seed(t, "x/y", int64Ptr(42), []stageSeed{
		{Type: run.StageTypePlan, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStateSucceeded},
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStatePending},
		{Type: run.StageTypeReview, ExecutorKind: run.ExecutorHuman, ExecutorRef: "human", State: run.StageStatePending},
	})

	out, err := o.Advance(context.Background(), r.ID)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if out != OutcomeDispatched {
		t.Errorf("Outcome = %q, want dispatched", out)
	}

	// Stage 1 (implement) should now be dispatched.
	if stages[1].State != run.StageStateDispatched {
		t.Errorf("stage[1].State = %q, want dispatched", stages[1].State)
	}
	// Stage 2 (review) should still be pending — we only advance one.
	if stages[2].State != run.StageStatePending {
		t.Errorf("stage[2].State = %q, want pending", stages[2].State)
	}

	// workflow_dispatch fired once.
	if len(gh.calls) != 1 {
		t.Fatalf("dispatch calls = %d, want 1", len(gh.calls))
	}
	call := gh.calls[0]
	if call.InstallationID != 42 {
		t.Errorf("installation_id = %d", call.InstallationID)
	}
	if call.Repo.Owner != "x" || call.Repo.Name != "y" {
		t.Errorf("repo = %+v", call.Repo)
	}
	if call.Inputs["run_id"] != r.ID.String() {
		t.Errorf("inputs.run_id = %q", call.Inputs["run_id"])
	}
	if call.Inputs["stage_id"] != stages[1].ID.String() {
		t.Errorf("inputs.stage_id = %q", call.Inputs["stage_id"])
	}
}

func TestAdvance_HumanStage_TransitionsToAwaitingApproval(t *testing.T) {
	o, rs, gh := newOrchestrator(t)
	_, stages := rs.seed(t, "x/y", int64Ptr(42), []stageSeed{
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStateSucceeded},
		{Type: run.StageTypeReview, ExecutorKind: run.ExecutorHuman, ExecutorRef: "human", State: run.StageStatePending},
	})
	out, err := o.Advance(context.Background(), stages[0].RunID)
	if err != nil {
		t.Fatal(err)
	}
	if out != OutcomeDispatched {
		t.Errorf("Outcome = %q", out)
	}
	if stages[1].State != run.StageStateAwaitingApproval {
		t.Errorf("human stage state = %q, want awaiting_approval", stages[1].State)
	}
	// Human stages don't fire workflow_dispatch.
	if len(gh.calls) != 0 {
		t.Errorf("workflow_dispatch fired for human stage: %d", len(gh.calls))
	}
}

func TestAdvance_AutoMergeStage_QueuesAndSucceeds(t *testing.T) {
	// routine_change canonical case (#255 / ADR-017): the review
	// stage carries a check-only gate. Advance must queue
	// gh pr merge --auto rather than fire workflow_dispatch, then
	// transition the stage straight to succeeded — Fishhawk's role
	// is done; GitHub owns the merge.
	o, rs, gh := newOrchestrator(t)
	r, stages := rs.seed(t, "kuhlman-labs/example", int64Ptr(99), []stageSeed{
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStateSucceeded},
		{Type: run.StageTypeReview, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStatePending},
	})
	stages[1].Gate = &run.Gate{Kind: run.GateKindCheck}
	prURL := "https://github.com/kuhlman-labs/example/pull/42"
	r.PullRequestURL = &prURL

	out, err := o.Advance(context.Background(), r.ID)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if out != OutcomeDispatched {
		t.Errorf("Outcome = %q, want dispatched", out)
	}
	if stages[1].State != run.StageStateSucceeded {
		t.Errorf("review stage state = %q, want succeeded", stages[1].State)
	}
	if len(gh.calls) != 0 {
		t.Errorf("workflow_dispatch fired for auto-merge stage: %d", len(gh.calls))
	}
	if len(gh.autoMergeCalls) != 1 {
		t.Fatalf("auto-merge calls = %d, want 1", len(gh.autoMergeCalls))
	}
	got := gh.autoMergeCalls[0]
	if got.PRNumber != 42 || got.InstallationID != 99 || got.Method != githubclient.MergeMethodSquash {
		t.Errorf("auto-merge call = %+v", got)
	}
	if got.Repo.Owner != "kuhlman-labs" || got.Repo.Name != "example" {
		t.Errorf("repo = %+v", got.Repo)
	}
}

func TestAdvance_AutoMergeStage_FailureLeavesStageDispatched(t *testing.T) {
	// Best-effort: a GitHub-side rejection (auto-merge disabled on
	// the repo, branch protection misconfigured, etc.) leaves the
	// stage in dispatched and surfaces the error. Re-running
	// Advance retries — same idempotency posture as workflow_dispatch.
	o, rs, gh := newOrchestrator(t)
	r, stages := rs.seed(t, "kuhlman-labs/example", int64Ptr(99), []stageSeed{
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStateSucceeded},
		{Type: run.StageTypeReview, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStatePending},
	})
	stages[1].Gate = &run.Gate{Kind: run.GateKindCheck}
	prURL := "https://github.com/kuhlman-labs/example/pull/42"
	r.PullRequestURL = &prURL
	gh.autoMergeErr = errors.New("auto-merge not enabled on this repo")

	if _, err := o.Advance(context.Background(), r.ID); err == nil {
		t.Fatal("Advance returned nil err; want error from auto-merge enable")
	}
	if stages[1].State != run.StageStateDispatched {
		t.Errorf("stage state = %q, want dispatched (not transitioned past)", stages[1].State)
	}
}

func TestAdvance_AutoMergeStage_MissingPRURL_Errors(t *testing.T) {
	// The implement stage's PR artifact upload backfills
	// runs.pull_request_url (#216). When that hasn't happened yet
	// the orchestrator has no PR number to call against — surface
	// the gap as an error rather than calling enable-auto-merge with
	// a zero number.
	o, rs, gh := newOrchestrator(t)
	r, stages := rs.seed(t, "kuhlman-labs/example", int64Ptr(99), []stageSeed{
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStateSucceeded},
		{Type: run.StageTypeReview, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStatePending},
	})
	stages[1].Gate = &run.Gate{Kind: run.GateKindCheck}
	// PullRequestURL deliberately nil.

	if _, err := o.Advance(context.Background(), r.ID); err == nil {
		t.Fatal("Advance returned nil err; want missing-pr-url error")
	}
	if len(gh.autoMergeCalls) != 0 {
		t.Errorf("auto-merge fired without a PR url: %+v", gh.autoMergeCalls)
	}
}

func TestPullRequestNumberFromURL(t *testing.T) {
	cases := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{"https://github.com/x/y/pull/42", 42, false},
		{"https://github.com/x/y/pull/1", 1, false},
		{"https://github.com/x/y/pull/123/files", 123, false},
		{"https://github.com/x/y/pull/456?diff=split", 456, false},
		{"https://github.com/x/y/issues/42", 0, true},
		{"https://github.com/x/y/pull/abc", 0, true},
		{"https://github.com/x/y/pull/0", 0, true},
		{"", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := pullRequestNumberFromURL(tc.in)
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestAdvance_AllStagesTerminal_TransitionsRun(t *testing.T) {
	o, rs, gh := newOrchestrator(t)
	r, _ := rs.seed(t, "x/y", int64Ptr(42), []stageSeed{
		{Type: run.StageTypePlan, ExecutorKind: run.ExecutorAgent, State: run.StageStateSucceeded},
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, State: run.StageStateSucceeded},
	})
	out, err := o.Advance(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if out != OutcomeRunCompleted {
		t.Errorf("Outcome = %q, want run_completed", out)
	}
	if rs.runs[r.ID].State != run.StateSucceeded {
		t.Errorf("run state = %q, want succeeded", rs.runs[r.ID].State)
	}
	if len(gh.calls) != 0 {
		t.Errorf("dispatch fired when no next stage: %d", len(gh.calls))
	}
}

func TestAdvance_AnyStageFailed_RunFails(t *testing.T) {
	o, rs, _ := newOrchestrator(t)
	r, _ := rs.seed(t, "x/y", int64Ptr(42), []stageSeed{
		{Type: run.StageTypePlan, ExecutorKind: run.ExecutorAgent, State: run.StageStateSucceeded},
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, State: run.StageStateFailed},
	})
	out, err := o.Advance(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if out != OutcomeRunCompleted {
		t.Errorf("Outcome = %q", out)
	}
	if rs.runs[r.ID].State != run.StateFailed {
		t.Errorf("run state = %q, want failed", rs.runs[r.ID].State)
	}
}

func TestAdvance_TerminalRunIsNoOp(t *testing.T) {
	o, rs, gh := newOrchestrator(t)
	r, _ := rs.seed(t, "x/y", int64Ptr(42), []stageSeed{
		{Type: run.StageTypePlan, ExecutorKind: run.ExecutorAgent, State: run.StageStateSucceeded},
	})
	rs.runs[r.ID].State = run.StateSucceeded

	out, err := o.Advance(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if out != OutcomeNoOp {
		t.Errorf("Outcome = %q, want noop", out)
	}
	if len(gh.calls) != 0 {
		t.Errorf("dispatch on terminal run: %d", len(gh.calls))
	}
}

func TestAdvance_NoInstallationID_SkipsDispatchButTransitions(t *testing.T) {
	// trigger_source=cli runs don't have an installation_id.
	// Orchestrator should still transition the next agent stage
	// (so its state is observable) but skip the workflow_dispatch
	// firing.
	o, rs, gh := newOrchestrator(t)
	_, stages := rs.seed(t, "x/y", nil, []stageSeed{
		{Type: run.StageTypePlan, ExecutorKind: run.ExecutorAgent, State: run.StageStateSucceeded},
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, State: run.StageStatePending},
	})
	if _, err := o.Advance(context.Background(), stages[0].RunID); err != nil {
		t.Fatal(err)
	}
	if stages[1].State != run.StageStateDispatched {
		t.Errorf("stage state = %q, want dispatched", stages[1].State)
	}
	if len(gh.calls) != 0 {
		t.Errorf("dispatch fired without installation_id: %d", len(gh.calls))
	}
}

func TestAdvance_GitHubNil_SkipsDispatchButTransitions(t *testing.T) {
	o, rs, _ := newOrchestrator(t)
	o.GitHub = nil
	_, stages := rs.seed(t, "x/y", int64Ptr(42), []stageSeed{
		{Type: run.StageTypePlan, ExecutorKind: run.ExecutorAgent, State: run.StageStateSucceeded},
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, State: run.StageStatePending},
	})
	if _, err := o.Advance(context.Background(), stages[0].RunID); err != nil {
		t.Fatal(err)
	}
	if stages[1].State != run.StageStateDispatched {
		t.Errorf("stage state = %q, want dispatched", stages[1].State)
	}
}

func TestAdvance_RunsNil_Errors(t *testing.T) {
	o := &Orchestrator{}
	_, err := o.Advance(context.Background(), uuid.New())
	if err == nil {
		t.Fatal("expected error on nil Runs")
	}
}

func TestAdvance_GetRunError(t *testing.T) {
	o, rs, _ := newOrchestrator(t)
	rs.getRunErr = errors.New("db down")
	_, err := o.Advance(context.Background(), uuid.New())
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestAdvance_ListStagesError(t *testing.T) {
	o, rs, _ := newOrchestrator(t)
	r, _ := rs.seed(t, "x/y", int64Ptr(42), nil)
	rs.listStagesErr = errors.New("db down")
	_, err := o.Advance(context.Background(), r.ID)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestAdvance_TransitionStageError_Bubbles(t *testing.T) {
	o, rs, _ := newOrchestrator(t)
	r, _ := rs.seed(t, "x/y", int64Ptr(42), []stageSeed{
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, State: run.StageStatePending},
	})
	rs.transitionErr = errors.New("state machine refusal")
	_, err := o.Advance(context.Background(), r.ID)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestAdvance_DispatchError_StageStillTransitioned(t *testing.T) {
	// If GitHub fails the workflow_dispatch call AFTER we've
	// already transitioned the stage to dispatched, surface the
	// error but the stage is now in dispatched. The runner can be
	// woken manually + a fresh Advance hits the idempotent path.
	o, rs, gh := newOrchestrator(t)
	gh.dispatchErr = errors.New("github 500")
	r, stages := rs.seed(t, "x/y", int64Ptr(42), []stageSeed{
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStatePending},
	})
	out, err := o.Advance(context.Background(), r.ID)
	if err == nil {
		t.Fatal("expected dispatch error")
	}
	if out != OutcomeDispatched {
		t.Errorf("Outcome = %q, want dispatched (state machine moved forward)", out)
	}
	if stages[0].State != run.StageStateDispatched {
		t.Errorf("stage state = %q, want dispatched", stages[0].State)
	}
}

func TestAdvance_BadRepo_DispatchSkipped(t *testing.T) {
	o, rs, gh := newOrchestrator(t)
	r, _ := rs.seed(t, "no-slash", int64Ptr(42), []stageSeed{
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, State: run.StageStatePending},
	})
	_, err := o.Advance(context.Background(), r.ID)
	if err == nil {
		t.Fatal("expected error on malformed repo")
	}
	if len(gh.calls) != 0 {
		t.Errorf("dispatch with bad repo: %d", len(gh.calls))
	}
}

func TestParseRepo(t *testing.T) {
	cases := []struct {
		in    string
		ok    bool
		owner string
		name  string
	}{
		{"x/y", true, "x", "y"},
		{"kuhlman-labs/fishhawk", true, "kuhlman-labs", "fishhawk"},
		{"no-slash", false, "", ""},
		{"/y", false, "", ""},
		{"x/", false, "", ""},
		{"", false, "", ""},
	}
	for _, c := range cases {
		got, err := parseRepo(c.in)
		if c.ok != (err == nil) {
			t.Errorf("parseRepo(%q): err=%v, wantOK=%v", c.in, err, c.ok)
		}
		if c.ok && (got.Owner != c.owner || got.Name != c.name) {
			t.Errorf("parseRepo(%q) = %+v", c.in, got)
		}
	}
}

func TestAdvance_PendingRun_WalksToRunningBeforeProcessingStages(t *testing.T) {
	// Regression for the "runs stuck in pending" bug: every run
	// is created in StatePending, but the state machine rejects
	// pending → terminal directly. Without an explicit pending →
	// running step in Advance, completeRun fails and the run is
	// stuck forever.
	//
	// All-stages-succeeded path: Advance must walk pending →
	// running → succeeded.
	o, rs, _ := newOrchestrator(t)
	r, _ := rs.seed(t, "x/y", int64Ptr(42), []stageSeed{
		{Type: run.StageTypePlan, ExecutorKind: run.ExecutorAgent, State: run.StageStateSucceeded},
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, State: run.StageStateSucceeded},
	})
	rs.runs[r.ID].State = run.StatePending // override the helper's default Running

	out, err := o.Advance(context.Background(), r.ID)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if out != OutcomeRunCompleted {
		t.Errorf("Outcome = %q, want run_completed", out)
	}
	if rs.runs[r.ID].State != run.StateSucceeded {
		t.Errorf("run state = %q, want succeeded (pending → running → succeeded)", rs.runs[r.ID].State)
	}
}

func TestAdvance_PendingRun_WithFailedStage_WalksToFailed(t *testing.T) {
	// The all-failures variant of the regression: a stage failed
	// while the run was still in pending. Advance must walk
	// pending → running → failed; without that, every run with a
	// failed stage stays stuck in pending too.
	o, rs, _ := newOrchestrator(t)
	r, _ := rs.seed(t, "x/y", int64Ptr(42), []stageSeed{
		{Type: run.StageTypePlan, ExecutorKind: run.ExecutorAgent, State: run.StageStateFailed},
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, State: run.StageStatePending},
	})
	rs.runs[r.ID].State = run.StatePending

	out, err := o.Advance(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if out != OutcomeRunCompleted {
		t.Errorf("Outcome = %q, want run_completed", out)
	}
	if rs.runs[r.ID].State != run.StateFailed {
		t.Errorf("run state = %q, want failed (pending → running → failed)", rs.runs[r.ID].State)
	}
}

func TestAdvance_FailedStageBeforePending_DoesNotDispatchDownstream(t *testing.T) {
	// When stage 0 has failed and stage 1 is still pending, the
	// orchestrator must not dispatch stage 1 — its upstream
	// output never landed. The run completes as failed instead.
	// Without this short-circuit, a rejected gate or
	// constraint-violation failure on stage 0 would still fire
	// the implement stage's runner, wasting the run and leaving
	// the audit log telling two contradictory stories.
	o, rs, gh := newOrchestrator(t)
	r, _ := rs.seed(t, "x/y", int64Ptr(42), []stageSeed{
		{Type: run.StageTypePlan, ExecutorKind: run.ExecutorAgent, State: run.StageStateFailed},
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, State: run.StageStatePending},
	})

	if _, err := o.Advance(context.Background(), r.ID); err != nil {
		t.Fatal(err)
	}
	if rs.runs[r.ID].State != run.StateFailed {
		t.Errorf("run state = %q, want failed", rs.runs[r.ID].State)
	}
	if len(gh.calls) != 0 {
		t.Errorf("orchestrator dispatched a stage past the failure: %d calls", len(gh.calls))
	}
}

func TestCompleteRun_AllChildrenSucceed_AdvancesParent(t *testing.T) {
	// D4 inline hook: when the last child of a decomposed parent
	// completes successfully, completeRun should inline-advance the
	// parent's awaiting_children stage to succeeded and dispatch the
	// next parent stage (review). This avoids a sweeper round-trip.
	o, rs, _ := newOrchestrator(t)

	// Parent run: implement (awaiting_children) + review (pending).
	parentRun, parentStages := rs.seed(t, "x/y", nil, []stageSeed{
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, State: run.StageStateAwaitingChildren},
		{Type: run.StageTypeReview, ExecutorKind: run.ExecutorHuman, State: run.StageStatePending},
	})

	// First child: already succeeded.
	child1, _ := rs.seed(t, "x/y", nil, nil)
	child1.DecomposedFrom = &parentRun.ID
	child1.State = run.StateSucceeded

	// Second child: still running, implement stage succeeded.
	// Calling Advance will complete it and trigger the inline hook.
	child2, _ := rs.seed(t, "x/y", nil, []stageSeed{
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, State: run.StageStateSucceeded},
	})
	child2.DecomposedFrom = &parentRun.ID

	out, err := o.Advance(context.Background(), child2.ID)
	if err != nil {
		t.Fatalf("Advance(child2): %v", err)
	}
	if out != OutcomeRunCompleted {
		t.Errorf("Outcome = %q, want run_completed", out)
	}

	// Parent's implement stage must be succeeded (transitioned from awaiting_children).
	if parentStages[0].State != run.StageStateSucceeded {
		t.Errorf("parent implement stage = %q, want succeeded", parentStages[0].State)
	}
	// Parent's review stage must have been dispatched (human → awaiting_approval).
	if parentStages[1].State != run.StageStateAwaitingApproval {
		t.Errorf("parent review stage = %q, want awaiting_approval", parentStages[1].State)
	}
	// Parent run stays running while the review gate is open.
	if rs.runs[parentRun.ID].State != run.StateRunning {
		t.Errorf("parent run state = %q, want running", rs.runs[parentRun.ID].State)
	}
}

// auditHasCategory reports whether the recordingAudit captured an
// AppendChained call for the given category.
func auditHasCategory(a *recordingAudit, category string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, p := range a.appended {
		if p.Category == category {
			return true
		}
	}
	return false
}

func TestCompleteRun_AllFailedChildrenRetryable_ParksParent(t *testing.T) {
	// #698: when a child fails with a retryable category (C), the
	// event-driven parent-resolution path must NOT resolve the parent
	// to failed-C; it parks the awaiting_children stage and emits a
	// one-time parent_awaiting_redrive audit so an operator can
	// re-drive without racing the sweeper.
	o, rs, _ := newOrchestrator(t)
	au := &recordingAudit{}
	o.Audit = au

	parentRun, parentStages := rs.seed(t, "x/y", nil, []stageSeed{
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, State: run.StageStateAwaitingChildren},
		{Type: run.StageTypeReview, ExecutorKind: run.ExecutorHuman, State: run.StageStatePending},
	})

	// Child fails now with a retryable infra (C) implement failure.
	cat := run.FailureC
	reason := "runner OOM"
	child, childStages := rs.seed(t, "x/y", nil, []stageSeed{
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, State: run.StageStateRunning},
	})
	child.DecomposedFrom = &parentRun.ID
	childStages[0].State = run.StageStateFailed
	childStages[0].FailureCategory = &cat
	childStages[0].FailureReason = &reason

	if _, err := o.Advance(context.Background(), child.ID); err != nil {
		t.Fatalf("Advance(child): %v", err)
	}

	// Parent's implement stage must remain parked in awaiting_children.
	if parentStages[0].State != run.StageStateAwaitingChildren {
		t.Errorf("parent implement stage = %q, want awaiting_children (parked)", parentStages[0].State)
	}
	// Parent run must stay running, not resolved to failed.
	if rs.runs[parentRun.ID].State != run.StateRunning {
		t.Errorf("parent run state = %q, want running (parked)", rs.runs[parentRun.ID].State)
	}
	if !auditHasCategory(au, "parent_awaiting_redrive") {
		t.Errorf("audit categories = %v, want a parent_awaiting_redrive entry", au.appended)
	}
}

func TestCompleteRun_FailedChildCategoryB_ResolvesFailed(t *testing.T) {
	// #698: a genuine non-retryable category-B child failure still
	// resolves the parent to failed-C (no parking).
	o, rs, _ := newOrchestrator(t)
	au := &recordingAudit{}
	o.Audit = au

	parentRun, parentStages := rs.seed(t, "x/y", nil, []stageSeed{
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, State: run.StageStateAwaitingChildren},
		{Type: run.StageTypeReview, ExecutorKind: run.ExecutorHuman, State: run.StageStatePending},
	})

	cat := run.FailureB
	reason := "scope violation"
	child, childStages := rs.seed(t, "x/y", nil, []stageSeed{
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, State: run.StageStateRunning},
	})
	child.DecomposedFrom = &parentRun.ID
	childStages[0].State = run.StageStateFailed
	childStages[0].FailureCategory = &cat
	childStages[0].FailureReason = &reason

	if _, err := o.Advance(context.Background(), child.ID); err != nil {
		t.Fatalf("Advance(child): %v", err)
	}

	if parentStages[0].State != run.StageStateFailed {
		t.Errorf("parent implement stage = %q, want failed (B is non-retryable)", parentStages[0].State)
	}
	if rs.runs[parentRun.ID].State != run.StateFailed {
		t.Errorf("parent run state = %q, want failed", rs.runs[parentRun.ID].State)
	}
	if auditHasCategory(au, "parent_awaiting_redrive") {
		t.Errorf("parent_awaiting_redrive emitted for non-retryable B child: %v", au.appended)
	}
}

func TestCompleteRun_OneChildFails_AdvancesParentToFailed(t *testing.T) {
	// D4 inline hook: when any child failed, the parent's
	// awaiting_children stage must transition to failed (category C)
	// and the parent run must complete as failed.
	o, rs, _ := newOrchestrator(t)

	// Parent run: implement (awaiting_children) + review (pending).
	parentRun, parentStages := rs.seed(t, "x/y", nil, []stageSeed{
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, State: run.StageStateAwaitingChildren},
		{Type: run.StageTypeReview, ExecutorKind: run.ExecutorHuman, State: run.StageStatePending},
	})

	// First child: already failed.
	child1, _ := rs.seed(t, "x/y", nil, nil)
	child1.DecomposedFrom = &parentRun.ID
	child1.State = run.StateFailed

	// Second child: succeeds now, triggering the inline hook.
	child2, _ := rs.seed(t, "x/y", nil, []stageSeed{
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, State: run.StageStateSucceeded},
	})
	child2.DecomposedFrom = &parentRun.ID

	out, err := o.Advance(context.Background(), child2.ID)
	if err != nil {
		t.Fatalf("Advance(child2): %v", err)
	}
	if out != OutcomeRunCompleted {
		t.Errorf("Outcome = %q, want run_completed", out)
	}

	// Parent's implement stage must be failed.
	if parentStages[0].State != run.StageStateFailed {
		t.Errorf("parent implement stage = %q, want failed", parentStages[0].State)
	}
	// Parent run must be failed.
	if rs.runs[parentRun.ID].State != run.StateFailed {
		t.Errorf("parent run state = %q, want failed", rs.runs[parentRun.ID].State)
	}
}
