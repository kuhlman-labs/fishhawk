package mcpe2e_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	planpkg "github.com/kuhlman-labs/fishhawk/backend/internal/plan"
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

// rawEstimateWorkflowSpec is predictedRuntimeWorkflowSpec with an explicit
// 60-minute IMPLEMENT budget and a deliberately different 10-minute plan budget,
// so the gate under test must resolve the implement stage's number. With no
// runtime_observed history in the database, resolvePlanGateBudget leaves the
// budget at that 60m spec floor.
var rawEstimateWorkflowSpec = []byte(`version: "0.3"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
          timeout: "10m"
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
          timeout: "60m"
`)

// TestE2E_RawPredictedRuntime_GatesOnMaxNotCalibrated is the #2862 cross-boundary
// done-means test. It carries ONE plan artifact carrying BOTH runtime estimates
// through the whole composition, which per-layer unit tests each cover only in
// isolation:
//
//	artifact JSON  (the raw_predicted_runtime_minutes key as the planner writes it)
//	→ standard_v1 schema validation (the field is expressible at all — the plan
//	  schema root carries additionalProperties:false, so an unmirrored schema
//	  would reject these bytes outright)
//	→ the plan domain type (json decode into plan.Plan, then GateRuntimeMinutes)
//	→ the budget gate (checkPlanBudget's max(predicted, raw) comparison)
//	→ the HTTP 422 surface and the MCP tool error the operator actually sees
//
// The seam that broke in #2862 is exactly this one: a number that exists in the
// artifact but never reaches the comparison. Every per-layer test can pass while
// the field fails to cross — a json tag mismatch, an unsynced schema mirror, or
// a gate still reading PredictedRuntimeMinutes all leave the units green.
//
// No fakeBackend anywhere: the router, the repos, the database and the MCP
// server process are all real.
func TestE2E_RawPredictedRuntime_GatesOnMaxNotCalibrated(t *testing.T) {
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

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

	// The #2862 shape: a raw 90-minute estimate the planner multiplied by a
	// sub-1.0 fleet factor down to 50, against a 60-minute implement budget.
	const (
		rawMinutes        = 90
		calibratedMinutes = 50
	)

	// seedRun creates a run whose plan stage is parked at the approval gate and
	// carries a plan artifact with the given decomposition state, and returns
	// the run and plan stage ids.
	seedRun := func(decomposed bool) (uuid.UUID, uuid.UUID) {
		t.Helper()
		r, err := fx.runRepo.CreateRun(ctx, runpkg.CreateRunParams{
			Repo:          "kuhlman-labs/fishhawk",
			WorkflowID:    "feature_change",
			WorkflowSHA:   "deadbeef",
			TriggerSource: runpkg.TriggerCLI,
			WorkflowSpec:  rawEstimateWorkflowSpec,
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
		body := rawEstimatePlanJSON(t, calibratedMinutes, rawMinutes, decomposed)
		if _, err := artifactRepo.Create(ctx, artifact.CreateParams{
			StageID:       planStage.ID,
			Kind:          artifact.KindPlan,
			SchemaVersion: &schema,
			Content:       body,
			ContentHash:   "rawpred" + r.ID.String()[:8],
		}); err != nil {
			t.Fatalf("seed plan artifact: %v", err)
		}
		if _, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
			RunID:        r.ID,
			Sequence:     2,
			Type:         runpkg.StageTypeImplement,
			ExecutorKind: runpkg.ExecutorAgent,
			ExecutorRef:  "fishhawk/runner@v1",
		}); err != nil {
			t.Fatalf("CreateStage(implement): %v", err)
		}
		parkAtGate(t, ctx, fx.runRepo, planStage.ID)
		return r.ID, planStage.ID
	}

	// (a) The refusal. The calibrated 50 is UNDER the 60m budget; only the raw
	// 90 is over it. Before #2862 this approve returned 200.
	refusedRun, refusedPlanStage := seedRun(false)
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "fishhawk_approve_plan",
		Arguments: map[string]any{
			"run_id": refusedRun.String(),
			"reason": "looks reasonable",
		},
	})
	if err != nil {
		t.Fatalf("CallTool approve_plan (raw over budget): %v", err)
	}
	if !result.IsError {
		t.Fatalf("approve_plan succeeded for a plan whose RAW estimate (%d) exceeds the 60m implement budget; "+
			"the gate read the calibrated %d instead of max(%d, %d)", rawMinutes, calibratedMinutes, calibratedMinutes, rawMinutes)
	}
	errText := toolContentString(t, result)
	if !strings.Contains(errText, "plan_violates_budget") {
		t.Errorf("refusal must name plan_violates_budget; got:\n%s", errText)
	}
	// The error surface names BOTH numbers, so the operator can see that the
	// pre-calibration estimate is what the gate read.
	for _, want := range []string{"90", "50"} {
		if !strings.Contains(errText, want) {
			t.Errorf("refusal text missing %q (both estimates must reach the operator):\n%s", want, errText)
		}
	}
	// COMMITTED STATE: the refusal is PRE-Submit, so no approval row exists —
	// asserted from the database rather than inferred from the tool error.
	rows, err := approvalRepo.ListForStage(ctx, refusedPlanStage)
	if err != nil {
		t.Fatalf("ListForStage after refusal: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("approval rows after the refused approve = %d, want 0", len(rows))
	}
	// COMMITTED STATE: the crossing entry was recorded on the real audit chain.
	crossings, err := auditRepo.ListForRunByCategory(ctx, refusedRun, "plan_budget_calibration_crossing")
	if err != nil {
		t.Fatalf("ListForRunByCategory(plan_budget_calibration_crossing): %v", err)
	}
	if len(crossings) != 1 {
		t.Fatalf("plan_budget_calibration_crossing entries = %d, want 1", len(crossings))
	}
	var cx struct {
		RawPredictedMinutes  int    `json:"raw_predicted_minutes"`
		PredictedMinutes     int    `json:"predicted_minutes"`
		GatePredictedMinutes int    `json:"gate_predicted_minutes"`
		BudgetMinutes        int    `json:"budget_minutes"`
		GateOutcome          string `json:"gate_outcome"`
	}
	if err := json.Unmarshal(crossings[0].Payload, &cx); err != nil {
		t.Fatalf("decode crossing payload: %v", err)
	}
	if cx.RawPredictedMinutes != rawMinutes || cx.PredictedMinutes != calibratedMinutes ||
		cx.GatePredictedMinutes != rawMinutes || cx.BudgetMinutes != 60 || cx.GateOutcome != "refused" {
		t.Errorf("crossing payload = %+v, want raw %d / predicted %d / gate %d / budget 60 / refused",
			cx, rawMinutes, calibratedMinutes, rawMinutes)
	}

	// (b) The SAME two estimates with decomposition.sub_plans: the gate is
	// satisfied and the approve succeeds. This is what makes (a) a gate
	// refusal rather than a plan that simply cannot be approved — the remedy
	// #2862 says must stay reachable is reachable.
	okRun, okPlanStage := seedRun(true)
	result, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name: "fishhawk_approve_plan",
		Arguments: map[string]any{
			"run_id": okRun.String(),
			"reason": "decomposed as the gate requires",
		},
	})
	if err != nil {
		t.Fatalf("CallTool approve_plan (decomposed): %v", err)
	}
	if result.IsError {
		t.Fatalf("decomposed approve returned a tool error: %s", toolContentString(t, result))
	}
	rows, err = approvalRepo.ListForStage(ctx, okPlanStage)
	if err != nil {
		t.Fatalf("ListForStage after decomposed approve: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("approval rows after the decomposed approve = %d, want 1", len(rows))
	}
	crossings, err = auditRepo.ListForRunByCategory(ctx, okRun, "plan_budget_calibration_crossing")
	if err != nil {
		t.Fatalf("ListForRunByCategory (decomposed): %v", err)
	}
	if len(crossings) != 1 {
		t.Fatalf("plan_budget_calibration_crossing entries on the decomposed run = %d, want 1 "+
			"(the crossing is recorded even where the gate lets the approval proceed)", len(crossings))
	}
	if err := json.Unmarshal(crossings[0].Payload, &cx); err != nil {
		t.Fatalf("decode crossing payload (decomposed): %v", err)
	}
	if cx.GateOutcome != "decomposition_satisfied" {
		t.Errorf("gate_outcome = %q, want decomposition_satisfied", cx.GateOutcome)
	}
}

// rawEstimatePlanJSON is a standard_v1 plan carrying BOTH the calibrated
// predicted_runtime_minutes and the pre-calibration
// raw_predicted_runtime_minutes, optionally decomposed. It is validated against
// the real embedded schema before being handed to the artifact store, so a
// mirror that never gained the field fails HERE, at the composition's first
// seam, rather than silently downgrading the test to a legacy-shape plan that
// would pass for the wrong reason.
func rawEstimatePlanJSON(t *testing.T, calibrated, raw int, decomposed bool) []byte {
	t.Helper()
	m := map[string]any{
		"plan_version":                  "standard_v1",
		"ticket_reference":              map[string]any{"type": "github_issue", "url": "https://github.com/x/y/issues/2862", "id": "x/y#2862"},
		"generated_by":                  map[string]any{"agent": "claude-code", "model": "claude-opus-5", "timestamp": "2026-08-31T00:00:00Z"},
		"summary":                       "gate on the pre-calibration runtime estimate",
		"scope":                         map[string]any{"files": []map[string]any{{"path": "backend/internal/server/approvals.go", "operation": "modify"}}},
		"approach":                      []map[string]any{{"step": 1, "description": "Read the larger of the two estimates."}},
		"verification":                  map[string]any{"test_strategy": "Run the tests.", "rollback_plan": "Revert the PR."},
		"predicted_runtime_minutes":     calibrated,
		"raw_predicted_runtime_minutes": raw,
		"predicted_runtime_confidence":  "medium",
	}
	if decomposed {
		m["decomposition"] = map[string]any{
			"rationale": "the raw estimate exceeds the implement budget",
			"sub_plans": []map[string]any{
				{
					"title":                        "Slice one",
					"scope_hint":                   "the gate comparison",
					"scope":                        map[string]any{"files": []map[string]any{{"path": "backend/internal/server/approvals.go", "operation": "modify"}}},
					"predicted_runtime_minutes":    45,
					"predicted_runtime_confidence": "medium",
				},
				{
					"title":                        "Slice two",
					"scope_hint":                   "the crossing audit entry",
					"scope":                        map[string]any{"files": []map[string]any{{"path": "backend/internal/audit/categories.go", "operation": "modify"}}},
					"predicted_runtime_minutes":    45,
					"predicted_runtime_confidence": "medium",
					"depends_on":                   []any{0},
				},
			},
		}
	}
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	if err := planpkg.Validate(body); err != nil {
		t.Fatalf("fixture plan does not validate against the embedded standard_v1 schema "+
			"(raw_predicted_runtime_minutes may be missing from the mirror): %v", err)
	}
	return body
}
