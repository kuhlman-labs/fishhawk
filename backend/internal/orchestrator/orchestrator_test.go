package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
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
	listStagesErrIDs map[uuid.UUID]error // per-run ListStagesForRun failure (reconcile skip path)
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
	if err, ok := s.listStagesErrIDs[runID]; ok {
		return nil, err
	}
	if s.listStagesErr != nil {
		return nil, s.listStagesErr
	}
	return s.stages[runID], nil
}

func (s *stubRuns) ListStagesAwaitingApproval(context.Context) ([]*run.Stage, error) {
	return nil, nil
}
func (s *stubRuns) ListReviewStagesAwaitingApproval(context.Context) ([]*run.Stage, error) {
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

func (s *stubRuns) RetryRun(_ context.Context, id uuid.UUID, to run.State) (*run.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.runs[id]
	if r == nil {
		return nil, run.ErrNotFound
	}
	if !run.ValidRunRetryTransition(r.State, to) {
		return nil, run.InvalidTransitionError{Kind: "run", From: string(r.State), To: string(to)}
	}
	r.State = to
	s.runTransitions = append(s.runTransitions, runTransition{RunID: id, To: to})
	return r, nil
}

func (s *stubRuns) TransitionStage(_ context.Context, id uuid.UUID, to run.StageState, completion *run.StageCompletion) (*run.Stage, error) {
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
				// Record failure metadata so tests can assert the category/
				// reason a failing transition carried (the slice-integration
				// conflict path stamps failed-B with a stable reason).
				if completion != nil {
					st.FailureCategory = completion.FailureCategory
					st.FailureReason = completion.FailureReason
				}
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
	// State filter (ReconcileStuckRuns). Returns every matching run in
	// one page; the seeded fixtures stay well under the sweep page size
	// so the caller breaks after the first page (Offset never advances).
	if f.State != "" {
		var out []*run.Run
		for _, r := range s.runs {
			if string(r.State) == f.State {
				out = append(out, r)
			}
		}
		return out, nil
	}
	if f.DecomposedFrom == nil {
		return nil, errors.New("not used")
	}
	var out []*run.Run
	for _, r := range s.runs {
		if r.DecomposedFrom != nil && *r.DecomposedFrom == *f.DecomposedFrom {
			out = append(out, r)
		}
	}
	// Deterministic order so pagination (Offset/Limit) windows are stable
	// across calls: ascending by SliceIndex (nil last), then by id.
	sort.SliceStable(out, func(i, j int) bool {
		si, sj := out[i].SliceIndex, out[j].SliceIndex
		switch {
		case si == nil && sj == nil:
			return out[i].ID.String() < out[j].ID.String()
		case si == nil:
			return false
		case sj == nil:
			return true
		case *si != *sj:
			return *si < *sj
		default:
			return out[i].ID.String() < out[j].ID.String()
		}
	})
	// Honor Offset/Limit so the fan-in pagination walk (#1142) is exercised.
	if f.Offset > 0 {
		if f.Offset >= len(out) {
			return nil, nil
		}
		out = out[f.Offset:]
	}
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}
func (s *stubRuns) SetRunPullRequestURL(_ context.Context, id uuid.UUID, url string) (*run.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.runs[id]
	if r == nil {
		return nil, run.ErrNotFound
	}
	u := url
	r.PullRequestURL = &u
	return r, nil
}
func (s *stubRuns) CreateStage(context.Context, run.CreateStageParams) (*run.Stage, error) {
	return nil, errors.New("not used")
}
func (s *stubRuns) GetStage(context.Context, uuid.UUID) (*run.Stage, error) {
	return nil, errors.New("not used")
}

// stubGitHub records DispatchWorkflow + EnableAutoMerge +
// CreatePullRequest calls without making network requests.
type stubGitHub struct {
	mu             sync.Mutex
	calls          []dispatchCall
	dispatchErr    error
	autoMergeCalls []autoMergeCall
	autoMergeErr   error

	createPRCalls []createPRCall
	// createPRErr, when set, is returned from CreatePullRequest. Set
	// to githubclient.ErrPullRequestExists to exercise the lost-race
	// recovery path.
	createPRErr error
	// createPRURL is the html_url CreatePullRequest returns on success
	// (defaults to a canned URL when empty).
	createPRURL string
	// listByHeadResult is what ListOpenPullRequestsByHead returns (the
	// recovery lookup). listByHeadErr forces an error from it.
	listByHeadResult []githubclient.PullRequest
	listByHeadErr    error
	listByHeadCalls  int

	// Fan-in (#1142) recording + programming. branchSHAs maps an existing
	// branch to its tip sha (absence => GetBranchSHA reports not-found).
	branchSHAs      map[string]string
	getBranchSHAErr error
	createRefErr    error
	createRefCalls  []createRefCall
	// mergeErrByHead programs a per-head-branch MergeBranch error (e.g.
	// githubclient.ErrMergeConflict on a specific slice branch).
	mergeErrByHead map[string]error
	mergeCalls     []mergeBranchCall
}

type createRefCall struct {
	Branch string
	SHA    string
}

type mergeBranchCall struct {
	Base string
	Head string
	Msg  string
}

type createPRCall struct {
	InstallationID int64
	Repo           githubclient.RepoRef
	Head           string
	Base           string
	Title          string
	Body           string
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

func (g *stubGitHub) CreatePullRequest(_ context.Context, installationID int64,
	repo githubclient.RepoRef, head, base, title, body string) (*githubclient.PullRequest, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.createPRCalls = append(g.createPRCalls, createPRCall{
		InstallationID: installationID, Repo: repo,
		Head: head, Base: base, Title: title, Body: body,
	})
	if g.createPRErr != nil {
		return nil, g.createPRErr
	}
	url := g.createPRURL
	if url == "" {
		url = "https://github.com/" + repo.Owner + "/" + repo.Name + "/pull/777"
	}
	return &githubclient.PullRequest{Number: 777, HTMLURL: url, State: "open"}, nil
}

func (g *stubGitHub) ListOpenPullRequestsByHead(_ context.Context, _ int64,
	_ githubclient.RepoRef, _, _ string) ([]githubclient.PullRequest, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.listByHeadCalls++
	if g.listByHeadErr != nil {
		return nil, g.listByHeadErr
	}
	return g.listByHeadResult, nil
}

func (g *stubGitHub) GetBranchSHA(_ context.Context, _ int64,
	_ githubclient.RepoRef, branch string) (string, bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.getBranchSHAErr != nil {
		return "", false, g.getBranchSHAErr
	}
	sha, ok := g.branchSHAs[branch]
	if !ok {
		return "", false, nil
	}
	return sha, true, nil
}

func (g *stubGitHub) CreateRef(_ context.Context, _ int64,
	_ githubclient.RepoRef, branch, sha string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.createRefCalls = append(g.createRefCalls, createRefCall{Branch: branch, SHA: sha})
	if g.createRefErr != nil {
		return g.createRefErr
	}
	if g.branchSHAs == nil {
		g.branchSHAs = map[string]string{}
	}
	g.branchSHAs[branch] = sha
	return nil
}

func (g *stubGitHub) MergeBranch(_ context.Context, _ int64,
	_ githubclient.RepoRef, base, head, msg string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.mergeCalls = append(g.mergeCalls, mergeBranchCall{Base: base, Head: head, Msg: msg})
	if err, ok := g.mergeErrByHead[head]; ok {
		return err
	}
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

func TestAdvance_GatedStageNoPending_NoOp(t *testing.T) {
	// #968: the exact incident shape — nothing pending, but the review
	// gate is still open at awaiting_approval. Advance must NOT route to
	// completeRun (which stamped run 68e13183 succeeded with its gate
	// open); it no-ops and the run stays running at the gate.
	o, rs, gh := newOrchestrator(t)
	r, _ := rs.seed(t, "x/y", int64Ptr(42), []stageSeed{
		{Type: run.StageTypePlan, ExecutorKind: run.ExecutorAgent, State: run.StageStateSucceeded},
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, State: run.StageStateSucceeded},
		{Type: run.StageTypeReview, ExecutorKind: run.ExecutorHuman, State: run.StageStateAwaitingApproval},
	})

	out, err := o.Advance(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if out != OutcomeNoOp {
		t.Errorf("Outcome = %q, want noop", out)
	}
	if rs.runs[r.ID].State != run.StateRunning {
		t.Errorf("run state = %q, want running (gate still open)", rs.runs[r.ID].State)
	}
	if len(rs.runTransitions) != 0 {
		t.Errorf("run transitions = %d, want 0", len(rs.runTransitions))
	}
	if len(gh.calls) != 0 {
		t.Errorf("dispatch fired with nothing pending: %d", len(gh.calls))
	}
}

func TestAdvance_AwaitingChildrenNoPending_NoOp(t *testing.T) {
	// #968: same invariant for a decomposed parent parked at
	// awaiting_children — non-terminal but not pending must hold the run
	// open, not complete it.
	o, rs, gh := newOrchestrator(t)
	r, _ := rs.seed(t, "x/y", int64Ptr(42), []stageSeed{
		{Type: run.StageTypePlan, ExecutorKind: run.ExecutorAgent, State: run.StageStateSucceeded},
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, State: run.StageStateAwaitingChildren},
	})

	out, err := o.Advance(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if out != OutcomeNoOp {
		t.Errorf("Outcome = %q, want noop", out)
	}
	if rs.runs[r.ID].State != run.StateRunning {
		t.Errorf("run state = %q, want running (children still settling)", rs.runs[r.ID].State)
	}
	if len(gh.calls) != 0 {
		t.Errorf("dispatch fired with nothing pending: %d", len(gh.calls))
	}
}

func TestCompleteRun_RefusesSucceededWithNonTerminalStage(t *testing.T) {
	// #968 belt-and-suspenders: completeRun itself refuses to stamp a run
	// succeeded while any stage is non-terminal, covering every caller.
	o, rs, _ := newOrchestrator(t)
	r, stages := rs.seed(t, "x/y", int64Ptr(42), []stageSeed{
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, State: run.StageStateSucceeded},
		{Type: run.StageTypeReview, ExecutorKind: run.ExecutorHuman, State: run.StageStateAwaitingApproval},
	})

	out, err := o.completeRun(context.Background(), r, stages)
	if err != nil {
		t.Fatal(err)
	}
	if out != OutcomeNoOp {
		t.Errorf("Outcome = %q, want noop", out)
	}
	if rs.runs[r.ID].State != run.StateRunning {
		t.Errorf("run state = %q, want running", rs.runs[r.ID].State)
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

func TestCompleteRun_FailedChildCategoryB_ParksParent(t *testing.T) {
	// #1081: a category-B child is now recoverable in decomposition
	// (re-driven in place via the recover path), so the event-driven
	// parent-resolution path parks the awaiting_children stage and emits
	// parent_awaiting_redrive rather than resolving to failed-C.
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

	if parentStages[0].State != run.StageStateAwaitingChildren {
		t.Errorf("parent implement stage = %q, want awaiting_children (parked for recoverable B)", parentStages[0].State)
	}
	if rs.runs[parentRun.ID].State != run.StateRunning {
		t.Errorf("parent run state = %q, want running (parked)", rs.runs[parentRun.ID].State)
	}
	if !auditHasCategory(au, "parent_awaiting_redrive") {
		t.Errorf("audit categories = %v, want a parent_awaiting_redrive entry", au.appended)
	}
}

func TestCompleteRun_FailedChildNonRecoverable_ResolvesFailed(t *testing.T) {
	// A D-rejection child (approver said no) stays non-recoverable and
	// still resolves the parent to failed-C with no parking.
	o, rs, _ := newOrchestrator(t)
	au := &recordingAudit{}
	o.Audit = au

	parentRun, parentStages := rs.seed(t, "x/y", nil, []stageSeed{
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, State: run.StageStateAwaitingChildren},
		{Type: run.StageTypeReview, ExecutorKind: run.ExecutorHuman, State: run.StageStatePending},
	})

	cat := run.FailureD
	reason := "gate rejected by approver"
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
		t.Errorf("parent implement stage = %q, want failed (D-rejection is non-recoverable)", parentStages[0].State)
	}
	if rs.runs[parentRun.ID].State != run.StateFailed {
		t.Errorf("parent run state = %q, want failed", rs.runs[parentRun.ID].State)
	}
	if auditHasCategory(au, "parent_awaiting_redrive") {
		t.Errorf("parent_awaiting_redrive emitted for non-recoverable D-rejection child: %v", au.appended)
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

// --- Consolidated decomposition PR (#714 / ADR-032) ---

// seedDecomposedParent seeds a decomposed parent (implement succeeded,
// review pending) plus one already-succeeded child, returning both so
// the test can drive Advance(parent) straight into the review gate.
func seedDecomposedParent(t *testing.T, rs *stubRuns, installationID *int64, reviewKind run.ExecutorKind) (*run.Run, []*run.Stage) {
	t.Helper()
	parent, stages := rs.seed(t, "kuhlman-labs/fishhawk", installationID, []stageSeed{
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, State: run.StageStateSucceeded},
		{Type: run.StageTypeReview, ExecutorKind: reviewKind, ExecutorRef: "human", State: run.StageStatePending},
	})
	child, _ := rs.seed(t, "kuhlman-labs/fishhawk", installationID, nil)
	child.DecomposedFrom = &parent.ID
	child.State = run.StateSucceeded
	return parent, stages
}

func TestBranchNames_NoDFConflict(t *testing.T) {
	// Contract assertion #1243: the slice-branch name MUST stay
	// byte-identical to the runner's childSliceBranch convention
	// (fishhawk/run-<first8>/slice-<n>), while the consolidated branch is
	// renamed to the non-nesting sibling fishhawk/run-<first8>-consolidated.
	// The two MUST NOT share a path prefix or git's ref store rejects the
	// create-ref with a directory/file (D/F) conflict — the 422 that broke
	// fan-in 100% in production. This catches a regression in the unit
	// suite, not only the Docker e2e.
	id := uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")

	// (i) consolidated branch is the -consolidated sibling.
	if got := consolidatedBranch(id); got != "fishhawk/run-aaaaaaaa-consolidated" {
		t.Errorf("consolidatedBranch = %q, want fishhawk/run-aaaaaaaa-consolidated", got)
	}

	// (i.a) The exported wrapper used by out-of-package consumers
	// (server.fixupBranchForRun, #1245) MUST emit the byte-identical name.
	// Locking the exported symbol + its output here means the wrapper cannot
	// be removed or renamed — or desync from the unexported formula — without
	// failing this unit suite.
	if got := ConsolidatedBranch(id); got != "fishhawk/run-aaaaaaaa-consolidated" {
		t.Errorf("ConsolidatedBranch = %q, want fishhawk/run-aaaaaaaa-consolidated", got)
	}

	// (ii) slice branch unchanged — byte-identical to the runner's name.
	if got := sliceBranch(id, 0); got != "fishhawk/run-aaaaaaaa/slice-0" {
		t.Errorf("sliceBranch(id,0) = %q, want fishhawk/run-aaaaaaaa/slice-0", got)
	}

	// (iii) D/F regression guard: the consolidated name is never a path
	// prefix of any slice name, and vice-versa. A path prefix means one ref
	// nests under the other, which is the D/F conflict.
	cons := consolidatedBranch(id)
	for n := 0; n < 4; n++ {
		slice := sliceBranch(id, n)
		if strings.HasPrefix(slice, cons+"/") {
			t.Errorf("slice %q nests under consolidated %q (D/F conflict)", slice, cons)
		}
		if strings.HasPrefix(cons, slice+"/") {
			t.Errorf("consolidated %q nests under slice %q (D/F conflict)", cons, slice)
		}
		// Exact-equality would also be a collision.
		if slice == cons {
			t.Errorf("slice %q equals consolidated %q", slice, cons)
		}
	}
}

func TestAdvance_DecomposedParent_OpensConsolidatedPR(t *testing.T) {
	o, rs, gh := newOrchestrator(t)
	o.DefaultRef = "main"
	au := &recordingAudit{}
	o.Audit = au

	parent, stages := seedDecomposedParent(t, rs, int64Ptr(55), run.ExecutorHuman)
	parent.IssueContext = &run.IssueContext{Title: "Add widget", Number: 714}

	out, err := o.Advance(context.Background(), parent.ID)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if out != OutcomeDispatched {
		t.Errorf("Outcome = %q, want dispatched", out)
	}

	// Exactly one consolidated PR opened, with the right head/base.
	if len(gh.createPRCalls) != 1 {
		t.Fatalf("CreatePullRequest calls = %d, want 1", len(gh.createPRCalls))
	}
	call := gh.createPRCalls[0]
	wantHead := consolidatedBranch(parent.ID)
	if call.Head != wantHead {
		t.Errorf("head = %q, want %q", call.Head, wantHead)
	}
	if call.Base != "main" {
		t.Errorf("base = %q, want main (DefaultRef, not TriggerRef)", call.Base)
	}
	if call.InstallationID != 55 {
		t.Errorf("installation_id = %d, want 55", call.InstallationID)
	}
	if call.Title != "Add widget" {
		t.Errorf("title = %q, want issue title", call.Title)
	}

	// pull_request_url stamped on the parent.
	if rs.runs[parent.ID].PullRequestURL == nil || *rs.runs[parent.ID].PullRequestURL == "" {
		t.Error("parent run pull_request_url not stamped")
	}
	// Review dispatched (human → awaiting_approval), NOT auto-succeeded.
	if stages[1].State != run.StageStateAwaitingApproval {
		t.Errorf("review stage = %q, want awaiting_approval", stages[1].State)
	}
	if !auditHasCategory(au, "consolidated_pr_opened") {
		t.Errorf("audit categories = %v, want consolidated_pr_opened", au.appended)
	}
}

func TestAdvance_DecomposedParent_Idempotent(t *testing.T) {
	// The double-open race (sweeper + event-driven) must net exactly one
	// CreatePullRequest. After the URL is stamped, a second Advance over
	// the same review gate opens no second PR.
	o, rs, gh := newOrchestrator(t)
	o.DefaultRef = "main"

	parent, stages := seedDecomposedParent(t, rs, int64Ptr(55), run.ExecutorHuman)

	if _, err := o.Advance(context.Background(), parent.ID); err != nil {
		t.Fatalf("Advance #1: %v", err)
	}
	if len(gh.createPRCalls) != 1 {
		t.Fatalf("after Advance #1: CreatePullRequest calls = %d, want 1", len(gh.createPRCalls))
	}

	// Simulate a redelivered settle hitting the still-pending review
	// gate again (the URL is now set on the run row).
	stages[1].State = run.StageStatePending
	if _, err := o.Advance(context.Background(), parent.ID); err != nil {
		t.Fatalf("Advance #2: %v", err)
	}
	if len(gh.createPRCalls) != 1 {
		t.Errorf("after Advance #2: CreatePullRequest calls = %d, want still 1 (idempotent)", len(gh.createPRCalls))
	}
}

func TestAdvance_DecomposedParent_LostRace_RecoversURL(t *testing.T) {
	// A lost double-open race surfaces as ErrPullRequestExists; the
	// orchestrator recovers the already-open PR's URL via
	// ListOpenPullRequestsByHead rather than failing the settle.
	o, rs, gh := newOrchestrator(t)
	o.DefaultRef = "main"
	gh.createPRErr = githubclient.ErrPullRequestExists
	gh.listByHeadResult = []githubclient.PullRequest{
		{Number: 42, HTMLURL: "https://github.com/kuhlman-labs/fishhawk/pull/42", State: "open"},
	}

	parent, _ := seedDecomposedParent(t, rs, int64Ptr(55), run.ExecutorHuman)

	if _, err := o.Advance(context.Background(), parent.ID); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if gh.listByHeadCalls != 1 {
		t.Errorf("ListOpenPullRequestsByHead calls = %d, want 1", gh.listByHeadCalls)
	}
	got := rs.runs[parent.ID].PullRequestURL
	if got == nil || *got != "https://github.com/kuhlman-labs/fishhawk/pull/42" {
		t.Errorf("pull_request_url = %v, want recovered URL", got)
	}
}

func TestAdvance_DecomposedParent_LostRace_EmptyList_RetryableError(t *testing.T) {
	// A lost double-open race surfaces as ErrPullRequestExists, but
	// GitHub's read-after-write consistency can lag so the recovery
	// ListOpenPullRequestsByHead returns nothing yet. The settle must
	// fail with a (retryable) error rather than stamp an empty/nil URL —
	// the next Advance re-enters and recovers once the list catches up.
	o, rs, gh := newOrchestrator(t)
	o.DefaultRef = "main"
	gh.createPRErr = githubclient.ErrPullRequestExists
	gh.listByHeadResult = nil // consistency gap: 422 says exists, list empty

	parent, _ := seedDecomposedParent(t, rs, int64Ptr(55), run.ExecutorHuman)

	_, err := o.Advance(context.Background(), parent.ID)
	if err == nil {
		t.Fatal("Advance: want a retryable error when ErrPullRequestExists but the list returns empty, got nil")
	}
	if gh.listByHeadCalls != 1 {
		t.Errorf("ListOpenPullRequestsByHead calls = %d, want 1", gh.listByHeadCalls)
	}
	if got := rs.runs[parent.ID].PullRequestURL; got != nil {
		t.Errorf("pull_request_url = %v, want nil (no URL stamped on the failed recovery)", *got)
	}
}

func TestAdvance_NonDecomposedParent_NoConsolidatedPR(t *testing.T) {
	// A plain run (no decomposed children) reaching its review gate must
	// NOT open a PR — only decomposed parents do.
	o, rs, gh := newOrchestrator(t)
	o.DefaultRef = "main"

	r, stages := rs.seed(t, "kuhlman-labs/fishhawk", int64Ptr(55), []stageSeed{
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, State: run.StageStateSucceeded},
		{Type: run.StageTypeReview, ExecutorKind: run.ExecutorHuman, ExecutorRef: "human", State: run.StageStatePending},
	})

	if _, err := o.Advance(context.Background(), r.ID); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if len(gh.createPRCalls) != 0 {
		t.Errorf("CreatePullRequest calls = %d, want 0 for non-decomposed run", len(gh.createPRCalls))
	}
	if stages[1].State != run.StageStateAwaitingApproval {
		t.Errorf("review stage = %q, want awaiting_approval", stages[1].State)
	}
}

func TestAdvance_DecomposedParent_NoGitHub_GracefulSkip(t *testing.T) {
	// Without a GitHub client (CLI/dev posture) the orchestrator can't
	// open the PR — it WARN-logs, opens no PR, and still dispatches the
	// review (the parent stays PR-less, same posture as fireDispatch).
	rs := newStubRuns()
	o := &Orchestrator{Runs: rs, DefaultRef: "main"} // GitHub nil

	parent, stages := seedDecomposedParent(t, rs, int64Ptr(55), run.ExecutorHuman)

	out, err := o.Advance(context.Background(), parent.ID)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if out != OutcomeDispatched {
		t.Errorf("Outcome = %q, want dispatched", out)
	}
	if rs.runs[parent.ID].PullRequestURL != nil {
		t.Errorf("pull_request_url = %v, want nil (no GitHub)", rs.runs[parent.ID].PullRequestURL)
	}
	if stages[1].State != run.StageStateAwaitingApproval {
		t.Errorf("review stage = %q, want awaiting_approval", stages[1].State)
	}
}

// TestReconcileStuckRuns_AdvancesAllTerminalRunOnly is the unit guard
// for the startup recovery (#727): a run whose every stage is terminal
// but is still {run running} gets completed, while a genuinely in-flight
// run (a non-terminal stage) is left untouched.
func TestReconcileStuckRuns_AdvancesAllTerminalRunOnly(t *testing.T) {
	rr := newStubRuns()
	o := &Orchestrator{Runs: rr}

	// Stuck run: every stage terminal (succeeded), run still running.
	stuck, _ := rr.seed(t, "owner/stuck", nil, []stageSeed{
		{Type: run.StageTypePlan, ExecutorKind: run.ExecutorAgent, State: run.StageStateSucceeded},
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, State: run.StageStateSucceeded},
		{Type: run.StageTypeReview, ExecutorKind: run.ExecutorHuman, State: run.StageStateSucceeded},
	})
	// Control run: a non-terminal stage (awaiting_approval) → in-flight.
	inflight, _ := rr.seed(t, "owner/inflight", nil, []stageSeed{
		{Type: run.StageTypePlan, ExecutorKind: run.ExecutorAgent, State: run.StageStateSucceeded},
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, State: run.StageStateSucceeded},
		{Type: run.StageTypeReview, ExecutorKind: run.ExecutorHuman, State: run.StageStateAwaitingApproval},
	})

	n, err := o.ReconcileStuckRuns(context.Background())
	if err != nil {
		t.Fatalf("ReconcileStuckRuns: %v", err)
	}
	if n != 1 {
		t.Errorf("advanced = %d, want 1", n)
	}
	if got := rr.runs[stuck.ID].State; got != run.StateSucceeded {
		t.Errorf("stuck run state = %q, want succeeded", got)
	}
	if got := rr.runs[inflight.ID].State; got != run.StateRunning {
		t.Errorf("in-flight run state = %q, want running (untouched)", got)
	}
}

// TestReconcileStuckRuns_PerRunErrorDoesNotBlockOthers guards the
// best-effort-per-run posture (#727 review concern): a run that errors
// during the sweep (here, a stage-scan failure — modelling a partially
// cleaned-up record) is logged and skipped, and a sibling stuck run is
// still rescued in the SAME pass. The pre-fix code aborted the whole
// sweep on the first per-run error, so every subsequent run went
// unrecovered until the next restart.
func TestReconcileStuckRuns_PerRunErrorDoesNotBlockOthers(t *testing.T) {
	rr := newStubRuns()
	o := &Orchestrator{Runs: rr}

	// A stuck run whose stage scan fails (record partially cleaned up).
	broken, _ := rr.seed(t, "owner/broken", nil, []stageSeed{
		{Type: run.StageTypePlan, ExecutorKind: run.ExecutorAgent, State: run.StageStateSucceeded},
		{Type: run.StageTypeReview, ExecutorKind: run.ExecutorHuman, State: run.StageStateSucceeded},
	})
	rr.listStagesErrIDs = map[uuid.UUID]error{broken.ID: errors.New("record gone")}

	// A healthy stuck run that must still be rescued despite the broken one.
	good, _ := rr.seed(t, "owner/good", nil, []stageSeed{
		{Type: run.StageTypePlan, ExecutorKind: run.ExecutorAgent, State: run.StageStateSucceeded},
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, State: run.StageStateSucceeded},
		{Type: run.StageTypeReview, ExecutorKind: run.ExecutorHuman, State: run.StageStateSucceeded},
	})

	n, err := o.ReconcileStuckRuns(context.Background())
	if err != nil {
		t.Fatalf("ReconcileStuckRuns returned error, want nil (per-run errors are skipped): %v", err)
	}
	if n != 1 {
		t.Errorf("advanced = %d, want 1 (only the healthy run)", n)
	}
	if got := rr.runs[good.ID].State; got != run.StateSucceeded {
		t.Errorf("good run state = %q, want succeeded (sibling error must not block it)", got)
	}
	if got := rr.runs[broken.ID].State; got != run.StateRunning {
		t.Errorf("broken run state = %q, want running (left for next boot)", got)
	}
}

// recordingConsolidatedReview records DispatchConsolidatedReview calls for
// the #1060 trigger-condition assertions.
type recordingConsolidatedReview struct {
	calls []struct {
		runID      uuid.UUID
		base, head string
	}
}

func (r *recordingConsolidatedReview) DispatchConsolidatedReview(_ context.Context, runID uuid.UUID, base, head string) {
	r.calls = append(r.calls, struct {
		runID      uuid.UUID
		base, head string
	}{runID, base, head})
}

func TestAdvance_DecomposedParent_DispatchesConsolidatedReview(t *testing.T) {
	// Once the decomposed parent's review stage dispatches WITH the
	// consolidated PR present, the orchestrator fires the consolidated
	// implement review against base...consolidatedBranch (#1060).
	o, rs, _ := newOrchestrator(t)
	o.DefaultRef = "main"
	rec := &recordingConsolidatedReview{}
	o.ConsolidatedReview = rec

	parent, _ := seedDecomposedParent(t, rs, int64Ptr(55), run.ExecutorHuman)

	if _, err := o.Advance(context.Background(), parent.ID); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("DispatchConsolidatedReview calls = %d, want 1", len(rec.calls))
	}
	call := rec.calls[0]
	if call.runID != parent.ID {
		t.Errorf("run id = %s, want parent %s", call.runID, parent.ID)
	}
	if call.base != "main" {
		t.Errorf("base = %q, want main (DefaultRef)", call.base)
	}
	if want := consolidatedBranch(parent.ID); call.head != want {
		t.Errorf("head = %q, want %q (consolidated branch)", call.head, want)
	}
}

func TestAdvance_NoConsolidatedReview_WhenNoPR(t *testing.T) {
	// A run reaching its review gate with no consolidated PR opened (e.g. a
	// non-decomposed run that graceful-skips maybeOpenConsolidatedPR, or
	// the CLI/dev posture) must NOT fire the consolidated review — there is
	// no PR diff to review.
	o, rs, _ := newOrchestrator(t)
	o.DefaultRef = "main"
	rec := &recordingConsolidatedReview{}
	o.ConsolidatedReview = rec

	r, _ := rs.seed(t, "kuhlman-labs/fishhawk", int64Ptr(55), []stageSeed{
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, State: run.StageStateSucceeded},
		{Type: run.StageTypeReview, ExecutorKind: run.ExecutorHuman, ExecutorRef: "human", State: run.StageStatePending},
	})
	// No decomposed children → maybeOpenConsolidatedPR graceful-skips, PR
	// stays nil.

	if _, err := o.Advance(context.Background(), r.ID); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if len(rec.calls) != 0 {
		t.Errorf("DispatchConsolidatedReview calls = %d, want 0 (no PR present)", len(rec.calls))
	}
}

// sliceCapturingRuns wraps fanoutRunsRepo (defined in fanout_test.go) to record
// the SliceIndex passed to each CreateRun. It records the param directly so the
// orchestrator-mint half of the slice_index contract (E24.1 / #1141) can be
// asserted without changing the shared fanout fixture's stub.
type sliceCapturingRuns struct {
	*fanoutRunsRepo
	mintedSliceIndexes []*int
}

func (r *sliceCapturingRuns) CreateRun(ctx context.Context, p run.CreateRunParams) (*run.Run, error) {
	r.mintedSliceIndexes = append(r.mintedSliceIndexes, p.SliceIndex)
	return r.fanoutRunsRepo.CreateRun(ctx, p)
}

// TestAdvance_FanoutAssignsSliceIndexInOrder is the orchestrator-mint end of the
// slice_index contract (E24.1 / #1141): each child minted from an N-element
// decomposition.sub_plans is assigned SliceIndex 0..N-1 in sub_plan order, which
// the runner reads back to route the child onto fishhawk/run-<parent>/slice-<n>.
func TestAdvance_FanoutAssignsSliceIndexInOrder(t *testing.T) {
	base := newFanoutRunsRepo()
	rs := &sliceCapturingRuns{fanoutRunsRepo: base}
	parent, stages := base.seed(t, "example/repo", nil, []stageSeed{
		{Type: run.StageTypePlan, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStateSucceeded},
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStatePending},
		{Type: run.StageTypeReview, ExecutorKind: run.ExecutorHuman, ExecutorRef: "human", State: run.StageStatePending},
	})
	planStage := stages[0]

	planBytes := decomposedPlanBytes(t, []string{"Part A", "Part B", "Part C"})
	schemaV := "standard_v1"
	arts := &fakeArtifacts{
		byStage: map[uuid.UUID][]*artifact.Artifact{
			planStage.ID: {{
				ID:            uuid.New(),
				StageID:       planStage.ID,
				Kind:          artifact.KindPlan,
				SchemaVersion: &schemaV,
				Content:       planBytes,
				CreatedAt:     time.Now().UTC(),
			}},
		},
	}

	o := &Orchestrator{Runs: rs, Logger: slog.Default(), Artifacts: arts, Audit: &recordingAudit{}}
	out, err := o.Advance(context.Background(), parent.ID)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if out != OutcomeDecomposed {
		t.Fatalf("Advance outcome = %q, want %q", out, OutcomeDecomposed)
	}

	want := []int{0, 1, 2}
	if len(rs.mintedSliceIndexes) != len(want) {
		t.Fatalf("minted %d children, want %d (one per sub_plan)", len(rs.mintedSliceIndexes), len(want))
	}
	for i, got := range rs.mintedSliceIndexes {
		if got == nil {
			t.Errorf("child %d SliceIndex = nil, want %d", i, want[i])
		} else if *got != want[i] {
			t.Errorf("child %d SliceIndex = %d, want %d", i, *got, want[i])
		}
	}
}

// seedFanInParent seeds a decomposed parent parked in awaiting_children
// (implement awaiting_children, review pending). The caller adds children.
func seedFanInParent(t *testing.T, rs *stubRuns, installationID *int64) (*run.Run, []*run.Stage) {
	t.Helper()
	return rs.seed(t, "kuhlman-labs/fishhawk", installationID, []stageSeed{
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, State: run.StageStateAwaitingChildren},
		{Type: run.StageTypeReview, ExecutorKind: run.ExecutorHuman, ExecutorRef: "human", State: run.StageStatePending},
	})
}

// seedSucceededSlice seeds one succeeded decomposed child with the given
// slice index.
func seedSucceededSlice(t *testing.T, rs *stubRuns, parentID uuid.UUID, installationID *int64, sliceIdx int) *run.Run {
	t.Helper()
	child, _ := rs.seed(t, "kuhlman-labs/fishhawk", installationID, nil)
	child.DecomposedFrom = &parentID
	child.State = run.StateSucceeded
	idx := sliceIdx
	child.SliceIndex = &idx
	return child
}

// auditPayload returns the decoded payload of the most recent audit entry
// of the given category, failing the test when none was recorded.
func auditPayload(t *testing.T, a *recordingAudit, category string) map[string]any {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := len(a.appended) - 1; i >= 0; i-- {
		if a.appended[i].Category != category {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(a.appended[i].Payload, &m); err != nil {
			t.Fatalf("decode %s payload: %v", category, err)
		}
		return m
	}
	t.Fatalf("no audit entry of category %q (have %v)", category, a.appended)
	return nil
}

func TestIntegrateSlices_Success_MergesInSliceOrder(t *testing.T) {
	o, rs, gh := newOrchestrator(t)
	o.DefaultRef = "main"
	au := &recordingAudit{}
	o.Audit = au
	gh.branchSHAs = map[string]string{"main": "basesha"} // consolidated branch absent

	parent, stages := seedFanInParent(t, rs, int64Ptr(55))
	parent.IssueContext = &run.IssueContext{Title: "Add widget", Number: 1142}
	// Seed slice 1 BEFORE slice 0 to prove the ascending-index sort.
	child1 := seedSucceededSlice(t, rs, parent.ID, int64Ptr(55), 1)
	child0 := seedSucceededSlice(t, rs, parent.ID, int64Ptr(55), 0)
	_ = child0

	o.maybeAdvanceDecomposedParent(context.Background(), parent.ID)

	// The consolidated branch was absent, so it must be created from the
	// base sha (BRANCH-CREATE path).
	if len(gh.createRefCalls) != 1 {
		t.Fatalf("CreateRef calls = %d, want 1", len(gh.createRefCalls))
	}
	consolidated := consolidatedBranch(parent.ID)
	if gh.createRefCalls[0].Branch != consolidated || gh.createRefCalls[0].SHA != "basesha" {
		t.Errorf("CreateRef = %+v, want branch=%q sha=basesha", gh.createRefCalls[0], consolidated)
	}

	// Merges happen in ascending slice-index order: slice-0 then slice-1.
	if len(gh.mergeCalls) != 2 {
		t.Fatalf("MergeBranch calls = %d, want 2", len(gh.mergeCalls))
	}
	wantHead0 := sliceBranch(parent.ID, 0)
	wantHead1 := sliceBranch(parent.ID, 1)
	if gh.mergeCalls[0].Head != wantHead0 || gh.mergeCalls[1].Head != wantHead1 {
		t.Errorf("merge order heads = [%q, %q], want [%q, %q]",
			gh.mergeCalls[0].Head, gh.mergeCalls[1].Head, wantHead0, wantHead1)
	}
	if gh.mergeCalls[0].Base != consolidated {
		t.Errorf("merge base = %q, want %q", gh.mergeCalls[0].Base, consolidated)
	}

	// The awaiting_children stage resolved succeeded and the review
	// dispatched (human → awaiting_approval), opening the consolidated PR.
	if stages[0].State != run.StageStateSucceeded {
		t.Errorf("implement stage = %q, want succeeded", stages[0].State)
	}
	if stages[1].State != run.StageStateAwaitingApproval {
		t.Errorf("review stage = %q, want awaiting_approval", stages[1].State)
	}
	if len(gh.createPRCalls) != 1 {
		t.Errorf("CreatePullRequest calls = %d, want 1 (consolidated PR off the integrated branch)", len(gh.createPRCalls))
	}

	// slices_integrated emitted with both children and the slice count.
	p := auditPayload(t, au, "slices_integrated")
	if got, _ := p["slice_count"].(float64); int(got) != 2 {
		t.Errorf("slices_integrated slice_count = %v, want 2", p["slice_count"])
	}
	if p["consolidated_branch"] != consolidated {
		t.Errorf("slices_integrated consolidated_branch = %v, want %q", p["consolidated_branch"], consolidated)
	}
	_ = child1
}

// TestIntegrateSlices_CreatesConsolidatedRef_WithPrefixSharingSlices is the
// #1243 D/F-conflict CI guard: it exercises the exact create-consolidated-ref
// step that 422'd in production, with the REAL prefix-sharing slice branches
// already present in the ref store (fishhawk/run-<short>/slice-<n>). The
// consolidated branch is absent (cexists=false) so CreateRef is invoked; the
// assertion is that it is created under the NON-NESTING -consolidated name and
// never under a name that nests within (or contains) the slice path.
func TestIntegrateSlices_CreatesConsolidatedRef_WithPrefixSharingSlices(t *testing.T) {
	o, rs, gh := newOrchestrator(t)
	o.DefaultRef = "main"
	au := &recordingAudit{}
	o.Audit = au

	parent, _ := seedFanInParent(t, rs, int64Ptr(55))
	child0 := seedSucceededSlice(t, rs, parent.ID, int64Ptr(55), 0)
	child1 := seedSucceededSlice(t, rs, parent.ID, int64Ptr(55), 1)
	_, _ = child0, child1

	// Seed the base ref AND the real prefix-sharing slice branches as
	// existing refs — but NOT the consolidated branch, so CreateRef fires.
	// This reproduces the production state where the slice refs already
	// occupy fishhawk/run-<short>/slice-<n> when create-consolidated-ref runs.
	slice0 := sliceBranch(parent.ID, 0)
	slice1 := sliceBranch(parent.ID, 1)
	gh.branchSHAs = map[string]string{
		"main": "basesha",
		slice0: "slice0sha",
		slice1: "slice1sha",
	}

	conflict, err := o.IntegrateSlices(context.Background(), parent.ID)
	if err != nil {
		t.Fatalf("IntegrateSlices: %v", err)
	}
	if conflict != nil {
		t.Fatalf("conflict = %+v, want nil", conflict)
	}

	// CreateRef fired exactly once, under the -consolidated name.
	if len(gh.createRefCalls) != 1 {
		t.Fatalf("CreateRef calls = %d, want 1", len(gh.createRefCalls))
	}
	consolidated := consolidatedBranch(parent.ID)
	created := gh.createRefCalls[0].Branch
	if created != consolidated {
		t.Errorf("CreateRef branch = %q, want %q", created, consolidated)
	}
	// The created consolidated ref must NOT nest under, equal, or be a
	// parent directory of any slice ref — the D/F conflict that 422'd.
	for _, slice := range []string{slice0, slice1} {
		if created == slice {
			t.Errorf("consolidated ref %q equals slice ref %q (D/F collision)", created, slice)
		}
		if strings.HasPrefix(created, slice+"/") {
			t.Errorf("consolidated ref %q nests under slice ref %q (D/F conflict)", created, slice)
		}
		if strings.HasPrefix(slice, created+"/") {
			t.Errorf("slice ref %q nests under consolidated ref %q (D/F conflict)", slice, created)
		}
	}

	// Each slice merges onto the -consolidated branch (never the reverse).
	if len(gh.mergeCalls) != 2 {
		t.Fatalf("MergeBranch calls = %d, want 2", len(gh.mergeCalls))
	}
	for i, m := range gh.mergeCalls {
		if m.Base != consolidated {
			t.Errorf("merge[%d] base = %q, want %q", i, m.Base, consolidated)
		}
		if m.Head != sliceBranch(parent.ID, i) {
			t.Errorf("merge[%d] head = %q, want %q", i, m.Head, sliceBranch(parent.ID, i))
		}
	}
}

func TestIntegrateSlices_Conflict_FailsParentRecoverable(t *testing.T) {
	o, rs, gh := newOrchestrator(t)
	o.DefaultRef = "main"
	au := &recordingAudit{}
	o.Audit = au
	gh.branchSHAs = map[string]string{"main": "basesha"}

	parent, stages := seedFanInParent(t, rs, int64Ptr(55))
	_ = seedSucceededSlice(t, rs, parent.ID, int64Ptr(55), 0)
	child1 := seedSucceededSlice(t, rs, parent.ID, int64Ptr(55), 1)

	// Slice-1's branch fails to merge (a conflict).
	gh.mergeErrByHead = map[string]error{
		sliceBranch(parent.ID, 1): githubclient.ErrMergeConflict,
	}

	o.maybeAdvanceDecomposedParent(context.Background(), parent.ID)

	// The awaiting_children stage failed category-B with the stable reason.
	if stages[0].State != run.StageStateFailed {
		t.Fatalf("implement stage = %q, want failed", stages[0].State)
	}
	if stages[0].FailureCategory == nil || *stages[0].FailureCategory != run.FailureB {
		t.Errorf("failure category = %v, want B", stages[0].FailureCategory)
	}
	if stages[0].FailureReason == nil || (*stages[0].FailureReason)[:len(sliceIntegrationConflictReasonPrefix)] != sliceIntegrationConflictReasonPrefix {
		t.Errorf("failure reason = %v, want %q prefix", stages[0].FailureReason, sliceIntegrationConflictReasonPrefix)
	}

	// No consolidated PR — review never dispatched.
	if len(gh.createPRCalls) != 0 {
		t.Errorf("CreatePullRequest calls = %d, want 0 on conflict", len(gh.createPRCalls))
	}

	// slice_integration_conflict carries the STRUCTURED provenance: the
	// conflicting slice index AND child run id (sourced from data, not the
	// reason string).
	p := auditPayload(t, au, "slice_integration_conflict")
	if got, _ := p["conflicting_slice_index"].(float64); int(got) != 1 {
		t.Errorf("conflicting_slice_index = %v, want 1", p["conflicting_slice_index"])
	}
	if p["conflicting_child_run_id"] != child1.ID.String() {
		t.Errorf("conflicting_child_run_id = %v, want %q", p["conflicting_child_run_id"], child1.ID.String())
	}
}

func TestIntegrateSlices_Idempotent_ExistingBranchAndMergedSlices(t *testing.T) {
	o, rs, gh := newOrchestrator(t)
	o.DefaultRef = "main"
	au := &recordingAudit{}
	o.Audit = au

	parent, _ := seedFanInParent(t, rs, int64Ptr(55))
	_ = seedSucceededSlice(t, rs, parent.ID, int64Ptr(55), 0)
	// Both base AND the consolidated branch already exist (a prior pass
	// created it); merges return nil (204 "already merged" / clean re-merge).
	consolidated := consolidatedBranch(parent.ID)
	gh.branchSHAs = map[string]string{"main": "basesha", consolidated: "consha"}

	conflict, err := o.IntegrateSlices(context.Background(), parent.ID)
	if err != nil {
		t.Fatalf("IntegrateSlices: %v", err)
	}
	if conflict != nil {
		t.Fatalf("conflict = %+v, want nil on a clean re-run", conflict)
	}
	// The consolidated branch already existed → CreateRef must NOT fire.
	if len(gh.createRefCalls) != 0 {
		t.Errorf("CreateRef calls = %d, want 0 (branch already exists)", len(gh.createRefCalls))
	}
	if !auditHasCategory(au, "slices_integrated") {
		t.Errorf("want slices_integrated audit on clean re-run")
	}
}

func TestIntegrateSlices_PaginatesToCompletion(t *testing.T) {
	// #1142 partial-integration safety: the children listing must paginate
	// to completion, never silently integrate only the first page. Shrink
	// the page size and seed children spanning more than one page; ALL
	// slices must integrate in order.
	saved := integrateSlicesPageSize
	integrateSlicesPageSize = 2
	t.Cleanup(func() { integrateSlicesPageSize = saved })

	o, rs, gh := newOrchestrator(t)
	o.DefaultRef = "main"
	au := &recordingAudit{}
	o.Audit = au
	gh.branchSHAs = map[string]string{"main": "basesha"}

	parent, _ := seedFanInParent(t, rs, int64Ptr(55))
	for i := 0; i < 3; i++ { // 3 children across 2 pages of size 2
		_ = seedSucceededSlice(t, rs, parent.ID, int64Ptr(55), i)
	}

	conflict, err := o.IntegrateSlices(context.Background(), parent.ID)
	if err != nil {
		t.Fatalf("IntegrateSlices: %v", err)
	}
	if conflict != nil {
		t.Fatalf("conflict = %+v, want nil", conflict)
	}
	if len(gh.mergeCalls) != 3 {
		t.Fatalf("MergeBranch calls = %d, want 3 (all pages integrated)", len(gh.mergeCalls))
	}
	for i := 0; i < 3; i++ {
		want := sliceBranch(parent.ID, i)
		if gh.mergeCalls[i].Head != want {
			t.Errorf("merge[%d] head = %q, want %q", i, gh.mergeCalls[i].Head, want)
		}
	}
	p := auditPayload(t, au, "slices_integrated")
	if got, _ := p["slice_count"].(float64); int(got) != 3 {
		t.Errorf("slice_count = %v, want 3", p["slice_count"])
	}
}

func TestIntegrateSlices_SkipsChildMissingSliceIndex(t *testing.T) {
	o, rs, gh := newOrchestrator(t)
	o.DefaultRef = "main"
	gh.branchSHAs = map[string]string{"main": "basesha"}

	parent, _ := seedFanInParent(t, rs, int64Ptr(55))
	_ = seedSucceededSlice(t, rs, parent.ID, int64Ptr(55), 0)
	// A succeeded child with NO slice index has no derivable branch — it is
	// a defensive skip, not a guessed merge.
	orphan, _ := rs.seed(t, "kuhlman-labs/fishhawk", int64Ptr(55), nil)
	orphan.DecomposedFrom = &parent.ID
	orphan.State = run.StateSucceeded

	if _, err := o.IntegrateSlices(context.Background(), parent.ID); err != nil {
		t.Fatalf("IntegrateSlices: %v", err)
	}
	if len(gh.mergeCalls) != 1 {
		t.Errorf("MergeBranch calls = %d, want 1 (orphan child skipped)", len(gh.mergeCalls))
	}
}

func TestIntegrateSlices_GracefulSkip(t *testing.T) {
	t.Run("github nil", func(t *testing.T) {
		rs := newStubRuns()
		o := &Orchestrator{Runs: rs} // no GitHub
		parent, _ := seedFanInParent(t, rs, int64Ptr(55))
		_ = seedSucceededSlice(t, rs, parent.ID, int64Ptr(55), 0)
		conflict, err := o.IntegrateSlices(context.Background(), parent.ID)
		if err != nil || conflict != nil {
			t.Errorf("got (%v, %v), want (nil, nil) when GitHub unwired", conflict, err)
		}
	})
	t.Run("nil installation id", func(t *testing.T) {
		o, rs, gh := newOrchestrator(t)
		parent, _ := seedFanInParent(t, rs, nil) // nil installation
		_ = seedSucceededSlice(t, rs, parent.ID, nil, 0)
		conflict, err := o.IntegrateSlices(context.Background(), parent.ID)
		if err != nil || conflict != nil {
			t.Errorf("got (%v, %v), want (nil, nil) when installation_id nil", conflict, err)
		}
		if len(gh.mergeCalls) != 0 || len(gh.createRefCalls) != 0 {
			t.Errorf("no GitHub writes expected on skip; merges=%d createRefs=%d", len(gh.mergeCalls), len(gh.createRefCalls))
		}
	})
	t.Run("zero children", func(t *testing.T) {
		o, rs, gh := newOrchestrator(t)
		parent, _ := seedFanInParent(t, rs, int64Ptr(55)) // no children seeded
		conflict, err := o.IntegrateSlices(context.Background(), parent.ID)
		if err != nil || conflict != nil {
			t.Errorf("got (%v, %v), want (nil, nil) with zero children", conflict, err)
		}
		if len(gh.mergeCalls) != 0 {
			t.Errorf("merges = %d, want 0 with zero children", len(gh.mergeCalls))
		}
	})
}

func TestIntegrateSlices_BaseRefMissing_Errors(t *testing.T) {
	// A non-conflict error (the base ref does not resolve) must NOT mark
	// the parent succeeded — it surfaces so the next settle retries.
	o, rs, gh := newOrchestrator(t)
	o.DefaultRef = "main"
	gh.branchSHAs = map[string]string{} // base "main" absent

	parent, _ := seedFanInParent(t, rs, int64Ptr(55))
	_ = seedSucceededSlice(t, rs, parent.ID, int64Ptr(55), 0)

	conflict, err := o.IntegrateSlices(context.Background(), parent.ID)
	if err == nil {
		t.Fatal("want an error when the base ref is absent")
	}
	if conflict != nil {
		t.Errorf("conflict = %+v, want nil (absent base is an error, not a conflict)", conflict)
	}
}

// TestAdvance_Fanout_ResolvesEffectiveMaxParallel drives the concurrency
// cap end-to-end (E24.6 / #1146): the decomposition.max_parallel knob is set
// in raw workflow YAML BYTES on run.WorkflowSpec, parsed by the REAL
// spec.ParseBytes inside fanoutIfDecomposed, and the resolved effective cap
// is asserted via the plan_decomposed audit payload — NOT a hand-built
// cached spec. It covers the precedence (per-workflow knob wins over the
// global default) and every degradation branch of resolveEffectiveMaxParallel
// (absent spec, unparseable spec, workflow-not-in-spec) all falling through
// to the global default.
func TestAdvance_Fanout_ResolvesEffectiveMaxParallel(t *testing.T) {
	const specWithKnob = `version: "0.6"
workflows:
  feature_change:
    decomposition:
      max_parallel: 2
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`
	const specNoKnob = `version: "0.6"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`

	tests := []struct {
		name          string
		workflowSpec  string
		workflowID    string // override seed default "feature_change" when non-empty
		globalDefault int
		want          float64
	}{
		{
			// success branch: per-workflow knob (2) wins over the global (9).
			name:          "per-workflow knob wins over global",
			workflowSpec:  specWithKnob,
			globalDefault: 9,
			want:          2,
		},
		{
			// success branch, EffectiveMaxParallel fallthrough: no knob => global.
			name:          "no knob falls through to global default",
			workflowSpec:  specNoKnob,
			globalDefault: 5,
			want:          5,
		},
		{
			// branch 1: absent cached spec degrades to the global default.
			name:          "absent spec degrades to global default",
			workflowSpec:  "",
			globalDefault: 6,
			want:          6,
		},
		{
			// branch 2: unparseable spec degrades to the global default (WARN).
			name:          "unparseable spec degrades to global default",
			workflowSpec:  "this is not: valid: workflow: yaml: : :",
			globalDefault: 4,
			want:          4,
		},
		{
			// branch 3: workflow id not in the parsed spec degrades to global.
			name:          "workflow not in spec degrades to global default",
			workflowSpec:  specWithKnob,
			workflowID:    "not_feature_change",
			globalDefault: 3,
			want:          3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rs := newFanoutRunsRepo()
			parent, stages := rs.seed(t, "example/repo", nil, []stageSeed{
				{Type: run.StageTypePlan, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStateSucceeded},
				{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStatePending},
				{Type: run.StageTypeReview, ExecutorKind: run.ExecutorHuman, ExecutorRef: "human", State: run.StageStatePending},
			})
			parent.WorkflowSpec = []byte(tt.workflowSpec)
			if tt.workflowID != "" {
				parent.WorkflowID = tt.workflowID
			}
			planStage := stages[0]

			schemaV := "standard_v1"
			arts := &fakeArtifacts{
				byStage: map[uuid.UUID][]*artifact.Artifact{
					planStage.ID: {{
						ID:            uuid.New(),
						StageID:       planStage.ID,
						Kind:          artifact.KindPlan,
						SchemaVersion: &schemaV,
						Content:       decomposedPlanBytes(t, []string{"Part A", "Part B", "Part C"}),
						CreatedAt:     time.Now().UTC(),
					}},
				},
			}
			au := &recordingAudit{}
			o := &Orchestrator{
				Runs:                rs,
				Logger:              slog.Default(),
				Artifacts:           arts,
				Audit:               au,
				MaxParallelChildren: tt.globalDefault,
			}

			out, err := o.Advance(context.Background(), parent.ID)
			if err != nil {
				t.Fatalf("Advance: %v", err)
			}
			if out != OutcomeDecomposed {
				t.Fatalf("Advance outcome = %q, want %q", out, OutcomeDecomposed)
			}

			// All children are still minted — #1146 surfaces the cap, it does
			// not throttle the fan-out (that is E24.3 / #1143).
			if got := len(rs.createdRuns); got != 3 {
				t.Errorf("minted children = %d, want 3 (cap is surfaced, not enforced)", got)
			}

			payload := auditPayload(t, au, "plan_decomposed")
			got, ok := payload["effective_max_parallel"].(float64)
			if !ok {
				t.Fatalf("plan_decomposed payload missing effective_max_parallel: %v", payload)
			}
			if got != tt.want {
				t.Errorf("effective_max_parallel = %v, want %v", got, tt.want)
			}
		})
	}
}
