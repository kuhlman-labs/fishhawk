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
