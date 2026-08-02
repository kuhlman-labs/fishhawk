package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/oauthas"
	"github.com/kuhlman-labs/fishhawk/backend/internal/oauthstore"
)

// ---------------------------------------------------------------------------
// Shared in-memory oauthstore.Repository fake + CIMD round-tripper. Defined
// here (not an edit to a shared harness) and used across the OAuth AS tests.
// ---------------------------------------------------------------------------

// fakeOAuthStore is an in-memory oauthstore.Repository. It models
// verify-before-consume (RedeemAuthorizationCode / RotateRefreshToken call
// verify AFTER classification and BEFORE consuming; a verify error rolls back)
// and refresh-reuse lineage revocation, so the token tests read real state back.
type fakeOAuthStore struct {
	mu      sync.Mutex
	now     func() time.Time
	clients map[string]*oauthstore.Client            // provider|client_id
	codes   map[string]*oauthstore.AuthorizationCode // plaintext
	access  map[string]*oauthstore.AccessToken       // plaintext
	refresh map[string]*oauthstore.RefreshToken      // plaintext
	getErr  error                                    // when set, GetClient returns it
}

func newFakeOAuthStore() *fakeOAuthStore {
	return &fakeOAuthStore{
		now:     time.Now,
		clients: map[string]*oauthstore.Client{},
		codes:   map[string]*oauthstore.AuthorizationCode{},
		access:  map[string]*oauthstore.AccessToken{},
		refresh: map[string]*oauthstore.RefreshToken{},
	}
}

func (f *fakeOAuthStore) seedClient(c *oauthstore.Client) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	f.clients[c.Provider+"|"+c.ClientID] = c
}

func (f *fakeOAuthStore) UpsertClient(_ context.Context, in oauthstore.NewClient) (*oauthstore.Client, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := &oauthstore.Client{
		ID:                      uuid.New(),
		Provider:                in.Provider,
		ClientID:                in.Metadata.ClientID,
		RedirectURIs:            in.Metadata.RedirectURIs,
		GrantTypes:              in.Metadata.GrantTypes,
		ResponseTypes:           in.Metadata.ResponseTypes,
		TokenEndpointAuthMethod: in.Metadata.TokenEndpointAuthMethod,
		ClientName:              in.Metadata.ClientName,
		ClientURI:               in.Metadata.ClientURI,
		LogoURI:                 in.Metadata.LogoURI,
		Scope:                   in.Metadata.Scope,
		AccountID:               in.AccountID,
	}
	f.clients[c.Provider+"|"+c.ClientID] = c
	return c, nil
}

func (f *fakeOAuthStore) GetClient(_ context.Context, provider, clientID string) (*oauthstore.Client, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	if c, ok := f.clients[provider+"|"+clientID]; ok {
		cp := *c
		return &cp, nil
	}
	return nil, oauthstore.ErrNotFound
}

func (f *fakeOAuthStore) CreateAuthorizationCode(_ context.Context, in oauthstore.NewAuthorizationCode) (*oauthstore.AuthorizationCode, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pt, err := oauthstore.GeneratePlaintext(oauthstore.CodePrefix)
	if err != nil {
		return nil, err
	}
	code := &oauthstore.AuthorizationCode{
		ID:                  uuid.New(),
		ClientID:            in.ClientID,
		RedirectURI:         in.RedirectURI,
		CodeChallenge:       in.CodeChallenge,
		CodeChallengeMethod: in.CodeChallengeMethod,
		Scopes:              in.Scopes,
		Resource:            in.Resource,
		Subject:             in.Subject,
		Provider:            in.Provider,
		AccountID:           in.AccountID,
		IssuedAt:            f.now(),
		ExpiresAt:           in.ExpiresAt,
	}
	f.codes[pt] = code
	out := *code
	out.PlainText = pt
	return &out, nil
}

func (f *fakeOAuthStore) LookupAuthorizationCode(_ context.Context, plaintext string) (*oauthstore.AuthorizationCode, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.codes[plaintext]; ok {
		cp := *c
		return &cp, nil
	}
	return nil, oauthstore.ErrNotFound
}

func (f *fakeOAuthStore) RedeemAuthorizationCode(_ context.Context, plaintext string, verify func(*oauthstore.AuthorizationCode) error, redeem oauthstore.RedemptionRequest) (*oauthstore.IssuedGrant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	code, ok := f.codes[plaintext]
	if !ok {
		return nil, oauthstore.ErrNotFound
	}
	if code.IsConsumed() {
		return nil, oauthstore.ErrCodeConsumed
	}
	if code.IsExpired(f.now()) {
		return nil, oauthstore.ErrExpired
	}
	if verify != nil {
		if err := verify(codeCopy(code)); err != nil {
			return nil, err // unchanged, no consume
		}
	}
	now := f.now()
	code.ConsumedAt = &now
	access := f.mintAccessLocked(code.Subject, code.ClientID, code.Resource, code.Scopes, code.Provider, code.AccountID, code.ID, redeem.AccessTokenExpiry)
	refresh := f.mintRefreshLocked(code.Subject, code.ClientID, code.Resource, code.Scopes, code.Provider, code.AccountID, code.ID, access.ID, redeem.RefreshTokenExpiry)
	return &oauthstore.IssuedGrant{Code: codeCopy(code), Access: access, Refresh: refresh}, nil
}

func (f *fakeOAuthStore) RevokeGrantsForCode(_ context.Context, codeID uuid.UUID) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.revokeLineageLocked(codeID), nil
}

func (f *fakeOAuthStore) AuthenticateAccessToken(_ context.Context, plaintext string) (*oauthstore.AccessToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.access[plaintext]
	if !ok {
		return nil, oauthstore.ErrNotFound
	}
	if t.IsRevoked() {
		return nil, oauthstore.ErrRevoked
	}
	if t.IsExpired(f.now()) {
		return nil, oauthstore.ErrExpired
	}
	cp := *t
	cp.PlainText = ""
	return &cp, nil
}

func (f *fakeOAuthStore) RotateRefreshToken(_ context.Context, plaintext string, verify func(*oauthstore.RefreshToken) error, rotate oauthstore.RotationRequest) (*oauthstore.IssuedGrant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.refresh[plaintext]
	if !ok {
		return nil, oauthstore.ErrNotFound
	}
	// Classification order: revoked -> reused -> expired.
	if t.IsRevoked() {
		return nil, oauthstore.ErrRevoked
	}
	if t.IsConsumed() {
		f.revokeLineageLocked(t.AuthorizationCodeID) // committed before return
		return nil, oauthstore.ErrRefreshReused
	}
	if t.IsExpired(f.now()) {
		return nil, oauthstore.ErrExpired
	}
	if verify != nil {
		if err := verify(refreshCopy(t)); err != nil {
			return nil, err // unchanged, no consume
		}
	}
	now := f.now()
	t.ConsumedAt = &now
	access := f.mintAccessLocked(t.Subject, t.ClientID, t.Audience, t.Scopes, t.Provider, t.AccountID, t.AuthorizationCodeID, rotate.AccessTokenExpiry)
	refresh := f.mintRefreshLocked(t.Subject, t.ClientID, t.Audience, t.Scopes, t.Provider, t.AccountID, t.AuthorizationCodeID, access.ID, rotate.RefreshTokenExpiry)
	t.ReplacedByID = &refresh.ID
	return &oauthstore.IssuedGrant{Access: access, Refresh: refresh}, nil
}

func (f *fakeOAuthStore) RevokeAccessToken(_ context.Context, id uuid.UUID) (*oauthstore.AccessToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.access {
		if t.ID == id {
			if t.RevokedAt == nil {
				now := f.now()
				t.RevokedAt = &now
			}
			cp := *t
			cp.PlainText = ""
			return &cp, nil
		}
	}
	return nil, oauthstore.ErrNotFound
}

func (f *fakeOAuthStore) RevokeRefreshToken(_ context.Context, id uuid.UUID) (*oauthstore.RefreshToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.refresh {
		if t.ID == id {
			if t.RevokedAt == nil {
				now := f.now()
				t.RevokedAt = &now
			}
			cp := *t
			cp.PlainText = ""
			return &cp, nil
		}
	}
	return nil, oauthstore.ErrNotFound
}

func (f *fakeOAuthStore) mintAccessLocked(subject, clientID, audience string, scopes []string, provider, accountID string, codeID uuid.UUID, exp time.Time) *oauthstore.AccessToken {
	pt, _ := oauthstore.GeneratePlaintext(oauthstore.AccessTokenPrefix)
	t := &oauthstore.AccessToken{
		ID:                  uuid.New(),
		Subject:             subject,
		ClientID:            clientID,
		Audience:            audience,
		Scopes:              scopes,
		Provider:            provider,
		AccountID:           accountID,
		AuthorizationCodeID: codeID,
		IssuedAt:            f.now(),
		ExpiresAt:           exp,
		PlainText:           pt,
	}
	f.access[pt] = t
	out := *t
	return &out
}

func (f *fakeOAuthStore) mintRefreshLocked(subject, clientID, audience string, scopes []string, provider, accountID string, codeID, accessID uuid.UUID, exp time.Time) *oauthstore.RefreshToken {
	pt, _ := oauthstore.GeneratePlaintext(oauthstore.RefreshTokenPrefix)
	t := &oauthstore.RefreshToken{
		ID:                  uuid.New(),
		Subject:             subject,
		ClientID:            clientID,
		Audience:            audience,
		Scopes:              scopes,
		Provider:            provider,
		AccountID:           accountID,
		AuthorizationCodeID: codeID,
		AccessTokenID:       accessID,
		IssuedAt:            f.now(),
		ExpiresAt:           exp,
		PlainText:           pt,
	}
	f.refresh[pt] = t
	out := *t
	return &out
}

func (f *fakeOAuthStore) revokeLineageLocked(codeID uuid.UUID) int64 {
	var n int64
	now := f.now()
	for _, t := range f.access {
		if t.AuthorizationCodeID == codeID && t.RevokedAt == nil {
			t.RevokedAt = &now
			n++
		}
	}
	for _, t := range f.refresh {
		if t.AuthorizationCodeID == codeID && t.RevokedAt == nil {
			t.RevokedAt = &now
			n++
		}
	}
	return n
}

func codeCopy(c *oauthstore.AuthorizationCode) *oauthstore.AuthorizationCode {
	cp := *c
	cp.PlainText = ""
	return &cp
}

func refreshCopy(t *oauthstore.RefreshToken) *oauthstore.RefreshToken {
	cp := *t
	cp.PlainText = ""
	return &cp
}

// cimdRoundTripper is a fetch-counting http.RoundTripper serving canned CIMD
// documents. It lets the never-dialed assertions be real (fetches()==0).
type cimdRoundTripper struct {
	mu    sync.Mutex
	count int
	docs  map[string]string // client_id URL -> JSON body
}

func newCIMD() *cimdRoundTripper { return &cimdRoundTripper{docs: map[string]string{}} }

func (rt *cimdRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.mu.Lock()
	rt.count++
	body, ok := rt.docs[req.URL.String()]
	rt.mu.Unlock()
	h := http.Header{}
	if !ok {
		return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("not found")), Header: h}, nil
	}
	h.Set("Content-Type", "application/json")
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: h}, nil
}

func (rt *cimdRoundTripper) fetches() int {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.count
}

func newCIMDFetcher(rt *cimdRoundTripper) *oauthas.Fetcher {
	return &oauthas.Fetcher{HTTPClient: &http.Client{Transport: rt}}
}

// newEnabledOAuthServer builds an AS-enabled Server over the given store and
// fetcher, at issuer https://as.example (resource https://as.example/mcp).
func newEnabledOAuthServer(store oauthstore.Repository, fetcher *oauthas.Fetcher) *Server {
	return New(Config{
		OAuthASIssuer:    "https://as.example",
		OAuthStore:       store,
		OAuthCIMDFetcher: fetcher,
	})
}

const testIssuer = "https://as.example"
const testResource = "https://as.example/mcp"

// resolveReq builds a request whose RemoteAddr keys the CIMD limiter. Since
// resolveOAuthClient now derives the limiter source key from the request, the
// resolver tests hand it a request with an explicit RemoteAddr rather than a
// bare context.
func resolveReq(remoteAddr string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/v0/oauth/authorize", nil)
	req.RemoteAddr = remoteAddr
	return req
}

// ---------------------------------------------------------------------------
// Metadata + AS-state tests.
// ---------------------------------------------------------------------------

func getMetadata(t *testing.T, srv *Server) (*httptest.ResponseRecorder, oauthASMetadata) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/.well-known/oauth-authorization-server", nil)
	rr := httptest.NewRecorder()
	srv.handleOAuthASMetadata(rr, req)
	var doc oauthASMetadata
	if rr.Code == http.StatusOK {
		if err := json.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
			t.Fatalf("decode metadata: %v", err)
		}
	}
	return rr, doc
}

func TestOAuthASMetadata_AdvertisesCIMDSupported(t *testing.T) {
	srv := newEnabledOAuthServer(newFakeOAuthStore(), newCIMDFetcher(newCIMD()))
	_, doc := getMetadata(t, srv)
	if !doc.ClientIDMetadataDocumentSupported {
		t.Fatal("client_id_metadata_document_supported must be true")
	}
}

func TestOAuthASMetadata_AdvertisesNoneAuthMethod(t *testing.T) {
	srv := newEnabledOAuthServer(newFakeOAuthStore(), newCIMDFetcher(newCIMD()))
	_, doc := getMetadata(t, srv)
	if len(doc.TokenEndpointAuthMethodsSupported) != 1 || doc.TokenEndpointAuthMethodsSupported[0] != "none" {
		t.Fatalf("token_endpoint_auth_methods_supported = %v, want [none]", doc.TokenEndpointAuthMethodsSupported)
	}
}

func TestOAuthASMetadata_CompleteRFC8414FieldSet(t *testing.T) {
	srv := newEnabledOAuthServer(newFakeOAuthStore(), newCIMDFetcher(newCIMD()))
	rr, doc := getMetadata(t, srv)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", rr.Header().Get("Cache-Control"))
	}
	if doc.Issuer != testIssuer {
		t.Errorf("issuer = %q", doc.Issuer)
	}
	if doc.AuthorizationEndpoint != testIssuer+"/v0/oauth/authorize" {
		t.Errorf("authorization_endpoint = %q", doc.AuthorizationEndpoint)
	}
	if doc.TokenEndpoint != testIssuer+"/v0/oauth/token" {
		t.Errorf("token_endpoint = %q", doc.TokenEndpoint)
	}
	if len(doc.ResponseTypesSupported) != 1 || doc.ResponseTypesSupported[0] != "code" {
		t.Errorf("response_types_supported = %v", doc.ResponseTypesSupported)
	}
	if !equalStrings(doc.GrantTypesSupported, []string{"authorization_code", "refresh_token"}) {
		t.Errorf("grant_types_supported = %v", doc.GrantTypesSupported)
	}
	if !doc.AuthorizationResponseISSParameterSupported {
		t.Error("authorization_response_iss_parameter_supported must be true")
	}
}

func TestOAuthASMetadata_ScopesSupportedMirrorsOperatorVocabulary(t *testing.T) {
	srv := newEnabledOAuthServer(newFakeOAuthStore(), newCIMDFetcher(newCIMD()))
	_, doc := getMetadata(t, srv)
	if !equalStrings(doc.ScopesSupported, oauthas.SupportedScopes) {
		t.Fatalf("scopes_supported = %v, want %v (equal by value AND order)", doc.ScopesSupported, oauthas.SupportedScopes)
	}
}

func TestOAuthASMetadata_S256OnlyAndIssParameterSupported(t *testing.T) {
	srv := newEnabledOAuthServer(newFakeOAuthStore(), newCIMDFetcher(newCIMD()))
	_, doc := getMetadata(t, srv)
	if len(doc.CodeChallengeMethodsSupported) != 1 || doc.CodeChallengeMethodsSupported[0] != "S256" {
		t.Fatalf("code_challenge_methods_supported = %v, want [S256]", doc.CodeChallengeMethodsSupported)
	}
	if !doc.AuthorizationResponseISSParameterSupported {
		t.Fatal("authorization_response_iss_parameter_supported must be true")
	}
}

func TestOAuthASMetadata_EndpointsAreIssuerRooted(t *testing.T) {
	srv := newEnabledOAuthServer(newFakeOAuthStore(), newCIMDFetcher(newCIMD()))
	_, doc := getMetadata(t, srv)
	if !strings.HasPrefix(doc.AuthorizationEndpoint, testIssuer) || !strings.HasPrefix(doc.TokenEndpoint, testIssuer) {
		t.Fatalf("endpoints not issuer-rooted: %q, %q", doc.AuthorizationEndpoint, doc.TokenEndpoint)
	}
}

func TestOAuthASState_DisabledWhenNoIssuer(t *testing.T) {
	srv := New(Config{}) // no issuer
	rr, _ := getMetadata(t, srv)
	assertUnconfigured(t, rr)
}

func TestOAuthASState_MisconfiguredIssuerReturns503(t *testing.T) {
	srv := New(Config{OAuthASIssuer: "http://as.example", OAuthStore: newFakeOAuthStore(), OAuthCIMDFetcher: newCIMDFetcher(newCIMD())})
	rr, _ := getMetadata(t, srv)
	assertUnconfigured(t, rr)
}

func TestOAuthASState_MisconfiguredResourceReturns503(t *testing.T) {
	srv := New(Config{OAuthASIssuer: testIssuer, OAuthASResource: "not a uri", OAuthStore: newFakeOAuthStore(), OAuthCIMDFetcher: newCIMDFetcher(newCIMD())})
	rr, _ := getMetadata(t, srv)
	assertUnconfigured(t, rr)
}

func TestOAuthASState_NilStoreIsMisconfigured(t *testing.T) {
	srv := New(Config{OAuthASIssuer: testIssuer, OAuthCIMDFetcher: newCIMDFetcher(newCIMD())})
	rr, _ := getMetadata(t, srv)
	assertUnconfigured(t, rr)
}

func TestOAuthASState_NilCIMDFetcherIsMisconfiguredNotEnabled(t *testing.T) {
	srv := New(Config{OAuthASIssuer: testIssuer, OAuthStore: newFakeOAuthStore()}) // fetcher nil
	// ALL FOUR route patterns must 503, and NONE render a metadata document.
	paths := []struct {
		method, path string
	}{
		{http.MethodGet, "/.well-known/oauth-authorization-server"},
		{http.MethodGet, "/v0/oauth/authorize"},
		{http.MethodPost, "/v0/oauth/authorize"},
		{http.MethodPost, "/v0/oauth/token"},
	}
	for _, p := range paths {
		req := httptest.NewRequest(p.method, p.path, nil)
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s: status = %d, want 503", p.method, p.path, rr.Code)
		}
		if strings.Contains(rr.Body.String(), "client_id_metadata_document_supported") {
			t.Fatalf("%s %s: a fetcher-less server rendered a metadata document", p.method, p.path)
		}
	}
}

func TestNew_OAuthASStateVerdicts(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		mode oauthASMode
	}{
		{"disabled", Config{}, oauthASDisabled},
		{"enabled", Config{OAuthASIssuer: testIssuer, OAuthStore: newFakeOAuthStore(), OAuthCIMDFetcher: newCIMDFetcher(newCIMD())}, oauthASEnabled},
		{"bad-issuer", Config{OAuthASIssuer: "ftp://x", OAuthStore: newFakeOAuthStore(), OAuthCIMDFetcher: newCIMDFetcher(newCIMD())}, oauthASMisconfigured},
		{"nil-store", Config{OAuthASIssuer: testIssuer, OAuthCIMDFetcher: newCIMDFetcher(newCIMD())}, oauthASMisconfigured},
		{"nil-fetcher", Config{OAuthASIssuer: testIssuer, OAuthStore: newFakeOAuthStore()}, oauthASMisconfigured},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := New(c.cfg) // must not panic
			if srv.oauthAS.mode != c.mode {
				t.Fatalf("mode = %d, want %d", srv.oauthAS.mode, c.mode)
			}
		})
	}
}

// TestOAuthASState_NotLoopbackWhenRequiredAndPublicBind (#2441 gate #14): the
// gate on + a public bind resolves to the not-loopback verdict.
func TestOAuthASState_NotLoopbackWhenRequiredAndPublicBind(t *testing.T) {
	srv := New(Config{
		Addr: "0.0.0.0:8080", OAuthASRequireLoopback: true,
		OAuthASIssuer: testIssuer, OAuthStore: newFakeOAuthStore(), OAuthCIMDFetcher: newCIMDFetcher(newCIMD()),
	})
	if srv.oauthAS.mode != oauthASNotLoopback {
		t.Fatalf("mode = %d, want oauthASNotLoopback (%d)", srv.oauthAS.mode, oauthASNotLoopback)
	}
	if srv.oauthAS.listenHost != "0.0.0.0" {
		t.Fatalf("listenHost = %q, want 0.0.0.0", srv.oauthAS.listenHost)
	}
}

// TestOAuthASState_LoopbackBindPassesGate: the gate on + a loopback bind still
// resolves ENABLED.
func TestOAuthASState_LoopbackBindPassesGate(t *testing.T) {
	srv := New(Config{
		Addr: "127.0.0.1:8080", OAuthASRequireLoopback: true,
		OAuthASIssuer: testIssuer, OAuthStore: newFakeOAuthStore(), OAuthCIMDFetcher: newCIMDFetcher(newCIMD()),
	})
	if srv.oauthAS.mode != oauthASEnabled {
		t.Fatalf("mode = %d, want oauthASEnabled on a loopback bind under the gate", srv.oauthAS.mode)
	}
}

// TestOAuthASState_DisabledBeatsLoopbackGate (#16): no issuer + gate on + public
// bind yields the DISABLED 503 verdict, pinning the ladder order (disabled is
// decided before the loopback check). Deleting the disabled-first ordering would
// surface not-loopback here instead.
func TestOAuthASState_DisabledBeatsLoopbackGate(t *testing.T) {
	srv := New(Config{Addr: "0.0.0.0:8080", OAuthASRequireLoopback: true}) // no issuer
	if srv.oauthAS.mode != oauthASDisabled {
		t.Fatalf("mode = %d, want oauthASDisabled (disabled must beat the loopback gate)", srv.oauthAS.mode)
	}
	rr, _ := getMetadata(t, srv)
	assertUnconfigured(t, rr)
}

// TestOAuthASState_MisconfiguredBeatsLoopbackGate: a defective issuer + gate on +
// public bind reports the CONFIG defect (503), not the 403 — the loopback check
// runs last on purpose.
func TestOAuthASState_MisconfiguredBeatsLoopbackGate(t *testing.T) {
	srv := New(Config{
		Addr: "0.0.0.0:8080", OAuthASRequireLoopback: true,
		OAuthASIssuer: "http://as.example", OAuthStore: newFakeOAuthStore(), OAuthCIMDFetcher: newCIMDFetcher(newCIMD()),
	})
	if srv.oauthAS.mode != oauthASMisconfigured {
		t.Fatalf("mode = %d, want oauthASMisconfigured (config defect beats the loopback gate)", srv.oauthAS.mode)
	}
}

// TestOAuthASState_LoopbackGateDefaultsOff (#17, DONE-MEANS): a Config that never
// mentions OAuthASRequireLoopback, on a 0.0.0.0 bind, resolves ENABLED and
// serves. This pins the SERVER-side (Config zero-value) default: a resolver that
// treated an unset gate as on would 403 here. The CLI flag default (serve.go's
// --oauth-require-loopback registration) is pinned separately by
// TestResolveOAuthCIMDLimiterFlags in the fishhawkd package.
func TestOAuthASState_LoopbackGateDefaultsOff(t *testing.T) {
	srv := New(Config{
		Addr:          "0.0.0.0:8080", // public bind, gate UNSET
		OAuthASIssuer: testIssuer, OAuthStore: newFakeOAuthStore(), OAuthCIMDFetcher: newCIMDFetcher(newCIMD()),
	})
	if srv.oauthAS.mode != oauthASEnabled {
		t.Fatalf("mode = %d, want oauthASEnabled — the loopback gate must default OFF", srv.oauthAS.mode)
	}
	rr, doc := getMetadata(t, srv)
	if rr.Code != http.StatusOK || !doc.ClientIDMetadataDocumentSupported {
		t.Fatalf("default-off AS must serve metadata on a public bind; status=%d", rr.Code)
	}
}

// TestOAuthASState_LoopbackGateFailsClosedOnUnclassifiableHost pins the two
// fail-closed branches of the loopback gate (#2441): under the gate, a listen
// address that cannot be PARSED (net.SplitHostPort fails) and one whose host
// cannot be CLASSIFIED (the DNS seam errors) both resolve MISCONFIGURED, never
// silently ENABLED. Without the gate on, the same defective Addr must NOT reach
// the classifier — the gate is what makes the listen host load-bearing — so the
// disabled-gate control still resolves enabled. Calls resolveOAuthASState
// directly to inject the DNS seam.
func TestOAuthASState_LoopbackGateFailsClosedOnUnclassifiableHost(t *testing.T) {
	base := func() Config {
		return Config{
			OAuthASIssuer:    testIssuer,
			OAuthStore:       newFakeOAuthStore(),
			OAuthCIMDFetcher: newCIMDFetcher(newCIMD()),
		}
	}
	// A DNS seam that always fails, standing in for a resolver outage while
	// classifying a non-IP listen host.
	failingLookup := func(string) ([]net.IP, error) {
		return nil, errors.New("resolver unreachable")
	}

	t.Run("unparseable listen address under the gate is misconfigured", func(t *testing.T) {
		cfg := base()
		cfg.Addr = "garbage-no-port" // net.SplitHostPort fails: missing port
		cfg.OAuthASRequireLoopback = true
		st := resolveOAuthASState(cfg, net.LookupIP)
		if st.mode != oauthASMisconfigured {
			t.Fatalf("mode = %d, want oauthASMisconfigured (%d) for an unparseable Addr under the gate", st.mode, oauthASMisconfigured)
		}
	})

	t.Run("unresolvable listen host under the gate is misconfigured", func(t *testing.T) {
		cfg := base()
		cfg.Addr = "as.internal.example:8080" // non-IP host → hits the DNS seam
		cfg.OAuthASRequireLoopback = true
		st := resolveOAuthASState(cfg, failingLookup)
		if st.mode != oauthASMisconfigured {
			t.Fatalf("mode = %d, want oauthASMisconfigured (%d) when the DNS seam errors under the gate", st.mode, oauthASMisconfigured)
		}
	})

	t.Run("without the gate a defective Addr never reaches the classifier", func(t *testing.T) {
		cfg := base()
		cfg.Addr = "garbage-no-port" // would fail SplitHostPort IF the gate ran
		// gate OFF, failing lookup that must never be called
		st := resolveOAuthASState(cfg, failingLookup)
		if st.mode != oauthASEnabled {
			t.Fatalf("mode = %d, want oauthASEnabled (%d) — a defective Addr must not matter when the gate is off", st.mode, oauthASEnabled)
		}
	})
}

// TestOAuthASRoutes_LoopbackGateRefusesAllFour (#15): with the gate on and a
// public bind, ALL FOUR OAuth route patterns answer 403 oauth_as_loopback_only
// and none renders a metadata document.
func TestOAuthASRoutes_LoopbackGateRefusesAllFour(t *testing.T) {
	srv := New(Config{
		Addr: "0.0.0.0:8080", OAuthASRequireLoopback: true,
		OAuthASIssuer: testIssuer, OAuthStore: newFakeOAuthStore(), OAuthCIMDFetcher: newCIMDFetcher(newCIMD()),
	})
	paths := []struct{ method, path string }{
		{http.MethodGet, "/.well-known/oauth-authorization-server"},
		{http.MethodGet, "/v0/oauth/authorize"},
		{http.MethodPost, "/v0/oauth/authorize"},
		{http.MethodPost, "/v0/oauth/token"},
	}
	for _, p := range paths {
		req := httptest.NewRequest(p.method, p.path, nil)
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("%s %s: status = %d, want 403", p.method, p.path, rr.Code)
		}
		if !strings.Contains(rr.Body.String(), "oauth_as_loopback_only") {
			t.Fatalf("%s %s: body = %q, want oauth_as_loopback_only", p.method, p.path, rr.Body.String())
		}
		if strings.Contains(rr.Body.String(), "client_id_metadata_document_supported") {
			t.Fatalf("%s %s: gated route rendered a metadata document", p.method, p.path)
		}
	}
}

// TestResolveOAuthClient_StoreHitIsNeverRateLimited (#9, the PLACEMENT
// counterfactual): burst+10 store-resolved resolutions from ONE source all
// succeed, the counting round-tripper reports ZERO fetches, and the source's
// limiter token count is UNCHANGED (a state read). Moving the Allow call to the
// top of resolveOAuthClient turns this RED.
func TestResolveOAuthClient_StoreHitIsNeverRateLimited(t *testing.T) {
	store := newFakeOAuthStore()
	store.seedClient(storeClient("github", "client-x", []string{"https://app.example/cb"}))
	rt := newCIMD()
	srv := newEnabledOAuthServer(store, newCIMDFetcher(rt))
	const remote = "203.0.113.7:5555"
	key := oauthCIMDSourceKey(resolveReq(remote))
	n := defaultOAuthCIMDSourceBurst + 10
	for i := 0; i < n; i++ {
		if _, err := srv.resolveOAuthClient(resolveReq(remote), "client-x"); err != nil {
			t.Fatalf("store resolution %d must never be rate limited, got %v", i, err)
		}
	}
	if rt.fetches() != 0 {
		t.Fatalf("store hits dialled CIMD %d times", rt.fetches())
	}
	if got := srv.oauthCIMDLimiter.sourceTokens(key); got != float64(defaultOAuthCIMDSourceBurst) {
		t.Fatalf("store-hit path drained the source bucket: tokens = %v, want %d (untouched)", got, defaultOAuthCIMDSourceBurst)
	}
}

func assertUnconfigured(t *testing.T, rr *httptest.ResponseRecorder) {
	t.Helper()
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "oauth_as_unconfigured") {
		t.Fatalf("body = %q, want oauth_as_unconfigured", rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "client_id_metadata_document_supported") {
		t.Fatal("a non-enabled AS rendered a metadata document")
	}
}

// ---------------------------------------------------------------------------
// Client resolution (FIX 1).
// ---------------------------------------------------------------------------

func storeClient(provider, clientID string, redirects []string) *oauthstore.Client {
	return &oauthstore.Client{
		Provider:                provider,
		ClientID:                clientID,
		RedirectURIs:            redirects,
		GrantTypes:              []string{"authorization_code", "refresh_token"},
		ResponseTypes:           []string{"code"},
		TokenEndpointAuthMethod: "none",
	}
}

func TestResolveOAuthClient_SingleProviderHit(t *testing.T) {
	store := newFakeOAuthStore()
	store.seedClient(storeClient("github", "client-x", []string{"https://app.example/cb"}))
	rt := newCIMD()
	srv := newEnabledOAuthServer(store, newCIMDFetcher(rt))
	c, err := srv.resolveOAuthClient(resolveReq("192.0.2.1:1234"), "client-x")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !equalStrings(c.RedirectURIs, []string{"https://app.example/cb"}) {
		t.Fatalf("redirect uris = %v", c.RedirectURIs)
	}
	if rt.fetches() != 0 {
		t.Fatalf("CIMD fetched %d times on a store hit", rt.fetches())
	}
}

func TestResolveOAuthClient_IdenticalRegistrationsAcrossProvidersResolve(t *testing.T) {
	store := newFakeOAuthStore()
	store.seedClient(storeClient("github", "client-x", []string{"https://app.example/cb"}))
	store.seedClient(storeClient("gitlab", "client-x", []string{"https://app.example/cb"}))
	srv := newEnabledOAuthServer(store, newCIMDFetcher(newCIMD()))
	c, err := srv.resolveOAuthClient(resolveReq("192.0.2.1:1234"), "client-x")
	if err != nil {
		t.Fatalf("identical duplicates should resolve, got %v", err)
	}
	if !equalStrings(c.RedirectURIs, []string{"https://app.example/cb"}) {
		t.Fatalf("redirect uris = %v", c.RedirectURIs)
	}
}

func TestResolveOAuthClient_DivergentRegistrationsAcrossProvidersFailClosed(t *testing.T) {
	store := newFakeOAuthStore()
	store.seedClient(storeClient("github", "client-x", []string{"https://a.example/cb"}))
	store.seedClient(storeClient("gitlab", "client-x", []string{"https://b.example/cb"}))
	rt := newCIMD()
	srv := newEnabledOAuthServer(store, newCIMDFetcher(rt))
	_, err := srv.resolveOAuthClient(resolveReq("192.0.2.1:1234"), "client-x")
	if err == nil {
		t.Fatal("divergent duplicates must fail closed")
	}
	if err.Code != oauthas.ErrCodeInvalidClient {
		t.Fatalf("code = %q, want invalid_client", err.Code)
	}
	if rt.fetches() != 0 {
		t.Fatalf("ambiguity fell through to CIMD (%d fetches)", rt.fetches())
	}
}

func TestResolveOAuthClient_CIMDFetchOnStoreMiss(t *testing.T) {
	store := newFakeOAuthStore()
	rt := newCIMD()
	clientID := "https://client.example/cimd"
	rt.docs[clientID] = cimdDoc(clientID, []string{"https://client.example/cb"}, nil, nil, "")
	srv := newEnabledOAuthServer(store, newCIMDFetcher(rt))
	c, err := srv.resolveOAuthClient(resolveReq("192.0.2.1:1234"), clientID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !equalStrings(c.RedirectURIs, []string{"https://client.example/cb"}) {
		t.Fatalf("redirect uris = %v", c.RedirectURIs)
	}
	if rt.fetches() == 0 {
		t.Fatal("expected a CIMD fetch on store miss")
	}
}

func TestResolveOAuthClient_PreRegistrationBeatsCIMDCache(t *testing.T) {
	store := newFakeOAuthStore()
	clientID := "https://client.example/cimd"
	rt := newCIMD()
	// The CIMD document registers ONLY redirect B.
	rt.docs[clientID] = cimdDoc(clientID, []string{"https://client.example/B"}, nil, nil, "")
	srv := newEnabledOAuthServer(store, newCIMDFetcher(rt))

	// WARM THE CIMD CACHE FIRST. With no store row, the id resolves via CIMD (B)
	// and the fetcher now holds a cached document for clientID. This is the cache
	// hit the test's name claims: a cache-first resolver that consulted a
	// POPULATED cache before the store would serve B on the next call. (The prior
	// version started with an empty cache, so a cache-first resolver would miss
	// the cache, fall through to the store, and pass — the test was vacuous.)
	if c, err := srv.resolveOAuthClient(resolveReq("192.0.2.1:1234"), clientID); err != nil {
		t.Fatalf("warm: %v", err)
	} else if !equalStrings(c.RedirectURIs, []string{"https://client.example/B"}) {
		t.Fatalf("warm redirect uris = %v, want B", c.RedirectURIs)
	}
	if rt.fetches() != 1 {
		t.Fatalf("warm expected exactly one fetch, got %d", rt.fetches())
	}

	// Now pre-register the SAME client_id in the store with redirect A.
	store.seedClient(storeClient("github", clientID, []string{"https://client.example/A"}))

	// The store hit must beat the WARM CIMD cache entry (B), and must not trigger
	// a second fetch — store-first short-circuits before the fetcher is touched.
	c, err := srv.resolveOAuthClient(resolveReq("192.0.2.1:1234"), clientID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !equalStrings(c.RedirectURIs, []string{"https://client.example/A"}) {
		t.Fatalf("pre-registration must beat the warm cache: redirect uris = %v", c.RedirectURIs)
	}
	if rt.fetches() != 1 {
		t.Fatalf("store hit consulted CIMD again (%d fetches, want 1)", rt.fetches())
	}
}

func TestResolveOAuthClient_UnknownClientIsInvalidClient(t *testing.T) {
	srv := newEnabledOAuthServer(newFakeOAuthStore(), newCIMDFetcher(newCIMD()))
	_, err := srv.resolveOAuthClient(resolveReq("192.0.2.1:1234"), "not-a-url")
	if err == nil || err.Code != oauthas.ErrCodeInvalidClient {
		t.Fatalf("err = %v, want invalid_client", err)
	}
}

func TestResolveOAuthClient_StoreErrorIsNotInvalidClient(t *testing.T) {
	store := newFakeOAuthStore()
	store.getErr = context.DeadlineExceeded
	srv := newEnabledOAuthServer(store, newCIMDFetcher(newCIMD()))
	_, err := srv.resolveOAuthClient(resolveReq("192.0.2.1:1234"), "client-x")
	if err == nil || err.Code != oauthas.ErrCodeTemporarilyUnavailable {
		t.Fatalf("err = %v, want temporarily_unavailable", err)
	}
}

// cimdDoc renders a CIMD JSON document. grantTypes/responseTypes nil omits the
// field entirely (so the RFC 7591 default applies); scope "" omits scope.
func cimdDoc(clientID string, redirects, grantTypes, responseTypes []string, scope string) string {
	m := map[string]any{
		"client_id":                  clientID,
		"redirect_uris":              redirects,
		"token_endpoint_auth_method": "none",
	}
	if grantTypes != nil {
		m["grant_types"] = grantTypes
	}
	if responseTypes != nil {
		m["response_types"] = responseTypes
	}
	if scope != "" {
		m["scope"] = scope
	}
	b, _ := json.Marshal(m)
	return string(b)
}
