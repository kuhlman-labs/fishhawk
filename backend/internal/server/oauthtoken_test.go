package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/oauthas"
	"github.com/kuhlman-labs/fishhawk/backend/internal/oauthstore"
)

// newOAuthTokenServer builds an AS-enabled server with client-x registered public.
func newOAuthTokenServer(t *testing.T) (*Server, *fakeOAuthStore) {
	t.Helper()
	store := newFakeOAuthStore()
	store.seedClient(storeClient("github", "client-x", []string{"https://app.example/cb"}))
	srv := newEnabledOAuthServer(store, newCIMDFetcher(newCIMD()))
	return srv, store
}

// mintCode inserts a valid, unconsumed code bound to the standard PKCE
// challenge and returns its plaintext.
func mintCode(t *testing.T, store *fakeOAuthStore) string {
	t.Helper()
	_, challenge := testPKCE()
	code, err := store.CreateAuthorizationCode(context.Background(), oauthstore.NewAuthorizationCode{
		ClientID:            "client-x",
		RedirectURI:         "https://app.example/cb",
		CodeChallenge:       challenge,
		CodeChallengeMethod: oauthas.CodeChallengeMethodS256,
		Scopes:              []string{"read:runs"},
		Resource:            testResource,
		Subject:             "github:octocat",
		Provider:            "github",
		AccountID:           "acct-1",
		ExpiresAt:           time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("mint code: %v", err)
	}
	return code.PlainText
}

func postToken(srv *Server, form url.Values, mut func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v0/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if mut != nil {
		mut(req)
	}
	rr := httptest.NewRecorder()
	srv.handleOAuthToken(rr, req)
	return rr
}

// TestOAuthToken_CIMDFetchesStopAtBurst (#11) is the token-endpoint twin of the
// authorize burst test: the REAL token route driven with a DISTINCT client_id
// per request, asserting outbound fetches STOP at the burst and the over-burst
// request is a 429 with Retry-After.
func TestOAuthToken_CIMDFetchesStopAtBurst(t *testing.T) {
	store := newFakeOAuthStore()
	rt := newCIMD()
	srv := newEnabledOAuthServer(store, newCIMDFetcher(rt))
	burst := defaultOAuthCIMDSourceBurst

	form := func(clientID string) url.Values {
		v := url.Values{}
		v.Set("grant_type", "authorization_code")
		v.Set("code", "irrelevant")
		v.Set("client_id", clientID)
		return v
	}
	for i := 0; i < burst; i++ {
		rr := postToken(srv, form("https://tc"+itoa16(i)+".example/cimd"), nil)
		if rr.Code == http.StatusTooManyRequests {
			t.Fatalf("token request %d within burst was rate limited (body=%s)", i, rr.Body.String())
		}
	}
	if rt.fetches() != burst {
		t.Fatalf("within-burst fetches = %d, want %d", rt.fetches(), burst)
	}
	before := rt.fetches()

	rr := postToken(srv, form("https://tc-over.example/cimd"), nil)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("over-burst token status = %d, want 429; body=%s", rr.Code, rr.Body.String())
	}
	if ra := rr.Header().Get("Retry-After"); ra == "" {
		t.Fatal("missing Retry-After on the token 429")
	} else if n, err := strconv.Atoi(ra); err != nil || n < 1 {
		t.Fatalf("Retry-After = %q, want an integer >= 1", ra)
	}
	if rt.fetches() != before {
		t.Fatalf("a rate-limited token request still dialled CIMD: fetches %d -> %d", before, rt.fetches())
	}
}

func codeExchangeForm(code string) url.Values {
	verifier, _ := testPKCE()
	v := url.Values{}
	v.Set("grant_type", "authorization_code")
	v.Set("code", code)
	v.Set("redirect_uri", "https://app.example/cb")
	v.Set("code_verifier", verifier)
	v.Set("client_id", "client-x")
	return v
}

func decodeToken(t *testing.T, rr *httptest.ResponseRecorder) oauthTokenResponse {
	t.Helper()
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp oauthTokenResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode token: %v", err)
	}
	return resp
}

func oauthErrCode(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var b oauthErrorBody
	if err := json.Unmarshal(rr.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode oauth error: %v (body=%s)", err, rr.Body.String())
	}
	return b.Error
}

// ---- Client authentication (FIX 3) ----

func TestToken_BasicAuthorizationHeaderRefusedAsInvalidClient(t *testing.T) {
	srv, store := newOAuthTokenServer(t)
	rr := postToken(srv, codeExchangeForm(mintCode(t, store)), func(r *http.Request) {
		r.Header.Set("Authorization", "Basic Zm9vOmJhcg==")
	})
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if oauthErrCode(t, rr) != "invalid_client" {
		t.Fatalf("error = %q, want invalid_client", oauthErrCode(t, rr))
	}
	if got := rr.Header().Get("WWW-Authenticate"); !strings.HasPrefix(got, "Basic") {
		t.Fatalf("WWW-Authenticate = %q, want Basic ...", got)
	}
}

func TestToken_AnyAuthorizationSchemeRefusedAsInvalidClient(t *testing.T) {
	srv, store := newOAuthTokenServer(t)
	for _, scheme := range []string{"Bearer fhk_x", "Weird abc"} {
		rr := postToken(srv, codeExchangeForm(mintCode(t, store)), func(r *http.Request) {
			r.Header.Set("Authorization", scheme)
		})
		if rr.Code != http.StatusUnauthorized || oauthErrCode(t, rr) != "invalid_client" {
			t.Fatalf("scheme %q: status=%d err=%s, want 401 invalid_client", scheme, rr.Code, oauthErrCode(t, rr))
		}
		want := strings.Fields(scheme)[0]
		if got := rr.Header().Get("WWW-Authenticate"); !strings.HasPrefix(got, want) {
			t.Fatalf("scheme %q: WWW-Authenticate = %q, want %s ...", scheme, got, want)
		}
	}
}

func TestToken_ClientIDReadFromPostFormOnly(t *testing.T) {
	srv, store := newOAuthTokenServer(t)
	// The DISCRIMINATING case for FIX 3's single-source property. A both-present
	// request (body client-x, query wrong-client) is UNATTAINABLE as a
	// counterfactual: Go's ParseForm builds r.Form by copying PostForm first and
	// appending the query after, so r.Form.Get("client_id") returns the BODY
	// value whenever both are present — a handler rewritten to read r.Form would
	// pass identically. So instead the BODY omits client_id while the query
	// supplies a VALID one: r.PostForm-only reading yields an empty client_id ->
	// invalid_client, while r.Form would resolve client-x from the query and
	// SUCCEED. This refusal is the property; a 200 here means r.Form leaked in.
	form := codeExchangeForm(mintCode(t, store))
	form.Del("client_id") // body carries NO client_id
	rr := postToken(srv, form, func(r *http.Request) {
		r.URL.RawQuery = "client_id=client-x" // a valid client, ONLY in the query
	})
	if rr.Code != http.StatusUnauthorized || oauthErrCode(t, rr) != "invalid_client" {
		t.Fatalf("query-string client_id was honored (r.Form leak); status=%d err=%s, want 401 invalid_client",
			rr.Code, oauthErrCode(t, rr))
	}
}

func TestToken_SessionCookieIdentityIsInert(t *testing.T) {
	srv, store := newOAuthTokenServer(t)

	// Baseline: a fully anonymous exchange.
	anon := decodeToken(t, postToken(srv, codeExchangeForm(mintCode(t, store)), nil))

	// A session cookie for a DIFFERENT user must produce an outcome IDENTICAL to
	// the anonymous baseline and must not leak its subject into the token's
	// authority. A bare "still 200" assertion (the prior form) passes even if the
	// handler reads session identity, so this compares the full observable
	// outcome against the baseline AND checks the minted token's derived subject.
	sessionSubject := "github:eve" // NOT the code's subject (github:octocat)
	id := Identity{Subject: sessionSubject, UserID: uuid.NewString(), SessionID: uuid.NewString()}
	code := mintCode(t, store)
	withCookie := decodeToken(t, postToken(srv, codeExchangeForm(code), func(r *http.Request) {
		*r = *r.WithContext(context.WithValue(r.Context(), ctxKeyIdentity, id))
	}))

	if withCookie.Scope != anon.Scope || withCookie.TokenType != anon.TokenType {
		t.Fatalf("cookie-carrying outcome diverged from anonymous: %+v vs %+v", withCookie, anon)
	}
	if (withCookie.RefreshToken == "") != (anon.RefreshToken == "") {
		t.Fatal("refresh-token presence diverged between the cookie and anonymous requests")
	}

	// The authority boundary: no minted access token may carry the SESSION
	// subject — every token's subject is derived from the code row. A handler
	// that read session identity into issuance would mint a github:eve token.
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, at := range store.access {
		if at.Subject == sessionSubject {
			t.Fatalf("the session subject %q leaked into a minted access token; issuance read session identity", sessionSubject)
		}
	}
}

func TestToken_ClientSecretRefusedAsInvalidClient(t *testing.T) {
	srv, store := newOAuthTokenServer(t)
	form := codeExchangeForm(mintCode(t, store))
	form.Set("client_secret", "s3cr3t")
	rr := postToken(srv, form, nil)
	if rr.Code != http.StatusUnauthorized || oauthErrCode(t, rr) != "invalid_client" {
		t.Fatalf("status=%d err=%s, want 401 invalid_client", rr.Code, oauthErrCode(t, rr))
	}
}

// TestToken_EmptyFirstAuthorizationValueDoesNotMaskCredential pins the
// duplicate-value bypass: an empty first Authorization value followed by a real
// credential makes Header.Get return "" and would slip the credential past a
// Get-only refusal. Counterfactual: against a handler that read Header.Get, this
// exact request is a VALID code exchange and returns 200 — the 401 assertion is
// the whole test.
func TestToken_EmptyFirstAuthorizationValueDoesNotMaskCredential(t *testing.T) {
	srv, store := newOAuthTokenServer(t)
	rr := postToken(srv, codeExchangeForm(mintCode(t, store)), func(r *http.Request) {
		r.Header["Authorization"] = []string{"", "Basic Zm9vOmJhcg=="}
	})
	if rr.Code != http.StatusUnauthorized || oauthErrCode(t, rr) != "invalid_client" {
		t.Fatalf("empty-first Authorization value masked the credential; status=%d err=%s, want 401 invalid_client",
			rr.Code, oauthErrCode(t, rr))
	}
	if got := rr.Header().Get("WWW-Authenticate"); !strings.HasPrefix(got, "Basic") {
		t.Fatalf("WWW-Authenticate = %q, want Basic ... (scheme from the non-empty value)", got)
	}
}

// TestToken_EmptyFirstClientSecretValueDoesNotMaskCredential pins the same
// bypass for the body field: client_secret=&client_secret=secret makes
// PostForm.Get return "". Counterfactual: a Get-only refusal proceeds to a valid
// 200 exchange.
func TestToken_EmptyFirstClientSecretValueDoesNotMaskCredential(t *testing.T) {
	srv, store := newOAuthTokenServer(t)
	form := codeExchangeForm(mintCode(t, store))
	form["client_secret"] = []string{"", "s3cr3t"} // empty first value, then a real secret
	rr := postToken(srv, form, nil)
	if rr.Code != http.StatusUnauthorized || oauthErrCode(t, rr) != "invalid_client" {
		t.Fatalf("empty-first client_secret value masked the credential; status=%d err=%s, want 401 invalid_client",
			rr.Code, oauthErrCode(t, rr))
	}
}

// ---- Verify-before-consume: state read AFTER the call ----

func assertCodeStillRedeemable(t *testing.T, store *fakeOAuthStore, code string) {
	t.Helper()
	c, err := store.LookupAuthorizationCode(context.Background(), code)
	if err != nil {
		t.Fatalf("lookup code: %v", err)
	}
	if c.ConsumedAt != nil {
		t.Fatal("code was consumed by a failed verify (must stay redeemable)")
	}
}

func TestToken_WrongVerifierLeavesCodeRedeemable(t *testing.T) {
	srv, store := newOAuthTokenServer(t)
	code := mintCode(t, store)
	form := codeExchangeForm(code)
	form.Set("code_verifier", strings.Repeat("b", 43)) // wrong verifier
	rr := postToken(srv, form, nil)
	if rr.Code != http.StatusBadRequest || oauthErrCode(t, rr) != "invalid_grant" {
		t.Fatalf("status=%d err=%s, want 400 invalid_grant", rr.Code, oauthErrCode(t, rr))
	}
	assertCodeStillRedeemable(t, store, code)
	// The load-bearing half: a correct exchange still succeeds.
	if rr2 := postToken(srv, codeExchangeForm(code), nil); rr2.Code != http.StatusOK {
		t.Fatalf("correct exchange after failed verify: status=%d body=%s", rr2.Code, rr2.Body.String())
	}
}

func TestToken_ClientIDMismatchLeavesCodeRedeemable(t *testing.T) {
	srv, store := newOAuthTokenServer(t)
	// Register a second public client so the mismatched client_id resolves.
	store.seedClient(storeClient("github", "client-y", []string{"https://y.example/cb"}))
	code := mintCode(t, store)
	form := codeExchangeForm(code)
	form.Set("client_id", "client-y") // code was issued to client-x
	rr := postToken(srv, form, nil)
	if oauthErrCode(t, rr) != "invalid_grant" {
		t.Fatalf("err=%s, want invalid_grant", oauthErrCode(t, rr))
	}
	assertCodeStillRedeemable(t, store, code)
	if rr2 := postToken(srv, codeExchangeForm(code), nil); rr2.Code != http.StatusOK {
		t.Fatalf("correct exchange: status=%d", rr2.Code)
	}
}

func TestToken_RedirectURIMismatchLeavesCodeRedeemable(t *testing.T) {
	srv, store := newOAuthTokenServer(t)
	// Register the mismatched redirect so redirect-matching is not the failure.
	store.seedClient(storeClient("github", "client-x", []string{"https://app.example/cb", "https://app.example/other"}))
	code := mintCode(t, store)
	form := codeExchangeForm(code)
	form.Set("redirect_uri", "https://app.example/other")
	rr := postToken(srv, form, nil)
	if oauthErrCode(t, rr) != "invalid_grant" {
		t.Fatalf("err=%s, want invalid_grant", oauthErrCode(t, rr))
	}
	assertCodeStillRedeemable(t, store, code)
	if rr2 := postToken(srv, codeExchangeForm(code), nil); rr2.Code != http.StatusOK {
		t.Fatalf("correct exchange: status=%d", rr2.Code)
	}
}

func TestToken_ResourceMismatchLeavesCodeRedeemable(t *testing.T) {
	srv, store := newOAuthTokenServer(t)
	code := mintCode(t, store)
	form := codeExchangeForm(code)
	form.Set("resource", "https://other.example/mcp")
	rr := postToken(srv, form, nil)
	if oauthErrCode(t, rr) != "invalid_grant" {
		t.Fatalf("err=%s, want invalid_grant", oauthErrCode(t, rr))
	}
	assertCodeStillRedeemable(t, store, code)
	if rr2 := postToken(srv, codeExchangeForm(code), nil); rr2.Code != http.StatusOK {
		t.Fatalf("correct exchange: status=%d", rr2.Code)
	}
}

func TestToken_ResourceOmittedInheritsCodeAudience(t *testing.T) {
	srv, store := newOAuthTokenServer(t)
	rr := postToken(srv, codeExchangeForm(mintCode(t, store)), nil) // no resource param
	_ = decodeToken(t, rr)
	// The minted access token's audience must equal the code's recorded resource.
	store.mu.Lock()
	defer store.mu.Unlock()
	found := false
	for _, at := range store.access {
		if at.Audience == testResource {
			found = true
		}
	}
	if !found {
		t.Fatal("minted access token did not inherit the code's audience")
	}
}

func TestToken_ExpiredCodeIsInvalidGrant(t *testing.T) {
	srv, store := newOAuthTokenServer(t)
	_, challenge := testPKCE()
	code, _ := store.CreateAuthorizationCode(context.Background(), oauthstore.NewAuthorizationCode{
		ClientID: "client-x", RedirectURI: "https://app.example/cb", CodeChallenge: challenge,
		CodeChallengeMethod: oauthas.CodeChallengeMethodS256, Scopes: []string{"read:runs"},
		Resource: testResource, Subject: "github:octocat", Provider: "github",
		ExpiresAt: time.Now().Add(-time.Minute), // already expired
	})
	rr := postToken(srv, codeExchangeForm(code.PlainText), nil)
	if oauthErrCode(t, rr) != "invalid_grant" {
		t.Fatalf("err=%s, want invalid_grant", oauthErrCode(t, rr))
	}
}

func TestToken_ReplayedCodeIsInvalidGrantAndRevokesLineage(t *testing.T) {
	srv, store := newOAuthTokenServer(t)
	code := mintCode(t, store)
	first := decodeToken(t, postToken(srv, codeExchangeForm(code), nil))
	// Replay the redeemed code.
	replay := postToken(srv, codeExchangeForm(code), nil)
	if oauthErrCode(t, replay) != "invalid_grant" {
		t.Fatalf("replay err=%s, want invalid_grant", oauthErrCode(t, replay))
	}
	// The previously issued access token must no longer authenticate.
	if _, err := store.AuthenticateAccessToken(context.Background(), first.AccessToken); err == nil {
		t.Fatal("previously issued access token still authenticates after replay")
	}
}

func TestToken_ResponseIsNoStoreAndScopeComesFromTheDerivedRow(t *testing.T) {
	srv, store := newOAuthTokenServer(t)
	rr := postToken(srv, codeExchangeForm(mintCode(t, store)), nil)
	resp := decodeToken(t, rr)
	if rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", rr.Header().Get("Cache-Control"))
	}
	if resp.Scope != "read:runs" {
		t.Fatalf("scope = %q, want read:runs (from the derived row)", resp.Scope)
	}
	if resp.TokenType != "Bearer" {
		t.Fatalf("token_type = %q", resp.TokenType)
	}
}

func TestToken_UnsupportedGrantTypeRefused(t *testing.T) {
	srv, _ := newOAuthTokenServer(t)
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", "client-x")
	rr := postToken(srv, form, nil)
	if rr.Code != http.StatusBadRequest || oauthErrCode(t, rr) != "unsupported_grant_type" {
		t.Fatalf("status=%d err=%s, want 400 unsupported_grant_type", rr.Code, oauthErrCode(t, rr))
	}
}

// TestToken_UnregisteredGrantRefusedUnauthorizedClient is the CONDITION 2
// token-side check: a client whose registered grant_types excludes
// refresh_token cannot use it.
func TestToken_UnregisteredGrantRefusedUnauthorizedClient(t *testing.T) {
	store := newFakeOAuthStore()
	c := storeClient("github", "client-x", []string{"https://app.example/cb"})
	c.GrantTypes = []string{"authorization_code"} // no refresh_token
	store.seedClient(c)
	srv := newEnabledOAuthServer(store, newCIMDFetcher(newCIMD()))
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", "fhr_whatever")
	form.Set("client_id", "client-x")
	rr := postToken(srv, form, nil)
	if oauthErrCode(t, rr) != "unauthorized_client" {
		t.Fatalf("err=%s, want unauthorized_client", oauthErrCode(t, rr))
	}
}

// ---- Refresh rotation ----

func exchangeForRefresh(t *testing.T, srv *Server, store *fakeOAuthStore) oauthTokenResponse {
	t.Helper()
	return decodeToken(t, postToken(srv, codeExchangeForm(mintCode(t, store)), nil))
}

func refreshForm(refreshToken string) url.Values {
	v := url.Values{}
	v.Set("grant_type", "refresh_token")
	v.Set("refresh_token", refreshToken)
	v.Set("client_id", "client-x")
	return v
}

func TestToken_RefreshRotationSucceedsAndConsumesPresentedToken(t *testing.T) {
	srv, store := newOAuthTokenServer(t)
	first := exchangeForRefresh(t, srv, store)
	rotated := decodeToken(t, postToken(srv, refreshForm(first.RefreshToken), nil))
	if rotated.RefreshToken == "" || rotated.RefreshToken == first.RefreshToken {
		t.Fatal("rotation did not issue a fresh refresh token")
	}
	// Presenting the consumed token again is reuse.
	reuse := postToken(srv, refreshForm(first.RefreshToken), nil)
	if oauthErrCode(t, reuse) != "invalid_grant" {
		t.Fatalf("reuse err=%s, want invalid_grant", oauthErrCode(t, reuse))
	}
}

func TestToken_RefreshRotationRequiresClientBinding(t *testing.T) {
	srv, store := newOAuthTokenServer(t)
	store.seedClient(storeClient("github", "client-y", []string{"https://y.example/cb"}))
	first := exchangeForRefresh(t, srv, store)
	// A different client_id must be refused.
	form := refreshForm(first.RefreshToken)
	form.Set("client_id", "client-y")
	rr := postToken(srv, form, nil)
	if oauthErrCode(t, rr) != "invalid_grant" {
		t.Fatalf("err=%s, want invalid_grant", oauthErrCode(t, rr))
	}
	// The token still rotates for its real client afterwards.
	if rr2 := postToken(srv, refreshForm(first.RefreshToken), nil); rr2.Code != http.StatusOK {
		t.Fatalf("real client rotation after a rejected one: status=%d", rr2.Code)
	}
}

func TestToken_RefreshReuseIsInvalidGrantAndLineageIsRevokedAfterReturn(t *testing.T) {
	srv, store := newOAuthTokenServer(t)
	first := exchangeForRefresh(t, srv, store)
	rotated := decodeToken(t, postToken(srv, refreshForm(first.RefreshToken), nil))
	// Reuse the original (consumed) token: lineage revoked, committed.
	reuse := postToken(srv, refreshForm(first.RefreshToken), nil)
	if oauthErrCode(t, reuse) != "invalid_grant" {
		t.Fatalf("err=%s, want invalid_grant", oauthErrCode(t, reuse))
	}
	// The successor access token no longer authenticates.
	if _, err := store.AuthenticateAccessToken(context.Background(), rotated.AccessToken); err == nil {
		t.Fatal("successor access token still authenticates after reuse-triggered lineage revocation")
	}
}

func TestToken_RefreshRevokedAndReusedCollapseToOneOpaqueError(t *testing.T) {
	srv, store := newOAuthTokenServer(t)
	first := exchangeForRefresh(t, srv, store)
	// Rotate once (consumes first, mints a successor), then reuse the consumed
	// token: the reuse -> invalid_grant AND its lineage sweep revokes the
	// successor. The successor is now GENUINELY REVOKED — it was never itself
	// presented, so it exercises the store's ErrRevoked branch, not
	// ErrRefreshReused.
	rotated := decodeToken(t, postToken(srv, refreshForm(first.RefreshToken), nil))
	reuse := oauthErrCode(t, postToken(srv, refreshForm(first.RefreshToken), nil))
	// Presenting the REVOKED successor must collapse to the SAME opaque
	// invalid_grant (the branch the test name promises but the prior form never
	// reached). A regression mapping ErrRevoked to a distinguishable response
	// fails here.
	revoked := oauthErrCode(t, postToken(srv, refreshForm(rotated.RefreshToken), nil))
	// A brand-new, never-issued token -> also invalid_grant (indistinguishable).
	notfound := oauthErrCode(t, postToken(srv, refreshForm("fhr_"+strings.Repeat("z", 40)), nil))
	if reuse != "invalid_grant" || revoked != "invalid_grant" || notfound != "invalid_grant" {
		t.Fatalf("reuse=%s revoked=%s notfound=%s, all want invalid_grant", reuse, revoked, notfound)
	}
}
