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

// decomposedPlanJSON builds a schema-shaped standard_v1 plan body carrying a
// decomposition whose sub_plans each declare their OWN scope — the shape the
// per-slice add channel (#2515) resolves keys and ownership against. titles and
// sliceFiles are positional; the top-level scope is the union.
func decomposedPlanJSON(summary string, titles []string, sliceFiles [][]string) []byte {
	union := make([]map[string]any, 0)
	subs := make([]map[string]any, 0, len(titles))
	for i, ti := range titles {
		files := make([]map[string]any, 0, len(sliceFiles[i]))
		for _, f := range sliceFiles[i] {
			entry := map[string]any{"path": f, "operation": "modify"}
			files = append(files, entry)
			union = append(union, entry)
		}
		subs = append(subs, map[string]any{
			"title":                        ti,
			"scope_hint":                   "implement " + ti,
			"scope":                        map[string]any{"files": files},
			"predicted_runtime_minutes":    10,
			"predicted_runtime_confidence": "high",
		})
	}
	body, _ := json.Marshal(map[string]any{
		"plan_version":                 "standard_v1",
		"ticket_reference":             map[string]any{"type": "github_issue", "url": "https://github.com/x/y/issues/1", "id": "x/y#1"},
		"generated_by":                 map[string]any{"agent": "claude-code", "model": "claude-opus-4-8", "timestamp": "2026-06-15T00:00:00Z"},
		"summary":                      summary,
		"scope":                        map[string]any{"files": union},
		"approach":                     []map[string]any{{"step": 1, "description": "Do the thing."}},
		"verification":                 map[string]any{"test_strategy": "Run the tests.", "rollback_plan": "Revert the PR."},
		"predicted_runtime_minutes":    20,
		"predicted_runtime_confidence": "high",
		"decomposition": map[string]any{
			"rationale": "split for size",
			"sub_plans": subs,
		},
	})
	return body
}

// TestE2E_SliceScopeAdd_AtPlanGate is the #2515 cross-component done-means. It
// drives the seam the per-layer unit tests cannot cover together:
// fishhawk_approve_plan carrying add_scope_files_to_slice through the REAL
// fishhawk-mcp binary → the backend approval handler → the approval_submitted
// audit entry in Postgres → the derived implement prompt-response ScopeFiles of
// TWO different fan-out children. It asserts the audit payload carries the
// CANONICAL index-keyed map (the operator keyed by TITLE), the targeted child's
// scope contains the added path, and the SIBLING child's does not — the
// single-owner-file guarantee, pinned across the real wire.
func TestE2E_SliceScopeAdd_AtPlanGate(t *testing.T) {
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

	const sliceAFile = "backend/internal/server/prompt.go"
	const sliceBFile = "backend/internal/mcpserver/tools.go"
	const addedFile = "backend/internal/mcpserver/README_extra.md"
	titles := []string{"server slice", "tools slice"}

	// Parent run: decomposed plan artifact on a plan stage parked at the gate.
	parent, err := fx.runRepo.CreateRun(ctx, runpkg.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef",
		TriggerSource: runpkg.TriggerCLI,
		WorkflowSpec:  scopeEditWorkflowSpec,
	})
	if err != nil {
		t.Fatalf("CreateRun(parent): %v", err)
	}
	planStage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:            parent.ID,
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
	if _, err := artifactRepo.Create(ctx, artifact.CreateParams{
		StageID:       planStage.ID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &schema,
		Content:       decomposedPlanJSON("decomposed plan", titles, [][]string{{sliceAFile}, {sliceBFile}}),
		ContentHash:   "sliceadd" + parent.ID.String()[:8],
	}); err != nil {
		t.Fatalf("seed decomposed plan artifact: %v", err)
	}
	parkAtGate(t, ctx, fx.runRepo, planStage.ID)

	// Two fan-out children carrying DISTINCT slice_index values, each with an
	// implement stage — the rows the prompt builder resolves the per-slice fold
	// against.
	childStage := make([]uuid.UUID, 2)
	for i := 0; i < 2; i++ {
		idx := i
		child, err := fx.runRepo.CreateRun(ctx, runpkg.CreateRunParams{
			Repo:           "kuhlman-labs/fishhawk",
			WorkflowID:     "feature_change",
			WorkflowSHA:    "deadbeef",
			TriggerSource:  runpkg.TriggerCLI,
			WorkflowSpec:   scopeEditWorkflowSpec,
			ParentRunID:    &parent.ID,
			DecomposedFrom: &parent.ID,
			SliceIndex:     &idx,
		})
		if err != nil {
			t.Fatalf("CreateRun(child %d): %v", i, err)
		}
		st, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
			RunID:        child.ID,
			Sequence:     1,
			Type:         runpkg.StageTypeImplement,
			ExecutorKind: runpkg.ExecutorAgent,
			ExecutorRef:  "fishhawk/runner@v1",
		})
		if err != nil {
			t.Fatalf("CreateStage(child %d implement): %v", i, err)
		}
		childStage[i] = st.ID
	}

	// Approve keying by TITLE — the canonicalisation the audit payload must show.
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "fishhawk_approve_plan",
		Arguments: map[string]any{
			"run_id": parent.ID.String(),
			"reason": "restore the dropped doc into the tools slice --override-budget",
			"add_scope_files_to_slice": map[string]any{
				"tools slice": []string{addedFile},
			},
		},
	})
	if err != nil {
		t.Fatalf("CallTool approve (slice add): %v", err)
	}
	if result.IsError {
		t.Fatalf("approve (slice add) returned error: %s", toolContentString(t, result))
	}

	// Audit: the CANONICAL index-keyed map, even though the operator keyed by title.
	entries, err := auditRepo.ListForRunByCategory(ctx, parent.ID, "approval_submitted")
	if err != nil {
		t.Fatalf("ListForRunByCategory(approval_submitted): %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("approval_submitted entries = %d, want 1", len(entries))
	}
	var payload struct {
		AddScopeFilesToSlice map[string][]string `json:"add_scope_files_to_slice"`
	}
	if err := json.Unmarshal(entries[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal approval_submitted payload: %v", err)
	}
	if len(payload.AddScopeFilesToSlice) != 1 {
		t.Fatalf("add_scope_files_to_slice = %v, want a single index-keyed entry", payload.AddScopeFilesToSlice)
	}
	if !equalStringSets(payload.AddScopeFilesToSlice["1"], []string{addedFile}) {
		t.Errorf("add_scope_files_to_slice[\"1\"] = %v, want [%s] (canonical index-keyed recording)",
			payload.AddScopeFilesToSlice, addedFile)
	}

	// Prompt-response ScopeFiles: present on the TARGETED child, absent on the sibling.
	targeted := getPromptRenderScopeFiles(t, ctx, httpSrv.URL, childStage[1])
	sibling := getPromptRenderScopeFiles(t, ctx, httpSrv.URL, childStage[0])

	inTargeted := map[string]bool{}
	for _, p := range targeted {
		inTargeted[p] = true
	}
	if !inTargeted[addedFile] {
		t.Errorf("targeted slice-1 child ScopeFiles missing %q: %v", addedFile, targeted)
	}
	if !inTargeted[sliceBFile] {
		t.Errorf("targeted slice-1 child ScopeFiles missing its own planned file %q: %v", sliceBFile, targeted)
	}
	for _, p := range sibling {
		if p == addedFile {
			t.Errorf("sibling slice-0 child ScopeFiles contains %q — the per-slice add fanned into another slice (single-owner-file violated): %v", addedFile, sibling)
		}
	}
}

// promptRenderScope captures both the enforced scope_files and the agent-facing
// scope_constraint.scope_files of a prompt-render response.
type promptRenderScope struct {
	ScopeFiles []struct {
		Path string `json:"path"`
	} `json:"scope_files"`
	ScopeConstraint struct {
		ScopeFiles []string `json:"scope_files"`
	} `json:"scope_constraint"`
}

func (p promptRenderScope) scopePaths() []string {
	out := make([]string, 0, len(p.ScopeFiles))
	for _, f := range p.ScopeFiles {
		out = append(out, f.Path)
	}
	return out
}

func getPromptRenderScope(t *testing.T, ctx context.Context, baseURL string, stageID uuid.UUID) promptRenderScope {
	t.Helper()
	var out promptRenderScope
	getPromptRenderJSON(t, ctx, baseURL, stageID, &out)
	return out
}

// TestE2E_SliceScopeMove_AtPlanGate is the #2596 cross-component done-means. It
// drives fishhawk_approve_plan carrying move_scope_files_to_slice through the
// REAL fishhawk-mcp binary → the backend approval handler → the approval_submitted
// audit entry in Postgres → the derived implement prompt-response of the SOURCE
// and DESTINATION fan-out children. It asserts the audit carries the CANONICAL
// index-keyed map + the resolved [{path,from_slice,to_slice}] list, that the
// SOURCE child's scope_files AND scope_constraint.scope_files both DROP the moved
// path, and that the DESTINATION child's enforced scope_files GAINS it — both
// sides of the move, across the real wire. A second approve exercises the
// composition decision: add + move over disjoint paths in ONE audited call.
func TestE2E_SliceScopeMove_AtPlanGate(t *testing.T) {
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

	// Source slice 0 owns movedFile + srcKeepFile; destination slice 1 owns
	// destOwnFile. Moving movedFile from 0 to 1 leaves slice 0 with srcKeepFile.
	const movedFile = "backend/internal/server/prompt.go"
	const srcKeepFile = "backend/internal/server/reads.go"
	const destOwnFile = "backend/internal/mcpserver/tools.go"
	titles := []string{"server slice", "tools slice"}

	parent, err := fx.runRepo.CreateRun(ctx, runpkg.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef",
		TriggerSource: runpkg.TriggerCLI,
		WorkflowSpec:  scopeEditWorkflowSpec,
	})
	if err != nil {
		t.Fatalf("CreateRun(parent): %v", err)
	}
	planStage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:            parent.ID,
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
	if _, err := artifactRepo.Create(ctx, artifact.CreateParams{
		StageID:       planStage.ID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &schema,
		Content:       decomposedPlanJSON("decomposed plan", titles, [][]string{{movedFile, srcKeepFile}, {destOwnFile}}),
		ContentHash:   "slicemove" + parent.ID.String()[:8],
	}); err != nil {
		t.Fatalf("seed decomposed plan artifact: %v", err)
	}
	parkAtGate(t, ctx, fx.runRepo, planStage.ID)

	childStage := make([]uuid.UUID, 2)
	for i := 0; i < 2; i++ {
		idx := i
		child, err := fx.runRepo.CreateRun(ctx, runpkg.CreateRunParams{
			Repo:           "kuhlman-labs/fishhawk",
			WorkflowID:     "feature_change",
			WorkflowSHA:    "deadbeef",
			TriggerSource:  runpkg.TriggerCLI,
			WorkflowSpec:   scopeEditWorkflowSpec,
			ParentRunID:    &parent.ID,
			DecomposedFrom: &parent.ID,
			SliceIndex:     &idx,
		})
		if err != nil {
			t.Fatalf("CreateRun(child %d): %v", i, err)
		}
		st, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
			RunID:        child.ID,
			Sequence:     1,
			Type:         runpkg.StageTypeImplement,
			ExecutorKind: runpkg.ExecutorAgent,
			ExecutorRef:  "fishhawk/runner@v1",
		})
		if err != nil {
			t.Fatalf("CreateStage(child %d implement): %v", i, err)
		}
		childStage[i] = st.ID
	}

	// Approve keying the DESTINATION by TITLE — the canonicalisation the audit
	// must show as index-keyed.
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "fishhawk_approve_plan",
		Arguments: map[string]any{
			"run_id": parent.ID.String(),
			"reason": "relocate prompt.go into the tools slice --override-budget",
			"move_scope_files_to_slice": map[string]any{
				"tools slice": []string{movedFile},
			},
		},
	})
	if err != nil {
		t.Fatalf("CallTool approve (slice move): %v", err)
	}
	if result.IsError {
		t.Fatalf("approve (slice move) returned error: %s", toolContentString(t, result))
	}

	// Audit: the CANONICAL index-keyed move map + resolved list.
	entries, err := auditRepo.ListForRunByCategory(ctx, parent.ID, "approval_submitted")
	if err != nil {
		t.Fatalf("ListForRunByCategory(approval_submitted): %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("approval_submitted entries = %d, want 1", len(entries))
	}
	var payload struct {
		MoveScopeFilesToSlice  map[string][]string `json:"move_scope_files_to_slice"`
		MoveScopeFilesResolved []struct {
			Path      string `json:"path"`
			FromSlice int    `json:"from_slice"`
			ToSlice   int    `json:"to_slice"`
		} `json:"move_scope_files_resolved"`
	}
	if err := json.Unmarshal(entries[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal approval_submitted payload: %v", err)
	}
	if !equalStringSets(payload.MoveScopeFilesToSlice["1"], []string{movedFile}) {
		t.Errorf("move_scope_files_to_slice[\"1\"] = %v, want [%s] (canonical index-keyed)", payload.MoveScopeFilesToSlice, movedFile)
	}
	if len(payload.MoveScopeFilesResolved) != 1 ||
		payload.MoveScopeFilesResolved[0].Path != movedFile ||
		payload.MoveScopeFilesResolved[0].FromSlice != 0 ||
		payload.MoveScopeFilesResolved[0].ToSlice != 1 {
		t.Errorf("move_scope_files_resolved = %+v, want one {%s, from 0, to 1}", payload.MoveScopeFilesResolved, movedFile)
	}

	// SOURCE child (slice 0): both scope_files AND scope_constraint.scope_files
	// DROP the moved path but keep srcKeepFile.
	src := getPromptRenderScope(t, ctx, httpSrv.URL, childStage[0])
	if containsPath(src.scopePaths(), movedFile) {
		t.Errorf("source slice-0 scope_files still contains the moved path %q: %v", movedFile, src.scopePaths())
	}
	if containsPath(src.ScopeConstraint.ScopeFiles, movedFile) {
		t.Errorf("source slice-0 scope_constraint.scope_files still contains the moved path %q: %v", movedFile, src.ScopeConstraint.ScopeFiles)
	}
	if !containsPath(src.scopePaths(), srcKeepFile) {
		t.Errorf("source slice-0 scope_files dropped its retained path %q: %v", srcKeepFile, src.scopePaths())
	}

	// DESTINATION child (slice 1): enforced scope_files GAINS the moved-in path
	// (via the resolveApprovalAddScopeFiles fold) alongside its own file.
	dst := getPromptRenderScope(t, ctx, httpSrv.URL, childStage[1])
	if !containsPath(dst.scopePaths(), movedFile) {
		t.Errorf("destination slice-1 scope_files missing the moved-in path %q: %v", movedFile, dst.scopePaths())
	}
	if !containsPath(dst.scopePaths(), destOwnFile) {
		t.Errorf("destination slice-1 scope_files missing its own planned file %q: %v", destOwnFile, dst.scopePaths())
	}
}

// TestE2E_SliceScopeMoveAndAdd_ComposeInOneApprove asserts the #2596 composition
// decision: one fishhawk_approve_plan carrying BOTH add_scope_files_to_slice and
// move_scope_files_to_slice over DISJOINT paths lands both edits in one audited
// call, and both fold into the destination children's scopes.
func TestE2E_SliceScopeMoveAndAdd_ComposeInOneApprove(t *testing.T) {
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

	const movedFile = "backend/internal/server/prompt.go"
	const srcKeepFile = "backend/internal/server/reads.go"
	const destOwnFile = "backend/internal/mcpserver/tools.go"
	const addedFile = "backend/internal/mcpserver/README_extra.md"
	titles := []string{"server slice", "tools slice"}

	parent, err := fx.runRepo.CreateRun(ctx, runpkg.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef",
		TriggerSource: runpkg.TriggerCLI,
		WorkflowSpec:  scopeEditWorkflowSpec,
	})
	if err != nil {
		t.Fatalf("CreateRun(parent): %v", err)
	}
	planStage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:            parent.ID,
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
	if _, err := artifactRepo.Create(ctx, artifact.CreateParams{
		StageID:       planStage.ID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &schema,
		Content:       decomposedPlanJSON("decomposed plan", titles, [][]string{{movedFile, srcKeepFile}, {destOwnFile}}),
		ContentHash:   "slicemix" + parent.ID.String()[:8],
	}); err != nil {
		t.Fatalf("seed decomposed plan artifact: %v", err)
	}
	parkAtGate(t, ctx, fx.runRepo, planStage.ID)

	childStage := make([]uuid.UUID, 2)
	for i := 0; i < 2; i++ {
		idx := i
		child, err := fx.runRepo.CreateRun(ctx, runpkg.CreateRunParams{
			Repo:           "kuhlman-labs/fishhawk",
			WorkflowID:     "feature_change",
			WorkflowSHA:    "deadbeef",
			TriggerSource:  runpkg.TriggerCLI,
			WorkflowSpec:   scopeEditWorkflowSpec,
			ParentRunID:    &parent.ID,
			DecomposedFrom: &parent.ID,
			SliceIndex:     &idx,
		})
		if err != nil {
			t.Fatalf("CreateRun(child %d): %v", i, err)
		}
		st, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
			RunID:        child.ID,
			Sequence:     1,
			Type:         runpkg.StageTypeImplement,
			ExecutorKind: runpkg.ExecutorAgent,
			ExecutorRef:  "fishhawk/runner@v1",
		})
		if err != nil {
			t.Fatalf("CreateStage(child %d implement): %v", i, err)
		}
		childStage[i] = st.ID
	}

	// One approve: MOVE prompt.go -> tools slice AND ADD a net-new doc to tools
	// slice, over DISJOINT paths.
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "fishhawk_approve_plan",
		Arguments: map[string]any{
			"run_id": parent.ID.String(),
			"reason": "relocate prompt.go and add a doc --override-budget",
			"move_scope_files_to_slice": map[string]any{
				"tools slice": []string{movedFile},
			},
			"add_scope_files_to_slice": map[string]any{
				"tools slice": []string{addedFile},
			},
		},
	})
	if err != nil {
		t.Fatalf("CallTool approve (move+add): %v", err)
	}
	if result.IsError {
		t.Fatalf("approve (move+add) returned error: %s", toolContentString(t, result))
	}

	entries, err := auditRepo.ListForRunByCategory(ctx, parent.ID, "approval_submitted")
	if err != nil {
		t.Fatalf("ListForRunByCategory: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("approval_submitted entries = %d, want 1", len(entries))
	}
	var payload struct {
		MoveScopeFilesToSlice map[string][]string `json:"move_scope_files_to_slice"`
		AddScopeFilesToSlice  map[string][]string `json:"add_scope_files_to_slice"`
	}
	if err := json.Unmarshal(entries[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if !equalStringSets(payload.MoveScopeFilesToSlice["1"], []string{movedFile}) {
		t.Errorf("move map = %v, want {1:[%s]}", payload.MoveScopeFilesToSlice, movedFile)
	}
	if !equalStringSets(payload.AddScopeFilesToSlice["1"], []string{addedFile}) {
		t.Errorf("add map = %v, want {1:[%s]}", payload.AddScopeFilesToSlice, addedFile)
	}

	// Destination slice 1 gains BOTH the moved and the added path; source slice 0
	// drops the moved one.
	dst := getPromptRenderScope(t, ctx, httpSrv.URL, childStage[1])
	for _, want := range []string{movedFile, addedFile, destOwnFile} {
		if !containsPath(dst.scopePaths(), want) {
			t.Errorf("destination scope_files missing %q: %v", want, dst.scopePaths())
		}
	}
	src := getPromptRenderScope(t, ctx, httpSrv.URL, childStage[0])
	if containsPath(src.scopePaths(), movedFile) {
		t.Errorf("source scope_files still contains the moved path %q: %v", movedFile, src.scopePaths())
	}
}

func containsPath(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// scopeCapOverrideSpec is a feature_change spec whose implement stage caps
// max_files_changed at 3 — the tight cap the #2415 override cases drive against.
var scopeCapOverrideSpec = []byte(`version: "0.3"
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
          - max_files_changed: 3
`)

// opScopedPlanJSON builds a schema-valid standard_v1 plan whose scope.files
// carry the given per-operation paths, so a rename-shaped scope (creates+deletes)
// can be seeded. Paths are positional within each operation bucket.
func opScopedPlanJSON(summary string, creates, deletes, modifies []string) []byte {
	files := make([]map[string]any, 0, len(creates)+len(deletes)+len(modifies))
	for _, f := range creates {
		files = append(files, map[string]any{"path": f, "operation": "create"})
	}
	for _, f := range deletes {
		files = append(files, map[string]any{"path": f, "operation": "delete"})
	}
	for _, f := range modifies {
		files = append(files, map[string]any{"path": f, "operation": "modify"})
	}
	body, _ := json.Marshal(map[string]any{
		"plan_version":     "standard_v1",
		"ticket_reference": map[string]any{"type": "github_issue", "url": "https://github.com/x/y/issues/1", "id": "x/y#1"},
		"generated_by":     map[string]any{"agent": "claude-code", "model": "claude-opus-4-8", "timestamp": "2026-06-15T00:00:00Z"},
		"summary":          summary,
		"scope":            map[string]any{"files": files},
		"approach":         []map[string]any{{"step": 1, "description": "Do the thing."}},
		"verification":     map[string]any{"test_strategy": "Run the tests.", "rollback_plan": "Revert the PR."},
		// Under the 15m default budget so the budget gate (which runs before the
		// scope-cap gate) never fires — the scope-cap outcome is the sole variable.
		"predicted_runtime_minutes":    10,
		"predicted_runtime_confidence": "high",
	})
	return body
}

// TestE2E_ScopeCapOverride_RefusedAcknowledgedAndFieldReachesGetPlan is the
// #2415 cross-boundary done-means. It drives the seam per-layer unit tests
// cannot cover together — the REAL fishhawk-mcp binary → the backend approval
// handler → the Postgres audit store — for BOTH override outcomes on one fixture
// spec, plus the end-to-end field pin the change exists for: the
// minimum-physical-count travels the server pre-check payload → audit
// persistence → get_plan → MCP response through the real ship path.
func TestE2E_ScopeCapOverride_RefusedAcknowledgedAndFieldReachesGetPlan(t *testing.T) {
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
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

	// seedGatedRun creates a run carrying scopeCapOverrideSpec with a plan stage
	// holding the given plan artifact, parked at the approval gate.
	seedGatedRun := func(t *testing.T, planBytes []byte) (uuid.UUID, uuid.UUID) {
		t.Helper()
		r, err := fx.runRepo.CreateRun(ctx, runpkg.CreateRunParams{
			Repo:          "kuhlman-labs/fishhawk",
			WorkflowID:    "feature_change",
			WorkflowSHA:   "deadbeef",
			TriggerSource: runpkg.TriggerCLI,
			WorkflowSpec:  scopeCapOverrideSpec,
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
		if _, err := artifactRepo.Create(ctx, artifact.CreateParams{
			StageID: planStage.ID, Kind: artifact.KindPlan, SchemaVersion: &schema,
			Content: planBytes, ContentHash: "capovr" + r.ID.String()[:8],
		}); err != nil {
			t.Fatalf("seed plan artifact: %v", err)
		}
		parkAtGate(t, ctx, fx.runRepo, planStage.ID)
		return r.ID, planStage.ID
	}

	auditCategories := func(t *testing.T, runID uuid.UUID, category string) int {
		t.Helper()
		entries, err := auditRepo.ListForRunByCategory(ctx, runID, category)
		if err != nil {
			t.Fatalf("ListForRunByCategory(%s): %v", category, err)
		}
		return len(entries)
	}

	t.Run("override refused when min physical over cap", func(t *testing.T) {
		// 4 all-modify files vs cap 3: over BOTH the declared and the physical
		// count, so the override cannot authorize the landing.
		runID, planStageID := seedGatedRun(t, opScopedPlanJSON("all-modify over cap", nil, nil,
			[]string{"backend/a.go", "backend/b.go", "backend/c.go", "backend/d.go"}))

		result, err := session.CallTool(ctx, &mcp.CallToolParams{
			Name: "fishhawk_approve_plan",
			Arguments: map[string]any{
				"run_id": runID.String(),
				"reason": "force it through --override-scope-cap",
			},
		})
		if err != nil {
			t.Fatalf("CallTool approve (override refused): %v", err)
		}
		if !result.IsError {
			t.Fatalf("approve must return a tool error; got success: %s", toolContentString(t, result))
		}
		if txt := toolContentString(t, result); !strings.Contains(txt, "plan_scope_cap_override_unavailable") {
			t.Errorf("error text missing the 422 code: %s", txt)
		}
		// Committed state: no approval row, a refused audit entry, no ack.
		approvals, err := approvalRepo.ListForStage(ctx, planStageID)
		if err != nil {
			t.Fatalf("ListForStage: %v", err)
		}
		if len(approvals) != 0 {
			t.Errorf("approval rows = %d, want 0 (override refusal is pre-insert)", len(approvals))
		}
		if n := auditCategories(t, runID, "plan_scope_cap_override_refused"); n != 1 {
			t.Errorf("plan_scope_cap_override_refused entries = %d, want 1", n)
		}
		if n := auditCategories(t, runID, "plan_scope_cap_override_acknowledged"); n != 0 {
			t.Errorf("plan_scope_cap_override_acknowledged entries = %d, want 0 on a refusal", n)
		}
	})

	t.Run("override acknowledged when min physical fits", func(t *testing.T) {
		// Rename-shaped: 2 create + 2 delete = 4 declared (over cap 3) but a
		// minimum physical count of 2 (<= 3), so the override legitimately works.
		runID, planStageID := seedGatedRun(t, opScopedPlanJSON("rename-shaped",
			[]string{"backend/new0.go", "backend/new1.go"},
			[]string{"backend/old0.go", "backend/old1.go"}, nil))

		result, err := session.CallTool(ctx, &mcp.CallToolParams{
			Name: "fishhawk_approve_plan",
			Arguments: map[string]any{
				"run_id": runID.String(),
				"reason": "byte-preserving rename, real diff fits --override-scope-cap",
			},
		})
		if err != nil {
			t.Fatalf("CallTool approve (override acknowledged): %v", err)
		}
		if result.IsError {
			t.Fatalf("approve must succeed for a rename-shaped scope; got error: %s", toolContentString(t, result))
		}
		approvals, err := approvalRepo.ListForStage(ctx, planStageID)
		if err != nil {
			t.Fatalf("ListForStage: %v", err)
		}
		if len(approvals) != 1 {
			t.Errorf("approval rows = %d, want 1 after the override succeeds", len(approvals))
		}
		if n := auditCategories(t, runID, "plan_scope_cap_override_acknowledged"); n != 1 {
			t.Errorf("plan_scope_cap_override_acknowledged entries = %d, want 1", n)
		}
		if n := auditCategories(t, runID, "plan_scope_cap_override_refused"); n != 0 {
			t.Errorf("plan_scope_cap_override_refused entries = %d, want 0 when the override works", n)
		}
	})

	t.Run("min_changed_files reaches get_plan through the real ship path", func(t *testing.T) {
		// A NEW run whose cap (5) admits the ship (a 4-declared plan is under cap
		// by count, so overCapSplitRejection does not fire) — this exercises the
		// REAL handleShipPlan -> runScopePrecheck computation of min_changed_files,
		// not a seeded payload.
		r, err := fx.runRepo.CreateRun(ctx, runpkg.CreateRunParams{
			Repo:          "kuhlman-labs/fishhawk",
			WorkflowID:    "feature_change",
			WorkflowSHA:   "deadbeef",
			TriggerSource: runpkg.TriggerCLI,
			WorkflowSpec:  scopeEditWorkflowSpec, // cap 5
		})
		if err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		issued, err := signingRepo.Issue(ctx, r.ID, time.Hour)
		if err != nil {
			t.Fatalf("Issue signing key: %v", err)
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
		walkToRunning(t, ctx, fx, planStage.ID)

		// Rename-shaped: 2 create + 2 delete = scanned_files 4, min_changed_files 2.
		resp := shipPlanSigned(t, ctx, httpSrv.URL, r.ID, planStage.ID, issued.PrivateKey,
			opScopedPlanJSON("rename-shaped shipped",
				[]string{"backend/internal/x/new0.go", "backend/internal/x/new1.go"},
				[]string{"backend/internal/x/old0.go", "backend/internal/x/old1.go"}, nil))
		_ = resp.Body.Close()
		if resp.StatusCode != 201 {
			t.Fatalf("ship plan status = %d, want 201", resp.StatusCode)
		}

		result, err := session.CallTool(ctx, &mcp.CallToolParams{
			Name:      "fishhawk_get_plan",
			Arguments: map[string]any{"run_id": r.ID.String()},
		})
		if err != nil {
			t.Fatalf("CallTool fishhawk_get_plan: %v", err)
		}
		if result.IsError {
			t.Fatalf("get_plan returned error: %s", toolContentString(t, result))
		}
		var out struct {
			ScopePrecheck struct {
				ScannedFiles    int `json:"scanned_files"`
				MinChangedFiles int `json:"min_changed_files"`
			} `json:"scope_precheck"`
		}
		decodeStructured(t, result, &out)
		if out.ScopePrecheck.ScannedFiles != 4 {
			t.Errorf("scope_precheck.scanned_files = %d, want 4", out.ScopePrecheck.ScannedFiles)
		}
		if out.ScopePrecheck.MinChangedFiles != 2 {
			t.Errorf("scope_precheck.min_changed_files = %d, want 2 (reached the operator across every layer)", out.ScopePrecheck.MinChangedFiles)
		}
	})
}
