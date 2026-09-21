package server

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

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
// OAuth AS CSRF: the session-bound signed csrf_token form field (#2442) and
// CONDITION 3 (bounded body) — ADR-076 slice 3 / #2436.
//
// Every test here drives srv.Handler() so recovery → bearerAuth → csrf → mux →
// handleOAuthAuthorize/handleOAuthConsent → consent template are crossed in
// one test. httptest does not model SameSite, so the cross-site arrival is
// modelled by simply not attaching a __Host-csrf cookie.
// ---------------------------------------------------------------------------

// newOAuthStackServer signs a user in through the real fake auth repo (so the
// session cookie authenticates through the middleware) and wires the AS.
func newOAuthStackServer(t *testing.T) (*Server, *http.Cookie) {
	t.Helper()
	srv, sess, _ := newOAuthStackServerWithRepo(t)
	return srv, sess
}

// newOAuthStackServerWithRepo is newOAuthStackServer also returning the fake
// auth repo, for tests that need a SECOND signed-in session on the same server.
func newOAuthStackServerWithRepo(t *testing.T) (*Server, *http.Cookie, *fakeAuthRepo) {
	t.Helper()
	store := newFakeOAuthStore()
	store.seedClient(storeClient("github", "client-x", []string{"https://app.example/cb"}))
	repo := newFakeAuthRepo()
	_, sess, err := repo.SignIn(context.Background(), "github", auth.GitHubProfile{ID: 7, Login: "octocat"}, uuid.New())
	if err != nil {
		t.Fatalf("SignIn: %v", err)
	}
	srv := New(Config{OAuthASIssuer: testIssuer, OAuthStore: store, OAuthCIMDFetcher: newCIMDFetcher(newCIMD()), AuthRepo: repo})
	return srv, &http.Cookie{Name: auth.SessionCookieName, Value: sess.PlainText}, repo
}

var consentFormTokenRE = regexp.MustCompile(`name="csrf_token" value="([^"]+)"`)

// consentFormTokenFromBody extracts the hidden csrf_token from a rendered
// consent page, or "" when absent.
func consentFormTokenFromBody(t *testing.T, body string) string {
	t.Helper()
	m := consentFormTokenRE.FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return m[1]
}

// consentGETFormToken drives the consent GET (session cookie, NO csrf cookie —
// the SameSite=Strict arrival) through the full stack and returns the hidden
// csrf_token embedded in the rendered page.
func consentGETFormToken(t *testing.T, srv *Server, sess *http.Cookie) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v0/oauth/authorize?"+authorizeQuery(nil), nil)
	req.AddCookie(sess) // Lax session cookie IS sent; Strict CSRF cookie is NOT
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("consent GET: status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	tok := consentFormTokenFromBody(t, w.Body.String())
	if tok == "" {
		t.Fatal("consent GET rendered no csrf_token form field")
	}
	return tok
}

// postConsentForm submits form as the consent POST with the session cookie and
// NO __Host-csrf cookie (the form path must succeed without one) plus any
// extra cookies the caller wants attached.
func postConsentForm(srv *Server, sess *http.Cookie, form url.Values, extra ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v0/oauth/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(sess)
	for _, c := range extra {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

func assertConsentApproved(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusFound {
		t.Fatalf("consent POST: status = %d, want 302; body=%s", w.Code, w.Body.String())
	}
	loc, _ := url.Parse(w.Header().Get("Location"))
	if loc.Query().Get("code") == "" {
		t.Fatalf("consent POST redirected without a code: %s", w.Header().Get("Location"))
	}
}

func assertCSRFRefused(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "csrf_required") {
		t.Fatalf("status = %d, want 403 csrf_required; body=%s", w.Code, w.Body.String())
	}
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

// TestCSRF_ConsentAcceptsFormFieldFallback: the form token alone, with NO
// __Host-csrf cookie on the POST, authorizes the consent. A 403-asserting test
// proves only fail-safe; this is the success proof.
func TestCSRF_ConsentAcceptsFormFieldFallback(t *testing.T) {
	srv, sess := newOAuthStackServer(t)
	tok := consentGETFormToken(t, srv, sess)
	form := consentForm(nil)
	form.Set("csrf_token", tok)
	assertConsentApproved(t, postConsentForm(srv, sess, form))
}

// TestCSRF_ConcurrentConsentFlows_FirstFormSucceeds is the #2442 obligation: two
// consent pages rendered for the same session, and the FIRST one's form still
// submits after the second render (the pre-change cookie double-submit 403'd
// it, because the second GET overwrote the single __Host-csrf slot). The second
// form submits too — the tokens are independent.
func TestCSRF_ConcurrentConsentFlows_FirstFormSucceeds(t *testing.T) {
	srv, sess := newOAuthStackServer(t)
	tokA := consentGETFormToken(t, srv, sess) // tab A
	tokB := consentGETFormToken(t, srv, sess) // tab B
	if tokA == tokB {
		t.Fatal("two consent renders embedded the same token")
	}
	formA := consentForm(nil)
	formA.Set("csrf_token", tokA)
	assertConsentApproved(t, postConsentForm(srv, sess, formA))
	formB := consentForm(nil)
	formB.Set("csrf_token", tokB)
	assertConsentApproved(t, postConsentForm(srv, sess, formB))
}

func TestCSRF_FormFallbackPreservesBodyForHandler(t *testing.T) {
	srv, sess := newOAuthStackServer(t)
	tok := consentGETFormToken(t, srv, sess)
	form := consentForm(nil)
	form.Set("csrf_token", tok)
	w := postConsentForm(srv, sess, form)
	// A 302 with a code proves the downstream handler still saw the full form
	// (client_id, redirect_uri, scope, ...) after the middleware read the body.
	if w.Code != http.StatusFound || w.Header().Get("Location") == "" {
		t.Fatalf("handler did not see the restored form: status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestCSRF_ConsentFormFieldMismatchRefused(t *testing.T) {
	srv, sess := newOAuthStackServer(t)
	_ = consentGETFormToken(t, srv, sess)
	form := consentForm(nil)
	form.Set("csrf_token", "wrong-token")
	assertCSRFRefused(t, postConsentForm(srv, sess, form))
}

func TestCSRF_ConsentMissingFormFieldRefused(t *testing.T) {
	srv, sess := newOAuthStackServer(t)
	_ = consentGETFormToken(t, srv, sess)
	form := consentForm(nil) // no csrf_token field
	assertCSRFRefused(t, postConsentForm(srv, sess, form))
}

// TestCSRF_ConsentTokenTamperedRefused: flipping one byte of the decoded MAC
// and re-encoding is refused (RED when the ConstantTimeCompare check is
// deleted).
func TestCSRF_ConsentTokenTamperedRefused(t *testing.T) {
	srv, sess := newOAuthStackServer(t)
	tok := consentGETFormToken(t, srv, sess)
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	raw[len(raw)-1] ^= 0x01
	form := consentForm(nil)
	form.Set("csrf_token", base64.RawURLEncoding.EncodeToString(raw))
	assertCSRFRefused(t, postConsentForm(srv, sess, form))
}

// TestCSRF_ConsentTokenNotBoundToSessionRefused: a token minted for session A
// is refused when submitted under session B's cookie (RED when the key ignores
// the session).
func TestCSRF_ConsentTokenNotBoundToSessionRefused(t *testing.T) {
	srv, sessA, repo := newOAuthStackServerWithRepo(t)
	tokA := consentGETFormToken(t, srv, sessA)
	// A second signed-in session on the SAME server.
	_, sessBRow, err := repo.SignIn(context.Background(), "github", auth.GitHubProfile{ID: 8, Login: "hubot"}, uuid.New())
	if err != nil {
		t.Fatalf("SignIn B: %v", err)
	}
	sessB := &http.Cookie{Name: auth.SessionCookieName, Value: sessBRow.PlainText}
	form := consentForm(nil)
	form.Set("csrf_token", tokA)
	assertCSRFRefused(t, postConsentForm(srv, sessB, form))
	// Control: the same token under its own session is accepted.
	assertConsentApproved(t, postConsentForm(srv, sessA, form))
}

// TestCSRF_ConsentTokenExpiredRefused: the middleware reads s.nowFunc at REQUEST
// time, so advancing the clock past consentCSRFTokenTTL refuses a token a
// within-TTL sibling still accepts (RED when the TTL check is deleted).
func TestCSRF_ConsentTokenExpiredRefused(t *testing.T) {
	srv, sess := newOAuthStackServer(t)
	base := time.Now()
	srv.nowFunc = func() time.Time { return base }
	tok := consentGETFormToken(t, srv, sess)
	form := consentForm(nil)
	form.Set("csrf_token", tok)

	srv.nowFunc = func() time.Time { return base.Add(consentCSRFTokenTTL + time.Second) }
	assertCSRFRefused(t, postConsentForm(srv, sess, form))

	// Within-TTL control: a token minted now under the advanced clock is fine.
	tok2 := consentGETFormToken(t, srv, sess)
	form2 := consentForm(nil)
	form2.Set("csrf_token", tok2)
	assertConsentApproved(t, postConsentForm(srv, sess, form2))
}

// TestCSRF_ConsentCookieValueInFormRefused pins that the form branch is
// signature-only: the OLD protocol — a generateCSRFToken value in the form
// field with the matching __Host-csrf cookie attached — is refused (RED if the
// cookie-compare fallback is re-added).
func TestCSRF_ConsentCookieValueInFormRefused(t *testing.T) {
	srv, sess := newOAuthStackServer(t)
	cookieTok, err := generateCSRFToken()
	if err != nil {
		t.Fatalf("generateCSRFToken: %v", err)
	}
	form := consentForm(nil)
	form.Set("csrf_token", cookieTok)
	assertCSRFRefused(t, postConsentForm(srv, sess, form, &http.Cookie{Name: CSRFCookieName, Value: cookieTok}))
}

// TestCSRF_AnonymousConsentPostIs401NotCSRF (approval condition 1): the
// anonymous bypass precedes the form branch, so a form-encoded POST carrying a
// csrf_token but NO session cookie reaches the handler and is refused 401
// auth_required — never 403 csrf_required.
func TestCSRF_AnonymousConsentPostIs401NotCSRF(t *testing.T) {
	srv, sess := newOAuthStackServer(t)
	tok := consentGETFormToken(t, srv, sess)
	form := consentForm(nil)
	form.Set("csrf_token", tok)
	req := httptest.NewRequest(http.MethodPost, "/v0/oauth/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// No cookies at all.
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "auth_required") {
		t.Fatalf("anonymous consent POST: status = %d, want 401 auth_required; body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "csrf_required") {
		t.Fatalf("anonymous consent POST was CSRF-refused instead of 401: %s", w.Body.String())
	}
}

// TestCSRF_FormFallbackIsPathScoped is the counterfactual for the exact-match
// allowlist: a DIFFERENT state-changing route carrying only a form field still
// 403s (widening the fallback to all paths would redden this).
func TestCSRF_FormFallbackIsPathScoped(t *testing.T) {
	srv, sess := newOAuthStackServer(t)
	tok := consentGETFormToken(t, srv, sess)
	form := url.Values{}
	form.Set("csrf_token", tok) // a valid form token, but /logout is not a fallback path
	req := httptest.NewRequest(http.MethodPost, "/v0/auth/logout", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(sess)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (form-field fallback must be path-scoped)", w.Code)
	}
}

// TestCSRF_HeaderPathUnchanged (approval condition 3): the header path
// X-CSRF-Token ≡ __Host-csrf still works on the consent POST with no form
// field; when a header is present the middleware never enters the form branch.
func TestCSRF_HeaderPathUnchanged(t *testing.T) {
	srv, sess := newOAuthStackServer(t)
	tok, err := generateCSRFToken()
	if err != nil {
		t.Fatalf("generateCSRFToken: %v", err)
	}
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
	form := url.Values{}
	form.Set("filler", strings.Repeat("a", (1<<20)+4096)) // > 1 MiB
	w := postConsentForm(srv, sess, form)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", w.Code)
	}
}
