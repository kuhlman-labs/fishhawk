package githubclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
)

// The merge-settings read reuses snapshotClient's scripted /graphql fake.

func TestAllowedMergeMethods_DecodesSettingsAndRequestShape(t *testing.T) {
	c, f, _ := snapshotClient(t)
	f.pages = []string{`{"data":{"repository":{"mergeCommitAllowed":false,"squashMergeAllowed":true,"rebaseMergeAllowed":true}}}`}
	got, err := c.AllowedMergeMethods(context.Background(), forge.FromGitHubInstallationID(55), RepoRef{Owner: "acme", Name: "widgets"})
	if err != nil {
		t.Fatalf("AllowedMergeMethods: %v", err)
	}
	want := forge.AllowedMergeMethods{Squash: true, Merge: false, Rebase: true}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if f.calls != 1 {
		t.Fatalf("calls = %d, want exactly one GraphQL query", f.calls)
	}
	if f.auth[0] != "Bearer ghs_run_install" {
		t.Errorf("Authorization = %q, want the run's installation token", f.auth[0])
	}
	for _, field := range []string{"mergeCommitAllowed", "squashMergeAllowed", "rebaseMergeAllowed", "repository(owner: $owner, name: $name)"} {
		if !strings.Contains(f.queries[0], field) {
			t.Errorf("query missing %q:\n%s", field, f.queries[0])
		}
	}
}

func TestAllowedMergeMethods_RequestVariables(t *testing.T) {
	var gotPath string
	var gotVars map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var req struct {
			Variables map[string]any `json:"variables"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &req)
		gotVars = req.Variables
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"repository":{"mergeCommitAllowed":true,"squashMergeAllowed":true,"rebaseMergeAllowed":false}}}`)
	}))
	t.Cleanup(srv.Close)
	c := &Client{BaseURL: srv.URL, Tokens: &stubTokens{token: "ghs_run_install"}, HTTP: &http.Client{Timeout: 5 * time.Second}}
	if _, err := c.AllowedMergeMethods(context.Background(), forge.FromGitHubInstallationID(55), RepoRef{Owner: "acme", Name: "widgets"}); err != nil {
		t.Fatalf("AllowedMergeMethods: %v", err)
	}
	if gotPath != "/graphql" {
		t.Errorf("path = %q, want /graphql", gotPath)
	}
	if gotVars["owner"] != "acme" || gotVars["name"] != "widgets" || len(gotVars) != 2 {
		t.Errorf("variables = %v, want exactly owner=acme name=widgets", gotVars)
	}
}

// TestAllowedMergeMethods_FailsClosed: one case per failure mode; each is an
// error with the zero value, never a guessed setting.
func TestAllowedMergeMethods_FailsClosed(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		page    string
		wantIs  error
		wantSub string
	}{
		{name: "non-2xx", status: http.StatusBadGateway, page: `{}`},
		{name: "forbidden", status: http.StatusForbidden, page: `{}`, wantIs: ErrForbidden},
		{name: "graphql errors envelope", status: http.StatusOK, page: `{"errors":[{"message":"boom"}]}`, wantIs: ErrValidation},
		{name: "null repository", status: http.StatusOK, page: `{"data":{"repository":null}}`, wantIs: ErrNotFound},
		{name: "missing squashMergeAllowed", status: http.StatusOK, page: `{"data":{"repository":{"mergeCommitAllowed":true,"rebaseMergeAllowed":true}}}`, wantSub: "absent"},
		{name: "null mergeCommitAllowed", status: http.StatusOK, page: `{"data":{"repository":{"mergeCommitAllowed":null,"squashMergeAllowed":true,"rebaseMergeAllowed":true}}}`, wantSub: "absent"},
		{name: "missing rebaseMergeAllowed", status: http.StatusOK, page: `{"data":{"repository":{"mergeCommitAllowed":true,"squashMergeAllowed":true}}}`, wantSub: "absent"},
		{name: "undecodable body", status: http.StatusOK, page: `not json`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, f, _ := snapshotClient(t)
			f.status = tc.status
			f.pages = []string{tc.page}
			got, err := c.AllowedMergeMethods(context.Background(), forge.FromGitHubInstallationID(55), RepoRef{Owner: "acme", Name: "widgets"})
			if err == nil {
				t.Fatalf("err = nil (got %+v), want a fail-closed error", got)
			}
			if got != (forge.AllowedMergeMethods{}) {
				t.Errorf("got %+v on error, want the zero value", got)
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Errorf("err = %v, want errors.Is %v", err, tc.wantIs)
			}
			if tc.wantSub != "" && !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("err = %v, want it to contain %q", err, tc.wantSub)
			}
		})
	}
}

func TestAllowedMergeMethods_TransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // a closed listener: the request fails in transport
	c := &Client{BaseURL: url, Tokens: &stubTokens{token: "ghs_run_install"}, HTTP: &http.Client{Timeout: 5 * time.Second}}
	if _, err := c.AllowedMergeMethods(context.Background(), forge.FromGitHubInstallationID(55), RepoRef{Owner: "acme", Name: "widgets"}); err == nil {
		t.Fatal("err = nil on a transport error")
	}
}

func TestAllowedMergeMethods_RefusesBeforeRequest(t *testing.T) {
	c, f, _ := snapshotClient(t)
	if _, err := c.AllowedMergeMethods(context.Background(), forge.CredentialScope{}, RepoRef{Owner: "a", Name: "b"}); err == nil {
		t.Error("zero scope: err = nil")
	}
	if _, err := c.AllowedMergeMethods(context.Background(), forge.FromGitHubInstallationID(55), RepoRef{Owner: "", Name: "b"}); err == nil {
		t.Error("blank owner: err = nil")
	}
	noTok := &Client{BaseURL: c.BaseURL, HTTP: c.HTTP}
	if _, err := noTok.AllowedMergeMethods(context.Background(), forge.FromGitHubInstallationID(55), RepoRef{Owner: "a", Name: "b"}); err == nil {
		t.Error("no TokenProvider: err = nil")
	}
	if f.calls != 0 {
		t.Errorf("server calls = %d, want 0 (refused before any request)", f.calls)
	}
}
