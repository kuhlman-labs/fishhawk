package mcpe2e_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
)

// nearCapWorkflowSpec is a feature_change spec whose implement stage declares
// max_files_changed = 5 — the cap the near-cap advisory measures a plan's
// scope.files headroom against.
var nearCapWorkflowSpec = []byte(`version: "0.3"
workflows:
  feature_change:
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
        constraints:
          - max_files_changed: 5
`)

// TestE2E_PlanWarnings_NearCapAdvisory_ShipToGetPlan is the #2492 (E50.14)
// cross-component done-means — binding approval condition 1's genuinely
// end-to-end span. It proves the producer->consumer path per-layer unit tests
// cannot: a NEAR-cap plan (4 scope.files against the implement stage's cap of 5,
// leaving 1 file of headroom) shipped through the REAL backend HTTP plan-ship
// path (handleShipPlan -> runPlanWarnings PRODUCES the advisory string and writes
// it to a plan_warnings audit row in Postgres) is read back through the REAL
// fishhawk-mcp binary's get_plan surface (getPlan/loadPlanWarnings DELIVERS it in
// GetPlanOutput.plan_warnings). It asserts the RENDERED advisory carries the
// count, cap AND headroom — so a break anywhere on the real produce->deliver path
// (the string runPlanWarnings emits, the audit round-trip, or the get_plan
// resolver) fails HERE, not silently between two green per-layer tests.
//
// runPlanWarnings (server package) and getPlan (mcpserver package) are both
// unexported, so no single package test can reach both producer and consumer;
// this integration package is the proven home for the real span (as #2515's
// scope-edit E2E is for its seam).
func TestE2E_PlanWarnings_NearCapAdvisory_ShipToGetPlan(t *testing.T) {
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// One backend over the fixture's pool with the ship-plan write path
	// (Signing/Artifact/Audit/Run) AND the get_plan read path wired. GitHub is
	// wired so prompt-adjacent resolution never 503s; ApprovalRepo mirrors the
	// sibling E2E shape though this test stops at get_plan.
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

	// A fresh run CARRYING the near-cap workflow spec (the fixture run has none,
	// so its cap would not resolve), with its own per-run signing key so the
	// plan-ship signature verifies.
	r, err := fx.runRepo.CreateRun(ctx, runpkg.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef",
		TriggerSource: runpkg.TriggerCLI,
		WorkflowSpec:  nearCapWorkflowSpec,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	issued, err := signingRepo.Issue(ctx, r.ID, time.Hour)
	if err != nil {
		t.Fatalf("Issue signing key: %v", err)
	}

	planStage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:            r.ID,
		Sequence:         1,
		Type:             runpkg.StageTypePlan,
		ExecutorKind:     runpkg.ExecutorAgent,
		ExecutorRef:      "fishhawk/runner@v1",
		RequiresApproval: true,
	})
	if err != nil {
		t.Fatalf("CreateStage(plan): %v", err)
	}
	// handleShipPlan performs the terminal running -> awaiting_approval
	// transition, so leave the plan stage in running before shipping.
	walkToRunning(t, ctx, fx, planStage.ID)

	// Ship a NEAR-cap plan: 4 scope.files against the cap of 5 -> 1 file of
	// headroom, inside nearCapMargin but NOT over cap.
	const count, capLimit, headroom = 4, 5, 1
	scopeFiles := []string{
		"backend/internal/near/a.go",
		"backend/internal/near/b.go",
		"backend/internal/near/c.go",
		"backend/internal/near/d.go",
	}
	resp := shipPlanSigned(t, ctx, httpSrv.URL, r.ID, planStage.ID, issued.PrivateKey,
		regressionPlanJSON("near-cap plan", scopeFiles))
	_ = resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("ship near-cap plan status = %d, want 201", resp.StatusCode)
	}

	// Read the plan back through the REAL fishhawk-mcp get_plan surface.
	session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, httpSrv.URL)
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "fishhawk_get_plan",
		Arguments: map[string]any{"run_id": r.ID.String()},
	})
	if err != nil {
		t.Fatalf("CallTool fishhawk_get_plan: %v", err)
	}
	if result.IsError {
		t.Fatalf("get_plan returned error: %s", toolContentString(t, result))
	}

	var out struct {
		PlanWarnings []string `json:"plan_warnings"`
	}
	decodeStructured(t, result, &out)

	// The near-cap advisory produced by runPlanWarnings must reach the get_plan
	// plan_warnings field, naming the count, cap AND headroom.
	found := false
	for _, w := range out.PlanWarnings {
		if strings.Contains(w, "declares 4 files against the implement-stage max_files_changed cap of 5") &&
			strings.Contains(w, "only 1 file(s) of headroom remain") {
			found = true
		}
	}
	if !found {
		t.Errorf("get_plan plan_warnings = %v, want the near-cap advisory naming count=%d cap=%d headroom=%d",
			out.PlanWarnings, count, capLimit, headroom)
	}
}
