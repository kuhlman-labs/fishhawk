package mcpe2e_test

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
	"github.com/kuhlman-labs/fishhawk/backend/internal/stagecheck"
)

// TestE2E_CIFailed_ObserverToDerivedStatusToNextActions drives the
// product-detected CI-failure seam (#1045) end-to-end across the four
// layers the per-layer units cannot exercise together: the drive
// observer's audit write → GET /v0/runs/{id} derived_status read model →
// the MCP next_actions classifier.
//
// A drive-enabled run is parked at its review gate with a red required
// StageCheck. ObserveParkedReviewForDrive (the mergereconciler-invoked
// observer, called directly here as server_test does) stamps the
// ci_failed run_auto_advanced entry. GET /v0/runs/{id} then surfaces
// derived_status "ci_failed", and the MCP get_run_status next_actions
// block classifies ci_failed_unroutable (no open concerns) naming the
// commit_and_vouch operator-remediation arm (#1044). This is the
// layer-crossing test #618 mandates: a per-layer unit passes while the
// seam (a derived_status literal not matching the classifier switch)
// breaks.
func TestE2E_CIFailed_ObserverToDerivedStatusToNextActions(t *testing.T) {
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Stand up a backend over the SAME pool with the stage-check repo
	// wired (newFixture's server omits it) so the observer can read the
	// red required check. The operator fhk_* token authenticates against
	// the same apitoken rows.
	auditRepo := audit.NewPostgresRepository(fx.pool)
	stageCheckRepo := stagecheck.NewPostgresRepository(fx.pool)
	srv := server.New(server.Config{
		Addr:           "127.0.0.1:0",
		RunRepo:        fx.runRepo,
		AuditRepo:      auditRepo,
		SigningRepo:    signing.NewPostgresRepository(fx.pool),
		APITokenRepo:   fx.apitokenRepo,
		StageCheckRepo: stageCheckRepo,
	})

	const requiredCheck = "ci/required"
	const prURL = "https://github.com/kuhlman-labs/fishhawk/pull/4545"

	// A drive-enabled run with a required-checks snapshot and an open PR.
	// No WorkflowSpec → zero implement reviewers configured, so the review
	// round is vacuously terminal and the observer reaches the checks gate.
	run, err := fx.runRepo.CreateRun(ctx, runpkg.CreateRunParams{
		Repo:                   "kuhlman-labs/fishhawk",
		WorkflowID:             "feature_change",
		WorkflowSHA:            "deadbeef",
		TriggerSource:          runpkg.TriggerCLI,
		Drive:                  true,
		RequiredChecksSnapshot: &runpkg.RequiredChecksSnapshot{Contexts: []string{requiredCheck}},
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := fx.runRepo.SetRunPullRequestURL(ctx, run.ID, prURL); err != nil {
		t.Fatalf("SetRunPullRequestURL: %v", err)
	}
	if _, err := fx.runRepo.TransitionRun(ctx, run.ID, runpkg.StateRunning); err != nil {
		t.Fatalf("TransitionRun → running: %v", err)
	}

	// A review stage parked at its approval gate (the observer's gate point).
	stage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:            run.ID,
		Sequence:         1,
		Type:             runpkg.StageTypeReview,
		ExecutorKind:     runpkg.ExecutorAgent,
		ExecutorRef:      "fishhawk/runner@v1",
		RequiresApproval: true,
	})
	if err != nil {
		t.Fatalf("CreateStage(review): %v", err)
	}
	parkAtGate(t, ctx, fx.runRepo, stage.ID)

	// The required check concluded red on the review stage.
	failure := "failure"
	if _, err := stageCheckRepo.Append(ctx, stagecheck.AppendParams{
		StageID:    stage.ID,
		Name:       requiredCheck,
		Status:     "completed",
		Conclusion: &failure,
		HeadSHA:    "cafebabe",
		Timestamp:  time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Append red stage check: %v", err)
	}

	// Drive the observer one tick — it stamps the ci_failed entry.
	parked, err := fx.runRepo.GetStage(ctx, stage.ID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	srv.ObserveParkedReviewForDrive(ctx, parked, prURL)

	// Layer 2 + 3: the MCP get_run_status surfaces derived_status ci_failed.
	session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, mountServer(t, srv))
	if got := getDerivedStatus(t, ctx, session, run.ID); got != "ci_failed" {
		t.Fatalf("drive_status.derived_status = %q, want ci_failed", got)
	}

	// Layer 4: the next_actions classifier names the legal remediation arm.
	na := getNextActions(t, ctx, session, run.ID)
	if na == nil {
		t.Fatal("next_actions absent on the ci_failed run")
	}
	if na.State != "ci_failed_unroutable" {
		t.Fatalf("next_actions.state = %q, want ci_failed_unroutable", na.State)
	}
	// The drive distilled next_action (classify_ci_failure) folds in first,
	// then the classifier's commit_and_vouch operator-remediation arm
	// (#1044) — the legal move with no open concerns to route back.
	if len(na.Actions) == 0 || na.Actions[0].Action != "classify_ci_failure" {
		t.Fatalf("next_actions.actions[0] = %+v, want the drive classify_ci_failure folded first", na.Actions)
	}
	var sawCommitVouch bool
	for _, a := range na.Actions {
		if a.Action == "commit_and_vouch" {
			sawCommitVouch = true
		}
	}
	if !sawCommitVouch {
		t.Fatalf("next_actions.actions = %+v, want commit_and_vouch present (#1044 operator remediation)", na.Actions)
	}
}

// mountServer mounts the server on a throwaway httptest server and
// returns its URL, registering teardown.
func mountServer(t *testing.T, srv *server.Server) string {
	t.Helper()
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)
	return httpSrv.URL
}

// getDerivedStatus calls fishhawk_get_run_status and returns the decoded
// drive_status.derived_status ("" when absent).
func getDerivedStatus(t *testing.T, ctx context.Context, session *mcp.ClientSession, runID uuid.UUID) string {
	t.Helper()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "fishhawk_get_run_status",
		Arguments: map[string]any{"run_id": runID.String()},
	})
	if err != nil {
		t.Fatalf("CallTool fishhawk_get_run_status: %v", err)
	}
	if result.IsError {
		t.Fatalf("get_run_status tool returned error: %s", toolContentString(t, result))
	}
	var out struct {
		DriveStatus *struct {
			DerivedStatus string `json:"derived_status"`
		} `json:"drive_status"`
	}
	decodeStructured(t, result, &out)
	if out.DriveStatus == nil {
		return ""
	}
	return out.DriveStatus.DerivedStatus
}
