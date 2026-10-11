package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Dependency-source surfaces on the campaign tools (#4028). Each test serves
// the backend's JSON from an httptest server, drives the REAL tool handler
// through the REAL apiClient, then marshals the tool's structured output and
// reads it back as a generic map — so a field whose json tag is dropped (or
// renamed) on the client mirror fails here, which a test against the Go
// struct fields alone would not catch.

// campaignJSONBackend serves body for POST path with the given status.
func campaignJSONBackend(t *testing.T, path string, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != path {
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// outputMap marshals a tool's structured output and decodes it as a map.
func outputMap(t *testing.T, out any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal output: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	return m
}

// dig walks a decoded JSON value by map keys and slice indexes.
func dig(t *testing.T, v any, path ...any) any {
	t.Helper()
	for _, step := range path {
		switch k := step.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				t.Fatalf("at %q: not an object: %#v", k, v)
			}
			v = m[k]
		case int:
			s, ok := v.([]any)
			if !ok || k >= len(s) {
				t.Fatalf("at [%d]: not an array of that length: %#v", k, v)
			}
			v = s[k]
		}
	}
	return v
}

// TestStartCampaign_SurfacesLegacyDependencyWarnings is C17's start half: the
// create response's warnings[] and satisfied_dependencies[].source reach the
// fishhawk_start_campaign structured output.
func TestStartCampaign_SurfacesLegacyDependencyWarnings(t *testing.T) {
	srv := campaignJSONBackend(t, "/v0/campaigns", http.StatusCreated, `{
		"id":"0b6f0a43-7a3d-4a0e-8d40-2d4b3f1c9a11","repo":"kuhlman-labs/fishhawk","epic_ref":"","state":"pending",
		"pause_policy":"pause_campaign","created_at":"2026-10-11T00:00:00Z","updated_at":"2026-10-11T00:00:00Z",
		"satisfied_dependencies":[{"from":3,"to":51,"state":"closed","state_reason":"completed","source":"legacy_text"}],
		"warnings":[{"issue_ref":"issue:3","code":"legacy_text_dependency","targets":["issue:1","issue:51"],"message":"issue #3 depends on issue:1, issue:51 only through a legacy line"}]
	}`)
	r := newResolver(srv, nil)
	_, out, err := r.startCampaign(context.Background(), nil, StartCampaignInput{
		Repo: "kuhlman-labs/fishhawk", Items: []string{"issue:1", "issue:3"},
	})
	if err != nil {
		t.Fatalf("startCampaign: %v", err)
	}
	m := outputMap(t, out)
	if got := dig(t, m, "campaign", "warnings", 0, "issue_ref"); got != "issue:3" {
		t.Errorf("campaign.warnings[0].issue_ref = %v, want issue:3", got)
	}
	if got := dig(t, m, "campaign", "warnings", 0, "code"); got != "legacy_text_dependency" {
		t.Errorf("campaign.warnings[0].code = %v, want legacy_text_dependency", got)
	}
	if got := dig(t, m, "campaign", "warnings", 0, "targets", 1); got != "issue:51" {
		t.Errorf("campaign.warnings[0].targets[1] = %v, want issue:51", got)
	}
	if got, _ := dig(t, m, "campaign", "warnings", 0, "message").(string); !strings.Contains(got, "issue #3") {
		t.Errorf("campaign.warnings[0].message = %q, want it to name issue #3", got)
	}
	if got := dig(t, m, "campaign", "satisfied_dependencies", 0, "source"); got != "legacy_text" {
		t.Errorf("campaign.satisfied_dependencies[0].source = %v, want legacy_text", got)
	}
}

// TestPreviewCampaign_SurfacesEdgeSourcesAndWarnings is C17's preview half:
// edges[*].source, dangling[0].source, satisfied_dependencies[0].source and
// warnings[] reach the fishhawk_preview_campaign structured output.
func TestPreviewCampaign_SurfacesEdgeSourcesAndWarnings(t *testing.T) {
	srv := campaignJSONBackend(t, "/v0/campaigns/preview", http.StatusOK, `{
		"valid":false,"repo":"kuhlman-labs/fishhawk","item_count":3,
		"items":[{"issue_ref":"issue:1","depends_on":[],"position":0},{"issue_ref":"issue:2","depends_on":["issue:1"],"position":1},{"issue_ref":"issue:3","depends_on":["issue:1"],"position":2}],
		"edges":[{"from":"issue:2","to":"issue:1","source":"native"},{"from":"issue:3","to":"issue:1","source":"legacy_text"}],
		"warnings":[{"issue_ref":"issue:3","code":"legacy_text_dependency","targets":["issue:1","issue:99"],"message":"issue #3 relies on a legacy line"}],
		"satisfied_dependencies":[{"from":2,"to":50,"state":"closed","state_reason":"completed","source":"native"}],
		"dangling":[{"from":"issue:3","to":"issue:99","reason":"not_child","remedy":"add it","source":"legacy_text"}],
		"closure_candidates":["issue:99"],"message":"dangling"
	}`)
	r := newResolver(srv, nil)
	_, out, err := r.previewCampaign(context.Background(), nil, PreviewCampaignInput{
		Repo: "kuhlman-labs/fishhawk", Items: []string{"issue:1", "issue:2", "issue:3"},
	})
	if err != nil {
		t.Fatalf("previewCampaign: %v", err)
	}
	m := outputMap(t, out)
	if got := dig(t, m, "preview", "edges", 0, "source"); got != "native" {
		t.Errorf("preview.edges[0].source = %v, want native", got)
	}
	if got := dig(t, m, "preview", "edges", 1, "source"); got != "legacy_text" {
		t.Errorf("preview.edges[1].source = %v, want legacy_text", got)
	}
	if got := dig(t, m, "preview", "edges", 1, "from"); got != "issue:3" {
		t.Errorf("preview.edges[1].from = %v, want issue:3", got)
	}
	if got := dig(t, m, "preview", "dangling", 0, "source"); got != "legacy_text" {
		t.Errorf("preview.dangling[0].source = %v, want legacy_text", got)
	}
	if got := dig(t, m, "preview", "satisfied_dependencies", 0, "source"); got != "native" {
		t.Errorf("preview.satisfied_dependencies[0].source = %v, want native", got)
	}
	if got := dig(t, m, "preview", "warnings", 0, "issue_ref"); got != "issue:3" {
		t.Errorf("preview.warnings[0].issue_ref = %v, want issue:3", got)
	}
}

// TestCampaignTools_NativeDependencyReadFailed_SurfacesIssueAndReason pins
// that the 502 native_dependency_read_failed refusal reaches both tools'
// errors with its code, issue_ref and reason intact, through the generic
// apiError rendering (no tool-specific arm drops them).
func TestCampaignTools_NativeDependencyReadFailed_SurfacesIssueAndReason(t *testing.T) {
	refusal := `{"error":{"code":"native_dependency_read_failed","message":"could not read the native \"blocked by\" dependencies of issue #2 (forbidden): grant access","details":{"issue_ref":"issue:2","reason":"forbidden"}}}`
	check := func(t *testing.T, err error) {
		t.Helper()
		var ae *apiError
		if !errors.As(err, &ae) {
			t.Fatalf("err = %v (%T), want an *apiError", err, err)
		}
		if ae.Code != "native_dependency_read_failed" || ae.Details["issue_ref"] != "issue:2" || ae.Details["reason"] != "forbidden" {
			t.Errorf("apiError = %s %v, want native_dependency_read_failed issue:2 forbidden", ae.Code, ae.Details)
		}
		if !strings.Contains(err.Error(), "issue #2") || !strings.Contains(err.Error(), "native_dependency_read_failed") {
			t.Errorf("error text = %q, want the code and issue #2", err.Error())
		}
	}
	t.Run("start", func(t *testing.T) {
		srv := campaignJSONBackend(t, "/v0/campaigns", http.StatusBadGateway, refusal)
		_, _, err := newResolver(srv, nil).startCampaign(context.Background(), nil, StartCampaignInput{
			Repo: "kuhlman-labs/fishhawk", Items: []string{"issue:1", "issue:2"},
		})
		check(t, err)
	})
	t.Run("preview", func(t *testing.T) {
		srv := campaignJSONBackend(t, "/v0/campaigns/preview", http.StatusBadGateway, refusal)
		_, _, err := newResolver(srv, nil).previewCampaign(context.Background(), nil, PreviewCampaignInput{
			Repo: "kuhlman-labs/fishhawk", Items: []string{"issue:1", "issue:2"},
		})
		check(t, err)
	})
}
