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

// fakeGitHub stands in for github.com / api.github.com. Tests
// configure responses via fields and assert on captured input.
type fakeGitHub struct {
	tokenResp  string // JSON for the /access_token endpoint
	tokenCode  int
	userResp   string
	userCode   int
	gotCode    string
	gotToken   string
	gotRedir   string
	gotClient  string
	gotSecret  string
	tokenCalls int
	userCalls  int
}

func newFakeGitHub(t *testing.T) (*fakeGitHub, OAuthURLs) {
	t.Helper()
	fg := &fakeGitHub{tokenCode: 200, userCode: 200}
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
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	urls := OAuthURLs{
		AuthorizeURL: srv.URL + "/login/oauth/authorize",
		TokenURL:     srv.URL + "/login/oauth/access_token",
		UserURL:      srv.URL + "/user",
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
	if q.Get("scope") != "read:user user:email" {
		t.Errorf("scope = %q", q.Get("scope"))
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
