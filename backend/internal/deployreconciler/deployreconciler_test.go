package deployreconciler

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// fakeRepo is the run.Repository surface the reconciler uses (Tick lists,
// reconcileStage reads the run). Embeds BaseFake and overrides the two
// methods the ticker calls.
type fakeRepo struct {
	run.BaseFake
	awaiting []*run.Stage
	awaitErr error
	runs     map[uuid.UUID]*run.Run
	getErr   error
}

func (f *fakeRepo) ListDeployStagesAwaitingDeployment(_ context.Context) ([]*run.Stage, error) {
	if f.awaitErr != nil {
		return nil, f.awaitErr
	}
	return f.awaiting, nil
}

func (f *fakeRepo) GetRun(_ context.Context, id uuid.UUID) (*run.Run, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	r, ok := f.runs[id]
	if !ok {
		return nil, run.ErrNotFound
	}
	return r, nil
}

// fakeAudit serves the deployment_dispatched handle entries.
type fakeAudit struct {
	audit.BaseFake
	entries []*audit.Entry
	err     error
}

func (f *fakeAudit) ListForRunByCategory(_ context.Context, _ uuid.UUID, _ string) ([]*audit.Entry, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.entries, nil
}

// stubPoller returns canned workflow-run states and counts calls. get is
// returned by GetWorkflowRun (handle carried a gha_run_id); resolve is
// returned by ResolveDispatchedRun (handle had no id — re-resolve path).
type stubPoller struct {
	get          *githubclient.WorkflowRun
	getErr       error
	getCalls     int
	resolve      *githubclient.WorkflowRun
	resolveErr   error
	resolveCalls int
}

func (s *stubPoller) GetWorkflowRun(_ context.Context, _ int64, _ githubclient.RepoRef, _ int64) (*githubclient.WorkflowRun, error) {
	s.getCalls++
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.get, nil
}

func (s *stubPoller) ResolveDispatchedRun(_ context.Context, _ int64, _ githubclient.RepoRef, _ string, _ map[string]string, _ time.Time) (*githubclient.WorkflowRun, error) {
	s.resolveCalls++
	if s.resolveErr != nil {
		return nil, s.resolveErr
	}
	return s.resolve, nil
}

// recordingResolver captures the terminal resolve call so a test can assert
// what outcome the reconciler mapped (or that it was never called).
type recordingResolver struct {
	calls   int
	outcome run.DeployOutcome
	gitRef  string
	wr      *githubclient.WorkflowRun
	err     error
}

func (r *recordingResolver) ResolveDeploymentFromPollState(_ context.Context, _, _ uuid.UUID, outcome run.DeployOutcome, gitRef string, wr *githubclient.WorkflowRun) error {
	r.calls++
	r.outcome = outcome
	r.gitRef = gitRef
	r.wr = wr
	return r.err
}

const testInstallID int64 = 42

// dispatchEntry builds a deployment_dispatched audit entry with the given
// payload fields, modeling slice-1's recordDispatchAndPark.
func dispatchEntry(runID, stageID uuid.UUID, payload map[string]any) *audit.Entry {
	raw, _ := json.Marshal(payload)
	return &audit.Entry{RunID: &runID, StageID: &stageID, Category: categoryDeploymentDispatched, Payload: raw}
}

// scenario wires a single parked deploy stage + run + dispatch handle and a
// ticker, returning the resolver so the test asserts the resolve call.
type scenario struct {
	ticker   *Ticker
	resolver *recordingResolver
	poller   *stubPoller
	runID    uuid.UUID
	stageID  uuid.UUID
}

func newScenario(install *int64, handle map[string]any, poller *stubPoller) *scenario {
	runID := uuid.New()
	stageID := uuid.New()
	st := &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeDeploy, State: run.StageStateAwaitingDeployment}
	r := &run.Run{ID: runID, Repo: "octo/repo", InstallationID: install, WorkflowID: "deploy"}
	repo := &fakeRepo{awaiting: []*run.Stage{st}, runs: map[uuid.UUID]*run.Run{runID: r}}
	aud := &fakeAudit{}
	if handle != nil {
		aud.entries = []*audit.Entry{dispatchEntry(runID, stageID, handle)}
	}
	res := &recordingResolver{}
	return &scenario{
		ticker:   &Ticker{Runs: repo, GH: poller, Audit: aud, Resolver: res},
		resolver: res,
		poller:   poller,
		runID:    runID,
		stageID:  stageID,
	}
}

func ghaHandle(runID int64) map[string]any {
	return map[string]any{
		"target":        "github_actions",
		"gha_run_id":    runID,
		"git_ref":       "main",
		"dispatched_at": time.Now().UTC().Format(time.RFC3339),
	}
}

func TestTick_SuccessConclusion_ResolvesSucceeded(t *testing.T) {
	s := newScenario(ptr(testInstallID), ghaHandle(777),
		&stubPoller{get: &githubclient.WorkflowRun{ID: 777, HTMLURL: "https://gh/run/777", Status: "completed", Conclusion: "success"}})
	s.ticker.Tick(context.Background())
	if s.resolver.calls != 1 {
		t.Fatalf("resolver calls = %d, want 1", s.resolver.calls)
	}
	if s.resolver.outcome != run.DeployOutcomeSucceeded {
		t.Errorf("outcome = %q, want succeeded", s.resolver.outcome)
	}
	if s.resolver.gitRef != "main" {
		t.Errorf("gitRef = %q, want main", s.resolver.gitRef)
	}
	if s.resolver.wr == nil || s.resolver.wr.HTMLURL != "https://gh/run/777" {
		t.Errorf("wr not threaded to resolver: %+v", s.resolver.wr)
	}
}

func TestTick_FailureConclusion_ResolvesFailed(t *testing.T) {
	s := newScenario(ptr(testInstallID), ghaHandle(1),
		&stubPoller{get: &githubclient.WorkflowRun{ID: 1, Status: "completed", Conclusion: "failure"}})
	s.ticker.Tick(context.Background())
	if s.resolver.calls != 1 || s.resolver.outcome != run.DeployOutcomeFailed {
		t.Fatalf("calls=%d outcome=%q, want 1/failed", s.resolver.calls, s.resolver.outcome)
	}
}

func TestTick_CancelledConclusion_ResolvesFailed(t *testing.T) {
	s := newScenario(ptr(testInstallID), ghaHandle(1),
		&stubPoller{get: &githubclient.WorkflowRun{ID: 1, Status: "completed", Conclusion: "cancelled"}})
	s.ticker.Tick(context.Background())
	if s.resolver.calls != 1 || s.resolver.outcome != run.DeployOutcomeFailed {
		t.Fatalf("calls=%d outcome=%q, want 1/failed", s.resolver.calls, s.resolver.outcome)
	}
}

func TestTick_NeutralConclusion_ResolvesPartial(t *testing.T) {
	s := newScenario(ptr(testInstallID), ghaHandle(1),
		&stubPoller{get: &githubclient.WorkflowRun{ID: 1, Status: "completed", Conclusion: "neutral"}})
	s.ticker.Tick(context.Background())
	if s.resolver.calls != 1 || s.resolver.outcome != run.DeployOutcomePartial {
		t.Fatalf("calls=%d outcome=%q, want 1/partial", s.resolver.calls, s.resolver.outcome)
	}
}

func TestTick_InProgress_StaysParked(t *testing.T) {
	s := newScenario(ptr(testInstallID), ghaHandle(1),
		&stubPoller{get: &githubclient.WorkflowRun{ID: 1, Status: "in_progress", Conclusion: ""}})
	s.ticker.Tick(context.Background())
	if s.resolver.calls != 0 {
		t.Fatalf("resolver called %d times on in-progress run, want 0", s.resolver.calls)
	}
}

func TestTick_CompletedUnmappedConclusion_StaysParked(t *testing.T) {
	s := newScenario(ptr(testInstallID), ghaHandle(1),
		&stubPoller{get: &githubclient.WorkflowRun{ID: 1, Status: "completed", Conclusion: ""}})
	s.ticker.Tick(context.Background())
	if s.resolver.calls != 0 {
		t.Fatalf("resolver called %d times on unmapped conclusion, want 0", s.resolver.calls)
	}
}

func TestTick_TransientGetError_StaysParkedAndRetries(t *testing.T) {
	poller := &stubPoller{getErr: errors.New("502 bad gateway")}
	s := newScenario(ptr(testInstallID), ghaHandle(1), poller)
	s.ticker.Tick(context.Background())
	if s.resolver.calls != 0 {
		t.Fatalf("resolver called on transient poll error, want 0")
	}
	// A second tick retries (NOT a terminal-fail) — the poll is attempted again.
	s.ticker.Tick(context.Background())
	if poller.getCalls != 2 {
		t.Fatalf("GetWorkflowRun calls = %d across two ticks, want 2 (retried)", poller.getCalls)
	}
}

func TestTick_WebhookTarget_Skipped(t *testing.T) {
	s := newScenario(ptr(testInstallID), map[string]any{"target": "webhook", "url": "https://hook"},
		&stubPoller{})
	s.ticker.Tick(context.Background())
	if s.resolver.calls != 0 {
		t.Fatalf("resolver called for webhook target, want 0 (callback path)")
	}
	if s.poller.getCalls != 0 || s.poller.resolveCalls != 0 {
		t.Fatalf("webhook target should not poll GitHub: get=%d resolve=%d", s.poller.getCalls, s.poller.resolveCalls)
	}
}

func TestTick_NoDispatchHandle_Skipped(t *testing.T) {
	s := newScenario(ptr(testInstallID), nil, &stubPoller{})
	s.ticker.Tick(context.Background())
	if s.resolver.calls != 0 {
		t.Fatalf("resolver called with no dispatch handle, want 0")
	}
}

func TestTick_NoInstallation_Skipped(t *testing.T) {
	s := newScenario(nil, ghaHandle(1),
		&stubPoller{get: &githubclient.WorkflowRun{ID: 1, Status: "completed", Conclusion: "success"}})
	s.ticker.Tick(context.Background())
	if s.resolver.calls != 0 {
		t.Fatalf("resolver called with no installation id, want 0")
	}
}

// Binding condition 1 (#1386): when the handle carries no gha_run_id and the
// re-resolve is AMBIGUOUS (multiple concurrent workflow_dispatch runs with no
// echoed correlation inputs → ResolveDispatchedRun returns nil), NO outcome is
// recorded — the stage stays parked rather than associating a wrong run.
func TestTick_AmbiguousReResolve_RecordsNoOutcome(t *testing.T) {
	poller := &stubPoller{resolve: nil} // (nil, nil) == indeterminate
	handle := map[string]any{"target": "github_actions", "gha_run_id": 0, "git_ref": "main",
		"dispatched_at": time.Now().UTC().Format(time.RFC3339)}
	s := newScenario(ptr(testInstallID), handle, poller)
	s.ticker.Tick(context.Background())
	if s.resolver.calls != 0 {
		t.Fatalf("resolver called on ambiguous correlation, want 0 (no mis-association)")
	}
	if poller.resolveCalls != 1 {
		t.Fatalf("ResolveDispatchedRun calls = %d, want 1 (re-resolve attempted)", poller.resolveCalls)
	}
	if poller.getCalls != 0 {
		t.Fatalf("GetWorkflowRun should not be called when handle has no run id, got %d", poller.getCalls)
	}
}

// The single-match re-resolve fallback: handle has no id, ResolveDispatchedRun
// finds exactly one terminal run, the reconciler resolves from it.
func TestTick_ReResolveSingleMatch_Resolves(t *testing.T) {
	poller := &stubPoller{resolve: &githubclient.WorkflowRun{ID: 9, Status: "completed", Conclusion: "success"}}
	handle := map[string]any{"target": "github_actions", "gha_run_id": 0, "git_ref": "main",
		"dispatched_at": time.Now().UTC().Format(time.RFC3339)}
	s := newScenario(ptr(testInstallID), handle, poller)
	s.ticker.Tick(context.Background())
	if s.resolver.calls != 1 || s.resolver.outcome != run.DeployOutcomeSucceeded {
		t.Fatalf("calls=%d outcome=%q, want 1/succeeded from re-resolve", s.resolver.calls, s.resolver.outcome)
	}
	if poller.getCalls != 0 {
		t.Fatalf("GetWorkflowRun should not be called on the re-resolve path, got %d", poller.getCalls)
	}
}

func TestTick_ReResolveError_StaysParked(t *testing.T) {
	poller := &stubPoller{resolveErr: errors.New("403")}
	handle := map[string]any{"target": "github_actions", "gha_run_id": 0, "git_ref": "main"}
	s := newScenario(ptr(testInstallID), handle, poller)
	s.ticker.Tick(context.Background())
	if s.resolver.calls != 0 {
		t.Fatalf("resolver called on re-resolve error, want 0")
	}
}

func TestTick_ListError_NoResolve(t *testing.T) {
	repo := &fakeRepo{awaitErr: errors.New("db down")}
	res := &recordingResolver{}
	tk := &Ticker{Runs: repo, GH: &stubPoller{}, Audit: &fakeAudit{}, Resolver: res}
	tk.Tick(context.Background())
	if res.calls != 0 {
		t.Fatalf("resolver called after list error, want 0")
	}
}

func TestRun_MissingDeps(t *testing.T) {
	cases := map[string]*Ticker{
		"no runs":     {GH: &stubPoller{}, Audit: &fakeAudit{}, Resolver: &recordingResolver{}},
		"no gh":       {Runs: &fakeRepo{}, Audit: &fakeAudit{}, Resolver: &recordingResolver{}},
		"no audit":    {Runs: &fakeRepo{}, GH: &stubPoller{}, Resolver: &recordingResolver{}},
		"no resolver": {Runs: &fakeRepo{}, GH: &stubPoller{}, Audit: &fakeAudit{}},
	}
	for name, tk := range cases {
		t.Run(name, func(t *testing.T) {
			if err := tk.Run(context.Background()); err == nil {
				t.Fatalf("Run() = nil, want a required-field error")
			}
		})
	}
}

func TestMapConclusion(t *testing.T) {
	cases := []struct {
		conclusion string
		want       run.DeployOutcome
		terminal   bool
	}{
		{"success", run.DeployOutcomeSucceeded, true},
		{"SUCCESS", run.DeployOutcomeSucceeded, true},
		{"failure", run.DeployOutcomeFailed, true},
		{"cancelled", run.DeployOutcomeFailed, true},
		{"timed_out", run.DeployOutcomeFailed, true},
		{"neutral", run.DeployOutcomePartial, true},
		{"", "", false},
		{"weird", "", false},
	}
	for _, c := range cases {
		got, terminal := mapConclusion(c.conclusion)
		if got != c.want || terminal != c.terminal {
			t.Errorf("mapConclusion(%q) = (%q, %v), want (%q, %v)", c.conclusion, got, terminal, c.want, c.terminal)
		}
	}
}

func ptr[T any](v T) *T { return &v }
