package server

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/policy"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/scopeamendment"
)

// promptRunRepo is a run.Repository fake that supports GetStage +
// GetRun. Other methods panic to make accidental calls loud.
type promptRunRepo struct {
	stage    *run.Stage
	stageErr error
	runRow   *run.Run
	runErr   error
	// listRunsErr, when set, makes ListRuns return an error. The
	// decomposition-aware lineage ledger (#1038) enumerates child runs
	// via ListRuns and must treat a lookup error as an incomplete
	// ledger; this lets a test exercise that path.
	listRunsErr          error
	getStages            map[uuid.UUID]*run.Stage
	getRuns              map[uuid.UUID]*run.Run
	setPRURLCalls        []promptSetPRURLCall
	transitionStageCalls []promptTransitionStageCall
	// stagesByRunID backs ListStagesForRun. When non-nil, the map is
	// consulted; when nil the method returns an error so accidental
	// calls in tests that don't seed it stay loud.
	stagesByRunID map[uuid.UUID][]*run.Stage
	// addRunCostDeltas records every AddRunCost delta so the plan-review
	// cost-rollup seam test (#681) can assert the rollup was actually
	// driven with a non-zero delta rather than silently skipped.
	addRunCostDeltas []float64
}

type promptTransitionStageCall struct {
	StageID    uuid.UUID
	To         run.StageState
	Completion *run.StageCompletion
}

type promptSetPRURLCall struct {
	RunID uuid.UUID
	URL   string
}

func newPromptRunRepo() *promptRunRepo {
	return &promptRunRepo{
		getStages: map[uuid.UUID]*run.Stage{},
		getRuns:   map[uuid.UUID]*run.Run{},
	}
}

func (r *promptRunRepo) GetStage(_ context.Context, id uuid.UUID) (*run.Stage, error) {
	if r.stageErr != nil {
		return nil, r.stageErr
	}
	if s, ok := r.getStages[id]; ok {
		return s, nil
	}
	if r.stage != nil && r.stage.ID == id {
		return r.stage, nil
	}
	return nil, run.ErrNotFound
}

func (r *promptRunRepo) GetRun(_ context.Context, id uuid.UUID) (*run.Run, error) {
	if r.runErr != nil {
		return nil, r.runErr
	}
	if rn, ok := r.getRuns[id]; ok {
		return rn, nil
	}
	if r.runRow != nil && r.runRow.ID == id {
		return r.runRow, nil
	}
	return nil, run.ErrNotFound
}

// AddRunCost satisfies the trace handler's runCostRecorder optional
// capability (#681) so the plan-review cost-rollup seam test can assert the
// per-run total accumulates via a real AddRunCost call (not a vacuous skip).
func (r *promptRunRepo) AddRunCost(_ context.Context, id uuid.UUID, deltaUSD float64, resolvedModel string) (*run.Run, error) {
	r.addRunCostDeltas = append(r.addRunCostDeltas, deltaUSD)
	rn, ok := r.getRuns[id]
	if !ok {
		if r.runRow != nil && r.runRow.ID == id {
			rn = r.runRow
		} else {
			return nil, run.ErrNotFound
		}
	}
	rn.CostUSDTotal += deltaUSD
	if resolvedModel != "" {
		rn.ResolvedModel = resolvedModel
	}
	return rn, nil
}

func (r *promptRunRepo) CreateRun(context.Context, run.CreateRunParams) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *promptRunRepo) GetRunByIdempotencyKey(context.Context, string, string) (*run.Run, error) {
	return nil, run.ErrNotFound
}

// ListRuns honors the DecomposedFrom filter (scanning getRuns) so lineage
// tests can register decomposition children (#1038). It mirrors the repo
// contract that Limit must be > 0 by returning nothing on a zero limit.
// Filters the fake doesn't model return empty, preserving existing call
// sites.
func (r *promptRunRepo) ListRuns(_ context.Context, f run.ListRunsFilter) ([]*run.Run, error) {
	if r.listRunsErr != nil {
		return nil, r.listRunsErr
	}
	var out []*run.Run
	if f.DecomposedFrom != nil && f.Limit > 0 {
		for _, rn := range r.getRuns {
			if rn.DecomposedFrom != nil && *rn.DecomposedFrom == *f.DecomposedFrom {
				out = append(out, rn)
				if len(out) >= f.Limit {
					break
				}
			}
		}
	}
	return out, nil
}
func (r *promptRunRepo) TransitionRun(context.Context, uuid.UUID, run.State) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *promptRunRepo) RetryRun(context.Context, uuid.UUID, run.State) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *promptRunRepo) SetRunPullRequestURL(_ context.Context, id uuid.UUID, url string) (*run.Run, error) {
	r.setPRURLCalls = append(r.setPRURLCalls, promptSetPRURLCall{RunID: id, URL: url})
	if rn, ok := r.getRuns[id]; ok {
		u := url
		rn.PullRequestURL = &u
		return rn, nil
	}
	if r.runRow != nil && r.runRow.ID == id {
		u := url
		r.runRow.PullRequestURL = &u
		return r.runRow, nil
	}
	// Run not seeded — return a synthetic row so the handler's
	// best-effort log path still works.
	return &run.Run{ID: id}, nil
}
func (r *promptRunRepo) CreateStage(context.Context, run.CreateStageParams) (*run.Stage, error) {
	return nil, errors.New("not used")
}
func (r *promptRunRepo) ListStagesForRun(_ context.Context, runID uuid.UUID) ([]*run.Stage, error) {
	if r.stagesByRunID == nil {
		return nil, errors.New("not used")
	}
	return r.stagesByRunID[runID], nil
}
func (r *promptRunRepo) ListStagesAwaitingApproval(context.Context) ([]*run.Stage, error) {
	return nil, errors.New("not used")
}
func (r *promptRunRepo) ListReviewStagesAwaitingApproval(context.Context) ([]*run.Stage, error) {
	return nil, errors.New("not used")
}

func (r *promptRunRepo) ListStagesAwaitingChildren(context.Context) ([]*run.Stage, error) {
	return nil, errors.New("not used")
}

func (r *promptRunRepo) ListStagesDispatched(context.Context) ([]*run.Stage, error) {
	return nil, nil
}

func (r *promptRunRepo) RetryStage(context.Context, uuid.UUID, run.StageState) (*run.Stage, error) {
	return nil, errors.New("not used")
}
func (r *promptRunRepo) TransitionStage(_ context.Context, id uuid.UUID, to run.StageState, c *run.StageCompletion) (*run.Stage, error) {
	r.transitionStageCalls = append(r.transitionStageCalls, promptTransitionStageCall{
		StageID:    id,
		To:         to,
		Completion: c,
	})
	if st, ok := r.getStages[id]; ok {
		st.State = to
		return st, nil
	}
	return &run.Stage{ID: id, State: to}, nil
}

// stubIssueGetter records calls and returns canned issues.
type stubIssueGetter struct {
	called  bool
	issue   *githubclient.Issue
	getErr  error
	gotInst int64
	gotRepo githubclient.RepoRef
	gotNum  int

	// Comment-fetch seam (#621): canned thread + optional error, plus
	// a recorded-call flag and args so branch-2 tests can assert the
	// fetch happened with the right installation/repo/number.
	comments        []githubclient.FetchedIssueComment
	commentsErr     error
	commentsCalled  bool
	commentsGotInst int64
	commentsGotRepo githubclient.RepoRef
	commentsGotNum  int
}

func (s *stubIssueGetter) GetIssue(_ context.Context, installationID int64, repo githubclient.RepoRef, number int) (*githubclient.Issue, error) {
	s.called = true
	s.gotInst = installationID
	s.gotRepo = repo
	s.gotNum = number
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.issue, nil
}

func (s *stubIssueGetter) ListIssueComments(_ context.Context, installationID int64, repo githubclient.RepoRef, number int) ([]githubclient.FetchedIssueComment, error) {
	s.commentsCalled = true
	s.commentsGotInst = installationID
	s.commentsGotRepo = repo
	s.commentsGotNum = number
	if s.commentsErr != nil {
		return nil, s.commentsErr
	}
	return s.comments, nil
}

func newPromptServer(t *testing.T) (*Server, *promptRunRepo, *signingFake, *stubIssueGetter) {
	t.Helper()
	rr := newPromptRunRepo()
	sf := newSigningFake()
	gh := &stubIssueGetter{}
	s := New(Config{
		Addr:        "127.0.0.1:0",
		RunRepo:     rr,
		SigningRepo: sf,
	})
	// Inject the stub by overriding the default issueGetter resolver
	// via a dedicated test-only field. promptIssueGetterOverride is
	// nil in production.
	s.promptIssueGetterOverride = gh
	return s, rr, sf, gh
}

func promptRequest(t *testing.T, s *Server, runID, stageID uuid.UUID, priv ed25519.PrivateKey, sigOverride string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/stages/%s/prompt", stageID), nil)
	if sigOverride != "" {
		req.Header.Set("X-Fishhawk-Signature", sigOverride)
	} else if priv != nil {
		sig := ed25519.Sign(priv, PromptCanonicalMessage(stageID))
		req.Header.Set("X-Fishhawk-Signature", hex.EncodeToString(sig))
	}
	_ = runID
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

func TestGetStagePrompt_HappyPath_ImplementWithIssue(t *testing.T) {
	s, rr, sf, gh := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()
	priv, _ := sf.issue(t, runID)

	installation := int64(99)
	triggerRef := "issue:42"
	rr.runRow = &run.Run{
		ID:             runID,
		Repo:           "kuhlman-labs/example",
		WorkflowID:     "feature_change",
		TriggerSource:  run.TriggerGitHubIssue,
		TriggerRef:     &triggerRef,
		InstallationID: &installation,
	}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeImplement}
	gh.issue = &githubclient.Issue{Number: 42, Title: "Add foo", Body: "Body text", State: "open"}

	w := promptRequest(t, s, runID, stageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.StageID != stageID.String() {
		t.Errorf("StageID = %q", resp.StageID)
	}
	if resp.StageType != "implement" {
		t.Errorf("StageType = %q", resp.StageType)
	}
	if resp.Prompt == "" || len(resp.PromptHash) != 64 {
		t.Errorf("empty/short Prompt or PromptHash: %+v", resp)
	}
	// Implement-stage prompt links the issue (#244): title + URL
	// appear, but the body is dropped — the agent is told to fetch.
	for _, want := range []string{
		"Add foo",
		"Triggering issue: #42 · Add foo",
		"https://github.com/kuhlman-labs/example/issues/42",
		"Fetch the issue body via your GitHub tooling",
		"kuhlman-labs/example",
	} {
		if !contains(resp.Prompt, want) {
			t.Errorf("prompt missing %q\n---\n%s", want, resp.Prompt)
		}
	}
	if contains(resp.Prompt, "Body text") {
		t.Errorf("implement prompt should not include the issue body verbatim:\n%s", resp.Prompt)
	}

	if !gh.called || gh.gotNum != 42 || gh.gotInst != installation {
		t.Errorf("github stub not called as expected: %+v", gh)
	}
	if gh.gotRepo.Owner != "kuhlman-labs" || gh.gotRepo.Name != "example" {
		t.Errorf("repo parsed wrong: %+v", gh.gotRepo)
	}
}

func TestGetStagePrompt_PlanStage_DoesNotFetchIssue_WhenNotIssueTriggered(t *testing.T) {
	s, rr, sf, gh := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()
	priv, _ := sf.issue(t, runID)

	rr.runRow = &run.Run{
		ID:            runID,
		Repo:          "x/y",
		WorkflowID:    "feature_change",
		TriggerSource: "manual",
		// TriggerRef nil → no issue fetch
	}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypePlan}

	w := promptRequest(t, s, runID, stageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if gh.called {
		t.Errorf("GetIssue called for non-issue trigger")
	}
}

func TestGetStagePrompt_GitHubFetchFails_StillReturns200(t *testing.T) {
	s, rr, sf, gh := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()
	priv, _ := sf.issue(t, runID)

	installation := int64(99)
	triggerRef := "issue:42"
	rr.runRow = &run.Run{
		ID:             runID,
		Repo:           "x/y",
		TriggerSource:  run.TriggerGitHubIssue,
		TriggerRef:     &triggerRef,
		InstallationID: &installation,
	}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeImplement}
	gh.getErr = errors.New("github down")

	w := promptRequest(t, s, runID, stageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (best-effort issue fetch):\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	// Number is preserved (parsed from TriggerRef) even when GitHub
	// fetch fails — the agent still knows which issue to look at,
	// it just doesn't have the body inline.
	if !contains(resp.Prompt, "Triggering issue: #42") {
		t.Errorf("prompt missing issue header:\n%s", resp.Prompt)
	}
	if contains(resp.Prompt, "Title:") {
		t.Errorf("Title: header should be omitted when fetch failed:\n%s", resp.Prompt)
	}
}

// TestGetStagePrompt_PrefersCachedIssueContext is the #415
// headline check: when the run row carries a cached IssueContext
// (operator-side `gh issue view` shipped the payload inline at
// run-create), the prompt builder reads from it and never calls
// GitHub. Local-runner runs that have no installation_id depend
// on this path; webhook flows that DO have installation_id should
// still prefer the cache when present (cheaper, no rate-limit
// pressure).
func TestGetStagePrompt_PrefersCachedIssueContext(t *testing.T) {
	s, rr, sf, gh := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()
	priv, _ := sf.issue(t, runID)

	triggerRef := "issue:42"
	rr.runRow = &run.Run{
		ID:            runID,
		Repo:          "kuhlman-labs/example",
		WorkflowID:    "feature_change",
		TriggerSource: run.TriggerGitHubIssue,
		TriggerRef:    &triggerRef,
		// No InstallationID — the local-runner shape. The cache
		// MUST work without one.
		IssueContext: &run.IssueContext{
			Number: 42,
			Title:  "Cached title",
			Body:   "Cached body — operator's gh fetched this.",
			URL:    "https://github.com/kuhlman-labs/example/issues/42",
		},
	}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypePlan}

	w := promptRequest(t, s, runID, stageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if gh.called {
		t.Errorf("GetIssue should NOT be called when IssueContext is cached on the run row")
	}
	var resp promptResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	// Plan stage renders the issue body verbatim (unlike implement
	// which links only). Cached body should appear.
	if !contains(resp.Prompt, "Cached body — operator's gh fetched this.") {
		t.Errorf("prompt missing cached body:\n%s", resp.Prompt)
	}
	if !contains(resp.Prompt, "Cached title") {
		t.Errorf("prompt missing cached title:\n%s", resp.Prompt)
	}
}

// TestGetStagePrompt_CachedIssueContext_PreferredOverGitHubFetch
// guards the resolution-order invariant: even when InstallationID
// is set (webhook-dispatched run that ALSO happened to carry
// inline context — an unlikely cohabitation, but worth pinning),
// the cache wins and the GitHub fetch is skipped. Prevents a
// future "let's just always re-fetch" regression.
func TestGetStagePrompt_CachedIssueContext_PreferredOverGitHubFetch(t *testing.T) {
	s, rr, sf, gh := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()
	priv, _ := sf.issue(t, runID)

	installation := int64(99)
	triggerRef := "issue:42"
	rr.runRow = &run.Run{
		ID:             runID,
		Repo:           "kuhlman-labs/example",
		TriggerSource:  run.TriggerGitHubIssue,
		TriggerRef:     &triggerRef,
		InstallationID: &installation,
		IssueContext: &run.IssueContext{
			Number: 42,
			Title:  "Cached",
			Body:   "Cached body",
		},
	}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypePlan}
	// If the GitHub fetch wins (regression), this would clobber
	// the cache values; the assertions below catch it.
	gh.issue = &githubclient.Issue{Number: 42, Title: "FROM GITHUB", Body: "FROM GITHUB"}

	w := promptRequest(t, s, runID, stageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if gh.called {
		t.Errorf("GetIssue should NOT be called when cache is populated")
	}
	var resp promptResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if !contains(resp.Prompt, "Cached body") {
		t.Errorf("cache should win; prompt missing cached body:\n%s", resp.Prompt)
	}
	if contains(resp.Prompt, "FROM GITHUB") {
		t.Errorf("cache should win; prompt unexpectedly contained GitHub fetch:\n%s", resp.Prompt)
	}
}

// TestGetStagePrompt_CachedIssueComments_MappedIntoTrigger is the
// #618 check: a cached IssueContext carrying comments renders the
// '### Issue comments' section in the plan-stage prompt with the
// commenter's login.
func TestGetStagePrompt_CachedIssueComments_MappedIntoTrigger(t *testing.T) {
	s, rr, sf, gh := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()
	priv, _ := sf.issue(t, runID)

	triggerRef := "issue:42"
	rr.runRow = &run.Run{
		ID:            runID,
		Repo:          "kuhlman-labs/example",
		WorkflowID:    "feature_change",
		TriggerSource: run.TriggerGitHubIssue,
		TriggerRef:    &triggerRef,
		IssueContext: &run.IssueContext{
			Number: 42,
			Title:  "Cached title",
			Body:   "Cached body.",
			URL:    "https://github.com/kuhlman-labs/example/issues/42",
			Comments: []run.IssueComment{
				{Author: "alice", Body: "Comment-borne refinement.", CreatedAt: "2026-05-01T10:00:00Z"},
			},
		},
	}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypePlan}

	w := promptRequest(t, s, runID, stageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if gh.called {
		t.Errorf("GetIssue should NOT be called when IssueContext is cached")
	}
	var resp promptResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if !contains(resp.Prompt, "### Issue comments") {
		t.Errorf("prompt missing comments section:\n%s", resp.Prompt)
	}
	if !contains(resp.Prompt, "Comment-borne refinement.") {
		t.Errorf("prompt missing comment body:\n%s", resp.Prompt)
	}
	if !contains(resp.Prompt, "@alice") {
		t.Errorf("prompt missing comment author:\n%s", resp.Prompt)
	}
}

// TestGetStagePrompt_CachedIssueContext_NoComments guards the
// regression case: a cached IssueContext with no comments still
// renders the body-only plan prompt unchanged — no comments section.
func TestGetStagePrompt_CachedIssueContext_NoComments(t *testing.T) {
	s, rr, sf, _ := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()
	priv, _ := sf.issue(t, runID)

	triggerRef := "issue:42"
	rr.runRow = &run.Run{
		ID:            runID,
		Repo:          "kuhlman-labs/example",
		WorkflowID:    "feature_change",
		TriggerSource: run.TriggerGitHubIssue,
		TriggerRef:    &triggerRef,
		IssueContext: &run.IssueContext{
			Number: 42,
			Title:  "Cached title",
			Body:   "Cached body.",
			URL:    "https://github.com/kuhlman-labs/example/issues/42",
		},
	}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypePlan}

	w := promptRequest(t, s, runID, stageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if contains(resp.Prompt, "### Issue comments") {
		t.Errorf("no comments section expected when IssueContext has no comments:\n%s", resp.Prompt)
	}
	if !contains(resp.Prompt, "Cached body.") {
		t.Errorf("body-only prompt should still render the body:\n%s", resp.Prompt)
	}
}

// TestGetStagePrompt_WebhookFetchedComments_MappedIntoTrigger is the
// #621 headline check: a webhook-triggered run (InstallationID set, no
// cached IssueContext) fetches the issue comment thread via branch 2
// and renders it in the plan-stage prompt. It also proves the shared
// writeIssueComments bot-filter applies on this path — a [bot]-authored
// comment is dropped — exercising fetch -> server mapping -> render end
// to end.
func TestGetStagePrompt_WebhookFetchedComments_MappedIntoTrigger(t *testing.T) {
	s, rr, sf, gh := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()
	priv, _ := sf.issue(t, runID)

	var installation int64 = 555
	triggerRef := "issue:42"
	rr.runRow = &run.Run{
		ID:             runID,
		Repo:           "kuhlman-labs/example",
		WorkflowID:     "feature_change",
		TriggerSource:  run.TriggerGitHubIssue,
		TriggerRef:     &triggerRef,
		InstallationID: &installation,
		// No IssueContext — forces branch 2 (webhook fetch).
	}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypePlan}
	gh.issue = &githubclient.Issue{Number: 42, Title: "Add foo", Body: "Body text", State: "open"}
	gh.comments = []githubclient.FetchedIssueComment{
		{Author: "alice", Body: "Comment-borne refinement.", CreatedAt: "2026-05-01T10:00:00Z"},
		{Author: "github-actions[bot]", Body: "CI failed on main.", CreatedAt: "2026-05-01T11:00:00Z"},
	}

	w := promptRequest(t, s, runID, stageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if !gh.commentsCalled {
		t.Fatalf("ListIssueComments should be called on the webhook branch")
	}
	if gh.commentsGotInst != installation || gh.commentsGotNum != 42 ||
		gh.commentsGotRepo != (githubclient.RepoRef{Owner: "kuhlman-labs", Name: "example"}) {
		t.Errorf("ListIssueComments args = inst %d repo %+v num %d",
			gh.commentsGotInst, gh.commentsGotRepo, gh.commentsGotNum)
	}
	var resp promptResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if !contains(resp.Prompt, "### Issue comments") {
		t.Errorf("prompt missing comments section:\n%s", resp.Prompt)
	}
	if !contains(resp.Prompt, "Comment-borne refinement.") || !contains(resp.Prompt, "@alice") {
		t.Errorf("prompt missing human comment:\n%s", resp.Prompt)
	}
	if contains(resp.Prompt, "CI failed on main.") || contains(resp.Prompt, "github-actions[bot]") {
		t.Errorf("bot-authored comment should be filtered on the webhook path:\n%s", resp.Prompt)
	}
}

// TestGetStagePrompt_WebhookCommentsFetchError_DegradesToBody confirms a
// ListIssueComments failure on the webhook path is best-effort: the
// prompt still returns 200 with title+body and no comments section.
func TestGetStagePrompt_WebhookCommentsFetchError_DegradesToBody(t *testing.T) {
	s, rr, sf, gh := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()
	priv, _ := sf.issue(t, runID)

	var installation int64 = 555
	triggerRef := "issue:42"
	rr.runRow = &run.Run{
		ID:             runID,
		Repo:           "kuhlman-labs/example",
		WorkflowID:     "feature_change",
		TriggerSource:  run.TriggerGitHubIssue,
		TriggerRef:     &triggerRef,
		InstallationID: &installation,
	}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypePlan}
	gh.issue = &githubclient.Issue{Number: 42, Title: "Add foo", Body: "Body text", State: "open"}
	gh.commentsErr = errors.New("boom")

	w := promptRequest(t, s, runID, stageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if !contains(resp.Prompt, "Body text") {
		t.Errorf("title+body should still render on comment-fetch failure:\n%s", resp.Prompt)
	}
	if contains(resp.Prompt, "### Issue comments") {
		t.Errorf("no comments section expected when the fetch failed:\n%s", resp.Prompt)
	}
}

func TestGetStagePrompt_UnsupportedStageType(t *testing.T) {
	s, rr, sf, _ := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()
	priv, _ := sf.issue(t, runID)

	rr.runRow = &run.Run{ID: runID, Repo: "x/y"}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageType("review")}

	w := promptRequest(t, s, runID, stageID, priv, "")
	if w.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501:\n%s", w.Code, w.Body.String())
	}
}

func TestGetStagePrompt_StageNotFound(t *testing.T) {
	s, _, sf, _ := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()
	priv, _ := sf.issue(t, runID)

	w := promptRequest(t, s, runID, stageID, priv, "")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestGetStagePrompt_SignatureMissing(t *testing.T) {
	s, rr, _, _ := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()
	rr.runRow = &run.Run{ID: runID, Repo: "x/y"}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeImplement}

	w := promptRequest(t, s, runID, stageID, nil, "")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestGetStagePrompt_SignatureBadHex(t *testing.T) {
	s, rr, _, _ := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()
	rr.runRow = &run.Run{ID: runID, Repo: "x/y"}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeImplement}

	w := promptRequest(t, s, runID, stageID, nil, "not-hex")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestGetStagePrompt_SignatureWrongKey(t *testing.T) {
	s, rr, sf, _ := newPromptServer(t)
	runID := uuid.New()
	otherRunID := uuid.New()
	stageID := uuid.New()
	// Issue a key for a DIFFERENT run; sign with that.
	otherPriv, _ := sf.issue(t, otherRunID)
	// But also issue one for the real run so the lookup hits a key
	// (otherwise we'd test ErrNotFound, not ErrSignatureInvalid).
	_, _ = sf.issue(t, runID)
	rr.runRow = &run.Run{ID: runID, Repo: "x/y"}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeImplement}

	sig := ed25519.Sign(otherPriv, PromptCanonicalMessage(stageID))
	w := promptRequest(t, s, runID, stageID, nil, hex.EncodeToString(sig))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestGetStagePrompt_BadStageUUID(t *testing.T) {
	s, _, _, _ := newPromptServer(t)
	req := httptest.NewRequest(http.MethodGet, "/v0/stages/not-a-uuid/prompt", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestGetStagePrompt_Unconfigured(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	req := httptest.NewRequest(http.MethodGet,
		"/v0/stages/"+uuid.New().String()+"/prompt", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestParseIssueRef(t *testing.T) {
	cases := []struct {
		in     string
		want   int
		wantOK bool
	}{
		{"issue:42", 42, true},
		{"issue:0", 0, false},
		{"issue:-1", 0, false},
		{"pr:42", 0, false},
		{"42", 0, false},
		{"issue:", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, ok := parseIssueRef(c.in)
			if got != c.want || ok != c.wantOK {
				t.Errorf("got (%d, %v), want (%d, %v)", got, ok, c.want, c.wantOK)
			}
		})
	}
}

func TestParseRepoOwnerName(t *testing.T) {
	r, err := parseRepoOwnerName("owner/name")
	if err != nil {
		t.Errorf("err = %v", err)
	}
	if r.Owner != "owner" || r.Name != "name" {
		t.Errorf("got %+v", r)
	}
	if _, err := parseRepoOwnerName("nope"); err == nil {
		t.Error("expected error for missing slash")
	}
	if _, err := parseRepoOwnerName("/x"); err == nil {
		t.Error("expected error for empty owner")
	}
	if _, err := parseRepoOwnerName("x/"); err == nil {
		t.Error("expected error for empty name")
	}
	if _, err := parseRepoOwnerName("a/b/c"); err == nil {
		t.Error("expected error for multi-slash")
	}
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// TestGetStagePromptRender_HappyPath_NoSignatureRequired confirms the
// SPA-side endpoint constructs the same prompt as the runner's path
// without requiring X-Fishhawk-Signature (#215). Auth tracks the
// existing stage/audit reads — no header check at the handler level.
func TestGetStagePromptRender_HappyPath_NoSignatureRequired(t *testing.T) {
	s, rr, _, gh := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()

	installation := int64(99)
	triggerRef := "issue:42"
	rr.runRow = &run.Run{
		ID:             runID,
		Repo:           "kuhlman-labs/example",
		WorkflowID:     "feature_change",
		TriggerSource:  run.TriggerGitHubIssue,
		TriggerRef:     &triggerRef,
		InstallationID: &installation,
	}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeImplement}
	gh.issue = &githubclient.Issue{Number: 42, Title: "Add foo", Body: "Body text", State: "open"}

	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/stages/%s/prompt-render", stageID), nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.StageType != "implement" {
		t.Errorf("StageType = %q", resp.StageType)
	}
	// Implement-stage prompt links the issue (#244): title + URL
	// appear, but the body is dropped — the agent is told to fetch.
	for _, want := range []string{
		"Add foo",
		"Triggering issue: #42 · Add foo",
		"https://github.com/kuhlman-labs/example/issues/42",
		"Fetch the issue body via your GitHub tooling",
	} {
		if !strings.Contains(resp.Prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, resp.Prompt)
		}
	}
	if strings.Contains(resp.Prompt, "Body text") {
		t.Errorf("implement prompt should not include the issue body verbatim:\n%s", resp.Prompt)
	}
}

// TestGetStagePromptRender_MatchesSignatureAuthedPath asserts both
// endpoints produce byte-identical prompts for the same stage —
// they have to, because the audit story depends on the SPA showing
// the same text the runner saw.
func TestGetStagePromptRender_MatchesSignatureAuthedPath(t *testing.T) {
	s, rr, sf, gh := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()
	priv, _ := sf.issue(t, runID)

	installation := int64(99)
	triggerRef := "issue:42"
	rr.runRow = &run.Run{
		ID:             runID,
		Repo:           "kuhlman-labs/example",
		WorkflowID:     "feature_change",
		TriggerSource:  run.TriggerGitHubIssue,
		TriggerRef:     &triggerRef,
		InstallationID: &installation,
	}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypePlan}
	gh.issue = &githubclient.Issue{Number: 42, Title: "T", Body: "B", State: "open"}

	signed := promptRequest(t, s, runID, stageID, priv, "")
	if signed.Code != http.StatusOK {
		t.Fatalf("signed status = %d", signed.Code)
	}
	rendered := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/stages/%s/prompt-render", stageID), nil)
	rw := httptest.NewRecorder()
	s.Handler().ServeHTTP(rw, rendered)
	if rw.Code != http.StatusOK {
		t.Fatalf("rendered status = %d", rw.Code)
	}

	var signedBody, renderedBody promptResponse
	_ = json.Unmarshal(signed.Body.Bytes(), &signedBody)
	_ = json.Unmarshal(rw.Body.Bytes(), &renderedBody)
	if signedBody.Prompt != renderedBody.Prompt {
		t.Errorf("prompt diverged between signed + rendered paths:\nsigned:\n%s\n---\nrendered:\n%s",
			signedBody.Prompt, renderedBody.Prompt)
	}
	if signedBody.PromptHash != renderedBody.PromptHash {
		t.Errorf("hash diverged: %q vs %q", signedBody.PromptHash, renderedBody.PromptHash)
	}
	// The fix-up wire flag (#784) is set in BOTH handlers; assert they stay
	// consistent so a change to one (runner-facing) handler that misses the
	// other (SPA render) is caught. The fixup=true path itself is covered by
	// TestGetStagePrompt_Implement_FixupConcerns_RenderedAndFolded on the
	// runner-facing handler; here both are false (plan stage, no fix-up entry),
	// which still guards against a structural divergence between the two.
	if signedBody.Fixup != renderedBody.Fixup || signedBody.FixupBranch != renderedBody.FixupBranch {
		t.Errorf("fix-up wire flag diverged: signed={%v,%q} rendered={%v,%q}",
			signedBody.Fixup, signedBody.FixupBranch, renderedBody.Fixup, renderedBody.FixupBranch)
	}
}

// planStageSpecYAML is a valid feature_change workflow spec with a
// workflow-level policy max_stage_runtime of 30m. No per-stage executor
// timeouts so both plan and implement resolve to the policy value.
const planStageSpecYAML30m = `version: "0.3"
workflows:
  feature_change:
    policy:
      max_stage_runtime: "30m"
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`

// planStageSpecYAML45mImpl is the same workflow but the implement stage
// declares executor.timeout: "45m", which overrides the 30m workflow policy.
const planStageSpecYAML45mImpl = `version: "0.3"
workflows:
  feature_change:
    policy:
      max_stage_runtime: "30m"
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
          timeout: "45m"
        produces:
          - artifact: pull_request
`

// TestGetStagePrompt_PlanBudget_WorkflowPolicy exercises the three-level
// timeout precedence (stage executor > workflow policy > 15m default)
// through the full server path for a plan-stage prompt. Each case asserts
// on the "implement stage N minutes" text rendered into the prompt body.
func TestGetStagePrompt_PlanBudget_WorkflowPolicy(t *testing.T) {
	s, rr, _, _ := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()

	rr.runRow = &run.Run{
		ID:            runID,
		Repo:          "x/y",
		WorkflowID:    "feature_change",
		TriggerSource: run.TriggerCLI,
		WorkflowSpec:  []byte(planStageSpecYAML30m),
	}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypePlan}

	req := httptest.NewRequest(http.MethodGet, "/v0/stages/"+stageID.String()+"/prompt-render", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(resp.Prompt, "implement stage 30 minutes") {
		t.Errorf("prompt missing 'implement stage 30 minutes' (workflow policy):\n%s", resp.Prompt)
	}
}

func TestGetStagePrompt_PlanBudget_StageExecutorOverridesPolicy(t *testing.T) {
	s, rr, _, _ := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()

	rr.runRow = &run.Run{
		ID:            runID,
		Repo:          "x/y",
		WorkflowID:    "feature_change",
		TriggerSource: run.TriggerCLI,
		WorkflowSpec:  []byte(planStageSpecYAML45mImpl),
	}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypePlan}

	req := httptest.NewRequest(http.MethodGet, "/v0/stages/"+stageID.String()+"/prompt-render", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(resp.Prompt, "implement stage 45 minutes") {
		t.Errorf("prompt missing 'implement stage 45 minutes' (stage executor override):\n%s", resp.Prompt)
	}
}

func TestGetStagePrompt_PlanBudget_NilSpecFallsBackTo15m(t *testing.T) {
	s, rr, _, _ := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()

	rr.runRow = &run.Run{
		ID:            runID,
		Repo:          "x/y",
		WorkflowID:    "feature_change",
		TriggerSource: run.TriggerCLI,
		WorkflowSpec:  nil,
	}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypePlan}

	req := httptest.NewRequest(http.MethodGet, "/v0/stages/"+stageID.String()+"/prompt-render", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(resp.Prompt, "implement stage 15 minutes") {
		t.Errorf("prompt missing 'implement stage 15 minutes' (nil spec default):\n%s", resp.Prompt)
	}
}

func TestGetStagePromptRender_StageNotFound(t *testing.T) {
	s, rr, _, _ := newPromptServer(t)
	rr.stageErr = run.ErrNotFound
	req := httptest.NewRequest(http.MethodGet,
		"/v0/stages/"+uuid.New().String()+"/prompt-render", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestGetStagePromptRender_BadStageUUID(t *testing.T) {
	s, _, _, _ := newPromptServer(t)
	req := httptest.NewRequest(http.MethodGet, "/v0/stages/not-a-uuid/prompt-render", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestGetStagePromptRender_Unconfigured(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	req := httptest.NewRequest(http.MethodGet,
		"/v0/stages/"+uuid.New().String()+"/prompt-render", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestGetStagePrompt_DecomposedFromRunID_Present(t *testing.T) {
	s, rr, sf, _ := newPromptServer(t)
	runID := uuid.New()
	parentRunID := uuid.New()
	stageID := uuid.New()
	priv, _ := sf.issue(t, runID)

	rr.runRow = &run.Run{
		ID:             runID,
		Repo:           "x/y",
		WorkflowID:     "feature_change",
		TriggerSource:  run.TriggerCLI,
		DecomposedFrom: &parentRunID,
	}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeImplement}

	w := promptRequest(t, s, runID, stageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.DecomposedFromRunID != parentRunID.String() {
		t.Errorf("DecomposedFromRunID = %q, want %q", resp.DecomposedFromRunID, parentRunID.String())
	}
}

func TestGetStagePrompt_DecomposedFromRunID_Absent_ForStandaloneRun(t *testing.T) {
	s, rr, sf, _ := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()
	priv, _ := sf.issue(t, runID)

	rr.runRow = &run.Run{
		ID:            runID,
		Repo:          "x/y",
		WorkflowID:    "feature_change",
		TriggerSource: run.TriggerCLI,
		// DecomposedFrom nil → standalone run
	}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeImplement}

	w := promptRequest(t, s, runID, stageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.DecomposedFromRunID != "" {
		t.Errorf("DecomposedFromRunID = %q, want empty for standalone run", resp.DecomposedFromRunID)
	}
}

// TestGetStagePrompt_StateGuard_* cover the 409 state guard on the
// runner-facing prompt endpoint. One test per non-runnable state, plus
// three no-regression checks for the runnable states.
func TestGetStagePrompt_StateGuard_AwaitingApproval(t *testing.T) {
	testPromptStateGuard(t, run.StageStateAwaitingApproval, http.StatusConflict)
}

func TestGetStagePrompt_StateGuard_AwaitingChildren(t *testing.T) {
	testPromptStateGuard(t, run.StageStateAwaitingChildren, http.StatusConflict)
}

func TestGetStagePrompt_StateGuard_Succeeded(t *testing.T) {
	testPromptStateGuard(t, run.StageStateSucceeded, http.StatusConflict)
}

func TestGetStagePrompt_StateGuard_Failed(t *testing.T) {
	testPromptStateGuard(t, run.StageStateFailed, http.StatusConflict)
}

func TestGetStagePrompt_StateGuard_Cancelled(t *testing.T) {
	testPromptStateGuard(t, run.StageStateCancelled, http.StatusConflict)
}

func TestGetStagePrompt_StateGuard_Pending_Passes(t *testing.T) {
	testPromptStateGuard(t, run.StageStatePending, http.StatusOK)
}

func TestGetStagePrompt_StateGuard_Dispatched_Passes(t *testing.T) {
	testPromptStateGuard(t, run.StageStateDispatched, http.StatusOK)
}

func TestGetStagePrompt_StateGuard_Running_Passes(t *testing.T) {
	testPromptStateGuard(t, run.StageStateRunning, http.StatusOK)
}

func testPromptStateGuard(t *testing.T, state run.StageState, wantStatus int) {
	t.Helper()
	s, rr, sf, _ := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()
	priv, _ := sf.issue(t, runID)

	rr.runRow = &run.Run{ID: runID, Repo: "x/y", WorkflowID: "feature_change", TriggerSource: run.TriggerCLI}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeImplement, State: state}

	w := promptRequest(t, s, runID, stageID, priv, "")
	if w.Code != wantStatus {
		t.Fatalf("status = %d, want %d:\n%s", w.Code, wantStatus, w.Body.String())
	}
	if wantStatus == http.StatusConflict {
		var env errorEnvelope
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if env.Error.Code != "stage_not_runnable" {
			t.Errorf("error.code = %q, want stage_not_runnable", env.Error.Code)
		}
		if env.Error.Details["current_state"] != string(state) {
			t.Errorf("current_state = %v, want %q", env.Error.Details["current_state"], string(state))
		}
		if env.Error.Details["stage_id"] != stageID.String() {
			t.Errorf("stage_id = %v, want %q", env.Error.Details["stage_id"], stageID.String())
		}
	}
}

// TestGetStagePromptRender_StateGuard_* cover the 409 state guard on
// the SPA-facing prompt-render endpoint.
func TestGetStagePromptRender_StateGuard_AwaitingApproval(t *testing.T) {
	testPromptRenderStateGuard(t, run.StageStateAwaitingApproval, http.StatusConflict)
}

func TestGetStagePromptRender_StateGuard_AwaitingChildren(t *testing.T) {
	testPromptRenderStateGuard(t, run.StageStateAwaitingChildren, http.StatusConflict)
}

func TestGetStagePromptRender_StateGuard_Succeeded(t *testing.T) {
	testPromptRenderStateGuard(t, run.StageStateSucceeded, http.StatusConflict)
}

func TestGetStagePromptRender_StateGuard_Failed(t *testing.T) {
	testPromptRenderStateGuard(t, run.StageStateFailed, http.StatusConflict)
}

func TestGetStagePromptRender_StateGuard_Cancelled(t *testing.T) {
	testPromptRenderStateGuard(t, run.StageStateCancelled, http.StatusConflict)
}

func TestGetStagePromptRender_StateGuard_Pending_Passes(t *testing.T) {
	testPromptRenderStateGuard(t, run.StageStatePending, http.StatusOK)
}

func TestGetStagePromptRender_StateGuard_Dispatched_Passes(t *testing.T) {
	testPromptRenderStateGuard(t, run.StageStateDispatched, http.StatusOK)
}

func TestGetStagePromptRender_StateGuard_Running_Passes(t *testing.T) {
	testPromptRenderStateGuard(t, run.StageStateRunning, http.StatusOK)
}

func testPromptRenderStateGuard(t *testing.T, state run.StageState, wantStatus int) {
	t.Helper()
	s, rr, _, _ := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()

	rr.runRow = &run.Run{ID: runID, Repo: "x/y", WorkflowID: "feature_change", TriggerSource: run.TriggerCLI}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeImplement, State: state}

	req := httptest.NewRequest(http.MethodGet, "/v0/stages/"+stageID.String()+"/prompt-render", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != wantStatus {
		t.Fatalf("status = %d, want %d:\n%s", w.Code, wantStatus, w.Body.String())
	}
	if wantStatus == http.StatusConflict {
		var env errorEnvelope
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if env.Error.Code != "stage_not_runnable" {
			t.Errorf("error.code = %q, want stage_not_runnable", env.Error.Code)
		}
		if env.Error.Details["current_state"] != string(state) {
			t.Errorf("current_state = %v, want %q", env.Error.Details["current_state"], string(state))
		}
		if env.Error.Details["stage_id"] != stageID.String() {
			t.Errorf("stage_id = %v, want %q", env.Error.Details["stage_id"], stageID.String())
		}
	}
}

func TestGetStagePromptRender_DecomposedFromRunID_Present(t *testing.T) {
	s, rr, _, _ := newPromptServer(t)
	runID := uuid.New()
	parentRunID := uuid.New()
	stageID := uuid.New()

	rr.runRow = &run.Run{
		ID:             runID,
		Repo:           "x/y",
		WorkflowID:     "feature_change",
		TriggerSource:  run.TriggerCLI,
		DecomposedFrom: &parentRunID,
	}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeImplement}

	req := httptest.NewRequest(http.MethodGet, "/v0/stages/"+stageID.String()+"/prompt-render", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.DecomposedFromRunID != parentRunID.String() {
		t.Errorf("DecomposedFromRunID = %q, want %q", resp.DecomposedFromRunID, parentRunID.String())
	}
}

// specWithVerifyYAML is a minimal feature_change spec where the implement
// stage declares executor.verify.command, .timeout, and .max_iterations.
const specWithVerifyYAML = `version: "0.3"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
          verify:
            command: "scripts/test"
            timeout: "5m"
            max_iterations: 3
        produces:
          - artifact: pull_request
`

// TestGetStagePrompt_VerifyConfig_Present confirms that when the workflow
// spec declares executor.verify, the prompt response carries verify_command
// and verify_timeout_seconds.
func TestGetStagePrompt_VerifyConfig_Present(t *testing.T) {
	s, rr, _, _ := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()

	rr.runRow = &run.Run{
		ID:            runID,
		Repo:          "x/y",
		WorkflowID:    "feature_change",
		TriggerSource: run.TriggerCLI,
		WorkflowSpec:  []byte(specWithVerifyYAML),
	}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeImplement}

	req := httptest.NewRequest(http.MethodGet, "/v0/stages/"+stageID.String()+"/prompt-render", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.VerifyCommand != "scripts/test" {
		t.Errorf("VerifyCommand = %q, want %q", resp.VerifyCommand, "scripts/test")
	}
	if resp.VerifyTimeoutSeconds != 300 {
		t.Errorf("VerifyTimeoutSeconds = %d, want 300 (5m)", resp.VerifyTimeoutSeconds)
	}
	if resp.VerifyMaxIterations != 3 {
		t.Errorf("VerifyMaxIterations = %d, want 3", resp.VerifyMaxIterations)
	}
}

// TestGetStagePrompt_VerifyMaxIterations_SignedEndpoint confirms the
// signed GET /v0/stages/{id}/prompt endpoint serves verify_max_iterations
// from executor.verify.max_iterations, mirroring the prompt-render path.
func TestGetStagePrompt_VerifyMaxIterations_SignedEndpoint(t *testing.T) {
	s, rr, sf, _ := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()
	priv, _ := sf.issue(t, runID)

	rr.runRow = &run.Run{
		ID:            runID,
		Repo:          "x/y",
		WorkflowID:    "feature_change",
		TriggerSource: run.TriggerCLI,
		WorkflowSpec:  []byte(specWithVerifyYAML),
	}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeImplement}

	w := promptRequest(t, s, runID, stageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.VerifyMaxIterations != 3 {
		t.Errorf("VerifyMaxIterations = %d, want 3", resp.VerifyMaxIterations)
	}
}

// TestGetStagePrompt_VerifyConfig_Absent confirms that when the workflow
// spec declares no executor.verify block, both verify fields are omitted
// from the JSON response (omitempty).
func TestGetStagePrompt_VerifyConfig_Absent(t *testing.T) {
	s, rr, _, _ := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()

	rr.runRow = &run.Run{
		ID:            runID,
		Repo:          "x/y",
		WorkflowID:    "feature_change",
		TriggerSource: run.TriggerCLI,
		WorkflowSpec:  []byte(planStageSpecYAML30m),
	}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeImplement}

	req := httptest.NewRequest(http.MethodGet, "/v0/stages/"+stageID.String()+"/prompt-render", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	// Assert on the raw JSON bytes so omitempty behaviour is visible.
	body := w.Body.String()
	if strings.Contains(body, "verify_command") {
		t.Errorf("response JSON should not contain verify_command when spec has none:\n%s", body)
	}
	if strings.Contains(body, "verify_timeout_seconds") {
		t.Errorf("response JSON should not contain verify_timeout_seconds when spec has none:\n%s", body)
	}
}

// --- loadPriorRejectionFeedback unit tests ---

// feedbackRunRepo wraps promptRunRepo to supply canned ListRuns results.
type feedbackRunRepo struct {
	*promptRunRepo
	listResult []*run.Run
	listErr    error
}

func (r *feedbackRunRepo) ListRuns(_ context.Context, _ run.ListRunsFilter) ([]*run.Run, error) {
	if r.listErr != nil {
		return nil, r.listErr
	}
	return r.listResult, nil
}

// feedbackAuditRepo is a minimal audit.Repository for loadPriorRejectionFeedback tests.
type feedbackAuditRepo struct {
	byRunID map[uuid.UUID][]*audit.Entry
	listErr error
}

func (f *feedbackAuditRepo) Append(_ context.Context, _ audit.AppendParams) (*audit.Entry, error) {
	return nil, errors.New("not used")
}
func (f *feedbackAuditRepo) AppendChained(_ context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	rid := p.RunID
	return &audit.Entry{ID: uuid.New(), RunID: &rid}, nil
}
func (f *feedbackAuditRepo) AppendGlobalChained(_ context.Context, _ audit.GlobalChainAppendParams) (*audit.Entry, error) {
	return nil, errors.New("not used")
}
func (f *feedbackAuditRepo) Get(_ context.Context, _ uuid.UUID) (*audit.Entry, error) {
	return nil, errors.New("not used")
}
func (f *feedbackAuditRepo) ListForRun(_ context.Context, _ uuid.UUID) ([]*audit.Entry, error) {
	return nil, errors.New("not used")
}
func (f *feedbackAuditRepo) ListGlobal(_ context.Context) ([]*audit.Entry, error) {
	return nil, nil
}
func (f *feedbackAuditRepo) LastForRun(_ context.Context, _ uuid.UUID) (*audit.Entry, error) {
	return nil, errors.New("not used")
}
func (f *feedbackAuditRepo) ListForRunByCategory(_ context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	// Honour the category filter so callers that query the wrong constant
	// (e.g. a typo in CategoryStageFixupTriggered) see nothing — otherwise
	// the resolver tests would pass without pinning the constant.
	var out []*audit.Entry
	for _, e := range f.byRunID[runID] {
		if e.Category == category {
			out = append(out, e)
		}
	}
	return out, nil
}
func (f *feedbackAuditRepo) ListAll(_ context.Context, _ audit.ListAllParams) ([]*audit.Entry, error) {
	return nil, nil
}
func (f *feedbackAuditRepo) ChainsByParent(_ context.Context, _ uuid.UUID, _ bool) ([]*audit.Entry, error) {
	return nil, nil
}

func newFeedbackServer(t *testing.T, runs []*run.Run, auditByRun map[uuid.UUID][]*audit.Entry) *Server {
	t.Helper()
	rr := &feedbackRunRepo{
		promptRunRepo: newPromptRunRepo(),
		listResult:    runs,
	}
	ar := &feedbackAuditRepo{byRunID: auditByRun}
	return New(Config{
		Addr:      "127.0.0.1:0",
		RunRepo:   rr,
		AuditRepo: ar,
	})
}

func makeRejectionEntry(runID uuid.UUID, comment string) *audit.Entry {
	payload, _ := json.Marshal(map[string]any{
		"decision":          "reject",
		"rejection_comment": comment,
	})
	rid := runID
	return &audit.Entry{ID: uuid.New(), Category: "approval_submitted", RunID: &rid, Payload: payload}
}

func makeApproveEntry(runID uuid.UUID) *audit.Entry {
	payload, _ := json.Marshal(map[string]any{"decision": "approve"})
	rid := runID
	return &audit.Entry{ID: uuid.New(), Category: "approval_submitted", RunID: &rid, Payload: payload}
}

func TestLoadPriorRejectionFeedback_NoPriorRuns_ReturnsNil(t *testing.T) {
	s := newFeedbackServer(t, nil, nil)
	got := s.loadPriorRejectionFeedback(context.Background(), "x/y", "issue:42", uuid.New())
	if got != nil {
		t.Errorf("got %q, want nil (no prior runs)", *got)
	}
}

func TestLoadPriorRejectionFeedback_PriorRunNoRejection_ReturnsNil(t *testing.T) {
	priorID := uuid.New()
	currentID := uuid.New()
	s := newFeedbackServer(t,
		[]*run.Run{{ID: priorID}},
		map[uuid.UUID][]*audit.Entry{priorID: {makeApproveEntry(priorID)}},
	)
	got := s.loadPriorRejectionFeedback(context.Background(), "x/y", "issue:42", currentID)
	if got != nil {
		t.Errorf("got %q, want nil (no rejection in prior run)", *got)
	}
}

func TestLoadPriorRejectionFeedback_PriorRunRejectionEmptyComment_ReturnsNil(t *testing.T) {
	priorID := uuid.New()
	currentID := uuid.New()
	payload, _ := json.Marshal(map[string]any{"decision": "reject", "rejection_comment": ""})
	rid := priorID
	s := newFeedbackServer(t,
		[]*run.Run{{ID: priorID}},
		map[uuid.UUID][]*audit.Entry{priorID: {{ID: uuid.New(), RunID: &rid, Payload: payload}}},
	)
	got := s.loadPriorRejectionFeedback(context.Background(), "x/y", "issue:42", currentID)
	if got != nil {
		t.Errorf("got %q, want nil (rejection with empty comment)", *got)
	}
}

func TestLoadPriorRejectionFeedback_PriorRunRejectionNonEmptyComment_ReturnsComment(t *testing.T) {
	priorID := uuid.New()
	currentID := uuid.New()
	s := newFeedbackServer(t,
		[]*run.Run{{ID: priorID}},
		map[uuid.UUID][]*audit.Entry{priorID: {makeRejectionEntry(priorID, "plan is too vague")}},
	)
	got := s.loadPriorRejectionFeedback(context.Background(), "x/y", "issue:42", currentID)
	if got == nil {
		t.Fatal("got nil, want comment")
	}
	if *got != "plan is too vague" {
		t.Errorf("got %q, want 'plan is too vague'", *got)
	}
}

func TestLoadPriorRejectionFeedback_CurrentRunIDExcluded(t *testing.T) {
	currentID := uuid.New()
	// The only run in the list is the current one — must be skipped.
	s := newFeedbackServer(t,
		[]*run.Run{{ID: currentID}},
		map[uuid.UUID][]*audit.Entry{currentID: {makeRejectionEntry(currentID, "do not return this")}},
	)
	got := s.loadPriorRejectionFeedback(context.Background(), "x/y", "issue:42", currentID)
	if got != nil {
		t.Errorf("got %q, want nil (current run should be excluded)", *got)
	}
}

// --- loadPriorSchemaValidationError unit tests (#646) ---

func makeSchemaRetryEntry(runID uuid.UUID, validationErr string) *audit.Entry {
	payload, _ := json.Marshal(map[string]any{
		"validation_error": validationErr,
	})
	rid := runID
	return &audit.Entry{ID: uuid.New(), Category: "plan_schema_retry", RunID: &rid, Payload: payload}
}

func TestLoadPriorSchemaValidationError_NewestWins(t *testing.T) {
	runID := uuid.New()
	// Entries are returned ASC by ts; the newest (last) must win.
	s := newFeedbackServer(t, nil, map[uuid.UUID][]*audit.Entry{
		runID: {
			makeSchemaRetryEntry(runID, "first error"),
			makeSchemaRetryEntry(runID, "second error"),
		},
	})
	got := s.loadPriorSchemaValidationError(context.Background(), runID)
	if got == nil {
		t.Fatal("got nil, want newest validation_error")
	}
	if *got != "second error" {
		t.Errorf("got %q, want %q (newest entry wins)", *got, "second error")
	}
}

func TestLoadPriorSchemaValidationError_NoEntries_ReturnsNil(t *testing.T) {
	s := newFeedbackServer(t, nil, nil)
	if got := s.loadPriorSchemaValidationError(context.Background(), uuid.New()); got != nil {
		t.Errorf("got %q, want nil (no entries)", *got)
	}
}

func TestLoadPriorSchemaValidationError_ListError_ReturnsNil(t *testing.T) {
	rr := &feedbackRunRepo{promptRunRepo: newPromptRunRepo()}
	ar := &feedbackAuditRepo{listErr: errors.New("boom")}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: ar})
	if got := s.loadPriorSchemaValidationError(context.Background(), uuid.New()); got != nil {
		t.Errorf("got %q, want nil (list error degrades to nil)", *got)
	}
}

// TestGetStagePrompt_Implement_EchoesScopeFiles verifies that the
// implement-stage prompt response echoes the approved plan's
// scope.files into the scope_files field, so the runner can bound the
// commit to exactly those declared paths (#581).
func TestGetStagePrompt_Implement_EchoesScopeFiles(t *testing.T) {
	rr := newPromptRunRepo()
	sf := newSigningFake()
	art := newFakeArtifactRepo()

	runID := uuid.New()
	planStageID := uuid.New()
	implStageID := uuid.New()

	p := &plan.Plan{
		PlanVersion:  "standard_v1",
		Summary:      "scoped plan",
		Verification: plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
		Scope: plan.Scope{
			Files: []plan.ScopeFile{
				{Path: "backend/internal/server/prompt.go", Operation: plan.FileOpModify},
				{Path: "docs/api/v0.md", Operation: plan.FileOpModify},
			},
		},
	}
	planBytes, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	sv := "standard_v1"
	if _, err := art.Create(context.Background(), artifact.CreateParams{
		StageID:       planStageID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &sv,
		Content:       planBytes,
	}); err != nil {
		t.Fatalf("seed plan artifact: %v", err)
	}

	rr.stagesByRunID = map[uuid.UUID][]*run.Stage{
		runID: {
			{ID: planStageID, RunID: runID, Type: run.StageTypePlan},
			{ID: implStageID, RunID: runID, Type: run.StageTypeImplement},
		},
	}
	rr.getRuns[runID] = &run.Run{ID: runID, Repo: "o/r", WorkflowID: "feature_change"}
	rr.getStages[implStageID] = &run.Stage{ID: implStageID, RunID: runID, Type: run.StageTypeImplement}

	priv, _ := sf.issue(t, runID)
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		SigningRepo:  sf,
		ArtifactRepo: art,
	})
	s.promptIssueGetterOverride = &stubIssueGetter{}

	w := promptRequest(t, s, runID, implStageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.ScopeFiles) != 2 {
		t.Fatalf("scope_files len = %d, want 2: %+v", len(resp.ScopeFiles), resp.ScopeFiles)
	}
	if resp.ScopeFiles[0].Path != "backend/internal/server/prompt.go" || resp.ScopeFiles[0].Operation != "modify" {
		t.Errorf("scope_files[0] = %+v", resp.ScopeFiles[0])
	}
	if resp.ScopeFiles[1].Path != "docs/api/v0.md" || resp.ScopeFiles[1].Operation != "modify" {
		t.Errorf("scope_files[1] = %+v", resp.ScopeFiles[1])
	}
}

// makeFixupEntry builds a stage_fixup_triggered audit entry bound to the
// given stage, carrying the resolved selected concerns the prompt renderer
// reads back (matching server/fixup.go's writeFixupAudit payload shape).
func makeFixupEntry(runID, stageID uuid.UUID, concerns []planreview.Concern) *audit.Entry {
	payload, _ := json.Marshal(map[string]any{
		"stage_id":         stageID.String(),
		"selected_indices": []int{0},
		"concerns":         concerns,
		"reason":           "operator routed concerns back",
		"pass_ordinal":     1,
	})
	rid := runID
	sid := stageID
	return &audit.Entry{ID: uuid.New(), Category: CategoryStageFixupTriggered, RunID: &rid, StageID: &sid, Payload: payload}
}

// makeReportedHeadEntry builds a reported-head ledger audit entry
// (pull_request_opened / child_pushed / fixup_pushed) carrying a head_sha
// at the given timestamp, so resolveFixupExpectedHeadSHA's newest-entry
// pick can be exercised (#967).
func makeReportedHeadEntry(runID, stageID uuid.UUID, category, headSHA string, ts time.Time) *audit.Entry {
	payload, _ := json.Marshal(map[string]any{"head_sha": headSHA})
	rid := runID
	sid := stageID
	return &audit.Entry{ID: uuid.New(), Category: category, RunID: &rid, StageID: &sid,
		Timestamp: ts, Payload: payload}
}

// TestGetStagePrompt_Implement_FixupConcerns_RenderedAndFolded confirms that
// when an implement stage carries a stage_fixup_triggered audit entry, the
// prompt renders the selected concerns as binding instructions and folds a
// file the concern names into the effective scope set (#762).
func TestGetStagePrompt_Implement_FixupConcerns_RenderedAndFolded(t *testing.T) {
	rr := newPromptRunRepo()
	sf := newSigningFake()
	art := newFakeArtifactRepo()

	runID := uuid.New()
	planStageID := uuid.New()
	implStageID := uuid.New()

	p := &plan.Plan{
		PlanVersion:  "standard_v1",
		Summary:      "scoped plan",
		Verification: plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
		Scope: plan.Scope{
			Files: []plan.ScopeFile{
				{Path: "backend/internal/server/prompt.go", Operation: plan.FileOpModify},
			},
		},
	}
	planBytes, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	sv := "standard_v1"
	if _, err := art.Create(context.Background(), artifact.CreateParams{
		StageID:       planStageID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &sv,
		Content:       planBytes,
	}); err != nil {
		t.Fatalf("seed plan artifact: %v", err)
	}

	rr.stagesByRunID = map[uuid.UUID][]*run.Stage{
		runID: {
			{ID: planStageID, RunID: runID, Type: run.StageTypePlan},
			{ID: implStageID, RunID: runID, Type: run.StageTypeImplement},
		},
	}
	rr.getRuns[runID] = &run.Run{ID: runID, Repo: "o/r", WorkflowID: "feature_change"}
	rr.getStages[implStageID] = &run.Stage{ID: implStageID, RunID: runID, Type: run.StageTypeImplement}

	concerns := []planreview.Concern{
		{Severity: planreview.SeverityHigh, Category: "coverage",
			Note: "add a test in backend/internal/run/fixup_test.go for the bound"},
	}
	// Seed the reported-head ledger: a PR-open head then a NEWER fixup_pushed
	// head, so the expected-head resolver must pick the newest entry across
	// categories rather than the first one it sees (#967).
	auditByRun := map[uuid.UUID][]*audit.Entry{
		runID: {
			makeFixupEntry(runID, implStageID, concerns),
			makeReportedHeadEntry(runID, implStageID, "pull_request_opened",
				"aaaa000000000000000000000000000000000000", time.Now().Add(-2*time.Hour)),
			makeReportedHeadEntry(runID, implStageID, "fixup_pushed",
				"bbbb111111111111111111111111111111111111", time.Now().Add(-1*time.Hour)),
		},
	}

	priv, _ := sf.issue(t, runID)
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		SigningRepo:  sf,
		ArtifactRepo: art,
		AuditRepo:    &feedbackAuditRepo{byRunID: auditByRun},
	})
	s.promptIssueGetterOverride = &stubIssueGetter{}

	w := promptRequest(t, s, runID, implStageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// The prompt renders the binding fix-up concerns section.
	for _, want := range []string{
		"### Fix-up concerns",
		"[high/coverage]",
		"add a test in backend/internal/run/fixup_test.go for the bound",
	} {
		if !strings.Contains(resp.Prompt, want) {
			t.Errorf("prompt missing %q\n---\n%s", want, resp.Prompt)
		}
	}

	// The concern-named file folds into the effective scope set alongside
	// the plan's own scope file.
	paths := map[string]bool{}
	for _, f := range resp.ScopeFiles {
		paths[f.Path] = true
	}
	if !paths["backend/internal/server/prompt.go"] {
		t.Errorf("plan scope file missing from scope_files: %+v", resp.ScopeFiles)
	}
	if !paths["backend/internal/run/fixup_test.go"] {
		t.Errorf("concern-named file not folded into scope_files: %+v", resp.ScopeFiles)
	}

	// Cross-boundary assertion (#784): the response carries the fix-up wire
	// flag the runner reads, and fixup_branch matches the runner's per-stage
	// branch formula byte-for-byte. A divergence here would re-create the
	// `checkout -b <existing branch>` already-exists failure.
	if !resp.Fixup {
		t.Errorf("fixup = false, want true for a stage with an unconsumed stage_fixup_triggered entry")
	}
	wantBranch := fmt.Sprintf("fishhawk/run-%s/stage-%s", runID.String()[:8], implStageID.String()[:8])
	if resp.FixupBranch != wantBranch {
		t.Errorf("fixup_branch = %q, want %q", resp.FixupBranch, wantBranch)
	}

	// The fix-up dispatch advertises the run's recorded head — the NEWEST
	// reported head across the lineage ledger categories (#967): here the
	// fixup_pushed head, not the older pull_request_opened one.
	if want := "bbbb111111111111111111111111111111111111"; resp.FixupExpectedHeadSHA != want {
		t.Errorf("fixup_expected_head_sha = %q, want %q (the newest reported head)",
			resp.FixupExpectedHeadSHA, want)
	}
}

// TestGetStagePrompt_Implement_NoFixup_OmitsWireFlag is the negative case for
// #784: a normal implement dispatch with no stage_fixup_triggered audit entry
// must leave fixup=false and fixup_branch empty so the runner's default
// per-stage branch routing (checkout -b) stays unchanged.
func TestGetStagePrompt_Implement_NoFixup_OmitsWireFlag(t *testing.T) {
	rr := newPromptRunRepo()
	sf := newSigningFake()
	art := newFakeArtifactRepo()

	runID := uuid.New()
	planStageID := uuid.New()
	implStageID := uuid.New()

	p := &plan.Plan{
		PlanVersion:  "standard_v1",
		Summary:      "scoped plan",
		Verification: plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
		Scope: plan.Scope{
			Files: []plan.ScopeFile{
				{Path: "backend/internal/server/prompt.go", Operation: plan.FileOpModify},
			},
		},
	}
	planBytes, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	sv := "standard_v1"
	if _, err := art.Create(context.Background(), artifact.CreateParams{
		StageID:       planStageID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &sv,
		Content:       planBytes,
	}); err != nil {
		t.Fatalf("seed plan artifact: %v", err)
	}

	rr.stagesByRunID = map[uuid.UUID][]*run.Stage{
		runID: {
			{ID: planStageID, RunID: runID, Type: run.StageTypePlan},
			{ID: implStageID, RunID: runID, Type: run.StageTypeImplement},
		},
	}
	rr.getRuns[runID] = &run.Run{ID: runID, Repo: "o/r", WorkflowID: "feature_change"}
	rr.getStages[implStageID] = &run.Stage{ID: implStageID, RunID: runID, Type: run.StageTypeImplement}

	priv, _ := sf.issue(t, runID)
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		SigningRepo:  sf,
		ArtifactRepo: art,
		AuditRepo:    &feedbackAuditRepo{}, // no fix-up entry
	})
	s.promptIssueGetterOverride = &stubIssueGetter{}

	w := promptRequest(t, s, runID, implStageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Fixup {
		t.Errorf("fixup = true, want false for a normal (non-fix-up) implement dispatch")
	}
	if resp.FixupBranch != "" {
		t.Errorf("fixup_branch = %q, want empty for a normal implement dispatch", resp.FixupBranch)
	}
	if resp.FixupExpectedHeadSHA != "" {
		t.Errorf("fixup_expected_head_sha = %q, want empty for a normal implement dispatch", resp.FixupExpectedHeadSHA)
	}
}

// TestResolveFixupExpectedHeadSHA_ReadErrorOmitsField: the expected-head
// resolver is best-effort — a ListForRunByCategory failure on the
// reported-head ledger must WARN and return "" (the runner then skips the
// SHA comparison) rather than failing the dispatch (#967).
func TestResolveFixupExpectedHeadSHA_ReadErrorOmitsField(t *testing.T) {
	s := New(Config{
		Addr:      "127.0.0.1:0",
		AuditRepo: &feedbackAuditRepo{listErr: errors.New("audit store unavailable")},
	})
	if got := s.resolveFixupExpectedHeadSHA(context.Background(), uuid.New(), uuid.New()); got != "" {
		t.Errorf("resolveFixupExpectedHeadSHA = %q, want empty on a ledger read error", got)
	}
}

// TestGetStagePrompt_Implement_FixupDecomposedChild_SharedBranch covers the
// decomposed-child fix-up branch form (#784): the runner routes a decomposed
// child onto the shared parent branch fishhawk/run-<shortID(parentRunID)>, so
// a fix-up on such a child must derive the same shared branch, not the
// per-stage form.
func TestGetStagePrompt_Implement_FixupDecomposedChild_SharedBranch(t *testing.T) {
	rr := newPromptRunRepo()
	sf := newSigningFake()
	art := newFakeArtifactRepo()

	parentRunID := uuid.New()
	childRunID := uuid.New()
	planStageID := uuid.New()
	implStageID := uuid.New()

	p := &plan.Plan{
		PlanVersion:  "standard_v1",
		Summary:      "scoped plan",
		Verification: plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
		Scope: plan.Scope{
			Files: []plan.ScopeFile{
				{Path: "backend/internal/server/prompt.go", Operation: plan.FileOpModify},
			},
		},
	}
	planBytes, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	sv := "standard_v1"
	if _, err := art.Create(context.Background(), artifact.CreateParams{
		StageID:       planStageID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &sv,
		Content:       planBytes,
	}); err != nil {
		t.Fatalf("seed plan artifact: %v", err)
	}

	rr.stagesByRunID = map[uuid.UUID][]*run.Stage{
		childRunID: {
			{ID: planStageID, RunID: childRunID, Type: run.StageTypePlan},
			{ID: implStageID, RunID: childRunID, Type: run.StageTypeImplement},
		},
	}
	rr.getRuns[childRunID] = &run.Run{
		ID:             childRunID,
		Repo:           "o/r",
		WorkflowID:     "feature_change",
		DecomposedFrom: &parentRunID,
	}
	rr.getStages[implStageID] = &run.Stage{ID: implStageID, RunID: childRunID, Type: run.StageTypeImplement}

	concerns := []planreview.Concern{
		{Severity: planreview.SeverityHigh, Category: "coverage", Note: "tighten the bound"},
	}
	auditByRun := map[uuid.UUID][]*audit.Entry{
		childRunID: {makeFixupEntry(childRunID, implStageID, concerns)},
	}

	priv, _ := sf.issue(t, childRunID)
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		SigningRepo:  sf,
		ArtifactRepo: art,
		AuditRepo:    &feedbackAuditRepo{byRunID: auditByRun},
	})
	s.promptIssueGetterOverride = &stubIssueGetter{}

	w := promptRequest(t, s, childRunID, implStageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Fixup {
		t.Fatalf("fixup = false, want true")
	}
	wantBranch := "fishhawk/run-" + parentRunID.String()[:8]
	if resp.FixupBranch != wantBranch {
		t.Errorf("fixup_branch = %q, want shared parent branch %q", resp.FixupBranch, wantBranch)
	}
}

// TestResolveFixupConcerns covers the audit-payload reader directly: no
// trigger entry, a wrong-stage entry, the happy path, and a malformed payload.
func TestResolveFixupConcerns(t *testing.T) {
	runID := uuid.New()
	stageID := uuid.New()
	concerns := []planreview.Concern{
		{Severity: planreview.SeverityMedium, Category: "security", Note: "check authz"},
		{Severity: planreview.SeverityLow, Category: "scope", Note: "touch pkg/a/file.go"},
	}

	t.Run("no entries returns nil", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0", AuditRepo: &feedbackAuditRepo{}})
		rendered, joined := s.resolveFixupConcerns(context.Background(), runID, stageID)
		if rendered != nil || joined != "" {
			t.Errorf("got (%v, %q), want (nil, \"\")", rendered, joined)
		}
	})

	t.Run("entry for a different stage is ignored", func(t *testing.T) {
		other := uuid.New()
		s := New(Config{Addr: "127.0.0.1:0", AuditRepo: &feedbackAuditRepo{
			byRunID: map[uuid.UUID][]*audit.Entry{runID: {makeFixupEntry(runID, other, concerns)}},
		}})
		rendered, _ := s.resolveFixupConcerns(context.Background(), runID, stageID)
		if rendered != nil {
			t.Errorf("got %v, want nil (entry bound to a different stage)", rendered)
		}
	})

	t.Run("happy path renders and joins", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0", AuditRepo: &feedbackAuditRepo{
			byRunID: map[uuid.UUID][]*audit.Entry{runID: {makeFixupEntry(runID, stageID, concerns)}},
		}})
		rendered, joined := s.resolveFixupConcerns(context.Background(), runID, stageID)
		if len(rendered) != 2 {
			t.Fatalf("rendered len = %d, want 2: %v", len(rendered), rendered)
		}
		if rendered[0] != "[medium/security] check authz" {
			t.Errorf("rendered[0] = %q", rendered[0])
		}
		// The joined notes feed scope-file extraction.
		if !strings.Contains(joined, "touch pkg/a/file.go") {
			t.Errorf("joined notes missing the second concern: %q", joined)
		}
	})

	t.Run("malformed payload is skipped", func(t *testing.T) {
		rid := runID
		sid := stageID
		bad := &audit.Entry{ID: uuid.New(), Category: CategoryStageFixupTriggered, RunID: &rid, StageID: &sid, Payload: []byte("{not json")}
		s := New(Config{Addr: "127.0.0.1:0", AuditRepo: &feedbackAuditRepo{
			byRunID: map[uuid.UUID][]*audit.Entry{runID: {bad}},
		}})
		rendered, _ := s.resolveFixupConcerns(context.Background(), runID, stageID)
		if rendered != nil {
			t.Errorf("got %v, want nil (malformed payload)", rendered)
		}
	})
}

// makeFixupEntryWithAllowCreate builds a stage_fixup_triggered audit entry
// carrying both the selected concerns and the declared allow_create paths
// (#823), matching server/fixup.go's writeFixupAudit payload shape.
func makeFixupEntryWithAllowCreate(runID, stageID uuid.UUID, concerns []planreview.Concern, allowCreate []string) *audit.Entry {
	payload, _ := json.Marshal(map[string]any{
		"stage_id":         stageID.String(),
		"selected_indices": []int{0},
		"concerns":         concerns,
		"allow_create":     allowCreate,
		"reason":           "operator declared a net-new file",
		"pass_ordinal":     1,
	})
	rid := runID
	sid := stageID
	return &audit.Entry{ID: uuid.New(), Category: CategoryStageFixupTriggered, RunID: &rid, StageID: &sid, Payload: payload}
}

func TestResolveFixupAllowCreate(t *testing.T) {
	runID := uuid.New()
	stageID := uuid.New()
	concerns := []planreview.Concern{{Severity: planreview.SeverityMedium, Category: "scope", Note: "needs a new file"}}

	t.Run("no entries returns nil", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0", AuditRepo: &feedbackAuditRepo{}})
		if got := s.resolveFixupAllowCreate(context.Background(), runID, stageID); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})

	t.Run("entry for a different stage is ignored", func(t *testing.T) {
		other := uuid.New()
		s := New(Config{Addr: "127.0.0.1:0", AuditRepo: &feedbackAuditRepo{
			byRunID: map[uuid.UUID][]*audit.Entry{runID: {makeFixupEntryWithAllowCreate(runID, other, concerns, []string{"a/b.go"})}},
		}})
		if got := s.resolveFixupAllowCreate(context.Background(), runID, stageID); got != nil {
			t.Errorf("got %v, want nil (entry bound to a different stage)", got)
		}
	})

	t.Run("returns the newest entry's declared paths", func(t *testing.T) {
		old := makeFixupEntryWithAllowCreate(runID, stageID, concerns, []string{"old/path.go"})
		newest := makeFixupEntryWithAllowCreate(runID, stageID, concerns, []string{"new/a.go", "new/b.go"})
		s := New(Config{Addr: "127.0.0.1:0", AuditRepo: &feedbackAuditRepo{
			byRunID: map[uuid.UUID][]*audit.Entry{runID: {old, newest}},
		}})
		got := s.resolveFixupAllowCreate(context.Background(), runID, stageID)
		if len(got) != 2 || got[0] != "new/a.go" || got[1] != "new/b.go" {
			t.Errorf("got %v, want [new/a.go new/b.go] (newest entry)", got)
		}
	})

	t.Run("entry without allow_create returns nil", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0", AuditRepo: &feedbackAuditRepo{
			byRunID: map[uuid.UUID][]*audit.Entry{runID: {makeFixupEntry(runID, stageID, concerns)}},
		}})
		if got := s.resolveFixupAllowCreate(context.Background(), runID, stageID); got != nil {
			t.Errorf("got %v, want nil (no allow_create on the entry)", got)
		}
	})
}

// TestGetStagePrompt_Implement_FixupAllowCreate_Folded confirms an operator-
// declared net-new file (allow_create, #823) folds into the effective
// scope.files — the exact set the runner's #818 created-out-of-scope gate
// diffs against — while an undeclared path stays absent.
func TestGetStagePrompt_Implement_FixupAllowCreate_Folded(t *testing.T) {
	rr := newPromptRunRepo()
	sf := newSigningFake()
	art := newFakeArtifactRepo()

	runID := uuid.New()
	planStageID := uuid.New()
	implStageID := uuid.New()

	p := &plan.Plan{
		PlanVersion:  "standard_v1",
		Summary:      "scoped plan",
		Verification: plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
		Scope: plan.Scope{
			Files: []plan.ScopeFile{
				{Path: "backend/internal/server/prompt.go", Operation: plan.FileOpModify},
			},
		},
	}
	planBytes, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	sv := "standard_v1"
	if _, err := art.Create(context.Background(), artifact.CreateParams{
		StageID:       planStageID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &sv,
		Content:       planBytes,
	}); err != nil {
		t.Fatalf("seed plan artifact: %v", err)
	}

	rr.stagesByRunID = map[uuid.UUID][]*run.Stage{
		runID: {
			{ID: planStageID, RunID: runID, Type: run.StageTypePlan},
			{ID: implStageID, RunID: runID, Type: run.StageTypeImplement},
		},
	}
	rr.getRuns[runID] = &run.Run{ID: runID, Repo: "o/r", WorkflowID: "feature_change"}
	rr.getStages[implStageID] = &run.Stage{ID: implStageID, RunID: runID, Type: run.StageTypeImplement}

	concerns := []planreview.Concern{
		{Severity: planreview.SeverityMedium, Category: "scope", Note: "extract the helper into a new file"},
	}
	auditByRun := map[uuid.UUID][]*audit.Entry{
		runID: {makeFixupEntryWithAllowCreate(runID, implStageID, concerns, []string{"backend/internal/server/helper.go"})},
	}

	priv, _ := sf.issue(t, runID)
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		SigningRepo:  sf,
		ArtifactRepo: art,
		AuditRepo:    &feedbackAuditRepo{byRunID: auditByRun},
	})
	s.promptIssueGetterOverride = &stubIssueGetter{}

	w := promptRequest(t, s, runID, implStageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	paths := map[string]bool{}
	for _, f := range resp.ScopeFiles {
		paths[f.Path] = true
	}
	// The declared net-new file is folded in alongside the plan scope file.
	if !paths["backend/internal/server/prompt.go"] {
		t.Errorf("plan scope file missing from scope_files: %+v", resp.ScopeFiles)
	}
	if !paths["backend/internal/server/helper.go"] {
		t.Errorf("allow_create file not folded into scope_files: %+v", resp.ScopeFiles)
	}
	// An undeclared path is NOT in the effective scope — the #818 gate would
	// still trip for it (the silent-strip hole stays closed).
	if paths["backend/internal/server/undeclared.go"] {
		t.Errorf("undeclared path leaked into scope_files: %+v", resp.ScopeFiles)
	}
}

// TestGetStagePrompt_Implement_NoScopeFilesWhenPlanMissing confirms the
// scope_files field is omitted when no approved plan is available, so
// the runner falls back to `git add -A`.
func TestGetStagePrompt_Implement_NoScopeFilesWhenPlanMissing(t *testing.T) {
	s, rr, sf, _ := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()
	priv, _ := sf.issue(t, runID)
	rr.runRow = &run.Run{ID: runID, Repo: "o/r", WorkflowID: "feature_change"}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeImplement}

	w := promptRequest(t, s, runID, stageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ScopeFiles != nil {
		t.Errorf("scope_files = %+v, want nil/omitted when plan missing", resp.ScopeFiles)
	}
}

// TestGetStagePrompt_DecomposedChild_ScopeConstraintInjected verifies that
// when a child run has DecomposedFrom set and a matching IssueContext, the
// implement-stage prompt contains a SCOPE CONSTRAINT block with this child's
// scope_hint and the sibling's scope_hint (#541).
func TestGetStagePrompt_DecomposedChild_ScopeConstraintInjected(t *testing.T) {
	rr := newPromptRunRepo()
	sf := newSigningFake()
	art := newFakeArtifactRepo()

	parentRunID := uuid.New()
	childRunID := uuid.New()
	parentPlanStageID := uuid.New()
	childStageID := uuid.New()

	// Build a standard_v1 plan artifact with a two-sub-plan decomposition.
	parentPlan := &plan.Plan{
		PlanVersion: "standard_v1",
		Summary:     "parent plan",
		Verification: plan.Verification{
			TestStrategy: "ts",
			RollbackPlan: "rb",
		},
		Decomposition: &plan.Decomposition{
			Rationale: "scope split",
			SubPlans: []plan.SubPlanSummary{
				{Title: "Part A title", ScopeHint: "Implement Part A in pkg/a."},
				{Title: "Part B title", ScopeHint: "Implement Part B in pkg/b."},
			},
		},
	}
	planBytes, err := json.Marshal(parentPlan)
	if err != nil {
		t.Fatalf("marshal parent plan: %v", err)
	}
	sv := "standard_v1"
	if _, err := art.Create(context.Background(), artifact.CreateParams{
		StageID:       parentPlanStageID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &sv,
		Content:       planBytes,
	}); err != nil {
		t.Fatalf("seed plan artifact: %v", err)
	}

	// Seed parent run with a plan stage.
	rr.stagesByRunID = map[uuid.UUID][]*run.Stage{
		parentRunID: {
			{ID: parentPlanStageID, RunID: parentRunID, Type: run.StageTypePlan},
		},
	}
	rr.getRuns[parentRunID] = &run.Run{
		ID:   parentRunID,
		Repo: "o/r",
	}

	// Child run: DecomposedFrom=parentRunID, IssueContext.Body matches Part A.
	childBody := "## Part A title\n\nImplement Part A in pkg/a.\n\n---\n*Decomposed sub-plan.*"
	rr.getRuns[childRunID] = &run.Run{
		ID:             childRunID,
		Repo:           "o/r",
		WorkflowID:     "feature_change",
		TriggerSource:  run.TriggerCLI,
		ParentRunID:    &parentRunID,
		DecomposedFrom: &parentRunID,
		IssueContext: &run.IssueContext{
			Title: "Part A title",
			Body:  childBody,
		},
	}
	rr.getStages[childStageID] = &run.Stage{
		ID:    childStageID,
		RunID: childRunID,
		Type:  run.StageTypeImplement,
	}

	priv, _ := sf.issue(t, childRunID)

	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		SigningRepo:  sf,
		ArtifactRepo: art,
	})
	s.promptIssueGetterOverride = &stubIssueGetter{}

	w := promptRequest(t, s, childRunID, childStageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	for _, want := range []string{
		"SCOPE CONSTRAINT",
		"Implement Part A in pkg/a.",
		"Implement Part B in pkg/b.",
		"do NOT modify code in sibling scope",
	} {
		if !strings.Contains(resp.Prompt, want) {
			t.Errorf("prompt missing %q\n---\n%s", want, resp.Prompt)
		}
	}
}

// TestGetStagePrompt_DecomposedChild_ScopeFiles is the cross-boundary seam
// test for #676: it threads a per-sub-plan scope.files slice through schema
// validation -> backend plan domain type -> the prompt-response wire payload
// (the field the runner's scope_handoff/scope_drift consumer reads), and
// asserts the child's prompt response carries the MATCHED sub-plan's slice,
// not the parent's full union. The fallback subtest asserts a sub-plan
// without scope inherits the parent's full scope.files (backward compat).
func TestGetStagePrompt_DecomposedChild_ScopeFiles(t *testing.T) {
	// Parent decomposed plan: full scope is the union (a.go, b.go), but each
	// sub-plan carries its own narrower slice. Part B intentionally omits
	// scope to exercise the parent-scope fallback.
	newParentPlan := func() *plan.Plan {
		return &plan.Plan{
			PlanVersion: "standard_v1",
			Summary:     "parent plan",
			Scope: plan.Scope{
				Files: []plan.ScopeFile{
					{Path: "pkg/a/a.go", Operation: plan.FileOpCreate},
					{Path: "pkg/b/b.go", Operation: plan.FileOpModify},
				},
			},
			Verification: plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
			Decomposition: &plan.Decomposition{
				Rationale: "scope split",
				SubPlans: []plan.SubPlanSummary{
					{
						Title:     "Part A title",
						ScopeHint: "Implement Part A in pkg/a.",
						Scope: &plan.Scope{
							Files: []plan.ScopeFile{
								{Path: "pkg/a/a.go", Operation: plan.FileOpCreate},
							},
						},
					},
					{
						Title:     "Part B title",
						ScopeHint: "Implement Part B in pkg/b.",
						// No Scope — must fall back to the parent's full scope.
					},
				},
			},
		}
	}

	// seedChildPrompt seeds a parent plan artifact + a decomposed child run
	// whose IssueContext.Body matches the sub-plan named by childTitle, then
	// fetches the child's implement-stage prompt response.
	seedChildPrompt := func(t *testing.T, childTitle, childHint string) promptResponse {
		t.Helper()
		rr := newPromptRunRepo()
		sf := newSigningFake()
		art := newFakeArtifactRepo()

		parentRunID := uuid.New()
		childRunID := uuid.New()
		parentPlanStageID := uuid.New()
		childStageID := uuid.New()

		planBytes, err := json.Marshal(newParentPlan())
		if err != nil {
			t.Fatalf("marshal parent plan: %v", err)
		}
		sv := "standard_v1"
		if _, err := art.Create(context.Background(), artifact.CreateParams{
			StageID:       parentPlanStageID,
			Kind:          artifact.KindPlan,
			SchemaVersion: &sv,
			Content:       planBytes,
		}); err != nil {
			t.Fatalf("seed plan artifact: %v", err)
		}

		rr.stagesByRunID = map[uuid.UUID][]*run.Stage{
			parentRunID: {
				{ID: parentPlanStageID, RunID: parentRunID, Type: run.StageTypePlan},
			},
		}
		rr.getRuns[parentRunID] = &run.Run{ID: parentRunID, Repo: "o/r"}

		childBody := "## " + childTitle + "\n\n" + childHint + "\n\n---\n*Decomposed sub-plan.*"
		rr.getRuns[childRunID] = &run.Run{
			ID:             childRunID,
			Repo:           "o/r",
			WorkflowID:     "feature_change",
			TriggerSource:  run.TriggerCLI,
			ParentRunID:    &parentRunID,
			DecomposedFrom: &parentRunID,
			IssueContext: &run.IssueContext{
				Title: childTitle,
				Body:  childBody,
			},
		}
		rr.getStages[childStageID] = &run.Stage{
			ID:    childStageID,
			RunID: childRunID,
			Type:  run.StageTypeImplement,
		}

		priv, _ := sf.issue(t, childRunID)
		s := New(Config{
			Addr:         "127.0.0.1:0",
			RunRepo:      rr,
			SigningRepo:  sf,
			ArtifactRepo: art,
		})
		s.promptIssueGetterOverride = &stubIssueGetter{}

		w := promptRequest(t, s, childRunID, childStageID, priv, "")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
		}
		var resp promptResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return resp
	}

	scopePaths := func(sfs []scopeFile) []string {
		out := make([]string, 0, len(sfs))
		for _, f := range sfs {
			out = append(out, f.Path)
		}
		return out
	}

	t.Run("sub-plan with scope narrows to its own slice", func(t *testing.T) {
		resp := seedChildPrompt(t, "Part A title", "Implement Part A in pkg/a.")
		got := scopePaths(resp.ScopeFiles)
		want := []string{"pkg/a/a.go"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("scope_files = %v, want the sub-plan's slice %v (NOT the parent union)", got, want)
		}
	})

	t.Run("sub-plan without scope falls back to parent full scope", func(t *testing.T) {
		resp := seedChildPrompt(t, "Part B title", "Implement Part B in pkg/b.")
		got := scopePaths(resp.ScopeFiles)
		want := []string{"pkg/a/a.go", "pkg/b/b.go"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("scope_files = %v, want the parent's full scope %v", got, want)
		}
	})
}

// makeApproveWithCommentEntry builds an approval_submitted audit entry with
// decision=approve and a non-empty operator comment (the approve-with-
// conditions text loadApprovalConditions reads).
func makeApproveWithCommentEntry(runID uuid.UUID, comment string) *audit.Entry {
	payload, _ := json.Marshal(map[string]any{
		"decision": "approve",
		"comment":  comment,
	})
	rid := runID
	return &audit.Entry{ID: uuid.New(), Category: "approval_submitted", RunID: &rid, Payload: payload}
}

// TestGetStagePrompt_ApprovalConditions_DecompositionFallback is the
// integration test for #677: it crosses the audit-load -> handler ->
// rendered-prompt-text path and asserts the parent plan-gate's binding
// approve-with-conditions text propagates into a decomposed child's
// implement prompt (the #558 approval-note delivery, which a child with no
// plan stage of its own would otherwise silently drop). The standalone and
// no-conditions subtests guard the backward-compatible boundaries.
func TestGetStagePrompt_ApprovalConditions_DecompositionFallback(t *testing.T) {
	const parentCondition = "Use the orthogonal-lens reviewer; do NOT touch the legacy adapter."

	// parentPlan carries a two-sub-plan decomposition so the child can match
	// a sub-plan via its IssueContext.Body prefix (matchDecomposedSubPlan).
	newParentPlan := func() *plan.Plan {
		return &plan.Plan{
			PlanVersion:  "standard_v1",
			Summary:      "parent plan",
			Verification: plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
			Decomposition: &plan.Decomposition{
				Rationale: "scope split",
				SubPlans: []plan.SubPlanSummary{
					{Title: "Part A title", ScopeHint: "Implement Part A in pkg/a."},
					{Title: "Part B title", ScopeHint: "Implement Part B in pkg/b."},
				},
			},
		}
	}

	// seedDecomposedChild wires a parent plan artifact + a decomposed child run
	// whose IssueContext.Body matches Part A, with the supplied audit entries
	// keyed by run ID, and returns the child's implement-stage prompt response.
	seedDecomposedChild := func(t *testing.T, auditByRun map[uuid.UUID][]*audit.Entry, parentRunID uuid.UUID) promptResponse {
		t.Helper()
		rr := newPromptRunRepo()
		sf := newSigningFake()
		art := newFakeArtifactRepo()

		childRunID := uuid.New()
		parentPlanStageID := uuid.New()
		childStageID := uuid.New()

		planBytes, err := json.Marshal(newParentPlan())
		if err != nil {
			t.Fatalf("marshal parent plan: %v", err)
		}
		sv := "standard_v1"
		if _, err := art.Create(context.Background(), artifact.CreateParams{
			StageID:       parentPlanStageID,
			Kind:          artifact.KindPlan,
			SchemaVersion: &sv,
			Content:       planBytes,
		}); err != nil {
			t.Fatalf("seed plan artifact: %v", err)
		}

		rr.stagesByRunID = map[uuid.UUID][]*run.Stage{
			parentRunID: {
				{ID: parentPlanStageID, RunID: parentRunID, Type: run.StageTypePlan},
			},
		}
		rr.getRuns[parentRunID] = &run.Run{ID: parentRunID, Repo: "o/r"}

		childBody := "## Part A title\n\nImplement Part A in pkg/a.\n\n---\n*Decomposed sub-plan.*"
		rr.getRuns[childRunID] = &run.Run{
			ID:             childRunID,
			Repo:           "o/r",
			WorkflowID:     "feature_change",
			TriggerSource:  run.TriggerCLI,
			ParentRunID:    &parentRunID,
			DecomposedFrom: &parentRunID,
			IssueContext: &run.IssueContext{
				Title: "Part A title",
				Body:  childBody,
			},
		}
		rr.getStages[childStageID] = &run.Stage{
			ID:    childStageID,
			RunID: childRunID,
			Type:  run.StageTypeImplement,
		}

		priv, _ := sf.issue(t, childRunID)
		s := New(Config{
			Addr:         "127.0.0.1:0",
			RunRepo:      rr,
			SigningRepo:  sf,
			ArtifactRepo: art,
			AuditRepo:    &feedbackAuditRepo{byRunID: auditByRun},
		})
		s.promptIssueGetterOverride = &stubIssueGetter{}

		w := promptRequest(t, s, childRunID, childStageID, priv, "")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
		}
		var resp promptResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return resp
	}

	t.Run("child inherits parent approval conditions", func(t *testing.T) {
		parentRunID := uuid.New()
		resp := seedDecomposedChild(t, map[uuid.UUID][]*audit.Entry{
			parentRunID: {makeApproveWithCommentEntry(parentRunID, parentCondition)},
		}, parentRunID)
		for _, want := range []string{"### Approval conditions", parentCondition} {
			if !strings.Contains(resp.Prompt, want) {
				t.Errorf("child prompt missing %q\n---\n%s", want, resp.Prompt)
			}
		}
	})

	t.Run("child with no parent conditions renders no block", func(t *testing.T) {
		parentRunID := uuid.New()
		// Parent approved with an empty comment → no conditions.
		resp := seedDecomposedChild(t, map[uuid.UUID][]*audit.Entry{
			parentRunID: {makeApproveEntry(parentRunID)},
		}, parentRunID)
		if strings.Contains(resp.Prompt, "### Approval conditions") {
			t.Errorf("child prompt should carry no approval-conditions block:\n%s", resp.Prompt)
		}
	})

	t.Run("standalone run still renders its own conditions", func(t *testing.T) {
		const standaloneCondition = "Cap the retry budget at 2 and keep the timeout drift fix."
		rr := newPromptRunRepo()
		sf := newSigningFake()
		art := newFakeArtifactRepo()

		runID := uuid.New()
		planStageID := uuid.New()
		implStageID := uuid.New()

		standalonePlan := &plan.Plan{
			PlanVersion:  "standard_v1",
			Summary:      "standalone plan",
			Verification: plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
		}
		planBytes, err := json.Marshal(standalonePlan)
		if err != nil {
			t.Fatalf("marshal plan: %v", err)
		}
		sv := "standard_v1"
		if _, err := art.Create(context.Background(), artifact.CreateParams{
			StageID:       planStageID,
			Kind:          artifact.KindPlan,
			SchemaVersion: &sv,
			Content:       planBytes,
		}); err != nil {
			t.Fatalf("seed plan artifact: %v", err)
		}

		rr.stagesByRunID = map[uuid.UUID][]*run.Stage{
			runID: {{ID: planStageID, RunID: runID, Type: run.StageTypePlan}},
		}
		// DecomposedFrom nil → standalone; the helper reads the run's OWN
		// approval_submitted entries with no parent fallback in play.
		rr.getRuns[runID] = &run.Run{
			ID:            runID,
			Repo:          "o/r",
			WorkflowID:    "feature_change",
			TriggerSource: run.TriggerCLI,
		}
		rr.getStages[implStageID] = &run.Stage{
			ID:    implStageID,
			RunID: runID,
			Type:  run.StageTypeImplement,
		}

		priv, _ := sf.issue(t, runID)
		s := New(Config{
			Addr:         "127.0.0.1:0",
			RunRepo:      rr,
			SigningRepo:  sf,
			ArtifactRepo: art,
			AuditRepo: &feedbackAuditRepo{byRunID: map[uuid.UUID][]*audit.Entry{
				runID: {makeApproveWithCommentEntry(runID, standaloneCondition)},
			}},
		})
		s.promptIssueGetterOverride = &stubIssueGetter{}

		w := promptRequest(t, s, runID, implStageID, priv, "")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
		}
		var resp promptResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		for _, want := range []string{"### Approval conditions", standaloneCondition} {
			if !strings.Contains(resp.Prompt, want) {
				t.Errorf("standalone prompt missing %q\n---\n%s", want, resp.Prompt)
			}
		}
	})
}

func TestExtractScopePathsFromConditions(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{name: "empty", in: "", want: nil},
		{
			name: "backtick-quoted and bare paths",
			in:   "Also update `backend/internal/server/prompt.go` and docs/api/v0.md please.",
			want: []string{"backend/internal/server/prompt.go", "docs/api/v0.md"},
		},
		{
			name: "dedups repeated path",
			in:   "Touch pkg/a/file.go; then re-run pkg/a/file.go.",
			want: []string{"pkg/a/file.go"},
		},
		{
			name: "ignores prose tokens and extension-less words",
			in:   "Use and/or as needed; keep the README and the TODO list intact.",
			want: nil,
		},
		{
			name: "ignores bare filename without slash",
			in:   "Edit Makefile and config.yaml only.",
			want: nil,
		},
		{
			name: "trims trailing punctuation, parens and quotes",
			in:   `Update ("dir/sub/file.ts"), and also (lib/x/y.rb).`,
			want: []string{"dir/sub/file.ts", "lib/x/y.rb"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractScopePathsFromConditions(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("extractScopePathsFromConditions(%q) = %#v, want %#v", tc.in, got, tc.want)
			}
		})
	}
}

func TestMergeConditionScopeFiles(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	strptr := func(v string) *string { return &v }

	t.Run("no-op on empty scopeFiles", func(t *testing.T) {
		got := s.mergeConditionScopeFiles(context.Background(), nil, strptr("touch pkg/a/file.go"))
		if got != nil {
			t.Errorf("empty scope must stay empty, got %#v", got)
		}
	})

	t.Run("no-op on nil conditions", func(t *testing.T) {
		in := []scopeFile{{Path: "a/b.go", Operation: "modify"}}
		got := s.mergeConditionScopeFiles(context.Background(), in, nil)
		if !reflect.DeepEqual(got, in) {
			t.Errorf("nil conditions must not alter scope, got %#v", got)
		}
	})

	t.Run("appends a new condition path", func(t *testing.T) {
		in := []scopeFile{{Path: "a/b.go", Operation: "modify"}}
		got := s.mergeConditionScopeFiles(context.Background(), in, strptr("also edit c/d.go"))
		want := []scopeFile{
			{Path: "a/b.go", Operation: "modify"},
			{Path: "c/d.go", Operation: "modify"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %#v, want %#v", got, want)
		}
	})

	t.Run("no duplicate when condition names a declared file", func(t *testing.T) {
		in := []scopeFile{{Path: "a/b.go", Operation: "create"}}
		got := s.mergeConditionScopeFiles(context.Background(), in, strptr("keep editing a/b.go"))
		if !reflect.DeepEqual(got, in) {
			t.Errorf("already-declared file must not be duplicated, got %#v", got)
		}
	})

	t.Run("no-op when conditions name no path", func(t *testing.T) {
		in := []scopeFile{{Path: "a/b.go", Operation: "modify"}}
		got := s.mergeConditionScopeFiles(context.Background(), in, strptr("use the orthogonal-lens reviewer and/or skip"))
		if !reflect.DeepEqual(got, in) {
			t.Errorf("non-path conditions must not alter scope, got %#v", got)
		}
	})
}

// TestGetStagePrompt_Implement_ConditionFileFoldedIntoScope crosses the full
// audit-load -> resolveApprovalConditions -> mergeConditionScopeFiles ->
// promptResponse.ScopeFiles seam (#730): an approved plan declares one scope
// file, the binding approve-with-conditions comment names a SECOND file, and
// the rendered implement-prompt's scope_files must carry BOTH — proving a
// condition-authorized edit ships as a declared path rather than benign
// scope_drift. The negative guard asserts a plan-missing (empty scope) run is
// NOT augmented, preserving the runner's git add -A fallback.
func TestGetStagePrompt_Implement_ConditionFileFoldedIntoScope(t *testing.T) {
	const plannedFile = "backend/internal/server/prompt.go"
	const conditionFile = "backend/internal/server/runs_fake_test.go"

	t.Run("condition file folded into declared scope", func(t *testing.T) {
		rr := newPromptRunRepo()
		sf := newSigningFake()
		art := newFakeArtifactRepo()

		runID := uuid.New()
		planStageID := uuid.New()
		implStageID := uuid.New()

		p := &plan.Plan{
			PlanVersion:  "standard_v1",
			Summary:      "scoped plan",
			Verification: plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
			Scope: plan.Scope{
				Files: []plan.ScopeFile{
					{Path: plannedFile, Operation: plan.FileOpModify},
				},
			},
		}
		planBytes, err := json.Marshal(p)
		if err != nil {
			t.Fatalf("marshal plan: %v", err)
		}
		sv := "standard_v1"
		if _, err := art.Create(context.Background(), artifact.CreateParams{
			StageID:       planStageID,
			Kind:          artifact.KindPlan,
			SchemaVersion: &sv,
			Content:       planBytes,
		}); err != nil {
			t.Fatalf("seed plan artifact: %v", err)
		}

		rr.stagesByRunID = map[uuid.UUID][]*run.Stage{
			runID: {{ID: planStageID, RunID: runID, Type: run.StageTypePlan}},
		}
		rr.getRuns[runID] = &run.Run{
			ID:            runID,
			Repo:          "o/r",
			WorkflowID:    "feature_change",
			TriggerSource: run.TriggerCLI,
		}
		rr.getStages[implStageID] = &run.Stage{ID: implStageID, RunID: runID, Type: run.StageTypeImplement}

		condition := "Also update `" + conditionFile + "` to seed the audit entry."
		priv, _ := sf.issue(t, runID)
		s := New(Config{
			Addr:         "127.0.0.1:0",
			RunRepo:      rr,
			SigningRepo:  sf,
			ArtifactRepo: art,
			AuditRepo: &feedbackAuditRepo{byRunID: map[uuid.UUID][]*audit.Entry{
				runID: {makeApproveWithCommentEntry(runID, condition)},
			}},
		})
		s.promptIssueGetterOverride = &stubIssueGetter{}

		w := promptRequest(t, s, runID, implStageID, priv, "")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
		}
		var resp promptResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		got := make(map[string]bool, len(resp.ScopeFiles))
		for _, f := range resp.ScopeFiles {
			got[f.Path] = true
		}
		for _, want := range []string{plannedFile, conditionFile} {
			if !got[want] {
				t.Errorf("resp.ScopeFiles missing %q; got %#v", want, resp.ScopeFiles)
			}
		}
	})

	t.Run("plan-missing run is not augmented", func(t *testing.T) {
		rr := newPromptRunRepo()
		sf := newSigningFake()
		art := newFakeArtifactRepo()

		runID := uuid.New()
		implStageID := uuid.New()

		// No plan artifact seeded → empty scope (plan_missing_for_implement).
		rr.stagesByRunID = map[uuid.UUID][]*run.Stage{runID: {}}
		rr.getRuns[runID] = &run.Run{
			ID:            runID,
			Repo:          "o/r",
			WorkflowID:    "feature_change",
			TriggerSource: run.TriggerCLI,
		}
		rr.getStages[implStageID] = &run.Stage{ID: implStageID, RunID: runID, Type: run.StageTypeImplement}

		condition := "Also update `" + conditionFile + "` to seed the audit entry."
		priv, _ := sf.issue(t, runID)
		s := New(Config{
			Addr:         "127.0.0.1:0",
			RunRepo:      rr,
			SigningRepo:  sf,
			ArtifactRepo: art,
			AuditRepo: &feedbackAuditRepo{byRunID: map[uuid.UUID][]*audit.Entry{
				runID: {makeApproveWithCommentEntry(runID, condition)},
			}},
		})
		s.promptIssueGetterOverride = &stubIssueGetter{}

		w := promptRequest(t, s, runID, implStageID, priv, "")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
		}
		var resp promptResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.ScopeFiles != nil {
			t.Errorf("plan-missing run must keep empty scope (git add -A fallback), got %#v", resp.ScopeFiles)
		}
	})
}

// makeApproveWithScopeFilesEntry builds an approval_submitted audit entry with
// decision=approve carrying the structured add_scope_files slice (#824) that
// loadApprovalAddScopeFiles reads back.
func makeApproveWithScopeFilesEntry(runID uuid.UUID, addScopeFiles []string) *audit.Entry {
	payload, _ := json.Marshal(map[string]any{
		"decision":        "approve",
		"add_scope_files": addScopeFiles,
	})
	rid := runID
	return &audit.Entry{ID: uuid.New(), Category: "approval_submitted", RunID: &rid, Payload: payload}
}

// TestGetStagePrompt_Implement_AddScopeFilesFoldedIntoScope crosses the full
// #824 seam: persisted approval_submitted.add_scope_files ->
// resolveApprovalAddScopeFiles -> mergeStructuredScopeFiles ->
// promptResponse.ScopeFiles. An approved plan declares one scope file; the
// structured add_scope_files names three the prose fold cannot reach — a
// DIRECTORY (trailing slash), an extensionless ROOT file (go.work), and a
// slash-path with a dotted name — and all four must surface on the rendered
// implement-prompt scope. Subtests pin the empty-scope guard and decomposition-
// parent inheritance.
func TestGetStagePrompt_Implement_AddScopeFilesFoldedIntoScope(t *testing.T) {
	const plannedFile = "backend/internal/server/prompt.go"
	structured := []string{
		"backend/internal/agenteval/testdata/corpus/newcase/", // directory
		"go.work",                  // extensionless repo-root file
		"docs/spec/standard_v1.md", // slash-path with a dotted name
	}

	t.Run("structured paths folded into declared scope", func(t *testing.T) {
		rr := newPromptRunRepo()
		sf := newSigningFake()
		art := newFakeArtifactRepo()

		runID := uuid.New()
		planStageID := uuid.New()
		implStageID := uuid.New()

		p := &plan.Plan{
			PlanVersion:  "standard_v1",
			Summary:      "scoped plan",
			Verification: plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
			Scope: plan.Scope{
				Files: []plan.ScopeFile{{Path: plannedFile, Operation: plan.FileOpModify}},
			},
		}
		planBytes, err := json.Marshal(p)
		if err != nil {
			t.Fatalf("marshal plan: %v", err)
		}
		sv := "standard_v1"
		if _, err := art.Create(context.Background(), artifact.CreateParams{
			StageID:       planStageID,
			Kind:          artifact.KindPlan,
			SchemaVersion: &sv,
			Content:       planBytes,
		}); err != nil {
			t.Fatalf("seed plan artifact: %v", err)
		}

		rr.stagesByRunID = map[uuid.UUID][]*run.Stage{
			runID: {{ID: planStageID, RunID: runID, Type: run.StageTypePlan}},
		}
		rr.getRuns[runID] = &run.Run{
			ID:            runID,
			Repo:          "o/r",
			WorkflowID:    "feature_change",
			TriggerSource: run.TriggerCLI,
		}
		rr.getStages[implStageID] = &run.Stage{ID: implStageID, RunID: runID, Type: run.StageTypeImplement}

		priv, _ := sf.issue(t, runID)
		s := New(Config{
			Addr:         "127.0.0.1:0",
			RunRepo:      rr,
			SigningRepo:  sf,
			ArtifactRepo: art,
			AuditRepo: &feedbackAuditRepo{byRunID: map[uuid.UUID][]*audit.Entry{
				runID: {makeApproveWithScopeFilesEntry(runID, structured)},
			}},
		})
		s.promptIssueGetterOverride = &stubIssueGetter{}

		w := promptRequest(t, s, runID, implStageID, priv, "")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
		}
		var resp promptResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		got := make(map[string]bool, len(resp.ScopeFiles))
		for _, f := range resp.ScopeFiles {
			got[f.Path] = true
		}
		for _, want := range append([]string{plannedFile}, structured...) {
			if !got[want] {
				t.Errorf("resp.ScopeFiles missing %q; got %#v", want, resp.ScopeFiles)
			}
		}
	})

	t.Run("plan-missing run is not augmented", func(t *testing.T) {
		rr := newPromptRunRepo()
		sf := newSigningFake()
		art := newFakeArtifactRepo()

		runID := uuid.New()
		implStageID := uuid.New()

		// No plan artifact → empty scope. The structured fold must keep it
		// empty so the runner's git add -A fallback isn't silently narrowed.
		rr.stagesByRunID = map[uuid.UUID][]*run.Stage{runID: {}}
		rr.getRuns[runID] = &run.Run{
			ID:            runID,
			Repo:          "o/r",
			WorkflowID:    "feature_change",
			TriggerSource: run.TriggerCLI,
		}
		rr.getStages[implStageID] = &run.Stage{ID: implStageID, RunID: runID, Type: run.StageTypeImplement}

		priv, _ := sf.issue(t, runID)
		s := New(Config{
			Addr:         "127.0.0.1:0",
			RunRepo:      rr,
			SigningRepo:  sf,
			ArtifactRepo: art,
			AuditRepo: &feedbackAuditRepo{byRunID: map[uuid.UUID][]*audit.Entry{
				runID: {makeApproveWithScopeFilesEntry(runID, structured)},
			}},
		})
		s.promptIssueGetterOverride = &stubIssueGetter{}

		w := promptRequest(t, s, runID, implStageID, priv, "")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
		}
		var resp promptResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.ScopeFiles != nil {
			t.Errorf("plan-missing run must keep empty scope, got %#v", resp.ScopeFiles)
		}
	})

	t.Run("decomposed child inherits parent add_scope_files", func(t *testing.T) {
		rr := newPromptRunRepo()
		sf := newSigningFake()
		art := newFakeArtifactRepo()

		parentRunID := uuid.New()
		childRunID := uuid.New()
		parentPlanStageID := uuid.New()
		childStageID := uuid.New()

		// Parent plan declares a scope file so the child's resolved scope is
		// non-empty (the fold guard requires it) — the child matches no
		// sub-plan, so it falls back to the parent's top-level scope.
		parentPlan := &plan.Plan{
			PlanVersion:  "standard_v1",
			Summary:      "parent plan",
			Verification: plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
			Scope: plan.Scope{
				Files: []plan.ScopeFile{{Path: plannedFile, Operation: plan.FileOpModify}},
			},
		}
		planBytes, err := json.Marshal(parentPlan)
		if err != nil {
			t.Fatalf("marshal parent plan: %v", err)
		}
		sv := "standard_v1"
		if _, err := art.Create(context.Background(), artifact.CreateParams{
			StageID:       parentPlanStageID,
			Kind:          artifact.KindPlan,
			SchemaVersion: &sv,
			Content:       planBytes,
		}); err != nil {
			t.Fatalf("seed plan artifact: %v", err)
		}

		rr.stagesByRunID = map[uuid.UUID][]*run.Stage{
			parentRunID: {{ID: parentPlanStageID, RunID: parentRunID, Type: run.StageTypePlan}},
		}
		rr.getRuns[parentRunID] = &run.Run{ID: parentRunID, Repo: "o/r"}
		rr.getRuns[childRunID] = &run.Run{
			ID:             childRunID,
			Repo:           "o/r",
			WorkflowID:     "feature_change",
			TriggerSource:  run.TriggerCLI,
			ParentRunID:    &parentRunID,
			DecomposedFrom: &parentRunID,
		}
		rr.getStages[childStageID] = &run.Stage{ID: childStageID, RunID: childRunID, Type: run.StageTypeImplement}

		priv, _ := sf.issue(t, childRunID)
		s := New(Config{
			Addr:         "127.0.0.1:0",
			RunRepo:      rr,
			SigningRepo:  sf,
			ArtifactRepo: art,
			// add_scope_files is keyed to the PARENT run; the child has no
			// gate of its own, so resolveApprovalAddScopeFiles must walk up.
			AuditRepo: &feedbackAuditRepo{byRunID: map[uuid.UUID][]*audit.Entry{
				parentRunID: {makeApproveWithScopeFilesEntry(parentRunID, structured)},
			}},
		})
		s.promptIssueGetterOverride = &stubIssueGetter{}

		w := promptRequest(t, s, childRunID, childStageID, priv, "")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
		}
		var resp promptResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		got := make(map[string]bool, len(resp.ScopeFiles))
		for _, f := range resp.ScopeFiles {
			got[f.Path] = true
		}
		for _, want := range structured {
			if !got[want] {
				t.Errorf("child resp.ScopeFiles missing inherited %q; got %#v", want, resp.ScopeFiles)
			}
		}
	})
}

// TestAddScopeFiles_DoesNotBypassForbiddenPathsGate is the #824 security
// assertion (added at approval): folding a path into the implement scope via
// add_scope_files must NOT let it slip past the forbidden_paths policy gate.
// The two layers are independent by construction — the structured fold only
// shapes promptResponse.ScopeFiles (the runner's staging set), while the
// category-B gate is policy.Evaluate(diff, constraints), which reads the
// PRODUCED diff and the spec's forbidden_paths and has no scope.files input at
// all. This test pins both halves: (1) the fold genuinely stages the forbidden
// path into scope, and (2) the same path in the produced diff is still a
// forbidden_paths violation, so it fails category-B regardless of the fold.
func TestAddScopeFiles_DoesNotBypassForbiddenPathsGate(t *testing.T) {
	const forbidden = ".github/workflows/x.yml"

	// (1) The structured fold stages the forbidden path into scope.
	rr := newPromptRunRepo()
	sf := newSigningFake()
	art := newFakeArtifactRepo()

	runID := uuid.New()
	planStageID := uuid.New()
	implStageID := uuid.New()

	p := &plan.Plan{
		PlanVersion:  "standard_v1",
		Summary:      "scoped plan",
		Verification: plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
		Scope: plan.Scope{
			Files: []plan.ScopeFile{{Path: "backend/internal/server/prompt.go", Operation: plan.FileOpModify}},
		},
	}
	planBytes, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	sv := "standard_v1"
	if _, err := art.Create(context.Background(), artifact.CreateParams{
		StageID:       planStageID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &sv,
		Content:       planBytes,
	}); err != nil {
		t.Fatalf("seed plan artifact: %v", err)
	}
	rr.stagesByRunID = map[uuid.UUID][]*run.Stage{
		runID: {{ID: planStageID, RunID: runID, Type: run.StageTypePlan}},
	}
	rr.getRuns[runID] = &run.Run{ID: runID, Repo: "o/r", WorkflowID: "feature_change", TriggerSource: run.TriggerCLI}
	rr.getStages[implStageID] = &run.Stage{ID: implStageID, RunID: runID, Type: run.StageTypeImplement}

	priv, _ := sf.issue(t, runID)
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		SigningRepo:  sf,
		ArtifactRepo: art,
		AuditRepo: &feedbackAuditRepo{byRunID: map[uuid.UUID][]*audit.Entry{
			runID: {makeApproveWithScopeFilesEntry(runID, []string{forbidden})},
		}},
	})
	s.promptIssueGetterOverride = &stubIssueGetter{}

	w := promptRequest(t, s, runID, implStageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	folded := false
	for _, f := range resp.ScopeFiles {
		if f.Path == forbidden {
			folded = true
		}
	}
	if !folded {
		t.Fatalf("precondition: add_scope_files did not fold %q into scope; got %#v", forbidden, resp.ScopeFiles)
	}

	// (2) The produced diff touching that same path still violates the
	// forbidden_paths gate — the fold gave it no special pass.
	diff := policy.Diff{ChangedFiles: []policy.ChangedFile{{Path: forbidden, Status: policy.StatusAdded}}}
	constraints := policy.Constraints{ForbiddenPaths: []string{".github/**"}}
	violations := policy.Evaluate(diff, constraints)
	if len(violations) == 0 {
		t.Fatalf("forbidden_paths gate did not fire on folded path %q — fold must NOT bypass the gate", forbidden)
	}
	if violations[0].Constraint != "forbidden_paths" {
		t.Errorf("violation = %q, want forbidden_paths", violations[0].Constraint)
	}
}

// TestGetStagePrompt_Implement_ApprovedScopeAmendmentsFolded crosses the
// prompt-side half of the #961 activation path: persisted scope_amendments
// rows -> resolveApprovedScopeAmendments -> mergeApprovedScopeAmendments ->
// promptResponse.ScopeFiles. Only APPROVED rows fold; pending and denied
// rows confer nothing; paths already in scope dedupe by path; an empty
// (plan-missing) scope stays empty.
func TestGetStagePrompt_Implement_ApprovedScopeAmendmentsFolded(t *testing.T) {
	const plannedFile = "backend/internal/server/prompt.go"

	seedAmendments := func(sa *fakeScopeAmendmentRepo, runID, stageID uuid.UUID) {
		approve := func(id uuid.UUID) {
			if _, err := sa.Decide(context.Background(), scopeamendment.DecideParams{
				ID: id, Status: scopeamendment.StatusApproved, Reason: "ok", DecidedBy: "github:operator",
			}); err != nil {
				t.Fatalf("approve: %v", err)
			}
		}
		// Approved: one net-new file, one modify, plus the already-
		// declared plan file (dedupe check).
		a, err := sa.Create(context.Background(), scopeamendment.CreateParams{
			RunID: runID, StageID: stageID,
			Paths: []scopeamendment.PathEntry{
				{Path: "backend/internal/server/newfile.go", Operation: scopeamendment.OperationCreate},
				{Path: "docs/extra.md", Operation: scopeamendment.OperationModify},
				{Path: plannedFile, Operation: scopeamendment.OperationModify},
			},
			Reason: "seam",
		})
		if err != nil {
			t.Fatal(err)
		}
		approve(a.ID)
		// Pending: must NOT fold.
		if _, err := sa.Create(context.Background(), scopeamendment.CreateParams{
			RunID: runID, StageID: stageID,
			Paths:  []scopeamendment.PathEntry{{Path: "pending/never.go", Operation: scopeamendment.OperationModify}},
			Reason: "pending",
		}); err != nil {
			t.Fatal(err)
		}
		// Denied: must NOT fold.
		d, err := sa.Create(context.Background(), scopeamendment.CreateParams{
			RunID: runID, StageID: stageID,
			Paths:  []scopeamendment.PathEntry{{Path: "denied/never.go", Operation: scopeamendment.OperationModify}},
			Reason: "denied",
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := sa.Decide(context.Background(), scopeamendment.DecideParams{
			ID: d.ID, Status: scopeamendment.StatusDenied, Reason: "no", DecidedBy: "github:operator",
		}); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("approved paths folded, pending and denied excluded, deduped", func(t *testing.T) {
		rr := newPromptRunRepo()
		sf := newSigningFake()
		art := newFakeArtifactRepo()
		sa := newFakeScopeAmendmentRepo()

		runID := uuid.New()
		planStageID := uuid.New()
		implStageID := uuid.New()

		p := &plan.Plan{
			PlanVersion:  "standard_v1",
			Summary:      "scoped plan",
			Verification: plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
			Scope: plan.Scope{
				Files: []plan.ScopeFile{{Path: plannedFile, Operation: plan.FileOpModify}},
			},
		}
		planBytes, err := json.Marshal(p)
		if err != nil {
			t.Fatalf("marshal plan: %v", err)
		}
		sv := "standard_v1"
		if _, err := art.Create(context.Background(), artifact.CreateParams{
			StageID:       planStageID,
			Kind:          artifact.KindPlan,
			SchemaVersion: &sv,
			Content:       planBytes,
		}); err != nil {
			t.Fatalf("seed plan artifact: %v", err)
		}

		rr.stagesByRunID = map[uuid.UUID][]*run.Stage{
			runID: {{ID: planStageID, RunID: runID, Type: run.StageTypePlan}},
		}
		rr.getRuns[runID] = &run.Run{
			ID:            runID,
			Repo:          "o/r",
			WorkflowID:    "feature_change",
			TriggerSource: run.TriggerCLI,
		}
		rr.getStages[implStageID] = &run.Stage{ID: implStageID, RunID: runID, Type: run.StageTypeImplement}

		seedAmendments(sa, runID, implStageID)

		priv, _ := sf.issue(t, runID)
		s := New(Config{
			Addr:               "127.0.0.1:0",
			RunRepo:            rr,
			SigningRepo:        sf,
			ArtifactRepo:       art,
			ScopeAmendmentRepo: sa,
		})
		s.promptIssueGetterOverride = &stubIssueGetter{}

		w := promptRequest(t, s, runID, implStageID, priv, "")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
		}
		var resp promptResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		counts := map[string]int{}
		for _, f := range resp.ScopeFiles {
			counts[f.Path]++
		}
		for _, want := range []string{plannedFile, "backend/internal/server/newfile.go", "docs/extra.md"} {
			if counts[want] != 1 {
				t.Errorf("ScopeFiles[%q] count = %d, want exactly 1; got %#v", want, counts[want], resp.ScopeFiles)
			}
		}
		for _, never := range []string{"pending/never.go", "denied/never.go"} {
			if counts[never] != 0 {
				t.Errorf("ScopeFiles must not contain %q (undecided/denied)", never)
			}
		}
	})

	t.Run("plan-missing run is not augmented", func(t *testing.T) {
		rr := newPromptRunRepo()
		sf := newSigningFake()
		art := newFakeArtifactRepo()
		sa := newFakeScopeAmendmentRepo()

		runID := uuid.New()
		implStageID := uuid.New()

		rr.stagesByRunID = map[uuid.UUID][]*run.Stage{runID: {}}
		rr.getRuns[runID] = &run.Run{
			ID:            runID,
			Repo:          "o/r",
			WorkflowID:    "feature_change",
			TriggerSource: run.TriggerCLI,
		}
		rr.getStages[implStageID] = &run.Stage{ID: implStageID, RunID: runID, Type: run.StageTypeImplement}

		seedAmendments(sa, runID, implStageID)

		priv, _ := sf.issue(t, runID)
		s := New(Config{
			Addr:               "127.0.0.1:0",
			RunRepo:            rr,
			SigningRepo:        sf,
			ArtifactRepo:       art,
			ScopeAmendmentRepo: sa,
		})
		s.promptIssueGetterOverride = &stubIssueGetter{}

		w := promptRequest(t, s, runID, implStageID, priv, "")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
		}
		var resp promptResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.ScopeFiles != nil {
			t.Errorf("plan-missing run must keep empty scope (git add -A fallback), got %#v", resp.ScopeFiles)
		}
	})
}

// TestResolveApprovalConditions_ParentRunIDFallback covers the #978
// single-level ParentRunID fallback: a CI-retry / category-B recovery
// child (ParentRunID set, DecomposedFrom nil) has no plan gate of its
// own, so the parent's binding approve-with-conditions text must reach
// its implement prompt. Own-run entries win; the DecomposedFrom branch
// keeps precedence over ParentRunID; nil when neither yields anything.
func TestResolveApprovalConditions_ParentRunIDFallback(t *testing.T) {
	parentID := uuid.New()
	decompParentID := uuid.New()
	const ownCondition = "own: keep the adapter."
	const parentCondition = "parent: keep the recover endpoint idempotent."
	const decompCondition = "decomp: split per module."

	newSrv := func(byRun map[uuid.UUID][]*audit.Entry) *Server {
		return New(Config{Addr: "127.0.0.1:0", AuditRepo: &feedbackAuditRepo{byRunID: byRun}})
	}

	t.Run("own entries win over parent", func(t *testing.T) {
		runID := uuid.New()
		s := newSrv(map[uuid.UUID][]*audit.Entry{
			runID:    {makeApproveWithCommentEntry(runID, ownCondition)},
			parentID: {makeApproveWithCommentEntry(parentID, parentCondition)},
		})
		got := s.resolveApprovalConditions(context.Background(), &run.Run{ID: runID, ParentRunID: &parentID})
		if got == nil || *got != ownCondition {
			t.Errorf("got %v, want own condition", got)
		}
	})

	t.Run("parent inherited when own absent", func(t *testing.T) {
		runID := uuid.New()
		s := newSrv(map[uuid.UUID][]*audit.Entry{
			parentID: {makeApproveWithCommentEntry(parentID, parentCondition)},
		})
		got := s.resolveApprovalConditions(context.Background(), &run.Run{ID: runID, ParentRunID: &parentID})
		if got == nil || *got != parentCondition {
			t.Errorf("got %v, want parent condition", got)
		}
	})

	t.Run("DecomposedFrom precedence preserved", func(t *testing.T) {
		runID := uuid.New()
		s := newSrv(map[uuid.UUID][]*audit.Entry{
			decompParentID: {makeApproveWithCommentEntry(decompParentID, decompCondition)},
			parentID:       {makeApproveWithCommentEntry(parentID, parentCondition)},
		})
		got := s.resolveApprovalConditions(context.Background(), &run.Run{
			ID: runID, ParentRunID: &parentID, DecomposedFrom: &decompParentID,
		})
		if got == nil || *got != decompCondition {
			t.Errorf("got %v, want decomposition parent's condition", got)
		}
		// A decomposed child whose decomposition parent has no
		// conditions does NOT fall through to ParentRunID — the
		// decomposition branch terminates the lookup.
		s = newSrv(map[uuid.UUID][]*audit.Entry{
			parentID: {makeApproveWithCommentEntry(parentID, parentCondition)},
		})
		got = s.resolveApprovalConditions(context.Background(), &run.Run{
			ID: runID, ParentRunID: &parentID, DecomposedFrom: &decompParentID,
		})
		if got != nil {
			t.Errorf("got %v, want nil (decomposition branch terminates)", got)
		}
	})

	t.Run("nil when neither", func(t *testing.T) {
		runID := uuid.New()
		s := newSrv(map[uuid.UUID][]*audit.Entry{})
		if got := s.resolveApprovalConditions(context.Background(), &run.Run{ID: runID}); got != nil {
			t.Errorf("standalone run with no entries: got %v, want nil", got)
		}
		if got := s.resolveApprovalConditions(context.Background(), &run.Run{ID: runID, ParentRunID: &parentID}); got != nil {
			t.Errorf("parented run with no entries anywhere: got %v, want nil", got)
		}
	})
}

// TestResolveApprovalAddScopeFiles_ParentRunIDFallback mirrors the
// conditions fallback test for the #824 structured add_scope_files
// slice across the #978 ParentRunID boundary.
func TestResolveApprovalAddScopeFiles_ParentRunIDFallback(t *testing.T) {
	parentID := uuid.New()
	decompParentID := uuid.New()
	ownPaths := []string{"own/a.go"}
	parentPaths := []string{"parent/b.go", "parent/c.md"}
	decompPaths := []string{"decomp/d.go"}

	newSrv := func(byRun map[uuid.UUID][]*audit.Entry) *Server {
		return New(Config{Addr: "127.0.0.1:0", AuditRepo: &feedbackAuditRepo{byRunID: byRun}})
	}

	t.Run("own entries win over parent", func(t *testing.T) {
		runID := uuid.New()
		s := newSrv(map[uuid.UUID][]*audit.Entry{
			runID:    {makeApproveWithScopeFilesEntry(runID, ownPaths)},
			parentID: {makeApproveWithScopeFilesEntry(parentID, parentPaths)},
		})
		got := s.resolveApprovalAddScopeFiles(context.Background(), &run.Run{ID: runID, ParentRunID: &parentID})
		if !reflect.DeepEqual(got, ownPaths) {
			t.Errorf("got %v, want own paths %v", got, ownPaths)
		}
	})

	t.Run("parent inherited when own absent", func(t *testing.T) {
		runID := uuid.New()
		s := newSrv(map[uuid.UUID][]*audit.Entry{
			parentID: {makeApproveWithScopeFilesEntry(parentID, parentPaths)},
		})
		got := s.resolveApprovalAddScopeFiles(context.Background(), &run.Run{ID: runID, ParentRunID: &parentID})
		if !reflect.DeepEqual(got, parentPaths) {
			t.Errorf("got %v, want parent paths %v", got, parentPaths)
		}
	})

	t.Run("DecomposedFrom precedence preserved", func(t *testing.T) {
		runID := uuid.New()
		s := newSrv(map[uuid.UUID][]*audit.Entry{
			decompParentID: {makeApproveWithScopeFilesEntry(decompParentID, decompPaths)},
			parentID:       {makeApproveWithScopeFilesEntry(parentID, parentPaths)},
		})
		got := s.resolveApprovalAddScopeFiles(context.Background(), &run.Run{
			ID: runID, ParentRunID: &parentID, DecomposedFrom: &decompParentID,
		})
		if !reflect.DeepEqual(got, decompPaths) {
			t.Errorf("got %v, want decomposition parent's paths %v", got, decompPaths)
		}
	})

	t.Run("nil when neither", func(t *testing.T) {
		runID := uuid.New()
		s := newSrv(map[uuid.UUID][]*audit.Entry{})
		if got := s.resolveApprovalAddScopeFiles(context.Background(), &run.Run{ID: runID, ParentRunID: &parentID}); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})
}
