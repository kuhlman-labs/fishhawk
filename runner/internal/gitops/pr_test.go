package gitops

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// stubGitHubAPI mounts a /repos/{o}/{r}/pulls handler with a
// configurable response.
type stubAPI struct {
	respCode  int
	respBody  string
	gotMethod string
	gotPath   string
	gotAuth   string
	gotBody   map[string]any
}

func newStubAPI(t *testing.T) (*stubAPI, *httptest.Server) {
	t.Helper()
	stub := &stubAPI{respCode: http.StatusCreated}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		stub.gotMethod = r.Method
		stub.gotPath = r.URL.Path
		stub.gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &stub.gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(stub.respCode)
		_, _ = io.WriteString(w, stub.respBody)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return stub, srv
}

func TestOpenPR_HappyPath(t *testing.T) {
	stub, srv := newStubAPI(t)
	stub.respBody = `{"number":42,"html_url":"https://github.com/owner/repo/pull/42"}`
	c := &OpenPRClient{
		HTTP:    &http.Client{Timeout: time.Second},
		APIBase: srv.URL,
		Token:   "ghs_xyz",
	}

	got, err := c.OpenPR(context.Background(), OpenPRArgs{
		Owner: "owner", Repo: "repo",
		Head: "fishhawk/branch", Base: "main",
		Title: "Add a thing", Body: "context",
	})
	if err != nil {
		t.Fatalf("OpenPR: %v", err)
	}
	if got.PRNumber != 42 || got.PRURL == "" {
		t.Errorf("got = %+v", got)
	}
	if stub.gotMethod != http.MethodPost {
		t.Errorf("method = %q", stub.gotMethod)
	}
	if stub.gotPath != "/repos/owner/repo/pulls" {
		t.Errorf("path = %q", stub.gotPath)
	}
	if stub.gotAuth != "Bearer ghs_xyz" {
		t.Errorf("Authorization = %q", stub.gotAuth)
	}
	for _, want := range []string{"title", "head", "base"} {
		if _, ok := stub.gotBody[want]; !ok {
			t.Errorf("body missing %q: %+v", want, stub.gotBody)
		}
	}
}

func TestOpenPR_GitHubError(t *testing.T) {
	stub, srv := newStubAPI(t)
	stub.respCode = http.StatusUnprocessableEntity
	stub.respBody = `{"message":"A pull request already exists for fishhawk/branch.","status":"422"}`
	c := &OpenPRClient{APIBase: srv.URL, Token: "ghs_xyz"}

	_, err := c.OpenPR(context.Background(), OpenPRArgs{
		Owner: "owner", Repo: "repo",
		Head: "fishhawk/branch", Base: "main",
		Title: "Add a thing",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "422") {
		t.Errorf("err = %v, want 422 in message", err)
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("err = %v, want GitHub message in error", err)
	}
}

func TestOpenPR_RejectsBadInputs(t *testing.T) {
	c := &OpenPRClient{Token: "x"}
	cases := map[string]OpenPRArgs{
		"missing owner": {Repo: "r", Head: "h", Base: "b", Title: "t"},
		"missing repo":  {Owner: "o", Head: "h", Base: "b", Title: "t"},
		"missing head":  {Owner: "o", Repo: "r", Base: "b", Title: "t"},
		"missing base":  {Owner: "o", Repo: "r", Head: "h", Title: "t"},
		"missing title": {Owner: "o", Repo: "r", Head: "h", Base: "b"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := c.OpenPR(context.Background(), args)
			if err == nil {
				t.Error("expected error")
			}
		})
	}

	c2 := &OpenPRClient{}
	_, err := c2.OpenPR(context.Background(), OpenPRArgs{
		Owner: "o", Repo: "r", Head: "h", Base: "b", Title: "t",
	})
	if err == nil {
		t.Error("expected token-required error")
	}
}

func TestOpenPR_RejectsMalformedSuccess(t *testing.T) {
	stub, srv := newStubAPI(t)
	stub.respBody = `{"number":0,"html_url":""}` // 201 but missing fields
	c := &OpenPRClient{APIBase: srv.URL, Token: "x"}

	_, err := c.OpenPR(context.Background(), OpenPRArgs{
		Owner: "o", Repo: "r", Head: "h", Base: "b", Title: "t",
	})
	if err == nil {
		t.Fatal("expected error on missing fields")
	}
}
