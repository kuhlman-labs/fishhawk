package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// approvalRequest mirrors POST /v0/stages/{stage_id}/approvals's
// request body in docs/api/v0.openapi.yaml.
type approvalRequest struct {
	Decision string `json:"decision"`
	Comment  string `json:"comment,omitempty"`
}

// handleSubmitApproval implements POST /v0/stages/{stage_id}/approvals.
//
// Per the OpenAPI contract:
//   - approve transitions the stage to succeeded
//   - reject fails the stage as category D (gate didn't pass —
//     same category SLA timeout uses, since both mean "no human
//     approval")
//
// Idempotency: a re-submission from the same authenticated subject
// returns the existing approval and the current stage state with
// a 200. The first decision wins for any_of-style gates.
func (s *Server) handleSubmitApproval(w http.ResponseWriter, r *http.Request) {
	if s.cfg.ApprovalRepo == nil || s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "approvals_unconfigured",
			"approvals endpoint requires approval, run, and audit repositories", nil)
		return
	}

	stageID, err := uuid.Parse(r.PathValue("stage_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"stage_id must be a valid UUID",
			map[string]any{"field": "stage_id", "got": r.PathValue("stage_id")})
		return
	}

	var req approvalRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"request body is not valid JSON or contains unknown fields",
			map[string]any{"error": err.Error()})
		return
	}

	decision := approval.Decision(req.Decision)
	if !decision.Valid() {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"decision must be 'approve' or 'reject'",
			map[string]any{"field": "decision", "got": req.Decision})
		return
	}

	// Identity comes from the authStub middleware until E4 lands
	// real auth. The stub returns "anonymous" — captured verbatim
	// in the approval record so future audits can see the gap.
	ident := IdentityFrom(r.Context())
	subject := ident.Subject
	if subject == "" {
		subject = "anonymous"
	}

	var commentPtr *string
	if req.Comment != "" {
		commentPtr = &req.Comment
	}

	// Confirm the stage exists before recording. Lets us 404 cleanly
	// rather than INSERTing an approval against a non-existent
	// foreign key.
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

	res, err := s.cfg.ApprovalRepo.Submit(r.Context(), approval.SubmitParams{
		StageID:         stageID,
		ApproverSubject: subject,
		Decision:        decision,
		Comment:         commentPtr,
		Surface:         approval.SurfaceAPI,
	})
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"submit approval failed", map[string]any{"error": err.Error()})
		return
	}

	// Only the FIRST submission for this approver triggers a stage
	// transition. Subsequent submissions return the prior decision
	// and the stage's current state.
	if res.Inserted {
		stage, err = s.advanceStage(r, stageID, decision)
		if err != nil {
			var inv run.InvalidTransitionError
			if errors.As(err, &inv) {
				s.writeError(w, r, http.StatusConflict, "invalid_state_transition",
					err.Error(),
					map[string]any{"stage_id": stageID.String(),
						"from": inv.From, "to": inv.To})
				return
			}
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"transition stage failed", map[string]any{"error": err.Error()})
			return
		}

		s.writeApprovalAudit(r, stage, res.Approval)

		// On approve, hand off to the orchestrator to dispatch
		// the next stage (or transition the run to succeeded if
		// this was the last stage). On reject we don't advance —
		// the run is over and the orchestrator would no-op
		// against the now-terminal state anyway.
		if decision == approval.DecisionApprove && s.cfg.Orchestrator != nil {
			if _, err := s.cfg.Orchestrator.Advance(r.Context(), stage.RunID); err != nil {
				// Don't fail the approval: the gate did pass,
				// the audit row is in place. Surface the
				// orchestration failure in logs and let a
				// follow-up call recover.
				s.cfg.Logger.LogAttrs(r.Context(), slog.LevelError,
					"orchestrator advance failed",
					slog.String("run_id", stage.RunID.String()),
					slog.String("stage_id", stage.ID.String()),
					slog.String("error", err.Error()),
				)
			}
		}
	}

	s.writeJSON(w, r, http.StatusOK, toStageResponse(stage))
}

// advanceStage applies the state-machine transition for the
// decision: approve → succeeded, reject → failed-D. The repo's
// TransitionStage wraps the SELECT FOR UPDATE + UPDATE in a
// transaction, so concurrent decisions can't fork the chain.
func (s *Server) advanceStage(r *http.Request, stageID uuid.UUID, decision approval.Decision) (*run.Stage, error) {
	switch decision {
	case approval.DecisionApprove:
		return s.cfg.RunRepo.TransitionStage(r.Context(), stageID,
			run.StageStateSucceeded, nil)
	case approval.DecisionReject:
		category := run.FailureD
		reason := "gate rejected by approver"
		return s.cfg.RunRepo.TransitionStage(r.Context(), stageID,
			run.StageStateFailed,
			&run.StageCompletion{
				FailureCategory: &category,
				FailureReason:   &reason,
			})
	}
	// Unreachable — decision was validated earlier.
	return nil, errors.New("approval: unknown decision (programmer error)")
}

// writeApprovalAudit appends an entry tying the decision to the
// run. Best-effort: a failure logs but doesn't unwind, since the
// approval is already recorded.
func (s *Server) writeApprovalAudit(r *http.Request, stage *run.Stage, app *approval.Approval) {
	systemKind := audit.ActorKind("user")
	payload, _ := json.Marshal(map[string]any{
		"stage_id": stage.ID.String(),
		"decision": string(app.Decision),
		"surface":  string(app.Surface),
		"approver": app.ApproverSubject,
	})

	approver := app.ApproverSubject
	if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:        stage.RunID,
		StageID:      &stage.ID,
		Timestamp:    time.Now().UTC(),
		Category:     "approval_submitted",
		ActorKind:    &systemKind,
		ActorSubject: &approver,
		Payload:      payload,
	}); err != nil {
		s.cfg.Logger.Error("audit append failed for approval",
			"run_id", stage.RunID,
			"stage_id", stage.ID,
			"error", err.Error(),
		)
	}
}
