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
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcheckpublisher"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergereconciler"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
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
// payload-shape mismatch; this drives RepublishAuditCheck (the
// reconciler's per-tick entry point) across the whole seam. The New()
// callback wiring proper is pinned separately by
// TestRepublishAuditCheck_EpisodeCallbacksWiredByNew.
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

// TestRepublishAuditCheck_EpisodeCallbacksWiredByNew asserts the
// PRODUCTION wiring: New() itself attaches the #993 episode callbacks
// to the publisher it builds from cfg.GitHub. Unlike the other episode
// tests (which rebuild the publisher around a CheckRunCreator fake for
// brevity), this one never touches s.auditCheckPublisher — cfg.GitHub
// is a real *githubclient.Client pointed at an httptest fake of
// api.github.com — so a forgotten OnDegraded/OnRecovered in New()
// fails here and nowhere else.
func TestRepublishAuditCheck_EpisodeCallbacksWiredByNew(t *testing.T) {
	rr := newOrchestratorRepo()
	au := newAuditCompleteAuditFake()
	arts := newFakeArtifactRepo()
	r := seedPublishableRun(t, rr, au, arts, "abc12345")

	api := &checkRunsAPIFake{failing: true}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /repos/x/y/check-runs", api.handle)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	s := New(Config{
		Addr: "127.0.0.1:0", RunRepo: rr,
		AuditRepo:      au,
		ArtifactRepo:   arts,
		StageCheckRepo: newFakeStageCheckRepo(),
		ExternalURL:    "https://app.fishhawk.example.com",
		GitHub: &githubclient.Client{
			BaseURL: srv.URL,
			Tokens:  &fakeTokenProvider{tok: "ghs_t"},
			HTTP:    &http.Client{Timeout: 5 * time.Second},
		},
	})

	ctx := context.Background()
	for i := 0; i < auditcheckpublisher.DefaultDegradedThreshold; i++ {
		s.RepublishAuditCheck(ctx, r.ID)
	}
	degraded := listEpisodeEntries(t, au, r.ID, CategoryAuditCheckPublishDegraded)
	if len(degraded) != 1 {
		t.Fatalf("degraded entries = %d, want 1 (New() didn't wire OnDegraded?)", len(degraded))
	}
	p := decodeEpisodePayload(t, degraded[0])
	if p.HeadSHA != "abc12345" {
		t.Errorf("degraded head_sha = %q, want abc12345", p.HeadSHA)
	}
	if p.Attempts != auditcheckpublisher.DefaultDegradedThreshold {
		t.Errorf("degraded attempts = %d, want %d", p.Attempts, auditcheckpublisher.DefaultDegradedThreshold)
	}

	api.setFailing(false)
	s.RepublishAuditCheck(ctx, r.ID)
	if got := api.created(); got != 1 {
		t.Fatalf("check runs created on the wire = %d, want 1", got)
	}
	if got := api.lastHeadSHA(); got != "abc12345" {
		t.Errorf("posted check-run head_sha = %q, want abc12345", got)
	}
	if got := listEpisodeEntries(t, au, r.ID, CategoryAuditCheckPublishRecovered); len(got) != 1 {
		t.Fatalf("recovered entries = %d, want 1 (New() didn't wire OnRecovered?)", len(got))
	}
}

// TestRepublishAuditCheck_SharedHeadSHA_DedupHitClosesOpenEpisode pins
// the dedup-cache edge the (run_id, head_sha) episode keying alone
// can't close: run B degrades, GitHub recovers, and run A — sharing
// the repo AND head commit — publishes clean FIRST, recording the
// (repo, head_sha) dedup cache. Run B's next sweep is then a dedup
// no-op, but it must still append B's paired recovered entry: the
// state B wanted is live on GitHub, so B's episode is over.
func TestRepublishAuditCheck_SharedHeadSHA_DedupHitClosesOpenEpisode(t *testing.T) {
	rr := newOrchestratorRepo()
	base := newAuditCompleteAuditFake()
	au := &perRunAuditFake{auditCompleteAuditFake: base}
	arts := newFakeArtifactRepo()
	const sharedSHA = "abc12345"
	runA := seedPublishableRun(t, rr, base, arts, sharedSHA)
	runB := seedPublishableRun(t, rr, base, arts, sharedSHA)

	gh := &selectiveFailGitHub{failSubstr: runB.ID.String()}
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
		s.RepublishAuditCheck(ctx, runB.ID)
	}
	if got := listEpisodeEntries(t, au, runB.ID, CategoryAuditCheckPublishDegraded); len(got) != 1 {
		t.Fatalf("run B degraded entries = %d, want 1", len(got))
	}

	// GitHub recovers; run A publishes clean first.
	gh.failSubstr = "\x00never-matches"
	s.RepublishAuditCheck(ctx, runA.ID)
	if got := gh.calls(); len(got) != 1 {
		t.Fatalf("check runs created = %d, want 1 (run A's publish)", len(got))
	}

	// Run B's sweep dedup-hits — no new check run — but closes B's
	// open episode with exactly one recovered entry on B's chain.
	s.RepublishAuditCheck(ctx, runB.ID)
	if got := gh.calls(); len(got) != 1 {
		t.Fatalf("check runs created = %d, want 1 (dedup hit must not re-POST)", len(got))
	}
	recovered := listEpisodeEntries(t, au, runB.ID, CategoryAuditCheckPublishRecovered)
	if len(recovered) != 1 {
		t.Fatalf("run B recovered entries = %d, want 1 (dedup hit orphaned the episode)", len(recovered))
	}
	rp := decodeEpisodePayload(t, recovered[0])
	if rp.HeadSHA != sharedSHA {
		t.Errorf("recovered head_sha = %q, want %s", rp.HeadSHA, sharedSHA)
	}
	if rp.Attempts != auditcheckpublisher.DefaultDegradedThreshold {
		t.Errorf("recovered attempts = %d, want %d", rp.Attempts, auditcheckpublisher.DefaultDegradedThreshold)
	}
	// Idempotent: a further sweep appends nothing, and run A (which
	// never had an episode) carries no recovered entry.
	s.RepublishAuditCheck(ctx, runB.ID)
	if got := listEpisodeEntries(t, au, runB.ID, CategoryAuditCheckPublishRecovered); len(got) != 1 {
		t.Fatalf("run B recovered entries = %d, want exactly 1", len(got))
	}
	if got := listEpisodeEntries(t, au, runA.ID, CategoryAuditCheckPublishRecovered); len(got) != 0 {
		t.Fatalf("run A recovered entries = %d, want 0", len(got))
	}
}

// --- helpers ---

// checkRunsAPIFake is an httptest handler for GitHub's
// POST /repos/{owner}/{repo}/check-runs: 401 while failing, then 201
// with a minimal check-run body. Used by the New()-wiring test, which
// needs a real *githubclient.Client rather than a CheckRunCreator fake.
type checkRunsAPIFake struct {
	mu      sync.Mutex
	failing bool
	posts   int
	headSHA string
}

func (f *checkRunsAPIFake) setFailing(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failing = v
}

func (f *checkRunsAPIFake) created() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.posts
}

func (f *checkRunsAPIFake) lastHeadSHA() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.headSHA
}

func (f *checkRunsAPIFake) handle(w http.ResponseWriter, r *http.Request) {
	var body struct {
		HeadSHA string `json:"head_sha"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	failing := f.failing
	if !failing {
		f.posts++
		f.headSHA = body.HeadSHA
	}
	f.mu.Unlock()
	if failing {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
		return
	}
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write([]byte(`{"id":1,"html_url":"https://github.com/x/y/runs/1"}`))
}

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
// including the #993 episode callbacks — against an in-process
// CheckRunCreator fake, which is lighter than standing up an httptest
// api.github.com for cfg.GitHub. The production New() wiring itself is
// asserted by TestRepublishAuditCheck_EpisodeCallbacksWiredByNew,
// which goes through cfg.GitHub and never touches s.auditCheckPublisher.
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

func (f *selectiveFailGitHub) CreateCheckRun(ctx context.Context, scope forge.CredentialScope, repo githubclient.RepoRef, p githubclient.CreateCheckRunParams) (*githubclient.CreateCheckRunResult, error) {
	if strings.Contains(p.DetailsURL, f.failSubstr) {
		f.failMu.Lock()
		f.failed++
		f.failMu.Unlock()
		return nil, errors.New("POST /repos/x/y/check-runs: 401 Bad credentials")
	}
	return f.publisherFakeGitHub.CreateCheckRun(ctx, scope, repo, p)
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

func (openPRGetter) GetPullRequest(context.Context, forge.CredentialScope, githubclient.RepoRef, int) (*githubclient.PullRequest, error) {
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

func (f *flakyCheckRunGitHub) CreateCheckRun(ctx context.Context, scope forge.CredentialScope, repo githubclient.RepoRef, p githubclient.CreateCheckRunParams) (*githubclient.CreateCheckRunResult, error) {
	f.failMu.Lock()
	if f.failuresLeft > 0 {
		f.failuresLeft--
		f.failed++
		f.failMu.Unlock()
		return nil, errors.New("POST /repos/x/y/check-runs: 401 Bad credentials")
	}
	f.failMu.Unlock()
	return f.publisherFakeGitHub.CreateCheckRun(ctx, scope, repo, p)
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
	scope  forge.CredentialScope
	repo   githubclient.RepoRef
	params githubclient.CreateCheckRunParams
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

func (f *publisherFakeGitHub) CreateCheckRun(_ context.Context, scope forge.CredentialScope, repo githubclient.RepoRef, p githubclient.CreateCheckRunParams) (*githubclient.CreateCheckRunResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stored = append(f.stored, publisherFakeCall{scope: scope, repo: repo, params: p})
	return &githubclient.CreateCheckRunResult{ID: 1, HTMLURL: "https://github.com/" + repo.String() + "/runs/1"}, nil
}

// --- E64.42 / #3159: republish on run termination ---------------------------
//
// A fix-up push publishes fishhawk_audit_complete as `in_progress` against the
// NEW head, and nothing between that synchronize and the merge is guaranteed
// to recompute it. The merge reconciler's heal sweep enumerates only review
// stages PARKED at awaiting_approval, so merging removes the run from the only
// retry path and the check stays in_progress on the merged head forever.
//
// The tests below drive that shape end to end through the REAL handlers. THE
// TEST ISSUES NO `GET /v0/stages/{id}/checks` REQUEST ANYWHERE — that read
// endpoint recomputes and republishes, so touching it would heal the check and
// green the test vacuously. Its absence is the whole point and is structural:
// no such request is ever constructed.

// fixupRepublishFixture seeds the stranding shape by construction and returns
// the server, the fakes, the run, and the two stages the test drives.
//
// Shape: plan succeeded; implement RUNNING (so the synchronize-time recompute
// is genuinely mid-flight → pending → in_progress); review parked at
// awaiting_approval; acceptance PENDING with an empty approved plan, so the
// orchestrator's Advance short-circuits it to succeeded — this is what makes
// the "republish AFTER advanceRunAfterReviewResolve" ordering observable: a
// non-review stage that is non-terminal BEFORE the Advance and terminal after.
//
// The PR-open artifact head and the fixup_pushed head are DELIBERATELY
// different values, so an assertion on the published head distinguishes the
// fix-up head resolution (#1682) from the stale artifact head.
func fixupRepublishFixture(t *testing.T, withReviewStage bool) (
	*Server, *orchestratorRepo, *auditCompleteAuditFake, *publisherFakeGitHub, *run.Run, *run.Stage, *run.Stage,
) {
	t.Helper()
	rr := newOrchestratorRepo()
	r := rr.seedRun()
	r.InstallationID = ptrInt64(99)
	r.Repo = "x/y"
	prURL := fixupRepublishPRURL
	r.PullRequestURL = &prURL

	planStage := rr.seedStage(r.ID, 0, run.StageStateSucceeded)
	planStage.Type = run.StageTypePlan
	impl := rr.seedStage(r.ID, 1, run.StageStateRunning)
	impl.Type = run.StageTypeImplement
	var rev *run.Stage
	if withReviewStage {
		rev = rr.seedStage(r.ID, 2, run.StageStateAwaitingApproval)
		rev.Type = run.StageTypeReview
		rev.Gate = &run.Gate{Kind: run.GateKindApproval}
	}
	// A PENDING acceptance stage is what makes the "republish AFTER
	// advanceRunAfterReviewResolve" ordering observable on the REVIEW arm: the
	// orchestrator's Advance short-circuits it to succeeded (the approved plan
	// declares zero criteria and zero out_of_scope), so it is a non-review
	// stage that is non-terminal BEFORE the Advance and terminal after —
	// exactly the discriminator a hoist mutation trips. The implement-only
	// shape has no acceptance stage (routine_change is implement-only), which
	// is also why that arm's Advance is conditional; see its own test.
	var acc *run.Stage
	if withReviewStage {
		acc = rr.seedStage(r.ID, 3, run.StageStatePending)
		acc.Type = run.StageTypeAcceptance
	}

	au := newAuditCompleteAuditFake()
	au.appendTrace(t, r.ID, planStage.ID, "raw")
	au.appendTrace(t, r.ID, planStage.ID, "redacted")
	au.appendTrace(t, r.ID, impl.ID, "raw")
	au.appendTrace(t, r.ID, impl.ID, "redacted")
	// The fix-up push: a head-report entry carrying a head the PR-open
	// artifact does not, so findHeadSHA (#1682) resolves THIS sha.
	fixupPayload, _ := json.Marshal(map[string]any{"head_sha": fixupHeadSHA, "branch": "feat"})
	au.appendChained(t, r.ID, &impl.ID, "fixup_pushed", fixupPayload)

	arts := newFakeArtifactRepo()
	seedPlanArtifact(arts, planStage.ID)
	arts.all = append(arts.all, &artifact.Artifact{
		ID: uuid.New(), StageID: impl.ID,
		Kind:    artifact.KindPullRequest,
		Content: pullRequestArtifactBody(prOpenHeadSHA),
	})

	gh := newPublisherFakeGitHub()
	s := New(Config{
		Addr: "127.0.0.1:0", RunRepo: rr,
		AuditRepo:      au,
		ArtifactRepo:   arts,
		StageCheckRepo: newFakeStageCheckRepo(),
		ExternalURL:    "https://app.fishhawk.example.com",
		Orchestrator: &orchestrator.Orchestrator{
			Runs: rr, Artifacts: arts, Audit: au,
		},
	})
	// Audit wired so findHeadSHA prefers the fixup_pushed head (#1682) — the
	// production publisher gets the same dep from New().
	s.auditCheckPublisher = auditcheckpublisher.New(auditcheckpublisher.Deps{
		GitHub:      gh,
		Runs:        rr,
		Artifacts:   arts,
		Audit:       au,
		ExternalURL: "https://app.fishhawk.example.com",
	})
	return s, rr, au, gh, r, impl, acc
}

const (
	fixupRepublishPRURL = "https://github.com/x/y/pull/1"
	// Deliberately distinct so the published head discriminates the fix-up
	// head resolution from the stale PR-open artifact head.
	prOpenHeadSHA = "0penhead"
	fixupHeadSHA  = "f1xuphead"
)

// closedMergedPayload is the pull_request.closed(merged) webhook body the
// tests drive handlePullRequestClosed with.
func closedMergedPayload(prURL, headSHA string) []byte {
	b, _ := json.Marshal(map[string]any{
		"action": "closed",
		"pull_request": map[string]any{
			"html_url": prURL,
			"number":   1,
			"merged":   true,
			"head":     map[string]any{"sha": headSHA},
			"base":     map[string]any{"sha": "base0"},
			"merged_by": map[string]any{
				"login": "operator",
			},
		},
		"sender": map[string]any{"login": "operator"},
	})
	return b
}

// synchronizeEvent builds the fix-up-push `pull_request.synchronize` delivery.
func synchronizeEvent(prURL, headSHA string) webhook.Event {
	raw, _ := json.Marshal(map[string]any{
		"action": "synchronize",
		"pull_request": map[string]any{
			"html_url": prURL,
			"number":   1,
			"head":     map[string]any{"sha": headSHA},
			"user":     map[string]any{"login": "fishhawk-dev[bot]", "type": "Bot"},
		},
	})
	return webhook.Event{
		Type: "pull_request", Action: "synchronize",
		Repo: "x/y", InstallationID: 99, RawBody: raw,
	}
}

// TestResolveReviewStageOnMerge_RepublishesAuditCheckOnTermination is THE
// end-to-end #3159 regression, driven across every layer the strand crosses:
// webhook handler → auditcomplete derivation → auditcheckpublisher → forge
// CheckRunCreator → orchestrator run completion. A per-layer unit passes while
// this seam strands.
//
// It asserts a TRANSITION, not a state: the FIRST CreateCheckRun (from the
// fix-up synchronize, while the implement stage is still running) is
// in_progress, and the LAST is completed/success — at the FIX-UP head, not the
// stale PR-open artifact head. Two publishes at the same head through one
// Publisher instance is also a dedup-suppression detector.
//
// NO `GET /v0/stages/{id}/checks` REQUEST IS MADE. That endpoint recomputes
// and republishes, which would heal the check and pass this test vacuously.
func TestResolveReviewStageOnMerge_RepublishesAuditCheckOnTermination(t *testing.T) {
	s, rr, _, gh, r, impl, acc := fixupRepublishFixture(t, true)
	ctx := context.Background()

	// 1. The fix-up push. The implement stage is still running, so the
	//    recompute is mid-flight → pending → in_progress on the NEW head.
	s.republishOnPullRequestEvent(ctx, synchronizeEvent(fixupRepublishPRURL, fixupHeadSHA))
	first := gh.calls()
	if len(first) != 1 {
		t.Fatalf("after fix-up synchronize: %d check runs, want 1", len(first))
	}
	if first[0].params.Status != githubclient.CheckRunStatusInProgress {
		t.Fatalf("first publish status = %q, want in_progress (fixture did not seed the stranding state)",
			first[0].params.Status)
	}
	if first[0].params.HeadSHA != fixupHeadSHA {
		t.Fatalf("first publish head_sha = %q, want the fix-up head %q", first[0].params.HeadSHA, fixupHeadSHA)
	}

	// 2. The implement stage settles. Nothing recomputes: the reconciler heal
	//    sweep only visits parked review stages, and no SPA read happens.
	impl.State = run.StageStateSucceeded
	if got := len(gh.calls()); got != 1 {
		t.Fatalf("stage settle published %d check runs, want 1 (test drove a heal path)", got)
	}

	// 3. The merge.
	s.handlePullRequestClosed(ctx, closedMergedPayload(fixupRepublishPRURL, fixupHeadSHA))

	// The run reached its terminal state — the precondition the republish
	// rides on, and proof the Advance really ran.
	if got := stageStateOnOrchestratorRepo(t, rr, r.ID, acc.ID); !got.IsTerminal() {
		t.Fatalf("acceptance stage state = %q, want terminal (Advance did not short-circuit it)", got)
	}
	if got, _ := rr.GetRun(ctx, r.ID); got.State != run.StateSucceeded {
		t.Fatalf("run state = %q, want succeeded", got.State)
	}

	calls := gh.calls()
	if len(calls) != 2 {
		t.Fatalf("check runs published = %d, want 2 (in_progress then terminal); statuses=%v",
			len(calls), publishStatuses(calls))
	}
	last := calls[len(calls)-1]
	if last.params.Status != githubclient.CheckRunStatusCompleted {
		t.Errorf("terminal publish status = %q, want completed", last.params.Status)
	}
	if last.params.Conclusion != githubclient.CheckRunConclusionSuccess {
		t.Errorf("terminal publish conclusion = %q, want success", last.params.Conclusion)
	}
	if last.params.HeadSHA != fixupHeadSHA {
		t.Errorf("terminal publish head_sha = %q, want the fix-up head %q (not the stale PR-open head %q)",
			last.params.HeadSHA, fixupHeadSHA, prOpenHeadSHA)
	}
}

// TestResolveReviewStageOnMerge_ImplementOnly_RepublishesAuditCheckOnTermination
// is the implement-only (routine_change-shaped) twin: the run carries NO
// review stage, so resolveReviewStageOnMerge takes its `reviewStage == nil`
// arm — a SEPARATE call site with its own conditional Advance. Deleting that
// arm's republish leaves the sibling test above green, which is why this one
// exists.
func TestResolveReviewStageOnMerge_ImplementOnly_RepublishesAuditCheckOnTermination(t *testing.T) {
	s, _, _, gh, _, impl, _ := fixupRepublishFixture(t, false)
	ctx := context.Background()

	s.republishOnPullRequestEvent(ctx, synchronizeEvent(fixupRepublishPRURL, fixupHeadSHA))
	if got := gh.calls(); len(got) != 1 || got[0].params.Status != githubclient.CheckRunStatusInProgress {
		t.Fatalf("after fix-up synchronize: %d calls, statuses=%v; want 1 in_progress",
			len(got), publishStatuses(got))
	}
	impl.State = run.StageStateSucceeded

	s.handlePullRequestClosed(ctx, closedMergedPayload(fixupRepublishPRURL, fixupHeadSHA))

	calls := gh.calls()
	if len(calls) != 2 {
		t.Fatalf("check runs published = %d, want 2 (in_progress then terminal); statuses=%v",
			len(calls), publishStatuses(calls))
	}
	last := calls[len(calls)-1]
	if last.params.Status != githubclient.CheckRunStatusCompleted ||
		last.params.Conclusion != githubclient.CheckRunConclusionSuccess {
		t.Errorf("terminal publish = %q/%q, want completed/success", last.params.Status, last.params.Conclusion)
	}
	if last.params.HeadSHA != fixupHeadSHA {
		t.Errorf("terminal publish head_sha = %q, want %q", last.params.HeadSHA, fixupHeadSHA)
	}
}

// TestResolveReviewStageOnMerge_HeldPendingReview_DoesNotRepublish pins the
// ADR-036 hold arm: when an agent implement review is still in flight the
// merge resolution RETURNS EARLY, leaving the review stage parked. That run is
// still inside the reconciler's heal sweep, so it needs no republish — and
// publishing there would post a pending state the sweep would only have to
// undo. Zero CreateCheckRun calls after the merge.
func TestResolveReviewStageOnMerge_HeldPendingReview_DoesNotRepublish(t *testing.T) {
	s, rr, au, gh, r, impl, _ := fixupRepublishFixture(t, true)
	ctx := context.Background()
	// Arm the ADR-036 hold BY CONSTRUCTION: the implement stage declares one
	// agent reviewer, a review was dispatched, and no terminal review entry
	// has landed — so checkImplementReviewSettled holds and the resolver
	// returns early, before any republish.
	r.WorkflowID = "feature_change"
	r.WorkflowSpec = specImplementReviewers(1)
	// Timestamp NOW: the shared appendChained helper stamps a fixed 2026-05-07
	// date, which is far enough in the past that the review backstop would
	// have elapsed and the gate would ALLOW the merge — the hold would never
	// engage and this test could not discriminate.
	if _, err := au.AppendChained(ctx, audit.ChainAppendParams{
		RunID: r.ID, Timestamp: time.Now().UTC(),
		Category: "implement_review_started", Payload: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("seed implement_review_started: %v", err)
	}
	impl.State = run.StageStateSucceeded

	s.handlePullRequestClosed(ctx, closedMergedPayload(fixupRepublishPRURL, fixupHeadSHA))

	// The DISCRIMINATOR, mirroring the cancelled-stage assertion in
	// TestResolveReviewStageOnMerge_ClosedWithoutMerge_DoesNotRepublish: pin
	// that the ADR-036 hold actually ENGAGED before reading the empty call
	// log. Without it a silent recompute/publish failure on the merged arm
	// would satisfy the zero-call assertion while testing nothing.
	if got := stageStateByTypeOnOrchestratorRepo(t, rr, r.ID, run.StageTypeReview); got != run.StageStateAwaitingApproval {
		t.Fatalf("review stage state = %q, want awaiting_approval (the ADR-036 hold did not engage, so the zero-call assertion below cannot discriminate)", got)
	}

	if got := gh.calls(); len(got) != 0 {
		t.Fatalf("held-pending-review merge published %d check runs, want 0; statuses=%v",
			len(got), publishStatuses(got))
	}
}

// TestResolveReviewStageOnMerge_RepublishFailure_StillCompletesRun pins the
// best-effort posture of the terminal republish in BOTH failure directions,
// because "republish never unwinds the merge" is a claim about the whole
// helper, not only about the forge.
//
// Two legs, sharing the completion assertions:
//
//   - publish_error — the FORGE half. CreateCheckRun fails permanently, so the
//     recompute succeeds and the publish is attempted and rejected. (Distinct
//     from TestMergeRun_RepublishFailure_StillDispatchesMerge, which pins the
//     same posture at the OTHER call site: republishAuditCheckBeforeMerge on
//     the operator merge endpoint. Same failure mode, different control.)
//
//   - audit_read_error — the RECOMPUTE half, and the one the previous shape of
//     this test promised but did not deliver. The audit READ that
//     auditcomplete.ComputeResult performs FIRST is made to fail, so the
//     recompute errors and NO publish is ever attempted. A broken audit-chain
//     read must not unwind or block the merge either.
//
// Each leg pins its injected failure as genuinely REACHED before asserting the
// completion outcome, so neither can pass while injecting nothing: the publish
// leg requires a failed CreateCheckRun call, the audit-read leg requires a
// failed read AND zero CreateCheckRun calls (the recompute error means the
// publish is never reached — which is also what distinguishes the two legs
// from each other rather than re-running one failure mode twice).
func TestResolveReviewStageOnMerge_RepublishFailure_StillCompletesRun(t *testing.T) {
	t.Run("publish_error", func(t *testing.T) {
		s, rr, au, _, r, impl, _ := fixupRepublishFixture(t, true)
		ctx := context.Background()
		// Swap in a permanently failing CheckRunCreator.
		gh := &flakyCheckRunGitHub{failuresLeft: 1 << 30}
		s.auditCheckPublisher = auditcheckpublisher.New(auditcheckpublisher.Deps{
			GitHub:      gh,
			Runs:        rr,
			Artifacts:   s.cfg.ArtifactRepo,
			Audit:       au,
			ExternalURL: "https://app.fishhawk.example.com",
		})
		impl.State = run.StageStateSucceeded

		s.handlePullRequestClosed(ctx, closedMergedPayload(fixupRepublishPRURL, fixupHeadSHA))

		if got := gh.failedCalls(); got == 0 {
			t.Fatal("republish was never attempted; this test cannot discriminate")
		}
		assertMergeSurvivedRepublishFailure(t, ctx, rr, au, r.ID, "publish")
	})

	t.Run("audit_read_error", func(t *testing.T) {
		s, rr, au, gh, r, impl, _ := fixupRepublishFixture(t, true)
		ctx := context.Background()
		// Wrap the audit repository the RECOMPUTE reads through
		// (auditCompleteDeps takes cfg.AuditRepo) so exactly one
		// category-scoped read fails. Writes and every other read pass
		// through unchanged, so the merge resolution's own pr_merged
		// append still lands — the failure is scoped to the recompute,
		// which is the path this leg is about.
		// CategoryAcceptanceSkippedOutOfScope is the FIRST audit read
		// auditcomplete.ComputeResult performs, so the recompute cannot
		// reach the publish without hitting it.
		failing := newAuditReadFailingFake(au, CategoryAcceptanceSkippedOutOfScope)
		s.cfg.AuditRepo = failing
		failing.arm()
		impl.State = run.StageStateSucceeded

		s.handlePullRequestClosed(ctx, closedMergedPayload(fixupRepublishPRURL, fixupHeadSHA))

		// Reached: the injected error was genuinely hit. A leg whose
		// injection is never exercised passes while asserting nothing.
		if got := failing.failedReads(); got == 0 {
			t.Fatal("the injected audit read never failed; this leg cannot discriminate")
		}
		// And it failed BEFORE the publish: a recompute error returns
		// early, so nothing is posted. This is what separates this leg
		// from the publish leg above rather than duplicating it.
		if got := gh.calls(); len(got) != 0 {
			t.Fatalf("audit-read failure still published %d check runs, want 0; statuses=%v",
				len(got), publishStatuses(got))
		}
		assertMergeSurvivedRepublishFailure(t, ctx, rr, au, r.ID, "audit read")
	})
}

// assertMergeSurvivedRepublishFailure is the completion contract both
// republish-failure legs share: the run still reaches its terminal state and
// the durable pr_merged row is still written. The republish is a tail, never a
// precondition.
func assertMergeSurvivedRepublishFailure(
	t *testing.T, ctx context.Context, rr *orchestratorRepo, au *auditCompleteAuditFake, runID uuid.UUID, failureKind string,
) {
	t.Helper()
	if got, _ := rr.GetRun(ctx, runID); got.State != run.StateSucceeded {
		t.Errorf("run state = %q, want succeeded (a %s failure must not unwind the merge)", got.State, failureKind)
	}
	if n := len(listEpisodeEntries(t, au, runID, "pr_merged")); n != 1 {
		t.Errorf("pr_merged rows = %d, want 1 (a %s failure must not unwind the merge audit row)", n, failureKind)
	}
}

// auditReadFailingFake wraps auditCompleteAuditFake and makes ONE
// category-scoped audit READ fail once armed, delegating every other read and
// every write to the real fake. Scoped rather than blanket so the merge
// resolution's own audit writes still land and the failure lands squarely on
// the recompute path under test.
type auditReadFailingFake struct {
	*auditCompleteAuditFake
	failMu   sync.Mutex
	category string
	armed    bool
	failed   int
}

func newAuditReadFailingFake(base *auditCompleteAuditFake, category string) *auditReadFailingFake {
	return &auditReadFailingFake{auditCompleteAuditFake: base, category: category}
}

func (f *auditReadFailingFake) arm() {
	f.failMu.Lock()
	defer f.failMu.Unlock()
	f.armed = true
}

func (f *auditReadFailingFake) failedReads() int {
	f.failMu.Lock()
	defer f.failMu.Unlock()
	return f.failed
}

func (f *auditReadFailingFake) ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	f.failMu.Lock()
	if f.armed && category == f.category {
		f.failed++
		f.failMu.Unlock()
		return nil, errors.New("audit: list for run by category: connection reset by peer")
	}
	f.failMu.Unlock()
	return f.auditCompleteAuditFake.ListForRunByCategory(ctx, runID, category)
}

// publishStatuses renders a call log's statuses for failure messages.
func publishStatuses(calls []publisherFakeCall) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, string(c.params.Status)+"/"+string(c.params.Conclusion))
	}
	return out
}

// stageStateByTypeOnOrchestratorRepo reads the COMMITTED state of the run's
// stage of the given type from the fake. Used where the fixture does not hand
// the test that stage's id back.
func stageStateByTypeOnOrchestratorRepo(t *testing.T, rr *orchestratorRepo, runID uuid.UUID, typ run.StageType) run.StageState {
	t.Helper()
	sts, err := rr.ListStagesForRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListStagesForRun: %v", err)
	}
	for _, st := range sts {
		if st.Type == typ {
			return st.State
		}
	}
	t.Fatalf("no %s stage on run %s", typ, runID)
	return ""
}

// stageStateOnOrchestratorRepo reads a stage's COMMITTED state from the fake.
func stageStateOnOrchestratorRepo(t *testing.T, rr *orchestratorRepo, runID, stageID uuid.UUID) run.StageState {
	t.Helper()
	sts, err := rr.ListStagesForRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListStagesForRun: %v", err)
	}
	for _, st := range sts {
		if st.ID == stageID {
			return st.State
		}
	}
	t.Fatalf("stage %s not found on run %s", stageID, runID)
	return ""
}
