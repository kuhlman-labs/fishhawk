package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// newClarificationServer wires RunRepo + AuditRepo for the answer-and-resume
// handler and seeds a plan stage at awaiting_input plus the
// clarification_requested park entry holding the questions.
func newClarificationServer(t *testing.T, runID, stageID uuid.UUID, state run.StageState, stageType run.StageType) (*Server, *promptRunRepo, *auditFake) {
	t.Helper()
	rr := newPromptRunRepo()
	au := newAuditFake()
	rr.getStages[stageID] = &run.Stage{ID: stageID, RunID: runID, Type: stageType, State: state}
	rr.getRuns[runID] = &run.Run{ID: runID, Repo: "kuhlman-labs/example", WorkflowID: "feature_change"}
	seedClarificationRequested(au, runID, stageID)
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: au})
	return s, rr, au
}

// seedClarificationRequested adds a clarification_requested park entry with a
// single parked question (id "auth-backend"), mirroring the payload
// handleClarificationRequest writes.
func seedClarificationRequested(au *auditFake, runID, stageID uuid.UUID) {
	rid := runID
	sid := stageID
	payload, _ := json.Marshal(map[string]any{
		"run_id":   runID.String(),
		"stage_id": stageID.String(),
		"clarification_request": map[string]any{
			"kind":    "clarification_request",
			"summary": "needs an operator decision",
			"questions": []map[string]any{
				{"id": "auth-backend", "question": "Which auth backend should the token store use?", "recommended_default": "Postgres", "tradeoffs": "x"},
			},
		},
	})
	au.seeded = append(au.seeded, &audit.Entry{
		RunID:    &rid,
		StageID:  &sid,
		Category: "clarification_requested",
		Payload:  payload,
	})
}

// answerClarification posts a clarification answer body to the handler with an
// authenticated session identity (bypasses the scope guard like the approval
// tests).
func answerClarification(t *testing.T, s *Server, stageID uuid.UUID, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost,
		"/v0/stages/"+stageID.String()+"/clarification", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("stage_id", stageID.String())
	w := httptest.NewRecorder()
	s.handleAnswerClarification(w, withAuth(req))
	return w
}

func TestAnswerClarification_HappyPath_ResumesStage(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, rr, au := newClarificationServer(t, runID, stageID, run.StageStateAwaitingInput, run.StageTypePlan)

	w := answerClarification(t, s, stageID,
		`{"answers":[{"id":"auth-backend","answer":"Postgres"}],"comment":"go with the default"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	var got stageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.State != string(run.StageStatePending) {
		t.Errorf("State = %q, want pending", got.State)
	}

	// Stage transitioned AwaitingInput → Pending.
	var sawResume bool
	for _, c := range rr.transitionStageCalls {
		if c.To == run.StageStatePending {
			sawResume = true
		}
	}
	if !sawResume {
		t.Errorf("stage was not transitioned to pending; transitions=%v", rr.transitionStageCalls)
	}

	// A dedicated clarification_answered entry was written — never an
	// approval_submitted / decision=approve entry.
	if len(au.appended) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(au.appended))
	}
	got0 := au.appended[0]
	if got0.Category != "clarification_answered" {
		t.Errorf("audit category = %q, want clarification_answered", got0.Category)
	}
	if bytes.Contains(got0.Payload, []byte(`"decision"`)) {
		t.Errorf("clarification_answered payload must not carry a decision: %s", got0.Payload)
	}
	if !bytes.Contains(got0.Payload, []byte("Postgres")) {
		t.Errorf("audit payload missing the rendered answer: %s", got0.Payload)
	}
}

// casClarificationRepo wraps promptRunRepo with a compare-and-set
// ResumeAwaitingInputStage that models the postgres row-lock CAS: the first
// call wins (returns a pending stage + won=true), every later call loses
// (won=false). It deliberately leaves the embedded getStages entry at
// awaiting_input so a second submit still passes the handler's read-time
// pre-check — reproducing the double-submit TOCTOU window where both requests
// observe the stage parked and the CAS is the sole gate.
type casClarificationRepo struct {
	*promptRunRepo
	resumeCalls int
}

func (r *casClarificationRepo) ResumeAwaitingInputStage(_ context.Context, id uuid.UUID) (*run.Stage, bool, error) {
	st, ok := r.getStages[id]
	if !ok {
		return nil, false, run.ErrNotFound
	}
	r.resumeCalls++
	if r.resumeCalls > 1 {
		return st, false, nil // lost the race: another request already re-opened it
	}
	resumed := *st
	resumed.State = run.StageStatePending
	return &resumed, true, nil
}

// TestAnswerClarification_DoubleSubmit_OnlyFirstWins covers the double-submit /
// transition-race path (#1088 fixup): two answers race past the awaiting_input
// read, but the compare-and-set re-open admits exactly one. The loser is
// rejected 409 BEFORE any audit write, so only the winner's
// clarification_answered entry exists and the resumed prompt cannot be
// overridden by the failed request.
func TestAnswerClarification_DoubleSubmit_OnlyFirstWins(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	rr := &casClarificationRepo{promptRunRepo: newPromptRunRepo()}
	au := newAuditFake()
	rr.getStages[stageID] = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypePlan, State: run.StageStateAwaitingInput}
	rr.getRuns[runID] = &run.Run{ID: runID, Repo: "kuhlman-labs/example", WorkflowID: "feature_change"}
	seedClarificationRequested(au, runID, stageID)
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: au})

	// First submit wins the CAS and persists its answer.
	w1 := answerClarification(t, s, stageID, `{"answers":[{"id":"auth-backend","answer":"Postgres"}]}`)
	if w1.Code != http.StatusOK {
		t.Fatalf("first submit status = %d, want 200:\n%s", w1.Code, w1.Body.String())
	}

	// Second submit (different answer) loses the CAS even though it passed the
	// read-time pre-check — it must be rejected and must NOT append.
	w2 := answerClarification(t, s, stageID, `{"answers":[{"id":"auth-backend","answer":"in-memory"}]}`)
	if w2.Code != http.StatusConflict {
		t.Fatalf("second submit status = %d, want 409:\n%s", w2.Code, w2.Body.String())
	}
	if !bytes.Contains(w2.Body.Bytes(), []byte("invalid_state_transition")) {
		t.Errorf("second submit missing invalid_state_transition: %s", w2.Body.String())
	}

	// Exactly one clarification_answered entry, and it is the winner's — the
	// loser never overrode the resumed answer.
	if len(au.appended) != 1 {
		t.Fatalf("audit entries = %d, want 1 (loser must not append)", len(au.appended))
	}
	if !bytes.Contains(au.appended[0].Payload, []byte("Postgres")) {
		t.Errorf("winning answer overwritten or missing: %s", au.appended[0].Payload)
	}
	if bytes.Contains(au.appended[0].Payload, []byte("in-memory")) {
		t.Errorf("loser's answer leaked into the persisted entry: %s", au.appended[0].Payload)
	}
}

func TestAnswerClarification_NonPlanStage_409(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, _, _ := newClarificationServer(t, runID, stageID, run.StageStateAwaitingInput, run.StageTypeImplement)

	w := answerClarification(t, s, stageID, `{"answers":[{"id":"auth-backend","answer":"Postgres"}]}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("invalid_state_transition")) {
		t.Errorf("error code missing invalid_state_transition: %s", w.Body.String())
	}
}

func TestAnswerClarification_NotAwaitingInput_409(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, _, _ := newClarificationServer(t, runID, stageID, run.StageStateRunning, run.StageTypePlan)

	w := answerClarification(t, s, stageID, `{"answers":[{"id":"auth-backend","answer":"Postgres"}]}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("invalid_state_transition")) {
		t.Errorf("error code missing invalid_state_transition: %s", w.Body.String())
	}
}

func TestAnswerClarification_EmptyAnswers_400(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, _, _ := newClarificationServer(t, runID, stageID, run.StageStateAwaitingInput, run.StageTypePlan)

	w := answerClarification(t, s, stageID, `{"answers":[]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("validation_failed")) {
		t.Errorf("error code missing validation_failed: %s", w.Body.String())
	}
}

func TestAnswerClarification_UnknownID_400(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, _, au := newClarificationServer(t, runID, stageID, run.StageStateAwaitingInput, run.StageTypePlan)

	w := answerClarification(t, s, stageID, `{"answers":[{"id":"nope","answer":"x"}]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("clarification_answer_invalid")) {
		t.Errorf("error code missing clarification_answer_invalid: %s", w.Body.String())
	}
	// An invalid answer writes no audit entry and does not resume the stage.
	if len(au.appended) != 0 {
		t.Errorf("invalid answer should append no audit entry; got %d", len(au.appended))
	}
}

func TestAnswerClarification_MissingAnswer_400(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	// Seed a second parked question so a single answer leaves one unanswered.
	s, _, au := newClarificationServer(t, runID, stageID, run.StageStateAwaitingInput, run.StageTypePlan)
	rid := runID
	payload, _ := json.Marshal(map[string]any{
		"clarification_request": map[string]any{
			"questions": []map[string]any{
				{"id": "auth-backend", "question": "q1"},
				{"id": "storage", "question": "q2"},
			},
		},
	})
	// Replace the seeded park entry with the two-question one (newest wins).
	au.seeded = append(au.seeded, &audit.Entry{RunID: &rid, Category: "clarification_requested", Payload: payload})

	w := answerClarification(t, s, stageID, `{"answers":[{"id":"auth-backend","answer":"Postgres"}]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("clarification_answer_invalid")) {
		t.Errorf("error code missing clarification_answer_invalid: %s", w.Body.String())
	}
}

func TestAnswerClarification_DuplicateID_400(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, _, _ := newClarificationServer(t, runID, stageID, run.StageStateAwaitingInput, run.StageTypePlan)

	w := answerClarification(t, s, stageID,
		`{"answers":[{"id":"auth-backend","answer":"Postgres"},{"id":"auth-backend","answer":"in-memory"}]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("clarification_answer_invalid")) {
		t.Errorf("error code missing clarification_answer_invalid: %s", w.Body.String())
	}
}

func TestAnswerClarification_StageNotFound_404(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, rr, _ := newClarificationServer(t, runID, stageID, run.StageStateAwaitingInput, run.StageTypePlan)
	delete(rr.getStages, stageID)

	w := answerClarification(t, s, stageID, `{"answers":[{"id":"auth-backend","answer":"Postgres"}]}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("stage_not_found")) {
		t.Errorf("error code missing stage_not_found: %s", w.Body.String())
	}
}

// TestAnswerClarification_CrossLayer_ParkAnswerResumePrompt is the required
// cross-boundary e2e (#1088): a plan stage parked at awaiting_input is answered
// through the endpoint (request → clarification_answered audit), the stage
// resumes to Pending, and GET /v0/stages/{id}/prompt renders the operator's
// answers in the binding "Clarification answers" section (audit → prompt
// render). It crosses request → persistence → prompt-render, so the seam is
// covered, not just the per-layer units.
func TestAnswerClarification_CrossLayer_ParkAnswerResumePrompt(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()

	sf := newSigningFake()
	priv, _ := sf.issue(t, runID)
	rr := newPromptRunRepo()
	au := newAuditFake()
	rr.getStages[stageID] = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypePlan, State: run.StageStateAwaitingInput}
	rr.getRuns[runID] = &run.Run{ID: runID, Repo: "kuhlman-labs/example", WorkflowID: "feature_change", TriggerSource: run.TriggerCLI, RequiresCharter: chFalse()}
	seedClarificationRequested(au, runID, stageID)

	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: au, SigningRepo: sf})
	s.promptIssueGetterOverride = &stubIssueGetter{}

	// POST the answer.
	w := answerClarification(t, s, stageID,
		`{"answers":[{"id":"auth-backend","answer":"Use the Postgres token store"}],"comment":"migration is acceptable"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("answer status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if got := rr.getStages[stageID].State; got != run.StageStatePending {
		t.Fatalf("stage state after answer = %q, want pending", got)
	}

	// GET the resumed plan prompt: the answers must render in the binding
	// "Clarification answers" section.
	pw := promptRequest(t, s, runID, stageID, priv, "")
	if pw.Code != http.StatusOK {
		t.Fatalf("prompt status = %d, want 200:\n%s", pw.Code, pw.Body.String())
	}
	var resp promptResponse
	if err := json.NewDecoder(pw.Body).Decode(&resp); err != nil {
		t.Fatalf("decode prompt: %v", err)
	}
	if !strings.Contains(resp.Prompt, "Clarification answers (binding") {
		t.Errorf("resumed plan prompt missing the Clarification answers section:\n%s", resp.Prompt)
	}
	if !strings.Contains(resp.Prompt, "Use the Postgres token store") {
		t.Errorf("resumed plan prompt missing the operator's answer text:\n%s", resp.Prompt)
	}
	if !strings.Contains(resp.Prompt, "migration is acceptable") {
		t.Errorf("resumed plan prompt missing the operator's comment:\n%s", resp.Prompt)
	}
}

// TestLoadClarificationAnswers_NewestWins confirms the loader returns the most
// recent clarification_answered entry's rendered conditions and caps the blob.
func TestLoadClarificationAnswers_NewestWins(t *testing.T) {
	runID := uuid.New()
	au := newAuditFake()
	rid := runID
	older, _ := json.Marshal(map[string]any{"conditions": "OLD answers"})
	newer, _ := json.Marshal(map[string]any{"conditions": "NEW answers"})
	au.seeded = append(au.seeded,
		&audit.Entry{RunID: &rid, Category: "clarification_answered", Payload: older},
		&audit.Entry{RunID: &rid, Category: "clarification_answered", Payload: newer},
	)
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au})

	got := s.loadClarificationAnswers(context.Background(), runID)
	if got == nil {
		t.Fatal("loadClarificationAnswers returned nil, want the newest blob")
	}
	if *got != "NEW answers" {
		t.Errorf("loaded conditions = %q, want %q", *got, "NEW answers")
	}

	// No entries → nil.
	if s.loadClarificationAnswers(context.Background(), uuid.New()) != nil {
		t.Error("loadClarificationAnswers should be nil when no entry exists")
	}
}

// TestLoadClarificationAnswers_TruncatesOversizedBlob exercises the residual
// cap, retargeted from the historical 4000 to prompt.MaxClarificationAnswerBytes
// (#3063). Two halves:
//
//   - the #3063 live payload size (8256 bytes, three answers plus a trailing
//     comment) now survives WHOLE where the old cap destroyed its tail;
//   - a blob genuinely over the new cap is still truncated with the marker, so
//     a pathological answer set can never blow up the resumed plan prompt.
func TestLoadClarificationAnswers_TruncatesOversizedBlob(t *testing.T) {
	load := func(t *testing.T, blob string) *string {
		t.Helper()
		runID := uuid.New()
		au := newAuditFake()
		rid := runID
		payload, _ := json.Marshal(map[string]any{"conditions": blob})
		au.seeded = append(au.seeded,
			&audit.Entry{RunID: &rid, Category: "clarification_answered", Payload: payload},
		)
		s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au})
		return s.loadClarificationAnswers(context.Background(), runID)
	}

	t.Run("live-shaped 8256-byte blob survives whole", func(t *testing.T) {
		// The exact #3063 loss: the LAST answer and the trailing comment are
		// the bytes the 4000-byte cut destroyed first.
		const lastAnswer = "LAST_ANSWER_MARKER_7984A5A8"
		const comment = "TRAILING_COMMENT_MARKER_7984A5A8"
		blob := "Qa (first?): " + strings.Repeat("a", 4000) + "\n" +
			"Qb (second?): " + strings.Repeat("b", 4000) + "\n" +
			"Qc (third?): " + strings.Repeat("c", 151) + " " + lastAnswer + "\n\n" + comment + "\n"
		if len(blob) < 8256 {
			t.Fatalf("fixture is %d bytes, want at least the 8256-byte #3063 payload", len(blob))
		}
		got := load(t, blob)
		if got == nil {
			t.Fatal("loadClarificationAnswers returned nil, want the whole blob")
		}
		if *got != blob {
			t.Errorf("the %d-byte live-shaped blob was altered by the loader (got %d bytes)", len(blob), len(*got))
		}
		for _, want := range []string{lastAnswer, comment} {
			if !strings.Contains(*got, want) {
				t.Errorf("the loader dropped %q — the exact bytes #3063 lost at the old 4000-byte cap", want)
			}
		}
		if strings.Contains(*got, "...[truncated]") {
			t.Errorf("an under-cap blob was marked truncated")
		}
	})

	t.Run("over the new cap is still truncated", func(t *testing.T) {
		oversized := strings.Repeat("x", prompt.MaxClarificationAnswerBytes+500)
		got := load(t, oversized)
		if got == nil {
			t.Fatal("loadClarificationAnswers returned nil, want the truncated blob")
		}
		want := strings.Repeat("x", prompt.MaxClarificationAnswerBytes) + "...[truncated]"
		if *got != want {
			t.Errorf("blob not truncated: len=%d, want %d + marker", len(*got), prompt.MaxClarificationAnswerBytes)
		}
	})
}

// overCapClarificationAnswer builds an answer string sized BY CONSTRUCTION so
// that renderClarificationAnswers' output for the single seeded parked question
// lands exactly `overBy` bytes past prompt.MaxClarificationAnswerBytes.
//
// It derives the size from the constant and the renderer's own framing rather
// than calling the validator in the fixture: a fixture that consulted the
// control under test would make a deleted control fail in SETUP, and the RED
// would land on the fixture instead of on the behavioral assertion.
func overCapClarificationAnswer(t *testing.T, overBy int) string {
	t.Helper()
	// renderClarificationAnswers emits "Q<id> (<question>): <answer>\n" — the
	// framing counts toward the cap, which is why the gate measures the
	// RENDERED blob and not the operator's raw text.
	framing := len("Qauth-backend (Which auth backend should the token store use?): ") + len("\n")
	n := prompt.MaxClarificationAnswerBytes + overBy - framing
	if n <= 0 {
		t.Fatalf("framing %d already exceeds the cap; fixture cannot be built", framing)
	}
	return strings.Repeat("x", n)
}

// TestAnswerClarification_OverCapAnswersRefused is CONTROL 1's error-identity
// half (#3063): the handler refuses a rendered blob one byte over the cap with
// 400 validation_failed naming bytes / max_bytes / overflow_bytes. This is
// necessary but NOT sufficient — the control's real effect is COMMITTED STATE,
// which the sibling test below reads.
func TestAnswerClarification_OverCapAnswersRefused(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, _, _ := newClarificationServer(t, runID, stageID, run.StageStateAwaitingInput, run.StageTypePlan)

	body, _ := json.Marshal(clarificationAnswerRequest{
		Answers: []clarificationAnswerItem{{ID: "auth-backend", Answer: overCapClarificationAnswer(t, 1)}},
	})
	w := answerClarification(t, s, stageID, string(body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	var errBody struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("decode error body: %v\n%s", err, w.Body.String())
	}
	if errBody.Error.Code != "validation_failed" {
		t.Errorf("code = %q, want validation_failed", errBody.Error.Code)
	}
	wantDetails := map[string]float64{
		"bytes":          float64(prompt.MaxClarificationAnswerBytes + 1),
		"max_bytes":      float64(prompt.MaxClarificationAnswerBytes),
		"overflow_bytes": 1,
	}
	for k, want := range wantDetails {
		got, ok := errBody.Error.Details[k].(float64)
		if !ok {
			t.Errorf("details[%q] absent or not a number: %v", k, errBody.Error.Details[k])
			continue
		}
		if got != want {
			t.Errorf("details[%q] = %v, want %v", k, got, want)
		}
	}
	if !strings.Contains(errBody.Error.Message, "re-answerable") {
		t.Errorf("the refusal message must tell the operator the stage is re-answerable: %q", errBody.Error.Message)
	}
}

// TestAnswerClarification_OverCapAnswersRefused_StageStillParkedNoAudit is
// CONTROL 1's load-bearing half. The refusal's effect is COMMITTED STATE, and a
// control that fired and was then rolled back would return a byte-identical
// error — so this reads the state AFTER the call returns: no
// clarification_answered entry, the stage still plan/awaiting_input, and a
// subsequent UNDER-cap answer through the same handler SUCCEEDS, proving the
// stage is genuinely re-answerable rather than merely reported as such.
//
// Deleting the validateClarificationAnswers call site in
// handleAnswerClarification reddens the persisted-entry assertion here, which
// no error-identity check could catch.
func TestAnswerClarification_OverCapAnswersRefused_StageStillParkedNoAudit(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, rr, au := newClarificationServer(t, runID, stageID, run.StageStateAwaitingInput, run.StageTypePlan)

	body, _ := json.Marshal(clarificationAnswerRequest{
		Answers: []clarificationAnswerItem{{ID: "auth-backend", Answer: overCapClarificationAnswer(t, 1)}},
		Comment: "and one more thing",
	})
	// Deliberately NON-fatal: the point of this test is the COMMITTED-STATE
	// reads below, which an error-identity assertion cannot make. If the status
	// assertion were fatal, deleting the control would redden this test on its
	// precondition and the state assertions would never run — leaving the claim
	// that they bite unproven.
	if w := answerClarification(t, s, stageID, string(body)); w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}

	// (a) Nothing was recorded.
	answered, err := au.ListForRunByCategory(context.Background(), runID, "clarification_answered")
	if err != nil {
		t.Fatalf("ListForRunByCategory: %v", err)
	}
	if len(answered) != 0 {
		t.Fatalf("clarification_answered entries after the refused call = %d, want 0 — the refusal must precede every stateful step", len(answered))
	}

	// (b) The stage never moved: no transition was even attempted.
	for _, c := range rr.transitionStageCalls {
		t.Errorf("the refused call attempted a stage transition to %s", c.To)
	}
	st, err := rr.GetStage(context.Background(), stageID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	if st.Type != run.StageTypePlan || st.State != run.StageStateAwaitingInput {
		t.Errorf("stage is %s/%s, want plan/awaiting_input (still parked)", st.Type, st.State)
	}

	// (c) The stage is genuinely re-answerable: an under-cap answer succeeds.
	ok, _ := json.Marshal(clarificationAnswerRequest{
		Answers: []clarificationAnswerItem{{ID: "auth-backend", Answer: "Postgres"}},
	})
	w := answerClarification(t, s, stageID, string(ok))
	if w.Code != http.StatusOK {
		t.Fatalf("the re-answer status = %d, want 200 — the refusal must consume nothing:\n%s", w.Code, w.Body.String())
	}
	answered, err = au.ListForRunByCategory(context.Background(), runID, "clarification_answered")
	if err != nil {
		t.Fatalf("ListForRunByCategory (post re-answer): %v", err)
	}
	if len(answered) != 1 {
		t.Errorf("clarification_answered entries after the re-answer = %d, want exactly 1", len(answered))
	}
}

// TestAnswerClarification_UnderCapAnswersAccepted is the control the issue
// demands: an ordinary submission — including one at the #3063 live payload
// size, which the OLD cap would have cut — still returns 200, transitions to
// pending, and writes exactly one clarification_answered entry carrying the
// answer WHOLE. A refusal that fires too eagerly is caught here.
func TestAnswerClarification_UnderCapAnswersAccepted(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, _, au := newClarificationServer(t, runID, stageID, run.StageStateAwaitingInput, run.StageTypePlan)

	const tailMarker = "ANSWER_TAIL_MARKER_7984A5A8"
	answer := "Postgres. " + strings.Repeat("a", 8000) + " " + tailMarker
	body, _ := json.Marshal(clarificationAnswerRequest{
		Answers: []clarificationAnswerItem{{ID: "auth-backend", Answer: answer}},
		Comment: "go with the default",
	})
	w := answerClarification(t, s, stageID, string(body))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for an under-cap submission:\n%s", w.Code, w.Body.String())
	}
	var got stageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.State != string(run.StageStatePending) {
		t.Errorf("State = %q, want pending", got.State)
	}
	if len(au.appended) != 1 {
		t.Fatalf("audit entries = %d, want exactly 1", len(au.appended))
	}
	if au.appended[0].Category != "clarification_answered" {
		t.Errorf("audit category = %q, want clarification_answered", au.appended[0].Category)
	}
	if !bytes.Contains(au.appended[0].Payload, []byte(tailMarker)) {
		t.Errorf("the persisted payload lost the answer's tail %q — an 8000-byte answer must be stored whole", tailMarker)
	}
}

// TestValidateClarificationAnswers_Boundary pins the > not >= boundary the
// validator shares with CapText: a rendered blob of EXACTLY the cap is
// admissible; one byte more is not.
func TestValidateClarificationAnswers_Boundary(t *testing.T) {
	atCap := strings.Repeat("x", prompt.MaxClarificationAnswerBytes)
	if ok, msg, details := validateClarificationAnswers(atCap); !ok {
		t.Errorf("an exactly-at-cap blob was refused: %s %v", msg, details)
	}
	ok, msg, details := validateClarificationAnswers(atCap + "x")
	if ok {
		t.Fatalf("a one-byte-over blob was admitted")
	}
	if details["overflow_bytes"] != 1 {
		t.Errorf("overflow_bytes = %v, want 1", details["overflow_bytes"])
	}
	if !strings.Contains(msg, "LAST answer") {
		t.Errorf("the refusal message must name WHICH answer a cut destroys first: %q", msg)
	}
}

// committerClarificationRepo wraps promptRunRepo with the
// ResumeAwaitingInputAndAppend combined committer (#1090): the postgres tier
// that folds the transition AND the clarification_answered append into one
// transaction. The fake records the append it receives so the test asserts the
// standalone AuditRepo.AppendChained is NOT used in this tier (no double write)
// and that a loser/error path appends nothing.
type committerClarificationRepo struct {
	*promptRunRepo
	won      bool
	err      error
	appended []audit.ChainAppendParams
	calls    int
}

func (r *committerClarificationRepo) ResumeAwaitingInputAndAppend(_ context.Context, id uuid.UUID, p audit.ChainAppendParams) (*run.Stage, bool, error) {
	r.calls++
	if r.err != nil {
		return nil, false, r.err
	}
	st, ok := r.getStages[id]
	if !ok {
		return nil, false, run.ErrNotFound
	}
	if !r.won {
		// Loser of the CAS: no transition, no append.
		return st, false, nil
	}
	// Winner: append in the same (notional) tx and report the resumed stage.
	r.appended = append(r.appended, p)
	resumed := *st
	resumed.State = run.StageStatePending
	r.getStages[id] = &resumed
	return &resumed, true, nil
}

func newCommitterServer(t *testing.T, runID, stageID uuid.UUID, won bool, cerr error) (*Server, *committerClarificationRepo, *auditFake) {
	t.Helper()
	rr := &committerClarificationRepo{promptRunRepo: newPromptRunRepo(), won: won, err: cerr}
	au := newAuditFake()
	rr.getStages[stageID] = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypePlan, State: run.StageStateAwaitingInput}
	rr.getRuns[runID] = &run.Run{ID: runID, Repo: "kuhlman-labs/example", WorkflowID: "feature_change"}
	seedClarificationRequested(au, runID, stageID)
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: au})
	return s, rr, au
}

// TestAnswerClarification_AtomicTier_HappyPath covers the
// clarificationAnswerCommitter dispatch: the combined committer resumes the
// stage and persists the answer in one call, so the handler does NOT also call
// the standalone AuditRepo.AppendChained (no duplicate audit write).
func TestAnswerClarification_AtomicTier_HappyPath(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, rr, au := newCommitterServer(t, runID, stageID, true, nil)

	w := answerClarification(t, s, stageID,
		`{"answers":[{"id":"auth-backend","answer":"Postgres"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	var got stageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.State != string(run.StageStatePending) {
		t.Errorf("State = %q, want pending", got.State)
	}
	if rr.calls != 1 {
		t.Errorf("committer calls = %d, want 1", rr.calls)
	}
	// The committer folded the append; the standalone AuditRepo must not be
	// used for the clarification_answered entry.
	if len(rr.appended) != 1 {
		t.Errorf("committer appended = %d, want 1", len(rr.appended))
	}
	if rr.appended[0].Category != "clarification_answered" {
		t.Errorf("committed entry category = %q, want clarification_answered", rr.appended[0].Category)
	}
	if len(au.appended) != 0 {
		t.Errorf("standalone AuditRepo.AppendChained called %d times in atomic tier, want 0", len(au.appended))
	}
}

// TestAnswerClarification_AtomicTier_Loser409 covers the single-winner CAS in
// the committer tier: won=false (a concurrent double-submit already re-opened
// the stage) returns 409 invalid_state_transition with no orphaned append.
func TestAnswerClarification_AtomicTier_Loser409(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, rr, au := newCommitterServer(t, runID, stageID, false, nil)

	w := answerClarification(t, s, stageID,
		`{"answers":[{"id":"auth-backend","answer":"Postgres"}]}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("invalid_state_transition")) {
		t.Errorf("missing invalid_state_transition: %s", w.Body.String())
	}
	if len(rr.appended) != 0 {
		t.Errorf("loser appended %d entries, want 0", len(rr.appended))
	}
	if len(au.appended) != 0 {
		t.Errorf("loser triggered %d standalone appends, want 0", len(au.appended))
	}
}

// TestAnswerClarification_AtomicTier_Error500 covers an unexpected committer
// error (not ErrNotFound, not a CAS loss) surfacing as a 500.
func TestAnswerClarification_AtomicTier_Error500(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, _, au := newCommitterServer(t, runID, stageID, true, errors.New("tx aborted"))

	w := answerClarification(t, s, stageID,
		`{"answers":[{"id":"auth-backend","answer":"Postgres"}]}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500:\n%s", w.Code, w.Body.String())
	}
	if len(au.appended) != 0 {
		t.Errorf("error path triggered %d standalone appends, want 0", len(au.appended))
	}
}
