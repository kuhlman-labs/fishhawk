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
	"github.com/jackc/pgx/v5/pgxpool"

	accountdb "github.com/kuhlman-labs/fishhawk/backend/internal/account/db"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auth"
	"github.com/kuhlman-labs/fishhawk/backend/internal/identity"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
	"github.com/kuhlman-labs/fishhawk/backend/internal/repoacl"
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

func (f *fakeAuthRepo) SignIn(_ context.Context, provider string, p auth.GitHubProfile, accountID uuid.UUID) (*auth.User, *auth.Session, error) {
	if f.signInErr != nil {
		return nil, nil, f.signInErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	if provider == "" {
		provider = "github"
	}
	now := time.Now().UTC()
	user := &auth.User{
		ID:           uuid.New().String(),
		Provider:     provider,
		GitHubUserID: p.ID,
		GitHubLogin:  p.Login,
		Name:         p.Name,
		Email:        p.Email,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	f.users[user.ID] = user

	boundAccount := ""
	if accountID != uuid.Nil {
		boundAccount = accountID.String()
	}
	plain := auth.SessionTokenPrefix + uuid.New().String() + uuid.New().String()
	hash, _ := auth.HashPlaintext(plain)
	sess := &auth.Session{
		ID:                uuid.New().String(),
		UserID:            user.ID,
		IssuedAt:          now,
		LastUsedAt:        now,
		SlidingExpiresAt:  now.Add(auth.SessionSlidingTTL),
		AbsoluteExpiresAt: now.Add(auth.SessionAbsoluteTTL),
		AccountID:         boundAccount,
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
	return stubGitHubOAuthServerWithLogin(t, "octocat")
}

// stubGitHubOAuthServerWithLogin is stubGitHubOAuthServer with a
// caller-chosen profile login, so the EMU case can drive an
// "<username>_<shortcode>" login through the real callback.
func stubGitHubOAuthServerWithLogin(t *testing.T, login string) (*httptest.Server, *auth.GitHubOAuth) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"gho_xxx"}`))
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		body, _ := json.Marshal(map[string]any{
			"id":    int64(42),
			"login": login,
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

// fakeMembershipResolver is the injectable membership gate for
// handler tests. Zero value denies (empty result).
type fakeMembershipResolver struct {
	ids         []uuid.UUID
	err         error
	calls       int
	gotProvider string
	gotToken    string
	gotLogin    string
}

func (f *fakeMembershipResolver) ResolveAccounts(_ context.Context, provider, accessToken string, profile auth.GitHubProfile) ([]uuid.UUID, error) {
	f.calls++
	f.gotProvider = provider
	f.gotToken = accessToken
	f.gotLogin = profile.Login
	return f.ids, f.err
}

// testAccountID is the account the default test resolver admits.
var testAccountID = uuid.MustParse("11111111-2222-3333-4444-555555555555")

func newAuthServer(t *testing.T) (*Server, *fakeAuthRepo) {
	t.Helper()
	s, repo, _ := newAuthServerWithResolver(t, &fakeMembershipResolver{ids: []uuid.UUID{testAccountID}})
	return s, repo
}

func newAuthServerWithResolver(t *testing.T, resolver auth.MembershipResolver) (*Server, *fakeAuthRepo, *auth.GitHubOAuth) {
	t.Helper()
	repo := newFakeAuthRepo()
	_, gh := stubGitHubOAuthServer(t)
	s := New(Config{
		Addr:                   "127.0.0.1:0",
		AuthRepo:               repo,
		GitHubOAuth:            gh,
		AuthMembership:         resolver,
		AuthRedirectAfterLogin: "/app",
	})
	return s, repo, gh
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

// E7.2.1 (#153): /login carries forward an optional ?next= query
// param so post-sign-in routing lands on the page the user actually
// asked for. The cookie is short-lived, single-use, and the value
// must pass the same open-redirect validation as the operator-set
// default.

func TestGitHubLogin_StoresValidNextInCookie(t *testing.T) {
	s, _ := newAuthServer(t)
	req := httptest.NewRequest(http.MethodGet,
		"/v0/auth/github/login?next=/runs/abc", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	var next *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == auth.NextCookieName {
			next = c
		}
	}
	if next == nil || next.Value != "/runs/abc" {
		t.Fatalf("next cookie = %+v, want value /runs/abc", next)
	}
	if !next.HttpOnly || !next.Secure {
		t.Errorf("next cookie missing HttpOnly/Secure: %+v", next)
	}
}

func TestGitHubLogin_DropsUnsafeNext(t *testing.T) {
	cases := []string{
		"https://evil.example.com/x",
		"//evil.example.com/x",
		`/\evil.example.com/x`,
		"javascript:alert(1)",
		"app://x",
		"runs/abc", // no leading slash → not a relative path
		"",
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			s, _ := newAuthServer(t)
			url := "/v0/auth/github/login"
			if in != "" {
				url += "?next=" + in
			}
			req := httptest.NewRequest(http.MethodGet, url, nil)
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, req)
			for _, c := range w.Result().Cookies() {
				if c.Name == auth.NextCookieName {
					t.Errorf("next cookie set with unsafe value %q: %+v", in, c)
				}
			}
		})
	}
}

func TestGitHubCallback_RedirectsToNextWhenSet(t *testing.T) {
	s, _ := newAuthServer(t)
	state := "state-next"
	req := httptest.NewRequest(http.MethodGet,
		"/v0/auth/github/callback?code=abc&state="+state, nil)
	req.AddCookie(&http.Cookie{Name: auth.StateCookieName, Value: state})
	req.AddCookie(&http.Cookie{Name: auth.NextCookieName, Value: "/audit"})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/audit" {
		t.Errorf("Location = %q, want /audit (overrides /app default)", loc)
	}
	// Cookie must be cleared on use (single-use).
	var cleared bool
	for _, c := range w.Result().Cookies() {
		if c.Name == auth.NextCookieName && c.MaxAge == -1 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("next cookie not cleared on callback")
	}
}

func TestGitHubCallback_DropsUnsafeNextCookieValue(t *testing.T) {
	// A tampered cookie value (e.g., from a malicious extension) must
	// not become an open-redirect vector. The callback re-validates
	// before honoring.
	s, _ := newAuthServer(t)
	state := "state-evil"
	req := httptest.NewRequest(http.MethodGet,
		"/v0/auth/github/callback?code=abc&state="+state, nil)
	req.AddCookie(&http.Cookie{Name: auth.StateCookieName, Value: state})
	req.AddCookie(&http.Cookie{Name: auth.NextCookieName, Value: "//evil.example.com/"})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if loc := w.Header().Get("Location"); loc != "/app" {
		t.Errorf("Location = %q, want /app (default; tampered cookie ignored)", loc)
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
		AuthMembership:         &fakeMembershipResolver{ids: []uuid.UUID{testAccountID}},
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
	user, sess, err := repo.SignIn(context.Background(), "github", auth.GitHubProfile{
		ID: 42, Login: "octocat", Name: "The Octo Cat",
	}, testAccountID)
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
	_, sess, _ := repo.SignIn(context.Background(), "github", auth.GitHubProfile{
		ID: 42, Login: "octocat", Name: "Octocat",
	}, testAccountID)

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
	user, sess, _ := repo.SignIn(context.Background(), "github", auth.GitHubProfile{
		ID: 42, Login: "octocat", Name: "x",
	}, testAccountID)

	var captured Identity
	h := newServer(t, newFakeRepo()).bearerAuth(nil, nil, repo)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
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
	_, sess, _ := repo.SignIn(context.Background(), "github", auth.GitHubProfile{
		ID: 42, Login: "octocat", Name: "x",
	}, testAccountID)
	sid, _ := uuid.Parse(sess.ID)
	if err := repo.Revoke(context.Background(), sid); err != nil {
		t.Fatal(err)
	}

	var captured Identity
	h := newServer(t, newFakeRepo()).bearerAuth(nil, nil, repo)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
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
	h := newServer(t, newFakeRepo()).bearerAuth(nil, nil, nil)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
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
	_, _, err := r.SignIn(context.Background(), "github", auth.GitHubProfile{ID: 1, Login: "x"}, uuid.Nil)
	if err == nil {
		t.Error("expected propagated error")
	}
}

// --- E44.3 membership-gate tests (ADR-057 Amendment A2) ---

// Named fail-closed branch 1: a nil resolver denies EVERY sign-in —
// access-denied redirect, no session cookie, no CSRF cookie, no user.
func TestGitHubCallback_NilResolver_DeniesFailClosed(t *testing.T) {
	repo := newFakeAuthRepo()
	_, gh := stubGitHubOAuthServer(t)
	s := New(Config{
		Addr:        "127.0.0.1:0",
		AuthRepo:    repo,
		GitHubOAuth: gh,
		// AuthMembership deliberately nil.
	})

	w := callbackRequest(t, s)
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302:\n%s", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); loc != "/access-denied" {
		t.Errorf("Location = %q, want /access-denied", loc)
	}
	assertNoAuthCookies(t, w)
	if len(repo.users) != 0 {
		t.Errorf("users = %d, want 0 (no SignIn on deny)", len(repo.users))
	}
}

// Named fail-closed branch 2: a resolver ERROR (forge down with no
// invited grant) fails closed with a non-2xx and no session.
func TestGitHubCallback_ResolverError_FailsClosed(t *testing.T) {
	resolver := &fakeMembershipResolver{err: errors.New("github is down")}
	s, repo, _ := newAuthServerWithResolver(t, resolver)

	w := callbackRequest(t, s)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "membership_resolution_failed") {
		t.Errorf("body missing membership_resolution_failed:\n%s", w.Body.String())
	}
	assertNoAuthCookies(t, w)
	if len(repo.users) != 0 {
		t.Errorf("users = %d, want 0 (no SignIn on resolver error)", len(repo.users))
	}
}

// Named fail-closed branch 3: no admitting account -> access-denied
// redirect with NO session cookie and NO CSRF cookie.
func TestGitHubCallback_NoAdmittingAccount_RedirectsAccessDenied(t *testing.T) {
	resolver := &fakeMembershipResolver{} // empty result = deny
	s, repo, _ := newAuthServerWithResolver(t, resolver)

	w := callbackRequest(t, s)
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302:\n%s", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); loc != "/access-denied" {
		t.Errorf("Location = %q, want /access-denied", loc)
	}
	assertNoAuthCookies(t, w)
	if len(repo.users) != 0 {
		t.Errorf("users = %d, want 0 (no SignIn on deny)", len(repo.users))
	}
}

// The configured deny target is honored when safe and replaced by the
// default when it is an open-redirect vector.
func TestGitHubCallback_AccessDeniedRedirectConfig(t *testing.T) {
	for _, tc := range []struct{ configured, want string }{
		{"/no-entry", "/no-entry"},
		{"//evil.example.com/", "/access-denied"},
		{"", "/access-denied"},
	} {
		t.Run(tc.configured, func(t *testing.T) {
			repo := newFakeAuthRepo()
			_, gh := stubGitHubOAuthServer(t)
			s := New(Config{
				Addr:                     "127.0.0.1:0",
				AuthRepo:                 repo,
				GitHubOAuth:              gh,
				AuthMembership:           &fakeMembershipResolver{},
				AuthAccessDeniedRedirect: tc.configured,
			})
			w := callbackRequest(t, s)
			if loc := w.Header().Get("Location"); loc != tc.want {
				t.Errorf("Location = %q, want %q", loc, tc.want)
			}
		})
	}
}

// Admission threads through: the resolver sees the provider + the
// exchanged token + profile, and the admitted account binds the
// session, surfacing on /v0/auth/me as account_id.
func TestGitHubCallback_AdmittedAccountBindsSession(t *testing.T) {
	resolver := &fakeMembershipResolver{ids: []uuid.UUID{testAccountID}}
	s, _, _ := newAuthServerWithResolver(t, resolver)

	w := callbackRequest(t, s)
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302:\n%s", w.Code, w.Body.String())
	}
	if resolver.calls != 1 || resolver.gotProvider != "github" ||
		resolver.gotToken != "gho_xxx" || resolver.gotLogin != "octocat" {
		t.Errorf("resolver saw %+v, want github/gho_xxx/octocat once", resolver)
	}
	var sessCookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == auth.SessionCookieName {
			sessCookie = c
		}
	}
	if sessCookie == nil {
		t.Fatal("session cookie not set on admitted sign-in")
	}

	req := httptest.NewRequest(http.MethodGet, "/v0/auth/me", nil)
	req.AddCookie(sessCookie)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/v0/auth/me status = %d, want 200:\n%s", rec.Code, rec.Body.String())
	}
	var resp userResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.AccountID == nil || *resp.AccountID != testAccountID.String() {
		t.Errorf("account_id = %v, want %s", resp.AccountID, testAccountID)
	}
}

// Defense in depth: a session whose account binding is gone (account
// deleted after sign-in, or a pre-gate session) gets 403
// account_unresolved from /v0/auth/me, not another tenant's data.
func TestGetMe_SessionWithoutAccount_403AccountUnresolved(t *testing.T) {
	s, repo := newAuthServer(t)
	_, sess, err := repo.SignIn(context.Background(), "github", auth.GitHubProfile{
		ID: 42, Login: "octocat", Name: "x",
	}, uuid.Nil) // unbound session
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v0/auth/me", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sess.PlainText})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "account_unresolved") {
		t.Errorf("body missing account_unresolved:\n%s", w.Body.String())
	}
}

// callbackRequest drives a state-valid GET /v0/auth/github/callback.
func callbackRequest(t *testing.T, s *Server) *httptest.ResponseRecorder {
	t.Helper()
	state := "state-gate"
	req := httptest.NewRequest(http.MethodGet,
		"/v0/auth/github/callback?code=abc&state="+state, nil)
	req.AddCookie(&http.Cookie{Name: auth.StateCookieName, Value: state})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

// assertNoAuthCookies fails if the response set a session or CSRF
// cookie — a denied sign-in must leave the browser credential-free.
func assertNoAuthCookies(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if (c.Name == auth.SessionCookieName || c.Name == CSRFCookieName) && c.MaxAge >= 0 && c.Value != "" {
			t.Errorf("denied sign-in set cookie %s=%q", c.Name, c.Value)
		}
	}
}

// TestGitHubCallback_MembershipGate_PostgresE2E drives the FULL
// callback against a real migrated database: real auth repo, real
// membership resolver + account/db store, fake GitHub OAuth endpoints,
// fake forge lister. The admission source is real account_members /
// accounts rows — the cross-boundary seam the per-layer units can't
// cover (sessions.account_id persisted end-to-end).
func TestGitHubCallback_MembershipGate_PostgresE2E(t *testing.T) {
	// newPGAuthServerAs wires the REAL resolver over the real store with
	// the given profile login and (optionally) an EMU-posture OAuth host.
	newPGAuthServerAs := func(t *testing.T, lister *e2eOrgLister, login, emuOAuthHost string) (*Server, *pgxpool.Pool) {
		t.Helper()
		url := pgtest.NewURL(t)
		if err := postgres.MigrateUp(url); err != nil {
			t.Fatalf("MigrateUp: %v", err)
		}
		pool, err := pgxpool.New(context.Background(), url)
		if err != nil {
			t.Fatalf("pool: %v", err)
		}
		t.Cleanup(pool.Close)
		_, gh := stubGitHubOAuthServerWithLogin(t, login)
		s := New(Config{
			Addr:        "127.0.0.1:0",
			AuthRepo:    auth.NewPostgresRepository(pool),
			GitHubOAuth: gh,
			AuthMembership: auth.NewMembershipResolver(
				auth.NewAccountMembershipStore(accountdb.New(pool)),
				map[string]auth.ForgeMembershipLister{"github": lister},
				auth.WithEMUOAuthHost(emuOAuthHost)),
			AuthRedirectAfterLogin: "/app",
		})
		return s, pool
	}
	newPGAuthServer := func(t *testing.T, lister *e2eOrgLister) (*Server, *pgxpool.Pool) {
		t.Helper()
		return newPGAuthServerAs(t, lister, "octocat", "")
	}
	seedAccountAt := func(t *testing.T, pool *pgxpool.Pool, key, granularity string, autoJoinRole *string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO accounts (id, provider, account_key, granularity, auto_join_role)
			 VALUES ($1, 'github', $2, $3, $4)`,
			id, key, granularity, autoJoinRole,
		); err != nil {
			t.Fatalf("seed account: %v", err)
		}
		return id
	}
	seedAccount := func(t *testing.T, pool *pgxpool.Pool, key string, autoJoinRole *string) uuid.UUID {
		t.Helper()
		return seedAccountAt(t, pool, key, "organization", autoJoinRole)
	}

	// EMU end-to-end: an "<username>_<shortcode>" login under EMU
	// posture resolves profile → short code → the pair-wise auto-join
	// SQL → minted grant → session bound to the ENTERPRISE account,
	// across all four layers. No org membership is involved.
	t.Run("EMU enterprise auto-join admits and binds the session", func(t *testing.T) {
		lister := &e2eOrgLister{} // no org memberships at all
		s, pool := newPGAuthServerAs(t, lister, "alice_acme",
			"https://acme.ghe.com/login/oauth/authorize")
		role := "member"
		accountID := seedAccountAt(t, pool, "acme", "enterprise", &role)

		w := callbackRequest(t, s)
		if w.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302:\n%s", w.Code, w.Body.String())
		}
		var persisted *uuid.UUID
		if err := pool.QueryRow(context.Background(),
			`SELECT account_id FROM sessions`).Scan(&persisted); err != nil {
			t.Fatalf("read sessions.account_id: %v", err)
		}
		if persisted == nil || *persisted != accountID {
			t.Errorf("sessions.account_id = %v, want the enterprise account %s", persisted, accountID)
		}
		var origin string
		if err := pool.QueryRow(context.Background(),
			`SELECT origin FROM account_members WHERE account_id = $1 AND member_ref = 'alice_acme'`,
			accountID).Scan(&origin); err != nil {
			t.Fatalf("read minted grant: %v", err)
		}
		if origin != "auto_join" {
			t.Errorf("origin = %q, want auto_join", origin)
		}
	})

	// The same login on github.com posture is DENIED: no enterprise key
	// is derived, so a crafted underscore login cannot claim an
	// enterprise end-to-end either.
	t.Run("underscore login on github.com posture is denied", func(t *testing.T) {
		lister := &e2eOrgLister{}
		s, pool := newPGAuthServerAs(t, lister, "alice_acme", "")
		role := "member"
		seedAccountAt(t, pool, "acme", "enterprise", &role)

		w := callbackRequest(t, s)
		if loc := w.Header().Get("Location"); loc != "/access-denied" {
			t.Errorf("Location = %q, want /access-denied", loc)
		}
		assertNoAuthCookies(t, w)
		var sessions int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM sessions`).Scan(&sessions); err != nil {
			t.Fatalf("count sessions: %v", err)
		}
		if sessions != 0 {
			t.Errorf("sessions rows = %d, want 0 on deny", sessions)
		}
	})

	t.Run("auto-join admits and binds the session", func(t *testing.T) {
		lister := &e2eOrgLister{keys: []string{"acme-corp"}}
		s, pool := newPGAuthServer(t, lister)
		role := "member"
		accountID := seedAccount(t, pool, "acme-corp", &role)

		w := callbackRequest(t, s)
		if w.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302:\n%s", w.Code, w.Body.String())
		}
		var sessCookie *http.Cookie
		for _, c := range w.Result().Cookies() {
			if c.Name == auth.SessionCookieName {
				sessCookie = c
			}
		}
		if sessCookie == nil {
			t.Fatal("session cookie not set")
		}
		// The persisted sessions row carries the resolved account.
		var persisted *uuid.UUID
		if err := pool.QueryRow(context.Background(),
			`SELECT account_id FROM sessions`).Scan(&persisted); err != nil {
			t.Fatalf("read sessions.account_id: %v", err)
		}
		if persisted == nil || *persisted != accountID {
			t.Errorf("sessions.account_id = %v, want %s", persisted, accountID)
		}
		// The minted grant is audited as auto_join.
		var origin string
		if err := pool.QueryRow(context.Background(),
			`SELECT origin FROM account_members WHERE account_id = $1 AND member_ref = 'octocat'`,
			accountID).Scan(&origin); err != nil {
			t.Fatalf("read minted grant: %v", err)
		}
		if origin != "auto_join" {
			t.Errorf("origin = %q, want auto_join", origin)
		}
		// /v0/auth/me surfaces the account context.
		req := httptest.NewRequest(http.MethodGet, "/v0/auth/me", nil)
		req.AddCookie(sessCookie)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("/v0/auth/me status = %d:\n%s", rec.Code, rec.Body.String())
		}
		var resp userResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		if resp.AccountID == nil || *resp.AccountID != accountID.String() {
			t.Errorf("account_id = %v, want %s", resp.AccountID, accountID)
		}
	})

	t.Run("invited row admits with the forge erroring", func(t *testing.T) {
		lister := &e2eOrgLister{err: errors.New("github is down")}
		s, pool := newPGAuthServer(t, lister)
		accountID := seedAccount(t, pool, "acme-corp", nil)
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO account_members (id, account_id, provider, member_ref, origin)
			 VALUES ($1, $2, 'github', 'octocat', 'invited')`,
			uuid.New(), accountID); err != nil {
			t.Fatalf("seed invited grant: %v", err)
		}

		w := callbackRequest(t, s)
		if w.Code != http.StatusFound || w.Header().Get("Location") != "/app" {
			t.Fatalf("status/Location = %d %q, want 302 /app:\n%s",
				w.Code, w.Header().Get("Location"), w.Body.String())
		}
	})

	t.Run("no admitting row denies with no session row", func(t *testing.T) {
		lister := &e2eOrgLister{keys: []string{"some-other-org"}}
		s, pool := newPGAuthServer(t, lister)
		seedAccount(t, pool, "acme-corp", nil)

		w := callbackRequest(t, s)
		if w.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302:\n%s", w.Code, w.Body.String())
		}
		if loc := w.Header().Get("Location"); loc != "/access-denied" {
			t.Errorf("Location = %q, want /access-denied", loc)
		}
		assertNoAuthCookies(t, w)
		var sessions int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM sessions`).Scan(&sessions); err != nil {
			t.Fatalf("count sessions: %v", err)
		}
		if sessions != 0 {
			t.Errorf("sessions rows = %d, want 0 on deny", sessions)
		}
	})

	t.Run("forge error with no invited row fails closed", func(t *testing.T) {
		lister := &e2eOrgLister{err: errors.New("github is down")}
		s, pool := newPGAuthServer(t, lister)
		role := "member"
		seedAccount(t, pool, "acme-corp", &role)

		w := callbackRequest(t, s)
		if w.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502:\n%s", w.Code, w.Body.String())
		}
		assertNoAuthCookies(t, w)
		var sessions int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM sessions`).Scan(&sessions); err != nil {
			t.Fatalf("count sessions: %v", err)
		}
		if sessions != 0 {
			t.Errorf("sessions rows = %d, want 0 on fail-closed", sessions)
		}
	})
}

// e2eOrgLister is the fake ForgeMembershipLister for the postgres
// end-to-end callback tests.
type e2eOrgLister struct {
	keys []string
	err  error
}

func (f *e2eOrgLister) ListUserOrgKeys(context.Context, string) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.keys, nil
}

// ---- login-time repo-ACL mirror purge (ADR-057 Amendment A2 / #2071) ------

// newAuthServerWithVisibility is newAuthServer plus a wired RepoVisibility
// seam — the only difference that matters to the purge tests.
func newAuthServerWithVisibility(t *testing.T, vis RepoVisibility) *Server {
	t.Helper()
	_, gh := stubGitHubOAuthServer(t)
	return New(Config{
		Addr:                   "127.0.0.1:0",
		AuthRepo:               newFakeAuthRepo(),
		GitHubOAuth:            gh,
		AuthMembership:         &fakeMembershipResolver{ids: []uuid.UUID{testAccountID}},
		AuthRedirectAfterLogin: "/app",
		RepoVisibility:         vis,
	})
}

// signIn drives one full OAuth callback and returns the recorder.
func signIn(t *testing.T, s *Server) *httptest.ResponseRecorder {
	t.Helper()
	state := "state-purge"
	req := httptest.NewRequest(http.MethodGet,
		"/v0/auth/github/callback?code=abc&state="+state, nil)
	req.AddCookie(&http.Cookie{Name: auth.StateCookieName, Value: state})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

func TestGitHubCallback_PurgesRepoACLMirrorForSubject(t *testing.T) {
	vis := newFakeRepoVisibility(map[string]bool{})
	s := newAuthServerWithVisibility(t, vis)

	if w := signIn(t, s); w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302:\n%s", w.Code, w.Body.String())
	}
	if vis.purgeCall != 1 {
		t.Fatalf("InvalidateSubject calls = %d, want 1", vis.purgeCall)
	}
	// The purge keys on (provider, forge-neutral subject) — the SAME pair the
	// read filter derives from the session identity, so a purge and a lookup
	// address one row set.
	if got, want := vis.purged[0], "github|octocat"; got != want {
		t.Errorf("purged = %q, want %q", got, want)
	}
}

// TestGitHubCallback_PurgeFailureIsNonFatal is the operator's binding condition
// (2). Two things are asserted, and the SECOND is the one the plan originally
// got wrong: a failed purge does NOT merely leave the mirror needing a
// re-resolve — the previously cached entries, GRANTS INCLUDED, survive. What
// bounds that exposure is the TTL, which is asserted separately below.
func TestGitHubCallback_PurgeFailureIsNonFatal(t *testing.T) {
	vis := newFakeRepoVisibility(map[string]bool{})
	vis.purgeErr = errors.New("mirror store unavailable")
	s := newAuthServerWithVisibility(t, vis)

	w := signIn(t, s)
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 — a purge failure must not fail sign-in closed:\n%s",
			w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); loc != "/app" {
		t.Errorf("Location = %q, want /app (normal post-login redirect)", loc)
	}
	var sess *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == auth.SessionCookieName {
			sess = c
		}
	}
	if sess == nil {
		t.Fatal("no session cookie: the purge failure aborted sign-in")
	}
	if vis.purgeCall != 1 {
		t.Errorf("InvalidateSubject calls = %d, want 1 (attempted, then tolerated)", vis.purgeCall)
	}
}

// staleStore is a repoacl.Store holding ONE pre-seeded entry whose age the test
// controls, and whose purge always fails. It is the fixture for the bound in
// condition (2): the entries that survive a failed purge are still subject to
// TTL expiry.
type staleStore struct {
	entry     repoacl.Entry
	present   bool
	upserts   int
	bumps     int
	deleteErr error
}

func (s *staleStore) Get(context.Context, string, string, string) (repoacl.Entry, bool, error) {
	return s.entry, s.present, nil
}

func (s *staleStore) Upsert(_ context.Context, _, _, _ string, perm identity.Permission, _ int64) error {
	s.upserts++
	s.entry = repoacl.Entry{Permission: perm, CheckedAt: time.Now()}
	s.present = true
	return nil
}

// EnsurePurgeGeneration is a no-op watermark that always reports generation 0 —
// this fixture models the TTL bound, not the generation guard, so Get ignores
// the generation entirely and the captured value is inert.
func (s *staleStore) EnsurePurgeGeneration(context.Context, string, string) (int64, error) {
	return 0, nil
}

// BumpPurgeWatermark records the bump so the test can confirm InvalidateSubject
// bumps before it hits the failing delete; the bump itself succeeds.
func (s *staleStore) BumpPurgeWatermark(context.Context, string, string) error {
	s.bumps++
	return nil
}

func (s *staleStore) DeleteForSubject(context.Context, string, string) error {
	return s.deleteErr
}

// staleResolver answers the live forge lookup a TTL expiry forces.
type staleResolver struct {
	perm  identity.Permission
	calls int
}

func (r *staleResolver) PermissionLevel(context.Context, string, string) (identity.Permission, error) {
	r.calls++
	return r.perm, nil
}

// TestGitHubCallback_PurgeFailure_EntriesStillExpireByTTL is the second half of
// binding condition (2): with the purge failing, a surviving GRANT is bounded
// by the TTL rather than persisting indefinitely.
//
// It drives a REAL repoacl.Mirror (not the seam stub) because the bound being
// asserted is the mirror's, and asserts both arms against one store: an entry
// INSIDE the freshness window is served from the mirror with no forge call
// (this is the exposure the failed purge leaves), while the SAME entry aged
// past the TTL is re-resolved from the forge — which now answers "none", so
// the surviving grant is gone.
func TestGitHubCallback_PurgeFailure_EntriesStillExpireByTTL(t *testing.T) {
	const ttl = time.Hour
	store := &staleStore{
		entry:     repoacl.Entry{Permission: identity.PermissionWrite, CheckedAt: time.Now()},
		present:   true,
		deleteErr: errors.New("mirror store unavailable"),
	}
	// The forge has since REVOKED the grant.
	resolver := &staleResolver{perm: identity.PermissionNone}
	mirror := repoacl.NewMirror(store, resolver, ttl, nil)

	s := newAuthServerWithVisibility(t, mirror)
	if w := signIn(t, s); w.Code != http.StatusFound {
		t.Fatalf("sign-in status = %d, want 302 (purge failure is non-fatal)", w.Code)
	}

	// Arm 1: the entry survived the failed purge and is still fresh, so the
	// stale GRANT is still served. This is the exposure, stated as a test.
	visible, err := mirror.Visible(context.Background(), "github", "octocat", "acme/app")
	if err != nil {
		t.Fatalf("Visible: %v", err)
	}
	if !visible {
		t.Fatal("expected the surviving cached grant to still be visible inside the TTL")
	}
	if resolver.calls != 0 {
		t.Errorf("forge calls = %d, want 0 while the entry is fresh", resolver.calls)
	}

	// Arm 2: age the SAME entry past the TTL. The mirror must re-resolve and
	// serve the forge's current answer, not the stale grant — so the failed
	// purge's exposure is bounded by the TTL.
	store.entry.CheckedAt = time.Now().Add(-2 * ttl)
	visible, err = mirror.Visible(context.Background(), "github", "octocat", "acme/app")
	if err != nil {
		t.Fatalf("Visible after TTL expiry: %v", err)
	}
	if visible {
		t.Error("a surviving entry aged past the TTL is still visible: the purge-failure exposure is UNBOUNDED")
	}
	if resolver.calls != 1 {
		t.Errorf("forge calls = %d, want 1 (TTL expiry forces a re-resolve)", resolver.calls)
	}
}

// TestGitHubCallback_NoMirrorWired_SignsInNormally pins the untenanted-allow
// posture on the login path: with no RepoVisibility seam there is no purge and
// sign-in is unchanged.
func TestGitHubCallback_NoMirrorWired_SignsInNormally(t *testing.T) {
	s, _ := newAuthServer(t)
	if w := signIn(t, s); w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 with no mirror wired", w.Code)
	}
}

// ---- GitLab browser sign-in (E44.22 / #2109) --------------------------------

// stubGitLabOAuthServer mounts httptest endpoints the GitLabOAuth client
// (and, for the e2e, the real GitLabMembershipLister) point at: /oauth/token,
// /api/v4/user, and /api/v4/groups. The username + group full_paths are
// caller-chosen so the auto-join case can drive a real group listing.
func stubGitLabOAuthServer(t *testing.T, username string, groups []string) (*httptest.Server, *auth.GitLabOAuth) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"glpat_xxx"}`))
	})
	mux.HandleFunc("/api/v4/user", func(w http.ResponseWriter, _ *http.Request) {
		body, _ := json.Marshal(map[string]any{
			"id":       int64(4242),
			"username": username,
			"name":     "GitLab User",
			"email":    "gl@example.com",
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
	mux.HandleFunc("/api/v4/groups", func(w http.ResponseWriter, _ *http.Request) {
		rows := make([]map[string]any, 0, len(groups))
		for _, g := range groups {
			rows = append(rows, map[string]any{"full_path": g})
		}
		body, _ := json.Marshal(rows)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	gl := auth.NewGitLabOAuth(srv.URL, "gitlab-client", "gitlab-secret",
		"https://example.com/gitlab/cb", auth.GitLabOAuthURLs{})
	return srv, gl
}

// newGitLabAuthServerWithResolver wires a server with the GitLab OAuth client
// and an injectable membership resolver (the fake, for handler-unit cases).
func newGitLabAuthServerWithResolver(t *testing.T, resolver auth.MembershipResolver) (*Server, *fakeAuthRepo) {
	t.Helper()
	repo := newFakeAuthRepo()
	_, gl := stubGitLabOAuthServer(t, "gluser", nil)
	s := New(Config{
		Addr:                   "127.0.0.1:0",
		AuthRepo:               repo,
		GitLabOAuth:            gl,
		AuthMembership:         resolver,
		AuthRedirectAfterLogin: "/app",
	})
	return s, repo
}

// gitlabCallbackRequest drives a state-valid GET /v0/auth/gitlab/callback.
func gitlabCallbackRequest(t *testing.T, s *Server) *httptest.ResponseRecorder {
	t.Helper()
	state := "gl-state"
	req := httptest.NewRequest(http.MethodGet,
		"/v0/auth/gitlab/callback?code=abc&state="+state, nil)
	req.AddCookie(&http.Cookie{Name: auth.StateCookieName, Value: state})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

func TestGitLabLogin_RedirectsAndSetsStateCookie(t *testing.T) {
	s, _ := newGitLabAuthServerWithResolver(t, &fakeMembershipResolver{ids: []uuid.UUID{testAccountID}})
	req := httptest.NewRequest(http.MethodGet, "/v0/auth/gitlab/login", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "/oauth/authorize") || !strings.Contains(loc, "state=") ||
		!strings.Contains(loc, "scope=read_api") {
		t.Errorf("Location missing GitLab authorize params (want /oauth/authorize, state, scope=read_api): %q", loc)
	}
	var state *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == auth.StateCookieName {
			state = c
		}
	}
	if state == nil {
		t.Fatal("state cookie not set")
	}
	if state.Path != "/v0/auth/gitlab/" {
		t.Errorf("state cookie Path = %q, want /v0/auth/gitlab/", state.Path)
	}
	if !state.HttpOnly || !state.Secure {
		t.Errorf("state cookie missing HttpOnly/Secure: %+v", state)
	}
}

// The GitLab OAuth endpoints fail closed with 503 when unconfigured — the same
// posture the GitHub pair has.
func TestGitLabLogin_Unconfigured_503(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0", AuthRepo: newFakeAuthRepo()})
	req := httptest.NewRequest(http.MethodGet, "/v0/auth/gitlab/login", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "oauth_unconfigured") {
		t.Errorf("body = %s, want oauth_unconfigured", w.Body.String())
	}
}

func TestGitLabCallback_Unconfigured_503(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0", AuthRepo: newFakeAuthRepo()})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v0/auth/gitlab/callback?code=x&state=y", nil)
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "oauth_unconfigured") {
		t.Errorf("body = %s, want oauth_unconfigured", w.Body.String())
	}
}

func TestGitLabCallback_StateMismatch_400(t *testing.T) {
	s, _ := newGitLabAuthServerWithResolver(t, &fakeMembershipResolver{ids: []uuid.UUID{testAccountID}})
	req := httptest.NewRequest(http.MethodGet,
		"/v0/auth/gitlab/callback?code=abc&state=fromBrowser", nil)
	req.AddCookie(&http.Cookie{Name: auth.StateCookieName, Value: "different"})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestGitLabCallback_StateCookieMissing_400(t *testing.T) {
	s, _ := newGitLabAuthServerWithResolver(t, &fakeMembershipResolver{ids: []uuid.UUID{testAccountID}})
	req := httptest.NewRequest(http.MethodGet,
		"/v0/auth/gitlab/callback?code=abc&state=whatever", nil)
	// No state cookie attached.
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestGitLabCallback_MembershipResolutionError_502(t *testing.T) {
	resolver := &fakeMembershipResolver{err: errors.New("gitlab is down")}
	s, repo := newGitLabAuthServerWithResolver(t, resolver)
	w := gitlabCallbackRequest(t, s)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502:\n%s", w.Code, w.Body.String())
	}
	assertNoAuthCookies(t, w)
	if len(repo.users) != 0 {
		t.Errorf("users = %d, want 0 (no SignIn on resolver error)", len(repo.users))
	}
}

func TestGitLabCallback_EmptyAccounts_AccessDenied(t *testing.T) {
	resolver := &fakeMembershipResolver{} // empty result = deny
	s, repo := newGitLabAuthServerWithResolver(t, resolver)
	w := gitlabCallbackRequest(t, s)
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302:\n%s", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); loc != "/access-denied" {
		t.Errorf("Location = %q, want /access-denied", loc)
	}
	assertNoAuthCookies(t, w)
	if len(repo.users) != 0 {
		t.Errorf("users = %d, want 0 (no SignIn on deny)", len(repo.users))
	}
}

// TestGitLabCallback_ThreadsProviderGitLab pins the cross-boundary contract at
// the handler level: the callback passes provider="gitlab" (never "github") to
// the resolver, along with the exchanged token and the GitLab username.
func TestGitLabCallback_ThreadsProviderGitLab(t *testing.T) {
	resolver := &fakeMembershipResolver{ids: []uuid.UUID{testAccountID}}
	s, _ := newGitLabAuthServerWithResolver(t, resolver)
	w := gitlabCallbackRequest(t, s)
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302:\n%s", w.Code, w.Body.String())
	}
	if resolver.calls != 1 || resolver.gotProvider != "gitlab" ||
		resolver.gotToken != "glpat_xxx" || resolver.gotLogin != "gluser" {
		t.Errorf("resolver saw %+v, want gitlab/glpat_xxx/gluser once", resolver)
	}
}

// TestGitLabCallback_MembershipGate_PostgresE2E is the cross-boundary
// end-to-end: a GitLab stub server (token + /api/v4/user + /api/v4/groups)
// drives GET /v0/auth/gitlab/callback through the REAL membership resolver +
// account/db store against a migrated Postgres — profile fetch → group list →
// group-granularity auto-join → account-bound session + CSRF cookie — asserting
// the resolver saw provider="gitlab" and no GitLab id clobbers a pre-existing
// GitHub user of the same numeric id.
func TestGitLabCallback_MembershipGate_PostgresE2E(t *testing.T) {
	newPGGitLabServer := func(t *testing.T, username string, groups []string) (*Server, *pgxpool.Pool, *stubGitLabLister) {
		t.Helper()
		url := pgtest.NewURL(t)
		if err := postgres.MigrateUp(url); err != nil {
			t.Fatalf("MigrateUp: %v", err)
		}
		pool, err := pgxpool.New(context.Background(), url)
		if err != nil {
			t.Fatalf("pool: %v", err)
		}
		t.Cleanup(pool.Close)
		_, gl := stubGitLabOAuthServer(t, username, groups)
		lister := &stubGitLabLister{keys: groups}
		s := New(Config{
			Addr:        "127.0.0.1:0",
			AuthRepo:    auth.NewPostgresRepository(pool),
			GitLabOAuth: gl,
			AuthMembership: auth.NewMembershipResolver(
				auth.NewAccountMembershipStore(accountdb.New(pool)),
				map[string]auth.ForgeMembershipLister{"gitlab": lister}),
			AuthRedirectAfterLogin: "/app",
		})
		return s, pool, lister
	}

	t.Run("group auto-join admits and binds the session", func(t *testing.T) {
		s, pool, lister := newPGGitLabServer(t, "gluser", []string{"acme-group/team"})
		// A group-granularity account whose key is the group full_path.
		accountID := uuid.New()
		role := "member"
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO accounts (id, provider, account_key, granularity, auto_join_role)
			 VALUES ($1, 'gitlab', $2, 'group', $3)`,
			accountID, "acme-group/team", &role,
		); err != nil {
			t.Fatalf("seed gitlab group account: %v", err)
		}

		w := gitlabCallbackRequest(t, s)
		if w.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302:\n%s", w.Code, w.Body.String())
		}
		if lister.gotToken != "glpat_xxx" {
			t.Errorf("lister saw token %q, want the exchanged glpat_xxx", lister.gotToken)
		}
		var sessCookie, csrfCookie *http.Cookie
		for _, c := range w.Result().Cookies() {
			switch c.Name {
			case auth.SessionCookieName:
				sessCookie = c
			case CSRFCookieName:
				csrfCookie = c
			}
		}
		if sessCookie == nil || csrfCookie == nil {
			t.Fatalf("session/CSRF cookies = %v/%v, want both set", sessCookie, csrfCookie)
		}
		// The persisted session binds the group account.
		var persisted *uuid.UUID
		if err := pool.QueryRow(context.Background(),
			`SELECT account_id FROM sessions`).Scan(&persisted); err != nil {
			t.Fatalf("read sessions.account_id: %v", err)
		}
		if persisted == nil || *persisted != accountID {
			t.Errorf("sessions.account_id = %v, want the group account %s", persisted, accountID)
		}
		// The minted grant is provider=gitlab, origin=auto_join.
		var origin, provider string
		if err := pool.QueryRow(context.Background(),
			`SELECT origin, provider FROM account_members WHERE account_id = $1 AND member_ref = 'gluser'`,
			accountID).Scan(&origin, &provider); err != nil {
			t.Fatalf("read minted grant: %v", err)
		}
		if origin != "auto_join" || provider != "gitlab" {
			t.Errorf("minted grant origin/provider = %q/%q, want auto_join/gitlab", origin, provider)
		}
		// The user row carries provider=gitlab.
		var userProvider string
		if err := pool.QueryRow(context.Background(),
			`SELECT provider FROM users WHERE github_login = 'gluser'`).Scan(&userProvider); err != nil {
			t.Fatalf("read user provider: %v", err)
		}
		if userProvider != "gitlab" {
			t.Errorf("users.provider = %q, want gitlab", userProvider)
		}
	})

	t.Run("gitlab id does not clobber a pre-existing github user of the same id", func(t *testing.T) {
		s, pool, _ := newPGGitLabServer(t, "gluser", []string{"acme-group/team"})
		accountID := uuid.New()
		role := "member"
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO accounts (id, provider, account_key, granularity, auto_join_role)
			 VALUES ($1, 'gitlab', $2, 'group', $3)`,
			accountID, "acme-group/team", &role,
		); err != nil {
			t.Fatalf("seed gitlab group account: %v", err)
		}
		// Pre-seed a GitHub user whose numeric id equals the GitLab stub's id
		// (4242) via the real repo.
		ghUser, _, err := auth.NewPostgresRepository(pool).SignIn(context.Background(), "github",
			auth.GitHubProfile{ID: 4242, Login: "gh-collide", Name: "GH"}, uuid.Nil)
		if err != nil {
			t.Fatalf("seed github user: %v", err)
		}

		w := gitlabCallbackRequest(t, s)
		if w.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302:\n%s", w.Code, w.Body.String())
		}
		// Two DISTINCT users rows share github_user_id=4242 across providers.
		var n int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM users WHERE github_user_id = 4242`).Scan(&n); err != nil {
			t.Fatalf("count users: %v", err)
		}
		if n != 2 {
			t.Fatalf("users with github_user_id=4242 = %d, want 2 (github + gitlab, no clobber)", n)
		}
		// The github row is untouched.
		var ghLogin, ghProvider string
		if err := pool.QueryRow(context.Background(),
			`SELECT github_login, provider FROM users WHERE id = $1`, uuid.MustParse(ghUser.ID)).
			Scan(&ghLogin, &ghProvider); err != nil {
			t.Fatalf("read github row: %v", err)
		}
		if ghLogin != "gh-collide" || ghProvider != "github" {
			t.Errorf("github row = %q/%q, want gh-collide/github (not overwritten)", ghLogin, ghProvider)
		}
	})
}

// stubGitLabLister is the fake ForgeMembershipLister for the GitLab postgres
// e2e — it records the token the resolver forwards and returns preset keys.
type stubGitLabLister struct {
	keys     []string
	gotToken string
	err      error
}

func (f *stubGitLabLister) ListUserOrgKeys(_ context.Context, token string) ([]string, error) {
	f.gotToken = token
	if f.err != nil {
		return nil, f.err
	}
	return f.keys, nil
}
