package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// forceStageState directly sets a seeded stage's state under the stub's lock,
// bypassing transition validation — the concurrency tests use it to simulate the
// host-dispatch marker CASing a stage to 'dispatched' or a walk settling it to
// 'succeeded' without threading a full transition sequence.
func forceStageState(rs *stubRuns, id uuid.UUID, st run.StageState) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	for _, list := range rs.stages {
		for _, s := range list {
			if s.ID == id {
				s.State = st
				return
			}
		}
	}
}

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
	// mergeEmptyByHead programs a per-head-branch 204 (already-merged) no-op:
	// MergeBranch returns ("", nil), modeling a re-entrant pass whose slices
	// were already merged on a prior pass (#1806).
	mergeEmptyByHead map[string]bool
	mergeCalls       []mergeBranchCall
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
	Scope forge.CredentialScope
	Repo  githubclient.RepoRef
	Head  string
	Base  string
	Title string
	Body  string
}

type dispatchCall struct {
	Scope        forge.CredentialScope
	Repo         githubclient.RepoRef
	WorkflowFile string
	Ref          string
	Inputs       githubclient.DispatchInputs
}

type autoMergeCall struct {
	Scope    forge.CredentialScope
	Repo     githubclient.RepoRef
	PRNumber int
	Method   githubclient.MergeMethod
}

func (g *stubGitHub) DispatchWorkflow(_ context.Context, scope forge.CredentialScope,
	repo githubclient.RepoRef, file, ref string, inputs githubclient.DispatchInputs) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.dispatchErr != nil {
		return g.dispatchErr
	}
	g.calls = append(g.calls, dispatchCall{
		Scope: scope, Repo: repo,
		WorkflowFile: file, Ref: ref, Inputs: inputs,
	})
	return nil
}

func (g *stubGitHub) EnableAutoMerge(_ context.Context, scope forge.CredentialScope,
	repo githubclient.RepoRef, prNumber int, method githubclient.MergeMethod) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.autoMergeErr != nil {
		return g.autoMergeErr
	}
	g.autoMergeCalls = append(g.autoMergeCalls, autoMergeCall{
		Scope: scope, Repo: repo,
		PRNumber: prNumber, Method: method,
	})
	return nil
}

func (g *stubGitHub) CreatePullRequest(_ context.Context, scope forge.CredentialScope,
	repo githubclient.RepoRef, head, base, title, body string) (*githubclient.PullRequest, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.createPRCalls = append(g.createPRCalls, createPRCall{
		Scope: scope, Repo: repo,
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

func (g *stubGitHub) ListOpenPullRequestsByHead(_ context.Context, _ forge.CredentialScope,
	_ githubclient.RepoRef, _, _ string) ([]githubclient.PullRequest, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.listByHeadCalls++
	if g.listByHeadErr != nil {
		return nil, g.listByHeadErr
	}
	return g.listByHeadResult, nil
}

func (g *stubGitHub) GetBranchSHA(_ context.Context, _ forge.CredentialScope,
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

func (g *stubGitHub) CreateRef(_ context.Context, _ forge.CredentialScope,
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

func (g *stubGitHub) MergeBranch(_ context.Context, _ forge.CredentialScope,
	_ githubclient.RepoRef, base, head, msg string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.mergeCalls = append(g.mergeCalls, mergeBranchCall{Base: base, Head: head, Msg: msg})
	if err, ok := g.mergeErrByHead[head]; ok {
		return "", err
	}
	// A 204 already-merged no-op returns an empty SHA (#1806 re-entrant pass).
	if g.mergeEmptyByHead[head] {
		return "", nil
	}
	// Deterministic per-head merge commit SHA so tests can assert the
	// slices_integrated integration_commit_shas payload (#1459).
	return "mergesha-" + head, nil
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
	if call.Scope != forge.FromGitHubInstallationID(42) {
		t.Errorf("installation_id scope = %v, want scope for 42", call.Scope)
	}
	if call.Repo.Owner != "x" || call.Repo.Name != "y" {
		t.Errorf("repo = %+v", call.Repo)
	}
	if call.Inputs["run_id"] != r.ID.String() {
		t.Errorf("inputs.run_id = %q", call.Inputs["run_id"])
	}
	// #1227: a non-decomposed run carries no parent_run_id, so the customer
	// workflow's concurrency group falls back to a per-run unique key and never
	// serializes.
	if got, ok := call.Inputs["parent_run_id"]; ok {
		t.Errorf("non-decomposed run set parent_run_id=%q, want it absent", got)
	}
	if call.Inputs["stage_id"] != stages[1].ID.String() {
		t.Errorf("inputs.stage_id = %q", call.Inputs["stage_id"])
	}
}

// TestAdvance_LocalLockedRun_ParksAwaitingHostDispatch asserts the #1912 park:
// a run already LOCKED to runner_kind=local parks its agent stage at
// awaiting_host_dispatch (the backend cannot spawn the host-local runner per
// ADR-024) instead of the pre-split 'dispatched', and must NOT fire a
// github_actions workflow_dispatch. The stage still advances (OutcomeDispatched);
// the spawn marker endpoint (or an MCP spawn verb) later flips it to dispatched.
func TestAdvance_LocalLockedRun_ParksAwaitingHostDispatch(t *testing.T) {
	o, rs, gh := newOrchestrator(t)
	r, stages := rs.seed(t, "x/y", int64Ptr(42), []stageSeed{
		{Type: run.StageTypePlan, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStateSucceeded},
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStatePending},
	})
	// Lock the run to the local channel.
	r.RunnerKind = run.RunnerKindLocal
	r.RunnerKindResolved = true

	out, err := o.Advance(context.Background(), r.ID)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if out != OutcomeDispatched {
		t.Errorf("Outcome = %q, want dispatched (the stage still advances)", out)
	}
	if stages[1].State != run.StageStateAwaitingHostDispatch {
		t.Errorf("stage[1].State = %q, want awaiting_host_dispatch (the #1912 local park)", stages[1].State)
	}
	// The local lock must suppress the github_actions workflow_dispatch.
	if len(gh.calls) != 0 {
		t.Errorf("workflow_dispatch fired for a local-locked run: %d", len(gh.calls))
	}
}

// TestAdvance_LocalUnresolvedRun_DispatchesNotParked is the negative control for
// the #1912 park: an UN-resolved run (RunnerKindResolved == false) is NOT parked
// even if its RunnerKind string happens to read local — the park engages only on
// the LOCKED state, matching the runnerbackend.Resolver's own locked-local
// posture. The stage takes the normal dispatched path (the github_actions
// backend's TriggerStage fires because the run has no lock).
func TestAdvance_LocalUnresolvedRun_DispatchesNotParked(t *testing.T) {
	o, rs, gh := newOrchestrator(t)
	r, stages := rs.seed(t, "x/y", int64Ptr(42), []stageSeed{
		{Type: run.StageTypePlan, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStateSucceeded},
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStatePending},
	})
	r.RunnerKind = run.RunnerKindLocal
	r.RunnerKindResolved = false

	if _, err := o.Advance(context.Background(), r.ID); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if stages[1].State != run.StageStateDispatched {
		t.Errorf("stage[1].State = %q, want dispatched (un-resolved run is not parked)", stages[1].State)
	}
	// Un-resolved run auto-resolves on first dispatch (#1346) → the dispatch fires.
	if len(gh.calls) != 1 {
		t.Errorf("workflow_dispatch calls = %d, want 1 for an un-resolved run", len(gh.calls))
	}
}

// TestAdvance_GitHubActionsLockedRun_StillFires is the negative control for the
// #1355 Actions-direction guard: a run LOCKED to runner_kind=github_actions
// fires the workflow_dispatch normally (the guard engages only on the local
// lock). The default-seeded un-resolved case is covered by
// TestAdvance_DispatchesNextAgentStage.
func TestAdvance_GitHubActionsLockedRun_StillFires(t *testing.T) {
	o, rs, gh := newOrchestrator(t)
	r, _ := rs.seed(t, "x/y", int64Ptr(42), []stageSeed{
		{Type: run.StageTypePlan, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStateSucceeded},
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStatePending},
	})
	r.RunnerKind = run.RunnerKindGitHubActions
	r.RunnerKindResolved = true

	if _, err := o.Advance(context.Background(), r.ID); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if len(gh.calls) != 1 {
		t.Errorf("workflow_dispatch calls = %d, want 1 for a github_actions-locked run", len(gh.calls))
	}
}

// seedDecomposedChild seeds a decomposition child run (DecomposedFrom set, one
// pending implement stage) with the given inherited runner-kind fields, for the
// per-branch resolver tests (#1980). The parent is NOT seeded here — each test
// controls whether the parent exists / is resolved to exercise the
// runnerbackend.Resolver's fail-toward-recoverable arms.
func seedDecomposedChild(t *testing.T, rs *stubRuns, installationID *int64, decomposedFrom uuid.UUID, kind string, resolved bool) (*run.Run, *run.Stage) {
	t.Helper()
	child, stages := rs.seed(t, "x/y", installationID, []stageSeed{
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStatePending},
	})
	df := decomposedFrom
	child.DecomposedFrom = &df
	child.RunnerKind = kind
	child.RunnerKindResolved = resolved
	return child, stages[0]
}

// TestAdvance_DecomposedChild_GitHubActionsParent_StillFires pins the resolver's
// inherited-non-local branch: an un-resolved decomposed child whose inherited
// kind is NOT local never enters the local-park arm — it walks pending→dispatched
// and fires its workflow_dispatch byte-identically (the unchanged E24.5 path).
// The resolver short-circuits on the kind, so the parent is never even read.
func TestAdvance_DecomposedChild_GitHubActionsParent_StillFires(t *testing.T) {
	o, rs, gh := newOrchestrator(t)
	child, stage := seedDecomposedChild(t, rs, int64Ptr(42), uuid.New(), run.RunnerKindGitHubActions, false)

	if _, err := o.Advance(context.Background(), child.ID); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if stage.State != run.StageStateDispatched {
		t.Errorf("child stage state = %q, want dispatched (github_actions child unchanged)", stage.State)
	}
	if len(gh.calls) != 1 {
		t.Errorf("workflow_dispatch calls = %d, want 1 for a github_actions child", len(gh.calls))
	}
}

// TestAdvance_DecomposedChild_ParentReadError_ParksRecoverable pins
// resolver branch (d) fail-toward-recoverable: an un-resolved local-kind
// child whose parent GetRun fails (the parent row is absent) parks at
// awaiting_host_dispatch and fires ZERO workflow_dispatch — a wrong park costs
// one host-dispatch verb, a wrong fire is an unrecoverable side effect.
func TestAdvance_DecomposedChild_ParentReadError_ParksRecoverable(t *testing.T) {
	o, rs, gh := newOrchestrator(t)
	// The parent id points at a run that was never seeded → GetRun returns
	// ErrNotFound. (rs.getRunErr is deliberately NOT used: it would also fail
	// the child's own GetRun in Advance.)
	child, stage := seedDecomposedChild(t, rs, int64Ptr(42), uuid.New(), run.RunnerKindLocal, false)

	if _, err := o.Advance(context.Background(), child.ID); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if stage.State != run.StageStateAwaitingHostDispatch {
		t.Errorf("child stage state = %q, want awaiting_host_dispatch (fail-toward-recoverable)", stage.State)
	}
	if len(gh.calls) != 0 {
		t.Errorf("workflow_dispatch fired for a parent-read-error local child: %d calls", len(gh.calls))
	}
}

// TestAdvance_DecomposedChild_ParentUnresolved_ParksRecoverable pins
// resolver branch (d) parent-unresolved: an un-resolved local-kind child
// whose parent is itself un-resolved parks toward the recoverable state (the
// inherited local hint is the best signal available).
func TestAdvance_DecomposedChild_ParentUnresolved_ParksRecoverable(t *testing.T) {
	o, rs, gh := newOrchestrator(t)
	parent, _ := rs.seed(t, "x/y", int64Ptr(42), nil)
	parent.RunnerKind = run.RunnerKindLocal
	parent.RunnerKindResolved = false
	child, stage := seedDecomposedChild(t, rs, int64Ptr(42), parent.ID, run.RunnerKindLocal, false)

	if _, err := o.Advance(context.Background(), child.ID); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if stage.State != run.StageStateAwaitingHostDispatch {
		t.Errorf("child stage state = %q, want awaiting_host_dispatch (parent unresolved → park)", stage.State)
	}
	if len(gh.calls) != 0 {
		t.Errorf("workflow_dispatch fired for an unresolved-parent local child: %d calls", len(gh.calls))
	}
}

// TestAdvance_DecomposedChild_ParentResolvedLocal_Parks pins the resolver
// branch (d) authoritative-park: an un-resolved local-kind child whose parent is
// RESOLVED local parks at awaiting_host_dispatch — the #1980 dogfood shape.
func TestAdvance_DecomposedChild_ParentResolvedLocal_Parks(t *testing.T) {
	o, rs, gh := newOrchestrator(t)
	parent, _ := rs.seed(t, "x/y", int64Ptr(42), nil)
	parent.RunnerKind = run.RunnerKindLocal
	parent.RunnerKindResolved = true
	child, stage := seedDecomposedChild(t, rs, int64Ptr(42), parent.ID, run.RunnerKindLocal, false)

	if _, err := o.Advance(context.Background(), child.ID); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if stage.State != run.StageStateAwaitingHostDispatch {
		t.Errorf("child stage state = %q, want awaiting_host_dispatch (parent resolved local → park)", stage.State)
	}
	if len(gh.calls) != 0 {
		t.Errorf("workflow_dispatch fired for a resolved-local-parent child: %d calls", len(gh.calls))
	}
}

// TestAdvance_DecomposedChild_ParentResolvedNonLocal_Fires pins the resolver
// branch (d) superseded-hint: an un-resolved child carrying a STALE local kind
// whose parent is RESOLVED non-local (github_actions) falls through to dispatched
// + workflow_dispatch — the parent lock is authoritative over the inherited hint.
func TestAdvance_DecomposedChild_ParentResolvedNonLocal_Fires(t *testing.T) {
	o, rs, gh := newOrchestrator(t)
	parent, _ := rs.seed(t, "x/y", int64Ptr(42), nil)
	parent.RunnerKind = run.RunnerKindGitHubActions
	parent.RunnerKindResolved = true
	child, stage := seedDecomposedChild(t, rs, int64Ptr(42), parent.ID, run.RunnerKindLocal, false)

	if _, err := o.Advance(context.Background(), child.ID); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if stage.State != run.StageStateDispatched {
		t.Errorf("child stage state = %q, want dispatched (parent lock supersedes stale local hint)", stage.State)
	}
	if len(gh.calls) != 1 {
		t.Errorf("workflow_dispatch calls = %d, want 1 (parent locked github_actions)", len(gh.calls))
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

// TestAdvance_DeployStage_ParksPreExecution asserts the ADR-038 / #1384
// pre-execution park: a pending deploy stage transitions to
// awaiting_deploy_approval and fires ZERO workflow_dispatch — nothing ships
// before the operator approves the deploy intent at the gate. The deploy
// stage is seeded with an AGENT executor to prove the deploy guard short-
// circuits the dispatch path that an agent stage would otherwise take.
func TestAdvance_DeployStage_ParksPreExecution(t *testing.T) {
	o, rs, gh := newOrchestrator(t)
	_, stages := rs.seed(t, "x/y", int64Ptr(42), []stageSeed{
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStateSucceeded},
		{Type: run.StageTypeDeploy, ExecutorKind: run.ExecutorAgent, ExecutorRef: "deploy", State: run.StageStatePending},
	})
	out, err := o.Advance(context.Background(), stages[0].RunID)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if out != OutcomeDispatched {
		t.Errorf("Outcome = %q, want dispatched (advanced to the deploy gate)", out)
	}
	if stages[1].State != run.StageStateAwaitingDeployApproval {
		t.Errorf("deploy stage state = %q, want awaiting_deploy_approval", stages[1].State)
	}
	// Nothing ships pre-gate: no workflow_dispatch fired.
	if len(gh.calls) != 0 {
		t.Errorf("workflow_dispatch fired for a parked deploy stage: %d", len(gh.calls))
	}

	// Idempotent: a re-entrant Advance is a no-op (the deploy stage is now
	// settled at the gate, not pending), still firing zero dispatch.
	out2, err := o.Advance(context.Background(), stages[0].RunID)
	if err != nil {
		t.Fatalf("re-entrant Advance: %v", err)
	}
	if out2 != OutcomeNoOp {
		t.Errorf("re-entrant Outcome = %q, want noop", out2)
	}
	if stages[1].State != run.StageStateAwaitingDeployApproval {
		t.Errorf("deploy stage state after re-entry = %q, want awaiting_deploy_approval", stages[1].State)
	}
	if len(gh.calls) != 0 {
		t.Errorf("workflow_dispatch fired on re-entry: %d", len(gh.calls))
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
	if got.PRNumber != 42 || got.Scope != forge.FromGitHubInstallationID(99) || got.Method != githubclient.MergeMethodSquash {
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

// auditRunIDForCategory reports whether every AppendChained call of the given
// category was recorded on the given run's chain (RunID == want). Returns
// false if no such entry exists.
func auditRunIDForCategory(a *recordingAudit, category string, want uuid.UUID) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	found := false
	for _, p := range a.appended {
		if p.Category != category {
			continue
		}
		found = true
		if p.RunID != want {
			return false
		}
	}
	return found
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

	// (ii.a) The exported wrapper used by out-of-package consumers
	// (server.fixupBranchFor, #1246) MUST emit the byte-identical name AND
	// match the unexported formula the runner's childSliceBranch mirrors.
	// Locking the exported symbol + its output here means the wrapper cannot
	// be removed, renamed, or desynced from sliceBranch without failing this
	// unit suite.
	for n := 0; n < 4; n++ {
		want := "fishhawk/run-aaaaaaaa/slice-" + strconv.Itoa(n)
		if got := SliceBranch(id, n); got != want {
			t.Errorf("SliceBranch(id,%d) = %q, want %q", n, got, want)
		}
		if got, unexp := SliceBranch(id, n), sliceBranch(id, n); got != unexp {
			t.Errorf("SliceBranch(id,%d) = %q, want byte-identical to sliceBranch %q", n, got, unexp)
		}
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

// TestConsolidatedPRTitleBody drives the pure #1774 helper across every
// branch: the chore-prefix / verbatim / no-issue title rules, the ## Summary
// heading with plan summary vs default fallback, the per-slice bullet list
// (paired-with-child-ids vs titles-only degrade), the Closes line, and the
// byte-identical attribution footer (asserted field-for-field against
// prTitleAndBody's shape, including the empty-baseURL relative-URL degrade).
func TestConsolidatedPRTitleBody(t *testing.T) {
	runID := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	implID := uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	child0 := uuid.MustParse("c0000000-0000-0000-0000-000000000000")
	child1 := uuid.MustParse("c1111111-1111-1111-1111-111111111111")
	head := "fishhawk/run-11111111-consolidated"

	planWithSlices := &plan.Plan{
		Summary: "Align the consolidated PR body with single-run conventions.",
		Decomposition: &plan.Decomposition{
			SubPlans: []plan.SubPlanSummary{
				{Title: "Slice one: backend helper"},
				{Title: "Slice two: runner mirror"},
			},
		},
	}

	// The exact footer literal a byte-identical mirror of prTitleAndBody must
	// render for the (a) case (non-empty baseURL, right-trimmed of the slash).
	wantFooterA := "\n\n---\n_Opened by [Fishhawk](https://github.com/kuhlman-labs/fishhawk) for run `" +
		runID.String() + "`, stage `" + implID.String() + "`._\n_Branch: `" + head +
		"` · Audit log: `https://app.fishhawk.test/v0/runs/" + runID.String() + "/audit`._\n"

	tests := []struct {
		name         string
		r            *run.Run
		p            *plan.Plan
		baseURL      string
		childIDs     []uuid.UUID
		wantTitle    string
		wantContains []string
		wantAbsent   []string
	}{
		{
			name:      "issue-triggered parent, non-conventional title, paired slices",
			r:         &run.Run{ID: runID, IssueContext: &run.IssueContext{Title: "Add widget", Number: 714}},
			p:         planWithSlices,
			baseURL:   "https://app.fishhawk.test/", // trailing slash trimmed
			childIDs:  []uuid.UUID{child0, child1},
			wantTitle: "chore: Add widget",
			wantContains: []string{
				"## Summary\n\nAlign the consolidated PR body with single-run conventions.",
				"\n- Slice one: backend helper (run c0000000)",
				"\n- Slice two: runner mirror (run c1111111)",
				"\n\nCloses #714",
				wantFooterA,
			},
		},
		{
			name:      "already-conventional issue title used verbatim (no double prefix)",
			r:         &run.Run{ID: runID, IssueContext: &run.IssueContext{Title: "feat(api): add endpoint", Number: 5}},
			p:         nil,
			baseURL:   "https://app.fishhawk.test",
			wantTitle: "feat(api): add endpoint",
			wantContains: []string{
				"## Summary\n\nConsolidated changes for decomposed run " + runID.String() + ".",
				"\n\nCloses #5",
			},
			wantAbsent: []string{"chore: feat(api)"},
		},
		{
			name:      "nil IssueContext falls back to run-id-stamped chore title",
			r:         &run.Run{ID: runID},
			p:         planWithSlices,
			baseURL:   "https://app.fishhawk.test",
			childIDs:  []uuid.UUID{child0, child1},
			wantTitle: "chore: fishhawk consolidated run 11111111",
			wantContains: []string{
				"## Summary\n\nAlign the consolidated PR body with single-run conventions.",
				"\n- Slice one: backend helper (run c0000000)",
				"Audit log: `https://app.fishhawk.test/v0/runs/" + runID.String() + "/audit`",
			},
			wantAbsent: []string{"Closes #"},
		},
		{
			name:      "plan without summary or sub_plans uses default summary, no bullets",
			r:         &run.Run{ID: runID, IssueContext: &run.IssueContext{Title: "Fix thing"}},
			p:         &plan.Plan{},
			baseURL:   "",
			wantTitle: "chore: Fix thing",
			wantContains: []string{
				"## Summary\n\nConsolidated changes for decomposed run " + runID.String() + ".",
				// Empty baseURL degrades the audit-log URL to a relative path.
				"Audit log: `/v0/runs/" + runID.String() + "/audit`",
			},
			wantAbsent: []string{"\n- ", "Closes #"},
		},
		{
			name:      "child-count mismatch degrades to titles-only bullets",
			r:         &run.Run{ID: runID, IssueContext: &run.IssueContext{Title: "Add widget"}},
			p:         planWithSlices,
			baseURL:   "https://app.fishhawk.test",
			childIDs:  []uuid.UUID{child0}, // 1 id, 2 sub_plans
			wantTitle: "chore: Add widget",
			wantContains: []string{
				"\n- Slice one: backend helper",
				"\n- Slice two: runner mirror",
			},
			wantAbsent: []string{"(run c0000000)", "(run "},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			title, body := consolidatedPRTitleBody(tc.r, tc.p, implID, head, tc.baseURL, tc.childIDs)
			if title != tc.wantTitle {
				t.Errorf("title = %q, want %q", title, tc.wantTitle)
			}
			for _, want := range tc.wantContains {
				if !strings.Contains(body, want) {
					t.Errorf("body missing %q:\n%s", want, body)
				}
			}
			for _, absent := range tc.wantAbsent {
				if strings.Contains(body, absent) {
					t.Errorf("body unexpectedly contains %q:\n%s", absent, body)
				}
			}
			// The body always opens with the ## Summary heading.
			if !strings.HasPrefix(body, "## Summary\n\n") {
				t.Errorf("body does not open with ## Summary heading:\n%s", body)
			}
		})
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
	if call.Scope != forge.FromGitHubInstallationID(55) {
		t.Errorf("installation_id scope = %v, want scope for 55", call.Scope)
	}
	// #1774: a non-conventional issue title is chore-prefixed to a
	// Conventional Commits header.
	if call.Title != "chore: Add widget" {
		t.Errorf("title = %q, want %q (chore-prefixed issue title)", call.Title, "chore: Add widget")
	}
	// Body carries the ## Summary heading, the Closes line, and the mirrored
	// attribution footer.
	if !strings.Contains(call.Body, "## Summary") {
		t.Errorf("body missing ## Summary heading:\n%s", call.Body)
	}
	if !strings.Contains(call.Body, "Closes #714") {
		t.Errorf("body missing Closes #714:\n%s", call.Body)
	}
	if !strings.Contains(call.Body, "_Opened by [Fishhawk](https://github.com/kuhlman-labs/fishhawk) for run `"+parent.ID.String()+"`") {
		t.Errorf("body missing attribution footer:\n%s", call.Body)
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

// TestAdvance_DecomposedParent_RecordsPullRequestArtifact is the #1732
// regression: opening the consolidated PR must ALSO record a kind=pull_request
// artifact on the parent IMPLEMENT stage so audit_complete's Rule 3
// (hasPullRequest over the implement stage) passes on a decomposed parent —
// previously it recorded none and audit_complete was permanently red. It
// asserts: (a) the URL is stamped; (b) exactly one kind=pull_request artifact
// lands on the IMPLEMENT stage carrying a non-empty head_sha + pr_number (the
// exact predicate auditcomplete Rule 3 and the publisher's decodeHeadSHA
// evaluate); (c) idempotency — a second pass over the gate records no second
// artifact; (d) graceful skip — with Artifacts=nil the flow still stamps the
// URL and returns no error.
func TestAdvance_DecomposedParent_RecordsPullRequestArtifact(t *testing.T) {
	o, rs, gh := newOrchestrator(t)
	o.DefaultRef = "main"
	arts := &fakeArtifacts{byStage: map[uuid.UUID][]*artifact.Artifact{}}
	o.Artifacts = arts

	parent, stages := seedDecomposedParent(t, rs, int64Ptr(55), run.ExecutorHuman)
	implStageID := stages[0].ID // implement is stage 0 (seedDecomposedParent)
	// The consolidated head resolves to a real tip SHA so the recorded artifact
	// carries a non-empty head_sha (the publisher-readiness predicate).
	gh.branchSHAs = map[string]string{
		consolidatedBranch(parent.ID): "headsha-consolidated",
		"main":                        "basesha-main",
	}

	if _, err := o.Advance(context.Background(), parent.ID); err != nil {
		t.Fatalf("Advance: %v", err)
	}

	// (a) URL stamped.
	if rs.runs[parent.ID].PullRequestURL == nil || *rs.runs[parent.ID].PullRequestURL == "" {
		t.Fatal("parent run pull_request_url not stamped")
	}

	// (b) exactly one pull_request artifact on the IMPLEMENT stage, decoding to
	// a non-empty head_sha + pr_number (audit_complete Rule 3 satisfied).
	implArts, err := arts.ListForStage(context.Background(), implStageID)
	if err != nil {
		t.Fatalf("ListForStage(implement): %v", err)
	}
	prArts := prArtifacts(implArts)
	if len(prArts) != 1 {
		t.Fatalf("pull_request artifacts on implement stage = %d, want 1", len(prArts))
	}
	// The artifact must NOT be attached to the review stage.
	if revArts, _ := arts.ListForStage(context.Background(), stages[1].ID); len(prArtifacts(revArts)) != 0 {
		t.Errorf("pull_request artifacts on review stage = %d, want 0 (must land on implement)", len(prArtifacts(revArts)))
	}
	var content struct {
		HeadSHA  string `json:"head_sha"`
		PRNumber int    `json:"pr_number"`
		BaseSHA  string `json:"base_sha"`
		Origin   string `json:"origin"`
		Branch   string `json:"branch"`
	}
	if err := json.Unmarshal(prArts[0].Content, &content); err != nil {
		t.Fatalf("decode pull_request artifact content: %v", err)
	}
	if content.HeadSHA != "headsha-consolidated" {
		t.Errorf("artifact head_sha = %q, want headsha-consolidated (empty => publisher never publishes)", content.HeadSHA)
	}
	if content.PRNumber != 777 {
		t.Errorf("artifact pr_number = %d, want 777 (the opened PR number)", content.PRNumber)
	}
	if content.Origin != "orchestrator_consolidated" {
		t.Errorf("artifact origin = %q, want orchestrator_consolidated", content.Origin)
	}
	if content.Branch != consolidatedBranch(parent.ID) {
		t.Errorf("artifact branch = %q, want %q", content.Branch, consolidatedBranch(parent.ID))
	}

	// (c) idempotency: a redelivered settle over the still-pending review gate
	// records no second artifact.
	stages[1].State = run.StageStatePending
	// Clear the stamped URL so Advance re-enters maybeOpenConsolidatedPR (the
	// URL-set short-circuit would otherwise return before the record path).
	rs.runs[parent.ID].PullRequestURL = nil
	if _, err := o.Advance(context.Background(), parent.ID); err != nil {
		t.Fatalf("Advance #2 (idempotency): %v", err)
	}
	implArts2, _ := arts.ListForStage(context.Background(), implStageID)
	if got := len(prArtifacts(implArts2)); got != 1 {
		t.Errorf("pull_request artifacts after second pass = %d, want still 1 (idempotent)", got)
	}
}

// TestAdvance_DecomposedParent_NilArtifacts_GracefulSkip asserts the (d)
// graceful-skip branch (#1732): with Artifacts=nil the consolidated-PR flow
// still stamps the URL and returns no error — the artifact recording is simply
// skipped, mirroring the GitHub/installation nil-skip posture.
func TestAdvance_DecomposedParent_NilArtifacts_GracefulSkip(t *testing.T) {
	o, rs, gh := newOrchestrator(t)
	o.DefaultRef = "main"
	o.Artifacts = nil // no artifact repo wired
	gh.branchSHAs = map[string]string{}

	parent, stages := seedDecomposedParent(t, rs, int64Ptr(55), run.ExecutorHuman)

	out, err := o.Advance(context.Background(), parent.ID)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if out != OutcomeDispatched {
		t.Errorf("Outcome = %q, want dispatched", out)
	}
	if rs.runs[parent.ID].PullRequestURL == nil || *rs.runs[parent.ID].PullRequestURL == "" {
		t.Error("parent run pull_request_url not stamped (nil Artifacts must not block the URL stamp)")
	}
	if stages[1].State != run.StageStateAwaitingApproval {
		t.Errorf("review stage = %q, want awaiting_approval", stages[1].State)
	}
}

// TestAdvance_DecomposedParent_ArtifactRecordError_LeavesURLUnstamped is the
// #1732 retry-correctness invariant: when recordConsolidatedPRArtifact returns
// an error (here o.GitHub.GetBranchSHA on the load-bearing head fails), the
// error must unwind maybeOpenConsolidatedPR WITHOUT stamping pull_request_url —
// so the next Advance re-enters the gate with the URL still empty, re-opens the
// PR idempotently, and records the artifact on retry. This is the crux of the
// 502-then-retry regression the issue exists to fix, asserted at the
// artifact-record seam (not just the IntegrateSlices seam).
func TestAdvance_DecomposedParent_ArtifactRecordError_LeavesURLUnstamped(t *testing.T) {
	o, rs, gh := newOrchestrator(t)
	o.DefaultRef = "main"
	arts := &fakeArtifacts{byStage: map[uuid.UUID][]*artifact.Artifact{}}
	o.Artifacts = arts

	parent, stages := seedDecomposedParent(t, rs, int64Ptr(55), run.ExecutorHuman)
	implStageID := stages[0].ID // implement is stage 0 (seedDecomposedParent)
	// The head-SHA resolution inside recordConsolidatedPRArtifact fails, so the
	// helper returns an error and maybeOpenConsolidatedPR unwinds before the URL
	// stamp. (The PR itself is opened first, so a retry recovers it via
	// ErrPullRequestExists.)
	gh.getBranchSHAErr = errors.New("transient github failure resolving head")

	if _, err := o.Advance(context.Background(), parent.ID); err == nil {
		t.Fatal("Advance: want error from artifact-record unwind, got nil")
	}

	// The URL must remain unstamped so the next Advance re-enters the gate.
	if url := rs.runs[parent.ID].PullRequestURL; url != nil {
		t.Errorf("pull_request_url = %q, want nil (record error must not stamp the URL)", *url)
	}
	// No artifact was recorded either (the head resolution failed before Create).
	implArts, _ := arts.ListForStage(context.Background(), implStageID)
	if got := len(prArtifacts(implArts)); got != 0 {
		t.Errorf("pull_request artifacts = %d, want 0 (record failed pre-Create)", got)
	}
}

// prArtifacts filters a stage's artifacts down to the kind=pull_request ones.
func prArtifacts(arts []*artifact.Artifact) []*artifact.Artifact {
	var out []*artifact.Artifact
	for _, a := range arts {
		if a.Kind == artifact.KindPullRequest {
			out = append(out, a)
		}
	}
	return out
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
	// review (the parent stays PR-less, same posture as the github_actions
	// backend's TriggerStage skip on a nil client).
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
	// integration_commit_shas records each fan-in merge commit in ascending
	// slice order, so the ADR-035 lineage guard attributes them (#1459).
	wantSHAs := []string{"mergesha-" + wantHead0, "mergesha-" + wantHead1}
	if got := integrationSHAsFromPayload(p); !reflect.DeepEqual(got, wantSHAs) {
		t.Errorf("slices_integrated integration_commit_shas = %v, want %v", got, wantSHAs)
	}
	_ = child1
}

// integrationSHAsFromPayload extracts the integration_commit_shas []string
// from a decoded slices_integrated audit payload (JSON arrays decode to
// []interface{}).
func integrationSHAsFromPayload(p map[string]any) []string {
	raw, _ := p["integration_commit_shas"].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
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

// TestIntegrateSlices_DisjointSlices_ConsolidateWithoutConflict is the #1669
// fan-in half of done-means: when each fan-out child is scoped to its slice,
// the slice branches touch non-overlapping file sets and consolidate cleanly.
// The stubGitHub is the git-merge simulator — disjoint file sets are modeled
// by MergeBranch returning success (no ErrMergeConflict programmed), which is
// exactly what a real three-way merge of non-overlapping diffs produces. A
// full live-agent fan-out is not unit-testable; this asserts the git
// integration path returns NO SliceConflict and merges every slice onto the
// consolidated branch in ascending order.
func TestIntegrateSlices_DisjointSlices_ConsolidateWithoutConflict(t *testing.T) {
	o, rs, gh := newOrchestrator(t)
	o.DefaultRef = "main"
	au := &recordingAudit{}
	o.Audit = au
	gh.branchSHAs = map[string]string{"main": "basesha"} // consolidated absent

	parent, _ := seedFanInParent(t, rs, int64Ptr(55))
	// Three disjoint slices (no programmed merge conflict on any head).
	_ = seedSucceededSlice(t, rs, parent.ID, int64Ptr(55), 0)
	_ = seedSucceededSlice(t, rs, parent.ID, int64Ptr(55), 1)
	_ = seedSucceededSlice(t, rs, parent.ID, int64Ptr(55), 2)

	conflict, err := o.IntegrateSlices(context.Background(), parent.ID)
	if err != nil {
		t.Fatalf("IntegrateSlices: %v", err)
	}
	if conflict != nil {
		t.Fatalf("conflict = %+v, want nil for disjoint slices", conflict)
	}

	consolidated := consolidatedBranch(parent.ID)
	if len(gh.mergeCalls) != 3 {
		t.Fatalf("MergeBranch calls = %d, want 3 (one per disjoint slice)", len(gh.mergeCalls))
	}
	for i, m := range gh.mergeCalls {
		if m.Base != consolidated {
			t.Errorf("merge[%d] base = %q, want %q", i, m.Base, consolidated)
		}
		if m.Head != sliceBranch(parent.ID, i) {
			t.Errorf("merge[%d] head = %q, want ascending slice %q", i, m.Head, sliceBranch(parent.ID, i))
		}
	}
	// A clean fan-in records slices_integrated, never slice_integration_conflict.
	p := auditPayload(t, au, "slices_integrated")
	if got, _ := p["slice_count"].(float64); int(got) != 3 {
		t.Errorf("slices_integrated slice_count = %v, want 3", p["slice_count"])
	}
}

// TestIntegrateSlices_OverlappingSlice_ReturnsConflict is the counterpart to
// the disjoint case: when a slice branch cannot merge onto the consolidated
// branch (overlapping edits — the wholesale-conflict failure mode #1669
// eliminates by scoping each child), IntegrateSlices returns a *SliceConflict
// naming the conflicting slice index and child run id, rather than silently
// dropping the slice. Asserts the return value directly (the maybe-advance
// wrapper is covered by TestIntegrateSlices_Conflict_FailsParentRecoverable).
func TestIntegrateSlices_OverlappingSlice_ReturnsConflict(t *testing.T) {
	o, rs, gh := newOrchestrator(t)
	o.DefaultRef = "main"
	au := &recordingAudit{}
	o.Audit = au
	gh.branchSHAs = map[string]string{"main": "basesha"}

	parent, _ := seedFanInParent(t, rs, int64Ptr(55))
	_ = seedSucceededSlice(t, rs, parent.ID, int64Ptr(55), 0)
	child1 := seedSucceededSlice(t, rs, parent.ID, int64Ptr(55), 1)

	// Slice-1's branch overlaps slice-0 and cannot merge.
	gh.mergeErrByHead = map[string]error{
		sliceBranch(parent.ID, 1): githubclient.ErrMergeConflict,
	}

	conflict, err := o.IntegrateSlices(context.Background(), parent.ID)
	if err != nil {
		t.Fatalf("IntegrateSlices: %v", err)
	}
	if conflict == nil {
		t.Fatal("conflict = nil, want a *SliceConflict for the overlapping slice")
	}
	if conflict.SliceIndex != 1 {
		t.Errorf("conflict.SliceIndex = %d, want 1", conflict.SliceIndex)
	}
	if conflict.ChildRunID != child1.ID {
		t.Errorf("conflict.ChildRunID = %s, want %s", conflict.ChildRunID, child1.ID)
	}
}

// integrationCommitRecords returns the decoded payloads of every
// integration_commit_recorded audit entry, in append order (#1806).
func integrationCommitRecords(t *testing.T, a *recordingAudit) []map[string]any {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []map[string]any
	for _, p := range a.appended {
		if p.Category != "integration_commit_recorded" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(p.Payload, &m); err != nil {
			t.Fatalf("decode integration_commit_recorded payload: %v", err)
		}
		out = append(out, m)
	}
	return out
}

// TestIntegrateSlices_PartialMergeThenConflict_RecordsCreatedMergeSHAs is the
// a26835f7 shape (#1806): slice-0 and slice-1 merge (201), then slice-2
// CONFLICTS and the fan-in bails early — so the terminal slices_integrated
// never fires. The merges already created must STILL be attributable, recorded
// incrementally via integration_commit_recorded at merge time.
func TestIntegrateSlices_PartialMergeThenConflict_RecordsCreatedMergeSHAs(t *testing.T) {
	o, rs, gh := newOrchestrator(t)
	o.DefaultRef = "main"
	au := &recordingAudit{}
	o.Audit = au
	gh.branchSHAs = map[string]string{"main": "basesha"}

	parent, _ := seedFanInParent(t, rs, int64Ptr(55))
	child0 := seedSucceededSlice(t, rs, parent.ID, int64Ptr(55), 0)
	child1 := seedSucceededSlice(t, rs, parent.ID, int64Ptr(55), 1)
	_ = seedSucceededSlice(t, rs, parent.ID, int64Ptr(55), 2)

	// Slice-2 conflicts, so the loop returns *SliceConflict before reaching
	// the terminal emitSlicesIntegrated.
	gh.mergeErrByHead = map[string]error{
		sliceBranch(parent.ID, 2): githubclient.ErrMergeConflict,
	}

	conflict, err := o.IntegrateSlices(context.Background(), parent.ID)
	if err != nil {
		t.Fatalf("IntegrateSlices: %v", err)
	}
	if conflict == nil {
		t.Fatal("conflict = nil, want a *SliceConflict for slice 2")
	}
	if conflict.SliceIndex != 2 {
		t.Errorf("conflict.SliceIndex = %d, want 2", conflict.SliceIndex)
	}

	// The terminal slices_integrated must NOT have fired (early return).
	if auditHasCategory(au, "slices_integrated") {
		t.Error("slices_integrated recorded on a partial pass; want none (early return)")
	}

	// slice-0 and slice-1's merges were recorded incrementally, so the SHAs
	// survive the early return.
	records := integrationCommitRecords(t, au)
	if len(records) != 2 {
		t.Fatalf("integration_commit_recorded entries = %d, want 2 (slice 0 and 1)", len(records))
	}
	wantSHA0 := "mergesha-" + sliceBranch(parent.ID, 0)
	wantSHA1 := "mergesha-" + sliceBranch(parent.ID, 1)
	if records[0]["merge_sha"] != wantSHA0 || records[1]["merge_sha"] != wantSHA1 {
		t.Errorf("recorded merge_sha = [%v, %v], want [%q, %q]",
			records[0]["merge_sha"], records[1]["merge_sha"], wantSHA0, wantSHA1)
	}
	// The entry carries the slice/child provenance and the parent's own run id.
	if got, _ := records[0]["slice_index"].(float64); int(got) != 0 {
		t.Errorf("records[0] slice_index = %v, want 0", records[0]["slice_index"])
	}
	if records[0]["child_run_id"] != child0.ID.String() {
		t.Errorf("records[0] child_run_id = %v, want %q", records[0]["child_run_id"], child0.ID.String())
	}
	if records[1]["child_run_id"] != child1.ID.String() {
		t.Errorf("records[1] child_run_id = %v, want %q", records[1]["child_run_id"], child1.ID.String())
	}
	// The recorded entry is on the PARENT's own chain (RunID == parent).
	if !auditRunIDForCategory(au, "integration_commit_recorded", parent.ID) {
		t.Errorf("integration_commit_recorded not appended on parent run %s chain", parent.ID)
	}
}

// TestIntegrateSlices_PartialMergeThenAPIError_RecordsCreatedMergeSHAs is the
// binding-condition companion to the conflict-path test: slice-0 merges (201),
// then slice-1's GitHub merge call ERRORS (a non-conflict API failure) and the
// fan-in returns (nil, err) before the terminal emit. Slice-0's merge SHA must
// still be recorded (#1806 — BOTH early-return paths lose the SHA).
func TestIntegrateSlices_PartialMergeThenAPIError_RecordsCreatedMergeSHAs(t *testing.T) {
	o, rs, gh := newOrchestrator(t)
	o.DefaultRef = "main"
	au := &recordingAudit{}
	o.Audit = au
	gh.branchSHAs = map[string]string{"main": "basesha"}

	parent, _ := seedFanInParent(t, rs, int64Ptr(55))
	child0 := seedSucceededSlice(t, rs, parent.ID, int64Ptr(55), 0)
	_ = seedSucceededSlice(t, rs, parent.ID, int64Ptr(55), 1)

	// Slice-1's merge fails with a generic (non-conflict) GitHub error, taking
	// the default branch that returns (nil, err) — a DIFFERENT early return
	// than the conflict path.
	apiErr := errors.New("github: 500 internal error")
	gh.mergeErrByHead = map[string]error{
		sliceBranch(parent.ID, 1): apiErr,
	}

	conflict, err := o.IntegrateSlices(context.Background(), parent.ID)
	if conflict != nil {
		t.Fatalf("conflict = %+v, want nil (this is the error path, not a conflict)", conflict)
	}
	if err == nil || !errors.Is(err, apiErr) {
		t.Fatalf("IntegrateSlices err = %v, want wrapped %v", err, apiErr)
	}

	// The terminal slices_integrated must NOT have fired.
	if auditHasCategory(au, "slices_integrated") {
		t.Error("slices_integrated recorded on an errored pass; want none (early return)")
	}

	// slice-0's merge was recorded incrementally before slice-1 errored.
	records := integrationCommitRecords(t, au)
	if len(records) != 1 {
		t.Fatalf("integration_commit_recorded entries = %d, want 1 (slice 0)", len(records))
	}
	wantSHA0 := "mergesha-" + sliceBranch(parent.ID, 0)
	if records[0]["merge_sha"] != wantSHA0 {
		t.Errorf("recorded merge_sha = %v, want %q", records[0]["merge_sha"], wantSHA0)
	}
	if records[0]["child_run_id"] != child0.ID.String() {
		t.Errorf("records[0] child_run_id = %v, want %q", records[0]["child_run_id"], child0.ID.String())
	}
}

// TestIntegrateSlices_RecordsOneMergePerSuccessfulMerge asserts the per-merge
// durability contract in both directions (#1806): a clean 3-slice pass emits
// three integration_commit_recorded entries AND the terminal slices_integrated;
// a re-entrant all-204 pass (slices already merged) emits NO new records — the
// SHAs were recorded on the original 201 pass.
func TestIntegrateSlices_RecordsOneMergePerSuccessfulMerge(t *testing.T) {
	o, rs, gh := newOrchestrator(t)
	o.DefaultRef = "main"
	au := &recordingAudit{}
	o.Audit = au
	gh.branchSHAs = map[string]string{"main": "basesha"}

	parent, _ := seedFanInParent(t, rs, int64Ptr(55))
	for i := 0; i < 3; i++ {
		_ = seedSucceededSlice(t, rs, parent.ID, int64Ptr(55), i)
	}

	// First pass: all three merge (201), recording three SHAs + the terminal.
	if conflict, err := o.IntegrateSlices(context.Background(), parent.ID); err != nil || conflict != nil {
		t.Fatalf("first IntegrateSlices: conflict=%v err=%v", conflict, err)
	}
	records := integrationCommitRecords(t, au)
	if len(records) != 3 {
		t.Fatalf("integration_commit_recorded entries = %d, want 3", len(records))
	}
	for i := 0; i < 3; i++ {
		want := "mergesha-" + sliceBranch(parent.ID, i)
		if records[i]["merge_sha"] != want {
			t.Errorf("records[%d] merge_sha = %v, want %q", i, records[i]["merge_sha"], want)
		}
	}
	if !auditHasCategory(au, "slices_integrated") {
		t.Error("clean pass did not record terminal slices_integrated")
	}

	// Second (re-entrant) pass: every slice is already merged → 204 no-ops.
	// No NEW integration_commit_recorded entries: an empty merge SHA records
	// nothing (the SHA was durable from the first pass).
	gh.mergeEmptyByHead = map[string]bool{}
	for i := 0; i < 3; i++ {
		gh.mergeEmptyByHead[sliceBranch(parent.ID, i)] = true
	}
	if conflict, err := o.IntegrateSlices(context.Background(), parent.ID); err != nil || conflict != nil {
		t.Fatalf("second IntegrateSlices: conflict=%v err=%v", conflict, err)
	}
	if got := integrationCommitRecords(t, au); len(got) != 3 {
		t.Errorf("integration_commit_recorded entries after re-entrant pass = %d, want 3 (no new records)", len(got))
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
	// All three integration merges recorded, ascending slice order (#1459).
	wantSHAs := []string{
		"mergesha-" + sliceBranch(parent.ID, 0),
		"mergesha-" + sliceBranch(parent.ID, 1),
		"mergesha-" + sliceBranch(parent.ID, 2),
	}
	if got := integrationSHAsFromPayload(p); !reflect.DeepEqual(got, wantSHAs) {
		t.Errorf("integration_commit_shas = %v, want %v", got, wantSHAs)
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

// TestAdvance_AcceptanceStage_DispatchesAndEmits pins E31.6 / #1534: unlike
// deploy, an acceptance-typed stage rides the ordinary agent dispatch path
// (pending → dispatched, NOT a pre-execution park) AND the orchestrator emits
// an acceptance_dispatched audit entry with a stage_id/sequence/executor
// payload so the living anchor renders the dispatch line.
func TestAdvance_AcceptanceStage_DispatchesAndEmits(t *testing.T) {
	rs := newStubRuns()
	gh := &stubGitHub{}
	ra := &recordingAudit{}
	o := &Orchestrator{Runs: rs, GitHub: gh, Audit: ra}
	_, stages := rs.seed(t, "x/y", int64Ptr(42), []stageSeed{
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStateSucceeded},
		{Type: run.StageTypeAcceptance, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStatePending},
	})

	out, err := o.Advance(context.Background(), stages[0].RunID)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if out != OutcomeDispatched {
		t.Errorf("Outcome = %q, want dispatched", out)
	}
	// Ordinary agent path, NOT a deploy-style park.
	if stages[1].State != run.StageStateDispatched {
		t.Errorf("acceptance stage state = %q, want dispatched (no park)", stages[1].State)
	}
	if stages[1].State == run.StageStateAwaitingDeployApproval {
		t.Errorf("acceptance stage must not park at a deploy gate")
	}

	var dispatched []audit.ChainAppendParams
	for _, p := range ra.appended {
		if p.Category == "acceptance_dispatched" {
			dispatched = append(dispatched, p)
		}
	}
	if len(dispatched) != 1 {
		t.Fatalf("acceptance_dispatched entries = %d, want 1", len(dispatched))
	}
	entry := dispatched[0]
	if entry.StageID == nil || *entry.StageID != stages[1].ID {
		t.Errorf("acceptance_dispatched stage_id = %v, want %s", entry.StageID, stages[1].ID)
	}
	for _, want := range []string{
		fmt.Sprintf(`"stage_id":%q`, stages[1].ID.String()),
		`"executor":"agent"`,
		`"sequence":`,
	} {
		if !strings.Contains(string(entry.Payload), want) {
			t.Errorf("acceptance_dispatched payload missing %s: %s", want, entry.Payload)
		}
	}
}

// TestAdvance_AcceptanceStage_NilAudit_StillDispatches pins the best-effort
// emit: a nil-Audit orchestrator still dispatches the acceptance stage (the
// emit WARN-logs and never unwinds the dispatch).
func TestAdvance_AcceptanceStage_NilAudit_StillDispatches(t *testing.T) {
	o, rs, _ := newOrchestrator(t) // Audit is nil
	_, stages := rs.seed(t, "x/y", int64Ptr(42), []stageSeed{
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStateSucceeded},
		{Type: run.StageTypeAcceptance, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStatePending},
	})
	out, err := o.Advance(context.Background(), stages[0].RunID)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if out != OutcomeDispatched || stages[1].State != run.StageStateDispatched {
		t.Errorf("nil-audit acceptance dispatch: out=%q state=%q", out, stages[1].State)
	}
}

// acceptanceSkipPlanBytes builds a standard_v1 plan body with the given
// out_of_scope entries and acceptance_criteria (E38.3 / #1657 fixtures). Only
// the verification block is load-bearing for the skip predicate.
func acceptanceSkipPlanBytes(t *testing.T, outOfScope []string, criteria []map[string]any) []byte {
	t.Helper()
	verification := map[string]any{
		"test_strategy": "unit",
		"rollback_plan": "revert the PR",
	}
	if len(outOfScope) > 0 {
		verification["out_of_scope"] = outOfScope
	}
	if len(criteria) > 0 {
		verification["acceptance_criteria"] = criteria
	}
	b, err := json.Marshal(map[string]any{
		"plan_version": "standard_v1",
		"summary":      "ship the widget endpoint",
		"verification": verification,
	})
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	return b
}

// seedAcceptanceSkipRun seeds a plan(succeeded)+implement(succeeded)+review
// (succeeded)+acceptance(pending) run with the plan artifact under the plan
// stage, and returns the run, stages, and wired orchestrator (Artifacts +
// recordingAudit). It is the shared scaffold for the E38.3 skip + control legs.
func seedAcceptanceSkipRun(t *testing.T, planBytes []byte) (*run.Run, []*run.Stage, *stubRuns, *recordingAudit, *Orchestrator) {
	t.Helper()
	rs := newStubRuns()
	ra := &recordingAudit{}
	r, stages := rs.seed(t, "x/y", int64Ptr(42), []stageSeed{
		{Type: run.StageTypePlan, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStateSucceeded},
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStateSucceeded},
		{Type: run.StageTypeReview, ExecutorKind: run.ExecutorHuman, ExecutorRef: "human", State: run.StageStateSucceeded},
		{Type: run.StageTypeAcceptance, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStatePending},
	})
	schemaV := "standard_v1"
	arts := &fakeArtifacts{byStage: map[uuid.UUID][]*artifact.Artifact{
		stages[0].ID: {{
			ID:            uuid.New(),
			StageID:       stages[0].ID,
			Kind:          artifact.KindPlan,
			SchemaVersion: &schemaV,
			Content:       planBytes,
			CreatedAt:     time.Now().UTC(),
		}},
	}}
	o := &Orchestrator{Runs: rs, GitHub: &stubGitHub{}, Audit: ra, Artifacts: arts}
	return r, stages, rs, ra, o
}

func countAcceptanceCategory(ra *recordingAudit, category string) int {
	n := 0
	for _, p := range ra.appended {
		if p.Category == category {
			n++
		}
	}
	return n
}

// TestAdvance_AcceptanceSkippedOutOfScope pins the E38.3 (#1657) auto-terminate:
// when the approved plan declares verification.out_of_scope with ZERO
// acceptance_criteria, Advance walks the acceptance stage straight to succeeded
// (NOT dispatched), emits exactly one acceptance_skipped_out_of_scope entry (and
// NO acceptance_dispatched), and drives the run to succeeded in the same call.
// The two controls prove the predicate gates the behavior: a plan with a
// blocking criterion, or one with no out_of_scope, still dispatches acceptance.
func TestAdvance_AcceptanceSkippedOutOfScope(t *testing.T) {
	t.Run("out_of_scope + zero criteria -> auto-terminated", func(t *testing.T) {
		planBytes := acceptanceSkipPlanBytes(t, []string{"deletion deferred to a follow-up"}, nil)
		r, stages, _, ra, o := seedAcceptanceSkipRun(t, planBytes)

		out, err := o.Advance(context.Background(), r.ID)
		if err != nil {
			t.Fatalf("Advance: %v", err)
		}
		if out != OutcomeRunCompleted {
			t.Errorf("Outcome = %q, want run_completed (the skip re-enters Advance to complete the run)", out)
		}
		if stages[3].State != run.StageStateSucceeded {
			t.Errorf("acceptance stage state = %q, want succeeded (auto-terminated, NOT dispatched)", stages[3].State)
		}
		if r.State != run.StateSucceeded {
			t.Errorf("run state = %q, want succeeded", r.State)
		}
		if n := countAcceptanceCategory(ra, "acceptance_skipped_out_of_scope"); n != 1 {
			t.Fatalf("acceptance_skipped_out_of_scope entries = %d, want 1", n)
		}
		if n := countAcceptanceCategory(ra, "acceptance_dispatched"); n != 0 {
			t.Errorf("acceptance_dispatched entries = %d, want 0 on the skip path", n)
		}
		// The marker payload carries the stage id + out_of_scope_count.
		var marker audit.ChainAppendParams
		for _, p := range ra.appended {
			if p.Category == "acceptance_skipped_out_of_scope" {
				marker = p
			}
		}
		if marker.StageID == nil || *marker.StageID != stages[3].ID {
			t.Errorf("skip marker stage_id = %v, want the acceptance stage %s", marker.StageID, stages[3].ID)
		}
		if !strings.Contains(string(marker.Payload), `"out_of_scope_count":1`) {
			t.Errorf("skip marker payload missing out_of_scope_count: %s", marker.Payload)
		}
	})

	t.Run("out_of_scope + a blocking criterion -> still dispatches", func(t *testing.T) {
		planBytes := acceptanceSkipPlanBytes(t,
			[]string{"deletion deferred"},
			[]map[string]any{{"id": "ac-1", "statement": "POST returns 201", "source": "explicit", "source_ref": "#1", "blocking": true}})
		r, stages, _, ra, o := seedAcceptanceSkipRun(t, planBytes)

		if _, err := o.Advance(context.Background(), r.ID); err != nil {
			t.Fatalf("Advance: %v", err)
		}
		if stages[3].State != run.StageStateDispatched {
			t.Errorf("acceptance stage state = %q, want dispatched (criteria present -> not skippable)", stages[3].State)
		}
		if n := countAcceptanceCategory(ra, "acceptance_skipped_out_of_scope"); n != 0 {
			t.Errorf("acceptance_skipped_out_of_scope entries = %d, want 0 when a criterion is present", n)
		}
		if n := countAcceptanceCategory(ra, "acceptance_dispatched"); n != 1 {
			t.Errorf("acceptance_dispatched entries = %d, want 1 (operator-dispatched path)", n)
		}
	})

	// Control: a drivable blocking criterion with NO out_of_scope is neither
	// E38.3-skippable (out_of_scope==0) NOR #1728 short-circuitable
	// (acceptance_criteria>0) — it still dispatches to the operator/agent path.
	// (Was "no out_of_scope -> still dispatches" with an empty plan; that empty
	// plan now SHORT-CIRCUITS under #1728, asserted separately.)
	t.Run("drivable blocking criterion, no out_of_scope -> still dispatches", func(t *testing.T) {
		planBytes := acceptanceSkipPlanBytes(t, nil,
			[]map[string]any{{"id": "ac-1", "statement": "POST returns 201", "source": "explicit", "source_ref": "#1", "blocking": true}})
		r, stages, _, ra, o := seedAcceptanceSkipRun(t, planBytes)

		if _, err := o.Advance(context.Background(), r.ID); err != nil {
			t.Fatalf("Advance: %v", err)
		}
		if stages[3].State != run.StageStateDispatched {
			t.Errorf("acceptance stage state = %q, want dispatched (criteria present, no out_of_scope -> neither skip fires)", stages[3].State)
		}
		if n := countAcceptanceCategory(ra, "acceptance_skipped_out_of_scope"); n != 0 {
			t.Errorf("acceptance_skipped_out_of_scope entries = %d, want 0", n)
		}
		if n := countAcceptanceCategory(ra, "acceptance_outcome_recorded"); n != 0 {
			t.Errorf("acceptance_outcome_recorded entries = %d, want 0 at dispatch (no short-circuit)", n)
		}
		if n := countAcceptanceCategory(ra, "acceptance_dispatched"); n != 1 {
			t.Errorf("acceptance_dispatched entries = %d, want 1", n)
		}
	})

	// Defensive branch: a plan artifact that cannot be decoded surfaces the
	// load error rather than silently dispatching (or silently skipping) — the
	// acceptance stage stays pending, no transition, no marker.
	t.Run("unparseable plan artifact -> load error surfaces", func(t *testing.T) {
		r, stages, _, ra, o := seedAcceptanceSkipRun(t, []byte(`{not valid json`))

		if _, err := o.Advance(context.Background(), r.ID); err == nil {
			t.Fatal("Advance must surface the plan-load error, not silently dispatch")
		}
		if stages[3].State != run.StageStatePending {
			t.Errorf("acceptance stage state = %q, want pending (load error -> no transition)", stages[3].State)
		}
		if n := countAcceptanceCategory(ra, "acceptance_skipped_out_of_scope"); n != 0 {
			t.Errorf("acceptance_skipped_out_of_scope entries = %d, want 0 on a load error", n)
		}
	})

	// Defensive branch: a nil Audit is best-effort — the skip still walks the
	// acceptance stage to succeeded and completes the run (the emit WARN-logs
	// and never unwinds), mirroring emitAcceptanceDispatched's nil-Audit posture.
	t.Run("nil audit -> still auto-terminates", func(t *testing.T) {
		planBytes := acceptanceSkipPlanBytes(t, []string{"deletion deferred"}, nil)
		r, stages, _, _, o := seedAcceptanceSkipRun(t, planBytes)
		o.Audit = nil

		out, err := o.Advance(context.Background(), r.ID)
		if err != nil {
			t.Fatalf("Advance: %v", err)
		}
		if out != OutcomeRunCompleted || stages[3].State != run.StageStateSucceeded || r.State != run.StateSucceeded {
			t.Errorf("nil-audit skip: out=%q acceptance=%q run=%q, want run_completed/succeeded/succeeded", out, stages[3].State, r.State)
		}
	})
}

// TestAdvance_AcceptanceShortCircuitEmptyCriteria pins the #1728 (E41.5)
// pre-spawn short-circuit: when the approved plan declares ZERO
// acceptance_criteria AND ZERO out_of_scope, Advance walks the acceptance stage
// straight to succeeded (NOT dispatched), records exactly one
// acceptance_outcome_recorded verdict=passed carrying basis=empty-criteria (and
// NO acceptance_dispatched, NO acceptance_skipped_out_of_scope marker), and
// drives the run to succeeded in the same call. The nil-Audit subtest pins the
// best-effort emit: the walk still completes the run without an appended entry.
func TestAdvance_AcceptanceShortCircuitEmptyCriteria(t *testing.T) {
	t.Run("zero criteria + zero out_of_scope -> short-circuit records verdict", func(t *testing.T) {
		planBytes := acceptanceSkipPlanBytes(t, nil, nil)
		r, stages, _, ra, o := seedAcceptanceSkipRun(t, planBytes)

		out, err := o.Advance(context.Background(), r.ID)
		if err != nil {
			t.Fatalf("Advance: %v", err)
		}
		if out != OutcomeRunCompleted {
			t.Errorf("Outcome = %q, want run_completed (the short-circuit re-enters Advance to complete the run)", out)
		}
		if stages[3].State != run.StageStateSucceeded {
			t.Errorf("acceptance stage state = %q, want succeeded (short-circuited, NOT dispatched)", stages[3].State)
		}
		if r.State != run.StateSucceeded {
			t.Errorf("run state = %q, want succeeded", r.State)
		}
		if n := countAcceptanceCategory(ra, "acceptance_dispatched"); n != 0 {
			t.Errorf("acceptance_dispatched entries = %d, want 0 on the short-circuit path", n)
		}
		if n := countAcceptanceCategory(ra, "acceptance_skipped_out_of_scope"); n != 0 {
			t.Errorf("acceptance_skipped_out_of_scope entries = %d, want 0 (empty-criteria records a verdict, not a skip marker)", n)
		}
		if n := countAcceptanceCategory(ra, "acceptance_outcome_recorded"); n != 1 {
			t.Fatalf("acceptance_outcome_recorded entries = %d, want 1", n)
		}
		var verdict audit.ChainAppendParams
		for _, p := range ra.appended {
			if p.Category == "acceptance_outcome_recorded" {
				verdict = p
			}
		}
		if verdict.StageID == nil || *verdict.StageID != stages[3].ID {
			t.Errorf("verdict stage_id = %v, want the acceptance stage %s", verdict.StageID, stages[3].ID)
		}
		for _, want := range []string{
			`"verdict":"passed"`,
			`"outcome":"accepted"`,
			`"criteria_total":0`,
			fmt.Sprintf("%q:%q", plan.AcceptanceBasisKey, plan.AcceptanceBasisEmptyCriteria),
		} {
			if !strings.Contains(string(verdict.Payload), want) {
				t.Errorf("acceptance_outcome_recorded payload missing %s: %s", want, verdict.Payload)
			}
		}
	})

	// Defensive branch: a nil Audit is best-effort — the short-circuit still
	// walks the acceptance stage to succeeded and completes the run (the emit
	// WARN-logs and never unwinds), mirroring the E38.3 nil-Audit posture.
	t.Run("nil audit -> still short-circuits", func(t *testing.T) {
		planBytes := acceptanceSkipPlanBytes(t, nil, nil)
		r, stages, _, _, o := seedAcceptanceSkipRun(t, planBytes)
		o.Audit = nil

		out, err := o.Advance(context.Background(), r.ID)
		if err != nil {
			t.Fatalf("Advance: %v", err)
		}
		if out != OutcomeRunCompleted || stages[3].State != run.StageStateSucceeded || r.State != run.StateSucceeded {
			t.Errorf("nil-audit short-circuit: out=%q acceptance=%q run=%q, want run_completed/succeeded/succeeded", out, stages[3].State, r.State)
		}
	})
}

// TestAdvance_AcceptanceShortCircuitAllSkipWithBasis pins the #1748 (E41.6)
// pre-spawn short-circuit: when EVERY acceptance criterion carries
// skip_expected with a non-empty expectation_basis, Advance walks the acceptance
// stage straight to succeeded (NOT dispatched), records exactly one
// acceptance_outcome_recorded verdict=passed carrying basis=all-skip-with-basis
// with criteria_total/criteria_skipped == N (and NO acceptance_dispatched, NO
// skip marker, NO preview), and drives the run to succeeded in the same call.
// The mixed control (one drivable criterion) proves the predicate gates the
// behavior: it takes the normal operator-dispatched path with no short-circuit
// entry.
func TestAdvance_AcceptanceShortCircuitAllSkipWithBasis(t *testing.T) {
	allSkip := []map[string]any{
		{"id": "webhook-fires", "statement": "webhook fires on close", "source": "inferred", "rationale": "external", "skip_expected": true, "expectation_basis": "validated in webhook_integration_test.go with a fake"},
		{"id": "issue-closes", "statement": "issue auto-closes", "source": "inferred", "rationale": "external", "skip_expected": true, "expectation_basis": "validated in closer_e2e_test.go"},
	}

	t.Run("every criterion skip_expected with basis -> short-circuit records verdict", func(t *testing.T) {
		planBytes := acceptanceSkipPlanBytes(t, nil, allSkip)
		r, stages, _, ra, o := seedAcceptanceSkipRun(t, planBytes)

		out, err := o.Advance(context.Background(), r.ID)
		if err != nil {
			t.Fatalf("Advance: %v", err)
		}
		if out != OutcomeRunCompleted {
			t.Errorf("Outcome = %q, want run_completed (the short-circuit re-enters Advance to complete the run)", out)
		}
		if stages[3].State != run.StageStateSucceeded {
			t.Errorf("acceptance stage state = %q, want succeeded (short-circuited, NOT dispatched)", stages[3].State)
		}
		if r.State != run.StateSucceeded {
			t.Errorf("run state = %q, want succeeded", r.State)
		}
		if n := countAcceptanceCategory(ra, "acceptance_dispatched"); n != 0 {
			t.Errorf("acceptance_dispatched entries = %d, want 0 on the short-circuit path", n)
		}
		if n := countAcceptanceCategory(ra, "acceptance_skipped_out_of_scope"); n != 0 {
			t.Errorf("acceptance_skipped_out_of_scope entries = %d, want 0 (all-skip records a verdict, not a skip marker)", n)
		}
		if n := countAcceptanceCategory(ra, "acceptance_outcome_recorded"); n != 1 {
			t.Fatalf("acceptance_outcome_recorded entries = %d, want 1", n)
		}
		var verdict audit.ChainAppendParams
		for _, p := range ra.appended {
			if p.Category == "acceptance_outcome_recorded" {
				verdict = p
			}
		}
		if verdict.StageID == nil || *verdict.StageID != stages[3].ID {
			t.Errorf("verdict stage_id = %v, want the acceptance stage %s", verdict.StageID, stages[3].ID)
		}
		for _, want := range []string{
			`"verdict":"passed"`,
			`"outcome":"accepted"`,
			`"criteria_total":2`,
			`"criteria_skipped":2`,
			`"criteria_passed":0`,
			`"criteria_failed":0`,
			fmt.Sprintf("%q:%q", plan.AcceptanceBasisKey, plan.AcceptanceBasisAllSkipWithBasis),
		} {
			if !strings.Contains(string(verdict.Payload), want) {
				t.Errorf("acceptance_outcome_recorded payload missing %s: %s", want, verdict.Payload)
			}
		}
	})

	// Control: one drivable criterion (no marker) among skip-expected ones is
	// NOT all-skip-with-basis (nor empty-criteria, nor out_of_scope) -> normal
	// dispatch, no short-circuit entry.
	t.Run("mixed: one drivable criterion -> still dispatches", func(t *testing.T) {
		mixed := []map[string]any{
			allSkip[0],
			{"id": "get-returns-200", "statement": "GET returns 200", "source": "explicit", "source_ref": "#1", "blocking": true},
		}
		planBytes := acceptanceSkipPlanBytes(t, nil, mixed)
		r, stages, _, ra, o := seedAcceptanceSkipRun(t, planBytes)

		if _, err := o.Advance(context.Background(), r.ID); err != nil {
			t.Fatalf("Advance: %v", err)
		}
		if stages[3].State != run.StageStateDispatched {
			t.Errorf("acceptance stage state = %q, want dispatched (a drivable criterion -> not short-circuitable)", stages[3].State)
		}
		if n := countAcceptanceCategory(ra, "acceptance_outcome_recorded"); n != 0 {
			t.Errorf("acceptance_outcome_recorded entries = %d, want 0 at dispatch (no short-circuit)", n)
		}
		if n := countAcceptanceCategory(ra, "acceptance_dispatched"); n != 1 {
			t.Errorf("acceptance_dispatched entries = %d, want 1 (operator-dispatched path)", n)
		}
	})

	// Defensive branch: a nil Audit is best-effort — the short-circuit still
	// walks the acceptance stage to succeeded and completes the run.
	t.Run("nil audit -> still short-circuits", func(t *testing.T) {
		planBytes := acceptanceSkipPlanBytes(t, nil, allSkip)
		r, stages, _, _, o := seedAcceptanceSkipRun(t, planBytes)
		o.Audit = nil

		out, err := o.Advance(context.Background(), r.ID)
		if err != nil {
			t.Fatalf("Advance: %v", err)
		}
		if out != OutcomeRunCompleted || stages[3].State != run.StageStateSucceeded || r.State != run.StateSucceeded {
			t.Errorf("nil-audit short-circuit: out=%q acceptance=%q run=%q, want run_completed/succeeded/succeeded", out, stages[3].State, r.State)
		}
	})
}

// seedAcceptanceSkipRunWithAcceptanceState is seedAcceptanceSkipRun with the
// acceptance stage seeded in an explicit state (the parked-'dispatched' host-
// dispatch case, #1928). It returns the run, stages, stub, audit, and wired
// orchestrator.
func seedAcceptanceSkipRunWithAcceptanceState(t *testing.T, planBytes []byte, acceptanceState run.StageState) (*run.Run, []*run.Stage, *stubRuns, *recordingAudit, *Orchestrator) {
	t.Helper()
	rs := newStubRuns()
	ra := &recordingAudit{}
	r, stages := rs.seed(t, "x/y", int64Ptr(42), []stageSeed{
		{Type: run.StageTypePlan, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStateSucceeded},
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStateSucceeded},
		{Type: run.StageTypeReview, ExecutorKind: run.ExecutorHuman, ExecutorRef: "human", State: run.StageStateSucceeded},
		{Type: run.StageTypeAcceptance, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: acceptanceState},
	})
	schemaV := "standard_v1"
	arts := &fakeArtifacts{byStage: map[uuid.UUID][]*artifact.Artifact{
		stages[0].ID: {{
			ID:            uuid.New(),
			StageID:       stages[0].ID,
			Kind:          artifact.KindPlan,
			SchemaVersion: &schemaV,
			Content:       planBytes,
			CreatedAt:     time.Now().UTC(),
		}},
	}}
	o := &Orchestrator{Runs: rs, GitHub: &stubGitHub{}, Audit: ra, Artifacts: arts}
	return r, stages, rs, ra, o
}

// TestTryShortCircuitAcceptance pins the exported entry point the acceptance-
// admission endpoint calls at initial host dispatch (#1928): it settles an
// all-skip-with-basis / empty-criteria / out-of-scope acceptance stage straight
// to succeeded (from pending OR the parked 'dispatched' state), fires NO
// dispatch, and re-enters Advance to roll the run to succeeded — while a
// mixed-criteria plan, a non-acceptance stage, and an already-terminal stage all
// no-op with no state change.
func TestTryShortCircuitAcceptance(t *testing.T) {
	allSkip := []map[string]any{
		{"id": "webhook-fires", "statement": "webhook fires on close", "source": "inferred", "rationale": "external", "skip_expected": true, "expectation_basis": "validated in webhook_integration_test.go with a fake"},
		{"id": "issue-closes", "statement": "issue auto-closes", "source": "inferred", "rationale": "external", "skip_expected": true, "expectation_basis": "validated in closer_e2e_test.go"},
	}

	// (a) all-skip-with-basis + pending acceptance stage settles to succeeded,
	// records the verdict, fires NO dispatch, rolls the run to succeeded.
	t.Run("all-skip-with-basis pending -> short-circuits", func(t *testing.T) {
		planBytes := acceptanceSkipPlanBytes(t, nil, allSkip)
		r, stages, _, ra, o := seedAcceptanceSkipRun(t, planBytes)

		sc, liveReq, err := o.TryShortCircuitAcceptance(context.Background(), r.ID, stages[3].ID)
		if err != nil {
			t.Fatalf("TryShortCircuitAcceptance: %v", err)
		}
		if liveReq {
			t.Error("liveValidationRequired = true, want false on an all-skip short-circuit hit")
		}
		if sc == nil {
			t.Fatal("short-circuit = nil, want a hit")
		}
		if sc.Kind != AcceptanceShortCircuitAllSkipWithBasis || sc.Basis != plan.AcceptanceBasisAllSkipWithBasis || sc.CriteriaTotal != 2 {
			t.Errorf("short-circuit = %+v, want kind=%s basis=%s total=2", sc, AcceptanceShortCircuitAllSkipWithBasis, plan.AcceptanceBasisAllSkipWithBasis)
		}
		if stages[3].State != run.StageStateSucceeded {
			t.Errorf("acceptance stage = %q, want succeeded", stages[3].State)
		}
		if r.State != run.StateSucceeded {
			t.Errorf("run state = %q, want succeeded (Advance re-entered)", r.State)
		}
		if n := countAcceptanceCategory(ra, "acceptance_dispatched"); n != 0 {
			t.Errorf("acceptance_dispatched = %d, want 0", n)
		}
		if n := countAcceptanceCategory(ra, "acceptance_outcome_recorded"); n != 1 {
			t.Errorf("acceptance_outcome_recorded = %d, want 1", n)
		}
	})

	// (b) #1936 (failure mode a: dispatched-non-admissible): a 'dispatched'
	// acceptance stage is NO LONGER admissible. Post-#1912 'dispatched' means the
	// host-dispatch marker already stamped a spawn attempt, so short-circuiting it
	// under a live runner is exactly the double-drive #1936 closes. It must now
	// no-op — (nil, false, nil), ZERO stage transitions, ZERO verdict audits — and
	// degrade to the normal operator-dispatched spawn path.
	t.Run("dispatched-park -> non-admissible no-op (double-drive fence)", func(t *testing.T) {
		planBytes := acceptanceSkipPlanBytes(t, nil, allSkip)
		r, stages, rs, ra, o := seedAcceptanceSkipRunWithAcceptanceState(t, planBytes, run.StageStateDispatched)

		sc, liveReq, err := o.TryShortCircuitAcceptance(context.Background(), r.ID, stages[3].ID)
		if err != nil {
			t.Fatalf("TryShortCircuitAcceptance: %v", err)
		}
		if sc != nil {
			t.Errorf("short-circuit = %+v, want nil (dispatched is non-admissible post-#1936)", sc)
		}
		if liveReq {
			t.Error("liveValidationRequired = true, want false on a non-admissible 'dispatched' stage")
		}
		if stages[3].State != run.StageStateDispatched {
			t.Errorf("acceptance stage = %q, want dispatched (untouched — no state change)", stages[3].State)
		}
		for _, tr := range rs.stageTransitions {
			if tr.StageID == stages[3].ID {
				t.Errorf("stage was transitioned to %q; want ZERO transitions on a non-admissible no-op", tr.To)
			}
		}
		if n := countAcceptanceCategory(ra, "acceptance_outcome_recorded"); n != 0 {
			t.Errorf("acceptance_outcome_recorded = %d, want 0 (no walk fired)", n)
		}
	})

	// (b2) #1912: the stage starts parked in 'awaiting_host_dispatch' (the new
	// local host-dispatch park, replacing the conflated 'dispatched'): the walk
	// starts from awaiting_host_dispatch and still settles to succeeded without
	// re-issuing the already-passed pending->awaiting_host_dispatch transition.
	t.Run("all-skip-with-basis awaiting_host_dispatch-park -> short-circuits", func(t *testing.T) {
		planBytes := acceptanceSkipPlanBytes(t, nil, allSkip)
		r, stages, rs, ra, o := seedAcceptanceSkipRunWithAcceptanceState(t, planBytes, run.StageStateAwaitingHostDispatch)

		sc, _, err := o.TryShortCircuitAcceptance(context.Background(), r.ID, stages[3].ID)
		if err != nil {
			t.Fatalf("TryShortCircuitAcceptance: %v", err)
		}
		if sc == nil || sc.Kind != AcceptanceShortCircuitAllSkipWithBasis {
			t.Fatalf("short-circuit = %+v, want an all-skip-with-basis hit", sc)
		}
		if stages[3].State != run.StageStateSucceeded {
			t.Errorf("acceptance stage = %q, want succeeded", stages[3].State)
		}
		if r.State != run.StateSucceeded {
			t.Errorf("run state = %q, want succeeded", r.State)
		}
		// The walk must NOT re-issue the already-passed pending->awaiting_host_dispatch
		// transition: it starts at dispatched.
		for _, tr := range rs.stageTransitions {
			if tr.StageID == stages[3].ID && tr.To == run.StageStateAwaitingHostDispatch {
				t.Errorf("walk re-issued pending->awaiting_host_dispatch for a stage already parked")
			}
		}
		if n := countAcceptanceCategory(ra, "acceptance_outcome_recorded"); n != 1 {
			t.Errorf("acceptance_outcome_recorded = %d, want 1", n)
		}
	})

	// (c) empty-criteria + out-of-scope parity — both other disjoint predicates
	// settle through the same exported entry point.
	t.Run("empty-criteria pending -> short-circuits", func(t *testing.T) {
		planBytes := acceptanceSkipPlanBytes(t, nil, nil)
		r, stages, _, _, o := seedAcceptanceSkipRun(t, planBytes)
		sc, liveReq, err := o.TryShortCircuitAcceptance(context.Background(), r.ID, stages[3].ID)
		if err != nil {
			t.Fatalf("TryShortCircuitAcceptance: %v", err)
		}
		if liveReq {
			t.Error("liveValidationRequired = true, want false on an empty-criteria short-circuit hit")
		}
		if sc == nil || sc.Kind != AcceptanceShortCircuitEmptyCriteria {
			t.Fatalf("short-circuit = %+v, want an empty-criteria hit", sc)
		}
		if stages[3].State != run.StageStateSucceeded || r.State != run.StateSucceeded {
			t.Errorf("acceptance=%q run=%q, want succeeded/succeeded", stages[3].State, r.State)
		}
	})
	t.Run("out-of-scope pending -> short-circuits", func(t *testing.T) {
		planBytes := acceptanceSkipPlanBytes(t, []string{"deletion deferred to a follow-up"}, nil)
		r, stages, _, ra, o := seedAcceptanceSkipRun(t, planBytes)
		sc, liveReq, err := o.TryShortCircuitAcceptance(context.Background(), r.ID, stages[3].ID)
		if err != nil {
			t.Fatalf("TryShortCircuitAcceptance: %v", err)
		}
		if liveReq {
			t.Error("liveValidationRequired = true, want false on an out-of-scope short-circuit hit")
		}
		if sc == nil || sc.Kind != AcceptanceShortCircuitOutOfScope {
			t.Fatalf("short-circuit = %+v, want an out-of-scope hit", sc)
		}
		if n := countAcceptanceCategory(ra, "acceptance_skipped_out_of_scope"); n != 1 {
			t.Errorf("acceptance_skipped_out_of_scope = %d, want 1", n)
		}
		if stages[3].State != run.StageStateSucceeded || r.State != run.StateSucceeded {
			t.Errorf("acceptance=%q run=%q, want succeeded/succeeded", stages[3].State, r.State)
		}
	})

	// (d) mixed-criteria plan (one drivable criterion) returns nil and leaves the
	// stage untouched (short_circuited:false at the endpoint).
	t.Run("mixed criteria -> nil no-op", func(t *testing.T) {
		mixed := []map[string]any{
			allSkip[0],
			{"id": "get-returns-200", "statement": "GET returns 200", "source": "explicit", "source_ref": "#1", "blocking": true},
		}
		planBytes := acceptanceSkipPlanBytes(t, nil, mixed)
		r, stages, _, ra, o := seedAcceptanceSkipRun(t, planBytes)
		sc, liveReq, err := o.TryShortCircuitAcceptance(context.Background(), r.ID, stages[3].ID)
		if err != nil {
			t.Fatalf("TryShortCircuitAcceptance: %v", err)
		}
		if sc != nil {
			t.Errorf("short-circuit = %+v, want nil (mixed criteria)", sc)
		}
		if !liveReq {
			t.Error("liveValidationRequired = false, want true on a mixed-criteria plan (at least one criterion needs a live target)")
		}
		if stages[3].State != run.StageStatePending {
			t.Errorf("acceptance stage = %q, want pending (untouched)", stages[3].State)
		}
		if n := countAcceptanceCategory(ra, "acceptance_outcome_recorded"); n != 0 {
			t.Errorf("acceptance_outcome_recorded = %d, want 0", n)
		}
	})

	// (e) a non-acceptance stage id -> nil no-op, no state change.
	t.Run("non-acceptance stage -> nil no-op", func(t *testing.T) {
		planBytes := acceptanceSkipPlanBytes(t, nil, allSkip)
		r, stages, _, _, o := seedAcceptanceSkipRun(t, planBytes)
		// stages[1] is the implement stage (already succeeded).
		sc, liveReq, err := o.TryShortCircuitAcceptance(context.Background(), r.ID, stages[1].ID)
		if err != nil {
			t.Fatalf("TryShortCircuitAcceptance: %v", err)
		}
		if sc != nil {
			t.Errorf("short-circuit = %+v, want nil (non-acceptance stage)", sc)
		}
		if liveReq {
			t.Error("liveValidationRequired = true, want false for a non-acceptance stage")
		}
	})

	// (f) an already-terminal (succeeded) acceptance stage -> nil no-op: a
	// non-admissible state is short_circuited:false, never a re-settle.
	t.Run("already-terminal acceptance -> nil no-op", func(t *testing.T) {
		planBytes := acceptanceSkipPlanBytes(t, nil, allSkip)
		r, stages, _, ra, o := seedAcceptanceSkipRunWithAcceptanceState(t, planBytes, run.StageStateSucceeded)
		sc, liveReq, err := o.TryShortCircuitAcceptance(context.Background(), r.ID, stages[3].ID)
		if err != nil {
			t.Fatalf("TryShortCircuitAcceptance: %v", err)
		}
		if sc != nil {
			t.Errorf("short-circuit = %+v, want nil (already terminal)", sc)
		}
		if liveReq {
			t.Error("liveValidationRequired = true, want false for an already-settled acceptance stage")
		}
		if n := countAcceptanceCategory(ra, "acceptance_outcome_recorded"); n != 0 {
			t.Errorf("acceptance_outcome_recorded = %d, want 0", n)
		}
	})

	// (g) an unknown stage id -> nil no-op.
	t.Run("unknown stage -> nil no-op", func(t *testing.T) {
		planBytes := acceptanceSkipPlanBytes(t, nil, allSkip)
		r, _, _, _, o := seedAcceptanceSkipRun(t, planBytes)
		sc, liveReq, err := o.TryShortCircuitAcceptance(context.Background(), r.ID, uuid.New())
		if err != nil {
			t.Fatalf("TryShortCircuitAcceptance: %v", err)
		}
		if sc != nil {
			t.Errorf("short-circuit = %+v, want nil (unknown stage)", sc)
		}
		if liveReq {
			t.Error("liveValidationRequired = true, want false for an unknown stage id")
		}
	})

	// (h) a run with NO approved plan artifact -> nil no-op with
	// liveValidationRequired false: the endpoint can't report needs_target when
	// it can't load the plan (it renders a plain short_circuited:false).
	t.Run("nil plan -> nil no-op, live-validation false", func(t *testing.T) {
		rs := newStubRuns()
		ra := &recordingAudit{}
		r, stages := rs.seed(t, "x/y", int64Ptr(42), []stageSeed{
			{Type: run.StageTypePlan, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStateSucceeded},
			{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStateSucceeded},
			{Type: run.StageTypeReview, ExecutorKind: run.ExecutorHuman, ExecutorRef: "human", State: run.StageStateSucceeded},
			{Type: run.StageTypeAcceptance, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStatePending},
		})
		// Artifacts wired but no plan artifact seeded -> loadApprovedPlan returns nil.
		arts := &fakeArtifacts{byStage: map[uuid.UUID][]*artifact.Artifact{}}
		o := &Orchestrator{Runs: rs, GitHub: &stubGitHub{}, Audit: ra, Artifacts: arts}
		sc, liveReq, err := o.TryShortCircuitAcceptance(context.Background(), r.ID, stages[3].ID)
		if err != nil {
			t.Fatalf("TryShortCircuitAcceptance: %v", err)
		}
		if sc != nil {
			t.Errorf("short-circuit = %+v, want nil (nil plan)", sc)
		}
		if liveReq {
			t.Error("liveValidationRequired = true, want false when the approved plan could not be loaded")
		}
		if stages[3].State != run.StageStatePending {
			t.Errorf("acceptance stage = %q, want pending (untouched)", stages[3].State)
		}
	})
}

// TestTryShortCircuitAcceptance_AdmissionLock pins the per-stage admission fence
// (#1936): the short-circuit walk serializes behind LockStageAdmission, and the
// admissibility read happens UNDER the lock so a marker that wins the CAS while
// the admission is blocked is observed and no-ops.
func TestTryShortCircuitAcceptance_AdmissionLock(t *testing.T) {
	allSkip := []map[string]any{
		{"id": "webhook-fires", "statement": "webhook fires on close", "source": "inferred", "rationale": "external", "skip_expected": true, "expectation_basis": "validated in webhook_integration_test.go with a fake"},
		{"id": "issue-closes", "statement": "issue auto-closes", "source": "inferred", "rationale": "external", "skip_expected": true, "expectation_basis": "validated in closer_e2e_test.go"},
	}

	// (i) lock-blocking: while a test-held LockStageAdmission for the target stage
	// is outstanding, TryShortCircuitAcceptance blocks; it proceeds (and short-
	// circuits) only after the lock is released.
	t.Run("blocks while target-stage admission lock is held", func(t *testing.T) {
		planBytes := acceptanceSkipPlanBytes(t, nil, allSkip)
		r, stages, _, _, o := seedAcceptanceSkipRun(t, planBytes)

		unlock := o.LockStageAdmission(stages[3].ID)

		type res struct {
			sc  *AcceptanceShortCircuit
			err error
		}
		done := make(chan res, 1)
		go func() {
			sc, _, err := o.TryShortCircuitAcceptance(context.Background(), r.ID, stages[3].ID)
			done <- res{sc, err}
		}()

		// The admission MUST NOT complete while the lock is held: it acquires the
		// same per-stage mutex before its admissibility reads.
		select {
		case got := <-done:
			t.Fatalf("TryShortCircuitAcceptance returned (sc=%+v err=%v) while the admission lock was held; it did not serialize behind the lock", got.sc, got.err)
		case <-time.After(timescale.D(150 * time.Millisecond)):
		}

		unlock()

		got := <-done
		if got.err != nil {
			t.Fatalf("TryShortCircuitAcceptance after unlock: %v", got.err)
		}
		if got.sc == nil || got.sc.Kind != AcceptanceShortCircuitAllSkipWithBasis {
			t.Fatalf("short-circuit = %+v, want an all-skip-with-basis hit after the lock released", got.sc)
		}
		if stages[3].State != run.StageStateSucceeded {
			t.Errorf("acceptance stage = %q, want succeeded", stages[3].State)
		}
	})

	// (ii) marker-wins interleaving (failure mode c): hold the lock, CAS the seeded
	// pending stage to 'dispatched' (simulating the host-dispatch marker winning),
	// release, and assert the admission — which was blocked on the lock and so reads
	// the stage UNDER the lock — observes 'dispatched', is non-admissible, and
	// no-ops with NO state change and NO verdict audit. Without the lock the
	// admission would read the still-pending stage and double-drive it; without the
	// narrowed admissible set it would short-circuit the 'dispatched' stage anyway.
	t.Run("marker wins CAS -> under-lock re-read no-ops", func(t *testing.T) {
		planBytes := acceptanceSkipPlanBytes(t, nil, allSkip)
		r, stages, rs, ra, o := seedAcceptanceSkipRun(t, planBytes)

		unlock := o.LockStageAdmission(stages[3].ID)

		type res struct {
			sc      *AcceptanceShortCircuit
			liveReq bool
			err     error
		}
		done := make(chan res, 1)
		go func() {
			sc, liveReq, err := o.TryShortCircuitAcceptance(context.Background(), r.ID, stages[3].ID)
			done <- res{sc, liveReq, err}
		}()

		// Ensure the admission is parked on the lock before the marker wins.
		select {
		case got := <-done:
			t.Fatalf("admission ran without waiting for the lock (sc=%+v err=%v)", got.sc, got.err)
		case <-time.After(timescale.D(150 * time.Millisecond)):
		}

		// Marker wins: the seeded pending stage flips to 'dispatched'.
		forceStageState(rs, stages[3].ID, run.StageStateDispatched)
		unlock()

		got := <-done
		if got.err != nil {
			t.Fatalf("TryShortCircuitAcceptance: %v", got.err)
		}
		if got.sc != nil {
			t.Errorf("short-circuit = %+v, want nil (under-lock re-read observed the marker's 'dispatched')", got.sc)
		}
		if got.liveReq {
			t.Error("liveValidationRequired = true, want false on the non-admissible 'dispatched' re-read")
		}
		if stages[3].State != run.StageStateDispatched {
			t.Errorf("acceptance stage = %q, want dispatched (untouched by the no-op admission)", stages[3].State)
		}
		if n := countAcceptanceCategory(ra, "acceptance_outcome_recorded"); n != 0 {
			t.Errorf("acceptance_outcome_recorded = %d, want 0 (no walk fired)", n)
		}
	})
}

// TestAdvance_ImplementStage_NoAcceptanceEmit pins that a non-acceptance
// dispatch appends no acceptance_dispatched entry (the emit is stage-typed).
func TestAdvance_ImplementStage_NoAcceptanceEmit(t *testing.T) {
	rs := newStubRuns()
	gh := &stubGitHub{}
	ra := &recordingAudit{}
	o := &Orchestrator{Runs: rs, GitHub: gh, Audit: ra}
	r, _ := rs.seed(t, "x/y", int64Ptr(42), []stageSeed{
		{Type: run.StageTypePlan, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStateSucceeded},
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStatePending},
	})
	if _, err := o.Advance(context.Background(), r.ID); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	for _, p := range ra.appended {
		if p.Category == "acceptance_dispatched" {
			t.Errorf("implement dispatch wrongly emitted acceptance_dispatched: %s", p.Payload)
		}
	}
}
