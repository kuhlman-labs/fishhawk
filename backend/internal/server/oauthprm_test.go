package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/oauthas"
)

// TestProtectedResourceMetadataURL tables the RFC 9728 §3.1 derivation. The
// percent-encoded row is the load-bearing one: prmURL keeps the escaped segment
// (so the advertised URL is valid) while suffix is DECODED (so it matches the
// decoded PathValue the suffixed route compares against — CONDITION 1).
func TestProtectedResourceMetadataURL(t *testing.T) {
	cases := []struct {
		name       string
		resource   string
		wantURL    string
		wantSuffix string
	}{
		{"empty path", "https://as.example", "https://as.example/.well-known/oauth-protected-resource", ""},
		{"mcp path", "https://as.example/mcp", "https://as.example/.well-known/oauth-protected-resource/mcp", "mcp"},
		{"issuer with base path", "https://as.example/tenant/mcp", "https://as.example/.well-known/oauth-protected-resource/tenant/mcp", "tenant/mcp"},
		{"trailing slash trimmed", "https://as.example/mcp/", "https://as.example/.well-known/oauth-protected-resource/mcp", "mcp"},
		{"percent-encoded segment", "https://as.example/a%2Fb", "https://as.example/.well-known/oauth-protected-resource/a%2Fb", "a/b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := oauthas.ParseResourceIdentifier(tc.resource)
			if err != nil {
				t.Fatalf("ParseResourceIdentifier(%q): %v", tc.resource, err)
			}
			gotURL, gotSuffix, err := protectedResourceMetadataURL(res)
			if err != nil {
				t.Fatalf("protectedResourceMetadataURL(%q): %v", tc.resource, err)
			}
			if gotURL != tc.wantURL {
				t.Errorf("prmURL = %q, want %q", gotURL, tc.wantURL)
			}
			if gotSuffix != tc.wantSuffix {
				t.Errorf("suffix = %q, want %q", gotSuffix, tc.wantSuffix)
			}
		})
	}
}

// TestProtectedResourceMetadataURL_NonHTTPSchemeErrors pins the derivation
// error branch resolveOAuthASState fails closed on: a resource whose scheme
// cannot host an http(s) PRM document.
func TestProtectedResourceMetadataURL_NonHTTPSchemeErrors(t *testing.T) {
	res, err := oauthas.ParseResourceIdentifier("ftp://as.example/mcp")
	if err != nil {
		t.Fatalf("ParseResourceIdentifier: %v", err)
	}
	if _, _, err := protectedResourceMetadataURL(res); err == nil {
		t.Fatal("protectedResourceMetadataURL(ftp://…) = nil error, want an error naming the scheme")
	}
}

// TestOAuthChallengeHeader covers the two correct forms: realm-only when no
// issuer is configured (prmURL empty), and the resource_metadata form otherwise.
func TestOAuthChallengeHeader(t *testing.T) {
	const prm = "https://as.example/.well-known/oauth-protected-resource/mcp"
	if got := oauthChallengeHeader(prm); got != `Bearer realm="fishhawk", resource_metadata="`+prm+`"` {
		t.Errorf("with prmURL: got %q", got)
	}
	if got := oauthChallengeHeader(""); got != `Bearer realm="fishhawk"` {
		t.Errorf("empty prmURL: got %q, want realm-only", got)
	}
}

// TestOAuthChallengeHeader_OmitsUnescapableResourceMetadata pins the
// fail-closed guard: an unescapable value is OMITTED and the realm-only
// challenge returned, never spliced into the quoted-string header.
func TestOAuthChallengeHeader_OmitsUnescapableResourceMetadata(t *testing.T) {
	for _, bad := range []struct {
		name, in string
	}{
		{"double quote", `https://as.example/"injected`},
		{"backslash", `https://as.example/\injected`},
		{"control byte", "https://as.example/\x01injected"},
		{"DEL byte", "https://as.example/\x7finjected"},
	} {
		t.Run(bad.name, func(t *testing.T) {
			if got := oauthChallengeHeader(bad.in); got != `Bearer realm="fishhawk"` {
				t.Errorf("oauthChallengeHeader(%q) = %q, want realm-only", bad.in, got)
			}
		})
	}
}

// TestOAuthPRM_ServedAndSelfConsistentWithASMetadata drives both discovery
// documents through the real mux and asserts they agree: the PRM's
// authorization_servers[0] is the issuer the AS metadata reports, and their
// scopes_supported sets are identical.
func TestOAuthPRM_ServedAndSelfConsistentWithASMetadata(t *testing.T) {
	s := newEnabledOAuthServer(newFakeOAuthStore(), newCIMDFetcher(newCIMD()))

	var prm oauthPRMetadata
	getJSON(t, s, "/.well-known/oauth-protected-resource", &prm)
	var asMeta oauthASMetadata
	getJSON(t, s, "/.well-known/oauth-authorization-server", &asMeta)

	if prm.Resource != testResource {
		t.Errorf("PRM resource = %q, want %q", prm.Resource, testResource)
	}
	if len(prm.AuthorizationServers) != 1 || prm.AuthorizationServers[0] != asMeta.Issuer {
		t.Errorf("authorization_servers = %v, want [%q]", prm.AuthorizationServers, asMeta.Issuer)
	}
	if !slices.Equal(prm.ScopesSupported, asMeta.ScopesSupported) {
		t.Errorf("PRM scopes_supported %v != AS metadata scopes_supported %v", prm.ScopesSupported, asMeta.ScopesSupported)
	}
	if !slices.Equal(prm.BearerMethodsSupported, []string{"header"}) {
		t.Errorf("bearer_methods_supported = %v, want [header]", prm.BearerMethodsSupported)
	}
}

// getJSON drives one GET through the full handler and decodes a 200 body.
func getJSON(t *testing.T, s *Server, path string, out any) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: status = %d, want 200:\n%s", path, rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
		t.Fatalf("GET %s: decode body: %v\n%s", path, err, rec.Body.String())
	}
}

// TestOAuthPRM_503WhenASDisabled: an unconfigured AS answers 503
// oauth_as_unconfigured with NO partial document, on both PRM routes.
func TestOAuthPRM_503WhenASDisabled(t *testing.T) {
	s := New(Config{}) // AS disabled
	for _, path := range []string{
		"/.well-known/oauth-protected-resource",
		"/.well-known/oauth-protected-resource/mcp",
	} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("GET %s: status = %d, want 503:\n%s", path, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "oauth_as_unconfigured") {
			t.Errorf("GET %s: body = %s, want oauth_as_unconfigured", path, rec.Body.String())
		}
	}
}

// TestOAuthPRM_503WhenASMisconfigured: an issuer configured but a defective
// prerequisite (nil store) is a 503, never a partial document.
func TestOAuthPRM_503WhenASMisconfigured(t *testing.T) {
	s := New(Config{OAuthASIssuer: testIssuer}) // issuer but no store → misconfigured
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503:\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "oauth_as_unconfigured") {
		t.Errorf("body = %s, want oauth_as_unconfigured", rec.Body.String())
	}
}

// TestOAuthPRM_SuffixedRouteExactMatch serves on an exact suffix match.
func TestOAuthPRM_SuffixedRouteExactMatch(t *testing.T) {
	s := newEnabledOAuthServer(newFakeOAuthStore(), newCIMDFetcher(newCIMD()))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource/mcp", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", rec.Code, rec.Body.String())
	}
	var doc oauthPRMetadata
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if doc.Resource != testResource {
		t.Errorf("resource = %q, want %q", doc.Resource, testResource)
	}
}

// TestOAuthPRM_SuffixedRoute404OnWrongSuffix: a suffix that is not the derived
// prmSuffix is 404 route_not_found — the document is never served under a URL
// claiming a different resource.
func TestOAuthPRM_SuffixedRoute404OnWrongSuffix(t *testing.T) {
	s := newEnabledOAuthServer(newFakeOAuthStore(), newCIMDFetcher(newCIMD()))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource/not-the-resource", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404:\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "route_not_found") {
		t.Errorf("body = %s, want route_not_found", rec.Body.String())
	}
}

// TestOAuthPRM_SuffixedRouteServedForEncodedResourcePath is the CONDITION-1
// routing-layer proof: for a resource whose path REQUIRES encoding, a REAL
// request to the derived PRM URL must resolve 200 through the mux. Reverting the
// suffix derivation to the escaped (mismatched) form reddens the encoded row
// while the plain /mcp row stays green — which is precisely why a derivation-only
// string test would have missed it.
func TestOAuthPRM_SuffixedRouteServedForEncodedResourcePath(t *testing.T) {
	cases := []struct{ name, resource string }{
		{"plain path needs no encoding", "https://as.example/mcp"},
		{"path requires encoding", "https://as.example/a%2Fb"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(Config{
				OAuthASIssuer:    testIssuer,
				OAuthASResource:  tc.resource,
				OAuthStore:       newFakeOAuthStore(),
				OAuthCIMDFetcher: newCIMDFetcher(newCIMD()),
			})
			if s.oauthAS.mode != oauthASEnabled {
				t.Fatalf("AS mode = %v (reason %q), want enabled", s.oauthAS.mode, s.oauthAS.reason)
			}
			// A REAL request to the derived PRM URL (routing layer), not a
			// string comparison.
			req := httptest.NewRequest(http.MethodGet, s.oauthAS.prmURL, nil)
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s: status = %d, want 200:\n%s", s.oauthAS.prmURL, rec.Code, rec.Body.String())
			}
			var doc oauthPRMetadata
			if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
				t.Fatalf("decode PRM doc: %v", err)
			}
			if doc.Resource != tc.resource {
				t.Errorf("resource = %q, want %q", doc.Resource, tc.resource)
			}
		})
	}
}
