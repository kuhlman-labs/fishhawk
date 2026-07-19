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
	byPlain   map[string]*mcptoken.Token
	issueErr  error
	nextToken func(p mcptoken.IssueParams) *mcptoken.Token
}

func newFakeMCPTokenRepo() *fakeMCPTokenRepo {
	return &fakeMCPTokenRepo{
		tokens:  map[uuid.UUID]*mcptoken.Token{},
		byPlain: map[string]*mcptoken.Token{},
	}
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
		id := uuid.New()
		tok = &mcptoken.Token{
			ID:        id,
			RunID:     p.RunID,
			IssuedAt:  time.Now().UTC(),
			ExpiresAt: time.Now().UTC().Add(ttl),
			Scopes:    append([]string(nil), p.Scopes...),
			PlainText: mcptoken.TokenPrefix + "stub-plaintext-" + id.String(),
		}
	}
	f.tokens[tok.ID] = tok
	if tok.PlainText != "" {
		f.byPlain[tok.PlainText] = tok
	}
	return tok, nil
}

func (f *fakeMCPTokenRepo) Authenticate(_ context.Context, plaintext string) (*mcptoken.Token, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if tok, ok := f.byPlain[plaintext]; ok {
		return tok, nil
	}
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

	// Register the token's run (untenanted) so the mcp:run account lookup
	// returns ("", nil): bearerAuth fails closed on ANY GetRunAccountID error
	// (incl. ErrNotFound), and an mcp:run token is always run-bound in
	// production, so a registered run is the realistic fixture for routing.
	fr := newFakeRepo()
	fr.runs[runID] = &run.Run{ID: runID}

	var captured Identity
	h := newServer(t, fr).bearerAuth(nil, mcpAuth, nil)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
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

// --- write:scope-amendments grant (E22.X / #961) ---

// scopeAmendmentTokenServer is mcpTokenServer plus a seeded executing
// stage of the given type and a scope-amendment repo, exercising the
// NON-self-retry implement path: the run carries no workflow spec, so
// resolveAgentSelfRetry contributes nothing and the grant must come
// from the unconditional implement-stage branch alone.
func scopeAmendmentTokenServer(t *testing.T, stageType run.StageType) (*Server, *signingFake, *fakeMCPTokenRepo, *orchestratorRepo, *run.Run, *run.Stage) {
	t.Helper()
	sf := newSigningFake()
	mt := newFakeMCPTokenRepo()
	rr := newOrchestratorRepo()
	runRow := rr.seedRun()
	stage := rr.seedStage(runRow.ID, 1, run.StageStateRunning)
	stage.Type = stageType
	s := New(Config{
		Addr:               "127.0.0.1:0",
		RunRepo:            rr,
		SigningRepo:        sf,
		MCPTokenRepo:       mt,
		AuditRepo:          &auditCapture{},
		ScopeAmendmentRepo: newFakeScopeAmendmentRepo(),
	})
	return s, sf, mt, rr, runRow, stage
}

func TestHandleIssueMCPToken_ImplementStageGrantsScopeAmendments(t *testing.T) {
	s, sf, mt, _, runRow, _ := scopeAmendmentTokenServer(t, run.StageTypeImplement)
	priv, _ := sf.issue(t, runRow.ID)

	resp := signedMCPTokenRequest(t, s, runRow.ID, priv, []byte{})
	if resp.Code != http.StatusCreated {
		t.Fatalf("status = %d; body = %s", resp.Code, resp.Body.String())
	}
	if len(mt.issued) != 1 {
		t.Fatalf("Issue calls = %d, want 1", len(mt.issued))
	}
	scopes := mt.issued[0].Scopes
	want := map[string]bool{"mcp:read": false, "write:scope-amendments": false}
	for _, sc := range scopes {
		if sc == "write:retries" {
			t.Errorf("non-self-retry implement token granted write:retries: %v", scopes)
		}
		if _, ok := want[sc]; ok {
			want[sc] = true
		}
	}
	for sc, got := range want {
		if !got {
			t.Errorf("implement-stage token missing scope %q: %v", sc, scopes)
		}
	}
}

func TestHandleIssueMCPToken_PlanStageOmitsScopeAmendments(t *testing.T) {
	s, sf, mt, _, runRow, _ := scopeAmendmentTokenServer(t, run.StageTypePlan)
	priv, _ := sf.issue(t, runRow.ID)

	resp := signedMCPTokenRequest(t, s, runRow.ID, priv, []byte{})
	if resp.Code != http.StatusCreated {
		t.Fatalf("status = %d; body = %s", resp.Code, resp.Body.String())
	}
	for _, sc := range mt.issued[0].Scopes {
		if sc == "write:scope-amendments" {
			t.Errorf("plan-stage token granted write:scope-amendments: %v", mt.issued[0].Scopes)
		}
	}
}

// --- #1030: local-runner first-stage fallback ---

// stageSeed is one stage to seed for the fallback contract tests;
// slice order is sequence order.
type stageSeed struct {
	typ   run.StageType
	state run.StageState
}

// scopeAmendmentTokenServerSeeded is scopeAmendmentTokenServer with
// caller-controlled stage shapes, for the #1030 fallback contracts:
// local-runner stages stay `pending` for their whole execution, so
// the grant must resolve runs with NO dispatched/running stage.
func scopeAmendmentTokenServerSeeded(t *testing.T, seeds ...stageSeed) (*Server, *signingFake, *fakeMCPTokenRepo, *run.Run) {
	t.Helper()
	sf := newSigningFake()
	mt := newFakeMCPTokenRepo()
	rr := newOrchestratorRepo()
	runRow := rr.seedRun()
	for i, seed := range seeds {
		st := rr.seedStage(runRow.ID, i+1, seed.state)
		st.Type = seed.typ
	}
	s := New(Config{
		Addr:               "127.0.0.1:0",
		RunRepo:            rr,
		SigningRepo:        sf,
		MCPTokenRepo:       mt,
		AuditRepo:          &auditCapture{},
		ScopeAmendmentRepo: newFakeScopeAmendmentRepo(),
	})
	return s, sf, mt, runRow
}

func issuedScopes(t *testing.T, s *Server, sf *signingFake, mt *fakeMCPTokenRepo, runRow *run.Run) []string {
	t.Helper()
	priv, _ := sf.issue(t, runRow.ID)
	resp := signedMCPTokenRequest(t, s, runRow.ID, priv, []byte{})
	if resp.Code != http.StatusCreated {
		t.Fatalf("issue status = %d; body = %s", resp.Code, resp.Body.String())
	}
	if len(mt.issued) != 1 {
		t.Fatalf("Issue calls = %d, want 1", len(mt.issued))
	}
	return mt.issued[0].Scopes
}

func TestHandleIssueMCPToken_PendingImplementOnlyGrantsScopeAmendments(t *testing.T) {
	// Decomposition-child shape (#1030, run 8b0282a2): the run's ONLY
	// stage is a still-pending implement stage. Fails on the pre-fix
	// dispatched/running-only gate.
	s, sf, mt, runRow := scopeAmendmentTokenServerSeeded(t,
		stageSeed{run.StageTypeImplement, run.StageStatePending})

	scopes := issuedScopes(t, s, sf, mt, runRow)
	found := false
	for _, sc := range scopes {
		if sc == "write:scope-amendments" {
			found = true
		}
	}
	if !found {
		t.Errorf("pending-implement child token missing write:scope-amendments: %v", scopes)
	}
}

func TestHandleIssueMCPToken_PlanFirstOmitsScopeAmendments(t *testing.T) {
	// The fallback stops at the FIRST non-terminal stage: a plan stage
	// ahead of the pending implement stage keeps the scope off the
	// token — pending AND awaiting_approval (non-terminal, must not be
	// skipped) both block.
	for _, planState := range []run.StageState{run.StageStatePending, run.StageStateAwaitingApproval} {
		t.Run(string(planState), func(t *testing.T) {
			s, sf, mt, runRow := scopeAmendmentTokenServerSeeded(t,
				stageSeed{run.StageTypePlan, planState},
				stageSeed{run.StageTypeImplement, run.StageStatePending})

			for _, sc := range issuedScopes(t, s, sf, mt, runRow) {
				if sc == "write:scope-amendments" {
					t.Errorf("plan-first token granted write:scope-amendments")
				}
			}
		})
	}
}

func TestHandleIssueMCPToken_SucceededPlanPendingImplementGrantsScopeAmendments(t *testing.T) {
	// Post-approval local gap: plan succeeded, implement still pending
	// (no orchestrator dispatch under a local runner) — terminal
	// stages are skipped and the implement stage resolves.
	s, sf, mt, runRow := scopeAmendmentTokenServerSeeded(t,
		stageSeed{run.StageTypePlan, run.StageStateSucceeded},
		stageSeed{run.StageTypeImplement, run.StageStatePending})

	scopes := issuedScopes(t, s, sf, mt, runRow)
	found := false
	for _, sc := range scopes {
		if sc == "write:scope-amendments" {
			found = true
		}
	}
	if !found {
		t.Errorf("succeeded-plan/pending-implement token missing write:scope-amendments: %v", scopes)
	}
}

// --- #1030 fallback × agent_self_retry (direct pins, PR #1032 concern 4f13d8b2) ---

func TestHandleIssueMCPToken_PendingImplementSelfRetryGrantsWriteRetries(t *testing.T) {
	// Decomposition-child shape: the run's ONLY stage is a still-
	// pending implement stage whose spec stage opts into
	// executor.agent_self_retry. The #1030 fallback must resolve the
	// pending stage so the token carries write:retries. Workflow key
	// matches seedRun's WorkflowID ("w"); resolveAgentSelfRetry looks
	// the spec stage up by sequence, so the implement-only spec mirrors
	// the implement-only stage row.
	s, sf, mt, runRow := scopeAmendmentTokenServerSeeded(t,
		stageSeed{run.StageTypeImplement, run.StageStatePending})
	runRow.WorkflowSpec = []byte(`
version: "0.3"
workflows:
  w:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
          agent_self_retry: true
        produces:
          - artifact: pull_request
`)

	scopes := issuedScopes(t, s, sf, mt, runRow)
	found := false
	for _, sc := range scopes {
		if sc == "write:retries" {
			found = true
		}
	}
	if !found {
		t.Errorf("pending-implement self-retry child token missing write:retries: %v", scopes)
	}
}

func TestHandleIssueMCPToken_PlanFirstSelfRetryOmitsWriteRetries(t *testing.T) {
	// Plan-first run with the implement stage opted into
	// agent_self_retry: the fallback stops at the FIRST non-terminal
	// stage (the plan stage), which does not opt in, so write:retries
	// must stay off the token.
	s, sf, mt, runRow := scopeAmendmentTokenServerSeeded(t,
		stageSeed{run.StageTypePlan, run.StageStatePending},
		stageSeed{run.StageTypeImplement, run.StageStatePending})
	runRow.WorkflowSpec = []byte(`
version: "0.3"
workflows:
  w:
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
          agent_self_retry: true
        produces:
          - artifact: pull_request
`)

	for _, sc := range issuedScopes(t, s, sf, mt, runRow) {
		if sc == "write:retries" {
			t.Errorf("plan-first token granted write:retries")
		}
	}
}

// TestHandleIssueMCPToken_TokenCanReadOwnRunAmendments is the
// end-to-end auth check the #961 plan names: a freshly issued
// implement-stage agent token GETs its own run's scope amendments
// (200) through the full bearer-auth middleware, and gets 403 on
// another run's.
func TestHandleIssueMCPToken_TokenCanReadOwnRunAmendments(t *testing.T) {
	s, sf, _, rr, runRow, _ := scopeAmendmentTokenServer(t, run.StageTypeImplement)
	priv, _ := sf.issue(t, runRow.ID)

	resp := signedMCPTokenRequest(t, s, runRow.ID, priv, []byte{})
	if resp.Code != http.StatusCreated {
		t.Fatalf("issue status = %d; body = %s", resp.Code, resp.Body.String())
	}
	var issued mcpTokenResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &issued); err != nil {
		t.Fatal(err)
	}

	// Own run → 200 through the real middleware + handler chain.
	req := httptest.NewRequest(http.MethodGet,
		"/v0/runs/"+runRow.ID.String()+"/scope-amendments", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+issued.Token)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("own-run GET status = %d; body = %s", w.Code, w.Body.String())
	}

	// Another run → 403.
	otherRun := rr.seedRun()
	req = httptest.NewRequest(http.MethodGet,
		"/v0/runs/"+otherRun.ID.String()+"/scope-amendments", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+issued.Token)
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-run GET status = %d, want 403; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "cross_run_scope_amendment") {
		t.Errorf("body: %s", w.Body.String())
	}
}
