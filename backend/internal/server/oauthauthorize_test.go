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
	"github.com/kuhlman-labs/fishhawk/backend/internal/oauthas"
	"github.com/kuhlman-labs/fishhawk/backend/internal/oauthstore"
)

func testPKCE() (verifier, challenge string) {
	verifier = strings.Repeat("a", 43)
	return verifier, oauthas.DeriveS256Challenge(verifier)
}

// authorizeQuery builds the base valid authorize query, applying overrides. A
// value of "\x00" removes the key entirely.
func authorizeQuery(overrides map[string]string) string {
	_, challenge := testPKCE()
	v := url.Values{}
	v.Set("client_id", "client-x")
	v.Set("redirect_uri", "https://app.example/cb")
	v.Set("response_type", "code")
	v.Set("code_challenge", challenge)
	v.Set("code_challenge_method", "S256")
	v.Set("resource", testResource)
	v.Set("scope", "read:runs")
	v.Set("state", "st-123")
	for k, val := range overrides {
		if val == "\x00" {
			v.Del(k)
			continue
		}
		v.Set(k, val)
	}
	return v.Encode()
}

// seedAuthorizeClient registers the standard store client-x with redirect
// https://app.example/cb, and returns an AS-enabled server plus the CIMD RT.
func newAuthorizeServer(t *testing.T) (*Server, *fakeOAuthStore, *cimdRoundTripper) {
	t.Helper()
	store := newFakeOAuthStore()
	store.seedClient(storeClient("github", "client-x", []string{"https://app.example/cb"}))
	rt := newCIMD()
	srv := newEnabledOAuthServer(store, newCIMDFetcher(rt))
	return srv, store, rt
}

func getAuthorize(srv *Server, query string, id *Identity) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/v0/oauth/authorize?"+query, nil)
	if id != nil {
		req = req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, *id))
	}
	rr := httptest.NewRecorder()
	srv.handleOAuthAuthorize(rr, req)
	return rr
}

func redirectQuery(t *testing.T, rr *httptest.ResponseRecorder) url.Values {
	t.Helper()
	if rr.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body=%s", rr.Code, rr.Body.String())
	}
	u, err := url.Parse(rr.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	return u.Query()
}

func TestAuthorize_UnredirectableErrorsRenderInPlace(t *testing.T) {
	srv, _, _ := newAuthorizeServer(t)
	cases := []struct {
		name  string
		query string
	}{
		{"unresolvable-client", authorizeQuery(map[string]string{"client_id": "not-a-url"})},
		{"redirect-mismatch", authorizeQuery(map[string]string{"redirect_uri": "https://evil.example/cb"})},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rr := getAuthorize(srv, c.query, nil)
			if rr.Code == http.StatusFound {
				t.Fatalf("error was redirected (%q); §4.1.2.1 forbids it", rr.Header().Get("Location"))
			}
			if strings.Contains(rr.Header().Get("Location"), "evil.example") {
				t.Fatal("response redirected to the attacker URI")
			}
		})
	}
}

func TestAuthorize_UnsupportedResponseTypeIsRedirected(t *testing.T) {
	srv, _, _ := newAuthorizeServer(t)
	rr := getAuthorize(srv, authorizeQuery(map[string]string{"response_type": "token"}), nil)
	q := redirectQuery(t, rr)
	if q.Get("error") != "unsupported_response_type" {
		t.Fatalf("error = %q, want unsupported_response_type", q.Get("error"))
	}
}

func TestAuthorize_RedirectedErrorsCarryStateAndIss(t *testing.T) {
	srv, _, _ := newAuthorizeServer(t)
	cases := []struct {
		name      string
		overrides map[string]string
		wantErr   string
	}{
		{"unsupported_response_type", map[string]string{"response_type": "token"}, "unsupported_response_type"},
		{"missing_code_challenge", map[string]string{"code_challenge": "\x00"}, "invalid_request"},
		{"plain_method", map[string]string{"code_challenge_method": "plain"}, "invalid_request"},
		{"absent_method", map[string]string{"code_challenge_method": "\x00"}, "invalid_request"},
		{"malformed_challenge", map[string]string{"code_challenge": "short"}, "invalid_request"},
		{"missing_resource", map[string]string{"resource": "\x00"}, "invalid_target"},
		{"mismatched_resource", map[string]string{"resource": "https://other.example/mcp"}, "invalid_target"},
		{"unsupported_scope", map[string]string{"scope": "bogus:scope"}, "invalid_scope"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rr := getAuthorize(srv, authorizeQuery(c.overrides), nil)
			q := redirectQuery(t, rr)
			if q.Get("error") != c.wantErr {
				t.Errorf("error = %q, want %q", q.Get("error"), c.wantErr)
			}
			if q.Get("state") != "st-123" {
				t.Errorf("state = %q, want st-123 (byte-exact echo)", q.Get("state"))
			}
			if q.Get("iss") != testIssuer {
				t.Errorf("iss = %q, want %q", q.Get("iss"), testIssuer)
			}
		})
	}
}

func TestAuthorize_AnonymousRedirectsToForgeSignInCarryingNext(t *testing.T) {
	store := newFakeOAuthStore()
	store.seedClient(storeClient("github", "client-x", []string{"https://app.example/cb"}))
	_, gh := stubGitHubOAuthServer(t)
	srv := New(Config{
		OAuthASIssuer:    testIssuer,
		OAuthStore:       store,
		OAuthCIMDFetcher: newCIMDFetcher(newCIMD()),
		GitHubOAuth:      gh,
	})
	rr := getAuthorize(srv, authorizeQuery(nil), nil) // anonymous
	if rr.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rr.Code)
	}
	loc := rr.Header().Get("Location")
	if !strings.HasPrefix(loc, "/v0/auth/github/login?next=") {
		t.Fatalf("Location = %q, want forge sign-in with next", loc)
	}
	if !strings.Contains(loc, url.QueryEscape("/v0/oauth/authorize")) {
		t.Fatalf("next does not carry the authorize path: %q", loc)
	}
}

func TestAuthorize_ValidationPrecedesSignIn(t *testing.T) {
	store := newFakeOAuthStore()
	store.seedClient(storeClient("github", "client-x", []string{"https://app.example/cb"}))
	_, gh := stubGitHubOAuthServer(t)
	srv := New(Config{OAuthASIssuer: testIssuer, OAuthStore: store, OAuthCIMDFetcher: newCIMDFetcher(newCIMD()), GitHubOAuth: gh})
	// Signed out AND a bad scope: must get the RFC error redirect at the
	// client, NOT a sign-in redirect.
	rr := getAuthorize(srv, authorizeQuery(map[string]string{"scope": "bogus:scope"}), nil)
	q := redirectQuery(t, rr)
	if q.Get("error") != "invalid_scope" {
		t.Fatalf("expected invalid_scope error redirect, got %q (Location=%s)", q.Get("error"), rr.Header().Get("Location"))
	}
}

func TestAuthorize_SignInUnconfiguredReturns503(t *testing.T) {
	srv, _, _ := newAuthorizeServer(t) // no GitHubOAuth
	rr := getAuthorize(srv, authorizeQuery(nil), nil)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "oauth_unconfigured") {
		t.Fatalf("body = %q, want oauth_unconfigured", rr.Body.String())
	}
}

// signedInIdentity seeds a user in repo and returns the matching session
// identity.
func signedInIdentity(repo *fakeAuthRepo, provider string) Identity {
	uid := uuid.New()
	repo.mu.Lock()
	repo.users[uid.String()] = &auth.User{ID: uid.String(), Provider: provider, GitHubLogin: "octocat"}
	repo.mu.Unlock()
	return Identity{
		Subject:   "github:octocat",
		UserID:    uid.String(),
		SessionID: uuid.New().String(),
		AccountID: uuid.New().String(),
	}
}

func TestAuthorize_ConsentPageNamesClientResourceAndScopes(t *testing.T) {
	store := newFakeOAuthStore()
	c := storeClient("github", "client-x", []string{"https://app.example/cb"})
	c.ClientName = "My Cool App"
	store.seedClient(c)
	repo := newFakeAuthRepo()
	srv := New(Config{OAuthASIssuer: testIssuer, OAuthStore: store, OAuthCIMDFetcher: newCIMDFetcher(newCIMD()), AuthRepo: repo})
	id := signedInIdentity(repo, "github")
	rr := getAuthorize(srv, authorizeQuery(nil), &id)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{"My Cool App", testResource, "read:runs"} {
		if !strings.Contains(body, want) {
			t.Errorf("consent page missing %q", want)
		}
	}
}

func TestAuthorize_ConsentPageEscapesClientSuppliedMetadata(t *testing.T) {
	store := newFakeOAuthStore()
	rt := newCIMD()
	clientID := "https://client.example/cimd"
	// A CIMD document whose client_name carries markup.
	doc := `{"client_id":"` + clientID + `","redirect_uris":["https://client.example/cb"],"token_endpoint_auth_method":"none","client_name":"<script>alert(1)</script>"}`
	rt.docs[clientID] = doc
	repo := newFakeAuthRepo()
	srv := New(Config{OAuthASIssuer: testIssuer, OAuthStore: store, OAuthCIMDFetcher: newCIMDFetcher(rt), AuthRepo: repo})
	id := signedInIdentity(repo, "github")
	q := authorizeQuery(map[string]string{"client_id": clientID, "redirect_uri": "https://client.example/cb"})
	rr := getAuthorize(srv, q, &id)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "<script>alert(1)</script>") {
		t.Fatal("client_name rendered unescaped — XSS")
	}
	if !strings.Contains(rr.Body.String(), "&lt;script&gt;") {
		t.Fatal("expected escaped client_name markup")
	}
}

// ---------------------------------------------------------------------------
// Consent POST.
// ---------------------------------------------------------------------------

// postConsent drives the consent POST DIRECTLY on the handler (CSRF middleware
// bypassed — CSRF is exercised in csrf_test.go). id nil means anonymous.
func postConsent(srv *Server, form url.Values, id *Identity) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v0/oauth/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if id != nil {
		req = req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, *id))
	}
	rr := httptest.NewRecorder()
	srv.handleOAuthConsent(rr, req)
	return rr
}

func consentForm(overrides map[string]string) url.Values {
	q := authorizeQuery(overrides)
	v, _ := url.ParseQuery(q)
	v.Set("action", "approve")
	for k, val := range overrides {
		if val == "\x00" {
			v.Del(k)
			continue
		}
		v.Set(k, val)
	}
	return v
}

func newConsentServer(t *testing.T) (*Server, *fakeOAuthStore, *fakeAuthRepo) {
	t.Helper()
	store := newFakeOAuthStore()
	store.seedClient(storeClient("github", "client-x", []string{"https://app.example/cb"}))
	repo := newFakeAuthRepo()
	srv := New(Config{OAuthASIssuer: testIssuer, OAuthStore: store, OAuthCIMDFetcher: newCIMDFetcher(newCIMD()), AuthRepo: repo})
	return srv, store, repo
}

func TestAuthorizeConsent_ApprovalRedirectCarriesCodeStateAndIss(t *testing.T) {
	srv, _, repo := newConsentServer(t)
	id := signedInIdentity(repo, "github")
	rr := postConsent(srv, consentForm(nil), &id)
	q := redirectQuery(t, rr)
	if q.Get("code") == "" {
		t.Fatal("approval redirect carried no code")
	}
	if q.Get("state") != "st-123" {
		t.Errorf("state = %q", q.Get("state"))
	}
	if q.Get("iss") != testIssuer {
		t.Errorf("iss = %q", q.Get("iss"))
	}
}

func TestAuthorizeConsent_DenialRedirectsAccessDeniedWithStateAndIss(t *testing.T) {
	srv, _, repo := newConsentServer(t)
	id := signedInIdentity(repo, "github")
	form := consentForm(nil)
	form.Set("action", "deny")
	rr := postConsent(srv, form, &id)
	q := redirectQuery(t, rr)
	if q.Get("error") != "access_denied" {
		t.Fatalf("error = %q, want access_denied", q.Get("error"))
	}
	if q.Get("state") != "st-123" || q.Get("iss") != testIssuer {
		t.Errorf("state/iss = %q/%q", q.Get("state"), q.Get("iss"))
	}
}

func TestAuthorizeConsent_DenialBeforeClientValidatesDoesNotRedirect(t *testing.T) {
	srv, _, repo := newConsentServer(t)
	id := signedInIdentity(repo, "github")
	form := consentForm(map[string]string{"client_id": "not-a-url"})
	form.Set("action", "deny")
	rr := postConsent(srv, form, &id)
	if rr.Code == http.StatusFound {
		t.Fatalf("a denial before client validates must not redirect (Location=%q)", rr.Header().Get("Location"))
	}
}

func TestAuthorizeConsent_RevalidatesPostedParametersNotTrustingHiddenFields(t *testing.T) {
	srv, store, repo := newConsentServer(t)
	id := signedInIdentity(repo, "github")
	// Tamper the redirect_uri to an unregistered value: must be refused in
	// place, and NO code minted.
	form := consentForm(map[string]string{"redirect_uri": "https://evil.example/cb"})
	rr := postConsent(srv, form, &id)
	if rr.Code == http.StatusFound {
		t.Fatalf("tampered redirect_uri was accepted (Location=%q)", rr.Header().Get("Location"))
	}
	store.mu.Lock()
	n := len(store.codes)
	store.mu.Unlock()
	if n != 0 {
		t.Fatalf("a code was minted for a tampered request: %d codes", n)
	}
}

func TestAuthorizeConsent_AnonymousRefused(t *testing.T) {
	srv, store, _ := newConsentServer(t)
	rr := postConsent(srv, consentForm(nil), nil)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	store.mu.Lock()
	n := len(store.codes)
	store.mu.Unlock()
	if n != 0 {
		t.Fatal("anonymous consent minted a code")
	}
}

func TestAuthorizeConsent_MintedCodeRecordsResourceScopesSubjectProvider(t *testing.T) {
	store := newFakeOAuthStore()
	store.seedClient(storeClient("github", "client-x", []string{"https://app.example/cb"}))
	repo := newFakeAuthRepo()
	srv := New(Config{OAuthASIssuer: testIssuer, OAuthStore: store, OAuthCIMDFetcher: newCIMDFetcher(newCIMD()), AuthRepo: repo})
	// A GitLab user: provider must come from auth.User.Provider, NOT the
	// "github:"-prefixed subject.
	id := signedInIdentity(repo, "gitlab")
	rr := postConsent(srv, consentForm(nil), &id)
	_ = redirectQuery(t, rr)

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.codes) != 1 {
		t.Fatalf("expected 1 minted code, got %d", len(store.codes))
	}
	var code *oauthstore.AuthorizationCode
	for _, c := range store.codes {
		code = c
	}
	if code.Resource != testResource {
		t.Errorf("resource = %q, want %q", code.Resource, testResource)
	}
	if !equalStrings(code.Scopes, []string{"read:runs"}) {
		t.Errorf("scopes = %v", code.Scopes)
	}
	if code.Subject != "github:octocat" {
		t.Errorf("subject = %q", code.Subject)
	}
	if code.Provider != "gitlab" {
		t.Errorf("provider = %q, want gitlab (from auth.User.Provider, not the subject prefix)", code.Provider)
	}
	if code.AccountID != id.AccountID {
		t.Errorf("account_id = %q, want %q", code.AccountID, id.AccountID)
	}
}

// TestAuthorizeConsent_EnforcesRegisteredResponseType is the CONDITION 2
// authorize-side check: a client whose registered response_types excludes code
// is refused unauthorized_client.
func TestAuthorizeConsent_EnforcesRegisteredResponseType(t *testing.T) {
	store := newFakeOAuthStore()
	c := storeClient("github", "client-x", []string{"https://app.example/cb"})
	c.ResponseTypes = []string{"id_token"} // does NOT include "code"
	store.seedClient(c)
	srv := newEnabledOAuthServer(store, newCIMDFetcher(newCIMD()))
	rr := getAuthorize(srv, authorizeQuery(nil), nil)
	q := redirectQuery(t, rr)
	if q.Get("error") != "unauthorized_client" {
		t.Fatalf("error = %q, want unauthorized_client", q.Get("error"))
	}
}

// TestAuthorize_ClaudeCodeShapedDocumentAuthorizes pins the CONDITION 2
// portless-landmine fixture: a document with grant_types present but
// response_types absent and scope absent must authorize (absent fields take
// their RFC 7591 defaults).
func TestAuthorize_ClaudeCodeShapedDocumentAuthorizes(t *testing.T) {
	store := newFakeOAuthStore()
	rt := newCIMD()
	clientID := "https://claude.example/cimd"
	rt.docs[clientID] = cimdDoc(clientID, []string{"https://claude.example/cb"},
		[]string{"authorization_code", "refresh_token"}, nil, "") // response_types + scope ABSENT
	repo := newFakeAuthRepo()
	srv := New(Config{OAuthASIssuer: testIssuer, OAuthStore: store, OAuthCIMDFetcher: newCIMDFetcher(rt), AuthRepo: repo})
	id := signedInIdentity(repo, "github")
	q := authorizeQuery(map[string]string{"client_id": clientID, "redirect_uri": "https://claude.example/cb"})
	rr := getAuthorize(srv, q, &id)
	if rr.Code != http.StatusOK {
		t.Fatalf("Claude Code-shaped document must authorize; status=%d body=%s", rr.Code, rr.Body.String())
	}
}

// TestAuthorize_EnforcesRegisteredScope is the registration-driven scope
// boundary (the sibling of the response_types/grant_types checks): a client the
// operator registered narrow cannot request a scope outside its set, while an
// ABSENT registered scope imposes no restriction — the documented
// absent-vs-empty distinction. Without enforcement a narrow-registered client
// could obtain any operator scope the user consents to, widening the authority
// baked into the code and every derived token.
func TestAuthorize_EnforcesRegisteredScope(t *testing.T) {
	// Exceeds the registered set -> invalid_scope. This runs in the redirect
	// phase (client + redirect URI already matched), so no identity is needed to
	// observe the refusal. Deleting the enforcement makes this authorize instead.
	t.Run("exceeds registered scope", func(t *testing.T) {
		store := newFakeOAuthStore()
		c := storeClient("github", "client-x", []string{"https://app.example/cb"})
		c.Scope = "read:runs" // registered narrow; write:runs is outside it
		store.seedClient(c)
		srv := newEnabledOAuthServer(store, newCIMDFetcher(newCIMD()))
		rr := getAuthorize(srv, authorizeQuery(map[string]string{"scope": "write:runs"}), nil)
		q := redirectQuery(t, rr)
		if q.Get("error") != "invalid_scope" {
			t.Fatalf("error = %q, want invalid_scope", q.Get("error"))
		}
	})

	// Within the registered set -> authorizes (consent renders).
	t.Run("within registered scope authorizes", func(t *testing.T) {
		store := newFakeOAuthStore()
		c := storeClient("github", "client-x", []string{"https://app.example/cb"})
		c.Scope = "read:runs write:runs" // registered
		store.seedClient(c)
		repo := newFakeAuthRepo()
		srv := New(Config{OAuthASIssuer: testIssuer, OAuthStore: store, OAuthCIMDFetcher: newCIMDFetcher(newCIMD()), AuthRepo: repo})
		id := signedInIdentity(repo, "github")
		rr := getAuthorize(srv, authorizeQuery(map[string]string{"scope": "write:runs"}), &id)
		if rr.Code != http.StatusOK {
			t.Fatalf("within-registration scope must authorize; status=%d body=%s", rr.Code, rr.Body.String())
		}
	})

	// ABSENT registered scope -> NO restriction: the SAME request the narrow
	// client above was refused for now succeeds. This is the counterfactual for
	// the absent-vs-empty distinction — treating an absent scope as an empty set
	// (dropping the len>0 guard) would refuse this and redden the test.
	t.Run("absent registered scope is unrestricted", func(t *testing.T) {
		store := newFakeOAuthStore()
		c := storeClient("github", "client-x", []string{"https://app.example/cb"})
		c.Scope = "" // ABSENT
		store.seedClient(c)
		repo := newFakeAuthRepo()
		srv := New(Config{OAuthASIssuer: testIssuer, OAuthStore: store, OAuthCIMDFetcher: newCIMDFetcher(newCIMD()), AuthRepo: repo})
		id := signedInIdentity(repo, "github")
		rr := getAuthorize(srv, authorizeQuery(map[string]string{"scope": "write:runs"}), &id)
		if rr.Code != http.StatusOK {
			t.Fatalf("absent registered scope must impose no restriction; status=%d body=%s", rr.Code, rr.Body.String())
		}
	})
}
