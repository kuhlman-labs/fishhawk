package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandleHealth(t *testing.T) {
	s := New(Config{})
	rec := httptest.NewRecorder()
	s.handleHealth(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("content-type = %q, want application/json", got)
	}

	var body healthResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("status field = %q, want ok", body.Status)
	}
	if body.Version == "" {
		t.Error("version field must not be empty")
	}
	if body.GitSHA == "" {
		t.Error("git_sha field must not be empty")
	}
	if body.MinRunnerVersion == "" {
		t.Error("min_runner_version field must not be empty")
	}
	if len(body.Schemas) == 0 {
		t.Error("schemas field must not be empty")
	}
	if body.Schemas["plan-standard-v1"] == "" {
		t.Error("schemas[plan-standard-v1] must not be empty")
	}
	if body.Schemas["workflow-v0"] == "" {
		t.Error("schemas[workflow-v0] must not be empty")
	}
	if body.Schemas["workflow-v1"] == "" {
		t.Error("schemas[workflow-v1] must not be empty")
	}

	// Wire-level omission pin (#1018): with no StartNonce configured, the
	// RAW body must not carry the key at all (omitempty), so a pre-nonce
	// scripts/dev — or its degraded rc=2 path — never sees a bogus field.
	if raw := rec.Body.String(); strings.Contains(raw, "start_nonce") {
		t.Errorf("raw body contains start_nonce despite empty Config.StartNonce: %s", raw)
	}
}

// TestHandleHealth_StartNonce pins the exact compact JSON byte shape
// scripts/dev's _nonce_from_healthz_body greps for (#1018). Asserting
// on the raw body (not a struct round-trip) means a JSON-tag rename or
// a switch to indented output breaks this test before it breaks the
// zsh parser on the other side of the seam.
func TestHandleHealth_StartNonce(t *testing.T) {
	s := New(Config{StartNonce: "test-nonce-123"})
	rec := httptest.NewRecorder()
	s.handleHealth(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	raw := rec.Body.String()
	if want := `"start_nonce":"test-nonce-123"`; !strings.Contains(raw, want) {
		t.Errorf("raw body missing %s: %s", want, raw)
	}
}

// TestCacheEfficiencyRouteRegistered guards the route table: GET
// /v0/runs/{run_id}/cache-efficiency (#1352) must reach
// handleGetRunCacheEfficiency. With no RunRepo configured the handler
// returns 503 — an UNregistered route would instead 404 with a default
// not-found body, so a 503 here proves the route is wired in handlers.go.
func TestCacheEfficiencyRouteRegistered(t *testing.T) {
	s := New(Config{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v0/runs/"+"00000000-0000-0000-0000-000000000000"+"/cache-efficiency", nil)
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (route reaches handler with no RunRepo)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "run_repo_unconfigured") {
		t.Errorf("body = %s, want run_repo_unconfigured (handleGetRunCacheEfficiency reached)", rec.Body.String())
	}
}

// TestCostRouteRegistered guards the route table: GET /v0/runs/{run_id}/cost
// (#1372) must reach handleGetRunCost. With no RunRepo configured the handler
// returns 503 — an UNregistered route would instead 404 with a default
// not-found body, so a 503 here proves the route is wired in handlers.go.
func TestCostRouteRegistered(t *testing.T) {
	s := New(Config{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v0/runs/"+"00000000-0000-0000-0000-000000000000"+"/cost", nil)
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (route reaches handler with no RunRepo)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "run_repo_unconfigured") {
		t.Errorf("body = %s, want run_repo_unconfigured (handleGetRunCost reached)", rec.Body.String())
	}
}

// TestResumeCampaignRouteRegistered guards the route table: POST
// /v0/campaigns/{campaign_id}/resume (#1446) must reach handleResumeCampaign.
// With no CampaignRepo configured the handler returns 503 — an UNregistered
// route would instead 404 with a default not-found body, so a 503 here proves
// the route is wired in handlers.go. (handleResumeCampaign checks the
// nil-CampaignRepo guard BEFORE the write-scope check precisely so this idiom
// reaches the handler.)
func TestResumeCampaignRouteRegistered(t *testing.T) {
	s := New(Config{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v0/campaigns/"+"00000000-0000-0000-0000-000000000000"+"/resume", nil)
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (route reaches handler with no CampaignRepo)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "campaign_repo_unconfigured") {
		t.Errorf("body = %s, want campaign_repo_unconfigured (handleResumeCampaign reached)", rec.Body.String())
	}
}
