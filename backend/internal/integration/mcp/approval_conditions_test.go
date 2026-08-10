package mcpe2e_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
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
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
)

// TestE2E_ApprovalConditions_OverCapRefused_AtCapReachesPrompt is the
// cross-boundary proof for #2583: it drives the real MCP fishhawk_approve_plan →
// HTTP approvals handler → approval row → implement-prompt build seam that the
// per-layer unit tests cannot cross (cf. #618). It asserts (a) an over-cap
// approve reason is refused end to end with NO approval recorded, and (b) an
// exactly-at-cap reason whose final line is a distinctive marker reaches the
// implement prompt with that marker intact and no "...[truncated]" suffix.
func TestE2E_ApprovalConditions_OverCapRefused_AtCapReachesPrompt(t *testing.T) {
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Second backend over the SAME pool with ArtifactRepo + ApprovalRepo +
	// GitHub wired so the implement prompt can load the approved plan and the
	// approve tool can record a row.
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

	// 1. Plan stage parked at the approval gate carrying an approved
	// standard_v1 plan (the implement prompt needs a loadable approved plan).
	planStage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:            fx.runID,
		Sequence:         1,
		Type:             runpkg.StageTypePlan,
		ExecutorKind:     runpkg.ExecutorAgent,
		ExecutorRef:      "fishhawk/runner@v1",
		RequiresApproval: true,
	})
	if err != nil {
		t.Fatalf("CreateStage plan: %v", err)
	}
	planContent, err := json.Marshal(map[string]any{
		"plan_version": "standard_v1",
		"summary":      "scoped plan",
		"verification": map[string]any{"test_strategy": "ts", "rollback_plan": "rb"},
		"scope": map[string]any{
			"files": []map[string]any{
				{"path": "backend/internal/server/prompt.go", "operation": "modify"},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	sv := "standard_v1"
	sum := sha256.Sum256(planContent)
	if _, err := artifactRepo.Create(ctx, artifact.CreateParams{
		StageID:       planStage.ID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &sv,
		Content:       planContent,
		ContentHash:   hex.EncodeToString(sum[:]),
	}); err != nil {
		t.Fatalf("Create plan artifact: %v", err)
	}
	parkAtGate(t, ctx, fx.runRepo, planStage.ID)

	// 2. Implement stage left pending (a runnable state for prompt-render).
	implStage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:        fx.runID,
		Sequence:     2,
		Type:         runpkg.StageTypeImplement,
		ExecutorKind: runpkg.ExecutorAgent,
		ExecutorRef:  "fishhawk/runner@v1",
	})
	if err != nil {
		t.Fatalf("CreateStage implement: %v", err)
	}

	session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, httpSrv.URL)

	// 3. (a) Over-cap reason (1 byte over the cap) is refused end to end, and
	// NO approval is recorded. The reason is built BY CONSTRUCTION (Repeat to
	// cap+1), not via the validator, so the failure lands on the tool-error /
	// no-row assertions.
	overCap := strings.Repeat("a", prompt.MaxApprovalConditionBytes+1)
	overResult, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "fishhawk_approve_plan",
		Arguments: map[string]any{
			"run_id": fx.runID.String(),
			"reason": overCap,
		},
	})
	if err != nil {
		t.Fatalf("CallTool fishhawk_approve_plan (over-cap): %v", err)
	}
	if !overResult.IsError {
		t.Fatalf("over-cap approve should surface a tool error; got success")
	}
	errText := toolContentString(t, overResult)
	if !strings.Contains(errText, "validation_failed") {
		t.Errorf("over-cap refusal must name validation_failed; got:\n%s", errText)
	}

	// COMMITTED-STATE: no approval row exists for the plan stage.
	rows, err := approvalRepo.ListForStage(ctx, planStage.ID)
	if err != nil {
		t.Fatalf("ListForStage after over-cap: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("approval rows after over-cap approve = %d, want 0", len(rows))
	}

	// 4. (b) At-cap reason whose FINAL line is a distinctive marker is accepted
	// and reaches the implement prompt verbatim with no truncation marker.
	const marker = "ZZZ_FINAL_CONDITION_LINE_ZZZ"
	prefix := strings.Repeat("a", prompt.MaxApprovalConditionBytes-len(marker)-1)
	reason := prefix + "\n" + marker
	if len(reason) != prompt.MaxApprovalConditionBytes {
		t.Fatalf("at-cap reason is %d bytes, want exactly %d", len(reason), prompt.MaxApprovalConditionBytes)
	}
	atResult, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "fishhawk_approve_plan",
		Arguments: map[string]any{
			"run_id": fx.runID.String(),
			"reason": reason,
		},
	})
	if err != nil {
		t.Fatalf("CallTool fishhawk_approve_plan (at-cap): %v", err)
	}
	if atResult.IsError {
		t.Fatalf("at-cap approve returned tool error: %s", toolContentString(t, atResult))
	}

	// The at-cap approve recorded exactly one row.
	rows, err = approvalRepo.ListForStage(ctx, planStage.ID)
	if err != nil {
		t.Fatalf("ListForStage after at-cap: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("approval rows after at-cap approve = %d, want 1", len(rows))
	}

	// 5. The built implement prompt carries the trailing marker verbatim with
	// no "...[truncated]" suffix — the request-validation → approval-row →
	// prompt-render seam.
	promptText := getPromptRenderText(t, ctx, httpSrv.URL, implStage.ID)
	if strings.Contains(promptText, "...[truncated]") {
		t.Errorf("at-cap conditions must render untruncated in the implement prompt")
	}
	if !strings.Contains(promptText, marker) {
		t.Errorf("implement prompt missing the at-cap trailing condition marker %q", marker)
	}
}

// getPromptRenderText fetches GET /v0/stages/{id}/prompt-render and returns the
// full built prompt text.
func getPromptRenderText(t *testing.T, ctx context.Context, baseURL string, stageID uuid.UUID) string {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		baseURL+"/v0/stages/"+stageID.String()+"/prompt-render", nil)
	if err != nil {
		t.Fatalf("build prompt-render request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("prompt-render request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("prompt-render status %d: %s", resp.StatusCode, raw)
	}
	var out struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode prompt-render response: %v", err)
	}
	return out.Prompt
}
