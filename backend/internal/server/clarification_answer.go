package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// clarificationResumer is the optional repo capability handleAnswerClarification
// uses to close the double-submit race on a parked plan stage. It performs an
// atomic compare-and-set under the stage row lock: awaiting_input → pending,
// reporting won=true only for the single caller that actually moved the stage.
// Concurrent callers that arrive after the stage left awaiting_input get
// won=false (nil error), so exactly one request proceeds to append the
// clarification_answered audit entry. Returns run.ErrNotFound when the stage
// does not exist. Mirrors the runCostRecorder optional-capability pattern
// (trace.go) — a repo that does not implement it (test fakes) falls back to the
// plain TransitionStage path.
type clarificationResumer interface {
	ResumeAwaitingInputStage(ctx context.Context, stageID uuid.UUID) (stage *run.Stage, won bool, err error)
}

// clarificationAnswerRequest mirrors POST /v0/stages/{stage_id}/
// clarification's request body in docs/api/v0.openapi.yaml. The operator
// answers the planner's parked clarification_request questions, keyed by
// question id, plus an optional free-text comment.
type clarificationAnswerRequest struct {
	Answers []clarificationAnswerItem `json:"answers"`
	Comment string                    `json:"comment,omitempty"`
}

// clarificationAnswerItem is one operator answer, matched back to a parked
// question by id.
type clarificationAnswerItem struct {
	ID     string `json:"id"`
	Answer string `json:"answer"`
}

// handleAnswerClarification implements POST /v0/stages/{stage_id}/
// clarification (#1088, the #1057 slice-4/5 answer-and-resume seam).
//
// A plan stage parked at awaiting_input by a clarification_request (#1080)
// is stranded until the operator answers: nothing populates the answers or
// re-opens the stage. This handler closes that gap. It validates the
// operator's answers against the newest clarification_requested entry's
// parked questions, persists them as a DEDICATED clarification_answered
// audit entry (NEVER an approval_submitted / decision=approve entry — the
// stage is not yet approved, and loadApprovalConditions matches only
// decision=='approve', so the two channels stay isolated), transitions the
// stage AwaitingInput → Pending, and hands off to the orchestrator so a
// drive run re-dispatches the plan stage. A local-runner run re-runs via the
// operator's next fishhawk_run_stage plan.
//
// write:approvals is the correct scope: this is the #558 binding-conditions /
// gate-answer family — the operator answering a parked gate.
//
// Failure modes:
//   - non-plan or non-awaiting_input stage   → 409 invalid_state_transition
//   - empty answers                          → 400 validation_failed
//   - unknown / missing / duplicate answer id → 400 clarification_answer_invalid
func (s *Server) handleAnswerClarification(w http.ResponseWriter, r *http.Request) {
	if !s.requireWriteScope(w, r, "write:approvals") {
		return
	}
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "clarification_unconfigured",
			"clarification endpoint requires run and audit repositories", nil)
		return
	}

	stageID, err := uuid.Parse(r.PathValue("stage_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"stage_id must be a valid UUID",
			map[string]any{"field": "stage_id", "got": r.PathValue("stage_id")})
		return
	}

	var req clarificationAnswerRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"request body is not valid JSON or contains unknown fields",
			map[string]any{"error": err.Error()})
		return
	}
	if len(req.Answers) == 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"answers must contain at least one {id, answer}",
			map[string]any{"field": "answers"})
		return
	}

	stage, err := s.cfg.RunRepo.GetStage(r.Context(), stageID)
	if err != nil {
		if errors.Is(err, run.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "stage_not_found",
				"no stage with that id", map[string]any{"stage_id": stageID.String()})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get stage failed", map[string]any{"error": err.Error()})
		return
	}

	// The answer-and-resume seam is plan-only and only re-opens a stage
	// the planner actually parked. A stage in any other type/state has no
	// parked questions to answer — refuse rather than write a stray
	// clarification_answered entry against it.
	if stage.Type != run.StageTypePlan || stage.State != run.StageStateAwaitingInput {
		s.writeError(w, r, http.StatusConflict, "invalid_state_transition",
			"clarification answers are accepted only for a plan stage parked at awaiting_input",
			map[string]any{
				"stage_id":    stageID.String(),
				"stage_type":  string(stage.Type),
				"stage_state": string(stage.State),
			})
		return
	}

	// Validate the answers against the questions the planner actually
	// parked (the newest clarification_requested entry). Keying answers to
	// real question ids keeps the resume prompt unambiguous.
	questions, err := s.loadParkedQuestions(r.Context(), stage.RunID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"load parked clarification questions failed", map[string]any{"error": err.Error()})
		return
	}
	rendered, verr := renderClarificationAnswers(questions, req.Answers, req.Comment)
	if verr != "" {
		s.writeError(w, r, http.StatusBadRequest, "clarification_answer_invalid", verr,
			map[string]any{"stage_id": stageID.String()})
		return
	}

	// Re-open the parked stage FIRST, then persist the audit entry — the
	// transition is the concurrency gate so only the request that actually
	// moves the stage out of awaiting_input writes a clarification_answered
	// entry. Were the audit appended first, two concurrent double-submits
	// could both pass the awaiting_input read above and both append; the
	// loser's newer entry would then override the winner's answer in the
	// resumed plan prompt (loadClarificationAnswers reads newest-first).
	//
	// When the repo provides the ResumeAwaitingInputStage compare-and-set
	// (postgres, under the row lock), use it: the loser observes the stage
	// already re-opened and is rejected here, before any audit write. A repo
	// without the capability (test fakes) falls back to the plain transition,
	// mirroring the runCostRecorder optional-capability pattern.
	if resumer, ok := s.cfg.RunRepo.(clarificationResumer); ok {
		resumed, won, rerr := resumer.ResumeAwaitingInputStage(r.Context(), stageID)
		if rerr != nil {
			if errors.Is(rerr, run.ErrNotFound) {
				s.writeError(w, r, http.StatusNotFound, "stage_not_found",
					"no stage with that id", map[string]any{"stage_id": stageID.String()})
				return
			}
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"resume parked stage failed", map[string]any{"error": rerr.Error()})
			return
		}
		if !won {
			s.writeError(w, r, http.StatusConflict, "invalid_state_transition",
				"stage is no longer parked at awaiting_input (already answered)",
				map[string]any{"stage_id": stageID.String()})
			return
		}
		stage = resumed
	} else {
		// The transition rule already exists (run/transition.go); map an
		// unexpected rejection to a 409 like the approval handler.
		resumed, terr := s.cfg.RunRepo.TransitionStage(r.Context(), stageID, run.StageStatePending, nil)
		if terr != nil {
			var inv run.InvalidTransitionError
			if errors.As(terr, &inv) {
				s.writeError(w, r, http.StatusConflict, "invalid_state_transition",
					terr.Error(),
					map[string]any{"stage_id": stageID.String(), "from": inv.From, "to": inv.To})
				return
			}
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"transition stage to pending failed", map[string]any{"error": terr.Error()})
			return
		}
		stage = resumed
	}

	ident := IdentityFrom(r.Context())
	subject := ident.Subject
	if subject == "" {
		subject = "anonymous"
	}
	actorKind := actorKindForSubject(subject)

	auditPayload := map[string]any{
		"run_id":     stage.RunID.String(),
		"stage_id":   stageID.String(),
		"answers":    req.Answers,
		"conditions": rendered,
	}
	if req.Comment != "" {
		auditPayload["comment"] = req.Comment
	}
	payload, _ := json.Marshal(auditPayload)
	if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:        stage.RunID,
		StageID:      &stageID,
		Timestamp:    time.Now().UTC(),
		Category:     "clarification_answered",
		ActorKind:    &actorKind,
		ActorSubject: &subject,
		Payload:      payload,
	}); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"append clarification_answered audit entry failed", map[string]any{"error": err.Error()})
		return
	}

	// Hand off to the orchestrator so a github_actions/drive run
	// re-dispatches the plan stage. Best-effort, log-on-error — exactly
	// like handleSubmitApproval: the answers are recorded and the stage is
	// re-opened; a local-runner run re-runs via the operator's next
	// fishhawk_run_stage plan (ADR-024: the backend has no execution
	// channel to a host-spawned local runner).
	if s.cfg.Orchestrator != nil {
		if _, err := s.cfg.Orchestrator.Advance(r.Context(), stage.RunID); err != nil {
			s.cfg.Logger.LogAttrs(r.Context(), slog.LevelError,
				"orchestrator advance failed after clarification answer",
				slog.String("run_id", stage.RunID.String()),
				slog.String("stage_id", stage.ID.String()),
				slog.String("error", err.Error()),
			)
		}
	}

	// Sticky status comment (E20.4 / #330): the parked stage just resumed.
	s.notifyStatusUpdate(r.Context(), stage.RunID, "clarification_answer")

	s.writeJSON(w, r, http.StatusOK, toStageResponse(stage))
}

// parkedQuestion is the id + prompt text of one question the planner parked
// in a clarification_request. Used to validate operator answers and to
// render them into the binding-conditions blob.
type parkedQuestion struct {
	ID       string
	Question string
}

// loadParkedQuestions returns the questions from the newest
// clarification_requested audit entry for the run, in their declared order.
// Returns an empty slice (not an error) when no park entry exists or its
// payload doesn't carry questions — renderClarificationAnswers then rejects
// every answer id as unknown, which is the correct outcome for a stage with
// no parked questions.
func (s *Server) loadParkedQuestions(ctx context.Context, runID uuid.UUID) ([]parkedQuestion, error) {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, "clarification_requested")
	if err != nil {
		return nil, fmt.Errorf("list clarification_requested: %w", err)
	}
	if len(entries) == 0 {
		return nil, nil
	}
	// Newest-last (ListForRunByCategory is sequence-ascending): the most
	// recent park holds the live question set.
	newest := entries[len(entries)-1]
	var entryPayload struct {
		ClarificationRequest struct {
			Questions []struct {
				ID       string `json:"id"`
				Question string `json:"question"`
			} `json:"questions"`
		} `json:"clarification_request"`
	}
	if err := json.Unmarshal(newest.Payload, &entryPayload); err != nil {
		return nil, fmt.Errorf("unmarshal clarification_requested payload: %w", err)
	}
	out := make([]parkedQuestion, 0, len(entryPayload.ClarificationRequest.Questions))
	for _, q := range entryPayload.ClarificationRequest.Questions {
		out = append(out, parkedQuestion{ID: q.ID, Question: q.Question})
	}
	return out, nil
}

// renderClarificationAnswers validates answers against the parked questions
// and renders them into a deterministic binding-conditions blob. It returns
// the rendered text and an empty validation message on success, or an empty
// string and a non-empty validation message on the first failure (unknown
// id, duplicate id, or an unanswered question).
//
// The blob is rendered in question-declaration order — `Q<id> (<question>):
// <answer>` per line — so the resume prompt is stable regardless of the order
// answers arrive in. The optional free-text comment follows.
func renderClarificationAnswers(questions []parkedQuestion, answers []clarificationAnswerItem, comment string) (string, string) {
	byID := make(map[string]string, len(answers))
	for _, a := range answers {
		if a.ID == "" {
			return "", "every answer must carry a non-empty question id"
		}
		if _, dup := byID[a.ID]; dup {
			return "", fmt.Sprintf("duplicate answer id %q", a.ID)
		}
		byID[a.ID] = a.Answer
	}

	valid := make(map[string]bool, len(questions))
	for _, q := range questions {
		valid[q.ID] = true
	}
	for _, a := range answers {
		if !valid[a.ID] {
			return "", fmt.Sprintf("answer id %q does not match any parked question", a.ID)
		}
	}
	for _, q := range questions {
		if _, ok := byID[q.ID]; !ok {
			return "", fmt.Sprintf("missing an answer for parked question %q", q.ID)
		}
	}

	var b strings.Builder
	for _, q := range questions {
		fmt.Fprintf(&b, "Q%s (%s): %s\n", q.ID, q.Question, byID[q.ID])
	}
	if comment != "" {
		b.WriteString("\n")
		b.WriteString(comment)
		b.WriteString("\n")
	}
	return b.String(), ""
}
