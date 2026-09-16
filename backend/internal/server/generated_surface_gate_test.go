package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan/planfixture"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// The generated_surface plan-gate REFUSAL (#3437). These tests drive the REAL
// handleShipPlan on the Orchestrator-wired newPlanSequenceServer harness and
// read COMMITTED repo state — the ship responds 201 whether or not the gate
// fires, so a response-only assertion would pass under a deleted control.

const (
	gsOpenAPI   = "docs/api/v0.openapi.yaml"
	gsAPIMd     = "site/src/content/docs/reference/api.md"
	gsWMCanon   = "docs/spec/work-management-v0.schema.json"
	gsWMBackend = "backend/internal/workmgmt/schemas/work-management-v0.schema.json"
	gsWMCli     = "cli/internal/spec/schemas/work-management-v0.schema.json"
	gsWf2Canon  = "docs/spec/workflow-v2.schema.json"
	gsWf2Site   = "site/src/content/docs/reference/workflow-spec.md"
	gsWf2Back   = "backend/internal/spec/schemas/workflow-v2.schema.json"
	gsWf2Cli    = "cli/internal/spec/schemas/workflow-v2.schema.json"
)

// genSurfacePlanBody builds a schema-valid standard_v1 plan carrying the given
// scope files and optional surface_sweep_exemptions.
func genSurfacePlanBody(t *testing.T, files []plan.ScopeFile, exemptions []map[string]any) []byte {
	t.Helper()
	fileMaps := make([]any, 0, len(files))
	for _, f := range files {
		fileMaps = append(fileMaps, map[string]any{"path": f.Path, "operation": string(f.Operation)})
	}
	m := planfixture.Valid(func(p map[string]any) {
		p["scope"] = map[string]any{"files": fileMaps}
		if len(exemptions) > 0 {
			ex := make([]any, 0, len(exemptions))
			for _, e := range exemptions {
				ex = append(ex, e)
			}
			p["surface_sweep_exemptions"] = ex
		}
	})
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	if err := plan.Validate(body); err != nil {
		t.Fatalf("fixture plan does not validate: %v", err)
	}
	return body
}

func countGeneratedSurfaceRetryEntries(t *testing.T, au *storingAuditFake, runID uuid.UUID) int {
	t.Helper()
	entries, err := au.ListForRunByCategory(context.Background(), runID, categoryPlanGeneratedSurfaceRetry)
	if err != nil {
		t.Fatalf("list plan_generated_surface_retry: %v", err)
	}
	return len(entries)
}

type generatedSurfaceRetryPayload struct {
	RequiredScopeFiles []string `json:"required_scope_files"`
	Findings           []struct {
		TriggerPath  string   `json:"trigger_path"`
		MissingTests []string `json:"missing_tests"`
		Generator    string   `json:"generator"`
		SubPlanTitle string   `json:"sub_plan_title"`
	} `json:"findings"`
}

func lastGeneratedSurfaceRetryPayload(t *testing.T, au *storingAuditFake, runID uuid.UUID) generatedSurfaceRetryPayload {
	t.Helper()
	entries, err := au.ListForRunByCategory(context.Background(), runID, categoryPlanGeneratedSurfaceRetry)
	if err != nil {
		t.Fatalf("list plan_generated_surface_retry: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no plan_generated_surface_retry entry recorded")
	}
	var p generatedSurfaceRetryPayload
	if err := json.Unmarshal(entries[len(entries)-1].Payload, &p); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return p
}

// seedGeneratedSurfaceRetryEntry pre-loads the #3437 budget so the exhausted
// branch is reached deterministically.
func seedGeneratedSurfaceRetryEntry(t *testing.T, au *storingAuditFake, runID, stageID uuid.UUID) {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{
		"run_id":               runID.String(),
		"stage_id":             stageID.String(),
		"attempt":              1,
		"findings":             []any{},
		"required_scope_files": []string{gsAPIMd},
	})
	kind := audit.ActorKind("system")
	if _, err := au.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Category:  categoryPlanGeneratedSurfaceRetry,
		ActorKind: &kind,
		Payload:   payload,
	}); err != nil {
		t.Fatalf("seed plan_generated_surface_retry: %v", err)
	}
}

// TestShipPlan_GeneratedSurfaceMissing_RefusesAndReopens is the done-means
// COUNTERFACTUAL for the plan.go call site: a plan scoping docs/api/v0.openapi.yaml
// without the generated api.md is REFUSED — exactly one entry naming the trigger,
// missing derivative and generator; the stage never reaches awaiting_approval
// (re-opened, FailureCategory cleared); the run is not failed. Deleting the
// tryGeneratedSurfaceRetry call site advances the stage with no entry.
func TestShipPlan_GeneratedSurfaceMissing_RefusesAndReopens(t *testing.T) {
	s, rr, _, sf, au := newPlanSequenceServer(t)
	runRow := rr.seedRun()
	planStage := rr.seedStage(runRow.ID, 0, run.StageStateRunning)
	planStage.RequiresApproval = true
	priv, _ := sf.issue(t, runRow.ID)

	body := genSurfacePlanBody(t, []plan.ScopeFile{{Path: gsOpenAPI, Operation: plan.FileOpModify}}, nil)
	wp := shipPlanRequest(t, s, runRow.ID, planStage.ID, priv, body, "")
	if wp.Code != http.StatusCreated {
		t.Fatalf("plan status = %d, want 201:\n%s", wp.Code, wp.Body.String())
	}

	if n := countGeneratedSurfaceRetryEntries(t, au, runRow.ID); n != 1 {
		t.Fatalf("plan_generated_surface_retry entries = %d, want 1", n)
	}
	p := lastGeneratedSurfaceRetryPayload(t, au, runRow.ID)
	if len(p.Findings) != 1 || p.Findings[0].TriggerPath != gsOpenAPI ||
		p.Findings[0].Generator != testSweepGeneratorSiteReference ||
		len(p.Findings[0].MissingTests) != 1 || p.Findings[0].MissingTests[0] != gsAPIMd {
		t.Fatalf("finding = %+v, want openapi->api.md via gen-site-reference", p.Findings)
	}
	if len(p.RequiredScopeFiles) != 1 || p.RequiredScopeFiles[0] != gsAPIMd {
		t.Errorf("required_scope_files = %v, want [%s]", p.RequiredScopeFiles, gsAPIMd)
	}
	// COMMITTED stage state: re-opened, never awaiting_approval, FailureCategory
	// cleared by RetryStage.
	got := rr.stagesByID[planStage.ID]
	if got.State == run.StageStateAwaitingApproval {
		t.Errorf("stage reached awaiting_approval; the refusal must re-open it")
	}
	// Re-opened then orchestrator-advanced on the local path (pending →
	// dispatched), mirroring the scope-retry refusal.
	if got.State != run.StageStateDispatched {
		t.Errorf("stage state = %q, want dispatched (re-opened then orchestrator-advanced)", got.State)
	}
	if got.FailureCategory != nil {
		t.Errorf("FailureCategory = %v, want nil (RetryStage clears it)", *got.FailureCategory)
	}
	if rr.sawTransitionTo(run.StageStateAwaitingApproval) {
		t.Errorf("stage transitioned to awaiting_approval\ntransitions: %+v", rr.stageTransitions)
	}
	if st := rr.runs[runRow.ID].State; st == run.StateFailed {
		t.Errorf("run state = failed; a refusal must stay recoverable")
	}
}

// TestShipPlan_GeneratedSurfaceScoped_Admitted: the same plan WITH api.md parks
// normally with zero entries.
func TestShipPlan_GeneratedSurfaceScoped_Admitted(t *testing.T) {
	s, rr, _, sf, au := newPlanSequenceServer(t)
	runRow := rr.seedRun()
	planStage := rr.seedStage(runRow.ID, 0, run.StageStateRunning)
	planStage.RequiresApproval = true
	priv, _ := sf.issue(t, runRow.ID)

	body := genSurfacePlanBody(t, []plan.ScopeFile{
		{Path: gsOpenAPI, Operation: plan.FileOpModify},
		{Path: gsAPIMd, Operation: plan.FileOpModify},
	}, nil)
	wp := shipPlanRequest(t, s, runRow.ID, planStage.ID, priv, body, "")
	if wp.Code != http.StatusCreated {
		t.Fatalf("plan status = %d, want 201:\n%s", wp.Code, wp.Body.String())
	}
	if n := countGeneratedSurfaceRetryEntries(t, au, runRow.ID); n != 0 {
		t.Errorf("plan_generated_surface_retry entries = %d, want 0 (scoped derivative admits)", n)
	}
	if got := rr.stagesByID[planStage.ID].State; got != run.StageStateAwaitingApproval {
		t.Errorf("stage state = %q, want awaiting_approval", got)
	}
}

// TestShipPlan_GeneratedSurfaceExempted_Admitted is the COUNTERFACTUAL for the
// exemption subtraction: an exemption {pattern: generator, sibling: derivative}
// admits the plan; a mismatched pattern name still refuses. It also pins
// approval condition 2: the surface-sweep payload records ZERO applied
// exemptions (a generator-string pattern is a non-matching, harmless no-op to
// evaluateSurfaceSweep).
func TestShipPlan_GeneratedSurfaceExempted_Admitted(t *testing.T) {
	s, rr, _, sf, au := newPlanSequenceServer(t)
	runRow := rr.seedRun()
	planStage := rr.seedStage(runRow.ID, 0, run.StageStateRunning)
	planStage.RequiresApproval = true
	priv, _ := sf.issue(t, runRow.ID)

	body := genSurfacePlanBody(t,
		[]plan.ScopeFile{{Path: gsOpenAPI, Operation: plan.FileOpModify}},
		[]map[string]any{{"pattern": testSweepGeneratorSiteReference, "sibling": gsAPIMd, "reason": "openapi comment-only, api.md byte-identical"}},
	)
	wp := shipPlanRequest(t, s, runRow.ID, planStage.ID, priv, body, "")
	if wp.Code != http.StatusCreated {
		t.Fatalf("plan status = %d, want 201:\n%s", wp.Code, wp.Body.String())
	}
	if n := countGeneratedSurfaceRetryEntries(t, au, runRow.ID); n != 0 {
		t.Errorf("plan_generated_surface_retry entries = %d, want 0 (exemption admits)", n)
	}
	if got := rr.stagesByID[planStage.ID].State; got != run.StageStateAwaitingApproval {
		t.Errorf("stage state = %q, want awaiting_approval", got)
	}
	// Approval condition 2: the generator-string pattern draws no surface-sweep
	// applied exemption (non-matching → harmless no-op).
	sweep := lastSurfaceSweepEntry(t, au.auditFake)
	if len(sweep.AppliedExemptions) != 0 {
		t.Errorf("surface_sweep applied_exemptions = %+v, want 0", sweep.AppliedExemptions)
	}
}

// TestShipPlan_GeneratedSurfaceExempted_MismatchedPattern_Refused is the other
// arm of the exemption counterfactual: a wrong pattern name does NOT exempt.
func TestShipPlan_GeneratedSurfaceExempted_MismatchedPattern_Refused(t *testing.T) {
	s, rr, _, sf, au := newPlanSequenceServer(t)
	runRow := rr.seedRun()
	planStage := rr.seedStage(runRow.ID, 0, run.StageStateRunning)
	planStage.RequiresApproval = true
	priv, _ := sf.issue(t, runRow.ID)

	body := genSurfacePlanBody(t,
		[]plan.ScopeFile{{Path: gsOpenAPI, Operation: plan.FileOpModify}},
		[]map[string]any{{"pattern": "not-the-generator", "sibling": gsAPIMd, "reason": "wrong pattern"}},
	)
	wp := shipPlanRequest(t, s, runRow.ID, planStage.ID, priv, body, "")
	if wp.Code != http.StatusCreated {
		t.Fatalf("plan status = %d, want 201:\n%s", wp.Code, wp.Body.String())
	}
	if n := countGeneratedSurfaceRetryEntries(t, au, runRow.ID); n != 1 {
		t.Errorf("plan_generated_surface_retry entries = %d, want 1 (mismatched pattern does not exempt)", n)
	}
}

// TestShipPlan_GeneratedSurfaceRetryBudgetExhausted_Parks is the COUNTERFACTUAL
// for the bound: with one entry already seeded, a second miss parks at
// awaiting_approval with NO second entry. Deleting the `if !granted return false`
// guard (or raising the max) lands a second entry.
func TestShipPlan_GeneratedSurfaceRetryBudgetExhausted_Parks(t *testing.T) {
	s, rr, _, sf, au := newPlanSequenceServer(t)
	runRow := rr.seedRun()
	planStage := rr.seedStage(runRow.ID, 0, run.StageStateRunning)
	planStage.RequiresApproval = true
	priv, _ := sf.issue(t, runRow.ID)
	seedGeneratedSurfaceRetryEntry(t, au, runRow.ID, planStage.ID)

	body := genSurfacePlanBody(t, []plan.ScopeFile{{Path: gsOpenAPI, Operation: plan.FileOpModify}}, nil)
	wp := shipPlanRequest(t, s, runRow.ID, planStage.ID, priv, body, "")
	if wp.Code != http.StatusCreated {
		t.Fatalf("plan status = %d, want 201:\n%s", wp.Code, wp.Body.String())
	}
	if n := countGeneratedSurfaceRetryEntries(t, au, runRow.ID); n != 1 {
		t.Errorf("plan_generated_surface_retry entries = %d, want 1 (budget spent, no second entry)", n)
	}
	if got := rr.stagesByID[planStage.ID].State; got != run.StageStateAwaitingApproval {
		t.Errorf("stage state = %q, want awaiting_approval (exhaustion parks, never terminal)", got)
	}
	if st := rr.runs[runRow.ID].State; st == run.StateFailed {
		t.Errorf("run state = failed; exhaustion must never fail the run terminally")
	}
}

// TestShipPlan_GeneratedSurfaceRetry_IndependentOfGitHubClient is the
// COUNTERFACTUAL for the standalone evaluation: cfg.GitHub is nil (runTestSweep
// fails open, records nothing), yet the refusal still fires because
// evaluateGeneratedSurfaceGate reads the scope set directly. Replacing the
// standalone call with one over the runTestSweep payload would make this RED.
func TestShipPlan_GeneratedSurfaceRetry_IndependentOfGitHubClient(t *testing.T) {
	s, rr, _, sf, au := newPlanSequenceServer(t)
	if s.cfg.GitHub != nil {
		t.Fatal("precondition: cfg.GitHub must be nil for this test")
	}
	runRow := rr.seedRun()
	planStage := rr.seedStage(runRow.ID, 0, run.StageStateRunning)
	planStage.RequiresApproval = true
	priv, _ := sf.issue(t, runRow.ID)

	body := genSurfacePlanBody(t, []plan.ScopeFile{{Path: gsOpenAPI, Operation: plan.FileOpModify}}, nil)
	wp := shipPlanRequest(t, s, runRow.ID, planStage.ID, priv, body, "")
	if wp.Code != http.StatusCreated {
		t.Fatalf("plan status = %d, want 201:\n%s", wp.Code, wp.Body.String())
	}
	if n := countGeneratedSurfaceRetryEntries(t, au, runRow.ID); n != 1 {
		t.Errorf("plan_generated_surface_retry entries = %d, want 1 (refusal fires with no GitHub client)", n)
	}
	// runTestSweep failed open — no plan_test_sweep entry — proving the gate did
	// NOT rely on it.
	if n := countTestSweepEntries(au.auditFake); n != 0 {
		t.Errorf("plan_test_sweep entries = %d, want 0 (fail-open confirms the gate is independent)", n)
	}
}

// TestShipPlan_GeneratedSurfaceRetry_SubPlanScope: a decomposed plan whose
// sub-plan scopes docs/spec/workflow-v2.schema.json without its mirrors is
// refused with the sub_plan_title and BOTH generators' findings. COUNTERFACTUAL
// for the SubPlans loop.
func TestShipPlan_GeneratedSurfaceRetry_SubPlanScope(t *testing.T) {
	s, rr, _, sf, au := newPlanSequenceServer(t)
	runRow := rr.seedRun()
	planStage := rr.seedStage(runRow.ID, 0, run.StageStateRunning)
	planStage.RequiresApproval = true
	priv, _ := sf.issue(t, runRow.ID)

	body := decomposedScopePlanBody(t,
		[]plan.ScopeFile{{Path: "backend/internal/server/upload.go", Operation: plan.FileOpModify}},
		[]subPlanScope{
			{title: "unrelated slice", files: []plan.ScopeFile{{Path: "backend/internal/foo/foo.go", Operation: plan.FileOpModify}}},
			{title: "schema slice", files: []plan.ScopeFile{{Path: gsWf2Canon, Operation: plan.FileOpModify}}},
		},
	)
	wp := shipPlanRequest(t, s, runRow.ID, planStage.ID, priv, body, "")
	if wp.Code != http.StatusCreated {
		t.Fatalf("plan status = %d, want 201:\n%s", wp.Code, wp.Body.String())
	}
	if n := countGeneratedSurfaceRetryEntries(t, au, runRow.ID); n != 1 {
		t.Fatalf("plan_generated_surface_retry entries = %d, want 1", n)
	}
	p := lastGeneratedSurfaceRetryPayload(t, au, runRow.ID)
	if len(p.Findings) != 2 {
		t.Fatalf("findings = %+v, want 2 (one per generator)", p.Findings)
	}
	gens := map[string]bool{}
	for _, f := range p.Findings {
		if f.SubPlanTitle != "schema slice" {
			t.Errorf("finding sub_plan_title = %q, want %q", f.SubPlanTitle, "schema slice")
		}
		gens[f.Generator] = true
	}
	if !gens[testSweepGeneratorSiteReference] || !gens[testSweepGeneratorSyncSchemas] {
		t.Errorf("generators = %v, want both site-reference and sync-schemas", gens)
	}
}

// TestShipPlan_GeneratedSurfaceRetry_SkippedAfterScopeRefusal is the
// COUNTERFACTUAL for the `!gatingRejected` guard: a revise that both narrows
// undeclaredly AND misses a derivative yields exactly one plan_scope_retry entry
// and ZERO plan_generated_surface_retry entries (a stage is failed once).
func TestShipPlan_GeneratedSurfaceRetry_SkippedAfterScopeRefusal(t *testing.T) {
	s, rr, art, sf, au := newPlanSequenceServer(t)
	runRow := rr.seedRun()
	planStage := rr.seedStage(runRow.ID, 0, run.StageStateRunning)
	planStage.RequiresApproval = true
	priv, _ := sf.issue(t, runRow.ID)
	// Base scopes the canonical source AND its derivative.
	base := removalPlanBody(t, []string{"b/a.go", gsOpenAPI, gsAPIMd}, nil)
	seedReviseBase(t, art, au, runRow.ID, planStage.ID, base)

	// Narrowed drops api.md undeclared (scope regression) AND keeps openapi.yaml
	// (so the generated-surface gate WOULD fire, if reached).
	narrowed := removalPlanBody(t, []string{"b/a.go", gsOpenAPI}, nil)
	wp := shipPlanRequest(t, s, runRow.ID, planStage.ID, priv, narrowed, "")
	if wp.Code != http.StatusCreated {
		t.Fatalf("plan status = %d, want 201:\n%s", wp.Code, wp.Body.String())
	}
	if n := countScopeRetryEntries(t, au, runRow.ID); n != 1 {
		t.Errorf("plan_scope_retry entries = %d, want 1 (the scope refusal fires first)", n)
	}
	if n := countGeneratedSurfaceRetryEntries(t, au, runRow.ID); n != 0 {
		t.Errorf("plan_generated_surface_retry entries = %d, want 0 (skipped after the scope refusal)", n)
	}
}

// TestShipPlan_GeneratedSurfaceRetry_AdvanceFailure_StillRefuses is the
// COUNTERFACTUAL for the Advance-error `return true` leg. The stage-state alone
// cannot distinguish (a re-opened pending stage cannot transition to
// awaiting_approval, so it parks either way), so the observable is a GATING
// reviewer: with `return true` the refusal stands and runPlanReviews is SKIPPED
// (reviewer NEVER called); flipping the leg to `return false` falls through and
// the reviewer runs. The entry is committed and the stage is left at pending
// regardless.
func TestShipPlan_GeneratedSurfaceRetry_AdvanceFailure_StillRefuses(t *testing.T) {
	rr := &recordingOrchestratorRepo{orchestratorRepo: newOrchestratorRepo()}
	art, sf, au := newFakeArtifactRepo(), newSigningFake(), newStoringAuditFake()
	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-sonnet-4-6",
	}
	s := New(Config{Addr: "127.0.0.1:0", SigningRepo: sf, TraceStore: newTraceStoreFake(),
		AuditRepo: au, RunRepo: rr, ArtifactRepo: art,
		PlanReviewers: singleReviewerSet{reviewer},
		Orchestrator: &orchestrator.Orchestrator{Runs: &advanceFailRepo{
			recordingOrchestratorRepo: rr, err: fmt.Errorf("advance unavailable")}}})
	runRow := rr.seedRun()
	runRow.WorkflowID = "feature_change"
	runRow.WorkflowSpec = specGatingReviewersWithConstraints // gating reviewers, cap 3
	planStage := rr.seedStage(runRow.ID, 0, run.StageStateRunning)
	planStage.RequiresApproval = true
	priv, _ := sf.issue(t, runRow.ID)

	body := genSurfacePlanBody(t, []plan.ScopeFile{{Path: gsOpenAPI, Operation: plan.FileOpModify}}, nil)
	wp := shipPlanRequest(t, s, runRow.ID, planStage.ID, priv, body, "")
	if wp.Code != http.StatusCreated {
		t.Fatalf("plan status = %d, want 201:\n%s", wp.Code, wp.Body.String())
	}
	if n := countGeneratedSurfaceRetryEntries(t, au, runRow.ID); n != 1 {
		t.Errorf("plan_generated_surface_retry entries = %d, want 1 (the refusal stands through an Advance failure)", n)
	}
	if got := rr.stagesByID[planStage.ID].State; got != run.StageStatePending {
		t.Errorf("stage state = %q, want pending (re-opened; Advance failure returns true)", got)
	}
	// The load-bearing signal: gatingRejected stayed true, so the gating review
	// was SKIPPED. A `return false` on the Advance-error leg would run it.
	reviewer.mu.Lock()
	calls := len(reviewer.calls)
	reviewer.mu.Unlock()
	if calls != 0 {
		t.Errorf("reviewer calls = %d, want 0 (the refusal must skip plan review; a return-false Advance leg would run it)", calls)
	}
}

// TestShipPlan_GeneratedSurfaceRetry_NilOrchestrator_Parks: with no Orchestrator
// the gate declines and the plan parks at the gate with no entry. COUNTERFACTUAL
// for the nil-dependency guard.
func TestShipPlan_GeneratedSurfaceRetry_NilOrchestrator_Parks(t *testing.T) {
	rr := &recordingOrchestratorRepo{orchestratorRepo: newOrchestratorRepo()}
	art, sf, au := newFakeArtifactRepo(), newSigningFake(), newStoringAuditFake()
	s := New(Config{Addr: "127.0.0.1:0", SigningRepo: sf, TraceStore: newTraceStoreFake(),
		AuditRepo: au, RunRepo: rr, ArtifactRepo: art}) // no Orchestrator
	runRow := rr.seedRun()
	planStage := rr.seedStage(runRow.ID, 0, run.StageStateRunning)
	planStage.RequiresApproval = true
	priv, _ := sf.issue(t, runRow.ID)

	body := genSurfacePlanBody(t, []plan.ScopeFile{{Path: gsOpenAPI, Operation: plan.FileOpModify}}, nil)
	wp := shipPlanRequest(t, s, runRow.ID, planStage.ID, priv, body, "")
	if wp.Code != http.StatusCreated {
		t.Fatalf("plan status = %d, want 201:\n%s", wp.Code, wp.Body.String())
	}
	if n := countGeneratedSurfaceRetryEntries(t, au, runRow.ID); n != 0 {
		t.Errorf("plan_generated_surface_retry entries = %d, want 0 (nil Orchestrator declines)", n)
	}
	if got := rr.stagesByID[planStage.ID].State; got != run.StageStateAwaitingApproval {
		t.Errorf("stage state = %q, want awaiting_approval (declined refusal falls through)", got)
	}
}

// TestShipPlan_GeneratedSurfaceRetry_FailureReasonCapped: a decomposition with a
// large exemption-free fan-out drives the stage-failure reason over the cap, so
// it is truncated to <= maxSchemaValidationErrorBytes + the marker. COUNTERFACTUAL
// for the reason cap.
func TestShipPlan_GeneratedSurfaceRetry_FailureReasonCapped(t *testing.T) {
	rr := &recordingOrchestratorRepo{orchestratorRepo: newOrchestratorRepo()}
	// RetryStage faults so the failed stage KEEPS its FailureReason for us to
	// read (RetryStage would otherwise clear it), mirroring the scope-retry
	// capped test.
	fault := &scopeRetryFaultRepo{recordingOrchestratorRepo: rr, retryStageErr: fmt.Errorf("retry-stage unavailable")}
	art, sf, au := newFakeArtifactRepo(), newSigningFake(), newStoringAuditFake()
	s := New(Config{Addr: "127.0.0.1:0", SigningRepo: sf, TraceStore: newTraceStoreFake(),
		AuditRepo: au, RunRepo: fault, ArtifactRepo: art,
		Orchestrator: &orchestrator.Orchestrator{Runs: fault}})
	runRow := rr.seedRun()
	planStage := rr.seedStage(runRow.ID, 0, run.StageStateRunning)
	planStage.RequiresApproval = true
	priv, _ := sf.issue(t, runRow.ID)

	// Each 200-char title inflates that finding's reason part; 7 distinct
	// canonical sources (five two-generator, two single) give >= 12 findings,
	// pushing the joined reason past the 4000-byte cap.
	longTitle := func(seed string) string {
		return (seed + " ") + strings.Repeat("x", 200-len(seed)-1)
	}
	subs := []subPlanScope{
		{title: longTitle("t1"), files: []plan.ScopeFile{{Path: gsOpenAPI, Operation: plan.FileOpModify}}},
		{title: longTitle("t2"), files: []plan.ScopeFile{{Path: "cli/internal/cmdinfo/cmdinfo.go", Operation: plan.FileOpModify}}},
		{title: longTitle("t3"), files: []plan.ScopeFile{{Path: "docs/spec/workflow-v0.schema.json", Operation: plan.FileOpModify}}},
		{title: longTitle("t4"), files: []plan.ScopeFile{{Path: "docs/spec/workflow-v1.schema.json", Operation: plan.FileOpModify}}},
		{title: longTitle("t5"), files: []plan.ScopeFile{{Path: gsWf2Canon, Operation: plan.FileOpModify}}},
		{title: longTitle("t6"), files: []plan.ScopeFile{{Path: "docs/spec/plan-standard-v1.schema.json", Operation: plan.FileOpModify}}},
		{title: longTitle("t7"), files: []plan.ScopeFile{{Path: gsWMCanon, Operation: plan.FileOpModify}}},
	}
	body := decomposedScopePlanBody(t,
		[]plan.ScopeFile{{Path: "backend/internal/server/upload.go", Operation: plan.FileOpModify}}, subs)
	wp := shipPlanRequest(t, s, runRow.ID, planStage.ID, priv, body, "")
	if wp.Code != http.StatusCreated {
		t.Fatalf("plan status = %d, want 201:\n%s", wp.Code, wp.Body.String())
	}
	if n := countGeneratedSurfaceRetryEntries(t, au, runRow.ID); n != 1 {
		t.Fatalf("plan_generated_surface_retry entries = %d, want 1", n)
	}
	got := rr.stagesByID[planStage.ID]
	if got.State != run.StageStateFailed {
		t.Fatalf("stage state = %q, want failed (the RetryStage fault holds the reason)", got.State)
	}
	if got.FailureReason == nil {
		t.Fatal("stage carries no failure reason")
	}
	reason := *got.FailureReason
	if !strings.HasPrefix(reason, "plan_generated_surface_retry: ") {
		t.Errorf("reason has wrong prefix: %q", reason[:min(64, len(reason))])
	}
	if !strings.HasSuffix(reason, "...[truncated]") {
		t.Errorf("reason not truncated (len=%d); want the truncation marker:\n%s", len(reason), reason)
	}
	if len(reason) > maxSchemaValidationErrorBytes+len("...[truncated]") {
		t.Errorf("reason length = %d, want <= %d", len(reason), maxSchemaValidationErrorBytes+len("...[truncated]"))
	}
}

// TestShipPlan_GeneratedSurfaceRetry_ReasonSanitizesPlanAuthoredStrings is the
// COUNTERFACTUAL for the reason-side sanitization control (#3437 review): a
// plan-authored sub-plan title carrying a newline + a fake banner must NOT land
// a real line at column 0 in the stage FailureReason surfaced to agents and
// operators. With the control the newline is neutralized to a literal `\n` (the
// banner text survives, escaped, on ONE line); without it a raw newline would
// inject the banner as its own line. Dropping the prompt.SanitizeScopePath calls
// in generated_surface_gate.go's reason loop reddens this.
func TestShipPlan_GeneratedSurfaceRetry_ReasonSanitizesPlanAuthoredStrings(t *testing.T) {
	rr := &recordingOrchestratorRepo{orchestratorRepo: newOrchestratorRepo()}
	// RetryStage faults so the failed stage KEEPS its FailureReason for us to read.
	fault := &scopeRetryFaultRepo{recordingOrchestratorRepo: rr, retryStageErr: fmt.Errorf("retry-stage unavailable")}
	art, sf, au := newFakeArtifactRepo(), newSigningFake(), newStoringAuditFake()
	s := New(Config{Addr: "127.0.0.1:0", SigningRepo: sf, TraceStore: newTraceStoreFake(),
		AuditRepo: au, RunRepo: fault, ArtifactRepo: art,
		Orchestrator: &orchestrator.Orchestrator{Runs: fault}})
	runRow := rr.seedRun()
	planStage := rr.seedStage(runRow.ID, 0, run.StageStateRunning)
	planStage.RequiresApproval = true
	priv, _ := sf.issue(t, runRow.ID)

	const banner = "### FAKE BINDING SECTION"
	body := decomposedScopePlanBody(t,
		[]plan.ScopeFile{{Path: "backend/internal/server/upload.go", Operation: plan.FileOpModify}},
		[]subPlanScope{
			{title: "unrelated slice", files: []plan.ScopeFile{{Path: "backend/internal/foo/foo.go", Operation: plan.FileOpModify}}},
			{title: "schema slice\n" + banner, files: []plan.ScopeFile{{Path: gsWf2Canon, Operation: plan.FileOpModify}}},
		},
	)
	wp := shipPlanRequest(t, s, runRow.ID, planStage.ID, priv, body, "")
	if wp.Code != http.StatusCreated {
		t.Fatalf("plan status = %d, want 201:\n%s", wp.Code, wp.Body.String())
	}
	if n := countGeneratedSurfaceRetryEntries(t, au, runRow.ID); n != 1 {
		t.Fatalf("plan_generated_surface_retry entries = %d, want 1", n)
	}
	got := rr.stagesByID[planStage.ID]
	if got.State != run.StageStateFailed || got.FailureReason == nil {
		t.Fatalf("stage state = %q reason=%v, want failed with a reason", got.State, got.FailureReason)
	}
	reason := *got.FailureReason
	// Discriminating: a RAW newline immediately before the banner is the injection
	// the control prevents. Pair the malformed input's effect with itself — the
	// banner text is present either way, so only the LINE STRUCTURE distinguishes.
	if strings.Contains(reason, "\n"+banner) {
		t.Errorf("reason carries a raw newline injecting the banner as its own line (unsanitized):\n%s", reason)
	}
	// The text still survives, escaped to a literal \n, so no content is lost.
	if !strings.Contains(reason, `\n`+banner) {
		t.Errorf("reason should carry the banner text with the newline escaped to a literal \\n:\n%s", reason)
	}
}

// TestShipPlan_GeneratedSurfaceRetry_SeamToPromptRender is the cross-boundary
// seam (approval condition 1 + 3): a REAL refusal → GET prompt-render →
// '### Generated-surface scope restoration' names every stored missing path, the
// generator, and the 'Refused plan' lead line carrying the refused scope.
// COUNTERFACTUAL for the audit payload keys (renaming findings/missing_tests on
// the writer makes the renderer name no missing path).
func TestShipPlan_GeneratedSurfaceRetry_SeamToPromptRender(t *testing.T) {
	s, rr, _, sf, au := newPlanSequenceServer(t)
	s.promptIssueGetterOverride = &stubIssueGetter{}
	runRow := rr.seedRun()
	runRow.RequiresCharter = chFalse()
	planStage := rr.seedStage(runRow.ID, 0, run.StageStateRunning)
	planStage.RequiresApproval = true
	priv, _ := sf.issue(t, runRow.ID)

	body := genSurfacePlanBody(t, []plan.ScopeFile{{Path: gsOpenAPI, Operation: plan.FileOpModify}}, nil)
	wp := shipPlanRequest(t, s, runRow.ID, planStage.ID, priv, body, "")
	if wp.Code != http.StatusCreated {
		t.Fatalf("plan status = %d, want 201:\n%s", wp.Code, wp.Body.String())
	}
	p := lastGeneratedSurfaceRetryPayload(t, au, runRow.ID)
	if len(p.Findings) == 0 {
		t.Fatal("no findings recorded")
	}

	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/stages/%s/prompt-render", planStage.ID), nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("prompt-render status = %d:\n%s", w.Code, w.Body.String())
	}
	var pr promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &pr); err != nil {
		t.Fatalf("decode prompt response: %v", err)
	}
	if !strings.Contains(pr.Prompt, "### Generated-surface scope restoration (binding — this plan was REFUSED)") {
		t.Errorf("rendered prompt missing the restoration heading:\n%s", pr.Prompt)
	}
	// Assert the RESTORATION LINE names the derivative INLINE — a FIXED expected
	// string, NOT one re-decoded through the same `missing_tests` key the writer
	// used (trap (a): decoding the readback with the same key would go empty in
	// lockstep with a corrupted writer key and pass vacuously). Renaming the
	// writer's missing_tests key empties the loader's decode, blanking this line.
	// gsAPIMd also appears in the first-shot derivative map, so we match the
	// restoration LINE specifically, not a bare substring.
	wantLine := gsOpenAPI + " generates " + gsAPIMd + " via `" + testSweepGeneratorSiteReference + "`"
	if !strings.Contains(pr.Prompt, wantLine) {
		t.Errorf("rendered restoration line missing/blank:\nwant substring: %s\n---\n%s", wantLine, pr.Prompt)
	}
	// The payload keys the seam depends on were populated on the writer.
	if len(p.Findings) != 1 || len(p.Findings[0].MissingTests) != 1 || p.Findings[0].MissingTests[0] != gsAPIMd {
		t.Errorf("stored finding payload = %+v, want one finding with missing_tests=[%s]", p.Findings, gsAPIMd)
	}
	// Establish the Refused plan block's CONTENTS, not a bare prompt-wide
	// substring (#3437 review): gsOpenAPI already appears ABOVE this block in the
	// first-shot derivative map and the restoration line, so a prompt-wide
	// strings.Contains would pass even with the block empty. Anchor to the lead
	// line and assert the scope appears in the region AFTER it — the refused plan
	// blob — which is discriminating against those earlier occurrences and would
	// go red if the loader attached the wrong (or no) artifact.
	leadIdx := strings.Index(pr.Prompt, "Refused plan (re-emit it with the derivative paths added")
	if leadIdx < 0 {
		t.Fatalf("rendered prompt missing the Refused plan lead line:\n%s", pr.Prompt)
	}
	refusedBlock := pr.Prompt[leadIdx:]
	if !strings.Contains(refusedBlock, gsOpenAPI) {
		t.Errorf("rendered Refused plan block missing the refused scope %q:\n%s", gsOpenAPI, refusedBlock)
	}
}

// TestLoadGeneratedSurfaceRestoration_UndecodablePayload_Nil: an undecodable
// entry payload returns nil restoration (prompt renders without the section).
func TestLoadGeneratedSurfaceRestoration_UndecodablePayload_Nil(t *testing.T) {
	s, rr, _, _, au := newPlanSequenceServer(t)
	runRow := rr.seedRun()
	stageID := uuid.New()
	kind := audit.ActorKind("system")
	if _, err := au.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID:     runRow.ID,
		StageID:   &stageID,
		Category:  categoryPlanGeneratedSurfaceRetry,
		ActorKind: &kind,
		Payload:   json.RawMessage(`{"findings": "not-an-array"}`),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if got := s.loadGeneratedSurfaceRestoration(context.Background(), runRow.ID, stageID); got != nil {
		t.Errorf("loadGeneratedSurfaceRestoration = %+v, want nil (undecodable payload)", got)
	}
}

// TestLoadGeneratedSurfaceRestoration_OtherStageIgnored: an entry recorded for a
// DIFFERENT stage is not returned for this stage.
func TestLoadGeneratedSurfaceRestoration_OtherStageIgnored(t *testing.T) {
	s, rr, _, _, au := newPlanSequenceServer(t)
	runRow := rr.seedRun()
	thisStage, otherStage := uuid.New(), uuid.New()
	payload, _ := json.Marshal(map[string]any{
		"findings": []map[string]any{{"trigger_path": gsOpenAPI, "missing_tests": []string{gsAPIMd}, "generator": testSweepGeneratorSiteReference}},
	})
	kind := audit.ActorKind("system")
	if _, err := au.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID:     runRow.ID,
		StageID:   &otherStage,
		Category:  categoryPlanGeneratedSurfaceRetry,
		ActorKind: &kind,
		Payload:   payload,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if got := s.loadGeneratedSurfaceRestoration(context.Background(), runRow.ID, thisStage); got != nil {
		t.Errorf("loadGeneratedSurfaceRestoration = %+v, want nil (entry belongs to another stage)", got)
	}
}
