package mcpe2e_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestE2E_Revise_ConstraintInjectedAndPlanRebound is the cross-component
// done-means test for the plan-gate `revise` verdict (E22.X / #1099). It
// drives the seam the per-layer unit tests cannot cover on their own (cf.
// #618): an operator triggering a revise through the REAL fishhawk-mcp
// binary → the backend re-opening the parked plan stage → the
// plan_revised audit entry landing in Postgres carrying the operator
// constraint → the prompt renderer reading it back AND loading the prior
// plan as the revision base → the stage returning to the review→approve
// gate where an approve succeeds.
//
// What this harness exercises end-to-end (real MCP binary → real backend
// HTTP → real Postgres):
//
//   - the MCP revise tool resolves the plan stage from the run id, the
//     operator fhk_* token (write:approvals) authorizes, run.RevisePlanStage
//     re-opens the plan stage awaiting_approval → pending, and the
//     plan_revised audit entry persists carrying the binding constraint;
//   - the deterministic prompt renderer reads that audit entry back AND
//     loads the prior plan artifact, emitting the dedicated "### Revision
//     constraint (binding ...)" section with both the constraint and the
//     prior plan as the revision base — the audit+artifact → prompt seam;
//   - the gate round-trips: re-parked at awaiting_approval, an approve
//     through the MCP binary succeeds.
func TestE2E_Revise_ConstraintInjectedAndPlanRebound(t *testing.T) {
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// newFixture's server has no GitHub or ArtifactRepo wired; the
	// prompt-render handler short-circuits to 503 without GitHub
	// (issueGetter() == nil) and cannot load a revision base without the
	// artifact store. Stand up a second backend over the SAME pool with
	// both wired so we can assert the rendered prompt. The operator fhk_*
	// token authenticates against the same apitoken rows (same pool).
	auditRepo := audit.NewPostgresRepository(fx.pool)
	signingRepo := signing.NewPostgresRepository(fx.pool)
	artifactRepo := artifact.NewPostgresRepository(fx.pool)
	approvalRepo := approval.NewPostgresRepository(fx.pool)
	srv := server.New(server.Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      fx.runRepo,
		AuditRepo:    auditRepo,
		SigningRepo:  signingRepo,
		ArtifactRepo: artifactRepo,
		ApprovalRepo: approvalRepo,
		APITokenRepo: fx.apitokenRepo,
		GitHub:       githubclient.New(nil),
	})
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)

	// 1. Seed a plan stage parked at the approval gate. CreateStage lands
	// it in pending; parkAtGate walks it pending → dispatched → running →
	// awaiting_approval so it is a valid revise candidate.
	planStage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:            fx.runID,
		Sequence:         1,
		Type:             runpkg.StageTypePlan,
		ExecutorKind:     runpkg.ExecutorAgent,
		ExecutorRef:      "fishhawk/runner@v1",
		RequiresApproval: true,
	})
	if err != nil {
		t.Fatalf("CreateStage(plan): %v", err)
	}

	// 2. Seed the prior plan artifact on the plan stage — the revision base
	// the re-dispatched prompt must carry. A recognizable summary marker
	// lets us assert the base block rendered.
	const basePlanSummary = "REVISE_BASE_PLAN_MARKER add a dryRun flag to the dispatcher"
	schema := "standard_v1"
	planContent, _ := json.Marshal(map[string]any{
		"plan_version":                 "standard_v1",
		"ticket_reference":             map[string]any{"type": "github_issue", "url": "https://github.com/x/y/issues/1", "id": "x/y#1"},
		"generated_by":                 map[string]any{"agent": "claude-code", "model": "claude-opus-4-8", "timestamp": "2026-06-15T00:00:00Z"},
		"summary":                      basePlanSummary,
		"scope":                        map[string]any{"files": []map[string]any{{"path": "backend/internal/webhook/dispatcher.go", "operation": "modify"}}},
		"approach":                     []map[string]any{{"step": 1, "description": "Plumb dryRun through Handle."}},
		"verification":                 map[string]any{"test_strategy": "Run the dispatcher tests.", "rollback_plan": "Revert the PR."},
		"predicted_runtime_minutes":    20,
		"predicted_runtime_confidence": "high",
	})
	if _, err := artifactRepo.Create(ctx, artifact.CreateParams{
		StageID:       planStage.ID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &schema,
		Content:       planContent,
		ContentHash:   "deadbeef",
	}); err != nil {
		t.Fatalf("seed plan artifact: %v", err)
	}

	parkAtGate(t, ctx, fx.runRepo, planStage.ID)

	// 3. Trigger the revise through the real fishhawk-mcp binary, pointed at
	// the GitHub+artifact-wired backend. The tool resolves the plan stage
	// from the run id internally.
	session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, httpSrv.URL)

	const constraint = "REVISE_CONSTRAINT_MARKER use the existing httpclient retry helper, do not add a new backoff package"
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "fishhawk_revise_plan",
		Arguments: map[string]any{
			"run_id":     fx.runID.String(),
			"constraint": constraint,
		},
	})
	if err != nil {
		t.Fatalf("CallTool fishhawk_revise_plan: %v", err)
	}
	if result.IsError {
		t.Fatalf("revise tool returned error: %s", toolContentString(t, result))
	}

	var reviseOut struct {
		Stage struct {
			ID    string `json:"id"`
			State string `json:"state"`
			Type  string `json:"type"`
		} `json:"stage"`
		StageID string `json:"stage_id"`
	}
	decodeStructured(t, result, &reviseOut)
	if reviseOut.Stage.ID != planStage.ID.String() {
		t.Errorf("revise stage id = %q, want %s", reviseOut.Stage.ID, planStage.ID)
	}
	if reviseOut.Stage.State != string(runpkg.StageStatePending) {
		t.Errorf("revise stage state = %q, want pending (re-opened, no orchestrator wired)", reviseOut.Stage.State)
	}

	// 4. The plan_revised audit entry landed in Postgres carrying the
	// binding constraint — the durable record the bound is counted against
	// and the prompt renderer reads back.
	entries, err := auditRepo.ListForRunByCategory(ctx, fx.runID, server.CategoryPlanRevised)
	if err != nil {
		t.Fatalf("ListForRunByCategory: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("plan_revised entries = %d, want 1", len(entries))
	}
	var revised struct {
		PassOrdinal     int    `json:"pass_ordinal"`
		RemainingBudget int    `json:"remaining_budget"`
		Conditions      string `json:"conditions"`
	}
	if err := json.Unmarshal(entries[0].Payload, &revised); err != nil {
		t.Fatalf("unmarshal plan_revised payload: %v", err)
	}
	if revised.PassOrdinal != 1 {
		t.Errorf("pass_ordinal = %d, want 1", revised.PassOrdinal)
	}
	if revised.RemainingBudget != 0 {
		t.Errorf("remaining_budget = %d, want 0 (default bound is 1)", revised.RemainingBudget)
	}
	if !strings.Contains(revised.Conditions, constraint) {
		t.Errorf("plan_revised conditions = %q, want the binding constraint", revised.Conditions)
	}

	// 5. The deterministic prompt now renders the binding constraint AND
	// the prior plan as the revision base — the done-means seam. The stage
	// is in pending (runnable) after the re-open, so prompt-render serves it.
	rendered := getPromptRender(t, ctx, httpSrv.URL, planStage.ID)
	if !strings.Contains(rendered, "### Revision constraint (binding") {
		t.Errorf("rendered prompt missing the Revision constraint section:\n%s", rendered)
	}
	if !strings.Contains(rendered, constraint) {
		t.Errorf("rendered prompt missing the binding constraint %q", constraint)
	}
	if !strings.Contains(rendered, "Prior plan (the revision base):") {
		t.Errorf("rendered prompt missing the revision-base block:\n%s", rendered)
	}
	if !strings.Contains(rendered, basePlanSummary) {
		t.Errorf("rendered prompt missing the prior plan as revision base (marker %q):\n%s", basePlanSummary, rendered)
	}

	// 6. The gate round-trips: re-park the plan stage at awaiting_approval
	// (modelling the re-planned re-dispatch landing back at the gate, which
	// this agent-less harness does not run), then approve through the MCP
	// binary — it must succeed.
	parkAtGate(t, ctx, fx.runRepo, planStage.ID)

	// --override-budget keeps the gate's predicted-runtime check from
	// failing this agent-less fixture's seeded plan; the revise→approve
	// round-trip is what this asserts, not the budget gate.
	approveResult, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "fishhawk_approve_plan",
		Arguments: map[string]any{"run_id": fx.runID.String(), "reason": "--override-budget"},
	})
	if err != nil {
		t.Fatalf("CallTool fishhawk_approve_plan: %v", err)
	}
	if approveResult.IsError {
		t.Fatalf("approve after revise returned error: %s", toolContentString(t, approveResult))
	}
	var approveOut struct {
		Stage struct {
			State string `json:"state"`
		} `json:"stage"`
	}
	decodeStructured(t, approveResult, &approveOut)
	if approveOut.Stage.State != string(runpkg.StageStateSucceeded) {
		t.Errorf("post-approve plan stage state = %q, want succeeded", approveOut.Stage.State)
	}

	// Still exactly one plan_revised entry — the approve wrote none.
	entries, err = auditRepo.ListForRunByCategory(ctx, fx.runID, server.CategoryPlanRevised)
	if err != nil {
		t.Fatalf("ListForRunByCategory (post-approve): %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("plan_revised entries after approve = %d, want 1", len(entries))
	}
}
