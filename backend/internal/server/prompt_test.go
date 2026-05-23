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

	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// promptRunRepo is a run.Repository fake that supports GetStage +
// GetRun. Other methods panic to make accidental calls loud.
type promptRunRepo struct {
	stage         *run.Stage
	stageErr      error
	runRow        *run.Run
	runErr        error
	getStages     map[uuid.UUID]*run.Stage
	getRuns       map[uuid.UUID]*run.Run
	setPRURLCalls []promptSetPRURLCall
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
func (r *promptRunRepo) ListStagesForRun(context.Context, uuid.UUID) ([]*run.Stage, error) {
	return nil, errors.New("not used")
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
func (r *promptRunRepo) TransitionStage(context.Context, uuid.UUID, run.StageState, *run.StageCompletion) (*run.Stage, error) {
	return nil, errors.New("not used")
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
