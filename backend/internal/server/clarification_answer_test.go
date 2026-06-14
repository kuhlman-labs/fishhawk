package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
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
	rr.getRuns[runID] = &run.Run{ID: runID, Repo: "kuhlman-labs/example", WorkflowID: "feature_change", TriggerSource: run.TriggerCLI}
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
