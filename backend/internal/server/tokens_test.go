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

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
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
	h := bearerAuth(repo)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
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
	h := bearerAuth(repo)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
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
	h := bearerAuth(repo)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
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
	h := bearerAuth(nil)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured = IdentityFrom(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer fhk_xxx")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if !captured.IsAnonymous() {
		t.Errorf("nil repo should produce anonymous, got %+v", captured)
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
