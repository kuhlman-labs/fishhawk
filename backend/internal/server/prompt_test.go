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
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// promptRunRepo is a run.Repository fake that supports GetStage +
// GetRun. Other methods panic to make accidental calls loud.
type promptRunRepo struct {
	stage                *run.Stage
	stageErr             error
	runRow               *run.Run
	runErr               error
	getStages            map[uuid.UUID]*run.Stage
	getRuns              map[uuid.UUID]*run.Run
	setPRURLCalls        []promptSetPRURLCall
	transitionStageCalls []promptTransitionStageCall
	// stagesByRunID backs ListStagesForRun. When non-nil, the map is
	// consulted; when nil the method returns an error so accidental
	// calls in tests that don't seed it stay loud.
	stagesByRunID map[uuid.UUID][]*run.Stage
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

func (r *promptRunRepo) CreateRun(context.Context, run.CreateRunParams) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *promptRunRepo) GetRunByIdempotencyKey(context.Context, string, string) (*run.Run, error) {
	return nil, run.ErrNotFound
}
func (r *promptRunRepo) ListRuns(context.Context, run.ListRunsFilter) ([]*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *promptRunRepo) TransitionRun(context.Context, uuid.UUID, run.State) (*run.Run, error) {
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
// stage declares executor.verify.command and executor.verify.timeout.
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
func (f *feedbackAuditRepo) ListForRunByCategory(_ context.Context, runID uuid.UUID, _ string) ([]*audit.Entry, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.byRunID[runID], nil
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
	return &audit.Entry{ID: uuid.New(), RunID: &rid, Payload: payload}
}

func makeApproveEntry(runID uuid.UUID) *audit.Entry {
	payload, _ := json.Marshal(map[string]any{"decision": "approve"})
	rid := runID
	return &audit.Entry{ID: uuid.New(), RunID: &rid, Payload: payload}
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
