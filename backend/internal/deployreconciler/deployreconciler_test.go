package deployreconciler

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// fakeRepo is the run.Repository surface the reconciler uses (Tick lists,
// reconcileStage reads the run). Embeds BaseFake and overrides the two
// methods the ticker calls.
type fakeRepo struct {
	run.BaseFake
	awaiting        []*run.Stage
	awaitErr        error
	rollbackPending []*run.Stage
	rollbackErr     error
	runs            map[uuid.UUID]*run.Run
	getErr          error
}

func (f *fakeRepo) ListDeployStagesAwaitingDeployment(_ context.Context) ([]*run.Stage, error) {
	if f.awaitErr != nil {
		return nil, f.awaitErr
	}
	return f.awaiting, nil
}

func (f *fakeRepo) ListDeployStagesRollbackPending(_ context.Context) ([]*run.Stage, error) {
	if f.rollbackErr != nil {
		return nil, f.rollbackErr
	}
	return f.rollbackPending, nil
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

// fakeAudit serves handle entries, filtered by the queried category so the
// forward scan (deployment_dispatched) and the rollback scan
// (deployment_rollback_initiated) read their own handles. entries holds the
// forward handle for back-compat with the existing scenarios; byCategory, when
// set, overrides per-category lookups.
type fakeAudit struct {
	audit.BaseFake
	entries    []*audit.Entry
	byCategory map[string][]*audit.Entry
	err        error
}

func (f *fakeAudit) ListForRunByCategory(_ context.Context, _ uuid.UUID, category string) ([]*audit.Entry, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.byCategory != nil {
		return f.byCategory[category], nil
	}
	// Back-compat: the forward scenarios seed `entries` as deployment_dispatched
	// handles. Only return them for that category so a rollback-category query
	// against a forward fixture reads empty.
	if category == categoryDeploymentDispatched {
		return f.entries, nil
	}
	return nil, nil
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
	// lastCorrelation captures the correlation map the most recent
	// ResolveDispatchedRun call received, so a test can assert the rollback
	// scan supplies the fishhawk_rollback marker.
	lastCorrelation map[string]string
}

func (s *stubPoller) GetWorkflowRunScoped(_ context.Context, _ forge.CredentialScope, _ githubclient.RepoRef, _ int64) (*githubclient.WorkflowRun, error) {
	s.getCalls++
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.get, nil
}

func (s *stubPoller) ResolveDispatchedRunScoped(_ context.Context, _ forge.CredentialScope, _ githubclient.RepoRef, _ string, correlation map[string]string, _ time.Time) (*githubclient.WorkflowRun, error) {
	s.resolveCalls++
	s.lastCorrelation = correlation
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

	rollbackCalls  int
	rollbackGitRef string
	rollbackWR     *githubclient.WorkflowRun
	rollbackErr    error
}

func (r *recordingResolver) ResolveDeploymentFromPollState(_ context.Context, _, _ uuid.UUID, outcome run.DeployOutcome, gitRef string, wr *githubclient.WorkflowRun) error {
	r.calls++
	r.outcome = outcome
	r.gitRef = gitRef
	r.wr = wr
	return r.err
}

func (r *recordingResolver) ResolveDeploymentRollbackFromPollState(_ context.Context, _, _ uuid.UUID, gitRef string, wr *githubclient.WorkflowRun) error {
	r.rollbackCalls++
	r.rollbackGitRef = gitRef
	r.rollbackWR = wr
	return r.rollbackErr
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

// rollbackEntry builds a deployment_rollback_initiated audit entry, modeling
// slice-1's handleRollbackDeployment handle.
func rollbackEntry(runID, stageID uuid.UUID, payload map[string]any) *audit.Entry {
	raw, _ := json.Marshal(payload)
	return &audit.Entry{RunID: &runID, StageID: &stageID, Category: categoryDeploymentRollbackInitiated, Payload: raw}
}

// newRollbackScenario wires a single ALREADY-TERMINAL deploy stage (succeeded)
// carrying a pending rollback handle (deployment_rollback_initiated with no
// completed). The stage is listed by the rollback scan, not the forward
// awaiting_deployment scan.
func newRollbackScenario(install *int64, handle map[string]any, poller *stubPoller) *scenario {
	runID := uuid.New()
	stageID := uuid.New()
	st := &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeDeploy, State: run.StageStateSucceeded}
	r := &run.Run{ID: runID, Repo: "octo/repo", InstallationID: install, WorkflowID: "deploy"}
	repo := &fakeRepo{rollbackPending: []*run.Stage{st}, runs: map[uuid.UUID]*run.Run{runID: r}}
	aud := &fakeAudit{byCategory: map[string][]*audit.Entry{}}
	if handle != nil {
		aud.byCategory[categoryDeploymentRollbackInitiated] = []*audit.Entry{rollbackEntry(runID, stageID, handle)}
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

// rollbackHandle builds a github_actions rollback handle payload.
func rollbackHandle(ghaRunID int64) map[string]any {
	return map[string]any{
		"target":        "github_actions",
		"gha_run_id":    ghaRunID,
		"git_ref":       "release",
		"rollback":      true,
		"dispatched_at": time.Now().UTC().Format(time.RFC3339),
	}
}

// --- rollback scan (#1398): poll a github_actions rollback handle to terminal ---

// HAPPY: a rollback handle with a gha_run_id whose run completed terminal drives
// exactly one rollback resolve carrying the handle git ref + the polled run.
func TestTick_Rollback_TerminalSuccess_RecordsRolledBack(t *testing.T) {
	s := newRollbackScenario(ptr(testInstallID), rollbackHandle(888),
		&stubPoller{get: &githubclient.WorkflowRun{ID: 888, HTMLURL: "https://gh/run/888", Status: "completed", Conclusion: "success"}})
	s.ticker.Tick(context.Background())
	if s.resolver.rollbackCalls != 1 {
		t.Fatalf("rollback resolver calls = %d, want 1", s.resolver.rollbackCalls)
	}
	if s.resolver.rollbackGitRef != "release" {
		t.Errorf("rollback gitRef = %q, want release", s.resolver.rollbackGitRef)
	}
	if s.resolver.rollbackWR == nil || s.resolver.rollbackWR.HTMLURL != "https://gh/run/888" {
		t.Errorf("rollback wr not threaded: %+v", s.resolver.rollbackWR)
	}
	// The forward resolve must NOT fire for a rollback-only stage.
	if s.resolver.calls != 0 {
		t.Errorf("forward resolver calls = %d, want 0 on a rollback stage", s.resolver.calls)
	}
}

// ANY terminal conclusion (here a failed rollback run) still records rolled_back,
// mirroring the webhook callback.
func TestTick_Rollback_TerminalFailure_StillRecordsRolledBack(t *testing.T) {
	s := newRollbackScenario(ptr(testInstallID), rollbackHandle(1),
		&stubPoller{get: &githubclient.WorkflowRun{ID: 1, Status: "completed", Conclusion: "failure"}})
	s.ticker.Tick(context.Background())
	if s.resolver.rollbackCalls != 1 {
		t.Fatalf("rollback resolver calls = %d, want 1 (any terminal conclusion records rolled_back)", s.resolver.rollbackCalls)
	}
}

// CORRELATION MARKER: when the handle has no gha_run_id, the re-resolve MUST
// carry fishhawk_rollback="true" so it never matches the forward deploy run.
func TestTick_Rollback_ReResolveCarriesRollbackMarker(t *testing.T) {
	poller := &stubPoller{resolve: &githubclient.WorkflowRun{ID: 9, Status: "completed", Conclusion: "success"}}
	handle := map[string]any{"target": "github_actions", "gha_run_id": 0, "git_ref": "release",
		"dispatched_at": time.Now().UTC().Format(time.RFC3339)}
	s := newRollbackScenario(ptr(testInstallID), handle, poller)
	s.ticker.Tick(context.Background())
	if poller.resolveCalls != 1 {
		t.Fatalf("ResolveDispatchedRun calls = %d, want 1", poller.resolveCalls)
	}
	if got := poller.lastCorrelation[rollbackCorrelationMarker]; got != "true" {
		t.Errorf("re-resolve correlation[%q] = %q, want \"true\"", rollbackCorrelationMarker, got)
	}
	if s.resolver.rollbackCalls != 1 {
		t.Errorf("rollback resolver calls = %d, want 1 from re-resolve", s.resolver.rollbackCalls)
	}
}

// AMBIGUOUS CORRELATION: gha_run_id=0 and ResolveDispatchedRun returns (nil,nil)
// → no resolve, stage untouched (no mis-association of the forward run).
func TestTick_Rollback_AmbiguousReResolve_RecordsNothing(t *testing.T) {
	poller := &stubPoller{resolve: nil}
	handle := map[string]any{"target": "github_actions", "gha_run_id": 0, "git_ref": "release",
		"dispatched_at": time.Now().UTC().Format(time.RFC3339)}
	s := newRollbackScenario(ptr(testInstallID), handle, poller)
	s.ticker.Tick(context.Background())
	if s.resolver.rollbackCalls != 0 {
		t.Fatalf("rollback resolver called on ambiguous correlation, want 0")
	}
	if poller.resolveCalls != 1 {
		t.Fatalf("ResolveDispatchedRun calls = %d, want 1 (re-resolve attempted)", poller.resolveCalls)
	}
	if poller.getCalls != 0 {
		t.Fatalf("GetWorkflowRun called with no run id, got %d", poller.getCalls)
	}
}

// WEBHOOK TARGET: a webhook rollback reports terminal via the callback, not the
// reconciler — no poll, no resolve.
func TestTick_Rollback_WebhookTarget_Skipped(t *testing.T) {
	s := newRollbackScenario(ptr(testInstallID), map[string]any{"target": "webhook", "url": "https://hook"},
		&stubPoller{})
	s.ticker.Tick(context.Background())
	if s.resolver.rollbackCalls != 0 {
		t.Fatalf("rollback resolver called for webhook target, want 0 (callback path)")
	}
	if s.poller.getCalls != 0 || s.poller.resolveCalls != 0 {
		t.Fatalf("webhook rollback should not poll GitHub: get=%d resolve=%d", s.poller.getCalls, s.poller.resolveCalls)
	}
}

// IN-FLIGHT: the rollback run is still in_progress → no resolve, left pending.
func TestTick_Rollback_InProgress_StaysPending(t *testing.T) {
	s := newRollbackScenario(ptr(testInstallID), rollbackHandle(1),
		&stubPoller{get: &githubclient.WorkflowRun{ID: 1, Status: "in_progress", Conclusion: ""}})
	s.ticker.Tick(context.Background())
	if s.resolver.rollbackCalls != 0 {
		t.Fatalf("rollback resolver called on in-progress run, want 0")
	}
}

// UNMAPPED CONCLUSION: completed but with an empty/unknown conclusion → no
// resolve, left pending (no guessed disposition).
func TestTick_Rollback_CompletedUnmappedConclusion_StaysPending(t *testing.T) {
	s := newRollbackScenario(ptr(testInstallID), rollbackHandle(1),
		&stubPoller{get: &githubclient.WorkflowRun{ID: 1, Status: "completed", Conclusion: ""}})
	s.ticker.Tick(context.Background())
	if s.resolver.rollbackCalls != 0 {
		t.Fatalf("rollback resolver called on unmapped conclusion, want 0")
	}
}

// NO INSTALLATION: no installation id → no creds to poll with, left pending.
func TestTick_Rollback_NoInstallation_Skipped(t *testing.T) {
	s := newRollbackScenario(nil, rollbackHandle(1),
		&stubPoller{get: &githubclient.WorkflowRun{ID: 1, Status: "completed", Conclusion: "success"}})
	s.ticker.Tick(context.Background())
	if s.resolver.rollbackCalls != 0 {
		t.Fatalf("rollback resolver called with no installation id, want 0")
	}
}

// LIST ERROR on the rollback scan does not prevent (or crash) the forward scan,
// and records nothing.
func TestTick_Rollback_ListError_NoResolve(t *testing.T) {
	repo := &fakeRepo{rollbackErr: errors.New("db down")}
	res := &recordingResolver{}
	tk := &Ticker{Runs: repo, GH: &stubPoller{}, Audit: &fakeAudit{}, Resolver: res}
	tk.Tick(context.Background())
	if res.rollbackCalls != 0 {
		t.Fatalf("rollback resolver called after list error, want 0")
	}
}

// TRANSIENT POLL ERROR: a GetWorkflowRun error leaves the rollback pending and
// is retried next tick (never a terminal record).
func TestTick_Rollback_TransientGetError_StaysPendingAndRetries(t *testing.T) {
	poller := &stubPoller{getErr: errors.New("502 bad gateway")}
	s := newRollbackScenario(ptr(testInstallID), rollbackHandle(1), poller)
	s.ticker.Tick(context.Background())
	if s.resolver.rollbackCalls != 0 {
		t.Fatalf("rollback resolver called on transient poll error, want 0")
	}
	s.ticker.Tick(context.Background())
	if poller.getCalls != 2 {
		t.Fatalf("GetWorkflowRun calls = %d across two ticks, want 2 (retried)", poller.getCalls)
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
