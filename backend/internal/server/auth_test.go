package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/auth"
)

// fakeAuthRepo is the in-memory auth.Repository for handler tests.
type fakeAuthRepo struct {
	mu       sync.Mutex
	users    map[string]*auth.User
	sessions map[string]*auth.Session
	byHash   map[string]*auth.Session

	signInErr       error
	authenticateErr error
	revokeErr       error
}

func newFakeAuthRepo() *fakeAuthRepo {
	return &fakeAuthRepo{
		users:    map[string]*auth.User{},
		sessions: map[string]*auth.Session{},
		byHash:   map[string]*auth.Session{},
	}
}

func (f *fakeAuthRepo) SignIn(_ context.Context, p auth.GitHubProfile) (*auth.User, *auth.Session, error) {
	if f.signInErr != nil {
		return nil, nil, f.signInErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	now := time.Now().UTC()
	user := &auth.User{
		ID:           uuid.New().String(),
		GitHubUserID: p.ID,
		GitHubLogin:  p.Login,
		Name:         p.Name,
		Email:        p.Email,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	f.users[user.ID] = user

	plain := auth.SessionTokenPrefix + uuid.New().String() + uuid.New().String()
	hash, _ := auth.HashPlaintext(plain)
	sess := &auth.Session{
		ID:                uuid.New().String(),
		UserID:            user.ID,
		IssuedAt:          now,
		LastUsedAt:        now,
		SlidingExpiresAt:  now.Add(auth.SessionSlidingTTL),
		AbsoluteExpiresAt: now.Add(auth.SessionAbsoluteTTL),
		PlainText:         plain,
	}
	stored := *sess
	stored.PlainText = ""
	f.sessions[sess.ID] = &stored
	f.byHash[hash] = &stored
	return user, sess, nil
}

func (f *fakeAuthRepo) Authenticate(_ context.Context, plaintext string) (*auth.User, *auth.Session, error) {
	if f.authenticateErr != nil {
		return nil, nil, f.authenticateErr
	}
	hash, err := auth.HashPlaintext(plaintext)
	if err != nil {
		return nil, nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	sess, ok := f.byHash[hash]
	if !ok {
		return nil, nil, auth.ErrSessionNotFound
	}
	if sess.IsExpired(time.Now().UTC()) {
		return nil, nil, auth.ErrSessionNotFound
	}
	user := f.users[sess.UserID]
	return user, sess, nil
}

func (f *fakeAuthRepo) Revoke(_ context.Context, id uuid.UUID) error {
	if f.revokeErr != nil {
		return f.revokeErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if sess, ok := f.sessions[id.String()]; ok {
		now := time.Now().UTC()
		sess.RevokedAt = &now
	}
	return nil
}

func (f *fakeAuthRepo) GetUser(_ context.Context, id uuid.UUID) (*auth.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u, ok := f.users[id.String()]; ok {
		return u, nil
	}
	return nil, auth.ErrSessionNotFound
}

func (f *fakeAuthRepo) EvictExpired(_ context.Context, _ int64) (int64, error) {
	return 0, nil
}

// stubGitHubOAuthServer mounts httptest endpoints the GitHubOAuth
// client points at, so the callback test exercises the full
// exchange + profile fetch.
func stubGitHubOAuthServer(t *testing.T) (*httptest.Server, *auth.GitHubOAuth) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"gho_xxx"}`))
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		body, _ := json.Marshal(map[string]any{
			"id":    int64(42),
			"login": "octocat",
			"name":  "The Octo Cat",
			"email": "octo@example.com",
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	gh := auth.NewGitHubOAuth("client-id", "secret", "https://example.com/cb",
		auth.OAuthURLs{
			AuthorizeURL: srv.URL + "/login/oauth/authorize",
			TokenURL:     srv.URL + "/login/oauth/access_token",
			UserURL:      srv.URL + "/user",
		})
	return srv, gh
}

func newAuthServer(t *testing.T) (*Server, *fakeAuthRepo) {
	t.Helper()
	repo := newFakeAuthRepo()
	_, gh := stubGitHubOAuthServer(t)
	s := New(Config{
		Addr:                   "127.0.0.1:0",
		AuthRepo:               repo,
		GitHubOAuth:            gh,
		AuthRedirectAfterLogin: "/app",
	})
	return s, repo
}

func TestGitHubLogin_RedirectsAndSetsStateCookie(t *testing.T) {
	s, _ := newAuthServer(t)
	req := httptest.NewRequest(http.MethodGet, "/v0/auth/github/login", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", w.Code)
	}
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "client_id=") || !strings.Contains(loc, "state=") {
		t.Errorf("Location missing OAuth params: %q", loc)
	}
	cookies := w.Result().Cookies()
	var state *http.Cookie
	for _, c := range cookies {
		if c.Name == auth.StateCookieName {
			state = c
		}
	}
	if state == nil {
		t.Fatal("state cookie not set")
	}
	if !state.HttpOnly || !state.Secure {
		t.Errorf("state cookie missing HttpOnly/Secure: %+v", state)
	}
}

func TestGitHubCallback_HappyPath(t *testing.T) {
	s, repo := newAuthServer(t)

	// Pre-set a state cookie matching the callback's state param.
	state := "state-xyz"
	req := httptest.NewRequest(http.MethodGet,
		"/v0/auth/github/callback?code=abc&state="+state, nil)
	req.AddCookie(&http.Cookie{Name: auth.StateCookieName, Value: state})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302:\n%s", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); loc != "/app" {
		t.Errorf("Location = %q, want /app", loc)
	}

	// Session cookie should be set; user record should exist.
	var sessCookie, csrfCookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		switch c.Name {
		case auth.SessionCookieName:
			sessCookie = c
		case CSRFCookieName:
			csrfCookie = c
		}
	}
	if sessCookie == nil {
		t.Fatal("session cookie not set")
	}
	if !sessCookie.HttpOnly || !sessCookie.Secure {
		t.Errorf("session cookie missing HttpOnly/Secure: %+v", sessCookie)
	}
	if sessCookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("session cookie SameSite = %v, want Lax", sessCookie.SameSite)
	}
	if !strings.HasPrefix(sessCookie.Value, auth.SessionTokenPrefix) {
		t.Errorf("session cookie value missing prefix: %q", sessCookie.Value)
	}

	// E4.6: CSRF cookie minted alongside the session. JS reads it
	// (HttpOnly: false) and mirrors the value back as X-CSRF-Token
	// on state-changing requests.
	if csrfCookie == nil {
		t.Fatal("CSRF cookie not set")
	}
	if csrfCookie.HttpOnly {
		t.Error("CSRF cookie must be readable from JS (HttpOnly false)")
	}
	if !csrfCookie.Secure || csrfCookie.SameSite != http.SameSiteStrictMode || csrfCookie.Path != "/" {
		t.Errorf("CSRF cookie attributes: Secure=%v SameSite=%v Path=%q", csrfCookie.Secure, csrfCookie.SameSite, csrfCookie.Path)
	}
	if len(csrfCookie.Value) != 2*csrfTokenBytes {
		t.Errorf("CSRF cookie value length = %d, want %d (hex of %d bytes)", len(csrfCookie.Value), 2*csrfTokenBytes, csrfTokenBytes)
	}

	// User row created.
	if len(repo.users) != 1 {
		t.Errorf("users = %d, want 1", len(repo.users))
	}
}

func TestGitHubCallback_StateMismatch_400(t *testing.T) {
	s, _ := newAuthServer(t)
	req := httptest.NewRequest(http.MethodGet,
		"/v0/auth/github/callback?code=abc&state=fromBrowser", nil)
	req.AddCookie(&http.Cookie{Name: auth.StateCookieName, Value: "different"})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestGitHubCallback_StateCookieMissing_400(t *testing.T) {
	s, _ := newAuthServer(t)
	req := httptest.NewRequest(http.MethodGet,
		"/v0/auth/github/callback?code=abc&state=fromBrowser", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestGitHubCallback_RejectsUnsafeRedirect(t *testing.T) {
	repo := newFakeAuthRepo()
	_, gh := stubGitHubOAuthServer(t)
	s := New(Config{
		Addr:                   "127.0.0.1:0",
		AuthRepo:               repo,
		GitHubOAuth:            gh,
		AuthRedirectAfterLogin: "//evil.example.com/", // open-redirect
	})

	state := "x"
	req := httptest.NewRequest(http.MethodGet,
		"/v0/auth/github/callback?code=abc&state="+state, nil)
	req.AddCookie(&http.Cookie{Name: auth.StateCookieName, Value: state})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/" {
		t.Errorf("Location = %q, want / (unsafe target should be rejected)", loc)
	}
}

func TestGetMe_HappyPath(t *testing.T) {
	s, repo := newAuthServer(t)
	// Sign in once via the repo to get a user + session.
	user, sess, err := repo.SignIn(context.Background(), auth.GitHubProfile{
		ID: 42, Login: "octocat", Name: "The Octo Cat",
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v0/auth/me", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sess.PlainText})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp userResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.GitHubLogin != "octocat" {
		t.Errorf("login = %q", resp.GitHubLogin)
	}
	if resp.ID != user.ID {
		t.Errorf("id mismatch: got %s, want %s", resp.ID, user.ID)
	}
}

func TestGetMe_NoSession_401(t *testing.T) {
	s, _ := newAuthServer(t)
	req := httptest.NewRequest(http.MethodGet, "/v0/auth/me", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestLogout_HappyPath(t *testing.T) {
	s, repo := newAuthServer(t)
	_, sess, _ := repo.SignIn(context.Background(), auth.GitHubProfile{
		ID: 42, Login: "octocat", Name: "Octocat",
	})

	req := httptest.NewRequest(http.MethodPost, "/v0/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sess.PlainText})
	// E4.6 #152: session-cookie-authed POST must double-submit the
	// CSRF token. Issue one freshly here — the OAuth callback path
	// is what mints it for real callers.
	const csrfTok = "deadbeef"
	req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: csrfTok})
	req.Header.Set(CSRFHeaderName, csrfTok)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204:\n%s", w.Code, w.Body.String())
	}
	// Session should be revoked (now expired).
	sid, _ := uuid.Parse(sess.ID)
	repo.mu.Lock()
	got := repo.sessions[sid.String()]
	repo.mu.Unlock()
	if got == nil || got.RevokedAt == nil {
		t.Errorf("session not revoked: %+v", got)
	}
	// Subsequent /me with the same cookie returns 401.
	req2 := httptest.NewRequest(http.MethodGet, "/v0/auth/me", nil)
	req2.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sess.PlainText})
	w2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(w2, req2)
	if w2.Code != http.StatusUnauthorized {
		t.Errorf("post-logout /me status = %d, want 401", w2.Code)
	}
}

func TestLogout_NoSession_401(t *testing.T) {
	s, _ := newAuthServer(t)
	req := httptest.NewRequest(http.MethodPost, "/v0/auth/logout", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestAuth_Unconfigured_503(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/v0/auth/github/login"},
		{http.MethodGet, "/v0/auth/github/callback?code=x&state=x"},
		{http.MethodGet, "/v0/auth/me"},
		{http.MethodPost, "/v0/auth/logout"},
	} {
		t.Run(route.path, func(t *testing.T) {
			req := httptest.NewRequest(route.method, route.path, nil)
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, req)
			if w.Code != http.StatusServiceUnavailable {
				t.Errorf("status = %d, want 503", w.Code)
			}
		})
	}
}

func TestIsSafeRelativeRedirect(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"/", true},
		{"/app", true},
		{"/app/runs", true},
		{"", false},
		{"http://example.com", false},
		{"https://evil.example.com", false},
		{"//evil.example.com", false},
		{`/\evil.example.com`, false},
		{"app", false}, // not absolute path
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			if got := isSafeRelativeRedirect(c.in); got != c.want {
				t.Errorf("isSafeRelativeRedirect(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestBearerAuth_SessionCookieResolvesIdentity(t *testing.T) {
	repo := newFakeAuthRepo()
	user, sess, _ := repo.SignIn(context.Background(), auth.GitHubProfile{
		ID: 42, Login: "octocat", Name: "x",
	})

	var captured Identity
	h := bearerAuth(nil, repo)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured = IdentityFrom(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sess.PlainText})
	h.ServeHTTP(httptest.NewRecorder(), req)

	if captured.Subject != "github:octocat" {
		t.Errorf("Subject = %q, want github:octocat", captured.Subject)
	}
	if captured.UserID != user.ID {
		t.Errorf("UserID mismatch")
	}
	if captured.SessionID != sess.ID {
		t.Errorf("SessionID mismatch")
	}
}

func TestBearerAuth_RevokedSessionFallsBackToAnonymous(t *testing.T) {
	repo := newFakeAuthRepo()
	_, sess, _ := repo.SignIn(context.Background(), auth.GitHubProfile{
		ID: 42, Login: "octocat", Name: "x",
	})
	sid, _ := uuid.Parse(sess.ID)
	if err := repo.Revoke(context.Background(), sid); err != nil {
		t.Fatal(err)
	}

	var captured Identity
	h := bearerAuth(nil, repo)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured = IdentityFrom(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sess.PlainText})
	h.ServeHTTP(httptest.NewRecorder(), req)

	if !captured.IsAnonymous() {
		t.Errorf("revoked session should produce anonymous, got %+v", captured)
	}
}

func TestBearerAuth_AuthRepoNil_AnonymousOnCookie(t *testing.T) {
	var captured Identity
	h := bearerAuth(nil, nil)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured = IdentityFrom(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: "fhs_anything"})
	h.ServeHTTP(httptest.NewRecorder(), req)
	if !captured.IsAnonymous() {
		t.Errorf("nil AuthRepo should produce anonymous, got %+v", captured)
	}
}

// Surface a known-stale repo.SignIn error path so coverage on the
// branch is exercised without integration plumbing.
func TestFakeAuthRepo_SignInError(t *testing.T) {
	r := newFakeAuthRepo()
	r.signInErr = errors.New("boom")
	_, _, err := r.SignIn(context.Background(), auth.GitHubProfile{ID: 1, Login: "x"})
	if err == nil {
		t.Error("expected propagated error")
	}
}
