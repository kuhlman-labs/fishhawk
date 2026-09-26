package mcpserver

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
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

// TestAnswerClarification_OverCap_MapsByteAccountingError pins the #3063
// over-cap arm: the backend's 400 validation_failed carries bytes / max_bytes /
// overflow_bytes in details, and the tool error must surface all three plus the
// consumed-nothing guarantee, so the operator can resize without a second round
// trip. Deleting the details-reading branch drops the byte accounting and the
// "still parked" clause, reddening this.
func TestAnswerClarification_OverCap_MapsByteAccountingError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{{ID: planStageID.String(), Type: "plan", State: "awaiting_input"}}
	fb.clarificationStatus = http.StatusBadRequest
	fb.clarificationErrBody = `{"error":{"code":"validation_failed","message":"clarification answers render to 12500 bytes; the maximum is 12000","details":{"field":"answers","bytes":12500,"max_bytes":12000,"overflow_bytes":500}}}`
	r := newResolver(srv, nil)

	_, _, err := r.answerClarification(context.Background(), nil, AnswerClarificationInput{
		RunID:   runID.String(),
		Answers: []ClarificationAnswer{{ID: "1", Answer: "x"}},
	})
	if err == nil {
		t.Fatal("err = nil, want the over-cap validation_failed mapping")
	}
	for _, want := range []string{
		"validation_failed", "12500 bytes", "maximum is 12000", "500 over",
		"nothing was recorded", "still parked at awaiting_input",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err %q missing %q", err.Error(), want)
		}
	}
}

// TestAnswerClarification_ValidationFailedWithoutDetails_FallsBack is the
// fallback control: a validation_failed carrying NO byte details (a malformed
// body, a bad stage id) must degrade to the bare server message rather than
// printing zeros the handler invented.
func TestAnswerClarification_ValidationFailedWithoutDetails_FallsBack(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{{ID: planStageID.String(), Type: "plan", State: "awaiting_input"}}
	fb.clarificationStatus = http.StatusBadRequest
	fb.clarificationErrBody = `{"error":{"code":"validation_failed","message":"stage_id must be a valid UUID"}}`
	r := newResolver(srv, nil)

	_, _, err := r.answerClarification(context.Background(), nil, AnswerClarificationInput{
		RunID:   runID.String(),
		Answers: []ClarificationAnswer{{ID: "1", Answer: "x"}},
	})
	if err == nil {
		t.Fatal("err = nil, want the bare validation_failed mapping")
	}
	if got := err.Error(); got != "validation_failed: stage_id must be a valid UUID" {
		t.Errorf("err = %q, want the bare server message with no invented byte accounting", got)
	}
}

// TestAnswerClarification_ToolDescriptionDocumentsTheCap pins the issue's core
// complaint: the cap must be DISCOVERABLE from the tool description, not only
// from a refusal after the fact. The assertions are on the cap VALUE and the
// refuse-not-truncate posture, not on a full sentence a copy-edit would delete.
func TestAnswerClarification_ToolDescriptionDocumentsTheCap(t *testing.T) {
	ctx := context.Background()
	srv := mcp.NewServer(&mcp.Implementation{Name: "fishhawk", Version: "test"}, nil)
	registerAnswerClarification(srv, nil)

	// Drive a real ListTools round-trip so the assertion runs against the
	// WIRE-VISIBLE description the operator's agent actually reads, not the
	// in-process registration struct.
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := srv.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer func() { _ = serverSession.Close() }()
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer func() { _ = clientSession.Close() }()

	res, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var desc string
	for _, tool := range res.Tools {
		if tool.Name == "fishhawk_answer_clarification" {
			desc = tool.Description
		}
	}
	if desc == "" {
		t.Fatal("fishhawk_answer_clarification is not wire-visible")
	}
	for _, want := range []string{"12000 bytes", "REFUSED", "re-answerable"} {
		if !strings.Contains(desc, want) {
			t.Errorf("fishhawk_answer_clarification description missing %q:\n%s", want, desc)
		}
	}
}
