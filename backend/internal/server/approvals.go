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
	"github.com/kuhlman-labs/fishhawk/backend/internal/budget"
	"github.com/kuhlman-labs/fishhawk/backend/internal/delegation"
	"github.com/kuhlman-labs/fishhawk/backend/internal/drive"
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
	// BindingAssertions is an OPTIONAL list of operator-declared,
	// deterministic binding-assertion checks (#1171). Each is a typed
	// substring assertion (file_contains | test_asserts) the operator
	// attaches at approval time so an explicit approval condition becomes
	// machine-checkable post-implement. Recorded on the approval audit
	// payload alongside add_scope_files and echoed on the implement
	// prompt-response; the runner decodes and evaluates them (slice 2).
	// Declared here so the DisallowUnknownFields decode accepts it; callers
	// omit it (omitempty) and stay byte-identical to today. Validated
	// pre-Submit via validateBindingAssertions — no enforcement happens at
	// approve time, only declaration validation.
	BindingAssertions []bindingAssertion `json:"binding_assertions,omitempty"`
	// ImplementModel is the OPTIONAL operator override for the implement
	// stage's model (#1013) — the highest rung of the implement-model
	// resolution ladder (deployment default < spec executor.model < plan
	// model_recommendation < this operator override). On a plan-stage
	// approve the gate resolves the full ladder with this as the operator
	// rung, validates the RESOLVED non-empty value against
	// ImplementAllowedModels.IsAllowed for the run's adapter (rejecting 422
	// plan_invalid_model, naming the resolved source, on an unknown model),
	// and emits the source-tagged model_resolved audit the runner spawn
	// routes through. Empty (the default) leaves resolution to the lower
	// rungs and stays byte-identical to today. Declared here so the
	// DisallowUnknownFields decode accepts it; callers omit it (omitempty).
	ImplementModel string `json:"implement_model,omitempty"`
	// Delegated opts the submission into the ADR-040 delegated-action
	// path (#1026): the operator agent asserts it acts under the
	// workflow's operator_agent.may_approve knob. The server NEVER
	// trusts that assertion — checkDelegation re-evaluates the named
	// condition against current run state at action time, refusing with
	// 403 delegation_not_configured (no effective block / knob,
	// fail-closed) or delegation_condition_unmet (named failed
	// predicate). When met, the approval's audit payload records
	// `delegated: "<condition>"`. Requests without the field are
	// byte-identical to today.
	Delegated bool `json:"delegated,omitempty"`
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

	// Delegated-action enforcement (ADR-040 / #1026): a delegated:true
	// submission must hold the may_approve condition against CURRENT run
	// state, re-evaluated server-side — never trusted from the client's
	// read of GET /v0/runs/{id}'s advisory delegation block. Placed
	// PRE-Submit like the plan gates so a refusal inserts no approval
	// row. Delegation covers the approve verb only: a reject is the
	// reviewer_reject judgment that always pages the human.
	var delegatedRule string
	if req.Delegated {
		if decision != approval.DecisionApprove {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"delegated submissions support decision 'approve' only; rejection is a human judgment (reviewer_reject pages the human)",
				map[string]any{"field": "delegated", "decision": req.Decision})
			return
		}
		rule, ok := s.checkDelegation(w, r, stage.RunID, delegation.ActionApprove)
		if !ok {
			return
		}
		delegatedRule = rule
	}

	// Binding-assertion declaration validation (#1171): when an approve
	// carries binding_assertions, validate the typed open enum BEFORE
	// ApprovalRepo.Submit — like the other pre-Submit gates, a malformed
	// declaration inserts no approval row, so a retry with a corrected
	// declaration flows normally. No enforcement runs here; the runner
	// evaluates the assertions post-implement (slice 2). Reject/empty
	// approves skip this and stay byte-identical to today.
	if decision == approval.DecisionApprove && len(req.BindingAssertions) > 0 {
		if err := validateBindingAssertions(req.BindingAssertions); err != nil {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				err.Error(), map[string]any{"field": "binding_assertions"})
			return
		}
	}

	// ADR-036 (#875): refuse a plan-stage approve while a configured
	// agent plan review is still in-flight. Placed BEFORE
	// ApprovalRepo.Submit (not in the res.Inserted block) so a refused
	// approval inserts no row — a retry once the review lands flows
	// normally through Submit → advanceStage. A post-Submit gate would
	// strand the stage on the idempotent-first-wins retry (Submit would
	// return Inserted=false and skip the advance block).
	//
	// resolvedModel, when non-nil, carries the source-tagged implement
	// model the model gate resolved on this plan-stage approve. It is
	// emitted as the model_resolved audit AFTER Submit+advance succeed
	// (the slice-1 reader routes it to the runner spawn). nil means no
	// emission — either a non-plan/reject path, or the gate read the run
	// row failed (fail-open: proceed, but emit nothing so the prompt path
	// falls through to live resolution rather than a shadowing empty
	// audit).
	var resolvedModel *ResolvedModel
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
		// Periodic-budget escalation gate (#1371): refuse an approve once
		// the run's advisory periodic budget has escalated to the
		// ack_required/page tier, unless the comment carries --ack-budget.
		// PRE-Submit for the same ADR-036 reason as its siblings: a 422
		// must insert no row so an --ack-budget retry flows normally.
		if !s.checkPeriodicBudgetTier(w, r, stage, req.Comment) {
			return
		}
		// Model validity gate (#1339): BEFORE the allow-list, reject a
		// resolved model that is definitively not a real, currently-served
		// model for the run adapter (validity → policy → pricing layering).
		// Pre-Submit for the same ADR-036 reason as its siblings: a 422
		// inserts no row. Fail-OPEN everywhere (nil oracle, no/stale snapshot,
		// empty model) so the wired no-data oracle can never hard-fail prod.
		if runRow, rerr := s.cfg.RunRepo.GetRun(r.Context(), stage.RunID); rerr == nil {
			rmv := s.gateResolveImplementModel(r.Context(), runRow, req.ImplementModel)
			adapter := adapterForImplementAgent(specImplementExecutorAgent(runRow.WorkflowSpec, runRow.WorkflowID))
			if !s.checkModelValidityGate(w, r, stage, rmv.Value, adapter) {
				return
			}
		}
		// Model gate (#1013): resolve the implement-model ladder with the
		// operator override as the highest rung, then validate the RESOLVED
		// non-empty value against the per-adapter allow-list. PRE-Submit for
		// the same ADR-036 reason as its siblings: a 422 must insert no row
		// so a corrected re-approval flows normally. Fail-OPEN: an
		// empty/unconfigured allow-list accepts any model (IsAllowed). On a
		// pass, rm carries the resolution to emit post-advance.
		rm, ok := s.checkPlanModelAllowed(w, r, stage, req.ImplementModel)
		if !ok {
			return
		}
		resolvedModel = rm
	}

	// Deploy gate (#1384 / E23.4 / ADR-038): the deploy stage's PRE-execution
	// approval gate. Unlike the post-hoc plan/review gates, a deploy stage's
	// effect IS the side effect, so the gate evaluates the deploy's pre-flight
	// constraints (allowed_environments / change_freeze / required_upstream)
	// BEFORE the approval advances the stage off the gate to dispatch.
	// PRE-Submit for the same ADR-036 ordering reason as the plan gates: a
	// refused approval inserts no row, so a corrected retry (e.g. with
	// --environment / --override-freeze) flows normally. Unlike the plan
	// gates' fail-open posture, checkDeployPreflight FAILS CLOSED (#1384,
	// operator binding condition 1): an unverifiable deploy is denied.
	if decision == approval.DecisionApprove && stage.Type == run.StageTypeDeploy {
		// write:deploy scope (ADR-038 / #1390): the deploy gate is an
		// operator bearer path, so it requires the deploy-specific scope on
		// top of the write:approvals the handler already enforced at entry.
		// requireWriteScope 401s anonymous, 403s a token missing the scope,
		// and exempts cookie sessions (OAuth callers carry no scope list).
		// Placed before checkDeployPreflight so an unauthorized caller never
		// reaches the pre-flight evaluation. The reject path is unaffected: a
		// deploy reject routes through advanceStage (not this approve-only
		// block), so a rejection still pages the human without write:deploy.
		if !s.requireWriteScope(w, r, "write:deploy") {
			return
		}
		if !s.checkDeployPreflight(w, r, stage, req.Comment) {
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

	// Persist the approval audits BEFORE advancing the stage (#1351). The
	// stage transition below is the dispatch-gating signal: BOTH the
	// synchronous Orchestrator.Advance handoff AND an asynchronous reconciler
	// that observes a succeeded plan stage with a still-pending implement stage
	// can dispatch the implement stage — and the runner can fetch its prompt —
	// the instant the plan stage flips to succeeded. That prompt-fetch reads
	// these audits: loadApprovalAddScopeFiles folds approval_submitted's
	// add_scope_files into the implement agent's enforced scope, and the
	// runner-spawn router reads model_resolved. With the writes AFTER the
	// transition (the prior ordering), the initial dispatch's fetch raced the
	// write and lost — the operator-declared add_scope_files paths were absent
	// from the enforced scope on the first dispatch, forcing a redundant
	// mid-stage amendment for the very files already declared (reproduced in
	// runs 03f6e28a/#1349 and e6e379fd/#1352). Writing them first makes them
	// durably visible before any dispatch path can observe the transition. The
	// `stage` here is the pre-advance row; advanceStage mutates only its State,
	// not the ID/RunID these audits read, so the persisted payloads are
	// byte-identical to the prior ordering — only their commit point moves
	// earlier. Both writes are best-effort (a logged append failure never
	// unwinds the approval the gate already recorded via Submit).
	s.writeApprovalAudit(r, stage, res.Approval, req.Comment, req.ApproverGithubLogin, req.AddScopeFiles, req.BindingAssertions, delegatedRule)

	// Model resolution (#1013): emit the source-tagged model_resolved audit
	// the gate computed on this plan-stage approve — the INTRODUCTION of the
	// model_resolved kind (CategoryModelResolved). The slice-1 reader
	// (gateResolvedModel) consumes the most-recent entry to route the runner
	// spawn's --model. Emitted even when the resolution is empty
	// (ModelSourceNone): the reader returns it as a deliberate "use the
	// default spawn" decision, byte-identical to today's no---model spawn. Like
	// the approval audit above, it must precede advanceStage so the runner-spawn
	// router cannot read an empty model_resolved on a dispatch that races the
	// transition.
	if resolvedModel != nil {
		s.writeModelResolvedAudit(r, stage, res.Approval, *resolvedModel)
	}

	stage, err = s.advanceForDecision(r.Context(), stage, decision)
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

	// Drive (#1023): a plan-gate approval on a drive-enabled run is the
	// plan_approved_dispatch transition point — the orchestrator handoff
	// above IS the auto-advance (workflow_dispatch for runner_kind
	// github_actions), so stamp the run_auto_advanced entry that makes
	// it attributable; runner_kind local parks with a ready-to-run next
	// action instead (ADR-024: the runner is host-spawned, the backend
	// has no execution channel to it). After the orchestrator block so
	// the entry documents an advance that was actually attempted.
	if decision == approval.DecisionApprove && stage.Type == run.StageTypePlan {
		s.recordDrivePlanApproved(r.Context(), stage)
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

// recordDrivePlanApproved stamps the drive engine's
// plan_approved_dispatch rule (#1023) after a plan-gate approval.
// No-ops for non-drive runs, when no engine is wired, or on a run
// read failure (best-effort: the approval already landed; a missing
// stamp degrades attribution, never the run). The entry is keyed to
// the approved plan stage.
func (s *Server) recordDrivePlanApproved(ctx context.Context, stage *run.Stage) {
	if s.drive == nil || s.cfg.RunRepo == nil {
		return
	}
	runRow, err := s.cfg.RunRepo.GetRun(ctx, stage.RunID)
	if err != nil || !runRow.Drive {
		return
	}
	out := drive.EvaluatePlanApproved(runRow.RunnerKind)
	adv := drive.Advance{
		Rule: drive.RulePlanApprovedDispatch,
		From: "plan:approved",
	}
	if out.Advance {
		adv.To = "implement:dispatched"
		adv.Event = "plan gate approved; orchestrator dispatched implement via workflow_dispatch"
	} else {
		adv.To = "implement:ready"
		adv.Event = "plan gate approved; runner_kind local parks for a host-side dispatch"
		adv.Parked = true
		adv.NextAction = out.NextAction
	}
	s.drive.Record(ctx, stage.RunID, &stage.ID, adv)
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
//
// NOTE (ADR-038 / #1384): a DEPLOY-stage approve does NOT route through here.
// Its pre-execution gate advances awaiting_deploy_approval → dispatched (the
// delegating executor still has to fire — the work is NOT done), so the
// caller special-cases it BEFORE calling advanceStage rather than threading a
// stage-type parameter through every call site (see handleSubmitApproval's
// advanceForDecision). advanceStage keeps the generic approve → succeeded
// semantics every non-deploy gated stage relies on.
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

// advanceForDecision applies the gate decision for a stage, special-casing
// the DEPLOY pre-execution gate (ADR-038 / #1384): an approved deploy advances
// awaiting_deploy_approval → dispatched, then IMMEDIATELY fires the external
// delegating pipeline and parks at awaiting_deployment (E23.6 / #1386) — NOT the
// generic approve → succeeded. Every other stage and the reject path delegate to
// advanceStage unchanged. The full stage is already in the caller's hand, so
// this needs no extra read.
//
// triggerDeploy owns the dispatch → running → awaiting_deployment walk and, on a
// trigger error, fails the stage category C (returning the failed stage) rather
// than silently parking at dispatched. A nil error from triggerDeploy means the
// approval response should reflect the returned stage state.
func (s *Server) advanceForDecision(ctx context.Context, stage *run.Stage, decision approval.Decision) (*run.Stage, error) {
	if decision == approval.DecisionApprove && stage.Type == run.StageTypeDeploy {
		dispatched, err := s.cfg.RunRepo.TransitionStage(ctx, stage.ID,
			run.StageStateDispatched, nil)
		if err != nil {
			return nil, err
		}
		return s.triggerDeploy(ctx, dispatched)
	}
	return s.advanceStage(ctx, stage.ID, decision)
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
//
// When delegatedRule is non-empty the approval landed via the ADR-040
// delegated path (#1026) and the payload records `delegated: "<rule>"`
// — the condition checkDelegation re-evaluated and found met. Token-
// subject attribution for the operator agent is #1027's scope.
func (s *Server) writeApprovalAudit(r *http.Request, stage *run.Stage, app *approval.Approval, comment, approverGithubLogin string, addScopeFiles []string, bindingAssertions []bindingAssertion, delegatedRule string) {
	// ADR-040 D4 (#1027): the acting subject selects the kind — an
	// operator-agent token records agent, every other subject (human
	// tokens, GitHub logins from the PR-review-event path) stays user.
	actorKind := actorKindForSubject(app.ApproverSubject)
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
	// Binding-assertion declaration (#1171): record the operator's declared
	// assertions so the prompt builder reads them back via
	// loadApprovalBindingAssertions. Only on approve with a non-empty slice;
	// the key is omitted otherwise so a no-declaration approve is
	// byte-identical to today.
	if app.Decision == approval.DecisionApprove && len(bindingAssertions) > 0 {
		auditPayload["binding_assertions"] = bindingAssertions
	}
	if delegatedRule != "" {
		auditPayload["delegated"] = delegatedRule
	}
	payload, _ := json.Marshal(auditPayload)

	approver := app.ApproverSubject
	if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:        stage.RunID,
		StageID:      &stage.ID,
		Timestamp:    time.Now().UTC(),
		Category:     "approval_submitted",
		ActorKind:    &actorKind,
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

// checkPlanModelAllowed is the plan-stage model gate (#1013). It resolves the
// implement-model ladder with req.ImplementModel as the operator rung, then
// validates the RESOLVED non-empty value against the run adapter's allow-list.
// Returns (*ResolvedModel, true) to proceed — the pointer is the resolution to
// emit as model_resolved after Submit+advance. Returns (nil, false) after
// writing a 422 plan_invalid_model when the resolved model is non-empty and the
// adapter's configured allow-set omits it; the message names the resolved
// SOURCE (default|spec|plan|operator), so an unknown plan- or spec-recommended
// model — not just the operator field — is caught here rather than at runner
// spawn. A deployment default outside its own allow-list is likewise a config
// error surfaced as 422 source=default (the gate validates the resolved value
// regardless of which rung supplied it).
//
// Fail-OPEN, matching the sibling plan gates:
//   - GetRun failure returns (nil, true): proceed, but emit NOTHING (a nil
//     pointer), so the prompt path falls through to live resolution rather
//     than a shadowing empty model_resolved audit.
//   - An empty/unconfigured allow-list (or an adapter with no set) accepts any
//     model via IsAllowed — byte-identical to today.
//   - An empty resolved model (ModelSourceNone) skips the allow-list check and
//     proceeds; the emitted entry records the deliberate default spawn.
func (s *Server) checkPlanModelAllowed(w http.ResponseWriter, r *http.Request, stage *run.Stage, operatorModel string) (*ResolvedModel, bool) {
	runRow, err := s.cfg.RunRepo.GetRun(r.Context(), stage.RunID)
	if err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn, "model gate: get run failed",
			slog.String("stage_id", stage.ID.String()),
			slog.String("error", err.Error()),
		)
		return nil, true
	}
	rm := s.gateResolveImplementModel(r.Context(), runRow, operatorModel)
	if rm.Value == "" {
		// Empty resolution: today's default spawn. Nothing to validate.
		return &rm, true
	}
	adapter := adapterForImplementAgent(specImplementExecutorAgent(runRow.WorkflowSpec, runRow.WorkflowID))
	if s.cfg.ImplementAllowedModels.IsAllowed(adapter, rm.Value) {
		return &rm, true
	}
	s.writeError(w, r, http.StatusUnprocessableEntity, "plan_invalid_model",
		fmt.Sprintf("resolved implement model %q (source %s) is not in the configured allow-list for adapter %q; choose an allowed model via the spec executor.model, the plan model_recommendation, or the implement_model approval override, or widen the deployment allow-list",
			rm.Value, rm.Source, adapter),
		map[string]any{
			"stage_id":     stage.ID.String(),
			"model":        rm.Value,
			"model_source": string(rm.Source),
			"adapter":      adapter,
		})
	return nil, false
}

// writeModelResolvedAudit emits the source-tagged model_resolved audit entry
// (CategoryModelResolved, #1013) recording the implement model the gate
// resolved on a valid plan-stage approve. The payload is the ResolvedModel's
// {model, model_source} json shape the slice-1 reader (gateResolvedModel)
// consumes to route the runner spawn. Actor attribution mirrors
// writeApprovalAudit (the acting subject selects agent vs user). Best-effort: a
// logged append failure never unwinds the approval the gate already recorded.
func (s *Server) writeModelResolvedAudit(r *http.Request, stage *run.Stage, app *approval.Approval, rm ResolvedModel) {
	actorKind := actorKindForSubject(app.ApproverSubject)
	approver := app.ApproverSubject
	payload, _ := json.Marshal(rm)
	if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:        stage.RunID,
		StageID:      &stage.ID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryModelResolved,
		ActorKind:    &actorKind,
		ActorSubject: &approver,
		Payload:      payload,
	}); err != nil {
		s.cfg.Logger.Error("audit append failed for model_resolved",
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
// exceeds the resolved implement-stage budget and neither
// decomposition nor --override-budget is present in the comment.
//
// The budget is the IMPLEMENT stage's spec-resolved timeout widened by
// resolvePlanGateBudget (#994) — max(spec, calibration p95×1.5) clamped
// to spec×2 — the same base the dynamic kill cap builds on, so the gate
// and the runtime the stage actually gets cannot drift apart. Fail-open:
// any spec-parse or calibration unavailability leaves the budget at the
// spec-resolved floor.
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

	// Resolve the IMPLEMENT stage's spec budget explicitly — the gate
	// compares the plan's prediction against the implement budget, not
	// the plan stage under approval (stage.Type), which this code used
	// to resolve.
	wf, specStage, timeoutSource, err := resolveSpecStageForRun(runRow, run.StageTypeImplement)
	if err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn, "budget check: resolve spec stage failed",
			slog.String("stage_id", stage.ID.String()),
			slog.String("error", err.Error()),
		)
		return true
	}

	specBudget := spec.ResolveStageTimeout(wf, specStage, spec.DefaultStageTimeout)
	budget, budgetSource := s.resolvePlanGateBudget(r.Context(), runRow.WorkflowID, specBudget)
	budgetMinutes := int(budget.Minutes())
	specBudgetMinutes := int(specBudget.Minutes())

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

	// budget_minutes is the resolved (p95-aware) value the gate enforces;
	// spec_budget_minutes records the raw spec-resolved floor so historical
	// pre-#994 entries (where budget_minutes WAS the spec value) stay
	// interpretable. timeout_source keeps describing the spec value's
	// provenance; budget_source says which term won the resolution.
	auditPayload, _ := json.Marshal(map[string]any{
		"stage_id":            stage.ID.String(),
		"predicted_minutes":   approvedPlan.PredictedRuntimeMinutes,
		"budget_minutes":      budgetMinutes,
		"budget_source":       budgetSource,
		"spec_budget_minutes": specBudgetMinutes,
		"timeout_source":      timeoutSource,
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
		"plan predicted_runtime_minutes exceeds the resolved implement-stage budget; add decomposition.sub_plans or include --override-budget in the comment",
		map[string]any{
			"stage_id":            stage.ID.String(),
			"predicted_minutes":   approvedPlan.PredictedRuntimeMinutes,
			"budget_minutes":      budgetMinutes,
			"budget_source":       budgetSource,
			"spec_budget_minutes": specBudgetMinutes,
			"timeout_source":      timeoutSource,
		})
	return false
}

// checkPeriodicBudgetTier enforces the escalating periodic-budget
// acknowledgment gate on plan-stage approvals (#1371). Returns true when
// the approval should proceed; returns false (and writes a 422
// periodic_budget_requires_ack response) when the run's advisory periodic
// budget has escalated to the ack_required or page tier — period spend has
// reached the configured ack multiple of the (possibly overridden) limit —
// and the comment lacks --ack-budget.
//
// This is the calibrate-OR-escalate other half of #1371: once the limit is
// calibrated, a normal week sits below 1x and never reaches this gate; an
// over-budget signal escalates through tiers requiring an audited
// acknowledgment instead of reading 'over' forever. Mirrors checkPlanBudget's
// --override-budget posture: --ack-budget records a
// plan_periodic_budget_tier_acknowledged audit entry; its absence at the ack
// rung records plan_violates_periodic_budget and refuses.
//
// Fail-OPEN throughout, matching the sibling plan gates — a degraded backend
// can never brick the approval gate. Proceeds (return true) when:
//   - RunRepo is nil or doesn't implement runCostSummer (no period sum
//     available),
//   - the run lookup fails, the cached spec is absent/unparseable, the
//     workflow is absent, or it declares no advisory budget,
//   - the budget's period is unrecognized, or
//   - the period-sum query errors,
//   - the evaluated tier is below the ack rung (ok|warn|over).
func (s *Server) checkPeriodicBudgetTier(w http.ResponseWriter, r *http.Request, stage *run.Stage, comment string) bool {
	ctx := r.Context()
	if s.cfg.RunRepo == nil {
		return true
	}
	summer, ok := s.cfg.RunRepo.(runCostSummer)
	if !ok {
		return true
	}
	runRow, err := s.cfg.RunRepo.GetRun(ctx, stage.RunID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "periodic-budget gate: get run failed",
			slog.String("stage_id", stage.ID.String()),
			slog.String("error", err.Error()),
		)
		return true
	}
	if len(runRow.WorkflowSpec) == 0 {
		return true
	}
	parsed, err := spec.ParseBytes(runRow.WorkflowSpec)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "periodic-budget gate: parse spec failed",
			slog.String("stage_id", stage.ID.String()),
			slog.String("error", err.Error()),
		)
		return true
	}
	wf, ok := parsed.Workflows[runRow.WorkflowID]
	if !ok {
		return true
	}

	// The first advisory budget is the one the dogfood workflows declare;
	// blocking budgets are an admission-time gate, never this plan-approval
	// path. No advisory budget → nothing to gate on.
	var b spec.PeriodicBudget
	found := false
	for _, candidate := range wf.Budgets {
		if candidate.Enforcement == spec.EnforcementBlocking {
			continue
		}
		b = candidate
		found = true
		break
	}
	if !found {
		return true
	}

	loc := s.cfg.BudgetLocation
	if loc == nil {
		loc = time.UTC
	}
	b.LimitUSD = s.effectiveBudgetLimit(b)

	d, ok, err := evaluateWorkflowBudget(ctx, summer, runRow.Repo, runRow.WorkflowID, b, time.Now(), loc)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "periodic-budget gate: sum period spend failed",
			slog.String("stage_id", stage.ID.String()),
			slog.String("error", err.Error()),
		)
		return true
	}
	if !ok {
		// Unrecognized period — schema enum makes this unreachable.
		return true
	}

	tier := budget.Tier(d, s.cfg.BudgetAckMultiple, s.cfg.BudgetPageMultiple)
	if !budget.AckRequired(tier) {
		// ok|warn|over — below the acknowledgment rung; nothing to gate.
		return true
	}

	// Resolve the reported ack multiple through the same defensive
	// fallback budget.Tier applied, so the threshold the 422 message and
	// audit payload advertise matches the rung the gate actually evaluated
	// — including the inverted-pair case (e.g. ack=5/page=3 gates at the 2x
	// default and must report 2x, not the configured 5x) (#1371).
	ackMultiple, _ := budget.EffectiveMultiples(s.cfg.BudgetAckMultiple, s.cfg.BudgetPageMultiple)
	auditPayload, _ := json.Marshal(map[string]any{
		"stage_id":     stage.ID.String(),
		"workflow_id":  runRow.WorkflowID,
		"period":       b.Period,
		"spent":        d.Spent,
		"limit":        d.Limit,
		"fraction":     d.Fraction,
		"tier":         tier,
		"ack_multiple": ackMultiple,
	})
	systemKind := audit.ActorKind("system")

	if strings.Contains(comment, "--ack-budget") {
		if s.cfg.AuditRepo != nil {
			if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
				RunID:     stage.RunID,
				StageID:   &stage.ID,
				Timestamp: time.Now().UTC(),
				Category:  "plan_periodic_budget_tier_acknowledged",
				ActorKind: &systemKind,
				Payload:   auditPayload,
			}); err != nil {
				s.cfg.Logger.Error("audit append failed for periodic-budget ack",
					"run_id", stage.RunID, "stage_id", stage.ID, "error", err.Error())
			}
		}
		return true
	}

	if s.cfg.AuditRepo != nil {
		if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
			RunID:     stage.RunID,
			StageID:   &stage.ID,
			Timestamp: time.Now().UTC(),
			Category:  "plan_violates_periodic_budget",
			ActorKind: &systemKind,
			Payload:   auditPayload,
		}); err != nil {
			s.cfg.Logger.Error("audit append failed for periodic-budget violation",
				"run_id", stage.RunID, "stage_id", stage.ID, "error", err.Error())
		}
	}

	s.writeError(w, r, http.StatusUnprocessableEntity, "periodic_budget_requires_ack",
		fmt.Sprintf("period spend $%.2f has reached %.2gx the effective periodic budget limit $%.2f (tier %s); acknowledge the over-budget state by including --ack-budget in the approval comment, or wait for the calendar period to reset",
			d.Spent, ackMultiple, d.Limit, tier),
		map[string]any{
			"stage_id":     stage.ID.String(),
			"workflow_id":  runRow.WorkflowID,
			"period":       b.Period,
			"spent":        d.Spent,
			"limit":        d.Limit,
			"fraction":     d.Fraction,
			"tier":         tier,
			"ack_multiple": ackMultiple,
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

// checkDelegation enforces the ADR-040 delegated-action path (#1026),
// shared by the approval, fix-up, retry, and waive handlers. When a
// request opts in with delegated:true, the named action must be
// delegated by the run's effective operator_agent block AND its
// condition must hold against CURRENT run state — re-evaluated here at
// action time through the same backend/internal/delegation code that
// computes GET /v0/runs/{id}'s advisory block, never trusted from a
// client-supplied verdict.
//
// Fail-closed, unlike the human-path gates' fail-open posture: a spec
// that resolves no effective operator_agent block, a block with no knob
// for this action, a legacy run with no cached spec, or missing
// repository wiring all refuse with 403 delegation_not_configured;
// a configured knob whose condition is unmet refuses with 403
// delegation_condition_unmet, details naming the exact failed
// predicate. Repository read failures are 500 internal_error — still a
// refusal, reported honestly. Returns the met condition name (the rule
// the caller stamps into its audit payload as `delegated: "<rule>"`)
// and true to proceed.
func (s *Server) checkDelegation(w http.ResponseWriter, r *http.Request, runID uuid.UUID, action string) (string, bool) {
	if s.cfg.RunRepo == nil || s.cfg.ConcernRepo == nil || s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusForbidden, "delegation_not_configured",
			"delegated actions require run, concern, and audit repositories; nothing is delegated on this deployment (fail-closed)",
			map[string]any{"action": action})
		return "", false
	}
	runRow, err := s.cfg.RunRepo.GetRun(r.Context(), runID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get run for delegation check failed", map[string]any{"error": err.Error()})
		return "", false
	}
	if len(runRow.WorkflowSpec) == 0 {
		s.writeError(w, r, http.StatusForbidden, "delegation_not_configured",
			"the run carries no cached workflow spec, so no operator_agent block can govern it; nothing is delegated (fail-closed)",
			map[string]any{"action": action, "run_id": runID.String()})
		return "", false
	}
	parsed, err := spec.ParseBytes(runRow.WorkflowSpec)
	if err != nil {
		s.writeError(w, r, http.StatusForbidden, "delegation_not_configured",
			"the run's cached workflow spec does not parse, so no operator_agent block can be resolved; nothing is delegated (fail-closed)",
			map[string]any{"action": action, "error": err.Error()})
		return "", false
	}
	wf, ok := parsed.Workflows[runRow.WorkflowID]
	if !ok {
		s.writeError(w, r, http.StatusForbidden, "delegation_not_configured",
			"the run's workflow is not in its cached spec, so no operator_agent block can be resolved; nothing is delegated (fail-closed)",
			map[string]any{"action": action, "workflow_id": runRow.WorkflowID})
		return "", false
	}
	ev := &delegation.Evaluator{
		Stages:   s.cfg.RunRepo,
		Concerns: s.cfg.ConcernRepo,
		Audit:    s.cfg.AuditRepo,
	}
	res, err := ev.Evaluate(r.Context(), runRow, &wf)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"delegation condition evaluation failed", map[string]any{"action": action, "error": err.Error()})
		return "", false
	}
	if res == nil {
		s.writeError(w, r, http.StatusForbidden, "delegation_not_configured",
			"the run's workflow declares no effective operator_agent block; nothing is delegated (fail-closed)",
			map[string]any{"action": action})
		return "", false
	}
	for _, d := range res.Actions {
		if d.Action != action {
			continue
		}
		if !d.Met {
			s.writeError(w, r, http.StatusForbidden, "delegation_condition_unmet",
				"the delegated action's condition is not satisfied by current run state",
				map[string]any{
					"action":       action,
					"condition":    string(d.Condition),
					"unmet_reason": d.UnmetReason,
				})
			return "", false
		}
		return string(d.Condition), true
	}
	s.writeError(w, r, http.StatusForbidden, "delegation_not_configured",
		"the effective operator_agent block does not delegate this action (fail-closed)",
		map[string]any{"action": action})
	return "", false
}

// checkDeployPreflight is the deploy stage's PRE-execution approval gate
// (ADR-038 / #1384). It resolves the deploy stage from the run's cached
// workflow spec, collects its pre-flight constraints (allowed_environments /
// change_freeze / required_upstream), and refuses the approval (422 + a
// deploy_preflight_refused audit) when any is violated. Returns true to
// proceed; false after writing the error response.
//
// FAIL CLOSED (#1384, operator binding condition 1) — the inverse of
// checkPlanBudget's fail-open posture. A deploy stage's effect IS the side
// effect, so an unverifiable deploy must be DENIED, not waved through. Every
// can't-EVALUATE branch (nil repos, run-read failure, absent/unparseable
// spec, deploy stage not found) refuses with 422 deploy_preflight_unevaluable
// and a deploy_preflight_refused audit.
//
// NUANCE: a deploy stage whose spec parses but declares NO pre-flight
// constraints PASSES — there is nothing to enforce. Fail-closed targets the
// can't-evaluate-due-to-error path only, not the no-constraints-declared
// case.
func (s *Server) checkDeployPreflight(w http.ResponseWriter, r *http.Request, stage *run.Stage, comment string) bool {
	ctx := r.Context()

	if s.cfg.RunRepo == nil {
		s.refuseDeploy(w, r, stage, "deploy_preflight_unevaluable",
			"deploy pre-flight cannot be evaluated: run repository is not configured; an unverifiable deploy is denied (fail-closed)", nil)
		return false
	}
	runRow, err := s.cfg.RunRepo.GetRun(ctx, stage.RunID)
	if err != nil {
		s.refuseDeploy(w, r, stage, "deploy_preflight_unevaluable",
			"deploy pre-flight cannot be evaluated: run lookup failed; an unverifiable deploy is denied (fail-closed)",
			map[string]any{"error": err.Error()})
		return false
	}
	if len(runRow.WorkflowSpec) == 0 {
		s.refuseDeploy(w, r, stage, "deploy_preflight_unevaluable",
			"deploy pre-flight cannot be evaluated: the run carries no cached workflow spec; an unverifiable deploy is denied (fail-closed)", nil)
		return false
	}
	parsed, err := spec.ParseBytes(runRow.WorkflowSpec)
	if err != nil {
		s.refuseDeploy(w, r, stage, "deploy_preflight_unevaluable",
			"deploy pre-flight cannot be evaluated: the cached workflow spec does not parse; an unverifiable deploy is denied (fail-closed)",
			map[string]any{"error": err.Error()})
		return false
	}
	wf, ok := parsed.Workflows[runRow.WorkflowID]
	if !ok {
		s.refuseDeploy(w, r, stage, "deploy_preflight_unevaluable",
			"deploy pre-flight cannot be evaluated: the run's workflow is not in its cached spec; an unverifiable deploy is denied (fail-closed)",
			map[string]any{"workflow_id": runRow.WorkflowID})
		return false
	}
	var deployStage spec.Stage
	foundDeploy := false
	for _, st := range wf.Stages {
		if st.Type == spec.StageTypeDeploy {
			deployStage = st
			foundDeploy = true
			break
		}
	}
	if !foundDeploy {
		s.refuseDeploy(w, r, stage, "deploy_preflight_unevaluable",
			"deploy pre-flight cannot be evaluated: no deploy stage found in the run's workflow; an unverifiable deploy is denied (fail-closed)", nil)
		return false
	}

	// Collect the pre-flight constraints. NUANCE (#1384 condition 1): a
	// deploy stage that parses but declares NO pre-flight constraints passes
	// — there is nothing to enforce, and fail-closed targets the
	// can't-evaluate path, not the nothing-declared case.
	var (
		allowedEnvs   []string
		changeFreeze  bool
		requiredUp    []string
		hasConstraint bool
	)
	for _, c := range deployStage.Constraints {
		if len(c.AllowedEnvironments) > 0 {
			allowedEnvs = c.AllowedEnvironments
			hasConstraint = true
		}
		if c.ChangeFreeze != nil {
			changeFreeze = *c.ChangeFreeze
			hasConstraint = true
		}
		if len(c.RequiredUpstream) > 0 {
			requiredUp = c.RequiredUpstream
			hasConstraint = true
		}
	}
	if !hasConstraint {
		return true
	}

	// (a) allowed_environments — the requested target environment is read
	// from a `--environment=<env>` approval-comment flag (#1384 design
	// default, mirroring --override-budget's comment-flag convention).
	if len(allowedEnvs) > 0 {
		env := parseEnvironmentFlag(comment)
		if env == "" || !sliceContains(allowedEnvs, env) {
			s.refuseDeploy(w, r, stage, "deploy_environment_not_allowed",
				fmt.Sprintf("requested deploy environment %q is not in the deploy stage's allowed_environments %v; pass --environment=<env> with an allowed value in the approval comment", env, allowedEnvs),
				map[string]any{"requested_environment": env, "allowed_environments": allowedEnvs})
			return false
		}
	}

	// (b) change_freeze — a spec-declared `change_freeze: true` gates the
	// deploy. The live freeze-window signal is downstream (E23.5/6/10); in
	// this slice the operator overrides an active freeze with an explicit
	// --override-freeze comment flag (an explicit operator sub-action,
	// consistent with the issue's "never a blind retry" philosophy).
	if changeFreeze && !commentHasFlag(comment, "--override-freeze") {
		s.refuseDeploy(w, r, stage, "deploy_change_freeze_active",
			"the deploy stage declares change_freeze; a deploy during an active change freeze requires an explicit --override-freeze in the approval comment",
			map[string]any{"change_freeze": true})
		return false
	}

	// (c) required_upstream — ci_green and review_merged proxies (#1384
	// design default). A required upstream that is not satisfied refuses.
	for _, up := range requiredUp {
		switch up {
		case "ci_green":
			if !s.deployCIGreen(ctx, runRow) {
				s.refuseDeploy(w, r, stage, "deploy_upstream_not_satisfied",
					"required upstream ci_green is not satisfied: not every required status check has reported green on the implement stage",
					map[string]any{"required_upstream": up})
				return false
			}
		case "review_merged":
			if !s.deployReviewMerged(ctx, runRow) {
				s.refuseDeploy(w, r, stage, "deploy_upstream_not_satisfied",
					"required upstream review_merged is not satisfied: the run has no pull_request_url and a succeeded review stage",
					map[string]any{"required_upstream": up})
				return false
			}
		default:
			// Unrecognized required_upstream token: fail closed — an
			// upstream the gate cannot evaluate must not pass an
			// unverifiable deploy.
			s.refuseDeploy(w, r, stage, "deploy_upstream_not_satisfied",
				fmt.Sprintf("required upstream %q is not a recognized pre-flight signal; an unevaluable upstream denies the deploy (fail-closed)", up),
				map[string]any{"required_upstream": up})
			return false
		}
	}

	return true
}

// refuseDeploy emits a deploy_preflight_refused audit (system actor) and
// writes a 422 with the given code/message (#1384). Shared by every
// checkDeployPreflight refusal — both the can't-evaluate (fail-closed) path
// and the constraint-violation paths — so every deploy-gate refusal lands a
// uniform audit receipt carrying the specific reason code. Best-effort audit:
// a logged append failure never suppresses the refusal the gate already
// decided.
func (s *Server) refuseDeploy(w http.ResponseWriter, r *http.Request, stage *run.Stage, code, message string, details map[string]any) {
	if details == nil {
		details = map[string]any{}
	}
	details["stage_id"] = stage.ID.String()

	if s.cfg.AuditRepo != nil {
		payload, _ := json.Marshal(map[string]any{
			"stage_id":      stage.ID.String(),
			"refusal_code":  code,
			"refusal_field": message,
		})
		systemKind := audit.ActorKind("system")
		if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
			RunID:     stage.RunID,
			StageID:   &stage.ID,
			Timestamp: time.Now().UTC(),
			Category:  "deploy_preflight_refused",
			ActorKind: &systemKind,
			Payload:   payload,
		}); err != nil {
			s.cfg.Logger.Error("audit append failed for deploy_preflight_refused",
				"run_id", stage.RunID, "stage_id", stage.ID, "error", err.Error())
		}
	}

	s.writeError(w, r, http.StatusUnprocessableEntity, code, message, details)
}

// parseEnvironmentFlag extracts the value of a `--environment=<env>` flag
// from an approval comment (#1384). Returns the empty string when absent.
func parseEnvironmentFlag(comment string) string {
	const flag = "--environment="
	for _, tok := range strings.Fields(comment) {
		if strings.HasPrefix(tok, flag) {
			return strings.TrimPrefix(tok, flag)
		}
	}
	return ""
}

// commentHasFlag reports whether flag appears as a standalone,
// whitespace-delimited token in an approval comment (#1384 safety). Unlike
// strings.Contains, it does NOT match an embedded occurrence — so a comment
// like "do not --override-freeze" or "see --override-freeze-docs" does not
// count as the operator invoking the flag. Overriding a change freeze is an
// explicit operator sub-action; an incidental substring must never bypass the
// freeze gate.
func commentHasFlag(comment, flag string) bool {
	for _, tok := range strings.Fields(comment) {
		if tok == flag {
			return true
		}
	}
	return false
}

// sliceContains reports whether want is a member of xs.
func sliceContains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// deployCIGreen evaluates the required_upstream `ci_green` pre-flight signal
// (#1384): every required status check has reported green on the run's
// implement stage, reusing aggregateCIGreen over the run's
// RequiredChecksSnapshot. Returns false (not satisfied) when the snapshot or
// the stage-check repo is unwired, the implement stage is absent, the check
// read errors, or the aggregate is nil/false — the safe direction for a
// pre-execution deploy gate.
func (s *Server) deployCIGreen(ctx context.Context, runRow *run.Run) bool {
	if runRow.RequiredChecksSnapshot == nil || s.cfg.StageCheckRepo == nil {
		return false
	}
	implStage := s.findImplementStage(ctx, runRow.ID)
	if implStage == nil {
		return false
	}
	checks, err := s.cfg.StageCheckRepo.LatestForStage(ctx, implStage.ID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "deploy gate: list stage checks failed",
			slog.String("run_id", runRow.ID.String()),
			slog.String("error", err.Error()),
		)
		return false
	}
	g := aggregateCIGreen(runRow.RequiredChecksSnapshot.Contexts, checks)
	return g != nil && *g
}

// deployReviewMerged evaluates the required_upstream `review_merged`
// pre-flight signal (#1384): the run carries a pull_request_url AND a
// succeeded review stage — a proxy for "the change merged", since merged
// state is not tracked on the run row today (the precise signal tightens when
// the deploy executor lands, E23.5/6/10). Returns false on a stage-list error
// (the safe direction).
func (s *Server) deployReviewMerged(ctx context.Context, runRow *run.Run) bool {
	if runRow.PullRequestURL == nil || *runRow.PullRequestURL == "" {
		return false
	}
	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, runRow.ID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "deploy gate: list stages failed",
			slog.String("run_id", runRow.ID.String()),
			slog.String("error", err.Error()),
		)
		return false
	}
	for _, st := range stages {
		if st.Type == run.StageTypeReview && st.State == run.StageStateSucceeded {
			return true
		}
	}
	return false
}
