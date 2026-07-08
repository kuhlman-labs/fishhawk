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

// scopeEditWorkflowSpec is a minimal feature_change spec whose implement stage
// carries a max_files_changed cap, so resolveImplementConstraints resolves and
// the effective-scope path set (and its before/after audit lists) is computed.
var scopeEditWorkflowSpec = []byte(`version: "0.3"
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
        constraints:
          - max_files_changed: 5
`)

// TestE2E_ScopeEdit_RemoveAndReplaceAtPlanGate is the #1726 cross-component
// done-means test. It drives the seam the per-layer unit tests cannot cover
// together: fishhawk_approve_plan carrying remove_scope_files (remove flow) and
// remove_scope_files+add_scope_files (replace flow) through the REAL fishhawk-mcp
// binary → the backend approval handler → the approval_submitted audit entry in
// Postgres → the derived implement prompt-response ScopeFiles (the runner
// handoff). It asserts BOTH that the prompt-response ScopeFiles reflect the edit
// AND that the approval_submitted audit payload carries remove_scope_files +
// scope_files_before + scope_files_after.
func TestE2E_ScopeEdit_RemoveAndReplaceAtPlanGate(t *testing.T) {
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// Stand up a GitHub+artifact+approval-wired backend over the SAME pool so
	// prompt-render (which 503s without GitHub) and the plan gate resolve. The
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

	const keptA = "backend/internal/server/prompt.go"
	const keptB = "backend/internal/server/approvals.go"
	const removed = "backend/internal/server/scope_headroom.go"
	const added = "backend/internal/server/scope_edit_new.go"

	// seedRun creates a run carrying the workflow spec, a plan stage with an
	// approved 3-file plan artifact parked at the approval gate, and an
	// implement stage. Returns the run + stage ids.
	seedRun := func(t *testing.T) (uuid.UUID, uuid.UUID, uuid.UUID) {
		t.Helper()
		r, err := fx.runRepo.CreateRun(ctx, runpkg.CreateRunParams{
			Repo:          "kuhlman-labs/fishhawk",
			WorkflowID:    "feature_change",
			WorkflowSHA:   "deadbeef",
			TriggerSource: runpkg.TriggerCLI,
			WorkflowSpec:  scopeEditWorkflowSpec,
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
		planBytes := regressionPlanJSON("scoped plan", []string{keptA, keptB, removed})
		if _, err := artifactRepo.Create(ctx, artifact.CreateParams{
			StageID:       planStage.ID,
			Kind:          artifact.KindPlan,
			SchemaVersion: &schema,
			Content:       planBytes,
			ContentHash:   "scopeedit" + r.ID.String()[:8],
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
		return r.ID, planStage.ID, implStage.ID
	}

	// assertApprovalAudit reads the run's approval_submitted entry and checks
	// the remove/before/after payload keys.
	assertApprovalAudit := func(t *testing.T, runID uuid.UUID, wantRemove, wantBefore, wantAfter []string) {
		t.Helper()
		entries, err := auditRepo.ListForRunByCategory(ctx, runID, "approval_submitted")
		if err != nil {
			t.Fatalf("ListForRunByCategory(approval_submitted): %v", err)
		}
		if len(entries) != 1 {
			t.Fatalf("approval_submitted entries = %d, want 1", len(entries))
		}
		var payload struct {
			RemoveScopeFiles []string `json:"remove_scope_files"`
			ScopeFilesBefore []string `json:"scope_files_before"`
			ScopeFilesAfter  []string `json:"scope_files_after"`
		}
		if err := json.Unmarshal(entries[0].Payload, &payload); err != nil {
			t.Fatalf("unmarshal approval_submitted payload: %v", err)
		}
		if !equalStringSets(payload.RemoveScopeFiles, wantRemove) {
			t.Errorf("remove_scope_files = %v, want %v", payload.RemoveScopeFiles, wantRemove)
		}
		if !equalStringSets(payload.ScopeFilesBefore, wantBefore) {
			t.Errorf("scope_files_before = %v, want %v", payload.ScopeFilesBefore, wantBefore)
		}
		if !equalStringSets(payload.ScopeFilesAfter, wantAfter) {
			t.Errorf("scope_files_after = %v, want %v", payload.ScopeFilesAfter, wantAfter)
		}
	}

	t.Run("remove flow", func(t *testing.T) {
		runID, _, implStageID := seedRun(t)

		result, err := session.CallTool(ctx, &mcp.CallToolParams{
			Name: "fishhawk_approve_plan",
			Arguments: map[string]any{
				"run_id":             runID.String(),
				"reason":             "drop the headroom helper from this slice --override-budget",
				"remove_scope_files": []string{removed},
			},
		})
		if err != nil {
			t.Fatalf("CallTool approve (remove): %v", err)
		}
		if result.IsError {
			t.Fatalf("approve (remove) returned error: %s", toolContentString(t, result))
		}

		// Audit: remove recorded with before (3) / after (2).
		assertApprovalAudit(t, runID,
			[]string{removed},
			[]string{keptA, keptB, removed},
			[]string{keptA, keptB})

		// Prompt-response ScopeFiles: the removed path is gone, the kept ones stay.
		scope := getPromptRenderScopeFiles(t, ctx, httpSrv.URL, implStageID)
		inScope := map[string]bool{}
		for _, p := range scope {
			inScope[p] = true
		}
		if inScope[removed] {
			t.Errorf("removed path %q still in prompt ScopeFiles: %v", removed, scope)
		}
		if !inScope[keptA] || !inScope[keptB] {
			t.Errorf("kept paths missing from prompt ScopeFiles: %v", scope)
		}
	})

	t.Run("replace flow", func(t *testing.T) {
		runID, _, implStageID := seedRun(t)

		result, err := session.CallTool(ctx, &mcp.CallToolParams{
			Name: "fishhawk_approve_plan",
			Arguments: map[string]any{
				"run_id":             runID.String(),
				"reason":             "replace the headroom helper with a new file --override-budget",
				"remove_scope_files": []string{removed},
				"add_scope_files":    []string{added},
			},
		})
		if err != nil {
			t.Fatalf("CallTool approve (replace): %v", err)
		}
		if result.IsError {
			t.Fatalf("approve (replace) returned error: %s", toolContentString(t, result))
		}

		// Audit: before is plan ∪ add (4), after subtracts the removal (3).
		assertApprovalAudit(t, runID,
			[]string{removed},
			[]string{keptA, keptB, removed, added},
			[]string{keptA, keptB, added})

		// Prompt-response ScopeFiles: removed gone, added present, kept retained.
		scope := getPromptRenderScopeFiles(t, ctx, httpSrv.URL, implStageID)
		inScope := map[string]bool{}
		for _, p := range scope {
			inScope[p] = true
		}
		if inScope[removed] {
			t.Errorf("removed path %q still in prompt ScopeFiles: %v", removed, scope)
		}
		if !inScope[added] {
			t.Errorf("added path %q missing from prompt ScopeFiles: %v", added, scope)
		}
		if !inScope[keptA] || !inScope[keptB] {
			t.Errorf("kept paths missing from prompt ScopeFiles: %v", scope)
		}
	})
}

// equalStringSets reports whether a and b contain the same elements, ignoring
// order (the effective-scope lists are sorted server-side, but the assertion
// should not be brittle to that ordering).
func equalStringSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}
