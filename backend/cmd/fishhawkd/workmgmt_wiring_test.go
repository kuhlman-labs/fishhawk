package main

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
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

	got, err := adapter.SearchOpenIssues(context.Background(), forge.FromGitHubInstallationID(99), githubclient.RepoRef{Owner: "o", Name: "r"}, "repo:o/r is:open")
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

// graphqlCall is one recorded GraphQL round trip: the operation text the
// client put on the wire and the variables it bound. The three
// board-placement client methods all POST to /graphql, so the operation +
// variables are what distinguish them — which is exactly what a
// wrong-but-signature-compatible delegation would get wrong.
type graphqlCall struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

// newGraphQLFake returns a real *githubclient.Client pointed at an httptest
// server that records every /graphql request and replies with the given
// canned data envelope. The returned pointer is read only after the call
// under test has returned, so no synchronization is needed.
func newGraphQLFake(t *testing.T, dataEnvelope string) (*githubclient.Client, *[]graphqlCall) {
	t.Helper()
	calls := &[]graphqlCall{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		var got graphqlCall
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode graphql request: %v", err)
		}
		*calls = append(*calls, got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, dataEnvelope)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  stubTokenProvider{},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
	}, calls
}

// TestFeedbackAPIAdapter_BoardPlacementDelegations closes the one seam the
// board-placement tests (#1737) otherwise leave un-exercised: every other
// placement test drives a fake FeedbackAPI, so nothing reaches the
// production adapter -> real *githubclient.Client edge these three methods
// exist for.
//
// The residual bug class is a delegation to a WRONG-BUT-SIGNATURE-COMPATIBLE
// client call — a transposed argument on the four-string
// SetProjectItemSingleSelect, a swapped project/content pair on
// AddProjectItem, a hard-coded field name on ProjectFields. All three
// compile clean and only surface on a real boarding attempt. So each
// sub-test binds a DISTINCT sentinel per argument and asserts the operation
// + variables observed on the wire, which no transposition can satisfy.
func TestFeedbackAPIAdapter_BoardPlacementDelegations(t *testing.T) {
	scope := forge.FromGitHubInstallationID(99)

	t.Run("ProjectFields", func(t *testing.T) {
		c, calls := newGraphQLFake(t,
			`{"data":{"user":{"projectV2":{"id":"PVT_proj","field":{"id":"FLD_status","options":[{"id":"OPT_backlog","name":"Backlog"}]}}}}}`)
		adapter := feedbackAPIAdapter{c}

		meta, err := adapter.ProjectFields(context.Background(), scope,
			githubclient.ProjectCoord{Owner: "kuhlman-labs", OwnerType: "user", Number: 7}, "Priority")
		if err != nil {
			t.Fatalf("ProjectFields: %v", err)
		}
		if len(*calls) != 1 {
			t.Fatalf("graphql calls = %d, want 1", len(*calls))
		}
		got := (*calls)[0]
		if !strings.Contains(got.Query, "query ProjectFields") {
			t.Errorf("operation = %q, want the ProjectFields query", got.Query)
		}
		// "Priority", not a hard-coded "Status": the field name must be the
		// one the caller passed.
		wantVars := map[string]any{"login": "kuhlman-labs", "number": float64(7), "field": "Priority"}
		if !maps.Equal(got.Variables, wantVars) {
			t.Errorf("variables = %v, want %v", got.Variables, wantVars)
		}
		want := githubclient.ProjectMeta{
			ProjectID:     "PVT_proj",
			FieldID:       "FLD_status",
			StatusOptions: map[string]string{"Backlog": "OPT_backlog"},
		}
		if meta == nil || meta.ProjectID != want.ProjectID || meta.FieldID != want.FieldID ||
			!maps.Equal(meta.StatusOptions, want.StatusOptions) {
			t.Errorf("meta = %+v, want %+v", meta, want)
		}
	})

	t.Run("AddProjectItem", func(t *testing.T) {
		c, calls := newGraphQLFake(t,
			`{"data":{"addProjectV2ItemById":{"item":{"id":"ITEM_created"}}}}`)
		adapter := feedbackAPIAdapter{c}

		itemID, err := adapter.AddProjectItem(context.Background(), scope, "PVT_proj", "ISSUE_content")
		if err != nil {
			t.Fatalf("AddProjectItem: %v", err)
		}
		if itemID != "ITEM_created" {
			t.Errorf("item id = %q, want %q", itemID, "ITEM_created")
		}
		if len(*calls) != 1 {
			t.Fatalf("graphql calls = %d, want 1", len(*calls))
		}
		got := (*calls)[0]
		if !strings.Contains(got.Query, "addProjectV2ItemById") {
			t.Errorf("operation = %q, want the addProjectV2ItemById mutation", got.Query)
		}
		// Distinct sentinels: a swapped (projectID, contentID) pair fails here.
		wantVars := map[string]any{"projectId": "PVT_proj", "contentId": "ISSUE_content"}
		if !maps.Equal(got.Variables, wantVars) {
			t.Errorf("variables = %v, want %v", got.Variables, wantVars)
		}
	})

	t.Run("SetProjectItemSingleSelect", func(t *testing.T) {
		c, calls := newGraphQLFake(t,
			`{"data":{"updateProjectV2ItemFieldValue":{"projectV2Item":{"id":"ITEM_created"}}}}`)
		adapter := feedbackAPIAdapter{c}

		if err := adapter.SetProjectItemSingleSelect(context.Background(), scope,
			"PVT_proj", "ITEM_created", "FLD_status", "OPT_backlog"); err != nil {
			t.Fatalf("SetProjectItemSingleSelect: %v", err)
		}
		if len(*calls) != 1 {
			t.Fatalf("graphql calls = %d, want 1", len(*calls))
		}
		got := (*calls)[0]
		if !strings.Contains(got.Query, "updateProjectV2ItemFieldValue") {
			t.Errorf("operation = %q, want the updateProjectV2ItemFieldValue mutation", got.Query)
		}
		// Four distinct sentinels: any transposition among the four string
		// arguments lands a value under the wrong key and fails here.
		wantVars := map[string]any{
			"projectId": "PVT_proj",
			"itemId":    "ITEM_created",
			"fieldId":   "FLD_status",
			"optionId":  "OPT_backlog",
		}
		if !maps.Equal(got.Variables, wantVars) {
			t.Errorf("variables = %v, want %v", got.Variables, wantVars)
		}
	})
}
