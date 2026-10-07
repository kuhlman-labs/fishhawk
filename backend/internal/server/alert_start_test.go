package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/account"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

const (
	alertTestKey   = "alert:4242:deadbeef"
	alertTestIssue = 4242
)

// alertSpec is scheduledSpec's v2 spec under the workflow id hotfix_change
// (the alert source's default workflow), with the caller's applies_to trigger
// list.
func alertSpec(triggers ...string) string {
	return strings.Replace(scheduledSpec(triggers...), "  upkeep:\n", "  hotfix_change:\n", 1)
}

func alertParams(specYAML string) AlertRunParams {
	return AlertRunParams{
		Repo:           "kuhlman-labs/fishhawk",
		WorkflowID:     "hotfix_change",
		WorkflowSHA:    "blobsha",
		WorkflowSpec:   []byte(specYAML),
		IssueNumber:    alertTestIssue,
		IdempotencyKey: alertTestKey,
	}
}

// alertReservedBody is a FULLY VALID create body naming trigger_source=alert,
// anchored on an issue and carrying the same spec StartAlertRun is admitted
// with, so the unexported marker is the only difference between it and the
// in-process call.
func alertReservedBody(t *testing.T) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"repo":           "kuhlman-labs/fishhawk",
		"workflow_id":    "hotfix_change",
		"workflow_sha":   "blobsha",
		"trigger_source": "alert",
		"trigger_ref":    "issue:4242",
		"workflow_spec":  alertSpec("diff"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func assertAlertReserved(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 trigger_source_reserved:\n%s", w.Code, w.Body.String())
	}
	env := decodeErrorEnvelope(t, w)
	if env.Code != "trigger_source_reserved" {
		t.Errorf("error code = %q, want trigger_source_reserved (NOT the generic validation_failed):\n%s", env.Code, w.Body.String())
	}
	if !strings.Contains(env.Message, "alert ingress") || !strings.Contains(env.Message, "auto_start") {
		t.Errorf("message %q must name the alert ingress and the per-source auto_start it is reserved to", env.Message)
	}
}

// TestCreateRun_AlertReservedOverHTTP is the alert reservation guard through
// the REAL mux: an ordinary POST /v0/runs carrying a bearer token with
// write:runs and a fully valid body naming trigger_source=alert is refused 400
// trigger_source_reserved — the dedicated code — and mints no run. The
// positive control on the SAME server and spec (StartAlertRun → started)
// proves the fixture is otherwise admissible, isolating the marker as the
// control.
//
// Counterfactual: delete the alert arm of the guard. `alert` is not in
// ValidTriggerSources, so the request then falls to the membership check and
// answers validation_failed — RED on the CODE assertion.
func TestCreateRun_AlertReservedOverHTTP(t *testing.T) {
	runs := newFakeRepo()
	tokens := newFakeTokenRepo()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: runs, APITokenRepo: tokens,
		AccountRoles: fakeAccountRoles{role: account.RoleAdmin}})
	tok, err := tokens.Issue(context.Background(), "github:42", []string{"write:runs"})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v0/runs", bytes.NewReader(alertReservedBody(t)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok.PlainText)
	req.Header.Set("Idempotency-Key", alertTestKey)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	assertAlertReserved(t, w)
	if n := runRowCount(runs); n != 0 {
		t.Fatalf("refused POST minted %d runs, want 0", n)
	}

	// Positive control: the in-process call with the same spec is admitted.
	out, err := s.StartAlertRun(context.Background(), alertParams(alertSpec("diff")))
	if err != nil || out.Kind != AlertStartStarted {
		t.Fatalf("positive control: outcome = %+v, err = %v; want started", out, err)
	}
}

// TestCreateRun_AlertReservedForCookieOperator: the cookie-session operator,
// who bypasses scope enforcement, is refused the reserved source too.
func TestCreateRun_AlertReservedForCookieOperator(t *testing.T) {
	runs := newFakeRepo()
	s := newServer(t, runs)
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", bytes.NewReader(alertReservedBody(t)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleCreateRun(w, withAuth(req))

	assertAlertReserved(t, w)
	if n := runRowCount(runs); n != 0 {
		t.Errorf("refused request minted %d runs, want 0", n)
	}
}

// TestCreateRun_SchedulerMarkerDoesNotAdmitAlert pins that each marker admits
// ONLY its own source: a request carrying the scheduler's marker but naming
// trigger_source=alert is still refused trigger_source_reserved.
//
// Counterfactual: make the alert arm accept either marker
// (`isAlertAdmission(ctx) || isScheduledAdmission(ctx)`) and this request is
// admitted 201 — RED.
func TestCreateRun_SchedulerMarkerDoesNotAdmitAlert(t *testing.T) {
	runs := newFakeRepo()
	s := newServer(t, runs)
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", bytes.NewReader(alertReservedBody(t)))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(withScheduledAdmission(req.Context()))
	w := httptest.NewRecorder()
	s.handleCreateRun(w, withAuth(req))

	assertAlertReserved(t, w)
	if n := runRowCount(runs); n != 0 {
		t.Errorf("refused request minted %d runs, want 0", n)
	}
}

// TestCreateRun_AlertReservedBeforeFieldValidation pins the guard's ORDERING:
// it fires FIRST in body validation, so an alert request missing every other
// required field still answers trigger_source_reserved.
func TestCreateRun_AlertReservedBeforeFieldValidation(t *testing.T) {
	s := newServer(t, newFakeRepo())
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader(`{"trigger_source":"alert"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleCreateRun(w, withAuth(req))
	assertAlertReserved(t, w)
}

// TestStartAlertRun_StartsThenReplaysAlreadyStarted is the auto-start
// producer's headline: the in-process call is ADMITTED past the reservation
// guard and the ValidTriggerSources membership check, and mints a run stamped
// trigger_source=alert, trigger_ref issue:N and the caller's Idempotency-Key;
// the SAME key again replays → already_started with the same run id.
//
// Counterfactuals: (i) make isAlertAdmission return false → the guard refuses
// trigger_source_reserved → RED on Kind; (ii) drop the `!reservedAdmitted`
// exemption from the membership check → refused validation_failed → RED.
func TestStartAlertRun_StartsThenReplaysAlreadyStarted(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)

	first, err := s.StartAlertRun(context.Background(), alertParams(alertSpec("diff")))
	if err != nil {
		t.Fatalf("StartAlertRun: %v", err)
	}
	if first.Kind != AlertStartStarted || first.Status != http.StatusCreated || first.RunID == uuid.Nil {
		t.Fatalf("first outcome = %+v, want started/201 with a run id", first)
	}
	r := onlyRun(t, repo)
	if r.ID != first.RunID {
		t.Errorf("outcome run id %s != persisted run %s", first.RunID, r.ID)
	}
	if r.TriggerSource != run.TriggerAlert {
		t.Errorf("TriggerSource = %q, want alert", r.TriggerSource)
	}
	if r.TriggerRef == nil || *r.TriggerRef != "issue:4242" {
		t.Errorf("TriggerRef = %v, want issue:4242", r.TriggerRef)
	}
	if r.IdempotencyKey == nil || *r.IdempotencyKey != alertTestKey {
		t.Errorf("IdempotencyKey = %v, want %q", r.IdempotencyKey, alertTestKey)
	}
	if r.IssueContext != nil {
		t.Errorf("IssueContext = %+v, want nil with no GitHub client wired", r.IssueContext)
	}

	second, err := s.StartAlertRun(context.Background(), alertParams(alertSpec("diff")))
	if err != nil {
		t.Fatalf("StartAlertRun (replay): %v", err)
	}
	if second.Kind != AlertStartAlreadyStarted || second.Status != http.StatusOK || second.RunID != first.RunID {
		t.Fatalf("replay outcome = %+v, want already_started/200 for run %s", second, first.RunID)
	}
	_ = onlyRun(t, repo)
}

// TestStartAlertRun_IssueAnchorHydratesIssueContext: with a GitHub client
// wired, the filed incident issue is hydrated onto the run row.
func TestStartAlertRun_IssueAnchorHydratesIssueContext(t *testing.T) {
	repo := newFakeRepo()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, GitHub: fakeScheduledGitHub(t, http.StatusOK)})

	p := alertParams(alertSpec("diff"))
	p.IssueNumber = 3112
	out, err := s.StartAlertRun(context.Background(), p)
	if err != nil || out.Kind != AlertStartStarted {
		t.Fatalf("outcome = %+v, err = %v; want started", out, err)
	}
	r := onlyRun(t, repo)
	if r.IssueContext == nil || r.IssueContext.Number != 3112 || r.IssueContext.Title != "Groom the backlog" {
		t.Errorf("IssueContext = %+v, want the hydrated issue 3112", r.IssueContext)
	}
}

// TestStartAlertRun_AppliesToTriggerFormIsDiff pins the trigger-form contract
// the E35.5 hotfix_change preset (#1602) must match: an alert run is routed as
// spec.TriggerDiff, so a workflow declaring applies_to.trigger [diff] admits
// it and one declaring only [scheduled, on_demand] refuses it 422
// workflow_not_applicable, minting nothing.
func TestStartAlertRun_AppliesToTriggerFormIsDiff(t *testing.T) {
	repo := newFakeRepo()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, AuditRepo: newAuditFake()})

	out, err := s.StartAlertRun(context.Background(), alertParams(alertSpec("scheduled", "on_demand")))
	if err != nil {
		t.Fatalf("StartAlertRun: %v", err)
	}
	if out.Kind != AlertStartRefused || out.Status != http.StatusUnprocessableEntity || out.Code != "workflow_not_applicable" {
		t.Fatalf("outcome = %+v, want refused/422 workflow_not_applicable", out)
	}
	if n := runRowCount(repo); n != 0 {
		t.Errorf("refused start minted %d runs, want 0", n)
	}

	ok, err := s.StartAlertRun(context.Background(), alertParams(alertSpec("diff")))
	if err != nil || ok.Kind != AlertStartStarted {
		t.Fatalf("[diff] outcome = %+v, err = %v; want started", ok, err)
	}
}

// TestStartAlertRun_WorkflowAbsentFromSpecRefused is the
// hotfix_change-not-yet-shipped case: the fetched spec has no workflow of the
// source's id, so the handler refuses with its own 4xx code and nothing is
// minted.
func TestStartAlertRun_WorkflowAbsentFromSpecRefused(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)

	out, err := s.StartAlertRun(context.Background(), alertParams(scheduledSpec("diff")))
	if err != nil {
		t.Fatalf("StartAlertRun: %v", err)
	}
	if out.Kind != AlertStartRefused || out.Status < 400 || out.Status >= 500 || out.Code == "" {
		t.Fatalf("outcome = %+v, want a refused 4xx carrying the handler's code", out)
	}
	if n := runRowCount(repo); n != 0 {
		t.Errorf("refused start minted %d runs, want 0", n)
	}
}

// TestStartAlertRun_NonPositiveIssueIsError: an alert run is always anchored,
// so a missing issue number is an error before the handler is driven.
//
// Counterfactual: delete the guard; the run is minted un-anchored (201) and
// the error assertion goes RED.
func TestStartAlertRun_NonPositiveIssueIsError(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)
	p := alertParams(alertSpec("diff"))
	p.IssueNumber = 0
	out, err := s.StartAlertRun(context.Background(), p)
	if err == nil {
		t.Fatalf("StartAlertRun with issue 0 = %+v, nil; want an error", out)
	}
	if n := runRowCount(repo); n != 0 {
		t.Errorf("un-anchored start minted %d runs, want 0", n)
	}
}

// TestStartAlertRun_5xxIsError: a server-side failure is returned as an
// ERROR, never a refusal or a start.
func TestStartAlertRun_5xxIsError(t *testing.T) {
	repo := newFakeRepo()
	repo.createErr = errors.New("disk full")
	s := newServer(t, repo)

	out, err := s.StartAlertRun(context.Background(), alertParams(alertSpec("diff")))
	if err == nil {
		t.Fatalf("StartAlertRun returned nil error on a 500 (outcome %+v)", out)
	}
	if out.Kind != "" || out.Status != http.StatusInternalServerError {
		t.Errorf("outcome = %+v, want no kind and status 500", out)
	}
}

// TestClassifyAlertStart_MalformedAnswersAreErrors pins the fail-closed arms:
// a 2xx with no decodable run, a 4xx with no envelope code, and an
// unrecognised status are all ERRORS.
func TestClassifyAlertStart_MalformedAnswersAreErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"201 empty body", http.StatusCreated, ``},
		{"200 run with nil id", http.StatusOK, `{"id":"00000000-0000-0000-0000-000000000000"}`},
		{"400 no envelope", http.StatusBadRequest, `not json`},
		{"409 envelope without code", http.StatusConflict, `{"error":{"message":"m"}}`},
		{"302 unrecognised", http.StatusFound, ``},
		{"503", http.StatusServiceUnavailable, `{"error":{"code":"run_repo_unconfigured","message":"m"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := classifyAlertStart(tc.status, []byte(tc.body), "hotfix_change")
			if err == nil {
				t.Errorf("classifyAlertStart(%d, %q) = %+v, nil; want an error", tc.status, tc.body, out)
			}
			if out.Kind != "" {
				t.Errorf("Kind = %q, want empty on an error", out.Kind)
			}
		})
	}
}

// TestClassifyAlertStart_ReplayOccupancy: only an ALERT run of the SAME
// workflow is already_started; any other occupant of the key is refused 409
// alert_key_occupied carrying the occupant's id.
//
// Counterfactuals: (i) make the occupancy check permissive → every refused row
// reads already_started — RED; (ii) drop ONLY the workflow_id disjunct → the
// "alert occupant of another workflow" row goes RED.
func TestClassifyAlertStart_ReplayOccupancy(t *testing.T) {
	id := uuid.New()
	for _, tc := range []struct {
		name       string
		status     int
		triggerSrc string
		workflowID string
		wantKind   AlertStartKind
		wantStatus int
		wantCode   string
	}{
		{"on_demand occupant of the same workflow", http.StatusOK, "on_demand", "hotfix_change",
			AlertStartRefused, http.StatusConflict, AlertKeyOccupiedCode},
		{"scheduled occupant of the same workflow", http.StatusOK, "scheduled", "hotfix_change",
			AlertStartRefused, http.StatusConflict, AlertKeyOccupiedCode},
		{"alert occupant of another workflow", http.StatusOK, "alert", "feature_change",
			AlertStartRefused, http.StatusConflict, AlertKeyOccupiedCode},
		{"empty trigger_source fails closed", http.StatusOK, "", "hotfix_change",
			AlertStartRefused, http.StatusConflict, AlertKeyOccupiedCode},
		{"genuine alert same-workflow replay", http.StatusOK, "alert", "hotfix_change",
			AlertStartAlreadyStarted, http.StatusOK, ""},
		{"201 minted by this request", http.StatusCreated, "on_demand", "hotfix_change",
			AlertStartStarted, http.StatusCreated, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := classifyAlertStart(tc.status, replayBody(id, tc.triggerSrc, tc.workflowID), "hotfix_change")
			if err != nil {
				t.Fatalf("classifyAlertStart: %v", err)
			}
			if out.Kind != tc.wantKind || out.Status != tc.wantStatus || out.Code != tc.wantCode {
				t.Fatalf("outcome = %+v, want kind %s status %d code %q", out, tc.wantKind, tc.wantStatus, tc.wantCode)
			}
			if out.RunID != id {
				t.Errorf("RunID = %s, want %s", out.RunID, id)
			}
			if tc.wantKind == AlertStartRefused && !strings.Contains(out.Message, id.String()) {
				t.Errorf("message %q must name the occupant %s", out.Message, id)
			}
		})
	}
}

// TestStartAlertRun_OccupiedKeyRefused drives the REAL replay path: a run an
// ordinary create minted under the auto-start's key is refused
// alert_key_occupied naming the occupant, and nothing is minted.
func TestStartAlertRun_OccupiedKeyRefused(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)
	key := alertTestKey
	occupant, err := repo.CreateRun(context.Background(), run.CreateRunParams{
		Repo:           "kuhlman-labs/fishhawk",
		WorkflowID:     "hotfix_change",
		WorkflowSHA:    "blobsha",
		TriggerSource:  run.TriggerOnDemand,
		IdempotencyKey: &key,
	})
	if err != nil {
		t.Fatalf("seed occupant: %v", err)
	}

	out, err := s.StartAlertRun(context.Background(), alertParams(alertSpec("diff")))
	if err != nil {
		t.Fatalf("StartAlertRun: %v", err)
	}
	if out.Kind != AlertStartRefused || out.Status != http.StatusConflict || out.Code != AlertKeyOccupiedCode || out.RunID != occupant.ID {
		t.Fatalf("outcome = %+v, want refused/409 %s for occupant %s", out, AlertKeyOccupiedCode, occupant.ID)
	}
	if n := runRowCount(repo); n != 1 {
		t.Errorf("run rows = %d, want 1 (only the occupant)", n)
	}
}
