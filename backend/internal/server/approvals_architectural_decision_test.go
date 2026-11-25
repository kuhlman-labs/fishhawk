package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/operatorrole"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// --- #5507: delegated REST approve refused on a declared new_architectural_decision ---

// newArchitecturalDecisionApprovalServer is newDelegatedApprovalServer plus a
// wired in-memory artifact store, with a delegationSpecYAML run started and
// clean_dual_approval MET by construction (two approve verdicts of two
// configured agents, no concerns), so checkDelegation passes and the only
// thing that can refuse a delegated approve is the architectural-decision
// hold over the plan artifact the test seeds.
func newArchitecturalDecisionApprovalServer(t *testing.T) (*Server, *driveE2ERepo, *auditFake, *fakeApprovalRepo, *fakeArtifactRepo, *run.Stage) {
	t.Helper()
	s, repo, au, _, ar := newDelegatedApprovalServer(t)
	arts := newFakeArtifactRepo()
	s.cfg.ArtifactRepo = arts
	runID, planStage := startDriveE2ERun(t, s, repo, map[string]any{
		"repo": "x/y", "workflow_id": "feature_change", "workflow_sha": "abc",
		"trigger_source": "cli", "workflow_spec": delegationSpecYAML,
	})
	seedCleanPlanApproval(t, au, runID)
	return s, repo, au, ar, arts, planStage
}

// inBudgetPlanBytes is validPlanBytes with a 5-minute runtime prediction, under
// delegationSpecYAML's resolved implement-stage budget, so checkPlanBudget
// (a later pre-Submit gate) admits every fixture and the architectural
// decision hold is the only thing that can refuse.
func inBudgetPlanBytes(t *testing.T) []byte {
	t.Helper()
	var p plan.Plan
	if err := json.Unmarshal(validPlanBytes(t), &p); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	p.PredictedRuntimeMinutes = 5
	out, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return out
}

// assertDelegatedArchitecturalRefusal asserts the #5507 refusal and reads the
// COMMITTED state after the call: no approval row, no approval_submitted
// entry, and the stored plan stage still parked at its gate.
func assertDelegatedArchitecturalRefusal(t *testing.T, w *httptest.ResponseRecorder, repo *driveE2ERepo, au *auditFake, ar *fakeApprovalRepo, planStage *run.Stage, wantReason string) errorBody {
	t.Helper()
	code, body := w.Code, w.Body.Bytes()
	if code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", code, body)
	}
	var env errorEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("unmarshal error envelope: %v\n%s", err, body)
	}
	errBody := env.Error
	if errBody.Code != operatorrole.DecisionStateArchitecturalDecisionAwaitingCaptain {
		t.Fatalf("code = %q, want %q", errBody.Code, operatorrole.DecisionStateArchitecturalDecisionAwaitingCaptain)
	}
	if got, _ := errBody.Details["reason"].(string); got != wantReason {
		t.Errorf("details.reason = %q, want %q", got, wantReason)
	}
	if got, _ := errBody.Details["run_id"].(string); got != planStage.RunID.String() {
		t.Errorf("details.run_id = %q, want %q", got, planStage.RunID)
	}
	if got, _ := errBody.Details["stage_id"].(string); got != planStage.ID.String() {
		t.Errorf("details.stage_id = %q, want %q", got, planStage.ID)
	}
	if got, _ := errBody.Details["next_step"].(string); got != delegatedArchitecturalDecisionNextStep {
		t.Errorf("details.next_step = %q, want the static next step", got)
	}
	if rows, err := ar.ListForStage(context.Background(), planStage.ID); err != nil || len(rows) != 0 {
		t.Errorf("approval rows = %d (err %v) after refusal, want 0", len(rows), err)
	}
	if n := countAudit(au, "approval_submitted"); n != 0 {
		t.Errorf("approval_submitted entries = %d, want 0 (a refused delegated approve records no vote)", n)
	}
	stored, err := repo.GetStage(context.Background(), planStage.ID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	if stored.State != run.StageStateAwaitingApproval {
		t.Errorf("stored plan stage = %q, want still awaiting_approval", stored.State)
	}
	return errBody
}

// assertDelegatedApprovalAdmitted asserts a delegated approve went through:
// 200, the stored stage succeeded, and exactly one approval_submitted entry
// stamped delegated clean_dual_approval.
func assertDelegatedApprovalAdmitted(t *testing.T, w *httptest.ResponseRecorder, repo *driveE2ERepo, au *auditFake, planStage *run.Stage) {
	t.Helper()
	code, body := w.Code, w.Body.Bytes()
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", code, body)
	}
	stored, err := repo.GetStage(context.Background(), planStage.ID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	if stored.State != run.StageStateSucceeded {
		t.Errorf("stored plan stage = %q, want succeeded", stored.State)
	}
	if rule := delegatedAuditRule(t, au, "approval_submitted"); rule != "clean_dual_approval" {
		t.Errorf("audit delegated = %q, want clean_dual_approval", rule)
	}
}

// TestSubmitApproval_DelegatedArchitecturalDecision_DeclaredRefused is the
// #5507 done-means: a delegated approve that meets clean_dual_approval on a
// plan that declares a new_architectural_decision is refused 403
// architectural_decision_awaiting_captain, writes no approval row, and leaves
// the plan gate parked. Neither message nor details echo planner text.
func TestSubmitApproval_DelegatedArchitecturalDecision_DeclaredRefused(t *testing.T) {
	s, repo, au, ar, arts, planStage := newArchitecturalDecisionApprovalServer(t)
	seedStandardPlanArtifact(t, arts, planStage.ID, withNADPlanJSON(t, inBudgetPlanBytes(t)))

	w := submitApproval(t, s, planStage.ID, `{"decision":"approve","delegated":true}`)
	errBody := assertDelegatedArchitecturalRefusal(t, w, repo, au, ar, planStage, "declared")

	detailsRaw, _ := json.Marshal(errBody.Details)
	for _, planner := range []string{"durable crew message bus", "crew messages move to a durable queue", "ADR-082", "#3728"} {
		if strings.Contains(errBody.Message, planner) || strings.Contains(string(detailsRaw), planner) {
			t.Errorf("refusal echoes planner-authored text %q (injection surface): message %q details %s", planner, errBody.Message, detailsRaw)
		}
	}
	if !strings.Contains(errBody.Message, "new architectural decision") {
		t.Errorf("message = %q, want it to name the new architectural decision", errBody.Message)
	}
}

// TestSubmitApproval_DelegatedArchitecturalDecision_UndeclaredAdmitted is the
// control: the same fixture over a plan with NO declaration is admitted, so
// the refusal above is the hold's doing and not the fixture's.
func TestSubmitApproval_DelegatedArchitecturalDecision_UndeclaredAdmitted(t *testing.T) {
	s, repo, au, _, arts, planStage := newArchitecturalDecisionApprovalServer(t)
	seedStandardPlanArtifact(t, arts, planStage.ID, inBudgetPlanBytes(t))

	w := submitApproval(t, s, planStage.ID, `{"decision":"approve","delegated":true}`)
	assertDelegatedApprovalAdmitted(t, w, repo, au, planStage)
}

// TestSubmitApproval_DelegatedArchitecturalDecision_ListErrorRefused: an
// unreadable artifact store cannot show the plan declares no new decision, so
// the delegated approve fails closed with reason plan_artifact_unreadable and
// the raw store error stays out of the response.
func TestSubmitApproval_DelegatedArchitecturalDecision_ListErrorRefused(t *testing.T) {
	s, repo, au, ar, arts, planStage := newArchitecturalDecisionApprovalServer(t)
	seedStandardPlanArtifact(t, arts, planStage.ID, inBudgetPlanBytes(t))
	arts.listErr = errors.New("artifact store unavailable")

	w := submitApproval(t, s, planStage.ID, `{"decision":"approve","delegated":true}`)
	errBody := assertDelegatedArchitecturalRefusal(t, w, repo, au, ar, planStage, "plan_artifact_unreadable")
	if strings.Contains(w.Body.String(), "artifact store unavailable") {
		t.Errorf("response echoes the raw store error: %s", w.Body.String())
	}
	if !strings.Contains(errBody.Message, "fail-closed") {
		t.Errorf("message = %q, want the fail-closed message", errBody.Message)
	}
}

// TestSubmitApproval_DelegatedArchitecturalDecision_UndecodableRefused: a
// newest plan artifact that does not decode fails closed to the same refusal.
func TestSubmitApproval_DelegatedArchitecturalDecision_UndecodableRefused(t *testing.T) {
	s, repo, au, ar, arts, planStage := newArchitecturalDecisionApprovalServer(t)
	seedStandardPlanArtifact(t, arts, planStage.ID, []byte(`{"new_architectural_decision": 5}`))

	w := submitApproval(t, s, planStage.ID, `{"decision":"approve","delegated":true}`)
	errBody := assertDelegatedArchitecturalRefusal(t, w, repo, au, ar, planStage, "plan_artifact_unreadable")
	if !strings.Contains(errBody.Message, "fail-closed") {
		t.Errorf("message = %q, want the fail-closed message", errBody.Message)
	}
}

// whitespaceNADPlanJSON adds a NON-NIL new_architectural_decision whose
// rationale and decision_summary are whitespace-only and related_adrs empty:
// Declared() is false, so the shared predicate deliberately does not hold it
// (the plan-gate advisory shows the captain nothing for it either, and
// plan.Parse rejects the shape at submission).
func whitespaceNADPlanJSON(t *testing.T, raw []byte) []byte {
	t.Helper()
	var p plan.Plan
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	p.NewArchitecturalDecision = &plan.NewArchitecturalDecision{
		Rationale:       "   \t ",
		RelatedADRs:     []string{},
		DecisionSummary: " \n ",
	}
	out, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	if !strings.Contains(string(out), `"new_architectural_decision"`) {
		t.Fatalf("fixture lost the non-nil declaration: %s", out)
	}
	return out
}

// TestSubmitApproval_DelegatedArchitecturalDecision_WhitespaceDeclarationAdmitted
// pins the INTENDED fail-open branch shared with the advisory through
// Declared(): a non-nil but whitespace-only declaration is admitted.
func TestSubmitApproval_DelegatedArchitecturalDecision_WhitespaceDeclarationAdmitted(t *testing.T) {
	s, repo, au, _, arts, planStage := newArchitecturalDecisionApprovalServer(t)
	seedStandardPlanArtifact(t, arts, planStage.ID, whitespaceNADPlanJSON(t, inBudgetPlanBytes(t)))

	w := submitApproval(t, s, planStage.ID, `{"decision":"approve","delegated":true}`)
	assertDelegatedApprovalAdmitted(t, w, repo, au, planStage)
}

// TestSubmitApproval_DelegatedArchitecturalDecision_NilArtifactRepoAdmitted
// pins approval condition D2: an unwired ArtifactRepo is architecturalHoldNone
// on the REST arm, exactly as on the #5265 auto-drive arm (one shared
// predicate). This is a deliberate choice, not a fail-closed gap: production
// always wires ArtifactRepo (backend/cmd/fishhawkd/serve.go).
func TestSubmitApproval_DelegatedArchitecturalDecision_NilArtifactRepoAdmitted(t *testing.T) {
	s, repo, au, _, arts, planStage := newArchitecturalDecisionApprovalServer(t)
	// The declared plan is in the store, but the server does not read it.
	seedStandardPlanArtifact(t, arts, planStage.ID, withNADPlanJSON(t, inBudgetPlanBytes(t)))
	s.cfg.ArtifactRepo = nil

	w := submitApproval(t, s, planStage.ID, `{"decision":"approve","delegated":true}`)
	assertDelegatedApprovalAdmitted(t, w, repo, au, planStage)
}

// TestSubmitApproval_NonDelegated_DeclaredPlanUnchanged: a plain approve (no
// delegated field) of the declared plan never reaches the hold, so it is
// admitted exactly as before #5507.
func TestSubmitApproval_NonDelegated_DeclaredPlanUnchanged(t *testing.T) {
	s, repo, au, ar, arts, planStage := newArchitecturalDecisionApprovalServer(t)
	seedStandardPlanArtifact(t, arts, planStage.ID, withNADPlanJSON(t, inBudgetPlanBytes(t)))

	w := submitApproval(t, s, planStage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	stored, err := repo.GetStage(context.Background(), planStage.ID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	if stored.State != run.StageStateSucceeded {
		t.Errorf("stored plan stage = %q, want succeeded", stored.State)
	}
	if rows, _ := ar.ListForStage(context.Background(), planStage.ID); len(rows) != 1 {
		t.Errorf("approval rows = %d, want 1", len(rows))
	}
	if n := countAudit(au, "approval_submitted"); n != 1 {
		t.Errorf("approval_submitted entries = %d, want 1", n)
	}
}
