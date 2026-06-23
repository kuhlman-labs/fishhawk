package mcpe2e_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/bundle"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
	"github.com/kuhlman-labs/fishhawk/backend/internal/tracestore"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestE2E_Fixup_ConcernRoutedBackAndBounded is the cross-component
// integration test for the implement-review fix-up stage (E22.X /
// #762). It drives the seam the per-layer unit tests can't cover on
// their own (cf. #618): a concern recorded on the implement-review
// audit entry → an operator triggering a fix-up through the REAL
// fishhawk-mcp binary → the run state machine re-opening the stage →
// the prompt renderer delivering the selected concern as a binding
// instruction → the bounded second pass being refused.
//
// What this harness CAN exercise end-to-end (real MCP binary → real
// backend HTTP → real Postgres):
//
//   - the MCP fix-up tool reaches the backend, the subject (operator
//     fhk_* token) authorizes, run.FixupStage re-opens the implement
//     stage awaiting_approval → pending, and the stage_fixup_triggered
//     audit entry lands in Postgres carrying the operator-selected
//     concern;
//   - the deterministic prompt renderer reads that audit entry back and
//     emits the "### Fix-up concerns" binding section (the #558 framing)
//     — the audit → prompt seam that sub-plan B wired;
//   - the bound: a second fix-up once the (default-1) budget is spent is
//     refused with fixup_budget_exhausted.
//
// The two legs this harness deliberately does NOT drive — the runner
// committing onto the SAME PR branch (sub-plan C, covered by
// runner/cmd/fishhawk-runner unit tests) and the implement review
// auto-re-running on the fix-up's trace upload (keyed to the trace
// upload, covered in backend/internal/server) — require a spawned
// runner + a real workflow dispatch that this fixture has no agent for.
func TestE2E_Fixup_ConcernRoutedBackAndBounded(t *testing.T) {
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// newFixture's server has no GitHub wired, but the prompt-render
	// handler short-circuits to 503 without it (issueGetter() == nil).
	// Stand up a second backend over the SAME pool with GitHub wired so
	// we can assert the rendered prompt. The run has no issue ref, so the
	// client never makes a GitHub call; New(nil) is enough to clear the
	// nil guard. The operator fhk_* token authenticates against the same
	// apitoken rows (same pool), so it works against this server too.
	auditRepo := audit.NewPostgresRepository(fx.pool)
	signingRepo := signing.NewPostgresRepository(fx.pool)
	// Configure the per-adapter allow-list (#1164) so the operator's
	// implement_model fix-up override is validated, not merely fail-open
	// accepted. The fixture run carries no WorkflowSpec, so the implement
	// adapter resolves to "claudecode" (the default spawn's adapter).
	const fixupModelOverride = "claude-haiku-4-5-20251001"
	srv := server.New(server.Config{
		Addr:                   "127.0.0.1:0",
		RunRepo:                fx.runRepo,
		AuditRepo:              auditRepo,
		SigningRepo:            signingRepo,
		APITokenRepo:           fx.apitokenRepo,
		GitHub:                 githubclient.New(nil),
		ImplementAllowedModels: server.AllowedModels{"claudecode": {fixupModelOverride: true}},
	})
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)

	// 1. Seed an implement stage parked at the review gate. CreateStage
	// lands it in pending; walk it pending → dispatched → running →
	// awaiting_approval so it is a valid fix-up candidate.
	stage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:            fx.runID,
		Sequence:         1,
		Type:             runpkg.StageTypeImplement,
		ExecutorKind:     runpkg.ExecutorAgent,
		ExecutorRef:      "fishhawk/runner@v1",
		RequiresApproval: true,
	})
	if err != nil {
		t.Fatalf("CreateStage: %v", err)
	}
	parkAtGate(t, ctx, fx.runRepo, stage.ID)

	// 2. Record the implement-review verdict the fix-up routes back:
	// approve_with_concerns with two concerns. The operator will select
	// the first (a scope concern naming a file).
	scopeConcern := planreview.Concern{
		Severity: planreview.SeverityMedium,
		Category: "scope",
		Note:     "guard backend/internal/run/fixup.go against a nil stage before transition",
	}
	styleConcern := planreview.Concern{
		Severity: planreview.SeverityLow,
		Category: "style",
		Note:     "rename the loop variable for clarity",
	}
	seedImplementReview(t, ctx, auditRepo, fx.runID, stage.ID, scopeConcern, styleConcern)

	// 3. Trigger the fix-up through the real fishhawk-mcp binary,
	// pointed at the GitHub-wired backend. Operator fhk_* token carries
	// write:stages, so the fix-up handler authorizes it.
	session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, httpSrv.URL)

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "fishhawk_fixup_stage",
		Arguments: map[string]any{
			"stage_id":        stage.ID.String(),
			"concerns":        []int{0},
			"reason":          "address the scope concern on the existing branch",
			"implement_model": fixupModelOverride,
		},
	})
	if err != nil {
		t.Fatalf("CallTool fishhawk_fixup_stage: %v", err)
	}
	if result.IsError {
		t.Fatalf("fix-up tool returned error: %s", toolContentString(t, result))
	}

	// The re-opened stage comes back as pending (no orchestrator wired in
	// this fixture, so it stays in pending after the re-open rather than
	// advancing to dispatched).
	var fixupOut struct {
		Stage struct {
			ID    string `json:"id"`
			State string `json:"state"`
			Type  string `json:"type"`
		} `json:"stage"`
	}
	decodeStructured(t, result, &fixupOut)
	if fixupOut.Stage.ID != stage.ID.String() {
		t.Errorf("fix-up stage id = %q, want %s", fixupOut.Stage.ID, stage.ID)
	}
	if fixupOut.Stage.State != string(runpkg.StageStatePending) {
		t.Errorf("fix-up stage state = %q, want pending", fixupOut.Stage.State)
	}

	// 4. The stage_fixup_triggered audit entry landed in Postgres
	// carrying the selected concern — the durable record the bound is
	// counted against and the prompt renderer reads back.
	entries, err := auditRepo.ListForRunByCategory(ctx, fx.runID, server.CategoryStageFixupTriggered)
	if err != nil {
		t.Fatalf("ListForRunByCategory: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("stage_fixup_triggered entries = %d, want 1", len(entries))
	}
	var triggered struct {
		PassOrdinal      int                  `json:"pass_ordinal"`
		RemainingBudget  int                  `json:"remaining_budget"`
		Concerns         []planreview.Concern `json:"concerns"`
		FixupModel       string               `json:"fixup_model"`
		FixupModelSource string               `json:"fixup_model_source"`
	}
	if err := json.Unmarshal(entries[0].Payload, &triggered); err != nil {
		t.Fatalf("unmarshal stage_fixup_triggered payload: %v", err)
	}
	if triggered.PassOrdinal != 1 {
		t.Errorf("pass_ordinal = %d, want 1", triggered.PassOrdinal)
	}
	if triggered.RemainingBudget != 0 {
		t.Errorf("remaining_budget = %d, want 0 (default bound is 1)", triggered.RemainingBudget)
	}
	if len(triggered.Concerns) != 1 || triggered.Concerns[0].Category != "scope" {
		t.Fatalf("persisted concerns = %+v, want the single scope concern", triggered.Concerns)
	}
	// #1164: the operator's allow-listed implement_model override is pinned on
	// the stage_fixup_triggered entry at trigger time (the persist leg of the
	// seam).
	if triggered.FixupModel != fixupModelOverride {
		t.Errorf("fixup_model = %q, want the operator override %q", triggered.FixupModel, fixupModelOverride)
	}
	if triggered.FixupModelSource != "operator" {
		t.Errorf("fixup_model_source = %q, want operator", triggered.FixupModelSource)
	}

	// 5. The deterministic prompt now renders the selected concern as a
	// binding instruction. The stage is in pending (runnable) after the
	// re-open, so prompt-render serves it. Assert the binding section and
	// the concern note are both present (the #558 MANDATORY framing).
	rendered := getPromptRender(t, ctx, httpSrv.URL, stage.ID)
	if !strings.Contains(rendered, "### Fix-up concerns") {
		t.Errorf("rendered prompt missing the Fix-up concerns section:\n%s", rendered)
	}
	if !strings.Contains(rendered, "MANDATORY") {
		t.Errorf("rendered Fix-up concerns section missing the binding MANDATORY framing:\n%s", rendered)
	}
	if !strings.Contains(rendered, scopeConcern.Note) {
		t.Errorf("rendered prompt missing the selected concern note %q", scopeConcern.Note)
	}
	if strings.Contains(rendered, styleConcern.Note) {
		t.Errorf("rendered prompt leaked the UNSELECTED concern note %q", styleConcern.Note)
	}

	// 5b. #1164: the fix-up prompt response carries the pinned model on the
	// implement_model wire field — the read-back leg of the seam (the runner
	// threads this to --model). Read back via the prompt-render JSON so the
	// model survives audit-persist → prompt-fetch.
	var promptResp struct {
		ImplementModel string `json:"implement_model"`
	}
	getPromptRenderJSON(t, ctx, httpSrv.URL, stage.ID, &promptResp)
	if promptResp.ImplementModel != fixupModelOverride {
		t.Errorf("prompt implement_model = %q, want the pinned override %q", promptResp.ImplementModel, fixupModelOverride)
	}

	// 5c. #1164: GET /v0/runs/{id} distills the run's fix-up model into the
	// fixup_model status field — the run-status leg of the seam.
	var runResp struct {
		FixupModel *struct {
			Model       string `json:"model"`
			Source      string `json:"source"`
			PassOrdinal int    `json:"pass_ordinal"`
		} `json:"fixup_model"`
	}
	getRunJSON(t, ctx, httpSrv.URL, fx.operatorTok, fx.runID, &runResp)
	if runResp.FixupModel == nil {
		t.Fatalf("run-status fixup_model absent; want the surfaced pin")
	}
	if runResp.FixupModel.Model != fixupModelOverride || runResp.FixupModel.Source != "operator" || runResp.FixupModel.PassOrdinal != 1 {
		t.Errorf("run-status fixup_model = %+v, want {model:%q source:operator pass_ordinal:1}", *runResp.FixupModel, fixupModelOverride)
	}

	// 6. The bound: a second fix-up is refused. Re-park the stage at the
	// gate first (modelling the re-review landing it on awaiting_approval
	// again), so the only thing blocking the second pass is the spent
	// budget — not the state machine.
	parkAtGate(t, ctx, fx.runRepo, stage.ID)

	second, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "fishhawk_fixup_stage",
		Arguments: map[string]any{
			"stage_id": stage.ID.String(),
			"concerns": []int{0},
		},
	})
	if err != nil {
		t.Fatalf("CallTool second fishhawk_fixup_stage: %v", err)
	}
	if !second.IsError {
		t.Fatalf("second fix-up unexpectedly succeeded; want fixup_budget_exhausted")
	}
	if body := toolContentString(t, second); !strings.Contains(body, "fixup_budget_exhausted") {
		t.Errorf("second fix-up error missing fixup_budget_exhausted code: %s", body)
	}

	// Still exactly one stage_fixup_triggered entry — the refused pass
	// wrote none.
	entries, err = auditRepo.ListForRunByCategory(ctx, fx.runID, server.CategoryStageFixupTriggered)
	if err != nil {
		t.Fatalf("ListForRunByCategory (post-refusal): %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("stage_fixup_triggered entries after refused pass = %d, want 1", len(entries))
	}
}

// TestE2E_Fixup_AllowCreateFoldsIntoEffectiveScope is the cross-boundary
// integration test for the fix-up allow-create allow-list (#823). It
// drives the seam the per-layer unit tests can't cover alone (cf. #618):
// the MCP tool input → HTTP request → stage_fixup_triggered audit payload
// persist → prompt renderer's effective scope.files. Under the #1314
// full-plan-scope-retention behavior it proves three directions at once:
//
//   - a path DECLARED via allow_create is folded into the implement prompt's
//     effective scope.files (#823) — the exact set the runner's #818
//     created-out-of-scope gate diffs created files against — so the runner
//     stages it and the gate no longer trips for it;
//   - the full approved plan scope is RETAINED (#1314): the plan-only file NOT
//     named by the routed concern is PRESENT, so the agent's in-plan edits ship
//     in the fix-up commit rather than being drift-excluded (the silent-no-op
//     class #1314 fixes);
//   - a path NOT declared (nor in plan scope) does NOT appear in the effective
//     scope.files, so the #818 silent-strip hole stays closed: an undeclared
//     created file is still category-B.
func TestE2E_Fixup_AllowCreateFoldsIntoEffectiveScope(t *testing.T) {
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Second backend over the SAME pool with GitHub + ArtifactRepo wired so
	// the implement prompt can load the approved plan's scope.files. (The
	// fixture's own server has neither.)
	auditRepo := audit.NewPostgresRepository(fx.pool)
	signingRepo := signing.NewPostgresRepository(fx.pool)
	artifactRepo := artifact.NewPostgresRepository(fx.pool)
	srv := server.New(server.Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      fx.runRepo,
		AuditRepo:    auditRepo,
		SigningRepo:  signingRepo,
		ArtifactRepo: artifactRepo,
		APITokenRepo: fx.apitokenRepo,
		GitHub:       githubclient.New(nil),
	})
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)

	// 1. Seed a plan stage carrying an approved standard_v1 plan whose
	// scope.files is non-empty (the empty-scope fold guard requires it).
	planStage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:        fx.runID,
		Sequence:     1,
		Type:         runpkg.StageTypePlan,
		ExecutorKind: runpkg.ExecutorAgent,
		ExecutorRef:  "fishhawk/runner@v1",
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

	// 2. Seed the implement stage parked at the review gate.
	implStage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:            fx.runID,
		Sequence:         2,
		Type:             runpkg.StageTypeImplement,
		ExecutorKind:     runpkg.ExecutorAgent,
		ExecutorRef:      "fishhawk/runner@v1",
		RequiresApproval: true,
	})
	if err != nil {
		t.Fatalf("CreateStage implement: %v", err)
	}
	parkAtGate(t, ctx, fx.runRepo, implStage.ID)

	// 3. Record the implement-review concern the fix-up routes back: a
	// concern requiring a NET-NEW file.
	concern := planreview.Concern{
		Severity: planreview.SeverityMedium,
		Category: "scope",
		Note:     "extract the helper into a new file backend/internal/server/helper.go",
	}
	seedImplementReview(t, ctx, auditRepo, fx.runID, implStage.ID, concern)

	// 4. Trigger the fix-up through the real MCP binary, declaring the
	// net-new file via allow_create. An UNDECLARED sibling path is named
	// only here in the test — never passed to the tool.
	const declared = "backend/internal/server/helper.go"
	const undeclared = "backend/internal/server/undeclared.go"
	session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, httpSrv.URL)
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "fishhawk_fixup_stage",
		Arguments: map[string]any{
			"stage_id":     implStage.ID.String(),
			"concerns":     []int{0},
			"reason":       "create the declared helper file",
			"allow_create": []string{declared},
		},
	})
	if err != nil {
		t.Fatalf("CallTool fishhawk_fixup_stage: %v", err)
	}
	if result.IsError {
		t.Fatalf("fix-up tool returned error: %s", toolContentString(t, result))
	}

	// 5. The stage_fixup_triggered audit entry persisted the declared path.
	entries, err := auditRepo.ListForRunByCategory(ctx, fx.runID, server.CategoryStageFixupTriggered)
	if err != nil {
		t.Fatalf("ListForRunByCategory: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("stage_fixup_triggered entries = %d, want 1", len(entries))
	}
	var triggered struct {
		AllowCreate []string `json:"allow_create"`
	}
	if err := json.Unmarshal(entries[0].Payload, &triggered); err != nil {
		t.Fatalf("unmarshal stage_fixup_triggered payload: %v", err)
	}
	if len(triggered.AllowCreate) != 1 || triggered.AllowCreate[0] != declared {
		t.Fatalf("persisted allow_create = %v, want [%s]", triggered.AllowCreate, declared)
	}

	// 6. The end-to-end assertion: under #1314 the implement prompt's effective
	// scope.files RETAINS the full approved plan scope and folds the declared
	// allow_create path on top — it CONTAINS the plan-only file (so the agent's
	// in-plan edits ship rather than drift-excluded), CONTAINS the declared
	// allow_create path, and does NOT contain the undeclared sibling.
	scopeFiles := getPromptRenderScopeFiles(t, ctx, httpSrv.URL, implStage.ID)
	inScope := map[string]bool{}
	for _, p := range scopeFiles {
		inScope[p] = true
	}
	if !inScope["backend/internal/server/prompt.go"] {
		t.Errorf("approved-plan file must be RETAINED in the effective scope.files (#1314 full-scope retention): %v", scopeFiles)
	}
	if !inScope[declared] {
		t.Errorf("declared allow_create path %q missing from effective scope.files: %v", declared, scopeFiles)
	}
	if inScope[undeclared] {
		t.Errorf("undeclared path %q leaked into effective scope.files — #818 silent-strip hole reopened: %v", undeclared, scopeFiles)
	}
}

// TestE2E_Fixup_PushOpenPRReopensImplementAndReparksReview drives the
// push_and_open_pr fix-up seam end-to-end (#780): with push_and_open_pr
// the implement stage SUCCEEDS (it opens the PR) and the human gate is a
// SEPARATE review stage at awaiting_approval. A fix-up must re-open the
// succeeded implement stage AND re-park the review stage, both persisted
// through real MCP binary → backend HTTP → Postgres. This is the leg the
// per-layer unit tests can't cover on their own (cf. #618).
func TestE2E_Fixup_PushOpenPRReopensImplementAndReparksReview(t *testing.T) {
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	auditRepo := audit.NewPostgresRepository(fx.pool)

	// 1. Implement stage that SUCCEEDED — the push_and_open_pr shape: it
	// committed and opened the PR, so it is terminal, not at a gate.
	impl, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:            fx.runID,
		Sequence:         1,
		Type:             runpkg.StageTypeImplement,
		ExecutorKind:     runpkg.ExecutorAgent,
		ExecutorRef:      "fishhawk/runner@v1",
		RequiresApproval: false,
	})
	if err != nil {
		t.Fatalf("CreateStage(implement): %v", err)
	}
	walkToSucceeded(t, ctx, fx.runRepo, impl.ID)

	// 2. Separate review stage holding the human gate at awaiting_approval.
	review, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:            fx.runID,
		Sequence:         2,
		Type:             runpkg.StageTypeReview,
		ExecutorKind:     runpkg.ExecutorAgent,
		ExecutorRef:      "fishhawk/runner@v1",
		RequiresApproval: true,
	})
	if err != nil {
		t.Fatalf("CreateStage(review): %v", err)
	}
	parkAtGate(t, ctx, fx.runRepo, review.ID)

	// 3. Record the implement-review concern keyed to the implement stage.
	scopeConcern := planreview.Concern{
		Severity: planreview.SeverityMedium,
		Category: "scope",
		Note:     "guard the new fix-up edge against a missing review stage",
	}
	seedImplementReview(t, ctx, auditRepo, fx.runID, impl.ID, scopeConcern)

	// 4. Trigger the fix-up through the real fishhawk-mcp binary.
	session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, fx.url)
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "fishhawk_fixup_stage",
		Arguments: map[string]any{
			"stage_id": impl.ID.String(),
			"concerns": []int{0},
			"reason":   "address the scope concern on the open PR",
		},
	})
	if err != nil {
		t.Fatalf("CallTool fishhawk_fixup_stage: %v", err)
	}
	if result.IsError {
		t.Fatalf("fix-up tool returned error: %s", toolContentString(t, result))
	}

	// The succeeded implement stage re-opened to pending.
	var fixupOut struct {
		Stage struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"stage"`
	}
	decodeStructured(t, result, &fixupOut)
	if fixupOut.Stage.ID != impl.ID.String() {
		t.Errorf("fix-up stage id = %q, want %s", fixupOut.Stage.ID, impl.ID)
	}
	if fixupOut.Stage.State != string(runpkg.StageStatePending) {
		t.Errorf("fix-up stage state = %q, want pending", fixupOut.Stage.State)
	}

	// The review stage was re-parked awaiting_approval → pending.
	curReview, err := fx.runRepo.GetStage(ctx, review.ID)
	if err != nil {
		t.Fatalf("GetStage(review): %v", err)
	}
	if curReview.State != runpkg.StageStatePending {
		t.Errorf("review state = %q, want pending (re-parked)", curReview.State)
	}

	// The stage_fixup_triggered audit entry landed carrying the selected
	// concern AND the re-parked review stage id (#780 CONDITION 3).
	entries, err := auditRepo.ListForRunByCategory(ctx, fx.runID, server.CategoryStageFixupTriggered)
	if err != nil {
		t.Fatalf("ListForRunByCategory: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("stage_fixup_triggered entries = %d, want 1", len(entries))
	}
	var triggered struct {
		PassOrdinal           int                  `json:"pass_ordinal"`
		PriorState            string               `json:"prior_state"`
		Concerns              []planreview.Concern `json:"concerns"`
		ReparkedReviewStageID string               `json:"reparked_review_stage_id"`
	}
	if err := json.Unmarshal(entries[0].Payload, &triggered); err != nil {
		t.Fatalf("unmarshal stage_fixup_triggered payload: %v", err)
	}
	if triggered.PassOrdinal != 1 {
		t.Errorf("pass_ordinal = %d, want 1", triggered.PassOrdinal)
	}
	if triggered.PriorState != string(runpkg.StageStateSucceeded) {
		t.Errorf("prior_state = %q, want succeeded", triggered.PriorState)
	}
	if triggered.ReparkedReviewStageID != review.ID.String() {
		t.Errorf("reparked_review_stage_id = %q, want %s", triggered.ReparkedReviewStageID, review.ID)
	}
	if len(triggered.Concerns) != 1 || triggered.Concerns[0].Category != "scope" {
		t.Fatalf("persisted concerns = %+v, want the single scope concern", triggered.Concerns)
	}
}

// TestE2E_Fixup_FailedRedispatchRestoresReviewGate drives the fix-up
// FAILURE-RECOVERY seam end to end (#788/#794): a push_fixup fix-up
// re-dispatch that FAILS must restore the run to its pre-fix-up review
// gate rather than leaving it an unrecoverable failed casualty. This
// crosses the run state-machine ↔ backend HTTP ↔ Postgres ↔ audit seam
// that per-layer units cannot (cf. #618): the per-function tests pass
// while the FailStage → maybeRecoverFixupFailure → RestoreFixupStage
// chain across the /pull-request handler could break.
//
// The flow: implement SUCCEEDED + review at the gate → fix-up re-open
// via the real MCP binary (implement → pending, review → pending) →
// simulate the re-dispatched implement FAILING via the backend's
// /pull-request {outcome:"failed"} report → assert the run is restored
// to its review gate (implement succeeded, review awaiting_approval, run
// running, a stage_fixup_recovered audit entry present) so the original
// PR stays mergeable.
func TestE2E_Fixup_FailedRedispatchRestoresReviewGate(t *testing.T) {
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	auditRepo := audit.NewPostgresRepository(fx.pool)

	// Put the run in `running` (the state it holds while awaiting review
	// approval) so the post-recovery "still running, not failed" assertion
	// is meaningful.
	if _, err := fx.runRepo.TransitionRun(ctx, fx.runID, runpkg.StateRunning); err != nil {
		t.Fatalf("TransitionRun → running: %v", err)
	}

	// 1. push_and_open_pr shape: implement SUCCEEDED (PR opened), separate
	// review stage at the gate.
	impl, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:            fx.runID,
		Sequence:         1,
		Type:             runpkg.StageTypeImplement,
		ExecutorKind:     runpkg.ExecutorAgent,
		ExecutorRef:      "fishhawk/runner@v1",
		RequiresApproval: false,
	})
	if err != nil {
		t.Fatalf("CreateStage(implement): %v", err)
	}
	walkToSucceeded(t, ctx, fx.runRepo, impl.ID)

	review, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:            fx.runID,
		Sequence:         2,
		Type:             runpkg.StageTypeReview,
		ExecutorKind:     runpkg.ExecutorAgent,
		ExecutorRef:      "fishhawk/runner@v1",
		RequiresApproval: true,
	})
	if err != nil {
		t.Fatalf("CreateStage(review): %v", err)
	}
	parkAtGate(t, ctx, fx.runRepo, review.ID)

	// 2. Record the implement-review concern and trigger the fix-up through
	// the real fishhawk-mcp binary — re-opens implement → pending, re-parks
	// review → pending, writes stage_fixup_triggered.
	seedImplementReview(t, ctx, auditRepo, fx.runID, impl.ID,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "address the drift"})

	session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, fx.url)
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "fishhawk_fixup_stage",
		Arguments: map[string]any{
			"stage_id": impl.ID.String(),
			"concerns": []int{0},
			"reason":   "address the scope concern on the open PR",
		},
	})
	if err != nil {
		t.Fatalf("CallTool fishhawk_fixup_stage: %v", err)
	}
	if result.IsError {
		t.Fatalf("fix-up tool returned error: %s", toolContentString(t, result))
	}

	// 3. Ship the re-dispatched implement's trace bundle carrying push_fixup —
	// the trace handler walks pending → dispatched → running and the
	// fixupPushGated forward-gate DEFERS the terminal transition (#794). This
	// replaces a manual TransitionStage so the failure leg exercises the real
	// trace gate, not a simulated `running`.
	shipPushFixupTraceViaBackend(t, ctx, fx, impl.ID)

	// CONDITION 2: after the push_fixup trace upload and before any /pull-request
	// report, the fix-up implement stage is `running`, NOT succeeded — proving
	// the terminal transition is forward-gated so a later push failure can still
	// flip it to failed and fire #788 recovery.
	gated, err := fx.runRepo.GetStage(ctx, impl.ID)
	if err != nil {
		t.Fatalf("GetStage(implement) after trace: %v", err)
	}
	if gated.State != runpkg.StageStateRunning {
		t.Fatalf("implement state after push_fixup trace = %q, want running (forward-gated)", gated.State)
	}

	// Now simulate the re-dispatched implement FAILING its commit/push step.
	failPushPRViaBackend(t, ctx, fx, impl.ID)

	// 4. The run was restored to its pre-fix-up review gate — the PR stays
	// mergeable rather than the run becoming a failed casualty.
	curImpl, err := fx.runRepo.GetStage(ctx, impl.ID)
	if err != nil {
		t.Fatalf("GetStage(implement): %v", err)
	}
	if curImpl.State != runpkg.StageStateSucceeded {
		t.Errorf("implement state = %q, want succeeded (restored)", curImpl.State)
	}
	if curImpl.FailureCategory != nil {
		t.Errorf("implement FailureCategory = %v, want nil (cleared on recovery)", curImpl.FailureCategory)
	}
	curReview, err := fx.runRepo.GetStage(ctx, review.ID)
	if err != nil {
		t.Fatalf("GetStage(review): %v", err)
	}
	if curReview.State != runpkg.StageStateAwaitingApproval {
		t.Errorf("review state = %q, want awaiting_approval (restored)", curReview.State)
	}
	curRun, err := fx.runRepo.GetRun(ctx, fx.runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if curRun.State != runpkg.StateRunning {
		t.Errorf("run state = %q, want running (NOT failed)", curRun.State)
	}

	// 5. A stage_fixup_recovered audit entry landed recording the restore.
	recovered, err := auditRepo.ListForRunByCategory(ctx, fx.runID, server.CategoryStageFixupRecovered)
	if err != nil {
		t.Fatalf("ListForRunByCategory(recovered): %v", err)
	}
	if len(recovered) != 1 {
		t.Fatalf("stage_fixup_recovered entries = %d, want 1", len(recovered))
	}
	var payload struct {
		RestoredState         string `json:"restored_state"`
		RestoredReviewStageID string `json:"restored_review_stage_id"`
		SourceFailureCategory string `json:"source_failure_category"`
	}
	if err := json.Unmarshal(recovered[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal stage_fixup_recovered payload: %v", err)
	}
	if payload.RestoredState != string(runpkg.StageStateSucceeded) {
		t.Errorf("restored_state = %q, want succeeded", payload.RestoredState)
	}
	if payload.RestoredReviewStageID != review.ID.String() {
		t.Errorf("restored_review_stage_id = %q, want %s", payload.RestoredReviewStageID, review.ID)
	}
	if payload.SourceFailureCategory != "C" {
		t.Errorf("source_failure_category = %q, want C", payload.SourceFailureCategory)
	}
}

// failPushPRViaBackend POSTs a /pull-request {outcome:"failed"} report for
// the implement stage, signed with the run's signing key. It stands up a
// second backend server on the same Postgres pool with ArtifactRepo wired
// (the shared fixture omits it), since the /pull-request handler requires
// the artifact repo even on the failure variant. Both servers share the
// pool, so the state the recovery writes is visible through fx.runRepo.
// The Orchestrator is wired (as in production) so the post-failure Advance
// path is genuinely exercised — load-bearing for the #968 duplicate-report
// leg, where the old Advance completed the run with its review gate open.
func failPushPRViaBackend(t *testing.T, ctx context.Context, fx *e2eFixture, stageID uuid.UUID) {
	t.Helper()
	s := server.New(server.Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      fx.runRepo,
		SigningRepo:  signing.NewPostgresRepository(fx.pool),
		AuditRepo:    audit.NewPostgresRepository(fx.pool),
		ArtifactRepo: artifact.NewPostgresRepository(fx.pool),
		Orchestrator: &orchestrator.Orchestrator{Runs: fx.runRepo},
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	body := []byte(`{"outcome":"failed","category":"C","reason":"commit/push onto PR branch failed"}`)
	digest := sha256.Sum256(body)
	signature := ed25519.Sign(fx.signingPriv, digest[:])
	url := srv.URL + "/v0/runs/" + fx.runID.String() + "/pull-request?stage_id=" + stageID.String()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build pull-request request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Fishhawk-Signature", hex.EncodeToString(signature))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("pull-request failure POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("pull-request failure status %d: %s", resp.StatusCode, raw)
	}
}

// TestE2E_Fixup_PushFixupForwardGateDrivesTerminal drives the #794 fix-up
// forward-gate SUCCESS seam end to end: a fix-up re-dispatch whose trace bundle
// carries push_fixup must leave the implement stage in `running` at trace-upload
// time (CONDITION 2 — the terminal transition is deferred), and the subsequent
// /pull-request {outcome:"fixup_pushed"} report must drive the stage terminal
// AND write a fixup_pushed audit entry. This crosses the runner-emitted manifest
// flag → backend trace forward-gate → /pull-request terminal-drive seam the
// per-layer units cannot (cf. #618): without the gate the trace upload would
// terminalize the stage at upload time, swallowing a later push failure.
func TestE2E_Fixup_PushFixupForwardGateDrivesTerminal(t *testing.T) {
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	auditRepo := audit.NewPostgresRepository(fx.pool)

	if _, err := fx.runRepo.TransitionRun(ctx, fx.runID, runpkg.StateRunning); err != nil {
		t.Fatalf("TransitionRun → running: %v", err)
	}

	// 1. Fix-up shape: implement SUCCEEDED (PR open), separate review at the gate.
	impl, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:            fx.runID,
		Sequence:         1,
		Type:             runpkg.StageTypeImplement,
		ExecutorKind:     runpkg.ExecutorAgent,
		ExecutorRef:      "fishhawk/runner@v1",
		RequiresApproval: false,
	})
	if err != nil {
		t.Fatalf("CreateStage(implement): %v", err)
	}
	walkToSucceeded(t, ctx, fx.runRepo, impl.ID)

	review, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:            fx.runID,
		Sequence:         2,
		Type:             runpkg.StageTypeReview,
		ExecutorKind:     runpkg.ExecutorAgent,
		ExecutorRef:      "fishhawk/runner@v1",
		RequiresApproval: true,
	})
	if err != nil {
		t.Fatalf("CreateStage(review): %v", err)
	}
	parkAtGate(t, ctx, fx.runRepo, review.ID)

	// 2. Trigger the fix-up through the real fishhawk-mcp binary — re-opens
	// implement → pending, re-parks review → pending.
	seedImplementReview(t, ctx, auditRepo, fx.runID, impl.ID,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "address the drift"})
	session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, fx.url)
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "fishhawk_fixup_stage",
		Arguments: map[string]any{
			"stage_id": impl.ID.String(),
			"concerns": []int{0},
			"reason":   "address the scope concern on the open PR",
		},
	})
	if err != nil {
		t.Fatalf("CallTool fishhawk_fixup_stage: %v", err)
	}
	if result.IsError {
		t.Fatalf("fix-up tool returned error: %s", toolContentString(t, result))
	}

	// 3. Ship the re-dispatched implement's trace bundle carrying push_fixup.
	// The trace handler walks pending → dispatched → running, then the
	// fixupPushGated forward-gate DEFERS the terminal transition.
	shipPushFixupTraceViaBackend(t, ctx, fx, impl.ID)

	// CONDITION 2: immediately after the push_fixup trace upload and BEFORE any
	// /pull-request report, the fix-up implement stage is `running`, NOT
	// succeeded — the terminal transition is forward-gated.
	curImpl, err := fx.runRepo.GetStage(ctx, impl.ID)
	if err != nil {
		t.Fatalf("GetStage(implement): %v", err)
	}
	if curImpl.State != runpkg.StageStateRunning {
		t.Fatalf("implement state after push_fixup trace = %q, want running (terminal transition must be forward-gated)", curImpl.State)
	}

	// 4. The /pull-request {fixup_pushed} report drives the deferred terminal
	// transition. RequiresApproval=false → succeeded. The report carries the
	// #1165/#1213 deterministic-apply provenance, crossing the runner-wire →
	// backend decode → audit-persist boundary the per-layer units cannot.
	succeedFixupPushViaBackend(t, ctx, fx, impl.ID, "applied")

	curImpl, err = fx.runRepo.GetStage(ctx, impl.ID)
	if err != nil {
		t.Fatalf("GetStage(implement) after report: %v", err)
	}
	if curImpl.State != runpkg.StageStateSucceeded {
		t.Errorf("implement state after fixup_pushed = %q, want succeeded (report drives the gated terminal transition)", curImpl.State)
	}

	// 5. A fixup_pushed audit entry landed pinning the pushed commit AND carrying
	// the apply_path provenance the report threaded end to end.
	pushed, err := auditRepo.ListForRunByCategory(ctx, fx.runID, "fixup_pushed")
	if err != nil {
		t.Fatalf("ListForRunByCategory(fixup_pushed): %v", err)
	}
	if len(pushed) != 1 {
		t.Fatalf("fixup_pushed entries = %d, want 1", len(pushed))
	}
	var pushedPayload map[string]any
	if err := json.Unmarshal(pushed[0].Payload, &pushedPayload); err != nil {
		t.Fatalf("unmarshal fixup_pushed payload: %v", err)
	}
	if pushedPayload["apply_path"] != "applied" {
		t.Errorf("fixup_pushed apply_path = %v, want applied (provenance must survive wire → decode → persist)", pushedPayload["apply_path"])
	}
}

// buildPushFixupBundle returns a gzipped JSONL trace bundle whose manifest
// carries push_fixup=true plus a git_diff event with fileCount files — the
// signed wire form the trace handler's fixupPushGated check reads (#794).
// Mirrors the per-layer makeFixupPushBundle helper in the server package; kept
// here because the integration test is in a different package.
func buildPushFixupBundle(t *testing.T, fileCount int) []byte {
	t.Helper()
	type line struct {
		Seq  int             `json:"seq"`
		TS   time.Time       `json:"ts"`
		Kind string          `json:"kind"`
		Data json.RawMessage `json:"data,omitempty"`
	}
	t0 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(5 * time.Minute)
	mdata, err := json.Marshal(bundle.Manifest{BundleSchema: "v1", PushFixup: true})
	if err != nil {
		t.Fatal(err)
	}
	files := make([]map[string]string, 0, fileCount)
	for i := 0; i < fileCount; i++ {
		files = append(files, map[string]string{"path": fmt.Sprintf("file%d.go", i), "status": "modified"})
	}
	diffData, err := json.Marshal(map[string]any{
		"kind": "git_diff", "base_ref": "main", "files": files, "num_files": fileCount,
	})
	if err != nil {
		t.Fatal(err)
	}
	lines := []line{
		{Seq: 1, TS: t0.Add(-time.Second), Kind: bundle.EventKindManifest, Data: mdata},
		{Seq: 2, TS: t0, Kind: bundle.EventKindGitDiff, Data: diffData},
		{Seq: 3, TS: t1, Kind: "agent_end", Data: json.RawMessage(`{}`)},
		{Seq: 4, TS: t1.Add(time.Second), Kind: "trailer", Data: json.RawMessage(`{}`)},
	}
	var raw bytes.Buffer
	for _, l := range lines {
		b, err := json.Marshal(l)
		if err != nil {
			t.Fatal(err)
		}
		raw.Write(b)
		raw.WriteByte('\n')
	}
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	if _, err := w.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return gz.Bytes()
}

// shipPushFixupTraceViaBackend POSTs a signed push_fixup trace bundle for the
// implement stage, standing up a backend server with TraceStore wired (the
// shared fixture omits it). Both servers share the pool, so the state the trace
// handler writes is visible through fx.runRepo.
func shipPushFixupTraceViaBackend(t *testing.T, ctx context.Context, fx *e2eFixture, stageID uuid.UUID) {
	t.Helper()
	s := server.New(server.Config{
		Addr:        "127.0.0.1:0",
		RunRepo:     fx.runRepo,
		SigningRepo: signing.NewPostgresRepository(fx.pool),
		AuditRepo:   audit.NewPostgresRepository(fx.pool),
		TraceStore:  tracestore.NewMem(),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	body := buildPushFixupBundle(t, 2)
	message := signing.ComputeMessage(body)
	signature := ed25519.Sign(fx.signingPriv, message)
	url := srv.URL + "/v0/runs/" + fx.runID.String() + "/trace?stage_id=" + stageID.String() + "&variant=raw"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build trace request: %v", err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Fishhawk-Signature", hex.EncodeToString(signature))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("trace POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("trace status %d: %s", resp.StatusCode, raw)
	}
}

// succeedFixupPushViaBackend POSTs a /pull-request {outcome:"fixup_pushed"}
// report for the implement stage, signed with the run's signing key. Mirrors
// failPushPRViaBackend's server wiring. An optional applyPath (#1165/#1213) is
// threaded onto the report body when non-empty, crossing the runner-wire →
// backend DisallowUnknownFields decode → fixup_pushed audit persist boundary.
func succeedFixupPushViaBackend(t *testing.T, ctx context.Context, fx *e2eFixture, stageID uuid.UUID, applyPath ...string) {
	t.Helper()
	s := server.New(server.Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      fx.runRepo,
		SigningRepo:  signing.NewPostgresRepository(fx.pool),
		AuditRepo:    audit.NewPostgresRepository(fx.pool),
		ArtifactRepo: artifact.NewPostgresRepository(fx.pool),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	bodyFields := map[string]any{
		"outcome":             "fixup_pushed",
		"branch":              "fishhawk/fixup-branch",
		"head_sha":            "head-abc",
		"base_sha":            "base-def",
		"files_changed_count": 2,
	}
	if len(applyPath) > 0 && applyPath[0] != "" {
		bodyFields["apply_path"] = applyPath[0]
	}
	body, err := json.Marshal(bodyFields)
	if err != nil {
		t.Fatalf("marshal fixup_pushed body: %v", err)
	}
	digest := sha256.Sum256(body)
	signature := ed25519.Sign(fx.signingPriv, digest[:])
	url := srv.URL + "/v0/runs/" + fx.runID.String() + "/pull-request?stage_id=" + stageID.String()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build pull-request request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Fishhawk-Signature", hex.EncodeToString(signature))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("fixup_pushed POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("fixup_pushed status %d: %s", resp.StatusCode, raw)
	}
}

// TestE2E_Fixup_ExpectedHeadAdvertisedAndNoChangeRefund drives the #967
// seams end to end through the real HTTP server, crossing the backend wire
// payload → runner consumption → budget-refund layers the per-layer units
// cannot (cf. #618):
//
//   - (a) after a fix-up trigger, the stage prompt payload carries
//     fixup=true, fixup_branch, AND fixup_expected_head_sha equal to the
//     run's recorded run-authored head (the NEWEST reported head across
//     the lineage-ledger audit categories) — the value the runner's
//     pre-invoke base establishment compares the fetched branch tip
//     against;
//   - (b) a {outcome:"fixup_no_changes"} /pull-request report refunds the
//     spent pass against the NORMAL budget (a second trigger is admitted
//     without force_additional_pass, with refunded_passes recorded on the
//     audit payload), while the absolute 3-pass ceiling keeps counting RAW
//     triggered passes and still rejects the pass beyond it.
func TestE2E_Fixup_ExpectedHeadAdvertisedAndNoChangeRefund(t *testing.T) {
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	auditRepo := audit.NewPostgresRepository(fx.pool)
	signingRepo := signing.NewPostgresRepository(fx.pool)

	// GitHub-wired sibling server over the same pool so prompt-render
	// serves (same shape as TestE2E_Fixup_ConcernRoutedBackAndBounded).
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

	// 1. Implement stage parked at the review gate, with a recorded
	// implement-review concern to route back.
	stage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:            fx.runID,
		Sequence:         1,
		Type:             runpkg.StageTypeImplement,
		ExecutorKind:     runpkg.ExecutorAgent,
		ExecutorRef:      "fishhawk/runner@v1",
		RequiresApproval: true,
	})
	if err != nil {
		t.Fatalf("CreateStage: %v", err)
	}
	parkAtGate(t, ctx, fx.runRepo, stage.ID)
	seedImplementReview(t, ctx, auditRepo, fx.runID, stage.ID,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "address the drift"})

	// 2. Seed the reported-head ledger: an older PR-open head, then a NEWER
	// pushed head — the recorded run-authored head the prompt must advertise.
	const olderHead = "1111111111111111111111111111111111111111"
	const recordedHead = "2222222222222222222222222222222222222222"
	seedReportedHead(t, ctx, auditRepo, fx.runID, stage.ID, "pull_request_opened", olderHead, time.Now().Add(-2*time.Hour).UTC())
	seedReportedHead(t, ctx, auditRepo, fx.runID, stage.ID, "fixup_pushed", recordedHead, time.Now().Add(-1*time.Hour).UTC())

	// 3. Trigger the fix-up through the real fishhawk-mcp binary.
	session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, httpSrv.URL)
	callFixup := func(force bool) *mcp.CallToolResult {
		t.Helper()
		if cur, err := fx.runRepo.GetStage(ctx, stage.ID); err != nil {
			t.Fatalf("GetStage: %v", err)
		} else if cur.State != runpkg.StageStateAwaitingApproval {
			parkAtGate(t, ctx, fx.runRepo, stage.ID)
		}
		args := map[string]any{
			"stage_id": stage.ID.String(),
			"concerns": []int{0},
		}
		if force {
			args["force_additional_pass"] = true
		}
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "fishhawk_fixup_stage", Arguments: args})
		if err != nil {
			t.Fatalf("CallTool fishhawk_fixup_stage: %v", err)
		}
		return res
	}
	if res := callFixup(false); res.IsError {
		t.Fatalf("first fix-up returned error: %s", toolContentString(t, res))
	}

	// 4. Seam (a): the prompt payload advertises the fix-up routing AND the
	// recorded head — the value the runner's pre-invoke base establishment
	// verifies the fetched fixup_branch tip against (ADR-035).
	var promptOut struct {
		Fixup                bool   `json:"fixup"`
		FixupBranch          string `json:"fixup_branch"`
		FixupExpectedHeadSHA string `json:"fixup_expected_head_sha"`
	}
	getPromptRenderJSON(t, ctx, httpSrv.URL, stage.ID, &promptOut)
	if !promptOut.Fixup {
		t.Error("prompt fixup = false, want true after the fix-up trigger")
	}
	wantBranch := "fishhawk/run-" + fx.runID.String()[:8] + "/stage-" + stage.ID.String()[:8]
	if promptOut.FixupBranch != wantBranch {
		t.Errorf("prompt fixup_branch = %q, want %q", promptOut.FixupBranch, wantBranch)
	}
	if promptOut.FixupExpectedHeadSHA != recordedHead {
		t.Errorf("prompt fixup_expected_head_sha = %q, want the NEWEST recorded head %q (not the older %q)",
			promptOut.FixupExpectedHeadSHA, recordedHead, olderHead)
	}

	// 5. Seam (b): the re-dispatch produced no commit — ship the
	// {outcome:"fixup_no_changes"} report through the real /pull-request
	// handler, which writes the fixup_no_changes audit entry the refund
	// counts.
	reportFixupNoChangesViaBackend(t, ctx, fx, stage.ID, wantBranch)

	// 6. A second fix-up WITHOUT force is admitted — the no-change pass was
	// refunded against the normal budget — and the audit receipt records it.
	if res := callFixup(false); res.IsError {
		t.Fatalf("refunded second fix-up returned error: %s", toolContentString(t, res))
	}
	entries, err := auditRepo.ListForRunByCategory(ctx, fx.runID, server.CategoryStageFixupTriggered)
	if err != nil {
		t.Fatalf("ListForRunByCategory: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("stage_fixup_triggered entries = %d, want 2 after the refunded pass", len(entries))
	}
	var refunded struct {
		RefundedPasses int  `json:"refunded_passes"`
		Forced         bool `json:"forced"`
	}
	if err := json.Unmarshal(entries[len(entries)-1].Payload, &refunded); err != nil {
		t.Fatalf("unmarshal refunded payload: %v", err)
	}
	if refunded.RefundedPasses != 1 {
		t.Errorf("refunded_passes = %d, want 1", refunded.RefundedPasses)
	}
	if refunded.Forced {
		t.Error("refund-admitted pass audited as forced; want a normal-budget pass")
	}

	// 7. The ceiling counts RAW passes: a third pass (forced — the refund
	// is consumed) reaches 3 raw passes, and the next attempt is rejected
	// with the DISTINCT ceiling error even when forced.
	if res := callFixup(true); res.IsError {
		t.Fatalf("third fix-up returned error: %s", toolContentString(t, res))
	}
	ceil := callFixup(true)
	if !ceil.IsError {
		t.Fatalf("fix-up beyond the raw ceiling unexpectedly succeeded; want fixup_ceiling_reached")
	}
	if body := toolContentString(t, ceil); !strings.Contains(body, "fixup_ceiling_reached") {
		t.Errorf("ceiling fix-up error missing fixup_ceiling_reached code: %s", body)
	}
}

// seedReportedHead appends a reported-head ledger audit entry
// (pull_request_opened / child_pushed / fixup_pushed) carrying a head_sha
// at the given timestamp — the source resolveFixupExpectedHeadSHA reads.
func seedReportedHead(t *testing.T, ctx context.Context, repo audit.Repository, runID, stageID uuid.UUID, category, headSHA string, ts time.Time) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"head_sha": headSHA})
	if err != nil {
		t.Fatalf("marshal %s payload: %v", category, err)
	}
	kind := audit.ActorKind("agent")
	if _, err := repo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: ts,
		Category:  category,
		ActorKind: &kind,
		Payload:   payload,
	}); err != nil {
		t.Fatalf("AppendChained %s: %v", category, err)
	}
}

// getPromptRenderJSON fetches GET /v0/stages/{id}/prompt-render and decodes
// the full response body into out, so tests can assert wire fields beyond
// the prompt text (fixup routing, expected head).
func getPromptRenderJSON(t *testing.T, ctx context.Context, baseURL string, stageID uuid.UUID, out any) {
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
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("decode prompt-render response: %v", err)
	}
}

// getRunJSON GETs /v0/runs/{id} with the operator bearer token and decodes the
// run-status response into out — the run-status leg of the #1164 fix-up model
// seam assertion.
func getRunJSON(t *testing.T, ctx context.Context, baseURL, token string, runID uuid.UUID, out any) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		baseURL+"/v0/runs/"+runID.String(), nil)
	if err != nil {
		t.Fatalf("build run-status request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("run-status request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("run-status status %d: %s", resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("decode run-status response: %v", err)
	}
}

// reportFixupNoChangesViaBackend POSTs a /pull-request
// {outcome:"fixup_no_changes"} report for the implement stage, signed with
// the run's signing key — the real #856 report path that writes the
// fixup_no_changes audit entry the #967 budget refund counts.
func reportFixupNoChangesViaBackend(t *testing.T, ctx context.Context, fx *e2eFixture, stageID uuid.UUID, branch string) {
	t.Helper()
	s := server.New(server.Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      fx.runRepo,
		SigningRepo:  signing.NewPostgresRepository(fx.pool),
		AuditRepo:    audit.NewPostgresRepository(fx.pool),
		ArtifactRepo: artifact.NewPostgresRepository(fx.pool),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	body, err := json.Marshal(map[string]any{
		"outcome":  "fixup_no_changes",
		"branch":   branch,
		"base_sha": "2222222222222222222222222222222222222222",
	})
	if err != nil {
		t.Fatalf("marshal fixup_no_changes body: %v", err)
	}
	digest := sha256.Sum256(body)
	signature := ed25519.Sign(fx.signingPriv, digest[:])
	url := srv.URL + "/v0/runs/" + fx.runID.String() + "/pull-request?stage_id=" + stageID.String()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build pull-request request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Fishhawk-Signature", hex.EncodeToString(signature))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("fixup_no_changes POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("fixup_no_changes status %d: %s", resp.StatusCode, raw)
	}
}

// TestE2E_Fixup_ReviewActionHintSurfacesAndOverride drives the
// review-action-hint + operator-override seam end to end (#777, #860): the
// audit-persistence → hint-compute → MCP-response and the MCP-input → HTTP →
// run-state-machine → audit layers in one test (cf. #618), which the
// per-function units can't express. An implement-review approve_with_concerns
// verdict in Postgres must surface as review_action_hint on
// fishhawk_get_run_status; once a fix-up pass spends the NORMAL budget the
// hint must NOT suppress (direction D) but report OverrideAvailable=true; the
// operator override (force_additional_pass=true) must be admitted and audited
// as forced; and at the hard ceiling fishhawk_fixup_stage must return the
// distinct fixup_ceiling_reached error. The ceiling assertion is the guard
// against a silently-drifted fixupCeiling mirror.
func TestE2E_Fixup_ReviewActionHintSurfacesAndOverride(t *testing.T) {
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	auditRepo := audit.NewPostgresRepository(fx.pool)

	// 1. Implement stage parked at the review gate (a valid fix-up candidate).
	stage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:            fx.runID,
		Sequence:         1,
		Type:             runpkg.StageTypeImplement,
		ExecutorKind:     runpkg.ExecutorAgent,
		ExecutorRef:      "fishhawk/runner@v1",
		RequiresApproval: true,
	})
	if err != nil {
		t.Fatalf("CreateStage: %v", err)
	}
	parkAtGate(t, ctx, fx.runRepo, stage.ID)

	// 2. Record an implement-review approve_with_concerns verdict with two
	// concerns keyed to the stage.
	seedImplementReview(t, ctx, auditRepo, fx.runID, stage.ID,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "guard the nil stage"},
		planreview.Concern{Severity: planreview.SeverityLow, Category: "style", Note: "rename the var"})

	session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, fx.url)

	// 3. fishhawk_get_run_status surfaces the hint: two concerns, full budget.
	hint := getReviewActionHint(t, ctx, session, fx.runID)
	if hint == nil {
		t.Fatalf("review_action_hint absent; want a populated hint with the recorded concerns")
	}
	if hint.Concerns != 2 {
		t.Errorf("review_action_hint.concerns = %d, want 2", hint.Concerns)
	}
	if hint.RemainingFixupBudget != 1 {
		t.Errorf("review_action_hint.remaining_fixup_budget = %d, want 1", hint.RemainingFixupBudget)
	}
	if !strings.Contains(hint.Message, "fishhawk_fixup_stage") {
		t.Errorf("review_action_hint.message should point at fishhawk_fixup_stage; got %q", hint.Message)
	}

	// #1024: the next_actions concern arm derives FROM the same hint
	// computation, so the two surfaces must agree on the remaining-budget
	// number before the fix-up pass.
	na := getNextActions(t, ctx, session, fx.runID)
	if na == nil || na.State != "implement_concerns_open" {
		t.Fatalf("next_actions = %+v, want state implement_concerns_open alongside the hint", na)
	}
	fixupAction := na.Actions[0]
	if fixupAction.Action != "fishhawk_fixup_stage" || fixupAction.Consumes != "fixup_budget" {
		t.Fatalf("actions[0] = %+v, want fishhawk_fixup_stage consuming fixup_budget below budget", fixupAction)
	}
	if want := fmt.Sprintf("%d normal fix-up pass(es) remain", hint.RemainingFixupBudget); !strings.Contains(fixupAction.Reason, want) {
		t.Errorf("next_actions fixup reason %q disagrees with review_action_hint remaining budget %d", fixupAction.Reason, hint.RemainingFixupBudget)
	}
	if fixupAction.Params["stage_id"] != stage.ID.String() {
		t.Errorf("fixup action params.stage_id = %q, want %s", fixupAction.Params["stage_id"], stage.ID)
	}

	// callFixup drives one fix-up through the real MCP binary; force toggles
	// the bounded operator override (#860). Re-parks the stage at the gate
	// first so only the budget/ceiling decision gates the pass.
	callFixup := func(force bool) *mcp.CallToolResult {
		t.Helper()
		// A prior fix-up leaves the stage in pending; re-park it at the gate
		// so only the budget/ceiling decision gates the pass. On the first
		// call the stage is already at the gate, so skip the (invalid) walk.
		if cur, err := fx.runRepo.GetStage(ctx, stage.ID); err != nil {
			t.Fatalf("GetStage: %v", err)
		} else if cur.State != runpkg.StageStateAwaitingApproval {
			parkAtGate(t, ctx, fx.runRepo, stage.ID)
		}
		args := map[string]any{
			"stage_id": stage.ID.String(),
			"concerns": []int{0},
			"reason":   "address the scope concern",
		}
		if force {
			args["force_additional_pass"] = true
		}
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "fishhawk_fixup_stage", Arguments: args})
		if err != nil {
			t.Fatalf("CallTool fishhawk_fixup_stage: %v", err)
		}
		return res
	}

	// 4. Spend the NORMAL fix-up budget via the real fix-up tool — one
	// stage_fixup_triggered entry lands in Postgres.
	if res := callFixup(false); res.IsError {
		t.Fatalf("first fix-up returned error: %s", toolContentString(t, res))
	}
	entries, err := auditRepo.ListForRunByCategory(ctx, fx.runID, server.CategoryStageFixupTriggered)
	if err != nil {
		t.Fatalf("ListForRunByCategory: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("stage_fixup_triggered entries = %d, want exactly 1", len(entries))
	}

	// 5. Direction D: the re-review lands a fresh concern AFTER the fix-up.
	// With the normal budget spent but the hard ceiling still open, the hint
	// must NOT suppress — it surfaces the exhaustion with OverrideAvailable.
	// Re-park the stage at its gate first (the shape the redispatched
	// fix-up's re-review leaves): the post-trigger `pending` interlude is a
	// dispatch state, not a concern state, and next_actions classifies it
	// as such.
	parkAtGate(t, ctx, fx.runRepo, stage.ID)
	seedImplementReview(t, ctx, auditRepo, fx.runID, stage.ID,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "the re-review still sees drift"})
	hint = getReviewActionHint(t, ctx, session, fx.runID)
	if hint == nil {
		t.Fatalf("review_action_hint suppressed after budget spent; want it to surface with the override option (direction D)")
	}
	if hint.RemainingFixupBudget != 0 {
		t.Errorf("review_action_hint.remaining_fixup_budget = %d, want 0 (budget spent)", hint.RemainingFixupBudget)
	}
	if !hint.OverrideAvailable {
		t.Errorf("review_action_hint.override_available = false, want true (below the hard ceiling)")
	}

	// #1024 agreement, post-fix-up: with the normal budget spent and the
	// override open, next_actions must mirror the SAME hint state — the
	// forced fixup option plus merge-with-follow-up, no normal-budget pass.
	na = getNextActions(t, ctx, session, fx.runID)
	if na == nil || na.State != "implement_concerns_open" {
		t.Fatalf("post-fix-up next_actions = %+v, want state implement_concerns_open", na)
	}
	var sawForced, sawMergeFollowUp bool
	for _, a := range na.Actions {
		if a.Action == "fishhawk_fixup_stage" {
			if a.Params["force_additional_pass"] != "true" {
				t.Errorf("budget-spent fixup action must carry force_additional_pass=true; params = %v", a.Params)
			}
			sawForced = true
		}
		if a.Action == "merge_and_file_follow_up" {
			sawMergeFollowUp = true
		}
	}
	if !sawForced || !sawMergeFollowUp {
		t.Errorf("post-budget next_actions = %+v, want the forced-override fixup AND merge_and_file_follow_up (agreeing with override_available=%v)", na.Actions, hint.OverrideAvailable)
	}

	// 6. The operator override admits ONE pass beyond the budget, audited as
	// forced (#860).
	if res := callFixup(true); res.IsError {
		t.Fatalf("forced fix-up returned error: %s", toolContentString(t, res))
	}
	entries, err = auditRepo.ListForRunByCategory(ctx, fx.runID, server.CategoryStageFixupTriggered)
	if err != nil {
		t.Fatalf("ListForRunByCategory (post-override): %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("stage_fixup_triggered entries = %d, want 2 after the override", len(entries))
	}
	var forcedPayload struct {
		Forced bool `json:"forced"`
	}
	if err := json.Unmarshal(entries[len(entries)-1].Payload, &forcedPayload); err != nil {
		t.Fatalf("unmarshal forced payload: %v", err)
	}
	if !forcedPayload.Forced {
		t.Errorf("override fix-up audit forced = false, want true")
	}

	// 7. A third (forced) pass consumes the hard ceiling (3 total), then the
	// next attempt is refused with the DISTINCT fixup_ceiling_reached error —
	// the guard against a drifted fixupCeiling mirror.
	if res := callFixup(true); res.IsError {
		t.Fatalf("third fix-up returned error: %s", toolContentString(t, res))
	}
	ceil := callFixup(true)
	if !ceil.IsError {
		t.Fatalf("fix-up at the ceiling unexpectedly succeeded; want fixup_ceiling_reached")
	}
	if body := toolContentString(t, ceil); !strings.Contains(body, "fixup_ceiling_reached") {
		t.Errorf("ceiling fix-up error missing fixup_ceiling_reached code: %s", body)
	}
}

// TestE2E_Fixup_NoChangeRefundRestoresHintBudget drives the #1150 seam end to
// end: the MCP review-action hint must mirror the backend's no-change fix-up
// refund (#967). The per-layer units can't cover the seam (cf. #618) — it lives
// between the backend's refund accounting (handleFixupStage /
// countFixupNoChangeRefunds, which widens MaxPasses so a refunded normal pass is
// admissible WITHOUT force_additional_pass) and the INDEPENDENT MCP surface
// (reviewActionHintFor, which before #1150 derived RemainingFixupBudget from the
// RAW stage_fixup_triggered count alone). Without the fix the operator saw
// remaining_budget=0 + a forced-override route after a verified no-op pass while
// the backend would actually admit a normal pass — the perceived wedge in run
// a4cfd41b.
//
// Flow: implement stage at the gate with an approve_with_concerns verdict → one
// real fix-up pass (spends the normal budget, one stage_fixup_triggered) → a
// {outcome:"fixup_no_changes"} /pull-request report (writes the fixup_no_changes
// audit entry the refund counts) → a fresh round-2 implement_reviewed concern
// after the fix-up boundary → assert get_run_status's review_action_hint reports
// RemainingFixupBudget==1 and OverrideAvailable==false, proving the surface
// agrees with the backend's admit-a-normal-pass decision rather than reporting a
// spent budget + forced override.
func TestE2E_Fixup_NoChangeRefundRestoresHintBudget(t *testing.T) {
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	auditRepo := audit.NewPostgresRepository(fx.pool)

	// 1. Implement stage parked at the review gate (a valid fix-up candidate).
	stage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:            fx.runID,
		Sequence:         1,
		Type:             runpkg.StageTypeImplement,
		ExecutorKind:     runpkg.ExecutorAgent,
		ExecutorRef:      "fishhawk/runner@v1",
		RequiresApproval: true,
	})
	if err != nil {
		t.Fatalf("CreateStage: %v", err)
	}
	parkAtGate(t, ctx, fx.runRepo, stage.ID)

	// 2. Record an implement-review approve_with_concerns verdict keyed to the
	// stage — the round-1 concern routed back by the fix-up.
	seedImplementReview(t, ctx, auditRepo, fx.runID, stage.ID,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "address the drift"})

	session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, fx.url)

	// 3. Spend the NORMAL fix-up budget via the real fix-up tool — one
	// stage_fixup_triggered entry lands in Postgres.
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "fishhawk_fixup_stage",
		Arguments: map[string]any{
			"stage_id": stage.ID.String(),
			"concerns": []int{0},
			"reason":   "route the concern back onto the branch",
		},
	})
	if err != nil {
		t.Fatalf("CallTool fishhawk_fixup_stage: %v", err)
	}
	if res.IsError {
		t.Fatalf("fix-up tool returned error: %s", toolContentString(t, res))
	}

	// 4. The re-dispatch produced no commit — ship the
	// {outcome:"fixup_no_changes"} report through the real /pull-request
	// handler, which writes the fixup_no_changes audit entry the #967 refund
	// (and the #1150 hint mirror) count.
	branch := "fishhawk/run-" + fx.runID.String()[:8] + "/stage-" + stage.ID.String()[:8]
	reportFixupNoChangesViaBackend(t, ctx, fx, stage.ID, branch)

	// 5. The re-review of the fix-up head lands a fresh concern AFTER the
	// fix-up boundary (re-park the gate first, the shape the re-review leaves).
	// This both restores the implement review to 'complete' and supplies the
	// round-scoped concern the hint surfaces.
	parkAtGate(t, ctx, fx.runRepo, stage.ID)
	seedImplementReview(t, ctx, auditRepo, fx.runID, stage.ID,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "the re-review still sees drift"})

	// 6. The hint mirrors the backend's refund: the no-change pass was refunded
	// against the NORMAL budget, so a normal route-back is restored —
	// RemainingFixupBudget==1 and OverrideAvailable==false, NOT remaining=0 +
	// forced override (the pre-#1150 wedge surface).
	hint := getReviewActionHint(t, ctx, session, fx.runID)
	if hint == nil {
		t.Fatalf("review_action_hint absent after the refunded no-change pass; want a populated hint")
	}
	if hint.RemainingFixupBudget != 1 {
		t.Errorf("review_action_hint.remaining_fixup_budget = %d, want 1 (the no-change pass was refunded against the normal budget — the surface must agree with the backend's admit-a-normal-pass decision)", hint.RemainingFixupBudget)
	}
	if hint.OverrideAvailable {
		t.Errorf("review_action_hint.override_available = true, want false (a normal pass is available after the refund — no forced override needed)")
	}
	if !strings.Contains(hint.Message, "fishhawk_fixup_stage") {
		t.Errorf("review_action_hint.message should point at fishhawk_fixup_stage; got %q", hint.Message)
	}

	// #1024 agreement: the next_actions concern arm derives FROM the same hint
	// value, so the two surfaces must agree — a below-budget normal fixup
	// (consuming fixup_budget), not a forced override.
	na := getNextActions(t, ctx, session, fx.runID)
	if na == nil || na.State != "implement_concerns_open" {
		t.Fatalf("next_actions = %+v, want state implement_concerns_open alongside the refunded hint", na)
	}
	fixupAction := na.Actions[0]
	if fixupAction.Action != "fishhawk_fixup_stage" || fixupAction.Consumes != "fixup_budget" {
		t.Fatalf("actions[0] = %+v, want a below-budget fishhawk_fixup_stage consuming fixup_budget", fixupAction)
	}
	if _, forced := fixupAction.Params["force_additional_pass"]; forced {
		t.Errorf("refunded-budget fixup action must NOT carry force_additional_pass; params = %v", fixupAction.Params)
	}
}

// TestE2E_Fixup_AwaitReviewWaitsForReReviewOfFixupHead drives the #894 seam
// end to end: fishhawk_await_review (and the get_run_status
// implement_review_status it shares) must NOT report the PRE-fix-up review as
// 'complete' once a fix-up re-opens the implement stage — it must wait for the
// re-review of the fix-up head. This crosses the HTTP audit endpoint →
// reviewStatusFor fix-up-boundary derivation → fishhawk_await_review /
// get_run_status seam (cf. #618), and is MANDATORY because the fix anchors on
// the real Postgres audit store's monotonic per-run Sequence: a fix-up entry
// written after the round-1 verdict always carries a HIGHER sequence, so the
// flooring drops the stale verdict. An in-process fake assigns sequences by
// append order and cannot fully vouch for that property.
//
// Flow: implement stage at the gate, round-1 implement_review_started +
// implement_reviewed(approve_with_concerns) in Postgres → a real
// fishhawk_fixup_stage call (writes stage_fixup_triggered at a higher
// sequence) → assert get_run_status.implement_review_status == 'pending' and
// fishhawk_await_review blocks to its timeout returning 'pending' (NOT an
// instant stale 'complete') → land a round-2 implement_reviewed(approve) →
// assert it resolves 'complete' carrying ONLY the round-2 verdict.
func TestE2E_Fixup_AwaitReviewWaitsForReReviewOfFixupHead(t *testing.T) {
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	auditRepo := audit.NewPostgresRepository(fx.pool)

	// 1. Implement stage parked at the review gate (a valid fix-up candidate).
	stage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:            fx.runID,
		Sequence:         1,
		Type:             runpkg.StageTypeImplement,
		ExecutorKind:     runpkg.ExecutorAgent,
		ExecutorRef:      "fishhawk/runner@v1",
		RequiresApproval: true,
	})
	if err != nil {
		t.Fatalf("CreateStage: %v", err)
	}
	parkAtGate(t, ctx, fx.runRepo, stage.ID)

	// 2. Round 1: a review was dispatched (started proxy) and landed an
	// approve_with_concerns verdict. The started entry is what keeps the
	// post-fix-up status 'pending' (unfloored) rather than 'none'.
	seedImplementReviewStarted(t, ctx, auditRepo, fx.runID, stage.ID)
	seedImplementReview(t, ctx, auditRepo, fx.runID, stage.ID,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "round-1 stale concern"})

	session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, fx.url)

	// Sanity: before any fix-up the implement review reads 'complete' with the
	// round-1 verdict — the pre-fix-up state the bug would wrongly preserve.
	if st := getImplementReviewStatus(t, ctx, session, fx.runID); st == nil || st.Status != "complete" {
		t.Fatalf("pre-fix-up implement_review_status = %+v, want complete", st)
	}

	// 3. Trigger a real fix-up through the MCP binary. This writes a
	// stage_fixup_triggered audit entry whose Postgres-assigned Sequence is
	// strictly higher than the round-1 implement_reviewed — the boundary the
	// fix floors the terminal-verdict reads to.
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "fishhawk_fixup_stage",
		Arguments: map[string]any{
			"stage_id": stage.ID.String(),
			"concerns": []int{0},
			"reason":   "route the round-1 concern back onto the branch",
		},
	})
	if err != nil {
		t.Fatalf("CallTool fishhawk_fixup_stage: %v", err)
	}
	if res.IsError {
		t.Fatalf("fix-up tool returned error: %s", toolContentString(t, res))
	}

	// 4. get_run_status.implement_review_status now reads 'pending' — the
	// stale round-1 verdict is floored out and the re-review of the fix-up
	// head has not landed. Before the fix this read 'complete'.
	st := getImplementReviewStatus(t, ctx, session, fx.runID)
	if st == nil || st.Status != "pending" {
		t.Fatalf("post-fix-up implement_review_status = %+v, want pending (the stale verdict must not read complete)", st)
	}
	if len(st.Reviews) != 0 {
		t.Errorf("post-fix-up reviews = %+v, want empty while the re-review is in flight", st.Reviews)
	}

	// 5. fishhawk_await_review must BLOCK to its timeout and return 'pending'
	// rather than resolving instantly with the stale 'complete'. A short
	// timeout keeps the test fast; the assertion is that it waited ~timeout
	// (did not short-circuit) AND returned pending.
	const awaitTimeout = 2
	awaitStart := time.Now()
	awaitOut := callAwaitReview(t, ctx, session, fx.runID, awaitTimeout)
	elapsed := time.Since(awaitStart)
	if awaitOut.Status != "pending" {
		t.Fatalf("await_review status = %q, want pending (must not return the stale complete)", awaitOut.Status)
	}
	if elapsed < time.Duration(awaitTimeout)*time.Second/2 {
		t.Errorf("await_review returned after %s — it short-circuited instead of blocking on pending", elapsed)
	}

	// 6. Round 2: the re-review of the fix-up head lands a clean approve at a
	// sequence above the fix-up boundary.
	seedImplementReviewVerdict(t, ctx, auditRepo, fx.runID, stage.ID, planreview.ImplementReviewedPayload{
		ReviewerKind: "agent",
		Authority:    planreview.AuthorityAdvisory,
		Verdict:      planreview.VerdictApprove,
	})

	// 7. The status now resolves 'complete' carrying ONLY the round-2 verdict.
	st = getImplementReviewStatus(t, ctx, session, fx.runID)
	if st == nil || st.Status != "complete" {
		t.Fatalf("post-re-review implement_review_status = %+v, want complete", st)
	}
	if len(st.Reviews) != 1 {
		t.Fatalf("post-re-review reviews = %+v, want only the round-2 verdict", st.Reviews)
	}
	if st.Reviews[0].Verdict != "approve" {
		t.Errorf("post-re-review verdict = %q, want approve (round-2); the round-1 approve_with_concerns must be floored out", st.Reviews[0].Verdict)
	}

	// And await_review now returns 'complete' immediately with the round-2 verdict.
	finalOut := callAwaitReview(t, ctx, session, fx.runID, awaitTimeout)
	if finalOut.Status != "complete" {
		t.Errorf("final await_review status = %q, want complete", finalOut.Status)
	}
	if len(finalOut.Reviews) != 1 || finalOut.Reviews[0].Verdict != "approve" {
		t.Errorf("final await_review reviews = %+v, want only the round-2 approve verdict", finalOut.Reviews)
	}
}

// implementReviewStatus mirrors the MCP server's ReviewStatus output shape so
// the integration test can decode it off the get_run_status / await_review
// responses.
type implementReviewStatus struct {
	Stage   string `json:"stage"`
	Status  string `json:"status"`
	Reviews []struct {
		Verdict string `json:"verdict"`
	} `json:"reviews"`
}

// getImplementReviewStatus calls fishhawk_get_run_status and returns the
// decoded implement_review_status (nil when absent).
func getImplementReviewStatus(t *testing.T, ctx context.Context, session *mcp.ClientSession, runID uuid.UUID) *implementReviewStatus {
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
		ImplementReviewStatus *implementReviewStatus `json:"implement_review_status"`
	}
	decodeStructured(t, result, &out)
	return out.ImplementReviewStatus
}

// callAwaitReview calls fishhawk_await_review for the implement stage with the
// given timeout and returns the decoded output.
func callAwaitReview(t *testing.T, ctx context.Context, session *mcp.ClientSession, runID uuid.UUID, timeoutSeconds int) implementReviewStatus {
	t.Helper()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "fishhawk_await_review",
		Arguments: map[string]any{
			"run_id":          runID.String(),
			"stage":           "implement",
			"timeout_seconds": timeoutSeconds,
		},
	})
	if err != nil {
		t.Fatalf("CallTool fishhawk_await_review: %v", err)
	}
	if result.IsError {
		t.Fatalf("await_review tool returned error: %s", toolContentString(t, result))
	}
	var out implementReviewStatus
	decodeStructured(t, result, &out)
	return out
}

// seedImplementReviewStarted records an implement_review_started audit entry —
// the #600 proxy that keeps a not-yet-terminal review reading 'pending'
// (unfloored across the fix-up boundary, #894).
func seedImplementReviewStarted(t *testing.T, ctx context.Context, repo audit.Repository, runID, stageID uuid.UUID) {
	t.Helper()
	payload, err := json.Marshal(planreview.ReviewStartedPayload{
		ConfiguredAgents: 1,
		Authority:        planreview.AuthorityAdvisory,
	})
	if err != nil {
		t.Fatalf("marshal implement_review_started payload: %v", err)
	}
	kind := audit.ActorKind("agent")
	if _, err := repo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  "implement_review_started",
		ActorKind: &kind,
		Payload:   payload,
	}); err != nil {
		t.Fatalf("AppendChained implement_review_started: %v", err)
	}
}

// seedImplementReviewVerdict records an implement_reviewed audit entry with an
// arbitrary verdict payload — used to land the round-2 re-review of the fix-up
// head at a sequence above the fix-up boundary.
func seedImplementReviewVerdict(t *testing.T, ctx context.Context, repo audit.Repository, runID, stageID uuid.UUID, p planreview.ImplementReviewedPayload) {
	t.Helper()
	payload, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal implement_reviewed payload: %v", err)
	}
	kind := audit.ActorKind("agent")
	if _, err := repo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  "implement_reviewed",
		ActorKind: &kind,
		Payload:   payload,
	}); err != nil {
		t.Fatalf("AppendChained implement_reviewed (round 2): %v", err)
	}
}

// reviewActionHint mirrors the MCP server's ReviewActionHint output shape so
// the integration test can decode it off the get_run_status response.
type reviewActionHint struct {
	Concerns             int    `json:"concerns"`
	RemainingFixupBudget int    `json:"remaining_fixup_budget"`
	OverrideAvailable    bool   `json:"override_available"`
	Message              string `json:"message"`
}

// getReviewActionHint calls fishhawk_get_run_status and returns the decoded
// review_action_hint (nil when absent).
func getReviewActionHint(t *testing.T, ctx context.Context, session *mcp.ClientSession, runID uuid.UUID) *reviewActionHint {
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
		ReviewActionHint *reviewActionHint `json:"review_action_hint"`
	}
	decodeStructured(t, result, &out)
	return out.ReviewActionHint
}

// walkToSucceeded walks a stage pending → dispatched → running →
// succeeded, the push_and_open_pr terminal shape for an implement stage
// that committed and opened the PR.
func walkToSucceeded(t *testing.T, ctx context.Context, repo runpkg.Repository, stageID uuid.UUID) {
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

// parkAtGate walks an implement stage pending → dispatched → running →
// awaiting_approval, the precondition for a fix-up. The stage may start
// in pending (fresh) or pending (just re-opened by a prior fix-up).
func parkAtGate(t *testing.T, ctx context.Context, repo runpkg.Repository, stageID uuid.UUID) {
	t.Helper()
	for _, to := range []runpkg.StageState{
		runpkg.StageStateDispatched,
		runpkg.StageStateRunning,
		runpkg.StageStateAwaitingApproval,
	} {
		if _, err := repo.TransitionStage(ctx, stageID, to, nil); err != nil {
			t.Fatalf("TransitionStage → %s: %v", to, err)
		}
	}
}

// seedImplementReview records an implement_reviewed audit entry with an
// approve_with_concerns verdict carrying the given concerns — the set
// the operator's fix-up indices address.
func seedImplementReview(t *testing.T, ctx context.Context, repo audit.Repository, runID, stageID uuid.UUID, concerns ...planreview.Concern) {
	t.Helper()
	payload, err := json.Marshal(planreview.ImplementReviewedPayload{
		ReviewerKind: "agent",
		Authority:    planreview.AuthorityAdvisory,
		Verdict:      planreview.VerdictApproveWithConcerns,
		Concerns:     concerns,
	})
	if err != nil {
		t.Fatalf("marshal implement_reviewed payload: %v", err)
	}
	kind := audit.ActorKind("agent")
	if _, err := repo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  "implement_reviewed",
		ActorKind: &kind,
		Payload:   payload,
	}); err != nil {
		t.Fatalf("AppendChained implement_reviewed: %v", err)
	}
}

// getPromptRender fetches GET /v0/stages/{id}/prompt-render and returns
// the rendered prompt text. prompt-render needs no signature (unlike the
// runner-facing /prompt endpoint).
func getPromptRender(t *testing.T, ctx context.Context, baseURL string, stageID uuid.UUID) string {
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

// getPromptRenderScopeFiles fetches GET /v0/stages/{id}/prompt-render and
// returns the effective scope.files paths (the set the runner's #818
// created-out-of-scope gate diffs against), in order.
func getPromptRenderScopeFiles(t *testing.T, ctx context.Context, baseURL string, stageID uuid.UUID) []string {
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
		ScopeFiles []struct {
			Path string `json:"path"`
		} `json:"scope_files"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode prompt-render response: %v", err)
	}
	paths := make([]string, 0, len(out.ScopeFiles))
	for _, f := range out.ScopeFiles {
		paths = append(paths, f.Path)
	}
	return paths
}

// decodeStructured marshals a tool result's StructuredContent and
// unmarshals it into dst, failing the test on any error.
func decodeStructured(t *testing.T, r *mcp.CallToolResult, dst any) {
	t.Helper()
	if r.StructuredContent == nil {
		t.Fatal("tool returned no StructuredContent")
	}
	raw, err := json.Marshal(r.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		t.Fatalf("decode structured output: %v", err)
	}
}

// TestE2E_Fixup_DuplicateFailureReportThenForcedOverride drives the full
// #968 incident sequence across the MCP → server → run → orchestrator
// layers — the seam the per-layer units cannot cover (cf. #618):
//
//  1. a fix-up re-dispatch FAILS and #788 recovery restores the run to its
//     review gate (implement succeeded, review awaiting_approval);
//  2. a DUPLICATE failure report for the same stage arrives. FailStage
//     rejects it (the stage is already recovered), and the fall-through
//     Advance — which in the incident stamped run 68e13183 succeeded with
//     its gate open — must leave the run RUNNING at its gate;
//  3. the re-review lands a fresh concern: get_run_status's
//     review_action_hint advertises the operator override
//     (override_available=true), and the fixup endpoint AGREES — a forced
//     pass (force_additional_pass=true) is ACCEPTED within the 3-pass
//     ceiling and re-parks the review stage again, instead of the hint
//     advertising an override the server would refuse (the #968 disagreement).
func TestE2E_Fixup_DuplicateFailureReportThenForcedOverride(t *testing.T) {
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	auditRepo := audit.NewPostgresRepository(fx.pool)

	if _, err := fx.runRepo.TransitionRun(ctx, fx.runID, runpkg.StateRunning); err != nil {
		t.Fatalf("TransitionRun → running: %v", err)
	}

	// push_and_open_pr shape: implement SUCCEEDED (PR open), separate
	// review stage holding the human gate.
	impl, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:            fx.runID,
		Sequence:         1,
		Type:             runpkg.StageTypeImplement,
		ExecutorKind:     runpkg.ExecutorAgent,
		ExecutorRef:      "fishhawk/runner@v1",
		RequiresApproval: false,
	})
	if err != nil {
		t.Fatalf("CreateStage(implement): %v", err)
	}
	walkToSucceeded(t, ctx, fx.runRepo, impl.ID)

	review, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:            fx.runID,
		Sequence:         2,
		Type:             runpkg.StageTypeReview,
		ExecutorKind:     runpkg.ExecutorAgent,
		ExecutorRef:      "fishhawk/runner@v1",
		RequiresApproval: true,
	})
	if err != nil {
		t.Fatalf("CreateStage(review): %v", err)
	}
	parkAtGate(t, ctx, fx.runRepo, review.ID)

	seedImplementReview(t, ctx, auditRepo, fx.runID, impl.ID,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "address the drift"})

	// 1. Fix-up pass 1 through the real MCP binary, then the re-dispatch
	// fails its push step: #788 recovery restores the review gate.
	session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, fx.url)
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "fishhawk_fixup_stage",
		Arguments: map[string]any{
			"stage_id": impl.ID.String(),
			"concerns": []int{0},
			"reason":   "address the scope concern on the open PR",
		},
	})
	if err != nil {
		t.Fatalf("CallTool fishhawk_fixup_stage: %v", err)
	}
	if result.IsError {
		t.Fatalf("fix-up tool returned error: %s", toolContentString(t, result))
	}
	shipPushFixupTraceViaBackend(t, ctx, fx, impl.ID)
	failPushPRViaBackend(t, ctx, fx, impl.ID)

	assertReviewGateRestored := func(step string) {
		t.Helper()
		curImpl, err := fx.runRepo.GetStage(ctx, impl.ID)
		if err != nil {
			t.Fatalf("%s: GetStage(implement): %v", step, err)
		}
		if curImpl.State != runpkg.StageStateSucceeded {
			t.Errorf("%s: implement state = %q, want succeeded", step, curImpl.State)
		}
		curReview, err := fx.runRepo.GetStage(ctx, review.ID)
		if err != nil {
			t.Fatalf("%s: GetStage(review): %v", step, err)
		}
		if curReview.State != runpkg.StageStateAwaitingApproval {
			t.Errorf("%s: review state = %q, want awaiting_approval", step, curReview.State)
		}
		curRun, err := fx.runRepo.GetRun(ctx, fx.runID)
		if err != nil {
			t.Fatalf("%s: GetRun: %v", step, err)
		}
		if curRun.State != runpkg.StateRunning {
			t.Errorf("%s: run state = %q, want running (never succeeded with the gate open)", step, curRun.State)
		}
	}
	assertReviewGateRestored("after recovery")

	// 2. The DUPLICATE failure report — the 68e13183 trigger. FailStage
	// rejects it (the stage already recovered to succeeded); the run must
	// stay running at its gate, not roll up succeeded.
	failPushPRViaBackend(t, ctx, fx, impl.ID)
	assertReviewGateRestored("after duplicate failure report")

	// The duplicate recovered nothing — still exactly one
	// stage_fixup_recovered entry.
	recovered, err := auditRepo.ListForRunByCategory(ctx, fx.runID, server.CategoryStageFixupRecovered)
	if err != nil {
		t.Fatalf("ListForRunByCategory(recovered): %v", err)
	}
	if len(recovered) != 1 {
		t.Errorf("stage_fixup_recovered entries = %d, want 1 (the duplicate must not recover again)", len(recovered))
	}

	// 3. The re-review lands a fresh concern. The hint advertises the
	// operator override — and must AGREE with the fixup endpoint.
	seedImplementReview(t, ctx, auditRepo, fx.runID, impl.ID,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "the re-review still sees drift"})
	hint := getReviewActionHint(t, ctx, session, fx.runID)
	if hint == nil {
		t.Fatalf("review_action_hint absent; want the override pointer (budget spent, run running at its gate)")
	}
	if hint.RemainingFixupBudget != 0 {
		t.Errorf("review_action_hint.remaining_fixup_budget = %d, want 0", hint.RemainingFixupBudget)
	}
	if !hint.OverrideAvailable {
		t.Errorf("review_action_hint.override_available = false, want true (one pass used, ceiling 3)")
	}

	// The endpoint agrees with the advertised hint: the forced pass is
	// ACCEPTED within the ceiling and re-parks the review stage again.
	forcedRes, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "fishhawk_fixup_stage",
		Arguments: map[string]any{
			"stage_id":              impl.ID.String(),
			"concerns":              []int{0},
			"reason":                "operator-granted override pass",
			"force_additional_pass": true,
		},
	})
	if err != nil {
		t.Fatalf("CallTool forced fishhawk_fixup_stage: %v", err)
	}
	if forcedRes.IsError {
		t.Fatalf("forced fix-up refused — hint advertised an override the server would not grant (#968 disagreement): %s",
			toolContentString(t, forcedRes))
	}

	curImpl, err := fx.runRepo.GetStage(ctx, impl.ID)
	if err != nil {
		t.Fatalf("GetStage(implement) after forced pass: %v", err)
	}
	if curImpl.State != runpkg.StageStatePending {
		t.Errorf("implement state after forced pass = %q, want pending (re-opened)", curImpl.State)
	}
	curReview, err := fx.runRepo.GetStage(ctx, review.ID)
	if err != nil {
		t.Fatalf("GetStage(review) after forced pass: %v", err)
	}
	if curReview.State != runpkg.StageStatePending {
		t.Errorf("review state after forced pass = %q, want pending (re-parked, NOT stranded)", curReview.State)
	}

	// The forced pass is durably audited: two stage_fixup_triggered
	// entries, the latest marked forced and naming the re-parked review.
	triggered, err := auditRepo.ListForRunByCategory(ctx, fx.runID, server.CategoryStageFixupTriggered)
	if err != nil {
		t.Fatalf("ListForRunByCategory(triggered): %v", err)
	}
	if len(triggered) != 2 {
		t.Fatalf("stage_fixup_triggered entries = %d, want 2", len(triggered))
	}
	var forcedPayload struct {
		Forced                bool   `json:"forced"`
		ReparkedReviewStageID string `json:"reparked_review_stage_id"`
	}
	if err := json.Unmarshal(triggered[len(triggered)-1].Payload, &forcedPayload); err != nil {
		t.Fatalf("unmarshal forced payload: %v", err)
	}
	if !forcedPayload.Forced {
		t.Errorf("forced fix-up audit forced = false, want true")
	}
	if forcedPayload.ReparkedReviewStageID != review.ID.String() {
		t.Errorf("reparked_review_stage_id = %q, want %s", forcedPayload.ReparkedReviewStageID, review.ID)
	}

	// 4. After the forced pass the latest round has no landed concerns yet,
	// so the hint suppresses — consistent with there being nothing further
	// to route back until the re-review lands.
	if hint := getReviewActionHint(t, ctx, session, fx.runID); hint != nil {
		t.Errorf("review_action_hint = %+v, want nil after the forced pass (no concerns in the new round yet)", hint)
	}
}
