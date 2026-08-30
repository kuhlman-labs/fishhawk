package mcpe2e_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestE2E_Revise_ConstraintInjectedAndPlanRebound is the cross-component
// done-means test for the plan-gate `revise` verdict (E22.X / #1099). It
// drives the seam the per-layer unit tests cannot cover on their own (cf.
// #618): an operator triggering a revise through the REAL fishhawk-mcp
// binary → the backend re-opening the parked plan stage → the
// plan_revised audit entry landing in Postgres carrying the operator
// constraint → the prompt renderer reading it back AND loading the prior
// plan as the revision base → the stage returning to the review→approve
// gate where an approve succeeds.
//
// What this harness exercises end-to-end (real MCP binary → real backend
// HTTP → real Postgres):
//
//   - the MCP revise tool resolves the plan stage from the run id, the
//     operator fhk_* token (write:approvals) authorizes, run.RevisePlanStage
//     re-opens the plan stage awaiting_approval → pending, and the
//     plan_revised audit entry persists carrying the binding constraint;
//   - the deterministic prompt renderer reads that audit entry back AND
//     loads the prior plan artifact, emitting the dedicated "### Revision
//     constraint (binding ...)" section with both the constraint and the
//     prior plan as the revision base — the audit+artifact → prompt seam;
//   - the gate round-trips: re-parked at awaiting_approval, an approve
//     through the MCP binary succeeds.
func TestE2E_Revise_ConstraintInjectedAndPlanRebound(t *testing.T) {
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// newFixture's server has no GitHub or ArtifactRepo wired; the
	// prompt-render handler short-circuits to 503 without GitHub
	// (issueGetter() == nil) and cannot load a revision base without the
	// artifact store. Stand up a second backend over the SAME pool with
	// both wired so we can assert the rendered prompt. The operator fhk_*
	// token authenticates against the same apitoken rows (same pool).
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

	// 1. Seed a plan stage parked at the approval gate. CreateStage lands
	// it in pending; parkAtGate walks it pending → dispatched → running →
	// awaiting_approval so it is a valid revise candidate.
	planStage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:            fx.runID,
		Sequence:         1,
		Type:             runpkg.StageTypePlan,
		ExecutorKind:     runpkg.ExecutorAgent,
		ExecutorRef:      "fishhawk/runner@v1",
		RequiresApproval: true,
	})
	if err != nil {
		t.Fatalf("CreateStage(plan): %v", err)
	}

	// 2. Seed the prior plan artifact on the plan stage — the revision base
	// the re-dispatched prompt must carry. A recognizable summary marker
	// lets us assert the base block rendered.
	const basePlanSummary = "REVISE_BASE_PLAN_MARKER add a dryRun flag to the dispatcher"
	schema := "standard_v1"
	planContent, _ := json.Marshal(map[string]any{
		"plan_version":                 "standard_v1",
		"ticket_reference":             map[string]any{"type": "github_issue", "url": "https://github.com/x/y/issues/1", "id": "x/y#1"},
		"generated_by":                 map[string]any{"agent": "claude-code", "model": "claude-opus-4-8", "timestamp": "2026-06-15T00:00:00Z"},
		"summary":                      basePlanSummary,
		"scope":                        map[string]any{"files": []map[string]any{{"path": "backend/internal/webhook/dispatcher.go", "operation": "modify"}}},
		"approach":                     []map[string]any{{"step": 1, "description": "Plumb dryRun through Handle."}},
		"verification":                 map[string]any{"test_strategy": "Run the dispatcher tests.", "rollback_plan": "Revert the PR."},
		"predicted_runtime_minutes":    20,
		"predicted_runtime_confidence": "high",
	})
	if _, err := artifactRepo.Create(ctx, artifact.CreateParams{
		StageID:       planStage.ID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &schema,
		Content:       planContent,
		ContentHash:   "deadbeef",
	}); err != nil {
		t.Fatalf("seed plan artifact: %v", err)
	}

	parkAtGate(t, ctx, fx.runRepo, planStage.ID)

	// 3. Trigger the revise through the real fishhawk-mcp binary, pointed at
	// the GitHub+artifact-wired backend. The tool resolves the plan stage
	// from the run id internally.
	session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, httpSrv.URL)

	const constraint = "REVISE_CONSTRAINT_MARKER use the existing httpclient retry helper, do not add a new backoff package"
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "fishhawk_revise_plan",
		Arguments: map[string]any{
			"run_id":     fx.runID.String(),
			"constraint": constraint,
		},
	})
	if err != nil {
		t.Fatalf("CallTool fishhawk_revise_plan: %v", err)
	}
	if result.IsError {
		t.Fatalf("revise tool returned error: %s", toolContentString(t, result))
	}

	var reviseOut struct {
		Stage struct {
			ID    string `json:"id"`
			State string `json:"state"`
			Type  string `json:"type"`
		} `json:"stage"`
		StageID string `json:"stage_id"`
	}
	decodeStructured(t, result, &reviseOut)
	if reviseOut.Stage.ID != planStage.ID.String() {
		t.Errorf("revise stage id = %q, want %s", reviseOut.Stage.ID, planStage.ID)
	}
	if reviseOut.Stage.State != string(runpkg.StageStatePending) {
		t.Errorf("revise stage state = %q, want pending (re-opened, no orchestrator wired)", reviseOut.Stage.State)
	}

	// 4. The plan_revised audit entry landed in Postgres carrying the
	// binding constraint — the durable record the bound is counted against
	// and the prompt renderer reads back.
	entries, err := auditRepo.ListForRunByCategory(ctx, fx.runID, server.CategoryPlanRevised)
	if err != nil {
		t.Fatalf("ListForRunByCategory: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("plan_revised entries = %d, want 1", len(entries))
	}
	var revised struct {
		PassOrdinal     int    `json:"pass_ordinal"`
		RemainingBudget int    `json:"remaining_budget"`
		Conditions      string `json:"conditions"`
	}
	if err := json.Unmarshal(entries[0].Payload, &revised); err != nil {
		t.Fatalf("unmarshal plan_revised payload: %v", err)
	}
	if revised.PassOrdinal != 1 {
		t.Errorf("pass_ordinal = %d, want 1", revised.PassOrdinal)
	}
	if revised.RemainingBudget != 0 {
		t.Errorf("remaining_budget = %d, want 0 (default bound is 1)", revised.RemainingBudget)
	}
	if !strings.Contains(revised.Conditions, constraint) {
		t.Errorf("plan_revised conditions = %q, want the binding constraint", revised.Conditions)
	}

	// 5. The deterministic prompt now renders the binding constraint AND
	// the prior plan as the revision base — the done-means seam. The stage
	// is in pending (runnable) after the re-open, so prompt-render serves it.
	rendered := getPromptRender(t, ctx, httpSrv.URL, planStage.ID)
	if !strings.Contains(rendered, "### Revision constraint (binding") {
		t.Errorf("rendered prompt missing the Revision constraint section:\n%s", rendered)
	}
	if !strings.Contains(rendered, constraint) {
		t.Errorf("rendered prompt missing the binding constraint %q", constraint)
	}
	if !strings.Contains(rendered, "Prior plan (the revision base):") {
		t.Errorf("rendered prompt missing the revision-base block:\n%s", rendered)
	}
	if !strings.Contains(rendered, basePlanSummary) {
		t.Errorf("rendered prompt missing the prior plan as revision base (marker %q):\n%s", basePlanSummary, rendered)
	}

	// 6. The gate round-trips: re-park the plan stage at awaiting_approval
	// (modelling the re-planned re-dispatch landing back at the gate, which
	// this agent-less harness does not run), then approve through the MCP
	// binary — it must succeed.
	parkAtGate(t, ctx, fx.runRepo, planStage.ID)

	// --override-budget keeps the gate's predicted-runtime check from
	// failing this agent-less fixture's seeded plan; the revise→approve
	// round-trip is what this asserts, not the budget gate.
	approveResult, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "fishhawk_approve_plan",
		Arguments: map[string]any{"run_id": fx.runID.String(), "reason": "--override-budget"},
	})
	if err != nil {
		t.Fatalf("CallTool fishhawk_approve_plan: %v", err)
	}
	if approveResult.IsError {
		t.Fatalf("approve after revise returned error: %s", toolContentString(t, approveResult))
	}
	var approveOut struct {
		Stage struct {
			State string `json:"state"`
		} `json:"stage"`
	}
	decodeStructured(t, approveResult, &approveOut)
	if approveOut.Stage.State != string(runpkg.StageStateSucceeded) {
		t.Errorf("post-approve plan stage state = %q, want succeeded", approveOut.Stage.State)
	}

	// Still exactly one plan_revised entry — the approve wrote none.
	entries, err = auditRepo.ListForRunByCategory(ctx, fx.runID, server.CategoryPlanRevised)
	if err != nil {
		t.Fatalf("ListForRunByCategory (post-approve): %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("plan_revised entries after approve = %d, want 1", len(entries))
	}
}

// regressionPlanJSON builds a schema-valid standard_v1 plan body with the
// given summary and top-level scope files (all modify).
func regressionPlanJSON(summary string, scopeFiles []string) []byte {
	files := make([]map[string]any, 0, len(scopeFiles))
	for _, f := range scopeFiles {
		files = append(files, map[string]any{"path": f, "operation": "modify"})
	}
	body, _ := json.Marshal(map[string]any{
		"plan_version":                 "standard_v1",
		"ticket_reference":             map[string]any{"type": "github_issue", "url": "https://github.com/x/y/issues/1", "id": "x/y#1"},
		"generated_by":                 map[string]any{"agent": "claude-code", "model": "claude-opus-4-8", "timestamp": "2026-06-15T00:00:00Z"},
		"summary":                      summary,
		"scope":                        map[string]any{"files": files},
		"approach":                     []map[string]any{{"step": 1, "description": "Do the thing."}},
		"verification":                 map[string]any{"test_strategy": "Run the tests.", "rollback_plan": "Revert the PR."},
		"predicted_runtime_minutes":    20,
		"predicted_runtime_confidence": "high",
	})
	return body
}

// shipPlanSigned POSTs a plan body to /v0/runs/{id}/plan?stage_id=, signed
// with the run's per-run Ed25519 key (the runner's production shape).
func shipPlanSigned(t *testing.T, ctx context.Context, baseURL string, runID, stageID interface{ String() string }, priv ed25519.PrivateKey, body []byte) *http.Response {
	t.Helper()
	url := baseURL + "/v0/runs/" + runID.String() + "/plan?stage_id=" + stageID.String()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build ship-plan request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	sig := ed25519.Sign(priv, signing.ComputeMessage(body))
	req.Header.Set("X-Fishhawk-Signature", hex.EncodeToString(sig))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("ship-plan request: %v", err)
	}
	return resp
}

// scopeRefusalHarness stands up the shared cross-component rig the three
// #2516 cases drive: a backend over the fixture's Postgres pool with the
// artifact, audit, signing and approval repos wired, an ORCHESTRATOR (the
// refusal's re-dispatch precondition — without it tryScopeRetry returns false
// and the gate degrades), a plan stage parked at the approval gate, the
// revision-base plan artifact scoping baseFiles, and a connected MCP client
// speaking to the REAL fishhawk-mcp binary.
func scopeRefusalHarness(t *testing.T, ctx context.Context, fx *e2eFixture, baseFiles []string) (audit.Repository, *runpkg.Stage, *mcp.ClientSession, string) {
	t.Helper()
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
		Orchestrator: &orchestrator.Orchestrator{Runs: fx.runRepo},
	})
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)

	planStage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:            fx.runID,
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
		Content: regressionPlanJSON("base plan", baseFiles), ContentHash: "basehash2516",
	}); err != nil {
		t.Fatalf("seed base plan artifact: %v", err)
	}
	parkAtGate(t, ctx, fx.runRepo, planStage.ID)

	session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, httpSrv.URL)
	return auditRepo, planStage, session, httpSrv.URL
}

// callRevise drives the REAL fishhawk_revise_plan MCP tool and fails the test
// on a transport or tool error.
func callRevise(t *testing.T, ctx context.Context, session *mcp.ClientSession, runID interface{ String() string }, constraint string) {
	t.Helper()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "fishhawk_revise_plan",
		Arguments: map[string]any{"run_id": runID.String(), "constraint": constraint},
	})
	if err != nil {
		t.Fatalf("CallTool fishhawk_revise_plan: %v", err)
	}
	if res.IsError {
		t.Fatalf("fishhawk_revise_plan returned error: %s", toolContentString(t, res))
	}
}

// walkToRunning advances the re-opened plan stage to running, mirroring the
// production order this agent-less harness does not run: the runner ships its
// trace (dispatched -> running) before shipping the plan. handleShipPlan's
// terminal advance is running -> awaiting_approval, so without this step the
// park assertions would read a stage the state machine legitimately refused
// to move rather than a gate decision.
func walkToRunning(t *testing.T, ctx context.Context, fx *e2eFixture, stageID uuid.UUID) {
	t.Helper()
	st, err := fx.runRepo.GetStage(ctx, stageID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	if st.State == runpkg.StageStatePending {
		if _, err := fx.runRepo.TransitionStage(ctx, stageID, runpkg.StageStateDispatched, nil); err != nil {
			t.Fatalf("transition to dispatched: %v", err)
		}
	}
	if _, err := fx.runRepo.TransitionStage(ctx, stageID, runpkg.StageStateRunning, nil); err != nil {
		t.Fatalf("transition to running: %v", err)
	}
}

// removalPlanJSON builds a schema-valid standard_v1 plan scoping scopeFiles
// and DECLARING each removals entry in the top-level scope_removals array.
func removalPlanJSON(summary string, scopeFiles []string, removals map[string]string) []byte {
	var body map[string]any
	_ = json.Unmarshal(regressionPlanJSON(summary, scopeFiles), &body)
	entries := make([]map[string]any, 0, len(removals))
	for path, reason := range removals {
		entries = append(entries, map[string]any{"path": path, "reason": reason})
	}
	body["scope_removals"] = entries
	out, _ := json.Marshal(body)
	return out
}

// TestE2E_Revise_UndeclaredNarrowingRefused is the #2516 cross-component
// done-means test — the ticket's "a revise whose constraint touches one file,
// asserting every other scoped file survives". It drives the seam per-layer
// units cannot cover together: the REAL fishhawk_revise_plan MCP tool → the
// REAL backend HTTP plan-ship path → handleShipPlan capturing the revision
// base before ArtifactRepo.Create → the refusal writing a plan_scope_retry
// entry in Postgres whose required_scope_files STILL names the dropped file →
// the plan stage re-opened rather than parked at the gate, with ZERO reviewer
// passes spent.
func TestE2E_Revise_UndeclaredNarrowingRefused(t *testing.T) {
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const kept = "backend/internal/webhook/dispatcher.go"
	const droppedFile = "backend/internal/webhook/helper.go"
	const alsoKept = "backend/internal/webhook/helper_test.go"
	auditRepo, planStage, session, baseURL := scopeRefusalHarness(t, ctx, fx,
		[]string{kept, droppedFile, alsoKept})

	// The operator's constraint touches ONE file; the planner must keep the rest.
	callRevise(t, ctx, session, fx.runID, "rework the dispatcher retry; keep everything else")
	walkToRunning(t, ctx, fx, planStage.ID)

	// Ship a revision that silently drops helper.go (and helper_test.go).
	resp := shipPlanSigned(t, ctx, baseURL, fx.runID, planStage.ID, fx.signingPriv,
		regressionPlanJSON("revised plan narrowed to one file", []string{kept}))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("ship revised plan status = %d, want 201", resp.StatusCode)
	}

	// The refusal landed, and its required_scope_files is the ENUMERATED
	// carry-forward set: every base-scoped file, INCLUDING the dropped ones.
	retryEntries, err := auditRepo.ListForRunByCategory(ctx, fx.runID, "plan_scope_retry")
	if err != nil {
		t.Fatalf("ListForRunByCategory(plan_scope_retry): %v", err)
	}
	if len(retryEntries) != 1 {
		t.Fatalf("plan_scope_retry entries = %d, want 1 (the undeclared narrowing must be REFUSED)", len(retryEntries))
	}
	var retry struct {
		RequiredScopeFiles []string `json:"required_scope_files"`
		UndeclaredRemovals []string `json:"undeclared_removals"`
	}
	if err := json.Unmarshal(retryEntries[0].Payload, &retry); err != nil {
		t.Fatalf("unmarshal plan_scope_retry payload: %v", err)
	}
	for _, want := range []string{kept, droppedFile, alsoKept} {
		found := false
		for _, got := range retry.RequiredScopeFiles {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("required_scope_files = %v, missing %q — every other scoped file must survive the revise",
				retry.RequiredScopeFiles, want)
		}
	}
	if len(retry.UndeclaredRemovals) != 2 {
		t.Errorf("undeclared_removals = %v, want the two dropped files", retry.UndeclaredRemovals)
	}

	// The stage was RE-OPENED, not parked at the gate: the narrowed plan
	// never reached a reviewer or the operator's approval gate.
	got, err := fx.runRepo.GetStage(ctx, planStage.ID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	if got.State == runpkg.StageStateAwaitingApproval {
		t.Errorf("plan stage parked at awaiting_approval; the refusal must spend ZERO reviewer/operator passes")
	}
	if got.State == runpkg.StageStateFailed {
		t.Errorf("plan stage state = failed; the transient category A must not leak")
	}

	// ZERO reviewer passes spent — no plan_reviewed entry exists.
	reviewed, err := auditRepo.ListForRunByCategory(ctx, fx.runID, "plan_reviewed")
	if err != nil {
		t.Fatalf("ListForRunByCategory(plan_reviewed): %v", err)
	}
	if len(reviewed) != 0 {
		t.Errorf("plan_reviewed entries = %d, want 0 (a refused plan spends no reviewer pass)", len(reviewed))
	}
}

// TestE2E_Revise_DeclaredNarrowingAdmitted is the counterpart: the SAME drop,
// but the shipped plan DECLARES it in scope_removals. The gate admits it —
// no plan_scope_retry entry, regressed==false (so the budget refund correctly
// does NOT fire for a deliberate drop), declared_removals names the path, and
// the stage parks at awaiting_approval as it does today.
func TestE2E_Revise_DeclaredNarrowingAdmitted(t *testing.T) {
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const kept = "backend/internal/webhook/dispatcher.go"
	const droppedFile = "backend/internal/webhook/helper.go"
	auditRepo, planStage, session, baseURL := scopeRefusalHarness(t, ctx, fx, []string{kept, droppedFile})

	callRevise(t, ctx, session, fx.runID, "replace the helper seam entirely")
	walkToRunning(t, ctx, fx, planStage.ID)

	resp := shipPlanSigned(t, ctx, baseURL, fx.runID, planStage.ID, fx.signingPriv,
		removalPlanJSON("revised plan with a declared removal", []string{kept},
			map[string]string{droppedFile: "the constraint replaces this helper with the direct call path"}))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("ship revised plan status = %d, want 201", resp.StatusCode)
	}

	retryEntries, err := auditRepo.ListForRunByCategory(ctx, fx.runID, "plan_scope_retry")
	if err != nil {
		t.Fatalf("ListForRunByCategory(plan_scope_retry): %v", err)
	}
	if len(retryEntries) != 0 {
		t.Errorf("plan_scope_retry entries = %d, want 0 (a DECLARED narrowing must be admitted)", len(retryEntries))
	}

	regEntries, err := auditRepo.ListForRunByCategory(ctx, fx.runID, "plan_scope_regression")
	if err != nil {
		t.Fatalf("ListForRunByCategory(plan_scope_regression): %v", err)
	}
	if len(regEntries) != 1 {
		t.Fatalf("plan_scope_regression entries = %d, want 1", len(regEntries))
	}
	var reg struct {
		RemovedFiles     []string `json:"removed_files"`
		DeclaredRemovals []string `json:"declared_removals"`
		Regressed        bool     `json:"regressed"`
	}
	if err := json.Unmarshal(regEntries[0].Payload, &reg); err != nil {
		t.Fatalf("unmarshal plan_scope_regression payload: %v", err)
	}
	if reg.Regressed {
		t.Errorf("regressed = true for a fully-declared narrowing; a deliberate drop is not a mistake")
	}
	if len(reg.DeclaredRemovals) != 1 || reg.DeclaredRemovals[0] != droppedFile {
		t.Errorf("declared_removals = %v, want [%s]", reg.DeclaredRemovals, droppedFile)
	}
	if len(reg.RemovedFiles) != 1 || reg.RemovedFiles[0] != droppedFile {
		t.Errorf("removed_files = %v, want [%s] (its meaning is unchanged: EVERY dropped path)",
			reg.RemovedFiles, droppedFile)
	}

	got, err := fx.runRepo.GetStage(ctx, planStage.ID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	if got.State != runpkg.StageStateAwaitingApproval {
		t.Errorf("plan stage state = %q, want awaiting_approval (a declared narrowing parks normally)", got.State)
	}
}

// TestE2E_Revise_ScopeRetryExhausted_ParksWithBudgetRefund preserves the #1257
// budget-refund coverage the superseded test owned, on the exhaustion path:
// with the one-shot refusal budget already spent, a SECOND undeclared
// narrowing degrades to the prior behaviour — the plan parks at the gate
// carrying the regression evidence, and a subsequent fishhawk_revise_plan is
// still ADMITTED (the refund) where the spent normal budget would otherwise
// 409 revise_budget_exhausted. Exhaustion must never become a new way to lose
// a plan.
func TestE2E_Revise_ScopeRetryExhausted_ParksWithBudgetRefund(t *testing.T) {
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const kept = "backend/internal/webhook/dispatcher.go"
	const droppedFile = "backend/internal/webhook/helper.go"
	auditRepo, planStage, session, baseURL := scopeRefusalHarness(t, ctx, fx, []string{kept, droppedFile})

	callRevise(t, ctx, session, fx.runID, "narrow the scope; keep everything else")
	walkToRunning(t, ctx, fx, planStage.ID)

	// Spend the one-shot refusal budget up front, so this ship takes the
	// degrade path deterministically.
	spent, _ := json.Marshal(map[string]any{
		"run_id":               fx.runID.String(),
		"stage_id":             planStage.ID.String(),
		"attempt":              1,
		"undeclared_removals":  []string{droppedFile},
		"required_scope_files": []string{kept, droppedFile},
	})
	systemKind := audit.ActorKind("system")
	if _, err := auditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID: fx.runID, StageID: &planStage.ID, Timestamp: time.Now().UTC(),
		Category: "plan_scope_retry", ActorKind: &systemKind, Payload: spent,
	}); err != nil {
		t.Fatalf("seed plan_scope_retry: %v", err)
	}

	resp := shipPlanSigned(t, ctx, baseURL, fx.runID, planStage.ID, fx.signingPriv,
		regressionPlanJSON("second narrowing after the budget is spent", []string{kept}))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("ship revised plan status = %d, want 201", resp.StatusCode)
	}

	// No SECOND refusal — the budget is bounded.
	retryEntries, err := auditRepo.ListForRunByCategory(ctx, fx.runID, "plan_scope_retry")
	if err != nil {
		t.Fatalf("ListForRunByCategory(plan_scope_retry): %v", err)
	}
	if len(retryEntries) != 1 {
		t.Errorf("plan_scope_retry entries = %d, want 1 (no new refusal once the budget is spent)", len(retryEntries))
	}

	// The regression evidence is still recorded (the reviewer/operator signal
	// AND the refund counter).
	regEntries, err := auditRepo.ListForRunByCategory(ctx, fx.runID, "plan_scope_regression")
	if err != nil {
		t.Fatalf("ListForRunByCategory(plan_scope_regression): %v", err)
	}
	if len(regEntries) != 1 {
		t.Fatalf("plan_scope_regression entries = %d, want 1", len(regEntries))
	}
	var reg struct {
		RemovedFiles []string `json:"removed_files"`
		Regressed    bool     `json:"regressed"`
	}
	if err := json.Unmarshal(regEntries[0].Payload, &reg); err != nil {
		t.Fatalf("unmarshal plan_scope_regression payload: %v", err)
	}
	if !reg.Regressed {
		t.Errorf("regressed = false; the exhausted path must still flag the regression for the refund")
	}
	if len(reg.RemovedFiles) != 1 || reg.RemovedFiles[0] != droppedFile {
		t.Errorf("removed_files = %v, want [%s]", reg.RemovedFiles, droppedFile)
	}

	// The stage parked at the gate rather than failing terminally.
	got, err := fx.runRepo.GetStage(ctx, planStage.ID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	if got.State != runpkg.StageStateAwaitingApproval {
		t.Fatalf("plan stage state = %q, want awaiting_approval (exhaustion degrades to park-with-evidence)", got.State)
	}

	// The refund seam still holds: a subsequent revise is ADMITTED.
	revise2, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "fishhawk_revise_plan",
		Arguments: map[string]any{
			"run_id":     fx.runID.String(),
			"constraint": "put the dropped file back into scope",
		},
	})
	if err != nil {
		t.Fatalf("CallTool revise pass 2: %v", err)
	}
	if revise2.IsError {
		t.Fatalf("revise pass 2 returned error (budget refund seam broken): %s", toolContentString(t, revise2))
	}
	var revise2Out struct {
		Stage struct {
			State string `json:"state"`
		} `json:"stage"`
	}
	decodeStructured(t, revise2, &revise2Out)
	if revise2Out.Stage.State == string(runpkg.StageStateAwaitingApproval) {
		t.Errorf("revise pass 2 left the stage at awaiting_approval; it was not admitted")
	}
}

// advanceCountingRunRepo wraps a run.Repository and counts GetRun calls. The
// orchestrator's Advance calls GetRun EXACTLY ONCE at its entry (and does not
// re-enter for a plan stage), so when this decorator backs ONLY the
// orchestrator's Runs — the server's own RunRepo stays the unwrapped
// fx.runRepo — the GetRun count equals the number of Advance invocations. It
// is how the concurrent E2E test pins "exactly one re-dispatch" (approval
// condition 3), which a committed-row count alone cannot prove.
type advanceCountingRunRepo struct {
	runpkg.Repository
	mu    sync.Mutex
	count int
}

func (r *advanceCountingRunRepo) GetRun(ctx context.Context, id uuid.UUID) (*runpkg.Run, error) {
	r.mu.Lock()
	r.count++
	r.mu.Unlock()
	return r.Repository.GetRun(ctx, id)
}

func (r *advanceCountingRunRepo) advances() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count
}

// TestE2E_Revise_ConcurrentScopeRetryShips_GrantsExactlyOneRefusal is the #2518
// cross-layer concurrency proof: it fires TWO genuinely concurrent signed plan
// ships of the SAME undeclared-narrowing (distinct summaries so distinct
// content hashes — neither dedups to an early idempotent return, so both reach
// the count→append budget window) for the SAME stage, released by one barrier,
// crossing the HTTP handler → the server helper → the atomic audit primitive →
// Postgres in one exercise. It asserts the deterministic invariants that hold
// across every interleaving:
//
//   - EXACTLY ONE plan_scope_retry entry — the atomic budget guard admits one
//     refusal even when both ships race the window.
//   - AT MOST ONE re-dispatch (approval condition 3): one committed budget row
//     proves the budget was consumed once; the orchestrator Advance count
//     proves the stage was NOT advanced twice. Under genuine concurrency the
//     count is 0-OR-1, never 2: the budget entry-winner's re-open
//     (FailStage→RetryStage→Advance) can lose the stage-CAS race to the
//     loser's fall-through, in which case the refusal degrades to
//     park-with-evidence (the documented FailStage fail-open leg — the entry
//     stays committed, the stage parks at the gate) and no re-dispatch fires.
//     Asserting EXACTLY one would encode that coin flip; the invariant that
//     matters — and the one a broken budget guard violates (two entries ⇒ two
//     re-dispatches) — is <= 1.
//   - the run is not terminally failed — a refusal is never a way to lose a
//     plan.
//   - the plan_scope_regression evidence is still recorded.
//
// The FINAL stage state is deliberately NOT asserted for the same coin-flip
// reason: the loser's fall-through and the winner's re-open race on the stage
// CAS.
func TestE2E_Revise_ConcurrentScopeRetryShips_GrantsExactlyOneRefusal(t *testing.T) {
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const kept = "backend/internal/webhook/dispatcher.go"
	const droppedA = "backend/internal/webhook/helper.go"
	const droppedB = "backend/internal/webhook/helper_test.go"

	auditRepo := audit.NewPostgresRepository(fx.pool)
	signingRepo := signing.NewPostgresRepository(fx.pool)
	artifactRepo := artifact.NewPostgresRepository(fx.pool)
	approvalRepo := approval.NewPostgresRepository(fx.pool)
	// The orchestrator's Runs is wrapped to count Advance; the server's own
	// RunRepo stays the UNWRAPPED fx.runRepo, so only the orchestrator's
	// Advance bumps the count.
	countingRuns := &advanceCountingRunRepo{Repository: fx.runRepo}
	srv := server.New(server.Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      fx.runRepo,
		AuditRepo:    auditRepo,
		SigningRepo:  signingRepo,
		ArtifactRepo: artifactRepo,
		ApprovalRepo: approvalRepo,
		APITokenRepo: fx.apitokenRepo,
		GitHub:       githubclient.New(nil),
		Orchestrator: &orchestrator.Orchestrator{Runs: countingRuns},
	})
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)

	planStage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:            fx.runID,
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
		Content: regressionPlanJSON("base plan", []string{kept, droppedA, droppedB}), ContentHash: "concbase2518",
	}); err != nil {
		t.Fatalf("seed base plan artifact: %v", err)
	}
	parkAtGate(t, ctx, fx.runRepo, planStage.ID)

	session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, httpSrv.URL)
	callRevise(t, ctx, session, fx.runID, "narrow to the dispatcher; keep everything else")
	walkToRunning(t, ctx, fx, planStage.ID)

	// Baseline the Advance count AFTER the revise + walk setup, so the
	// assertion below measures ONLY the ships' re-dispatches — not any Advance
	// the revise path itself may have driven.
	baseAdvances := countingRuns.advances()

	// Two concurrent signed ships, distinct summaries (distinct hashes) but
	// both undeclared narrowings to [kept]. One start barrier releases both.
	summaries := []string{"concurrent narrowing A", "concurrent narrowing B"}
	codes := make([]int, len(summaries))
	var start sync.WaitGroup
	start.Add(1)
	var done sync.WaitGroup
	for i := range summaries {
		done.Add(1)
		go func(i int) {
			defer done.Done()
			start.Wait()
			resp := shipPlanSigned(t, ctx, httpSrv.URL, fx.runID, planStage.ID, fx.signingPriv,
				regressionPlanJSON(summaries[i], []string{kept}))
			codes[i] = resp.StatusCode
			_ = resp.Body.Close()
		}(i)
	}
	start.Done()
	done.Wait()

	for i, c := range codes {
		if c != http.StatusCreated {
			t.Errorf("ship %d status = %d, want 201", i, c)
		}
	}

	// Invariant 1: exactly ONE plan_scope_retry entry.
	retryEntries, err := auditRepo.ListForRunByCategory(ctx, fx.runID, "plan_scope_retry")
	if err != nil {
		t.Fatalf("ListForRunByCategory(plan_scope_retry): %v", err)
	}
	if len(retryEntries) != 1 {
		t.Fatalf("plan_scope_retry entries = %d, want exactly 1 (the atomic budget guard admits one refusal)", len(retryEntries))
	}

	// Invariant 2 (approval condition 3): AT MOST one re-dispatch. One row
	// proves the budget was consumed once; the Advance count proves the stage
	// was NOT advanced twice. It is 0-or-1 under concurrency (see the doc
	// comment): a broken budget guard that committed two entries would drive
	// two re-dispatches, which this refutes; the entry-winner losing the stage
	// CAS to the loser's fall-through legitimately yields 0.
	if got := countingRuns.advances() - baseAdvances; got > 1 {
		t.Errorf("orchestrator Advance count = %d, want <= 1 (one budget consumption must never drive two re-dispatches)", got)
	}

	// Invariant 3: the run is not terminally failed.
	r, err := fx.runRepo.GetRun(ctx, fx.runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if r.State == runpkg.StateFailed {
		t.Errorf("run state = failed; a refusal must never become a new way to lose a plan")
	}

	// Invariant 4: the regression evidence is still recorded.
	regEntries, err := auditRepo.ListForRunByCategory(ctx, fx.runID, "plan_scope_regression")
	if err != nil {
		t.Fatalf("ListForRunByCategory(plan_scope_regression): %v", err)
	}
	if len(regEntries) == 0 {
		t.Errorf("plan_scope_regression entries = 0; the evidence must survive the concurrent refusal")
	}
}

// --- #2871: the binding constraint survives the whole seam, or is refused ---

// reviseHarness stands up the shared cross-component rig the two #2871 cases
// drive: a backend over the fixture's Postgres pool with the artifact, audit,
// signing and approval repos wired (GitHub too, so prompt-render serves), a
// plan stage parked at the approval gate carrying a revision-base plan
// artifact, and a connected MCP client speaking to the REAL fishhawk-mcp
// binary.
func reviseHarness(t *testing.T, ctx context.Context, fx *e2eFixture) (audit.Repository, *runpkg.Stage, *mcp.ClientSession, string) {
	t.Helper()
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

	planStage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:            fx.runID,
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
		Content:     regressionPlanJSON("base plan 2871", []string{"backend/internal/server/revise.go"}),
		ContentHash: "basehash2871",
	}); err != nil {
		t.Fatalf("seed base plan artifact: %v", err)
	}
	parkAtGate(t, ctx, fx.runRepo, planStage.ID)

	session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, httpSrv.URL)
	return auditRepo, planStage, session, httpSrv.URL
}

// largeMultiItemConstraint builds an ~11000-byte enumerated operator constraint
// — the realistic shape of the two live losses (~7500 and 6000-9000 bytes) —
// comfortably over the historical 4000-byte cut and under
// prompt.MaxRevisionConstraintBytes, and returns it with its LAST item's text.
// The last item is what the old code dropped, so it is the discriminating
// assertion; it carries a multi-byte rune so a rune-unsafe cut is visible too.
func largeMultiItemConstraint(t *testing.T) (constraint, lastItem string) {
	t.Helper()
	var b strings.Builder
	i := 1
	for b.Len() < 10500 {
		fmt.Fprintf(&b, "C%d. %s\n\n", i, strings.Repeat("this condition must land verbatim. ", 12))
		i++
	}
	lastItem = fmt.Sprintf("C%d. FINAL_CONDITION_MARKER — keep the change additive; do not bump the schema major. ✅", i)
	b.WriteString(lastItem)
	constraint = b.String()
	if len(constraint) <= 4000 {
		t.Fatalf("fixture is %d bytes; it must exceed the historical 4000-byte cut to discriminate", len(constraint))
	}
	if len(constraint) > prompt.MaxRevisionConstraintBytes {
		t.Fatalf("fixture is %d bytes; it must stay under the %d-byte cap so it is ACCEPTED",
			len(constraint), prompt.MaxRevisionConstraintBytes)
	}
	return constraint, lastItem
}

// TestE2E_Revise_LargeConstraintDeliveredWhole is the cross-layer done-means
// test for #2871. Per-layer units cannot cover it together: the handler, the
// audit store, the loader and the renderer each looked correct in isolation
// while the SEAM dropped the tail, which is exactly the defect.
//
// It drives the REAL fishhawk-mcp binary → the REAL backend HTTP revise path →
// the plan_revised entry in Postgres → the prompt renderer reading it back, and
// asserts (i) the persisted `conditions` equals the submitted constraint
// BYTE-FOR-BYTE (the audit half — the critical one, because a truncated audit
// payload makes the loss undetectable after the fact) and (ii) the re-dispatched
// plan prompt carries the constraint's LAST item verbatim, followed by the
// end-of-constraint marker.
func TestE2E_Revise_LargeConstraintDeliveredWhole(t *testing.T) {
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	auditRepo, planStage, session, baseURL := reviseHarness(t, ctx, fx)
	constraint, lastItem := largeMultiItemConstraint(t)

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "fishhawk_revise_plan",
		Arguments: map[string]any{"run_id": fx.runID.String(), "constraint": constraint},
	})
	if err != nil {
		t.Fatalf("CallTool fishhawk_revise_plan: %v", err)
	}
	if result.IsError {
		t.Fatalf("revise tool returned error: %s", toolContentString(t, result))
	}

	// (i) The AUDIT half: the durable hash-chained record stores the operator's
	// constraint verbatim, so a later reader can detect any loss downstream.
	entries, err := auditRepo.ListForRunByCategory(ctx, fx.runID, server.CategoryPlanRevised)
	if err != nil {
		t.Fatalf("ListForRunByCategory: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("plan_revised entries = %d, want 1", len(entries))
	}
	var revised struct {
		Conditions string `json:"conditions"`
	}
	if err := json.Unmarshal(entries[0].Payload, &revised); err != nil {
		t.Fatalf("unmarshal plan_revised payload: %v", err)
	}
	if revised.Conditions != constraint {
		t.Errorf("stored conditions is %d bytes, want the submitted %d byte-for-byte (tail stored: %q)",
			len(revised.Conditions), len(constraint),
			revised.Conditions[max(0, len(revised.Conditions)-60):])
	}

	// (ii) The PROMPT half: the tail the old code dropped reaches the planner,
	// and the terminator follows it.
	rendered := getPromptRender(t, ctx, baseURL, planStage.ID)
	if !strings.Contains(rendered, lastItem) {
		t.Errorf("the re-dispatched plan prompt is missing the constraint's LAST item %q", lastItem)
	}
	if !strings.Contains(rendered, constraint) {
		t.Errorf("the re-dispatched plan prompt does not carry the constraint whole")
	}
	if !strings.Contains(rendered, lastItem+"\n\n"+prompt.RevisionConstraintEndMarker) {
		t.Errorf("the end-of-constraint marker does not follow the constraint's last item:\n%s",
			rendered[max(0, len(rendered)-600):])
	}
	if strings.Contains(rendered, "...[truncated]") || strings.Contains(rendered, "...[ELIDED") {
		t.Errorf("an under-cap constraint drew a truncation marker in the rendered prompt")
	}
}

// TestE2E_Revise_OverCapConstraintRefusedNoPassSpent is the refusal half across
// the same seam: the 400 surfaces THROUGH the MCP tool as a tool error, no
// plan_revised entry is written, and — the committed-state assertion an error
// identity cannot make — the revise budget is genuinely unspent, proved by a
// subsequent normal revise that succeeds.
func TestE2E_Revise_OverCapConstraintRefusedNoPassSpent(t *testing.T) {
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	auditRepo, _, session, _ := reviseHarness(t, ctx, fx)

	over := strings.Repeat("x", prompt.MaxRevisionConstraintBytes+1)
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "fishhawk_revise_plan",
		Arguments: map[string]any{"run_id": fx.runID.String(), "constraint": over},
	})
	if err != nil {
		t.Fatalf("CallTool fishhawk_revise_plan: %v", err)
	}
	if !res.IsError {
		t.Fatalf("an over-cap constraint was ACCEPTED through the MCP tool; want a refusal")
	}
	msg := toolContentString(t, res)
	for _, want := range []string{"validation_failed", strconv.Itoa(prompt.MaxRevisionConstraintBytes)} {
		if !strings.Contains(msg, want) {
			t.Errorf("tool error missing %q:\n%s", want, msg)
		}
	}

	entries, err := auditRepo.ListForRunByCategory(ctx, fx.runID, server.CategoryPlanRevised)
	if err != nil {
		t.Fatalf("ListForRunByCategory: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("plan_revised entries after the refused call = %d, want 0 (no pass may be consumed)", len(entries))
	}

	// The budget is unspent: a normal revise still succeeds on the first pass.
	callRevise(t, ctx, session, fx.runID, "REFUSAL_FOLLOWUP_MARKER keep it additive")
	entries, err = auditRepo.ListForRunByCategory(ctx, fx.runID, server.CategoryPlanRevised)
	if err != nil {
		t.Fatalf("ListForRunByCategory (post-followup): %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("plan_revised entries after the follow-up revise = %d, want 1", len(entries))
	}
	var revised struct {
		PassOrdinal int `json:"pass_ordinal"`
	}
	if err := json.Unmarshal(entries[0].Payload, &revised); err != nil {
		t.Fatalf("unmarshal plan_revised payload: %v", err)
	}
	if revised.PassOrdinal != 1 {
		t.Errorf("pass_ordinal = %d, want 1 — the refused call must not have consumed a pass", revised.PassOrdinal)
	}
}
