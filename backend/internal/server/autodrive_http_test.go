package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/delegation"
	"github.com/kuhlman-labs/fishhawk/backend/internal/drive"
	"github.com/kuhlman-labs/fishhawk/backend/internal/operatorrole"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// autodrive_http_test.go pins the two HTTP surfaces #1700 adds: POST
// /v0/runs/{run_id}/auto-drive (the local drive verb's gate call) and POST
// /v0/runs/{run_id}/auto-drive/acts (its record-before-dispatch call). One
// behavioral test per enumerated auth/failure mode, plus the cross-boundary
// acted-approve (HTTP → server → fake repo → audit-readback) that pins the
// delegated-approval-advances-the-gate semantic AND the binding conditions:
// the primary approval_submitted row carries delegated context independent
// of the supplementary run_auto_driven row (condition 2), and a failed
// supplementary append after a gate act fails LOUD (condition 1).
//
// The harness is the shared fake stack (newAutoDriveServer + the audit /
// concern fakes that read back their own writes), matching autodrive_test.go
// — the sibling AutoDriveRunGate seam file — so the HTTP wrapper is exercised
// end-to-end in-process without a per-package Postgres. The audit fake IS the
// server's AuditRepo, so asserting on its appended entries is a genuine
// read-back of the server's write path.

// autoDriveOperatorIdentity is the operator-agent HTTP identity the drive
// verb calls under: an operator-agent subject (→ audit.ActorAgent via
// actorKindForSubject) holding write:approvals, TokenID set so the handler's
// scope check applies (scope-acceptance parity, not the cookie bypass).
func autoDriveOperatorIdentity() Identity {
	return Identity{
		Subject: operatorrole.CampaignActorSubject,
		TokenID: "tok-op-agent",
		Scopes:  []string{"write:approvals"},
	}
}

// autoDrivePost issues POST run_id/auto-drive[/acts] against the handler
// directly with an injected identity, returning the recorder.
func autoDrivePost(t *testing.T, s *Server, handler http.HandlerFunc, runID uuid.UUID, suffix, body string, id Identity) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	var rdr *strings.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	} else {
		rdr = strings.NewReader("")
	}
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+runID.String()+"/auto-drive"+suffix, rdr)
	req.SetPathValue("run_id", runID.String())
	req = injectIdentity(req, id)
	handler(w, req)
	return w
}

// autoDrivenActRow returns the single appended run_auto_driven entry, decoded
// to its act discriminator + fields, or fails if not exactly one exists.
func autoDrivenActRow(t *testing.T, au *auditFake) (audit.ChainAppendParams, map[string]any) {
	t.Helper()
	e := auditEntry(t, au, CategoryRunAutoDriven)
	var fields map[string]any
	if err := json.Unmarshal(e.Payload, &fields); err != nil {
		t.Fatalf("unmarshal run_auto_driven payload: %v", err)
	}
	return e, fields
}

// --- route registration -----------------------------------------------------

func TestAutoDriveRouteRegistered(t *testing.T) {
	s := New(Config{})
	for _, suffix := range []string{"", "/acts"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v0/runs/00000000-0000-0000-0000-000000000000/auto-drive"+suffix, strings.NewReader("{}"))
		s.Handler().ServeHTTP(rec, req)
		// An UNregistered route 404s from the mux; the handler's anonymous
		// guard 401s — so a 401 proves the route reached the handler.
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("suffix %q: status = %d, want 401 (route reaches handler)", suffix, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "authentication_required") {
			t.Errorf("suffix %q: body = %s, want authentication_required", suffix, rec.Body.String())
		}
	}
}

// --- (1) 401 unauthenticated -------------------------------------------------

func TestAutoDrive_Unauthenticated(t *testing.T) {
	s, repo, _, _ := newAutoDriveServer(t)
	runID, _ := startAutoDriveRun(t, s, repo)
	for _, tc := range []struct {
		suffix  string
		handler http.HandlerFunc
	}{
		{"", s.handleAutoDrive},
		{"/acts", s.handleAutoDriveRecordAct},
	} {
		w := autoDrivePost(t, s, tc.handler, runID, tc.suffix, "{}", anonIdentity())
		if w.Code != http.StatusUnauthorized {
			t.Errorf("suffix %q: status = %d, want 401", tc.suffix, w.Code)
		}
	}
}

// --- (2) 403 insufficient scope ---------------------------------------------

func TestAutoDrive_InsufficientScope(t *testing.T) {
	s, repo, _, _ := newAutoDriveServer(t)
	runID, _ := startAutoDriveRun(t, s, repo)
	noScope := Identity{Subject: "github:op", TokenID: "tok-x", Scopes: []string{"mcp:read"}}
	for _, tc := range []struct {
		suffix  string
		handler http.HandlerFunc
	}{
		{"", s.handleAutoDrive},
		{"/acts", s.handleAutoDriveRecordAct},
	} {
		w := autoDrivePost(t, s, tc.handler, runID, tc.suffix, `{"action":"dispatch_stage","stage":"plan","source":"fishhawk_drive_run"}`, noScope)
		assertScopeError(t, w, http.StatusForbidden, "insufficient_scope")
	}
}

// --- (3) 404 unknown run -----------------------------------------------------

func TestAutoDrive_UnknownRun(t *testing.T) {
	s, _, au, _ := newAutoDriveServer(t)
	unknown := uuid.New()
	for _, tc := range []struct {
		suffix  string
		handler http.HandlerFunc
	}{
		{"", s.handleAutoDrive},
		{"/acts", s.handleAutoDriveRecordAct},
	} {
		w := autoDrivePost(t, s, tc.handler, unknown, tc.suffix, `{"action":"dispatch_stage","stage":"plan","source":"fishhawk_drive_run"}`, autoDriveOperatorIdentity())
		assertScopeError(t, w, http.StatusNotFound, "run_not_found")
	}
	if countAudit(au, CategoryRunAutoDriven) != 0 {
		t.Error("a run_auto_driven row was appended for an unknown run")
	}
}

// --- (4) observe-only fail-closed (no operator_agent) ------------------------

func TestAutoDrive_ObserveOnlyNoDelegation(t *testing.T) {
	s, repo, au, _ := newAutoDriveServer(t)
	// A spec with no operator_agent block: evaluateRunDelegation returns a nil
	// Result → observe-only, no state change, no act row.
	runID, _ := startDriveE2ERun(t, s, repo.driveE2ERepo, map[string]any{
		"repo": "x/y", "workflow_id": "feature_change", "workflow_sha": "abc",
		"trigger_source": "cli", "workflow_spec": gatedSpecYAML,
	})
	w := autoDrivePost(t, s, s.handleAutoDrive, runID, "", "{}", autoDriveOperatorIdentity())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	var out autoDriveResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Acted || out.Paged {
		t.Errorf("outcome = %+v, want observe-only (not acted, not paged)", out)
	}
	if countAudit(au, CategoryRunAutoDriven) != 0 {
		t.Error("run_auto_driven row appended on an observe-only outcome")
	}
}

// --- (5) CROSS-BOUNDARY end-to-end acted approve -----------------------------

func TestAutoDrive_ActedApprove_EndToEnd(t *testing.T) {
	s, repo, au, _ := newAutoDriveServer(t)
	runID, stages := startAutoDriveRun(t, s, repo)
	plan := stages[0]
	seedReviewEntry(t, au, runID, 1, "plan_review_started", planreview.ReviewStartedPayload{ConfiguredAgents: 2})
	seedReviewEntry(t, au, runID, 2, "plan_reviewed", planreview.PlanReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApprove})
	seedReviewEntry(t, au, runID, 3, "plan_reviewed", planreview.PlanReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApprove})

	w := autoDrivePost(t, s, s.handleAutoDrive, runID, "", "{}", autoDriveOperatorIdentity())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	var out autoDriveResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Acted || out.Action != delegation.ActionApprove {
		t.Fatalf("outcome = %+v, want acted approve", out)
	}
	// The delegated approve advanced the plan gate (semantic pin).
	if plan.State == run.StageStateAwaitingApproval {
		t.Errorf("plan stage still awaiting_approval; delegated approve did not advance the gate")
	}
	// Condition 2: the primary approval_submitted row carries the delegated
	// rule + agent actor, INDEPENDENT of the supplementary run_auto_driven row.
	primary := auditEntry(t, au, "approval_submitted")
	assertOperatorActor(t, primary)
	if rule := auditDelegatedRule(t, primary); rule != "clean_dual_approval" {
		t.Errorf("approval_submitted delegated rule = %q, want clean_dual_approval", rule)
	}
	// The supplementary run_auto_driven act:gate row landed with attribution.
	gateRow, fields := autoDrivenActRow(t, au)
	assertOperatorActor(t, gateRow)
	if fields["act"] != autoDriveActGate {
		t.Errorf("run_auto_driven act = %v, want %q", fields["act"], autoDriveActGate)
	}
	if fields["action"] != delegation.ActionApprove {
		t.Errorf("run_auto_driven action = %v, want %q", fields["action"], delegation.ActionApprove)
	}
	if fields["source"] != autoDriveSourceEndpoint {
		t.Errorf("run_auto_driven source = %v, want %q", fields["source"], autoDriveSourceEndpoint)
	}
	// The attribution row carries delegation provenance — the delegated rule
	// that governed the acted gate — not just act/action/source. This is the
	// authoritative check of the real write path (the audit fake IS the
	// server's AuditRepo), so a rule-less gate row (the concern-1 regression)
	// fails here.
	if fields["delegated_rule"] != "clean_dual_approval" {
		t.Errorf("run_auto_driven delegated_rule = %v, want clean_dual_approval", fields["delegated_rule"])
	}
}

// --- (6) paged: gating reviewer reject --------------------------------------

func TestAutoDrive_Paged_ReviewerReject(t *testing.T) {
	s, repo, au, _ := newAutoDriveServer(t)
	runID, stages := startAutoDriveRun(t, s, repo)
	plan, impl := stages[0], stages[1]
	plan.State = run.StageStateSucceeded
	impl.State = run.StageStateAwaitingApproval
	seedReviewEntry(t, au, runID, 1, "implement_review_started", planreview.ReviewStartedPayload{ConfiguredAgents: 2})
	seedReviewEntry(t, au, runID, 2, "implement_reviewed", planreview.ImplementReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApprove})
	seedReviewEntry(t, au, runID, 3, "implement_reviewed", planreview.ImplementReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictReject})

	w := autoDrivePost(t, s, s.handleAutoDrive, runID, "", "{}", autoDriveOperatorIdentity())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	var out autoDriveResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Acted || !out.Paged || out.PageEvent == "" {
		t.Fatalf("outcome = %+v, want paged with page_event", out)
	}
	if countAudit(au, CategoryCampaignGatePaged) != 1 {
		t.Error("campaign_gate_paged not appended on a must_page reject")
	}
	// No gate action, and NO supplementary run_auto_driven row (only Acted
	// outcomes append the gate-act row).
	if countAudit(au, CategoryRunAutoDriven) != 0 {
		t.Error("run_auto_driven row appended on a paged outcome")
	}
}

// --- (7) merge fail-closed on nil GateMerger --------------------------------

func TestAutoDrive_MergeFailClosed_NilMerger(t *testing.T) {
	s, repo, au := newAutoDriveMergeServer(t, nil) // no GateMerger configured
	runID := seedMergeReadyRun(t, s, repo, au)

	w := autoDrivePost(t, s, s.handleAutoDrive, runID, "", "{}", autoDriveOperatorIdentity())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	var out autoDriveResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Acted || out.Paged {
		t.Errorf("outcome = %+v, want observe-only (merge fail-closed on nil merger)", out)
	}
	if countAudit(au, CategoryRunAutoDriven) != 0 {
		t.Error("run_auto_driven row appended when merge was fail-closed observe-only")
	}
}

// --- (8) gate-action dispatch error surfaces (not swallowed) -----------------

func TestAutoDrive_MergeDispatchError_Surfaces(t *testing.T) {
	merger := &fakeMerger{err: errors.New("enable auto-merge boom")}
	s, repo, au := newAutoDriveMergeServer(t, merger)
	runID := seedMergeReadyRun(t, s, repo, au)

	w := autoDrivePost(t, s, s.handleAutoDrive, runID, "", "{}", autoDriveOperatorIdentity())
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (dispatch error surfaced):\n%s", w.Code, w.Body.String())
	}
	var env errorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Error.Code != "auto_drive_dispatch_failed" {
		t.Errorf("error code = %q, want auto_drive_dispatch_failed", env.Error.Code)
	}
}

// --- (5b, condition 1) append-failure after a gate act fails LOUD ------------

func TestAutoDrive_SupplementaryAppendFailure_FailsLoud(t *testing.T) {
	inner := newAuditFake()
	au := &failCategoryAudit{auditFake: inner, failCategory: CategoryRunAutoDriven}
	repo := &autoDriveRepo{driveE2ERepo: &driveE2ERepo{fakeRepo: newFakeRepo()}}
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      repo,
		AuditRepo:    au,
		ConcernRepo:  newFakeConcernRepo(),
		ApprovalRepo: newFakeApprovalRepo(),
		Orchestrator: &orchestrator.Orchestrator{Runs: repo},
	})
	runID, stages := startAutoDriveRun(t, s, repo)
	plan := stages[0]
	seedReviewEntry(t, inner, runID, 1, "plan_review_started", planreview.ReviewStartedPayload{ConfiguredAgents: 2})
	seedReviewEntry(t, inner, runID, 2, "plan_reviewed", planreview.PlanReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApprove})
	seedReviewEntry(t, inner, runID, 3, "plan_reviewed", planreview.PlanReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApprove})

	w := autoDrivePost(t, s, s.handleAutoDrive, runID, "", "{}", autoDriveOperatorIdentity())
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (append failed loud):\n%s", w.Code, w.Body.String())
	}
	var env errorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Error.Code != "auto_drive_record_failed" {
		t.Errorf("error code = %q, want auto_drive_record_failed", env.Error.Code)
	}
	// Condition 2: even in the append-failure window the gate action landed —
	// the plan gate advanced and its OWN approval_submitted row (the
	// authoritative delegation record) is durable with delegation context.
	if plan.State == run.StageStateAwaitingApproval {
		t.Error("plan gate did not advance; the primary action should still have committed")
	}
	primary := auditEntry(t, inner, "approval_submitted")
	if rule := auditDelegatedRule(t, primary); rule != "clean_dual_approval" {
		t.Errorf("approval_submitted delegated rule = %q, want clean_dual_approval (authoritative record intact)", rule)
	}
}

// --- (9) record-act happy path ----------------------------------------------

func TestAutoDriveRecordAct_HappyPath(t *testing.T) {
	s, repo, au, _ := newAutoDriveServer(t)
	runID, _ := startAutoDriveRun(t, s, repo)

	w := autoDrivePost(t, s, s.handleAutoDriveRecordAct, runID, "/acts",
		`{"action":"dispatch_stage","stage":"implement","source":"fishhawk_drive_run","note":"driving"}`,
		autoDriveOperatorIdentity())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	var resp recordAutoDriveActResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Act != autoDriveActDispatch || resp.Stage != "implement" {
		t.Errorf("resp = %+v, want act=dispatch stage=implement", resp)
	}
	// Read back the appended run_auto_driven act:dispatch row (via the audit
	// fake that IS the server's AuditRepo) with the full payload + agent actor.
	row, fields := autoDrivenActRow(t, au)
	assertOperatorActor(t, row)
	if fields["act"] != autoDriveActDispatch {
		t.Errorf("act = %v, want dispatch", fields["act"])
	}
	if fields["action"] != autoDriveDispatchAction {
		t.Errorf("action = %v, want dispatch_stage", fields["action"])
	}
	if fields["stage"] != "implement" || fields["source"] != "fishhawk_drive_run" {
		t.Errorf("fields = %v, want stage=implement source=fishhawk_drive_run", fields)
	}
	// Confirm it is discoverable via the run's audit stream (ListForRun is the
	// backing read for GET /v0/runs/{run_id}/audit).
	entries, err := au.ListForRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListForRun: %v", err)
	}
	var seen bool
	for _, e := range entries {
		if e.Category == CategoryRunAutoDriven {
			seen = true
		}
	}
	if !seen {
		t.Error("run_auto_driven not present in the run's audit stream")
	}
}

// --- (10) record-act rejection table (each appends nothing) -----------------

func TestAutoDriveRecordAct_RejectionTable(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		field string
	}{
		{"missing action", `{"stage":"plan","source":"fishhawk_drive_run"}`, "action"},
		{"empty action", `{"action":"  ","stage":"plan","source":"fishhawk_drive_run"}`, "action"},
		{"bogus action", `{"action":"merge_it","stage":"plan","source":"fishhawk_drive_run"}`, "action"},
		{"missing stage", `{"action":"dispatch_stage","source":"fishhawk_drive_run"}`, "stage"},
		{"bogus stage", `{"action":"dispatch_stage","stage":"deploy","source":"fishhawk_drive_run"}`, "stage"},
		{"missing source", `{"action":"dispatch_stage","stage":"plan"}`, "source"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, repo, au, _ := newAutoDriveServer(t)
			runID, _ := startAutoDriveRun(t, s, repo)
			w := autoDrivePost(t, s, s.handleAutoDriveRecordAct, runID, "/acts", tc.body, autoDriveOperatorIdentity())
			assertScopeError(t, w, http.StatusBadRequest, "validation_failed")
			var env errorEnvelope
			_ = json.Unmarshal(w.Body.Bytes(), &env)
			if got, _ := env.Error.Details["field"].(string); got != tc.field {
				t.Errorf("field = %q, want %q (body: %s)", got, tc.field, w.Body.String())
			}
			if countAudit(au, CategoryRunAutoDriven) != 0 {
				t.Error("a run_auto_driven row was appended on a rejected record-act")
			}
		})
	}
}

// --- shared harness for the merge modes --------------------------------------

// failCategoryAudit wraps an auditFake and injects an AppendChained failure
// for exactly one category, leaving every other write (the authoritative
// approval_submitted row) succeeding — the append-failure window condition 1
// requires.
type failCategoryAudit struct {
	*auditFake
	failCategory string
}

func (a *failCategoryAudit) AppendChained(ctx context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	if p.Category == a.failCategory {
		return nil, errors.New("injected append failure for " + p.Category)
	}
	return a.auditFake.AppendChained(ctx, p)
}

func newAutoDriveMergeServer(t *testing.T, merger GitHubMerger) (*Server, *autoDriveRepo, *auditFake) {
	t.Helper()
	repo := &autoDriveRepo{driveE2ERepo: &driveE2ERepo{fakeRepo: newFakeRepo()}}
	au := newAuditFake()
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      repo,
		AuditRepo:    au,
		ConcernRepo:  newFakeConcernRepo(),
		ApprovalRepo: newFakeApprovalRepo(),
		Orchestrator: &orchestrator.Orchestrator{Runs: repo},
		GateMerger:   merger,
	})
	return s, repo, au
}

// seedMergeReadyRun walks a run to the may_merge-Met, gates-resolved,
// acceptance-not-declared merge-ready state (mirrors TestAutoDriveRunGate_Merge).
func seedMergeReadyRun(t *testing.T, s *Server, repo *autoDriveRepo, au *auditFake) uuid.UUID {
	t.Helper()
	runID, stages := startAutoDriveRun(t, s, repo)
	stages[0].State = run.StageStateSucceeded
	stages[1].State = run.StageStateSucceeded
	if _, err := repo.TransitionRun(context.Background(), runID, run.StateRunning); err != nil {
		t.Fatalf("TransitionRun -> running: %v", err)
	}
	if _, err := repo.SetRunPullRequestURL(context.Background(), runID, "https://github.com/x/y/pull/7"); err != nil {
		t.Fatalf("SetRunPullRequestURL: %v", err)
	}
	// Latest drive auto-advance is checks_green_awaiting_merge → may_merge Met.
	seedReviewEntry(t, au, runID, 5, drive.Category, drive.Advance{Rule: drive.RuleChecksGreenAwaitingMerge})
	return runID
}
