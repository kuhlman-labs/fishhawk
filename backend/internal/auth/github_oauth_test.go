package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// fakeGitHub stands in for github.com / api.github.com. Tests
// configure responses via fields and assert on captured input.
type fakeGitHub struct {
	tokenResp    string // JSON for the /access_token endpoint
	tokenCode    int
	userResp     string
	userCode     int
	orgsResp     string
	orgsCode     int
	gotCode      string
	gotToken     string
	gotOrgsToken string
	gotOrgsQuery string
	gotRedir     string
	gotClient    string
	gotSecret    string
	tokenCalls   int
	userCalls    int
	orgsCalls    int
}

func newFakeGitHub(t *testing.T) (*fakeGitHub, OAuthURLs) {
	t.Helper()
	fg := &fakeGitHub{tokenCode: 200, userCode: 200, orgsCode: 200, orgsResp: `[]`}
	mux := http.NewServeMux()
	mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		fg.tokenCalls++
		_ = r.ParseForm()
		fg.gotCode = r.PostForm.Get("code")
		fg.gotRedir = r.PostForm.Get("redirect_uri")
		fg.gotClient = r.PostForm.Get("client_id")
		fg.gotSecret = r.PostForm.Get("client_secret")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fg.tokenCode)
		_, _ = w.Write([]byte(fg.tokenResp))
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		fg.userCalls++
		fg.gotToken = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fg.userCode)
		_, _ = w.Write([]byte(fg.userResp))
	})
	mux.HandleFunc("/user/orgs", func(w http.ResponseWriter, r *http.Request) {
		fg.orgsCalls++
		fg.gotOrgsToken = r.Header.Get("Authorization")
		fg.gotOrgsQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fg.orgsCode)
		_, _ = w.Write([]byte(fg.orgsResp))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	urls := OAuthURLs{
		AuthorizeURL: srv.URL + "/login/oauth/authorize",
		TokenURL:     srv.URL + "/login/oauth/access_token",
		UserURL:      srv.URL + "/user",
		OrgsURL:      srv.URL + "/user/orgs",
	}
	return fg, urls
}

func TestAuthorizeURL_BuildsExpectedShape(t *testing.T) {
	o := NewGitHubOAuth("client-abc", "secret-xyz", "https://example.com/callback", OAuthURLs{})
	got := o.AuthorizeURL("state-1")
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("client_id") != "client-abc" {
		t.Errorf("client_id = %q", q.Get("client_id"))
	}
	if q.Get("state") != "state-1" {
		t.Errorf("state = %q", q.Get("state"))
	}
	if q.Get("redirect_uri") != "https://example.com/callback" {
		t.Errorf("redirect_uri = %q", q.Get("redirect_uri"))
	}
	if q.Get("scope") != "read:user user:email read:org" {
		t.Errorf("scope = %q (read:org is required so the auto-join /user/orgs read sees private org memberships)", q.Get("scope"))
	}
	if q.Get("allow_signup") != "false" {
		t.Errorf("allow_signup = %q (want 'false' to keep org-only installs from prompting account creation)", q.Get("allow_signup"))
	}
}

func TestExchangeCode_HappyPath(t *testing.T) {
	fg, urls := newFakeGitHub(t)
	fg.tokenResp = `{"access_token":"gho_xxx"}`
	o := NewGitHubOAuth("cid", "csec", "https://example.com/cb", urls)

	tok, err := o.ExchangeCode(context.Background(), "code-1")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if tok != "gho_xxx" {
		t.Errorf("token = %q", tok)
	}
	if fg.gotCode != "code-1" {
		t.Errorf("code = %q", fg.gotCode)
	}
	if fg.gotClient != "cid" || fg.gotSecret != "csec" {
		t.Errorf("client/secret not forwarded: %q / %q", fg.gotClient, fg.gotSecret)
	}
	if fg.gotRedir != "https://example.com/cb" {
		t.Errorf("redirect_uri = %q", fg.gotRedir)
	}
}

func TestExchangeCode_OAuthErrorResponse(t *testing.T) {
	fg, urls := newFakeGitHub(t)
	fg.tokenResp = `{"error":"bad_verification_code","error_description":"code was wrong"}`
	o := NewGitHubOAuth("cid", "csec", "x", urls)

	_, err := o.ExchangeCode(context.Background(), "code")
	if err == nil || !strings.Contains(err.Error(), "bad_verification_code") {
		t.Errorf("err = %v, want bad_verification_code", err)
	}
}

func TestExchangeCode_HTTPNon200(t *testing.T) {
	fg, urls := newFakeGitHub(t)
	fg.tokenCode = 500
	o := NewGitHubOAuth("c", "s", "x", urls)
	_, err := o.ExchangeCode(context.Background(), "code")
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("err = %v, want 500", err)
	}
}

func TestExchangeCode_EmptyToken(t *testing.T) {
	fg, urls := newFakeGitHub(t)
	fg.tokenResp = `{}` // no access_token field
	o := NewGitHubOAuth("c", "s", "x", urls)
	_, err := o.ExchangeCode(context.Background(), "code")
	if err == nil || !strings.Contains(err.Error(), "access_token") {
		t.Errorf("err = %v", err)
	}
}

func TestExchangeCode_RejectsEmptyCode(t *testing.T) {
	o := NewGitHubOAuth("c", "s", "x", OAuthURLs{})
	_, err := o.ExchangeCode(context.Background(), "")
	if err == nil {
		t.Error("expected error on empty code")
	}
}

func TestFetchProfile_HappyPath(t *testing.T) {
	fg, urls := newFakeGitHub(t)
	body, _ := json.Marshal(map[string]any{
		"id":    int64(123),
		"login": "octocat",
		"name":  "The Octo Cat",
		"email": "octo@example.com",
	})
	fg.userResp = string(body)
	o := NewGitHubOAuth("c", "s", "x", urls)

	p, err := o.FetchProfile(context.Background(), "gho_xxx")
	if err != nil {
		t.Fatalf("FetchProfile: %v", err)
	}
	if p.ID != 123 || p.Login != "octocat" || p.Name != "The Octo Cat" {
		t.Errorf("profile = %+v", p)
	}
	if p.Email == nil || *p.Email != "octo@example.com" {
		t.Errorf("email = %v", p.Email)
	}
	if fg.gotToken != "Bearer gho_xxx" {
		t.Errorf("auth header = %q", fg.gotToken)
	}
}

// TestFetchProfile_EMULogin pins EMU handling (E44.2 / #1826): an Enterprise
// Managed User profile login carries a "<username>_<shortcode>" enterprise
// short-code suffix. FetchProfile must parse it without error and preserve the
// FULL login (short code included) on the profile — never stripped or split.
func TestFetchProfile_EMULogin(t *testing.T) {
	fg, urls := newFakeGitHub(t)
	fg.userResp = `{"id":42,"login":"alice_acme","name":"Alice"}`
	o := NewGitHubOAuth("c", "s", "x", urls)

	p, err := o.FetchProfile(context.Background(), "gho_emu")
	if err != nil {
		t.Fatalf("FetchProfile with an EMU login: %v", err)
	}
	if p.Login != "alice_acme" {
		t.Errorf("Login = %q, want alice_acme (full EMU login preserved)", p.Login)
	}
}

func TestFetchProfile_FallsBackToLoginWhenNameEmpty(t *testing.T) {
	fg, urls := newFakeGitHub(t)
	fg.userResp = `{"id":1,"login":"octocat","name":""}`
	o := NewGitHubOAuth("c", "s", "x", urls)
	p, err := o.FetchProfile(context.Background(), "tok")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "octocat" {
		t.Errorf("Name = %q, want fallback to login", p.Name)
	}
}

func TestFetchProfile_RejectsMissingID(t *testing.T) {
	fg, urls := newFakeGitHub(t)
	fg.userResp = `{"login":"octocat"}`
	o := NewGitHubOAuth("c", "s", "x", urls)
	_, err := o.FetchProfile(context.Background(), "tok")
	if err == nil {
		t.Error("expected error when id is missing")
	}
}

func TestFetchProfile_HTTPError(t *testing.T) {
	fg, urls := newFakeGitHub(t)
	fg.userCode = 401
	o := NewGitHubOAuth("c", "s", "x", urls)
	_, err := o.FetchProfile(context.Background(), "tok")
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("err = %v", err)
	}
}

func TestFetchProfile_RejectsEmptyToken(t *testing.T) {
	o := NewGitHubOAuth("c", "s", "x", OAuthURLs{})
	_, err := o.FetchProfile(context.Background(), "")
	if err == nil {
		t.Error("expected error on empty token")
	}
}

func TestListUserOrgKeys_HappyPath(t *testing.T) {
	fg, urls := newFakeGitHub(t)
	fg.orgsResp = `[{"login":"acme-corp","id":1},{"login":"other-org","id":2}]`
	o := NewGitHubOAuth("c", "s", "x", urls)

	keys, err := o.ListUserOrgKeys(context.Background(), "gho_xxx")
	if err != nil {
		t.Fatalf("ListUserOrgKeys: %v", err)
	}
	if len(keys) != 2 || keys[0] != "acme-corp" || keys[1] != "other-org" {
		t.Errorf("keys = %v", keys)
	}
	// The USER's OAuth token, never an App token.
	if fg.gotOrgsToken != "Bearer gho_xxx" {
		t.Errorf("auth header = %q", fg.gotOrgsToken)
	}
	if !strings.Contains(fg.gotOrgsQuery, "per_page=100") {
		t.Errorf("query = %q, want per_page=100", fg.gotOrgsQuery)
	}
}

func TestListUserOrgKeys_HTTPError(t *testing.T) {
	fg, urls := newFakeGitHub(t)
	fg.orgsCode = 403 // e.g. token missing read:org
	o := NewGitHubOAuth("c", "s", "x", urls)
	_, err := o.ListUserOrgKeys(context.Background(), "tok")
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("err = %v, want 403", err)
	}
}

func TestListUserOrgKeys_RejectsEmptyToken(t *testing.T) {
	o := NewGitHubOAuth("c", "s", "x", OAuthURLs{})
	_, err := o.ListUserOrgKeys(context.Background(), "")
	if err == nil {
		t.Error("expected error on empty token")
	}
}

func TestListUserOrgKeys_EmptyList(t *testing.T) {
	fg, urls := newFakeGitHub(t)
	fg.orgsResp = `[]`
	o := NewGitHubOAuth("c", "s", "x", urls)
	keys, err := o.ListUserOrgKeys(context.Background(), "tok")
	if err != nil {
		t.Fatalf("ListUserOrgKeys: %v", err)
	}
	if len(keys) != 0 {
		t.Errorf("keys = %v, want empty", keys)
	}
}

// TestListUserOrgKeys_RejectsOversizedBody mirrors
// TestGitLabMembershipLister_OversizedBody_FailsClosed: an oversized
// orgs body is REJECTED rather than truncated-and-parsed. The body is
// valid JSON (whitespace padding inside an empty array) padded past
// gitHubMaxOrgsBytes, so a truncate-and-parse implementation would
// happily decode it — only a byte-cap check catches this.
func TestListUserOrgKeys_RejectsOversizedBody(t *testing.T) {
	fg, urls := newFakeGitHub(t)
	fg.orgsResp = "[" + strings.Repeat(" ", gitHubMaxOrgsBytes+1) + "]"
	o := NewGitHubOAuth("c", "s", "x", urls)

	keys, err := o.ListUserOrgKeys(context.Background(), "tok")
	if err == nil {
		t.Fatalf("ListUserOrgKeys = %v, want an error on an oversized body", keys)
	}
	if keys != nil {
		t.Errorf("keys = %v on the oversized-body path, want nil (fail closed)", keys)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("%d bytes", gitHubMaxOrgsBytes)) {
		t.Errorf("error = %v, want it to name the byte cap", err)
	}
}

// erroringBody is an io.ReadCloser whose Read always fails, simulating
// a connection drop mid-body so ListUserOrgKeys's io.ReadAll surfaces
// an error distinct from the oversized-body case above.
type erroringBody struct{}

func (erroringBody) Read([]byte) (int, error) { return 0, errors.New("simulated read failure") }
func (erroringBody) Close() error             { return nil }

type erroringBodyTransport struct{}

func (erroringBodyTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       erroringBody{},
	}, nil
}

// TestListUserOrgKeys_ReadError covers the io.ReadAll error path,
// distinct from the oversized-body rejection: a body that fails to
// read (e.g. a dropped connection) must surface as an error, not a
// decode of a truncated partial body.
func TestListUserOrgKeys_ReadError(t *testing.T) {
	_, urls := newFakeGitHub(t)
	o := NewGitHubOAuth("c", "s", "x", urls)
	o.http = &http.Client{Transport: erroringBodyTransport{}}

	keys, err := o.ListUserOrgKeys(context.Background(), "tok")
	if err == nil {
		t.Fatalf("ListUserOrgKeys = %v, want an error on a body read failure", keys)
	}
	if keys != nil {
		t.Errorf("keys = %v on the read-error path, want nil (fail closed)", keys)
	}
	if !strings.Contains(err.Error(), "read orgs") {
		t.Errorf("error = %v, want it to identify the orgs read", err)
	}
}

// TestWebOAuthFlow_DeploymentDefaultHostOnly is the DEFECT-2 sequencing guard
// (E44.16 / #2094, binding conditions 1 + 2): every web sign-in operation runs
// at or before identification, so each MUST target the single deployment-default
// host and NEVER a per-installation host. AuthorizeURL, ExchangeCode,
// FetchProfile, AND ListUserOrgKeys (the org-discovery read that establishes the
// installation) are all asserted here. The structural proof that no
// per-installation resolver is consulted is that GitHubOAuth carries no
// ResolveBaseURL field at all — there is nothing for these methods to route
// through — and every HTTP call lands on the one httptest host below.
func TestWebOAuthFlow_DeploymentDefaultHostOnly(t *testing.T) {
	seenHosts := map[string]string{} // path -> Host
	mux := http.NewServeMux()
	record := func(path string, body string) {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			seenHosts[path] = r.Host
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		})
	}
	record("/login/oauth/access_token", `{"access_token":"gho_default"}`)
	record("/user", `{"id":7,"login":"octocat","name":"Octo"}`)
	record("/user/orgs", `[{"login":"acme"}]`)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	defaultHost := strings.TrimPrefix(srv.URL, "http://")

	urls := OAuthURLs{
		AuthorizeURL: srv.URL + "/login/oauth/authorize",
		TokenURL:     srv.URL + "/login/oauth/access_token",
		UserURL:      srv.URL + "/user",
		OrgsURL:      srv.URL + "/user/orgs",
	}
	o := NewGitHubOAuth("cid", "csec", "https://example.com/cb", urls)

	// AuthorizeURL: the browser-redirect host is the deployment default.
	authURL, err := url.Parse(o.AuthorizeURL("state-1"))
	if err != nil {
		t.Fatalf("parse authorize url: %v", err)
	}
	if authURL.Host != defaultHost {
		t.Errorf("AuthorizeURL host = %q, want deployment default %q (no per-installation routing pre-identification)", authURL.Host, defaultHost)
	}

	// ExchangeCode: still anonymous — must hit the default token host.
	tok, err := o.ExchangeCode(context.Background(), "code-1")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if tok != "gho_default" {
		t.Fatalf("ExchangeCode token = %q, want gho_default", tok)
	}

	// FetchProfile: the initial identifying read — default host.
	if _, err := o.FetchProfile(context.Background(), tok); err != nil {
		t.Fatalf("FetchProfile: %v", err)
	}

	// ListUserOrgKeys: the org-discovery read that establishes the
	// installation — still default host (binding condition 2).
	keys, err := o.ListUserOrgKeys(context.Background(), tok)
	if err != nil {
		t.Fatalf("ListUserOrgKeys: %v", err)
	}
	if len(keys) != 1 || keys[0] != "acme" {
		t.Fatalf("ListUserOrgKeys = %v, want [acme]", keys)
	}

	// Every HTTP operation landed on the ONE deployment-default host.
	for _, path := range []string{"/login/oauth/access_token", "/user", "/user/orgs"} {
		got, ok := seenHosts[path]
		if !ok {
			t.Errorf("%s was never requested; the operation did not target the deployment-default host", path)
			continue
		}
		if got != defaultHost {
			t.Errorf("%s targeted host %q, want deployment default %q", path, got, defaultHost)
		}
	}
}
