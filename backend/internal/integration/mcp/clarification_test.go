package mcpe2e_test

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"

	"net/http/httptest"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The three questions the planner parks. Their declaration ORDER is the
// rendering order server.renderClarificationAnswers uses, which is why the
// LAST one's answer is the byte #3063's 4000-byte cut destroyed first.
var clarificationQuestions = []struct {
	id       string
	question string
}{
	{"auth-backend", "Which auth backend should the token store use?"},
	{"migration", "Do we migrate existing rows in place?"},
	{"rollout", "What is the rollout order?"},
}

// clarificationHarness stands up the cross-component rig the two #3063 cases
// drive: a backend over the fixture's Postgres pool with the artifact, audit,
// signing and approval repos plus GitHub wired (prompt-render short-circuits to
// 503 without an issueGetter), a plan stage walked to awaiting_input, the
// clarification_requested park entry carrying the three questions, and a
// connected MCP client speaking to the REAL fishhawk-mcp binary.
//
// It is defined in-file rather than by editing the shared e2e_test.go /
// revise_test.go, exactly as reviseHarness is, so this file is self-contained
// on top of the shared e2eFixture.
func clarificationHarness(t *testing.T, ctx context.Context, fx *e2eFixture) (audit.Repository, *runpkg.Stage, *mcp.ClientSession, string) {
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
	parkAtAwaitingInput(t, ctx, fx, planStage.ID)
	seedClarificationRequest(t, ctx, auditRepo, fx.runID, planStage.ID)

	session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, httpSrv.URL)
	return auditRepo, planStage, session, httpSrv.URL
}

// parkAtAwaitingInput walks a freshly-created plan stage pending → dispatched →
// running → awaiting_input, the production order a planner emitting a
// clarification_request takes. The shared parkAtGate lands on
// awaiting_approval, which is a DIFFERENT gate and not answerable.
func parkAtAwaitingInput(t *testing.T, ctx context.Context, fx *e2eFixture, stageID uuid.UUID) {
	t.Helper()
	for _, to := range []runpkg.StageState{
		runpkg.StageStateDispatched,
		runpkg.StageStateRunning,
		runpkg.StageStateAwaitingInput,
	} {
		if _, err := fx.runRepo.TransitionStage(ctx, stageID, to, nil); err != nil {
			t.Fatalf("TransitionStage → %s: %v", to, err)
		}
	}
}

// seedClarificationRequest appends the clarification_requested park entry whose
// questions the answer handler validates against.
func seedClarificationRequest(t *testing.T, ctx context.Context, repo audit.Repository, runID, stageID uuid.UUID) {
	t.Helper()
	questions := make([]map[string]any, 0, len(clarificationQuestions))
	for _, q := range clarificationQuestions {
		questions = append(questions, map[string]any{
			"id":                  q.id,
			"question":            q.question,
			"recommended_default": "none",
			"tradeoffs":           "see the issue",
		})
	}
	payload, err := json.Marshal(map[string]any{
		"run_id":   runID.String(),
		"stage_id": stageID.String(),
		"clarification_request": map[string]any{
			"kind":      "clarification_request",
			"summary":   "not yet plannable",
			"questions": questions,
		},
	})
	if err != nil {
		t.Fatalf("marshal clarification_requested payload: %v", err)
	}
	kind := audit.ActorKind("agent")
	if _, err := repo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  "clarification_requested",
		ActorKind: &kind,
		Payload:   payload,
	}); err != nil {
		t.Fatalf("AppendChained clarification_requested: %v", err)
	}
}

// renderedAnswers mirrors server.renderClarificationAnswers byte-for-byte for
// the seeded question set: one "Q<id> (<question>): <answer>\n" line in
// declaration order, then a blank line and the trailing comment. The E2E
// assertion compares the PERSISTED payload against this, so a rendering change
// on either side is caught rather than papered over by a substring check.
func renderedAnswers(answers map[string]string, comment string) string {
	var b strings.Builder
	for _, q := range clarificationQuestions {
		b.WriteString("Q" + q.id + " (" + q.question + "): " + answers[q.id] + "\n")
	}
	if comment != "" {
		b.WriteString("\n" + comment + "\n")
	}
	return b.String()
}

// callAnswerClarification drives the REAL fishhawk_answer_clarification MCP tool
// and returns the raw result, leaving the error/success decision to the caller
// (one case wants the success, the other wants the refusal).
func callAnswerClarification(t *testing.T, ctx context.Context, session *mcp.ClientSession,
	runID interface{ String() string }, answers map[string]string, comment string) *mcp.CallToolResult {
	t.Helper()
	items := make([]map[string]any, 0, len(clarificationQuestions))
	for _, q := range clarificationQuestions {
		items = append(items, map[string]any{"id": q.id, "answer": answers[q.id]})
	}
	args := map[string]any{"run_id": runID.String(), "answers": items}
	if comment != "" {
		args["comment"] = comment
	}
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "fishhawk_answer_clarification",
		Arguments: args,
	})
	if err != nil {
		t.Fatalf("CallTool fishhawk_answer_clarification: %v", err)
	}
	return res
}

// TestE2E_Clarification_AnswersInjectedWhole is the cross-component done-means
// for #3063. Per-layer units pass while the seam breaks — this issue IS a seam
// failure (cf. #618): the answers were STORED whole and TRUNCATED on the way
// into the prompt, so only a test spanning the persistence half and the
// rendering half can catch the disagreement.
//
// It drives the REAL fishhawk-mcp binary → the real backend HTTP answer gate →
// real Postgres → the deterministic prompt renderer, with a live-shaped
// ~8256-byte three-answer-plus-comment payload (the #3063 loss size), and
// asserts the two halves whose disagreement WAS the bug:
//
//   - the clarification_answered audit payload stores the rendered blob
//     BYTE-FOR-BYTE;
//   - the resumed plan prompt carries the LAST answer AND the trailing comment
//     with NO truncation or elision marker — the exact bytes #3063 lost.
func TestE2E_Clarification_AnswersInjectedWhole(t *testing.T) {
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	auditRepo, planStage, session, baseURL := clarificationHarness(t, ctx, fx)

	const lastAnswerMarker = "LAST_ANSWER_MARKER_7984A5A8"
	const commentMarker = "TRAILING_COMMENT_MARKER_7984A5A8"
	answers := map[string]string{
		"auth-backend": "Postgres. " + strings.Repeat("a", 2700),
		"migration":    "Yes, in place. " + strings.Repeat("b", 2700),
		"rollout":      "Backend first. " + strings.Repeat("c", 2700) + " " + lastAnswerMarker,
	}
	comment := commentMarker + " — ship it behind the existing flag."
	want := renderedAnswers(answers, comment)
	if len(want) < 8256 {
		t.Fatalf("fixture renders to %d bytes, want at least the 8256-byte #3063 payload", len(want))
	}
	if len(want) > prompt.MaxClarificationAnswerBytes {
		t.Fatalf("fixture renders to %d bytes, over the %d cap — it must be accepted whole",
			len(want), prompt.MaxClarificationAnswerBytes)
	}

	res := callAnswerClarification(t, ctx, session, fx.runID, answers, comment)
	if res.IsError {
		t.Fatalf("answer_clarification returned error: %s", toolContentString(t, res))
	}
	var out struct {
		Stage struct {
			ID    string `json:"id"`
			State string `json:"state"`
			Type  string `json:"type"`
		} `json:"stage"`
		StageID string `json:"stage_id"`
	}
	decodeStructured(t, res, &out)
	if out.Stage.ID != planStage.ID.String() {
		t.Errorf("answered stage id = %q, want %s", out.Stage.ID, planStage.ID)
	}
	if out.Stage.State != string(runpkg.StageStatePending) {
		t.Errorf("stage state = %q, want pending (re-opened)", out.Stage.State)
	}

	// Half one: the persisted blob is byte-for-byte what the operator submitted.
	entries, err := auditRepo.ListForRunByCategory(ctx, fx.runID, "clarification_answered")
	if err != nil {
		t.Fatalf("ListForRunByCategory: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("clarification_answered entries = %d, want 1", len(entries))
	}
	var stored struct {
		Conditions string `json:"conditions"`
	}
	if err := json.Unmarshal(entries[0].Payload, &stored); err != nil {
		t.Fatalf("unmarshal clarification_answered payload: %v", err)
	}
	if stored.Conditions != want {
		t.Errorf("the persisted conditions blob is not byte-for-byte the submission: got %d bytes, want %d",
			len(stored.Conditions), len(want))
	}

	// Half two: the RENDERED prompt carries the whole thing. This is where
	// #3063 lost the tail while half one looked perfectly healthy.
	rendered := getPromptRender(t, ctx, baseURL, planStage.ID)
	if !strings.Contains(rendered, "### Clarification answers (binding") {
		t.Fatalf("rendered prompt missing the clarification-answers section:\n%s", rendered)
	}
	if !strings.Contains(rendered, want) {
		t.Errorf("the rendered prompt does not carry the %d-byte answers blob whole", len(want))
	}
	for _, marker := range []string{lastAnswerMarker, commentMarker} {
		if !strings.Contains(rendered, marker) {
			t.Errorf("the rendered prompt lost %q — the exact bytes #3063 dropped at the old 4000-byte cap", marker)
		}
	}
	for _, bad := range []string{"...[truncated]", "...[ELIDED"} {
		if strings.Contains(rendered, bad) {
			t.Errorf("the rendered prompt carries the truncation artifact %q for an under-cap blob", bad)
		}
	}
	// No residual-truncation audit row either: nothing was cut.
	trunc, err := auditRepo.ListForRunByCategory(ctx, fx.runID, server.CategoryClarificationAnswersTruncated)
	if err != nil {
		t.Fatalf("ListForRunByCategory(truncated): %v", err)
	}
	if len(trunc) != 0 {
		t.Errorf("clarification_answers_truncated entries = %d, want 0 for an under-cap blob", len(trunc))
	}
}

// TestE2E_Clarification_OverCapRefusedStageStillParked is the refusal half
// across the same real seam. Error IDENTITY alone is insufficient here — a
// control that fired and was then rolled back returns a byte-identical tool
// error — so after asserting the refusal surfaces THROUGH the MCP tool naming
// bytes/cap/overflow, it reads the COMMITTED STATE: zero clarification_answered
// entries, the stage still plan/awaiting_input, and a following UNDER-cap
// answer through the same tool succeeding.
func TestE2E_Clarification_OverCapRefusedStageStillParked(t *testing.T) {
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	auditRepo, planStage, session, _ := clarificationHarness(t, ctx, fx)

	over := map[string]string{
		"auth-backend": strings.Repeat("a", 5000),
		"migration":    strings.Repeat("b", 5000),
		"rollout":      strings.Repeat("c", 5000),
	}
	if n := len(renderedAnswers(over, "")); n <= prompt.MaxClarificationAnswerBytes {
		t.Fatalf("fixture renders to %d bytes, not over the %d cap — the refusal assertion would be vacuous",
			n, prompt.MaxClarificationAnswerBytes)
	}
	res := callAnswerClarification(t, ctx, session, fx.runID, over, "")
	if !res.IsError {
		t.Fatalf("an over-cap answer set was ACCEPTED through the MCP tool; want a refusal")
	}
	msg := toolContentString(t, res)
	for _, want := range []string{
		"validation_failed",
		strconv.Itoa(prompt.MaxClarificationAnswerBytes),
		"nothing was recorded",
		"still parked at awaiting_input",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("tool error missing %q:\n%s", want, msg)
		}
	}

	// COMMITTED STATE — the assertions an error identity cannot make.
	entries, err := auditRepo.ListForRunByCategory(ctx, fx.runID, "clarification_answered")
	if err != nil {
		t.Fatalf("ListForRunByCategory: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("clarification_answered entries after the refused call = %d, want 0 (the refusal must precede every stateful step)", len(entries))
	}
	st, err := fx.runRepo.GetStage(ctx, planStage.ID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	if st.Type != runpkg.StageTypePlan || st.State != runpkg.StageStateAwaitingInput {
		t.Fatalf("stage is %s/%s after the refusal, want plan/awaiting_input (still parked)", st.Type, st.State)
	}

	// Re-answerable: an under-cap submission through the SAME tool succeeds,
	// proving the refusal consumed nothing.
	okAnswers := map[string]string{
		"auth-backend": "Postgres",
		"migration":    "Yes, in place",
		"rollout":      "Backend first",
	}
	okRes := callAnswerClarification(t, ctx, session, fx.runID, okAnswers, "")
	if okRes.IsError {
		t.Fatalf("the follow-up under-cap answer was refused: %s", toolContentString(t, okRes))
	}
	entries, err = auditRepo.ListForRunByCategory(ctx, fx.runID, "clarification_answered")
	if err != nil {
		t.Fatalf("ListForRunByCategory (post re-answer): %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("clarification_answered entries after the re-answer = %d, want exactly 1", len(entries))
	}
}
