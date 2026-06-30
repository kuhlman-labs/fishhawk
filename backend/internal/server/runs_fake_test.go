package server

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// fakeRepo is a minimum in-memory implementation of run.Repository
// for handler tests. The Postgres adapter is exercised separately
// in backend/internal/run/postgres_test.go via testcontainers, so
// this fake only needs to satisfy the contract well enough that
// the handler logic gets coverage.
type fakeRepo struct {
	mu   sync.Mutex
	runs map[uuid.UUID]*run.Run

	// stagesByRun tracks CreateStage calls so #411 tests can assert
	// the API runs handler actually persists stages from the spec.
	// Ordered by spec sequence (the slice index).
	stagesByRun map[uuid.UUID][]*run.Stage

	// createStageErr lets a test fail the create-stage path so the
	// rollback / 500 surface is reachable.
	createStageErr error

	// createErr / getErr / transitionErr / listErr let tests inject
	// failures without instrumenting fakeRepo's internals.
	createErr     error
	getErr        error
	transitionErr error
	listErr       error

	// lastListFilter captures the most-recent ListRuns filter for
	// tests that need to assert filter-forwarding (runner_kind,
	// pull_request_url, etc.).
	lastListFilter run.ListRunsFilter

	// lastCreateRunParams captures the most-recent CreateRun call so
	// tests that need to assert workflow-spec persistence
	// (WorkflowSpec bytes, MaxRetriesSnapshot) can inspect the
	// params the handler built.
	lastCreateRunParams run.CreateRunParams

	// getRunCalls counts GetRun invocations so the calibration cache
	// tests can assert a cache hit skips the filterRuntimeObservedSamples
	// per-entry run-resolve N+1.
	getRunCalls int
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		runs:        map[uuid.UUID]*run.Run{},
		stagesByRun: map[uuid.UUID][]*run.Stage{},
	}
}

func (f *fakeRepo) CreateRun(_ context.Context, p run.CreateRunParams) (*run.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastCreateRunParams = p
	if f.createErr != nil {
		return nil, f.createErr
	}
	now := time.Now().UTC()
	// Mirror the postgres adapter's empty → github_actions default
	// so the fake's behavior tracks production.
	runnerKind := p.RunnerKind
	if runnerKind == "" {
		runnerKind = run.RunnerKindGitHubActions
	}
	r := &run.Run{
		ID:                 uuid.New(),
		Repo:               p.Repo,
		WorkflowID:         p.WorkflowID,
		WorkflowSHA:        p.WorkflowSHA,
		TriggerSource:      p.TriggerSource,
		TriggerRef:         p.TriggerRef,
		InstallationID:     p.InstallationID,
		IdempotencyKey:     p.IdempotencyKey,
		RunnerKind:         runnerKind,
		WorkflowSpec:       p.WorkflowSpec,
		MaxRetriesSnapshot: p.MaxRetriesSnapshot,
		IssueContext:       p.IssueContext,
		UpstreamRunID:      p.UpstreamRunID,
		State:              run.StatePending,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	f.runs[r.ID] = r
	return r, nil
}

func (f *fakeRepo) GetRun(_ context.Context, id uuid.UUID) (*run.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getRunCalls++
	if f.getErr != nil {
		return nil, f.getErr
	}
	r, ok := f.runs[id]
	if !ok {
		return nil, run.ErrNotFound
	}
	return r, nil
}

// transitionErr lets tests inject specific errors out of TransitionRun
// so the cancel handler's branches (404, 409, 500) are reachable.
// listErr does the same for ListRuns.
//
// The remaining stage methods aren't exercised by these handler tests
// but must exist so fakeRepo satisfies run.Repository.

func (f *fakeRepo) GetRunByIdempotencyKey(_ context.Context, repo, key string) (*run.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.runs {
		if r.Repo == repo && r.IdempotencyKey != nil && *r.IdempotencyKey == key {
			return r, nil
		}
	}
	return nil, run.ErrNotFound
}

func (f *fakeRepo) TransitionRun(_ context.Context, id uuid.UUID, to run.State) (*run.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.transitionErr != nil {
		return nil, f.transitionErr
	}
	r, ok := f.runs[id]
	if !ok {
		return nil, run.ErrNotFound
	}
	// Match the postgres adapter's transition rules: same-state is
	// idempotent, terminal states reject further transitions.
	if r.State == to {
		return r, nil
	}
	if r.State == run.StateSucceeded || r.State == run.StateFailed || r.State == run.StateCancelled {
		return nil, run.InvalidTransitionError{Kind: "run", From: string(r.State), To: string(to)}
	}
	r.State = to
	r.UpdatedAt = time.Now().UTC()
	return r, nil
}

// RetryRun mirrors postgresRepo's run-level reopen override: only the
// failed → running transition in the runRetryTransitions table is
// permitted (#698).
func (f *fakeRepo) RetryRun(_ context.Context, id uuid.UUID, to run.State) (*run.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.runs[id]
	if !ok {
		return nil, run.ErrNotFound
	}
	if !run.ValidRunRetryTransition(r.State, to) {
		return nil, run.InvalidTransitionError{Kind: "run", From: string(r.State), To: string(to)}
	}
	r.State = to
	r.UpdatedAt = time.Now().UTC()
	return r, nil
}

func (f *fakeRepo) ListRuns(_ context.Context, fil run.ListRunsFilter) ([]*run.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastListFilter = fil
	if f.listErr != nil {
		return nil, f.listErr
	}
	var matched []*run.Run
	for _, r := range f.runs {
		if fil.Repo != "" && r.Repo != fil.Repo {
			continue
		}
		if fil.WorkflowID != "" && r.WorkflowID != fil.WorkflowID {
			continue
		}
		if fil.State != "" && string(r.State) != fil.State {
			continue
		}
		if fil.PullRequestURL != nil {
			if r.PullRequestURL == nil || *r.PullRequestURL != *fil.PullRequestURL {
				continue
			}
		}
		if fil.TriggerRef != nil {
			if r.TriggerRef == nil || *r.TriggerRef != *fil.TriggerRef {
				continue
			}
		}
		matched = append(matched, r)
	}
	// Order matches the SQL: created_at DESC, id DESC.
	sort.Slice(matched, func(i, j int) bool {
		if !matched[i].CreatedAt.Equal(matched[j].CreatedAt) {
			return matched[i].CreatedAt.After(matched[j].CreatedAt)
		}
		return matched[i].ID.String() > matched[j].ID.String()
	})
	if fil.Offset >= len(matched) {
		return nil, nil
	}
	end := fil.Offset + fil.Limit
	if end > len(matched) {
		end = len(matched)
	}
	return matched[fil.Offset:end], nil
}

func (f *fakeRepo) SetRunPullRequestURL(_ context.Context, id uuid.UUID, url string) (*run.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.runs[id]
	if !ok {
		return nil, run.ErrNotFound
	}
	u := url
	r.PullRequestURL = &u
	r.UpdatedAt = time.Now().UTC()
	return r, nil
}

func (f *fakeRepo) CreateStage(_ context.Context, p run.CreateStageParams) (*run.Stage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createStageErr != nil {
		return nil, f.createStageErr
	}
	now := time.Now().UTC()
	stage := &run.Stage{
		ID:               uuid.New(),
		RunID:            p.RunID,
		Sequence:         p.Sequence,
		Type:             p.Type,
		ExecutorKind:     p.ExecutorKind,
		ExecutorRef:      p.ExecutorRef,
		State:            run.StageStatePending,
		GateSLA:          p.GateSLA,
		RequiresApproval: p.RequiresApproval,
		Gate:             p.Gate,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	f.stagesByRun[p.RunID] = append(f.stagesByRun[p.RunID], stage)
	return stage, nil
}

// stagesFor returns the stage slice the handler created for the
// given run, in spec order. Tests use this to assert one Stage row
// per stage definition lands when workflow_spec is present.
func (f *fakeRepo) stagesFor(runID uuid.UUID) []*run.Stage {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*run.Stage, len(f.stagesByRun[runID]))
	copy(out, f.stagesByRun[runID])
	return out
}

func (f *fakeRepo) GetStage(_ context.Context, id uuid.UUID) (*run.Stage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, stages := range f.stagesByRun {
		for _, st := range stages {
			if st.ID == id {
				return st, nil
			}
		}
	}
	return nil, run.ErrNotFound
}
func (f *fakeRepo) ListStagesForRun(_ context.Context, _ uuid.UUID) ([]*run.Stage, error) {
	return nil, errors.New("fakeRepo: ListStagesForRun not implemented")
}
func (f *fakeRepo) ListStagesAwaitingApproval(context.Context) ([]*run.Stage, error) {
	return nil, errors.New("fakeRepo: ListStagesAwaitingApproval not implemented")
}
func (f *fakeRepo) ListReviewStagesAwaitingApproval(context.Context) ([]*run.Stage, error) {
	return nil, errors.New("fakeRepo: ListStagesAwaitingApproval not implemented")
}

func (f *fakeRepo) ListStagesAwaitingChildren(context.Context) ([]*run.Stage, error) {
	return nil, errors.New("fakeRepo: ListStagesAwaitingApproval not implemented")
}

func (f *fakeRepo) ListStagesDispatched(context.Context) ([]*run.Stage, error) {
	return nil, nil
}

func (f *fakeRepo) RetryStage(context.Context, uuid.UUID, run.StageState) (*run.Stage, error) {
	return nil, errors.New("not used")
}
func (f *fakeRepo) TransitionStage(_ context.Context, _ uuid.UUID, _ run.StageState, _ *run.StageCompletion) (*run.Stage, error) {
	return nil, errors.New("fakeRepo: TransitionStage not implemented")
}

// newServer builds a Server wired to repo, with the Handler() exposed
// so tests can call it via httptest.NewRecorder.
func newServer(t *testing.T, repo run.Repository) *Server {
	t.Helper()
	return New(Config{Addr: "127.0.0.1:0", RunRepo: repo})
}

// testOperatorIdentity returns an Identity that represents a
// cookie-session operator (no TokenID, no Scopes). The requireWriteScope
// guard lets cookie-session callers through unconditionally, so this
// is the correct fixture for tests that exercise handler logic past
// the auth gate.
func testOperatorIdentity() Identity {
	return Identity{
		Subject:   "github:test-operator",
		UserID:    "00000000-0000-0000-0000-000000000001",
		SessionID: "00000000-0000-0000-0000-000000000002",
	}
}

// withAuth injects a session-user identity into req's context so
// the scope guard passes when calling handlers directly (not via
// s.Handler(), which would overwrite the identity via bearerAuth).
func withAuth(req *http.Request) *http.Request {
	return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, testOperatorIdentity()))
}
