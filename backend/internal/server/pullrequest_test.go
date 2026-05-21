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
