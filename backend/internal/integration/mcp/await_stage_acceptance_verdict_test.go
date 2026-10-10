package mcpe2e_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// TestE2E_AwaitStage_AcceptanceHoldsForVerdict drives the E72.56 / #4072
// acceptance verdict hold across the real chain the mcpserver unit tests fake:
// the fishhawk-mcp binary over stdio -> the real fishhawkd handlers -> Postgres.
// The acceptance stage is driven to succeeded through runRepo.TransitionStage
// (which stamps ended_at), and its stage-scoped acceptance_dispatched anchor is
// chained via AppendChained. The verdict is appended only AFTER the first
// GET /v0/audit read has been answered (gated on the proxy's firstDone), so the
// wait MUST hold past the settle to report it. This is what proves the hold
// against real /v0/audit ordering, stage_id serialization and the ended_at
// decode off the stage-wait envelope — the #371 / #875 seams the unit layer
// cannot see.
//
// Binding condition C4 (#3881): t.Parallel() like its package siblings, with no
// t.Setenv / t.Chdir; every bound that competes with a deadline is derived via
// timescale.D.
func TestE2E_AwaitStage_AcceptanceHoldsForVerdict(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), timescale.D(90*time.Second))
	defer cancel()

	auditRepo := audit.NewPostgresRepository(fx.pool)

	stage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:        fx.runID,
		Sequence:     1,
		Type:         runpkg.StageTypeAcceptance,
		ExecutorKind: runpkg.ExecutorAgent,
		ExecutorRef:  "fishhawk/runner@v1",
	})
	if err != nil {
		t.Fatalf("CreateStage(acceptance): %v", err)
	}
	kind := audit.ActorKind("system")
	if _, err := auditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     fx.runID,
		StageID:   &stage.ID,
		Timestamp: time.Now(),
		Category:  server.CategoryAcceptanceDispatched,
		ActorKind: &kind,
		Payload:   []byte(`{}`),
	}); err != nil {
		t.Fatalf("AppendChained acceptance_dispatched: %v", err)
	}
	// The trace upload settles the stage succeeded — and stamps ended_at —
	// BEFORE the runner ships the verdict.
	driveStageToSucceeded(t, ctx, fx.runRepo, stage.ID)

	proxy, probe := newStageReadProxy(t, fx.url, "/v0/audit")
	token := fetchMCPToken(t, ctx, fx.url, fx.runID, fx.signingPriv)
	session := connectMCPClient(t, ctx, fx.mcpBinary, token, proxy.URL)

	// The verdict ships only after the hold's first audit probe was answered.
	go func() {
		select {
		case <-probe.firstDone:
		case <-ctx.Done():
			return
		}
		payload, merr := json.Marshal(map[string]any{"verdict": "passed", "stage_id": stage.ID.String()})
		if merr != nil {
			t.Errorf("marshal acceptance_outcome_recorded payload: %v", merr)
			return
		}
		if _, aerr := auditRepo.AppendChained(context.Background(), audit.ChainAppendParams{
			RunID:     fx.runID,
			StageID:   &stage.ID,
			Timestamp: time.Now(),
			Category:  "acceptance_outcome_recorded",
			ActorKind: &kind,
			Payload:   payload,
		}); aerr != nil {
			t.Errorf("AppendChained acceptance_outcome_recorded: %v", aerr)
		}
	}()

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "fishhawk_await_stage",
		Arguments: map[string]any{
			"run_id":          fx.runID.String(),
			"stage":           "acceptance",
			"timeout_seconds": int(timescale.D(60 * time.Second).Seconds()),
		},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool returned error: %+v", result.Content)
	}
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var out struct {
		Status            string `json:"status"`
		State             string `json:"state"`
		AcceptanceVerdict string `json:"acceptance_verdict"`
		VerdictPending    bool   `json:"verdict_pending"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode await_stage output: %v\nraw: %s", err, raw)
	}
	if out.Status != "settled" || out.State != "succeeded" {
		t.Fatalf("status/state = %q/%q, want settled/succeeded\nraw: %s", out.Status, out.State, raw)
	}
	if out.AcceptanceVerdict != "passed" {
		t.Errorf("acceptance_verdict = %q, want passed (the wait must hold until the verdict lands)\nraw: %s", out.AcceptanceVerdict, raw)
	}
	if out.VerdictPending {
		t.Errorf("verdict_pending = true, want false once the verdict landed\nraw: %s", raw)
	}
	if reads, _ := probe.snapshot(); reads < 2 {
		t.Errorf("audit probe reads reaching the backend = %d, want >= 2 (the hold re-probed after the settle)", reads)
	}
}
