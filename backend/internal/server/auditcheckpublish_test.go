package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcheckpublisher"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
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

// --- helpers ---

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
