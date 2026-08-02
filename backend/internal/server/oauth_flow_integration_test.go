package server

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/auth"
	"github.com/kuhlman-labs/fishhawk/backend/internal/oauthas"
	"github.com/kuhlman-labs/fishhawk/backend/internal/oauthstore"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
)

// TestOAuthFlow_EndToEnd_AuthorizeConsentTokenRefresh drives the whole chain —
// consent POST -> token -> refresh — against the REAL oauthstore Postgres
// repository, asserting the DERIVED-authority contract observable only across
// the whole chain: the minted access token's persisted audience equals the
// requested RFC 8707 resource, its subject/provider/scopes equal the consenting
// session's, and the rotated successor inherits all of them verbatim.
func TestOAuthFlow_EndToEnd_AuthorizeConsentTokenRefresh(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	store := oauthstore.NewPostgresRepository(pool)

	// Pre-register a public client (store-first resolution).
	if _, err := store.UpsertClient(ctx, oauthstore.NewClient{
		Provider: "github",
		Metadata: oauthas.ClientMetadata{
			ClientID:                "client-x",
			RedirectURIs:            []string{"https://app.example/cb"},
			GrantTypes:              []string{"authorization_code", "refresh_token"},
			ResponseTypes:           []string{"code"},
			TokenEndpointAuthMethod: "none",
		},
	}); err != nil {
		t.Fatalf("UpsertClient: %v", err)
	}

	repo := newFakeAuthRepo()
	uid := uuid.New()
	repo.mu.Lock()
	repo.users[uid.String()] = &auth.User{ID: uid.String(), Provider: "github", GitHubLogin: "octocat"}
	repo.mu.Unlock()
	// Untenanted identity (empty AccountID -> NULL account_id, no accounts FK).
	id := Identity{Subject: "github:octocat", UserID: uid.String(), SessionID: uuid.NewString()}

	srv := New(Config{OAuthASIssuer: testIssuer, OAuthStore: store, OAuthCIMDFetcher: newCIMDFetcher(newCIMD()), AuthRepo: repo})

	// Consent POST -> 302 with a code.
	consentRR := postConsent(srv, consentForm(nil), &id)
	code := redirectQuery(t, consentRR).Get("code")
	if code == "" {
		t.Fatal("consent did not mint a code")
	}

	// Token exchange.
	resp := decodeToken(t, postToken(srv, codeExchangeForm(code), nil))
	if resp.AccessToken == "" || resp.RefreshToken == "" {
		t.Fatal("token response missing access/refresh token")
	}

	at, err := store.AuthenticateAccessToken(ctx, resp.AccessToken)
	if err != nil {
		t.Fatalf("authenticate minted access token: %v", err)
	}
	if at.Audience != testResource {
		t.Errorf("access token audience = %q, want %q (RFC 8707 resource)", at.Audience, testResource)
	}
	if at.Subject != "github:octocat" || at.Provider != "github" {
		t.Errorf("access token subject/provider = %q/%q", at.Subject, at.Provider)
	}
	if !equalStrings(at.Scopes, []string{"read:runs"}) {
		t.Errorf("access token scopes = %v", at.Scopes)
	}

	// Refresh rotation: the successor inherits every authority field.
	rotated := decodeToken(t, postToken(srv, refreshForm(resp.RefreshToken), nil))
	at2, err := store.AuthenticateAccessToken(ctx, rotated.AccessToken)
	if err != nil {
		t.Fatalf("authenticate rotated access token: %v", err)
	}
	if at2.Audience != testResource || at2.Subject != "github:octocat" || at2.Provider != "github" || !equalStrings(at2.Scopes, []string{"read:runs"}) {
		t.Errorf("rotated token did not inherit authority: aud=%q sub=%q prov=%q scopes=%v",
			at2.Audience, at2.Subject, at2.Provider, at2.Scopes)
	}
}

// TestOAuthFlow_CodeIsSingleUse re-presents a redeemed code against the real
// repository and asserts invalid_grant.
func TestOAuthFlow_CodeIsSingleUse(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	store := oauthstore.NewPostgresRepository(pool)
	if _, err := store.UpsertClient(ctx, oauthstore.NewClient{
		Provider: "github",
		Metadata: oauthas.ClientMetadata{
			ClientID:                "client-x",
			RedirectURIs:            []string{"https://app.example/cb"},
			GrantTypes:              []string{"authorization_code", "refresh_token"},
			ResponseTypes:           []string{"code"},
			TokenEndpointAuthMethod: "none",
		},
	}); err != nil {
		t.Fatalf("UpsertClient: %v", err)
	}
	repo := newFakeAuthRepo()
	uid := uuid.New()
	repo.mu.Lock()
	repo.users[uid.String()] = &auth.User{ID: uid.String(), Provider: "github", GitHubLogin: "octocat"}
	repo.mu.Unlock()
	id := Identity{Subject: "github:octocat", UserID: uid.String(), SessionID: uuid.NewString()}
	srv := New(Config{OAuthASIssuer: testIssuer, OAuthStore: store, OAuthCIMDFetcher: newCIMDFetcher(newCIMD()), AuthRepo: repo})

	code := redirectQuery(t, postConsent(srv, consentForm(nil), &id)).Get("code")
	first := decodeToken(t, postToken(srv, codeExchangeForm(code), nil))
	replay := postToken(srv, codeExchangeForm(code), nil)
	if oauthErrCode(t, replay) != "invalid_grant" {
		t.Fatalf("replay err = %s, want invalid_grant", oauthErrCode(t, replay))
	}
	// The invalid_grant response is not enough: RFC 6749 §4.1.2 requires the
	// replay to REVOKE the lineage. Assert it against the REAL Postgres
	// repository — the first exchange's access token must stop authenticating.
	// This is the cross-boundary seam the plan's integration-test rationale
	// names: the handler-driven sweep passes against the in-memory fake while a
	// real-store no-op (discarded lookup/revoke error) would leave this token
	// live with no unit test to catch it.
	if _, err := store.AuthenticateAccessToken(ctx, first.AccessToken); err == nil {
		t.Fatal("first-issued access token still authenticates after the replay swept the lineage")
	}
}

// TestOAuthFlow_LimiterLivePreservesHappyPathThenThrottlesCIMD is the
// cross-layer end-to-end for #2441: it drives a COMPLETE consent -> token flow
// through the real mux with the CIMD limiter LIVE at its default (proving the
// control does not break the store-resolved happy path), then drives the SAME
// stack's CIMD store-miss branch past the burst and asserts the outbound-fetch
// count FROZE at the burst. One test crossing flag-less Config -> New -> handler
// -> outbound fetch.
func TestOAuthFlow_LimiterLivePreservesHappyPathThenThrottlesCIMD(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	store := oauthstore.NewPostgresRepository(pool)
	if _, err := store.UpsertClient(ctx, oauthstore.NewClient{
		Provider: "github",
		Metadata: oauthas.ClientMetadata{
			ClientID:                "client-x",
			RedirectURIs:            []string{"https://app.example/cb"},
			GrantTypes:              []string{"authorization_code", "refresh_token"},
			ResponseTypes:           []string{"code"},
			TokenEndpointAuthMethod: "none",
		},
	}); err != nil {
		t.Fatalf("UpsertClient: %v", err)
	}
	repo := newFakeAuthRepo()
	uid := uuid.New()
	repo.mu.Lock()
	repo.users[uid.String()] = &auth.User{ID: uid.String(), Provider: "github", GitHubLogin: "octocat"}
	repo.mu.Unlock()
	id := Identity{Subject: "github:octocat", UserID: uid.String(), SessionID: uuid.NewString()}

	rt := newCIMD()
	srv := New(Config{OAuthASIssuer: testIssuer, OAuthStore: store, OAuthCIMDFetcher: newCIMDFetcher(rt), AuthRepo: repo})

	// Happy path (store-resolved client, never throttled): consent -> token.
	code := redirectQuery(t, postConsent(srv, consentForm(nil), &id)).Get("code")
	if code == "" {
		t.Fatal("limiter-live consent did not mint a code (happy path broken)")
	}
	resp := decodeToken(t, postToken(srv, codeExchangeForm(code), nil))
	if resp.AccessToken == "" {
		t.Fatal("limiter-live token exchange returned no access token")
	}
	if rt.fetches() != 0 {
		t.Fatalf("store-resolved happy path dialled CIMD %d times", rt.fetches())
	}

	// Same stack's CIMD branch: distinct store-miss client_ids past the burst.
	burst := defaultOAuthCIMDSourceBurst
	tokenForm := func(clientID string) url.Values {
		v := url.Values{}
		v.Set("grant_type", "authorization_code")
		v.Set("code", "irrelevant")
		v.Set("client_id", clientID)
		return v
	}
	for i := 0; i < burst; i++ {
		if rr := postToken(srv, tokenForm("https://ci"+itoa16(i)+".example/cimd"), nil); rr.Code == http.StatusTooManyRequests {
			t.Fatalf("within-burst CIMD request %d was rate limited", i)
		}
	}
	froze := rt.fetches()
	if rr := postToken(srv, tokenForm("https://ci-over.example/cimd"), nil); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("over-burst CIMD status = %d, want 429", rr.Code)
	}
	if rt.fetches() != froze {
		t.Fatalf("the rate-limited CIMD request still dialled: fetches %d -> %d", froze, rt.fetches())
	}
}
