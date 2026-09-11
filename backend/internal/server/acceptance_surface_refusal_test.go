package server

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// E72.6 / #3381: the E72.1 "acceptance_surface: none is mutually exclusive
// with a drivable criterion" guard lives in plan.checkAcceptanceSurfaceNone,
// which only plan.Parse reaches — and handleShipPlan validates SCHEMA only. So
// the parser-level pin (TestParse_AcceptanceSurfaceNone_RejectsDrivableCriterion)
// structurally CANNOT fail on the defect: a plan declaring none beside a
// drivable criterion shipped 201, was stored, reached the gate, and approval
// deleted the run's acceptance stage. These tests drive the REAL route through
// s.Handler() and read COMMITTED state (run repo, artifact repo, audit fake)
// after the call returns, so a comment-only touch of plan.go passes the
// presence gate but fails here.
//
// Counterfactual (run, not reasoned): delete the plan.CheckAcceptanceSurface
// call in handleShipPlan → the three refusal tests go RED (201 instead of 400,
// the artifact stored, a plan_acceptance_precheck entry present) while the
// parser-level test stays GREEN.

// drivableCriterion is a criterion NOT marked skip_expected whose statement
// names an HTTP route — the shape the guard must refuse beside none.
func drivableCriterion(id string) map[string]any {
	return map[string]any{
		"id": id, "statement": "POST /v0/runs/{run_id}/plan returns 400 plan_invalid naming the criterion id",
		"source": "explicit", "source_ref": "#3381",
	}
}

// surfaceRefusalHarness wires the full plan-stage path (newPlanSequenceServer)
// on a feature_change run whose workflow declares an acceptance stage, with a
// plan stage (seq 0, running, gated) and a PENDING acceptance stage row (seq 2)
// so stage retention is observable as committed state.
type surfaceRefusalHarness struct {
	s    *Server
	rr   *recordingOrchestratorRepo
	art  *fakeArtifactRepo
	au   *storingAuditFake
	run  *run.Run
	plan *run.Stage
	acc  *run.Stage
	priv ed25519.PrivateKey
}

func newSurfaceRefusalHarness(t *testing.T) *surfaceRefusalHarness {
	t.Helper()
	s, rr, art, sf, au := newPlanSequenceServer(t)
	runRow := rr.seedRun()
	runRow.WorkflowID = "feature_change"
	runRow.WorkflowSpec = specWithAcceptanceStage
	planStage := rr.seedStage(runRow.ID, 0, run.StageStateRunning)
	planStage.RequiresApproval = true
	impl := rr.seedStage(runRow.ID, 1, run.StageStatePending)
	impl.Type = run.StageTypeImplement
	acc := rr.seedStage(runRow.ID, 2, run.StageStatePending)
	acc.Type = run.StageTypeAcceptance
	priv, _ := sf.issue(t, runRow.ID)
	return &surfaceRefusalHarness{s: s, rr: rr, art: art, au: au, run: runRow, plan: planStage, acc: acc, priv: priv}
}

func (h *surfaceRefusalHarness) ship(t *testing.T, body []byte) (int, string) {
	t.Helper()
	w := shipPlanRequest(t, h.s, h.run.ID, h.plan.ID, h.priv, body, "")
	return w.Code, w.Body.String()
}

// planInvalidEnvelope decodes the 400 body's error envelope.
type planInvalidEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Details struct {
			Error          string `json:"error"`
			RetryScheduled bool   `json:"retry_scheduled"`
		} `json:"details"`
	} `json:"error"`
}

func decodePlanInvalid(t *testing.T, body string) planInvalidEnvelope {
	t.Helper()
	var env planInvalidEnvelope
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode error envelope: %v\n%s", err, body)
	}
	return env
}

// assertRefusalBody pins what BOTH refusal branches must carry: the
// plan_invalid code the runner maps to category-B (agentOutputInvalidCodes),
// and details.error naming the offending criterion id plus the rule — the
// operator-visible half of the guard message, which the REST acceptance
// criterion depends on.
func assertRefusalBody(t *testing.T, body, wantID string) planInvalidEnvelope {
	t.Helper()
	env := decodePlanInvalid(t, body)
	if env.Error.Code != "plan_invalid" {
		t.Errorf("error.code = %q, want plan_invalid (the runner's agent-output-invalid category-B code)", env.Error.Code)
	}
	for _, want := range []string{`"` + wantID + `"`, "acceptance_surface"} {
		if !strings.Contains(env.Error.Details.Error, want) {
			t.Errorf("details.error = %q, want it to contain %q", env.Error.Details.Error, want)
		}
	}
	return env
}

// assertNoSideEffects reads the COMMITTED state a refusal must leave
// untouched: no artifact row for the plan stage, no plan_acceptance_precheck
// entry, no acceptance_stage_omitted marker, the acceptance stage row still
// present and pending, and the plan stage never at awaiting_approval.
func (h *surfaceRefusalHarness) assertNoSideEffects(t *testing.T) {
	t.Helper()
	if h.rr.sawTransitionTo(run.StageStateAwaitingApproval) {
		t.Errorf("plan stage transitioned to awaiting_approval; a refused plan must never reach the gate\ntransitions: %+v", h.rr.stageTransitions)
	}
	arts, err := h.art.ListForStage(context.Background(), h.plan.ID)
	if err != nil {
		t.Fatalf("ListForStage: %v", err)
	}
	if len(arts) != 0 {
		t.Errorf("artifact rows for the plan stage = %d, want 0 (the refusal must precede storage)", len(arts))
	}
	if n := countAcceptancePrecheckEntries(h.au.auditFake); n != 0 {
		t.Errorf("plan_acceptance_precheck entries = %d, want 0 (the refusal must precede the precheck)", n)
	}
	omitted, _ := h.au.ListForRunByCategory(context.Background(), h.run.ID, CategoryAcceptanceStageOmitted)
	if len(omitted) != 0 {
		t.Errorf("acceptance_stage_omitted entries = %d, want 0", len(omitted))
	}
	h.rr.mu.Lock()
	accRow, present := h.rr.stagesByID[h.acc.ID]
	h.rr.mu.Unlock()
	if !present {
		t.Fatalf("acceptance stage %s is gone; a refused plan must leave it in place", h.acc.ID)
	}
	if accRow.State != run.StageStatePending {
		t.Errorf("acceptance stage state = %q, want pending", accRow.State)
	}
}

// TestShipPlan_AcceptanceSurfaceNone_DrivableCriterion_RefusedFailB pins the
// budget-exhausted branch: with the #646 schema-retry budget already spent, a
// none + drivable-criterion plan is refused 400 plan_invalid naming the
// criterion id, the plan stage ends failed category-B, and no side effect
// (artifact, precheck entry, omission marker, stage deletion) is committed.
// Counterfactual: delete the plan.CheckAcceptanceSurface call → 201, stage at
// awaiting_approval, artifact stored, precheck entry present.
func TestShipPlan_AcceptanceSurfaceNone_DrivableCriterion_RefusedFailB(t *testing.T) {
	h := newSurfaceRefusalHarness(t)
	seedSchemaRetryEntry(t, h.au, h.run.ID, h.plan.ID, "prior validation error")

	body := acceptancePlanBodyWithSurface(t, []map[string]any{drivableCriterion("drivable-c1")}, nil, plan.AcceptanceSurfaceValueNone)
	// Status is asserted with Errorf, not Fatalf, so a deleted control lands
	// its RED on the committed-state assertions below as well (artifact
	// stored, precheck entry present) rather than only on the status line.
	code, resp := h.ship(t, body)
	if code != http.StatusBadRequest {
		t.Errorf("plan status = %d, want 400:\n%s", code, resp)
	} else {
		env := assertRefusalBody(t, resp, "drivable-c1")
		if env.Error.Details.RetryScheduled {
			t.Errorf("details.retry_scheduled = true on the budget-exhausted branch; want absent/false:\n%s", resp)
		}
	}

	h.rr.mu.Lock()
	got := h.rr.stagesByID[h.plan.ID]
	h.rr.mu.Unlock()
	if got.State != run.StageStateFailed {
		t.Errorf("plan stage state = %q, want failed", got.State)
	}
	if got.FailureCategory == nil || *got.FailureCategory != run.FailureB {
		t.Errorf("plan stage failure category = %v, want B", got.FailureCategory)
	}
	h.assertNoSideEffects(t)
}

// TestShipPlan_AcceptanceSurfaceNone_DrivableCriterion_RefusedRetryScheduled
// pins the first-refusal branch: with a fresh budget the same plan is refused
// 400 plan_invalid with retry_scheduled:true, the 400 BODY still names the
// criterion id (the operator-visible half — a refactor dropping details.error
// from this branch would break the REST acceptance criterion while every
// other handler test stayed green), exactly one plan_schema_retry entry
// carries the guard message as validation_error (the prompt-feedback seam the
// re-plan depends on, cf. TestShipPlan_SchemaRetry_SeamToPromptRender), the
// stage is re-opened rather than failed, and no side effect is committed.
// Counterfactual: delete the plan.CheckAcceptanceSurface call → 201 and zero
// plan_schema_retry entries.
func TestShipPlan_AcceptanceSurfaceNone_DrivableCriterion_RefusedRetryScheduled(t *testing.T) {
	h := newSurfaceRefusalHarness(t)

	body := acceptancePlanBodyWithSurface(t, []map[string]any{drivableCriterion("drivable-c1")}, nil, plan.AcceptanceSurfaceValueNone)
	code, resp := h.ship(t, body)
	if code != http.StatusBadRequest {
		t.Errorf("plan status = %d, want 400:\n%s", code, resp)
	} else {
		// The operator-visible half: the 400 BODY names the criterion id.
		env := assertRefusalBody(t, resp, "drivable-c1")
		if !env.Error.Details.RetryScheduled {
			t.Errorf("details.retry_scheduled missing/false on the first refusal; want true:\n%s", resp)
		}
	}

	// The prompt-feedback seam: the recorded validation_error names the id.
	entries, _ := h.au.ListForRunByCategory(context.Background(), h.run.ID, "plan_schema_retry")
	if len(entries) != 1 {
		t.Errorf("plan_schema_retry entries = %d, want 1", len(entries))
	} else {
		var stored struct {
			ValidationError string `json:"validation_error"`
		}
		if err := json.Unmarshal(entries[0].Payload, &stored); err != nil {
			t.Fatalf("unmarshal plan_schema_retry payload: %v", err)
		}
		if !strings.Contains(stored.ValidationError, `"drivable-c1"`) {
			t.Errorf("plan_schema_retry validation_error = %q, want it to name drivable-c1", stored.ValidationError)
		}
	}

	h.rr.mu.Lock()
	got := h.rr.stagesByID[h.plan.ID]
	h.rr.mu.Unlock()
	if got.State == run.StageStateFailed || got.State == run.StageStateAwaitingApproval {
		t.Errorf("plan stage state = %q, want re-opened (neither failed nor awaiting_approval)", got.State)
	}
	if got.FailureCategory != nil {
		t.Errorf("plan stage still carries failure category %q; RetryStage must clear it", *got.FailureCategory)
	}
	h.assertNoSideEffects(t)
}

// TestShipPlan_AcceptanceSurfaceNone_DrivableAfterSkipped_Refused: a
// correctly-skipped criterion FIRST then a drivable one is refused naming the
// SECOND id — the scan does not stop at the first exempt entry (same shape as
// the parser-level test). Counterfactual: delete the handler call → 201.
func TestShipPlan_AcceptanceSurfaceNone_DrivableAfterSkipped_Refused(t *testing.T) {
	h := newSurfaceRefusalHarness(t)
	seedSchemaRetryEntry(t, h.au, h.run.ID, h.plan.ID, "prior validation error")

	body := acceptancePlanBodyWithSurface(t, []map[string]any{
		allSkipCriterion("skipped-c1", "the helper is renamed", "covered by the unit test"),
		drivableCriterion("drivable-c2"),
	}, nil, plan.AcceptanceSurfaceValueNone)
	code, resp := h.ship(t, body)
	if code != http.StatusBadRequest {
		t.Errorf("plan status = %d, want 400:\n%s", code, resp)
	} else {
		env := assertRefusalBody(t, resp, "drivable-c2")
		if strings.Contains(env.Error.Details.Error, `"skipped-c1"`) {
			t.Errorf("details.error names the correctly-skipped criterion: %q", env.Error.Details.Error)
		}
	}
	h.assertNoSideEffects(t)
}

// TestShipPlan_AcceptanceSurfaceNone_AllSkip_Admitted is the narrowness
// control: none + only skip_expected-with-basis criteria is the LEGAL pairing
// and is admitted as before — 201, stage at awaiting_approval, one
// plan_acceptance_precheck entry with acceptance_surface_none:true, no failure
// category.
func TestShipPlan_AcceptanceSurfaceNone_AllSkip_Admitted(t *testing.T) {
	h := newSurfaceRefusalHarness(t)

	body := acceptancePlanBodyWithSurface(t, []map[string]any{
		allSkipCriterion("skipped-c1", "the helper is renamed", "covered by the unit test"),
	}, nil, plan.AcceptanceSurfaceValueNone)
	code, resp := h.ship(t, body)
	if code != http.StatusCreated {
		t.Fatalf("plan status = %d, want 201:\n%s", code, resp)
	}
	h.rr.mu.Lock()
	got := h.rr.stagesByID[h.plan.ID]
	h.rr.mu.Unlock()
	if got.State != run.StageStateAwaitingApproval {
		t.Errorf("plan stage state = %q, want awaiting_approval", got.State)
	}
	if got.FailureCategory != nil {
		t.Errorf("plan stage carries failure category %q; want none", *got.FailureCategory)
	}
	entry := lastAcceptancePrecheckEntry(t, h.au.auditFake)
	if !entry.AcceptanceSurfaceNone {
		t.Error("persisted acceptance_surface_none = false, want true")
	}
	if n := countAcceptancePrecheckEntries(h.au.auditFake); n != 1 {
		t.Errorf("plan_acceptance_precheck entries = %d, want 1", n)
	}
}

// TestShipPlan_NoAcceptanceSurface_DrivableCriterion_Admitted is the second
// control: a drivable criterion with NO acceptance_surface declaration is
// admitted — the guard is inert for every non-none plan.
func TestShipPlan_NoAcceptanceSurface_DrivableCriterion_Admitted(t *testing.T) {
	h := newSurfaceRefusalHarness(t)

	body := acceptancePlanBodyWithSurface(t, []map[string]any{drivableCriterion("drivable-c1")}, nil, "")
	code, resp := h.ship(t, body)
	if code != http.StatusCreated {
		t.Fatalf("plan status = %d, want 201:\n%s", code, resp)
	}
	h.rr.mu.Lock()
	got := h.rr.stagesByID[h.plan.ID]
	h.rr.mu.Unlock()
	if got.State != run.StageStateAwaitingApproval {
		t.Errorf("plan stage state = %q, want awaiting_approval", got.State)
	}
	if got.FailureCategory != nil {
		t.Errorf("plan stage carries failure category %q; want none", *got.FailureCategory)
	}
	if n := countAcceptancePrecheckEntries(h.au.auditFake); n != 1 {
		t.Errorf("plan_acceptance_precheck entries = %d, want 1", n)
	}
	if _, err := h.rr.GetStage(context.Background(), h.acc.ID); err != nil {
		t.Errorf("acceptance stage lookup: %v; want the row retained at ship time", err)
	}
}
