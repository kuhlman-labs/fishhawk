package githubclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// fakeAlert is the per-page JSON body for the code-scanning alerts endpoint.
const alertPage0 = `[
  {
    "number": 7,
    "state": "open",
    "html_url": "https://github.com/o/r/security/code-scanning/7",
    "rule": {
      "id": "go/allocation-size-overflow",
      "security_severity_level": "high",
      "description": "Multiplication result may overflow"
    },
    "most_recent_instance": {
      "location": { "path": "backend/internal/foo/foo.go", "start_line": 42 }
    }
  }
]`

const alertPage1 = `[
  {
    "number": 8,
    "state": "open",
    "html_url": "https://github.com/o/r/security/code-scanning/8",
    "rule": {
      "id": "go/sql-injection",
      "security_severity_level": "critical",
      "description": "SQL query built from user input"
    },
    "most_recent_instance": {
      "location": { "path": "backend/internal/bar/bar.go", "start_line": 9 }
    }
  }
]`

// TestListCodeScanningAlerts_PathMethodAndDecode asserts the request shape
// (method, path, ref + state=open query) and that the response decodes onto
// securityscan.Finding with security_severity_level mapped to Severity and the
// location mapped to Path/Line.
func TestListCodeScanningAlerts_PathMethodAndDecode(t *testing.T) {
	var gotMethod, gotPath, gotRef, gotState, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotRef = r.URL.Query().Get("ref")
		gotState = r.URL.Query().Get("state")
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, alertPage0)
	}))
	defer srv.Close()

	c := New(&stubTokens{token: "tok"})
	c.BaseURL = srv.URL

	findings, err := c.ListCodeScanningAlerts(context.Background(), 123, RepoRef{Owner: "o", Name: "r"}, "deadbeef")
	if err != nil {
		t.Fatalf("ListCodeScanningAlerts: %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", gotMethod)
	}
	if gotPath != "/repos/o/r/code-scanning/alerts" {
		t.Errorf("path = %q, want /repos/o/r/code-scanning/alerts", gotPath)
	}
	if gotRef != "deadbeef" {
		t.Errorf("ref query = %q, want deadbeef", gotRef)
	}
	if gotState != "open" {
		t.Errorf("state query = %q, want open (only unresolved alerts gate)", gotState)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("auth header = %q, want Bearer tok", gotAuth)
	}
	if len(findings) != 1 {
		t.Fatalf("got %d findings, want 1", len(findings))
	}
	f := findings[0]
	if f.Number != 7 || f.RuleID != "go/allocation-size-overflow" || f.Severity != "high" {
		t.Errorf("finding rule/severity mismatch: %+v", f)
	}
	if f.Path != "backend/internal/foo/foo.go" || f.Line != 42 {
		t.Errorf("finding location mismatch: %+v", f)
	}
	if f.State != "open" || !strings.HasSuffix(f.URL, "/7") {
		t.Errorf("finding state/url mismatch: %+v", f)
	}
	if f.Description != "Multiplication result may overflow" {
		t.Errorf("finding description mismatch: %+v", f)
	}
}

// TestListCodeScanningAlerts_Pagination asserts the read follows the rel="next"
// Link header to exhaustion and unions the pages.
func TestListCodeScanningAlerts_Pagination(t *testing.T) {
	pages := []string{alertPage0, alertPage1}
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idx := hits
		hits++
		w.Header().Set("Content-Type", "application/json")
		if idx+1 < len(pages) {
			next := "http://" + r.Host + r.URL.Path + "?page=" + strconv.Itoa(idx+2)
			w.Header().Set("Link", "<"+next+`>; rel="next"`)
		}
		_, _ = io.WriteString(w, pages[idx])
	}))
	defer srv.Close()

	c := New(&stubTokens{token: "tok"})
	c.BaseURL = srv.URL

	findings, err := c.ListCodeScanningAlerts(context.Background(), 1, RepoRef{Owner: "o", Name: "r"}, "main")
	if err != nil {
		t.Fatalf("ListCodeScanningAlerts: %v", err)
	}
	if hits != 2 {
		t.Errorf("server hit %d times, want 2 (paginated)", hits)
	}
	if len(findings) != 2 {
		t.Fatalf("got %d findings across pages, want 2", len(findings))
	}
	if findings[0].Number != 7 || findings[1].Number != 8 {
		t.Errorf("paginated union out of order: %+v", findings)
	}
	if findings[1].Severity != "critical" {
		t.Errorf("page-1 severity = %q, want critical", findings[1].Severity)
	}
}

// TestListCodeScanningAlerts_NotFound asserts a 404 (code scanning not enabled
// / no analysis for the ref) maps to ErrNotFound, so the merge gate can fail
// OPEN on an unreadable scan rather than treating it as zero findings.
func TestListCodeScanningAlerts_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"no analysis found"}`)
	}))
	defer srv.Close()

	c := New(&stubTokens{token: "tok"})
	c.BaseURL = srv.URL

	_, err := c.ListCodeScanningAlerts(context.Background(), 1, RepoRef{Owner: "o", Name: "r"}, "main")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestListCodeScanningAlerts_Validation asserts the input guards reject a
// missing ref and a missing repo before any HTTP call.
func TestListCodeScanningAlerts_Validation(t *testing.T) {
	c := New(&stubTokens{token: "tok"})
	if _, err := c.ListCodeScanningAlerts(context.Background(), 1, RepoRef{Owner: "o", Name: "r"}, ""); err == nil {
		t.Error("expected error on empty ref")
	}
	if _, err := c.ListCodeScanningAlerts(context.Background(), 1, RepoRef{}, "main"); err == nil {
		t.Error("expected error on empty repo")
	}
}

// TestListCodeScanningAlerts_NilTokens asserts the missing-TokenProvider guard
// returns an error before any HTTP call, matching every other Client method.
func TestListCodeScanningAlerts_NilTokens(t *testing.T) {
	c := &Client{} // no TokenProvider
	if _, err := c.ListCodeScanningAlerts(context.Background(), 1, RepoRef{Owner: "o", Name: "r"}, "main"); err == nil {
		t.Error("expected error when TokenProvider is nil")
	}
}

// TestListCodeScanningAlerts_DecodeError asserts a malformed 200 body surfaces
// a decode error rather than silently returning zero findings (which would
// open the gate on a corrupt read).
func TestListCodeScanningAlerts_DecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{not valid json`)
	}))
	defer srv.Close()

	c := New(&stubTokens{token: "tok"})
	c.BaseURL = srv.URL

	if _, err := c.ListCodeScanningAlerts(context.Background(), 1, RepoRef{Owner: "o", Name: "r"}, "main"); err == nil {
		t.Fatal("expected decode error on malformed body")
	}
}
