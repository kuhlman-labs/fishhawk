package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan/planfixture"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// The ticket guard (#4067) refuses a plan-stage upload whose ticket_reference
// names a different issue than the run's issue:N trigger. Every test below
// seeds the run as issue:4012 on kuhlman-labs/fishhawk unless stated, and
// READS STATE after the call (artifact count, stage transitions, failure
// category/reason) rather than only the status code.

const guardRunRepo = "kuhlman-labs/fishhawk"

// newTicketGuardServer is newPlanServer with the run row seeded so the guard
// can load the run's trigger, and the stage seeded in running so FailStage
// walks running → failed in one transition.
func newTicketGuardServer(t *testing.T, triggerRef *string) (*Server, *signingFake, *fakeArtifactRepo, *auditFake, *promptRunRepo, uuid.UUID, uuid.UUID) {
	t.Helper()
	runID, stageID := uuid.New(), uuid.New()
	s, sf, ar, au, rr := newPlanServer(t, runID, stageID)
	rr.getStages[stageID] = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypePlan, State: run.StageStateRunning}
	rr.getRuns[runID] = &run.Run{ID: runID, Repo: guardRunRepo, TriggerRef: triggerRef}
	return s, sf, ar, au, rr, runID, stageID
}

// planWithTicket returns a schema-valid plan whose ticket_reference is ref.
func planWithTicket(t *testing.T, ref any) []byte {
	t.Helper()
	m := planfixture.Valid()
	m["ticket_reference"] = ref
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func ticketObj(id, url string) map[string]any {
	return map[string]any{"type": "github_issue", "id": id, "url": url}
}

// assertTicketRefused asserts the full refusal contract: 400
// plan_ticket_mismatch with the documented details, nothing stored, and the
// stage failed category B with a plan_ticket_mismatch reason naming both
// tickets.
func assertTicketRefused(t *testing.T, w interface {
	Result() *http.Response
}, body string, ar *fakeArtifactRepo, rr *promptRunRepo, stageID uuid.UUID, wantClaim string) {
	t.Helper()
	if code := w.Result().StatusCode; code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", code, body)
	}
	var env struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode error envelope: %v\n%s", err, body)
	}
	if env.Error.Code != "plan_ticket_mismatch" {
		t.Errorf("code = %q, want plan_ticket_mismatch", env.Error.Code)
	}
	for _, k := range []string{"artifact_kind", "claimed_id", "claimed_url", "run_trigger_ref", "run_repo"} {
		if _, ok := env.Error.Details[k]; !ok {
			t.Errorf("details missing %q: %v", k, env.Error.Details)
		}
	}
	if got := env.Error.Details["run_trigger_ref"]; got != "issue:4012" {
		t.Errorf("details.run_trigger_ref = %v, want issue:4012", got)
	}
	if len(ar.all) != 0 {
		t.Errorf("artifacts = %d, want 0 (a refused upload stores nothing)", len(ar.all))
	}
	var failed *promptTransitionStageCall
	for i := range rr.transitionStageCalls {
		c := rr.transitionStageCalls[i]
		if c.StageID != stageID {
			continue
		}
		if c.To == run.StageStateAwaitingInput || c.To == run.StageStateAwaitingApproval {
			t.Errorf("refused stage transitioned to %s", c.To)
		}
		if c.To == run.StageStateFailed {
			failed = &c
		}
	}
	if failed == nil {
		t.Fatalf("stage was not failed; transitions=%v", rr.transitionStageCalls)
	}
	if failed.Completion == nil || failed.Completion.FailureCategory == nil ||
		*failed.Completion.FailureCategory != run.FailureB {
		t.Errorf("failure category = %v, want B", failed.Completion)
	}
	reason := ""
	if failed.Completion != nil && failed.Completion.FailureReason != nil {
		reason = *failed.Completion.FailureReason
	}
	if !strings.HasPrefix(reason, "plan_ticket_mismatch: ") {
		t.Errorf("failure_reason = %q, want plan_ticket_mismatch prefix", reason)
	}
	if !strings.Contains(reason, wantClaim) || !strings.Contains(reason, "issue:4012") ||
		!strings.Contains(reason, guardRunRepo) {
		t.Errorf("failure_reason = %q, want it to name %q, issue:4012 and %s", reason, wantClaim, guardRunRepo)
	}
}

// assertTicketPassed asserts the upload was NOT refused by the guard: 201
// and a stored plan artifact.
func assertTicketPassed(t *testing.T, code int, body string, ar *fakeArtifactRepo) {
	t.Helper()
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (guard must fail open):\n%s", code, body)
	}
	if len(ar.all) != 1 {
		t.Errorf("artifacts = %d, want 1", len(ar.all))
	}
}

// (1) A plan whose id names a different issue in the same repo is refused.
func TestTicketGuard_RefusesPlanNamingForeignIssue(t *testing.T) {
	s, sf, ar, _, rr, runID, stageID := newTicketGuardServer(t, strPtr("issue:4012"))
	priv, _ := sf.issue(t, runID)
	body := planWithTicket(t, ticketObj("kuhlman-labs/fishhawk#4013", "https://github.com/kuhlman-labs/fishhawk/issues/4013"))

	w := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	assertTicketRefused(t, w, w.Body.String(), ar, rr, stageID, "kuhlman-labs/fishhawk#4013")
}

// (2) id "unknown" (the coercion default) but the url names a foreign issue.
func TestTicketGuard_RefusesForeignURLWithUnknownID(t *testing.T) {
	s, sf, ar, _, rr, runID, stageID := newTicketGuardServer(t, strPtr("issue:4012"))
	priv, _ := sf.issue(t, runID)
	body := planWithTicket(t, ticketObj("unknown", "https://github.com/kuhlman-labs/fishhawk/issues/4013"))

	w := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	assertTicketRefused(t, w, w.Body.String(), ar, rr, stageID, "kuhlman-labs/fishhawk#4013")
}

// (3) Same issue number, different repository.
func TestTicketGuard_RefusesSameNumberDifferentRepo(t *testing.T) {
	s, sf, ar, _, rr, runID, stageID := newTicketGuardServer(t, strPtr("issue:4012"))
	priv, _ := sf.issue(t, runID)
	body := planWithTicket(t, ticketObj("other/repo#4012", "https://github.com/other/repo/issues/4012"))

	w := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	assertTicketRefused(t, w, w.Body.String(), ar, rr, stageID, "other/repo#4012")
}

// (4) A clarification_request naming a foreign issue is refused and the stage
// is NOT parked at awaiting_input.
func TestTicketGuard_RefusesForeignClarification(t *testing.T) {
	s, sf, ar, au, rr, runID, stageID := newTicketGuardServer(t, strPtr("issue:4012"))
	priv, _ := sf.issue(t, runID)
	body := []byte(strings.ReplaceAll(string(validClarificationBytes(t)), "1057", "4013"))

	w := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	assertTicketRefused(t, w, w.Body.String(), ar, rr, stageID, "kuhlman-labs/fishhawk#4013")
	if n := countPlanAudit(au.appended, "clarification_requested"); n != 0 {
		t.Errorf("clarification_requested entries = %d, want 0", n)
	}
}

// (5) A grooming_report naming a foreign issue is refused; no grooming
// artifact or grooming_report_recorded entry is stored.
func TestTicketGuard_RefusesForeignGroomingReport(t *testing.T) {
	s, sf, ar, au, rr, runID, stageID := newTicketGuardServer(t, strPtr("issue:4012"))
	priv, _ := sf.issue(t, runID)
	body := []byte(strings.ReplaceAll(string(validGroomingReportBytes(t)), "fishhawk/issues/2235", "fishhawk/issues/4013"))
	body = []byte(strings.ReplaceAll(string(body), "kuhlman-labs/fishhawk#2235", "kuhlman-labs/fishhawk#4013"))

	w := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	assertTicketRefused(t, w, w.Body.String(), ar, rr, stageID, "kuhlman-labs/fishhawk#4013")
	if n := len(groomingAuditEntries(au)); n != 0 {
		t.Errorf("grooming_report_recorded entries = %d, want 0", n)
	}
}

// (6) A matching ticket whose repo differs only in case passes.
func TestTicketGuard_PassesMatchingTicketCaseInsensitiveRepo(t *testing.T) {
	s, sf, ar, _, _, runID, stageID := newTicketGuardServer(t, strPtr("issue:4012"))
	priv, _ := sf.issue(t, runID)
	body := planWithTicket(t, ticketObj("Kuhlman-Labs/Fishhawk#4012", "https://github.com/Kuhlman-Labs/Fishhawk/issues/4012"))

	w := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	assertTicketPassed(t, w.Code, w.Body.String(), ar)
}

// (7) A run with no TriggerRef is never guarded.
func TestTicketGuard_FailsOpenWithoutTriggerRef(t *testing.T) {
	s, sf, ar, _, _, runID, stageID := newTicketGuardServer(t, nil)
	priv, _ := sf.issue(t, runID)
	body := planWithTicket(t, ticketObj("kuhlman-labs/fishhawk#4013", "https://github.com/kuhlman-labs/fishhawk/issues/4013"))

	w := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	assertTicketPassed(t, w.Code, w.Body.String(), ar)
}

// (8) A non-issue TriggerRef (schedule:x) is never guarded.
func TestTicketGuard_FailsOpenOnNonIssueTrigger(t *testing.T) {
	s, sf, ar, _, _, runID, stageID := newTicketGuardServer(t, strPtr("schedule:x"))
	priv, _ := sf.issue(t, runID)
	body := planWithTicket(t, ticketObj("kuhlman-labs/fishhawk#4013", "https://github.com/kuhlman-labs/fishhawk/issues/4013"))

	w := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	assertTicketPassed(t, w.Code, w.Body.String(), ar)
}

// (9) A ticket_reference with no parseable claim passes.
func TestTicketGuard_FailsOpenOnUnparseableReference(t *testing.T) {
	s, sf, ar, _, _, runID, stageID := newTicketGuardServer(t, strPtr("issue:4012"))
	priv, _ := sf.issue(t, runID)
	body := planWithTicket(t, ticketObj("unknown", "https://example.com/tickets/abc"))

	w := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	assertTicketPassed(t, w.Code, w.Body.String(), ar)
}

// (10) A run the guard cannot load fails open. A missing run (ErrNotFound)
// flows through every later layer to 201.
func TestTicketGuard_FailsOpenOnRunNotFound(t *testing.T) {
	s, sf, ar, _, rr, runID, stageID := newTicketGuardServer(t, strPtr("issue:4012"))
	delete(rr.getRuns, runID)
	priv, _ := sf.issue(t, runID)
	body := planWithTicket(t, ticketObj("kuhlman-labs/fishhawk#4013", "https://github.com/kuhlman-labs/fishhawk/issues/4013"))

	w := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	assertTicketPassed(t, w.Code, w.Body.String(), ar)
}

// (10b) A generic run load error also fails open: the guard writes nothing
// and fails no stage. The error itself is owned by a later layer (the
// upkeep-declaration guard resolves the same run and answers 500), so the
// end-to-end assertion is "not refused by THIS guard", and the direct call
// pins that the guard declines to write.
func TestTicketGuard_FailsOpenOnRunLoadError(t *testing.T) {
	s, sf, _, _, rr, runID, stageID := newTicketGuardServer(t, strPtr("issue:4012"))
	rr.runErr = errors.New("db down")
	priv, _ := sf.issue(t, runID)
	body := planWithTicket(t, ticketObj("kuhlman-labs/fishhawk#4013", "https://github.com/kuhlman-labs/fishhawk/issues/4013"))

	w := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	if strings.Contains(w.Body.String(), "plan_ticket_mismatch") {
		t.Errorf("run load error was refused as a ticket mismatch:\n%s", w.Body.String())
	}
	for _, c := range rr.transitionStageCalls {
		if c.To == run.StageStateFailed {
			t.Errorf("run load error failed the stage: %+v", c)
		}
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+runID.String()+"/plan", nil)
	if s.guardPlanTicketReference(rec, req, runID, stageID, plan.ArtifactKindPlan, body) {
		t.Errorf("guard refused on a run load error; want fail open")
	}
	if rec.Body.Len() != 0 {
		t.Errorf("guard wrote a response on a run load error: %s", rec.Body.String())
	}
}

// A matching ticket passes and the plan is stored (the happy path).
func TestTicketGuard_PassesMatchingTicket(t *testing.T) {
	s, sf, ar, _, _, runID, stageID := newTicketGuardServer(t, strPtr("issue:4012"))
	priv, _ := sf.issue(t, runID)
	body := planWithTicket(t, ticketObj("kuhlman-labs/fishhawk#4012", "https://github.com/kuhlman-labs/fishhawk/issues/4012"))

	w := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	assertTicketPassed(t, w.Code, w.Body.String(), ar)
}

// An undetectable kind (a non-string "kind" fails plan.DetectArtifactKind)
// is never judged by the ticket guard, even when its ticket_reference names a
// foreign issue: the plan path owns that ParseError, so the refusal stays
// plan_invalid rather than plan_ticket_mismatch.
func TestTicketGuard_SkipsUndetectableKind(t *testing.T) {
	s, sf, _, _, _, runID, stageID := newTicketGuardServer(t, strPtr("issue:4012"))
	priv, _ := sf.issue(t, runID)
	body := []byte(`{"kind":5,"ticket_reference":{"type":"github_issue","id":"kuhlman-labs/fishhawk#4013","url":"https://github.com/kuhlman-labs/fishhawk/issues/4013"}}`)

	w := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v\n%s", err, w.Body.String())
	}
	if w.Code != http.StatusBadRequest || env.Error.Code != "plan_invalid" {
		t.Errorf("response = %d %q, want 400 plan_invalid (the plan path owns the parse error):\n%s",
			w.Code, env.Error.Code, w.Body.String())
	}
}

// A bare-string ticket_reference (the pre-coercion shape) naming a foreign
// issue is refused: the guard runs before coercion.
func TestTicketGuard_RefusesBareStringForeignURL(t *testing.T) {
	s, sf, ar, _, rr, runID, stageID := newTicketGuardServer(t, strPtr("issue:4012"))
	priv, _ := sf.issue(t, runID)
	body := planWithTicket(t, "https://github.com/kuhlman-labs/fishhawk/issues/4013")

	w := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	assertTicketRefused(t, w, w.Body.String(), ar, rr, stageID, "kuhlman-labs/fishhawk#4013")
}

func TestParseTicketClaims(t *testing.T) {
	cases := []struct {
		name string
		ref  string
		want []ticketClaim
	}{
		{"owner/name id", `{"id":"kuhlman-labs/fishhawk#12"}`, []ticketClaim{{"kuhlman-labs/fishhawk", 12}}},
		{"gitlab subgroup id", `{"id":"group/sub/project#7"}`, []ticketClaim{{"group/sub/project", 7}}},
		{"bare number id", `{"id":"#9"}`, []ticketClaim{{"", 9}}},
		{"github url", `{"url":"https://github.com/o/r/issues/5"}`, []ticketClaim{{"o/r", 5}}},
		{"gitlab url", `{"url":"https://gitlab.com/g/sub/p/-/issues/3"}`, []ticketClaim{{"g/sub/p", 3}}},
		{"api github url names number only", `{"url":"https://api.github.com/repos/o/r/issues/5"}`, []ticketClaim{{"", 5}}},
		{"gitlab api url names number only", `{"url":"https://gitlab.example.com/api/v4/projects/12/issues/3"}`, []ticketClaim{{"", 3}}},
		{"id and url both claim", `{"id":"o/r#1","url":"https://github.com/o/r/issues/2"}`, []ticketClaim{{"o/r", 1}, {"o/r", 2}}},
		{"bare string is a url", `"https://github.com/o/r/issues/8"`, []ticketClaim{{"o/r", 8}}},
		{"unknown id", `{"id":"unknown"}`, nil},
		{"comms unanchored", `{"id":"o/r","url":"https://github.com/o/r/issues"}`, nil},
		{"non-numeric id", `{"id":"o/r#abc"}`, nil},
		{"zero number", `{"id":"o/r#0"}`, nil},
		{"single-segment repo id", `{"id":"repo#4"}`, nil},
		{"relative url", `{"url":"/o/r/issues/4"}`, nil},
		{"url single-segment repo", `{"url":"https://github.com/r/issues/4"}`, nil},
		{"url non-numeric", `{"url":"https://github.com/o/r/issues/x"}`, nil},
		{"pull url", `{"url":"https://github.com/o/r/pull/4"}`, nil},
		{"non-string id ignored", `{"id":42}`, nil},
		{"array reference", `[1,2]`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := json.RawMessage(`{"ticket_reference":` + tc.ref + `}`)
			got := parseTicketClaims(raw)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseTicketClaims(%s) = %+v, want %+v", tc.ref, got, tc.want)
			}
		})
	}
	for _, raw := range []string{``, `not json`, `{}`, `{"ticket_reference":null}`} {
		if got := parseTicketClaims(json.RawMessage(raw)); got != nil {
			t.Errorf("parseTicketClaims(%q) = %+v, want nil", raw, got)
		}
	}
}

func TestTicketReferenceStrings(t *testing.T) {
	if id, u := ticketReferenceStrings([]byte(`{"ticket_reference":{"id":"o/r#1","url":"https://x/o/r/issues/1"}}`)); id != "o/r#1" || u != "https://x/o/r/issues/1" {
		t.Errorf("object form = (%q, %q)", id, u)
	}
	if id, u := ticketReferenceStrings([]byte(`{"ticket_reference":"https://x/o/r/issues/1"}`)); id != "" || u != "https://x/o/r/issues/1" {
		t.Errorf("string form = (%q, %q)", id, u)
	}
	if id, u := ticketReferenceStrings([]byte(`nope`)); id != "" || u != "" {
		t.Errorf("unparseable = (%q, %q)", id, u)
	}
}
