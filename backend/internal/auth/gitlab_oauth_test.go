package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// fakeGitLab stands in for a GitLab instance's /oauth/token +
// /api/v4/user endpoints. Tests configure responses via fields and
// assert on captured input.
type fakeGitLab struct {
	tokenResp  string // JSON for /oauth/token
	tokenCode  int
	userResp   string
	userCode   int
	gotCode    string
	gotGrant   string
	gotClient  string
	gotSecret  string
	gotRedir   string
	gotToken   string
	tokenCalls int
	userCalls  int
}

func newFakeGitLab(t *testing.T) (*fakeGitLab, string) {
	t.Helper()
	fg := &fakeGitLab{tokenCode: 200, userCode: 200}
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		fg.tokenCalls++
		_ = r.ParseForm()
		fg.gotCode = r.PostForm.Get("code")
		fg.gotGrant = r.PostForm.Get("grant_type")
		fg.gotClient = r.PostForm.Get("client_id")
		fg.gotSecret = r.PostForm.Get("client_secret")
		fg.gotRedir = r.PostForm.Get("redirect_uri")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fg.tokenCode)
		_, _ = w.Write([]byte(fg.tokenResp))
	})
	mux.HandleFunc("/api/v4/user", func(w http.ResponseWriter, r *http.Request) {
		fg.userCalls++
		fg.gotToken = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fg.userCode)
		_, _ = w.Write([]byte(fg.userResp))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return fg, srv.URL
}

// TestGitLabAuthorizeURL_BuildsExpectedShape pins the authorize redirect,
// including the DEFINITIVE scope decision (binding condition 2): read_api,
// because the flow reads BOTH /api/v4/user and /api/v4/groups.
func TestGitLabAuthorizeURL_BuildsExpectedShape(t *testing.T) {
	o := NewGitLabOAuth("https://gitlab.example.com", "client-abc", "secret-xyz",
		"https://example.com/callback", GitLabOAuthURLs{})
	got := o.AuthorizeURL("state-1")
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme+"://"+u.Host+u.Path != "https://gitlab.example.com/oauth/authorize" {
		t.Errorf("authorize endpoint = %q, want https://gitlab.example.com/oauth/authorize", u.Scheme+"://"+u.Host+u.Path)
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
	if q.Get("response_type") != "code" {
		t.Errorf("response_type = %q, want code", q.Get("response_type"))
	}
	if q.Get("scope") != "read_api" {
		t.Errorf("scope = %q, want read_api (authorizes BOTH GET /api/v4/user and GET /api/v4/groups; read_user does not grant the groups read the auto-join gate needs)", q.Get("scope"))
	}
}

func TestGitLabExchangeCode_HappyPath(t *testing.T) {
	fg, base := newFakeGitLab(t)
	fg.tokenResp = `{"access_token":"glpat_xxx"}`
	o := NewGitLabOAuth(base, "cid", "csec", "https://example.com/cb", GitLabOAuthURLs{})

	tok, err := o.ExchangeCode(context.Background(), "code-1")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if tok != "glpat_xxx" {
		t.Errorf("token = %q", tok)
	}
	if fg.gotCode != "code-1" {
		t.Errorf("code = %q", fg.gotCode)
	}
	if fg.gotGrant != "authorization_code" {
		t.Errorf("grant_type = %q, want authorization_code", fg.gotGrant)
	}
	if fg.gotClient != "cid" || fg.gotSecret != "csec" {
		t.Errorf("client/secret not forwarded: %q / %q", fg.gotClient, fg.gotSecret)
	}
	if fg.gotRedir != "https://example.com/cb" {
		t.Errorf("redirect_uri = %q", fg.gotRedir)
	}
}

func TestGitLabExchangeCode_OAuthErrorResponse(t *testing.T) {
	fg, base := newFakeGitLab(t)
	fg.tokenResp = `{"error":"invalid_grant","error_description":"code was wrong"}`
	o := NewGitLabOAuth(base, "cid", "csec", "x", GitLabOAuthURLs{})

	_, err := o.ExchangeCode(context.Background(), "code")
	if err == nil || !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("err = %v, want invalid_grant", err)
	}
}

func TestGitLabExchangeCode_HTTPNon200(t *testing.T) {
	fg, base := newFakeGitLab(t)
	fg.tokenCode = 500
	o := NewGitLabOAuth(base, "c", "s", "x", GitLabOAuthURLs{})
	_, err := o.ExchangeCode(context.Background(), "code")
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("err = %v, want 500", err)
	}
}

func TestGitLabExchangeCode_EmptyToken(t *testing.T) {
	fg, base := newFakeGitLab(t)
	fg.tokenResp = `{}` // no access_token field
	o := NewGitLabOAuth(base, "c", "s", "x", GitLabOAuthURLs{})
	_, err := o.ExchangeCode(context.Background(), "code")
	if err == nil || !strings.Contains(err.Error(), "access_token") {
		t.Errorf("err = %v", err)
	}
}

func TestGitLabExchangeCode_RejectsEmptyCode(t *testing.T) {
	o := NewGitLabOAuth("https://gitlab.example.com", "c", "s", "x", GitLabOAuthURLs{})
	_, err := o.ExchangeCode(context.Background(), "")
	if err == nil {
		t.Error("expected error on empty code")
	}
}

func TestGitLabExchangeCode_TransportError(t *testing.T) {
	// A server that is already closed: the token POST fails at the transport
	// layer (connection refused), exercising the http.Do error branch.
	srv := httptest.NewServer(http.NewServeMux())
	base := srv.URL
	srv.Close()
	o := NewGitLabOAuth(base, "c", "s", "x", GitLabOAuthURLs{})
	_, err := o.ExchangeCode(context.Background(), "code")
	if err == nil || !strings.Contains(err.Error(), "exchange gitlab code") {
		t.Errorf("err = %v, want a transport error", err)
	}
}

func TestGitLabExchangeCode_UndecodableBody(t *testing.T) {
	fg, base := newFakeGitLab(t)
	fg.tokenResp = `{not json` // 200 but garbage
	o := NewGitLabOAuth(base, "c", "s", "x", GitLabOAuthURLs{})
	_, err := o.ExchangeCode(context.Background(), "code")
	if err == nil || !strings.Contains(err.Error(), "decode gitlab token response") {
		t.Errorf("err = %v, want a decode error", err)
	}
}

func TestGitLabFetchProfile_TransportError(t *testing.T) {
	srv := httptest.NewServer(http.NewServeMux())
	base := srv.URL
	srv.Close()
	o := NewGitLabOAuth(base, "c", "s", "x", GitLabOAuthURLs{})
	_, err := o.FetchProfile(context.Background(), "tok")
	if err == nil || !strings.Contains(err.Error(), "fetch gitlab user") {
		t.Errorf("err = %v, want a transport error", err)
	}
}

func TestGitLabFetchProfile_UndecodableBody(t *testing.T) {
	fg, base := newFakeGitLab(t)
	fg.userResp = `{not json`
	o := NewGitLabOAuth(base, "c", "s", "x", GitLabOAuthURLs{})
	_, err := o.FetchProfile(context.Background(), "tok")
	if err == nil || !strings.Contains(err.Error(), "decode gitlab user") {
		t.Errorf("err = %v, want a decode error", err)
	}
}

func TestGitLabFetchProfile_HappyPath(t *testing.T) {
	fg, base := newFakeGitLab(t)
	body, _ := json.Marshal(map[string]any{
		"id":       int64(123),
		"username": "octo",
		"name":     "The Octo Cat",
		"email":    "octo@example.com",
	})
	fg.userResp = string(body)
	o := NewGitLabOAuth(base, "c", "s", "x", GitLabOAuthURLs{})

	p, err := o.FetchProfile(context.Background(), "glpat_xxx")
	if err != nil {
		t.Fatalf("FetchProfile: %v", err)
	}
	// Login is the GitLab username (never the display name).
	if p.ID != 123 || p.Login != "octo" || p.Name != "The Octo Cat" {
		t.Errorf("profile = %+v", p)
	}
	if p.Email == nil || *p.Email != "octo@example.com" {
		t.Errorf("email = %v", p.Email)
	}
	if fg.gotToken != "Bearer glpat_xxx" {
		t.Errorf("auth header = %q", fg.gotToken)
	}
}

func TestGitLabFetchProfile_FallsBackToUsernameWhenNameEmpty(t *testing.T) {
	fg, base := newFakeGitLab(t)
	fg.userResp = `{"id":1,"username":"octo","name":""}`
	o := NewGitLabOAuth(base, "c", "s", "x", GitLabOAuthURLs{})
	p, err := o.FetchProfile(context.Background(), "tok")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "octo" {
		t.Errorf("Name = %q, want fallback to username", p.Name)
	}
}

func TestGitLabFetchProfile_RejectsMissingID(t *testing.T) {
	fg, base := newFakeGitLab(t)
	fg.userResp = `{"username":"octo"}`
	o := NewGitLabOAuth(base, "c", "s", "x", GitLabOAuthURLs{})
	_, err := o.FetchProfile(context.Background(), "tok")
	if err == nil {
		t.Error("expected error when id is missing")
	}
}

func TestGitLabFetchProfile_HTTPError(t *testing.T) {
	fg, base := newFakeGitLab(t)
	fg.userCode = 401
	o := NewGitLabOAuth(base, "c", "s", "x", GitLabOAuthURLs{})
	_, err := o.FetchProfile(context.Background(), "tok")
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("err = %v", err)
	}
}

func TestGitLabFetchProfile_RejectsEmptyToken(t *testing.T) {
	o := NewGitLabOAuth("https://gitlab.example.com", "c", "s", "x", GitLabOAuthURLs{})
	_, err := o.FetchProfile(context.Background(), "")
	if err == nil {
		t.Error("expected error on empty token")
	}
}

// TestGitLabOAuth_DeploymentDefaultHostOnly mirrors the GitHub DEFECT-2
// sequencing guard: every web sign-in operation targets the single
// deployment-default GitLab host and never a per-installation host. The
// structural proof that no per-installation resolver is consulted is that
// GitLabOAuth carries no ResolveBaseURL field at all.
func TestGitLabOAuth_DeploymentDefaultHostOnly(t *testing.T) {
	seenHosts := map[string]string{}
	mux := http.NewServeMux()
	record := func(path, body string) {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			seenHosts[path] = r.Host
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		})
	}
	record("/oauth/token", `{"access_token":"glpat_default"}`)
	record("/api/v4/user", `{"id":7,"username":"octo","name":"Octo"}`)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	defaultHost := strings.TrimPrefix(srv.URL, "http://")
	o := NewGitLabOAuth(srv.URL, "cid", "csec", "https://example.com/cb", GitLabOAuthURLs{})

	authURL, err := url.Parse(o.AuthorizeURL("state-1"))
	if err != nil {
		t.Fatalf("parse authorize url: %v", err)
	}
	if authURL.Host != defaultHost {
		t.Errorf("AuthorizeURL host = %q, want deployment default %q", authURL.Host, defaultHost)
	}
	tok, err := o.ExchangeCode(context.Background(), "code-1")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if _, err := o.FetchProfile(context.Background(), tok); err != nil {
		t.Fatalf("FetchProfile: %v", err)
	}
	for _, path := range []string{"/oauth/token", "/api/v4/user"} {
		got, ok := seenHosts[path]
		if !ok {
			t.Errorf("%s was never requested", path)
			continue
		}
		if got != defaultHost {
			t.Errorf("%s targeted host %q, want deployment default %q", path, got, defaultHost)
		}
	}
}
