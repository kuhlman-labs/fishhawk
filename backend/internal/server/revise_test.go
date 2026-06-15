package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// newReviseServer wires RunRepo + AuditRepo for the plan-revise handler
// and seeds a plan stage in the given state.
func newReviseServer(t *testing.T, runID, stageID uuid.UUID, state run.StageState, stageType run.StageType) (*Server, *promptRunRepo, *auditFake) {
	t.Helper()
	rr := newPromptRunRepo()
	au := newAuditFake()
	rr.getStages[stageID] = &run.Stage{ID: stageID, RunID: runID, Type: stageType, State: state}
	rr.getRuns[runID] = &run.Run{ID: runID, Repo: "kuhlman-labs/example", WorkflowID: "feature_change"}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: au})
	return s, rr, au
}

// seedPlanRevised adds a prior plan_revised audit entry for the stage so
// budget-counting tests can simulate a consumed revise pass.
func seedPlanRevised(au *auditFake, runID, stageID uuid.UUID) {
	rid := runID
	sid := stageID
	payload, _ := json.Marshal(map[string]any{
		"stage_id":   stageID.String(),
		"conditions": "prior constraint",
	})
	au.seeded = append(au.seeded, &audit.Entry{
		RunID:    &rid,
		StageID:  &sid,
		Category: CategoryPlanRevised,
		Payload:  payload,
	})
}

// revisePlan posts a revise body to the handler with an authenticated
// session identity (bypasses the scope guard like the approval tests).
func revisePlan(t *testing.T, s *Server, stageID uuid.UUID, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost,
		"/v0/stages/"+stageID.String()+"/revise", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("stage_id", stageID.String())
	w := httptest.NewRecorder()
	s.handleRevisePlan(w, withAuth(req))
	return w
}

func TestRevisePlan_HappyPath_ReopensPlanStage(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, rr, au := newReviseServer(t, runID, stageID, run.StageStateAwaitingApproval, run.StageTypePlan)

	w := revisePlan(t, s, stageID,
		`{"constraint":"use the existing retry helper, do not add a new backoff package"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	var got stageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.State != string(run.StageStatePending) {
		t.Errorf("State = %q, want pending", got.State)
	}

	// Stage transitioned AwaitingApproval → Pending.
	var sawResume bool
	for _, c := range rr.transitionStageCalls {
		if c.To == run.StageStatePending {
			sawResume = true
		}
	}
	if !sawResume {
		t.Errorf("stage was not transitioned to pending; transitions=%v", rr.transitionStageCalls)
	}

	// A dedicated plan_revised entry was written carrying the constraint as
	// the conditions blob plus the receipt fields.
	if len(au.appended) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(au.appended))
	}
	got0 := au.appended[0]
	if got0.Category != CategoryPlanRevised {
		t.Errorf("audit category = %q, want %q", got0.Category, CategoryPlanRevised)
	}
	if !bytes.Contains(got0.Payload, []byte("use the existing retry helper")) {
		t.Errorf("audit payload missing the constraint: %s", got0.Payload)
	}
	if !bytes.Contains(got0.Payload, []byte(`"conditions"`)) {
		t.Errorf("audit payload missing the conditions key the prompt renderer reads: %s", got0.Payload)
	}
	if !bytes.Contains(got0.Payload, []byte(`"pass_ordinal":1`)) {
		t.Errorf("audit payload missing pass_ordinal receipt: %s", got0.Payload)
	}
	if !bytes.Contains(got0.Payload, []byte(`"forced":false`)) {
		t.Errorf("audit payload missing forced=false: %s", got0.Payload)
	}
}

func TestRevisePlan_WrongState_409(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, _, _ := newReviseServer(t, runID, stageID, run.StageStateRunning, run.StageTypePlan)

	w := revisePlan(t, s, stageID, `{"constraint":"do the thing"}`)
	assertScopeError(t, w, http.StatusConflict, "revise_not_applicable")
}

func TestRevisePlan_NonPlanStage_409(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, _, _ := newReviseServer(t, runID, stageID, run.StageStateAwaitingApproval, run.StageTypeImplement)

	w := revisePlan(t, s, stageID, `{"constraint":"do the thing"}`)
	assertScopeError(t, w, http.StatusConflict, "revise_not_applicable")
}

func TestRevisePlan_EmptyConstraint_400(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, _, _ := newReviseServer(t, runID, stageID, run.StageStateAwaitingApproval, run.StageTypePlan)

	w := revisePlan(t, s, stageID, `{"constraint":"   "}`)
	assertScopeError(t, w, http.StatusBadRequest, "validation_failed")
}

func TestRevisePlan_BudgetExhausted_409(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, _, au := newReviseServer(t, runID, stageID, run.StageStateAwaitingApproval, run.StageTypePlan)
	// One prior revise pass already consumed against a default budget of 1.
	seedPlanRevised(au, runID, stageID)

	w := revisePlan(t, s, stageID, `{"constraint":"another tweak"}`)
	assertScopeError(t, w, http.StatusConflict, "revise_budget_exhausted")
}

func TestRevisePlan_ForceGrantsPassPastBudget(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, _, au := newReviseServer(t, runID, stageID, run.StageStateAwaitingApproval, run.StageTypePlan)
	// Normal budget (1) spent; the operator override grants one more pass.
	seedPlanRevised(au, runID, stageID)

	w := revisePlan(t, s, stageID,
		`{"constraint":"one more tweak","force_additional_pass":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	// The forced pass is durably attributable.
	var forcedSeen bool
	for _, p := range au.appended {
		if p.Category == CategoryPlanRevised && bytes.Contains(p.Payload, []byte(`"forced":true`)) {
			forcedSeen = true
		}
	}
	if !forcedSeen {
		t.Errorf("expected a plan_revised entry with forced=true")
	}
}

func TestRevisePlan_CeilingReached_409(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, _, au := newReviseServer(t, runID, stageID, run.StageStateAwaitingApproval, run.StageTypePlan)
	// Three prior passes = the hard ceiling; even a forced pass is refused.
	seedPlanRevised(au, runID, stageID)
	seedPlanRevised(au, runID, stageID)
	seedPlanRevised(au, runID, stageID)

	w := revisePlan(t, s, stageID,
		`{"constraint":"past the ceiling","force_additional_pass":true}`)
	assertScopeError(t, w, http.StatusConflict, "revise_ceiling_reached")
}

// TestRevisePlan_CrossLayer_ReviseResumePrompt crosses request →
// persistence → prompt-render: a plan stage parked at awaiting_approval
// is revised through the endpoint (request → plan_revised audit), the
// stage resumes to Pending, and GET /v0/stages/{id}/prompt renders the
// operator's binding constraint in the dedicated "Revision constraint"
// section (audit → prompt render). The full base-plan seam is covered by
// the integration test; here the base load no-ops (no ArtifactRepo) so
// the constraint binds without the base block.
func TestRevisePlan_CrossLayer_ReviseResumePrompt(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()

	sf := newSigningFake()
	priv, _ := sf.issue(t, runID)
	rr := newPromptRunRepo()
	au := newAuditFake()
	rr.getStages[stageID] = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypePlan, State: run.StageStateAwaitingApproval}
	rr.getRuns[runID] = &run.Run{ID: runID, Repo: "kuhlman-labs/example", WorkflowID: "feature_change", TriggerSource: run.TriggerCLI}

	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: au, SigningRepo: sf})
	s.promptIssueGetterOverride = &stubIssueGetter{}

	w := revisePlan(t, s, stageID,
		`{"constraint":"keep the change additive; do not bump the schema major version"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("revise status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if got := rr.getStages[stageID].State; got != run.StageStatePending {
		t.Fatalf("stage state after revise = %q, want pending", got)
	}

	pw := promptRequest(t, s, runID, stageID, priv, "")
	if pw.Code != http.StatusOK {
		t.Fatalf("prompt status = %d, want 200:\n%s", pw.Code, pw.Body.String())
	}
	var resp promptResponse
	if err := json.NewDecoder(pw.Body).Decode(&resp); err != nil {
		t.Fatalf("decode prompt: %v", err)
	}
	if !strings.Contains(resp.Prompt, "Revision constraint (binding") {
		t.Errorf("re-dispatched plan prompt missing the Revision constraint section:\n%s", resp.Prompt)
	}
	if !strings.Contains(resp.Prompt, "keep the change additive") {
		t.Errorf("re-dispatched plan prompt missing the operator's constraint text:\n%s", resp.Prompt)
	}
}

// TestLoadRevisionConstraint_NewestWins confirms the loader returns the
// most recent plan_revised entry's rendered conditions and caps the blob.
func TestLoadRevisionConstraint_NewestWins(t *testing.T) {
	runID := uuid.New()
	au := newAuditFake()
	rid := runID
	older, _ := json.Marshal(map[string]any{"conditions": "OLD constraint"})
	newer, _ := json.Marshal(map[string]any{"conditions": "NEW constraint"})
	au.seeded = append(au.seeded,
		&audit.Entry{RunID: &rid, Category: CategoryPlanRevised, Payload: older},
		&audit.Entry{RunID: &rid, Category: CategoryPlanRevised, Payload: newer},
	)
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au})
	got := s.loadRevisionConstraint(context.Background(), runID)
	if got == nil || *got != "NEW constraint" {
		t.Errorf("loadRevisionConstraint = %v, want \"NEW constraint\"", got)
	}
}

func TestRevisePlan_InsufficientScope(t *testing.T) {
	s := scopeTestServer()
	stageID := uuid.New()
	req := httptest.NewRequest(http.MethodPost,
		"/v0/stages/"+stageID.String()+"/revise", strings.NewReader(`{"constraint":"x"}`))
	req.SetPathValue("stage_id", stageID.String())
	req = injectIdentity(req, mcpReadIdentity())
	w := httptest.NewRecorder()
	s.handleRevisePlan(w, req)
	assertScopeError(t, w, http.StatusForbidden, "insufficient_scope")
}
