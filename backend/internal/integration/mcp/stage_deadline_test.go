package mcpe2e_test

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
)

// stageDeadlineWorkflowSpec is a minimal feature_change spec whose implement
// stage declares an explicit 60m executor timeout, so resolveAgentTimeout
// resolves a deterministic 3600s agent wall clock (no plan/calibration widens
// it) — the value the whole seam under test must carry to the MCP surface.
var stageDeadlineWorkflowSpec = []byte(`version: "0.3"
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
          timeout: 60m
`)

// TestE2E_StageDeadline_RemainingBudgetOnWaitStatus is the #2540 cross-boundary
// done-means test. It carries ONE spec-resolved agent budget through the WHOLE
// composition, which the per-layer units each cover only in isolation:
//
//	spec-resolved agent timeout (resolveAgentTimeout on the REAL backend)
//	→ REST serialization (stageResponse.agent_timeout_seconds on
//	  GET /v0/runs/{id}/stages)
//	→ MCP client decode (the client Stage mirror's json tag)
//	→ classification (stageWaitStatusFor folds elapsed + remaining)
//	→ the surfaced fields (implement_stage_wait_status.{elapsed_seconds,
//	  agent_timeout_seconds, deadline_seconds_remaining} on
//	  fishhawk_get_run_status)
//
// No fakeBackend anywhere: the router, the repos, the database and the MCP
// server process are all real. A json-tag mismatch at either seam — the wire
// mirror the per-layer tests cannot catch — passes every unit test and fails
// here (#618). It also asserts the identity elapsed + remaining ==
// agent_timeout (so a clock/derivation drift between the two shows as a failing
// identity, not silent skew) and that all three fields drop once the stage goes
// terminal.
func TestE2E_StageDeadline_RemainingBudgetOnWaitStatus(t *testing.T) {
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	auditRepo := audit.NewPostgresRepository(fx.pool)
	signingRepo := signing.NewPostgresRepository(fx.pool)
	srv := server.New(server.Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      fx.runRepo,
		AuditRepo:    auditRepo,
		SigningRepo:  signingRepo,
		APITokenRepo: fx.apitokenRepo,
		GitHub:       githubclient.New(nil),
	})
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)

	session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, httpSrv.URL)

	r, err := fx.runRepo.CreateRun(ctx, runpkg.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef",
		TriggerSource: runpkg.TriggerCLI,
		WorkflowSpec:  stageDeadlineWorkflowSpec,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID: r.ID, Sequence: 1, Type: runpkg.StageTypePlan,
		ExecutorKind: runpkg.ExecutorAgent, ExecutorRef: "fishhawk/runner@v1",
		RequiresApproval: true,
	}); err != nil {
		t.Fatalf("CreateStage(plan): %v", err)
	}
	implStage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID: r.ID, Sequence: 2, Type: runpkg.StageTypeImplement,
		ExecutorKind: runpkg.ExecutorAgent, ExecutorRef: "fishhawk/runner@v1",
	})
	if err != nil {
		t.Fatalf("CreateStage(implement): %v", err)
	}

	// Put the implement stage ~600s into its run. started_at is written under
	// COALESCE, so the transition ladder cannot backdate it — set it directly.
	// This is the fixture, not the control: the derivation under test reads it.
	const elapsed = 600
	const wantBudget = 3600 // the spec's 60m implement executor timeout
	startedAt := time.Now().UTC().Add(-elapsed * time.Second)
	for _, to := range []runpkg.StageState{runpkg.StageStateDispatched, runpkg.StageStateRunning} {
		if _, err := fx.runRepo.TransitionStage(ctx, implStage.ID, to, nil); err != nil {
			t.Fatalf("TransitionStage(implement → %s): %v", to, err)
		}
	}
	if _, err := fx.pool.Exec(ctx,
		`UPDATE stages SET started_at = $1 WHERE id = $2`, startedAt, implStage.ID); err != nil {
		t.Fatalf("backdate implement started_at: %v", err)
	}

	// Running: the deadline fields ride the surface, spec-resolved and consistent.
	ws := implementDeadlineStatus(t, ctx, session, r.ID)
	if ws == nil {
		t.Fatal("implement_stage_wait_status is nil")
	}
	if ws.Status != "running" {
		t.Fatalf("status = %q, want running", ws.Status)
	}
	if ws.AgentTimeoutSeconds != wantBudget {
		t.Errorf("agent_timeout_seconds = %d, want %d (the spec's 60m implement timeout)", ws.AgentTimeoutSeconds, wantBudget)
	}
	// elapsed is 600 plus the sliver between backdate and the server's clock read.
	if ws.ElapsedSeconds < elapsed || ws.ElapsedSeconds > elapsed+30 {
		t.Errorf("elapsed_seconds = %d, want ~%d", ws.ElapsedSeconds, elapsed)
	}
	if ws.DeadlineSecondsRemaining == nil {
		t.Fatal("deadline_seconds_remaining is nil, want present for a running stage with a known budget")
	}
	// The load-bearing identity: elapsed + remaining == agent_timeout. A drift
	// between the two clocks shows as a failing identity rather than silent skew.
	if ws.ElapsedSeconds+*ws.DeadlineSecondsRemaining != ws.AgentTimeoutSeconds {
		t.Errorf("elapsed + remaining != agent_timeout: %d + %d != %d",
			ws.ElapsedSeconds, *ws.DeadlineSecondsRemaining, ws.AgentTimeoutSeconds)
	}

	// Terminal: all three deadline fields drop (mirrors poll_interval_seconds).
	if _, err := fx.runRepo.TransitionStage(ctx, implStage.ID, runpkg.StageStateSucceeded, nil); err != nil {
		t.Fatalf("TransitionStage(implement → succeeded): %v", err)
	}
	term := implementDeadlineStatus(t, ctx, session, r.ID)
	if term == nil {
		t.Fatal("implement_stage_wait_status is nil after terminal")
	}
	if term.Status != "succeeded" {
		t.Fatalf("status = %q, want succeeded", term.Status)
	}
	if term.AgentTimeoutSeconds != 0 || term.ElapsedSeconds != 0 || term.DeadlineSecondsRemaining != nil {
		t.Errorf("terminal stage still carries deadline fields: elapsed=%d agent_timeout=%d remaining=%v",
			term.ElapsedSeconds, term.AgentTimeoutSeconds, term.DeadlineSecondsRemaining)
	}
}

// implementDeadlineWaitStatus is the decoded implement_stage_wait_status,
// projecting the #2540 deadline fields alongside status.
type implementDeadlineWaitStatus struct {
	Stage                    string `json:"stage"`
	Status                   string `json:"status"`
	ElapsedSeconds           int    `json:"elapsed_seconds"`
	AgentTimeoutSeconds      int    `json:"agent_timeout_seconds"`
	DeadlineSecondsRemaining *int   `json:"deadline_seconds_remaining"`
}

// implementDeadlineStatus calls fishhawk_get_run_status through the MCP session
// and returns the decoded implement_stage_wait_status (nil when absent).
func implementDeadlineStatus(t *testing.T, ctx context.Context, session *mcp.ClientSession, runID uuid.UUID) *implementDeadlineWaitStatus {
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
		ImplementStageWaitStatus *implementDeadlineWaitStatus `json:"implement_stage_wait_status"`
	}
	decodeStructured(t, result, &out)
	return out.ImplementStageWaitStatus
}
