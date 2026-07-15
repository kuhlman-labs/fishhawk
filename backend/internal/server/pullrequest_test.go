package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
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

// TestShipPullRequest_RotationToleratesInFlightUpload is the
// cross-boundary seam test for incident bdf94763 (#1872): it drives
// the REAL signing repo through the real PR-upload handler. A run
// issues two keys (a sibling stage rotated a fresh key in while the
// implement runner's ship phase was still open), the pull-request
// artifact body is signed with the FIRST key, and the upload must
// still be accepted (not 401 signature_invalid) with pull_request_url
// recorded. Fails on the pre-fix latest-key-only Verify.
func TestShipPullRequest_RotationToleratesInFlightUpload(t *testing.T) {
	pool := pgtest.NewPool(t)

	// Real run row so the signing_keys FK is satisfied.
	runRepoPG := run.NewPostgresRepository(pool)
	realRun, err := runRepoPG.CreateRun(context.Background(), run.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef",
		TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	runID := realRun.ID
	stageID := uuid.New()

	signingRepo := signing.NewPostgresRepository(pool)
	// First key signs the in-flight implement upload...
	first, err := signingRepo.Issue(context.Background(), runID, signing.DefaultTTL)
	if err != nil {
		t.Fatalf("issue first key: %v", err)
	}
	// ...then a sibling stage rotates a fresh key in mid-flight.
	if _, err := signingRepo.Issue(context.Background(), runID, signing.DefaultTTL); err != nil {
		t.Fatalf("issue second key: %v", err)
	}

	ar := newFakeArtifactRepo()
	au := newAuditFake()
	rr := newPromptRunRepo()
	rr.getStages[stageID] = &run.Stage{ID: stageID, RunID: runID}
	rr.getRuns[runID] = &run.Run{ID: runID, Repo: "kuhlman-labs/fishhawk", State: run.StateRunning}
	s := New(Config{
		Addr:         "127.0.0.1:0",
		SigningRepo:  signingRepo,
		ArtifactRepo: ar,
		AuditRepo:    au,
		RunRepo:      rr,
	})

	body := validPRBytes(t)
	// Sign with the FIRST (now-older) key.
	w := shipPRRequest(t, s, runID, stageID, first.PrivateKey, body, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (older-key signature must verify after rotation):\n%s", w.Code, w.Body.String())
	}
	if len(rr.setPRURLCalls) != 1 {
		t.Fatalf("expected pull_request_url to be recorded once; got %d calls", len(rr.setPRURLCalls))
	}
	if url := rr.setPRURLCalls[0].URL; !strings.HasPrefix(url, "http") {
		t.Errorf("recorded pull_request_url = %q, want an http URL", url)
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

// TestShipPullRequest_RetryAfterAuditAppendFailure_Heals is the #1396 done-means
// cross-layer integration test for the PR handler: a partial write (artifact
// created, pull_request_opened append fails → 500) followed by an identical
// retry must end with BOTH the artifact and its governance audit entry present.
func TestShipPullRequest_RetryAfterAuditAppendFailure_Heals(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, ar, au, _ := newPRServer(t, runID, stageID)
	priv, _ := sf.issue(t, runID)
	body := validPRBytes(t)

	// Partial write: Create succeeds, AppendChained fails → 500.
	au.appendErr = errors.New("boom")
	w1 := shipPRRequest(t, s, runID, stageID, priv, body, "")
	if w1.Code != http.StatusInternalServerError {
		t.Fatalf("first ship status = %d, want 500:\n%s", w1.Code, w1.Body.String())
	}
	if len(ar.all) != 1 {
		t.Fatalf("artifacts after partial write = %d, want 1 (artifact persisted)", len(ar.all))
	}
	if n := countByCategory(au, "pull_request_opened"); n != 0 {
		t.Fatalf("pull_request_opened entries after partial write = %d, want 0 (append failed)", n)
	}

	// Identical retry heals the missing governance entry on the idempotent path.
	au.appendErr = nil
	w2 := shipPRRequest(t, s, runID, stageID, priv, body, "")
	if w2.Code != http.StatusOK {
		t.Fatalf("retry status = %d, want 200 (idempotent heal):\n%s", w2.Code, w2.Body.String())
	}
	var resp pullRequestResponse
	if err := json.NewDecoder(w2.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Idempotent {
		t.Error("retry should be marked idempotent=true")
	}
	if len(ar.all) != 1 {
		t.Errorf("artifacts after retry = %d, want 1 (no duplicate)", len(ar.all))
	}
	if n := countByCategory(au, "pull_request_opened"); n != 1 {
		t.Errorf("pull_request_opened entries after retry = %d, want 1 (healed)", n)
	}
}

// TestShipPullRequest_Idempotent_AuditPresent_NoDuplicate pins that a clean
// first ship followed by an identical second ship leaves exactly one
// pull_request_opened entry: the self-heal must not append a duplicate on the
// already-healthy idempotent path.
func TestShipPullRequest_Idempotent_AuditPresent_NoDuplicate(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, ar, au, _ := newPRServer(t, runID, stageID)
	priv, _ := sf.issue(t, runID)
	body := validPRBytes(t)

	if w := shipPRRequest(t, s, runID, stageID, priv, body, ""); w.Code != http.StatusCreated {
		t.Fatalf("first ship status = %d, want 201", w.Code)
	}
	w2 := shipPRRequest(t, s, runID, stageID, priv, body, "")
	if w2.Code != http.StatusOK {
		t.Fatalf("second ship status = %d, want 200", w2.Code)
	}
	if len(ar.all) != 1 {
		t.Errorf("artifacts = %d, want 1 (no duplicate)", len(ar.all))
	}
	if n := countByCategory(au, "pull_request_opened"); n != 1 {
		t.Errorf("pull_request_opened entries = %d, want 1 (no duplicate heal)", n)
	}
}

// TestShipPullRequest_IdempotentHeal_ListError_500 pins the fail-closed read
// branch: an idempotent retry while ListForRunByCategory errors returns 500
// (governance integrity, not a gapped 200).
func TestShipPullRequest_IdempotentHeal_ListError_500(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, ar, au, _ := newPRServer(t, runID, stageID)
	priv, _ := sf.issue(t, runID)
	body := validPRBytes(t)

	if w := shipPRRequest(t, s, runID, stageID, priv, body, ""); w.Code != http.StatusCreated {
		t.Fatalf("first ship status = %d, want 201", w.Code)
	}
	if len(ar.all) != 1 {
		t.Fatalf("artifacts = %d, want 1", len(ar.all))
	}
	au.listByCategoryErr = errors.New("audit read down")
	w2 := shipPRRequest(t, s, runID, stageID, priv, body, "")
	if w2.Code != http.StatusInternalServerError {
		t.Fatalf("retry status = %d, want 500 (fail closed on read error):\n%s", w2.Code, w2.Body.String())
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
	// Binding approval condition (2) of #1891: the persisted child_pushed audit
	// row must carry the reported files_changed_count — asserting the value
	// through the persistence path (succeedChildPushStage), not only the
	// runner's outbound value. The runner-side fix computes this count against
	// cap.BaseSHA; here we prove the handler faithfully persists whatever the
	// report carried (files_changed_count: 3 above).
	au.mu.Lock()
	defer au.mu.Unlock()
	var childPushed *audit.ChainAppendParams
	for i := range au.appended {
		if au.appended[i].Category == "child_pushed" {
			childPushed = &au.appended[i]
		}
	}
	if childPushed == nil {
		t.Fatal("no child_pushed audit entry recorded")
	}
	var payload struct {
		FilesChangedCount int `json:"files_changed_count"`
	}
	if err := json.Unmarshal(childPushed.Payload, &payload); err != nil {
		t.Fatalf("unmarshal child_pushed payload: %v", err)
	}
	if payload.FilesChangedCount != 3 {
		t.Errorf("child_pushed audit files_changed_count = %d, want 3 (the reported count must be persisted)", payload.FilesChangedCount)
	}
}

// TestShipPullRequest_ChildPushOutcome_IsIdempotent pins the #776 guard: a
// runner retry after a 5xx — or a duplicate delivery — of an identical pushed
// report must NOT append a second child_pushed audit entry (and, since the
// status-comment notify is downstream of the same guard, fires no redundant
// status update). The guard keys on (stage_id, head_sha), so a genuine push of
// NEW work carrying a different head_sha to the same shared branch is still
// recorded — asserted here as cheap insurance against an over-broad guard.
func TestShipPullRequest_ChildPushOutcome_IsIdempotent(t *testing.T) {
	s, sf, _, au, rr := newPRServerWithOrch(t)
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

	// Two identical deliveries — the second is the retry/duplicate the guard
	// must suppress. Both must return 200.
	for i := 0; i < 2; i++ {
		w := shipPRRequest(t, s, runRow.ID, implStage.ID, priv, body, "")
		if w.Code != http.StatusOK {
			t.Fatalf("delivery %d: status = %d, want 200:\n%s", i, w.Code, w.Body.String())
		}
	}

	countChildPushed := func() int {
		au.mu.Lock()
		defer au.mu.Unlock()
		var n int
		for _, e := range au.appended {
			if e.Category == "child_pushed" {
				n++
			}
		}
		return n
	}
	if got := countChildPushed(); got != 1 {
		t.Fatalf("child_pushed audit entries after duplicate = %d, want 1 (guard must suppress the retry)", got)
	}

	// A genuinely NEW push to the same shared branch carries a different
	// head_sha and must still be recorded — the equality keying guarantees
	// this by construction; assert it as insurance against an over-broad guard.
	body2, err := json.Marshal(map[string]any{
		"outcome":             "pushed",
		"branch":              "fishhawk/run-aaaaaaaa",
		"head_sha":            "head-xyz",
		"base_sha":            "base-def",
		"files_changed_count": 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if w := shipPRRequest(t, s, runRow.ID, implStage.ID, priv, body2, ""); w.Code != http.StatusOK {
		t.Fatalf("new-head_sha delivery: status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if got := countChildPushed(); got != 2 {
		t.Errorf("child_pushed audit entries after new head_sha = %d, want 2 (a different head_sha must not be suppressed)", got)
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

// TestShipPullRequest_FixupPushOutcome_DrivesTerminal is the #794 success half:
// a fix-up re-dispatch's {outcome:"fixup_pushed", branch, head_sha, base_sha}
// report must drive the gated fix-up implement stage's terminal transition
// (running → awaiting_approval) and record a fixup_pushed audit entry — WITHOUT
// creating a PR artifact or backfilling pull_request_url (the PR already exists
// and tracks the branch).
func TestShipPullRequest_FixupPushOutcome_DrivesTerminal(t *testing.T) {
	s, sf, ar, au, rr := newPRServerWithOrch(t)
	runRow := rr.seedRun()
	implStage := rr.seedStage(runRow.ID, 0, run.StageStateRunning)
	implStage.Type = run.StageTypeImplement
	implStage.RequiresApproval = true

	priv, _ := sf.issue(t, runRow.ID)
	body, err := json.Marshal(map[string]any{
		"outcome":             "fixup_pushed",
		"branch":              "fishhawk/run-aaaaaaaa/stage-bbbbbbbb",
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
		t.Errorf("stage.State = %q, want awaiting_approval (fixup_pushed body must drive the gated terminal transition)", got.State)
	}
	if len(ar.all) != 0 {
		t.Errorf("artifacts = %d, want 0 (no PR artifact for a fix-up push)", len(ar.all))
	}
	gotRun, err := rr.GetRun(t.Context(), runRow.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if gotRun.PullRequestURL != nil {
		t.Errorf("run.PullRequestURL = %q, want nil (fix-up updates the existing PR)", *gotRun.PullRequestURL)
	}
	au.mu.Lock()
	defer au.mu.Unlock()
	var found bool
	for _, e := range au.appended {
		if e.Category == "fixup_pushed" {
			found = true
		}
	}
	if !found {
		t.Error("no fixup_pushed audit entry recorded")
	}
}

// fixupPushedPayload pulls the apply_path out of the single fixup_pushed audit
// entry's payload; ok reports whether the key was present at all.
func fixupPushedApplyPath(t *testing.T, au *auditFake) (value string, present bool) {
	t.Helper()
	au.mu.Lock()
	defer au.mu.Unlock()
	for _, e := range au.appended {
		if e.Category != "fixup_pushed" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			t.Fatalf("unmarshal fixup_pushed payload: %v", err)
		}
		v, ok := payload["apply_path"]
		if !ok {
			return "", false
		}
		s, _ := v.(string)
		return s, true
	}
	t.Fatal("no fixup_pushed audit entry recorded")
	return "", false
}

// TestShipPullRequest_FixupPushOutcome_PersistsApplyPath pins the #1165/#1213
// apply-provenance persist: a fixup_pushed report carrying a recognized
// apply_path records it onto the fixup_pushed audit entry, so an operator can see
// whether the fix-up collapsed to a deterministic git-apply or fell back to the
// agent.
func TestShipPullRequest_FixupPushOutcome_PersistsApplyPath(t *testing.T) {
	s, sf, _, au, rr := newPRServerWithOrch(t)
	runRow := rr.seedRun()
	implStage := rr.seedStage(runRow.ID, 0, run.StageStateRunning)
	implStage.Type = run.StageTypeImplement
	implStage.RequiresApproval = true

	priv, _ := sf.issue(t, runRow.ID)
	body, err := json.Marshal(map[string]any{
		"outcome":             "fixup_pushed",
		"branch":              "fishhawk/run-aaaaaaaa/stage-bbbbbbbb",
		"head_sha":            "head-abc",
		"base_sha":            "base-def",
		"files_changed_count": 2,
		"apply_path":          "applied",
	})
	if err != nil {
		t.Fatal(err)
	}

	w := shipPRRequest(t, s, runRow.ID, implStage.ID, priv, body, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	value, present := fixupPushedApplyPath(t, au)
	if !present {
		t.Fatal("fixup_pushed audit entry missing apply_path despite a reported value")
	}
	if value != "applied" {
		t.Errorf("apply_path = %q, want applied", value)
	}
}

// TestShipPullRequest_FixupPushOutcome_OmitsAbsentApplyPath asserts the inverse:
// a fixup_pushed report with NO apply_path (an older runner, or the
// fixup_no_changes shape promoted) leaves the key off the audit entry rather than
// persisting an empty discriminator.
func TestShipPullRequest_FixupPushOutcome_OmitsAbsentApplyPath(t *testing.T) {
	s, sf, _, au, rr := newPRServerWithOrch(t)
	runRow := rr.seedRun()
	implStage := rr.seedStage(runRow.ID, 0, run.StageStateRunning)
	implStage.Type = run.StageTypeImplement
	implStage.RequiresApproval = true

	priv, _ := sf.issue(t, runRow.ID)
	body, err := json.Marshal(map[string]any{
		"outcome":             "fixup_pushed",
		"branch":              "fishhawk/run-aaaaaaaa/stage-bbbbbbbb",
		"head_sha":            "head-abc",
		"base_sha":            "base-def",
		"files_changed_count": 2,
	})
	if err != nil {
		t.Fatal(err)
	}

	w := shipPRRequest(t, s, runRow.ID, implStage.ID, priv, body, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	if _, present := fixupPushedApplyPath(t, au); present {
		t.Error("fixup_pushed audit entry must omit apply_path when the report carries none")
	}
}

// TestShipPullRequest_FixupPushOutcome_IsIdempotent pins the #794 guard
// (mirroring #776 for child push): a runner retry or duplicate delivery of an
// identical fixup_pushed report must NOT append a second fixup_pushed audit
// entry. The guard keys on (stage_id, head_sha), so a genuine push of NEW work
// carrying a different head_sha is still recorded.
func TestShipPullRequest_FixupPushOutcome_IsIdempotent(t *testing.T) {
	s, sf, _, au, rr := newPRServerWithOrch(t)
	runRow := rr.seedRun()
	implStage := rr.seedStage(runRow.ID, 0, run.StageStateRunning)
	implStage.Type = run.StageTypeImplement
	implStage.RequiresApproval = true

	priv, _ := sf.issue(t, runRow.ID)
	body, err := json.Marshal(map[string]any{
		"outcome":             "fixup_pushed",
		"branch":              "fishhawk/run-aaaaaaaa/stage-bbbbbbbb",
		"head_sha":            "head-abc",
		"base_sha":            "base-def",
		"files_changed_count": 3,
	})
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		w := shipPRRequest(t, s, runRow.ID, implStage.ID, priv, body, "")
		if w.Code != http.StatusOK {
			t.Fatalf("delivery %d: status = %d, want 200:\n%s", i, w.Code, w.Body.String())
		}
	}

	countFixupPushed := func() int {
		au.mu.Lock()
		defer au.mu.Unlock()
		var n int
		for _, e := range au.appended {
			if e.Category == "fixup_pushed" {
				n++
			}
		}
		return n
	}
	if got := countFixupPushed(); got != 1 {
		t.Fatalf("fixup_pushed audit entries after duplicate = %d, want 1 (guard must suppress the retry)", got)
	}

	body2, err := json.Marshal(map[string]any{
		"outcome":             "fixup_pushed",
		"branch":              "fishhawk/run-aaaaaaaa/stage-bbbbbbbb",
		"head_sha":            "head-xyz",
		"base_sha":            "base-def",
		"files_changed_count": 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if w := shipPRRequest(t, s, runRow.ID, implStage.ID, priv, body2, ""); w.Code != http.StatusOK {
		t.Fatalf("new-head_sha delivery: status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if got := countFixupPushed(); got != 2 {
		t.Errorf("fixup_pushed audit entries after new head_sha = %d, want 2 (a different head_sha must not be suppressed)", got)
	}
}

// --- Option A: fix-up-driven acceptance invalidation (#1682) ---
//
// reopenAcceptanceOnFixupPush is exercised directly (white-box) rather than
// through the fix-up push HTTP path: the acceptance re-open uses the
// succeeded → pending fix-up edge (run.ValidStageFixupTransition), and the
// orchestratorRepo test fake newPRServerWithOrch wires validates ONLY the
// normal transition table — so the reopen must ride the approvalRunRepo (via
// reopenRetryServer), which mirrors the production repo's fix-up edge.

// acceptanceReopenedCount returns how many acceptance_reopened audit entries the
// reopen fake recorded.
func acceptanceReopenedCount(au *reopenAuditFake) int {
	var n int
	for _, e := range au.appended {
		if e.Category == CategoryAcceptanceReopened {
			n++
		}
	}
	return n
}

// seedAcceptanceVerdict wires a recorded acceptance_outcome_recorded verdict for
// stageID into the reopen fake's category history.
func seedAcceptanceVerdict(au *reopenAuditFake, stageID uuid.UUID) {
	if au.byCategory == nil {
		au.byCategory = map[string][]*audit.Entry{}
	}
	au.byCategory[CategoryAcceptanceOutcomeRecorded] = append(
		au.byCategory[CategoryAcceptanceOutcomeRecorded], acceptanceOutcomeEntryFor(stageID))
}

// A fix-up push AFTER a settled, verdict-ful acceptance stage re-opens that
// stage (succeeded → pending) and appends an acceptance_reopened invalidation.
func TestReopenAcceptanceOnFixupPush_ReopensSettledVerdictfulStage(t *testing.T) {
	s, repo, au := reopenRetryServer(t)
	runID := uuid.New()
	repo.seedRun(&run.Run{ID: runID, State: run.StateRunning})
	acc := repo.seedStageOnRun(runID, run.StageTypeAcceptance, run.StageStateSucceeded)
	seedAcceptanceVerdict(au, acc.ID)

	s.reopenAcceptanceOnFixupPush(context.Background(), runID, "newhead")

	got, _ := repo.GetStage(context.Background(), acc.ID)
	if got.State != run.StageStatePending {
		t.Errorf("acceptance stage state = %q, want pending after fix-up invalidation", got.State)
	}
	if n := acceptanceReopenedCount(au); n != 1 {
		t.Errorf("acceptance_reopened entries = %d, want 1", n)
	}
}

// A fix-up push with the acceptance stage NOT yet succeeded (a PRE-acceptance
// fix-up) must NOT re-open anything.
func TestReopenAcceptanceOnFixupPush_PreAcceptance_LeavesStageAlone(t *testing.T) {
	s, repo, au := reopenRetryServer(t)
	runID := uuid.New()
	repo.seedRun(&run.Run{ID: runID, State: run.StateRunning})
	acc := repo.seedStageOnRun(runID, run.StageTypeAcceptance, run.StageStatePending)
	seedAcceptanceVerdict(au, acc.ID)

	s.reopenAcceptanceOnFixupPush(context.Background(), runID, "newhead")

	got, _ := repo.GetStage(context.Background(), acc.ID)
	if got.State != run.StageStatePending {
		t.Errorf("acceptance stage state = %q, want unchanged pending", got.State)
	}
	if n := acceptanceReopenedCount(au); n != 0 {
		t.Errorf("acceptance_reopened entries = %d, want 0 (pre-acceptance fix-up must not reopen)", n)
	}
}

// A fix-up push on a run with NO acceptance stage is unaffected.
func TestReopenAcceptanceOnFixupPush_NoAcceptanceStage_Unaffected(t *testing.T) {
	s, repo, au := reopenRetryServer(t)
	runID := uuid.New()
	repo.seedRun(&run.Run{ID: runID, State: run.StateRunning})
	repo.seedStageOnRun(runID, run.StageTypeImplement, run.StageStateRunning)

	s.reopenAcceptanceOnFixupPush(context.Background(), runID, "newhead")

	if n := acceptanceReopenedCount(au); n != 0 {
		t.Errorf("acceptance_reopened entries = %d, want 0", n)
	}
}

// A succeeded acceptance stage with NO recorded verdict is the #1567
// operator-reopen hole, not a fix-up invalidation — leave it untouched.
func TestReopenAcceptanceOnFixupPush_SucceededNoVerdict_NotReopened(t *testing.T) {
	s, repo, au := reopenRetryServer(t)
	runID := uuid.New()
	repo.seedRun(&run.Run{ID: runID, State: run.StateRunning})
	acc := repo.seedStageOnRun(runID, run.StageTypeAcceptance, run.StageStateSucceeded)
	// No acceptance_outcome_recorded seeded.

	s.reopenAcceptanceOnFixupPush(context.Background(), runID, "newhead")

	got, _ := repo.GetStage(context.Background(), acc.ID)
	if got.State != run.StageStateSucceeded {
		t.Errorf("acceptance stage state = %q, want unchanged succeeded (no verdict → #1567 owns it)", got.State)
	}
	if n := acceptanceReopenedCount(au); n != 0 {
		t.Errorf("acceptance_reopened entries = %d, want 0", n)
	}
}

// A verdict read error fails closed — the reopen is skipped (never fires on
// unknown evidence state).
func TestReopenAcceptanceOnFixupPush_VerdictReadError_Skips(t *testing.T) {
	s, repo, au := reopenRetryServer(t)
	runID := uuid.New()
	repo.seedRun(&run.Run{ID: runID, State: run.StateRunning})
	acc := repo.seedStageOnRun(runID, run.StageTypeAcceptance, run.StageStateSucceeded)
	au.byCategoryErr = errors.New("audit read down")

	s.reopenAcceptanceOnFixupPush(context.Background(), runID, "newhead")

	got, _ := repo.GetStage(context.Background(), acc.ID)
	if got.State != run.StageStateSucceeded {
		t.Errorf("acceptance stage state = %q, want unchanged succeeded (fail closed on read error)", got.State)
	}
	if n := acceptanceReopenedCount(au); n != 0 {
		t.Errorf("acceptance_reopened entries = %d, want 0", n)
	}
}

// Idempotency: a second call after the stage is already pending is a no-op —
// the reopen cannot double-fire (the guard rejects a non-succeeded stage).
func TestReopenAcceptanceOnFixupPush_Idempotent(t *testing.T) {
	s, repo, au := reopenRetryServer(t)
	runID := uuid.New()
	repo.seedRun(&run.Run{ID: runID, State: run.StateRunning})
	acc := repo.seedStageOnRun(runID, run.StageTypeAcceptance, run.StageStateSucceeded)
	seedAcceptanceVerdict(au, acc.ID)

	s.reopenAcceptanceOnFixupPush(context.Background(), runID, "newhead")
	s.reopenAcceptanceOnFixupPush(context.Background(), runID, "newhead") // re-delivery

	if n := acceptanceReopenedCount(au); n != 1 {
		t.Errorf("acceptance_reopened entries = %d, want 1 (second call must not double-reopen)", n)
	}
}

// TestShipPullRequest_FixupPushOutcome_RejectsMissingCoords pins the #794
// validation: the fixup_pushed variant requires branch + head_sha + base_sha
// (no new PR was opened, so those coordinates are the only record of what
// landed onto the existing PR branch).
func TestShipPullRequest_FixupPushOutcome_RejectsMissingCoords(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, _, _, _ := newPRServer(t, runID, stageID)
	priv, _ := sf.issue(t, runID)
	for _, body := range []string{
		`{"outcome":"fixup_pushed","head_sha":"h","base_sha":"b"}`, // missing branch
		`{"outcome":"fixup_pushed","branch":"br","base_sha":"b"}`,  // missing head_sha
		`{"outcome":"fixup_pushed","branch":"br","head_sha":"h"}`,  // missing base_sha
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

// TestShipPullRequest_FixupNoChangesOutcome_DrivesTerminal is the #856 fix: a
// fix-up re-dispatch that produces NO changes reports
// {outcome:"fixup_no_changes", branch, base_sha} (no head_sha — nothing
// landed). That report must drive the gated fix-up implement stage's terminal
// transition (running → awaiting_approval, the re-park seam #856 strands) and
// record a fixup_no_changes audit entry — WITHOUT creating a PR artifact or
// backfilling pull_request_url (the PR already exists, its tip is unchanged).
// This crosses the wire-decode → handler → run state-machine → orchestrator
// layers in one exercise, the seam a per-layer unit test would miss (cf. #618).
func TestShipPullRequest_FixupNoChangesOutcome_DrivesTerminal(t *testing.T) {
	s, sf, ar, au, rr := newPRServerWithOrch(t)
	runRow := rr.seedRun()
	implStage := rr.seedStage(runRow.ID, 0, run.StageStateRunning)
	implStage.Type = run.StageTypeImplement
	implStage.RequiresApproval = true

	priv, _ := sf.issue(t, runRow.ID)
	body, err := json.Marshal(map[string]any{
		"outcome":             "fixup_no_changes",
		"branch":              "fishhawk/run-aaaaaaaa/stage-bbbbbbbb",
		"base_sha":            "base-def",
		"files_changed_count": 0,
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
		t.Errorf("stage.State = %q, want awaiting_approval (fixup_no_changes body must drive the gated terminal re-park)", got.State)
	}
	if len(ar.all) != 0 {
		t.Errorf("artifacts = %d, want 0 (no PR artifact for a no-changes fix-up)", len(ar.all))
	}
	gotRun, err := rr.GetRun(t.Context(), runRow.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if gotRun.PullRequestURL != nil {
		t.Errorf("run.PullRequestURL = %q, want nil (no-changes fix-up touches no PR)", *gotRun.PullRequestURL)
	}
	au.mu.Lock()
	defer au.mu.Unlock()
	var found bool
	for _, e := range au.appended {
		if e.Category == "fixup_no_changes" {
			found = true
		}
	}
	if !found {
		t.Error("no fixup_no_changes audit entry recorded")
	}
}

// TestShipPullRequest_FixupNoChangesOutcome_IsIdempotent pins the #856 guard:
// since no commit landed there is no head_sha to key on, so the guard is
// STAGE-keyed — a runner retry or duplicate delivery of the no-changes report
// must NOT append a second fixup_no_changes audit entry for the same stage.
func TestShipPullRequest_FixupNoChangesOutcome_IsIdempotent(t *testing.T) {
	s, sf, _, au, rr := newPRServerWithOrch(t)
	runRow := rr.seedRun()
	implStage := rr.seedStage(runRow.ID, 0, run.StageStateRunning)
	implStage.Type = run.StageTypeImplement
	implStage.RequiresApproval = true

	priv, _ := sf.issue(t, runRow.ID)
	body, err := json.Marshal(map[string]any{
		"outcome":             "fixup_no_changes",
		"branch":              "fishhawk/run-aaaaaaaa/stage-bbbbbbbb",
		"base_sha":            "base-def",
		"files_changed_count": 0,
	})
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		w := shipPRRequest(t, s, runRow.ID, implStage.ID, priv, body, "")
		if w.Code != http.StatusOK {
			t.Fatalf("delivery %d: status = %d, want 200:\n%s", i, w.Code, w.Body.String())
		}
	}

	au.mu.Lock()
	defer au.mu.Unlock()
	var n int
	for _, e := range au.appended {
		if e.Category == "fixup_no_changes" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("fixup_no_changes audit entries after duplicate = %d, want 1 (stage-keyed guard must suppress the retry)", n)
	}
}

// TestShipPullRequest_FixupNoChangesOutcome_RejectsMissingCoords pins the #856
// validation: the fixup_no_changes variant requires branch + base_sha (no PR
// was opened and no commit landed, so those are the only record of the
// unchanged tip). head_sha is intentionally absent — supplying it is not
// required, and omitting branch or base_sha is a malformed body.
func TestShipPullRequest_FixupNoChangesOutcome_RejectsMissingCoords(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, _, _, _ := newPRServer(t, runID, stageID)
	priv, _ := sf.issue(t, runID)
	for _, body := range []string{
		`{"outcome":"fixup_no_changes","base_sha":"b"}`, // missing branch
		`{"outcome":"fixup_no_changes","branch":"br"}`,  // missing base_sha
		`{"outcome":"fixup_no_changes"}`,                // missing both
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

// TestShipPullRequest_BearerActorKindClosedSet is the #1027 regression
// for the bearer-auth branch, which used to stamp the literal
// "operator" — outside the DB CHECK (migration 0002: agent/user/system)
// and the OpenAPI actor_kind enum; fakes never enforced the constraint,
// so it shipped unnoticed. The recorded kind must come from the token
// subject and stay within the closed set: user for a plain subject,
// agent for an operator-agent subject (ADR-040 D4).
func TestShipPullRequest_BearerActorKindClosedSet(t *testing.T) {
	closedSet := map[audit.ActorKind]bool{
		audit.ActorAgent: true, audit.ActorUser: true, audit.ActorSystem: true,
	}
	cases := []struct {
		name    string
		subject string
		want    audit.ActorKind
	}{
		{"plain operator subject records user", "operator:test", audit.ActorUser},
		{"operator-agent subject records agent", operatorAgentSubject, audit.ActorAgent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runID, stageID := uuid.New(), uuid.New()
			s, _, _, au, _ := newPRServer(t, runID, stageID)

			url := fmt.Sprintf("/v0/runs/%s/pull-request?stage_id=%s", runID, stageID)
			req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader(validPRBytes(t)))
			req.Header.Set("Content-Type", "application/json")
			req.SetPathValue("run_id", runID.String())
			req.SetPathValue("stage_id", stageID.String())
			ctx := context.WithValue(req.Context(), ctxKeyIdentity, Identity{
				Subject: tc.subject,
				TokenID: "tok-abc",
				Scopes:  []string{"write:runs"},
			})
			w := httptest.NewRecorder()
			s.handleShipPullRequest(w, req.WithContext(ctx))

			if w.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
			}
			if len(au.appended) != 1 {
				t.Fatalf("audit entries = %d, want 1", len(au.appended))
			}
			entry := au.appended[0]
			if entry.ActorKind == nil || !closedSet[*entry.ActorKind] {
				t.Fatalf("ActorKind = %v, want a member of the closed set {agent,user,system}", entry.ActorKind)
			}
			if *entry.ActorKind != tc.want {
				t.Errorf("ActorKind = %q, want %q", *entry.ActorKind, tc.want)
			}
			if entry.ActorSubject == nil || *entry.ActorSubject != tc.subject {
				t.Errorf("ActorSubject = %v, want %q", entry.ActorSubject, tc.subject)
			}
		})
	}
}

// parkRecordingRepo embeds the orchestratorRepo and additionally implements
// the scopeCompletenessParker capability (#1231) so the park handler takes
// its production type-asserted path. It records the park struct + audit
// params it was handed and drives the real running → awaiting_scope_decision
// transition, so the test can assert the cross-module wire fields mapped
// correctly into run.ScopeCompletenessPark.
type parkRecordingRepo struct {
	*orchestratorRepo
	parkCalls []parkRecord
}

type parkRecord struct {
	stageID uuid.UUID
	park    run.ScopeCompletenessPark
	append  audit.ChainAppendParams
}

func (r *parkRecordingRepo) ParkScopeCompletenessAndAppend(ctx context.Context, stageID uuid.UUID, park run.ScopeCompletenessPark, p audit.ChainAppendParams) (*run.Stage, bool, error) {
	r.parkCalls = append(r.parkCalls, parkRecord{stageID: stageID, park: park, append: p})
	st, err := r.TransitionStage(ctx, stageID, run.StageStateAwaitingScopeDecision, nil)
	return st, true, err
}

// TestShipPullRequest_ScopeParkOutcome_ParksStage pins the #1231 backend
// park-report branch on the production (parker-capable) path: a
// {outcome:"scope_park"} report records the held-commit ScopeCompletenessPark
// payload, parks the implement stage in awaiting_scope_decision, opens no PR
// artifact, and the wire fields map byte-for-byte into the park struct.
func TestShipPullRequest_ScopeParkOutcome_ParksStage(t *testing.T) {
	sf := newSigningFake()
	ar := newFakeArtifactRepo()
	au := newAuditFake()
	rr := &parkRecordingRepo{orchestratorRepo: newOrchestratorRepo()}
	s := New(Config{
		Addr: "127.0.0.1:0", SigningRepo: sf, ArtifactRepo: ar, AuditRepo: au, RunRepo: rr,
		Orchestrator: &orchestrator.Orchestrator{Runs: rr},
	})
	runRow := rr.seedRun()
	implStage := rr.seedStage(runRow.ID, 0, run.StageStateRunning)
	implStage.Type = run.StageTypeImplement
	implStage.RequiresApproval = true

	priv, _ := sf.issue(t, runRow.ID)
	body, err := json.Marshal(map[string]any{
		"outcome":           "scope_park",
		"branch":            "fishhawk/run-aaaaaaaa/slice-0",
		"head_sha":          "1111111111111111111111111111111111111111",
		"base_sha":          "2222222222222222222222222222222222222222",
		"verified_tree_sha": "3333333333333333333333333333333333333333",
		"missing_paths":     []string{"backend/internal/foo/foo_test.go", "docs/foo.md"},
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
	if got.State != run.StageStateAwaitingScopeDecision {
		t.Errorf("stage.State = %q, want awaiting_scope_decision", got.State)
	}
	if len(ar.all) != 0 {
		t.Errorf("artifacts = %d, want 0 (no PR artifact for a scope-park report)", len(ar.all))
	}
	if len(rr.parkCalls) != 1 {
		t.Fatalf("park calls = %d, want 1", len(rr.parkCalls))
	}
	pc := rr.parkCalls[0]
	if pc.park.HeldCommitSHA != "1111111111111111111111111111111111111111" ||
		pc.park.RunBranch != "fishhawk/run-aaaaaaaa/slice-0" ||
		pc.park.VerifiedTreeSHA != "3333333333333333333333333333333333333333" ||
		len(pc.park.MissingPaths) != 2 {
		t.Errorf("recorded park = %+v, want the wire fields mapped 1:1", pc.park)
	}
	if pc.append.Category != CategoryScopeCompletenessParked {
		t.Errorf("park audit category = %q, want %q", pc.append.Category, CategoryScopeCompletenessParked)
	}
}

// TestShipPullRequest_ScopeParkOutcome_FallbackTransitions pins the degraded
// (non-parker repo) path: a repo that does NOT implement the parker still
// transitions the stage to awaiting_scope_decision and best-effort appends
// the scope_completeness_parked audit entry (#1231).
func TestShipPullRequest_ScopeParkOutcome_FallbackTransitions(t *testing.T) {
	s, sf, ar, au, rr := newPRServerWithOrch(t)
	runRow := rr.seedRun()
	implStage := rr.seedStage(runRow.ID, 0, run.StageStateRunning)
	implStage.Type = run.StageTypeImplement

	priv, _ := sf.issue(t, runRow.ID)
	body, err := json.Marshal(map[string]any{
		"outcome":           "scope_park",
		"branch":            "fishhawk/run-aaaaaaaa/slice-0",
		"head_sha":          "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		"base_sha":          "cafecafecafecafecafecafecafecafecafecafe",
		"verified_tree_sha": "f00df00df00df00df00df00df00df00df00df00d",
		"missing_paths":     []string{"backend/internal/foo/foo.go"},
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
	if got.State != run.StageStateAwaitingScopeDecision {
		t.Errorf("stage.State = %q, want awaiting_scope_decision (fallback must still park)", got.State)
	}
	if len(ar.all) != 0 {
		t.Errorf("artifacts = %d, want 0", len(ar.all))
	}
	au.mu.Lock()
	defer au.mu.Unlock()
	var found bool
	var missingInPayload bool
	for _, e := range au.appended {
		if e.Category == CategoryScopeCompletenessParked {
			found = true
			if strings.Contains(string(e.Payload), "foo.go") {
				missingInPayload = true
			}
		}
	}
	if !found {
		t.Error("no scope_completeness_parked audit entry recorded on the fallback path")
	}
	if !missingInPayload {
		t.Error("scope_completeness_parked payload missing the missing_paths content")
	}
}

// TestShipPullRequest_ScopeParkOutcome_RejectsMissingCoords pins the #1231
// validation: the scope_park variant requires branch + head_sha + base_sha +
// verified_tree_sha + a non-empty missing_paths.
func TestShipPullRequest_ScopeParkOutcome_RejectsMissingCoords(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, _, _, _ := newPRServer(t, runID, stageID)
	priv, _ := sf.issue(t, runID)
	for _, body := range []string{
		`{"outcome":"scope_park","head_sha":"h","base_sha":"b","verified_tree_sha":"t","missing_paths":["x"]}`,            // missing branch
		`{"outcome":"scope_park","branch":"br","base_sha":"b","verified_tree_sha":"t","missing_paths":["x"]}`,             // missing head_sha
		`{"outcome":"scope_park","branch":"br","head_sha":"h","verified_tree_sha":"t","missing_paths":["x"]}`,             // missing base_sha
		`{"outcome":"scope_park","branch":"br","head_sha":"h","base_sha":"b","missing_paths":["x"]}`,                      // missing verified_tree_sha
		`{"outcome":"scope_park","branch":"br","head_sha":"h","base_sha":"b","verified_tree_sha":"t"}`,                    // missing missing_paths
		`{"outcome":"scope_park","branch":"br","head_sha":"h","base_sha":"b","verified_tree_sha":"t","missing_paths":[]}`, // empty missing_paths
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

// successPRBytesWithSupplemental returns a complete success pullRequestBody
// payload that also carries the base-rebase re-invoke exemption delta (#1218):
// the supplemental_scope_exemptions array the runner ships on a post-re-invoke
// success ship.
func successPRBytesWithSupplemental(t *testing.T, exemptions []scopeExemption) []byte {
	t.Helper()
	body, err := json.Marshal(pullRequestBody{
		PRNumber:                    42,
		PRURL:                       "https://github.com/kuhlman-labs/fishhawk/pull/42",
		Branch:                      "fishhawk/run-aaa/stage-bbb",
		HeadSHA:                     "1111111111111111111111111111111111111111",
		BaseSHA:                     "2222222222222222222222222222222222222222",
		Title:                       "Add a make target.",
		Body:                        "Opened by Fishhawk on behalf of @octocat.",
		FilesChangedCount:           3,
		SupplementalScopeExemptions: exemptions,
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// findAuditByCategory returns the single appended entry of the given category,
// or nil if none. Fails the test if more than one is present.
func findAuditByCategory(t *testing.T, au *auditFake, category string) *audit.ChainAppendParams {
	t.Helper()
	au.mu.Lock()
	defer au.mu.Unlock()
	var got *audit.ChainAppendParams
	for i := range au.appended {
		if au.appended[i].Category == category {
			if got != nil {
				t.Fatalf("more than one %q audit entry", category)
			}
			e := au.appended[i]
			got = &e
		}
	}
	return got
}

// TestShipPullRequest_SupplementalScopeExemptions_EmitsAuditRow pins the DELTA
// PRESENT branch (#1218): a success ship carrying supplemental_scope_exemptions
// (the base-rebase re-invoke exemption delta) emits exactly one supplemental
// scope_files_exempted audit row whose payload carries origin
// "base_rebase_reinvoke" and the supplied path+reason entries — the observable
// shipped behavior, an audit-log row queryable via the audit endpoint.
func TestShipPullRequest_SupplementalScopeExemptions_EmitsAuditRow(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, _, au, _ := newPRServer(t, runID, stageID)
	priv, _ := sf.issue(t, runID)
	body := successPRBytesWithSupplemental(t, []scopeExemption{
		{Path: "backend/internal/foo/foo_test.go", Reason: "coupled test already correct after re-invoke"},
	})

	w := shipPRRequest(t, s, runID, stageID, priv, body, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}

	// pull_request_opened must still land, plus exactly one supplemental row.
	if got := findAuditByCategory(t, au, "pull_request_opened"); got == nil {
		t.Fatal("no pull_request_opened audit entry recorded")
	}
	sup := findAuditByCategory(t, au, CategoryScopeFilesExempted)
	if sup == nil {
		t.Fatalf("no %q audit entry recorded for the supplemental delta", CategoryScopeFilesExempted)
	}

	var payload struct {
		RunID      string           `json:"run_id"`
		StageID    string           `json:"stage_id"`
		Origin     string           `json:"origin"`
		Exemptions []scopeExemption `json:"exemptions"`
	}
	if err := json.Unmarshal(sup.Payload, &payload); err != nil {
		t.Fatalf("decode supplemental payload: %v", err)
	}
	if payload.Origin != "base_rebase_reinvoke" {
		t.Errorf("payload origin = %q, want base_rebase_reinvoke", payload.Origin)
	}
	if payload.RunID != runID.String() || payload.StageID != stageID.String() {
		t.Errorf("payload run/stage = %q/%q, want %q/%q", payload.RunID, payload.StageID, runID, stageID)
	}
	if len(payload.Exemptions) != 1 ||
		payload.Exemptions[0].Path != "backend/internal/foo/foo_test.go" ||
		payload.Exemptions[0].Reason != "coupled test already correct after re-invoke" {
		t.Errorf("payload exemptions = %+v, want the supplied path+reason", payload.Exemptions)
	}
}

// TestShipPullRequest_NoSupplementalScopeExemptions_NoAuditRow pins the DELTA
// EMPTY/ABSENT branch (#1218): a success ship WITHOUT
// supplemental_scope_exemptions (every non-re-invoke ship, the byte-identical
// pre-change path) emits only the pull_request_opened row and NO
// scope_files_exempted row.
func TestShipPullRequest_NoSupplementalScopeExemptions_NoAuditRow(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, _, au, _ := newPRServer(t, runID, stageID)
	priv, _ := sf.issue(t, runID)
	body := validPRBytes(t) // no supplemental_scope_exemptions field

	w := shipPRRequest(t, s, runID, stageID, priv, body, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	if got := findAuditByCategory(t, au, "pull_request_opened"); got == nil {
		t.Fatal("no pull_request_opened audit entry recorded")
	}
	if sup := findAuditByCategory(t, au, CategoryScopeFilesExempted); sup != nil {
		t.Errorf("unexpected %q audit entry on a non-re-invoke ship: %s", CategoryScopeFilesExempted, string(sup.Payload))
	}
}

// categoryFailAuditRepo wraps an auditFake and fails AppendChained ONLY for a
// chosen category, delegating every other category (and all other methods) to
// the embedded fake. It lets a test fail the best-effort supplemental
// scope_files_exempted append WITHOUT failing the authoritative
// pull_request_opened append that precedes it.
type categoryFailAuditRepo struct {
	*auditFake
	failCategory string
}

func (a *categoryFailAuditRepo) AppendChained(ctx context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	if p.Category == a.failCategory {
		return nil, fmt.Errorf("audit append forced failure for %q", p.Category)
	}
	return a.auditFake.AppendChained(ctx, p)
}

// TestShipPullRequest_SupplementalScopeExemptions_BestEffortOnAuditError pins
// the BEST-EFFORT FAILURE branch (#1218): when the supplemental
// scope_files_exempted append errors, the handler still returns success and
// persists the PR artifact — the observability row must never wedge the
// forward-gated terminal transition.
func TestShipPullRequest_SupplementalScopeExemptions_BestEffortOnAuditError(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	sf := newSigningFake()
	ar := newFakeArtifactRepo()
	au := newAuditFake()
	rr := newPromptRunRepo()
	rr.getStages[stageID] = &run.Stage{ID: stageID, RunID: runID}
	s := New(Config{
		Addr:         "127.0.0.1:0",
		SigningRepo:  sf,
		ArtifactRepo: ar,
		AuditRepo:    &categoryFailAuditRepo{auditFake: au, failCategory: CategoryScopeFilesExempted},
		RunRepo:      rr,
	})
	priv, _ := sf.issue(t, runID)
	body := successPRBytesWithSupplemental(t, []scopeExemption{
		{Path: "backend/internal/foo/foo_test.go", Reason: "re-invoke justified"},
	})

	w := shipPRRequest(t, s, runID, stageID, priv, body, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (a failed observability append must not wedge the stage):\n%s", w.Code, w.Body.String())
	}
	if len(ar.all) != 1 {
		t.Errorf("artifacts = %d, want 1 (the PR artifact must still persist)", len(ar.all))
	}
	// pull_request_opened landed (it precedes the forced-fail supplemental); the
	// supplemental row did not (its append errored and was swallowed).
	if got := findAuditByCategory(t, au, "pull_request_opened"); got == nil {
		t.Error("no pull_request_opened audit entry recorded")
	}
	if sup := findAuditByCategory(t, au, CategoryScopeFilesExempted); sup != nil {
		t.Errorf("supplemental row unexpectedly recorded despite forced append error: %s", string(sup.Payload))
	}
}

// --- Supplemental base-rebase re-invoke REVIEW dispatch (#1250 / ADR-042) ---

// newSupplementalReviewPRServer wires a reviewer-configured implement-review
// server (orchestratorRepo + plan artifact + reviewer set, via
// newImplementReviewServer) and flips the implement stage to `running` so the
// #742 terminal-drive branch of handleShipPullRequest fires and dispatches the
// #1250 supplemental review. Returns the server, signing fake, audit fake, run
// repo, run row, and the running implement stage.
func newSupplementalReviewPRServer(t *testing.T, reviewer PlanReviewer, spec []byte) (
	*Server, *signingFake, *auditFake, *orchestratorRepo, *run.Run, *run.Stage,
) {
	t.Helper()
	s, sf, au, rr, runRow, implStage := newImplementReviewServer(t, reviewer, spec)
	implStage.State = run.StageStateRunning
	return s, sf, au, rr, runRow, implStage
}

// runnerShapedSupplementalPRBody returns a realistic runner-shaped success
// pull-request body whose supplemental_scope_exemptions use the runner's
// lowercase {path,reason} wire keys (matching scopeExemptionEvidence) — the
// cross-boundary seam the integration test drives end-to-end.
func runnerShapedSupplementalPRBody(t *testing.T, headSHA, path, reason string) []byte {
	t.Helper()
	body := fmt.Sprintf(`{
		"pr_number": 42,
		"pr_url": "https://github.com/kuhlman-labs/example/pull/42",
		"branch": "fishhawk/run-aaa/stage-bbb",
		"head_sha": %q,
		"base_sha": "2222222222222222222222222222222222222222",
		"title": "Add foo helper",
		"body": "Opened by Fishhawk.",
		"files_changed_count": 2,
		"supplemental_scope_exemptions": [{"path": %q, "reason": %q}]
	}`, headSHA, path, reason)
	return []byte(body)
}

// TestShipPullRequest_SupplementalReinvokeReview_CrossBoundary is the
// cross-boundary integration test (#1250): a runner-shaped success body whose
// supplemental_scope_exemptions use the runner's lowercase {path,reason} wire
// keys flows through handleShipPullRequest's decode → the terminal-drive
// dispatch → runSupplementalReinvokeReview, and the captured reviewer prompt's
// gate_evidence section contains the supplied exemption path+reason plus the
// supplemental framing. This crosses payload-decode, dispatch, and
// prompt-render layers (cf. #618) — a per-layer unit would pass while the seam
// breaks. It also asserts the #1218 audit row lands complementarily.
func TestShipPullRequest_SupplementalReinvokeReview_CrossBoundary(t *testing.T) {
	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-opus-4-7",
	}
	s, sf, au, _, runRow, implStage := newSupplementalReviewPRServer(t, reviewer, specImplementAdvisoryReviewers)
	priv, _ := sf.issue(t, runRow.ID)

	const reHead = "abc123abc123abc123abc123abc123abc123abcd"
	const exPath = "backend/internal/foo/foo.go"
	const exReason = "already correct after the base rebase"
	body := runnerShapedSupplementalPRBody(t, reHead, exPath, exReason)

	w := shipPRRequest(t, s, runRow.ID, implStage.ID, priv, body, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}

	// Advisory dispatch is detached — drain before asserting on the prompt.
	s.waitBackgroundReviews()

	reviewer.mu.Lock()
	calls := append([]string(nil), reviewer.calls...)
	reviewer.mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("reviewer invoked %d times, want 1", len(calls))
	}
	got := calls[0]
	for _, want := range []string{
		// The supplemental framing.
		"Supplemental review: base-rebase re-invoke scope exemptions",
		"SUPPLEMENTAL, bounded review pass — NOT a full re-review",
		// The exemption delta rendered in the gate_evidence section — the seam
		// from the runner's lowercase wire keys to the reviewer-visible prompt.
		"Self-exempted declared scope files (agent justified leaving these unchanged):",
		"- " + exPath + " — " + exReason,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("supplemental reviewer prompt missing %q:\n%s", want, got)
		}
	}
	// No diff section in the supplemental pass.
	if strings.Contains(got, "### Diff under review") {
		t.Errorf("supplemental prompt must not render a diff section:\n%s", got)
	}

	// The verdict is recorded with the re-invoke provenance.
	sup := findSupplementalImplementReviewed(t, au, implStage.ID)
	if sup == nil {
		t.Fatal("no supplemental implement_reviewed entry recorded")
	}
	if sup.HeadSHA != reHead {
		t.Errorf("supplemental HeadSHA = %q, want %q", sup.HeadSHA, reHead)
	}
	// The #1218 audit row is complementary and still lands.
	if findAuditByCategory(t, au, CategoryScopeFilesExempted) == nil {
		t.Error("complementary #1218 scope_files_exempted audit row missing")
	}
}

// TestShipPullRequest_SupplementalReinvokeReview_EmptyDelta_NoDispatch pins the
// EMPTY-delta branch at the HTTP boundary (#1250 failure-mode 4): an ordinary
// non-re-invoke success ship (no supplemental_scope_exemptions) dispatches NO
// supplemental review and advances the stage normally — byte-identical to the
// pre-#1250 path.
func TestShipPullRequest_SupplementalReinvokeReview_EmptyDelta_NoDispatch(t *testing.T) {
	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-opus-4-7",
	}
	s, sf, au, rr, runRow, implStage := newSupplementalReviewPRServer(t, reviewer, specImplementAdvisoryReviewers)
	priv, _ := sf.issue(t, runRow.ID)

	w := shipPRRequest(t, s, runRow.ID, implStage.ID, priv, validPRBytes(t), "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	s.waitBackgroundReviews()

	reviewer.mu.Lock()
	calls := len(reviewer.calls)
	reviewer.mu.Unlock()
	if calls != 0 {
		t.Errorf("reviewer invoked %d times on an empty-delta ship, want 0", calls)
	}
	if n := countAuditCategory(au, "implement_reviewed"); n != 0 {
		t.Errorf("implement_reviewed = %d on an empty-delta ship, want 0", n)
	}
	// The stage advanced normally (running → awaiting_approval, it requires
	// approval) — the empty-delta path is the unchanged terminal drive.
	got, err := rr.GetStage(t.Context(), implStage.ID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	if got.State != run.StageStateAwaitingApproval {
		t.Errorf("stage state = %q, want awaiting_approval (unchanged terminal drive)", got.State)
	}
}

// TestShipPullRequest_SupplementalReinvokeReview_GatingApprove_Advances pins
// failure-mode (3) at the HTTP boundary: a gating-authority supplemental
// APPROVE advances the stage normally and records the additive verdict.
func TestShipPullRequest_SupplementalReinvokeReview_GatingApprove_Advances(t *testing.T) {
	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "gpt-5.5",
	}
	s, sf, au, rr, runRow, implStage := newSupplementalReviewPRServer(t, reviewer, specImplementGatingReviewers)
	priv, _ := sf.issue(t, runRow.ID)

	body := runnerShapedSupplementalPRBody(t, "feed00dfeed00dfeed00dfeed00dfeed00dfeed0",
		"backend/internal/foo/foo.go", "unchanged after rebase")
	w := shipPRRequest(t, s, runRow.ID, implStage.ID, priv, body, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}

	got, err := rr.GetStage(t.Context(), implStage.ID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	if got.State != run.StageStateAwaitingApproval {
		t.Errorf("stage state = %q, want awaiting_approval (gating approve advances)", got.State)
	}
	if findSupplementalImplementReviewed(t, au, implStage.ID) == nil {
		t.Error("no supplemental implement_reviewed entry recorded on gating approve")
	}
}

// TestShipPullRequest_SupplementalReinvokeReview_GatingReject_FailsAndClosesPR
// pins failure-mode (2) at the HTTP boundary: a gating-authority supplemental
// REJECT fails the implement stage category-B with implementReviewGatingRejectReason
// and closes the dangling PR via the #877 helper. Wires a GitHub fake +
// InstallationID so the close actually fires and is audited.
func TestShipPullRequest_SupplementalReinvokeReview_GatingReject_FailsAndClosesPR(t *testing.T) {
	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictReject},
		model:   "gpt-5.5",
	}
	s, sf, au, rr, runRow, implStage := newSupplementalReviewPRServer(t, reviewer, specImplementGatingReviewers)

	// Wire a GitHub fake so closePRAfterGatingReject actually closes + audits.
	rec := &closeRecorder{}
	ghSrv := rec.server(t)
	s.cfg.GitHub = &githubclient.Client{
		BaseURL: ghSrv.URL,
		Tokens:  &ghTokensStub{tok: "ghs_test"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		AppJWT:  func() (string, error) { return "gha_app_jwt_test", nil },
	}
	inst := int64(12345)
	runRow.InstallationID = &inst

	priv, _ := sf.issue(t, runRow.ID)
	body := runnerShapedSupplementalPRBody(t, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		"backend/internal/foo/foo.go", "hollow reason the reviewer rejects")
	w := shipPRRequest(t, s, runRow.ID, implStage.ID, priv, body, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}

	got, err := rr.GetStage(t.Context(), implStage.ID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	if got.State != run.StageStateFailed {
		t.Fatalf("stage state = %q, want failed (gating reject)", got.State)
	}
	if got.FailureCategory == nil || *got.FailureCategory != run.FailureB {
		t.Errorf("failure category = %v, want B", got.FailureCategory)
	}
	if got.FailureReason == nil || !strings.HasPrefix(*got.FailureReason, implementReviewGatingRejectPrefix) {
		t.Errorf("failure reason = %v, want the gating-reject prefix", got.FailureReason)
	}
	// The dangling PR was closed and audited (#877 helper reused).
	rec.mu.Lock()
	closeCalls := rec.closeCalls
	rec.mu.Unlock()
	if closeCalls != 1 {
		t.Errorf("PR close calls = %d, want 1 (closePRAfterGatingReject)", closeCalls)
	}
	if n := auditCategoryCount(au, "pull_request_closed_after_review_reject"); n != 1 {
		t.Errorf("pull_request_closed_after_review_reject audit entries = %d, want 1", n)
	}
}

// failTransitionToFailedRepo wraps a *orchestratorRepo and forces an error on
// the StageStateFailed transition ONLY, delegating every other transition (and
// all other methods) to the embedded repo. It drives run.FailStage's internal
// repo.TransitionStage(..., StageStateFailed, ...) call (run/failure.go:58) to
// error WITHOUT failing the handler's earlier reads or non-failed transitions —
// mirroring the categoryFailAuditRepo selective-failure pattern (lines
// 1307-1317) so handleShipPullRequest's ferr != nil degradation branch fires.
type failTransitionToFailedRepo struct {
	*orchestratorRepo
	err error
}

func (r *failTransitionToFailedRepo) TransitionStage(ctx context.Context, id uuid.UUID, to run.StageState, c *run.StageCompletion) (*run.Stage, error) {
	if to == run.StageStateFailed {
		return nil, r.err
	}
	return r.orchestratorRepo.TransitionStage(ctx, id, to, c)
}

// TransitionStageFrom mirrors the forced-error onto the compare-and-swap
// path (#1903). Since orchestratorRepo now implements StageCASTransitioner,
// run.FailStage routes its failed transition through TransitionStageFrom
// rather than TransitionStage — so the forced error must be applied here too
// to keep driving the handler's ferr != nil degradation branch.
func (r *failTransitionToFailedRepo) TransitionStageFrom(ctx context.Context, id uuid.UUID, from, to run.StageState, c *run.StageCompletion) (*run.Stage, error) {
	if to == run.StageStateFailed {
		return nil, r.err
	}
	return r.orchestratorRepo.TransitionStageFrom(ctx, id, from, to, c)
}

// TestShipPullRequest_SupplementalReinvokeReview_GatingReject_FailStageError
// pins the FailStage-ERROR (ferr != nil) degradation sub-branch of the
// gating-reject path: when the gating supplemental reviewer rejects AND
// run.FailStage errors, the handler only WARN-logs and still responds 201
// WITHOUT advancing the stage or closing the PR. This is the mirror of the
// FailStage-SUCCESS branch covered by
// TestShipPullRequest_SupplementalReinvokeReview_GatingReject_FailsAndClosesPR.
func TestShipPullRequest_SupplementalReinvokeReview_GatingReject_FailStageError(t *testing.T) {
	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictReject},
		model:   "gpt-5.5",
	}
	s, sf, au, rr, runRow, implStage := newSupplementalReviewPRServer(t, reviewer, specImplementGatingReviewers)

	// Wire a GitHub fake + InstallationID exactly as the happy-path test, so a
	// PR close WOULD fire and be audited if the degradation branch erroneously
	// attempted one — making closeCalls == 0 a meaningful assertion.
	rec := &closeRecorder{}
	ghSrv := rec.server(t)
	s.cfg.GitHub = &githubclient.Client{
		BaseURL: ghSrv.URL,
		Tokens:  &ghTokensStub{tok: "ghs_test"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		AppJWT:  func() (string, error) { return "gha_app_jwt_test", nil },
	}
	inst := int64(12345)
	runRow.InstallationID = &inst

	// Force run.FailStage's StageStateFailed transition to error, driving the
	// handler's ferr != nil branch. The implement stage is left `running` by
	// newSupplementalReviewPRServer, so FailStage single-steps straight to the
	// failed transition the wrapper intercepts (no dispatched→running hop).
	s.cfg.RunRepo = &failTransitionToFailedRepo{
		orchestratorRepo: rr,
		err:              fmt.Errorf("forced failed-transition error"),
	}

	// Capture the degradation WARN line (recover_test.go:1033 pattern).
	var logBuf bytes.Buffer
	s.cfg.Logger = slog.New(slog.NewJSONHandler(&logBuf, nil))

	priv, _ := sf.issue(t, runRow.ID)
	body := runnerShapedSupplementalPRBody(t, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		"backend/internal/foo/foo.go", "hollow reason the reviewer rejects")
	w := shipPRRequest(t, s, runRow.ID, implStage.ID, priv, body, "")

	// (a) The response is NOT unwound — degradation still returns 201.
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (degradation does not unwind the response):\n%s", w.Code, w.Body.String())
	}

	// (b) The failed transition did not apply — the stage is STILL running
	// (no-advance). rr reads the same underlying store the wrapper delegates to.
	got, err := rr.GetStage(t.Context(), implStage.ID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	if got.State != run.StageStateRunning {
		t.Errorf("stage state = %q, want running (FailStage errored → no-advance)", got.State)
	}

	// (c) The PR was NOT closed — the close helper runs only in the FailStage
	// success arm, which the error branch skips.
	rec.mu.Lock()
	closeCalls := rec.closeCalls
	rec.mu.Unlock()
	if closeCalls != 0 {
		t.Errorf("PR close calls = %d, want 0 (no close on the FailStage-error branch)", closeCalls)
	}

	// (d) No close audit row recorded.
	if n := auditCategoryCount(au, "pull_request_closed_after_review_reject"); n != 0 {
		t.Errorf("pull_request_closed_after_review_reject audit entries = %d, want 0", n)
	}

	// (e) The degradation WARN line was logged, carrying the stage_id.
	logged := logBuf.String()
	if !strings.Contains(logged, "transition to failed-B after supplemental reinvoke review gating reject failed") {
		t.Errorf("missing degradation WARN line:\n%s", logged)
	}
	if !strings.Contains(logged, implStage.ID.String()) {
		t.Errorf("WARN line missing stage_id %s:\n%s", implStage.ID, logged)
	}

	// The honest pull_request_opened audit row still landed (it precedes the
	// gating branch) — the upload is not unwound by the degradation.
	if findAuditByCategory(t, au, "pull_request_opened") == nil {
		t.Error("no pull_request_opened audit entry recorded")
	}
}

// TestShipPullRequest_FixupPush_BackstopReReview_WedgeRepro is the #1932
// cross-boundary seam test: it reproduces the run 98020210 wedge and proves the
// fix-up re-review backstop resolves it. A fixup_pushed report for a NEW head
// arrives with ONLY a prior-head implement_review_started on the ledger (the
// trace-time hook (#793) never fired for the new head — the fix-up's raw trace
// was routed to category-B by a policy re-evaluation, then #788 recovery restored
// the stage). Driving the REAL /pull-request handler with the audit repo, a fake
// GitHub compare endpoint, and a fake reviewer wired, the test asserts the
// observable end state crossing the report-handler → review-dispatch →
// audit-emission layers:
//
//   - BEFORE the report the implement-review merge gate is HELD (a started entry
//     exists but no terminal verdict has landed for the round);
//   - the backstop lands a NEW implement_review_started keyed to the NEW head;
//   - the reviewer is invoked exactly once with the COMPARE-DERIVED delta (the
//     base..head patch, not the full bundle diff);
//   - the fake reviewer drives to a terminal implement_reviewed verdict;
//   - AFTER the round the merge gate COMPUTES SETTLED (the user-visible wedge
//     resolution: implement_review_status/reviewStatusFor for the new head is
//     'complete', not 'pending' — checkImplementReviewSettled is the server-side
//     computation both share, counting terminal verdicts vs configured agents
//     against the implement_review_started ledger).
//
// The done-means assertion is the NEW-head started entry plus the settled gate:
// the test fails if the backstop is wired as a no-op touch.
func TestShipPullRequest_FixupPush_BackstopReReview_WedgeRepro(t *testing.T) {
	ctx := context.Background()
	reviewer := &fakePlanReviewer{verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove}, model: "claude-opus-4-8"}
	s, sf, au, _, runRow, implStage := newFixupReReviewBackstopServer(t, reviewer, cannedCompareOneFile, false)

	const (
		oldHead = "head-old-5732012c"
		newHead = "head-new-5d33d25f"
		base    = "base-def"
	)
	// The wedge shape: a prior-head round's started entry ONLY (no terminal
	// verdict for the round under the fix-up floor), recent so the review-gate
	// backstop bound has not elapsed.
	seedImplementReviewStarted(t, au, runRow.ID, implStage.ID, oldHead, time.Now().UTC())

	// Before the report: the implement-review merge gate is held (started, 0
	// terminal) — the wedge.
	if s.checkImplementReviewSettled(ctx, runRow, implStage) {
		t.Fatal("precondition: implement-review gate must be HELD before the backstop (started, no terminal verdict)")
	}

	priv, _ := sf.issue(t, runRow.ID)
	body, err := json.Marshal(map[string]any{
		"outcome":             "fixup_pushed",
		"branch":              "fishhawk/run-aaaaaaaa/stage-bbbbbbbb",
		"head_sha":            newHead,
		"base_sha":            base,
		"files_changed_count": 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	w := shipPRRequest(t, s, runRow.ID, implStage.ID, priv, body, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	s.waitBackgroundReviews()

	// A NEW implement_review_started keyed to the NEW head landed (the done-means).
	started := startedHeadSHAs(t, au, runRow.ID)
	if len(started) != 2 {
		t.Fatalf("implement_review_started heads = %v, want 2 (seeded prior + backstop's new-head round)", started)
	}
	var sawNewHead bool
	for _, h := range started {
		if h == newHead {
			sawNewHead = true
		}
	}
	if !sawNewHead {
		t.Errorf("no implement_review_started keyed to the new head %q: got %v", newHead, started)
	}

	// The reviewer was invoked exactly once with the COMPARE-DERIVED delta — the
	// base..head patch body from ComparePatch, not the full bundle diff.
	reviewer.mu.Lock()
	calls := append([]string(nil), reviewer.calls...)
	reviewer.mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("reviewer invocations = %d, want 1", len(calls))
	}
	if !strings.Contains(calls[0], "@@ -1 +1 @@") {
		t.Errorf("reviewer prompt did not carry the compare-derived delta patch:\n%s", calls[0])
	}

	// The fake reviewer drove to a terminal verdict.
	if n := countAuditCategory(au, "implement_reviewed"); n != 1 {
		t.Errorf("implement_reviewed entries = %d, want 1 (backstop reviewer must land a terminal verdict)", n)
	}

	// AFTER the round: the merge gate computes SETTLED — the wedge is resolved.
	gotRun, err := s.cfg.RunRepo.GetRun(ctx, runRow.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if !s.checkImplementReviewSettled(ctx, gotRun, implStage) {
		t.Error("implement-review gate must compute SETTLED after the backstop round lands its verdict (wedge resolved)")
	}
}

// TestShipPullRequest_FixupPush_BackstopReReview_DuplicateReport_NoDoubleDispatch
// pins that a redelivered fixup_pushed report does not double-dispatch the
// backstop: the second identical report short-circuits at the existing
// (stage_id, head_sha) dedup in succeedFixupPushStage before the backstop runs,
// so exactly ONE new-head review round is dispatched across both reports.
func TestShipPullRequest_FixupPush_BackstopReReview_DuplicateReport_NoDoubleDispatch(t *testing.T) {
	reviewer := &fakePlanReviewer{verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove}, model: "claude-opus-4-8"}
	s, sf, au, _, runRow, implStage := newFixupReReviewBackstopServer(t, reviewer, cannedCompareOneFile, false)

	const newHead = "head-new-dup"
	seedImplementReviewStarted(t, au, runRow.ID, implStage.ID, "head-old", time.Now().UTC())

	priv, _ := sf.issue(t, runRow.ID)
	body, err := json.Marshal(map[string]any{
		"outcome":             "fixup_pushed",
		"branch":              "fishhawk/run-aaaaaaaa/stage-bbbbbbbb",
		"head_sha":            newHead,
		"base_sha":            "base-old",
		"files_changed_count": 3,
	})
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		w := shipPRRequest(t, s, runRow.ID, implStage.ID, priv, body, "")
		if w.Code != http.StatusOK {
			t.Fatalf("report %d: status = %d, want 200:\n%s", i, w.Code, w.Body.String())
		}
		s.waitBackgroundReviews()
	}

	// One fixup_pushed audit entry (the redelivery deduped) and exactly one
	// new-head review round (started + reviewed), never two.
	if n := countAuditCategory(au, "fixup_pushed"); n != 1 {
		t.Errorf("fixup_pushed audit entries = %d, want 1 (redelivery must dedup)", n)
	}
	newHeadStarts := 0
	for _, h := range startedHeadSHAs(t, au, runRow.ID) {
		if h == newHead {
			newHeadStarts++
		}
	}
	if newHeadStarts != 1 {
		t.Errorf("new-head implement_review_started rounds = %d, want 1 (backstop must not double-dispatch on redelivery)", newHeadStarts)
	}
	if n := countAuditCategory(au, "implement_reviewed"); n != 1 {
		t.Errorf("implement_reviewed entries = %d, want 1 (no double review on redelivery)", n)
	}
	reviewer.mu.Lock()
	defer reviewer.mu.Unlock()
	if len(reviewer.calls) != 1 {
		t.Errorf("reviewer invocations = %d, want 1 (no double review on redelivery)", len(reviewer.calls))
	}
}
