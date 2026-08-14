package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
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
	t.Run("with details surfaces the allow-listed keys", func(t *testing.T) {
		// Post-#2587 a 5xx body carries only allow-listed structured keys (the
		// raw cause is redacted server-side), so the rendered suffix surfaces
		// those keys — the rendering mechanism itself is unchanged.
		e := &apiError{
			StatusCode: 502,
			Code:       "slice_integration_error",
			Message:    "integrate-wave failed",
			Details:    map[string]any{"run_id": "run-abc"},
		}
		got := e.Error()
		if !strings.Contains(got, "slice_integration_error") {
			t.Errorf("missing code in %q", got)
		}
		if !strings.Contains(got, "details:") {
			t.Errorf("missing details suffix in %q", got)
		}
		if !strings.Contains(got, "run-abc") {
			t.Errorf("details key not surfaced in %q", got)
		}
	})
	t.Run("with error_ref appends the ref", func(t *testing.T) {
		e := &apiError{
			StatusCode: 502,
			Code:       "work_item_filing_failed",
			Message:    "provider could not file the work item",
			ErrorRef:   "req-xyz789",
		}
		got := e.Error()
		if !strings.Contains(got, "error_ref=req-xyz789") {
			t.Errorf("missing error_ref suffix in %q", got)
		}
		if strings.Contains(got, "details:") {
			t.Errorf("no details, but a details suffix rendered: %q", got)
		}
	})
	t.Run("error_ref and details both render", func(t *testing.T) {
		e := &apiError{
			StatusCode: 502,
			Code:       "work_item_filing_failed",
			Message:    "boom",
			Details:    map[string]any{"provider": "github_projects"},
			ErrorRef:   "req-both",
		}
		got := e.Error()
		if !strings.Contains(got, "github_projects") || !strings.Contains(got, "error_ref=req-both") {
			t.Errorf("want both details and error_ref in %q", got)
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

	_, err := r.api.CreateCampaign(context.Background(), "x/y", "#1", "", nil, nil)
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
		json.RawMessage(`{"may_waive":"solo_low"}`), nil)
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

// TestRunMirror_DecodesWorkingDir pins the client Run mirror's working_dir json
// tag against the backend's runResponse tag (E66.42 / #2482): the tags must be
// byte-identical or the mirror silently decodes to empty and every inheriting
// verb reverts to demanding the parameter (the #371-class trap). The literal
// key here is the backend's wire tag; a rename on either side breaks this.
func TestRunMirror_DecodesWorkingDir(t *testing.T) {
	const body = `{"id":"11111111-1111-1111-1111-111111111111","repo":"x/y",` +
		`"workflow_id":"feature_change","workflow_sha":"deadbeef","trigger_source":"cli",` +
		`"state":"running","working_dir":"/Users/dev/src/fishhawk",` +
		`"created_at":"2026-08-05T00:00:00Z","updated_at":"2026-08-05T00:00:00Z"}`
	var got Run
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode Run: %v", err)
	}
	if got.WorkingDir != "/Users/dev/src/fishhawk" {
		t.Errorf("Run.WorkingDir = %q, want the decoded binding (json tag mismatch?)", got.WorkingDir)
	}
}

// TestRunMirror_DecodesPredictedRuntimeMinutes pins the client Run mirror's
// predicted_runtime_minutes json tag against the backend's runResponse tag
// (E48.62 / #2489). Same #371-class trap as working_dir above, with a quieter
// failure: a tag mismatch decodes to zero, the derivation silently falls back
// to its elapsed branch, and the poll cadence is merely wrong rather than
// broken. The literal key here is the backend's wire tag.
//
// The omitted case is the mixed-version degrade — an older backend that does
// not emit the key at all — asserted to reach zero (the unstamped state the
// fallback branch handles) rather than any other sentinel.
func TestRunMirror_DecodesPredictedRuntimeMinutes(t *testing.T) {
	const head = `{"id":"11111111-1111-1111-1111-111111111111","repo":"x/y",` +
		`"workflow_id":"feature_change","workflow_sha":"deadbeef","trigger_source":"cli",` +
		`"state":"running",`
	const tail = `"created_at":"2026-08-05T00:00:00Z","updated_at":"2026-08-05T00:00:00Z"}`

	var got Run
	if err := json.Unmarshal([]byte(head+`"predicted_runtime_minutes":115,`+tail), &got); err != nil {
		t.Fatalf("decode Run: %v", err)
	}
	if got.PredictedRuntimeMinutes != 115 {
		t.Errorf("Run.PredictedRuntimeMinutes = %d, want 115 (json tag mismatch?)", got.PredictedRuntimeMinutes)
	}

	var omitted Run
	if err := json.Unmarshal([]byte(head+tail), &omitted); err != nil {
		t.Fatalf("decode Run (key omitted): %v", err)
	}
	if omitted.PredictedRuntimeMinutes != 0 {
		t.Errorf("Run.PredictedRuntimeMinutes = %d, want 0 when the key is omitted", omitted.PredictedRuntimeMinutes)
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

// TestAcceptanceDispatchAdmission_WireShape pins the apiClient method's exact
// wire contract (#1928): POST to /v0/stages/{id}/acceptance-admission with the
// bearer token, and the 200 body decoded into AcceptanceAdmissionResult. Both a
// short-circuit hit and the no-op false path are asserted, plus HTTP-error
// surfacing as *apiError.
func TestAcceptanceDispatchAdmission_WireShape(t *testing.T) {
	stageID := uuid.New()

	t.Run("short-circuit hit decodes", func(t *testing.T) {
		var gotMethod, gotPath, gotAuth string
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"short_circuited":true,"kind":"all_skip_with_basis","basis":"all-skip-with-basis","criteria_total":2,"stage":{"id":"` + stageID.String() + `","type":"acceptance","state":"succeeded"}}`))
		}))
		defer ts.Close()
		c := newAPIClient(config{backendURL: ts.URL, apiToken: "tok-test"})

		res, err := c.AcceptanceDispatchAdmission(context.Background(), stageID)
		if err != nil {
			t.Fatalf("AcceptanceDispatchAdmission: %v", err)
		}
		if gotMethod != http.MethodPost {
			t.Errorf("method = %q, want POST", gotMethod)
		}
		if gotPath != "/v0/stages/"+stageID.String()+"/acceptance-admission" {
			t.Errorf("path = %q", gotPath)
		}
		if gotAuth != "Bearer tok-test" {
			t.Errorf("auth = %q, want Bearer tok-test", gotAuth)
		}
		if !res.ShortCircuited || res.Kind != "all_skip_with_basis" || res.Basis != "all-skip-with-basis" || res.CriteriaTotal != 2 {
			t.Errorf("res = %+v, want a decoded short-circuit hit", res)
		}
		if res.Stage == nil || res.Stage.State != "succeeded" {
			t.Errorf("res.Stage = %+v, want a succeeded stage", res.Stage)
		}
	})

	t.Run("no-op false decodes", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"short_circuited":false}`))
		}))
		defer ts.Close()
		c := newAPIClient(config{backendURL: ts.URL, apiToken: "tok-test"})
		res, err := c.AcceptanceDispatchAdmission(context.Background(), stageID)
		if err != nil {
			t.Fatalf("AcceptanceDispatchAdmission: %v", err)
		}
		if res.ShortCircuited {
			t.Errorf("short_circuited = true, want false")
		}
		// An old-backend body without the new fields decodes to zero values so
		// the verb spawns as today (the mixed-version degrade).
		if res.NeedsTarget || len(res.TargetHosts) != 0 || res.ExpectedHeadSHA != "" {
			t.Errorf("needs_target block = (%v, %v, %q), want all zero on an old-backend body", res.NeedsTarget, res.TargetHosts, res.ExpectedHeadSHA)
		}
	})

	t.Run("needs_target block decodes (#1953)", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"short_circuited":false,"needs_target":true,"target_hosts":["localhost:8090","staging.example:443"],"expected_head_sha":"abc1234def"}`))
		}))
		defer ts.Close()
		c := newAPIClient(config{backendURL: ts.URL, apiToken: "tok-test"})
		res, err := c.AcceptanceDispatchAdmission(context.Background(), stageID)
		if err != nil {
			t.Fatalf("AcceptanceDispatchAdmission: %v", err)
		}
		if res.ShortCircuited {
			t.Errorf("short_circuited = true, want false")
		}
		if !res.NeedsTarget {
			t.Errorf("needs_target = false, want true")
		}
		wantHosts := []string{"localhost:8090", "staging.example:443"}
		if len(res.TargetHosts) != 2 || res.TargetHosts[0] != wantHosts[0] || res.TargetHosts[1] != wantHosts[1] {
			t.Errorf("target_hosts = %v, want %v", res.TargetHosts, wantHosts)
		}
		if res.ExpectedHeadSHA != "abc1234def" {
			t.Errorf("expected_head_sha = %q, want abc1234def", res.ExpectedHeadSHA)
		}
	})

	t.Run("HTTP error surfaces as apiError", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"code":"internal_error","message":"boom"}}`))
		}))
		defer ts.Close()
		c := newAPIClient(config{backendURL: ts.URL, apiToken: "tok-test"})
		_, err := c.AcceptanceDispatchAdmission(context.Background(), stageID)
		if err == nil {
			t.Fatal("expected an error on 500")
		}
		var ae *apiError
		if !errors.As(err, &ae) || ae.StatusCode != http.StatusInternalServerError {
			t.Errorf("err = %v, want *apiError with 500", err)
		}
	})
}

// TestHostDispatchStage_WireShape pins the tool -> client -> HTTP boundary for
// HostDispatchStage (#1912): the POST method, the
// /v0/runs/{run_id}/stages/{stage_id}/host-dispatch path, the Bearer auth
// header, the decode of the 200 body (transitioned + stage_state), and that a
// 4xx (409 dispatch_not_admissible) surfaces as *apiError so the callers can
// fail closed.
func TestHostDispatchStage_WireShape(t *testing.T) {
	runID := uuid.New()
	stageID := uuid.New()
	wantPath := "/v0/runs/" + runID.String() + "/stages/" + stageID.String() + "/host-dispatch"

	t.Run("200 transitioned decodes", func(t *testing.T) {
		var gotMethod, gotPath, gotAuth string
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"transitioned":true,"stage_state":"dispatched"}`))
		}))
		defer ts.Close()
		c := newAPIClient(config{backendURL: ts.URL, apiToken: "tok-test"})

		res, err := c.HostDispatchStage(context.Background(), runID, stageID)
		if err != nil {
			t.Fatalf("HostDispatchStage: %v", err)
		}
		if gotMethod != http.MethodPost {
			t.Errorf("method = %q, want POST", gotMethod)
		}
		if gotPath != wantPath {
			t.Errorf("path = %q, want %q", gotPath, wantPath)
		}
		if gotAuth != "Bearer tok-test" {
			t.Errorf("auth = %q, want Bearer tok-test", gotAuth)
		}
		if !res.Transitioned || res.StageState != "dispatched" {
			t.Errorf("res = %+v, want transitioned:true stage_state:dispatched", res)
		}
	})

	t.Run("200 idempotent no-op decodes", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"transitioned":false,"stage_state":"dispatched"}`))
		}))
		defer ts.Close()
		c := newAPIClient(config{backendURL: ts.URL, apiToken: "tok-test"})
		res, err := c.HostDispatchStage(context.Background(), runID, stageID)
		if err != nil {
			t.Fatalf("HostDispatchStage: %v", err)
		}
		if res.Transitioned {
			t.Errorf("transitioned = true, want false (idempotent dead-runner re-dispatch)")
		}
	})

	t.Run("409 surfaces as apiError", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"code":"dispatch_not_admissible","message":"stage is not in a host-dispatchable state"}}`))
		}))
		defer ts.Close()
		c := newAPIClient(config{backendURL: ts.URL, apiToken: "tok-test"})
		_, err := c.HostDispatchStage(context.Background(), runID, stageID)
		if err == nil {
			t.Fatal("expected an error on 409")
		}
		var ae *apiError
		if !errors.As(err, &ae) || ae.StatusCode != http.StatusConflict || ae.Code != "dispatch_not_admissible" {
			t.Errorf("err = %v, want *apiError with 409 dispatch_not_admissible", err)
		}
		// A non-dependency 4xx is NOT annotated — the message stays the raw
		// apiError, with none of the wave-order guidance.
		if strings.Contains(err.Error(), "wave-order guard") {
			t.Errorf("dispatch_not_admissible must not gain the wave-order annotation: %v", err)
		}
	})

	// The wave-order refusal (409 dependency_not_satisfied) is annotated ONCE at
	// the client as a deliberate ordering refusal (#2546): the error still
	// errors.As into *apiError (so every fail-closed caller is unchanged) AND its
	// message carries the "dispatch the blocking sibling first" guidance pointing
	// at fishhawk_get_run_status children[]. Deleting the annotation makes the
	// message the raw apiError and drops the guidance — the counterfactual.
	t.Run("409 dependency_not_satisfied is annotated as an ordering refusal", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"code":"dependency_not_satisfied","message":"slice 1 is blocked on dependency slice 0 (run abc, state pending)","details":{"blocking_run_id":"abc"}}}`))
		}))
		defer ts.Close()
		c := newAPIClient(config{backendURL: ts.URL, apiToken: "tok-test"})
		_, err := c.HostDispatchStage(context.Background(), runID, stageID)
		if err == nil {
			t.Fatal("expected an error on 409 dependency_not_satisfied")
		}
		// Still typed — callers that switch on Code are unchanged.
		var ae *apiError
		if !errors.As(err, &ae) || ae.Code != "dependency_not_satisfied" {
			t.Errorf("err = %v, want the *apiError preserved in the chain", err)
		}
		// Annotated with the ordering guidance.
		if !strings.Contains(err.Error(), "dispatch the blocking sibling first") ||
			!strings.Contains(err.Error(), "fishhawk_get_run_status") {
			t.Errorf("err = %v, want the wave-order ordering guidance pointing at get_run_status children[]", err)
		}
	})

	// base_branch (#2363) decoded from a LITERAL body, not a re-marshal of
	// HostDispatchResult: a struct-marshalling fake shares the json tag with the
	// decoder, so a wrong tag round-trips symmetrically and is invisible. The
	// literal here is the client's half of that assertion; the fixture subtest
	// below is the half tied to the server's real bytes.
	t.Run("200 decodes base_branch from a literal body", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"transitioned":true,"stage_state":"dispatched","base_branch":"fishhawk/run-abc-consolidated"}`))
		}))
		defer ts.Close()
		c := newAPIClient(config{backendURL: ts.URL, apiToken: "tok-test"})
		res, err := c.HostDispatchStage(context.Background(), runID, stageID)
		if err != nil {
			t.Fatalf("HostDispatchStage: %v", err)
		}
		if res.BaseBranch != "fishhawk/run-abc-consolidated" {
			t.Errorf("base_branch = %q, want fishhawk/run-abc-consolidated", res.BaseBranch)
		}
	})

	// A body that omits base_branch (every non-fan-out caller, and any older
	// backend) decodes to "" — which means "keep the base you already had".
	t.Run("200 without base_branch decodes empty", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"transitioned":true,"stage_state":"dispatched"}`))
		}))
		defer ts.Close()
		c := newAPIClient(config{backendURL: ts.URL, apiToken: "tok-test"})
		res, err := c.HostDispatchStage(context.Background(), runID, stageID)
		if err != nil {
			t.Fatalf("HostDispatchStage: %v", err)
		}
		if res.BaseBranch != "" {
			t.Errorf("base_branch = %q, want empty when the server omits it", res.BaseBranch)
		}
	})

	// THE REAL DECODE BOUNDARY. The server serves the bytes in
	// backend/internal/server/testdata/host_dispatch_dependent_child.json —
	// proven byte-identical to handleHostDispatchStage's own 200 body by
	// server.TestHostDispatch_DependentChild_ResponseMatchesWireFixture. Feeding
	// THOSE bytes (os.ReadFile of the file, never a re-marshal) to a REAL
	// apiClient is what makes a json-tag drift on either side fail: the fixture
	// cannot drift from the server, and the client cannot share a wrong tag with
	// it (#2660).
	t.Run("200 decodes the server's own wire fixture bytes", func(t *testing.T) {
		fixture, err := os.ReadFile(filepath.Join("..", "server", "testdata", "host_dispatch_dependent_child.json"))
		if err != nil {
			t.Fatalf("read server wire fixture: %v", err)
		}
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fixture)
		}))
		defer ts.Close()
		c := newAPIClient(config{backendURL: ts.URL, apiToken: "tok-test"})
		res, err := c.HostDispatchStage(context.Background(), runID, stageID)
		if err != nil {
			t.Fatalf("HostDispatchStage: %v", err)
		}
		if !res.Transitioned || res.StageState != "dispatched" {
			t.Errorf("res = %+v, want transitioned:true stage_state:dispatched", res)
		}
		if res.BaseBranch == "" {
			t.Fatal("base_branch decoded empty from the server's own wire bytes — the client json tag disagrees with hostDispatchResponse")
		}
		// Cross-check against the fixture's own value so the assertion cannot
		// pass on a stray non-empty string.
		var raw map[string]any
		if err := json.Unmarshal(fixture, &raw); err != nil {
			t.Fatalf("decode fixture: %v", err)
		}
		if want, _ := raw["base_branch"].(string); res.BaseBranch != want {
			t.Errorf("base_branch = %q, want %q (the fixture's value)", res.BaseBranch, want)
		}
	})

	// The per-wave integration refusal (409 wave_not_integrated, #2363) is
	// annotated ONCE at the client: the error still errors.As into *apiError (so
	// every fail-closed caller is unchanged) AND its message says the
	// predecessors are integrating server-side and the action is to WAIT —
	// deliberately different guidance from the wave-ORDER refusal, which says to
	// dispatch a blocking sibling first.
	t.Run("409 wave_not_integrated is annotated as an integration wait", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"code":"wave_not_integrated","message":"slice 2 depends on slices [0 1]","details":{"missing_dependency_slices":[1]}}}`))
		}))
		defer ts.Close()
		c := newAPIClient(config{backendURL: ts.URL, apiToken: "tok-test"})
		_, err := c.HostDispatchStage(context.Background(), runID, stageID)
		if err == nil {
			t.Fatal("expected an error on 409 wave_not_integrated")
		}
		var ae *apiError
		if !errors.As(err, &ae) || ae.Code != "wave_not_integrated" {
			t.Errorf("err = %v, want the *apiError preserved in the chain", err)
		}
		if !strings.Contains(err.Error(), "not yet integrated") ||
			!strings.Contains(err.Error(), "retry the dispatch shortly") {
			t.Errorf("err = %v, want the integration-wait guidance", err)
		}
		// It must NOT inherit the wave-ORDER guidance: nothing needs dispatching
		// first, so telling the operator to dispatch a blocking sibling would be
		// wrong advice.
		if strings.Contains(err.Error(), "dispatch the blocking sibling first") {
			t.Errorf("wave_not_integrated must not carry the wave-order guidance: %v", err)
		}
	})
}

// TestRunMirror_DecodesDecompositionFields pins the client Run mirror's
// decomposed_from / slice_index / slice_depends_on json tags against the
// backend runResponse tags (E48.99 / #2546). A tag drift decodes the field to
// nil — the #371-class trap that lets childrenStatusFor silently report a
// blocked child as dispatchable. The omitted case is the mixed-version degrade
// (an older backend that omits the keys → nil).
func TestRunMirror_DecodesDecompositionFields(t *testing.T) {
	const head = `{"id":"11111111-1111-1111-1111-111111111111","repo":"x/y",` +
		`"workflow_id":"feature_change","workflow_sha":"deadbeef","trigger_source":"cli","state":"running",`
	const tail = `"created_at":"2026-08-05T00:00:00Z","updated_at":"2026-08-05T00:00:00Z"}`

	var got Run
	body := head + `"decomposed_from":"22222222-2222-2222-2222-222222222222","slice_index":1,"slice_depends_on":[0],` + tail
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode Run: %v", err)
	}
	if got.DecomposedFrom == nil || *got.DecomposedFrom != "22222222-2222-2222-2222-222222222222" {
		t.Errorf("Run.DecomposedFrom = %v, want the decoded parent (json tag mismatch?)", got.DecomposedFrom)
	}
	if got.SliceIndex == nil || *got.SliceIndex != 1 {
		t.Errorf("Run.SliceIndex = %v, want 1 (json tag mismatch?)", got.SliceIndex)
	}
	if len(got.SliceDependsOn) != 1 || got.SliceDependsOn[0] != 0 {
		t.Errorf("Run.SliceDependsOn = %v, want [0] (json tag mismatch?)", got.SliceDependsOn)
	}

	var omitted Run
	if err := json.Unmarshal([]byte(head+tail), &omitted); err != nil {
		t.Fatalf("decode Run (keys omitted): %v", err)
	}
	if omitted.DecomposedFrom != nil || omitted.SliceIndex != nil || omitted.SliceDependsOn != nil {
		t.Errorf("older-backend Run = %+v, want all decomposition fields nil (mixed-version degrade)", omitted)
	}
}

// TestReviveRun_WireShape pins the tool -> client -> HTTP boundary for
// ReviveRun (#1915): the POST method, the /v0/runs/{run_id}/revive path, the
// Bearer auth header, and the decode of the revive 200 body (the re-opened run
// plus the per-stage re-park summary). The revive_run_test.go tests exercise the
// tool handler against a fakeBackend seam; this is the only test that crosses the
// real apiClient.do HTTP wire for the revive endpoint.
func TestReviveRun_WireShape(t *testing.T) {
	runID := uuid.New()

	t.Run("200 decodes run + restored stages", func(t *testing.T) {
		var gotMethod, gotPath, gotAuth string
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"run":{"id":"` + runID.String() + `","state":"running"},` +
				`"restored_stages":[{"stage_id":"11111111-1111-1111-1111-111111111111",` +
				`"type":"implement","prior_category":"A","prior_reason":"agent crashed",` +
				`"restored_state":"pending"}]}`))
		}))
		defer ts.Close()
		c := newAPIClient(config{backendURL: ts.URL, apiToken: "tok-test"})

		res, err := c.ReviveRun(context.Background(), runID)
		if err != nil {
			t.Fatalf("ReviveRun: %v", err)
		}
		if gotMethod != http.MethodPost {
			t.Errorf("method = %q, want POST", gotMethod)
		}
		if gotPath != "/v0/runs/"+runID.String()+"/revive" {
			t.Errorf("path = %q", gotPath)
		}
		if gotAuth != "Bearer tok-test" {
			t.Errorf("auth = %q, want Bearer tok-test", gotAuth)
		}
		if res.Run.State != "running" {
			t.Errorf("run state = %q, want running", res.Run.State)
		}
		if len(res.RestoredStages) != 1 {
			t.Fatalf("restored %d stages, want 1", len(res.RestoredStages))
		}
		rs := res.RestoredStages[0]
		if rs.Type != "implement" || rs.PriorCategory != "A" || rs.RestoredState != "pending" {
			t.Errorf("restored stage = %+v, want implement/A/pending", rs)
		}
		// A clean revive omits audit_warning: absence decodes to the zero value
		// (proof an old-shape body without the field still decodes, #1943).
		if res.AuditWarning != "" {
			t.Errorf("audit_warning = %q, want empty on a clean revive", res.AuditWarning)
		}
	})

	t.Run("200 audit_warning decodes into the result", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"run":{"id":"` + runID.String() + `","state":"running"},` +
				`"restored_stages":[],` +
				`"audit_warning":"run_revived audit append failed: audit store down — the revive is committed but no chained provenance record was written; see server logs"}`))
		}))
		defer ts.Close()
		c := newAPIClient(config{backendURL: ts.URL, apiToken: "tok-test"})

		res, err := c.ReviveRun(context.Background(), runID)
		if err != nil {
			t.Fatalf("ReviveRun: %v", err)
		}
		if res.Run.State != "running" {
			t.Errorf("run state = %q, want running (revive committed despite the audit warning)", res.Run.State)
		}
		if !strings.Contains(res.AuditWarning, "run_revived") {
			t.Errorf("audit_warning = %q, want it to name the run_revived append failure", res.AuditWarning)
		}
	})

	t.Run("422 revive_not_applicable surfaces as apiError", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"error":{"code":"revive_not_applicable","message":"run is not failed"}}`))
		}))
		defer ts.Close()
		c := newAPIClient(config{backendURL: ts.URL, apiToken: "tok-test"})

		_, err := c.ReviveRun(context.Background(), runID)
		if err == nil {
			t.Fatal("expected an error on 422")
		}
		var ae *apiError
		if !errors.As(err, &ae) || ae.StatusCode != http.StatusUnprocessableEntity || ae.Code != "revive_not_applicable" {
			t.Errorf("err = %v, want *apiError 422 revive_not_applicable", err)
		}
	})
}

// TestMergeRun_WireShape pins the apiClient.MergeRun request/response
// contract (E48.7 / #1954): the POST target + method + auth header + verdict
// body, the decoded 200 fields, the already_recorded idempotence signal, and
// the error envelope surfacing (502 merge_dispatch_failed → *apiError).
func TestMergeRun_WireShape(t *testing.T) {
	runID := uuid.New()

	t.Run("200 posts verdict body and decodes result", func(t *testing.T) {
		var gotMethod, gotPath, gotAuth string
		var gotBody mergeRunRequest
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"merge_queued":true,"verdict_sequence":42,` +
				`"pr_url":"https://github.com/x/y/pull/7","already_recorded":false}`))
		}))
		defer ts.Close()
		c := newAPIClient(config{backendURL: ts.URL, apiToken: "tok-test"})

		res, err := c.MergeRun(context.Background(), runID, "ship it")
		if err != nil {
			t.Fatalf("MergeRun: %v", err)
		}
		if gotMethod != http.MethodPost {
			t.Errorf("method = %q, want POST", gotMethod)
		}
		if gotPath != "/v0/runs/"+runID.String()+"/merge" {
			t.Errorf("path = %q", gotPath)
		}
		if gotAuth != "Bearer tok-test" {
			t.Errorf("auth = %q, want Bearer tok-test", gotAuth)
		}
		if gotBody.Verdict != "ship it" {
			t.Errorf("body verdict = %q, want 'ship it'", gotBody.Verdict)
		}
		if !res.MergeQueued || res.VerdictSequence != 42 || res.PRURL != "https://github.com/x/y/pull/7" || res.AlreadyRecorded {
			t.Errorf("result = %+v, want queued/seq42/pr/not-already-recorded", res)
		}
	})

	t.Run("200 already_recorded idempotence signal decodes", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"merge_queued":true,"verdict_sequence":42,"already_recorded":true}`))
		}))
		defer ts.Close()
		c := newAPIClient(config{backendURL: ts.URL, apiToken: "tok-test"})

		res, err := c.MergeRun(context.Background(), runID, "ship it")
		if err != nil {
			t.Fatalf("MergeRun: %v", err)
		}
		if !res.AlreadyRecorded || !res.MergeQueued {
			t.Errorf("result = %+v, want already_recorded + merge_queued (endpoint idempotence)", res)
		}
	})

	t.Run("502 merge_dispatch_failed surfaces as apiError", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":{"code":"merge_dispatch_failed","message":"verdict durable, queue retryable"}}`))
		}))
		defer ts.Close()
		c := newAPIClient(config{backendURL: ts.URL, apiToken: "tok-test"})

		_, err := c.MergeRun(context.Background(), runID, "ship it")
		if err == nil {
			t.Fatal("expected an error on 502")
		}
		var ae *apiError
		if !errors.As(err, &ae) || ae.StatusCode != http.StatusBadGateway || ae.Code != "merge_dispatch_failed" {
			t.Errorf("err = %v, want *apiError 502 merge_dispatch_failed", err)
		}
	})
}

// TestAutoDriveRunGate_DecodesDecisionRequired drives the REAL
// apiClient.AutoDriveRunGate JSON unmarshalling across an httptest server that
// returns a decision_required=true / decision_state=fixup_budget_exhausted body
// (the #2091 outcome), and asserts the decoded local AutoDriveOutcome carries
// DecisionRequired=true and DecisionState=fixup_budget_exhausted.
//
// This is the ONE test that catches a mistyped/wrong json tag on the client.go
// AutoDriveOutcome struct copy: a wrong `decision_required` tag would decode to
// the zero value (false) and silently route the driver to the observe-only
// default — every other planned test (which builds the AutoDriveOutcome value
// through the interface fake in drive_run_test.go) bypasses this wire decode and
// would still pass. Binding approval condition 1.
func TestAutoDriveRunGate_DecodesDecisionRequired(t *testing.T) {
	const body = `{
		"acted": false,
		"paged": false,
		"decision_required": true,
		"decision_state": "fixup_budget_exhausted",
		"action": "route_fixup",
		"note": "fixup budget exhausted"
	}`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/auto-drive") {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer ts.Close()
	c := newAPIClient(config{backendURL: ts.URL, apiToken: "tok-test"})

	out, err := c.AutoDriveRunGate(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("AutoDriveRunGate: %v", err)
	}
	if !out.DecisionRequired {
		t.Errorf("DecisionRequired = false, want true (a wrong decision_required json tag drops it to the observe-only default)")
	}
	if out.DecisionState != "fixup_budget_exhausted" {
		t.Errorf("DecisionState = %q, want fixup_budget_exhausted", out.DecisionState)
	}
	// Sanity: the sibling flags decode as sent — this is a decision-required
	// outcome, not an acted/paged one.
	if out.Acted || out.Paged {
		t.Errorf("outcome = %+v, want a non-acted, non-paged decision_required decode", out)
	}
	if out.Action != "route_fixup" {
		t.Errorf("Action = %q, want route_fixup", out.Action)
	}
}

// TestRunReviewAuthority_WireShape pins the hand-maintained MCP wire mirror for
// the run-status review_authority block (E53.2 / #2225): the backend tags MUST
// byte-match Run.ReviewAuthority / RunReviewAuthority or the slice silently
// decodes to nil (the #371-class trap). A populated body decodes as sent, and
// an old-backend body that omits the field decodes to nil (the mixed-version
// degrade).
func TestRunReviewAuthority_WireShape(t *testing.T) {
	runID := uuid.New()
	serveRun := func(t *testing.T, body string) *Run {
		t.Helper()
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}))
		defer ts.Close()
		c := newAPIClient(config{backendURL: ts.URL, apiToken: "tok-test"})
		r, err := c.GetRun(context.Background(), runID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		return r
	}
	runBody := func(ra string) string {
		base := `{"id":"` + runID.String() + `","repo":"x/y","workflow_id":"feature_change","state":"running"`
		if ra != "" {
			base += `,"review_authority":` + ra
		}
		return base + `}`
	}

	t.Run("populated body decodes", func(t *testing.T) {
		r := serveRun(t, runBody(`[{"stage":"plan","stage_type":"plan","authority":"gating","source":"declared"},{"stage":"implement","stage_type":"implement","authority":"advisory","source":"derived"}]`))
		if len(r.ReviewAuthority) != 2 {
			t.Fatalf("ReviewAuthority = %+v, want 2 entries; the review_authority json tag may not byte-match the backend", r.ReviewAuthority)
		}
		if got := r.ReviewAuthority[0]; got.Stage != "plan" || got.StageType != "plan" || got.Authority != "gating" || got.Source != "declared" {
			t.Errorf("ReviewAuthority[0] = %+v, want {plan plan gating declared}", got)
		}
		if got := r.ReviewAuthority[1]; got.Stage != "implement" || got.Authority != "advisory" || got.Source != "derived" {
			t.Errorf("ReviewAuthority[1] = %+v, want {implement advisory derived}", got)
		}
	})

	t.Run("old-backend body omits the field -> nil", func(t *testing.T) {
		r := serveRun(t, runBody(""))
		if r.ReviewAuthority != nil {
			t.Errorf("ReviewAuthority = %+v, want nil when the backend omits review_authority (the mixed-version degrade)", r.ReviewAuthority)
		}
	})
}

// TestRunLiveValidation_WireShape pins the hand-maintained MCP wire mirror for
// the run-status / gate-view live_validation block (#2045, E48.35): the backend
// tags MUST byte-match Run.LiveValidation / GateView.LiveValidation or the
// pointer silently decodes to nil (the #371-class trap). Each of the three
// backend-emitted shapes decodes as sent — a healthy linked walk, a
// filing-failure marker, and a stranded intent-only marker — and an old-backend
// body that omits the field decodes to a nil pointer (the mixed-version degrade).
func TestRunLiveValidation_WireShape(t *testing.T) {
	runID := uuid.New()

	// serveRun serves GET /v0/runs/{id} with the given body so GetRun's decode
	// path (not a bespoke json.Unmarshal) is what is under test.
	serveRun := func(t *testing.T, body string) *Run {
		t.Helper()
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}))
		defer ts.Close()
		c := newAPIClient(config{backendURL: ts.URL, apiToken: "tok-test"})
		r, err := c.GetRun(context.Background(), runID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		return r
	}
	// A minimal Run body carrying the given live_validation object literal (or
	// none when lv == "").
	runBody := func(lv string) string {
		base := `{"id":"` + runID.String() + `","repo":"x/y","workflow_id":"feature_change","state":"succeeded"`
		if lv != "" {
			base += `,"live_validation":` + lv
		}
		return base + `}`
	}

	t.Run("healthy linked walk decodes", func(t *testing.T) {
		r := serveRun(t, runBody(`{"pending_criteria_count":3,"walk_ref":"#123","filing_failed":false}`))
		if r.LiveValidation == nil {
			t.Fatal("LiveValidation is nil; the live_validation json tag does not byte-match the backend")
		}
		lv := r.LiveValidation
		if lv.PendingCriteriaCount != 3 || lv.WalkRef != "#123" || lv.FilingFailed || lv.FilingIncomplete {
			t.Errorf("LiveValidation = %+v, want {3 #123 false false}", *lv)
		}
	})

	t.Run("filing-failure marker decodes", func(t *testing.T) {
		r := serveRun(t, runBody(`{"pending_criteria_count":2,"filing_failed":true}`))
		if r.LiveValidation == nil {
			t.Fatal("LiveValidation is nil on a filing-failure body")
		}
		lv := r.LiveValidation
		if lv.PendingCriteriaCount != 2 || lv.WalkRef != "" || !lv.FilingFailed || lv.FilingIncomplete {
			t.Errorf("LiveValidation = %+v, want {2 \"\" true false}", *lv)
		}
	})

	t.Run("stranded intent-only marker decodes", func(t *testing.T) {
		r := serveRun(t, runBody(`{"pending_criteria_count":1,"filing_failed":true,"filing_incomplete":true}`))
		if r.LiveValidation == nil {
			t.Fatal("LiveValidation is nil on an intent-only body")
		}
		lv := r.LiveValidation
		if lv.PendingCriteriaCount != 1 || lv.WalkRef != "" || !lv.FilingFailed || !lv.FilingIncomplete {
			t.Errorf("LiveValidation = %+v, want {1 \"\" true true}", *lv)
		}
	})

	t.Run("old-backend body omits the field -> nil", func(t *testing.T) {
		r := serveRun(t, runBody(""))
		if r.LiveValidation != nil {
			t.Errorf("LiveValidation = %+v, want nil when the backend omits live_validation (the mixed-version degrade)", r.LiveValidation)
		}
	})

	t.Run("gate-view mirrors the same block", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"run_id":"` + runID.String() + `","open":[],"settled":[],"suppressed_relitigations":[],` +
				`"live_validation":{"pending_criteria_count":4,"walk_ref":"#77","filing_failed":false}}`))
		}))
		defer ts.Close()
		c := newAPIClient(config{backendURL: ts.URL, apiToken: "tok-test"})
		gv, err := c.GetGateView(context.Background(), runID, "")
		if err != nil {
			t.Fatalf("GetGateView: %v", err)
		}
		if gv.LiveValidation == nil {
			t.Fatal("GateView.LiveValidation is nil; the live_validation json tag does not byte-match the backend")
		}
		if gv.LiveValidation.PendingCriteriaCount != 4 || gv.LiveValidation.WalkRef != "#77" || gv.LiveValidation.FilingFailed {
			t.Errorf("GateView.LiveValidation = %+v, want {4 #77 false ...}", *gv.LiveValidation)
		}
	})

	t.Run("gate-view old-backend body omits the field -> nil", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"run_id":"` + runID.String() + `","open":[],"settled":[],"suppressed_relitigations":[]}`))
		}))
		defer ts.Close()
		c := newAPIClient(config{backendURL: ts.URL, apiToken: "tok-test"})
		gv, err := c.GetGateView(context.Background(), runID, "")
		if err != nil {
			t.Fatalf("GetGateView: %v", err)
		}
		if gv.LiveValidation != nil {
			t.Errorf("GateView.LiveValidation = %+v, want nil when the backend omits live_validation", gv.LiveValidation)
		}
	})
}

// --- W1: the WHOLE MCP wire, gh subprocess → request body (#2226) ---
//
// The gap the operator named: issue_fetch_test.go ends at the decode and the
// StartRun tests begin at an already-populated IssueContext, so the
// orchestration BETWEEN them could drop labels entirely while every unit test
// passes — and every legitimate label-constrained run would then fail closed.
// This drives a fake `gh issue view` through the real ghIssueCommand seam into
// fetchIssueViaGh, hands the result to StartRun, and asserts on the JSON body
// the backend actually RECEIVES.
func TestStartRun_W1_IssueLabels_ReachTheRequestBody(t *testing.T) {
	withFakeGh(t, `{"title":"Bump dep","body":"b","url":"https://github.com/x/y/issues/9","number":9,"labels":[{"name":"dependencies"},{"name":"area:backend"}]}`)

	ic, err := fetchIssueViaGh("x/y", 9)
	if err != nil {
		t.Fatalf("fetchIssueViaGh: %v", err)
	}

	var received map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &received); err != nil {
			t.Errorf("backend received a body that is not JSON: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"` + uuid.NewString() + `","repo":"x/y","workflow_id":"feature_change","state":"pending"}`))
	}))
	defer ts.Close()

	c := newAPIClient(config{backendURL: ts.URL, apiToken: "tok-test"})
	if _, _, err := c.StartRun(context.Background(), StartRunParams{
		Repo: "x/y", WorkflowID: "feature_change", WorkflowSHA: "abc",
		TriggerSource: "github_issue", IssueContext: ic,
	}); err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	got, _ := received["issue_context"].(map[string]any)
	if got == nil {
		t.Fatalf("request body carried no issue_context: %+v", received)
	}
	labels, _ := got["labels"].([]any)
	if len(labels) != 2 || labels[0] != "dependencies" || labels[1] != "area:backend" {
		t.Errorf("issue_context.labels on the wire = %+v, want the two fetched names — a dropped field here fails closed on EVERY labels-declaring workflow", got["labels"])
	}
}

// TestStartRun_W1_AppliesToOverride_ReachesTheRequestBody is the same
// serialization proof for the escape hatch: an override the client silently
// drops leaves the operator with a rejection and no way past it.
func TestStartRun_W1_AppliesToOverride_ReachesTheRequestBody(t *testing.T) {
	var received map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &received)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"` + uuid.NewString() + `","repo":"x/y","workflow_id":"feature_change","state":"pending"}`))
	}))
	defer ts.Close()

	c := newAPIClient(config{backendURL: ts.URL, apiToken: "tok-test"})
	if _, _, err := c.StartRun(context.Background(), StartRunParams{
		Repo: "x/y", WorkflowID: "feature_change", WorkflowSHA: "abc",
		TriggerSource:     "cli",
		AppliesToOverride: true, AppliesToOverrideReason: "one-off backport",
	}); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if received["applies_to_override"] != true {
		t.Errorf("applies_to_override on the wire = %v, want true", received["applies_to_override"])
	}
	if received["applies_to_override_reason"] != "one-off backport" {
		t.Errorf("applies_to_override_reason on the wire = %v, want the operator's reason", received["applies_to_override_reason"])
	}
}

// TestStartRun_W1_OverrideOmittedWhenUnset keeps the default request byte-
// identical to a pre-#2226 one: omitempty means an ordinary run sends neither
// key, so the backend's DisallowUnknownFields decoder and every recorded
// fixture stay unaffected.
func TestStartRun_W1_OverrideOmittedWhenUnset(t *testing.T) {
	var received map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &received)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"` + uuid.NewString() + `","repo":"x/y","workflow_id":"feature_change","state":"pending"}`))
	}))
	defer ts.Close()

	c := newAPIClient(config{backendURL: ts.URL, apiToken: "tok-test"})
	if _, _, err := c.StartRun(context.Background(), StartRunParams{
		Repo: "x/y", WorkflowID: "feature_change", WorkflowSHA: "abc", TriggerSource: "cli",
	}); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	for _, k := range []string{"applies_to_override", "applies_to_override_reason"} {
		if _, present := received[k]; present {
			t.Errorf("%s present on an ordinary create request; omitempty must keep it off the wire", k)
		}
	}
}

// TestArbitrateAcceptance_ClientWireShape pins the acceptance-arbitration
// request/response wire contract (E66.37 / #2474). A mistyped json tag on
// outcome_sequence would silently decode to 0, which is exactly the value that
// aliases "no outcome recorded" in the correlation rule — so this is pinned
// rather than assumed.
func TestArbitrateAcceptance_ClientWireShape(t *testing.T) {
	var gotPath string
	var received map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &received)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"run_id":"r","acceptance_gate_state":"acceptance_arbitrated","outcome_sequence":60,"arbitration_sequence":62,"already_recorded":true}`))
	}))
	defer ts.Close()

	runID := uuid.New()
	c := newAPIClient(config{backendURL: ts.URL, apiToken: "tok-test"})
	res, err := c.ArbitrateAcceptance(context.Background(), runID, "class-5 all-skip", true)
	if err != nil {
		t.Fatalf("ArbitrateAcceptance: %v", err)
	}
	if gotPath != "/v0/runs/"+runID.String()+"/acceptance-arbitration" {
		t.Errorf("path = %q", gotPath)
	}
	if received["reason"] != "class-5 all-skip" {
		t.Errorf("reason on the wire = %v", received["reason"])
	}
	if received["acknowledge_failed_criteria"] != true {
		t.Errorf("acknowledge_failed_criteria on the wire = %v, want true", received["acknowledge_failed_criteria"])
	}
	if res.AcceptanceGateState != "acceptance_arbitrated" {
		t.Errorf("acceptance_gate_state = %q", res.AcceptanceGateState)
	}
	if res.OutcomeSequence != 60 || res.ArbitrationSequence != 62 {
		t.Errorf("sequences = %d/%d, want 60/62", res.OutcomeSequence, res.ArbitrationSequence)
	}
	if !res.AlreadyRecorded {
		t.Error("already_recorded = false, want true")
	}
}

// TestGetRunStageWait_PathQueryAndEnvelope pins the fishhawk_await_stage api
// client read (#2491): the GET path is /v0/runs/{run}/stages/{stage}; a
// positive waitSeconds appends ?wait=<n> and is CLAMPED to
// awaitStagePerCallWaitSeconds; a non-positive value omits the query; and the
// top-level {state, terminal, failure_*} envelope decodes into RunStageWait.
func TestGetRunStageWait_PathQueryAndEnvelope(t *testing.T) {
	runID := uuid.New()
	stageID := uuid.New()

	t.Run("wait forwarded and envelope decoded", func(t *testing.T) {
		var gotMethod, gotPath, gotQuery string
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"` + stageID.String() + `","run_id":"` + runID.String() +
				`","type":"implement","state":"failed","terminal":true,"failure_category":"category-a","failure_reason":"boom"}`))
		}))
		defer ts.Close()
		c := newAPIClient(config{backendURL: ts.URL, apiToken: "tok-test"})

		got, err := c.GetRunStageWait(context.Background(), runID, stageID, 10)
		if err != nil {
			t.Fatalf("GetRunStageWait: %v", err)
		}
		if gotMethod != http.MethodGet {
			t.Errorf("method = %q, want GET", gotMethod)
		}
		if gotPath != "/v0/runs/"+runID.String()+"/stages/"+stageID.String() {
			t.Errorf("path = %q", gotPath)
		}
		if gotQuery != "wait=10" {
			t.Errorf("query = %q, want wait=10", gotQuery)
		}
		if got.State != "failed" || !got.Terminal {
			t.Errorf("state/terminal = %q/%v, want failed/true", got.State, got.Terminal)
		}
		if got.Type != "implement" {
			t.Errorf("type = %q, want implement", got.Type)
		}
		if got.FailureCategory == nil || *got.FailureCategory != "category-a" {
			t.Errorf("failure_category = %v, want category-a", got.FailureCategory)
		}
		if got.FailureReason == nil || *got.FailureReason != "boom" {
			t.Errorf("failure_reason = %v, want boom", got.FailureReason)
		}
	})

	t.Run("wait clamped to the per-call ceiling", func(t *testing.T) {
		var gotQuery string
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotQuery = r.URL.RawQuery
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"state":"running","terminal":false}`))
		}))
		defer ts.Close()
		c := newAPIClient(config{backendURL: ts.URL, apiToken: "tok-test"})

		if _, err := c.GetRunStageWait(context.Background(), runID, stageID, 999); err != nil {
			t.Fatalf("GetRunStageWait: %v", err)
		}
		want := "wait=" + strconv.Itoa(awaitStagePerCallWaitSeconds)
		if gotQuery != want {
			t.Errorf("query = %q, want %q (clamped to the per-call ceiling)", gotQuery, want)
		}
	})

	t.Run("non-positive wait omits the query", func(t *testing.T) {
		var gotQuery string
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotQuery = r.URL.RawQuery
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"state":"succeeded","terminal":true}`))
		}))
		defer ts.Close()
		c := newAPIClient(config{backendURL: ts.URL, apiToken: "tok-test"})

		if _, err := c.GetRunStageWait(context.Background(), runID, stageID, 0); err != nil {
			t.Fatalf("GetRunStageWait: %v", err)
		}
		if gotQuery != "" {
			t.Errorf("query = %q, want empty (no ?wait for a single read)", gotQuery)
		}
	})

	t.Run("http error surfaces as apiError", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"code":"internal_error","message":"boom"}}`))
		}))
		defer ts.Close()
		c := newAPIClient(config{backendURL: ts.URL, apiToken: "tok-test"})

		if _, err := c.GetRunStageWait(context.Background(), runID, stageID, 0); err == nil {
			t.Fatal("expected an error on a 500 response")
		}
	})
}

// TestSubmitApproval_SendsSliceAddScopeFilesBody pins the apiClient half of the
// #2515 wire contract directly against a real HTTP server: SubmitApproval must
// POST add_scope_files_to_slice to /v0/stages/{id}/approvals with keys and
// paths byte-identical to what the caller passed (the backend's
// DisallowUnknownFields decoder rejects any drift in the field name), and must
// OMIT the field entirely when the caller passes nil so a flat-plan approve
// body is unchanged by this change.
func TestSubmitApproval_SendsSliceAddScopeFilesBody(t *testing.T) {
	stageID := uuid.New()
	perSlice := map[string][]string{"tools slice": {"backend/internal/mcpserver/tools.go"}}

	var gotMethod, gotPath string
	var gotRaw map[string]any
	c := releaseTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotRaw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"`+uuid.NewString()+`","state":"succeeded"}`)
	})

	if _, err := c.SubmitApproval(context.Background(), stageID, "approve", "restore the dropped file",
		"kuhlman-labs", nil, nil, perSlice, nil, nil, nil, ""); err != nil {
		t.Fatalf("SubmitApproval: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v0/stages/"+stageID.String()+"/approvals" {
		t.Errorf("request = %s %s, want POST /v0/stages/%s/approvals", gotMethod, gotPath, stageID)
	}
	raw, err := json.Marshal(gotRaw["add_scope_files_to_slice"])
	if err != nil {
		t.Fatalf("marshal decoded field: %v", err)
	}
	if want := `{"tools slice":["backend/internal/mcpserver/tools.go"]}`; string(raw) != want {
		t.Errorf("add_scope_files_to_slice on the wire = %s, want %s", raw, want)
	}

	// nil → the key must be absent, not present-and-null.
	gotRaw = nil
	if _, err := c.SubmitApproval(context.Background(), stageID, "approve", "plain approve",
		"kuhlman-labs", nil, nil, nil, nil, nil, nil, ""); err != nil {
		t.Fatalf("SubmitApproval (no slice map): %v", err)
	}
	if _, present := gotRaw["add_scope_files_to_slice"]; present {
		t.Errorf("add_scope_files_to_slice present on a no-targeting approve body: %#v", gotRaw)
	}
}

// TestSubmitApproval_SendsAmendAcceptanceCriteriaBody pins the apiClient half of
// the #2581 wire contract against a real HTTP server: SubmitApproval must POST
// amend_acceptance_criteria with the id/action/reason/statement tags byte-
// identical to what the caller passed (the backend's DisallowUnknownFields
// decoder rejects any drift in a field name), and must OMIT the key entirely
// when the caller passes nil so an amendment-less approve body is unchanged.
func TestSubmitApproval_SendsAmendAcceptanceCriteriaBody(t *testing.T) {
	stageID := uuid.New()
	amendments := []AcceptanceCriteriaAmendment{
		{ID: "crit-2", Action: "retire", Reason: "condition 1 dropped the surface it asserts"},
		{ID: "crit-3", Action: "restate", Reason: "narrowed at the gate", Statement: "the gate refuses instead of warning"},
	}

	var gotRaw map[string]any
	c := releaseTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotRaw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"`+uuid.NewString()+`","state":"succeeded"}`)
	})

	if _, err := c.SubmitApproval(context.Background(), stageID, "approve", "narrowed the design",
		"kuhlman-labs", nil, nil, nil, nil, nil, amendments, ""); err != nil {
		t.Fatalf("SubmitApproval: %v", err)
	}
	raw, err := json.Marshal(gotRaw["amend_acceptance_criteria"])
	if err != nil {
		t.Fatalf("marshal decoded field: %v", err)
	}
	// The handler decodes into map[string]any, so re-marshalling sorts keys —
	// the assertion is on the field NAMES and values, not their emitted order.
	want := `[{"action":"retire","id":"crit-2","reason":"condition 1 dropped the surface it asserts"},` +
		`{"action":"restate","id":"crit-3","reason":"narrowed at the gate","statement":"the gate refuses instead of warning"}]`
	if string(raw) != want {
		t.Errorf("amend_acceptance_criteria on the wire = %s, want %s", raw, want)
	}

	// nil → the key must be absent, not present-and-null.
	gotRaw = nil
	if _, err := c.SubmitApproval(context.Background(), stageID, "approve", "plain approve",
		"kuhlman-labs", nil, nil, nil, nil, nil, nil, ""); err != nil {
		t.Fatalf("SubmitApproval (no amendments): %v", err)
	}
	if _, present := gotRaw["amend_acceptance_criteria"]; present {
		t.Errorf("amend_acceptance_criteria present on an amendment-less approve body: %#v", gotRaw)
	}
}
