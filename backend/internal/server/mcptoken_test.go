package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mcptoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
)

// fakeMCPTokenRepo is an in-memory mcptoken.Repository for handler
// tests. Issue records its inputs + returns a deterministic token;
// Authenticate / Revoke / RevokeForRun are stubbed enough to let
// the rest of the suite read back the issuance.
type fakeMCPTokenRepo struct {
	mu        sync.Mutex
	issued    []mcptoken.IssueParams
	tokens    map[uuid.UUID]*mcptoken.Token
	issueErr  error
	nextToken func(p mcptoken.IssueParams) *mcptoken.Token
}

func newFakeMCPTokenRepo() *fakeMCPTokenRepo {
	return &fakeMCPTokenRepo{tokens: map[uuid.UUID]*mcptoken.Token{}}
}

func (f *fakeMCPTokenRepo) Issue(_ context.Context, p mcptoken.IssueParams) (*mcptoken.Token, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.issueErr != nil {
		return nil, f.issueErr
	}
	f.issued = append(f.issued, p)
	var tok *mcptoken.Token
	if f.nextToken != nil {
		tok = f.nextToken(p)
	} else {
		ttl := p.TTL
		if ttl <= 0 {
			ttl = mcptoken.DefaultTTL
		}
		tok = &mcptoken.Token{
			ID:        uuid.New(),
			RunID:     p.RunID,
			IssuedAt:  time.Now().UTC(),
			ExpiresAt: time.Now().UTC().Add(ttl),
			PlainText: mcptoken.TokenPrefix + "stub-plaintext-value",
		}
	}
	f.tokens[tok.ID] = tok
	return tok, nil
}

func (f *fakeMCPTokenRepo) Authenticate(_ context.Context, _ string) (*mcptoken.Token, error) {
	return nil, mcptoken.ErrNotFound
}

func (f *fakeMCPTokenRepo) Revoke(_ context.Context, id uuid.UUID) (*mcptoken.Token, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	tok, ok := f.tokens[id]
	if !ok {
		return nil, mcptoken.ErrNotFound
	}
	now := time.Now().UTC()
	tok.RevokedAt = &now
	return tok, nil
}

func (f *fakeMCPTokenRepo) RevokeForRun(_ context.Context, _ uuid.UUID) (int, error) {
	return 0, nil
}

func (f *fakeMCPTokenRepo) GetByID(_ context.Context, id uuid.UUID) (*mcptoken.Token, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	tok, ok := f.tokens[id]
	if !ok {
		return nil, mcptoken.ErrNotFound
	}
	return tok, nil
}

// auditCapture is a minimal audit.Repository that just records
// AppendChained calls. Sufficient for handler tests that assert
// the right audit row was written.
type auditCapture struct {
	mu       sync.Mutex
	appended []audit.ChainAppendParams
}

func (a *auditCapture) Append(_ context.Context, _ audit.AppendParams) (*audit.Entry, error) {
	return nil, errors.New("not implemented")
}

func (a *auditCapture) ChainsByParent(_ context.Context, _ uuid.UUID, _ bool) ([]*audit.Entry, error) {
	return nil, nil
}
func (a *auditCapture) AppendChained(_ context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.appended = append(a.appended, p)
	return &audit.Entry{}, nil
}
func (a *auditCapture) AppendGlobalChained(_ context.Context, _ audit.GlobalChainAppendParams) (*audit.Entry, error) {
	return nil, errors.New("not implemented")
}
func (a *auditCapture) ListForRun(_ context.Context, _ uuid.UUID) ([]*audit.Entry, error) {
	return nil, nil
}
func (a *auditCapture) ListForRunByCategory(_ context.Context, _ uuid.UUID, _ string) ([]*audit.Entry, error) {
	return nil, nil
}
func (a *auditCapture) ListGlobal(_ context.Context) ([]*audit.Entry, error) {
	return nil, nil
}
func (a *auditCapture) ListAll(_ context.Context, _ audit.ListAllParams) ([]*audit.Entry, error) {
	return nil, nil
}
func (a *auditCapture) Get(_ context.Context, _ uuid.UUID) (*audit.Entry, error) {
	return nil, errors.New("not implemented")
}
func (a *auditCapture) LastForRun(_ context.Context, _ uuid.UUID) (*audit.Entry, error) {
	return nil, errors.New("not implemented")
}

// mcpTokenServer wires the four repos the handler needs. Uses the
// existing signingFake (defined in trace_test.go) so the request-
// signing path is testable without a real Postgres backend.
func mcpTokenServer(t *testing.T, runID uuid.UUID) (*Server, *signingFake, *fakeMCPTokenRepo, *auditCapture) {
	t.Helper()
	sf := newSigningFake()
	mt := newFakeMCPTokenRepo()
	au := &auditCapture{}
	rr := newOrchestratorRepo()
	rr.runs[runID] = &run.Run{ID: runID, Repo: "x/y", State: run.StatePending}
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		SigningRepo:  sf,
		MCPTokenRepo: mt,
		AuditRepo:    au,
	})
	return s, sf, mt, au
}

// signedMCPTokenRequest builds a POST /v0/runs/{id}/mcp-token
// request signed with the run's private key. body may be empty;
// the signature is over signing.ComputeMessage(body) per the
// installation-token convention.
func signedMCPTokenRequest(t *testing.T, s *Server, runID uuid.UUID, priv ed25519.PrivateKey, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+runID.String()+"/mcp-token",
		bytes.NewReader(body))
	if priv != nil {
		sig := ed25519.Sign(priv, signing.ComputeMessage(body))
		req.Header.Set("X-Fishhawk-Signature", hex.EncodeToString(sig))
	}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func TestHandleIssueMCPToken_HappyPath(t *testing.T) {
	runID := uuid.New()
	s, sf, mt, _ := mcpTokenServer(t, runID)
	priv, _ := sf.issue(t, runID)

	resp := signedMCPTokenRequest(t, s, runID, priv, []byte{})
	if resp.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", resp.Code, resp.Body.String())
	}
	var body mcpTokenResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if !strings.HasPrefix(body.Token, mcptoken.TokenPrefix) {
		t.Errorf("token = %q, want prefix %q", body.Token, mcptoken.TokenPrefix)
	}
	if body.RunID != runID {
		t.Errorf("RunID = %s, want %s", body.RunID, runID)
	}
	if body.ExpiresAt.IsZero() {
		t.Error("ExpiresAt zero in response")
	}
	if len(mt.issued) != 1 {
		t.Fatalf("Issue calls = %d, want 1", len(mt.issued))
	}
	if got := mt.issued[0].RunID; got != runID {
		t.Errorf("Issue.RunID = %s, want %s", got, runID)
	}
	if got := mt.issued[0].TTL; got != mcptoken.DefaultTTL {
		t.Errorf("Issue.TTL = %v, want DefaultTTL %v", got, mcptoken.DefaultTTL)
	}
}

func TestHandleIssueMCPToken_WritesAuditRow(t *testing.T) {
	runID := uuid.New()
	s, sf, _, au := mcpTokenServer(t, runID)
	priv, _ := sf.issue(t, runID)

	resp := signedMCPTokenRequest(t, s, runID, priv, []byte{})
	if resp.Code != http.StatusCreated {
		t.Fatalf("status = %d; body = %s", resp.Code, resp.Body.String())
	}
	if len(au.appended) != 1 {
		t.Fatalf("audit appended = %d, want 1", len(au.appended))
	}
	row := au.appended[0]
	if row.Category != CategoryMCPTokenIssued {
		t.Errorf("audit category = %q", row.Category)
	}
	if row.RunID != runID {
		t.Errorf("audit run_id = %s, want %s", row.RunID, runID)
	}
}

func TestHandleIssueMCPToken_PlaintextNotInAuditPayload(t *testing.T) {
	runID := uuid.New()
	s, sf, mt, au := mcpTokenServer(t, runID)
	plaintext := mcptoken.TokenPrefix + "secret-plaintext-leaked-if-anywhere"
	mt.nextToken = func(p mcptoken.IssueParams) *mcptoken.Token {
		return &mcptoken.Token{
			ID:        uuid.New(),
			RunID:     p.RunID,
			IssuedAt:  time.Now().UTC(),
			ExpiresAt: time.Now().UTC().Add(time.Hour),
			PlainText: plaintext,
		}
	}
	priv, _ := sf.issue(t, runID)

	resp := signedMCPTokenRequest(t, s, runID, priv, []byte{})
	if resp.Code != http.StatusCreated {
		t.Fatalf("status = %d", resp.Code)
	}
	if len(au.appended) != 1 {
		t.Fatalf("audit appended = %d", len(au.appended))
	}
	if strings.Contains(string(au.appended[0].Payload), plaintext) {
		t.Errorf("audit payload leaked plaintext: %s", au.appended[0].Payload)
	}
}

func TestHandleIssueMCPToken_RejectsUnsignedRequest(t *testing.T) {
	runID := uuid.New()
	s, _, _, _ := mcpTokenServer(t, runID)

	resp := signedMCPTokenRequest(t, s, runID, nil, []byte{})
	if resp.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 on missing signature", resp.Code)
	}
}

func TestHandleIssueMCPToken_RejectsBadSignature(t *testing.T) {
	runID := uuid.New()
	s, sf, _, _ := mcpTokenServer(t, runID)
	// Issue a key for the run, then sign with a DIFFERENT random
	// key so Verify rejects.
	_, _ = sf.issue(t, runID)
	_, otherPriv, _ := ed25519.GenerateKey(nil)

	resp := signedMCPTokenRequest(t, s, runID, otherPriv, []byte{})
	if resp.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 on bad signature", resp.Code)
	}
}

func TestHandleIssueMCPToken_RejectsMissingSigningKey(t *testing.T) {
	// Run exists but no signing key was issued for it.
	runID := uuid.New()
	s, _, _, _ := mcpTokenServer(t, runID)
	_, priv, _ := ed25519.GenerateKey(nil)

	resp := signedMCPTokenRequest(t, s, runID, priv, []byte{})
	if resp.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 on missing signing key", resp.Code)
	}
}

func TestHandleIssueMCPToken_RejectsUnknownRun(t *testing.T) {
	// Server wired but the run id doesn't exist.
	runID := uuid.New()
	sf := newSigningFake()
	mt := newFakeMCPTokenRepo()
	au := &auditCapture{}
	rr := newOrchestratorRepo()
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		SigningRepo:  sf,
		MCPTokenRepo: mt,
		AuditRepo:    au,
	})

	_, priv, _ := ed25519.GenerateKey(nil)
	resp := signedMCPTokenRequest(t, s, runID, priv, []byte{})
	if resp.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.Code)
	}
	if len(mt.issued) != 0 {
		t.Errorf("Issue called despite unknown run")
	}
}

func TestHandleIssueMCPToken_RejectsBadUUID(t *testing.T) {
	runID := uuid.New()
	s, _, _, _ := mcpTokenServer(t, runID)

	req := httptest.NewRequest(http.MethodPost, "/v0/runs/not-a-uuid/mcp-token", http.NoBody)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

func TestHandleIssueMCPToken_503WhenMCPRepoMissing(t *testing.T) {
	s := New(Config{
		Addr:        "127.0.0.1:0",
		RunRepo:     newOrchestratorRepo(),
		SigningRepo: newSigningFake(),
	})

	resp := signedMCPTokenRequest(t, s, uuid.New(), nil, []byte{})
	if resp.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.Code)
	}
}

func TestHandleIssueMCPToken_AuditRepoMissing_StillIssues(t *testing.T) {
	// Best-effort audit: a missing AuditRepo doesn't unwind issuance.
	runID := uuid.New()
	sf := newSigningFake()
	mt := newFakeMCPTokenRepo()
	rr := newOrchestratorRepo()
	rr.runs[runID] = &run.Run{ID: runID, Repo: "x/y", State: run.StatePending}
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		SigningRepo:  sf,
		MCPTokenRepo: mt,
		// AuditRepo intentionally nil
	})
	priv, _ := sf.issue(t, runID)

	resp := signedMCPTokenRequest(t, s, runID, priv, []byte{})
	if resp.Code != http.StatusCreated {
		t.Errorf("status = %d, want 201 (audit-missing should not block)", resp.Code)
	}
	if len(mt.issued) != 1 {
		t.Errorf("Issue should still fire when AuditRepo is missing")
	}
}

// --- bearer-auth middleware tests (E19.8 routing) ---

// stubMCPAuthenticator implements mcptokenAuthenticator with a
// fixed return so the middleware test can assert routing without
// running the real Postgres-backed repo.
type stubMCPAuthenticator struct {
	token  *mcptoken.Token
	err    error
	called int
}

func (s *stubMCPAuthenticator) Authenticate(_ context.Context, _ string) (*mcptoken.Token, error) {
	s.called++
	if s.err != nil {
		return nil, s.err
	}
	return s.token, nil
}

func TestBearerAuth_MCPTokenRoutesToMCPAuthenticator(t *testing.T) {
	runID := uuid.New()
	mcpAuth := &stubMCPAuthenticator{
		token: &mcptoken.Token{
			ID:        uuid.New(),
			RunID:     runID,
			ExpiresAt: time.Now().UTC().Add(time.Hour),
			Scopes:    []string{"mcp:read"},
		},
	}

	var captured Identity
	h := newServer(t, newFakeRepo()).bearerAuth(nil, mcpAuth, nil)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured = IdentityFrom(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/v0/runs", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+mcptoken.TokenPrefix+"matchingplaintext")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if captured.Subject != "mcp:run:"+runID.String() {
		t.Errorf("Subject = %q, want mcp:run:%s", captured.Subject, runID)
	}
	if len(captured.Scopes) != 1 || captured.Scopes[0] != "mcp:read" {
		t.Errorf("Scopes = %v, want [mcp:read]", captured.Scopes)
	}
	if mcpAuth.called != 1 {
		t.Errorf("MCP authenticator called %d times; want 1", mcpAuth.called)
	}
}

func TestBearerAuth_APITokenSkipsMCPRoute(t *testing.T) {
	mcpAuth := &stubMCPAuthenticator{}
	h := newServer(t, newFakeRepo()).bearerAuth(nil, mcpAuth, nil)(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/v0/runs", http.NoBody)
	req.Header.Set("Authorization", "Bearer fhk_apitokenstylevaluethatishhopefullyenoughlong")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if mcpAuth.called != 0 {
		t.Errorf("MCP authenticator hit on fhk_ token; called %d times", mcpAuth.called)
	}
}
