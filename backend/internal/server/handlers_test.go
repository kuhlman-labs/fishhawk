package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
	// workflow-v2 (ADR-067 / #2213) exercises the full cross-boundary
	// seam: schema file -> go:embed -> embeddedSchemas table ->
	// computeSchemaHashes -> EmbeddedSchemaHashV2() -> handleHealth JSON.
	// It must be present, non-empty, and distinct from the v0 and v1
	// hashes (a forgotten embed directive or mistyped map key would
	// otherwise pass every unit test).
	if body.Schemas["workflow-v2"] == "" {
		t.Error("schemas[workflow-v2] must not be empty")
	}
	if v2 := body.Schemas["workflow-v2"]; v2 == body.Schemas["workflow-v0"] || v2 == body.Schemas["workflow-v1"] {
		t.Errorf("schemas[workflow-v2] = %q must differ from the v0 and v1 hashes", v2)
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

// TestHandleHealth_ProcessStart pins the #2712 restart boundary on the wire:
// the exact compact JSON byte shape a client greps/decodes, in RFC3339Nano
// UTC, and its OMISSION when the boot marker is zero. The omission branch is
// load-bearing: a caller must be able to tell "no boundary published" (an
// older daemon) from a real instant, because a zero time compares as BEFORE
// every audit entry and would turn every pending review into a false strand.
func TestHandleHealth_ProcessStart(t *testing.T) {
	boot := time.Date(2026, 8, 15, 9, 30, 15, 123456789, time.UTC)
	s := New(Config{ProcessStart: boot})
	rec := httptest.NewRecorder()
	s.handleHealth(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	raw := rec.Body.String()
	want := `"process_start":"` + boot.Format(time.RFC3339Nano) + `"`
	if !strings.Contains(raw, want) {
		t.Errorf("raw body missing %s: %s", want, raw)
	}
	// It round-trips back to the same instant through the client-side parse.
	var body healthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got, err := time.Parse(time.RFC3339Nano, body.ProcessStart)
	if err != nil {
		t.Fatalf("process_start %q does not parse as RFC3339Nano: %v", body.ProcessStart, err)
	}
	if !got.Equal(boot) {
		t.Errorf("parsed process_start = %v, want %v", got, boot)
	}
	// start_nonce is untouched — process_start is an additive sibling.
	if strings.Contains(raw, `"start_nonce"`) {
		t.Errorf("start_nonce must stay omitted when unset: %s", raw)
	}

	// Zero marker: the key is omitted entirely. New() always stamps a marker,
	// so the zero state is constructed directly.
	zeroSrv := New(Config{})
	zeroSrv.processStart = time.Time{}
	zrec := httptest.NewRecorder()
	zeroSrv.handleHealth(zrec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if strings.Contains(zrec.Body.String(), `"process_start"`) {
		t.Errorf("process_start must be omitted for a zero boot marker: %s", zrec.Body.String())
	}
}

// TestMCPRouteRegistered guards the route table: all three method-scoped
// /mcp patterns must reach handleMCP. Driven with a NON-loopback listener so
// the answer is the ladder's 403 — an UNREGISTERED method would instead 404
// with the mux's default not-found body, which is distinguishable from both
// the 403 and the route-off 404 (that one carries route_not_found).
func TestMCPRouteRegistered(t *testing.T) {
	s := New(Config{Addr: "0.0.0.0:8080", MCPServerFactory: testMCPServerFactory})
	for _, m := range []string{http.MethodPost, http.MethodGet, http.MethodDelete} {
		t.Run(m, func(t *testing.T) {
			req := httptest.NewRequest(m, "/mcp", strings.NewReader("{}"))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (route reaches handleMCP's ladder):\n%s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "mcp_route_loopback_only") {
				t.Errorf("body = %s, want mcp_route_loopback_only (handleMCP reached)", rec.Body.String())
			}
		})
	}
}

// TestOAuthASLoopbackGateRouteRegistered guards the route table: with the
// loopback gate on and a public bind, ALL FOUR OAuth patterns must reach
// oauthASEnabled and answer 403 oauth_as_loopback_only. An UNregistered pattern
// would 404 with the mux's default not-found body instead, distinguishable from
// the gate's 403 (#2441).
func TestOAuthASLoopbackGateRouteRegistered(t *testing.T) {
	s := New(Config{
		Addr: "0.0.0.0:8080", OAuthASRequireLoopback: true,
		OAuthASIssuer: testIssuer, OAuthStore: newFakeOAuthStore(), OAuthCIMDFetcher: newCIMDFetcher(newCIMD()),
	})
	patterns := []struct{ method, path string }{
		{http.MethodGet, "/.well-known/oauth-authorization-server"},
		// The RFC 9728 PRM routes (#2391) inherit the same loopback-gate 403 arm:
		// leaving one branch of an existing convention unpinned on brand-new
		// routes is how conventions rot (CONDITION 4).
		{http.MethodGet, "/.well-known/oauth-protected-resource"},
		{http.MethodGet, "/.well-known/oauth-protected-resource/mcp"},
		{http.MethodGet, "/v0/oauth/authorize"},
		{http.MethodPost, "/v0/oauth/authorize"},
		{http.MethodPost, "/v0/oauth/token"},
	}
	for _, p := range patterns {
		t.Run(p.method+" "+p.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(p.method, p.path, nil)
			s.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (route reaches oauthASEnabled):\n%s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "oauth_as_loopback_only") {
				t.Errorf("body = %s, want oauth_as_loopback_only", rec.Body.String())
			}
		})
	}
}

// TestReleaseNotesPreviewRouteRegistered guards the route table: GET
// /v0/releases/notes/preview (#1587) must reach handleReleaseNotesPreview. The
// anonymous request reaches the handler's auth ladder and returns 401 — an
// UNregistered route would instead 404 with a default not-found body, so a 401
// here proves the route is wired in handlers.go. (handleReleaseNotesPreview
// runs the auth ladder BEFORE the nil-dependency guard so an anonymous caller
// gets 401 rather than a 503 that would leak configuration state before
// authentication.)
func TestReleaseNotesPreviewRouteRegistered(t *testing.T) {
	s := New(Config{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v0/releases/notes/preview?repo=o/n&from=a&to=b", nil)
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (route reaches handler auth ladder)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "authentication_required") {
		t.Errorf("body = %s, want authentication_required (handleReleaseNotesPreview reached)", rec.Body.String())
	}
}

// TestReleaseNotesPersistRouteRegistered guards the route table: POST
// /v0/releases/notes (#1587) must reach handleReleaseNotesPersist. The
// anonymous request reaches the handler's auth ladder and returns 401 — an
// UNregistered route would instead 404 with a default not-found body, so a 401
// here proves the route is wired in handlers.go. (handleReleaseNotesPersist
// runs the auth ladder BEFORE the nil-dependency guard so an anonymous caller
// gets 401 rather than a 503 that would leak configuration state before
// authentication.)
func TestReleaseNotesPersistRouteRegistered(t *testing.T) {
	s := New(Config{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v0/releases/notes", strings.NewReader(`{}`))
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (route reaches handler auth ladder)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "authentication_required") {
		t.Errorf("body = %s, want authentication_required (handleReleaseNotesPersist reached)", rec.Body.String())
	}
}

// TestReleaseCutRouteRegistered guards the route table: POST /v0/releases/cut
// (#1590) must reach handleReleaseCut. The anonymous request reaches the
// handler's auth ladder and returns 401 — an UNregistered route would instead
// 404 with a default not-found body, so a 401 here proves the route is wired in
// handlers.go. (handleReleaseCut runs the auth ladder BEFORE the nil-dependency
// guard so an anonymous caller gets 401 rather than a 503 that would leak
// configuration state before authentication.)
func TestReleaseCutRouteRegistered(t *testing.T) {
	s := New(Config{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v0/releases/cut", strings.NewReader(`{}`))
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (route reaches handler auth ladder)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "authentication_required") {
		t.Errorf("body = %s, want authentication_required (handleReleaseCut reached)", rec.Body.String())
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

// TestLatencyRouteRegistered guards the route table: GET
// /v0/runs/{run_id}/latency (#1702) must reach handleGetRunLatency. With no
// RunRepo configured the handler returns 503 — an UNregistered route would
// instead 404 with a default not-found body, so a 503 here proves the route is
// wired in handlers.go.
func TestLatencyRouteRegistered(t *testing.T) {
	s := New(Config{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v0/runs/"+"00000000-0000-0000-0000-000000000000"+"/latency", nil)
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (route reaches handler with no RunRepo)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "run_repo_unconfigured") {
		t.Errorf("body = %s, want run_repo_unconfigured (handleGetRunLatency reached)", rec.Body.String())
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

// TestCancelCampaignRouteRegistered guards the route table: POST
// /v0/campaigns/{campaign_id}/cancel (#2355) must reach handleCancelCampaign.
// With no CampaignRepo configured the handler returns 503 — an UNregistered
// route would instead 404 — so a 503 here proves the route is wired in
// handlers.go. (handleCancelCampaign checks the nil-CampaignRepo guard BEFORE the
// write-scope check precisely so this idiom reaches the handler.)
func TestCancelCampaignRouteRegistered(t *testing.T) {
	s := New(Config{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v0/campaigns/"+"00000000-0000-0000-0000-000000000000"+"/cancel", nil)
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (route reaches handler with no CampaignRepo)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "campaign_repo_unconfigured") {
		t.Errorf("body = %s, want campaign_repo_unconfigured (handleCancelCampaign reached)", rec.Body.String())
	}
}

// TestReviveRouteRegistered guards the route table: POST
// /v0/runs/{run_id}/revive (#1915) must reach handleReviveRun through the mux.
// The anonymous request reaches the handler's auth ladder and returns 401 — an
// UNregistered route would instead 404 with a default not-found body, so a 401
// here proves the route is wired in handlers.go. (handleReviveRun runs the auth
// ladder BEFORE the nil-dependency guard, so an anonymous caller gets 401 rather
// than a 503 that would leak configuration state before authentication.) This is
// the only test that exercises the revive registration through the ServeMux;
// revive_test.go's postRevive helper calls s.handleReviveRun directly.
func TestReviveRouteRegistered(t *testing.T) {
	s := New(Config{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+"00000000-0000-0000-0000-000000000000"+"/revive", nil)
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (route reaches handler auth ladder)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "authentication_required") {
		t.Errorf("body = %s, want authentication_required (handleReviveRun reached)", rec.Body.String())
	}
}

// TestMergeRunRouteRegistered guards the route table: POST
// /v0/runs/{run_id}/merge (E48.7 / #1954) must reach handleMergeRun through the
// mux. The anonymous request reaches the handler's auth ladder and returns 401
// — an UNregistered route would instead 404 with a default not-found body, so a
// 401 here proves the route is wired in handlers.go. (handleMergeRun runs the
// auth ladder BEFORE the nil-dependency guard.) merge_run_test.go's
// postMergeRun helper calls s.handleMergeRun directly.
func TestMergeRunRouteRegistered(t *testing.T) {
	s := New(Config{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+"00000000-0000-0000-0000-000000000000"+"/merge", strings.NewReader(`{}`))
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (route reaches handler auth ladder)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "authentication_required") {
		t.Errorf("body = %s, want authentication_required (handleMergeRun reached)", rec.Body.String())
	}
}

// TestStartCampaignItemRunRouteRegistered guards the route table: POST
// /v0/campaigns/{campaign_id}/runs (#1481) must reach handleStartCampaignItemRun.
// With no CampaignRepo configured the handler returns 503 — an UNregistered
// route would instead 404 with a default not-found body, so a 503 here proves
// the route is wired in handlers.go. (handleStartCampaignItemRun checks the
// nil-CampaignRepo guard BEFORE the write-scope check precisely so this idiom
// reaches the handler.)
func TestStartCampaignItemRunRouteRegistered(t *testing.T) {
	s := New(Config{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v0/campaigns/"+"00000000-0000-0000-0000-000000000000"+"/runs", strings.NewReader(`{}`))
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (route reaches handler with no CampaignRepo)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "campaign_repo_unconfigured") {
		t.Errorf("body = %s, want campaign_repo_unconfigured (handleStartCampaignItemRun reached)", rec.Body.String())
	}
}

// TestWebhookGitLabRouteRegistered guards the route table: POST
// /webhooks/gitlab (E45.6 / #1860) must reach handleWebhookGitLab.
// With no GitLabWebhookSecret configured the handler returns 503 — an
// UNregistered route would instead 404 with the default not-found body,
// so a 503 here proves the route is wired in handlers.go (not only
// reachable via a direct handler call).
func TestWebhookGitLabRouteRegistered(t *testing.T) {
	s := New(Config{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/gitlab", strings.NewReader(`{}`))
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusNotFound {
		t.Fatalf("POST /webhooks/gitlab returned 404 — route not registered in handlers.go")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (routed but secret unconfigured)", rec.Code)
	}
}

// TestGitLabLoginRouteRegistered guards the route table: GET
// /v0/auth/gitlab/login (E44.22 / #2109) must reach handleGitLabLogin.
// With no GitLabOAuth configured the handler returns 503 oauth_unconfigured
// — an UNregistered route would 404 with the default not-found body, so a
// 503 here proves the route is wired in handlers.go.
func TestGitLabLoginRouteRegistered(t *testing.T) {
	s := New(Config{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v0/auth/gitlab/login", nil)
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusNotFound {
		t.Fatalf("GET /v0/auth/gitlab/login returned 404 — route not registered in handlers.go")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (routed but GitLab OAuth unconfigured)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "oauth_unconfigured") {
		t.Errorf("body = %s, want oauth_unconfigured (handleGitLabLogin reached)", rec.Body.String())
	}
}

// TestGitLabCallbackRouteRegistered guards the route table: GET
// /v0/auth/gitlab/callback (E44.22 / #2109) must reach handleGitLabCallback.
// With no GitLabOAuth configured the handler returns 503 oauth_unconfigured —
// an UNregistered route would 404 instead, so a 503 here proves the route is
// wired in handlers.go.
func TestGitLabCallbackRouteRegistered(t *testing.T) {
	s := New(Config{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v0/auth/gitlab/callback?code=x&state=y", nil)
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusNotFound {
		t.Fatalf("GET /v0/auth/gitlab/callback returned 404 — route not registered in handlers.go")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (routed but GitLab OAuth unconfigured)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "oauth_unconfigured") {
		t.Errorf("body = %s, want oauth_unconfigured (handleGitLabCallback reached)", rec.Body.String())
	}
}

// TestOnboardingStartRouteRegistered guards the route table: GET
// /v0/onboarding/start (ADR-062, E44.7 / #1831) must reach
// handleOnboardingStart THROUGH the region-pin middleware. An anonymous,
// handoff-less request passes straight through the middleware and lands on the
// handler's auth ladder (401) — an UNregistered route would 404 instead.
func TestOnboardingStartRouteRegistered(t *testing.T) {
	s := New(Config{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v0/onboarding/start?provider=github&account_key=acme", nil)
	s.Handler().ServeHTTP(rec, req)

	if rec.Code == http.StatusNotFound {
		t.Fatalf("GET /v0/onboarding/start returned 404 — route not registered in handlers.go")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (route reaches handler auth ladder)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "authentication_required") {
		t.Errorf("body = %s, want authentication_required (handleOnboardingStart reached)", rec.Body.String())
	}
}

// TestOnboardingStartRouteRefusesSignedHandoffWhenDisabled is the mounted-in-
// production half of the fail-closed contract (approval condition 8): on a
// cell with NO region configured, a request carrying fh_* parameters is
// refused 503 by the middleware ON THE REAL ROUTE — it does not fall through
// to the handler as though the handoff were absent.
func TestOnboardingStartRouteRefusesSignedHandoffWhenDisabled(t *testing.T) {
	s := New(Config{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/v0/onboarding/start?provider=github&account_key=acme&fh_sig=deadbeef", nil)
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (region pin disabled):\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "region_pin_disabled") {
		t.Errorf("body = %s, want region_pin_disabled", rec.Body.String())
	}
}

// OAuth AS route registration (ADR-076 slice 3, #2436). An UNCONFIGURED server
// answers the handler's own 503 oauth_as_unconfigured body — distinguishable
// from the mux's default 404 — so a 503 here proves the route is wired.
func TestOAuthASMetadataRouteRegistered(t *testing.T) {
	assertOAuthRouteRegistered(t, http.MethodGet, "/.well-known/oauth-authorization-server")
}

func TestOAuthAuthorizeRouteRegistered(t *testing.T) {
	assertOAuthRouteRegistered(t, http.MethodGet, "/v0/oauth/authorize")
}

func TestOAuthConsentRouteRegistered(t *testing.T) {
	assertOAuthRouteRegistered(t, http.MethodPost, "/v0/oauth/authorize")
}

func TestOAuthTokenRouteRegistered(t *testing.T) {
	assertOAuthRouteRegistered(t, http.MethodPost, "/v0/oauth/token")
}

// RFC 9728 Protected Resource Metadata route registration (#2391, CONDITION 3).
// An UNCONFIGURED server answers the handler's own 503 oauth_as_unconfigured
// body — distinguishable from the mux's default 404 — so a 503 proves the route
// is wired. The suffixed route gates on oauthASEnabled BEFORE the suffix check,
// so it too answers 503 when the AS is disabled.
func TestOAuthPRMRouteRegistered(t *testing.T) {
	assertOAuthRouteRegistered(t, http.MethodGet, "/.well-known/oauth-protected-resource")
}

func TestOAuthPRMSuffixedRouteRegistered(t *testing.T) {
	assertOAuthRouteRegistered(t, http.MethodGet, "/.well-known/oauth-protected-resource/mcp")
}

func assertOAuthRouteRegistered(t *testing.T, method, path string) {
	t.Helper()
	s := New(Config{}) // AS disabled
	req := httptest.NewRequest(method, path, nil)
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("%s %s: status = %d, want 503 (route reaches handler); body=%s", method, path, rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "oauth_as_unconfigured") {
		t.Fatalf("%s %s: body = %s, want oauth_as_unconfigured", method, path, rec.Body.String())
	}
}
