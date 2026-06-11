package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcheckpublisher"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergereconciler"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Verifies that the listStageChecks handler hooks the
// auditcheckpublisher when the gate declares
// fishhawk_audit_complete and the publisher is wired (#231). Other
// publisher behavior (dedup, skip-on-missing-PR, etc.) is unit-
// tested in internal/auditcheckpublisher; this only asserts the
// HTTP-handler → publisher wiring.

func TestListStageChecks_PublishesAuditCompleteToGitHub(t *testing.T) {
	rr := newOrchestratorRepo()
	r := rr.seedRun()
	r.InstallationID = ptrInt64(99)
	r.Repo = "x/y"

	plan := rr.seedStage(r.ID, 0, run.StageStateSucceeded)
	plan.Type = run.StageTypePlan
	impl := rr.seedStage(r.ID, 1, run.StageStateSucceeded)
	impl.Type = run.StageTypeImplement
	rev := rr.seedStage(r.ID, 2, run.StageStateAwaitingApproval)
	rev.Type = run.StageTypeReview
	rev.Gate = &run.Gate{
		Kind: run.GateKindApproval,
	}

	au := newAuditCompleteAuditFake()
	au.appendTrace(t, r.ID, plan.ID, "raw")
	au.appendTrace(t, r.ID, plan.ID, "redacted")
	au.appendTrace(t, r.ID, impl.ID, "raw")
	au.appendTrace(t, r.ID, impl.ID, "redacted")

	arts := newFakeArtifactRepo()
	seedPlanArtifact(arts, plan.ID)
	arts.all = append(arts.all, &artifact.Artifact{
		ID: uuid.New(), StageID: impl.ID,
		Kind:    artifact.KindPullRequest,
		Content: pullRequestArtifactBody("abc12345"),
	})

	// Build a server with the publisher wired through a fake
	// GitHub. We have to build the publisher manually (rather than
	// going through New) because the real cfg.GitHub is a typed
	// *githubclient.Client; the publisher's CheckRunCreator
	// interface lets us swap a fake.
	gh := newPublisherFakeGitHub()
	s := New(Config{
		Addr: "127.0.0.1:0", RunRepo: rr,
		AuditRepo:      au,
		ArtifactRepo:   arts,
		StageCheckRepo: newFakeStageCheckRepo(),
		ExternalURL:    "https://app.fishhawk.example.com",
	})
	s.auditCheckPublisher = auditcheckpublisher.New(auditcheckpublisher.Deps{
		GitHub:      gh,
		Runs:        rr,
		Artifacts:   arts,
		ExternalURL: "https://app.fishhawk.example.com",
	})

	url := "/v0/stages/" + rev.ID.String() + "/checks"
	req := httptest.NewRequest(http.MethodGet, url, nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}

	if got := gh.calls(); len(got) != 1 {
		t.Fatalf("expected 1 GitHub publish; got %d", len(got))
	}
	c := gh.calls()[0]
	if c.params.HeadSHA != "abc12345" {
		t.Errorf("head_sha = %q want abc12345", c.params.HeadSHA)
	}
	if c.params.Status != githubclient.CheckRunStatusCompleted {
		t.Errorf("status = %q want completed", c.params.Status)
	}
	if c.params.Conclusion != githubclient.CheckRunConclusionSuccess {
		t.Errorf("conclusion = %q want success", c.params.Conclusion)
	}
	if !strings.HasSuffix(c.params.DetailsURL, "/runs/"+r.ID.String()) {
		t.Errorf("details_url = %q (should end with /runs/<id>)", c.params.DetailsURL)
	}
}

func TestListStageChecks_NoPublisher_StillSucceeds(t *testing.T) {
	// Without ExternalURL configured, the publisher is nil and the
	// read endpoint should still return the synthetic row — no
	// GitHub call attempted.
	rr := newOrchestratorRepo()
	r := rr.seedRun()
	plan := rr.seedStage(r.ID, 0, run.StageStateSucceeded)
	plan.Type = run.StageTypePlan
	impl := rr.seedStage(r.ID, 1, run.StageStateSucceeded)
	impl.Type = run.StageTypeImplement
	rev := rr.seedStage(r.ID, 2, run.StageStateAwaitingApproval)
	rev.Type = run.StageTypeReview
	rev.Gate = &run.Gate{
		Kind: run.GateKindApproval,
	}

	au := newAuditCompleteAuditFake()
	arts := newFakeArtifactRepo()

	s := New(Config{
		Addr: "127.0.0.1:0", RunRepo: rr,
		AuditRepo: au, ArtifactRepo: arts,
		StageCheckRepo: newFakeStageCheckRepo(),
		// No ExternalURL → no publisher.
	})
	if s.auditCheckPublisher != nil {
		t.Fatal("publisher should be nil without ExternalURL/GitHub")
	}

	req := httptest.NewRequest(http.MethodGet, "/v0/stages/"+rev.ID.String()+"/checks", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
}

// TestReconcilerTick_HealsDroppedAuditCheckPublish is the #973 regression,
// end-to-end across the seam: a transient GitHub failure (the #971
// 401 shape) drops the fishhawk_audit_complete publish, and the next
// merge-reconciler tick must heal it through the REAL derivation →
// publisher → GitHub-client path (auditcomplete.Compute →
// auditcheckpublisher.Publish behind Server.RepublishAuditCheck),
// driven by a real mergereconciler.Ticker. Per-layer units would pass
// while this wiring breaks. The retry-enabling invariant — a FAILED
// publish must not record into the dedup cache — is what the second
// tick exercises here (also pinned by TestPublish_GitHubError_Returned
// in internal/auditcheckpublisher).
func TestReconcilerTick_HealsDroppedAuditCheckPublish(t *testing.T) {
	rr := newOrchestratorRepo()
	r := rr.seedRun()
	r.InstallationID = ptrInt64(99)
	r.Repo = "x/y"
	prURL := "https://github.com/x/y/pull/1"
	r.PullRequestURL = &prURL

	plan := rr.seedStage(r.ID, 0, run.StageStateSucceeded)
	plan.Type = run.StageTypePlan
	impl := rr.seedStage(r.ID, 1, run.StageStateSucceeded)
	impl.Type = run.StageTypeImplement
	rev := rr.seedStage(r.ID, 2, run.StageStateAwaitingApproval)
	rev.Type = run.StageTypeReview
	rev.Gate = &run.Gate{
		Kind: run.GateKindApproval,
	}

	au := newAuditCompleteAuditFake()
	au.appendTrace(t, r.ID, plan.ID, "raw")
	au.appendTrace(t, r.ID, plan.ID, "redacted")
	au.appendTrace(t, r.ID, impl.ID, "raw")
	au.appendTrace(t, r.ID, impl.ID, "redacted")

	arts := newFakeArtifactRepo()
	seedPlanArtifact(arts, plan.ID)
	arts.all = append(arts.all, &artifact.Artifact{
		ID: uuid.New(), StageID: impl.ID,
		Kind:    artifact.KindPullRequest,
		Content: pullRequestArtifactBody("abc12345"),
	})

	gh := &flakyCheckRunGitHub{failuresLeft: 1}
	s := New(Config{
		Addr: "127.0.0.1:0", RunRepo: rr,
		AuditRepo:      au,
		ArtifactRepo:   arts,
		StageCheckRepo: newFakeStageCheckRepo(),
		ExternalURL:    "https://app.fishhawk.example.com",
	})
	s.auditCheckPublisher = auditcheckpublisher.New(auditcheckpublisher.Deps{
		GitHub:      gh,
		Runs:        rr,
		Artifacts:   arts,
		ExternalURL: "https://app.fishhawk.example.com",
	})

	ticker := &mergereconciler.Ticker{
		Runs:                  &awaitingStagesRepo{orchestratorRepo: rr, awaiting: []*run.Stage{rev}},
		PRGetter:              openPRGetter{},
		Resolver:              s,
		AuditCheckRepublisher: s,
	}

	// First tick: the publish attempt fails 401-shaped. No check run
	// is recorded, and the failure must not poison the dedup cache.
	ticker.Tick(context.Background())
	if got := gh.calls(); len(got) != 0 {
		t.Fatalf("after failed publish: %d check runs created, want 0", len(got))
	}
	if got := gh.failedCalls(); got != 1 {
		t.Fatalf("failed publish attempts = %d, want 1 (publish not attempted?)", got)
	}

	// Second tick, GitHub recovered: the sweep retries the dropped
	// publish and the check run lands with the pass conclusion.
	ticker.Tick(context.Background())
	got := gh.calls()
	if len(got) != 1 {
		t.Fatalf("after recovery tick: %d check runs created, want 1", len(got))
	}
	c := got[0]
	if c.params.HeadSHA != "abc12345" {
		t.Errorf("head_sha = %q want abc12345", c.params.HeadSHA)
	}
	if c.params.Status != githubclient.CheckRunStatusCompleted {
		t.Errorf("status = %q want completed", c.params.Status)
	}
	if c.params.Conclusion != githubclient.CheckRunConclusionSuccess {
		t.Errorf("conclusion = %q want success", c.params.Conclusion)
	}
}

// --- helpers ---

// awaitingStagesRepo overrides the orchestratorRepo's no-op
// ListReviewStagesAwaitingApproval with a canned parked-stage list so a
// real mergereconciler.Ticker can be driven against the fake repo.
type awaitingStagesRepo struct {
	*orchestratorRepo
	awaiting []*run.Stage
}

func (r *awaitingStagesRepo) ListReviewStagesAwaitingApproval(context.Context) ([]*run.Stage, error) {
	return r.awaiting, nil
}

// openPRGetter satisfies mergereconciler.PRGetter with a permanently
// open PR — the reconciler leaves the stage parked, isolating the
// audit-check heal path from the merge-resolution path.
type openPRGetter struct{}

func (openPRGetter) GetPullRequest(context.Context, int64, githubclient.RepoRef, int) (*githubclient.PullRequest, error) {
	return &githubclient.PullRequest{State: "open"}, nil
}

// flakyCheckRunGitHub fails the first failuresLeft CreateCheckRun calls
// with a 401-shaped error (the #971 incident shape), then delegates to
// the recording fake.
type flakyCheckRunGitHub struct {
	publisherFakeGitHub
	failMu       sync.Mutex
	failuresLeft int
	failed       int
}

func (f *flakyCheckRunGitHub) failedCalls() int {
	f.failMu.Lock()
	defer f.failMu.Unlock()
	return f.failed
}

func (f *flakyCheckRunGitHub) CreateCheckRun(ctx context.Context, installationID int64, repo githubclient.RepoRef, p githubclient.CreateCheckRunParams) (*githubclient.CreateCheckRunResult, error) {
	f.failMu.Lock()
	if f.failuresLeft > 0 {
		f.failuresLeft--
		f.failed++
		f.failMu.Unlock()
		return nil, errors.New("POST /repos/x/y/check-runs: 401 Bad credentials")
	}
	f.failMu.Unlock()
	return f.publisherFakeGitHub.CreateCheckRun(ctx, installationID, repo, p)
}

func pullRequestArtifactBody(headSHA string) []byte {
	b, _ := json.Marshal(map[string]any{
		"pr_number": 1, "pr_url": "https://github.com/x/y/pull/1",
		"branch": "feat", "head_sha": headSHA, "base_sha": "0",
		"title": "t", "files_changed_count": 1,
	})
	return b
}

type publisherFakeCall struct {
	installationID int64
	repo           githubclient.RepoRef
	params         githubclient.CreateCheckRunParams
}

type publisherFakeGitHub struct {
	mu     sync.Mutex
	stored []publisherFakeCall
}

func newPublisherFakeGitHub() *publisherFakeGitHub { return &publisherFakeGitHub{} }

func (f *publisherFakeGitHub) calls() []publisherFakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]publisherFakeCall, len(f.stored))
	copy(out, f.stored)
	return out
}

func (f *publisherFakeGitHub) CreateCheckRun(_ context.Context, installationID int64, repo githubclient.RepoRef, p githubclient.CreateCheckRunParams) (*githubclient.CreateCheckRunResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stored = append(f.stored, publisherFakeCall{installationID: installationID, repo: repo, params: p})
	return &githubclient.CreateCheckRunResult{ID: 1, HTMLURL: "https://github.com/" + repo.String() + "/runs/1"}, nil
}
