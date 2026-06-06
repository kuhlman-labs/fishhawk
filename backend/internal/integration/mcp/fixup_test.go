package mcpe2e_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
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

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"

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
			"stage_id": stage.ID.String(),
			"concerns": []int{0},
			"reason":   "address the scope concern on the existing branch",
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
		PassOrdinal     int                  `json:"pass_ordinal"`
		RemainingBudget int                  `json:"remaining_budget"`
		Concerns        []planreview.Concern `json:"concerns"`
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
// FAILURE-RECOVERY seam end to end (#788): a push_and_open_pr fix-up
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

	// 3. Simulate the re-dispatched implement landing in `running` (the #742
	// trace gate's state) then FAILING its commit/push step.
	for _, to := range []runpkg.StageState{runpkg.StageStateDispatched, runpkg.StageStateRunning} {
		if _, err := fx.runRepo.TransitionStage(ctx, impl.ID, to, nil); err != nil {
			t.Fatalf("re-dispatch TransitionStage → %s: %v", to, err)
		}
	}
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
func failPushPRViaBackend(t *testing.T, ctx context.Context, fx *e2eFixture, stageID uuid.UUID) {
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

// TestE2E_Fixup_ReviewActionHintSurfacesAndSuppresses drives the
// review-action-hint seam end to end (#777): the audit-persistence →
// hint-compute → MCP-response layers in one test (cf. #618), which the
// per-function units can't express. An implement-review approve_with_concerns
// verdict in Postgres must surface as review_action_hint on
// fishhawk_get_run_status; once a fix-up pass spends the bounded budget, the
// hint must suppress. The suppression assertion is the only guard against a
// silently-wrong RemainingFixupBudget if the mirrored maxFixupPasses const
// drifts from the backend's enforced bound.
func TestE2E_Fixup_ReviewActionHintSurfacesAndSuppresses(t *testing.T) {
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

	// 4. Spend the bounded fix-up budget via the real fix-up tool — one
	// stage_fixup_triggered entry lands in Postgres.
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "fishhawk_fixup_stage",
		Arguments: map[string]any{
			"stage_id": stage.ID.String(),
			"concerns": []int{0},
			"reason":   "address the scope concern",
		},
	})
	if err != nil {
		t.Fatalf("CallTool fishhawk_fixup_stage: %v", err)
	}
	if result.IsError {
		t.Fatalf("fix-up tool returned error: %s", toolContentString(t, result))
	}
	entries, err := auditRepo.ListForRunByCategory(ctx, fx.runID, server.CategoryStageFixupTriggered)
	if err != nil {
		t.Fatalf("ListForRunByCategory: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("stage_fixup_triggered entries = %d, want exactly 1", len(entries))
	}

	// 5. The hint is now suppressed — the single budget unit is spent. This
	// is the guard against a drifting maxFixupPasses mirror.
	if hint := getReviewActionHint(t, ctx, session, fx.runID); hint != nil {
		t.Errorf("review_action_hint should suppress after one fix-up pass; got %+v", hint)
	}
}

// reviewActionHint mirrors the MCP server's ReviewActionHint output shape so
// the integration test can decode it off the get_run_status response.
type reviewActionHint struct {
	Concerns             int    `json:"concerns"`
	RemainingFixupBudget int    `json:"remaining_fixup_budget"`
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
