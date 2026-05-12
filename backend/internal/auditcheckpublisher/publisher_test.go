package auditcheckpublisher_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcheckpublisher"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcomplete"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/stagecheck"
)

func TestPublish_Pass_PostsCompletedSuccess(t *testing.T) {
	runID := uuid.New()
	implID := uuid.New()
	implRow := &run.Stage{ID: implID, Type: run.StageTypeImplement, RunID: runID}
	repoRuns := &fakeRuns{
		runs: map[uuid.UUID]*run.Run{runID: {
			ID: runID, Repo: "x/y", InstallationID: int64Ptr(42),
		}},
		stages: map[uuid.UUID][]*run.Stage{runID: {implRow}},
	}
	repoArts := &fakeArtifacts{byStage: map[uuid.UUID][]*artifact.Artifact{
		implID: {prArtifact(implID, "abc123")},
	}}
	gh := &fakeGitHub{}

	pub := auditcheckpublisher.New(auditcheckpublisher.Deps{
		GitHub:      gh,
		Runs:        repoRuns,
		Artifacts:   repoArts,
		ExternalURL: "https://app.fishhawk.example.com",
	})
	if pub == nil {
		t.Fatal("expected publisher; got nil")
	}

	ok, err := pub.Publish(context.Background(), runID, stagecheck.StatePass, nil)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if !ok {
		t.Fatal("expected published=true")
	}
	if got := gh.calls; len(got) != 1 {
		t.Fatalf("expected 1 call, got %d", len(got))
	}
	c := gh.calls[0]
	if c.repo.Owner != "x" || c.repo.Name != "y" {
		t.Errorf("repo = %+v", c.repo)
	}
	if c.installationID != 42 {
		t.Errorf("installationID = %d", c.installationID)
	}
	if c.params.Name != auditcheckpublisher.CheckName {
		t.Errorf("name = %q", c.params.Name)
	}
	if c.params.HeadSHA != "abc123" {
		t.Errorf("head_sha = %q", c.params.HeadSHA)
	}
	if c.params.Status != githubclient.CheckRunStatusCompleted {
		t.Errorf("status = %q", c.params.Status)
	}
	if c.params.Conclusion != githubclient.CheckRunConclusionSuccess {
		t.Errorf("conclusion = %q", c.params.Conclusion)
	}
	if c.params.DetailsURL != "https://app.fishhawk.example.com/runs/"+runID.String() {
		t.Errorf("details_url = %q", c.params.DetailsURL)
	}
}

func TestPublish_Fail_RendersMissingSummary(t *testing.T) {
	runID, gh, pub := happyDeps(t)
	missing := []auditcomplete.MissingItem{
		{Kind: auditcomplete.MissingTrace, Detail: "implement stage missing redacted bundle"},
		{Kind: auditcomplete.MissingPullRequest, Detail: "implement stage has no pull_request artifact"},
	}
	if _, err := pub.Publish(context.Background(), runID, stagecheck.StateFail, missing); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(gh.calls) != 1 {
		t.Fatalf("expected 1 call")
	}
	p := gh.calls[0].params
	if p.Status != githubclient.CheckRunStatusCompleted {
		t.Errorf("status = %q want completed", p.Status)
	}
	if p.Conclusion != githubclient.CheckRunConclusionFailure {
		t.Errorf("conclusion = %q want failure", p.Conclusion)
	}
	if !strings.Contains(p.OutputSummary, "trace_missing") || !strings.Contains(p.OutputSummary, "pr_missing") {
		t.Errorf("summary should list each missing kind: %q", p.OutputSummary)
	}
}

func TestPublish_Pending_PostsInProgress(t *testing.T) {
	runID, gh, pub := happyDeps(t)
	if _, err := pub.Publish(context.Background(), runID, stagecheck.StatePending, nil); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	p := gh.calls[0].params
	if p.Status != githubclient.CheckRunStatusInProgress {
		t.Errorf("status = %q want in_progress", p.Status)
	}
	if p.Conclusion != "" {
		t.Errorf("conclusion = %q want empty", p.Conclusion)
	}
}

func TestPublish_DedupsRepeatedState(t *testing.T) {
	runID, gh, pub := happyDeps(t)

	// First call: publishes.
	first, err := pub.Publish(context.Background(), runID, stagecheck.StatePass, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !first {
		t.Errorf("first call should publish")
	}

	// Second call same state: skipped.
	second, err := pub.Publish(context.Background(), runID, stagecheck.StatePass, nil)
	if err != nil {
		t.Fatal(err)
	}
	if second {
		t.Errorf("second identical call should be skipped")
	}

	// Third call different state: publishes again.
	third, err := pub.Publish(context.Background(), runID, stagecheck.StateFail, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !third {
		t.Errorf("third differing call should publish")
	}

	if len(gh.calls) != 2 {
		t.Errorf("expected 2 calls (pass + fail), got %d", len(gh.calls))
	}
}

func TestPublish_NoImplementStage_Skips(t *testing.T) {
	runID := uuid.New()
	repoRuns := &fakeRuns{
		runs: map[uuid.UUID]*run.Run{runID: {
			ID: runID, Repo: "x/y", InstallationID: int64Ptr(42),
		}},
		stages: map[uuid.UUID][]*run.Stage{runID: {
			// Only a plan stage; no implement.
			{ID: uuid.New(), Type: run.StageTypePlan, RunID: runID},
		}},
	}
	gh := &fakeGitHub{}
	pub := auditcheckpublisher.New(auditcheckpublisher.Deps{
		GitHub: gh, Runs: repoRuns, Artifacts: &fakeArtifacts{},
		ExternalURL: "https://app.fishhawk.example.com",
	})

	ok, err := pub.Publish(context.Background(), runID, stagecheck.StatePass, nil)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if ok {
		t.Errorf("should skip when no implement stage; got ok=true")
	}
	if len(gh.calls) != 0 {
		t.Errorf("expected 0 calls; got %d", len(gh.calls))
	}
}

func TestPublish_NoPRArtifact_Skips(t *testing.T) {
	runID := uuid.New()
	implID := uuid.New()
	repoRuns := &fakeRuns{
		runs: map[uuid.UUID]*run.Run{runID: {
			ID: runID, Repo: "x/y", InstallationID: int64Ptr(42),
		}},
		stages: map[uuid.UUID][]*run.Stage{runID: {
			{ID: implID, Type: run.StageTypeImplement, RunID: runID},
		}},
	}
	gh := &fakeGitHub{}
	pub := auditcheckpublisher.New(auditcheckpublisher.Deps{
		GitHub: gh, Runs: repoRuns,
		Artifacts:   &fakeArtifacts{}, // no PR artifact
		ExternalURL: "https://app.fishhawk.example.com",
	})

	ok, err := pub.Publish(context.Background(), runID, stagecheck.StatePass, nil)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if ok {
		t.Errorf("should skip when no PR artifact; got ok=true")
	}
}

func TestPublish_NoInstallationID_Skips(t *testing.T) {
	runID := uuid.New()
	implID := uuid.New()
	repoRuns := &fakeRuns{
		runs: map[uuid.UUID]*run.Run{runID: {
			ID: runID, Repo: "x/y", InstallationID: nil, // CLI run
		}},
		stages: map[uuid.UUID][]*run.Stage{runID: {
			{ID: implID, Type: run.StageTypeImplement, RunID: runID},
		}},
	}
	gh := &fakeGitHub{}
	pub := auditcheckpublisher.New(auditcheckpublisher.Deps{
		GitHub: gh, Runs: repoRuns,
		Artifacts: &fakeArtifacts{byStage: map[uuid.UUID][]*artifact.Artifact{
			implID: {prArtifact(implID, "abc")},
		}},
		ExternalURL: "https://app.fishhawk.example.com",
	})
	ok, err := pub.Publish(context.Background(), runID, stagecheck.StatePass, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Errorf("should skip when no installation ID")
	}
}

func TestPublish_GitHubError_Returned(t *testing.T) {
	runID, gh, pub := happyDeps(t)
	gh.err = errors.New("403 forbidden")

	_, err := pub.Publish(context.Background(), runID, stagecheck.StatePass, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("err should wrap GitHub error: %v", err)
	}
	// Cache MUST NOT record a published state on failure — the
	// next compute should retry.
	if _, err := pub.Publish(context.Background(), runID, stagecheck.StatePass, nil); err == nil {
		t.Errorf("expected retry after failure to also call GitHub")
	}
	if len(gh.calls) != 2 {
		t.Errorf("expected 2 attempts (both errored); got %d", len(gh.calls))
	}
}

// --- Retry chain (#281 / E16.5) ---

// TestCheckRunPublisher_RetryHeadSHA_PublishesIndependently asserts
// that publishing audit-complete state for a parent run + a retry
// run on the same PR (distinct head_shas, as the runner commits
// fresh each attempt) results in two independent CreateCheckRun
// calls — one against each head_sha. Branch protection re-evaluates
// against whatever sha is currently the PR's HEAD; if the publisher
// ever conflated the two head_shas, GitHub would receive the wrong
// (or no) signal for the retry's commit and the PR would stay
// merge-blocked even after the retry succeeded.
func TestCheckRunPublisher_RetryHeadSHA_PublishesIndependently(t *testing.T) {
	parentID := uuid.New()
	retryID := uuid.New()
	parentImplID := uuid.New()
	retryImplID := uuid.New()
	const parentSHA = "p1p1p1p1p1p1p1p1p1p1p1p1p1p1p1p1p1p1p1p1"
	const retrySHA = "r2r2r2r2r2r2r2r2r2r2r2r2r2r2r2r2r2r2r2r2"

	repoRuns := &fakeRuns{
		runs: map[uuid.UUID]*run.Run{
			parentID: {ID: parentID, Repo: "x/y", InstallationID: int64Ptr(42)},
			retryID:  {ID: retryID, Repo: "x/y", InstallationID: int64Ptr(42), ParentRunID: &parentID, RetryAttempt: 1},
		},
		stages: map[uuid.UUID][]*run.Stage{
			parentID: {{ID: parentImplID, Type: run.StageTypeImplement, RunID: parentID}},
			retryID:  {{ID: retryImplID, Type: run.StageTypeImplement, RunID: retryID}},
		},
	}
	repoArts := &fakeArtifacts{byStage: map[uuid.UUID][]*artifact.Artifact{
		parentImplID: {prArtifact(parentImplID, parentSHA)},
		retryImplID:  {prArtifact(retryImplID, retrySHA)},
	}}
	gh := &fakeGitHub{}
	pub := auditcheckpublisher.New(auditcheckpublisher.Deps{
		GitHub: gh, Runs: repoRuns, Artifacts: repoArts,
		ExternalURL: "https://app.fishhawk.example.com",
	})

	if _, err := pub.Publish(context.Background(), parentID, stagecheck.StatePass, nil); err != nil {
		t.Fatalf("Publish(parent): %v", err)
	}
	if _, err := pub.Publish(context.Background(), retryID, stagecheck.StatePass, nil); err != nil {
		t.Fatalf("Publish(retry): %v", err)
	}
	if len(gh.calls) != 2 {
		t.Fatalf("expected 2 CreateCheckRun calls, got %d", len(gh.calls))
	}
	// Each call carries its own head_sha — the publisher is keyed
	// off the implement-stage artifact, not the run's lineage, so
	// the retry's call must name retrySHA and the parent's must
	// name parentSHA.
	if gh.calls[0].params.HeadSHA != parentSHA {
		t.Errorf("first call head_sha = %q, want %q", gh.calls[0].params.HeadSHA, parentSHA)
	}
	if gh.calls[1].params.HeadSHA != retrySHA {
		t.Errorf("second call head_sha = %q, want %q", gh.calls[1].params.HeadSHA, retrySHA)
	}
	// Details URLs route to the correct run page on each call too —
	// reviewers clicking through from GitHub get the right run.
	if !strings.Contains(gh.calls[0].params.DetailsURL, parentID.String()) {
		t.Errorf("parent details_url should reference parent run id: %q", gh.calls[0].params.DetailsURL)
	}
	if !strings.Contains(gh.calls[1].params.DetailsURL, retryID.String()) {
		t.Errorf("retry details_url should reference retry run id: %q", gh.calls[1].params.DetailsURL)
	}
}

// TestCheckRunPublisher_DoesNotRepublishParentOnRetry covers the
// case where a retry exists and the publisher is later invoked
// again on the *parent* run — e.g. an audit-log read triggers a
// re-compute on the parent for some reason. The publisher must
// still post against the parent's own head_sha rather than trying
// to be clever about "latest on PR." If it tried to publish the
// retry's sha for the parent's run id, the details_url would point
// at the wrong run page and reviewers chasing the GitHub link
// would see mismatched audit context.
func TestCheckRunPublisher_DoesNotRepublishParentOnRetry(t *testing.T) {
	parentID := uuid.New()
	retryID := uuid.New()
	parentImplID := uuid.New()
	retryImplID := uuid.New()
	const parentSHA = "p1p1p1p1p1p1p1p1p1p1p1p1p1p1p1p1p1p1p1p1"
	const retrySHA = "r2r2r2r2r2r2r2r2r2r2r2r2r2r2r2r2r2r2r2r2"

	repoRuns := &fakeRuns{
		runs: map[uuid.UUID]*run.Run{
			parentID: {ID: parentID, Repo: "x/y", InstallationID: int64Ptr(42)},
			retryID:  {ID: retryID, Repo: "x/y", InstallationID: int64Ptr(42), ParentRunID: &parentID, RetryAttempt: 1},
		},
		stages: map[uuid.UUID][]*run.Stage{
			parentID: {{ID: parentImplID, Type: run.StageTypeImplement, RunID: parentID}},
			retryID:  {{ID: retryImplID, Type: run.StageTypeImplement, RunID: retryID}},
		},
	}
	repoArts := &fakeArtifacts{byStage: map[uuid.UUID][]*artifact.Artifact{
		parentImplID: {prArtifact(parentImplID, parentSHA)},
		retryImplID:  {prArtifact(retryImplID, retrySHA)},
	}}
	gh := &fakeGitHub{}
	pub := auditcheckpublisher.New(auditcheckpublisher.Deps{
		GitHub: gh, Runs: repoRuns, Artifacts: repoArts,
		ExternalURL: "https://app.fishhawk.example.com",
	})

	// Publish retry first — simulates the most-recent retry
	// finishing and being audit-complete.
	if _, err := pub.Publish(context.Background(), retryID, stagecheck.StatePass, nil); err != nil {
		t.Fatalf("Publish(retry): %v", err)
	}
	// Now publish parent — a second audit read on the older run
	// must still route through parent's own head_sha.
	if _, err := pub.Publish(context.Background(), parentID, stagecheck.StatePass, nil); err != nil {
		t.Fatalf("Publish(parent): %v", err)
	}
	if len(gh.calls) != 2 {
		t.Fatalf("expected 2 CreateCheckRun calls; got %d", len(gh.calls))
	}
	// First call (retry) used retrySHA; second call (parent) must
	// use parentSHA — the publisher reads the artifact for the run
	// being published, not whichever was most recently published.
	if gh.calls[0].params.HeadSHA != retrySHA {
		t.Errorf("first call (retry) head_sha = %q, want %q", gh.calls[0].params.HeadSHA, retrySHA)
	}
	if gh.calls[1].params.HeadSHA != parentSHA {
		t.Errorf("second call (parent) head_sha = %q, want %q", gh.calls[1].params.HeadSHA, parentSHA)
	}
}

func TestNew_NilDepsReturnsNilPublisher(t *testing.T) {
	cases := []struct {
		name string
		d    auditcheckpublisher.Deps
	}{
		{"no github", auditcheckpublisher.Deps{Runs: &fakeRuns{}, Artifacts: &fakeArtifacts{}, ExternalURL: "x"}},
		{"no runs", auditcheckpublisher.Deps{GitHub: &fakeGitHub{}, Artifacts: &fakeArtifacts{}, ExternalURL: "x"}},
		{"no artifacts", auditcheckpublisher.Deps{GitHub: &fakeGitHub{}, Runs: &fakeRuns{}, ExternalURL: "x"}},
		{"no external url", auditcheckpublisher.Deps{GitHub: &fakeGitHub{}, Runs: &fakeRuns{}, Artifacts: &fakeArtifacts{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if pub := auditcheckpublisher.New(tc.d); pub != nil {
				t.Errorf("expected nil; got %+v", pub)
			}
		})
	}
}

func TestPublish_NilReceiver_NoOp(t *testing.T) {
	var pub *auditcheckpublisher.Publisher
	ok, err := pub.Publish(context.Background(), uuid.New(), stagecheck.StatePass, nil)
	if err != nil {
		t.Errorf("nil receiver should return nil error; got %v", err)
	}
	if ok {
		t.Errorf("nil receiver should return ok=false")
	}
}

// --- helpers ---

func happyDeps(t *testing.T) (uuid.UUID, *fakeGitHub, *auditcheckpublisher.Publisher) {
	t.Helper()
	runID := uuid.New()
	implID := uuid.New()
	repoRuns := &fakeRuns{
		runs: map[uuid.UUID]*run.Run{runID: {
			ID: runID, Repo: "x/y", InstallationID: int64Ptr(42),
		}},
		stages: map[uuid.UUID][]*run.Stage{runID: {
			{ID: implID, Type: run.StageTypeImplement, RunID: runID},
		}},
	}
	repoArts := &fakeArtifacts{byStage: map[uuid.UUID][]*artifact.Artifact{
		implID: {prArtifact(implID, "abc123")},
	}}
	gh := &fakeGitHub{}
	pub := auditcheckpublisher.New(auditcheckpublisher.Deps{
		GitHub: gh, Runs: repoRuns, Artifacts: repoArts,
		ExternalURL: "https://app.fishhawk.example.com",
	})
	if pub == nil {
		t.Fatal("publisher nil")
	}
	return runID, gh, pub
}

func int64Ptr(v int64) *int64 { return &v }

func prArtifact(stageID uuid.UUID, headSHA string) *artifact.Artifact {
	body, _ := json.Marshal(map[string]any{
		"pr_number": 1, "pr_url": "https://github.com/x/y/pull/1",
		"branch": "feat", "head_sha": headSHA, "base_sha": "0",
		"title": "t", "files_changed_count": 1,
	})
	return &artifact.Artifact{
		ID:        uuid.New(),
		StageID:   stageID,
		Kind:      artifact.KindPullRequest,
		Content:   body,
		CreatedAt: time.Now().UTC(),
	}
}

// --- fakes ---

type checkRunCall struct {
	installationID int64
	repo           githubclient.RepoRef
	params         githubclient.CreateCheckRunParams
}

type fakeGitHub struct {
	mu    sync.Mutex
	calls []checkRunCall
	err   error
}

func (f *fakeGitHub) CreateCheckRun(_ context.Context, installationID int64, repo githubclient.RepoRef, p githubclient.CreateCheckRunParams) (*githubclient.CreateCheckRunResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, checkRunCall{installationID: installationID, repo: repo, params: p})
	if f.err != nil {
		return nil, f.err
	}
	return &githubclient.CreateCheckRunResult{ID: 1}, nil
}

type fakeRuns struct {
	run.Repository
	runs    map[uuid.UUID]*run.Run
	stages  map[uuid.UUID][]*run.Stage
	getErr  error
	listErr error
}

func (f *fakeRuns) GetRun(_ context.Context, id uuid.UUID) (*run.Run, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	r, ok := f.runs[id]
	if !ok {
		return nil, run.ErrNotFound
	}
	return r, nil
}

func (f *fakeRuns) ListStagesForRun(_ context.Context, runID uuid.UUID) ([]*run.Stage, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.stages[runID], nil
}

type fakeArtifacts struct {
	artifact.Repository
	byStage map[uuid.UUID][]*artifact.Artifact
}

func (f *fakeArtifacts) ListForStage(_ context.Context, stageID uuid.UUID) ([]*artifact.Artifact, error) {
	return f.byStage[stageID], nil
}
