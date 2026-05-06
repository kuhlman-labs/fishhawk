package server

import (
	"bytes"
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

	"github.com/kuhlman-labs/fishhawk/backend/internal/githubapp"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githuboidc"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
)

// fakeTokenProvider stands in for githubapp.TokenProvider.
type fakeTokenProvider struct {
	tok     string
	err     error
	gotInst int64
	calls   int
}

func (f *fakeTokenProvider) Token(_ context.Context, installationID int64) (string, error) {
	f.calls++
	f.gotInst = installationID
	if f.err != nil {
		return "", f.err
	}
	return f.tok, nil
}

func newInstTokenServer(t *testing.T, runID, stageID uuid.UUID, instID *int64) (*Server, *signingFake, *auditFake, *promptRunRepo, *fakeTokenProvider) {
	t.Helper()
	sf := newSigningFake()
	au := newAuditFake()
	rr := newPromptRunRepo()
	rr.getStages[stageID] = &run.Stage{ID: stageID, RunID: runID}
	rr.getRuns[runID] = &run.Run{ID: runID, InstallationID: instID}
	tp := &fakeTokenProvider{tok: "ghs_xyz"}

	var provider githubapp.TokenProvider = tp
	s := New(Config{
		Addr:         "127.0.0.1:0",
		SigningRepo:  sf,
		AuditRepo:    au,
		RunRepo:      rr,
		GitHubTokens: provider,
	})
	return s, sf, au, rr, tp
}

func issueTokenRequest(t *testing.T, s *Server, runID, stageID uuid.UUID, priv ed25519.PrivateKey, body []byte, sigOverride string) *httptest.ResponseRecorder {
	t.Helper()
	url := fmt.Sprintf("/v0/runs/%s/installation-token?stage_id=%s", runID, stageID)
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

func ptrInt64(v int64) *int64 { return &v }

func TestIssueInstallationToken_HappyPath(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, au, _, tp := newInstTokenServer(t, runID, stageID, ptrInt64(123456))
	priv, _ := sf.issue(t, runID)
	body := []byte(`{}`)

	w := issueTokenRequest(t, s, runID, stageID, priv, body, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	var resp installationTokenResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Token != "ghs_xyz" {
		t.Errorf("token = %q", resp.Token)
	}
	if tp.gotInst != 123456 {
		t.Errorf("installationID = %d, want 123456", tp.gotInst)
	}

	if len(au.appended) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(au.appended))
	}
	if au.appended[0].Category != "installation_token_issued" {
		t.Errorf("audit category = %q", au.appended[0].Category)
	}
	// Audit payload must NOT contain the raw token — only its sha256.
	var payload map[string]any
	_ = json.Unmarshal(au.appended[0].Payload, &payload)
	if got, ok := payload["token_sha256"].(string); !ok || got == "" {
		t.Errorf("audit payload missing token_sha256: %v", payload)
	}
	for k, v := range payload {
		if s, ok := v.(string); ok && s == "ghs_xyz" {
			t.Errorf("audit payload key %q leaks raw token: %v", k, payload)
		}
	}
}

func TestIssueInstallationToken_NoInstallationOnRun_400(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	// nil installationID — runs with no GitHub-attributed
	// installation can't have a token minted.
	s, sf, _, _, _ := newInstTokenServer(t, runID, stageID, nil)
	priv, _ := sf.issue(t, runID)

	w := issueTokenRequest(t, s, runID, stageID, priv, []byte(`{}`), "")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestIssueInstallationToken_TokenProviderError_502(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, _, _, tp := newInstTokenServer(t, runID, stageID, ptrInt64(42))
	tp.err = errors.New("github said no")
	priv, _ := sf.issue(t, runID)

	w := issueTokenRequest(t, s, runID, stageID, priv, []byte(`{}`), "")
	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502:\n%s", w.Code, w.Body.String())
	}
}

func TestIssueInstallationToken_SignatureMissing_401(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, _, _, _, _ := newInstTokenServer(t, runID, stageID, ptrInt64(42))
	url := fmt.Sprintf("/v0/runs/%s/installation-token?stage_id=%s", runID, stageID)
	req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader([]byte(`{}`)))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestIssueInstallationToken_StageMismatch_400(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, _, rr, _ := newInstTokenServer(t, runID, stageID, ptrInt64(42))
	rr.getStages[stageID] = &run.Stage{ID: stageID, RunID: uuid.New()} // different run
	priv, _ := sf.issue(t, runID)

	w := issueTokenRequest(t, s, runID, stageID, priv, []byte(`{}`), "")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (stage doesn't belong to run)", w.Code)
	}
}

func TestIssueInstallationToken_Unconfigured_503(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	url := fmt.Sprintf("/v0/runs/%s/installation-token?stage_id=%s", uuid.New(), uuid.New())
	req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader([]byte(`{}`)))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

// newInstTokenServerWithOIDC builds a server wired for OIDC bearer
// auth on the installation-token endpoint, mirroring the production
// posture used by the auth pre-step (#201).
func newInstTokenServerWithOIDC(t *testing.T, runID, stageID uuid.UUID, instID *int64, verifier *stubOIDCVerifier) (*Server, *auditFake, *promptRunRepo, *fakeTokenProvider) {
	t.Helper()
	au := newAuditFake()
	rr := newPromptRunRepo()
	rr.getStages[stageID] = &run.Stage{ID: stageID, RunID: runID}
	rr.getRuns[runID] = &run.Run{
		ID:             runID,
		Repo:           "kuhlman-labs/fishhawk",
		WorkflowID:     "feature_change",
		InstallationID: instID,
	}
	tp := &fakeTokenProvider{tok: "ghs_xyz"}

	var provider githubapp.TokenProvider = tp
	s := New(Config{
		Addr: "127.0.0.1:0",
		// SigningRepo intentionally nil — OIDC path doesn't touch it.
		SigningRepo:  newSigningFake(),
		AuditRepo:    au,
		RunRepo:      rr,
		GitHubTokens: provider,
		OIDCVerifier: verifier,
		OIDCAudience: "fishhawk-dev",
	})
	return s, au, rr, tp
}

func issueTokenOIDCRequest(t *testing.T, s *Server, runID, stageID uuid.UUID, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	url := fmt.Sprintf("/v0/runs/%s/installation-token?stage_id=%s", runID, stageID)
	req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

func TestIssueInstallationToken_OIDC_HappyPath(t *testing.T) {
	verifier := &stubOIDCVerifier{
		claims: &githuboidc.Claims{Repository: "kuhlman-labs/fishhawk", Workflow: "feature_change"},
	}
	runID, stageID := uuid.New(), uuid.New()
	s, au, _, tp := newInstTokenServerWithOIDC(t, runID, stageID, ptrInt64(123456), verifier)

	w := issueTokenOIDCRequest(t, s, runID, stageID, "fake-jwt")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	var resp installationTokenResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Token != "ghs_xyz" {
		t.Errorf("token = %q", resp.Token)
	}
	if tp.gotInst != 123456 {
		t.Errorf("installationID = %d", tp.gotInst)
	}
	if verifier.callCount != 1 {
		t.Errorf("OIDC Verify called %d times, want 1", verifier.callCount)
	}
	if verifier.gotToken != "fake-jwt" {
		t.Errorf("OIDC token forwarded = %q", verifier.gotToken)
	}
	if verifier.gotExp.Repository != "kuhlman-labs/fishhawk" {
		t.Errorf("OIDC expectations.Repository = %q", verifier.gotExp.Repository)
	}

	// Audit must record auth_method=oidc.
	if len(au.appended) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(au.appended))
	}
	var payload map[string]any
	_ = json.Unmarshal(au.appended[0].Payload, &payload)
	if got, _ := payload["auth_method"].(string); got != "oidc" {
		t.Errorf("auth_method = %q, want oidc", got)
	}
}

func TestIssueInstallationToken_OIDC_ClaimMismatch_401(t *testing.T) {
	verifier := &stubOIDCVerifier{err: githuboidc.ErrClaimMismatch}
	runID, stageID := uuid.New(), uuid.New()
	s, _, _, _ := newInstTokenServerWithOIDC(t, runID, stageID, ptrInt64(42), verifier)

	w := issueTokenOIDCRequest(t, s, runID, stageID, "fake-jwt")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestIssueInstallationToken_OIDC_AuthMethodPreferredOverEd25519(t *testing.T) {
	// When both Authorization: Bearer AND X-Fishhawk-Signature are
	// present, OIDC wins and the audit reflects that.
	verifier := &stubOIDCVerifier{
		claims: &githuboidc.Claims{Repository: "kuhlman-labs/fishhawk", Workflow: "feature_change"},
	}
	runID, stageID := uuid.New(), uuid.New()
	s, au, _, _ := newInstTokenServerWithOIDC(t, runID, stageID, ptrInt64(42), verifier)

	url := fmt.Sprintf("/v0/runs/%s/installation-token?stage_id=%s", runID, stageID)
	req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer some-jwt")
	req.Header.Set("X-Fishhawk-Signature", "deadbeef") // intentionally invalid hex-ish; OIDC should win first
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (OIDC should win):\n%s", w.Code, w.Body.String())
	}
	var payload map[string]any
	_ = json.Unmarshal(au.appended[0].Payload, &payload)
	if got, _ := payload["auth_method"].(string); got != "oidc" {
		t.Errorf("auth_method = %q, want oidc", got)
	}
}

func TestIssueInstallationToken_NoAuth_401(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, _, _, _, _ := newInstTokenServer(t, runID, stageID, ptrInt64(42))

	url := fmt.Sprintf("/v0/runs/%s/installation-token?stage_id=%s", runID, stageID)
	req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader([]byte(`{}`)))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}
