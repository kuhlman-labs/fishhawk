package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// --- merge-time remaining-scope comment (E83.52 / #4085) ---

// partialMergeComment is one POST /repos/{owner}/{repo}/issues/{n}/comments.
type partialMergeComment struct {
	issue int
	body  string
}

// partialMergeGitHub is a stateful GitHub stub: GET serves the CURRENT PR body
// and PATCH replaces it (so the body the stub holds at the end is the body the
// PR would merge with), and issue comments are recorded per issue.
type partialMergeGitHub struct {
	mu            sync.Mutex
	prBody        string
	prPatches     int
	comments      []partialMergeComment
	commentStatus int
}

func newPartialMergeGitHubClient(t *testing.T, stub *partialMergeGitHub) *githubclient.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, _ *http.Request) {
		stub.mu.Lock()
		body := stub.prBody
		stub.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		raw, _ := json.Marshal(map[string]any{
			"node_id": "PR_x", "state": "open", "body": body,
			"head": map[string]any{"sha": "h"}, "base": map[string]any{"ref": "main"},
		})
		_, _ = w.Write(raw)
	})
	mux.HandleFunc("PATCH /repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var p struct {
			Body string `json:"body"`
		}
		_ = json.Unmarshal(raw, &p)
		stub.mu.Lock()
		stub.prBody = p.Body
		stub.prPatches++
		stub.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /repos/{owner}/{repo}/issues/{number}/comments", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var p struct {
			Body string `json:"body"`
		}
		_ = json.Unmarshal(raw, &p)
		n, _ := strconv.Atoi(r.PathValue("number"))
		stub.mu.Lock()
		st := stub.commentStatus
		if st == 0 {
			stub.comments = append(stub.comments, partialMergeComment{issue: n, body: p.Body})
		}
		stub.mu.Unlock()
		if st != 0 {
			w.WriteHeader(st)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":1}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  &fakeTokenProvider{tok: "ghs_t"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
	}
}

func (g *partialMergeGitHub) snapshot() (body string, patches int, comments []partialMergeComment) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.prBody, g.prPatches, append([]partialMergeComment(nil), g.comments...)
}

// partialMergeFixture is a run triggered by issue #7, carrying a GitHub
// installation, a plan stage holding a standard_v1 artifact with the given
// delivery, an implement stage, and (optionally) a review stage parked at
// awaiting_approval.
type partialMergeFixture struct {
	s       *Server
	sf      *signingFake
	rr      *orchestratorRepo
	ar      *countingArtifactRepo
	au      *auditFake
	gh      *partialMergeGitHub
	runRow  *run.Run
	implStg *run.Stage
}

const partialMergePRURL = "https://github.com/kuhlman-labs/fishhawk/pull/42"

func newPartialMergeFixture(t *testing.T, delivery string, implState run.StageState, withReview bool) *partialMergeFixture {
	t.Helper()
	f := &partialMergeFixture{
		sf: newSigningFake(),
		rr: newOrchestratorRepo(),
		ar: &countingArtifactRepo{fakeArtifactRepo: newFakeArtifactRepo()},
		au: newAuditFake(),
		gh: &partialMergeGitHub{},
	}
	f.runRow = f.rr.seedRun()
	inst := int64(99)
	f.runRow.InstallationID = &inst
	f.runRow.IssueContext = &run.IssueContext{Number: 7, Title: "t"}
	planStage := f.rr.seedStage(f.runRow.ID, 0, run.StageStateSucceeded)
	p := plan.Plan{Summary: "Ship one slice.", Delivery: delivery}
	if delivery == plan.DeliveryPartial {
		p.RemainingScope = partialDeliveryRemainingScope
	}
	content, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	sv := "standard_v1"
	f.ar.all = append(f.ar.all, &artifact.Artifact{
		ID: uuid.New(), StageID: planStage.ID, Kind: artifact.KindPlan, SchemaVersion: &sv, Content: content,
	})
	f.implStg = f.rr.seedStage(f.runRow.ID, 1, implState)
	f.implStg.Type = run.StageTypeImplement
	f.implStg.RequiresApproval = implState == run.StageStateRunning
	if withReview {
		rv := f.rr.seedStage(f.runRow.ID, 2, run.StageStateAwaitingApproval)
		rv.Type = run.StageTypeReview
	}
	f.s = New(Config{
		Addr:         "127.0.0.1:0",
		SigningRepo:  f.sf,
		ArtifactRepo: f.ar,
		AuditRepo:    f.au,
		RunRepo:      f.rr,
		Orchestrator: &orchestrator.Orchestrator{Runs: f.rr},
		GitHub:       newPartialMergeGitHubClient(t, f.gh),
	})
	return f
}

func (f *partialMergeFixture) rows(category string) []audit.ChainAppendParams {
	f.au.mu.Lock()
	defer f.au.mu.Unlock()
	var out []audit.ChainAppendParams
	for _, p := range f.au.appended {
		if p.Category == category {
			out = append(out, p)
		}
	}
	return out
}

func (f *partialMergeFixture) partialRows() int {
	return len(f.rows(categoryPartialDeliveryClosingReferenceNeutralized)) +
		len(f.rows(categoryPartialDeliveryRemainingScopePosted))
}

// merge delivers a merged pull_request.closed webhook through the REAL handler.
func (f *partialMergeFixture) merge() {
	f.s.handlePullRequestClosed(context.Background(), stampMergedPayload(partialMergePRURL))
}

func (f *partialMergeFixture) closeUnmerged() {
	payload, _ := json.Marshal(map[string]any{
		"pull_request": map[string]any{"html_url": partialMergePRURL, "number": 42, "merged": false},
		"sender":       map[string]any{"login": "alice"},
	})
	f.s.handlePullRequestClosed(context.Background(), payload)
}

// postDirect calls the merge-comment writer directly with the fixture's run.
func (f *partialMergeFixture) postDirect(t *testing.T) {
	t.Helper()
	runRow, err := f.rr.GetRun(context.Background(), f.runRow.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.s.postPartialDeliveryRemainingScope(context.Background(), runRow, reviewMergeMeta{prURL: partialMergePRURL})
}

// TestPartialDelivery_MergedPRLeavesIssueOpenWithRemainingScopeComment is the
// DONE-MEANS + CROSS-BOUNDARY test: plan artifact → ship handler → forge client
// → merge resolver → audit. A partial plan's agent body still ends `Closes #7`;
// after the ship the PR the stub holds references #7 without closing it, and
// the merge posts exactly one comment on #7 carrying the remaining scope.
func TestPartialDelivery_MergedPRLeavesIssueOpenWithRemainingScopeComment(t *testing.T) {
	f := newPartialMergeFixture(t, plan.DeliveryPartial, run.StageStateRunning, false)
	f.gh.prBody = agentClosingBody
	priv, _ := f.sf.issue(t, f.runRow.ID)
	if w := shipPRRequest(t, f.s, f.runRow.ID, f.implStg.ID, priv, validPRBytes(t), ""); w.Code != http.StatusCreated {
		t.Fatalf("ship status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	f.merge()

	body, _, comments := f.gh.snapshot()
	if hasClosingReference(body, 7) {
		t.Errorf("the merged PR body still closes #7:\n%s", body)
	}
	if !strings.Contains(body, "Refs #7") {
		t.Errorf("the merged PR body must reference #7:\n%s", body)
	}
	if len(comments) != 1 {
		t.Fatalf("issue comments = %d, want exactly 1: %+v", len(comments), comments)
	}
	c := comments[0]
	if c.issue != 7 {
		t.Errorf("comment posted on issue %d, want 7", c.issue)
	}
	for _, want := range []string{
		partialDeliveryMarkerPrefix + f.runRow.ID.String() + " -->",
		partialDeliveryRemainingScope,
		"was not delivered by this PR",
	} {
		if !strings.Contains(c.body, want) {
			t.Errorf("comment missing %q:\n%s", want, c.body)
		}
	}
	if n := len(f.rows(categoryPartialDeliveryClosingReferenceNeutralized)); n != 1 {
		t.Errorf("neutralized rows = %d, want 1", n)
	}
	posted := f.rows(categoryPartialDeliveryRemainingScopePosted)
	if len(posted) != 1 {
		t.Fatalf("remaining-scope rows = %d, want 1", len(posted))
	}
	var payload map[string]any
	if err := json.Unmarshal(posted[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["issue_number"] != float64(7) || payload["pr_url"] != partialMergePRURL || payload["forge"] != forgeNameGitHub {
		t.Errorf("posted payload = %v, want issue_number 7, pr_url %s, forge github", payload, partialMergePRURL)
	}
	if posted[0].ActorKind == nil || *posted[0].ActorKind != audit.ActorSystem {
		t.Errorf("posted actor = %v, want system", posted[0].ActorKind)
	}
}

// TestPartialDelivery_FullDeliveryStillCloses is the same flow with a full
// plan: the guard never edits the body, the merge posts no comment, and no
// partial-delivery row is written — so the issue still closes on merge.
func TestPartialDelivery_FullDeliveryStillCloses(t *testing.T) {
	f := newPartialMergeFixture(t, plan.DeliveryFull, run.StageStateRunning, false)
	f.gh.prBody = agentClosingBody
	priv, _ := f.sf.issue(t, f.runRow.ID)
	if w := shipPRRequest(t, f.s, f.runRow.ID, f.implStg.ID, priv, validPRBytes(t), ""); w.Code != http.StatusCreated {
		t.Fatalf("ship status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	if _, patches, _ := f.gh.snapshot(); patches != 0 {
		t.Errorf("a full plan's ship edited the PR body %d time(s), want 0", patches)
	}
	f.merge()

	body, _, comments := f.gh.snapshot()
	if !hasClosingReference(body, 7) {
		t.Errorf("a full plan's merged PR body must still close #7:\n%s", body)
	}
	if len(comments) != 0 {
		t.Errorf("a full plan's merge posted %d issue comment(s), want 0", len(comments))
	}
	if n := f.partialRows(); n != 0 {
		t.Errorf("partial_delivery_* rows = %d, want 0", n)
	}
}

// TestPartialDelivery_MergeArms pins the call sites: BOTH merged arms of
// resolveReviewStageOnMerge post the comment, and the closed-without-merge arm
// never does.
func TestPartialDelivery_MergeArms(t *testing.T) {
	for _, tc := range []struct {
		name       string
		withReview bool
		merged     bool
		wantPosts  int
	}{
		{name: "implement-only merged", withReview: false, merged: true, wantPosts: 1},
		{name: "review stage merged", withReview: true, merged: true, wantPosts: 1},
		{name: "implement-only closed without merge", withReview: false, merged: false},
		{name: "review stage closed without merge", withReview: true, merged: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPartialMergeFixture(t, plan.DeliveryPartial, run.StageStateSucceeded, tc.withReview)
			if tc.merged {
				f.merge()
			} else {
				f.closeUnmerged()
			}
			_, _, comments := f.gh.snapshot()
			if len(comments) != tc.wantPosts {
				t.Errorf("issue comments = %d, want %d", len(comments), tc.wantPosts)
			}
			if n := len(f.rows(categoryPartialDeliveryRemainingScopePosted)); n != tc.wantPosts {
				t.Errorf("remaining-scope rows = %d, want %d", n, tc.wantPosts)
			}
		})
	}
}

// TestPartialDelivery_MergeRedelivery_PostsOnce pins the dedup. The fixture is
// the IMPLEMENT-ONLY shape deliberately: there a redelivered merge runs the
// whole tail again, so the prior posted row is the only thing stopping a second
// comment. (On the review-stage shape a redelivery stops earlier, which would
// mask the dedup.)
func TestPartialDelivery_MergeRedelivery_PostsOnce(t *testing.T) {
	f := newPartialMergeFixture(t, plan.DeliveryPartial, run.StageStateSucceeded, false)
	f.merge()
	f.merge()
	if _, _, comments := f.gh.snapshot(); len(comments) != 1 {
		t.Errorf("issue comments after a redelivered merge = %d, want 1", len(comments))
	}
	if n := len(f.rows(categoryPartialDeliveryRemainingScopePosted)); n != 1 {
		t.Errorf("remaining-scope rows = %d, want 1", n)
	}
}

// TestPostPartialDeliveryRemainingScope_SkipBranches pins every early return:
// each row posts NO comment and writes NO audit row.
func TestPostPartialDeliveryRemainingScope_SkipBranches(t *testing.T) {
	for _, tc := range []struct {
		name     string
		delivery string
		mutate   func(t *testing.T, f *partialMergeFixture)
		// wantPlanLoad reports whether the guard reaches the plan load.
		wantPlanLoad bool
	}{
		{name: "full plan", delivery: plan.DeliveryFull, wantPlanLoad: true},
		{name: "legacy plan without delivery", delivery: "", wantPlanLoad: true},
		{
			name: "no triggering issue", delivery: plan.DeliveryPartial,
			mutate: func(_ *testing.T, f *partialMergeFixture) { f.runRow.IssueContext = nil },
		},
		{
			name: "issue number zero", delivery: plan.DeliveryPartial,
			mutate: func(_ *testing.T, f *partialMergeFixture) { f.runRow.IssueContext = &run.IssueContext{Title: "t"} },
		},
		{
			name: "plan does not load (no artifact repo)", delivery: plan.DeliveryPartial,
			mutate: func(_ *testing.T, f *partialMergeFixture) { f.s.cfg.ArtifactRepo = nil },
		},
		{
			name: "plan load error", delivery: plan.DeliveryPartial, wantPlanLoad: true,
			mutate: func(_ *testing.T, f *partialMergeFixture) { f.ar.listErr = errors.New("artifact store down") },
		},
		{
			name: "prior posted row (dedup)", delivery: plan.DeliveryPartial, wantPlanLoad: true,
			mutate: func(_ *testing.T, f *partialMergeFixture) {
				f.au.seeded = append(f.au.seeded, &audit.Entry{
					RunID: &f.runRow.ID, Category: categoryPartialDeliveryRemainingScopePosted,
				})
			},
		},
		{
			name: "dedup read error", delivery: plan.DeliveryPartial, wantPlanLoad: true,
			mutate: func(_ *testing.T, f *partialMergeFixture) {
				f.au.listByCategoryErrCategory = categoryPartialDeliveryRemainingScopePosted
			},
		},
		{
			// A zero scope is refused by the GitHub client before any request.
			name: "no credential scope", delivery: plan.DeliveryPartial, wantPlanLoad: true,
			mutate: func(_ *testing.T, f *partialMergeFixture) { f.runRow.InstallationID = nil },
		},
		{
			name: "unparseable repo", delivery: plan.DeliveryPartial, wantPlanLoad: true,
			mutate: func(_ *testing.T, f *partialMergeFixture) { f.runRow.Repo = "no-slash" },
		},
		{
			name: "no GitHub client", delivery: plan.DeliveryPartial, wantPlanLoad: true,
			mutate: func(_ *testing.T, f *partialMergeFixture) { f.s.cfg.GitHub = nil },
		},
		{
			name: "GitLab family with no resolvable forge", delivery: plan.DeliveryPartial, wantPlanLoad: true,
			mutate: func(_ *testing.T, f *partialMergeFixture) {
				ref := "gitlab:123"
				f.runRow.InstallationRef = &ref
				f.s.cfg.ForgeResolver = func(string) (forge.Forge, error) { return nil, errors.New("not registered") }
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPartialMergeFixture(t, tc.delivery, run.StageStateSucceeded, false)
			if tc.mutate != nil {
				tc.mutate(t, f)
			}
			f.postDirect(t)
			if _, _, comments := f.gh.snapshot(); len(comments) != 0 {
				t.Errorf("issue comments = %d, want 0", len(comments))
			}
			if n := len(f.rows(categoryPartialDeliveryRemainingScopePosted)); n != 0 {
				t.Errorf("remaining-scope rows = %d, want 0", n)
			}
			if got := f.ar.listCount() > 0; got != tc.wantPlanLoad {
				t.Errorf("plan loaded = %v, want %v", got, tc.wantPlanLoad)
			}
		})
	}
}

// TestPostPartialDeliveryRemainingScope_PostErrorRecordsNothing pins that the
// audit row is appended ONLY after PostIssueComment succeeds: it reads the
// COMMITTED chain after the call, so an append moved above the post reddens it.
// With no row, a redelivery retries the post.
func TestPostPartialDeliveryRemainingScope_PostErrorRecordsNothing(t *testing.T) {
	f := newPartialMergeFixture(t, plan.DeliveryPartial, run.StageStateSucceeded, false)
	f.gh.commentStatus = http.StatusInternalServerError
	f.postDirect(t)
	if n := len(f.rows(categoryPartialDeliveryRemainingScopePosted)); n != 0 {
		t.Fatalf("remaining-scope rows = %d after a failed post, want 0", n)
	}
	f.gh.mu.Lock()
	f.gh.commentStatus = 0
	f.gh.mu.Unlock()
	f.postDirect(t)
	if _, _, comments := f.gh.snapshot(); len(comments) != 1 {
		t.Errorf("the retry after a failed post posted %d comment(s), want 1", len(comments))
	}
}

// TestPostPartialDeliveryRemainingScope_AppendFailureStillPosted pins the named
// duplicate residual's shape: the comment posts, the append fails, nothing
// panics, and no row exists for a later delivery to dedup against.
func TestPostPartialDeliveryRemainingScope_AppendFailureStillPosted(t *testing.T) {
	f := newPartialMergeFixture(t, plan.DeliveryPartial, run.StageStateSucceeded, false)
	f.au.appendErrCategory = categoryPartialDeliveryRemainingScopePosted
	f.postDirect(t)
	if _, _, comments := f.gh.snapshot(); len(comments) != 1 {
		t.Errorf("issue comments = %d, want 1", len(comments))
	}
	if n := len(f.rows(categoryPartialDeliveryRemainingScopePosted)); n != 0 {
		t.Errorf("remaining-scope rows = %d, want 0 (the append failed)", n)
	}
}

// TestPostPartialDeliveryRemainingScope_GitLabPosts pins residual (b)'s shape:
// a GitLab-family run resolves its issue operations through the forge resolver
// and posts the comment, even though the ship-time neutralizer is GitHub-only.
func TestPostPartialDeliveryRemainingScope_GitLabPosts(t *testing.T) {
	f := newPartialMergeFixture(t, plan.DeliveryPartial, run.StageStateSucceeded, false)
	ref := "gitlab:123"
	f.runRow.InstallationRef = &ref
	f.runRow.InstallationID = nil
	f.runRow.Repo = "group/sub/project"
	fake := &icAppendOnlyForge{}
	f.s.cfg.GitHub = nil
	f.s.cfg.ForgeResolver = func(id string) (forge.Forge, error) {
		if id != "gitlab" {
			return nil, errors.New("unexpected forge " + id)
		}
		return fake, nil
	}
	f.postDirect(t)
	calls := fake.callsFor("PostIssueComment")
	if len(calls) != 1 {
		t.Fatalf("PostIssueComment calls = %d, want 1", len(calls))
	}
	if calls[0].number != 7 || calls[0].repo != (forge.RepoRef{Owner: "group/sub", Name: "project"}) || calls[0].scope.Ref() != ref {
		t.Errorf("post = %+v, want issue 7 on group/sub/project under %s", calls[0], ref)
	}
	posted := f.rows(categoryPartialDeliveryRemainingScopePosted)
	if len(posted) != 1 {
		t.Fatalf("remaining-scope rows = %d, want 1", len(posted))
	}
	if !strings.Contains(string(posted[0].Payload), `"forge":"gitlab"`) {
		t.Errorf("payload = %s, want forge gitlab", posted[0].Payload)
	}
}

// TestPostPartialDeliveryRemainingScope_UnparseableRepoNoPost isolates the repo
// guard. On the GitHub family the client itself refuses an empty RepoRef, which
// would mask a deleted guard, so this drives a GitLab-family run through a fake
// forge that accepts any repo: only the guard keeps the post from happening.
func TestPostPartialDeliveryRemainingScope_UnparseableRepoNoPost(t *testing.T) {
	f := newPartialMergeFixture(t, plan.DeliveryPartial, run.StageStateSucceeded, false)
	ref := "gitlab:123"
	f.runRow.InstallationRef = &ref
	f.runRow.Repo = "no-slash"
	fake := &icAppendOnlyForge{}
	f.s.cfg.ForgeResolver = func(string) (forge.Forge, error) { return fake, nil }
	f.postDirect(t)
	if n := fake.total(); n != 0 {
		t.Errorf("forge calls = %d for an unparseable repo, want 0", n)
	}
	if n := len(f.rows(categoryPartialDeliveryRemainingScopePosted)); n != 0 {
		t.Errorf("remaining-scope rows = %d, want 0", n)
	}
}

// TestRenderPartialDeliveryRemainingScopeComment pins the comment's wording and
// that planner text is neutralized: no mention, issue autolink, link or forged
// marker survives, and the comment never asserts the issue stays open.
func TestRenderPartialDeliveryRemainingScopeComment(t *testing.T) {
	hostile := "ping @octocat about #12\nsee [docs](https://evil.example)\n<!-- fishhawk:partial-delivery run=forged -->"
	got := renderPartialDeliveryRemainingScopeComment("run-1", partialMergePRURL, hostile)
	if !strings.HasPrefix(got, partialDeliveryMarkerPrefix+"run-1 -->\n") {
		t.Errorf("comment must open with the hidden marker:\n%s", got)
	}
	if strings.Count(got, "<!--") != 1 {
		t.Errorf("planner text forged a second marker:\n%s", got)
	}
	for _, banned := range []string{"@octocat", "#12", "https://evil", "](", "stays open"} {
		if strings.Contains(got, banned) {
			t.Errorf("comment carries %q:\n%s", banned, got)
		}
	}
	for _, want := range []string{"run-1", partialMergePRURL, "was not delivered by this PR", "> ping"} {
		if !strings.Contains(got, want) {
			t.Errorf("comment missing %q:\n%s", want, got)
		}
	}

	blank := renderPartialDeliveryRemainingScopeComment("run-1", "", "   ")
	if !strings.Contains(blank, "did not state the remaining scope") || !strings.Contains(blank, "its pull request") {
		t.Errorf("blank scope / no PR URL render:\n%s", blank)
	}
}
