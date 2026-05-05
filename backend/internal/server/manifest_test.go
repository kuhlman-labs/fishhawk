package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/auth"
)

// stubManifestServer mounts an httptest endpoint that mimics
// api.github.com/app-manifests/{code}/conversions. failCode and
// failBody, when non-zero, replace the happy-path response.
func stubManifestServer(t *testing.T) (*httptest.Server, *auth.GitHubManifest, *manifestStub) {
	t.Helper()
	stub := &manifestStub{respCode: http.StatusCreated}
	mux := http.NewServeMux()
	mux.HandleFunc("/app-manifests/", func(w http.ResponseWriter, r *http.Request) {
		stub.calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(stub.respCode)
		_, _ = w.Write([]byte(stub.respBody))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	mc := auth.NewGitHubManifest(auth.ManifestURLs{ConversionsURL: srv.URL + "/app-manifests"})
	return srv, mc, stub
}

type manifestStub struct {
	respCode int
	respBody string
	calls    int
}

// newManifestServer builds a Server wired with a stub
// GitHubManifest client. AuthRepo is nil — manifest endpoints
// don't depend on it.
func newManifestServer(t *testing.T) (*Server, *manifestStub) {
	t.Helper()
	_, mc, stub := stubManifestServer(t)
	s := New(Config{
		Addr:           "127.0.0.1:0",
		GitHubManifest: mc,
	})
	return s, stub
}

func TestManifestFlowStart_HappyPath(t *testing.T) {
	s, _ := newManifestServer(t)
	q := url.Values{
		"backend_url": {"http://localhost:8080"},
		"webhook_url": {"https://smee.io/abc123"},
	}
	req := httptest.NewRequest(http.MethodGet,
		"/v0/auth/github/manifest-flow-start?"+q.Encode(), nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q", ct)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}

	// State cookie set, HttpOnly + Secure, on the auth path.
	var state *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == manifestStateCookieName {
			state = c
		}
	}
	if state == nil {
		t.Fatal("manifest state cookie not set")
	}
	if !state.HttpOnly || !state.Secure {
		t.Errorf("state cookie missing HttpOnly/Secure: %+v", state)
	}
	if state.Path != "/v0/auth/github/" {
		t.Errorf("state cookie path = %q", state.Path)
	}

	body := w.Body.String()
	if !strings.Contains(body, "https://github.com/settings/apps/new?state=") {
		t.Errorf("body missing GitHub form action:\n%s", body)
	}
	// The form action must round-trip the same state we set in the cookie.
	if !strings.Contains(body, "state="+url.QueryEscape(state.Value)) {
		t.Errorf("form action does not round-trip state cookie %q", state.Value)
	}
	// Manifest payload should embed the backend + webhook URLs.
	if !strings.Contains(body, "https://smee.io/abc123") {
		t.Errorf("body missing webhook URL:\n%s", body)
	}
	if !strings.Contains(body, "http://localhost:8080") {
		t.Errorf("body missing backend URL:\n%s", body)
	}
}

func TestManifestFlowStart_OrgOwner(t *testing.T) {
	s, _ := newManifestServer(t)
	q := url.Values{
		"backend_url": {"http://localhost:8080"},
		"webhook_url": {"https://smee.io/abc"},
		"owner":       {"my-org"},
	}
	req := httptest.NewRequest(http.MethodGet,
		"/v0/auth/github/manifest-flow-start?"+q.Encode(), nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "https://github.com/organizations/my-org/settings/apps/new?state=") {
		t.Errorf("body missing org-targeted form action:\n%s", w.Body.String())
	}
}

func TestManifestFlowStart_MissingURLs_400(t *testing.T) {
	cases := map[string]url.Values{
		"missing backend_url": {"webhook_url": {"https://smee.io/abc"}},
		"missing webhook_url": {"backend_url": {"http://localhost:8080"}},
		"non-http scheme":     {"backend_url": {"ssh://x"}, "webhook_url": {"https://smee.io/abc"}},
	}
	for name, q := range cases {
		t.Run(name, func(t *testing.T) {
			s, _ := newManifestServer(t)
			req := httptest.NewRequest(http.MethodGet,
				"/v0/auth/github/manifest-flow-start?"+q.Encode(), nil)
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", w.Code)
			}
		})
	}
}

func TestManifestFlowStart_Unconfigured_503(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"}) // no GitHubManifest
	req := httptest.NewRequest(http.MethodGet,
		"/v0/auth/github/manifest-flow-start?backend_url=http://x&webhook_url=http://y", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestManifestCallback_HappyPath(t *testing.T) {
	s, stub := newManifestServer(t)
	body, _ := json.Marshal(map[string]any{
		"id":             int64(123456),
		"slug":           "fishhawk-local",
		"name":           "Fishhawk (local)",
		"html_url":       "https://github.com/apps/fishhawk-local",
		"client_id":      "Iv1.abc",
		"client_secret":  "s3cret",
		"webhook_secret": "hookhook",
		"pem":            "-----BEGIN RSA PRIVATE KEY-----\n...\n-----END RSA PRIVATE KEY-----\n",
	})
	stub.respBody = string(body)

	state := "manifest-state-xyz"
	req := httptest.NewRequest(http.MethodGet,
		"/v0/auth/github/manifest-callback?code=abc&state="+state, nil)
	req.AddCookie(&http.Cookie{Name: manifestStateCookieName, Value: state})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q", ct)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}
	if stub.calls != 1 {
		t.Errorf("conversion endpoint called %d times, want 1", stub.calls)
	}

	// State cookie cleared on success.
	var cleared bool
	for _, c := range w.Result().Cookies() {
		if c.Name == manifestStateCookieName && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("state cookie not cleared on success")
	}

	// Page should render the App identity, env-var template, PEM,
	// and OAuth callback URL the operator should put in .env.
	page := w.Body.String()
	for _, want := range []string{
		"123456",
		"Fishhawk (local)",
		"https://github.com/apps/fishhawk-local",
		"FISHHAWKD_GITHUB_APP_ID=123456",
		"FISHHAWKD_OAUTH_CLIENT_ID=Iv1.abc",
		"FISHHAWKD_OAUTH_CLIENT_SECRET=s3cret",
		"FISHHAWKD_GITHUB_WEBHOOK_SECRET=hookhook",
		"-----BEGIN RSA PRIVATE KEY-----",
		"FISHHAWKD_OAUTH_CALLBACK_URL=http://example.com/v0/auth/github/callback",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page missing %q", want)
		}
	}
}

func TestManifestCallback_StateMismatch_400(t *testing.T) {
	s, stub := newManifestServer(t)
	req := httptest.NewRequest(http.MethodGet,
		"/v0/auth/github/manifest-callback?code=abc&state=fromBrowser", nil)
	req.AddCookie(&http.Cookie{Name: manifestStateCookieName, Value: "different"})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if stub.calls != 0 {
		t.Errorf("conversion endpoint called %d times on state mismatch", stub.calls)
	}
	// State cookie still cleared even on mismatch (single-use).
	var cleared bool
	for _, c := range w.Result().Cookies() {
		if c.Name == manifestStateCookieName && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("state cookie not cleared on mismatch")
	}
}

func TestManifestCallback_StateCookieMissing_400(t *testing.T) {
	s, stub := newManifestServer(t)
	req := httptest.NewRequest(http.MethodGet,
		"/v0/auth/github/manifest-callback?code=abc&state=fromBrowser", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if stub.calls != 0 {
		t.Errorf("conversion endpoint called %d times when state cookie missing", stub.calls)
	}
}

func TestManifestCallback_MissingCode_400(t *testing.T) {
	s, stub := newManifestServer(t)
	state := "state-xyz"
	req := httptest.NewRequest(http.MethodGet,
		"/v0/auth/github/manifest-callback?state="+state, nil)
	req.AddCookie(&http.Cookie{Name: manifestStateCookieName, Value: state})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if stub.calls != 0 {
		t.Errorf("conversion endpoint called %d times without code", stub.calls)
	}
}

func TestManifestCallback_ConversionFails_502(t *testing.T) {
	s, stub := newManifestServer(t)
	stub.respCode = http.StatusGone
	stub.respBody = `{"message":"code expired"}`
	state := "state-xyz"
	req := httptest.NewRequest(http.MethodGet,
		"/v0/auth/github/manifest-callback?code=stale&state="+state, nil)
	req.AddCookie(&http.Cookie{Name: manifestStateCookieName, Value: state})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "manifest_conversion_failed") {
		t.Errorf("body missing error code:\n%s", w.Body.String())
	}
}

func TestManifestCallback_Unconfigured_503(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"}) // no GitHubManifest
	req := httptest.NewRequest(http.MethodGet,
		"/v0/auth/github/manifest-callback?code=x&state=y", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestDeriveOAuthCallbackURL(t *testing.T) {
	cases := []struct {
		name string
		req  *http.Request
		want string
	}{
		{
			name: "plain http",
			req:  httptest.NewRequest(http.MethodGet, "http://localhost:8080/whatever", nil),
			want: "http://localhost:8080/v0/auth/github/callback",
		},
		{
			name: "x-forwarded-proto wins",
			req: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "http://api.example.com/whatever", nil)
				r.Header.Set("X-Forwarded-Proto", "https")
				return r
			}(),
			want: "https://api.example.com/v0/auth/github/callback",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := deriveOAuthCallbackURL(tc.req)
			if got != tc.want {
				t.Errorf("got = %q, want %q", got, tc.want)
			}
		})
	}
}
