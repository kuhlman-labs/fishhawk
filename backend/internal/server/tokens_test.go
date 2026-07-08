package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/identity"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
)

// fakeTokenRepo is the in-memory apitoken.Repository used by the
// handler tests. Issue / Authenticate / Revoke / List all behave
// like the Postgres impl; tests drive errors via the *Err fields.
type fakeTokenRepo struct {
	mu     sync.Mutex
	rows   map[uuid.UUID]*apitoken.Token
	byHash map[string]*apitoken.Token

	issueErr        error
	authenticateErr error
	listErr         error
	revokeErr       error
}

func newFakeTokenRepo() *fakeTokenRepo {
	return &fakeTokenRepo{
		rows:   map[uuid.UUID]*apitoken.Token{},
		byHash: map[string]*apitoken.Token{},
	}
}

func (f *fakeTokenRepo) Issue(_ context.Context, subject string, scopes []string) (*apitoken.Token, error) {
	if f.issueErr != nil {
		return nil, f.issueErr
	}
	if subject == "" {
		return nil, errors.New("subject required")
	}
	id := uuid.New()
	plain := apitoken.TokenPrefix + id.String()
	hash, _ := apitoken.HashPlaintext(plain)
	tok := &apitoken.Token{
		ID:        id,
		Subject:   subject,
		Scopes:    append([]string(nil), scopes...),
		CreatedAt: time.Now().UTC(),
		PlainText: plain,
	}
	stored := *tok
	stored.PlainText = ""
	f.mu.Lock()
	f.rows[id] = &stored
	f.byHash[hash] = &stored
	f.mu.Unlock()
	return tok, nil
}

// IssueOAuth makes fakeTokenRepo an apitoken.OAuthIssuer so the mint
// handler's type-assert passes. It mints like Issue but stamps
// auth_method='oauth' + the provider on both the returned token and the
// stored rows, mirroring the Postgres CreateOAuthToken path.
func (f *fakeTokenRepo) IssueOAuth(ctx context.Context, subject string, scopes []string, provider string) (*apitoken.Token, error) {
	if provider == "" {
		return nil, errors.New("provider required")
	}
	tok, err := f.Issue(ctx, subject, scopes)
	if err != nil {
		return nil, err
	}
	tok.AuthMethod = "oauth"
	tok.Provider = provider
	f.mu.Lock()
	for _, row := range []*apitoken.Token{f.rows[tok.ID]} {
		if row != nil {
			row.AuthMethod = "oauth"
			row.Provider = provider
		}
	}
	f.mu.Unlock()
	return tok, nil
}

func (f *fakeTokenRepo) Authenticate(_ context.Context, plaintext string) (*apitoken.Token, error) {
	if f.authenticateErr != nil {
		return nil, f.authenticateErr
	}
	hash, err := apitoken.HashPlaintext(plaintext)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	tok, ok := f.byHash[hash]
	if !ok || tok.IsRevoked() {
		return nil, apitoken.ErrNotFound
	}
	return tok, nil
}

func (f *fakeTokenRepo) ListForSubject(_ context.Context, subject string) ([]*apitoken.Token, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []*apitoken.Token{}
	for _, t := range f.rows {
		if t.Subject == subject && !t.IsRevoked() {
			out = append(out, t)
		}
	}
	return out, nil
}

func (f *fakeTokenRepo) Revoke(_ context.Context, id uuid.UUID, requesterSubject string) (*apitoken.Token, error) {
	if f.revokeErr != nil {
		return nil, f.revokeErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	tok, ok := f.rows[id]
	if !ok {
		return nil, apitoken.ErrNotFound
	}
	if tok.Subject != requesterSubject {
		return nil, apitoken.ErrForbidden
	}
	if tok.RevokedAt == nil {
		now := time.Now().UTC()
		tok.RevokedAt = &now
	}
	return tok, nil
}

func (f *fakeTokenRepo) GetByID(_ context.Context, id uuid.UUID) (*apitoken.Token, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	tok, ok := f.rows[id]
	if !ok {
		return nil, apitoken.ErrNotFound
	}
	return tok, nil
}

// newTokenServer wires a Server with the apitoken repo and pre-seeds
// a token belonging to "github:42". Returns the server, repo, and
// the bearer string for that user so tests can use it as auth.
func newTokenServer(t *testing.T) (*Server, *fakeTokenRepo, string) {
	t.Helper()
	repo := newFakeTokenRepo()
	s := New(Config{Addr: "127.0.0.1:0", APITokenRepo: repo})
	tok, err := repo.Issue(context.Background(), "github:42", []string{"runs:read"})
	if err != nil {
		t.Fatal(err)
	}
	return s, repo, tok.PlainText
}

func tokenRequest(t *testing.T, s *Server, method, path, bearer string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var bodyReader *strings.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		bodyReader = strings.NewReader(string(raw))
	}
	var req *http.Request
	if bodyReader != nil {
		req = httptest.NewRequest(method, path, bodyReader)
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

func TestCreateToken_HappyPath(t *testing.T) {
	s, _, bearer := newTokenServer(t)

	w := tokenRequest(t, s, http.MethodPost, "/v0/tokens", bearer,
		map[string]any{"scopes": []string{"runs:read", "runs:write"}})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}

	var resp apiTokenCreatedResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(resp.Token, apitoken.TokenPrefix) {
		t.Errorf("token plaintext missing prefix: %q", resp.Token)
	}
	if len(resp.Scopes) != 2 {
		t.Errorf("Scopes = %v, want 2", resp.Scopes)
	}
}

func TestCreateToken_AnonymousRejected(t *testing.T) {
	s, _, _ := newTokenServer(t)
	w := tokenRequest(t, s, http.MethodPost, "/v0/tokens", "", map[string]any{"scopes": []string{}})
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestCreateToken_BadJSON(t *testing.T) {
	s, _, bearer := newTokenServer(t)
	req := httptest.NewRequest(http.MethodPost, "/v0/tokens",
		strings.NewReader("{not json"))
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestCreateToken_RepoError(t *testing.T) {
	s, repo, bearer := newTokenServer(t)
	repo.issueErr = errors.New("disk full")
	w := tokenRequest(t, s, http.MethodPost, "/v0/tokens", bearer, map[string]any{"scopes": []string{}})
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestListTokens_HappyPath(t *testing.T) {
	s, repo, bearer := newTokenServer(t)
	// Seed a second token for the same user.
	if _, err := repo.Issue(context.Background(), "github:42", []string{"runs:write"}); err != nil {
		t.Fatal(err)
	}
	// And one for a different user that should NOT appear.
	if _, err := repo.Issue(context.Background(), "github:99", []string{}); err != nil {
		t.Fatal(err)
	}

	w := tokenRequest(t, s, http.MethodGet, "/v0/tokens", bearer, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp struct {
		Items []apiTokenResponse `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Items) != 2 {
		t.Errorf("got %d items, want 2 (other user's token must not leak)", len(resp.Items))
	}
}

func TestListTokens_AnonymousRejected(t *testing.T) {
	s, _, _ := newTokenServer(t)
	w := tokenRequest(t, s, http.MethodGet, "/v0/tokens", "", nil)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestRevokeToken_HappyPath(t *testing.T) {
	s, repo, bearer := newTokenServer(t)
	tok, _ := repo.Issue(context.Background(), "github:42", nil)

	w := tokenRequest(t, s, http.MethodDelete, fmt.Sprintf("/v0/tokens/%s", tok.ID), bearer, nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204:\n%s", w.Code, w.Body.String())
	}
	got, _ := repo.GetByID(context.Background(), tok.ID)
	if !got.IsRevoked() {
		t.Errorf("token not marked revoked")
	}
}

func TestRevokeToken_OtherUsersToken_403(t *testing.T) {
	s, repo, bearer := newTokenServer(t)
	others, _ := repo.Issue(context.Background(), "github:99", nil)

	w := tokenRequest(t, s, http.MethodDelete, fmt.Sprintf("/v0/tokens/%s", others.ID), bearer, nil)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

func TestRevokeToken_NotFound_404(t *testing.T) {
	s, _, bearer := newTokenServer(t)
	w := tokenRequest(t, s, http.MethodDelete, fmt.Sprintf("/v0/tokens/%s", uuid.New()), bearer, nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestRevokeToken_BadUUID_400(t *testing.T) {
	s, _, bearer := newTokenServer(t)
	w := tokenRequest(t, s, http.MethodDelete, "/v0/tokens/not-a-uuid", bearer, nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestRevokeToken_AnonymousRejected(t *testing.T) {
	s, repo, _ := newTokenServer(t)
	tok, _ := repo.Issue(context.Background(), "github:42", nil)
	w := tokenRequest(t, s, http.MethodDelete, fmt.Sprintf("/v0/tokens/%s", tok.ID), "", nil)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestTokens_Unconfigured_503(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"}) // no APITokenRepo

	for _, route := range []struct {
		method, path string
	}{
		{http.MethodPost, "/v0/tokens"},
		{http.MethodGet, "/v0/tokens"},
		{http.MethodDelete, fmt.Sprintf("/v0/tokens/%s", uuid.New())},
	} {
		t.Run(route.method, func(t *testing.T) {
			w := tokenRequest(t, s, route.method, route.path, "", map[string]any{})
			if w.Code != http.StatusServiceUnavailable {
				t.Errorf("status = %d, want 503", w.Code)
			}
		})
	}
}

// -------- bearerAuth middleware tests --------

func TestBearerAuth_ValidToken_ResolvesIdentity(t *testing.T) {
	repo := newFakeTokenRepo()
	tok, _ := repo.Issue(context.Background(), "github:42", []string{"runs:read"})

	var captured Identity
	h := newServer(t, newFakeRepo()).bearerAuth(repo, nil, nil)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured = IdentityFrom(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+tok.PlainText)
	h.ServeHTTP(httptest.NewRecorder(), req)

	if captured.IsAnonymous() {
		t.Errorf("expected non-anonymous identity, got %+v", captured)
	}
	if captured.Subject != "github:42" {
		t.Errorf("Subject = %q", captured.Subject)
	}
	if captured.TokenID != tok.ID.String() {
		t.Errorf("TokenID = %q", captured.TokenID)
	}
	if len(captured.Scopes) != 1 || captured.Scopes[0] != "runs:read" {
		t.Errorf("Scopes = %v", captured.Scopes)
	}
}

func TestBearerAuth_InvalidToken_FallsBackToAnonymous(t *testing.T) {
	repo := newFakeTokenRepo()
	var captured Identity
	h := newServer(t, newFakeRepo()).bearerAuth(repo, nil, nil)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured = IdentityFrom(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer fhk_unknown_token_xxx_yyyy_zzzzz")
	h.ServeHTTP(httptest.NewRecorder(), req)

	// Middleware doesn't 401 on invalid bearer; per-handler logic
	// decides whether anonymous is acceptable.
	if !captured.IsAnonymous() {
		t.Errorf("expected anonymous fallback, got %+v", captured)
	}
}

func TestBearerAuth_NonBearerScheme_Anonymous(t *testing.T) {
	repo := newFakeTokenRepo()
	var captured Identity
	h := newServer(t, newFakeRepo()).bearerAuth(repo, nil, nil)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured = IdentityFrom(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if !captured.IsAnonymous() {
		t.Errorf("Basic-auth header should produce anonymous, got %+v", captured)
	}
}

func TestBearerAuth_NilRepo_Anonymous(t *testing.T) {
	var captured Identity
	h := newServer(t, newFakeRepo()).bearerAuth(nil, nil, nil)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured = IdentityFrom(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer fhk_xxx")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if !captured.IsAnonymous() {
		t.Errorf("nil repo should produce anonymous, got %+v", captured)
	}
}

// -------- token-login (OAuth device-flow mint) tests --------

// fakeIdentityProvider stubs identity.IdentityProvider for the
// token-login handler tests. VerifyAccessToken maps any token to a fixed
// subject (or returns verifyErr); PermissionLevel returns a fixed tier
// (or permErr). ResolveMembership/VerifyUser are unused here.
type fakeIdentityProvider struct {
	subject   string
	verifyErr error
	perm      identity.Permission
	permErr   error
}

func (f *fakeIdentityProvider) VerifyUser(context.Context, identity.DeviceCodePrompt) (string, error) {
	return f.subject, f.verifyErr
}

func (f *fakeIdentityProvider) VerifyAccessToken(context.Context, string) (string, error) {
	if f.verifyErr != nil {
		return "", f.verifyErr
	}
	return f.subject, nil
}

func (f *fakeIdentityProvider) PermissionLevel(context.Context, string, string) (identity.Permission, error) {
	if f.permErr != nil {
		return identity.PermissionNone, f.permErr
	}
	return f.perm, nil
}

func (f *fakeIdentityProvider) ResolveMembership(context.Context, string, string) (bool, error) {
	return false, nil
}

// repoWithoutOAuth wraps a Repository but promotes only the base
// Repository method set (embedding the interface), so it deliberately
// does NOT satisfy apitoken.OAuthIssuer — driving the mint handler's
// "repo cannot mint OAuth" 503 branch.
type repoWithoutOAuth struct{ apitoken.Repository }

// loginConfig is the fully-configured token-login Config; individual
// per-failure-mode tests mutate one field via the mut callback.
func loginConfig(repo apitoken.Repository, idp identity.IdentityProvider) Config {
	return Config{
		Addr:                  "127.0.0.1:0",
		APITokenRepo:          repo,
		IdentityProvider:      idp,
		OAuthClientID:         "Iv1.testclient",
		OperatorRepo:          "kuhlman-labs/fishhawk",
		OperatorMinPermission: identity.PermissionWrite,
		OperatorDefaultScopes: []string{"read:runs", "write:runs", "write:approvals"},
	}
}

// newFakeLoginServer builds a token-login Server over the in-memory fake
// repo (an OAuthIssuer) + a fake identity provider, for the fast
// per-failure-mode branch tests. mut optionally tweaks the Config.
func newFakeLoginServer(t *testing.T, idp identity.IdentityProvider, mut func(*Config)) *Server {
	t.Helper()
	cfg := loginConfig(newFakeTokenRepo(), idp)
	if mut != nil {
		mut(&cfg)
	}
	return New(cfg)
}

func TestTokenLoginDiscovery_HappyPath(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0", OAuthClientID: "Iv1.abc123"})
	w := tokenRequest(t, s, http.MethodGet, "/v0/tokens/login", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp tokenLoginDiscoveryResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ClientID != "Iv1.abc123" {
		t.Errorf("client_id = %q, want Iv1.abc123", resp.ClientID)
	}
	if resp.Provider != "github" {
		t.Errorf("provider = %q, want github", resp.Provider)
	}
}

func TestTokenLoginDiscovery_Unconfigured_503(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"}) // no OAuthClientID
	w := tokenRequest(t, s, http.MethodGet, "/v0/tokens/login", "", nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

// TestTokenLoginMint_PersistsOAuthRow is the cross-boundary mint test
// (binding approval condition 2): a REAL apitoken repository backed by
// pgtest, faking ONLY the IdentityProvider. POST /v0/tokens/login is
// driven through VerifyToken to actual Postgres persistence, asserting
// the PERSISTED row's auth_method='oauth' + provider='github' and the GET
// /v0/tokens projection.
func TestTokenLoginMint_PersistsOAuthRow(t *testing.T) {
	url := pgtest.NewURL(t)
	if err := postgres.MigrateUp(url); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	repo := apitoken.NewPostgresRepository(pool)

	idp := &fakeIdentityProvider{subject: "github:octocat", perm: identity.PermissionAdmin}
	s := New(loginConfig(repo, idp))

	w := tokenRequest(t, s, http.MethodPost, "/v0/tokens/login", "",
		map[string]any{"access_token": "gho_cli", "scopes": []string{"read:runs", "write:runs"}})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	var created apiTokenCreatedResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.AuthMethod != "oauth" || created.Provider != "github" {
		t.Errorf("created projection auth_method/provider = %q/%q, want oauth/github",
			created.AuthMethod, created.Provider)
	}

	// The PERSISTED row (handler → apitoken → Postgres) carries the OAuth
	// provenance. Re-read via Authenticate on the returned plaintext.
	persisted, err := repo.Authenticate(context.Background(), created.Token)
	if err != nil {
		t.Fatalf("Authenticate minted token: %v", err)
	}
	if persisted.AuthMethod != "oauth" {
		t.Errorf("persisted auth_method = %q, want oauth", persisted.AuthMethod)
	}
	if persisted.Provider != "github" {
		t.Errorf("persisted provider = %q, want github", persisted.Provider)
	}
	if persisted.Subject != "github:octocat" {
		t.Errorf("persisted subject = %q, want github:octocat", persisted.Subject)
	}

	// GET /v0/tokens, authenticated as the minted subject via the new
	// token, projects the same OAuth provenance.
	lw := tokenRequest(t, s, http.MethodGet, "/v0/tokens", created.Token, nil)
	if lw.Code != http.StatusOK {
		t.Fatalf("GET /v0/tokens status = %d, want 200:\n%s", lw.Code, lw.Body.String())
	}
	var list struct {
		Items []apiTokenResponse `json:"items"`
	}
	if err := json.Unmarshal(lw.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("GET /v0/tokens items = %d, want 1", len(list.Items))
	}
	if list.Items[0].AuthMethod != "oauth" || list.Items[0].Provider != "github" {
		t.Errorf("projection auth_method/provider = %q/%q, want oauth/github",
			list.Items[0].AuthMethod, list.Items[0].Provider)
	}
}

func TestTokenLoginMint_APITokenRepoNil_503(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0",
		IdentityProvider: &fakeIdentityProvider{subject: "github:octocat", perm: identity.PermissionAdmin},
		OAuthClientID:    "Iv1.x", OperatorRepo: "o/r"})
	w := tokenRequest(t, s, http.MethodPost, "/v0/tokens/login", "",
		map[string]any{"access_token": "gho_cli"})
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestTokenLoginMint_RepoNotOAuthIssuer_503(t *testing.T) {
	s := newFakeLoginServer(t, &fakeIdentityProvider{subject: "github:octocat", perm: identity.PermissionAdmin},
		func(c *Config) { c.APITokenRepo = repoWithoutOAuth{newFakeTokenRepo()} })
	w := tokenRequest(t, s, http.MethodPost, "/v0/tokens/login", "",
		map[string]any{"access_token": "gho_cli"})
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestTokenLoginMint_OperatorRepoUnconfigured_503(t *testing.T) {
	s := newFakeLoginServer(t, &fakeIdentityProvider{subject: "github:octocat", perm: identity.PermissionAdmin},
		func(c *Config) { c.OperatorRepo = "" })
	w := tokenRequest(t, s, http.MethodPost, "/v0/tokens/login", "",
		map[string]any{"access_token": "gho_cli"})
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestTokenLoginMint_IdentityUnconfigured_503(t *testing.T) {
	// A NoOp identity provider (nil → defaulted in New) returns
	// ErrNotConfigured from VerifyAccessToken → 503 tokens_unconfigured.
	s := newFakeLoginServer(t, identity.NewNoOp(), nil)
	w := tokenRequest(t, s, http.MethodPost, "/v0/tokens/login", "",
		map[string]any{"access_token": "gho_cli"})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "tokens_unconfigured") {
		t.Errorf("body missing tokens_unconfigured:\n%s", w.Body.String())
	}
}

func TestTokenLoginMint_BadJSON_400(t *testing.T) {
	s := newFakeLoginServer(t, &fakeIdentityProvider{subject: "github:octocat", perm: identity.PermissionAdmin}, nil)
	req := httptest.NewRequest(http.MethodPost, "/v0/tokens/login", strings.NewReader("{not json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestTokenLoginMint_EmptyAccessToken_400(t *testing.T) {
	s := newFakeLoginServer(t, &fakeIdentityProvider{subject: "github:octocat", perm: identity.PermissionAdmin}, nil)
	w := tokenRequest(t, s, http.MethodPost, "/v0/tokens/login", "",
		map[string]any{"scopes": []string{"read:runs"}})
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestTokenLoginMint_VerificationFails_401(t *testing.T) {
	s := newFakeLoginServer(t, &fakeIdentityProvider{verifyErr: errors.New("bad token")}, nil)
	w := tokenRequest(t, s, http.MethodPost, "/v0/tokens/login", "",
		map[string]any{"access_token": "gho_bad"})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "token_verification_failed") {
		t.Errorf("body missing token_verification_failed:\n%s", w.Body.String())
	}
}

func TestTokenLoginMint_PermissionBelowMinimum_403(t *testing.T) {
	// Subject holds only read; the minimum is write → 403.
	s := newFakeLoginServer(t, &fakeIdentityProvider{subject: "github:octocat", perm: identity.PermissionRead}, nil)
	w := tokenRequest(t, s, http.MethodPost, "/v0/tokens/login", "",
		map[string]any{"access_token": "gho_cli"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "insufficient_permission") {
		t.Errorf("body missing insufficient_permission:\n%s", w.Body.String())
	}
}

func TestTokenLoginMint_ScopeOutsideDefault_403(t *testing.T) {
	s := newFakeLoginServer(t, &fakeIdentityProvider{subject: "github:octocat", perm: identity.PermissionAdmin}, nil)
	w := tokenRequest(t, s, http.MethodPost, "/v0/tokens/login", "",
		map[string]any{"access_token": "gho_cli", "scopes": []string{"read:runs", "admin:everything"}})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "scope_not_allowed") {
		t.Errorf("body missing scope_not_allowed:\n%s", w.Body.String())
	}
}

func TestTokenLoginMint_IssueError_500(t *testing.T) {
	repo := newFakeTokenRepo()
	repo.issueErr = errors.New("disk full")
	cfg := loginConfig(repo, &fakeIdentityProvider{subject: "github:octocat", perm: identity.PermissionAdmin})
	s := New(cfg)
	w := tokenRequest(t, s, http.MethodPost, "/v0/tokens/login", "",
		map[string]any{"access_token": "gho_cli"})
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestTokenLoginMint_PermissionCheckError_500(t *testing.T) {
	s := newFakeLoginServer(t, &fakeIdentityProvider{subject: "github:octocat", permErr: errors.New("rate limited")}, nil)
	w := tokenRequest(t, s, http.MethodPost, "/v0/tokens/login", "",
		map[string]any{"access_token": "gho_cli"})
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestIdentity_IsAnonymous(t *testing.T) {
	cases := []struct {
		id   Identity
		want bool
	}{
		{Identity{}, true},
		{Identity{Subject: "anonymous"}, true},
		{Identity{Subject: "github:42"}, false},
	}
	for _, c := range cases {
		if got := c.id.IsAnonymous(); got != c.want {
			t.Errorf("IsAnonymous(%+v) = %v, want %v", c.id, got, c.want)
		}
	}
}
