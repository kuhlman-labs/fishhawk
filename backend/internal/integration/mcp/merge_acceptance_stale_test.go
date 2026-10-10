package mcpe2e_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
)

// TestE2E_MergeRun_AcceptanceStale is the cross-boundary half of #4086's D2.
// One stale acceptance verdict crosses four layers the per-layer units cannot
// exercise together:
//
//  1. run/stage persistence — an acceptance stage driven to succeeded and then
//     re-opened to pending by run.ReopenAcceptanceStage, the real write path.
//  2. the audit chain — a CHAINED `passed` acceptance_outcome_recorded entry
//     for head verifiedHead, then a CHAINED acceptance_reopened entry scoped
//     to the stage carrying currentHead (the shape the fix-up push writes).
//  3. the server's POST /v0/runs/{id}/merge — the acceptance gate still reads
//     the stale `passed` outcome and admits; the #4086 guard refuses 409
//     acceptance_stale BEFORE the merge-seam check, so this fixture (which
//     wires no GateMerger) reaches it rather than 503 merge_seam_unconfigured.
//  4. the real fishhawk-mcp binary over stdio — fishhawk_merge_run maps the 409
//     to an immediate status=acceptance_stale instead of a tool error or a
//     timeout.
//
// Counterfactual: deleting the acceptanceStaleRefusal call in handleMergeRun
// lets the request reach merge_seam_unconfigured (503), which the tool
// surfaces as an error → RED.
func TestE2E_MergeRun_AcceptanceStale(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	auditRepo := audit.NewPostgresRepository(fx.pool)

	const (
		prURL        = "https://github.com/kuhlman-labs/fishhawk/pull/4086"
		verifiedHead = "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
		currentHead  = "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
	)

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
	impl, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID: run.ID, Sequence: 1, Type: runpkg.StageTypeImplement,
		ExecutorKind: runpkg.ExecutorAgent, ExecutorRef: "fishhawk/runner@v1",
	})
	if err != nil {
		t.Fatalf("CreateStage(implement): %v", err)
	}
	driveStageToSucceeded(t, ctx, fx.runRepo, impl.ID)
	acc, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID: run.ID, Sequence: 2, Type: runpkg.StageTypeAcceptance,
		ExecutorKind: runpkg.ExecutorAgent, ExecutorRef: "fishhawk/runner@v1",
	})
	if err != nil {
		t.Fatalf("CreateStage(acceptance): %v", err)
	}
	driveStageToSucceeded(t, ctx, fx.runRepo, acc.ID)

	kind := audit.ActorKind("system")
	appendEntry := func(category string, payload map[string]any) {
		t.Helper()
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal %s payload: %v", category, err)
		}
		if _, err := auditRepo.AppendChained(ctx, audit.ChainAppendParams{
			RunID: run.ID, StageID: &acc.ID, Timestamp: time.Now(),
			Category: category, ActorKind: &kind, Payload: raw,
		}); err != nil {
			t.Fatalf("AppendChained %s: %v", category, err)
		}
	}
	appendEntry(server.CategoryAcceptanceOutcomeRecorded, map[string]any{"verdict": "passed", "head_sha": verifiedHead})

	// THE RE-OPEN — the real write path, then the stage-scoped marker.
	if _, err := runpkg.ReopenAcceptanceStage(ctx, fx.runRepo, acc.ID); err != nil {
		t.Fatalf("ReopenAcceptanceStage: %v", err)
	}
	appendEntry(server.CategoryAcceptanceReopened, map[string]any{"prior_state": "succeeded", "head_sha": currentHead})

	if _, err := fx.runRepo.SetRunPullRequestURL(ctx, run.ID, prURL); err != nil {
		t.Fatalf("SetRunPullRequestURL: %v", err)
	}

	session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, fx.url)
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "fishhawk_merge_run",
		Arguments: map[string]any{
			"run_id":          run.ID.String(),
			"verdict":         "ship it",
			"timeout_seconds": 30,
		},
	})
	if err != nil {
		t.Fatalf("CallTool fishhawk_merge_run: %v", err)
	}
	if result.IsError {
		t.Fatalf("fishhawk_merge_run returned a tool error, want status=acceptance_stale: %s", toolContentString(t, result))
	}
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var out struct {
		Status          string  `json:"status"`
		Message         string  `json:"message"`
		VerdictRecorded bool    `json:"verdict_recorded"`
		MergeQueued     bool    `json:"merge_queued"`
		WaitedSeconds   float64 `json:"waited_seconds"`
		NextAction      *struct {
			Action string            `json:"action"`
			Params map[string]string `json:"params"`
		} `json:"next_action"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode merge_run output: %v\nraw: %s", err, raw)
	}
	if out.Status != "acceptance_stale" {
		t.Fatalf("status = %q, want acceptance_stale\nraw: %s", out.Status, raw)
	}
	for _, want := range []string{verifiedHead, currentHead} {
		if !strings.Contains(out.Message, want) {
			t.Errorf("message %q does not name head %s", out.Message, want)
		}
	}
	if out.VerdictRecorded || out.MergeQueued {
		t.Errorf("verdict_recorded=%v merge_queued=%v, want false/false", out.VerdictRecorded, out.MergeQueued)
	}
	if out.NextAction == nil || out.NextAction.Action != "fishhawk_dispatch_stage" || out.NextAction.Params["stage_id"] != acc.ID.String() {
		t.Errorf("next_action = %+v, want fishhawk_dispatch_stage on stage %s", out.NextAction, acc.ID)
	}

	// Committed state: the refusal recorded NO merge verdict.
	rows, err := auditRepo.ListForRunByCategory(ctx, run.ID, server.CategoryMergeVerdictRecorded)
	if err != nil {
		t.Fatalf("ListForRunByCategory merge_verdict_recorded: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("merge_verdict_recorded rows = %d, want 0 (refused before the append)", len(rows))
	}
}
