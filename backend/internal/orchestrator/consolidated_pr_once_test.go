package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// copyRuns is a race-safe in-memory run repository for the once-only
// consolidated-PR open tests (#4110). Unlike stubRuns, which hands out its
// stored pointers, every read returns a COPY taken under the mutex, so two
// concurrent Advance calls never share a *run.Run / *run.Stage with the fake —
// -race then flags the ORCHESTRATOR, not the fake (the #3226/#2586 fake-race
// trap). Fake mutations replace pointer fields (never write through them), so
// a shallow copy cannot alias a later write.
//
// TransitionStage delegates admissibility to run.ValidStageTransition exactly
// as stubRuns does, INCLUDING same-state moves: the losing Advance's
// dispatched -> dispatched re-dispatch of the agent review is a valid no-op,
// so both concurrent Advance calls return nil.
//
// It deliberately does NOT implement run.ConsolidatedPROpenLocker, so the
// orchestrator's in-process mutex is the only guard on this path.
type copyRuns struct {
	run.BaseFake

	mu     sync.Mutex
	runs   map[uuid.UUID]*run.Run
	stages map[uuid.UUID][]*run.Stage
}

func newCopyRuns() *copyRuns {
	return &copyRuns{runs: map[uuid.UUID]*run.Run{}, stages: map[uuid.UUID][]*run.Stage{}}
}

// seedDecomposedParent seeds a decomposed parent (implement succeeded, review
// pending with an AGENT executor, installation id set) plus one succeeded
// child, returning the parent id and its review stage id.
func (c *copyRuns) seedDecomposedParent() (parentID, reviewStageID uuid.UUID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now().UTC()
	inst := int64(55)
	parent := &run.Run{
		ID: uuid.New(), Repo: "kuhlman-labs/fishhawk", WorkflowID: "feature_change",
		WorkflowSHA: "sha", TriggerSource: run.TriggerGitHubIssue, InstallationID: &inst,
		State: run.StateRunning, CreatedAt: now, UpdatedAt: now,
	}
	impl := &run.Stage{ID: uuid.New(), RunID: parent.ID, Sequence: 0, Type: run.StageTypeImplement,
		ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStateSucceeded, CreatedAt: now, UpdatedAt: now}
	review := &run.Stage{ID: uuid.New(), RunID: parent.ID, Sequence: 1, Type: run.StageTypeReview,
		ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStatePending, CreatedAt: now, UpdatedAt: now}
	child := &run.Run{
		ID: uuid.New(), Repo: parent.Repo, WorkflowID: "feature_change", WorkflowSHA: "sha",
		TriggerSource: run.TriggerGitHubIssue, InstallationID: &inst, DecomposedFrom: &parent.ID,
		State: run.StateSucceeded, CreatedAt: now, UpdatedAt: now,
	}
	c.runs[parent.ID] = parent
	c.runs[child.ID] = child
	c.stages[parent.ID] = []*run.Stage{impl, review}
	return parent.ID, review.ID
}

func (c *copyRuns) GetRun(_ context.Context, id uuid.UUID) (*run.Run, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.runs[id]
	if !ok {
		return nil, run.ErrNotFound
	}
	cp := *r
	return &cp, nil
}

func (c *copyRuns) ListStagesForRun(_ context.Context, runID uuid.UUID) ([]*run.Stage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*run.Stage, 0, len(c.stages[runID]))
	for _, s := range c.stages[runID] {
		cp := *s
		out = append(out, &cp)
	}
	return out, nil
}

// ListRuns serves only the DecomposedFrom filter (the consolidated-PR path),
// honoring Offset/Limit for listAllDecomposedChildren's pagination walk.
func (c *copyRuns) ListRuns(_ context.Context, f run.ListRunsFilter) ([]*run.Run, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if f.DecomposedFrom == nil {
		return nil, errors.New("copyRuns: ListRuns without DecomposedFrom not used")
	}
	var out []*run.Run
	for _, r := range c.runs {
		if r.DecomposedFrom != nil && *r.DecomposedFrom == *f.DecomposedFrom {
			cp := *r
			out = append(out, &cp)
		}
	}
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

func (c *copyRuns) TransitionStage(_ context.Context, id uuid.UUID, to run.StageState, _ *run.StageCompletion) (*run.Stage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, list := range c.stages {
		for _, s := range list {
			if s.ID != id {
				continue
			}
			if !run.ValidStageTransition(s.State, to) {
				return nil, run.InvalidTransitionError{Kind: "stage", From: string(s.State), To: string(to)}
			}
			s.State = to
			cp := *s
			return &cp, nil
		}
	}
	return nil, run.ErrNotFound
}

func (c *copyRuns) SetRunPullRequestURL(_ context.Context, id uuid.UUID, url string) (*run.Run, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.runs[id]
	if !ok {
		return nil, run.ErrNotFound
	}
	u := url
	r.PullRequestURL = &u
	cp := *r
	return &cp, nil
}

// pullRequestURL reads the stored pull_request_url ("" when unset).
func (c *copyRuns) pullRequestURL(id uuid.UUID) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r := c.runs[id]; r != nil && r.PullRequestURL != nil {
		return *r.PullRequestURL
	}
	return ""
}

// lockErrRuns is a copyRuns that ALSO implements run.ConsolidatedPROpenLocker
// and fails every acquisition WITHOUT calling fn — the DB-lock error path.
type lockErrRuns struct {
	*copyRuns
	err error
}

func (l *lockErrRuns) WithConsolidatedPROpenLock(context.Context, uuid.UUID, func(context.Context) error) error {
	return l.err
}

// gatedCreateGitHub parks every CreatePullRequest caller until release is
// closed (or its ctx ends), counting the calls and signalling each entry on
// entered. Its ListOpenPullRequestsByHead always returns EMPTY — GitHub's list
// lag, the incident's condition — so the pre-create adopt can never stand in
// for the lock under a counterfactual. Everything else (DispatchWorkflow, ...)
// delegates to the embedded stubGitHub.
type gatedCreateGitHub struct {
	*stubGitHub

	creates atomic.Int32
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newGatedCreateGitHub() *gatedCreateGitHub {
	return &gatedCreateGitHub{
		stubGitHub: &stubGitHub{},
		entered:    make(chan struct{}, 8),
		release:    make(chan struct{}),
	}
}

func (g *gatedCreateGitHub) releaseAll() { g.once.Do(func() { close(g.release) }) }

// gatedCreatedURL is the html_url of the n-th (1-based) gated create.
func gatedCreatedURL(n int32) string {
	return fmt.Sprintf("https://github.com/kuhlman-labs/fishhawk/pull/%d", 4107+n)
}

func (g *gatedCreateGitHub) CreatePullRequest(ctx context.Context, _ forge.CredentialScope,
	_ forge.RepoRef, _, _, _, _ string) (*forge.PullRequest, error) {
	n := g.creates.Add(1)
	g.entered <- struct{}{}
	select {
	case <-g.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &forge.PullRequest{Number: int(4107 + n), HTMLURL: gatedCreatedURL(n), State: "open"}, nil
}

func (g *gatedCreateGitHub) ListOpenPullRequestsByHead(context.Context, forge.CredentialScope,
	forge.RepoRef, string, string) ([]forge.PullRequest, error) {
	return nil, nil
}

// raceTwoOpens starts two concurrent calls of open, waits for the first to
// enter the gated CreatePullRequest, gives a second caller a scaled window to
// enter too (it can only do so without a working lock), then releases and
// joins both, returning their errors.
func raceTwoOpens(t *testing.T, gh *gatedCreateGitHub, open func() error) [2]error {
	t.Helper()
	t.Cleanup(gh.releaseAll)
	var errs [2]error
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = open()
		}(i)
	}
	select {
	case <-gh.entered:
	case <-time.After(timescale.D(5 * time.Second)):
		gh.releaseAll()
		wg.Wait()
		t.Fatal("no caller reached CreatePullRequest")
	}
	select {
	case <-gh.entered:
		// A second caller is inside CreatePullRequest while the first is still
		// parked there: the lock did not hold. The count assertion reports it.
	case <-time.After(timescale.D(300 * time.Millisecond)):
	}
	gh.releaseAll()
	wg.Wait()
	return errs
}

// derefURL renders a stored pull_request_url ("" when nil).
func derefURL(u *string) string {
	if u == nil {
		return ""
	}
	return *u
}

// countAuditCategory counts the recordingAudit entries for a category.
func countAuditCategory(a *recordingAudit, category string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, p := range a.appended {
		if p.Category == category {
			n++
		}
	}
	return n
}

// TestConsolidatedPROpen_ConcurrentAdvance_OpensOnce is the #4110 done-means:
// two concurrent Advance calls on one decomposed parent at its review gate
// yield exactly one forge CreatePullRequest and one consolidated_pr_opened
// entry, both calls succeed, and the stored URL is the one created PR.
// Fixture mechanism: the gated create parks the first caller INSIDE
// CreatePullRequest (URL not yet stamped) while the list-by-head returns empty,
// so without the in-process lock the second caller passes the (b) re-read and
// creates inside the window (count 2). copyRuns has no locker capability, so
// the in-process mutex is the only guard.
func TestConsolidatedPROpen_ConcurrentAdvance_OpensOnce(t *testing.T) {
	rs := newCopyRuns()
	gh := newGatedCreateGitHub()
	au := &recordingAudit{}
	o := &Orchestrator{Runs: rs, GitHub: gh, Audit: au, DefaultRef: "main"}
	parentID, _ := rs.seedDecomposedParent()

	errs := raceTwoOpens(t, gh, func() error {
		_, err := o.Advance(context.Background(), parentID)
		return err
	})

	if got := gh.creates.Load(); got != 1 {
		t.Errorf("CreatePullRequest calls = %d, want 1 (the consolidated PR must open once)", got)
	}
	if got := countAuditCategory(au, "consolidated_pr_opened"); got != 1 {
		t.Errorf("consolidated_pr_opened entries = %d, want 1", got)
	}
	for i, err := range errs {
		if err != nil {
			t.Errorf("Advance #%d: %v", i, err)
		}
	}
	if got := rs.pullRequestURL(parentID); got != gatedCreatedURL(1) {
		t.Errorf("stored pull_request_url = %q, want the one created PR %q", got, gatedCreatedURL(1))
	}
}

// TestConsolidatedPROpen_TwoOrchestratorsSharedPostgres_OpensOnce is the
// cross-process case (#4110): two Orchestrator instances — distinct in-process
// lock maps, i.e. two replicas — over ONE real run.NewPostgresRepository (the
// constructor serve.go wires) race maybeOpenConsolidatedPR for the same parent.
// Fixture mechanism: the two in-process mutexes are distinct, so ONLY the
// Postgres advisory lock can serialize them; neutering it (or the
// orchestrator's capability probe) lets both create inside the window.
func TestConsolidatedPROpen_TwoOrchestratorsSharedPostgres_OpensOnce(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	runs := run.NewPostgresRepository(pool)
	inst := int64(55)

	parent, err := runs.CreateRun(ctx, run.CreateRunParams{
		Repo: "kuhlman-labs/fishhawk", WorkflowID: "feature_change", WorkflowSHA: "sha",
		TriggerSource: run.TriggerGitHubIssue, InstallationID: &inst,
	})
	if err != nil {
		t.Fatalf("create parent run: %v", err)
	}
	if _, err := runs.CreateStage(ctx, run.CreateStageParams{
		RunID: parent.ID, Sequence: 0, Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
	}); err != nil {
		t.Fatalf("create implement stage: %v", err)
	}
	review, err := runs.CreateStage(ctx, run.CreateStageParams{
		RunID: parent.ID, Sequence: 1, Type: run.StageTypeReview, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
	})
	if err != nil {
		t.Fatalf("create review stage: %v", err)
	}
	if _, err := runs.CreateRun(ctx, run.CreateRunParams{
		Repo: "kuhlman-labs/fishhawk", WorkflowID: "feature_change", WorkflowSHA: "sha",
		TriggerSource: run.TriggerGitHubIssue, InstallationID: &inst, DecomposedFrom: &parent.ID,
	}); err != nil {
		t.Fatalf("create decomposed child: %v", err)
	}

	gh := newGatedCreateGitHub()
	au := &recordingAudit{}
	replicas := [2]*Orchestrator{
		{Runs: runs, GitHub: gh, Audit: au, DefaultRef: "main"},
		{Runs: runs, GitHub: gh, Audit: au, DefaultRef: "main"},
	}
	var next atomic.Int32
	errs := raceTwoOpens(t, gh, func() error {
		o := replicas[next.Add(1)-1]
		_, err := o.maybeOpenConsolidatedPR(ctx, parent, review)
		return err
	})

	if got := gh.creates.Load(); got != 1 {
		t.Errorf("CreatePullRequest calls = %d, want 1 (two replicas must open the consolidated PR once)", got)
	}
	if got := countAuditCategory(au, "consolidated_pr_opened"); got != 1 {
		t.Errorf("consolidated_pr_opened entries = %d, want 1", got)
	}
	for i, err := range errs {
		if err != nil {
			t.Errorf("maybeOpenConsolidatedPR replica #%d: %v", i, err)
		}
	}
	stored, err := runs.GetRun(ctx, parent.ID)
	if err != nil {
		t.Fatalf("reload parent: %v", err)
	}
	if got := derefURL(stored.PullRequestURL); got != gatedCreatedURL(1) {
		t.Errorf("persisted pull_request_url = %q, want the one created PR %q", got, gatedCreatedURL(1))
	}
}

// TestConsolidatedPROpen_AdoptsExistingOpenPR pins the pre-create adopt
// (#4110): when PRs are already open for the consolidated head, no PR is
// created and the lowest-numbered one is adopted. Fixture mechanism: without
// the adopt branch CreatePullRequest is called (1 != 0) and the stored URL is
// the created #777, not #4108.
func TestConsolidatedPROpen_AdoptsExistingOpenPR(t *testing.T) {
	o, rs, gh := newOrchestrator(t)
	o.DefaultRef = "main"
	au := &recordingAudit{}
	o.Audit = au
	gh.listByHeadResult = []forge.PullRequest{
		{Number: 4109, HTMLURL: "https://github.com/kuhlman-labs/fishhawk/pull/4109", State: "open"},
		{Number: 4108, HTMLURL: "https://github.com/kuhlman-labs/fishhawk/pull/4108", State: "open"},
	}
	parent, _ := seedDecomposedParent(t, rs, int64Ptr(55), run.ExecutorHuman)

	if _, err := o.Advance(context.Background(), parent.ID); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if len(gh.createPRCalls) != 0 {
		t.Errorf("CreatePullRequest calls = %d, want 0 (an open PR is adopted)", len(gh.createPRCalls))
	}
	if got := derefURL(rs.runs[parent.ID].PullRequestURL); got != "https://github.com/kuhlman-labs/fishhawk/pull/4108" {
		t.Errorf("pull_request_url = %q, want the lowest-numbered open PR #4108", got)
	}
	if got := countAuditCategory(au, "consolidated_pr_opened"); got != 1 {
		t.Errorf("consolidated_pr_opened entries = %d, want 1", got)
	}
}

// TestConsolidatedPROpen_PreCreateListError_FailsClosed pins the pre-create
// list's fail-closed branch (#4110): a list error opens nothing and stamps no
// URL, and Advance returns the error so the next Advance retries. Fixture
// mechanism: a permissive branch would fall through to CreatePullRequest
// (count 1) and stamp its URL.
func TestConsolidatedPROpen_PreCreateListError_FailsClosed(t *testing.T) {
	o, rs, gh := newOrchestrator(t)
	o.DefaultRef = "main"
	au := &recordingAudit{}
	o.Audit = au
	gh.listByHeadErr = errors.New("github list pulls: 502")
	parent, _ := seedDecomposedParent(t, rs, int64Ptr(55), run.ExecutorHuman)

	_, err := o.Advance(context.Background(), parent.ID)
	if err == nil {
		t.Fatal("Advance: want an error when the pre-create list fails, got nil")
	}
	if !strings.Contains(err.Error(), "before create") {
		t.Errorf("Advance error = %q, want the pre-create list failure", err)
	}
	if len(gh.createPRCalls) != 0 {
		t.Errorf("CreatePullRequest calls = %d, want 0 (fail closed on a list error)", len(gh.createPRCalls))
	}
	if got := rs.runs[parent.ID].PullRequestURL; got != nil {
		t.Errorf("pull_request_url = %q, want nil", *got)
	}
	if got := countAuditCategory(au, "consolidated_pr_opened"); got != 0 {
		t.Errorf("consolidated_pr_opened entries = %d, want 0", got)
	}
}

// TestConsolidatedPROpen_LockError_FailsClosed pins the DB-lock error path
// (#4110): when the repository's ConsolidatedPROpenLocker fails to acquire,
// the open is never attempted unguarded. Fixture mechanism: the fake locker
// errors WITHOUT calling fn, so an orchestrator that fell back to running fn
// (or skipped the capability probe) would call CreatePullRequest.
func TestConsolidatedPROpen_LockError_FailsClosed(t *testing.T) {
	inner := newCopyRuns()
	parentID, _ := inner.seedDecomposedParent()
	lockErr := errors.New("acquire consolidated PR open lock: connection refused")
	rs := &lockErrRuns{copyRuns: inner, err: lockErr}
	gh := newGatedCreateGitHub()
	gh.releaseAll() // a create, if one wrongly happens, must not park
	o := &Orchestrator{Runs: rs, GitHub: gh, DefaultRef: "main"}

	_, err := o.Advance(context.Background(), parentID)
	if !errors.Is(err, lockErr) {
		t.Fatalf("Advance error = %v, want the lock acquisition error", err)
	}
	if !strings.Contains(err.Error(), "open consolidated pr") {
		t.Errorf("Advance error = %q, want it surfaced as an open consolidated pr failure", err)
	}
	if got := gh.creates.Load(); got != 0 {
		t.Errorf("CreatePullRequest calls = %d, want 0 (never open unguarded)", got)
	}
	if got := inner.pullRequestURL(parentID); got != "" {
		t.Errorf("pull_request_url = %q, want empty", got)
	}
}

func TestOldestPullRequest(t *testing.T) {
	pr := func(n int) forge.PullRequest {
		return forge.PullRequest{Number: n, HTMLURL: fmt.Sprintf("https://github.com/o/r/pull/%d", n)}
	}
	tests := []struct {
		name string
		in   []forge.PullRequest
		want int
	}{
		{name: "empty yields the zero PR", in: nil, want: 0},
		{name: "single", in: []forge.PullRequest{pr(7)}, want: 7},
		{name: "lowest first", in: []forge.PullRequest{pr(4108), pr(4109)}, want: 4108},
		{name: "lowest last", in: []forge.PullRequest{pr(4273), pr(4272)}, want: 4272},
		{name: "lowest in the middle", in: []forge.PullRequest{pr(9), pr(3), pr(5)}, want: 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := oldestPullRequest(tc.in)
			if got.Number != tc.want {
				t.Errorf("oldestPullRequest = #%d, want #%d", got.Number, tc.want)
			}
			if tc.want == 0 && got.HTMLURL != "" {
				t.Errorf("oldestPullRequest(empty).HTMLURL = %q, want empty", got.HTMLURL)
			}
		})
	}
}
