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

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
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
