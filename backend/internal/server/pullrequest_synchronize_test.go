package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcheckpublisher"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
)

// synchronizeRunRepo is a minimal run.Repository for the
// synchronize-webhook handler tests. Records ListRuns calls so the
// test can assert on the PR-URL filter, and stubs out the rest of
// the repository surface so the embedded auditcomplete.Compute path
// can walk without blowing up.
type synchronizeRunRepo struct {
	run.Repository
	mu             sync.Mutex
	listURLs       []string
	listResult     []*run.Run
	listErr        error
	stagesByRunID  map[uuid.UUID][]*run.Stage
	runsByID       map[uuid.UUID]*run.Run
	getStageByID   map[uuid.UUID]*run.Stage
	transitionsLog []run.StageState
}

func (r *synchronizeRunRepo) ListRuns(_ context.Context, f run.ListRunsFilter) ([]*run.Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if f.PullRequestURL != nil {
		r.listURLs = append(r.listURLs, *f.PullRequestURL)
	}
	return r.listResult, r.listErr
}
func (r *synchronizeRunRepo) GetRun(_ context.Context, id uuid.UUID) (*run.Run, error) {
	if rn, ok := r.runsByID[id]; ok {
		return rn, nil
	}
	return &run.Run{ID: id}, nil
}
func (r *synchronizeRunRepo) ListStagesForRun(_ context.Context, id uuid.UUID) ([]*run.Stage, error) {
	return r.stagesByRunID[id], nil
}
func (r *synchronizeRunRepo) GetStage(_ context.Context, id uuid.UUID) (*run.Stage, error) {
	if s, ok := r.getStageByID[id]; ok {
		return s, nil
	}
	return nil, run.ErrNotFound
}
func (r *synchronizeRunRepo) TransitionStage(_ context.Context, _ uuid.UUID, to run.StageState, _ *run.StageCompletion) (*run.Stage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.transitionsLog = append(r.transitionsLog, to)
	return nil, nil
}

type synchronizeArtifactRepo struct {
	artifact.Repository
	byStage map[uuid.UUID][]*artifact.Artifact
}

func (r *synchronizeArtifactRepo) ListForStage(_ context.Context, stageID uuid.UUID) ([]*artifact.Artifact, error) {
	return r.byStage[stageID], nil
}

type synchronizeAuditRepo struct {
	audit.Repository
	entries []*audit.Entry
}

func (r *synchronizeAuditRepo) ListForRunByCategory(_ context.Context, _ uuid.UUID, _ string) ([]*audit.Entry, error) {
	return r.entries, nil
}
func (r *synchronizeAuditRepo) ListForRun(_ context.Context, _ uuid.UUID) ([]*audit.Entry, error) {
	return r.entries, nil
}
func (r *synchronizeAuditRepo) AppendChained(_ context.Context, _ audit.ChainAppendParams) (*audit.Entry, error) {
	return nil, nil
}

func TestRepublishOnPullRequestEvent_NoMatchingRun_NoOp(t *testing.T) {
	// PR not managed by Fishhawk: ListRuns returns empty; the
	// handler short-circuits without computing or publishing. The
	// receiver still returns 202 to GitHub; the assertion here is
	// purely that we don't crash and don't reach into the compute
	// path with no data.
	rr := &synchronizeRunRepo{listResult: nil}
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		ArtifactRepo: &synchronizeArtifactRepo{},
		AuditRepo:    &synchronizeAuditRepo{},
	})

	payload, _ := json.Marshal(map[string]any{
		"pull_request": map[string]any{
			"html_url": "https://github.com/x/y/pull/42",
			"number":   42,
			"head":     map[string]any{"sha": "abc"},
		},
	})
	s.republishOnPullRequestEvent(context.Background(), webhook.Event{Type: "pull_request", Action: "synchronize", Repo: "x/y", RawBody: payload})

	rr.mu.Lock()
	defer rr.mu.Unlock()
	if len(rr.listURLs) != 1 {
		t.Errorf("expected 1 ListRuns call; got %d", len(rr.listURLs))
	}
	if rr.listURLs[0] != "https://github.com/x/y/pull/42" {
		t.Errorf("ListRuns filter = %q, want PR url", rr.listURLs[0])
	}
}

func TestRepublishOnPullRequestEvent_MatchingRun_LooksUpAndComputes(t *testing.T) {
	// Fishhawk-managed PR: ListRuns returns the run; the handler
	// passes it into auditcomplete.Compute. The end-to-end happy
	// path here is light — auditcomplete has its own thorough
	// tests; this case asserts the handler routes correctly.
	runID := uuid.New()
	installID := int64(99)
	matchingRun := &run.Run{
		ID:             runID,
		Repo:           "x/y",
		InstallationID: &installID,
	}
	rr := &synchronizeRunRepo{
		listResult: []*run.Run{matchingRun},
		runsByID:   map[uuid.UUID]*run.Run{runID: matchingRun},
	}
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		ArtifactRepo: &synchronizeArtifactRepo{},
		AuditRepo:    &synchronizeAuditRepo{},
	})

	payload, _ := json.Marshal(map[string]any{
		"pull_request": map[string]any{
			"html_url": "https://github.com/x/y/pull/275",
			"number":   275,
			"head":     map[string]any{"sha": "feedface"},
		},
	})
	s.republishOnPullRequestEvent(context.Background(), webhook.Event{Type: "pull_request", Action: "synchronize", Repo: "x/y", RawBody: payload})

	rr.mu.Lock()
	defer rr.mu.Unlock()
	if len(rr.listURLs) != 1 || rr.listURLs[0] != "https://github.com/x/y/pull/275" {
		t.Errorf("expected 1 ListRuns call for PR url; got %+v", rr.listURLs)
	}
}

func TestRepublishOnPullRequestEvent_MalformedPayload_NoCrash(t *testing.T) {
	// GitHub redelivery or hand-crafted payload missing the PR
	// object: handler logs + returns. No 5xx, no panic.
	rr := &synchronizeRunRepo{listErr: errors.New("never called")}
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		ArtifactRepo: &synchronizeArtifactRepo{},
		AuditRepo:    &synchronizeAuditRepo{},
	})

	s.republishOnPullRequestEvent(context.Background(), webhook.Event{Type: "pull_request", Action: "synchronize", Repo: "x/y", RawBody: []byte(`{not valid json`)})

	if len(rr.listURLs) != 0 {
		t.Errorf("malformed payload should NOT trigger a run lookup; got %+v", rr.listURLs)
	}
}

func TestRepublishOnPullRequestEvent_MissingDeps_NoOp(t *testing.T) {
	// Unconfigured server (no RunRepo / Artifact / Audit) must
	// still tolerate the call cleanly. The dev posture is "GitHub
	// isn't wired"; the synchronize handler shouldn't crash when
	// every dep is nil.
	s := New(Config{Addr: "127.0.0.1:0"})

	payload, _ := json.Marshal(map[string]any{
		"pull_request": map[string]any{
			"html_url": "https://github.com/x/y/pull/1",
			"number":   1,
			"head":     map[string]any{"sha": "abc"},
		},
	})
	s.republishOnPullRequestEvent(context.Background(), webhook.Event{Type: "pull_request", Action: "synchronize", Repo: "x/y", RawBody: payload})
	// No assertion needed — the test passes if we don't panic.
}

// --- E64.43 / #3160: the run-less not-applicable publish + its
// App-identity discriminator ---

// ourAppLogin is the login the stub App-identity resolver below yields
// (slug "fishhawk-dev" → "fishhawk-dev[bot]"). Every own-App case uses
// THIS constant, and every foreign case a literal that is definitionally
// different, so a counterfactual RED lands on the behavioural assertion
// rather than on fixture setup.
const ourAppLogin = "fishhawk-dev[bot]"

// notApplicableFixture wires a server whose run repo returns zero runs
// for the PR, with a real auditcheckpublisher over a recording GitHub
// fake and a resolvable App identity. Each field is overridden per case.
type notApplicableFixture struct {
	server *Server
	runs   *synchronizeRunRepo
	github *publisherFakeGitHub
}

func newNotApplicableFixture(t *testing.T) *notApplicableFixture {
	t.Helper()
	rr := &synchronizeRunRepo{listResult: nil}
	arts := &synchronizeArtifactRepo{}
	gh := newPublisherFakeGitHub()
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		ArtifactRepo: arts,
		AuditRepo:    &synchronizeAuditRepo{},
		ExternalURL:  "https://app.fishhawk.example.com",
	})
	s.appIdentityGetterOverride = &stubAppIdentityGetter{
		app:  &githubclient.App{Slug: "fishhawk-dev"},
		user: &githubclient.User{ID: 12345, Login: ourAppLogin},
	}
	s.auditCheckPublisher = auditcheckpublisher.New(auditcheckpublisher.Deps{
		GitHub:      gh,
		Runs:        rr,
		Artifacts:   arts,
		ExternalURL: "https://app.fishhawk.example.com",
	})
	if s.auditCheckPublisher == nil {
		t.Fatal("publisher nil")
	}
	return &notApplicableFixture{server: s, runs: rr, github: gh}
}

// prEvent builds a signed-shape pull_request Event with the given
// action, author login and sender.
func prEvent(action, authorLogin, sender, headSHA string) webhook.Event {
	payload, _ := json.Marshal(map[string]any{
		"action": action,
		"pull_request": map[string]any{
			"html_url": "https://github.com/x/y/pull/7",
			"number":   7,
			"head":     map[string]any{"sha": headSHA},
			"user":     map[string]any{"login": authorLogin, "type": "Bot"},
		},
	})
	return webhook.Event{
		Type:           "pull_request",
		Action:         action,
		Repo:           "x/y",
		Sender:         sender,
		SenderType:     "Bot",
		InstallationID: 99,
		RawBody:        payload,
	}
}

func TestRepublishOnPullRequestEvent_NoRuns_ForeignAuthor_PublishesNotApplicable(t *testing.T) {
	for _, action := range []string{"opened", "reopened", "synchronize"} {
		t.Run(action, func(t *testing.T) {
			f := newNotApplicableFixture(t)
			f.server.republishOnPullRequestEvent(context.Background(),
				prEvent(action, "dependabot[bot]", "dependabot[bot]", "cafebabe"))

			calls := f.github.calls()
			if len(calls) != 1 {
				t.Fatalf("CreateCheckRun calls = %d, want exactly 1", len(calls))
			}
			p := calls[0].params
			if p.Status != githubclient.CheckRunStatusCompleted {
				t.Errorf("status = %q, want completed", p.Status)
			}
			if p.Conclusion != githubclient.CheckRunConclusionNeutral {
				t.Errorf("conclusion = %q, want neutral", p.Conclusion)
			}
			if p.HeadSHA != "cafebabe" {
				t.Errorf("head_sha = %q, want cafebabe", p.HeadSHA)
			}
			if p.Name != auditcheckpublisher.CheckName {
				t.Errorf("name = %q, want %q", p.Name, auditcheckpublisher.CheckName)
			}
		})
	}
}

func TestRepublishOnPullRequestEvent_NoRuns_OwnAppAuthor_NoPublish(t *testing.T) {
	// The DISCRIMINATOR's counterfactual vehicle: a zero-run PR our own
	// App authored is a denormalization lag, not a foreign PR.
	f := newNotApplicableFixture(t)
	f.server.republishOnPullRequestEvent(context.Background(),
		prEvent("opened", ourAppLogin, "some-human", "cafebabe"))

	if got := f.github.calls(); len(got) != 0 {
		t.Fatalf("published %d check run(s) for a PR OUR OWN App authored; want 0", len(got))
	}
}

func TestRepublishOnPullRequestEvent_NoRuns_OwnAppSender_NoPublish(t *testing.T) {
	// The fix-up-push shape: a foreign author, but OUR App pushed.
	f := newNotApplicableFixture(t)
	f.server.republishOnPullRequestEvent(context.Background(),
		prEvent("synchronize", "some-human", ourAppLogin, "cafebabe"))

	if got := f.github.calls(); len(got) != 0 {
		t.Fatalf("published %d check run(s) for a PR OUR OWN App pushed; want 0", len(got))
	}
}

func TestRepublishOnPullRequestEvent_NoRuns_UnresolvableIdentity_NoPublish(t *testing.T) {
	// FAIL CLOSED: without a resolvable App identity we cannot establish
	// the PR is not ours, so we publish nothing at all.
	cases := []struct {
		name string
		with func(*Server)
	}{
		{"GetApp errors", func(s *Server) {
			s.appIdentityGetterOverride = &stubAppIdentityGetter{appErr: errors.New("502 from GET /app")}
		}},
		{"GetUser errors", func(s *Server) {
			s.appIdentityGetterOverride = &stubAppIdentityGetter{
				app:     &githubclient.App{Slug: "fishhawk-dev"},
				userErr: errors.New("404 from GET /users"),
			}
		}},
		{"GitHub entirely unwired", func(s *Server) {
			s.appIdentityGetterOverride = nil // and cfg.GitHub is nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newNotApplicableFixture(t)
			tc.with(f.server)
			f.server.republishOnPullRequestEvent(context.Background(),
				prEvent("opened", "dependabot[bot]", "dependabot[bot]", "cafebabe"))

			if got := f.github.calls(); len(got) != 0 {
				t.Fatalf("published %d check run(s) with an UNRESOLVABLE App identity; want 0 (fail closed)", len(got))
			}
		})
	}
}

func TestRepublishOnPullRequestEvent_ListRunsError_NoPublish(t *testing.T) {
	// A lookup ERROR is not evidence of zero runs.
	f := newNotApplicableFixture(t)
	f.runs.listErr = errors.New("db down")
	f.server.republishOnPullRequestEvent(context.Background(),
		prEvent("opened", "dependabot[bot]", "dependabot[bot]", "cafebabe"))

	if got := f.github.calls(); len(got) != 0 {
		t.Fatalf("published %d check run(s) on a ListRuns ERROR; want 0", len(got))
	}
}

func TestRepublishOnPullRequestEvent_NoRuns_EmptyHeadSHA_NoPublish(t *testing.T) {
	f := newNotApplicableFixture(t)
	f.server.republishOnPullRequestEvent(context.Background(),
		prEvent("opened", "dependabot[bot]", "dependabot[bot]", ""))

	if got := f.github.calls(); len(got) != 0 {
		t.Fatalf("published %d check run(s) with an empty head sha; want 0", len(got))
	}
}

func TestRepublishOnPullRequestEvent_NoRuns_EmptyPRURL_NoPublish(t *testing.T) {
	f := newNotApplicableFixture(t)
	payload, _ := json.Marshal(map[string]any{
		"action":       "opened",
		"pull_request": map[string]any{"number": 7, "head": map[string]any{"sha": "cafebabe"}},
	})
	f.server.republishOnPullRequestEvent(context.Background(), webhook.Event{
		Type: "pull_request", Action: "opened", Repo: "x/y",
		Sender: "dependabot[bot]", InstallationID: 99, RawBody: payload,
	})

	if got := f.github.calls(); len(got) != 0 {
		t.Fatalf("published %d check run(s) with no PR url; want 0", len(got))
	}
	if len(f.runs.listURLs) != 0 {
		t.Errorf("an empty PR url should not trigger a run lookup; got %+v", f.runs.listURLs)
	}
}

func TestRepublishOnPullRequestEvent_NoRuns_MalformedPayload_NoPublish(t *testing.T) {
	f := newNotApplicableFixture(t)
	f.server.republishOnPullRequestEvent(context.Background(), webhook.Event{
		Type: "pull_request", Action: "opened", Repo: "x/y",
		Sender: "dependabot[bot]", InstallationID: 99, RawBody: []byte(`{not valid json`),
	})

	if got := f.github.calls(); len(got) != 0 {
		t.Fatalf("published %d check run(s) on a malformed payload; want 0", len(got))
	}
}

func TestRepublishOnPullRequestEvent_NoRuns_UnparseableRepo_NoPublish(t *testing.T) {
	f := newNotApplicableFixture(t)
	ev := prEvent("opened", "dependabot[bot]", "dependabot[bot]", "cafebabe")
	ev.Repo = "not-an-owner-slash-name"
	f.server.republishOnPullRequestEvent(context.Background(), ev)

	if got := f.github.calls(); len(got) != 0 {
		t.Fatalf("published %d check run(s) with an unparseable repo; want 0", len(got))
	}
}

func TestRepublishOnPullRequestEvent_NoRuns_ZeroInstallation_NoPublish(t *testing.T) {
	// A zero installation id yields the zero CredentialScope, which the
	// publisher's guard refuses — nothing to authenticate with.
	f := newNotApplicableFixture(t)
	ev := prEvent("opened", "dependabot[bot]", "dependabot[bot]", "cafebabe")
	ev.InstallationID = 0
	f.server.republishOnPullRequestEvent(context.Background(), ev)

	if got := f.github.calls(); len(got) != 0 {
		t.Fatalf("published %d check run(s) with no installation id; want 0", len(got))
	}
}

// TestRepublishOnPullRequestEvent_WithRun_Unaffected is the
// unaffected-path pin: a PR that DOES have a run still goes through the
// ComputeResult + publishAuditCheck path across all three actions, and
// NEVER receives the not-applicable conclusion.
func TestRepublishOnPullRequestEvent_WithRun_Unaffected(t *testing.T) {
	for _, action := range []string{"opened", "reopened", "synchronize"} {
		t.Run(action, func(t *testing.T) {
			runID := uuid.New()
			installID := int64(99)
			implID := uuid.New()
			matching := &run.Run{ID: runID, Repo: "x/y", InstallationID: &installID}
			rr := &synchronizeRunRepo{
				listResult:    []*run.Run{matching},
				runsByID:      map[uuid.UUID]*run.Run{runID: matching},
				stagesByRunID: map[uuid.UUID][]*run.Stage{runID: {{ID: implID, RunID: runID, Type: run.StageTypeImplement}}},
			}
			arts := &synchronizeArtifactRepo{byStage: map[uuid.UUID][]*artifact.Artifact{
				implID: {{ID: uuid.New(), StageID: implID, Kind: artifact.KindPullRequest,
					Content: pullRequestArtifactBody("cafebabe")}},
			}}
			gh := newPublisherFakeGitHub()
			s := New(Config{
				Addr: "127.0.0.1:0", RunRepo: rr, ArtifactRepo: arts,
				AuditRepo:   &synchronizeAuditRepo{},
				ExternalURL: "https://app.fishhawk.example.com",
			})
			s.appIdentityGetterOverride = &stubAppIdentityGetter{
				app:  &githubclient.App{Slug: "fishhawk-dev"},
				user: &githubclient.User{ID: 12345, Login: ourAppLogin},
			}
			s.auditCheckPublisher = auditcheckpublisher.New(auditcheckpublisher.Deps{
				GitHub: gh, Runs: rr, Artifacts: arts,
				ExternalURL: "https://app.fishhawk.example.com",
			})

			s.republishOnPullRequestEvent(context.Background(),
				prEvent(action, "dependabot[bot]", "dependabot[bot]", "cafebabe"))

			calls := gh.calls()
			if len(calls) != 1 {
				t.Fatalf("CreateCheckRun calls = %d, want 1 (the run-bearing compute path)", len(calls))
			}
			p := calls[0].params
			if p.Conclusion == githubclient.CheckRunConclusionNeutral {
				t.Fatalf("a PR WITH a run received the not-applicable neutral conclusion")
			}
			if strings.Contains(p.OutputSummary, "No Fishhawk run is associated") {
				t.Fatalf("a PR WITH a run received the not-applicable summary: %q", p.OutputSummary)
			}
			if !strings.HasSuffix(p.DetailsURL, "/runs/"+runID.String()) {
				t.Errorf("details_url = %q, want the run link", p.DetailsURL)
			}
		})
	}
}
