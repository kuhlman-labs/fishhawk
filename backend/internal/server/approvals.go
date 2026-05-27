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

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
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
	if !s.requireWriteScope(w, r, "write:approvals") {
		return
	}
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

	// ADR-018 (#311, #313): review-stage approval is owned by GitHub.
	// The PR merge event (#312) transitions the stage to succeeded;
	// branch protection's required-reviewers enforces the approver
	// list. Refuse the in-Fishhawk submission with a 409 + the PR
	// URL so the caller knows where the merge gate actually lives.
	// Plan-stage approvals are unaffected — Fishhawk's vote at plan
	// time is independent and has no GitHub-side equivalent.
	if stage.Type == run.StageTypeReview {
		s.rejectReviewStageApproval(w, r, stage)
		return
	}

	// Authorization: when a RoleResolver is wired, the subject
	// must be in the gate's approvers list. Without the resolver,
	// any authenticated subject can approve — the v0 demo posture
	// before role resolution lands. See E4.4 (#50).
	if !s.checkApproverAuthorization(w, r, stage, subject) {
		return
	}

	// ADR-017 (#249, #253): the approval handler no longer gates on
	// stage_check state. Reviewers approve based on plan + diff;
	// GitHub branch protection blocks the merge until the required
	// checks (including fishhawk_audit_complete, published as a
	// Check Run per #231) report green. The live check state is
	// still rendered on the review page via GET /v0/stages/{id}/
	// checks — it's informational, not a gate.

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
		// Budget check: plan-stage approvals are gated on the plan's
		// predicted runtime fitting within the spec-resolved implement-
		// stage timeout. Decomposition or --override-budget bypasses
		// the check; a bare over-budget plan returns 422.
		if decision == approval.DecisionApprove && stage.Type == run.StageTypePlan {
			if !s.checkPlanBudget(w, r, stage, req.Comment) {
				return
			}
		}

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

		s.writeApprovalAudit(r, stage, res.Approval, req.Comment)

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

		// Plan-comment re-render (#377): a plan-stage approve or
		// reject re-fires the plan-on-issue hook, which edits the
		// existing comment in place (when the spec opts in to
		// `update_on_change`) and appends a `_Status:_` footer
		// naming the actor. The retired NotifyPlanApproved
		// broadcast (#274) used to live here; it duplicated what
		// the plan-comment edit + sticky status comment already
		// surface. Best-effort: notifyPlanReady logs but never
		// unwinds the approval.
		if stage.Type == run.StageTypePlan {
			s.notifyPlanReady(r.Context(), stage.RunID, stage)
		}

		// Sticky status comment (E20.4 / #330). Every approval —
		// approve or reject, plan stage or otherwise — changes the
		// run's surface state and is worth surfacing in the issue
		// thread.
		s.notifyStatusUpdate(r.Context(), stage.RunID, "approval_submit")
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

// Lookups (spec fetch, team fetch) happen on the request path.
// Spec fetch is one GitHub API call; team membership is cached by
// the resolver. Acceptable for v0 traffic; a follow-up can move
// the spec parse into a per-run cache.
func (s *Server) checkApproverAuthorization(w http.ResponseWriter, r *http.Request, stage *run.Stage, subject string) bool {
	if s.cfg.RoleResolver == nil {
		return true
	}
	if s.cfg.RunRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "approver_check_unconfigured",
			"role-based approver check requires RunRepo", nil)
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

// fetchGateForStage loads the workflow spec from the run row's
// cached bytes (#283) and returns the gate context. Returns
// (nil, nil) when the stage exists in the spec but has no
// approval gate.
//
// Pre-#283 this called GitHub directly using `runRow.WorkflowSHA`
// as the contents-API ref, but that's a blob SHA, not a commit
// ref — every call 404'd in production. checkApproverAuthorization
// falls open on fetch failure, so the role check was being silently
// bypassed for every approval. The cache fixes both call sites
// (this one + the trace handler's policy re-eval).
func (s *Server) fetchGateForStage(ctx context.Context, stage *run.Stage) (*gateContext, error) {
	runRow, err := s.cfg.RunRepo.GetRun(ctx, stage.RunID)
	if err != nil {
		return nil, fmt.Errorf("get run: %w", err)
	}
	if runRow.InstallationID == nil {
		return nil, errors.New("run missing installation_id")
	}
	if len(runRow.WorkflowSpec) == 0 {
		return nil, errors.New("run has no cached workflow spec (legacy or non-dispatcher run)")
	}
	parsed, err := spec.ParseBytes(runRow.WorkflowSpec)
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

// rejectReviewStageApproval returns a 409 explaining that review-
// stage approval moved to GitHub per ADR-018 (#311). The error
// body carries the PR URL when the run row has one stamped so the
// caller can point a misbehaving client at the right surface.
//
// 409 (not 410) because the resource still exists — only the
// action against this stage type is no longer valid. Plan-stage
// approvals continue to use the same endpoint.
func (s *Server) rejectReviewStageApproval(w http.ResponseWriter, r *http.Request, stage *run.Stage) {
	details := map[string]any{
		"stage_id":   stage.ID.String(),
		"stage_type": string(stage.Type),
	}
	if s.cfg.RunRepo != nil {
		if runRow, err := s.cfg.RunRepo.GetRun(r.Context(), stage.RunID); err == nil &&
			runRow.PullRequestURL != nil {
			details["pull_request_url"] = *runRow.PullRequestURL
		}
	}
	s.writeError(w, r, http.StatusConflict, "review_stage_managed_by_github",
		"review-stage approval is recorded from PR-side events (ADR-018); merge or review the PR on GitHub",
		details)
}

// writeApprovalAudit appends an entry tying the decision to the
// run. Best-effort: a failure logs but doesn't unwind, since the
// approval is already recorded.
// When decision is reject and the comment contains "--decompose",
// reject_reason=decompose_required is added to the payload so the
// next plan-stage prompt can inject a decompose-required hint.
func (s *Server) writeApprovalAudit(r *http.Request, stage *run.Stage, app *approval.Approval, comment string) {
	systemKind := audit.ActorKind("user")
	auditPayload := map[string]any{
		"stage_id": stage.ID.String(),
		"decision": string(app.Decision),
		"surface":  string(app.Surface),
		"approver": app.ApproverSubject,
	}
	if app.Decision == approval.DecisionReject && strings.Contains(comment, "--decompose") {
		auditPayload["reject_reason"] = "decompose_required"
	}
	if app.Decision == approval.DecisionReject && comment != "" {
		auditPayload["rejection_comment"] = comment
	}
	payload, _ := json.Marshal(auditPayload)

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

// resolveSpecStageForRun parses the run's cached WorkflowSpec and
// finds the spec.Stage whose ID or Type matches stageType. Returns
// the parent Workflow, the matched Stage, and the timeout source
// string used for audit payloads. When WorkflowSpec is absent the
// function returns zero values with timeoutSource="backend_default"
// and nil error — callers fall through to spec.DefaultStageTimeout.
func resolveSpecStageForRun(runRow *run.Run, stageType run.StageType) (spec.Workflow, spec.Stage, string, error) {
	if len(runRow.WorkflowSpec) == 0 {
		return spec.Workflow{}, spec.Stage{}, "backend_default", nil
	}
	parsed, err := spec.ParseBytes(runRow.WorkflowSpec)
	if err != nil {
		return spec.Workflow{}, spec.Stage{}, "", fmt.Errorf("parse workflow spec: %w", err)
	}
	wf, ok := parsed.Workflows[runRow.WorkflowID]
	if !ok {
		return spec.Workflow{}, spec.Stage{}, "", fmt.Errorf("workflow %q not in spec", runRow.WorkflowID)
	}

	// Primary match: spec stage ID == string(stageType).
	var specStage spec.Stage
	for _, st := range wf.Stages {
		if st.ID == string(stageType) {
			specStage = st
			break
		}
	}
	// Fallback: spec stage Type == stageType.
	if specStage.ID == "" {
		for _, st := range wf.Stages {
			if string(st.Type) == string(stageType) {
				specStage = st
				break
			}
		}
	}

	timeoutSource := "backend_default"
	if specStage.Executor.Timeout.Duration != 0 {
		timeoutSource = "stage_executor_timeout"
	} else if wf.Policy != nil && wf.Policy.MaxStageRuntime.Duration != 0 {
		timeoutSource = "workflow_policy_max_stage_runtime"
	}
	return wf, specStage, timeoutSource, nil
}

// checkPlanBudget enforces the budget gate on plan-stage approvals.
// Returns true when the approval should proceed; returns false (and
// writes the error response) when the plan's predicted runtime
// exceeds the spec-resolved implement-stage timeout and neither
// decomposition nor --override-budget is present in the comment.
//
// When ArtifactRepo is nil or no plan is found (race / manual run),
// the check is skipped and the approval proceeds.
func (s *Server) checkPlanBudget(w http.ResponseWriter, r *http.Request, stage *run.Stage, comment string) bool {
	if s.cfg.ArtifactRepo == nil {
		return true
	}

	runRow, err := s.cfg.RunRepo.GetRun(r.Context(), stage.RunID)
	if err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn, "budget check: get run failed",
			slog.String("stage_id", stage.ID.String()),
			slog.String("error", err.Error()),
		)
		return true
	}

	wf, specStage, timeoutSource, err := resolveSpecStageForRun(runRow, stage.Type)
	if err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn, "budget check: resolve spec stage failed",
			slog.String("stage_id", stage.ID.String()),
			slog.String("error", err.Error()),
		)
		return true
	}

	timeout := spec.ResolveStageTimeout(wf, specStage, spec.DefaultStageTimeout)
	budgetMinutes := int(timeout.Minutes())

	approvedPlan, err := s.loadApprovedPlanForRun(r.Context(), stage.RunID)
	if err != nil || approvedPlan == nil {
		return true
	}

	if approvedPlan.PredictedRuntimeMinutes <= budgetMinutes {
		return true
	}

	// Over budget: decomposition satisfies the gate without override.
	if approvedPlan.Decomposition != nil {
		return true
	}

	auditPayload, _ := json.Marshal(map[string]any{
		"stage_id":          stage.ID.String(),
		"predicted_minutes": approvedPlan.PredictedRuntimeMinutes,
		"budget_minutes":    budgetMinutes,
		"timeout_source":    timeoutSource,
	})
	systemKind := audit.ActorKind("system")

	if strings.Contains(comment, "--override-budget") {
		if s.cfg.AuditRepo != nil {
			if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
				RunID:     stage.RunID,
				StageID:   &stage.ID,
				Timestamp: time.Now().UTC(),
				Category:  "plan_budget_override_acknowledged",
				ActorKind: &systemKind,
				Payload:   auditPayload,
			}); err != nil {
				s.cfg.Logger.Error("audit append failed for budget override",
					"run_id", stage.RunID, "stage_id", stage.ID, "error", err.Error())
			}
		}
		return true
	}

	if s.cfg.AuditRepo != nil {
		if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
			RunID:     stage.RunID,
			StageID:   &stage.ID,
			Timestamp: time.Now().UTC(),
			Category:  "plan_violates_budget",
			ActorKind: &systemKind,
			Payload:   auditPayload,
		}); err != nil {
			s.cfg.Logger.Error("audit append failed for budget violation",
				"run_id", stage.RunID, "stage_id", stage.ID, "error", err.Error())
		}
	}

	s.writeError(w, r, http.StatusUnprocessableEntity, "plan_violates_budget",
		"plan predicted_runtime_minutes exceeds the implement-stage budget; add decomposition.sub_plans or include --override-budget in the comment",
		map[string]any{
			"stage_id":          stage.ID.String(),
			"predicted_minutes": approvedPlan.PredictedRuntimeMinutes,
			"budget_minutes":    budgetMinutes,
			"timeout_source":    timeoutSource,
		})
	return false
}
