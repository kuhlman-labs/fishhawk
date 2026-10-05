package mcpe2e_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
)

// delegationShadowSpec is a workflow-v2 `autonomy: low` spec (every delegable
// class gated) with two agent plan reviewers.
const delegationShadowSpec = `version: "2"
workflows:
  feature_change:
    autonomy: low
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        reviewers:
          agents:
            - provider: anthropic
            - provider: codex
        produces:
          - artifact: plan
            schema: standard_v1
        gates:
          - type: approval
            approvals:
              count: 1
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`

// TestE2E_DelegationShadow_HumanApproveStampedAndBlind is the cross-boundary
// proof for E82.1 / #3778: the real fishhawk-mcp binary's
// fishhawk_approve_plan under the HUMAN operator token → HTTP approvals
// handler → shadow capture → audit append (real Postgres) → the explicit
// category read returns exactly ONE stamp (class approve, mode gated,
// human_decision approve) sequenced AFTER approval_submitted; while the same
// binary's fishhawk_get_run_status (recent_audit) and fishhawk_get_gate_view
// outputs carry no shadow text. Per-layer fakes cannot cross this seam: the
// stamp, the Postgres ordering and the MCP-side rendering of the list reads.
func TestE2E_DelegationShadow_HumanApproveStampedAndBlind(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	auditRepo := audit.NewPostgresRepository(fx.pool)
	artifactRepo := artifact.NewPostgresRepository(fx.pool)
	srv := server.New(server.Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      fx.runRepo,
		AuditRepo:    auditRepo,
		SigningRepo:  signing.NewPostgresRepository(fx.pool),
		ArtifactRepo: artifactRepo,
		ApprovalRepo: approval.NewPostgresRepository(fx.pool),
		ConcernRepo:  concern.NewPostgresRepository(fx.pool),
		APITokenRepo: fx.apitokenRepo,
		GitHub:       githubclient.New(nil),
	})
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	httpSrv := hs.URL

	// A run carrying the cached autonomy:low spec (the fixture run carries
	// none, so the capture would degrade to unevaluable).
	requiresCharter := false
	runRow, err := fx.runRepo.CreateRun(ctx, runpkg.CreateRunParams{
		Repo:            "kuhlman-labs/fishhawk",
		WorkflowID:      "feature_change",
		WorkflowSHA:     "sha-3778-e2e",
		TriggerSource:   runpkg.TriggerCLI,
		RequiresCharter: &requiresCharter,
		WorkflowSpec:    []byte(delegationShadowSpec),
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := runRow.ID

	planStage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:            runID,
		Sequence:         1,
		Type:             runpkg.StageTypePlan,
		ExecutorKind:     runpkg.ExecutorAgent,
		ExecutorRef:      "fishhawk/runner@v1",
		RequiresApproval: true,
	})
	if err != nil {
		t.Fatalf("CreateStage plan: %v", err)
	}
	if _, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:        runID,
		Sequence:     2,
		Type:         runpkg.StageTypeImplement,
		ExecutorKind: runpkg.ExecutorAgent,
		ExecutorRef:  "fishhawk/runner@v1",
	}); err != nil {
		t.Fatalf("CreateStage implement: %v", err)
	}
	planContent, err := json.Marshal(map[string]any{
		"plan_version": "standard_v1",
		"summary":      "scoped plan",
		"verification": map[string]any{"test_strategy": "ts", "rollback_plan": "rb"},
		"scope": map[string]any{
			"files": []map[string]any{{"path": "backend/internal/server/reads.go", "operation": "modify"}},
		},
	})
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	sv := "standard_v1"
	sum := sha256.Sum256(planContent)
	if _, err := artifactRepo.Create(ctx, artifact.CreateParams{
		StageID: planStage.ID, Kind: artifact.KindPlan, SchemaVersion: &sv,
		Content: planContent, ContentHash: hex.EncodeToString(sum[:]),
	}); err != nil {
		t.Fatalf("Create plan artifact: %v", err)
	}
	// A settled clean two-reviewer round: clean_dual_approval's met shape.
	appendAudit(t, ctx, auditRepo, runID, planStage.ID, "plan_review_started", planreview.ReviewStartedPayload{ConfiguredAgents: 2})
	for i := 0; i < 2; i++ {
		appendAudit(t, ctx, auditRepo, runID, planStage.ID, "plan_reviewed", planreview.PlanReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApprove})
	}
	parkAtGate(t, ctx, fx.runRepo, planStage.ID)

	session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, httpSrv)

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "fishhawk_approve_plan",
		Arguments: map[string]any{"run_id": runID.String(), "reason": "looks right"},
	})
	if err != nil {
		t.Fatalf("CallTool fishhawk_approve_plan: %v", err)
	}
	if res.IsError {
		t.Fatalf("fishhawk_approve_plan returned a tool error: %s", toolContentString(t, res))
	}

	// Exactly one stamp, through the explicit-category read.
	stamps := listRunAuditItems(t, ctx, httpSrv, fx.operatorTok, runID.String(), "?category="+server.CategoryDelegationShadowEvaluated)
	if len(stamps) != 1 {
		t.Fatalf("delegation shadow stamps = %d, want exactly 1: %+v", len(stamps), stamps)
	}
	var st struct {
		Action, Class, Mode, Verdict string
		HumanDecision                string `json:"human_decision"`
		DecisionCategory             string `json:"decision_category"`
		ActorSubject                 string `json:"actor_subject"`
		StageID                      string `json:"stage_id"`
	}
	if err := json.Unmarshal(stamps[0].Payload, &st); err != nil {
		t.Fatalf("decode stamp: %v", err)
	}
	if st.Action != "approve" || st.Class != "approve" || st.Mode != "gated" {
		t.Errorf("stamp action/class/mode = %q/%q/%q, want approve/approve/gated", st.Action, st.Class, st.Mode)
	}
	if st.Verdict != "met" {
		t.Errorf("stamp verdict = %q, want met (clean two-approve round, no open concern)", st.Verdict)
	}
	if st.HumanDecision != "approve" || st.DecisionCategory != "approval_submitted" {
		t.Errorf("stamp decision = %q/%q, want approve/approval_submitted", st.HumanDecision, st.DecisionCategory)
	}
	if st.ActorSubject != "brett@e2e-test" || st.StageID != planStage.ID.String() {
		t.Errorf("stamp actor/stage = %q/%q, want the human operator on the plan stage", st.ActorSubject, st.StageID)
	}
	approvals := listRunAuditItems(t, ctx, httpSrv, fx.operatorTok, runID.String(), "?category=approval_submitted")
	if len(approvals) != 1 {
		t.Fatalf("approval_submitted rows = %d, want 1", len(approvals))
	}
	if stamps[0].Sequence <= approvals[0].Sequence {
		t.Errorf("stamp sequence %d must follow approval_submitted %d", stamps[0].Sequence, approvals[0].Sequence)
	}

	// Blind through the same binary: recent_audit (GET /v0/audit?run_id&limit)
	// and the gate view carry no shadow text. recent_audit's approval row is
	// the positive control that the read actually ran over the decision.
	status := callToolText(t, ctx, session, "fishhawk_get_run_status", map[string]any{"run_id": runID.String()})
	if !strings.Contains(status, "approval_submitted") {
		t.Fatalf("fishhawk_get_run_status recent_audit lacks approval_submitted; the read must cover the decision:\n%s", status)
	}
	if strings.Contains(status, "delegation_shadow") {
		t.Errorf("fishhawk_get_run_status renders the delegation shadow stamp; it must be withheld:\n%s", status)
	}
	gate := callToolText(t, ctx, session, "fishhawk_get_gate_view", map[string]any{"run_id": runID.String()})
	if strings.Contains(gate, "delegation_shadow") {
		t.Errorf("fishhawk_get_gate_view renders the delegation shadow stamp; it must be withheld:\n%s", gate)
	}
}

// appendAudit appends one chained audit row of category on the run's stage.
func appendAudit(t *testing.T, ctx context.Context, repo audit.Repository, runID, stageID uuid.UUID, category string, payload any) {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal %s: %v", category, err)
	}
	if _, err := repo.AppendChained(ctx, audit.ChainAppendParams{
		RunID: runID, StageID: &stageID, Timestamp: time.Now().UTC(), Category: category, Payload: b,
	}); err != nil {
		t.Fatalf("append %s: %v", category, err)
	}
}

// shadowAuditItem is the subset of an audit list item this test reads.
type shadowAuditItem struct {
	Sequence int64           `json:"sequence"`
	Category string          `json:"category"`
	Payload  json.RawMessage `json:"payload"`
}

// listRunAuditItems reads GET /v0/runs/{id}/audit<query> as the operator.
func listRunAuditItems(t *testing.T, ctx context.Context, baseURL, token, runID, query string) []shadowAuditItem {
	t.Helper()
	raw := getJSON(t, ctx, baseURL+"/v0/runs/"+runID+"/audit"+query, token)
	var out struct {
		Items []shadowAuditItem `json:"items"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode audit list: %v\n%s", err, raw)
	}
	return out.Items
}

// callToolText calls an MCP tool, fails on a tool error, and returns its
// content text.
func callToolText(t *testing.T, ctx context.Context, session *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool %s: %v", name, err)
	}
	text := toolContentString(t, res)
	if res.IsError {
		t.Fatalf("%s returned a tool error: %s", name, text)
	}
	return text
}
