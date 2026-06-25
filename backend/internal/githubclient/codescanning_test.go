package githubclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// codeScanningClient wires a Client to an httptest server serving the
// code-scanning alerts endpoint, recording the last request URL so
// assertions can verify the state/ref/per_page query params.
func codeScanningClient(t *testing.T, status int, body string) (*Client, *string) {
	t.Helper()
	var lastURL string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/code-scanning/alerts",
		func(w http.ResponseWriter, r *http.Request) {
			lastURL = r.URL.String()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
		})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := &Client{
		BaseURL: srv.URL,
		Tokens:  &stubTokens{token: "ghs_canned_token"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
	}
	return c, &lastURL
}

func TestListCodeScanningAlerts_HappyPath(t *testing.T) {
	body := `[
	  {
	    "number": 7,
	    "state": "open",
	    "html_url": "https://github.com/x/y/security/code-scanning/7",
	    "rule": {"id": "go/sql-injection", "name": "SQL injection",
	             "description": "User input flows to a SQL query",
	             "security_severity_level": "high"},
	    "tool": {"name": "CodeQL"},
	    "most_recent_instance": {
	      "ref": "refs/pull/12/merge",
	      "commit_sha": "deadbeef",
	      "location": {"path": "backend/db.go", "start_line": 42}
	    }
	  }
	]`
	c, lastURL := codeScanningClient(t, http.StatusOK, body)

	got, err := c.ListCodeScanningAlerts(context.Background(), 99,
		RepoRef{Owner: "x", Name: "y"}, "refs/pull/12/merge")
	if err != nil {
		t.Fatalf("ListCodeScanningAlerts: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1 (%v)", len(got), got)
	}
	f := got[0]
	want := struct {
		num     int
		ruleID  string
		desc    string
		sev     string
		state   string
		path    string
		line    int
		commit  string
		ref     string
		tool    string
		htmlURL string
	}{7, "go/sql-injection", "User input flows to a SQL query", "high", "open",
		"backend/db.go", 42, "deadbeef", "refs/pull/12/merge", "CodeQL",
		"https://github.com/x/y/security/code-scanning/7"}
	if f.Number != want.num || f.RuleID != want.ruleID || f.Description != want.desc ||
		f.Severity != want.sev || f.State != want.state || f.Path != want.path ||
		f.StartLine != want.line || f.CommitSHA != want.commit || f.Ref != want.ref ||
		f.Tool != want.tool || f.HTMLURL != want.htmlURL {
		t.Errorf("finding = %+v, want %+v", f, want)
	}

	// state=open, per_page, and the ref must be on the wire.
	for _, sub := range []string{"state=open", "per_page=100", "ref=refs%2Fpull%2F12%2Fmerge"} {
		if !strings.Contains(*lastURL, sub) {
			t.Errorf("request URL %q missing %q", *lastURL, sub)
		}
	}
}

// TestListCodeScanningAlerts_DescriptionFallback verifies the rule.name
// fallback when rule.description is empty.
func TestListCodeScanningAlerts_DescriptionFallback(t *testing.T) {
	body := `[{"number":1,"state":"open","rule":{"name":"Hardcoded credential",
	  "security_severity_level":"critical"},"most_recent_instance":{}}]`
	c, _ := codeScanningClient(t, http.StatusOK, body)
	got, err := c.ListCodeScanningAlerts(context.Background(), 1,
		RepoRef{Owner: "x", Name: "y"}, "")
	if err != nil {
		t.Fatalf("ListCodeScanningAlerts: %v", err)
	}
	if len(got) != 1 || got[0].Description != "Hardcoded credential" {
		t.Errorf("description = %q, want rule.name fallback", got[0].Description)
	}
}

// TestListCodeScanningAlerts_NoRef omits the ref param so the default
// branch is read.
func TestListCodeScanningAlerts_NoRef(t *testing.T) {
	c, lastURL := codeScanningClient(t, http.StatusOK, `[]`)
	if _, err := c.ListCodeScanningAlerts(context.Background(), 1,
		RepoRef{Owner: "x", Name: "y"}, ""); err != nil {
		t.Fatalf("ListCodeScanningAlerts: %v", err)
	}
	if strings.Contains(*lastURL, "ref=") {
		t.Errorf("request URL %q should not carry a ref param", *lastURL)
	}
}

func TestListCodeScanningAlerts_EmptyList(t *testing.T) {
	c, _ := codeScanningClient(t, http.StatusOK, `[]`)
	got, err := c.ListCodeScanningAlerts(context.Background(), 1,
		RepoRef{Owner: "x", Name: "y"}, "main")
	if err != nil {
		t.Fatalf("ListCodeScanningAlerts: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}

// TestListCodeScanningAlerts_Pagination walks two pages via the rel="next"
// Link header.
func TestListCodeScanningAlerts_Pagination(t *testing.T) {
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("GET /repos/{owner}/{repo}/code-scanning/alerts",
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Query().Get("page") == "2" {
				_, _ = io.WriteString(w, `[{"number":2,"state":"open","rule":{"security_severity_level":"low"},"most_recent_instance":{"location":{"path":"b.go"}}}]`)
				return
			}
			// Page 1 advertises page 2 via Link.
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/x/y/code-scanning/alerts?page=2>; rel="next"`, srv.URL))
			_, _ = io.WriteString(w, `[{"number":1,"state":"open","rule":{"security_severity_level":"high"},"most_recent_instance":{"location":{"path":"a.go"}}}]`)
		})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := &Client{BaseURL: srv.URL, Tokens: &stubTokens{token: "t"}, HTTP: &http.Client{Timeout: 5 * time.Second}}

	got, err := c.ListCodeScanningAlerts(context.Background(), 1,
		RepoRef{Owner: "x", Name: "y"}, "main")
	if err != nil {
		t.Fatalf("ListCodeScanningAlerts: %v", err)
	}
	if len(got) != 2 || got[0].Number != 1 || got[1].Number != 2 {
		t.Errorf("got %v, want two findings across pages", got)
	}
}

func TestListCodeScanningAlerts_NotFound(t *testing.T) {
	// 404 — repo invisible OR code scanning not enabled. Caller tolerates.
	c, _ := codeScanningClient(t, http.StatusNotFound, `{"message":"Not Found"}`)
	_, err := c.ListCodeScanningAlerts(context.Background(), 1,
		RepoRef{Owner: "x", Name: "y"}, "main")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestListCodeScanningAlerts_Forbidden(t *testing.T) {
	c, _ := codeScanningClient(t, http.StatusForbidden, `{"message":"Resource not accessible"}`)
	_, err := c.ListCodeScanningAlerts(context.Background(), 1,
		RepoRef{Owner: "x", Name: "y"}, "main")
	if !errors.Is(err, ErrForbidden) {
		t.Errorf("err = %v, want ErrForbidden", err)
	}
}

func TestListCodeScanningAlerts_ServerError(t *testing.T) {
	c, _ := codeScanningClient(t, http.StatusInternalServerError, `boom`)
	_, err := c.ListCodeScanningAlerts(context.Background(), 1,
		RepoRef{Owner: "x", Name: "y"}, "main")
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("err = %v, want a 500 error", err)
	}
}

func TestListCodeScanningAlerts_DecodeError(t *testing.T) {
	c, _ := codeScanningClient(t, http.StatusOK, `not json`)
	_, err := c.ListCodeScanningAlerts(context.Background(), 1,
		RepoRef{Owner: "x", Name: "y"}, "main")
	if err == nil || !strings.Contains(err.Error(), "decode") {
		t.Errorf("err = %v, want a decode error", err)
	}
}

func TestListCodeScanningAlerts_ValidationErrors(t *testing.T) {
	cases := []struct {
		name      string
		client    *Client
		repo      RepoRef
		wantSubst string
	}{
		{"missing tokens", &Client{}, RepoRef{Owner: "x", Name: "y"}, "TokenProvider"},
		{"missing owner", &Client{Tokens: &stubTokens{}}, RepoRef{Name: "y"}, "owner and name"},
		{"missing name", &Client{Tokens: &stubTokens{}}, RepoRef{Owner: "x"}, "owner and name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.client.ListCodeScanningAlerts(context.Background(), 1, tc.repo, "main")
			if err == nil || !strings.Contains(err.Error(), tc.wantSubst) {
				t.Errorf("err = %v, want substring %q", err, tc.wantSubst)
			}
		})
	}
}
