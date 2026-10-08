package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/account"
)

// fakeScheduleSource is a fixed ScheduleSource. scanned is the ok it returns;
// calls counts reads so a test can prove the source was never consulted.
type fakeScheduleSource struct {
	snap    ScheduleSnapshot
	scanned bool
	calls   int
}

func (f *fakeScheduleSource) ScheduleSnapshot(string) (ScheduleSnapshot, bool) {
	f.calls++
	return f.snap, f.scanned
}

// schedulesGET calls the handler directly as memberIdentity, a cookie session
// the repo-visibility filter applies to.
func schedulesGET(t *testing.T, s *Server, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v0/schedules"+query, nil)
	rec := httptest.NewRecorder()
	s.handleGetSchedules(rec, withIdentity(req, memberIdentity()))
	return rec
}

func decodeSchedules(t *testing.T, rec *httptest.ResponseRecorder) schedulesResponse {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	var out schedulesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v\n%s", err, rec.Body.String())
	}
	return out
}

// visibleSchedulesServer is a server whose visibility mirror shows acme/app
// (and nothing else) to memberIdentity.
func visibleSchedulesServer(src ScheduleSource) *Server {
	cfg := Config{
		Addr:           "127.0.0.1:0",
		AccountRoles:   fakeAccountRoles{role: account.RoleMember},
		RepoVisibility: newFakeRepoVisibility(map[string]bool{"acme/app": true}),
	}
	if src != nil {
		cfg.Schedules = src
	}
	return New(cfg)
}

// TestGetSchedules_NilSourceIsDisabled200: an unconfigured deployment answers
// 200 enabled:false naming the switch — not 501 — with an EMPTY (not null)
// schedules array.
func TestGetSchedules_NilSourceIsDisabled200(t *testing.T) {
	rec := schedulesGET(t, visibleSchedulesServer(nil), "?repo=acme/app")
	out := decodeSchedules(t, rec)
	if out.Enabled {
		t.Error("enabled = true, want false with no ScheduleSource wired")
	}
	if out.Reason != schedulerDisabledReason || !strings.Contains(out.Reason, "--enable-scheduler") {
		t.Errorf("reason = %q, want the disabled reason naming --enable-scheduler", out.Reason)
	}
	if out.Repo != "acme/app" {
		t.Errorf("repo = %q, want acme/app", out.Repo)
	}
	if !strings.Contains(rec.Body.String(), `"schedules":[]`) {
		t.Errorf("body = %s, want an empty schedules array, never null", rec.Body.String())
	}
}

// TestGetSchedules_PopulatedSnapshotRenders: the window, next due time and last
// outcome reach the wire in UTC, schedules sort by workflow_id, an empty
// timezone renders as the grammar's UTC default, and zero times render null.
func TestGetSchedules_PopulatedSnapshotRenders(t *testing.T) {
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	tick := time.Date(2026, 10, 2, 14, 30, 0, 0, time.UTC)
	window := time.Date(2026, 10, 2, 9, 0, 0, 0, chicago)
	next := time.Date(2026, 10, 3, 9, 0, 0, 0, chicago)
	src := &fakeScheduleSource{scanned: true, snap: ScheduleSnapshot{
		LastTickAt: tick,
		RunnerKind: "github_actions",
		Schedules: []ScheduleEntry{
			{WorkflowID: "zeta", Cron: "*/15 * * * *"}, // no fire yet, no next, no outcome
			{WorkflowID: "backlog_grooming", Cron: "0 9 * * *", Timezone: "America/Chicago", Issue: 3112,
				CurrentWindowStart: window, NextDueAt: next,
				LastOutcome: &ScheduleOutcome{Kind: ScheduleOutcomeKindRefused, WindowStart: window,
					Code: "budget_exhausted", Message: "monthly budget exhausted", At: tick}},
		},
	}}
	out := decodeSchedules(t, schedulesGET(t, visibleSchedulesServer(src), "?repo=acme/app"))

	if !out.Enabled || !out.RepoScanned || out.Reason != "" {
		t.Errorf("enabled/repo_scanned/reason = %v/%v/%q, want true/true/empty", out.Enabled, out.RepoScanned, out.Reason)
	}
	if out.LastTickAt == nil || !out.LastTickAt.Equal(tick) {
		t.Errorf("last_tick_at = %v, want %v", out.LastTickAt, tick)
	}
	if out.DispatchNote != "" {
		t.Errorf("dispatch_note = %q, want absent for github_actions", out.DispatchNote)
	}
	if len(out.Schedules) != 2 {
		t.Fatalf("schedules = %d, want 2: %+v", len(out.Schedules), out.Schedules)
	}
	g, z := out.Schedules[0], out.Schedules[1]
	if g.WorkflowID != "backlog_grooming" || z.WorkflowID != "zeta" {
		t.Fatalf("order = %q,%q, want sorted by workflow_id", g.WorkflowID, z.WorkflowID)
	}
	if g.Timezone != "America/Chicago" || g.Issue != 3112 || g.Cron != "0 9 * * *" {
		t.Errorf("grooming entry = %+v", g)
	}
	if g.CurrentWindowStart == nil || g.CurrentWindowStart.Location() != time.UTC || !g.CurrentWindowStart.Equal(window) {
		t.Errorf("current_window_start = %v, want %v in UTC", g.CurrentWindowStart, window.UTC())
	}
	if g.NextDueAt == nil || !g.NextDueAt.Equal(next) {
		t.Errorf("next_due_at = %v, want %v", g.NextDueAt, next)
	}
	lo := g.LastOutcome
	if lo == nil || lo.Kind != "refused" || lo.Code != "budget_exhausted" || lo.Message != "monthly budget exhausted" ||
		!lo.WindowStart.Equal(window) || !lo.At.Equal(tick) || lo.RunID != "" {
		t.Errorf("last_outcome = %+v, want the refusal carried verbatim", lo)
	}
	if z.Timezone != "UTC" {
		t.Errorf("empty timezone rendered %q, want the UTC default", z.Timezone)
	}
	if z.CurrentWindowStart != nil || z.NextDueAt != nil || z.LastOutcome != nil {
		t.Errorf("zero-valued entry = %+v, want null window/next/outcome", z)
	}
}

// TestGetSchedules_LocalRunnerCarriesDispatchNote: a local scheduler states
// that a started run parks at awaiting_host_dispatch and is not auto-dispatched.
func TestGetSchedules_LocalRunnerCarriesDispatchNote(t *testing.T) {
	src := &fakeScheduleSource{scanned: true, snap: ScheduleSnapshot{RunnerKind: "local"}}
	out := decodeSchedules(t, schedulesGET(t, visibleSchedulesServer(src), "?repo=acme/app"))
	if out.RunnerKind != "local" {
		t.Errorf("runner_kind = %q, want local", out.RunnerKind)
	}
	if !strings.Contains(out.DispatchNote, "awaiting_host_dispatch") || !strings.Contains(out.DispatchNote, "does not auto-dispatch") {
		t.Errorf("dispatch_note = %q, want the awaiting_host_dispatch / no auto-dispatch note", out.DispatchNote)
	}
	if out.LastTickAt != nil {
		t.Errorf("last_tick_at = %v, want null before the first tick", out.LastTickAt)
	}
}

// TestGetSchedules_RepoNotScanned: a repo outside --scheduler-repos reports
// repo_scanned:false with its reason and NO schedules, even when the source's
// snapshot carries some — per-repo fields are ignored when ok is false.
func TestGetSchedules_RepoNotScanned(t *testing.T) {
	src := &fakeScheduleSource{scanned: false, snap: ScheduleSnapshot{
		RunnerKind: "local", SpecError: "stale", LastTickAt: time.Now(),
		Schedules: []ScheduleEntry{{WorkflowID: "w", Cron: "* * * * *"}},
	}}
	out := decodeSchedules(t, schedulesGET(t, visibleSchedulesServer(src), "?repo=acme/app"))
	if !out.Enabled || out.RepoScanned {
		t.Errorf("enabled/repo_scanned = %v/%v, want true/false", out.Enabled, out.RepoScanned)
	}
	if out.Reason != schedulerRepoNotScannedReason {
		t.Errorf("reason = %q, want the not-scanned reason", out.Reason)
	}
	if len(out.Schedules) != 0 || out.SpecError != "" || out.LastTickAt != nil {
		t.Errorf("unscanned repo leaked per-repo state: %+v", out)
	}
	if out.DispatchNote == "" {
		t.Error("dispatch_note absent, want the deployment-level local note even for an unscanned repo")
	}
}

// TestGetSchedules_SpecErrorReported: a spec fetch/parse failure is reported.
func TestGetSchedules_SpecErrorReported(t *testing.T) {
	src := &fakeScheduleSource{scanned: true, snap: ScheduleSnapshot{SpecError: "fetch spec: 404"}}
	out := decodeSchedules(t, schedulesGET(t, visibleSchedulesServer(src), "?repo=acme/app"))
	if out.SpecError != "fetch spec: 404" {
		t.Errorf("spec_error = %q, want it carried verbatim", out.SpecError)
	}
}

// TestGetSchedules_RepoParamRequired: a missing or malformed repo is a 400
// naming the field, and the source is never consulted.
func TestGetSchedules_RepoParamRequired(t *testing.T) {
	for _, q := range []string{"", "?repo=", "?repo=acme", "?repo=/app", "?repo=acme/", "?repo=a/b/c"} {
		t.Run(q, func(t *testing.T) {
			src := &fakeScheduleSource{scanned: true}
			rec := schedulesGET(t, visibleSchedulesServer(src), q)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body %s", rec.Code, rec.Body.String())
			}
			assertErrorCode(t, rec, "validation_failed")
			assertErrorField(t, rec, "repo")
			if src.calls != 0 {
				t.Errorf("source consulted %d times on an invalid repo, want 0", src.calls)
			}
		})
	}
}

// TestGetSchedules_InvisibleRepoIs403 is the visibility control. Both arms run:
// with a populated source (the source would answer 200 with schedules if
// reached) and with NO source — the ordering arm of operator condition 4: the
// nil-source enabled:false 200 must not be reachable for a repo the caller
// cannot read, so a deployment cannot be probed about it.
func TestGetSchedules_InvisibleRepoIs403(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  *fakeScheduleSource
	}{
		{"populated source", &fakeScheduleSource{scanned: true, snap: ScheduleSnapshot{
			Schedules: []ScheduleEntry{{WorkflowID: "w", Cron: "* * * * *"}}}}},
		{"nil source", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var src ScheduleSource
			if tc.src != nil {
				src = tc.src
			}
			rec := schedulesGET(t, visibleSchedulesServer(src), "?repo=other/secret")
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body %s", rec.Code, rec.Body.String())
			}
			assertErrorCode(t, rec, "repo_forbidden")
			if tc.src != nil && tc.src.calls != 0 {
				t.Errorf("source consulted %d times for an invisible repo, want 0", tc.src.calls)
			}
		})
	}
}

// TestGetSchedules_AnonymousIs401: the read is authenticated.
func TestGetSchedules_AnonymousIs401(t *testing.T) {
	s := visibleSchedulesServer(nil)
	rec := httptest.NewRecorder()
	s.handleGetSchedules(rec, httptest.NewRequest(http.MethodGet, "/v0/schedules?repo=acme/app", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body %s", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec, "authentication_required")
}

// TestSchedulesRoute_Registered: the route is on the mux (an anonymous call
// reaches the handler's auth gate — 401 — rather than 404/405).
func TestSchedulesRoute_Registered(t *testing.T) {
	h := New(Config{Addr: "127.0.0.1:0"}).Handler()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v0/schedules?repo=o/r", nil))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("GET /v0/schedules = %d, want 401 (route registered, auth gate reached)", w.Code)
	}
}

// TestOpenAPI_SchedulesRouteDocumented: the route, its response fields and the
// reserved `scheduled` and `alert` trigger sources are in the OpenAPI source of
// truth, as is the alert ingress (E35.4 / #1601) that mints `alert`: its route,
// request/response schemas, the default-off auto-start, and every 401 code.
func TestOpenAPI_SchedulesRouteDocumented(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "api", "v0.openapi.yaml"))
	if err != nil {
		t.Fatalf("read openapi: %v", err)
	}
	doc := string(raw)
	for _, want := range []string{
		"\n  /v0/schedules:\n", "operationId: listSchedules", "ScheduleList:", "repo_scanned:",
		"current_window_start:", "next_due_at:", "last_outcome:", "dispatch_note:",
		"enum: [github_issue, cli, ui, on_demand, scheduled, alert]", "trigger_source_reserved",
		"\n  /v0/triggers/alert:\n", "operationId: receiveAlertTrigger", "\n    AlertTrigger:\n",
		"\n    AlertTriggerResult:\n", "enum: [disabled, started, already_started, refused, error]",
		"alert_signature_missing", "alert_timestamp_invalid", "alert_signature_invalid",
		"alert_replayed", "alert_trigger_unconfigured", "alert_store_unconfigured",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("docs/api/v0.openapi.yaml is missing %q", strings.TrimSpace(want))
		}
	}
}
