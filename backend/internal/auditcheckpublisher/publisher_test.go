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
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcheckpublisher"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcomplete"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
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
	if c.scope != forge.FromGitHubInstallationID(42) {
		t.Errorf("scope = %v, want scope for installation 42", c.scope)
	}
	if c.params.Name != auditcheckpublisher.CheckName {
		t.Errorf("name = %q", c.params.Name)
	}
	if c.params.HeadSHA != "abc123" {
		t.Errorf("head_sha = %q", c.params.HeadSHA)
	}
	if c.params.Status != forge.CheckRunStatusCompleted {
		t.Errorf("status = %q", c.params.Status)
	}
	if c.params.Conclusion != forge.CheckRunConclusionSuccess {
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
	if p.Status != forge.CheckRunStatusCompleted {
		t.Errorf("status = %q want completed", p.Status)
	}
	if p.Conclusion != forge.CheckRunConclusionFailure {
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
	if p.Status != forge.CheckRunStatusInProgress {
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

// --- runner_kind status routing (#1861 / E45.8) ---

// TestPublish_GitHubActions_UsesGitHubCheckRun pins the GitHub branch of the
// runner_kind guard: a github_actions run publishes through the GitHub forge
// against forge.FromGitHubInstallationID(installation_id) and never touches
// the GitLab forge.
func TestPublish_GitHubActions_UsesGitHubCheckRun(t *testing.T) {
	runID := uuid.New()
	implID := uuid.New()
	repoRuns := &fakeRuns{
		runs: map[uuid.UUID]*run.Run{runID: {
			ID: runID, Repo: "x/y", InstallationID: int64Ptr(42),
			RunnerKind: run.RunnerKindGitHubActions,
		}},
		stages: map[uuid.UUID][]*run.Stage{runID: {
			{ID: implID, Type: run.StageTypeImplement, RunID: runID},
		}},
	}
	repoArts := &fakeArtifacts{byStage: map[uuid.UUID][]*artifact.Artifact{
		implID: {prArtifact(implID, "abc123")},
	}}
	gh := &fakeGitHub{}
	gl := &fakeGitLab{}
	pub := auditcheckpublisher.New(auditcheckpublisher.Deps{
		GitHub: gh, GitLab: gl, Runs: repoRuns, Artifacts: repoArts,
		ExternalURL: "https://app.fishhawk.example.com",
	})

	ok, err := pub.Publish(context.Background(), runID, stagecheck.StatePass, nil)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if !ok {
		t.Fatal("expected published=true")
	}
	if len(gh.calls) != 1 {
		t.Fatalf("expected 1 GitHub call, got %d", len(gh.calls))
	}
	if len(gl.calls) != 0 {
		t.Fatalf("expected 0 GitLab calls for a github_actions run, got %d", len(gl.calls))
	}
	if gh.calls[0].scope != forge.FromGitHubInstallationID(42) {
		t.Errorf("scope = %v, want installation-42 scope", gh.calls[0].scope)
	}
	if gh.calls[0].params.HeadSHA != "abc123" {
		t.Errorf("head_sha = %q", gh.calls[0].params.HeadSHA)
	}
}

// TestPublish_GitLabCI_PublishesGitLabCommitStatus pins the GitLab branch: a
// gitlab_ci run (carrying NO GitHub installation id) publishes the commit
// status through the GitLab forge — not the GitHub client — against the
// findHeadSHA-resolved SHA and the repo's resolved "gitlab:<id>" scope.
func TestPublish_GitLabCI_PublishesGitLabCommitStatus(t *testing.T) {
	runID := uuid.New()
	implID := uuid.New()
	repoRuns := &fakeRuns{
		runs: map[uuid.UUID]*run.Run{runID: {
			ID: runID, Repo: "grp/proj", InstallationID: nil, // no GitHub installation
			RunnerKind: run.RunnerKindGitLabCI,
		}},
		stages: map[uuid.UUID][]*run.Stage{runID: {
			{ID: implID, Type: run.StageTypeImplement, RunID: runID},
		}},
	}
	repoArts := &fakeArtifacts{byStage: map[uuid.UUID][]*artifact.Artifact{
		implID: {prArtifact(implID, "gl9sha")},
	}}
	gh := &fakeGitHub{}
	gl := &fakeGitLab{scope: forge.FromRef("gitlab:77")}
	pub := auditcheckpublisher.New(auditcheckpublisher.Deps{
		GitHub: gh, GitLab: gl, Runs: repoRuns, Artifacts: repoArts,
		ExternalURL: "https://app.fishhawk.example.com",
	})

	ok, err := pub.Publish(context.Background(), runID, stagecheck.StatePass, nil)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if !ok {
		t.Fatal("expected published=true")
	}
	// The GitLab forge was called, NOT the GitHub client.
	if len(gh.calls) != 0 {
		t.Fatalf("expected 0 GitHub calls for a gitlab_ci run, got %d", len(gh.calls))
	}
	if len(gl.calls) != 1 {
		t.Fatalf("expected 1 GitLab call, got %d", len(gl.calls))
	}
	c := gl.calls[0]
	if c.params.HeadSHA != "gl9sha" {
		t.Errorf("head_sha = %q, want the findHeadSHA-resolved %q", c.params.HeadSHA, "gl9sha")
	}
	if c.scope != forge.FromRef("gitlab:77") {
		t.Errorf("scope = %v, want the resolved gitlab:77 scope", c.scope)
	}
	if c.repo.Owner != "grp" || c.repo.Name != "proj" {
		t.Errorf("repo = %+v", c.repo)
	}
	if c.params.Name != auditcheckpublisher.CheckName {
		t.Errorf("name = %q", c.params.Name)
	}
}

// TestPublish_GitLabCI_Unconfigured_Skips proves the unwired-GitLab
// best-effort skip: a gitlab_ci run whose GitLab forge is neither injected
// nor registered publishes nothing and returns (false, nil) — the
// installation_id guard being GitHub-only must NOT make it fall through to
// the GitHub path.
func TestPublish_GitLabCI_Unconfigured_Skips(t *testing.T) {
	runID := uuid.New()
	implID := uuid.New()
	repoRuns := &fakeRuns{
		runs: map[uuid.UUID]*run.Run{runID: {
			ID: runID, Repo: "grp/proj", InstallationID: nil,
			RunnerKind: run.RunnerKindGitLabCI,
		}},
		stages: map[uuid.UUID][]*run.Stage{runID: {
			{ID: implID, Type: run.StageTypeImplement, RunID: runID},
		}},
	}
	repoArts := &fakeArtifacts{byStage: map[uuid.UUID][]*artifact.Artifact{
		implID: {prArtifact(implID, "gl9sha")},
	}}
	gh := &fakeGitHub{}
	// No Deps.GitLab, and the global registry has no "gitlab" forge in this
	// test binary → resolveGitLab returns nil → skip.
	pub := auditcheckpublisher.New(auditcheckpublisher.Deps{
		GitHub: gh, Runs: repoRuns, Artifacts: repoArts,
		ExternalURL: "https://app.fishhawk.example.com",
	})

	ok, err := pub.Publish(context.Background(), runID, stagecheck.StatePass, nil)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if ok {
		t.Error("should skip when the GitLab forge is unconfigured; got ok=true")
	}
	if len(gh.calls) != 0 {
		t.Errorf("a gitlab_ci run must never hit the GitHub client, got %d calls", len(gh.calls))
	}
}

// TestPublish_GitLabCI_ResolveScopeError_Propagates proves the defensive
// branch: a ResolveRepoScope failure surfaces as a Publish error and no
// commit status is written.
func TestPublish_GitLabCI_ResolveScopeError_Propagates(t *testing.T) {
	runID := uuid.New()
	implID := uuid.New()
	repoRuns := &fakeRuns{
		runs: map[uuid.UUID]*run.Run{runID: {
			ID: runID, Repo: "grp/proj", InstallationID: nil,
			RunnerKind: run.RunnerKindGitLabCI,
		}},
		stages: map[uuid.UUID][]*run.Stage{runID: {
			{ID: implID, Type: run.StageTypeImplement, RunID: runID},
		}},
	}
	repoArts := &fakeArtifacts{byStage: map[uuid.UUID][]*artifact.Artifact{
		implID: {prArtifact(implID, "gl9sha")},
	}}
	gh := &fakeGitHub{}
	gl := &fakeGitLab{resolveErr: errors.New("not installed")}
	pub := auditcheckpublisher.New(auditcheckpublisher.Deps{
		GitHub: gh, GitLab: gl, Runs: repoRuns, Artifacts: repoArts,
		ExternalURL: "https://app.fishhawk.example.com",
	})

	_, err := pub.Publish(context.Background(), runID, stagecheck.StatePass, nil)
	if err == nil {
		t.Fatal("expected a Publish error from the scope-resolution failure")
	}
	if !strings.Contains(err.Error(), "resolve gitlab scope") {
		t.Errorf("err should wrap the scope-resolution failure: %v", err)
	}
	if len(gl.calls) != 0 {
		t.Errorf("no commit status should be written on a scope error, got %d", len(gl.calls))
	}
}

// TestPublish_GitLabCI_NoHead_Skips proves the empty-head skip is
// forge-agnostic: a gitlab_ci run with no implement-stage PR artifact yet
// resolves no head, so it skips at findHeadSHA before resolving the GitLab
// scope or writing any status.
func TestPublish_GitLabCI_NoHead_Skips(t *testing.T) {
	runID := uuid.New()
	implID := uuid.New()
	repoRuns := &fakeRuns{
		runs: map[uuid.UUID]*run.Run{runID: {
			ID: runID, Repo: "grp/proj", InstallationID: nil,
			RunnerKind: run.RunnerKindGitLabCI,
		}},
		stages: map[uuid.UUID][]*run.Stage{runID: {
			{ID: implID, Type: run.StageTypeImplement, RunID: runID},
		}},
	}
	gh := &fakeGitHub{}
	gl := &fakeGitLab{scope: forge.FromRef("gitlab:77")}
	pub := auditcheckpublisher.New(auditcheckpublisher.Deps{
		GitHub: gh, GitLab: gl, Runs: repoRuns,
		Artifacts:   &fakeArtifacts{}, // no PR artifact
		ExternalURL: "https://app.fishhawk.example.com",
	})

	ok, err := pub.Publish(context.Background(), runID, stagecheck.StatePass, nil)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if ok {
		t.Error("should skip when no head resolves; got ok=true")
	}
	if len(gl.calls) != 0 {
		t.Errorf("expected 0 GitLab calls, got %d", len(gl.calls))
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

// --- Persistent-failure episodes (#993) ---

// TestPublish_OnDegraded_FiresOnceAtThreshold pins the once-per-episode
// contract: the callback fires exactly when the consecutive-failure
// streak reaches DefaultDegradedThreshold and stays silent on attempts
// past it.
func TestPublish_OnDegraded_FiresOnceAtThreshold(t *testing.T) {
	rec := &episodeCalls{}
	runID, gh, pub := callbackDeps(t, rec)
	gh.err = errors.New("401 Bad credentials")

	for i := 1; i <= auditcheckpublisher.DefaultDegradedThreshold+3; i++ {
		if _, err := pub.Publish(context.Background(), runID, stagecheck.StatePass, nil); err == nil {
			t.Fatalf("attempt %d: expected error", i)
		}
		want := 0
		if i >= auditcheckpublisher.DefaultDegradedThreshold {
			want = 1
		}
		if got := len(rec.degradedCalls()); got != want {
			t.Fatalf("after attempt %d: degraded calls = %d, want %d", i, got, want)
		}
	}
	d := rec.degradedCalls()[0]
	if d.runID != runID {
		t.Errorf("degraded run_id = %s, want %s", d.runID, runID)
	}
	if d.headSHA != "abc123" {
		t.Errorf("degraded head_sha = %q, want abc123", d.headSHA)
	}
	if d.attempts != auditcheckpublisher.DefaultDegradedThreshold {
		t.Errorf("degraded attempts = %d, want %d", d.attempts, auditcheckpublisher.DefaultDegradedThreshold)
	}
	if d.lastErr == nil || !strings.Contains(d.lastErr.Error(), "401") {
		t.Errorf("degraded last_err should wrap the GitHub error: %v", d.lastErr)
	}
	if got := len(rec.recoveredCalls()); got != 0 {
		t.Errorf("recovered calls = %d, want 0 (never succeeded)", got)
	}
}

// TestPublish_SuccessMidStreak_ResetsCount: a success before the
// threshold clears the streak, so no degraded callback ever fires
// even though the total failure count crosses the threshold.
func TestPublish_SuccessMidStreak_ResetsCount(t *testing.T) {
	rec := &episodeCalls{}
	runID, gh, pub := callbackDeps(t, rec)

	gh.err = errors.New("boom")
	for i := 0; i < auditcheckpublisher.DefaultDegradedThreshold-1; i++ {
		_, _ = pub.Publish(context.Background(), runID, stagecheck.StatePass, nil)
	}
	gh.err = nil
	if ok, err := pub.Publish(context.Background(), runID, stagecheck.StatePass, nil); err != nil || !ok {
		t.Fatalf("mid-streak success: ok=%v err=%v", ok, err)
	}
	// Fail again with a DIFFERENT state (same state would dedup-skip
	// against the just-recorded success and never reach GitHub).
	gh.err = errors.New("boom")
	for i := 0; i < auditcheckpublisher.DefaultDegradedThreshold-1; i++ {
		_, _ = pub.Publish(context.Background(), runID, stagecheck.StateFail, nil)
	}
	if got := len(rec.degradedCalls()); got != 0 {
		t.Fatalf("degraded calls = %d, want 0 (success reset the streak)", got)
	}
}

// TestPublish_RecoveryAfterDegradation: success after degradation
// invokes OnRecovered carrying the streak it cleared, and a fresh
// failure streak afterwards starts a NEW episode that degrades again.
func TestPublish_RecoveryAfterDegradation(t *testing.T) {
	rec := &episodeCalls{}
	runID, gh, pub := callbackDeps(t, rec)

	gh.err = errors.New("boom")
	for i := 0; i < auditcheckpublisher.DefaultDegradedThreshold; i++ {
		_, _ = pub.Publish(context.Background(), runID, stagecheck.StatePass, nil)
	}
	if got := len(rec.degradedCalls()); got != 1 {
		t.Fatalf("degraded calls = %d, want 1", got)
	}

	gh.err = nil
	if ok, err := pub.Publish(context.Background(), runID, stagecheck.StatePass, nil); err != nil || !ok {
		t.Fatalf("recovery publish: ok=%v err=%v", ok, err)
	}
	recovered := rec.recoveredCalls()
	if len(recovered) != 1 {
		t.Fatalf("recovered calls = %d, want 1", len(recovered))
	}
	if recovered[0].runID != runID || recovered[0].headSHA != "abc123" {
		t.Errorf("recovered call = %+v", recovered[0])
	}
	if recovered[0].attempts != auditcheckpublisher.DefaultDegradedThreshold {
		t.Errorf("recovered attempts = %d, want %d", recovered[0].attempts, auditcheckpublisher.DefaultDegradedThreshold)
	}

	// A fresh streak (different state so the dedup cache doesn't
	// short-circuit) is a new episode and degrades independently.
	gh.err = errors.New("boom again")
	for i := 0; i < auditcheckpublisher.DefaultDegradedThreshold; i++ {
		_, _ = pub.Publish(context.Background(), runID, stagecheck.StateFail, nil)
	}
	if got := len(rec.degradedCalls()); got != 2 {
		t.Fatalf("degraded calls after second streak = %d, want 2", got)
	}
}

// TestPublish_CleanSuccess_InvokesOnRecoveredWithZeroAttempts pins the
// always-invoke contract OnRecovered's doc promises: the durable
// open-episode decision belongs to the callee, so even a success with
// no in-process streak (e.g. right after a daemon restart) reports.
func TestPublish_CleanSuccess_InvokesOnRecoveredWithZeroAttempts(t *testing.T) {
	rec := &episodeCalls{}
	runID, _, pub := callbackDeps(t, rec)

	if ok, err := pub.Publish(context.Background(), runID, stagecheck.StatePass, nil); err != nil || !ok {
		t.Fatalf("publish: ok=%v err=%v", ok, err)
	}
	recovered := rec.recoveredCalls()
	if len(recovered) != 1 {
		t.Fatalf("recovered calls = %d, want 1", len(recovered))
	}
	if recovered[0].attempts != 0 {
		t.Errorf("attempts = %d, want 0", recovered[0].attempts)
	}
}

// TestPublish_SkipAndReadErrorPaths_DontCount: only the CreateCheckRun
// attempt proper advances the streak — skip paths (no PR artifact) and
// GetRun read errors stay invisible no matter how often they repeat.
func TestPublish_SkipAndReadErrorPaths_DontCount(t *testing.T) {
	rec := &episodeCalls{}
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
	repoArts := &fakeArtifacts{byStage: map[uuid.UUID][]*artifact.Artifact{}}
	gh := &fakeGitHub{err: errors.New("boom")}
	pub := auditcheckpublisher.New(auditcheckpublisher.Deps{
		GitHub: gh, Runs: repoRuns, Artifacts: repoArts,
		ExternalURL: "https://app.fishhawk.example.com",
		OnDegraded:  rec.onDegraded,
		OnRecovered: rec.onRecovered,
	})

	// Skip path: no PR artifact yet — repeat well past the threshold.
	for i := 0; i < auditcheckpublisher.DefaultDegradedThreshold+2; i++ {
		if ok, err := pub.Publish(context.Background(), runID, stagecheck.StatePass, nil); ok || err != nil {
			t.Fatalf("skip publish: ok=%v err=%v", ok, err)
		}
	}
	// Read-error path: GetRun failing is not a publish attempt.
	repoRuns.getErr = errors.New("db down")
	for i := 0; i < auditcheckpublisher.DefaultDegradedThreshold+2; i++ {
		if _, err := pub.Publish(context.Background(), runID, stagecheck.StatePass, nil); err == nil {
			t.Fatal("expected GetRun error")
		}
	}
	repoRuns.getErr = nil
	if got := len(rec.degradedCalls()); got != 0 {
		t.Fatalf("degraded calls = %d, want 0 (no CreateCheckRun attempted)", got)
	}

	// Once the artifact lands, the streak starts from zero and fires
	// exactly at the threshold.
	repoArts.byStage[implID] = []*artifact.Artifact{prArtifact(implID, "abc123")}
	for i := 0; i < auditcheckpublisher.DefaultDegradedThreshold; i++ {
		_, _ = pub.Publish(context.Background(), runID, stagecheck.StatePass, nil)
	}
	if got := len(rec.degradedCalls()); got != 1 {
		t.Fatalf("degraded calls = %d, want 1", got)
	}
	if got := rec.degradedCalls()[0].attempts; got != auditcheckpublisher.DefaultDegradedThreshold {
		t.Errorf("attempts = %d, want %d (skips must not have counted)", got, auditcheckpublisher.DefaultDegradedThreshold)
	}
}

// TestPublish_EpisodesKeyedPerRun: two runs sharing repo AND head_sha
// (the #993 amendment case) have independent failure episodes — run
// A's sub-threshold streak never bleeds into run B's count, and the
// degraded callback names the run that actually crossed.
func TestPublish_EpisodesKeyedPerRun(t *testing.T) {
	rec := &episodeCalls{}
	runA, runB := uuid.New(), uuid.New()
	implA, implB := uuid.New(), uuid.New()
	const sharedSHA = "feedfeedfeedfeedfeedfeedfeedfeedfeedfeed"
	repoRuns := &fakeRuns{
		runs: map[uuid.UUID]*run.Run{
			runA: {ID: runA, Repo: "x/y", InstallationID: int64Ptr(42)},
			runB: {ID: runB, Repo: "x/y", InstallationID: int64Ptr(42)},
		},
		stages: map[uuid.UUID][]*run.Stage{
			runA: {{ID: implA, Type: run.StageTypeImplement, RunID: runA}},
			runB: {{ID: implB, Type: run.StageTypeImplement, RunID: runB}},
		},
	}
	repoArts := &fakeArtifacts{byStage: map[uuid.UUID][]*artifact.Artifact{
		implA: {prArtifact(implA, sharedSHA)},
		implB: {prArtifact(implB, sharedSHA)},
	}}
	gh := &fakeGitHub{err: errors.New("boom")}
	pub := auditcheckpublisher.New(auditcheckpublisher.Deps{
		GitHub: gh, Runs: repoRuns, Artifacts: repoArts,
		ExternalURL: "https://app.fishhawk.example.com",
		OnDegraded:  rec.onDegraded,
		OnRecovered: rec.onRecovered,
	})

	// Run A: one short of the threshold. Run B: one short, too. A
	// shared (repo, head_sha) key would have crossed long ago.
	for i := 0; i < auditcheckpublisher.DefaultDegradedThreshold-1; i++ {
		_, _ = pub.Publish(context.Background(), runA, stagecheck.StatePass, nil)
		_, _ = pub.Publish(context.Background(), runB, stagecheck.StatePass, nil)
	}
	if got := len(rec.degradedCalls()); got != 0 {
		t.Fatalf("degraded calls = %d, want 0 (episodes must be per-run)", got)
	}

	// Run B alone crosses; the callback must name run B.
	_, _ = pub.Publish(context.Background(), runB, stagecheck.StatePass, nil)
	degraded := rec.degradedCalls()
	if len(degraded) != 1 {
		t.Fatalf("degraded calls = %d, want 1", len(degraded))
	}
	if degraded[0].runID != runB {
		t.Errorf("degraded run_id = %s, want run B %s", degraded[0].runID, runB)
	}
	if degraded[0].headSHA != sharedSHA {
		t.Errorf("degraded head_sha = %q", degraded[0].headSHA)
	}
}

// TestPublish_DedupHit_ClosesOpenEpisode: run A publishes successfully
// first and records the (repo, head_sha) dedup cache; run B — same
// repo AND head, with an open degraded episode — then publishes the
// same state. The dedup no-op must still clear B's episode and fire
// OnRecovered with B's streak length, otherwise B's degraded entry
// would never get its paired recovered entry.
func TestPublish_DedupHit_ClosesOpenEpisode(t *testing.T) {
	rec := &episodeCalls{}
	runA, runB := uuid.New(), uuid.New()
	implA, implB := uuid.New(), uuid.New()
	const sharedSHA = "feedfeedfeedfeedfeedfeedfeedfeedfeedfeed"
	repoRuns := &fakeRuns{
		runs: map[uuid.UUID]*run.Run{
			runA: {ID: runA, Repo: "x/y", InstallationID: int64Ptr(42)},
			runB: {ID: runB, Repo: "x/y", InstallationID: int64Ptr(42)},
		},
		stages: map[uuid.UUID][]*run.Stage{
			runA: {{ID: implA, Type: run.StageTypeImplement, RunID: runA}},
			runB: {{ID: implB, Type: run.StageTypeImplement, RunID: runB}},
		},
	}
	repoArts := &fakeArtifacts{byStage: map[uuid.UUID][]*artifact.Artifact{
		implA: {prArtifact(implA, sharedSHA)},
		implB: {prArtifact(implB, sharedSHA)},
	}}
	gh := &fakeGitHub{err: errors.New("boom")}
	pub := auditcheckpublisher.New(auditcheckpublisher.Deps{
		GitHub: gh, Runs: repoRuns, Artifacts: repoArts,
		ExternalURL: "https://app.fishhawk.example.com",
		OnDegraded:  rec.onDegraded,
		OnRecovered: rec.onRecovered,
	})

	// Run B degrades against the shared head.
	for i := 0; i < auditcheckpublisher.DefaultDegradedThreshold; i++ {
		_, _ = pub.Publish(context.Background(), runB, stagecheck.StatePass, nil)
	}
	if got := len(rec.degradedCalls()); got != 1 {
		t.Fatalf("degraded calls = %d, want 1", got)
	}

	// GitHub recovers; run A publishes clean first, filling the
	// (repo, head_sha) dedup cache before B ever publishes itself.
	gh.err = nil
	if ok, err := pub.Publish(context.Background(), runA, stagecheck.StatePass, nil); err != nil || !ok {
		t.Fatalf("run A publish: ok=%v err=%v", ok, err)
	}
	ghCalls := len(gh.calls)

	// Run B's next publish is a dedup no-op — no GitHub call — but it
	// must still close B's open episode.
	published, err := pub.Publish(context.Background(), runB, stagecheck.StatePass, nil)
	if err != nil || published {
		t.Fatalf("run B dedup publish: published=%v err=%v", published, err)
	}
	if got := len(gh.calls); got != ghCalls {
		t.Fatalf("dedup hit hit GitHub: calls = %d, want %d", got, ghCalls)
	}
	recovered := rec.recoveredCalls()
	if len(recovered) != 2 {
		t.Fatalf("recovered calls = %d, want 2 (run A clean + run B dedup)", len(recovered))
	}
	last := recovered[len(recovered)-1]
	if last.runID != runB {
		t.Errorf("recovered run_id = %s, want run B %s", last.runID, runB)
	}
	if last.attempts != auditcheckpublisher.DefaultDegradedThreshold {
		t.Errorf("recovered attempts = %d, want %d (B's cleared streak)", last.attempts, auditcheckpublisher.DefaultDegradedThreshold)
	}

	// The episode is gone: a further dedup hit reports a zero streak.
	_, _ = pub.Publish(context.Background(), runB, stagecheck.StatePass, nil)
	recovered = rec.recoveredCalls()
	if got := recovered[len(recovered)-1].attempts; got != 0 {
		t.Errorf("post-close recovered attempts = %d, want 0 (episode not cleared)", got)
	}
}

// TestPublish_NilCallbacks_Safe: a publisher without the optional
// callbacks survives a full degrade → recover cycle untouched.
func TestPublish_NilCallbacks_Safe(t *testing.T) {
	runID, gh, pub := happyDeps(t)
	gh.err = errors.New("boom")
	for i := 0; i < auditcheckpublisher.DefaultDegradedThreshold+1; i++ {
		if _, err := pub.Publish(context.Background(), runID, stagecheck.StatePass, nil); err == nil {
			t.Fatal("expected error")
		}
	}
	gh.err = nil
	if ok, err := pub.Publish(context.Background(), runID, stagecheck.StatePass, nil); err != nil || !ok {
		t.Fatalf("recovery publish: ok=%v err=%v", ok, err)
	}
}

// TestPublish_Concurrent_CallbacksOutsideMutex drives concurrent
// Publish calls under -race with a degraded callback that RE-ENTERS
// the publisher. If the implementation invoked callbacks while
// holding p.mu, the nested Publish would deadlock on the
// non-reentrant mutex and the test would time out.
func TestPublish_Concurrent_CallbacksOutsideMutex(t *testing.T) {
	rec := &episodeCalls{}
	runID, gh, pub := callbackDeps(t, rec)
	gh.err = errors.New("boom")
	var reentered sync.WaitGroup
	reentered.Add(1)
	rec.onDegradedHook = func() {
		// Re-enter the publisher from inside the callback: this
		// acquires p.mu and deadlocks if the callback runs locked.
		_, _ = pub.Publish(context.Background(), runID, stagecheck.StateFail, nil)
		reentered.Done()
	}

	const goroutines = 8
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < auditcheckpublisher.DefaultDegradedThreshold; i++ {
				_, _ = pub.Publish(context.Background(), runID, stagecheck.StatePass, nil)
			}
		}()
	}
	wg.Wait()
	reentered.Wait()
	if got := len(rec.degradedCalls()); got != 1 {
		t.Fatalf("degraded calls = %d, want exactly 1 across all goroutines", got)
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

// callbackDeps is happyDeps with the #993 episode callbacks wired to
// a recorder.
func callbackDeps(t *testing.T, rec *episodeCalls) (uuid.UUID, *fakeGitHub, *auditcheckpublisher.Publisher) {
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
		OnDegraded:  rec.onDegraded,
		OnRecovered: rec.onRecovered,
	})
	if pub == nil {
		t.Fatal("publisher nil")
	}
	return runID, gh, pub
}

// episodeCalls records OnDegraded / OnRecovered invocations.
// onDegradedHook, when set, runs inside the callback (used to prove
// callbacks execute outside the publisher's mutex).
type episodeCalls struct {
	mu             sync.Mutex
	degraded       []episodeCall
	recovered      []episodeCall
	onDegradedHook func()
}

type episodeCall struct {
	runID    uuid.UUID
	headSHA  string
	attempts int
	lastErr  error
}

func (r *episodeCalls) onDegraded(_ context.Context, runID uuid.UUID, headSHA string, attempts int, lastErr error) {
	r.mu.Lock()
	r.degraded = append(r.degraded, episodeCall{runID: runID, headSHA: headSHA, attempts: attempts, lastErr: lastErr})
	hook := r.onDegradedHook
	r.mu.Unlock()
	if hook != nil {
		hook()
	}
}

func (r *episodeCalls) onRecovered(_ context.Context, runID uuid.UUID, headSHA string, attempts int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recovered = append(r.recovered, episodeCall{runID: runID, headSHA: headSHA, attempts: attempts})
}

func (r *episodeCalls) degradedCalls() []episodeCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]episodeCall{}, r.degraded...)
}

func (r *episodeCalls) recoveredCalls() []episodeCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]episodeCall{}, r.recovered...)
}

// TestPublish_PrefersFixupPushedHeadOverArtifact proves the #1682 head
// resolver: with an Audit dep wired, findHeadSHA targets the newest
// fixup_pushed head, NOT the stale PR-open artifact head.
func TestPublish_PrefersFixupPushedHeadOverArtifact(t *testing.T) {
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
	// The PR-open artifact records the STALE head; a later fix-up pushed a new one.
	repoArts := &fakeArtifacts{byStage: map[uuid.UUID][]*artifact.Artifact{
		implID: {prArtifact(implID, "stalehead")},
	}}
	aud := &fakeAuditReader{byCat: map[string][]*audit.Entry{
		"pull_request_opened": {headEntry("pull_request_opened", "stalehead", 1)},
		"fixup_pushed":        {headEntry("fixup_pushed", "freshhead", 5)},
	}}
	gh := &fakeGitHub{}
	pub := auditcheckpublisher.New(auditcheckpublisher.Deps{
		GitHub: gh, Runs: repoRuns, Artifacts: repoArts, Audit: aud,
		ExternalURL: "https://app.fishhawk.example.com",
	})
	if pub == nil {
		t.Fatal("publisher nil")
	}
	if _, err := pub.Publish(context.Background(), runID, stagecheck.StatePass, nil); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(gh.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(gh.calls))
	}
	if got := gh.calls[0].params.HeadSHA; got != "freshhead" {
		t.Errorf("head_sha = %q; want the newest fixup_pushed head %q", got, "freshhead")
	}
}

// TestPublish_NoAuditHead_FallsBackToArtifact proves the fallback branch: an
// Audit dep that records no head_sha degrades to the PR-open artifact head.
func TestPublish_NoAuditHead_FallsBackToArtifact(t *testing.T) {
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
	aud := &fakeAuditReader{byCat: map[string][]*audit.Entry{}} // no head entries
	gh := &fakeGitHub{}
	pub := auditcheckpublisher.New(auditcheckpublisher.Deps{
		GitHub: gh, Runs: repoRuns, Artifacts: repoArts, Audit: aud,
		ExternalURL: "https://app.fishhawk.example.com",
	})
	if pub == nil {
		t.Fatal("publisher nil")
	}
	if _, err := pub.Publish(context.Background(), runID, stagecheck.StatePass, nil); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(gh.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(gh.calls))
	}
	if got := gh.calls[0].params.HeadSHA; got != "abc123" {
		t.Errorf("head_sha = %q; want artifact fallback %q", got, "abc123")
	}
}

// TestPublish_AuditReadError_Propagates proves the defensive branch: a read
// error resolving the audit head surfaces as a Publish error (never a silent
// publish to a stale head).
func TestPublish_AuditReadError_Propagates(t *testing.T) {
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
	aud := &fakeAuditReader{err: errors.New("boom")}
	gh := &fakeGitHub{}
	pub := auditcheckpublisher.New(auditcheckpublisher.Deps{
		GitHub: gh, Runs: repoRuns, Artifacts: repoArts, Audit: aud,
		ExternalURL: "https://app.fishhawk.example.com",
	})
	if pub == nil {
		t.Fatal("publisher nil")
	}
	if _, err := pub.Publish(context.Background(), runID, stagecheck.StatePass, nil); err == nil {
		t.Fatal("expected a Publish error from the audit read failure")
	}
	if len(gh.calls) != 0 {
		t.Errorf("expected no GitHub call on a head-resolution error, got %d", len(gh.calls))
	}
}

// headEntry builds a head-report audit entry carrying a head_sha payload.
func headEntry(category, headSHA string, seq int64) *audit.Entry {
	payload, _ := json.Marshal(map[string]any{"head_sha": headSHA})
	return &audit.Entry{Category: category, Sequence: seq, Payload: payload}
}

// fakeAuditReader is a minimal auditcheckpublisher.AuditReader for the #1682
// head-resolution tests.
type fakeAuditReader struct {
	byCat map[string][]*audit.Entry
	err   error
}

func (f *fakeAuditReader) ListForRunByCategory(_ context.Context, _ uuid.UUID, category string) ([]*audit.Entry, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.byCat[category], nil
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
	scope  forge.CredentialScope
	repo   forge.RepoRef
	params forge.CreateCheckRunParams
}

type fakeGitHub struct {
	mu    sync.Mutex
	calls []checkRunCall
	err   error
}

func (f *fakeGitHub) CreateCheckRun(_ context.Context, scope forge.CredentialScope, repo forge.RepoRef, p forge.CreateCheckRunParams) (*forge.CreateCheckRunResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, checkRunCall{scope: scope, repo: repo, params: p})
	if f.err != nil {
		return nil, f.err
	}
	return &forge.CreateCheckRunResult{ID: 1}, nil
}

// fakeGitLab is a minimal auditcheckpublisher.GitLabStatusForge for the
// gitlab_ci routing tests (#1861). ResolveRepoScope returns `scope` (or
// `resolveErr`); CreateCheckRun records the (scope, repo, params) triple the
// publisher routed to GitLab.
type fakeGitLab struct {
	mu         sync.Mutex
	scope      forge.CredentialScope
	resolveErr error
	calls      []checkRunCall
	err        error
}

func (f *fakeGitLab) ResolveRepoScope(_ context.Context, _ forge.RepoRef) (forge.CredentialScope, error) {
	if f.resolveErr != nil {
		return forge.CredentialScope{}, f.resolveErr
	}
	return f.scope, nil
}

func (f *fakeGitLab) CreateCheckRun(_ context.Context, scope forge.CredentialScope, repo forge.RepoRef, p forge.CreateCheckRunParams) (*forge.CreateCheckRunResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, checkRunCall{scope: scope, repo: repo, params: p})
	if f.err != nil {
		return nil, f.err
	}
	return &forge.CreateCheckRunResult{ID: 1}, nil
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
