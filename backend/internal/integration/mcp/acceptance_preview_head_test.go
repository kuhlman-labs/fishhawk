package mcpe2e_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// TestE2E_AcceptancePreview_NamesAdvancedHead is the MCP consumer half of
// #4874: the REAL fishhawk-mcp binary renders the run_preview ritual step from
// the recent audit slice, and its expected_head_sha must name the head the
// server's acceptanceHeadAsOf would, not the un-advanced seed. Every row goes
// through the real server and Postgres chain, so the wire's `sequence` field
// is what orders the seed and the walk; a wire that dropped it would leave the
// walk at the seed and this test RED.
//
// Legs: a standalone run (pull_request_opened S + a clean rebase S -> M), a
// parent (integration_commit_recorded F + a clean rebase F -> P), a late
// integration leg (fixup_pushed H then integration_commit_recorded F2 names
// F2, then a newer fixup_pushed H2 names H2), an unclean rebase that keeps the
// seed, and a window limited so only the rebase row is inside, where
// expected_head_sha is absent and the command stays concrete without a SHA.
func TestE2E_AcceptancePreview_NamesAdvancedHead(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	auditRepo := audit.NewPostgresRepository(fx.pool)
	session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, fx.url)

	const (
		seedS  = "5eed000000000000000000000000000000000001"
		mergeM = "3e7e000000000000000000000000000000000002"
		baseB  = "bba5e00000000000000000000000000000000003"
		fanF   = "fa11000000000000000000000000000000000004"
		parP   = "9a7e000000000000000000000000000000000005"
		fixH   = "f1c5000000000000000000000000000000000006"
		fanF2  = "fa12000000000000000000000000000000000007"
		fixH2  = "f1c6000000000000000000000000000000000008"
	)

	t.Run("standalone: clean rebase S -> M names M", func(t *testing.T) {
		runID := seedPreviewRun(t, ctx, fx.runRepo, auditRepo)
		appendPreviewLedger(t, ctx, auditRepo, runID, "pull_request_opened", map[string]any{"head_sha": seedS})
		appendBranchRebased(t, ctx, auditRepo, runID, seedS, mergeM, mergeM, baseB)
		assertPreviewHead(t, ctx, session, runID, 0, mergeM)
	})

	t.Run("parent: integration F + clean rebase F -> P names P", func(t *testing.T) {
		runID := seedPreviewRun(t, ctx, fx.runRepo, auditRepo)
		appendPreviewLedger(t, ctx, auditRepo, runID, "integration_commit_recorded", map[string]any{"merge_sha": fanF})
		appendBranchRebased(t, ctx, auditRepo, runID, fanF, parP, parP, baseB)
		assertPreviewHead(t, ctx, session, runID, 0, parP)
	})

	t.Run("late integration: F2 beats older H, newer H2 beats F2", func(t *testing.T) {
		runID := seedPreviewRun(t, ctx, fx.runRepo, auditRepo)
		appendPreviewLedger(t, ctx, auditRepo, runID, "fixup_pushed", map[string]any{"head_sha": fixH})
		appendPreviewLedger(t, ctx, auditRepo, runID, "integration_commit_recorded", map[string]any{"merge_sha": fanF2})
		assertPreviewHead(t, ctx, session, runID, 0, fanF2)
		appendPreviewLedger(t, ctx, auditRepo, runID, "fixup_pushed", map[string]any{"head_sha": fixH2})
		assertPreviewHead(t, ctx, session, runID, 0, fixH2)
	})

	t.Run("unclean rebase keeps the seed", func(t *testing.T) {
		runID := seedPreviewRun(t, ctx, fx.runRepo, auditRepo)
		appendPreviewLedger(t, ctx, auditRepo, runID, "pull_request_opened", map[string]any{"head_sha": seedS})
		appendPreviewLedger(t, ctx, auditRepo, runID, "branch_rebased", map[string]any{
			"prior_head_sha": seedS, "merge_commit_sha": mergeM, "new_head_sha": mergeM,
			"already_up_to_date": false, "clean_verified": false, "base_tip_sha": baseB,
		})
		assertPreviewHead(t, ctx, session, runID, 0, seedS)
	})

	t.Run("seed outside the window: SHA absent, command concrete", func(t *testing.T) {
		runID := seedPreviewRun(t, ctx, fx.runRepo, auditRepo)
		appendPreviewLedger(t, ctx, auditRepo, runID, "pull_request_opened", map[string]any{"head_sha": seedS})
		appendBranchRebased(t, ctx, auditRepo, runID, seedS, mergeM, mergeM, baseB)
		assertPreviewHead(t, ctx, session, runID, 1, "")
	})
}

// seedPreviewRun creates a LOCAL run whose next_actions offers the acceptance
// dispatch: plan and implement succeeded, acceptance pending, review parked
// behind it, with one implement review verdict on the chain. Every ledger row
// a leg appends lands after these, so it is inside the default window.
func seedPreviewRun(t *testing.T, ctx context.Context, repo runpkg.Repository, auditRepo audit.Repository) uuid.UUID {
	t.Helper()
	run, err := repo.CreateRun(ctx, runpkg.CreateRunParams{
		Repo: "kuhlman-labs/fishhawk", WorkflowID: "feature_change", WorkflowSHA: "deadbeef",
		TriggerSource: runpkg.TriggerCLI, RunnerKind: runpkg.RunnerKindLocal,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := repo.TransitionRun(ctx, run.ID, runpkg.StateRunning); err != nil {
		t.Fatalf("TransitionRun → running: %v", err)
	}
	mk := func(seq int, typ runpkg.StageType, approval bool) *runpkg.Stage {
		t.Helper()
		st, err := repo.CreateStage(ctx, runpkg.CreateStageParams{
			RunID: run.ID, Sequence: seq, Type: typ,
			ExecutorKind: runpkg.ExecutorAgent, ExecutorRef: "fishhawk/runner@v1",
			RequiresApproval: approval,
		})
		if err != nil {
			t.Fatalf("CreateStage(%s): %v", typ, err)
		}
		return st
	}
	plan := mk(0, runpkg.StageTypePlan, false)
	driveStageToSucceeded(t, ctx, repo, plan.ID)
	impl := mk(1, runpkg.StageTypeImplement, false)
	driveStageToSucceeded(t, ctx, repo, impl.ID)
	mk(2, runpkg.StageTypeAcceptance, false)
	mk(3, runpkg.StageTypeReview, true)
	seedImplementReview(t, ctx, auditRepo, run.ID, impl.ID,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "guard the nil stage"})
	return run.ID
}

// appendPreviewLedger appends one chained system row on the run.
func appendPreviewLedger(t *testing.T, ctx context.Context, repo audit.Repository, runID uuid.UUID, category string, payload map[string]any) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal %s: %v", category, err)
	}
	kind := audit.ActorSystem
	if _, err := repo.AppendChained(ctx, audit.ChainAppendParams{
		RunID: runID, Timestamp: time.Now().UTC(), Category: category, ActorKind: &kind, Payload: raw,
	}); err != nil {
		t.Fatalf("AppendChained %s: %v", category, err)
	}
}

// assertPreviewHead reads fishhawk_get_run_status through the real binary
// (auditLimit 0 keeps the default window) and asserts the run_preview step
// sits immediately before the acceptance dispatch, with expected_head_sha ==
// want and a command ending in want; want "" asserts the SHA is absent and
// the command is still concrete.
func assertPreviewHead(t *testing.T, ctx context.Context, session *mcp.ClientSession, runID uuid.UUID, auditLimit int, want string) {
	t.Helper()
	args := map[string]any{"run_id": runID.String()}
	if auditLimit > 0 {
		args["audit_limit"] = auditLimit
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "fishhawk_get_run_status", Arguments: args})
	if err != nil {
		t.Fatalf("CallTool fishhawk_get_run_status: %v", err)
	}
	if result.IsError {
		t.Fatalf("get_run_status tool returned error: %s", toolContentString(t, result))
	}
	var out struct {
		NextActions *nextActionsView `json:"next_actions"`
	}
	decodeStructured(t, result, &out)
	na := out.NextActions
	if na == nil {
		t.Fatal("next_actions absent")
	}
	idx := -1
	for i, a := range na.Actions {
		if a.Action == "run_preview" {
			idx = i
			break
		}
	}
	if idx < 0 || idx+1 >= len(na.Actions) {
		t.Fatalf("no run_preview ahead of an action (actions %+v)", na.Actions)
	}
	if next := na.Actions[idx+1]; next.Params["stage"] != "acceptance" {
		t.Fatalf("run_preview precedes %s %v, want the acceptance dispatch", next.Action, next.Params)
	}
	params := na.Actions[idx].Params
	cmd := params["command"]
	if strings.TrimSpace(cmd) == "" {
		t.Fatalf("run_preview command is empty, want a concrete command (params %v)", params)
	}
	if want == "" {
		if got, ok := params["expected_head_sha"]; ok {
			t.Errorf("expected_head_sha = %q, want it absent", got)
		}
		return
	}
	if got := params["expected_head_sha"]; got != want {
		t.Errorf("run_preview expected_head_sha = %q, want %q (command %q)", got, want, cmd)
	}
	if !strings.HasSuffix(cmd, want) {
		t.Errorf("run_preview command = %q, want it to end in %s", cmd, want)
	}
}
