package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestAPIError_Error pins the apiError.Error() rendering (#1548): a
// non-empty Details map appends a deterministic JSON suffix so callers that
// format the error via %v (e.g. run_children's between-wave transport
// warning) surface the real cause; an empty/nil Details map renders the
// concise form with no "details:" suffix.
func TestAPIError_Error(t *testing.T) {
	t.Run("with details surfaces the cause", func(t *testing.T) {
		e := &apiError{
			StatusCode: 502,
			Code:       "slice_integration_error",
			Message:    "integrate-wave failed",
			Details:    map[string]any{"error": "merge conflict in foo.go"},
		}
		got := e.Error()
		if !strings.Contains(got, "slice_integration_error") {
			t.Errorf("missing code in %q", got)
		}
		if !strings.Contains(got, "details:") {
			t.Errorf("missing details suffix in %q", got)
		}
		if !strings.Contains(got, "merge conflict in foo.go") {
			t.Errorf("details cause not surfaced in %q", got)
		}
	})
	t.Run("empty details omits the suffix", func(t *testing.T) {
		e := &apiError{StatusCode: 500, Code: "internal", Message: "boom"}
		got := e.Error()
		if strings.Contains(got, "details:") {
			t.Errorf("empty Details must not render a details suffix: %q", got)
		}
		if got != "fishhawk: HTTP 500 (internal): boom" {
			t.Errorf("unexpected concise render: %q", got)
		}
	})
	t.Run("no code, no details", func(t *testing.T) {
		e := &apiError{StatusCode: 503}
		if got := e.Error(); got != "fishhawk: HTTP 503" {
			t.Errorf("unexpected render: %q", got)
		}
	})
}

// TestCreateCampaign_OperatorAgentBytes_OmittedWhenNil pins the apiClient wire
// contract for the OPTIONAL campaign-level operator_agent override (E25.12 /
// #1451) at the HTTP-body layer, below the tool handler: a nil/empty
// operatorAgent argument omits the field entirely (json.RawMessage + omitempty),
// so a campaign without an override sends NO operator_agent key — the
// byte-identical default where each issue-run inherits its workflow contract.
func TestCreateCampaign_OperatorAgentBytes_OmittedWhenNil(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, err := r.api.CreateCampaign(context.Background(), "x/y", "#1", "", nil)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if len(fb.createCampaignBody.OperatorAgent) != 0 {
		t.Errorf("operator_agent present on a nil override: %s", fb.createCampaignBody.OperatorAgent)
	}
}

// TestCreateCampaign_OperatorAgentBytes_CarriedVerbatim proves a non-nil
// operatorAgent argument travels in the POST body verbatim as opaque JSON (the
// client does not parse or validate it — the backend is the validation
// authority) and the created Campaign decodes back.
func TestCreateCampaign_OperatorAgentBytes_CarriedVerbatim(t *testing.T) {
	fb, srv := newFakeBackend(t)
	id := uuid.New()
	override := map[string]any{"may_waive": "solo_low"}
	fb.createCampaignResp = Campaign{
		ID: id.String(), Repo: "x/y", EpicRef: "#25", State: "pending", PausePolicy: "pause_campaign",
		OperatorAgent: override,
	}
	r := newResolver(srv, nil)

	got, err := r.api.CreateCampaign(context.Background(), "x/y", "#25", "",
		json.RawMessage(`{"may_waive":"solo_low"}`))
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	var sent map[string]any
	if err := json.Unmarshal(fb.createCampaignBody.OperatorAgent, &sent); err != nil {
		t.Fatalf("operator_agent body not valid JSON: %v", err)
	}
	if sent["may_waive"] != "solo_low" {
		t.Errorf("sent operator_agent.may_waive = %v, want solo_low", sent["may_waive"])
	}
	if got.OperatorAgent["may_waive"] != "solo_low" {
		t.Errorf("decoded Campaign.OperatorAgent = %+v", got.OperatorAgent)
	}
}

// TestFiledWorkItem_DecodesLabelCompleteness pins the MCP-side wire contract
// for the #1616 LOUD label-completeness report: a work-items response carrying
// defaulted_labels + missing_label_namespaces decodes into FiledWorkItem so the
// tool result surfaces them verbatim to the operator.
func TestFiledWorkItem_DecodesLabelCompleteness(t *testing.T) {
	const body = `{"type":"feature","title":"[E22.1] x","number":7,` +
		`"url":"https://example/7","provider":"github_projects",` +
		`"applied_labels":["type:feature","autonomy:medium"],` +
		`"defaulted_labels":["autonomy:medium"],` +
		`"missing_label_namespaces":["area"],"boarded":true,"epic_linked":true,"audited":false}`
	var got FiledWorkItem
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode FiledWorkItem: %v", err)
	}
	if len(got.DefaultedLabels) != 1 || got.DefaultedLabels[0] != "autonomy:medium" {
		t.Errorf("DefaultedLabels = %v, want [autonomy:medium]", got.DefaultedLabels)
	}
	if len(got.MissingLabelNamespaces) != 1 || got.MissingLabelNamespaces[0] != "area" {
		t.Errorf("MissingLabelNamespaces = %v, want [area]", got.MissingLabelNamespaces)
	}
}

// TestDeferFiledIssue_DecodesLabelCompleteness pins the same wire contract on
// the defer path's filed-issue block (#1616).
func TestDeferFiledIssue_DecodesLabelCompleteness(t *testing.T) {
	const body = `{"type":"chore","title":"[E22.4] x","number":9,` +
		`"url":"https://example/9","provider":"github_projects",` +
		`"applied_labels":["type:chore","autonomy:medium"],` +
		`"defaulted_labels":["autonomy:medium"],"missing_label_namespaces":["area"]}`
	var got DeferFiledIssue
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode DeferFiledIssue: %v", err)
	}
	if len(got.DefaultedLabels) != 1 || got.DefaultedLabels[0] != "autonomy:medium" {
		t.Errorf("DefaultedLabels = %v, want [autonomy:medium]", got.DefaultedLabels)
	}
	if len(got.MissingLabelNamespaces) != 1 || got.MissingLabelNamespaces[0] != "area" {
		t.Errorf("MissingLabelNamespaces = %v, want [area]", got.MissingLabelNamespaces)
	}
}

// TestListRunAudit_EmitsAllowUnknownAndCategory is the #1764 binding-condition
// (1) client seam proof: ListRunAudit must actually serialize allow_unknown=true
// (and the category) into the request RawQuery, so the MCP-input →
// client-serialization → handler path cannot silently drop the param and leave
// the endpoint's known-category validation re-rejecting the tool's own polling
// calls for an operator-approved unknown category.
func TestListRunAudit_EmitsAllowUnknownAndCategory(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()

	_, _, err := r.api.ListRunAudit(context.Background(), runID, ListRunAuditFilter{
		Category:     "scope_amendment_pending", // an unknown category the operator opted into
		AllowUnknown: true,
	})
	if err != nil {
		t.Fatalf("ListRunAudit: %v", err)
	}
	q := fb.perRunAuditLastQueryByID[runID]
	for _, want := range []string{"allow_unknown=true", "category=scope_amendment_pending"} {
		if !strings.Contains(q, want) {
			t.Errorf("request RawQuery %q missing %q", q, want)
		}
	}
}

// TestListRunAudit_OmitsAllowUnknownWhenFalse proves the param is absent
// (byte-identical to the pre-#1764 request) when AllowUnknown is false — the
// omitempty half of the client contract.
func TestListRunAudit_OmitsAllowUnknownWhenFalse(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()

	_, _, err := r.api.ListRunAudit(context.Background(), runID, ListRunAuditFilter{
		Category: "implement_reviewed",
	})
	if err != nil {
		t.Fatalf("ListRunAudit: %v", err)
	}
	q := fb.perRunAuditLastQueryByID[runID]
	if strings.Contains(q, "allow_unknown") {
		t.Errorf("allow_unknown must be omitted when false; got %q", q)
	}
}

// TestGetRunLatency_CollapsesEmptyObject pins the apiClient no-data contract
// (#1702): the backend returns 200 + `{}` when no gate interval has resolved,
// and GetRunLatency must collapse that to (nil, nil) so callers branch on a nil
// pointer — the same presence-sentinel convention as GetRunCost.
func TestGetRunLatency_CollapsesEmptyObject(t *testing.T) {
	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	// No seedRunLatency → the fake serves {}.

	rl, err := r.api.GetRunLatency(context.Background(), runID)
	if err != nil {
		t.Fatalf("GetRunLatency: %v", err)
	}
	if rl != nil {
		t.Errorf("empty object must collapse to nil, got %+v", rl)
	}
}

// TestGetRunLatency_DecodesGatedRollup proves a populated rollup decodes off the
// wire with its gates and totals intact.
func TestGetRunLatency_DecodesGatedRollup(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	seedRunLatency(fb, runID, RunLatency{
		Gates: []LatencyGate{
			{Gate: "plan_approval", WaitSeconds: 300},
		},
		TotalWaitOnHumanSeconds: 300,
		WallClockSeconds:        1200,
	})

	rl, err := r.api.GetRunLatency(context.Background(), runID)
	if err != nil {
		t.Fatalf("GetRunLatency: %v", err)
	}
	if rl == nil {
		t.Fatal("expected a rollup, got nil")
	}
	if len(rl.Gates) != 1 || rl.Gates[0].Gate != "plan_approval" || rl.Gates[0].WaitSeconds != 300 {
		t.Errorf("gates = %+v, want a single plan_approval/300", rl.Gates)
	}
	if rl.TotalWaitOnHumanSeconds != 300 || rl.WallClockSeconds != 1200 {
		t.Errorf("totals = wait %g wall %g, want 300 / 1200", rl.TotalWaitOnHumanSeconds, rl.WallClockSeconds)
	}
}

// releaseTestClient spins a bare httptest server around handler h and returns a
// wired apiClient — the release methods (E33.5 / #1590) are the only endpoints
// under test, so a self-contained server is simpler than the shared fakeBackend
// mux.
func releaseTestClient(t *testing.T, h http.HandlerFunc) *apiClient {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return newAPIClient(config{backendURL: ts.URL, apiToken: "tok-test"})
}

// TestPreviewReleaseNotes_ReadsMarkdownBody proves the preview method reads the
// text/markdown body verbatim (NOT a JSON envelope) and sends the coordinates
// as query params on a GET (E33.2 / #1587).
func TestPreviewReleaseNotes_ReadsMarkdownBody(t *testing.T) {
	const md = "# Release v1.2.0\n\nsuggested bump: minor (because ...)\n"
	var gotMethod, gotPath, gotQuery string
	c := releaseTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		_, _ = io.WriteString(w, md)
	})

	got, err := c.PreviewReleaseNotes(context.Background(), "x/y", "v1.1.0", "HEAD")
	if err != nil {
		t.Fatalf("PreviewReleaseNotes: %v", err)
	}
	if got != md {
		t.Errorf("markdown body = %q, want %q", got, md)
	}
	if gotMethod != http.MethodGet || gotPath != "/v0/releases/notes/preview" {
		t.Errorf("request = %s %s, want GET /v0/releases/notes/preview", gotMethod, gotPath)
	}
	for _, want := range []string{"repo=x%2Fy", "from=v1.1.0", "to=HEAD"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query %q missing %q", gotQuery, want)
		}
	}
}

// TestPreviewReleaseNotes_SurfacesAPIError proves a non-2xx markdown-endpoint
// response is still parsed as the OpenAPI error envelope and returned as
// *apiError, so the tool layer gets the same typed error surface as the JSON
// methods (the getText fail-closed branch).
func TestPreviewReleaseNotes_SurfacesAPIError(t *testing.T) {
	c := releaseTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"code":"authentication_required","message":"an authenticated token is required"}}`)
	})

	_, err := c.PreviewReleaseNotes(context.Background(), "x/y", "v1.1.0", "HEAD")
	var ae *apiError
	if !errors.As(err, &ae) {
		t.Fatalf("error = %v, want *apiError", err)
	}
	if ae.StatusCode != http.StatusUnauthorized || ae.Code != "authentication_required" {
		t.Errorf("apiError = %d/%q, want 401/authentication_required", ae.StatusCode, ae.Code)
	}
}

// TestPersistReleaseNotes_PostsBodyAndDecodes proves the persist method POSTs
// the coordinates + stage_id as JSON and decodes the 201 body into the typed
// result (E33.2 / #1587).
func TestPersistReleaseNotes_PostsBodyAndDecodes(t *testing.T) {
	stageID := uuid.NewString()
	artID := uuid.NewString()
	var gotMethod, gotPath string
	var gotBody releaseNotesPersistRequest
	c := releaseTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(ReleaseNotesPersistResult{
			ArtifactID: artID, StageID: stageID, Repo: "x/y", From: "v1.1.0", To: "HEAD",
			ContentHash: "deadbeef", Markdown: "# notes",
		})
	})

	res, err := c.PersistReleaseNotes(context.Background(), "x/y", "v1.1.0", "HEAD", stageID)
	if err != nil {
		t.Fatalf("PersistReleaseNotes: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v0/releases/notes" {
		t.Errorf("request = %s %s, want POST /v0/releases/notes", gotMethod, gotPath)
	}
	if gotBody.Repo != "x/y" || gotBody.From != "v1.1.0" || gotBody.To != "HEAD" || gotBody.StageID != stageID {
		t.Errorf("sent body = %+v, want the coordinates + stage_id", gotBody)
	}
	if res.ArtifactID != artID || res.ContentHash != "deadbeef" || res.Markdown != "# notes" {
		t.Errorf("decoded result = %+v", res)
	}
}

// TestPersistReleaseNotes_SurfacesAPIError proves a 404 stage_not_found from the
// persist endpoint surfaces as *apiError (the do error passthrough).
func TestPersistReleaseNotes_SurfacesAPIError(t *testing.T) {
	c := releaseTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"code":"stage_not_found","message":"no stage with that id"}}`)
	})

	_, err := c.PersistReleaseNotes(context.Background(), "x/y", "v1.1.0", "HEAD", uuid.NewString())
	var ae *apiError
	if !errors.As(err, &ae) {
		t.Fatalf("error = %v, want *apiError", err)
	}
	if ae.StatusCode != http.StatusNotFound || ae.Code != "stage_not_found" {
		t.Errorf("apiError = %d/%q, want 404/stage_not_found", ae.StatusCode, ae.Code)
	}
}

// bodyCap mirrors the backend's maxReapFailureBodyBytes
// (backend/internal/server/reap_failure.go) — the exact 32*1024 limit the
// reap-failure endpoint enforces (#1791).
const bodyCap = 32 * 1024

// TestReportStageFailure_OversizedReasonDetail_FitsCap is the #1791 client half:
// an oversized reason+detail (the multi-module verify dump the reaper re-POSTs)
// is truncated so the body fits the 32KB cap on the FIRST attempt — no 413, no
// retry — and the report succeeds.
func TestReportStageFailure_OversizedReasonDetail_FitsCap(t *testing.T) {
	var calls, gotLen int
	c := releaseTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		raw, _ := io.ReadAll(r.Body)
		gotLen = len(raw)
		if len(raw) > bodyCap {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "body_too_large"}})
			return
		}
		_ = json.NewEncoder(w).Encode(ReapFailureResult{Transitioned: true, StageState: "failed"})
	})

	res, err := c.ReportStageFailure(context.Background(), uuid.New(), uuid.New(), "C",
		strings.Repeat("r", 100*1024), strings.Repeat("d", 200*1024), 1)
	if err != nil {
		t.Fatalf("ReportStageFailure: %v", err)
	}
	if !res.Transitioned {
		t.Error("Transitioned = false, want true")
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (truncated body fits on first POST)", calls)
	}
	if gotLen > bodyCap {
		t.Errorf("posted body %d exceeds cap %d", gotLen, bodyCap)
	}
}

// TestReportStageFailure_413ThenAggressiveRetry drives EXACTLY ONE aggressive
// retry: a first POST that 413s re-marshals both fields with the aggressive cap
// and re-POSTs once with a strictly smaller body that succeeds.
func TestReportStageFailure_413ThenAggressiveRetry(t *testing.T) {
	var bodies [][]byte
	c := releaseTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, raw)
		if len(bodies) == 1 {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "body_too_large"}})
			return
		}
		_ = json.NewEncoder(w).Encode(ReapFailureResult{Transitioned: true, StageState: "failed"})
	})

	if _, err := c.ReportStageFailure(context.Background(), uuid.New(), uuid.New(), "C",
		"short reason", strings.Repeat("d", 200*1024), 2); err != nil {
		t.Fatalf("ReportStageFailure: %v", err)
	}
	if len(bodies) != 2 {
		t.Fatalf("calls = %d, want exactly 2 (413 then one aggressive retry)", len(bodies))
	}
	if len(bodies[1]) >= len(bodies[0]) {
		t.Errorf("aggressive body %d not smaller than first %d", len(bodies[1]), len(bodies[0]))
	}
	if len(bodies[1]) > bodyCap {
		t.Errorf("aggressive body %d exceeds cap %d", len(bodies[1]), bodyCap)
	}
}

// TestReportStageFailure_Persistent413_NoLoop asserts the aggressive retry is
// bounded: when even the aggressive-cap body 413s, ReportStageFailure surfaces
// the second 4xx after EXACTLY TWO requests (initial + one aggressive retry) —
// it does NOT keep retrying. Distinct from the 413-then-success and the
// 5xx-before-retry cases: only a persistent 4xx exercises the loop guard.
func TestReportStageFailure_Persistent413_NoLoop(t *testing.T) {
	var calls int
	c := releaseTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "body_too_large"}})
	})

	_, err := c.ReportStageFailure(context.Background(), uuid.New(), uuid.New(), "C",
		"reason", strings.Repeat("d", 200*1024), 1)
	if err == nil {
		t.Fatal("expected an error after a persistent 413")
	}
	var ae *apiError
	if !errors.As(err, &ae) || ae.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("err = %v, want *apiError with 413", err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want exactly 2 (initial + one aggressive retry, no loop)", calls)
	}
}

// TestReportStageFailure_5xx_NotRescuedByAggressiveRetry asserts the retry is
// scoped to 4xx: a 5xx surfaces unchanged after a SINGLE attempt — the
// aggressive-truncation path never fires for a transient server error.
func TestReportStageFailure_5xx_NotRescuedByAggressiveRetry(t *testing.T) {
	var calls int
	c := releaseTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "internal"}})
	})

	_, err := c.ReportStageFailure(context.Background(), uuid.New(), uuid.New(), "C",
		"reason", strings.Repeat("d", 200*1024), 1)
	if err == nil {
		t.Fatal("expected an error on 5xx")
	}
	var ae *apiError
	if !errors.As(err, &ae) || ae.StatusCode != http.StatusInternalServerError {
		t.Errorf("err = %v, want *apiError with 500", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (5xx must not trigger the aggressive retry)", calls)
	}
}
