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
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// promptRunRepo is a run.Repository fake that supports GetStage +
// GetRun. Other methods panic to make accidental calls loud.
type promptRunRepo struct {
	stage     *run.Stage
	stageErr  error
	runRow    *run.Run
	runErr    error
	getStages map[uuid.UUID]*run.Stage
	getRuns   map[uuid.UUID]*run.Run
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
func (r *promptRunRepo) ListRuns(context.Context, run.ListRunsFilter) ([]*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *promptRunRepo) TransitionRun(context.Context, uuid.UUID, run.State) (*run.Run, error) {
	return nil, errors.New("not used")
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
	// The prompt should reflect the fetched issue title/body.
	for _, want := range []string{"Add foo", "Body text", "Triggering issue: #42", "kuhlman-labs/example"} {
		if !contains(resp.Prompt, want) {
			t.Errorf("prompt missing %q\n---\n%s", want, resp.Prompt)
		}
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
