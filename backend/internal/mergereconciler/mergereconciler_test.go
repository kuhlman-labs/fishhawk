package mergereconciler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// fakeRepo is the run.Repository surface the reconciler uses. Embeds
// BaseFake and overrides the two methods the ticker calls.
type fakeRepo struct {
	run.BaseFake
	awaiting []*run.Stage
	awaitErr error
	runs     map[uuid.UUID]*run.Run
	getErr   error
	resolved map[uuid.UUID]bool // runs the resolver has moved out of awaiting
}

func (f *fakeRepo) ListReviewStagesAwaitingApproval(_ context.Context) ([]*run.Stage, error) {
	if f.awaitErr != nil {
		return nil, f.awaitErr
	}
	out := make([]*run.Stage, 0, len(f.awaiting))
	for _, s := range f.awaiting {
		if f.resolved[s.RunID] {
			// Resolution moved the stage to a terminal state, so it's
			// no longer awaiting approval — models the real query.
			continue
		}
		out = append(out, s)
	}
	return out, nil
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

// stubPRGetter returns a canned PR state and counts calls.
type stubPRGetter struct {
	pr    *githubclient.PullRequest
	err   error
	calls int
}

func (s *stubPRGetter) GetPullRequest(_ context.Context, _ int64, _ githubclient.RepoRef, _ int) (*githubclient.PullRequest, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return s.pr, nil
}

type resolveCall struct {
	runID  uuid.UUID
	merged bool
	prURL  string
}

// stubResolver records resolution calls. When repo is set it marks the
// run resolved so the next ListReviewStagesAwaitingApproval drops it —
// the mechanism by which a second tick is a no-op.
type stubResolver struct {
	repo  *fakeRepo
	calls []resolveCall
	err   error
}

func (s *stubResolver) ResolveReviewFromPollState(_ context.Context, runID uuid.UUID, merged bool, prURL string) error {
	s.calls = append(s.calls, resolveCall{runID: runID, merged: merged, prURL: prURL})
	if s.err != nil {
		return s.err
	}
	if s.repo != nil {
		if s.repo.resolved == nil {
			s.repo.resolved = map[uuid.UUID]bool{}
		}
		s.repo.resolved[runID] = true
	}
	return nil
}

func instID(v int64) *int64   { return &v }
func strPtr(s string) *string { return &s }

// reviewRun builds a run + a parked review stage wired to a PR URL.
func reviewRun(prURL string, installation *int64) (*run.Run, *run.Stage) {
	runID := uuid.New()
	r := &run.Run{
		ID:             runID,
		Repo:           "x/y",
		InstallationID: installation,
		PullRequestURL: nil,
	}
	if prURL != "" {
		r.PullRequestURL = strPtr(prURL)
	}
	s := &run.Stage{ID: uuid.New(), RunID: runID, Type: run.StageTypeReview, State: run.StageStateAwaitingApproval}
	return r, s
}

func newTicker(repo *fakeRepo, pg *stubPRGetter, res *stubResolver) *Ticker {
	return &Ticker{Runs: repo, PRGetter: pg, Resolver: res}
}

func TestTick_Merged_ResolvesSucceeded(t *testing.T) {
	r, s := reviewRun("https://github.com/x/y/pull/42", instID(99))
	repo := &fakeRepo{awaiting: []*run.Stage{s}, runs: map[uuid.UUID]*run.Run{r.ID: r}}
	pg := &stubPRGetter{pr: &githubclient.PullRequest{State: "closed", Merged: true}}
	res := &stubResolver{}
	newTicker(repo, pg, res).Tick(context.Background())

	if len(res.calls) != 1 {
		t.Fatalf("resolve calls = %d, want 1", len(res.calls))
	}
	if !res.calls[0].merged || res.calls[0].runID != r.ID {
		t.Errorf("resolve call = %+v, want merged=true runID=%s", res.calls[0], r.ID)
	}
}

func TestTick_SLALessReviewStage_Merged_Resolves(t *testing.T) {
	// #725 regression: a review stage carrying NO gate SLA (the
	// feature_change review gate's shape) must still be reconciled. The
	// reconciler now lists via ListReviewStagesAwaitingApproval, which is
	// SLA-independent, so a merged PR resolves merged=true even though the
	// old gate_sla-filtered query would have hidden the stage entirely.
	r, s := reviewRun("https://github.com/x/y/pull/42", instID(99))
	if s.GateSLA != nil {
		t.Fatalf("precondition: review stage should have no gate SLA, got %v", *s.GateSLA)
	}
	repo := &fakeRepo{awaiting: []*run.Stage{s}, runs: map[uuid.UUID]*run.Run{r.ID: r}}
	pg := &stubPRGetter{pr: &githubclient.PullRequest{State: "closed", Merged: true}}
	res := &stubResolver{}
	newTicker(repo, pg, res).Tick(context.Background())

	if len(res.calls) != 1 {
		t.Fatalf("resolve calls = %d, want 1 (SLA-less review stage must reconcile)", len(res.calls))
	}
	if !res.calls[0].merged || res.calls[0].runID != r.ID {
		t.Errorf("resolve call = %+v, want merged=true runID=%s", res.calls[0], r.ID)
	}
}

// stubReverifier records ReverifyBranchLineage calls and returns a canned
// clean verdict, modeling the server-side merge-resolution lineage re-check.
type stubReverifier struct {
	clean bool
	calls []int // pr numbers it was consulted on
}

func (s *stubReverifier) ReverifyBranchLineage(_ context.Context, _ uuid.UUID, prNumber int) bool {
	s.calls = append(s.calls, prNumber)
	return s.clean
}

// TestTick_Merged_ReverifierNotClean_SkipsResolve is the cross-package seam
// test (#862): on a verified merge, a non-clean lineage verdict must SUPPRESS
// the succeeded-resolve and leave the run parked/flagged. The Ticker→reverifier
// boundary is what's exercised — a per-package unit would pass while this
// wiring breaks.
func TestTick_Merged_ReverifierNotClean_SkipsResolve(t *testing.T) {
	r, s := reviewRun("https://github.com/x/y/pull/42", instID(99))
	repo := &fakeRepo{awaiting: []*run.Stage{s}, runs: map[uuid.UUID]*run.Run{r.ID: r}}
	pg := &stubPRGetter{pr: &githubclient.PullRequest{State: "closed", Merged: true}}
	res := &stubResolver{}
	rev := &stubReverifier{clean: false}
	tk := newTicker(repo, pg, res)
	tk.LineageReverifier = rev
	tk.Tick(context.Background())

	if len(rev.calls) != 1 || rev.calls[0] != 42 {
		t.Errorf("reverifier calls = %v, want [42]", rev.calls)
	}
	if len(res.calls) != 0 {
		t.Errorf("resolve calls = %d, want 0 (contaminated merge must not resolve succeeded)", len(res.calls))
	}
}

// TestTick_Merged_ReverifierClean_Resolves: a clean verdict falls through to
// the existing succeeded-resolve exactly as today.
func TestTick_Merged_ReverifierClean_Resolves(t *testing.T) {
	r, s := reviewRun("https://github.com/x/y/pull/42", instID(99))
	repo := &fakeRepo{awaiting: []*run.Stage{s}, runs: map[uuid.UUID]*run.Run{r.ID: r}}
	pg := &stubPRGetter{pr: &githubclient.PullRequest{State: "closed", Merged: true}}
	res := &stubResolver{}
	rev := &stubReverifier{clean: true}
	tk := newTicker(repo, pg, res)
	tk.LineageReverifier = rev
	tk.Tick(context.Background())

	if len(rev.calls) != 1 {
		t.Errorf("reverifier calls = %d, want 1", len(rev.calls))
	}
	if len(res.calls) != 1 || !res.calls[0].merged {
		t.Errorf("resolve calls = %+v, want one merged=true resolve", res.calls)
	}
}

// TestTick_Merged_NilReverifier_Resolves: a nil reverifier preserves today's
// behavior byte-for-byte — the merge resolves with no re-check.
func TestTick_Merged_NilReverifier_Resolves(t *testing.T) {
	r, s := reviewRun("https://github.com/x/y/pull/42", instID(99))
	repo := &fakeRepo{awaiting: []*run.Stage{s}, runs: map[uuid.UUID]*run.Run{r.ID: r}}
	pg := &stubPRGetter{pr: &githubclient.PullRequest{State: "closed", Merged: true}}
	res := &stubResolver{}
	tk := newTicker(repo, pg, res) // LineageReverifier left nil
	tk.Tick(context.Background())

	if len(res.calls) != 1 || !res.calls[0].merged {
		t.Errorf("resolve calls = %+v, want one merged=true resolve (nil reverifier = today's behavior)", res.calls)
	}
}

// TestTick_ClosedUnmerged_ReverifierNotConsulted: the cancelled (closed,
// !merged) branch lands nothing, so the lineage re-check must NOT run.
func TestTick_ClosedUnmerged_ReverifierNotConsulted(t *testing.T) {
	r, s := reviewRun("https://github.com/x/y/pull/42", instID(99))
	repo := &fakeRepo{awaiting: []*run.Stage{s}, runs: map[uuid.UUID]*run.Run{r.ID: r}}
	pg := &stubPRGetter{pr: &githubclient.PullRequest{State: "closed", Merged: false}}
	res := &stubResolver{}
	rev := &stubReverifier{clean: false}
	tk := newTicker(repo, pg, res)
	tk.LineageReverifier = rev
	tk.Tick(context.Background())

	if len(rev.calls) != 0 {
		t.Errorf("reverifier consulted on a closed-unmerged PR: %v", rev.calls)
	}
	if len(res.calls) != 1 || res.calls[0].merged {
		t.Errorf("resolve calls = %+v, want one cancelled (merged=false) resolve", res.calls)
	}
}

func TestTick_ClosedUnmerged_ResolvesCancelled(t *testing.T) {
	r, s := reviewRun("https://github.com/x/y/pull/42", instID(99))
	repo := &fakeRepo{awaiting: []*run.Stage{s}, runs: map[uuid.UUID]*run.Run{r.ID: r}}
	pg := &stubPRGetter{pr: &githubclient.PullRequest{State: "closed", Merged: false}}
	res := &stubResolver{}
	newTicker(repo, pg, res).Tick(context.Background())

	if len(res.calls) != 1 {
		t.Fatalf("resolve calls = %d, want 1", len(res.calls))
	}
	if res.calls[0].merged {
		t.Errorf("resolve call merged = true, want false (closed unmerged)")
	}
}

func TestTick_OpenPR_NoResolve(t *testing.T) {
	r, s := reviewRun("https://github.com/x/y/pull/42", instID(99))
	repo := &fakeRepo{awaiting: []*run.Stage{s}, runs: map[uuid.UUID]*run.Run{r.ID: r}}
	pg := &stubPRGetter{pr: &githubclient.PullRequest{State: "open", Merged: false}}
	res := &stubResolver{}
	newTicker(repo, pg, res).Tick(context.Background())

	if pg.calls != 1 {
		t.Errorf("PR get calls = %d, want 1", pg.calls)
	}
	if len(res.calls) != 0 {
		t.Errorf("resolve calls = %d, want 0 (open PR left parked)", len(res.calls))
	}
}

func TestTick_NilInstallation_Skips(t *testing.T) {
	r, s := reviewRun("https://github.com/x/y/pull/42", nil) // no installation
	repo := &fakeRepo{awaiting: []*run.Stage{s}, runs: map[uuid.UUID]*run.Run{r.ID: r}}
	pg := &stubPRGetter{pr: &githubclient.PullRequest{State: "closed", Merged: true}}
	res := &stubResolver{}
	newTicker(repo, pg, res).Tick(context.Background())

	if pg.calls != 0 {
		t.Errorf("PR get calls = %d, want 0 (nil installation skips before the GitHub read)", pg.calls)
	}
	if len(res.calls) != 0 {
		t.Errorf("resolve calls = %d, want 0", len(res.calls))
	}
}

func TestTick_NoPullRequestURL_Skips(t *testing.T) {
	r, s := reviewRun("", instID(99)) // no PR URL — pre-existing parked run
	repo := &fakeRepo{awaiting: []*run.Stage{s}, runs: map[uuid.UUID]*run.Run{r.ID: r}}
	pg := &stubPRGetter{pr: &githubclient.PullRequest{State: "closed", Merged: true}}
	res := &stubResolver{}
	newTicker(repo, pg, res).Tick(context.Background())

	if pg.calls != 0 || len(res.calls) != 0 {
		t.Errorf("expected clean skip; PR get calls=%d resolve calls=%d", pg.calls, len(res.calls))
	}
}

func TestTick_MalformedPRURL_Skips(t *testing.T) {
	r, s := reviewRun("https://example.com/not/a/pr", instID(99))
	repo := &fakeRepo{awaiting: []*run.Stage{s}, runs: map[uuid.UUID]*run.Run{r.ID: r}}
	pg := &stubPRGetter{pr: &githubclient.PullRequest{State: "closed", Merged: true}}
	res := &stubResolver{}
	newTicker(repo, pg, res).Tick(context.Background())

	if pg.calls != 0 {
		t.Errorf("PR get calls = %d, want 0 (malformed URL skips before the GitHub read)", pg.calls)
	}
	if len(res.calls) != 0 {
		t.Errorf("resolve calls = %d, want 0", len(res.calls))
	}
}

func TestTick_NonReviewStage_Filtered(t *testing.T) {
	runID := uuid.New()
	r := &run.Run{ID: runID, Repo: "x/y", InstallationID: instID(99), PullRequestURL: strPtr("https://github.com/x/y/pull/42")}
	planStage := &run.Stage{ID: uuid.New(), RunID: runID, Type: run.StageTypePlan, State: run.StageStateAwaitingApproval}
	repo := &fakeRepo{awaiting: []*run.Stage{planStage}, runs: map[uuid.UUID]*run.Run{runID: r}}
	pg := &stubPRGetter{pr: &githubclient.PullRequest{State: "closed", Merged: true}}
	res := &stubResolver{}
	newTicker(repo, pg, res).Tick(context.Background())

	if pg.calls != 0 || len(res.calls) != 0 {
		t.Errorf("plan stage should be filtered out; PR get calls=%d resolve calls=%d", pg.calls, len(res.calls))
	}
}

func TestTick_GetPRError_NoResolve(t *testing.T) {
	r, s := reviewRun("https://github.com/x/y/pull/42", instID(99))
	repo := &fakeRepo{awaiting: []*run.Stage{s}, runs: map[uuid.UUID]*run.Run{r.ID: r}}
	pg := &stubPRGetter{err: errors.New("rate limited")}
	res := &stubResolver{}
	newTicker(repo, pg, res).Tick(context.Background())

	if len(res.calls) != 0 {
		t.Errorf("resolve calls = %d, want 0 on a PR-get error", len(res.calls))
	}
}

func TestTick_SecondTickAfterResolution_IsNoOp(t *testing.T) {
	// Idempotency at the reconciler level: once a stage resolves it
	// leaves awaiting_approval, so the next tick has nothing to re-poll.
	r, s := reviewRun("https://github.com/x/y/pull/42", instID(99))
	repo := &fakeRepo{awaiting: []*run.Stage{s}, runs: map[uuid.UUID]*run.Run{r.ID: r}}
	pg := &stubPRGetter{pr: &githubclient.PullRequest{State: "closed", Merged: true}}
	res := &stubResolver{repo: repo}
	tk := newTicker(repo, pg, res)

	tk.Tick(context.Background())
	tk.Tick(context.Background())

	if len(res.calls) != 1 {
		t.Errorf("resolve calls = %d, want 1 (second tick is a no-op after resolution)", len(res.calls))
	}
	if pg.calls != 1 {
		t.Errorf("PR get calls = %d, want 1 (no re-poll after the stage left awaiting)", pg.calls)
	}
}

func TestTick_ResolverError_Logged_NoPanic(t *testing.T) {
	// A resolver error logs but doesn't abort the tick; a later tick
	// re-polls the still-terminal PR and retries idempotently.
	r, s := reviewRun("https://github.com/x/y/pull/42", instID(99))
	repo := &fakeRepo{awaiting: []*run.Stage{s}, runs: map[uuid.UUID]*run.Run{r.ID: r}}
	pg := &stubPRGetter{pr: &githubclient.PullRequest{State: "closed", Merged: true}}
	res := &stubResolver{err: errors.New("transition rejected")}
	newTicker(repo, pg, res).Tick(context.Background())

	if len(res.calls) != 1 {
		t.Fatalf("resolve calls = %d, want 1 (attempted despite the error)", len(res.calls))
	}
}

func TestTick_GetRunError_Skips(t *testing.T) {
	r, s := reviewRun("https://github.com/x/y/pull/42", instID(99))
	repo := &fakeRepo{awaiting: []*run.Stage{s}, getErr: errors.New("db down"), runs: map[uuid.UUID]*run.Run{r.ID: r}}
	pg := &stubPRGetter{pr: &githubclient.PullRequest{State: "closed", Merged: true}}
	res := &stubResolver{}
	newTicker(repo, pg, res).Tick(context.Background())

	if pg.calls != 0 || len(res.calls) != 0 {
		t.Errorf("a GetRun error should skip the stage; PR get=%d resolve=%d", pg.calls, len(res.calls))
	}
}

func TestTick_ListAwaitingError_NoPanic(t *testing.T) {
	repo := &fakeRepo{awaitErr: errors.New("db down")}
	pg := &stubPRGetter{}
	res := &stubResolver{}
	newTicker(repo, pg, res).Tick(context.Background())
	if pg.calls != 0 || len(res.calls) != 0 {
		t.Errorf("list error should be a clean no-op; PR get=%d resolve=%d", pg.calls, len(res.calls))
	}
}

func TestRun_FiresImmediatelyThenStopsOnCancel(t *testing.T) {
	r, s := reviewRun("https://github.com/x/y/pull/42", instID(99))
	repo := &fakeRepo{awaiting: []*run.Stage{s}, runs: map[uuid.UUID]*run.Run{r.ID: r}}
	pg := &stubPRGetter{pr: &githubclient.PullRequest{State: "closed", Merged: true}}
	// fired closes on the first resolve so the test synchronizes without
	// racing on the call slice.
	res := &chanResolver{fired: make(chan struct{}, 1)}
	tk := &Ticker{Runs: repo, PRGetter: pg, Resolver: res, Interval: time.Hour}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- tk.Run(ctx) }()

	// Run() fires an immediate first tick; wait for the resolve to land.
	select {
	case <-res.fired:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not fire its immediate first tick")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v, want nil on cancel", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// chanResolver signals each resolution on fired (non-blocking) so a
// concurrent Run() test can synchronize without touching shared state.
type chanResolver struct {
	fired chan struct{}
}

func (c *chanResolver) ResolveReviewFromPollState(_ context.Context, _ uuid.UUID, _ bool, _ string) error {
	select {
	case c.fired <- struct{}{}:
	default:
	}
	return nil
}

func TestRun_RequiresDeps(t *testing.T) {
	if err := (&Ticker{}).Run(context.Background()); err == nil {
		t.Error("Run with no deps should error")
	}
	if err := (&Ticker{Runs: &fakeRepo{}}).Run(context.Background()); err == nil {
		t.Error("Run without PRGetter should error")
	}
	if err := (&Ticker{Runs: &fakeRepo{}, PRGetter: &stubPRGetter{}}).Run(context.Background()); err == nil {
		t.Error("Run without Resolver should error")
	}
}

func TestParsePRURL(t *testing.T) {
	cases := []struct {
		in        string
		wantOwner string
		wantName  string
		wantNum   int
		wantErr   bool
	}{
		{"https://github.com/owner/name/pull/42", "owner", "name", 42, false},
		{"https://github.com/owner/name/pull/1/", "owner", "name", 1, false},
		{"https://github.com/owner/name/pulls/42", "", "", 0, true},
		{"https://github.com/owner/name/pull/abc", "", "", 0, true},
		{"https://github.com/owner/name", "", "", 0, true},
		{"", "", "", 0, true},
		{"https://github.com/owner/name/pull/0", "", "", 0, true},
	}
	for _, c := range cases {
		repo, n, err := parsePRURL(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parsePRURL(%q) = (%+v, %d, nil), want error", c.in, repo, n)
			}
			continue
		}
		if err != nil {
			t.Errorf("parsePRURL(%q) errored: %v", c.in, err)
			continue
		}
		if repo.Owner != c.wantOwner || repo.Name != c.wantName || n != c.wantNum {
			t.Errorf("parsePRURL(%q) = (%s/%s, %d), want (%s/%s, %d)",
				c.in, repo.Owner, repo.Name, n, c.wantOwner, c.wantName, c.wantNum)
		}
	}
}
