package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/gitlabclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/jiraclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
	workmgmtgithub "github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt/github"
	workmgmtgitlab "github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt/gitlab"
	workmgmtjira "github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt/jira"
)

// stubTokenProvider is a fixed-token githubapp.TokenProvider for wiring the
// adapter test's real *githubclient.Client to an httptest server.
type stubTokenProvider struct{}

func (stubTokenProvider) Token(context.Context, int64) (string, error) { return "ghs_test", nil }

// newSearchFake2 returns a real *githubclient.Client pointed at an httptest
// server that serves the given /search/issues body. It's the production
// search path the adapter mapping is asserted against.
func newSearchFake2(t *testing.T, body string) *githubclient.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /search/issues", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  stubTokenProvider{},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
	}
}

// var _ pins the feedbackAPIAdapter to the FeedbackAPI contract at compile
// time: a signature drift fails the build (the seam the production 501 bug
// lacked any guard for).
var _ workmgmtgithub.FeedbackAPI = feedbackAPIAdapter{}

// TestRegisterWorkmgmtProviders_RegistersGitHubProjects is the
// cross-boundary seam the per-layer + fake-provider tests missed: it drives
// the real startup wiring against a real *githubclient.Client and asserts
// BOTH registries carry github_projects, directly pinning the registration
// production lacked (#1104).
func TestRegisterWorkmgmtProviders_RegistersGitHubProjects(t *testing.T) {
	registerWorkmgmtProviders(&githubclient.Client{}, nil, nil)

	if !slices.Contains(workmgmt.Registered(), workmgmtgithub.ProviderName) {
		t.Errorf("work-item registry = %v, want it to contain %q", workmgmt.Registered(), workmgmtgithub.ProviderName)
	}
	if !slices.Contains(workmgmt.RegisteredFeedback(), workmgmtgithub.FeedbackProviderName) {
		t.Errorf("feedback registry = %v, want it to contain %q", workmgmt.RegisteredFeedback(), workmgmtgithub.FeedbackProviderName)
	}
}

// TestRegisterWorkmgmtProviders_RegistersJira asserts a configured Jira
// client registers the jira work-item provider, independently of GitHub:
// passing a nil GitHub client must still register jira (#1094).
func TestRegisterWorkmgmtProviders_RegistersJira(t *testing.T) {
	registerWorkmgmtProviders(nil, jiraclient.New("https://acme.atlassian.net", "e@x.com", "tok"), nil)

	if !slices.Contains(workmgmt.Registered(), workmgmtjira.ProviderName) {
		t.Errorf("work-item registry = %v, want it to contain %q", workmgmt.Registered(), workmgmtjira.ProviderName)
	}
}

// TestRegisterWorkmgmtProviders_RegistersGitLab asserts a configured GitLab
// client registers the gitlab work-item provider, independently of GitHub
// and Jira: passing nil for both must still register gitlab (ADR-058 #1856).
func TestRegisterWorkmgmtProviders_RegistersGitLab(t *testing.T) {
	registerWorkmgmtProviders(nil, nil, gitlabclient.New("https://gitlab.com", "glpat-tok"))

	if !slices.Contains(workmgmt.Registered(), workmgmtgitlab.ProviderName) {
		t.Errorf("work-item registry = %v, want it to contain %q", workmgmt.Registered(), workmgmtgitlab.ProviderName)
	}
}

// TestRegisterWorkmgmtProviders_NilClientNoOp asserts unconfigured GitHub,
// Jira, and GitLab clients leave both registries unchanged (no panic, no
// registration) — the v0 not-yet-wired posture where the endpoints keep
// returning 501. Snapshot-equality is order-independent: a sibling test may
// already have registered github_projects, so we assert the nil call adds
// nothing rather than asserting emptiness.
func TestRegisterWorkmgmtProviders_NilClientNoOp(t *testing.T) {
	beforeWork := slices.Clone(workmgmt.Registered())
	beforeFeedback := slices.Clone(workmgmt.RegisteredFeedback())

	registerWorkmgmtProviders(nil, nil, nil)

	if got := workmgmt.Registered(); !slices.Equal(got, beforeWork) {
		t.Errorf("work-item registry changed on nil client: before=%v after=%v", beforeWork, got)
	}
	if got := workmgmt.RegisteredFeedback(); !slices.Equal(got, beforeFeedback) {
		t.Errorf("feedback registry changed on nil client: before=%v after=%v", beforeFeedback, got)
	}
}

// TestFeedbackAPIAdapter_SearchOpenIssuesMapsFields drives the adapter
// against a stub githubclient surface and asserts SearchOpenIssues copies
// each result field into the right MatchedIssue field
// (number->Number, html_url->URL, body->Body). A wrong/missing copy fails
// here rather than only surfacing in post-merge operator acceptance.
func TestFeedbackAPIAdapter_SearchOpenIssuesMapsFields(t *testing.T) {
	c := newSearchFake2(t,
		`{"items":[{"number":7,"html_url":"https://github.com/o/r/issues/7","body":"marker-body"}]}`)
	adapter := feedbackAPIAdapter{c}

	got, err := adapter.SearchOpenIssuesScoped(context.Background(), forge.FromGitHubInstallationID(99), githubclient.RepoRef{Owner: "o", Name: "r"}, "repo:o/r is:open")
	if err != nil {
		t.Fatalf("SearchOpenIssues: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("matches = %d, want 1", len(got))
	}
	want := workmgmtgithub.MatchedIssue{Number: 7, URL: "https://github.com/o/r/issues/7", Body: "marker-body"}
	if got[0] != want {
		t.Errorf("mapped = %+v, want %+v", got[0], want)
	}
}
