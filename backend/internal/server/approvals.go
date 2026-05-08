package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcomplete"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
	"github.com/kuhlman-labs/fishhawk/backend/internal/stagecheck"
)

// AuditCompleteCheckName is the reserved name for the
// `fishhawk_audit_complete` blocking check (#229). Stage gates
// declare it like any other check; the backend self-derives its
// state from artifact + audit-log presence rather than pulling it
// from the stage_checks table.
const AuditCompleteCheckName = "fishhawk_audit_complete"

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

	// Identity is set by the bearerAuth middleware (E4.5).
	// Anonymous callers can't approve once the demo loop is past
	// the bootstrap phase; in v0 we still accept anonymous
	// submissions and tag them so the audit trail is honest about
	// who acted (or didn't).
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

	// Authorization: when a RoleResolver is wired, the subject
	// must be in the gate's approvers list. Without the resolver,
	// any authenticated subject can approve — the v0 demo posture
	// before role resolution lands. See E4.4 (#50).
	if !s.checkApproverAuthorization(w, r, stage, subject) {
		return
	}

	// Enforce gate's blocking_checks (#228). Approve refuses with
	// 409 when any declared check isn't `pass`; reject is always
	// allowed (rejecting a stage with failing checks is the path
	// the failing checks were intended to surface). When the
	// stage-check repo isn't wired or the stage has no gate, the
	// enforcement falls open — the legacy v0 deployments without
	// check ingestion shouldn't refuse every approve.
	if decision == approval.DecisionApprove {
		if blockers, ok := s.checkBlockingChecks(w, r, stage); !ok {
			return
		} else if len(blockers) > 0 {
			s.writeError(w, r, http.StatusConflict, "blocking_checks_not_passed",
				"one or more blocking checks have not passed",
				map[string]any{
					"stage_id": stageID.String(),
					"blockers": blockers,
				})
			return
		}
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
		stage, err = s.advanceStage(r.Context(), stageID, decision)
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

		// Hand off to the orchestrator on both approve AND reject
		// — approve dispatches the next stage; reject walks the
		// run's state machine to terminal (pending → running →
		// failed). Without the reject path the run would stay in
		// pending forever once an approver rejected.
		if s.cfg.Orchestrator != nil {
			if _, err := s.cfg.Orchestrator.Advance(r.Context(), stage.RunID); err != nil {
				// Don't fail the approval: the gate did pass /
				// reject, the audit row is in place. Surface
				// the orchestration failure in logs and let a
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
// decision: approve → succeeded, reject → failed-D. The reject
// path delegates to run.FailStage so the failure pattern is
// identical to the SLA path and the trace-time policy path
// (E8.1 #39).
func (s *Server) advanceStage(ctx context.Context, stageID uuid.UUID, decision approval.Decision) (*run.Stage, error) {
	switch decision {
	case approval.DecisionApprove:
		return s.cfg.RunRepo.TransitionStage(ctx, stageID,
			run.StageStateSucceeded, nil)
	case approval.DecisionReject:
		return run.FailStage(ctx, s.cfg.RunRepo, stageID,
			run.FailureD, "gate rejected by approver")
	}
	// Unreachable — decision was validated earlier.
	return nil, errors.New("approval: unknown decision (programmer error)")
}

// checkApproverAuthorization returns true when subject is allowed
// to act on the stage's gate. Returns false (and writes a 403 / 500
// response) on denial. With no RoleResolver configured the function
// returns true — any authenticated caller can approve. That's the
// v0 demo posture; production deployments wire a Resolver and a
// real subject (GitHub login).
//
// "Allowed" means: the stage's first approval gate's approvers
// resolve (via spec roles + GitHub teams) to a set that includes
// subject. For all_of-style approvers, every named role must
// contain subject.
//
// checkBlockingChecks reads the latest state of each declared
// blocking check on the stage's gate and returns the names of any
// that aren't `pass`. The bool return is the "no error" flag —
// false means this function already wrote the response (typically
// a 500 from a stage-check repo failure) and the caller should
// short-circuit.
//
// Falls open when:
//   - StageCheckRepo isn't configured (legacy deployments without
//     check ingestion shouldn't refuse every approve).
//   - The stage has no gate, or its gate has no blocking_checks.
//
// Per the issue's "no bypass flag" posture, an approver who really
// needs to approve over a failing check should change the spec to
// remove the check from blocking_checks rather than override here.
func (s *Server) checkBlockingChecks(w http.ResponseWriter, r *http.Request, stage *run.Stage) ([]string, bool) {
	if s.cfg.StageCheckRepo == nil {
		return nil, true
	}
	if stage.Gate == nil || len(stage.Gate.BlockingChecks) == 0 {
		return nil, true
	}
	var blockers []string
	for _, name := range stage.Gate.BlockingChecks {
		// fishhawk_audit_complete is self-derived (#229), not read
		// from the stage_checks table. The backend computes its
		// state on demand from artifact + audit-log presence so
		// it always reflects the freshest run state.
		if name == AuditCompleteCheckName {
			state, err := s.deriveAuditCompleteState(r.Context(), stage.RunID)
			if err != nil {
				s.writeError(w, r, http.StatusInternalServerError, "internal_error",
					"derive audit-complete state failed",
					map[string]any{"stage_id": stage.ID.String(), "check": name, "error": err.Error()})
				return nil, false
			}
			if state != stagecheck.StatePass {
				blockers = append(blockers, name)
			}
			continue
		}
		c, err := s.cfg.StageCheckRepo.LatestForStageAndName(r.Context(), stage.ID, name)
		if err != nil {
			if errors.Is(err, stagecheck.ErrNotFound) {
				// Never observed — treat as a blocker. The SPA
				// renders this as `not_tracked`; an approver
				// shouldn't be able to clear a gate that hasn't
				// reported a state.
				blockers = append(blockers, name)
				continue
			}
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"read stage check failed",
				map[string]any{"stage_id": stage.ID.String(), "check": name, "error": err.Error()})
			return nil, false
		}
		if c.State != stagecheck.StatePass {
			blockers = append(blockers, name)
		}
	}
	return blockers, true
}

// deriveAuditCompleteState calls auditcomplete.Compute and returns
// just the state. Falls open (returns pass) when the artifact or
// audit repo isn't configured — same posture as the other check-
// derivation paths in the v0 demo loop, where missing infra
// shouldn't refuse every approve. The full (state, missing) pair
// is exposed through GET /v0/stages/{id}/checks; this helper exists
// for the gate-enforcement read path that only cares about pass/
// not-pass.
//
// Side-effect: publishes the (state, missing) pair to GitHub as a
// Check Run (#231) so the same gate that refuses approve here also
// shows up on the PR's checks panel and (when wired to branch
// protection) blocks the merge button. Best-effort.
func (s *Server) deriveAuditCompleteState(ctx context.Context, runID uuid.UUID) (stagecheck.State, error) {
	if s.cfg.ArtifactRepo == nil || s.cfg.AuditRepo == nil {
		return stagecheck.StatePass, nil
	}
	state, missing, err := auditcomplete.Compute(ctx, runID, auditcomplete.Deps{
		Runs:      s.cfg.RunRepo,
		Artifacts: s.cfg.ArtifactRepo,
		Audit:     s.cfg.AuditRepo,
	})
	if err != nil {
		return "", err
	}
	s.publishAuditCheck(ctx, runID, state, missing)
	return state, nil
}

// Lookups (spec fetch, team fetch) happen on the request path.
// Spec fetch is one GitHub API call; team membership is cached by
// the resolver. Acceptable for v0 traffic; a follow-up can move
// the spec parse into a per-run cache.
func (s *Server) checkApproverAuthorization(w http.ResponseWriter, r *http.Request, stage *run.Stage, subject string) bool {
	if s.cfg.RoleResolver == nil {
		return true
	}
	if s.cfg.RunRepo == nil || s.workflowSpecFetcher() == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "approver_check_unconfigured",
			"role-based approver check requires RunRepo and GitHub client", nil)
		return false
	}

	gate, err := s.fetchGateForStage(r.Context(), stage)
	if err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"approval: fetch gate failed",
			slog.String("stage_id", stage.ID.String()),
			slog.String("error", err.Error()),
		)
		// Best-effort: a spec fetch failure shouldn't black-hole
		// approvals during a GitHub flap. Allow the submission
		// and let the trail through writeApprovalAudit reflect
		// reality. Operators with stricter budgets can flip a
		// follow-up flag once the spec-cache work lands.
		return true
	}
	if gate == nil || gate.approvers == nil {
		// Stage isn't gated by approval (gate type=check or no
		// gates). Submit-anyway is consistent with the v0 demo
		// where every agent stage carries an implicit approval.
		return true
	}

	allowed, err := s.cfg.RoleResolver.CanApprove(r.Context(), gate.installationID, gate.approvers, gate.roles, subject)
	if err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"approval: role resolution failed",
			slog.String("stage_id", stage.ID.String()),
			slog.String("subject", subject),
			slog.String("error", err.Error()),
		)
		// Same best-effort posture: don't lock up the gate when
		// upstream is flaky.
		return true
	}
	if !allowed {
		s.writeError(w, r, http.StatusForbidden, "approver_not_authorized",
			"subject is not in the gate's approvers list",
			map[string]any{"subject": subject})
		return false
	}
	return true
}

// gateContext carries the bits of the workflow spec the role
// check needs: the gate's approvers, the spec's roles map, and
// the run's installation_id (so the resolver can reach GitHub).
type gateContext struct {
	approvers      *spec.Approvers
	roles          map[string]spec.Role
	installationID int64
}

// fetchGateForStage fetches the workflow spec at the stage's
// run.WorkflowSHA and returns the gate context. Returns
// (nil, nil) when the stage exists in the spec but has no
// approval gate.
func (s *Server) fetchGateForStage(ctx context.Context, stage *run.Stage) (*gateContext, error) {
	runRow, err := s.cfg.RunRepo.GetRun(ctx, stage.RunID)
	if err != nil {
		return nil, fmt.Errorf("get run: %w", err)
	}
	if runRow.InstallationID == nil {
		return nil, errors.New("run missing installation_id")
	}
	repo, err := parseRepoOwnerName(runRow.Repo)
	if err != nil {
		return nil, err
	}
	ref := runRow.WorkflowSHA
	if ref == "" {
		ref = "main"
	}
	specFile, err := s.workflowSpecFetcher().GetWorkflowSpec(ctx, *runRow.InstallationID, repo, ref)
	if err != nil {
		return nil, fmt.Errorf("get workflow spec: %w", err)
	}
	parsed, err := spec.ParseBytes(specFile.Content)
	if err != nil {
		return nil, fmt.Errorf("parse workflow spec: %w", err)
	}
	wf, ok := parsed.Workflows[runRow.WorkflowID]
	if !ok {
		return nil, fmt.Errorf("workflow %q not in spec", runRow.WorkflowID)
	}
	for _, stg := range wf.Stages {
		if string(stg.Type) != string(stage.Type) {
			continue
		}
		for _, gate := range stg.Gates {
			if gate.Type == spec.GateTypeApproval && gate.Approvers != nil {
				return &gateContext{
					approvers:      gate.Approvers,
					roles:          parsed.Roles,
					installationID: *runRow.InstallationID,
				}, nil
			}
		}
		// Stage exists but has no approval gate.
		return &gateContext{roles: parsed.Roles, installationID: *runRow.InstallationID}, nil
	}
	return nil, fmt.Errorf("stage_type %q not in workflow %q", stage.Type, runRow.WorkflowID)
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
