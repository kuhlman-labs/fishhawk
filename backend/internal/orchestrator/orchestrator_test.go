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
func (s *stubRuns) ListRuns(context.Context, run.ListRunsFilter) ([]*run.Run, error) {
	return nil, errors.New("not used")
}
func (s *stubRuns) CreateStage(context.Context, run.CreateStageParams) (*run.Stage, error) {
	return nil, errors.New("not used")
}
func (s *stubRuns) GetStage(context.Context, uuid.UUID) (*run.Stage, error) {
	return nil, errors.New("not used")
}

// stubGitHub records DispatchWorkflow calls without making network
// requests.
type stubGitHub struct {
	mu          sync.Mutex
	calls       []dispatchCall
	dispatchErr error
}

type dispatchCall struct {
	InstallationID int64
	Repo           githubclient.RepoRef
	WorkflowFile   string
	Ref            string
	Inputs         githubclient.DispatchInputs
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
