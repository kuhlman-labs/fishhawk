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

// TestE2E_AmendAcceptanceCriteria_RetiredAtApproval_ReachesAcceptancePromptAndNeutralizesVerdict
// is the cross-boundary proof for #2581. Per-layer unit tests pass while the
// seam between them breaks (cf. #618), so this drives the REAL chain end to end:
// MCP fishhawk_approve_plan carrying `reason` + `amend_acceptance_criteria` →
// HTTP approvals handler → approval row + approval_submitted audit payload →
// acceptance-stage prompt build → POST /v0/runs/{id}/acceptance → the recorded
// acceptance_outcome_recorded payload.
//
// Arm 1 retires crit-2 and ships a verdict failing ONLY crit-2: the outcome is
// recorded passed with the reported verdict + retired ids reconstructable.
// Arm 2 is the counterfactual — a verdict that ALSO fails a NON-retired
// criterion is recorded FAILED and routed to triage, so this can never become a
// way to silence a live criterion.
func TestE2E_AmendAcceptanceCriteria_RetiredAtApproval_ReachesAcceptancePromptAndNeutralizesVerdict(t *testing.T) {
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

	// 1. Plan stage at the gate carrying a plan with THREE acceptance criteria.
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
		"verification": map[string]any{
			"test_strategy": "ts",
			"rollback_plan": "rb",
			"acceptance_criteria": []map[string]any{
				{"id": "crit-1", "statement": "the run settles", "source": "explicit"},
				{"id": "crit-2", "statement": "healthz reports the server budget", "source": "explicit"},
				{"id": "crit-3", "statement": "the advisory renders", "source": "explicit", "blocking": false},
			},
		},
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

	acceptStage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:        fx.runID,
		Sequence:     2,
		Type:         runpkg.StageTypeAcceptance,
		ExecutorKind: runpkg.ExecutorAgent,
		ExecutorRef:  "fishhawk/runner@v1",
	})
	if err != nil {
		t.Fatalf("CreateStage acceptance: %v", err)
	}

	// Seed the minimum ledger a really-dispatched acceptance stage carries so the
	// #3091 unbound-head clamp does NOT fire on this retirement arm (#3124): a
	// reported head plus an acceptance-dispatch anchor at a strictly greater
	// sequence. A head entry alone is necessary but not sufficient — without the
	// dispatch anchor acceptanceValidatedHeadSHA resolves to "" and the ship would
	// legitimately clamp the neutralized pass to undecidable.
	seedReportedHead(t, ctx, auditRepo, fx.runID, acceptStage.ID, "pull_request_opened", "e2evalidatedhead", time.Now())
	seedAcceptanceDispatch(t, ctx, auditRepo, fx.runID, acceptStage.ID)

	// 2. REAL MCP approve carrying the conditions AND the retirement.
	session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, httpSrv.URL)
	const conditionText = "1. Drop the /healthz budget line; the budget rides the run row instead."
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "fishhawk_approve_plan",
		Arguments: map[string]any{
			"run_id": fx.runID.String(),
			"reason": conditionText,
			"amend_acceptance_criteria": []map[string]any{{
				"id": "crit-2", "action": "retire",
				"reason": "condition 1 removed the surface this criterion asserts",
			}},
		},
	})
	if err != nil {
		t.Fatalf("CallTool fishhawk_approve_plan: %v", err)
	}
	if res.IsError {
		t.Fatalf("approve with amend_acceptance_criteria returned a tool error: %s", toolContentString(t, res))
	}
	rows, err := approvalRepo.ListForStage(ctx, planStage.ID)
	if err != nil {
		t.Fatalf("ListForStage: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("approval rows = %d, want 1", len(rows))
	}

	// 3. The amendment landed on the SAME approval_submitted row as the reason.
	subs, err := auditRepo.ListForRunByCategory(ctx, fx.runID, "approval_submitted")
	if err != nil {
		t.Fatalf("ListForRunByCategory approval_submitted: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("approval_submitted entries = %d, want 1", len(subs))
	}
	var submitted struct {
		Comment    string `json:"comment"`
		Amendments []struct {
			ID     string `json:"id"`
			Action string `json:"action"`
			Reason string `json:"reason"`
		} `json:"amend_acceptance_criteria"`
	}
	if err := json.Unmarshal(subs[0].Payload, &submitted); err != nil {
		t.Fatalf("decode approval_submitted payload: %v", err)
	}
	if submitted.Comment != conditionText {
		t.Errorf("comment = %q, want the approval reason on the same row", submitted.Comment)
	}
	if len(submitted.Amendments) != 1 || submitted.Amendments[0].ID != "crit-2" ||
		submitted.Amendments[0].Action != "retire" || submitted.Amendments[0].Reason == "" {
		t.Fatalf("recorded amendments = %+v, want one reasoned crit-2 retirement", submitted.Amendments)
	}

	// 4. The acceptance prompt carries the contested context + the retired block,
	// omits the retired criterion from the live checklist, and STILL serves the
	// retired id in acceptance_criteria_ids (the superset invariant).
	promptBody := getPromptRenderRaw(t, ctx, httpSrv.URL, acceptStage.ID)
	var rendered struct {
		Prompt                string   `json:"prompt"`
		AcceptanceCriteriaIDs []string `json:"acceptance_criteria_ids"`
	}
	if err := json.Unmarshal(promptBody, &rendered); err != nil {
		t.Fatalf("decode prompt-render: %v", err)
	}
	for _, want := range []string{
		"Binding approval conditions",
		conditionText,
		"never `result`=`failed`",
		"Retired at approval",
		"[crit-2] retired: condition 1 removed the surface this criterion asserts",
		"the run settles",
	} {
		if !strings.Contains(rendered.Prompt, want) {
			t.Errorf("acceptance prompt missing %q\n---\n%s", want, rendered.Prompt)
		}
	}
	if strings.Contains(rendered.Prompt, "healthz reports the server budget") {
		t.Errorf("retired criterion still renders in the live checklist\n---\n%s", rendered.Prompt)
	}
	var servesRetired bool
	for _, id := range rendered.AcceptanceCriteriaIDs {
		if id == "crit-2" {
			servesRetired = true
		}
	}
	if !servesRetired {
		t.Errorf("acceptance_criteria_ids = %v, want a SUPERSET still carrying the retired crit-2",
			rendered.AcceptanceCriteriaIDs)
	}

	// 5. Arm 1: a verdict failing ONLY the retired criterion is recorded passed.
	body := []byte(`{"verdict":"failed","failure_mode":"assertion_fail","criteria":[` +
		`{"id":"crit-1","result":"passed"},` +
		`{"id":"crit-2","result":"failed","observed":"no budget line on /healthz"}]}`)
	shipResp := shipAcceptanceE2E(t, ctx, httpSrv.URL, fx.operatorTok, fx.runID, acceptStage.ID, body)
	if shipResp["effective_verdict"] != "passed" {
		t.Errorf("ship response effective_verdict = %v, want passed", shipResp["effective_verdict"])
	}
	outcome := latestAcceptanceOutcomeE2E(t, ctx, auditRepo, fx.runID, acceptStage.ID)
	if outcome["verdict"] != "passed" || outcome["verdict_reported"] != "failed" {
		t.Errorf("recorded verdict/verdict_reported = %v/%v, want passed/failed",
			outcome["verdict"], outcome["verdict_reported"])
	}
	ids, _ := outcome["retired_criterion_ids"].([]any)
	if len(ids) != 1 || ids[0] != "crit-2" {
		t.Errorf("retired_criterion_ids = %v, want [crit-2]", outcome["retired_criterion_ids"])
	}
	// Fixture precondition (#3124): the seeded head + dispatch anchor must resolve
	// a non-empty validated head, so this arm records the neutralized `passed`
	// because retirement holds — NOT because the unbound-head clamp is absent. A
	// future fixture drift surfaces here as a named failure, not a verdict mystery.
	if hs, _ := outcome["head_sha"].(string); hs == "" {
		t.Fatalf("fixture precondition failed: recorded head_sha is empty; the seeded dispatch anchor + head must resolve a validated head")
	}

	// 6. Arm 2 (the counterfactual): a verdict that ALSO fails a NON-retired
	// criterion is recorded FAILED and routed to triage.
	acceptStage2, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:        fx.runID,
		Sequence:     3,
		Type:         runpkg.StageTypeAcceptance,
		ExecutorKind: runpkg.ExecutorAgent,
		ExecutorRef:  "fishhawk/runner@v1",
	})
	if err != nil {
		t.Fatalf("CreateStage acceptance 2: %v", err)
	}
	// Same bound-head ledger for arm 2, so it exercises a resolvable head rather
	// than silently relying on a shipped `failed` being un-softenable (#3124).
	seedReportedHead(t, ctx, auditRepo, fx.runID, acceptStage2.ID, "pull_request_opened", "e2evalidatedhead2", time.Now())
	seedAcceptanceDispatch(t, ctx, auditRepo, fx.runID, acceptStage2.ID)
	liveFailure := []byte(`{"verdict":"failed","failure_mode":"assertion_fail","criteria":[` +
		`{"id":"crit-1","result":"failed","observed":"the run never settled"},` +
		`{"id":"crit-2","result":"failed"}]}`)
	shipResp2 := shipAcceptanceE2E(t, ctx, httpSrv.URL, fx.operatorTok, fx.runID, acceptStage2.ID, liveFailure)
	if _, present := shipResp2["effective_verdict"]; present {
		t.Errorf("ship response carries effective_verdict for a LIVE criterion failure: %v", shipResp2)
	}
	outcome2 := latestAcceptanceOutcomeE2E(t, ctx, auditRepo, fx.runID, acceptStage2.ID)
	if outcome2["verdict"] != "failed" {
		t.Errorf("recorded verdict = %v, want failed (a non-retired failure still fails)", outcome2["verdict"])
	}
	if _, present := outcome2["verdict_reported"]; present {
		t.Errorf("downgrade keys present on a live-criterion failure: %v", outcome2)
	}
	triage, err := auditRepo.ListForRunByCategory(ctx, fx.runID, server.CategoryAcceptanceTriageDecided)
	if err != nil {
		t.Fatalf("ListForRunByCategory acceptance_triage_decided: %v", err)
	}
	if len(triage) == 0 {
		t.Error("no acceptance_triage_decided entry for a verdict failing a NON-retired criterion")
	}
}

// getPromptRenderRaw fetches GET /v0/stages/{id}/prompt-render and returns the
// raw response body, so a test can assert on wire fields beyond `prompt`.
func getPromptRenderRaw(t *testing.T, ctx context.Context, baseURL string, stageID uuid.UUID) []byte {
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
	return raw
}

// seedAcceptanceDispatch appends the acceptance-dispatch ANCHOR entry
// (acceptance_dispatched, scoped to the stage) that acceptanceValidatedHeadSHA
// requires before it will resolve any reported head — a head entry alone is
// necessary but NOT sufficient (#3124). It is deliberately its own helper rather
// than an overload of seedReportedHead: a dispatch entry is not a head-report
// category, and reusing the head helper would misstate what is being seeded.
// Append it AFTER the head entry so its audit sequence is strictly greater and
// the head falls at-or-before the anchor.
func seedAcceptanceDispatch(t *testing.T, ctx context.Context, repo audit.Repository, runID, stageID uuid.UUID) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"stage_id": stageID.String()})
	if err != nil {
		t.Fatalf("marshal acceptance_dispatched payload: %v", err)
	}
	kind := audit.ActorKind("system")
	if _, err := repo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now(),
		Category:  server.CategoryAcceptanceDispatched,
		ActorKind: &kind,
		Payload:   payload,
	}); err != nil {
		t.Fatalf("AppendChained acceptance_dispatched: %v", err)
	}
}

// shipAcceptanceE2E POSTs an acceptance verdict as the operator bearer and
// returns the decoded 201 body.
func shipAcceptanceE2E(t *testing.T, ctx context.Context, baseURL, token string, runID, stageID uuid.UUID, body []byte) map[string]any {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		baseURL+"/v0/runs/"+runID.String()+"/acceptance?stage_id="+stageID.String(),
		strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("build acceptance request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("acceptance request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("acceptance status %d: %s", resp.StatusCode, raw)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode acceptance response: %v", err)
	}
	return out
}

// latestAcceptanceOutcomeE2E returns the decoded acceptance_outcome_recorded
// payload scoped to the given acceptance stage.
func latestAcceptanceOutcomeE2E(t *testing.T, ctx context.Context, auditRepo audit.Repository, runID, stageID uuid.UUID) map[string]any {
	t.Helper()
	entries, err := auditRepo.ListForRunByCategory(ctx, runID, server.CategoryAcceptanceOutcomeRecorded)
	if err != nil {
		t.Fatalf("ListForRunByCategory acceptance_outcome_recorded: %v", err)
	}
	for i := len(entries) - 1; i >= 0; i-- {
		var p map[string]any
		if err := json.Unmarshal(entries[i].Payload, &p); err != nil {
			continue
		}
		if p["stage_id"] == stageID.String() {
			return p
		}
	}
	t.Fatalf("no acceptance_outcome_recorded entry for stage %s", stageID)
	return nil
}
