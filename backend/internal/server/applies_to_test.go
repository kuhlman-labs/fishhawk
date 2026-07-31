package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// Unit + handler coverage for the applies_to routing control (E53.3 / #2226).
// One assertion per enumerated branch — the control is a fail-closed
// governance gate, so "the happy path plus a subset" is exactly the shape
// that ships a control which rejects everything (or admits everything) and
// still passes (#1199).

// appliesToSpec builds a two-workflow v2 spec: `guarded` carries the caller's
// applies_to block, `open` declares none (and is therefore always a
// satisfying alternative). Both are minimal but real — a plan stage plus an
// implement stage — because handleCreateRun refuses a stageless workflow.
func appliesToSpec(appliesToBlock string) string {
	guarded := "  guarded:\n"
	if appliesToBlock != "" {
		guarded += appliesToBlock
	}
	guarded += `    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`
	return "version: \"2\"\nworkflows:\n" + guarded + `  open:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`
}

// createRunViaHandler POSTs body through the real handler and returns the
// recorder, so a test can assert on the status, the message and the audit
// trail the same way an operator sees them.
func createRunViaHandler(t *testing.T, s *Server, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	s.handleCreateRun(w, withAuth(req))
	return w
}

// createRunViaHandlerWithKey is createRunViaHandler carrying an
// Idempotency-Key header, so a test can drive the create + replay pair the
// admission-gate ordering (#2366) turns on.
func createRunViaHandlerWithKey(t *testing.T, s *Server, body map[string]any, key string) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	s.handleCreateRun(w, withAuth(req))
	return w
}

// guardedRunBody is the create-request skeleton every case below varies.
func guardedRunBody(appliesToBlock string) map[string]any {
	return map[string]any{
		"repo": "x/y", "workflow_id": "guarded", "workflow_sha": "abc",
		"trigger_source": "github_issue", "workflow_spec": appliesToSpec(appliesToBlock),
	}
}

const labelsGuard = `    applies_to:
      labels:
        - dependencies
        - chore
`

// issueCtx builds an inline issue_context payload carrying labels.
func issueCtx(labels ...string) map[string]any {
	return map[string]any{
		"title": "t", "body": "b", "url": "https://example.invalid/i/1",
		"number": 1, "labels": labels,
	}
}

// --- M1: a workflow with no applies_to admits any change (back-compat) ---

func TestAppliesTo_M1_NoDeclaration_AdmitsAnyChange(t *testing.T) {
	s, _, _, _ := newDelegationServer(t)
	body := guardedRunBody("")
	body["workflow_id"] = "open"
	body["trigger_source"] = "cli"
	w := createRunViaHandler(t, s, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 — a workflow declaring no applies_to must accept any change:\n%s", w.Code, w.Body.String())
	}
}

// --- M2: a satisfied labels criterion ADMITS (the issue's AC1) ---
//
// Binding CONDITION 2 raised this from a side effect of the overlap case to
// its own observable: a control that rejects EVERY ordinary run would satisfy
// every refusal-shaped criterion. This is the assertion every non-exceptional
// run depends on.

func TestAppliesTo_M2_SatisfiedLabels_Admits(t *testing.T) {
	s, _, _, _ := newDelegationServer(t)
	body := guardedRunBody(labelsGuard)
	body["issue_context"] = issueCtx("area:backend", "dependencies")
	w := createRunViaHandler(t, s, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 — an issue carrying a declared label must be ADMITTED:\n%s", w.Code, w.Body.String())
	}
	var created runResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.IssueContext == nil || len(created.IssueContext.Labels) != 2 {
		t.Fatalf("issue_context.labels = %+v, want the two posted labels echoed back", created.IssueContext)
	}
}

// --- M3: an unsatisfied labels criterion REJECTS with the full message ---

func TestAppliesTo_M3_UnsatisfiedLabels_Rejects(t *testing.T) {
	s, _, au, _ := newDelegationServer(t)
	body := guardedRunBody(labelsGuard)
	body["issue_context"] = issueCtx("bug", "area:backend")
	w := createRunViaHandler(t, s, body)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
	env := decodeErrorEnvelope(t, w)
	msg, details := env.Message, env.Details
	if env.Code != "workflow_not_applicable" {
		t.Errorf("code = %q, want workflow_not_applicable", env.Code)
	}
	// The four message elements the plan makes binding.
	for _, want := range []string{`"guarded"`, "labels", "dependencies", "bug", "open"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q is missing %q — the message must name the workflow, the failed criterion, the observed value and the satisfying workflows", msg, want)
		}
	}
	if details["criterion"] != "labels" {
		t.Errorf("details.criterion = %v, want labels", details["criterion"])
	}
	// The refusal is audited on the GLOBAL chain: no run row exists yet.
	if n := countGlobalAppends(au, "run_rejected_applies_to"); n != 1 {
		t.Errorf("run_rejected_applies_to global entries = %d, want 1", n)
	}
	if n := len(au.appended); n != 0 {
		t.Errorf("run-scoped appends = %d, want 0 — a pre-insert refusal has no run to scope an entry to", n)
	}
}

// --- M4: an ISSUE-LESS run rejects on the EMPTY label set, and says so ---
//
// The assertion is on the MESSAGE TEXT, not merely the status code: "your
// labels don't match" is actively misleading when the run has no labels at
// all, and the operator's fix (an issue-triggered run, or the override) is
// different from the fix for a genuine mismatch.

func TestAppliesTo_M4_IssuelessRun_RejectsOnEmptyLabelSet(t *testing.T) {
	s, _, _, _ := newDelegationServer(t)
	body := guardedRunBody(labelsGuard)
	body["trigger_source"] = "cli" // no issue_context is legal here
	w := createRunViaHandler(t, s, body)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
	env := decodeErrorEnvelope(t, w)
	msg, details := env.Message, env.Details
	if !strings.Contains(msg, "NO issue context") {
		t.Errorf("message %q must cite the ABSENT issue context specifically, not a generic predicate mismatch", msg)
	}
	if !strings.Contains(msg, "trigger_source=cli") {
		t.Errorf("message %q must name the trigger_source that produced the empty label set", msg)
	}
	if details["absent_issue_context"] != true {
		t.Errorf("details.absent_issue_context = %v, want true", details["absent_issue_context"])
	}
}

// TestAppliesTo_M4b_UnlabelledIssue_ReportsTheObservedValue covers the OTHER
// way a label set comes out empty: the run DOES carry issue context, and that
// issue carries no labels. It shares M4's empty-label-set branch and must not
// share M4's sentence — telling the operator of an issue-triggered run that it
// "carries NO issue context" sends them hunting a trigger problem they do not
// have, when the fix is to label the issue.
//
// It must also still report the OBSERVED VALUE. The message shape is binding on
// every rejection path (workflow, failed criterion, observed value, satisfying
// workflows); the empty-label-set branch is where it is most tempting to drop
// the observed value as uninteresting, and "[]" is precisely the actionable
// fact there.
func TestAppliesTo_M4b_UnlabelledIssue_ReportsTheObservedValue(t *testing.T) {
	s, _, _, _ := newDelegationServer(t)
	body := guardedRunBody(labelsGuard)
	body["issue_context"] = issueCtx() // present, zero labels
	w := createRunViaHandler(t, s, body)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
	env := decodeErrorEnvelope(t, w)
	msg := env.Message
	if strings.Contains(msg, "NO issue context") {
		t.Errorf("message %q claims the run has NO issue context, but it carries one with zero labels; the two have different fixes", msg)
	}
	if !strings.Contains(msg, "the change's labels is []") {
		t.Errorf("message %q must report the OBSERVED value ([]) like every other rejection path, not only the required one", msg)
	}
	for _, want := range []string{`"guarded"`, "labels", "dependencies", "no labels", "open"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q is missing %q", msg, want)
		}
	}
	if env.Details["absent_issue_context"] != true {
		t.Errorf("details.absent_issue_context = %v, want true — the label set IS empty", env.Details["absent_issue_context"])
	}
}

// TestAppliesTo_M4_IssuelessMessage_AlsoReportsObserved is M4's shape half:
// the issue-LESS sentence carries the observed value too, so neither empty-set
// branch is a hole in the binding message shape.
func TestAppliesTo_M4_IssuelessMessage_AlsoReportsObserved(t *testing.T) {
	got := renderAppliesToRejection(appliesToRejection{
		WorkflowID: "guarded", Criterion: "labels", ObservedLabel: "labels",
		Required: []string{"dependencies"}, AbsentIssueContext: true,
		IssueContextPresent: false, TriggerSource: "cli",
	})
	if !strings.Contains(got, "the change's labels is []") {
		t.Errorf("message %q must report the observed value", got)
	}
	if !strings.Contains(got, "NO issue context") || !strings.Contains(got, "trigger_source=cli") {
		t.Errorf("message %q must still name the absent issue context and its trigger_source", got)
	}
}

// --- M5: the trigger criterion routes by trigger_source ---

func TestAppliesTo_M5_TriggerCriterion(t *testing.T) {
	for _, tc := range []struct {
		name     string
		declared string
		want     int
	}{
		{"diff admits a github_issue run", "diff", http.StatusCreated},
		{"scheduled rejects a github_issue run", "scheduled", http.StatusUnprocessableEntity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _, _ := newDelegationServer(t)
			body := guardedRunBody("    applies_to:\n      trigger:\n        - " + tc.declared + "\n")
			body["issue_context"] = issueCtx("bug")
			w := createRunViaHandler(t, s, body)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d:\n%s", w.Code, tc.want, w.Body.String())
			}
		})
	}
}

// --- M5b: the non-diff trigger forms route correctly with no producer ---
//
// No trigger_source emits scheduled or on_demand today (ADR-065's groomer and
// ADR-053's incident intake will), so this is a POSITIVE matcher unit
// synthesizing each Change form directly. It proves the forms are wired
// rather than merely spelled, which is what makes accepting them consistent
// instead of dead surface.

func TestAppliesTo_M5b_NonDiffTriggerForms_RouteCorrectly(t *testing.T) {
	for _, tc := range []struct {
		form spec.TriggerForm
	}{{spec.TriggerScheduled}, {spec.TriggerOnDemand}} {
		t.Run(string(tc.form), func(t *testing.T) {
			wf := spec.Workflow{AppliesTo: &spec.Predicate{Triggers: []spec.TriggerForm{tc.form}}}
			ok, evaluated, err := evaluateAppliesTo(wf, spec.Change{Trigger: tc.form}, appliesToPhaseAdmission)
			if err != nil || !ok || !evaluated {
				t.Fatalf("(%v, %v, %v) for a %s change vs a %s predicate, want (true, true, nil)", ok, evaluated, err, tc.form, tc.form)
			}
			// And the same predicate must NOT match a diff run.
			ok, _, err = evaluateAppliesTo(wf, spec.Change{Trigger: spec.TriggerDiff}, appliesToPhaseAdmission)
			if err != nil {
				t.Fatalf("unexpected error matching a diff change: %v", err)
			}
			if ok {
				t.Errorf("a %s-only predicate matched a diff change", tc.form)
			}
		})
	}
}

// --- M7: a paths-ONLY predicate ADMITS at start_run (deferral, positive) ---
//
// The load-bearing proof that `paths` is DEFERRED rather than silently
// evaluated against zero paths — which would reject every run, since a
// paths-declaring predicate correctly does not match a pathless change.

func TestAppliesTo_M7_PathsOnly_AdmitsAtAdmission(t *testing.T) {
	s, _, _, _ := newDelegationServer(t)
	body := guardedRunBody("    applies_to:\n      paths:\n        - \"docs/**\"\n")
	body["issue_context"] = issueCtx("anything")
	w := createRunViaHandler(t, s, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 — paths is deferred to the plan gate:\n%s", w.Code, w.Body.String())
	}
	// And the phase split says so structurally.
	p := spec.Predicate{Paths: []string{"docs/**"}}
	if _, constrains := appliesToPhasePredicate(p, appliesToPhaseAdmission); constrains {
		t.Error("a paths-only predicate constrains ADMISSION; it must constrain only the plan gate")
	}
	if _, constrains := appliesToPhasePredicate(p, appliesToPhasePlanGate); !constrains {
		t.Error("a paths-only predicate does not constrain the PLAN GATE; the deferred criterion would never be enforced")
	}
}

// TestAppliesTo_PhaseSplit_IsExhaustive pins which criterion belongs to which
// phase. A criterion silently landing in neither half is a criterion that is
// declared and never enforced — the failure mode this whole control exists to
// prevent.
func TestAppliesTo_PhaseSplit_IsExhaustive(t *testing.T) {
	full := spec.Predicate{
		Paths:       []string{"docs/**"},
		Labels:      []string{"chore"},
		ChangeKinds: []string{"docs"},
		Triggers:    []spec.TriggerForm{spec.TriggerDiff},
	}
	adm, _ := appliesToPhasePredicate(full, appliesToPhaseAdmission)
	gate, _ := appliesToPhasePredicate(full, appliesToPhasePlanGate)
	if len(adm.Labels) == 0 || len(adm.Triggers) == 0 || len(adm.ChangeKinds) == 0 {
		t.Errorf("admission half = %+v, want labels + trigger + change_kind", adm)
	}
	if len(adm.Paths) != 0 {
		t.Error("admission half carries paths; paths is the deferred criterion")
	}
	if len(gate.Paths) == 0 {
		t.Errorf("plan-gate half = %+v, want paths", gate)
	}
	if len(gate.Labels) != 0 || len(gate.Triggers) != 0 || len(gate.ChangeKinds) != 0 {
		t.Errorf("plan-gate half = %+v, want paths ONLY (the rest are decided at admission)", gate)
	}

	// TRIPWIRE for a criterion the grammar gains LATER. The assertions above
	// pin today's four; appliesToPhasePredicate is a hand-maintained
	// field-by-field copy of spec.Predicate and its `constrains` check
	// enumerates the same four by name. A fifth criterion added by a sibling
	// slice (#2227 escalations, #2211 review conventions) is picked up by the
	// applies_to $ref automatically, but would be copied into NEITHER half and
	// counted by NEITHER `constrains` — declared and silently never enforced,
	// which is the exact failure mode this function's own comment names for
	// change_kind. So the "exhaustive over the predicate grammar" claim is
	// enforced against the STRUCT rather than against a list restated here.
	//
	// The field COUNT is the cheap half: it fails on the specific edit that
	// opens the gap and on nothing else.
	predT := reflect.TypeOf(spec.Predicate{})
	if n := predT.NumField(); n != 4 {
		t.Fatalf("spec.Predicate has %d fields, want 4 (paths, labels, change_kind, trigger). "+
			"A criterion was added or removed: route it in appliesToPhasePredicate (into a phase half AND its `constrains` check), "+
			"extend firstFailingCriterion so the rejection can name it, then update this count.", n)
	}

	// The load-bearing half: EVERY declared criterion must survive the split
	// into at least one phase. Populating `full` above is what feeds this, so a
	// new field left out of `full` fails here too — the author is pushed to
	// decide which phase owns it rather than to bump a number.
	fullV, admV, gateV := reflect.ValueOf(full), reflect.ValueOf(adm), reflect.ValueOf(gate)
	for i := range predT.NumField() {
		name := predT.Field(i).Name
		if predT.Field(i).Type.Kind() != reflect.Slice {
			t.Errorf("spec.Predicate.%s is not a slice; this tripwire assumes every criterion is a list — re-check the split by hand", name)
			continue
		}
		if fullV.Field(i).Len() == 0 {
			t.Errorf("the `full` fixture leaves %s unset, so the split is untested for it; populate it and route the criterion", name)
			continue
		}
		if admV.Field(i).Len() == 0 && gateV.Field(i).Len() == 0 {
			t.Errorf("spec.Predicate.%s is carried by NEITHER phase half: a declared criterion that is silently never enforced", name)
		}
	}
}

// --- M10: an override with a reason ADMITS and records the run-scoped entry ---

func TestAppliesTo_M10_Override_AdmitsAndAudits(t *testing.T) {
	s, _, au, _ := newDelegationServer(t)
	body := guardedRunBody(labelsGuard)
	body["issue_context"] = issueCtx("bug")
	body["applies_to_override"] = true
	body["applies_to_override_reason"] = "one-off backport; declaration widening tracked separately"
	w := createRunViaHandler(t, s, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 with an override:\n%s", w.Code, w.Body.String())
	}
	var created runResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	entry := findRunScopedAppend(au, created.ID, "run_admitted_applies_to_override")
	if entry == nil {
		t.Fatal("no run-scoped run_admitted_applies_to_override entry; the audit entry IS the override's source of truth")
	}
	var payload map[string]any
	if err := json.Unmarshal(entry.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(payload["reason"].(string), "one-off backport") {
		t.Errorf("payload.reason = %v, want the operator's verbatim reason", payload["reason"])
	}
	if s, _ := payload["suppressed_rejection"].(string); !strings.Contains(s, "guarded") {
		t.Errorf("payload.suppressed_rejection = %v, want the refusal the override bypassed", payload["suppressed_rejection"])
	}
	// No refusal was recorded — the run was admitted, not refused-then-forced.
	if n := countGlobalAppends(au, "run_rejected_applies_to"); n != 0 {
		t.Errorf("run_rejected_applies_to entries = %d, want 0 on the override path", n)
	}
}

// --- M11b: the AUDIT ENTRY, not the request, is the source of truth ---
//
// A run whose creation carried an override but whose RUN-SCOPED entry is
// ABSENT has no override — and no run either. The entry is the only record a
// run-scoped audit read returns, so a run that outlived its own entry would be
// a governance bypass legible nowhere on the chain of the run it applies to.
// That is not merely a lost carry-forward: for an ADMISSION-ONLY violation
// (labels, trigger — the shape this case uses) there is no second evaluation
// point, so the run would proceed to completion looking exactly like one that
// satisfied its declaration.
//
// So the carry-forward append is a precondition of the run SURVIVING, just as
// the grant append (M11c) is a precondition of it being admitted: the run is
// CANCELLED and the caller gets 503, and the lookup still reports no override.

func TestAppliesTo_M11b_OverrideLookup_IsAuditEntryNotRequest(t *testing.T) {
	s, repo, au, _ := newDelegationServer(t)
	// The GRANT (global chain) succeeds — the bypass is audited there — but
	// the run-scoped carry-forward append fails.
	au.appendErrCategory = "run_admitted_applies_to_override"
	body := guardedRunBody(labelsGuard)
	body["issue_context"] = issueCtx("bug")
	body["applies_to_override"] = true
	body["applies_to_override_reason"] = "forced"
	w := createRunViaHandler(t, s, body)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 — a run whose override entry could not be recorded must not proceed on the request field alone:\n%s", w.Code, w.Body.String())
	}
	if env := decodeErrorEnvelope(t, w); env.Code != "audit_unavailable" {
		t.Errorf("code = %q, want audit_unavailable", env.Code)
	}

	// The run row exists (the entry needed its id) but is CANCELLED, so no
	// stage of it can run on the unrecorded bypass.
	au.appendErrCategory = ""
	if n := len(repo.runs); n != 1 {
		t.Fatalf("run rows = %d, want the one row the entry needed an id from", n)
	}
	for id, rr := range repo.runs {
		if rr.State != run.StateCancelled {
			t.Errorf("run %s state = %q, want cancelled — it carries a governance bypass its own audit chain does not record", id, rr.State)
		}
		has, err := s.runHasAppliesToOverride(context.Background(), id)
		if err != nil {
			t.Fatalf("runHasAppliesToOverride: %v", err)
		}
		if has {
			t.Error("runHasAppliesToOverride returned true for a run with NO recorded entry; the override would carry forward on the strength of the request alone")
		}
	}
}

// --- M11c: an override that CANNOT BE AUDITED is refused, not granted ---
//
// The audit entry being the override's source of truth means the entry is a
// PRECONDITION of the bypass, not a best-effort record written after it. The
// asymmetry to the REFUSAL audit (TestAppliesTo_RefusalAuditFailure_StillRefuses,
// correctly best-effort) is the whole point: there the decision already went the
// safe way, so an audit outage must not soften it; here the decision goes the
// UNSAFE way, so an audit outage must not permit it.
//
// This matters most for an ADMISSION-ONLY violation, which is what this case
// uses (a labels-only declaration): `paths` gets a second evaluation at the plan
// gate, so a lost override there merely costs a re-run, but a labels or trigger
// bypass has NO downstream evaluation point. Warn-logging the failure and
// admitting anyway would run the change to completion having bypassed the
// declaration with nothing in the log saying so.

func TestAppliesTo_M11c_OverrideGrantAuditFailure_RefusesTheRun(t *testing.T) {
	s, repo, au, _ := newDelegationServer(t)
	au.appendErr = errors.New("audit down")
	body := guardedRunBody(labelsGuard)
	body["issue_context"] = issueCtx("bug")
	body["applies_to_override"] = true
	body["applies_to_override_reason"] = "forced"
	w := createRunViaHandler(t, s, body)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 — an override the audit log did not record is an UNAUDITED governance bypass:\n%s", w.Code, w.Body.String())
	}
	if n := len(repo.runs); n != 0 {
		t.Errorf("created %d run rows for an override that could not be audited, want 0", n)
	}
	if env := decodeErrorEnvelope(t, w); env.Code != "audit_unavailable" {
		t.Errorf("code = %q, want audit_unavailable", env.Code)
	}
}

// TestAppliesTo_OverrideWithNoAuditRepo_Refuses is M11c's capability-absent
// twin: with no audit repository wired there is nowhere to record the bypass,
// so the override is refused rather than silently granted. An override is only
// sanctioned because it leaves a trail; a deployment that cannot leave one does
// not get the escape hatch.
func TestAppliesTo_OverrideWithNoAuditRepo_Refuses(t *testing.T) {
	repo := newFakeRepo()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo}) // AuditRepo intentionally nil
	body := guardedRunBody(labelsGuard)
	body["issue_context"] = issueCtx("bug")
	body["applies_to_override"] = true
	body["applies_to_override_reason"] = "forced"
	w := createRunViaHandler(t, s, body)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 with no audit repo wired:\n%s", w.Code, w.Body.String())
	}
	if n := len(repo.runs); n != 0 {
		t.Errorf("created %d run rows, want 0", n)
	}
}

// --- M11d: the override reaches the DEFERRED paths criterion --------------
//
// `paths` is enforced at the plan gate, not at admission, so the common
// override case is a run whose labels/trigger are fine (or unconstrained) and
// whose planned scope is not. Recording the override only when ADMISSION
// refuses would make the escape hatch unreachable in exactly that case, leaving
// the operator to widen the declaration permanently — the behaviour the
// override exists to avoid.

func TestAppliesTo_M11d_Override_CarriesForwardWhenAdmissionPasses(t *testing.T) {
	for _, tc := range []struct {
		name  string
		guard string
		// labels the run's issue context carries; nil means an
		// issue-less run.
		labels []string
	}{
		{
			// A paths-ONLY declaration does not constrain admission at
			// all: nothing here can refuse, and the only rejection this
			// run will ever meet is the plan gate's.
			name:   "paths-only declaration, admission unconstrained",
			guard:  "    applies_to:\n      paths: [\"docs/**\"]\n",
			labels: []string{"bug"},
		},
		{
			// Admission is constrained and SATISFIED; the paths half is
			// still deferred and may still refuse.
			name:   "admission criteria satisfied, paths still deferred",
			guard:  "    applies_to:\n      labels: [dependencies]\n      paths: [\"docs/**\"]\n",
			labels: []string{"dependencies"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, au, _ := newDelegationServer(t)
			body := guardedRunBody(tc.guard)
			body["issue_context"] = issueCtx(tc.labels...)
			body["applies_to_override"] = true
			body["applies_to_override_reason"] = "scope reaches outside docs/ this once; widening tracked separately"
			w := createRunViaHandler(t, s, body)
			if w.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
			}
			var created runResponse
			if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
				t.Fatal(err)
			}
			if findRunScopedAppend(au, created.ID, "run_admitted_applies_to_override") == nil {
				t.Fatal("no run-scoped override entry for a run admitted on its merits but carrying an override; the plan gate's deferred paths refusal would then be unoverridable")
			}
			has, err := s.runHasAppliesToOverride(context.Background(), created.ID)
			if err != nil || !has {
				t.Fatalf("runHasAppliesToOverride = (%v, %v), want (true, nil) — the plan gate reads THIS", has, err)
			}
		})
	}
}

// TestAppliesTo_OverrideLookup_Positive is M11b's control: the entry present
// means the lookup reports the override. Without this the negative above
// would pass on a stub that always returns false.
func TestAppliesTo_OverrideLookup_Positive(t *testing.T) {
	s, _, _, _ := newDelegationServer(t)
	body := guardedRunBody(labelsGuard)
	body["issue_context"] = issueCtx("bug")
	body["applies_to_override"] = true
	body["applies_to_override_reason"] = "forced"
	w := createRunViaHandler(t, s, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	var created runResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	has, err := s.runHasAppliesToOverride(context.Background(), created.ID)
	if err != nil || !has {
		t.Fatalf("runHasAppliesToOverride = (%v, %v), want (true, nil)", has, err)
	}
}

// TestAppliesTo_OverrideLookup_ErrorFailsClosed pins the lookup's own
// defensive branch: a repository read error is RETURNED, never reported as
// "no override" with a nil error (which a caller would read as a clean
// answer) and never as "has override" (which would be permission granted by
// an outage).
func TestAppliesTo_OverrideLookup_ErrorFailsClosed(t *testing.T) {
	s, _, au, _ := newDelegationServer(t)
	au.listByCategoryErr = errors.New("db unavailable")
	has, err := s.runHasAppliesToOverride(context.Background(), uuid.New())
	if err == nil {
		t.Fatal("lookup error was swallowed; the caller cannot distinguish 'no override' from 'could not tell'")
	}
	if has {
		t.Error("lookup reported an override on a read error — an outage must never grant the bypass")
	}
}

// TestAppliesTo_OverrideLookup_NilAuditRepo covers the capability-absent
// branch: with no audit repository wired there is no entry to find, so the
// lookup reports no override rather than erroring.
func TestAppliesTo_OverrideLookup_NilAuditRepo(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	has, err := s.runHasAppliesToOverride(context.Background(), uuid.New())
	if err != nil || has {
		t.Fatalf("(%v, %v), want (false, nil) with no audit repo wired", has, err)
	}
}

// --- M12: a reasonless override is a 400 and admits nothing ---

func TestAppliesTo_M12_OverrideWithoutReason_Rejects(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason any
	}{
		{"omitted", nil},
		{"empty", ""},
		{"whitespace-only", "   \t\n "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, repo, _, _ := newDelegationServer(t)
			body := guardedRunBody(labelsGuard)
			body["issue_context"] = issueCtx("bug")
			body["applies_to_override"] = true
			if tc.reason != nil {
				body["applies_to_override_reason"] = tc.reason
			}
			w := createRunViaHandler(t, s, body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
			}
			msg := decodeErrorEnvelope(t, w).Message
			if !strings.Contains(msg, "applies_to_override_reason") {
				t.Errorf("message %q must name the missing field", msg)
			}
			if n := len(repo.runs); n != 0 {
				t.Errorf("created %d run rows; a reasonless override must admit NOTHING", n)
			}
		})
	}
}

// --- M13: a Match ERROR REJECTS rather than being swallowed into an admit ---
//
// The tempting bug is `if err != nil { admit }`, written by analogy with
// runScopePrecheck / runSurfaceSweep / runTestSweep / overCapSplitRejection,
// all of which correctly fail OPEN because they are advisory and all of which
// sit in this same package. This control is not advisory.

func TestAppliesTo_M13_MatchError_FailsClosed(t *testing.T) {
	// A malformed glob makes Predicate.Match return an error. Reached via
	// the shared core directly, since the spec validator rejects such a
	// declaration at parse time — the branch exists precisely for a caller
	// that got a predicate past validation.
	wf := spec.Workflow{AppliesTo: &spec.Predicate{Paths: []string{"docs/["}}}
	ok, _, err := evaluateAppliesTo(wf, spec.Change{Paths: []string{"docs/x.md"}}, appliesToPhasePlanGate)
	if err == nil {
		t.Fatal("a malformed glob did not error; the fail-closed branch is unreachable")
	}
	if ok {
		t.Error("evaluateAppliesTo matched despite the error")
	}

	// The empty-predicate error is the other Match error mode.
	wf = spec.Workflow{AppliesTo: &spec.Predicate{}}
	_, constrains := appliesToPhasePredicate(*wf.AppliesTo, appliesToPhaseAdmission)
	if constrains {
		t.Fatal("an empty predicate reports as constraining; Match would then be called and error")
	}

	// And the renderer turns a Match error into a REJECTION message, not an
	// admit — including at the admission point.
	msg := renderAppliesToRejection(appliesToRejection{
		WorkflowID: "guarded",
		MatchErr:   errors.New("path predicate: malformed glob \"docs/[\""),
		Phase:      appliesToPhaseAdmission,
	})
	if !strings.Contains(msg, "could not be evaluated") || !strings.Contains(msg, "never treated as match-all") {
		t.Errorf("message %q must say the declaration could not be evaluated and that this is a refusal", msg)
	}
}

// TestAppliesTo_AdmissionRefusal_RejectsAtTheCallSite drives an ordinary
// unsatisfied-criterion refusal through the ADMISSION call site rather than
// through the core, so the 422 + refusal-audit wiring is pinned end to end.
//
// It deliberately does NOT claim to cover checkAppliesTo's matchErr branch: a
// Match error is not reachable through that call site, and the previous name
// (…_AdmissionMatchError_Rejects) said otherwise while its own body conceded
// it. TestAppliesTo_AdmissionPhase_CannotProduceAMatchError below pins WHY it
// is unreachable, which is the assertion that actually protects the branch.
func TestAppliesTo_AdmissionRefusal_RejectsAtTheCallSite(t *testing.T) {
	s, _, au, _ := newDelegationServer(t)
	w := httptest.NewRecorder()
	req := withAuth(httptest.NewRequest(http.MethodPost, "/v0/runs", nil))
	crq := &createRunRequest{
		Repo: "x/y", WorkflowID: "guarded", TriggerSource: "cli",
	}
	wf := spec.Workflow{AppliesTo: &spec.Predicate{Labels: []string{"dependencies"}}}
	admit, rec := s.checkAppliesTo(w, req, crq, nil, wf)
	if admit {
		t.Fatal("an unsatisfied admission criterion admitted the run")
	}
	if rec != nil {
		t.Error("a plain refusal produced an override record")
	}
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", w.Code)
	}
	if n := countGlobalAppends(au, "run_rejected_applies_to"); n != 1 {
		t.Errorf("global refusal entries = %d, want 1", n)
	}
}

// TestAppliesTo_AdmissionPhase_CannotProduceAMatchError pins the structural
// reason checkAppliesTo's matchErr branch cannot be driven from that call site,
// which is the only honest way to guard it.
//
// Predicate.Match has exactly two error modes: an EMPTY predicate, and a
// malformed `paths` glob. The admission sub-predicate never carries `paths` (it
// is the plan gate's criterion), and the empty case is filtered ahead of Match
// by the `constrains` guard — so no declaration reachable through the call site
// can make Match error there. The branch is defense in depth behind parse-time
// Predicate.Validate, and the plan-gate call site is where the same posture IS
// exercised for real (TestPlanGateUnmatchedPaths_MalformedGlob_FailsClosed).
//
// This test is the tripwire on that reasoning rather than on the branch: if a
// future phase split routes `paths` — or any error-capable criterion — into the
// admission half, Match errors become reachable at admission and this fails,
// forcing whoever made that change to cover the fail-closed branch for real.
func TestAppliesTo_AdmissionPhase_CannotProduceAMatchError(t *testing.T) {
	// Every shape a workflow can declare, including one whose PATHS half is
	// malformed: the admission sub-predicate must still never error.
	for _, p := range []spec.Predicate{
		{Labels: []string{"dependencies"}},
		{Triggers: []spec.TriggerForm{spec.TriggerScheduled}},
		{ChangeKinds: []string{"feature"}},
		{Labels: []string{"dependencies"}, Paths: []string{"docs/["}},
		{Paths: []string{"docs/["}},
		{},
	} {
		sub, constrains := appliesToPhasePredicate(p, appliesToPhaseAdmission)
		if len(sub.Paths) > 0 {
			t.Fatalf("the admission sub-predicate of %+v carries paths; Match can now error at admission and checkAppliesTo's matchErr branch needs a real test", p)
		}
		if !constrains {
			// Match is never called — the caller admits without evaluating.
			continue
		}
		for _, c := range []spec.Change{
			{}, {Labels: []string{"bug"}, Trigger: spec.TriggerDiff},
		} {
			if _, err := sub.Match(c); err != nil {
				t.Fatalf("admission sub-predicate %+v errored on %+v (%v); the matchErr branch is now reachable and must be driven through checkAppliesTo", sub, c, err)
			}
		}
	}
}

// --- M15: overlapping predicates are BENIGN — applies_to filters, never selects ---

func TestAppliesTo_M15_OverlappingPredicates_AdmitTheRequestedWorkflow(t *testing.T) {
	const overlapping = `version: "2"
workflows:
  wide:
    applies_to:
      labels: [dependencies, chore, bug]
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
  narrow:
    applies_to:
      labels: [dependencies]
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`
	// A `dependencies` change matches BOTH. Each requested workflow is
	// admitted to the workflow the operator NAMED — there is no selection
	// step and therefore no coin flip.
	for _, wf := range []string{"wide", "narrow"} {
		t.Run(wf, func(t *testing.T) {
			s, _, _, _ := newDelegationServer(t)
			w := createRunViaHandler(t, s, map[string]any{
				"repo": "x/y", "workflow_id": wf, "workflow_sha": "abc",
				"trigger_source": "github_issue", "workflow_spec": overlapping,
				"issue_context": issueCtx("dependencies"),
			})
			if w.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201 — overlap is benign because applies_to FILTERS an operator-named workflow:\n%s", w.Code, w.Body.String())
			}
		})
	}
}

// --- satisfyingWorkflows: enumeration, ordering, and the error exclusion ---

func TestSatisfyingWorkflows_EnumeratesAndExcludesUnevaluable(t *testing.T) {
	parsed := &spec.Spec{Workflows: map[string]spec.Workflow{
		"zeta":   {}, // no declaration — accepts anything
		"alpha":  {AppliesTo: &spec.Predicate{Labels: []string{"chore"}}},
		"beta":   {AppliesTo: &spec.Predicate{Labels: []string{"bug"}}},
		"broken": {AppliesTo: &spec.Predicate{Paths: []string{"["}}},
	}}

	// At ADMISSION the paths-only `broken` declaration does not constrain, so
	// it is a genuine alternative and its malformed glob is not yet evaluated
	// — the same deferral M7 pins. `beta`'s labels criterion excludes it.
	got := satisfyingWorkflows(parsed, spec.Change{Labels: []string{"chore"}}, appliesToPhaseAdmission)
	if want := "alpha,broken,zeta"; strings.Join(got, ",") != want {
		t.Errorf("admission satisfyingWorkflows = %v, want %s (name-ordered)", got, want)
	}

	// At the PLAN GATE the malformed glob DOES evaluate, and an unevaluable
	// declaration must never be recommended as a place to take the change.
	got = satisfyingWorkflows(parsed, spec.Change{Paths: []string{"docs/x.md"}}, appliesToPhasePlanGate)
	for _, name := range got {
		if name == "broken" {
			t.Errorf("satisfyingWorkflows = %v, want `broken` EXCLUDED: a declaration that errors cannot be recommended", got)
		}
	}
	if strings.Join(got, ",") != "alpha,beta,zeta" {
		t.Errorf("plan-gate satisfyingWorkflows = %v, want the three workflows that do not constrain paths", got)
	}

	if satisfyingWorkflows(nil, spec.Change{}, appliesToPhaseAdmission) != nil {
		t.Error("a nil spec must enumerate nothing rather than panic")
	}
}

// --- the ONE renderer: shape parity across both rejection points ---

func TestRenderAppliesToRejection_BothPhasesCarryTheSameShape(t *testing.T) {
	base := appliesToRejection{
		WorkflowID: "routine_change",
		Criterion:  "paths",
		Required:   []string{"docs/**"},
		Observed:   []string{"backend/internal/server/runs.go"},
		Satisfying: []string{"feature_change"},
	}
	base.Phase = appliesToPhaseAdmission
	admission := renderAppliesToRejection(base)
	base.ObservedLabel = "scope.files"
	base.Phase = appliesToPhasePlanGate
	planGate := renderAppliesToRejection(base)

	for _, msg := range []string{admission, planGate} {
		for _, want := range []string{"routine_change", "paths", "docs/**", "backend/internal/server/runs.go", "feature_change", "applies_to_override"} {
			if !strings.Contains(msg, want) {
				t.Errorf("message %q is missing %q — an operator refused at the plan gate is further into a run and needs the same help, not less", msg, want)
			}
		}
	}
	if !strings.Contains(planGate, "scope.files") {
		t.Errorf("plan-gate message %q must describe the observed value as scope.files", planGate)
	}
	// A change no workflow accepts says so rather than trailing an empty list.
	base.Satisfying = nil
	if msg := renderAppliesToRejection(base); !strings.Contains(msg, "No workflow in this spec accepts this change") {
		t.Errorf("message %q must state that nothing accepts the change", msg)
	}
}

func TestFirstFailingCriterion_NamesTheCriterion(t *testing.T) {
	c := spec.Change{Labels: []string{"bug"}, Paths: []string{"backend/x.go"}, Trigger: spec.TriggerDiff}
	for _, tc := range []struct {
		name string
		sub  spec.Predicate
		want string
	}{
		{"paths", spec.Predicate{Paths: []string{"docs/**"}}, "paths"},
		{"labels", spec.Predicate{Labels: []string{"chore"}}, "labels"},
		{"change_kind", spec.Predicate{ChangeKinds: []string{"docs"}}, "change_kind"},
		{"trigger", spec.Predicate{Triggers: []spec.TriggerForm{spec.TriggerScheduled}}, "trigger"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, _, ok := firstFailingCriterion(tc.sub, c)
			if !ok || got != tc.want {
				t.Errorf("firstFailingCriterion = (%q, %v), want (%q, true)", got, ok, tc.want)
			}
		})
	}
	if _, _, _, ok := firstFailingCriterion(spec.Predicate{Labels: []string{"bug"}}, c); ok {
		t.Error("firstFailingCriterion reported a failure for a satisfied predicate")
	}
}

// TestTriggerFormForSource pins the documented trigger_source→TriggerForm
// mapping the M5 cases exercise through the handler.
func TestTriggerFormForSource(t *testing.T) {
	for _, src := range []string{"github_issue", "cli", "ui", ""} {
		if got := triggerFormForSource(src); got != spec.TriggerDiff {
			t.Errorf("triggerFormForSource(%q) = %q, want diff — every v0 trigger source produces a code diff", src, got)
		}
	}
}

// TestAdmissionChange_EmptyLabelSetIsNotMatchAll pins the fail-closed reading
// of an absent issue context at the Change-builder level.
func TestAdmissionChange_EmptyLabelSetIsNotMatchAll(t *testing.T) {
	c := admissionChange("cli", nil)
	if len(c.Labels) != 0 {
		t.Errorf("labels = %v, want empty for an issue-less run", c.Labels)
	}
	if len(c.Paths) != 0 {
		t.Error("admissionChange supplied paths; paths must come only from the plan artifact, never a caller self-attestation")
	}
	ok, err := (spec.Predicate{Labels: []string{"chore"}}).Match(c)
	if err != nil || ok {
		t.Errorf("(%v, %v): an EMPTY label set must NOT satisfy a labels criterion", ok, err)
	}
}

// countGlobalAppends counts global-chain APPENDS of a category recorded on
// the fake during the test. Distinct from refinement_test.go's
// countGlobalCategory, which reads back through ListGlobal — the applies_to
// refusal happens pre-insert and is asserted on the write itself.
func countGlobalAppends(au *auditFake, category string) int {
	au.mu.Lock()
	defer au.mu.Unlock()
	n := 0
	for _, g := range au.globalAppended {
		if g.Category == category {
			n++
		}
	}
	return n
}

// countRunScopedAppends counts the run-scoped appends of a category for one
// run. The COUNT — not merely the presence findRunScopedAppend asserts — is
// what a replay can inflate (#2366).
func countRunScopedAppends(au *auditFake, runID uuid.UUID, category string) int {
	au.mu.Lock()
	defer au.mu.Unlock()
	n := 0
	for i := range au.appended {
		if au.appended[i].RunID == runID && au.appended[i].Category == category {
			n++
		}
	}
	return n
}

// findRunScopedAppend returns the first run-scoped append of a category.
func findRunScopedAppend(au *auditFake, runID uuid.UUID, category string) *audit.ChainAppendParams {
	au.mu.Lock()
	defer au.mu.Unlock()
	for i := range au.appended {
		if au.appended[i].RunID == runID && au.appended[i].Category == category {
			return &au.appended[i]
		}
	}
	return nil
}

// TestAppliesTo_RefusalAuditFailure_StillRefuses covers the refusal path's own
// degrade: the audit append is best-effort (warn-logged), but the REFUSAL has
// already been decided and must not be softened by an audit problem. The
// tempting inversion — treating an unwritable audit entry as a reason to admit
// — would make an audit outage a bypass.
func TestAppliesTo_RefusalAuditFailure_StillRefuses(t *testing.T) {
	s, repo, au, _ := newDelegationServer(t)
	au.appendErr = errors.New("audit down")
	body := guardedRunBody(labelsGuard)
	body["issue_context"] = issueCtx("bug")
	w := createRunViaHandler(t, s, body)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 — an audit-append failure must not admit the run:\n%s", w.Code, w.Body.String())
	}
	if n := len(repo.runs); n != 0 {
		t.Errorf("created %d run rows on a refusal whose audit append failed", n)
	}
}

// TestAppliesTo_NoAuditRepo_StillRefuses covers the capability-absent branch of
// the refusal audit: with no audit repository wired the append is skipped and
// the refusal still stands. (The M14 inline-spec case exercises the same
// branch through a differently-configured server; this asserts it directly.)
func TestAppliesTo_NoAuditRepo_StillRefuses(t *testing.T) {
	repo := newFakeRepo()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo})
	w := createRunViaHandler(t, s, func() map[string]any {
		b := guardedRunBody(labelsGuard)
		b["issue_context"] = issueCtx("bug")
		return b
	}())
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 with no audit repo wired:\n%s", w.Code, w.Body.String())
	}
	if n := len(repo.runs); n != 0 {
		t.Errorf("created %d run rows", n)
	}
}

// TestAppliesTo_AdmittedRun_RecordsNoOverrideEntry pins recordAppliesToOverride's
// nil-record no-op: an ordinary admitted run must NOT acquire an override entry.
// Were it recorded unconditionally, every run would carry a standing bypass and
// the plan gate's deferred `paths` check would never fire for anyone.
func TestAppliesTo_AdmittedRun_RecordsNoOverrideEntry(t *testing.T) {
	s, _, au, _ := newDelegationServer(t)
	body := guardedRunBody(labelsGuard)
	body["issue_context"] = issueCtx("dependencies")
	w := createRunViaHandler(t, s, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	var created runResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if findRunScopedAppend(au, created.ID, "run_admitted_applies_to_override") != nil {
		t.Error("an ordinary admitted run recorded an override entry; the plan gate would then never reject anything")
	}
	has, err := s.runHasAppliesToOverride(context.Background(), created.ID)
	if err != nil || has {
		t.Errorf("runHasAppliesToOverride = (%v, %v), want (false, nil) for a run admitted on its merits", has, err)
	}
}

// --- #2366: the Idempotency-Key replay lookup precedes this gate ---
//
// The applies_to override grant is a governance record, not a log line: it is
// a PRECONDITION of admission (M11c), and "how many times was this override
// granted?" must have one answer per run. A replayed create that re-evaluates
// the gate appends a second grant to the global chain for a run that was only
// ever admitted once.

// TestCreateRun_Replay_DoesNotReappendAppliesToOverrideGrant is the issue's
// done-means: a create + replay pair leaves EXACTLY ONE grant entry on each
// chain and one run row. It fails on the pre-#2366 ordering, which re-runs
// checkAppliesTo on the replay and appends a duplicate grant.
func TestCreateRun_Replay_DoesNotReappendAppliesToOverrideGrant(t *testing.T) {
	s, repo, au, _ := newDelegationServer(t)
	body := guardedRunBody(labelsGuard)
	body["issue_context"] = issueCtx("bug")
	body["applies_to_override"] = true
	body["applies_to_override_reason"] = "one-off backport; declaration widening tracked separately"

	w1 := createRunViaHandlerWithKey(t, s, body, "k-override")
	if w1.Code != http.StatusCreated {
		t.Fatalf("first status = %d, want 201:\n%s", w1.Code, w1.Body.String())
	}
	var first runResponse
	if err := json.Unmarshal(w1.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}

	w2 := createRunViaHandlerWithKey(t, s, body, "k-override")
	if w2.Code != http.StatusOK {
		t.Fatalf("replay status = %d, want 200:\n%s", w2.Code, w2.Body.String())
	}
	var second runResponse
	if err := json.Unmarshal(w2.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Errorf("replay returned run %s, want the existing %s", second.ID, first.ID)
	}

	if n := countGlobalAppends(au, "run_admitted_applies_to_override"); n != 1 {
		t.Errorf("global run_admitted_applies_to_override entries = %d, want 1 across create+replay — the override was granted once", n)
	}
	if n := countRunScopedAppends(au, first.ID, "run_admitted_applies_to_override"); n != 1 {
		t.Errorf("run-scoped run_admitted_applies_to_override entries = %d, want 1 across create+replay", n)
	}
	if n := len(repo.runs); n != 1 {
		t.Errorf("run rows = %d, want 1 (the replay must not insert)", n)
	}
}

// TestCreateRun_Replay_DoesNotReevaluateAppliesTo is the falsifiable form of
// the ordering claim, stated as an observable outcome rather than a source
// position: the replayed body is one the gate WOULD REFUSE (override omitted,
// labels unsatisfied), yet it returns the existing run and appends ZERO
// rejection entries. That can only hold if the gate does not run at all on the
// replay path — deciding "the same way" is not available to it here.
func TestCreateRun_Replay_DoesNotReevaluateAppliesTo(t *testing.T) {
	s, repo, au, _ := newDelegationServer(t)
	create := guardedRunBody(labelsGuard)
	create["issue_context"] = issueCtx("bug")
	create["applies_to_override"] = true
	create["applies_to_override_reason"] = "one-off backport"

	w1 := createRunViaHandlerWithKey(t, s, create, "k-refusable")
	if w1.Code != http.StatusCreated {
		t.Fatalf("first status = %d, want 201:\n%s", w1.Code, w1.Body.String())
	}
	var first runResponse
	if err := json.Unmarshal(w1.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}

	// The replay body drops the override and keeps the unsatisfied label, so
	// evaluating it would produce 422 workflow_not_applicable.
	replay := guardedRunBody(labelsGuard)
	replay["issue_context"] = issueCtx("bug")

	w2 := createRunViaHandlerWithKey(t, s, replay, "k-refusable")
	if w2.Code != http.StatusOK {
		t.Fatalf("replay status = %d, want 200 with the existing run — a replay must not be re-admitted:\n%s", w2.Code, w2.Body.String())
	}
	var second runResponse
	if err := json.Unmarshal(w2.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Errorf("replay returned run %s, want the existing %s", second.ID, first.ID)
	}
	if n := countGlobalAppends(au, "run_rejected_applies_to"); n != 0 {
		t.Errorf("run_rejected_applies_to entries = %d, want 0 — the gate ran on a replay", n)
	}
	if n := countGlobalAppends(au, "run_admitted_applies_to_override"); n != 1 {
		t.Errorf("global run_admitted_applies_to_override entries = %d, want 1", n)
	}
	if n := len(repo.runs); n != 1 {
		t.Errorf("run rows = %d, want 1", n)
	}
}
