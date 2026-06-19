package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// scopeCompletenessServer wires an orchestratorRepo (run repo) + auditCapture
// and seeds one implement stage parked in awaiting_scope_decision carrying a
// held-commit ScopeCompletenessPark — the state the decision endpoint acts on.
func scopeCompletenessServer(t *testing.T) (*Server, *orchestratorRepo, *auditCapture, *run.Run, *run.Stage) {
	t.Helper()
	rr := newOrchestratorRepo()
	runRow := rr.seedRun()
	stage := rr.seedStage(runRow.ID, 1, run.StageStateAwaitingScopeDecision)
	stage.Type = run.StageTypeImplement
	stage.ScopeCompletenessPark = &run.ScopeCompletenessPark{
		HeldCommitSHA:   "1111111111111111111111111111111111111111",
		RunBranch:       "fishhawk/run-aaa/slice-0",
		VerifiedTreeSHA: "2222222222222222222222222222222222222222",
		MissingPaths:    []string{"backend/internal/foo/foo_test.go"},
	}
	au := &auditCapture{}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: au})
	return s, rr, au, runRow, stage
}

func postScopeCompletenessDecision(t *testing.T, s *Server, pathRunID uuid.UUID, body string, decorate func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost,
		"/v0/runs/"+pathRunID.String()+"/scope-completeness/decision",
		strings.NewReader(body))
	req.SetPathValue("run_id", pathRunID.String())
	if decorate != nil {
		req = decorate(req)
	}
	w := httptest.NewRecorder()
	s.handleDecideScopeCompleteness(w, req)
	return w
}

func operatorWriteStages(r *http.Request) *http.Request {
	return withOperatorIdentity(r, "write:stages")
}

// TestDecideScopeCompleteness_Exempt resumes the parked stage to running so
// the held commit's PR can be opened with NO agent re-run (the decision
// endpoint never fails the stage nor re-dispatches it), and appends the
// scope_completeness_exempted entry carrying the held commit + gate_evidence.
func TestDecideScopeCompleteness_Exempt(t *testing.T) {
	s, rr, au, runRow, stage := scopeCompletenessServer(t)

	w := postScopeCompletenessDecision(t, s, runRow.ID,
		`{"decision":"exempt","reason":"the coupled test file is genuinely already covered"}`,
		operatorWriteStages)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}

	var resp scopeCompletenessDecisionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Decision != "exempt" || resp.State != string(run.StageStateRunning) {
		t.Errorf("response = %+v, want exempt/running", resp)
	}
	if resp.HeldCommitSHA != "1111111111111111111111111111111111111111" {
		t.Errorf("response held_commit_sha = %q, want the parked held commit", resp.HeldCommitSHA)
	}

	got, _ := rr.GetStage(context.Background(), stage.ID)
	if got.State != run.StageStateRunning {
		t.Errorf("stage state = %q, want running (exempt resumes for PR-open, not failure)", got.State)
	}
	// Zero-re-run at this layer: the decision endpoint must NOT fail the stage
	// (the full agent-called-once invariant is asserted by the cross-layer
	// e2e). A failure here would mean it dropped to category-B, not exempt.
	if got.FailureCategory != nil {
		t.Errorf("stage failure category = %v, want nil (exempt never fails the stage)", got.FailureCategory)
	}

	au.mu.Lock()
	defer au.mu.Unlock()
	if len(au.appended) != 1 || au.appended[0].Category != CategoryScopeCompletenessExempted {
		t.Fatalf("audit = %+v, want one scope_completeness_exempted entry", au.appended)
	}
	var payload map[string]any
	_ = json.Unmarshal(au.appended[0].Payload, &payload)
	if payload["held_commit_sha"] != "1111111111111111111111111111111111111111" {
		t.Errorf("exempt payload held_commit_sha = %v", payload["held_commit_sha"])
	}
	if payload["gate_evidence"] != CategoryScopeCompletenessExempted {
		t.Errorf("exempt payload gate_evidence = %v, want the #1153 channel marker", payload["gate_evidence"])
	}
}

// TestDecideScopeCompleteness_Fail drops the parked stage to category-B
// (today's restore path) and appends the scope_completeness_failed entry.
func TestDecideScopeCompleteness_Fail(t *testing.T) {
	s, rr, au, runRow, stage := scopeCompletenessServer(t)

	w := postScopeCompletenessDecision(t, s, runRow.ID,
		`{"decision":"fail","reason":"the missing files are load-bearing; re-scope"}`,
		operatorWriteStages)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}

	got, _ := rr.GetStage(context.Background(), stage.ID)
	if got.State != run.StageStateFailed {
		t.Errorf("stage state = %q, want failed", got.State)
	}
	if got.FailureCategory == nil || *got.FailureCategory != run.FailureB {
		t.Errorf("stage failure category = %v, want B", got.FailureCategory)
	}

	au.mu.Lock()
	defer au.mu.Unlock()
	if len(au.appended) != 1 || au.appended[0].Category != CategoryScopeCompletenessFailed {
		t.Fatalf("audit = %+v, want one scope_completeness_failed entry", au.appended)
	}
}

// TestDecideScopeCompleteness_NotParked409 pins the gate: a decision on a run
// whose implement stage is not parked in awaiting_scope_decision is rejected.
func TestDecideScopeCompleteness_NotParked409(t *testing.T) {
	rr := newOrchestratorRepo()
	runRow := rr.seedRun()
	stage := rr.seedStage(runRow.ID, 1, run.StageStateRunning) // still running, not parked
	stage.Type = run.StageTypeImplement
	au := &auditCapture{}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: au})

	w := postScopeCompletenessDecision(t, s, runRow.ID,
		`{"decision":"exempt","reason":"r"}`, operatorWriteStages)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "stage_not_parked") {
		t.Errorf("body: %s", w.Body.String())
	}
}

// TestDecideScopeCompleteness_RunBoundToken403 pins the operator-only gate:
// the implement agent's own run-bound token may never decide its exemption.
func TestDecideScopeCompleteness_RunBoundToken403(t *testing.T) {
	s, _, _, runRow, _ := scopeCompletenessServer(t)
	w := postScopeCompletenessDecision(t, s, runRow.ID, `{"decision":"exempt","reason":"r"}`,
		func(r *http.Request) *http.Request {
			return withRunBoundIdentity(r, runRow.ID, "mcp:read", "write:scope-amendments")
		})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "self_decision") {
		t.Errorf("body: %s", w.Body.String())
	}
}

// TestDecideScopeCompleteness_MissingScope403 pins the token-scope gate.
func TestDecideScopeCompleteness_MissingScope403(t *testing.T) {
	s, _, _, runRow, _ := scopeCompletenessServer(t)
	w := postScopeCompletenessDecision(t, s, runRow.ID, `{"decision":"exempt","reason":"r"}`,
		func(r *http.Request) *http.Request { return withOperatorIdentity(r, "read:runs") })
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "insufficient_scope") {
		t.Errorf("body: %s", w.Body.String())
	}
}

// TestDecideScopeCompleteness_BadDecision400 / empty reason pin the input
// validation: only exempt|fail with a non-empty reason is accepted.
func TestDecideScopeCompleteness_BadInput400(t *testing.T) {
	s, _, _, runRow, _ := scopeCompletenessServer(t)
	for _, body := range []string{
		`{"decision":"maybe","reason":"r"}`,   // bad enum
		`{"decision":"exempt","reason":""}`,   // empty reason
		`{"decision":"exempt","reason":"  "}`, // whitespace-only reason
		`{"reason":"r"}`,                      // missing decision
	} {
		w := postScopeCompletenessDecision(t, s, runRow.ID, body, operatorWriteStages)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400; body = %s", body, w.Code, w.Body.String())
		}
	}
}

// TestDecideScopeCompleteness_UnknownRun404 pins the run lookup.
func TestDecideScopeCompleteness_UnknownRun404(t *testing.T) {
	s, _, _, _, _ := scopeCompletenessServer(t)
	w := postScopeCompletenessDecision(t, s, uuid.New(), `{"decision":"exempt","reason":"r"}`,
		operatorWriteStages)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", w.Code, w.Body.String())
	}
}
