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

	"github.com/kuhlman-labs/fishhawk/backend/internal/securityscan"
)

// codeScanningServer stands up a one-off httptest server for the
// code-scanning alerts endpoint. It serves pages[i] for the i-th
// request, advertising a rel="next" Link header for every page but the
// last so the client's shared pagination loop walks them in order. It
// records the first request's full URL (path + query) so assertions can
// verify the ref + state query params, and counts how many pages it
// served. A non-2xx status is returned for every page (single-page
// error cases pass a one-element pages slice).
func codeScanningServer(t *testing.T, status int, pages []string) (*Client, *string, *int) {
	t.Helper()
	var firstURL string
	served := 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/code-scanning/alerts",
		func(w http.ResponseWriter, r *http.Request) {
			if served == 0 {
				firstURL = r.URL.String()
			}
			idx := served
			if idx >= len(pages) {
				idx = len(pages) - 1
			}
			served++
			w.Header().Set("Content-Type", "application/json")
			if idx < len(pages)-1 {
				// Point at the next page on the same server. GitHub returns
				// absolute next URLs and the client follows them verbatim,
				// so build an absolute URL from the request host.
				next := fmt.Sprintf("http://%s%s?page=%d", r.Host, r.URL.Path, idx+2)
				w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next"`, next))
			}
			w.WriteHeader(status)
			_, _ = io.WriteString(w, pages[idx])
		})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := &Client{
		BaseURL: srv.URL,
		Tokens:  &stubTokens{token: "ghs_canned_token"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		AppJWT:  func() (string, error) { return "ghs_app_jwt", nil },
	}
	return c, &firstURL, &served
}

func TestListCodeScanningAlerts_HappyPath_DecodesAndQuery(t *testing.T) {
	body := `[
		{"rule":{"id":"go/sql-injection","security_severity_level":"high"},
		 "most_recent_instance":{"location":{"path":"backend/a.go","start_line":12}}},
		{"rule":{"id":"go/weak-crypto","security_severity_level":"critical"},
		 "most_recent_instance":{"location":{"path":"backend/b.go","start_line":7}}},
		{"rule":{"id":"go/style","security_severity_level":"medium"},
		 "most_recent_instance":{"location":{"path":"backend/c.go","start_line":3}}},
		{"rule":{"id":"go/info","security_severity_level":"low"},
		 "most_recent_instance":{"location":{"path":"backend/d.go","start_line":1}}},
		{"rule":{"id":"go/non-security"},
		 "most_recent_instance":{"location":{"path":"backend/e.go","start_line":99}}}
	]`
	c, firstURL, _ := codeScanningServer(t, http.StatusOK, []string{body})

	got, err := c.ListCodeScanningAlerts(context.Background(), 42,
		RepoRef{Owner: "x", Name: "y"}, "deadbeef")
	if err != nil {
		t.Fatalf("ListCodeScanningAlerts: %v", err)
	}

	// (a) request carries ref=<sha> and state=open.
	if !strings.Contains(*firstURL, "ref=deadbeef") {
		t.Errorf("request URL %q missing ref=deadbeef", *firstURL)
	}
	if !strings.Contains(*firstURL, "state=open") {
		t.Errorf("request URL %q missing state=open", *firstURL)
	}

	want := []securityscan.Finding{
		{RuleID: "go/sql-injection", Severity: "high", Path: "backend/a.go", StartLine: 12},
		{RuleID: "go/weak-crypto", Severity: "critical", Path: "backend/b.go", StartLine: 7},
		{RuleID: "go/style", Severity: "medium", Path: "backend/c.go", StartLine: 3},
		{RuleID: "go/info", Severity: "low", Path: "backend/d.go", StartLine: 1},
		// (b) absent security_severity_level decodes to "".
		{RuleID: "go/non-security", Severity: "", Path: "backend/e.go", StartLine: 99},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d findings, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		// (b) severity mapping + (c) location extraction.
		if got[i] != want[i] {
			t.Errorf("finding[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestListCodeScanningAlerts_Paginates(t *testing.T) {
	pages := []string{
		`[{"rule":{"id":"r1","security_severity_level":"high"},
		   "most_recent_instance":{"location":{"path":"a.go","start_line":1}}}]`,
		`[{"rule":{"id":"r2","security_severity_level":"critical"},
		   "most_recent_instance":{"location":{"path":"b.go","start_line":2}}}]`,
		`[{"rule":{"id":"r3","security_severity_level":"medium"},
		   "most_recent_instance":{"location":{"path":"c.go","start_line":3}}}]`,
	}
	c, _, served := codeScanningServer(t, http.StatusOK, pages)

	got, err := c.ListCodeScanningAlerts(context.Background(), 42,
		RepoRef{Owner: "x", Name: "y"}, "main")
	if err != nil {
		t.Fatalf("ListCodeScanningAlerts: %v", err)
	}
	if *served != 3 {
		t.Errorf("server served %d pages, want 3", *served)
	}
	// (d) multi-page accumulation in order.
	wantRules := []string{"r1", "r2", "r3"}
	if len(got) != len(wantRules) {
		t.Fatalf("got %d findings across pages, want %d: %+v", len(got), len(wantRules), got)
	}
	for i, rule := range wantRules {
		if got[i].RuleID != rule {
			t.Errorf("finding[%d].RuleID = %q, want %q (order not preserved)", i, got[i].RuleID, rule)
		}
	}
}

func TestListCodeScanningAlerts_NotFound(t *testing.T) {
	c, _, _ := codeScanningServer(t, http.StatusNotFound, []string{`{"message":"Not Found"}`})
	_, err := c.ListCodeScanningAlerts(context.Background(), 42,
		RepoRef{Owner: "x", Name: "y"}, "main")
	if err == nil || !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestListCodeScanningAlerts_Validation(t *testing.T) {
	c, _, _ := codeScanningServer(t, http.StatusUnprocessableEntity, []string{`{"message":"bad ref"}`})
	_, err := c.ListCodeScanningAlerts(context.Background(), 42,
		RepoRef{Owner: "x", Name: "y"}, "main")
	if err == nil || !errors.Is(err, ErrValidation) {
		t.Errorf("err = %v, want ErrValidation", err)
	}
}

func TestListCodeScanningAlerts_InputGuards(t *testing.T) {
	cases := []struct {
		name      string
		client    *Client
		repo      RepoRef
		ref       string
		wantSubst string
	}{
		{"missing tokens", &Client{}, RepoRef{Owner: "x", Name: "y"}, "main", "TokenProvider"},
		{"missing owner", &Client{Tokens: &stubTokens{}}, RepoRef{Name: "y"}, "main", "owner and name"},
		{"missing name", &Client{Tokens: &stubTokens{}}, RepoRef{Owner: "x"}, "main", "owner and name"},
		{"missing ref", &Client{Tokens: &stubTokens{}}, RepoRef{Owner: "x", Name: "y"}, "", "ref is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.client.ListCodeScanningAlerts(context.Background(), 1, tc.repo, tc.ref)
			if err == nil || !strings.Contains(err.Error(), tc.wantSubst) {
				t.Errorf("err = %v, want substring %q", err, tc.wantSubst)
			}
		})
	}
}
