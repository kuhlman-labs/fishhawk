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
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
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

// --- Persistent-failure episode surfacing (#993) ---

// TestRepublishAuditCheck_PersistentFailure_DegradedThenRecoveredOnRunChain
// is the cross-boundary #993 test: a sustained CreateCheckRun failure
// streak crosses the publisher's threshold, flows through the server's
// OnDegraded callback, and lands as exactly ONE chained
// audit_check_publish_degraded entry on the run — then the eventual
// successful publish appends exactly one paired
// audit_check_publish_recovered entry. Per-layer units can't catch a
// forgotten callback wiring or a payload-shape mismatch; this drives
// RepublishAuditCheck (the reconciler's per-tick entry point) across
// the whole seam.
func TestRepublishAuditCheck_PersistentFailure_DegradedThenRecoveredOnRunChain(t *testing.T) {
	rr := newOrchestratorRepo()
	au := newAuditCompleteAuditFake()
	arts := newFakeArtifactRepo()
	r := seedPublishableRun(t, rr, au, arts, "abc12345")

	gh := &flakyCheckRunGitHub{failuresLeft: 1 << 30}
	s := New(Config{
		Addr: "127.0.0.1:0", RunRepo: rr,
		AuditRepo:      au,
		ArtifactRepo:   arts,
		StageCheckRepo: newFakeStageCheckRepo(),
		ExternalURL:    "https://app.fishhawk.example.com",
	})
	wireEpisodePublisher(s, gh, rr, arts)

	ctx := context.Background()
	for i := 0; i < auditcheckpublisher.DefaultDegradedThreshold; i++ {
		s.RepublishAuditCheck(ctx, r.ID)
	}
	degraded := listEpisodeEntries(t, au, r.ID, CategoryAuditCheckPublishDegraded)
	if len(degraded) != 1 {
		t.Fatalf("degraded entries = %d, want 1", len(degraded))
	}
	if degraded[0].ActorKind == nil || *degraded[0].ActorKind != audit.ActorSystem {
		t.Errorf("degraded actor_kind = %v, want system", degraded[0].ActorKind)
	}
	p := decodeEpisodePayload(t, degraded[0])
	if p.HeadSHA != "abc12345" {
		t.Errorf("degraded head_sha = %q, want abc12345", p.HeadSHA)
	}
	if p.Attempts != auditcheckpublisher.DefaultDegradedThreshold {
		t.Errorf("degraded attempts = %d, want %d", p.Attempts, auditcheckpublisher.DefaultDegradedThreshold)
	}
	if !strings.Contains(p.LastError, "401") {
		t.Errorf("degraded last_error = %q, should carry the GitHub error", p.LastError)
	}

	// A further failing sweep appends nothing new — once per episode.
	s.RepublishAuditCheck(ctx, r.ID)
	if got := listEpisodeEntries(t, au, r.ID, CategoryAuditCheckPublishDegraded); len(got) != 1 {
		t.Fatalf("after extra failing sweep: degraded entries = %d, want 1", len(got))
	}
	if got := listEpisodeEntries(t, au, r.ID, CategoryAuditCheckPublishRecovered); len(got) != 0 {
		t.Fatalf("recovered entries = %d, want 0 before recovery", len(got))
	}

	// GitHub recovers: the next sweep publishes the check run AND
	// closes the episode with exactly one recovered entry.
	gh.failMu.Lock()
	gh.failuresLeft = 0
	gh.failMu.Unlock()
	s.RepublishAuditCheck(ctx, r.ID)
	if got := gh.calls(); len(got) != 1 {
		t.Fatalf("check runs created = %d, want 1", len(got))
	}
	recovered := listEpisodeEntries(t, au, r.ID, CategoryAuditCheckPublishRecovered)
	if len(recovered) != 1 {
		t.Fatalf("recovered entries = %d, want 1", len(recovered))
	}
	rp := decodeEpisodePayload(t, recovered[0])
	if rp.HeadSHA != "abc12345" {
		t.Errorf("recovered head_sha = %q, want abc12345", rp.HeadSHA)
	}
	if rp.Attempts != auditcheckpublisher.DefaultDegradedThreshold+1 {
		t.Errorf("recovered attempts = %d, want %d", rp.Attempts, auditcheckpublisher.DefaultDegradedThreshold+1)
	}

	// Steady-state sweeps dedup the publish and append nothing.
	s.RepublishAuditCheck(ctx, r.ID)
	if got := listEpisodeEntries(t, au, r.ID, CategoryAuditCheckPublishRecovered); len(got) != 1 {
		t.Fatalf("after steady-state sweep: recovered entries = %d, want 1", len(got))
	}
}

// TestRepublishAuditCheck_RestartMidEpisode_PairsRecoveredFromChain pins
// the restart-proofing amendment: the audit chain — not the publisher's
// in-memory counter — is the durable episode state. A fresh Server +
// publisher (simulating a daemon restart mid-outage) against the same
// audit repo must still close the open episode on its first successful
// publish, exactly once.
func TestRepublishAuditCheck_RestartMidEpisode_PairsRecoveredFromChain(t *testing.T) {
	rr := newOrchestratorRepo()
	au := newAuditCompleteAuditFake()
	arts := newFakeArtifactRepo()
	r := seedPublishableRun(t, rr, au, arts, "abc12345")

	gh := &flakyCheckRunGitHub{failuresLeft: 1 << 30}
	cfg := Config{
		Addr: "127.0.0.1:0", RunRepo: rr,
		AuditRepo:      au,
		ArtifactRepo:   arts,
		StageCheckRepo: newFakeStageCheckRepo(),
		ExternalURL:    "https://app.fishhawk.example.com",
	}
	ctx := context.Background()

	s1 := New(cfg)
	wireEpisodePublisher(s1, gh, rr, arts)
	for i := 0; i < auditcheckpublisher.DefaultDegradedThreshold; i++ {
		s1.RepublishAuditCheck(ctx, r.ID)
	}
	if got := listEpisodeEntries(t, au, r.ID, CategoryAuditCheckPublishDegraded); len(got) != 1 {
		t.Fatalf("degraded entries = %d, want 1", len(got))
	}

	// "Restart": a brand-new Server and publisher with empty in-memory
	// episode state, sharing the durable audit repo.
	s2 := New(cfg)
	wireEpisodePublisher(s2, gh, rr, arts)
	gh.failMu.Lock()
	gh.failuresLeft = 0
	gh.failMu.Unlock()

	s2.RepublishAuditCheck(ctx, r.ID)
	recovered := listEpisodeEntries(t, au, r.ID, CategoryAuditCheckPublishRecovered)
	if len(recovered) != 1 {
		t.Fatalf("recovered entries = %d, want 1 (restart orphaned the episode)", len(recovered))
	}
	rp := decodeEpisodePayload(t, recovered[0])
	if rp.HeadSHA != "abc12345" {
		t.Errorf("recovered head_sha = %q, want abc12345", rp.HeadSHA)
	}
	if rp.Attempts != 0 {
		t.Errorf("recovered attempts = %d, want 0 (no in-process streak after restart)", rp.Attempts)
	}

	// The closed episode stays closed on subsequent sweeps.
	s2.RepublishAuditCheck(ctx, r.ID)
	if got := listEpisodeEntries(t, au, r.ID, CategoryAuditCheckPublishRecovered); len(got) != 1 {
		t.Fatalf("recovered entries = %d, want exactly 1", len(got))
	}
	if got := listEpisodeEntries(t, au, r.ID, CategoryAuditCheckPublishDegraded); len(got) != 1 {
		t.Fatalf("degraded entries = %d, want exactly 1", len(got))
	}
}

// TestRepublishAuditCheck_SharedHeadSHA_EpisodeOnDegradedRunOnly pins the
// (run_id, head_sha) episode keying amendment: two runs sharing a repo
// AND head commit have independent episodes. Run A degrades while run B
// publishes clean — exactly one degraded entry lands, on run A's chain,
// and B's clean publish neither closes A's episode nor appends a
// recovered entry on B.
func TestRepublishAuditCheck_SharedHeadSHA_EpisodeOnDegradedRunOnly(t *testing.T) {
	rr := newOrchestratorRepo()
	base := newAuditCompleteAuditFake()
	au := &perRunAuditFake{auditCompleteAuditFake: base}
	arts := newFakeArtifactRepo()
	const sharedSHA = "abc12345"
	runA := seedPublishableRun(t, rr, base, arts, sharedSHA)
	runB := seedPublishableRun(t, rr, base, arts, sharedSHA)

	gh := &selectiveFailGitHub{failSubstr: runA.ID.String()}
	s := New(Config{
		Addr: "127.0.0.1:0", RunRepo: rr,
		AuditRepo:      au,
		ArtifactRepo:   arts,
		StageCheckRepo: newFakeStageCheckRepo(),
		ExternalURL:    "https://app.fishhawk.example.com",
	})
	wireEpisodePublisher(s, gh, rr, arts)

	ctx := context.Background()
	for i := 0; i < auditcheckpublisher.DefaultDegradedThreshold; i++ {
		s.RepublishAuditCheck(ctx, runA.ID)
	}
	// Run B publishes clean against the same repo + head.
	s.RepublishAuditCheck(ctx, runB.ID)

	if got := listEpisodeEntries(t, au, runA.ID, CategoryAuditCheckPublishDegraded); len(got) != 1 {
		t.Fatalf("run A degraded entries = %d, want 1", len(got))
	}
	if got := listEpisodeEntries(t, au, runB.ID, CategoryAuditCheckPublishDegraded); len(got) != 0 {
		t.Fatalf("run B degraded entries = %d, want 0", len(got))
	}
	// B never had an open episode, so its clean publish appends no
	// recovered entry; A is still failing, so none lands there either.
	if got := listEpisodeEntries(t, au, runA.ID, CategoryAuditCheckPublishRecovered); len(got) != 0 {
		t.Fatalf("run A recovered entries = %d, want 0", len(got))
	}
	if got := listEpisodeEntries(t, au, runB.ID, CategoryAuditCheckPublishRecovered); len(got) != 0 {
		t.Fatalf("run B recovered entries = %d, want 0", len(got))
	}
	// And B's check run actually landed, pointed at B's run page.
	calls := gh.calls()
	if len(calls) != 1 {
		t.Fatalf("check runs created = %d, want 1 (run B only)", len(calls))
	}
	if !strings.Contains(calls[0].params.DetailsURL, runB.ID.String()) {
		t.Errorf("published details_url = %q, want run B's page", calls[0].params.DetailsURL)
	}
}

// --- helpers ---

// seedPublishableRun seeds a run with succeeded plan + implement
// stages, a parked review stage, the trace entries auditcomplete's
// rules need, and a pull_request artifact carrying headSHA —
// everything RepublishAuditCheck needs to derive StatePass and reach
// the CreateCheckRun attempt.
func seedPublishableRun(t *testing.T, rr *orchestratorRepo, au *auditCompleteAuditFake, arts *fakeArtifactRepo, headSHA string) *run.Run {
	t.Helper()
	r := rr.seedRun()
	r.InstallationID = ptrInt64(99)
	r.Repo = "x/y"
	plan := rr.seedStage(r.ID, 0, run.StageStateSucceeded)
	plan.Type = run.StageTypePlan
	impl := rr.seedStage(r.ID, 1, run.StageStateSucceeded)
	impl.Type = run.StageTypeImplement
	rev := rr.seedStage(r.ID, 2, run.StageStateAwaitingApproval)
	rev.Type = run.StageTypeReview
	rev.Gate = &run.Gate{Kind: run.GateKindApproval}
	au.appendTrace(t, r.ID, plan.ID, "raw")
	au.appendTrace(t, r.ID, plan.ID, "redacted")
	au.appendTrace(t, r.ID, impl.ID, "raw")
	au.appendTrace(t, r.ID, impl.ID, "redacted")
	seedPlanArtifact(arts, plan.ID)
	arts.all = append(arts.all, &artifact.Artifact{
		ID: uuid.New(), StageID: impl.ID,
		Kind:    artifact.KindPullRequest,
		Content: pullRequestArtifactBody(headSHA),
	})
	return r
}

// wireEpisodePublisher mirrors New()'s publisher construction —
// including the #993 episode callbacks — against a fake GitHub. We
// can't go through New for the GitHub side because the real
// cfg.GitHub is a typed *githubclient.Client.
func wireEpisodePublisher(s *Server, gh auditcheckpublisher.CheckRunCreator, rr run.Repository, arts artifact.Repository) {
	s.auditCheckPublisher = auditcheckpublisher.New(auditcheckpublisher.Deps{
		GitHub:      gh,
		Runs:        rr,
		Artifacts:   arts,
		ExternalURL: "https://app.fishhawk.example.com",
		OnDegraded:  s.auditCheckPublishDegraded,
		OnRecovered: s.auditCheckPublishRecovered,
	})
}

type episodePayload struct {
	HeadSHA   string `json:"head_sha"`
	Attempts  int    `json:"attempts"`
	LastError string `json:"last_error"`
}

func decodeEpisodePayload(t *testing.T, e *audit.Entry) episodePayload {
	t.Helper()
	var p episodePayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		t.Fatalf("decode episode payload: %v", err)
	}
	return p
}

func listEpisodeEntries(t *testing.T, repo audit.Repository, runID uuid.UUID, category string) []*audit.Entry {
	t.Helper()
	entries, err := repo.ListForRunByCategory(context.Background(), runID, category)
	if err != nil {
		t.Fatalf("ListForRunByCategory(%s): %v", category, err)
	}
	out := []*audit.Entry{}
	for _, e := range entries {
		if e.RunID != nil && *e.RunID == runID {
			out = append(out, e)
		}
	}
	return out
}

// perRunAuditFake tightens auditCompleteAuditFake's
// ListForRunByCategory to actually filter by run — the shared fake
// ignores the run argument, which would let one run's degraded entry
// open or close another run's episode in the two-run test.
type perRunAuditFake struct {
	*auditCompleteAuditFake
}

func (f *perRunAuditFake) ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	entries, err := f.auditCompleteAuditFake.ListForRunByCategory(ctx, runID, category)
	if err != nil {
		return nil, err
	}
	out := []*audit.Entry{}
	for _, e := range entries {
		if e.RunID != nil && *e.RunID == runID {
			out = append(out, e)
		}
	}
	return out, nil
}

// selectiveFailGitHub fails CreateCheckRun only when the params'
// details_url names the configured run id — the one publisher-visible
// place a publish reveals which run it belongs to — and delegates the
// rest to the recording fake.
type selectiveFailGitHub struct {
	publisherFakeGitHub
	failSubstr string
	failMu     sync.Mutex
	failed     int
}

func (f *selectiveFailGitHub) CreateCheckRun(ctx context.Context, installationID int64, repo githubclient.RepoRef, p githubclient.CreateCheckRunParams) (*githubclient.CreateCheckRunResult, error) {
	if strings.Contains(p.DetailsURL, f.failSubstr) {
		f.failMu.Lock()
		f.failed++
		f.failMu.Unlock()
		return nil, errors.New("POST /repos/x/y/check-runs: 401 Bad credentials")
	}
	return f.publisherFakeGitHub.CreateCheckRun(ctx, installationID, repo, p)
}

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
