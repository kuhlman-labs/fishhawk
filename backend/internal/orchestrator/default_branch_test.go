package orchestrator

import (
	"context"
	"sync"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/defaultbranch"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// The tests below pin #4660 at the orchestrator: every base read — the fan-in
// base, the consolidated PR base and the consolidated-review base — resolves
// the repository's forge default branch through o.DefaultBranch and never
// falls back to a literal. They run a NON-Fishhawk repository (acme/widgets)
// whose forge default branch is `trunk` through the PRODUCTION
// defaultbranch.New resolver (ADR-110 rule 3(a)), never Static.
//
// COUNTERFACTUAL (CF-A6): replace baseBranch's body with a fixed literal →
// the trunk tests record that literal instead of trunk, and the unresolved
// tests get no error and record a base read → RED. The stub's GetBranchSHA
// answers for any mapped branch, so success alone masks nothing: each test
// asserts the RECORDED base argument.

const dbRepo = "acme/widgets"

// countingRepoGetter is a defaultbranch.Getter answering branch (or err) and
// counting reads.
type countingRepoGetter struct {
	branch string
	err    error

	mu    sync.Mutex
	reads int
}

func (g *countingRepoGetter) GetRepository(context.Context, forge.CredentialScope, forge.RepoRef) (*forge.Repository, error) {
	g.mu.Lock()
	g.reads++
	g.mu.Unlock()
	if g.err != nil {
		return nil, g.err
	}
	return &forge.Repository{DefaultBranch: g.branch}, nil
}

func (g *countingRepoGetter) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reads
}

// seedWidgetsFanIn seeds an acme/widgets decomposed parent (implement
// awaiting_children) with one succeeded slice.
func seedWidgetsFanIn(t *testing.T, rs *stubRuns) *run.Run {
	t.Helper()
	parent, _ := rs.seed(t, dbRepo, int64Ptr(55), []stageSeed{
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, State: run.StageStateAwaitingChildren},
		{Type: run.StageTypeReview, ExecutorKind: run.ExecutorHuman, ExecutorRef: "human", State: run.StageStatePending},
	})
	child, _ := rs.seed(t, dbRepo, int64Ptr(55), nil)
	child.DecomposedFrom = &parent.ID
	child.State = run.StateSucceeded
	idx := 0
	child.SliceIndex = &idx
	return parent
}

// seedWidgetsReviewGate seeds an acme/widgets decomposed parent whose next
// stage is the review gate (implement succeeded) with one succeeded child.
func seedWidgetsReviewGate(t *testing.T, rs *stubRuns) *run.Run {
	t.Helper()
	parent, _ := rs.seed(t, dbRepo, int64Ptr(55), []stageSeed{
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, State: run.StageStateSucceeded},
		{Type: run.StageTypeReview, ExecutorKind: run.ExecutorHuman, ExecutorRef: "human", State: run.StageStatePending},
	})
	child, _ := rs.seed(t, dbRepo, int64Ptr(55), nil)
	child.DecomposedFrom = &parent.ID
	child.State = run.StateSucceeded
	return parent
}

func TestDefaultBranch_FanInReadsForgeDefaultBranch(t *testing.T) {
	o, rs, gh := newOrchestrator(t)
	o.DefaultBranch = defaultbranch.New(&countingRepoGetter{branch: "trunk"})
	o.Audit = &recordingAudit{}
	gh.branchSHAs = map[string]string{"trunk": "trunksha"}
	parent := seedWidgetsFanIn(t, rs)

	if conflict, err := o.IntegrateSlices(context.Background(), parent.ID); err != nil || conflict != nil {
		t.Fatalf("IntegrateSlices: conflict=%+v err=%v", conflict, err)
	}
	gh.mu.Lock()
	defer gh.mu.Unlock()
	if len(gh.getBranchSHACalls) == 0 || gh.getBranchSHACalls[0] != "trunk" {
		t.Fatalf("GetBranchSHA calls = %v, want the base read on trunk first", gh.getBranchSHACalls)
	}
	if len(gh.createRefCalls) != 1 || gh.createRefCalls[0].SHA != "trunksha" {
		t.Errorf("CreateRef calls = %+v, want the consolidated branch cut from trunksha", gh.createRefCalls)
	}
}

func TestDefaultBranch_ConsolidatedPRAndReviewOnTrunkShareOneRead(t *testing.T) {
	o, rs, gh := newOrchestrator(t)
	getter := &countingRepoGetter{branch: "trunk"}
	o.DefaultBranch = defaultbranch.New(getter)
	o.Audit = &recordingAudit{}
	rec := &recordingConsolidatedReview{}
	o.ConsolidatedReview = rec
	parent := seedWidgetsReviewGate(t, rs)

	if _, err := o.Advance(context.Background(), parent.ID); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if len(gh.createPRCalls) != 1 || gh.createPRCalls[0].Base != "trunk" {
		t.Fatalf("CreatePullRequest calls = %+v, want one on base trunk", gh.createPRCalls)
	}
	if len(rec.calls) != 1 || rec.calls[0].base != "trunk" {
		t.Fatalf("DispatchConsolidatedReview calls = %+v, want one on base trunk", rec.calls)
	}
	if got := getter.count(); got != 1 {
		t.Fatalf("forge default-branch reads = %d, want 1 (the PR open and the review trigger share the cache)", got)
	}
}

func TestDefaultBranch_FanInUnresolvedRefuses(t *testing.T) {
	o, rs, gh := newOrchestrator(t)
	o.DefaultBranch = defaultbranch.New(&countingRepoGetter{err: forge.ErrNotFound})
	o.Audit = &recordingAudit{}
	gh.branchSHAs = map[string]string{"trunk": "trunksha", "main": "mainsha"}
	parent := seedWidgetsFanIn(t, rs)

	conflict, err := o.IntegrateSlices(context.Background(), parent.ID)
	if conflict != nil {
		t.Fatalf("conflict = %+v, want nil", conflict)
	}
	if reason, ok := defaultbranch.ReasonOf(err); !ok || reason != defaultbranch.ReasonRepositoryNotVisible {
		t.Fatalf("err = %v, want reason repository_not_visible", err)
	}
	gh.mu.Lock()
	defer gh.mu.Unlock()
	if len(gh.getBranchSHACalls) != 0 || len(gh.createRefCalls) != 0 || len(gh.mergeCalls) != 0 {
		t.Fatalf("forge writes/reads on an unresolved base: GetBranchSHA %v CreateRef %+v Merge %+v, want none",
			gh.getBranchSHACalls, gh.createRefCalls, gh.mergeCalls)
	}
}

func TestDefaultBranch_FanInNilResolverIsUnconfigured(t *testing.T) {
	o, rs, gh := newOrchestrator(t)
	o.Audit = &recordingAudit{}
	gh.branchSHAs = map[string]string{"main": "mainsha"}
	parent := seedWidgetsFanIn(t, rs)

	_, err := o.IntegrateSlices(context.Background(), parent.ID)
	if reason, ok := defaultbranch.ReasonOf(err); !ok || reason != defaultbranch.ReasonResolverUnconfigured {
		t.Fatalf("err = %v, want reason resolver_unconfigured (GitHub wired, no resolver)", err)
	}
	gh.mu.Lock()
	defer gh.mu.Unlock()
	if len(gh.getBranchSHACalls) != 0 {
		t.Fatalf("GetBranchSHA calls = %v, want none", gh.getBranchSHACalls)
	}
}

func TestDefaultBranch_ConsolidatedPRUnresolvedRefuses(t *testing.T) {
	o, rs, gh := newOrchestrator(t)
	o.DefaultBranch = defaultbranch.New(&countingRepoGetter{err: forge.ErrForbidden})
	o.Audit = &recordingAudit{}
	rec := &recordingConsolidatedReview{}
	o.ConsolidatedReview = rec
	parent := seedWidgetsReviewGate(t, rs)

	_, err := o.Advance(context.Background(), parent.ID)
	if reason, ok := defaultbranch.ReasonOf(err); !ok || reason != defaultbranch.ReasonForbidden {
		t.Fatalf("Advance err = %v, want the open-consolidated-pr error carrying reason forbidden", err)
	}
	if len(gh.createPRCalls) != 0 {
		t.Fatalf("CreatePullRequest calls = %+v, want none", gh.createPRCalls)
	}
	got, gerr := rs.GetRun(context.Background(), parent.ID)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if got.PullRequestURL != nil && *got.PullRequestURL != "" {
		t.Fatalf("PullRequestURL = %q, want none stamped", *got.PullRequestURL)
	}
	if len(rec.calls) != 0 {
		t.Fatalf("DispatchConsolidatedReview calls = %+v, want none", rec.calls)
	}
}

// TestDefaultBranch_ConsolidatedReviewUnresolvedSkipsDispatch: with the PR
// already stamped, an unresolved base SKIPS only the consolidated-review
// dispatch (WARN-logged); the review stage dispatch itself still happens.
func TestDefaultBranch_ConsolidatedReviewUnresolvedSkipsDispatch(t *testing.T) {
	o, rs, _ := newOrchestrator(t)
	o.DefaultBranch = defaultbranch.New(&countingRepoGetter{err: forge.ErrNotFound})
	rec := &recordingConsolidatedReview{}
	o.ConsolidatedReview = rec
	parent := seedWidgetsReviewGate(t, rs)
	pr := "https://github.com/acme/widgets/pull/9"
	parent.PullRequestURL = &pr

	out, err := o.Advance(context.Background(), parent.ID)
	if err != nil {
		t.Fatalf("Advance: %v (an unresolved review base must not fail the advance)", err)
	}
	if out != OutcomeDispatched {
		t.Fatalf("Outcome = %q, want dispatched (the review stage still dispatches)", out)
	}
	if len(rec.calls) != 0 {
		t.Fatalf("DispatchConsolidatedReview calls = %+v, want none on an unresolved base", rec.calls)
	}
	stages, serr := rs.ListStagesForRun(context.Background(), parent.ID)
	if serr != nil {
		t.Fatal(serr)
	}
	var review *run.Stage
	for _, s := range stages {
		if s.Type == run.StageTypeReview {
			review = s
		}
	}
	if review == nil || review.State == run.StageStatePending {
		t.Fatalf("review stage = %+v, want it advanced past pending", review)
	}
}

// TestDefaultBranch_RunBaseBranchInvalidRepo: an unparseable owner/name on the
// review trigger's run row refuses repository_invalid without a forge read.
func TestDefaultBranch_RunBaseBranchInvalidRepo(t *testing.T) {
	getter := &countingRepoGetter{branch: "trunk"}
	o := &Orchestrator{DefaultBranch: defaultbranch.New(getter)}
	_, err := o.runBaseBranch(context.Background(), &run.Run{Repo: "widgets", InstallationID: int64Ptr(55)})
	if reason, ok := defaultbranch.ReasonOf(err); !ok || reason != defaultbranch.ReasonRepositoryInvalid {
		t.Fatalf("err = %v, want reason repository_invalid", err)
	}
	_, err = o.runBaseBranch(context.Background(), &run.Run{Repo: dbRepo})
	if reason, ok := defaultbranch.ReasonOf(err); !ok || reason != defaultbranch.ReasonNoInstallation {
		t.Fatalf("err = %v, want reason no_installation for a nil installation id", err)
	}
	if getter.count() != 0 {
		t.Fatalf("forge reads = %d, want 0", getter.count())
	}
}
