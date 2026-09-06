package mcpe2e_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
)

// TestE2E_NextActions_AcceptanceReopenedBlocksMerge is the cross-boundary half
// of E64.63 / #3222. Its scope spans four layers the per-layer units cannot
// exercise together:
//
//  1. run/stage persistence — an acceptance stage driven to succeeded and then
//     re-opened to pending by run.ReopenAcceptanceStage, the real write path.
//  2. the audit chain — a CHAINED acceptance_reopened entry scoped to that
//     stage's id, written through audit.AppendChained exactly as the server's
//     fix-up push handler writes it.
//  3. the server's stage + audit HTTP surface — what the MCP tool actually
//     decodes, including the stage_id field whose json tag the correlation
//     depends on (the #371-class wire-mirror trap: a silently-nil StageID would
//     degrade the wording to generic and a pure-unit test would never see it).
//  4. the MCP tool's render — the real fishhawk-mcp binary over stdio.
//
// The defect it pins: a run whose acceptance stage was re-opened by a fix-up
// push sits succeeded with its PR open, so next_actions reported
// succeeded_pr_open and offered the merge — while the merge could not fire,
// because fishhawk_audit_complete was pending on that very stage. The operator,
// with nothing on this surface naming the cause, reached for an admin bypass
// twice.
//
// NO FORGE CLIENT is involved on this path: the run's PR url is set through the
// repository, and fishhawk_get_run_status performs no forge round-trip. That is
// what makes this criterion sandbox-decidable rather than a skip.
//
// Deliberately NOT attempted here: driving fishhawk_merge_run itself. newFixture
// wires no GateMerger, so the merge POST would 503 merge_seam_unconfigured
// before any advisory could render; the merge surface's own branches are pinned
// by mcpserver/merge_run_test.go against its httptest backend.
func TestE2E_NextActions_AcceptanceReopenedBlocksMerge(t *testing.T) {
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	auditRepo := audit.NewPostgresRepository(fx.pool)

	const prURL = "https://github.com/kuhlman-labs/fishhawk/pull/3222"

	run, err := fx.runRepo.CreateRun(ctx, runpkg.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef",
		TriggerSource: runpkg.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := fx.runRepo.TransitionRun(ctx, run.ID, runpkg.StateRunning); err != nil {
		t.Fatalf("TransitionRun → running: %v", err)
	}

	// An implement stage that succeeded, then an acceptance stage driven to
	// succeeded — the shape a fix-up push re-opens.
	impl, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:        run.ID,
		Sequence:     1,
		Type:         runpkg.StageTypeImplement,
		ExecutorKind: runpkg.ExecutorAgent,
		ExecutorRef:  "fishhawk/runner@v1",
	})
	if err != nil {
		t.Fatalf("CreateStage(implement): %v", err)
	}
	driveStageToSucceeded(t, ctx, fx.runRepo, impl.ID)

	acc, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:        run.ID,
		Sequence:     2,
		Type:         runpkg.StageTypeAcceptance,
		ExecutorKind: runpkg.ExecutorAgent,
		ExecutorRef:  "fishhawk/runner@v1",
	})
	if err != nil {
		t.Fatalf("CreateStage(acceptance): %v", err)
	}
	driveStageToSucceeded(t, ctx, fx.runRepo, acc.ID)

	kind := audit.ActorKind("system")
	outcomePayload, err := json.Marshal(map[string]any{"verdict": "passed", "stage_id": acc.ID.String()})
	if err != nil {
		t.Fatalf("marshal acceptance_outcome_recorded payload: %v", err)
	}
	if _, err := auditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     run.ID,
		StageID:   &acc.ID,
		Timestamp: time.Now(),
		Category:  "acceptance_outcome_recorded",
		ActorKind: &kind,
		Payload:   outcomePayload,
	}); err != nil {
		t.Fatalf("AppendChained acceptance_outcome_recorded: %v", err)
	}

	// THE RE-OPEN — the real write path, not a hand-set state.
	if _, err := runpkg.ReopenAcceptanceStage(ctx, fx.runRepo, acc.ID); err != nil {
		t.Fatalf("ReopenAcceptanceStage: %v", err)
	}
	reopenPayload, err := json.Marshal(map[string]any{"prior_state": "succeeded", "head_sha": "f1xuphead"})
	if err != nil {
		t.Fatalf("marshal acceptance_reopened payload: %v", err)
	}
	if _, err := auditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     run.ID,
		StageID:   &acc.ID, // SCOPED to this stage — the correlation the render depends on
		Timestamp: time.Now(),
		Category:  server.CategoryAcceptanceReopened,
		ActorKind: &kind,
		Payload:   reopenPayload,
	}); err != nil {
		t.Fatalf("AppendChained acceptance_reopened: %v", err)
	}

	// The run itself is succeeded with its PR open — the shape that made the
	// classifier offer a merge that could not fire.
	if _, err := fx.runRepo.SetRunPullRequestURL(ctx, run.ID, prURL); err != nil {
		t.Fatalf("SetRunPullRequestURL: %v", err)
	}
	if _, err := fx.runRepo.TransitionRun(ctx, run.ID, runpkg.StateSucceeded); err != nil {
		t.Fatalf("TransitionRun → succeeded: %v", err)
	}

	session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, fx.url)
	na := getNextActions(t, ctx, session, run.ID)
	if na == nil {
		t.Fatal("next_actions absent on the succeeded run")
	}
	if na.State != "succeeded_acceptance_reopened" {
		t.Fatalf("next_actions.state = %q, want succeeded_acceptance_reopened — the merge is blocked and the block is unnamed", na.State)
	}

	var dispatch *struct {
		Action       string            `json:"action"`
		Params       map[string]string `json:"params"`
		Precondition string            `json:"precondition"`
		Consumes     string            `json:"consumes"`
		Reason       string            `json:"reason"`
	}
	var sawMerge bool
	for i := range na.Actions {
		switch na.Actions[i].Action {
		case "fishhawk_dispatch_stage":
			dispatch = &na.Actions[i]
		case "fishhawk_merge_run":
			sawMerge = true
		}
	}
	if dispatch == nil {
		t.Fatalf("next_actions.actions = %+v, want fishhawk_dispatch_stage offered", na.Actions)
	}
	if dispatch.Params["stage"] != "acceptance" {
		t.Errorf("dispatch stage param = %q, want acceptance", dispatch.Params["stage"])
	}
	if dispatch.Params["stage_id"] != acc.ID.String() {
		t.Errorf("dispatch stage_id = %q, want the re-opened acceptance stage %s", dispatch.Params["stage_id"], acc.ID)
	}
	if dispatch.Params["run_id"] != run.ID.String() {
		t.Errorf("dispatch run_id = %q, want %s", dispatch.Params["run_id"], run.ID)
	}
	// The advisory NAMES the stage and reports the RE-OPENED cause — the
	// sharpened wording, which is only reachable when the stage_id survived
	// layers 2 and 3 intact.
	for _, want := range []string{
		acc.ID.String()[:8],
		"re-opened by a fix-up push",
		"the queued merge cannot fire",
	} {
		if !strings.Contains(dispatch.Reason, want) {
			t.Errorf("dispatch reason = %q, want it to contain %q", dispatch.Reason, want)
		}
	}
	// ADDITIVE: the merge stays offered — the server remains the authority on
	// whether it fires.
	if !sawMerge {
		t.Errorf("next_actions.actions = %+v, want the merge ritual retained", na.Actions)
	}
}

// driveStageToSucceeded walks a stage through the legal transition table to
// succeeded (the table refuses a pending → succeeded jump).
func driveStageToSucceeded(t *testing.T, ctx context.Context, repo runpkg.Repository, stageID uuid.UUID) {
	t.Helper()
	for _, to := range []runpkg.StageState{
		runpkg.StageStateDispatched,
		runpkg.StageStateRunning,
		runpkg.StageStateSucceeded,
	} {
		if _, err := repo.TransitionStage(ctx, stageID, to, nil); err != nil {
			t.Fatalf("TransitionStage → %s: %v", to, err)
		}
	}
}
