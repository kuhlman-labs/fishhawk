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
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
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
	// ApproverGithubLogin is the resolved GitHub login of the acting
	// operator, threaded through by the MCP approve/reject tools (#751)
	// so the issue-thread status footer `@`-mentions the real login
	// rather than the raw token subject (e.g. brett@local-mcp). Optional
	// and supplementary for rendering only: the audit `approver` field
	// stays the token subject (provenance). Declared here so the
	// DisallowUnknownFields decode accepts it; SPA/CLI callers omit it
	// (omitempty) and are unaffected.
	ApproverGithubLogin string `json:"approver_github_login,omitempty"`
	// AddScopeFiles is an explicit, authoritative list of repo-relative
	// paths to fold into the implement stage's effective scope.files on
	// approve (#824). It replaces the brittle regex-scrape of the free-text
	// reason (#730), which silently misses directories, extensionless or
	// repo-root files, and described-but-not-spelled paths. A trailing
	// slash marks a directory whose created files stage under it. Recorded
	// on the approval audit payload and consumed by the prompt builder;
	// the #730 prose fold remains as a fallback. Declared here so the
	// DisallowUnknownFields decode accepts it; callers omit it (omitempty).
	AddScopeFiles []string `json:"add_scope_files,omitempty"`
}

// approvalSubmitResponse is the 200 body for POST /v0/stages/{stage_id}/
// approvals (#986). On a first submission the three duplicate fields are
// omitted and the body is byte-identical to the bare Stage shape existing
// clients decode. On a duplicate submission — same (stage, subject) pair —
// they label the no-op honestly: the prior decision stands, the stage state
// is unchanged, and no gates re-ran. prior_decision/prior_submitted_at come
// from the EXISTING approval row, so they are authentic provenance, not
// echoes of the new request.
type approvalSubmitResponse struct {
	stageResponse
	DuplicateSubmission bool   `json:"duplicate_submission,omitempty"`
	PriorDecision       string `json:"prior_decision,omitempty"`
	PriorSubmittedAt    string `json:"prior_submitted_at,omitempty"`
}

// duplicateApprovalResponse labels a duplicate submission's 200 body with
// the prior approval row's decision and timestamp.
func duplicateApprovalResponse(stage *run.Stage, prior *approval.Approval) approvalSubmitResponse {
	return approvalSubmitResponse{
		stageResponse:       toStageResponse(stage),
		DuplicateSubmission: true,
		PriorDecision:       string(prior.Decision),
		PriorSubmittedAt:    prior.SubmittedAt.UTC().Format(time.RFC3339),
	}
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
// returns the current stage state with a 200 labeled
// duplicate_submission (#986) — prior_decision/prior_submitted_at
// carry the existing row's provenance, no gates re-run, and no audit
// entries are emitted. The first decision wins for any_of-style gates.
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

	// Duplicate pre-check (#986): a re-submission from the same subject
	// is answered BEFORE any plan gate runs — the labeled duplicate 200
	// below — so a duplicate can never re-emit gate audit entries
	// (e.g. plan_violates_budget) or 422 against a decision that already
	// stands. Authoritative read of the approval row for (stage,
	// subject); fail-open on a read error because Submit's
	// Inserted=false path is the race-safe second layer producing the
	// identical labeled response.
	if prior := s.findPriorApproval(r.Context(), stageID, subject); prior != nil {
		s.writeJSON(w, r, http.StatusOK, duplicateApprovalResponse(stage, prior))
		return
	}

	// ADR-036 (#875): refuse a plan-stage approve while a configured
	// agent plan review is still in-flight. Placed BEFORE
	// ApprovalRepo.Submit (not in the res.Inserted block) so a refused
	// approval inserts no row — a retry once the review lands flows
	// normally through Submit → advanceStage. A post-Submit gate would
	// strand the stage on the idempotent-first-wins retry (Submit would
	// return Inserted=false and skip the advance block).
	if decision == approval.DecisionApprove && stage.Type == run.StageTypePlan {
		if !s.checkPlanReviewSettled(w, r, stage) {
			return
		}
		// Scope-cap gate (#983): refuse an approve whose effective scope
		// (plan scope.files ∪ add_scope_files) exceeds the implement
		// stage's max_files_changed, unless the comment carries
		// --override-scope-cap. PRE-Submit for the same ADR-036 reason as
		// checkPlanReviewSettled: a refused approval must insert no row so
		// a retry after re-scope or with the override flows normally
		// (post-Submit, the idempotent-first-wins retry would skip gates).
		if !s.checkPlanScopeCap(w, r, stage, req.Comment, req.AddScopeFiles) {
			return
		}
		// Budget gate (#986): refuse an approve whose plan predicts a
		// runtime over the implement-stage budget, unless decomposition
		// or --override-budget satisfies it. PRE-Submit for the same
		// ADR-036 reason as its two siblings: a refused approval must
		// insert no row so a retry with the override flows normally
		// through Submit → advanceStage. Post-Submit (where this check
		// used to live), the 422 left a row behind and the documented
		// --override-budget retry dead-ended as an idempotent-first-wins
		// duplicate, silently stranding the stage.
		if !s.checkPlanBudget(w, r, stage, req.Comment) {
			return
		}
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
	// transition. A concurrent second submission that lost the race
	// past the duplicate pre-check gets the same labeled duplicate
	// 200 the pre-check produces.
	if !res.Inserted {
		s.writeJSON(w, r, http.StatusOK, duplicateApprovalResponse(stage, res.Approval))
		return
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

	s.writeApprovalAudit(r, stage, res.Approval, req.Comment, req.ApproverGithubLogin, req.AddScopeFiles)

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

	s.writeJSON(w, r, http.StatusOK, toStageResponse(stage))
}

// findPriorApproval returns the existing approval row for (stageID,
// subject), or nil when none exists. Read-only — the #986 duplicate
// pre-check uses it to answer re-submissions before any plan gate
// runs. Fail-open on a read error (WARN-log, return nil): the caller
// falls through to Submit, whose Inserted=false result is the
// race-safe second layer for the duplicate path.
func (s *Server) findPriorApproval(ctx context.Context, stageID uuid.UUID, subject string) *approval.Approval {
	existing, err := s.cfg.ApprovalRepo.ListForStage(ctx, stageID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"approval duplicate pre-check: list approvals failed",
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()),
		)
		return nil
	}
	for _, a := range existing {
		if a.ApproverSubject == subject {
			return a
		}
	}
	return nil
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
//
// When approverGithubLogin is non-empty (the MCP loop resolved the
// operator's real GitHub login, #751), it is recorded under
// approver_github_login for issue-thread `@`-mention rendering. The
// `approver` field is left as the token subject so the audit row keeps
// the true acting identity — the resolved login never overwrites
// provenance.
func (s *Server) writeApprovalAudit(r *http.Request, stage *run.Stage, app *approval.Approval, comment, approverGithubLogin string, addScopeFiles []string) {
	systemKind := audit.ActorKind("user")
	auditPayload := map[string]any{
		"stage_id": stage.ID.String(),
		"decision": string(app.Decision),
		"surface":  string(app.Surface),
		"approver": app.ApproverSubject,
	}
	if approverGithubLogin != "" {
		auditPayload["approver_github_login"] = approverGithubLogin
	}
	if app.Decision == approval.DecisionReject && strings.Contains(comment, "--decompose") {
		auditPayload["reject_reason"] = "decompose_required"
	}
	if app.Decision == approval.DecisionReject && comment != "" {
		auditPayload["rejection_comment"] = comment
	}
	if app.Decision == approval.DecisionApprove && comment != "" {
		auditPayload["comment"] = comment
	}
	// Structured scope amendment (#824): record the authoritative paths to
	// fold into the implement scope. Only on approve with a non-empty slice;
	// the prompt builder reads this back via loadApprovalAddScopeFiles.
	if app.Decision == approval.DecisionApprove && len(addScopeFiles) > 0 {
		auditPayload["add_scope_files"] = addScopeFiles
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

// checkPlanScopeCap enforces the scope-cap gate on plan-stage approvals
// (#983). Returns true when the approval should proceed; returns false
// (and writes the 422 plan_violates_scope_cap response) when the
// effective scope — the plan's scope.files unioned with the approval's
// add_scope_files, prior add_scope_files folds, and approved scope
// amendments, deduped exactly as the prompt builder's foldScopePaths
// dedupes — exceeds the implement stage's resolved max_files_changed
// and the comment lacks --override-scope-cap.
//
// Override-able rather than hard-fail because declared scope is an
// upper bound on the eventual diff, not a prediction: the post-implement
// gate counts actual diff files, and the cap may legitimately be about
// to change. --override-scope-cap mirrors checkPlanBudget's
// --override-budget posture, acknowledged via a
// plan_scope_cap_override_acknowledged audit entry.
//
// Fail-open matching checkPlanBudget: any read failure, absent spec, or
// missing plan skips the check (effectiveScopeHeadroom WARN-logs), so a
// degraded backend can never brick the approval gate. A cap of 0 means
// no cap is configured — nothing to enforce.
func (s *Server) checkPlanScopeCap(w http.ResponseWriter, r *http.Request, stage *run.Stage, comment string, addScopeFiles []string) bool {
	effectiveCount, maxFiles, ok := s.effectiveScopeHeadroom(r.Context(), stage.RunID, addScopeFiles)
	if !ok || maxFiles <= 0 || effectiveCount <= maxFiles {
		return true
	}

	auditPayload, _ := json.Marshal(map[string]any{
		"stage_id":              stage.ID.String(),
		"scoped_files":          effectiveCount,
		"max_files_changed":     maxFiles,
		"add_scope_files_count": len(addScopeFiles),
	})
	systemKind := audit.ActorKind("system")

	if strings.Contains(comment, "--override-scope-cap") {
		if s.cfg.AuditRepo != nil {
			if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
				RunID:     stage.RunID,
				StageID:   &stage.ID,
				Timestamp: time.Now().UTC(),
				Category:  "plan_scope_cap_override_acknowledged",
				ActorKind: &systemKind,
				Payload:   auditPayload,
			}); err != nil {
				s.cfg.Logger.Error("audit append failed for scope-cap override",
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
			Category:  "plan_violates_scope_cap",
			ActorKind: &systemKind,
			Payload:   auditPayload,
		}); err != nil {
			s.cfg.Logger.Error("audit append failed for scope-cap violation",
				"run_id", stage.RunID, "stage_id", stage.ID, "error", err.Error())
		}
	}

	s.writeError(w, r, http.StatusUnprocessableEntity, "plan_violates_scope_cap",
		"effective scope.files (plan scope plus add_scope_files) exceeds the implement stage's max_files_changed; re-scope the plan or include --override-scope-cap in the comment",
		map[string]any{
			"stage_id":              stage.ID.String(),
			"scoped_files":          effectiveCount,
			"max_files_changed":     maxFiles,
			"add_scope_files_count": len(addScopeFiles),
		})
	return false
}

// checkPlanReviewSettled enforces the ADR-036 (#875) plan-approval
// completion gate: it refuses a plan-stage approve while a configured agent
// plan review is still in-flight. Returns true to proceed; writes a typed
// 409 agent_review_pending and returns false to refuse.
//
// Posture mirrors checkPlanBudget / checkApproverAuthorization: every read
// failure fails OPEN (WARN-log, return true) so a transient backend hiccup
// can never brick the approval gate. The gate fires only when ALL of:
//   - the run's plan stage declares reviewers.agent > 0, AND
//   - at least one plan_review_started entry exists (the review was
//     dispatched), AND
//   - fewer than reviewers.agent TERMINAL review entries
//     (plan_reviewed | plan_review_failed | plan_review_skipped) have
//     landed, AND
//   - the elapsed time since the earliest plan_review_started is within
//     the backstop bound.
//
// ANY terminal review kind counts toward the unblock, so a timed-out
// reviewer (the #747 budget kill emits a terminal plan_review_failed) never
// strands the gate. The backstop is the belt for a reviewer that dies
// emitting NO terminal entry at all: past the bound, approval is ALLOWED and
// a plan_review_backstop_elapsed audit entry records the degrade.
func (s *Server) checkPlanReviewSettled(w http.ResponseWriter, r *http.Request, stage *run.Stage) bool {
	ctx := r.Context()
	runRow, err := s.cfg.RunRepo.GetRun(ctx, stage.RunID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "plan-review gate: get run failed",
			slog.String("stage_id", stage.ID.String()),
			slog.String("error", err.Error()),
		)
		return true
	}

	reviewersCfg := s.resolveStageReviewers(ctx, runRow, spec.StageTypePlan)
	if reviewersCfg == nil || reviewersCfg.Agent == 0 {
		// No agent reviewer configured — byte-for-byte the pre-ADR-036
		// approve path (gating reviewers with human==0 included: the
		// gate is keyed on a present plan_review_started entry, not on
		// the authority class).
		return true
	}

	started, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, stage.RunID, "plan_review_started")
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "plan-review gate: list plan_review_started failed",
			slog.String("stage_id", stage.ID.String()),
			slog.String("error", err.Error()),
		)
		return true
	}
	if len(started) == 0 {
		// Configured but not dispatched — nothing to wait for.
		return true
	}

	terminalCount := 0
	for _, cat := range []string{"plan_reviewed", "plan_review_failed", "plan_review_skipped"} {
		entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, stage.RunID, cat)
		if err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "plan-review gate: list terminal review entries failed",
				slog.String("stage_id", stage.ID.String()),
				slog.String("category", cat),
				slog.String("error", err.Error()),
			)
			return true
		}
		terminalCount += len(entries)
	}
	if terminalCount >= reviewersCfg.Agent {
		// Every configured agent review reached a terminal state.
		return true
	}

	// Backstop: the earliest plan_review_started timestamp anchors the
	// hard deadline. Past it, a reviewer that died emitting nothing can
	// never strand the gate.
	earliest := started[0].Timestamp
	for _, e := range started {
		if e.Timestamp.Before(earliest) {
			earliest = e.Timestamp
		}
	}
	bound := s.planReviewBackstop(reviewersCfg.Agent)
	if elapsed := time.Now().UTC().Sub(earliest); elapsed > bound {
		s.appendPlanReviewBackstopElapsed(ctx, stage, reviewersCfg.Agent, terminalCount, earliest, elapsed)
		return true
	}

	s.writeError(w, r, http.StatusConflict, "agent_review_pending",
		"a configured agent plan review is still in-flight; poll fishhawk_get_plan / fishhawk_await_review until the review reaches a terminal state, then retry the approval",
		map[string]any{
			"stage_id":          stage.ID.String(),
			"configured_agents": reviewersCfg.Agent,
			"landed_terminal":   terminalCount,
		})
	return false
}

// planReviewBackstop computes the hard max-wait bound for the plan-review
// completion gate (ADR-036). It is ReviewBudget.Cap (the #747 worst-case
// per-invocation ceiling) multiplied by the configured agent count, because
// the per-reviewer loop runs invocations serially under advisory authority —
// two reviewers each legitimately near Cap must not trip a false degrade.
// Falls back to planreview.DefaultReviewBudget.Cap when Cap is unset so the
// helper is correct even when the Server is constructed outside New (which
// already defaults a zero-value ReviewBudget).
func (s *Server) planReviewBackstop(agentCount int) time.Duration {
	capDur := s.cfg.ReviewBudget.Cap
	if capDur <= 0 {
		capDur = planreview.DefaultReviewBudget.Cap
	}
	if agentCount < 1 {
		agentCount = 1
	}
	return capDur * time.Duration(agentCount)
}

// appendPlanReviewBackstopElapsed records the ADR-036 backstop degrade: the
// plan-review completion gate allowed an approval because the hard bound
// elapsed before the configured agent reviews all reached a terminal state.
// Best-effort — a logged audit failure never unwinds the approval.
func (s *Server) appendPlanReviewBackstopElapsed(ctx context.Context, stage *run.Stage, configuredAgents, landedTerminal int, startedAt time.Time, elapsed time.Duration) {
	systemKind := audit.ActorKind("system")
	payload, _ := json.Marshal(map[string]any{
		"stage_id":          stage.ID.String(),
		"configured_agents": configuredAgents,
		"landed_terminal":   landedTerminal,
		"started_at":        startedAt.Format(time.RFC3339Nano),
		"elapsed_seconds":   int(elapsed.Seconds()),
	})
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     stage.RunID,
		StageID:   &stage.ID,
		Timestamp: time.Now().UTC(),
		Category:  "plan_review_backstop_elapsed",
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		s.cfg.Logger.Error("audit append failed for plan_review_backstop_elapsed",
			"run_id", stage.RunID, "stage_id", stage.ID, "error", err.Error())
	}
}
