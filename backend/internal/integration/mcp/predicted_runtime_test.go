package mcpe2e_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
)

// predictedRuntimeWorkflowSpec is a minimal feature_change spec with a plan and
// an implement stage — enough for the plan gate to resolve and for the implement
// stage to be the one whose wait status the assertion reads.
var predictedRuntimeWorkflowSpec = []byte(`version: "0.3"
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
`)

// TestE2E_PredictedRuntime_DrivesAdvertisedPollInterval is the E48.62 / #2489
// cross-component done-means test. It carries ONE real prediction through the
// WHOLE composition, which the per-layer tests each cover only in isolation:
//
//	plan approval (fishhawk_approve_plan through the REAL fishhawk-mcp binary
//	  → the real backend approval handler)
//	→ persistence  (the on-approval stamp onto runs.predicted_runtime_minutes
//	  in real Postgres)
//	→ REST serialization (runResponse.predicted_runtime_minutes on
//	  GET /v0/runs/{id})
//	→ MCP client decode (the client Run mirror's json tag)
//	→ the advertised interval (implement_stage_wait_status.poll_interval_seconds
//	  on fishhawk_get_run_status)
//
// No fakeBackend anywhere: the router, the repos, the database and the MCP
// server process are all real. An incompatible composition — a json-tag
// mismatch at either seam, a stamp that never fires, a derivation wired to the
// wrong stage — passes every unit test and fails here.
func TestE2E_PredictedRuntime_DrivesAdvertisedPollInterval(t *testing.T) {
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// Stand up an artifact+approval-wired backend over the SAME pool so the
	// plan gate resolves and loadApprovedPlanForRun finds the artifact. The
	// operator fhk_* token authenticates against the same apitoken rows.
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

	session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, httpSrv.URL)

	// The motivating fan-out prediction from #2489.
	const predictedMinutes = 115

	r, err := fx.runRepo.CreateRun(ctx, runpkg.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef",
		TriggerSource: runpkg.TriggerCLI,
		WorkflowSpec:  predictedRuntimeWorkflowSpec,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
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
	schema := "standard_v1"
	planBytes := predictedRuntimePlanJSON(predictedMinutes)
	if _, err := artifactRepo.Create(ctx, artifact.CreateParams{
		StageID:       planStage.ID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &schema,
		Content:       planBytes,
		ContentHash:   "predruntime" + r.ID.String()[:8],
	}); err != nil {
		t.Fatalf("seed plan artifact: %v", err)
	}
	parkAtGate(t, ctx, fx.runRepo, planStage.ID)

	implStage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:        r.ID,
		Sequence:     2,
		Type:         runpkg.StageTypeImplement,
		ExecutorKind: runpkg.ExecutorAgent,
		ExecutorRef:  "fishhawk/runner@v1",
	})
	if err != nil {
		t.Fatalf("CreateStage(implement): %v", err)
	}

	// Before the approval the run is unstamped, so the surface advertises the
	// FLOOR. Asserting this first makes the post-approval assertion a genuine
	// state CHANGE rather than a value that could have been there all along.
	if got := implementPollInterval(t, ctx, session, r.ID); got != 30 {
		t.Fatalf("pre-approval implement poll_interval_seconds = %d, want 30 (unstamped run, un-started stage)", got)
	}

	// The real approval, through the real MCP binary. 115 minutes is over the
	// default 15m advisory budget, so the override rides along exactly as an
	// operator would type it.
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "fishhawk_approve_plan",
		Arguments: map[string]any{
			"run_id": r.ID.String(),
			"reason": "a long fan-out; accepted deliberately --override-budget",
		},
	})
	if err != nil {
		t.Fatalf("CallTool approve_plan: %v", err)
	}
	if result.IsError {
		t.Fatalf("approve_plan returned error: %s", toolContentString(t, result))
	}

	// Persistence leg: the stamp landed on the run row in real Postgres.
	stamped, err := fx.runRepo.GetRun(ctx, r.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if stamped.PredictedRuntimeMinutes != predictedMinutes {
		t.Fatalf("runs.predicted_runtime_minutes = %d, want %d (the approval stamp did not land)",
			stamped.PredictedRuntimeMinutes, predictedMinutes)
	}

	// Put the implement stage 5400s into its run. started_at is written under
	// COALESCE, so the transition ladder cannot backdate it — set it directly.
	// This is the fixture, not the control: the derivation under test reads it.
	startedAt := time.Now().UTC().Add(-5400 * time.Second)
	for _, to := range []runpkg.StageState{runpkg.StageStateDispatched, runpkg.StageStateRunning} {
		if _, err := fx.runRepo.TransitionStage(ctx, implStage.ID, to, nil); err != nil {
			t.Fatalf("TransitionStage(implement → %s): %v", to, err)
		}
	}
	if _, err := fx.pool.Exec(ctx,
		`UPDATE stages SET started_at = $1 WHERE id = $2`, startedAt, implStage.ID); err != nil {
		t.Fatalf("backdate implement started_at: %v", err)
	}

	// The whole composition, read off the MCP surface: (115*60 - 5400) / 4 = 375.
	//
	// The server reads its own clock a fraction of a second after the backdate,
	// so elapsed is 5400s plus a sliver and the quarter can round down by one.
	// The band is one second wide — deliberately far too narrow to be satisfied
	// by the 30s floor or the 900s ceiling, which are the two values a broken
	// composition actually degrades to.
	got := implementPollInterval(t, ctx, session, r.ID)
	if got < 373 || got > 375 {
		t.Errorf("implement poll_interval_seconds = %d, want 375 ((115*60 - 5400) / 4); "+
			"30 means the prediction never reached the derivation, 900 means elapsed did not", got)
	}
}

// implementPollInterval calls fishhawk_get_run_status through the MCP session
// and returns the advertised implement_stage_wait_status.poll_interval_seconds.
func implementPollInterval(t *testing.T, ctx context.Context, session *mcp.ClientSession, runID uuid.UUID) int {
	t.Helper()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "fishhawk_get_run_status",
		Arguments: map[string]any{"run_id": runID.String()},
	})
	if err != nil {
		t.Fatalf("CallTool fishhawk_get_run_status: %v", err)
	}
	if result.IsError {
		t.Fatalf("get_run_status returned error: %s", toolContentString(t, result))
	}
	var out struct {
		ImplementStageWaitStatus *struct {
			Stage               string `json:"stage"`
			Status              string `json:"status"`
			PollIntervalSeconds int    `json:"poll_interval_seconds"`
		} `json:"implement_stage_wait_status"`
	}
	decodeStructured(t, result, &out)
	if out.ImplementStageWaitStatus == nil {
		t.Fatal("implement_stage_wait_status is nil")
	}
	return out.ImplementStageWaitStatus.PollIntervalSeconds
}

// predictedRuntimePlanJSON is a minimal valid standard_v1 plan carrying the
// given predicted_runtime_minutes.
func predictedRuntimePlanJSON(minutes int) []byte {
	body, _ := json.Marshal(map[string]any{
		"plan_version":                 "standard_v1",
		"ticket_reference":             map[string]any{"type": "github_issue", "url": "https://github.com/x/y/issues/2489", "id": "x/y#2489"},
		"generated_by":                 map[string]any{"agent": "claude-code", "model": "claude-opus-5", "timestamp": "2026-08-08T00:00:00Z"},
		"summary":                      "derive the advertised stage-wait poll cadence",
		"scope":                        map[string]any{"files": []map[string]any{{"path": "backend/internal/mcpserver/stage_wait.go", "operation": "modify"}}},
		"approach":                     []map[string]any{{"step": 1, "description": "Derive the cadence."}},
		"verification":                 map[string]any{"test_strategy": "Run the tests.", "rollback_plan": "Revert the PR."},
		"predicted_runtime_minutes":    minutes,
		"predicted_runtime_confidence": "medium",
	})
	return body
}
