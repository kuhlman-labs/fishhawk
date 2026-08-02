package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	form := codeExchangeForm(mintCode(t, store)) // body client_id=client-x
	// A DIFFERENT client_id in the query string must NOT influence the outcome.
	rr := postToken(srv, form, func(r *http.Request) {
		r.URL.RawQuery = "client_id=wrong-client"
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("query-string client_id changed the outcome; status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestToken_SessionCookieIdentityIsInert(t *testing.T) {
	srv, store := newOAuthTokenServer(t)
	id := Identity{Subject: "github:octocat", UserID: uuid.NewString(), SessionID: uuid.NewString()}
	rr := postToken(srv, codeExchangeForm(mintCode(t, store)), func(r *http.Request) {
		*r = *r.WithContext(context.WithValue(r.Context(), ctxKeyIdentity, id))
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("a session cookie must be inert on /token; status=%d body=%s", rr.Code, rr.Body.String())
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
	// Rotate once (consumes first), then reuse: reuse -> invalid_grant.
	_ = decodeToken(t, postToken(srv, refreshForm(first.RefreshToken), nil))
	reuse := oauthErrCode(t, postToken(srv, refreshForm(first.RefreshToken), nil))
	// A brand-new, never-issued token -> also invalid_grant (indistinguishable).
	notfound := oauthErrCode(t, postToken(srv, refreshForm("fhr_"+strings.Repeat("z", 40)), nil))
	if reuse != "invalid_grant" || notfound != "invalid_grant" {
		t.Fatalf("reuse=%s notfound=%s, both want invalid_grant", reuse, notfound)
	}
}
