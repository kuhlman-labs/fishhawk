package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/identity"
)

// Cross-layer tests for the token-login client binding (#4627): POST
// /v0/tokens/login → handleTokenLoginMint → the REAL identity providers →
// httptest forges → the token repository. Every fixture describes a
// non-Fishhawk operator deployment (repo acme/widgets, device clients
// Iv1.acme-device / acme-device-gl) per ADR-110.
//
// Each fake forge resolves a subject for EVERY token in its table and
// grants that subject owner/admin on acme/widgets, so the ONLY thing
// between each refused token class and a 201 is the client binding. Every
// case reads the token repository after the call: committed state, not
// error identity.

const (
	acmeRepo           = "acme/widgets"
	acmeGitHubClientID = "Iv1.acme-device"
	acmeGitHubSecret   = "acme-secret"
	acmeGitLabClientID = "acme-device-gl"
)

// forgeAnswer is one token's introspection answer in a fake forge table.
type forgeAnswer struct {
	status int // 0 → 200
	body   string
}

// newAcmeGitHubForge serves check-a-token over table (Basic credentials
// required), GET /user resolving every table token to alice, and admin on
// acme/widgets. userStatus/userBody, when set, replace the /user answer.
func newAcmeGitHubForge(t *testing.T, table map[string]forgeAnswer, userStatus int, userBody string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/applications/"+acmeGitHubClientID+"/token":
			id, secret, ok := r.BasicAuth()
			if !ok || id != acmeGitHubClientID || secret != acmeGitHubSecret {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			var body struct {
				AccessToken string `json:"access_token"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			ans, known := table[body.AccessToken]
			if !known {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if ans.status != 0 {
				w.WriteHeader(ans.status)
			}
			_, _ = w.Write([]byte(ans.body))
		case r.URL.Path == "/user":
			if _, known := table[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]; !known {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if userStatus != 0 {
				w.WriteHeader(userStatus)
				_, _ = w.Write([]byte(userBody))
				return
			}
			_, _ = w.Write([]byte(`{"login":"alice"}`))
		case r.URL.Path == "/repos/"+acmeRepo+"/collaborators/alice/permission":
			_, _ = w.Write([]byte(`{"role_name":"admin"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newAcmeGitLabForge serves token-info over table, GET /api/v4/user
// resolving every table token to alice, and owner on acme/widgets.
func newAcmeGitLabForge(t *testing.T, table map[string]forgeAnswer, userStatus int, userBody string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		switch r.URL.EscapedPath() {
		case "/oauth/token/info":
			ans, known := table[tok]
			if !known {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if ans.status != 0 {
				w.WriteHeader(ans.status)
			}
			_, _ = w.Write([]byte(ans.body))
		case "/api/v4/user":
			if _, known := table[tok]; !known {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if userStatus != 0 {
				w.WriteHeader(userStatus)
				_, _ = w.Write([]byte(userBody))
				return
			}
			_, _ = w.Write([]byte(`{"id":7,"username":"alice"}`))
		case "/api/v4/projects/acme%2Fwidgets/members/all":
			_, _ = w.Write([]byte(`[{"username":"alice","access_level":50}]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// acmeLoginFixture is one token-login server over the real provider(s),
// with every sink a refusal could reach captured.
type acmeLoginFixture struct {
	s    *Server
	repo *fakeTokenRepo
	aud  *foAuditFake
	logs *bytes.Buffer
}

func newAcmeLoginFixture(t *testing.T, mut func(*Config)) *acmeLoginFixture {
	t.Helper()
	fx := &acmeLoginFixture{repo: newFakeTokenRepo(), aud: &foAuditFake{}, logs: &bytes.Buffer{}}
	cfg := Config{
		Addr:                  "127.0.0.1:0",
		APITokenRepo:          fx.repo,
		AuditRepo:             fx.aud,
		Logger:                slog.New(slog.NewTextHandler(fx.logs, nil)),
		OperatorRepo:          acmeRepo,
		OperatorMinPermission: identity.PermissionWrite,
		OperatorDefaultScopes: []string{"read:runs", "write:runs"},
	}
	mut(&cfg)
	fx.s = New(cfg)
	return fx
}

func acmeGitHubConfig(forgeURL string, opts ...identity.Option) func(*Config) {
	return func(cfg *Config) {
		opts = append([]identity.Option{identity.WithBaseURLs(forgeURL, forgeURL)}, opts...)
		cfg.IdentityProvider = identity.NewGitHubIdentityProvider(acmeGitHubClientID, nil, opts...)
		cfg.OAuthClientID = acmeGitHubClientID
	}
}

func acmeGitLabConfig(forgeURL, providerClientID string) func(*Config) {
	return func(cfg *Config) {
		cfg.IdentityProviders = map[string]identity.IdentityProvider{
			identity.ProviderGitLab: identity.NewGitLabIdentityProvider(forgeURL, providerClientID, nil),
		}
		// Registered under a non-empty device client id so the provider is
		// enumerated as configured, whatever id it was built with.
		cfg.IdentityDeviceClientIDs = map[string]string{identity.ProviderGitLab: acmeGitLabClientID}
		cfg.IdentityBaseURLs = map[string]string{identity.ProviderGitLab: forgeURL}
	}
}

func (fx *acmeLoginFixture) rows() int {
	fx.repo.mu.Lock()
	defer fx.repo.mu.Unlock()
	return len(fx.repo.rows)
}

func (fx *acmeLoginFixture) mint(t *testing.T, provider, token string) *httptest.ResponseRecorder {
	t.Helper()
	return tokenRequest(t, fx.s, http.MethodPost, "/v0/tokens/login", "",
		map[string]any{"access_token": token, "provider": provider})
}

// refusedCase is one token class the binding must refuse, with the closed
// reason class the server log must name for it.
type refusedCase struct {
	name, token, reason string
}

func runClientBindingEndToEnd(t *testing.T, provider, acceptedToken, wantSubject string,
	newFixture func(t *testing.T) *acmeLoginFixture, refused []refusedCase,
) {
	t.Run("configured device client mints", func(t *testing.T) {
		fx := newFixture(t)
		w := fx.mint(t, provider, acceptedToken)
		if w.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
		}
		var created apiTokenCreatedResponse
		if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
			t.Fatal(err)
		}
		if created.Subject != wantSubject || created.Provider != provider {
			t.Errorf("minted subject/provider = %q/%q, want %q/%q", created.Subject, created.Provider, wantSubject, provider)
		}
		if n := fx.rows(); n != 1 {
			t.Errorf("token rows after mint = %d, want 1", n)
		}
	})
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFixture(t)
			w := fx.mint(t, provider, tc.token)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (the token is not bound to the deployment's device client):\n%s", w.Code, w.Body.String())
			}
			if code := errorCode(t, w); code != "token_verification_failed" {
				t.Errorf("error code = %q, want token_verification_failed", code)
			}
			if n := fx.rows(); n != 0 {
				t.Errorf("token rows after a refused mint = %d, want 0", n)
			}
			if logs := fx.logs.String(); !strings.Contains(logs, "reason="+tc.reason) || !strings.Contains(logs, "provider="+provider) {
				t.Errorf("server log missing provider=%s reason=%s:\n%s", provider, tc.reason, logs)
			}
		})
	}
}

// TestTokenLoginMint_ClientBinding_GitHub_EndToEnd: only a token GitHub's
// check-a-token binds to the configured device client mints.
func TestTokenLoginMint_ClientBinding_GitHub_EndToEnd(t *testing.T) {
	configured := fmt.Sprintf(`{"app":{"client_id":%q},"user":{"login":"alice"}}`, acmeGitHubClientID)
	table := map[string]forgeAnswer{
		"gho_acme":    {body: configured},
		"gho_other":   {status: http.StatusNotFound},
		"ghp_pat":     {status: http.StatusNotFound},
		"ghs_install": {status: http.StatusNotFound},
		"gho_noapp":   {body: `{"user":{"login":"alice"}}`},
		"gho_500":     {status: http.StatusInternalServerError},
	}
	newFixture := func(t *testing.T) *acmeLoginFixture {
		forge := newAcmeGitHubForge(t, table, 0, "")
		return newAcmeLoginFixture(t, acmeGitHubConfig(forge.URL, identity.WithClientSecret(acmeGitHubSecret)))
	}
	runClientBindingEndToEnd(t, identity.ProviderGitHub, "gho_acme", "github:alice", newFixture, []refusedCase{
		{"other-oauth-client", "gho_other", tokenLoginReasonIntrospectionRefused},
		{"personal-access-token", "ghp_pat", tokenLoginReasonIntrospectionRefused},
		{"app-installation-token", "ghs_install", tokenLoginReasonIntrospectionRefused},
		{"undeterminable-issuer", "gho_noapp", tokenLoginReasonIssuerMismatch},
		{"introspection-failure", "gho_500", tokenLoginReasonIntrospectionRefused},
	})
}

// TestTokenLoginMint_ClientBinding_GitLab_EndToEnd: only a token GitLab's
// token-info binds to the configured device application mints.
func TestTokenLoginMint_ClientBinding_GitLab_EndToEnd(t *testing.T) {
	table := map[string]forgeAnswer{
		"gloas-acme":    {body: fmt.Sprintf(`{"application":{"uid":%q}}`, acmeGitLabClientID)},
		"gloas-other":   {body: `{"application":{"uid":"other-oauth-application"}}`},
		"glpat-user":    {status: http.StatusUnauthorized},
		"glpat-project": {status: http.StatusUnauthorized},
		"glcbt-job":     {status: http.StatusUnauthorized},
		"gloas-null":    {body: `{"application":null}`},
		"gloas-500":     {status: http.StatusInternalServerError},
	}
	newFixture := func(t *testing.T) *acmeLoginFixture {
		forge := newAcmeGitLabForge(t, table, 0, "")
		return newAcmeLoginFixture(t, acmeGitLabConfig(forge.URL, acmeGitLabClientID))
	}
	runClientBindingEndToEnd(t, identity.ProviderGitLab, "gloas-acme", "gitlab:alice", newFixture, []refusedCase{
		{"other-oauth-application", "gloas-other", tokenLoginReasonIssuerMismatch},
		{"personal-access-token", "glpat-user", tokenLoginReasonIntrospectionRefused},
		{"project-access-token", "glpat-project", tokenLoginReasonIntrospectionRefused},
		{"ci-job-token", "glcbt-job", tokenLoginReasonIntrospectionRefused},
		{"undeterminable-issuer", "gloas-null", tokenLoginReasonIssuerMismatch},
		{"introspection-failure", "gloas-500", tokenLoginReasonIntrospectionRefused},
	})
}

// TestTokenLoginMint_ClientCredentialUnconfigured_503: a provider missing
// its client-binding credential is a config gate (503 naming the env var),
// never a 401 and never an owner-only mint. Each forge here WOULD accept
// the token, so only the credential gate and its handler mapping refuse.
func TestTokenLoginMint_ClientCredentialUnconfigured_503(t *testing.T) {
	cases := []struct {
		name, provider, token, wantEnv string
		fixture                        func(t *testing.T) *acmeLoginFixture
	}{
		{
			name: "github without the client secret", provider: identity.ProviderGitHub, token: "gho_acme",
			wantEnv: "FISHHAWKD_OAUTH_CLIENT_SECRET",
			fixture: func(t *testing.T) *acmeLoginFixture {
				forge := newAcmeGitHubForge(t, map[string]forgeAnswer{
					"gho_acme": {body: fmt.Sprintf(`{"app":{"client_id":%q}}`, acmeGitHubClientID)},
				}, 0, "")
				return newAcmeLoginFixture(t, acmeGitHubConfig(forge.URL)) // no WithClientSecret
			},
		},
		{
			name: "gitlab without the device client id", provider: identity.ProviderGitLab, token: "gloas-acme",
			wantEnv: "FISHHAWKD_GITLAB_DEVICE_CLIENT_ID",
			fixture: func(t *testing.T) *acmeLoginFixture {
				forge := newAcmeGitLabForge(t, map[string]forgeAnswer{
					"gloas-acme": {body: `{"application":{"uid":null}}`},
				}, 0, "")
				return newAcmeLoginFixture(t, acmeGitLabConfig(forge.URL, ""))
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := tc.fixture(t)
			w := fx.mint(t, tc.provider, tc.token)
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503 tokens_unconfigured:\n%s", w.Code, w.Body.String())
			}
			if code := errorCode(t, w); code != "tokens_unconfigured" {
				t.Errorf("error code = %q, want tokens_unconfigured", code)
			}
			if !strings.Contains(w.Body.String(), tc.wantEnv) {
				t.Errorf("503 body does not name %s:\n%s", tc.wantEnv, w.Body.String())
			}
			if n := fx.rows(); n != 0 {
				t.Errorf("token rows = %d, want 0", n)
			}
		})
	}
}

// TestTokenLoginMint_VerificationErrorNeverLeaksUpstreamBody (C1): the
// introspection binds the token, then the forge's user lookup fails with an
// error body that ECHOES the submitted access token. The provider's error
// text embeds that body, so the token must be absent from every sink the
// token-login path writes: (a) the server log, (b) the raw HTTP response
// body, (c) every audit row read back from the store. The log sink is
// asserted NON-EMPTY first, so its absence check is not vacuous.
func TestTokenLoginMint_VerificationErrorNeverLeaksUpstreamBody(t *testing.T) {
	cases := []struct {
		name, provider, token string
		fixture               func(t *testing.T, token string) *acmeLoginFixture
	}{
		{
			name: "github", provider: identity.ProviderGitHub, token: "gho_acmeLeakCanary",
			fixture: func(t *testing.T, token string) *acmeLoginFixture {
				forge := newAcmeGitHubForge(t, map[string]forgeAnswer{
					token: {body: fmt.Sprintf(`{"app":{"client_id":%q}}`, acmeGitHubClientID)},
				}, http.StatusUnauthorized, fmt.Sprintf(`{"message":"Bad credentials: %s"}`, token))
				return newAcmeLoginFixture(t, acmeGitHubConfig(forge.URL, identity.WithClientSecret(acmeGitHubSecret)))
			},
		},
		{
			name: "gitlab", provider: identity.ProviderGitLab, token: "gloas-acmeLeakCanary",
			fixture: func(t *testing.T, token string) *acmeLoginFixture {
				forge := newAcmeGitLabForge(t, map[string]forgeAnswer{
					token: {body: fmt.Sprintf(`{"application":{"uid":%q}}`, acmeGitLabClientID)},
				}, http.StatusUnauthorized, fmt.Sprintf(`{"message":"401 Unauthorized: %s"}`, token))
				return newAcmeLoginFixture(t, acmeGitLabConfig(forge.URL, acmeGitLabClientID))
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := tc.fixture(t, tc.token)
			w := fx.mint(t, tc.provider, tc.token)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401:\n%s", w.Code, w.Body.String())
			}

			// (a) the server log: written (provider + reason class), token-free.
			logs := fx.logs.String()
			if !strings.Contains(logs, "token-login verification refused") ||
				!strings.Contains(logs, "reason="+tokenLoginReasonSubjectResolutionFailed) {
				t.Fatalf("server log missing the refusal line with reason=%s:\n%s", tokenLoginReasonSubjectResolutionFailed, logs)
			}
			if strings.Contains(logs, tc.token) {
				t.Errorf("server log carries the submitted access token:\n%s", logs)
			}
			// (b) the raw HTTP response body.
			if strings.Contains(w.Body.String(), tc.token) {
				t.Errorf("response body carries the submitted access token:\n%s", w.Body.String())
			}
			// (c) every audit row the request wrote, read back from the store.
			fx.aud.mu.Lock()
			defer fx.aud.mu.Unlock()
			for _, row := range fx.aud.global {
				if strings.Contains(string(row.Payload), tc.token) {
					t.Errorf("global audit row %s carries the submitted access token: %s", row.Category, row.Payload)
				}
			}
			for _, row := range fx.aud.chained {
				if strings.Contains(string(row.Payload), tc.token) {
					t.Errorf("audit row %s carries the submitted access token: %s", row.Category, row.Payload)
				}
			}
			if n := fx.rows(); n != 0 {
				t.Errorf("token rows = %d, want 0", n)
			}
		})
	}
}

// TestTokenLoginRefusalReason pins the closed-set classification by
// sentinel identity.
func TestTokenLoginRefusalReason(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("x: %w", identity.ErrRateLimited), tokenLoginReasonRateLimited},
		{identity.ErrTokenClientMismatch, tokenLoginReasonIssuerMismatch},
		{fmt.Errorf("x: %w", identity.ErrIntrospectionRefused), tokenLoginReasonIntrospectionRefused},
		{fmt.Errorf("x: %w", identity.ErrIntrospectionFailed), tokenLoginReasonIntrospectionFailed},
		{fmt.Errorf("identity: get user: 401: body"), tokenLoginReasonSubjectResolutionFailed},
	} {
		if got := tokenLoginRefusalReason(tc.err); got != tc.want {
			t.Errorf("tokenLoginRefusalReason(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
	if got := tokenLoginCredentialUnconfiguredMessage("forgejo"); strings.Contains(got, "FISHHAWKD_") {
		t.Errorf("unmapped provider message = %q, want the generic wording", got)
	}
}
