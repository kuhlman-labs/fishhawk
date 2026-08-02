package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/auth"
)

func TestGenerateCSRFToken_LengthAndUniqueness(t *testing.T) {
	a, err := generateCSRFToken()
	if err != nil {
		t.Fatalf("generateCSRFToken: %v", err)
	}
	b, err := generateCSRFToken()
	if err != nil {
		t.Fatalf("generateCSRFToken: %v", err)
	}
	if len(a) != 2*csrfTokenBytes {
		t.Errorf("token length = %d, want %d", len(a), 2*csrfTokenBytes)
	}
	if a == b {
		t.Errorf("two consecutive tokens collided: %q", a)
	}
}

func TestCSRFSafeMethod(t *testing.T) {
	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		if !csrfSafeMethod(m) {
			t.Errorf("%s should be csrf-safe", m)
		}
	}
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		if csrfSafeMethod(m) {
			t.Errorf("%s should NOT be csrf-safe", m)
		}
	}
}

func TestCSRFExemptPath(t *testing.T) {
	exempt := []string{
		"/v0/auth/github/login",
		"/v0/auth/github/callback",
		// GitLab OAuth handshake (E44.22 / #2109): same rationale as
		// github — no session cookie exists yet on /login, and /callback's
		// POST-CSRF substitute is the OAuth `state` parameter.
		"/v0/auth/gitlab/login",
		"/v0/auth/gitlab/callback",
		"/webhooks/github",
		"/webhooks/gitlab",
		// The MCP surface (ADR-076 / #2390): a bearer-authenticated,
		// non-cookie JSON-RPC transport. handleMCP independently refuses
		// any cookie-session identity with 401, so the exemption cannot
		// become a browser-driven CSRF path onto the tool surface —
		// asserted end-to-end by
		// TestMCPRoute_CookieSessionReachesHandlerNotCSRF.
		"/mcp",
		// POST /v0/oauth/token (ADR-076 slice 3, #2436): non-cookie,
		// form-encoded, public-client; handleOAuthToken refuses any
		// Authorization header and ignores session identity.
		"/v0/oauth/token",
	}
	for _, p := range exempt {
		if !csrfExemptPath(p) {
			t.Errorf("%s should be exempt", p)
		}
	}
	notExempt := []string{
		"/v0/auth/me",
		"/v0/auth/logout",
		"/v0/runs",
		"/v0/stages/abc/approvals",
		// Exact match only: a sibling path must not inherit the /mcp
		// exemption.
		"/mcp/tools",
		"/v0/mcp",
		// The consent POST is deliberately NOT exempt — it is
		// cookie-authenticated and stays CSRF-enforced via the form-field
		// fallback.
		"/v0/oauth/authorize",
	}
	for _, p := range notExempt {
		if csrfExemptPath(p) {
			t.Errorf("%s should NOT be exempt", p)
		}
	}
}

// signInWithSession registers a fake GitHub identity, walks the
// session helper, and returns the cookies a real browser would have
// after a successful OAuth round-trip: session + CSRF.
func signInWithSession(t *testing.T) (s *Server, sessCookie, csrfCookie *http.Cookie) {
	t.Helper()
	srv, repo := newAuthServer(t)
	_, sess, err := repo.SignIn(context.Background(), "github", auth.GitHubProfile{
		ID: 99, Login: "csrf-tester", Name: "Tester",
	}, uuid.New())
	if err != nil {
		t.Fatalf("SignIn: %v", err)
	}
	csrfTok, err := generateCSRFToken()
	if err != nil {
		t.Fatalf("generateCSRFToken: %v", err)
	}
	return srv,
		&http.Cookie{Name: auth.SessionCookieName, Value: sess.PlainText},
		&http.Cookie{Name: CSRFCookieName, Value: csrfTok}
}

func TestCSRF_GETBypasses(t *testing.T) {
	s, sessCookie, _ := signInWithSession(t)
	req := httptest.NewRequest(http.MethodGet, "/v0/auth/me", nil)
	req.AddCookie(sessCookie)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("GET /v0/auth/me with no CSRF header: status = %d, want 200", w.Code)
	}
}

func TestCSRF_AnonymousPOSTBypasses(t *testing.T) {
	// No identity → middleware does NOT 403; the handler 401s.
	// This protects "POST returns 401 (auth_required)" semantics
	// for unauthenticated callers, who don't need a CSRF token to
	// learn they're not signed in.
	srv, _ := newAuthServer(t)
	req := httptest.NewRequest(http.MethodPost, "/v0/auth/logout", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("anonymous POST: status = %d, want 401 (handler), not 403 (csrf)", w.Code)
	}
}

func TestCSRF_SessionCookiePOSTWithoutHeaderRejected(t *testing.T) {
	s, sessCookie, csrfCookie := signInWithSession(t)
	req := httptest.NewRequest(http.MethodPost, "/v0/auth/logout", nil)
	req.AddCookie(sessCookie)
	req.AddCookie(csrfCookie)
	// Note: no X-CSRF-Token header.
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
	if !strings.Contains(w.Body.String(), "csrf_required") {
		t.Errorf("body missing csrf_required: %s", w.Body.String())
	}
}

func TestCSRF_SessionCookiePOSTWithMismatchedHeaderRejected(t *testing.T) {
	s, sessCookie, csrfCookie := signInWithSession(t)
	req := httptest.NewRequest(http.MethodPost, "/v0/auth/logout", nil)
	req.AddCookie(sessCookie)
	req.AddCookie(csrfCookie)
	req.Header.Set(CSRFHeaderName, "different-value")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

func TestCSRF_SessionCookiePOSTWithoutCookieRejected(t *testing.T) {
	// Header present, cookie missing — happens when the user has a
	// pre-CSRF-deploy session and JS sends a header it didn't read.
	s, sessCookie, csrfCookie := signInWithSession(t)
	req := httptest.NewRequest(http.MethodPost, "/v0/auth/logout", nil)
	req.AddCookie(sessCookie)
	req.Header.Set(CSRFHeaderName, csrfCookie.Value)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

func TestCSRF_SessionCookiePOSTWithMatchingHeaderPasses(t *testing.T) {
	s, sessCookie, csrfCookie := signInWithSession(t)
	req := httptest.NewRequest(http.MethodPost, "/v0/auth/logout", nil)
	req.AddCookie(sessCookie)
	req.AddCookie(csrfCookie)
	req.Header.Set(CSRFHeaderName, csrfCookie.Value)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204:\n%s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// OAuth AS CSRF: form-field fallback, CONDITION 1 (mint-on-GET) and
// CONDITION 3 (bounded body) — ADR-076 slice 3 / #2436.
// ---------------------------------------------------------------------------

// newOAuthStackServer signs a user in through the real fake auth repo (so the
// session cookie authenticates through the middleware) and wires the AS.
func newOAuthStackServer(t *testing.T) (*Server, *http.Cookie) {
	t.Helper()
	store := newFakeOAuthStore()
	store.seedClient(storeClient("github", "client-x", []string{"https://app.example/cb"}))
	repo := newFakeAuthRepo()
	_, sess, err := repo.SignIn(context.Background(), "github", auth.GitHubProfile{ID: 7, Login: "octocat"}, uuid.New())
	if err != nil {
		t.Fatalf("SignIn: %v", err)
	}
	srv := New(Config{OAuthASIssuer: testIssuer, OAuthStore: store, OAuthCIMDFetcher: newCIMDFetcher(newCIMD()), AuthRepo: repo})
	return srv, &http.Cookie{Name: auth.SessionCookieName, Value: sess.PlainText}
}

// consentGETMintedCSRF drives the consent GET (session cookie, NO csrf cookie)
// through the full stack and returns the minted __Host-csrf cookie value —
// modelling the SameSite=Strict arrival CONDITION 1 fixes.
func consentGETMintedCSRF(t *testing.T, srv *Server, sess *http.Cookie) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v0/oauth/authorize?"+authorizeQuery(nil), nil)
	req.AddCookie(sess) // Lax session cookie IS sent; Strict CSRF cookie is NOT
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("consent GET: status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == CSRFCookieName && c.Value != "" {
			return c.Value
		}
	}
	t.Fatal("consent GET did not mint a __Host-csrf cookie (CONDITION 1)")
	return ""
}

func TestCSRF_TokenEndpointExempt(t *testing.T) {
	srv, sess := newOAuthStackServer(t)
	// A session-authenticated POST to /token with NO CSRF header/cookie must
	// NOT be blocked by CSRF (the token endpoint is exempt); it reaches the
	// handler, which ignores the session.
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", "client-x")
	req := httptest.NewRequest(http.MethodPost, "/v0/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(sess)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if strings.Contains(w.Body.String(), "csrf_required") {
		t.Fatalf("token endpoint was CSRF-blocked: %s", w.Body.String())
	}
}

// TestCSRF_ConsentAcceptsFormFieldFallback is the CONDITION 1 proof: a GET
// carrying only the session cookie mints a CSRF cookie, and the POST using that
// Set-Cookie value SUCCEEDS. A 403-asserting test proves only fail-safe.
func TestCSRF_ConsentAcceptsFormFieldFallback(t *testing.T) {
	srv, sess := newOAuthStackServer(t)
	tok := consentGETMintedCSRF(t, srv, sess)

	form := consentForm(nil)
	form.Set("csrf_token", tok)
	req := httptest.NewRequest(http.MethodPost, "/v0/oauth/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(sess)
	req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: tok})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("consent POST: status = %d, want 302; body=%s", w.Code, w.Body.String())
	}
	loc, _ := url.Parse(w.Header().Get("Location"))
	if loc.Query().Get("code") == "" {
		t.Fatal("consent POST redirected without a code")
	}
}

func TestCSRF_FormFallbackPreservesBodyForHandler(t *testing.T) {
	srv, sess := newOAuthStackServer(t)
	tok := consentGETMintedCSRF(t, srv, sess)
	form := consentForm(nil)
	form.Set("csrf_token", tok)
	req := httptest.NewRequest(http.MethodPost, "/v0/oauth/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(sess)
	req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: tok})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	// A 302 with a code proves the downstream handler still saw the full form
	// (client_id, redirect_uri, scope, ...) after the middleware read the body.
	if w.Code != http.StatusFound || w.Header().Get("Location") == "" {
		t.Fatalf("handler did not see the restored form: status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestCSRF_ConsentFormFieldMismatchRefused(t *testing.T) {
	srv, sess := newOAuthStackServer(t)
	tok := consentGETMintedCSRF(t, srv, sess)
	form := consentForm(nil)
	form.Set("csrf_token", "wrong-token")
	req := httptest.NewRequest(http.MethodPost, "/v0/oauth/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(sess)
	req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: tok})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}

func TestCSRF_ConsentMissingFormFieldRefused(t *testing.T) {
	srv, sess := newOAuthStackServer(t)
	tok := consentGETMintedCSRF(t, srv, sess)
	form := consentForm(nil) // no csrf_token field
	req := httptest.NewRequest(http.MethodPost, "/v0/oauth/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(sess)
	req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: tok})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}

// TestCSRF_FormFallbackIsPathScoped is the counterfactual for the exact-match
// allowlist: a DIFFERENT state-changing route carrying only a form field still
// 403s (widening the fallback to all paths would redden this).
func TestCSRF_FormFallbackIsPathScoped(t *testing.T) {
	srv, sess := newOAuthStackServer(t)
	tok := consentGETMintedCSRF(t, srv, sess)
	form := url.Values{}
	form.Set("csrf_token", tok) // a form field, but /logout is not a fallback path
	req := httptest.NewRequest(http.MethodPost, "/v0/auth/logout", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(sess)
	req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: tok})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (form-field fallback must be path-scoped)", w.Code)
	}
}

func TestCSRF_HeaderPathUnchanged(t *testing.T) {
	srv, sess := newOAuthStackServer(t)
	tok := consentGETMintedCSRF(t, srv, sess)
	// The header path still works on the consent POST (no form field needed):
	// when a header is present the middleware uses it and skips the fallback.
	form := consentForm(nil)
	req := httptest.NewRequest(http.MethodPost, "/v0/oauth/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set(CSRFHeaderName, tok)
	req.AddCookie(sess)
	req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: tok})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("header-based CSRF on consent POST: status = %d, want 302; body=%s", w.Code, w.Body.String())
	}
}

// TestCSRF_ConsentOversizedBodyRejected is CONDITION 3: an oversized consent
// body is refused before the handler runs, bounding the allocation.
func TestCSRF_ConsentOversizedBodyRejected(t *testing.T) {
	srv, sess := newOAuthStackServer(t)
	tok := consentGETMintedCSRF(t, srv, sess)
	form := url.Values{}
	form.Set("filler", strings.Repeat("a", (1<<20)+4096)) // > 1 MiB
	req := httptest.NewRequest(http.MethodPost, "/v0/oauth/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(sess)
	req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: tok})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", w.Code)
	}
}
