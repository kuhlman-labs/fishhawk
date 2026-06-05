package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
)

// validPRBytes returns a complete pullRequestBody payload that
// satisfies the handler's structural validation.
func validPRBytes(t *testing.T) []byte {
	t.Helper()
	body, err := json.Marshal(pullRequestBody{
		PRNumber:          42,
		PRURL:             "https://github.com/kuhlman-labs/fishhawk/pull/42",
		Branch:            "fishhawk/run-aaa/stage-bbb",
		HeadSHA:           "1111111111111111111111111111111111111111",
		BaseSHA:           "2222222222222222222222222222222222222222",
		Title:             "Add a make target.",
		Body:              "Opened by Fishhawk on behalf of @octocat.",
		FilesChangedCount: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func newPRServer(t *testing.T, runID, stageID uuid.UUID) (*Server, *signingFake, *fakeArtifactRepo, *auditFake, *promptRunRepo) {
	t.Helper()
	sf := newSigningFake()
	ar := newFakeArtifactRepo()
	au := newAuditFake()
	rr := newPromptRunRepo()
	rr.getStages[stageID] = &run.Stage{ID: stageID, RunID: runID}
	s := New(Config{
		Addr:         "127.0.0.1:0",
		SigningRepo:  sf,
		ArtifactRepo: ar,
		AuditRepo:    au,
		RunRepo:      rr,
	})
	return s, sf, ar, au, rr
}

func shipPRRequest(t *testing.T, s *Server, runID, stageID uuid.UUID, priv ed25519.PrivateKey, body []byte, sigOverride string) *httptest.ResponseRecorder {
	t.Helper()
	url := fmt.Sprintf("/v0/runs/%s/pull-request?stage_id=%s", runID, stageID)
	req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if sigOverride != "" {
		req.Header.Set("X-Fishhawk-Signature", sigOverride)
	} else if priv != nil {
		sig := ed25519.Sign(priv, signing.ComputeMessage(body))
		req.Header.Set("X-Fishhawk-Signature", hex.EncodeToString(sig))
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

func TestShipPullRequest_HappyPath(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, ar, au, _ := newPRServer(t, runID, stageID)
	priv, _ := sf.issue(t, runID)
	body := validPRBytes(t)

	w := shipPRRequest(t, s, runID, stageID, priv, body, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}

	var resp pullRequestResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.PRNumber != 42 || resp.PRURL == "" {
		t.Errorf("response not populated: %+v", resp)
	}
	if resp.Idempotent {
		t.Error("first upload should not be marked idempotent")
	}
	if len(ar.all) != 1 {
		t.Errorf("artifacts = %d, want 1", len(ar.all))
	}
	if len(au.appended) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(au.appended))
	}
	if got := au.appended[0].Category; got != "pull_request_opened" {
		t.Errorf("audit category = %q", got)
	}
}

func TestShipPullRequest_BackfillsPullRequestURLOnRun(t *testing.T) {
	// Threaded-runs view (#216) groups runs by pull_request_url. The
	// PR-upload handler is the one path that knows the URL, so it
	// must call run.Repo.SetRunPullRequestURL after creating the
	// artifact.
	runID, stageID := uuid.New(), uuid.New()
	s, sf, _, _, rr := newPRServer(t, runID, stageID)
	rr.getRuns[runID] = &run.Run{ID: runID, Repo: "x/y", State: run.StateRunning}
	priv, _ := sf.issue(t, runID)
	body := validPRBytes(t)

	w := shipPRRequest(t, s, runID, stageID, priv, body, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d", w.Code)
	}
	if len(rr.setPRURLCalls) != 1 {
		t.Fatalf("expected 1 SetRunPullRequestURL call; got %d", len(rr.setPRURLCalls))
	}
	c := rr.setPRURLCalls[0]
	if c.RunID != runID {
		t.Errorf("RunID = %s want %s", c.RunID, runID)
	}
	if c.URL == "" || !strings.HasPrefix(c.URL, "http") {
		t.Errorf("URL = %q (should be the PR URL from the body)", c.URL)
	}
}

func TestShipPullRequest_Idempotent_SecondUpload(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, ar, au, _ := newPRServer(t, runID, stageID)
	priv, _ := sf.issue(t, runID)
	body := validPRBytes(t)

	if w := shipPRRequest(t, s, runID, stageID, priv, body, ""); w.Code != http.StatusCreated {
		t.Fatalf("first upload status = %d", w.Code)
	}
	w2 := shipPRRequest(t, s, runID, stageID, priv, body, "")
	if w2.Code != http.StatusOK {
		t.Fatalf("second upload status = %d, want 200", w2.Code)
	}
	var resp pullRequestResponse
	_ = json.NewDecoder(w2.Body).Decode(&resp)
	if !resp.Idempotent {
		t.Error("second upload should be marked idempotent=true")
	}
	if len(ar.all) != 1 {
		t.Errorf("artifacts = %d, want 1 (no duplicate row)", len(ar.all))
	}
	if len(au.appended) != 1 {
		t.Errorf("audit entries = %d, want 1 (no second pull_request_opened)", len(au.appended))
	}
}

func TestShipPullRequest_InvalidPayload_400(t *testing.T) {
	cases := map[string][]byte{
		"missing pr_number": []byte(`{"pr_url":"https://x/p/1","branch":"b","head_sha":"abc","base_sha":"def","title":"t"}`),
		"missing pr_url":    []byte(`{"pr_number":1,"branch":"b","head_sha":"abc","base_sha":"def","title":"t"}`),
		"unknown field":     []byte(`{"pr_number":1,"pr_url":"https://x/p/1","branch":"b","head_sha":"abc","base_sha":"def","title":"t","extra":true}`),
		"non-http url":      []byte(`{"pr_number":1,"pr_url":"ssh://x","branch":"b","head_sha":"abc","base_sha":"def","title":"t"}`),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			runID, stageID := uuid.New(), uuid.New()
			s, sf, ar, _, _ := newPRServer(t, runID, stageID)
			priv, _ := sf.issue(t, runID)
			w := shipPRRequest(t, s, runID, stageID, priv, body, "")
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400:\n%s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "pull_request_invalid") {
				t.Errorf("body missing pull_request_invalid:\n%s", w.Body.String())
			}
			if len(ar.all) != 0 {
				t.Errorf("artifacts = %d, want 0", len(ar.all))
			}
		})
	}
}

func TestShipPullRequest_NoAuth_401(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, _, _, _, _ := newPRServer(t, runID, stageID)
	body := validPRBytes(t)
	url := fmt.Sprintf("/v0/runs/%s/pull-request?stage_id=%s", runID, stageID)
	req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
	if !strings.Contains(w.Body.String(), "signature_or_bearer_required") {
		t.Errorf("body missing signature_or_bearer_required:\n%s", w.Body.String())
	}
}

func TestShipPullRequest_StageMismatch_400(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, _, _, rr := newPRServer(t, runID, stageID)
	rr.getStages[stageID] = &run.Stage{ID: stageID, RunID: uuid.New()}
	priv, _ := sf.issue(t, runID)
	w := shipPRRequest(t, s, runID, stageID, priv, validPRBytes(t), "")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (stage doesn't belong to run)", w.Code)
	}
}

func TestShipPullRequest_BodyTooLarge_413(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, _, _, _ := newPRServer(t, runID, stageID)
	priv, _ := sf.issue(t, runID)
	body := bytes.Repeat([]byte("x"), maxPullRequestBundleBytes+1)
	w := shipPRRequest(t, s, runID, stageID, priv, body, "")
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", w.Code)
	}
}

func TestShipPullRequest_Unconfigured_503(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	url := fmt.Sprintf("/v0/runs/%s/pull-request?stage_id=%s", uuid.New(), uuid.New())
	req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader([]byte(`{}`)))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestShipPullRequest_BearerHappyPath_201(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, _, ar, au, _ := newPRServer(t, runID, stageID)
	body := validPRBytes(t)

	url := fmt.Sprintf("/v0/runs/%s/pull-request?stage_id=%s", runID, stageID)
	req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("run_id", runID.String())
	req.SetPathValue("stage_id", stageID.String())
	ctx := context.WithValue(req.Context(), ctxKeyIdentity, Identity{
		Subject: "operator:test",
		TokenID: "tok-abc",
		Scopes:  []string{"write:runs"},
	})
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	s.handleShipPullRequest(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	if len(ar.all) != 1 {
		t.Errorf("artifacts = %d, want 1", len(ar.all))
	}
	if len(au.appended) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(au.appended))
	}
	if got := au.appended[0].Category; got != "pull_request_opened" {
		t.Errorf("audit category = %q", got)
	}
	payload := string(au.appended[0].Payload)
	if !strings.Contains(payload, `"auth_method":"bearer"`) {
		t.Errorf("audit payload missing auth_method=bearer: %s", payload)
	}
}

// newPRServerWithOrch wires the PR handler against the orchestratorRepo
// (which validates stage transitions) plus a real orchestrator, so the
// push-and-open-pr terminal-drive + failure paths (#742) can assert the
// stage actually transitions and the run advances.
func newPRServerWithOrch(t *testing.T) (*Server, *signingFake, *fakeArtifactRepo, *auditFake, *orchestratorRepo) {
	t.Helper()
	sf := newSigningFake()
	ar := newFakeArtifactRepo()
	au := newAuditFake()
	rr := newOrchestratorRepo()
	s := New(Config{
		Addr:         "127.0.0.1:0",
		SigningRepo:  sf,
		ArtifactRepo: ar,
		AuditRepo:    au,
		RunRepo:      rr,
		Orchestrator: &orchestrator.Orchestrator{Runs: rr},
	})
	return s, sf, ar, au, rr
}

// TestShipPullRequest_SuccessDrivesImplementTerminal is the #742 success
// half: when the implement stage was left in `running` by the trace gate,
// a success PR body is the authoritative driver of the terminal transition.
// The stage must advance to awaiting_approval WITH a non-null PR URL.
func TestShipPullRequest_SuccessDrivesImplementTerminal(t *testing.T) {
	s, sf, ar, _, rr := newPRServerWithOrch(t)
	runRow := rr.seedRun()
	implStage := rr.seedStage(runRow.ID, 0, run.StageStateRunning)
	implStage.Type = run.StageTypeImplement
	implStage.RequiresApproval = true

	priv, _ := sf.issue(t, runRow.ID)
	w := shipPRRequest(t, s, runRow.ID, implStage.ID, priv, validPRBytes(t), "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}

	got, err := rr.GetStage(t.Context(), implStage.ID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	if got.State != run.StageStateAwaitingApproval {
		t.Errorf("stage.State = %q, want awaiting_approval (success body must drive the gated terminal transition)", got.State)
	}
	gotRun, err := rr.GetRun(t.Context(), runRow.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if gotRun.PullRequestURL == nil || *gotRun.PullRequestURL == "" {
		t.Error("run.PullRequestURL not stamped on success")
	}
	if len(ar.all) != 1 {
		t.Errorf("artifacts = %d, want 1", len(ar.all))
	}
}

// TestShipPullRequest_FailureOutcome_FailsStageC is the #742 failure half:
// a runner-reported commit/push/PR-open failure body must fail the gated
// implement stage (category C, retryable) and advance the run — it must
// NEVER reach review:awaiting_approval with a null PR (the b6811dc9 zombie).
func TestShipPullRequest_FailureOutcome_FailsStageC(t *testing.T) {
	s, sf, ar, au, rr := newPRServerWithOrch(t)
	runRow := rr.seedRun()
	implStage := rr.seedStage(runRow.ID, 0, run.StageStateRunning)
	implStage.Type = run.StageTypeImplement
	implStage.RequiresApproval = true

	priv, _ := sf.issue(t, runRow.ID)
	body, err := json.Marshal(map[string]any{
		"outcome":  "failed",
		"category": "C",
		"reason":   "open PR: git push failed: network unreachable",
	})
	if err != nil {
		t.Fatal(err)
	}

	w := shipPRRequest(t, s, runRow.ID, implStage.ID, priv, body, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	got, err := rr.GetStage(t.Context(), implStage.ID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	if got.State != run.StageStateFailed {
		t.Errorf("stage.State = %q, want failed", got.State)
	}
	if got.State == run.StageStateAwaitingApproval {
		t.Error("stage reached awaiting_approval on a PR-open failure (the zombie shape)")
	}
	if got.FailureCategory == nil || *got.FailureCategory != run.FailureC {
		t.Errorf("FailureCategory = %v, want C (retryable)", got.FailureCategory)
	}
	gotRun, err := rr.GetRun(t.Context(), runRow.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if gotRun.PullRequestURL != nil {
		t.Errorf("run.PullRequestURL = %q, want nil on failure", *gotRun.PullRequestURL)
	}
	if len(ar.all) != 0 {
		t.Errorf("artifacts = %d, want 0 (no PR artifact on failure)", len(ar.all))
	}
	au.mu.Lock()
	defer au.mu.Unlock()
	var found bool
	for _, e := range au.appended {
		if e.Category == "pull_request_failed" {
			found = true
		}
	}
	if !found {
		t.Error("no pull_request_failed audit entry recorded")
	}
}

// TestShipPullRequest_ChildPushOutcome_DrivesChildTerminal is the #771
// success half: a decomposed child's {outcome:"pushed", branch, head_sha,
// base_sha} report must drive the gated child implement stage's terminal
// transition (running → awaiting_approval) and record a child_pushed audit
// entry — WITHOUT creating a PR artifact or backfilling pull_request_url
// (the parent run opens the consolidated PR).
func TestShipPullRequest_ChildPushOutcome_DrivesChildTerminal(t *testing.T) {
	s, sf, ar, au, rr := newPRServerWithOrch(t)
	runRow := rr.seedRun()
	implStage := rr.seedStage(runRow.ID, 0, run.StageStateRunning)
	implStage.Type = run.StageTypeImplement
	implStage.RequiresApproval = true

	priv, _ := sf.issue(t, runRow.ID)
	body, err := json.Marshal(map[string]any{
		"outcome":             "pushed",
		"branch":              "fishhawk/run-aaaaaaaa",
		"head_sha":            "head-abc",
		"base_sha":            "base-def",
		"files_changed_count": 3,
	})
	if err != nil {
		t.Fatal(err)
	}

	w := shipPRRequest(t, s, runRow.ID, implStage.ID, priv, body, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	got, err := rr.GetStage(t.Context(), implStage.ID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	if got.State != run.StageStateAwaitingApproval {
		t.Errorf("stage.State = %q, want awaiting_approval (pushed body must drive the gated terminal transition)", got.State)
	}
	if len(ar.all) != 0 {
		t.Errorf("artifacts = %d, want 0 (no PR artifact for a child push)", len(ar.all))
	}
	gotRun, err := rr.GetRun(t.Context(), runRow.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if gotRun.PullRequestURL != nil {
		t.Errorf("run.PullRequestURL = %q, want nil (parent opens the consolidated PR)", *gotRun.PullRequestURL)
	}
	au.mu.Lock()
	defer au.mu.Unlock()
	var found bool
	for _, e := range au.appended {
		if e.Category == "child_pushed" {
			found = true
		}
	}
	if !found {
		t.Error("no child_pushed audit entry recorded")
	}
}

// TestShipPullRequest_ChildPushOutcome_RejectsMissingCoords pins the #771
// validation: the pushed variant requires branch + head_sha + base_sha (no
// PR was opened, so those coordinates are the only record of what landed).
func TestShipPullRequest_ChildPushOutcome_RejectsMissingCoords(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, _, _, _ := newPRServer(t, runID, stageID)
	priv, _ := sf.issue(t, runID)
	for _, body := range []string{
		`{"outcome":"pushed","head_sha":"h","base_sha":"b"}`, // missing branch
		`{"outcome":"pushed","branch":"br","base_sha":"b"}`,  // missing head_sha
		`{"outcome":"pushed","branch":"br","head_sha":"h"}`,  // missing base_sha
	} {
		w := shipPRRequest(t, s, runID, stageID, priv, []byte(body), "")
		if w.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400:\n%s", body, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "pull_request_invalid") {
			t.Errorf("body %s: missing pull_request_invalid:\n%s", body, w.Body.String())
		}
	}
}

// TestShipPullRequest_FailureOutcome_InvalidCategory_400 pins the failure-
// variant validation: an unknown category is a malformed body (#742).
func TestShipPullRequest_FailureOutcome_InvalidCategory_400(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, _, _, _ := newPRServer(t, runID, stageID)
	priv, _ := sf.issue(t, runID)
	body := []byte(`{"outcome":"failed","category":"Z","reason":"x"}`)
	w := shipPRRequest(t, s, runID, stageID, priv, body, "")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "pull_request_invalid") {
		t.Errorf("body missing pull_request_invalid:\n%s", w.Body.String())
	}
}

func TestShipPullRequest_BearerInsufficientScope_401(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, _, _, _, _ := newPRServer(t, runID, stageID)
	body := validPRBytes(t)

	url := fmt.Sprintf("/v0/runs/%s/pull-request?stage_id=%s", runID, stageID)
	req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("run_id", runID.String())
	req.SetPathValue("stage_id", stageID.String())
	ctx := context.WithValue(req.Context(), ctxKeyIdentity, Identity{
		Subject: "operator:test",
		TokenID: "tok-abc",
		Scopes:  []string{"read:runs"},
	})
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	s.handleShipPullRequest(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
	if !strings.Contains(w.Body.String(), "signature_or_bearer_required") {
		t.Errorf("body missing signature_or_bearer_required:\n%s", w.Body.String())
	}
}
