package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// admissionSeam bundles the fakes + ids of a plan(succ)+implement(succ)+
// review(succ)+acceptance(state) run wired for the acceptance-admission
// endpoint: the server, the shared orchestratorRepo (functional transitions,
// used as BOTH the server RunRepo and the orchestrator Runs), the audit fake,
// and the wired orchestrator (nil GitHub, so a workflow_dispatch would be
// observable via the acceptance_dispatched emit — which the short-circuit path
// never fires).
type admissionSeam struct {
	s            *Server
	rr           *orchestratorRepo
	au           *auditFake
	o            *orchestrator.Orchestrator
	runID        uuid.UUID
	acceptanceID uuid.UUID
	implementID  uuid.UUID
}

// admissionPlanBytes builds a standard_v1 plan whose verification carries the
// given out_of_scope + acceptance_criteria — mirrors the orchestrator test's
// acceptanceSkipPlanBytes. No schema validation runs on artifact load, so a
// hand-built plan that decodes into plan.Plan is sufficient to drive the
// predicates.
func admissionPlanBytes(t *testing.T, outOfScope []string, criteria []map[string]any) json.RawMessage {
	t.Helper()
	verification := map[string]any{
		"test_strategy": "unit",
		"rollback_plan": "revert the PR",
	}
	if len(outOfScope) > 0 {
		verification["out_of_scope"] = outOfScope
	}
	if len(criteria) > 0 {
		verification["acceptance_criteria"] = criteria
	}
	b, err := json.Marshal(map[string]any{
		"plan_version": "standard_v1",
		"summary":      "ship the widget endpoint",
		"verification": verification,
	})
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	return b
}

func buildAdmissionSeam(t *testing.T, acceptanceState run.StageState, planBytes json.RawMessage) *admissionSeam {
	t.Helper()
	rr := newOrchestratorRepo()
	au := newAuditFake()
	r := rr.seedRun()
	r.WorkflowID = "feature_change"
	planS := rr.seedStage(r.ID, 0, run.StageStateSucceeded)
	planS.Type = run.StageTypePlan
	impl := rr.seedStage(r.ID, 1, run.StageStateSucceeded)
	impl.Type = run.StageTypeImplement
	rev := rr.seedStage(r.ID, 2, run.StageStateSucceeded)
	rev.Type = run.StageTypeReview
	acc := rr.seedStage(r.ID, 3, acceptanceState)
	acc.Type = run.StageTypeAcceptance

	ar := newFakeArtifactRepo()
	v := "standard_v1"
	ar.all = append(ar.all, &artifact.Artifact{
		ID: uuid.New(), StageID: planS.ID, Kind: artifact.KindPlan,
		SchemaVersion: &v, Content: planBytes, CreatedAt: time.Now().UTC(),
	})

	o := &orchestrator.Orchestrator{Runs: rr, Audit: au, Artifacts: ar}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: au, ArtifactRepo: ar, Orchestrator: o})
	return &admissionSeam{s: s, rr: rr, au: au, o: o, runID: r.ID, acceptanceID: acc.ID, implementID: impl.ID}
}

// postAdmission calls the admission endpoint under the given identity. A zero
// Identity is anonymous.
func postAdmission(t *testing.T, s *Server, stageID uuid.UUID, id Identity) *httptest.ResponseRecorder {
	t.Helper()
	url := "/v0/stages/" + stageID.String() + "/acceptance-admission"
	req := httptest.NewRequest(http.MethodPost, url, nil)
	req.SetPathValue("stage_id", stageID.String())
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, id))
	w := httptest.NewRecorder()
	s.handleAcceptanceAdmission(w, req)
	return w
}

var allSkipWithBasisCriteria = []map[string]any{
	{"id": "webhook-fires", "statement": "webhook fires on close", "source": "inferred", "rationale": "external", "skip_expected": true, "expectation_basis": "validated in webhook_integration_test.go with a fake"},
	{"id": "issue-closes", "statement": "issue auto-closes", "source": "inferred", "rationale": "external", "skip_expected": true, "expectation_basis": "validated in closer_e2e_test.go"},
}

// TestAcceptanceAdmission_AllSkipWithBasis_ShortCircuits is Mode 1 (the issue's
// done-means): POSTing admission on the acceptance stage of an all-skip-with-
// basis plan settles it succeeded, records an acceptance_outcome_recorded entry
// (accepted / passed / basis all-skip-with-basis), and fires NO acceptance
// dispatch — the no-runner-dispatch-evidence pin.
func TestAcceptanceAdmission_AllSkipWithBasis_ShortCircuits(t *testing.T) {
	seam := buildAdmissionSeam(t, run.StageStatePending, admissionPlanBytes(t, nil, allSkipWithBasisCriteria))

	w := postAdmission(t, seam.s, seam.acceptanceID, testOperatorIdentity())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp acceptanceAdmissionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.ShortCircuited {
		t.Fatalf("short_circuited = false, want true:\n%s", w.Body.String())
	}
	if resp.Kind != orchestrator.AcceptanceShortCircuitAllSkipWithBasis || resp.Basis != plan.AcceptanceBasisAllSkipWithBasis || resp.CriteriaTotal != 2 {
		t.Errorf("resp = %+v, want kind=%s basis=%s total=2", resp, orchestrator.AcceptanceShortCircuitAllSkipWithBasis, plan.AcceptanceBasisAllSkipWithBasis)
	}
	if resp.Stage == nil || resp.Stage.State != string(run.StageStateSucceeded) {
		t.Errorf("resp.Stage = %+v, want a succeeded stage", resp.Stage)
	}
	if got := seam.rr.stagesByID[seam.acceptanceID].State; got != run.StageStateSucceeded {
		t.Errorf("acceptance stage = %q, want succeeded", got)
	}
	if got := seam.rr.runs[seam.runID].State; got != run.StateSucceeded {
		t.Errorf("run state = %q, want succeeded (Advance re-entered)", got)
	}
	if n := countByCategory(seam.au, CategoryAcceptanceOutcomeRecorded); n != 1 {
		t.Fatalf("acceptance_outcome_recorded = %d, want 1", n)
	}
	if n := countByCategory(seam.au, CategoryAcceptanceDispatched); n != 0 {
		t.Errorf("acceptance_dispatched = %d, want 0 (no runner dispatch evidence)", n)
	}
	outcome := findAppendedByCategory(t, seam.au, CategoryAcceptanceOutcomeRecorded)
	for _, want := range []string{`"verdict":"passed"`, `"outcome":"accepted"`, `"basis":"all-skip-with-basis"`} {
		if !strings.Contains(string(outcome.Payload), want) {
			t.Errorf("outcome payload missing %s:\n%s", want, outcome.Payload)
		}
	}
}

// TestAcceptanceAdmission_OtherPredicates_ShortCircuit is Mode 8: the other two
// disjoint predicates (out-of-scope, empty-criteria) settle through the same
// admission path.
func TestAcceptanceAdmission_OtherPredicates_ShortCircuit(t *testing.T) {
	t.Run("out-of-scope", func(t *testing.T) {
		seam := buildAdmissionSeam(t, run.StageStatePending, admissionPlanBytes(t, []string{"deletion deferred to a follow-up"}, nil))
		w := postAdmission(t, seam.s, seam.acceptanceID, testOperatorIdentity())
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
		}
		var resp acceptanceAdmissionResponse
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if !resp.ShortCircuited || resp.Kind != orchestrator.AcceptanceShortCircuitOutOfScope {
			t.Fatalf("resp = %+v, want an out-of-scope short-circuit", resp)
		}
		if n := countByCategory(seam.au, CategoryAcceptanceSkippedOutOfScope); n != 1 {
			t.Errorf("acceptance_skipped_out_of_scope = %d, want 1", n)
		}
		if got := seam.rr.stagesByID[seam.acceptanceID].State; got != run.StageStateSucceeded {
			t.Errorf("acceptance stage = %q, want succeeded", got)
		}
	})
	t.Run("empty-criteria", func(t *testing.T) {
		seam := buildAdmissionSeam(t, run.StageStatePending, admissionPlanBytes(t, nil, nil))
		w := postAdmission(t, seam.s, seam.acceptanceID, testOperatorIdentity())
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
		}
		var resp acceptanceAdmissionResponse
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if !resp.ShortCircuited || resp.Kind != orchestrator.AcceptanceShortCircuitEmptyCriteria {
			t.Fatalf("resp = %+v, want an empty-criteria short-circuit", resp)
		}
		if got := seam.rr.stagesByID[seam.acceptanceID].State; got != run.StageStateSucceeded {
			t.Errorf("acceptance stage = %q, want succeeded", got)
		}
	})
}

// TestAcceptanceAdmission_MixedCriteria_NoShortCircuit is Mode 2: a mixed plan
// (one drivable criterion) returns short_circuited:false and leaves the stage
// pending, no state change, no verdict.
func TestAcceptanceAdmission_MixedCriteria_NoShortCircuit(t *testing.T) {
	mixed := []map[string]any{
		allSkipWithBasisCriteria[0],
		{"id": "get-returns-200", "statement": "GET returns 200", "source": "explicit", "source_ref": "#1", "blocking": true},
	}
	seam := buildAdmissionSeam(t, run.StageStatePending, admissionPlanBytes(t, nil, mixed))
	w := postAdmission(t, seam.s, seam.acceptanceID, testOperatorIdentity())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp acceptanceAdmissionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.ShortCircuited {
		t.Errorf("short_circuited = true, want false (mixed criteria)")
	}
	if got := seam.rr.stagesByID[seam.acceptanceID].State; got != run.StageStatePending {
		t.Errorf("acceptance stage = %q, want pending (untouched)", got)
	}
	if n := countByCategory(seam.au, CategoryAcceptanceOutcomeRecorded); n != 0 {
		t.Errorf("acceptance_outcome_recorded = %d, want 0", n)
	}
}

// TestAcceptanceAdmission_NonAdmissibleState_False is the already-settled no-op:
// a succeeded acceptance stage is a non-admissible state → short_circuited:false,
// no re-settle.
func TestAcceptanceAdmission_NonAdmissibleState_False(t *testing.T) {
	seam := buildAdmissionSeam(t, run.StageStateSucceeded, admissionPlanBytes(t, nil, allSkipWithBasisCriteria))
	w := postAdmission(t, seam.s, seam.acceptanceID, testOperatorIdentity())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp acceptanceAdmissionResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.ShortCircuited {
		t.Errorf("short_circuited = true, want false (already-settled acceptance stage)")
	}
	if n := countByCategory(seam.au, CategoryAcceptanceOutcomeRecorded); n != 0 {
		t.Errorf("acceptance_outcome_recorded = %d, want 0", n)
	}
}

// TestAcceptanceAdmission_NilOrchestrator_FailsOpen is Mode 5: a server with no
// orchestrator wired returns short_circuited:false (fail-open — a degraded
// server never blocks a legitimate dispatch).
func TestAcceptanceAdmission_NilOrchestrator_FailsOpen(t *testing.T) {
	seam := buildAdmissionSeam(t, run.StageStatePending, admissionPlanBytes(t, nil, allSkipWithBasisCriteria))
	// Rebuild the server with no orchestrator but the same repo.
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: seam.rr, AuditRepo: seam.au})
	w := postAdmission(t, s, seam.acceptanceID, testOperatorIdentity())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp acceptanceAdmissionResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.ShortCircuited {
		t.Errorf("short_circuited = true, want false (nil orchestrator)")
	}
	if got := seam.rr.stagesByID[seam.acceptanceID].State; got != run.StageStatePending {
		t.Errorf("acceptance stage = %q, want pending (untouched)", got)
	}
}

// TestAcceptanceAdmission_Auth is Mode 6: 401 unauthenticated, 403 missing
// write:stages, 403 cross-run MCP subject binding; plus the MCP-subject
// same-run happy path.
func TestAcceptanceAdmission_Auth(t *testing.T) {
	t.Run("anonymous -> 401", func(t *testing.T) {
		seam := buildAdmissionSeam(t, run.StageStatePending, admissionPlanBytes(t, nil, allSkipWithBasisCriteria))
		w := postAdmission(t, seam.s, seam.acceptanceID, Identity{})
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401:\n%s", w.Code, w.Body.String())
		}
	})
	t.Run("token missing write:stages -> 403", func(t *testing.T) {
		seam := buildAdmissionSeam(t, run.StageStatePending, admissionPlanBytes(t, nil, allSkipWithBasisCriteria))
		id := Identity{Subject: "github:agent", TokenID: "tok", Scopes: []string{"read:runs"}}
		w := postAdmission(t, seam.s, seam.acceptanceID, id)
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "insufficient_scope") {
			t.Errorf("body missing insufficient_scope: %s", w.Body.String())
		}
	})
	t.Run("cross-run MCP subject -> 403", func(t *testing.T) {
		seam := buildAdmissionSeam(t, run.StageStatePending, admissionPlanBytes(t, nil, allSkipWithBasisCriteria))
		id := Identity{Subject: "mcp:run:" + uuid.NewString(), TokenID: "tok", Scopes: []string{"write:stages"}}
		w := postAdmission(t, seam.s, seam.acceptanceID, id)
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "cross_run_admission") {
			t.Errorf("body missing cross_run_admission: %s", w.Body.String())
		}
	})
	t.Run("same-run MCP subject -> short-circuits", func(t *testing.T) {
		seam := buildAdmissionSeam(t, run.StageStatePending, admissionPlanBytes(t, nil, allSkipWithBasisCriteria))
		id := Identity{Subject: "mcp:run:" + seam.runID.String(), TokenID: "tok", Scopes: []string{"write:stages"}}
		w := postAdmission(t, seam.s, seam.acceptanceID, id)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
		}
		var resp acceptanceAdmissionResponse
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if !resp.ShortCircuited {
			t.Errorf("short_circuited = false, want true for the run's own agent token")
		}
	})
}

// TestAcceptanceAdmission_UnknownStage_404 is Mode: an unknown stage id 404s.
func TestAcceptanceAdmission_UnknownStage_404(t *testing.T) {
	seam := buildAdmissionSeam(t, run.StageStatePending, admissionPlanBytes(t, nil, allSkipWithBasisCriteria))
	w := postAdmission(t, seam.s, uuid.New(), testOperatorIdentity())
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "stage_not_found") {
		t.Errorf("body missing stage_not_found: %s", w.Body.String())
	}
}

// TestAcceptanceAdmission_NonAcceptanceStage_422 is Mode: a non-acceptance stage
// 422s (a misrouted call is diagnosable, not a silent false).
func TestAcceptanceAdmission_NonAcceptanceStage_422(t *testing.T) {
	seam := buildAdmissionSeam(t, run.StageStatePending, admissionPlanBytes(t, nil, allSkipWithBasisCriteria))
	w := postAdmission(t, seam.s, seam.implementID, testOperatorIdentity())
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "validation_failed") {
		t.Errorf("body missing validation_failed: %s", w.Body.String())
	}
}
