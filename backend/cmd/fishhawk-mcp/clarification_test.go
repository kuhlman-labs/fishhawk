package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// --- fishhawk_answer_clarification (#1088) ---

func TestAnswerClarification_HappyPath_ResolvesStageAndPostsBody(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), Type: "plan", State: "awaiting_input"},
	}
	r := newResolver(srv, nil)

	_, out, err := r.answerClarification(context.Background(), nil, AnswerClarificationInput{
		RunID: runID.String(),
		Answers: []ClarificationAnswer{
			{ID: "1", Answer: "use the existing endpoint"},
			{ID: "2", Answer: "no migration needed"},
		},
		Comment: "see the issue thread",
	})
	if err != nil {
		t.Fatalf("answerClarification: %v", err)
	}
	if out.StageID != planStageID.String() {
		t.Errorf("StageID = %q, want resolved plan stage %s", out.StageID, planStageID)
	}
	if out.Stage.State != "pending" {
		t.Errorf("Stage.State = %q, want pending (re-opened)", out.Stage.State)
	}
	if fb.clarificationCalledByID[planStageID] != 1 {
		t.Errorf("clarification call count = %d, want 1", fb.clarificationCalledByID[planStageID])
	}
	if len(fb.clarificationBody.Answers) != 2 ||
		fb.clarificationBody.Answers[0].ID != "1" ||
		fb.clarificationBody.Answers[1].Answer != "no migration needed" {
		t.Errorf("backend got Answers = %+v", fb.clarificationBody.Answers)
	}
	if fb.clarificationBody.Comment != "see the issue thread" {
		t.Errorf("backend got Comment = %q", fb.clarificationBody.Comment)
	}
}

func TestAnswerClarification_EmptyAnswers_FailsLocally(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.stagesByRun[runID] = []Stage{{ID: uuid.NewString(), Type: "plan", State: "awaiting_input"}}
	r := newResolver(srv, nil)

	_, _, err := r.answerClarification(context.Background(), nil, AnswerClarificationInput{
		RunID:   runID.String(),
		Answers: nil,
	})
	if err == nil || !strings.Contains(err.Error(), "at least one") {
		t.Fatalf("err = %v, want local empty-answers validation error", err)
	}
	// The short-circuit happens before any HTTP hop.
	for id := range fb.clarificationCalledByID {
		t.Errorf("unexpected clarification call for stage %s", id)
	}
}

func TestAnswerClarification_InvalidRunUUID_FailsLocally(t *testing.T) {
	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.answerClarification(context.Background(), nil, AnswerClarificationInput{
		RunID:   "not-a-uuid",
		Answers: []ClarificationAnswer{{ID: "1", Answer: "x"}},
	})
	if err == nil || !strings.Contains(err.Error(), "not a valid UUID") {
		t.Fatalf("err = %v, want local UUID validation error", err)
	}
}

func TestAnswerClarification_NoPlanStage_Errors(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.stagesByRun[runID] = []Stage{{ID: uuid.NewString(), Type: "implement", State: "pending"}}
	r := newResolver(srv, nil)

	_, _, err := r.answerClarification(context.Background(), nil, AnswerClarificationInput{
		RunID:   runID.String(),
		Answers: []ClarificationAnswer{{ID: "1", Answer: "x"}},
	})
	if err == nil || !strings.Contains(err.Error(), "no plan stage") {
		t.Fatalf("err = %v, want no-plan-stage error", err)
	}
}

func TestAnswerClarification_NotAwaitingInput_MapsActionableError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{{ID: planStageID.String(), Type: "plan", State: "succeeded"}}
	fb.clarificationStatus = http.StatusConflict
	fb.clarificationErrBody = `{"error":{"code":"invalid_state_transition","message":"clarification answers are accepted only for a plan stage parked at awaiting_input","details":{"stage_type":"plan","stage_state":"succeeded"}}}`
	r := newResolver(srv, nil)

	_, _, err := r.answerClarification(context.Background(), nil, AnswerClarificationInput{
		RunID:   runID.String(),
		Answers: []ClarificationAnswer{{ID: "1", Answer: "x"}},
	})
	if err == nil {
		t.Fatal("err = nil, want invalid_state_transition mapping")
	}
	for _, want := range []string{"not parked at awaiting_input", "stage_state=succeeded"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err %q missing %q", err.Error(), want)
		}
	}
}

func TestAnswerClarification_AnswerInvalid_MapsActionableError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{{ID: planStageID.String(), Type: "plan", State: "awaiting_input"}}
	fb.clarificationStatus = http.StatusBadRequest
	fb.clarificationErrBody = `{"error":{"code":"clarification_answer_invalid","message":"answer id \"9\" does not match any parked question"}}`
	r := newResolver(srv, nil)

	_, _, err := r.answerClarification(context.Background(), nil, AnswerClarificationInput{
		RunID:   runID.String(),
		Answers: []ClarificationAnswer{{ID: "9", Answer: "x"}},
	})
	if err == nil {
		t.Fatal("err = nil, want clarification_answer_invalid mapping")
	}
	for _, want := range []string{"clarification_answer_invalid", "does not match any parked question", "exactly one answer"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err %q missing %q", err.Error(), want)
		}
	}
}
