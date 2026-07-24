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
	"gopkg.in/yaml.v3"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/budget"
	"github.com/kuhlman-labs/fishhawk/backend/internal/delegation"
	"github.com/kuhlman-labs/fishhawk/backend/internal/drive"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/operatorrole"
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
	// RemoveScopeFiles is an explicit list of repo-relative paths to REMOVE
	// from the implement stage's effective scope.files on approve (#1726). It
	// is the inverse of AddScopeFiles: combined with it in the same approve
	// call, an operator expresses a scope REPLACE (remove old + add new) at
	// the plan gate with zero planner invocations. Removal subtracts from the
	// effective scope the prompt builder hands the runner, so every runner
	// gate (created-out-of-scope, category-B, scope-cap) honors it. Recorded
	// on the approval audit payload with before/after effective-scope lists.
	// Validated pre-Submit: each path must be repo-relative, present in the
	// current effective scope, and a removal that would empty a non-empty
	// scope is refused (an empty scope re-enables the runner's `git add -A`
	// fallback, disabling enforcement). Declared here so the
	// DisallowUnknownFields decode accepts it; callers omit it (omitempty) and
	// stay byte-identical to today.
	RemoveScopeFiles []string `json:"remove_scope_files,omitempty"`
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
	// ClaimsConcernIDs is an OPTIONAL list of plan-stage concern ids this
	// approval's binding condition answers (E48.9 / #1956). It is the
	// operator-confirmed, explicit lineage link (no NLP/heuristic matching):
	// each claimed concern auto-resolves to the terminal
	// addressed_by_condition state once ONE implement review returns a
	// confirming (non-reject) verdict — the operator's condition is the
	// authority, the reviewer the witness. Validated pre-Submit via
	// validateClaimsConcernIDs (approve-only, plan-stage-only, each id an OPEN
	// plan-stage concern of THIS run), so a malformed claim inserts no approval
	// row. Recorded on the approval_submitted audit payload alongside
	// binding_assertions and loaded back by loadApprovalConcernClaims. Declared
	// here so the DisallowUnknownFields decode accepts it; callers omit it
	// (omitempty) and stay byte-identical to today.
	ClaimsConcernIDs []string `json:"claims_concern_ids,omitempty"`
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
	// PlanModel is the OPTIONAL operator override for the PLAN stage's model
	// (#1416) — the highest rung of the plan-model ladder (deployment default <
	// spec executor.model (plan stage) < this operator override). On a
	// plan-stage approve the gate resolves the plan ladder with this as the
	// operator rung and emits the plan stage's model_resolved audit; a
	// re-dispatched plan stage then spawns under the resolved value
	// (resolvePlanModelForRun reads the gate entry). Empty (the default) leaves
	// resolution to the lower rungs and stays byte-identical to today. Declared
	// here so the DisallowUnknownFields decode accepts it; callers omit it
	// (omitempty).
	PlanModel string `json:"plan_model,omitempty"`
	// ReviewModel is the OPTIONAL operator override for the REVIEW stage's model
	// (#1416) — the highest rung of the review-model ladder (deployment default
	// < spec executor.model (review stage) < this operator override). On a
	// plan-stage approve the gate resolves the review ladder with this as the
	// operator rung and emits the review stage's model_resolved audit; the
	// post-plan-gate implement review (and any post-gate re-review) then invokes
	// each reviewer under the resolved value (resolveReviewerInvocations reads
	// gateResolvedReviewModel). Per the operator's binding approval condition it
	// governs the implement review, NOT the already-completed plan review. Empty
	// (the default) leaves the reviewer on its spec model, byte-identical to
	// today. Declared here so the DisallowUnknownFields decode accepts it;
	// callers omit it (omitempty).
	ReviewModel string `json:"review_model,omitempty"`
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

	// Condition-claim declaration validation (E48.9 / #1956): when an approve
	// carries claims_concern_ids, validate each claimed id resolves to an OPEN
	// plan-stage concern of THIS run BEFORE ApprovalRepo.Submit — like the
	// binding_assertions gate, a malformed claim inserts no approval row so a
	// corrected retry flows normally. No resolution runs here; the confirming
	// implement-review hook resolves the claims post-implement. Reject / empty
	// approves skip this and stay byte-identical to today.
	if !s.validateClaimsConcernIDs(w, r, stage, req.Decision, req.ClaimsConcernIDs) {
		return
	}

	// Author separation-of-duties (E39.4 / #1709): when the stage's gate
	// carries a forge-neutral approvals block, the change author may not
	// approve their own change. PRE-Submit like the sibling gates so a
	// refused approval inserts no row and the quorum count is not
	// incremented. Guarded strictly by "gate has an Approvals block AND
	// decision == approve" — a legacy Approvers / no-approvals gate skips
	// this entirely (byte-identical to today). Fail-open on an unresolved
	// author (no user-kind actor yet) or an unreadable spec: author-SoD is
	// skipped while agent-SoD and quorum still apply.
	if decision == approval.DecisionApprove {
		if approvals, aerr := s.fetchApprovalsForStage(r.Context(), stage); aerr == nil && approvals != nil {
			if author, ok := s.resolveChangeAuthor(r.Context(), stage.RunID); ok && author == subject {
				s.writeError(w, r, http.StatusForbidden, "approver_is_change_author",
					"the change author cannot approve their own change on a quorum gate",
					map[string]any{"stage_id": stageID.String(), "subject": subject})
				return
			}
		}
	}

	// Approval-gate predicate resolution (E39.5 / #1710): when the stage's
	// gate carries an approvals block that sets min_permission and/or
	// member_of, and the submitter is a real human (not a delegated / agent
	// submission), resolve the predicate against the forge PRE-Submit. An
	// insufficient permission or non-membership rejects the approver (403,
	// no row inserted); an unresolvable forge (error / rate-limit / empty
	// repo) fails the gate closed with a retryable 503. On success the
	// resolved values thread into the approval_submitted row's
	// predicate_snapshot. Reject / legacy-gate / count-only paths skip this
	// entirely (predicateRes stays nil), byte-identical to today.
	var predicateRes *predicateResolution
	if decision == approval.DecisionApprove {
		res, ok := s.checkApprovalPredicates(w, r, stage, subject, req.Delegated)
		if !ok {
			return
		}
		predicateRes = res
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
		// Gate-time scope removal (#1726): validate the remove_scope_files
		// shape and the two semantic fail-closed modes (path not in the
		// current effective scope; a removal that would empty a non-empty
		// scope) PRE-Submit, before the cap gate reads the post-removal count.
		// A refused approval inserts no row, so a corrected retry flows
		// normally. No-removal approves skip this entirely (empty slice).
		trimmedRemove, ok := s.checkRemoveScopeFiles(w, r, stage, req.AddScopeFiles, req.RemoveScopeFiles)
		if !ok {
			return
		}
		// Thread the trimmed removal paths back into the request so every
		// downstream consumer (checkPlanScopeCap, writeApprovalAudit, the
		// prompt-builder subtraction) subtracts the normalized scope path rather
		// than the raw whitespace-padded input (#1726). Without this a value like
		// " backend/b.go " passes the trimmed presence/empty checks yet fails to
		// subtract the actual scope entry backend/b.go downstream.
		req.RemoveScopeFiles = trimmedRemove
		// Single-owner-file gate (#2103): refuse an approve that supplies
		// add_scope_files on a DECOMPOSED plan, because an added path fans into
		// EVERY slice's effective scope (no per-slice add channel), guaranteeing
		// an add/add fan-in conflict. PRE-Submit for the same ADR-036 reason as
		// its siblings — a refused approve records no row, so a corrected retry
		// after re-planning the decomposition flows normally. Placed BEFORE the
		// scope-cap gate so this categorical (no-override) error precedes the
		// override-able cap error. Only add_scope_files is gated; a subtractive
		// remove_scope_files fan-out is harmless (removing an absent path no-ops).
		if !s.checkDecomposedAddScopeFiles(w, r, stage, req.AddScopeFiles) {
			return
		}
		// Scope-cap gate (#983): refuse an approve whose effective scope
		// (plan scope.files ∪ add_scope_files ∖ remove_scope_files) exceeds
		// the implement stage's max_files_changed, unless the comment carries
		// --override-scope-cap. PRE-Submit for the same ADR-036 reason as
		// checkPlanReviewSettled: a refused approval must insert no row so
		// a retry after re-scope or with the override flows normally
		// (post-Submit, the idempotent-first-wins retry would skip gates).
		if !s.checkPlanScopeCap(w, r, stage, req.Comment, req.AddScopeFiles, req.RemoveScopeFiles) {
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

		// Plan/review allow-list parity (#1416): the implement gate above
		// validates only the implement model. Validate the RESOLVED plan and
		// review models (the same ladders writeStageModelResolutions emits) against
		// their per-adapter allow-lists too, PRE-Submit for the same ADR-036 reason:
		// a 422 inserts no row so a corrected re-approval flows normally. Fail-OPEN
		// when a policy is unset (byte-identical to today).
		if !s.checkStageModelsAllowed(w, r, stage, req.PlanModel, req.ReviewModel) {
			return
		}
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

	// Gate-action core (E25.6 / ADR-047): the approval Submit + audit +
	// model-resolution emission + state advance + orchestrator handoff +
	// drive stamp + notifications are factored into approveStageAs, an
	// identity-parameterised service method the in-process campaign
	// auto-driver also calls. The HTTP handler owns every pre-Submit gate
	// above; the result/error it returns is rendered to HTTP here exactly as
	// the prior inline core did (duplicate 200, InvalidTransition 409, and
	// the two distinct submit/advance 500 messages).
	result, err := s.approveStageAs(r.Context(), ident, approveActionParams{
		Stage:               stage,
		Decision:            decision,
		Comment:             req.Comment,
		CommentPtr:          commentPtr,
		ApproverGithubLogin: req.ApproverGithubLogin,
		AddScopeFiles:       req.AddScopeFiles,
		RemoveScopeFiles:    req.RemoveScopeFiles,
		BindingAssertions:   req.BindingAssertions,
		ClaimsConcernIDs:    req.ClaimsConcernIDs,
		DelegatedRule:       delegatedRule,
		ResolvedModel:       resolvedModel,
		PlanModel:           req.PlanModel,
		ReviewModel:         req.ReviewModel,
		PredicateResolution: predicateRes,
	})
	if err != nil {
		var aerr *approveActionError
		if errors.As(err, &aerr) && aerr.failedAt == gateActionAdvance {
			var inv run.InvalidTransitionError
			if errors.As(aerr.err, &inv) {
				s.writeError(w, r, http.StatusConflict, "invalid_state_transition",
					aerr.err.Error(),
					map[string]any{"stage_id": stageID.String(),
						"from": inv.From, "to": inv.To})
				return
			}
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"transition stage failed", map[string]any{"error": aerr.err.Error()})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"submit approval failed", map[string]any{"error": err.Error()})
		return
	}

	if result.Duplicate != nil {
		s.writeJSON(w, r, http.StatusOK, duplicateApprovalResponse(stage, result.Duplicate))
		return
	}

	s.writeJSON(w, r, http.StatusOK, toStageResponse(result.Stage))
}

// gateActionStage names where in approveStageAs a failure occurred, so the
// HTTP handler can reproduce the two distinct 500 messages (Submit vs
// advance) plus the advance-only InvalidTransition → 409 mapping.
type gateActionStage int

const (
	gateActionSubmit gateActionStage = iota
	gateActionAdvance
)

// approveActionError wraps a failure from the advance step of approveStageAs
// so the caller can distinguish it from a Submit failure (the two map to
// different HTTP responses). A Submit failure is returned UNWRAPPED, so
// errors.As against *approveActionError is the discriminator.
type approveActionError struct {
	failedAt gateActionStage
	err      error
}

func (e *approveActionError) Error() string { return e.err.Error() }
func (e *approveActionError) Unwrap() error { return e.err }

// gateActionScopeError is returned by the extracted gate-action service
// methods (approveStageAs / fixupStageAs / retryStageAs) when the acting
// identity lacks the write scope the matching HTTP handler enforces. The
// HTTP path never produces it — the handler's requireWriteScope / inline
// hasScope check runs first and 401/403s before the service method is
// reached — but the in-process campaign auto-driver (E25.6 / ADR-047) calls
// the service methods directly, so the authz check must also live here or
// the auto-driver would act with an under-scoped identity (the authz
// regression #1445 flagged). The error is non-nil and surfaces to the
// driver as a dispatch failure; the actor never silently acts unauthorized.
type gateActionScopeError struct {
	scope string
}

func (e *gateActionScopeError) Error() string {
	return "identity is missing required scope: " + e.scope
}

// identityHasGateScope reports whether id is authorized for a gate action
// gated on any of scopes, mirroring the handler scope checks exactly: an
// anonymous identity is never authorized; a cookie-session identity
// (TokenID == "") is exempt from scope enforcement (OAuth callers carry no
// scope list, matching requireWriteScope and the fixup/retry inline checks);
// a token identity must carry at least one of scopes.
func identityHasGateScope(id Identity, scopes ...string) bool {
	if id.IsAnonymous() {
		return false
	}
	if id.TokenID == "" {
		return true
	}
	for _, sc := range scopes {
		if hasScope(id, sc) {
			return true
		}
	}
	return false
}

// approveActionParams carries the resolved inputs for approveStageAs. The
// HTTP handler computes them from the request body + every pre-Submit gate;
// the in-process campaign auto-driver (E25.6) supplies them directly.
type approveActionParams struct {
	Stage               *run.Stage
	Decision            approval.Decision
	Comment             string
	CommentPtr          *string
	ApproverGithubLogin string
	AddScopeFiles       []string
	RemoveScopeFiles    []string
	BindingAssertions   []bindingAssertion
	ClaimsConcernIDs    []string
	DelegatedRule       string
	ResolvedModel       *ResolvedModel
	PlanModel           string
	ReviewModel         string
	// PredicateResolution, when non-nil, carries the forge-resolved
	// min_permission / member_of values a satisfied approval-gate predicate
	// resolution produced (E39.5 / #1710). The HTTP handler stashes it from
	// checkApprovalPredicates; the in-process campaign auto-driver leaves it
	// nil (its snapshot records only the required values). Threaded into the
	// quorum-path predicate_snapshot's resolved fields.
	PredicateResolution *predicateResolution
}

// approveActionResult is approveStageAs's success outcome: either the
// advanced stage, or — when the (stage, subject) approval already existed —
// the prior approval row labelling a duplicate submission (no audit, no
// advance; the first decision stands).
type approveActionResult struct {
	Stage     *run.Stage
	Duplicate *approval.Approval
}

// approveStageAs performs the gate-action core of POST
// /v0/stages/{id}/approvals under the given identity: ApprovalRepo.Submit,
// the approval_submitted audit write, the model_resolved emissions
// (#1013/#1416), the state advance (advanceForDecision, which special-cases
// the deploy pre-execution gate), the orchestrator handoff on approve AND
// reject, the drive plan-approved stamp, and the plan-comment + sticky-status
// notifications. It is identity-parameterised so the HTTP handler and the
// in-process campaign auto-driver (E25.6 / ADR-047) drive the identical path
// and stamp identical audit.
//
// Ordering is preserved from the prior inline core: the audit + model writes
// precede advance (#1351) so a dispatch racing the transition observes them;
// the pre-advance Stage row is used for those writes (advance mutates only
// State, not the ID/RunID they read), and the advanced row drives the
// orchestrator/drive/notify steps. A Submit failure is returned unwrapped; an
// advance failure is wrapped in *approveActionError so the caller maps
// InvalidTransition → 409 and the two distinct 500 messages.
func (s *Server) approveStageAs(ctx context.Context, id Identity, p approveActionParams) (*approveActionResult, error) {
	// Enforce the approve gate's write scope on the acting identity. The HTTP
	// handler already gated via requireWriteScope, so this is a no-op on that
	// path; it is the authz check for the in-process campaign auto-driver,
	// which reaches this method directly (#1445).
	if !identityHasGateScope(id, "write:approvals") {
		return nil, &gateActionScopeError{scope: "write:approvals"}
	}
	// Resolve the acting subject from the identity with the same
	// "anonymous" fallback the handler applies, so the recorded
	// ApproverSubject (and the actor kind derived from it) is byte-identical
	// to the HTTP path.
	subject := id.Subject
	if subject == "" {
		subject = "anonymous"
	}
	res, err := s.cfg.ApprovalRepo.Submit(ctx, approval.SubmitParams{
		StageID:         p.Stage.ID,
		ApproverSubject: subject,
		Decision:        p.Decision,
		Comment:         p.CommentPtr,
		Surface:         approval.SurfaceAPI,
	})
	if err != nil {
		return nil, err
	}

	// Only the FIRST submission for this approver triggers a stage
	// transition. A concurrent second submission that lost the race past the
	// duplicate pre-check gets the same labeled duplicate 200 the pre-check
	// produces.
	if !res.Inserted {
		return &approveActionResult{Duplicate: res.Approval}, nil
	}

	// Quorum evaluation (E39.4 / #1709): only an APPROVE against a gate
	// carrying a forge-neutral approvals block engages distinct-approver
	// quorum. Every other path — a reject, or a legacy Approvers /
	// no-approvals gate — keeps today's first-vote-advances semantics. The
	// fetch fails open to the legacy path on any spec-read error, matching
	// checkApproverAuthorization's best-effort posture.
	var approvals *spec.Approvals
	if p.Decision == approval.DecisionApprove {
		if a, ferr := s.fetchApprovalsForStage(ctx, p.Stage); ferr == nil {
			approvals = a
		}
	}

	// Channel is recorded on every approval row (ADR-055 additive
	// enrichment), independent of whether quorum applies.
	channel := approvalChannel(id, p.DelegatedRule != "")

	if approvals == nil {
		// Legacy / no-approvals path: first vote advances, unchanged from
		// today except the additive identity{provider,subject}/channel
		// enrichment (ADR-055 record leg) on the approval_submitted row. No
		// predicate_snapshot — the gate declares no approvals block
		// (operator binding condition 2).
		s.writeApprovalAudit(ctx, p.Stage, res.Approval, p.Comment, p.ApproverGithubLogin, p.AddScopeFiles, p.RemoveScopeFiles, p.BindingAssertions, p.ClaimsConcernIDs, p.DelegatedRule, id.AuthMethod, channel, nil)
		return s.finishApprovalAdvance(ctx, p, res)
	}

	// Quorum path. Resolve the change author (fail-open: skipped when no
	// user-kind actor exists yet), then classify this submitter. A delegated
	// / agent-kind submission is recorded but never counts toward the human
	// quorum, and its channel is forced to "delegated".
	changeAuthor, _ := s.resolveChangeAuthor(ctx, p.Stage.RunID)
	submitterAgent := actorKindForSubject(res.Approval.ApproverSubject) == audit.ActorAgent
	delegated := submitterAgent || p.DelegatedRule != ""
	if delegated {
		channel = "delegated"
	}

	eligibleCount := s.countDistinctEligibleApprovers(ctx, p.Stage.RunID, p.Stage.ID, changeAuthor)
	required := 1
	if approvals.Count != nil {
		required = *approvals.Count
	}
	// A delegated/agent submission never advances the gate even if the count
	// otherwise suffices — it does not itself count and no human vote is
	// implied by it.
	reached := !delegated && eligibleCount >= required

	snapshot := &predicateSnapshot{
		CountRequired:  required,
		CountEligible:  eligibleCount,
		Identity:       snapshotIdentityFor(res.Approval.ApproverSubject),
		SubmitterClass: submitterClass(res.Approval.ApproverSubject, changeAuthor, submitterAgent),
		AuthMethod:     id.AuthMethod,
		Channel:        channel,
		MinPermission:  approvals.MinPermission,
		MemberOf:       approvals.MemberOf,
		QuorumReached:  reached,
	}
	// Record the forge-resolved predicate outcome on the counted-approver
	// row when the handler resolved it (E39.5 / #1710). The campaign
	// auto-driver / agent path leaves PredicateResolution nil, so its
	// snapshot records only the required values — byte-identical to today.
	if p.PredicateResolution != nil {
		snapshot.ResolvedPermission = p.PredicateResolution.ResolvedPermission
		snapshot.MemberResolved = p.PredicateResolution.MemberResolved
		snapshot.PredicateResult = "satisfied"
	}
	// Persist the enriched approval audit BEFORE any advance (#1351) so a
	// dispatch racing the transition observes it. Best-effort append.
	s.writeApprovalAudit(ctx, p.Stage, res.Approval, p.Comment, p.ApproverGithubLogin, p.AddScopeFiles, p.RemoveScopeFiles, p.BindingAssertions, p.ClaimsConcernIDs, p.DelegatedRule, id.AuthMethod, channel, snapshot)

	if !reached {
		// Recorded but below quorum (or a delegated/agent submission that
		// never counts): surface the state change but do NOT advance — the
		// stage stays awaiting_approval until approvals.count distinct
		// eligible approvers vote.
		s.notifyStatusUpdate(ctx, p.Stage.RunID, "approval_submit")
		return &approveActionResult{Stage: p.Stage}, nil
	}
	return s.finishApprovalAdvance(ctx, p, res)
}

// finishApprovalAdvance performs the post-audit tail shared by the legacy
// first-vote path and the quorum-reached path: the model_resolved emissions
// (#1013/#1416), the state advance (advanceForDecision, which special-cases
// the deploy pre-execution gate), the orchestrator handoff on approve AND
// reject, the drive plan-approved stamp, and the plan-comment + sticky-status
// notifications. The approval_submitted audit row is already written by the
// caller. An advance failure is wrapped in *approveActionError so the caller
// maps InvalidTransition → 409 and the two distinct 500 messages.
func (s *Server) finishApprovalAdvance(ctx context.Context, p approveActionParams, res *approval.SubmitResult) (*approveActionResult, error) {
	// Model resolution (#1013, extended #1416): emit the source-tagged
	// model_resolved audit entries the gate computed on this plan-stage
	// approve. Emitted even when a resolution is empty (the readers treat it
	// as a deliberate default spawn); must precede advance for the same race
	// reason as the approval audit. A nil ResolvedModel (GetRun fail-open, or
	// a non-plan/reject path) emits NOTHING.
	if p.ResolvedModel != nil {
		s.writeStageModelResolutions(ctx, p.Stage, res.Approval, *p.ResolvedModel, p.PlanModel, p.ReviewModel)
	}

	advanced, err := s.advanceForDecision(ctx, p.Stage, p.Decision)
	if err != nil {
		return nil, &approveActionError{failedAt: gateActionAdvance, err: err}
	}

	// Hand off to the orchestrator on both approve AND reject — approve
	// dispatches the next stage; reject walks the run's state machine to
	// terminal. Best-effort: the gate already passed/rejected and the audit
	// row is in place, so an orchestration failure logs and lets a follow-up
	// call recover.
	if s.cfg.Orchestrator != nil {
		if _, err := s.cfg.Orchestrator.Advance(ctx, advanced.RunID); err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelError,
				"orchestrator advance failed",
				slog.String("run_id", advanced.RunID.String()),
				slog.String("stage_id", advanced.ID.String()),
				slog.String("error", err.Error()),
			)
		}
	}

	// Drive (#1023): a plan-gate approval on a drive-enabled run is the
	// plan_approved_dispatch transition point — stamp it after the
	// orchestrator block so the entry documents an advance that was actually
	// attempted.
	if p.Decision == approval.DecisionApprove && advanced.Type == run.StageTypePlan {
		s.recordDrivePlanApproved(ctx, advanced)
		// On-approval split-proposal child filing (#2057, E50.5): when the
		// approved plan carries a split_proposal, file the phased children,
		// classify the contract phase, and emit the split_children_filed
		// completion marker. Best-effort like recordDrivePlanApproved above — a
		// failure logs and never unwinds the approval the gate already recorded;
		// a plan without a split_proposal no-ops.
		s.fileSplitProposalChildren(ctx, advanced)
	}

	// Plan-comment re-render (#377): a plan-stage approve or reject re-fires
	// the plan-on-issue hook. Best-effort: notifyPlanReady logs but never
	// unwinds the approval.
	if advanced.Type == run.StageTypePlan {
		s.notifyPlanReady(ctx, advanced.RunID, advanced)
	}

	// Sticky status comment (E20.4 / #330). Every approval changes the run's
	// surface state and is worth surfacing in the issue thread.
	s.notifyStatusUpdate(ctx, advanced.RunID, "approval_submit")

	return &approveActionResult{Stage: advanced}, nil
}

// checkApprovalPredicates resolves the stage gate's forge predicates
// (min_permission / member_of) against the submitter PRE-Submit (E39.5 /
// #1710). It returns (resolution, true) to continue to Submit — the
// resolution is non-nil and carries the resolved values ONLY when a
// predicate was actually evaluated and satisfied; it is nil when the gate is
// not predicate-guarded (no approvals block, no predicate fields) or the
// submission is a delegated / agent one that is recorded-but-never-counted
// and so not forge-gated. It returns (nil, false) after writing the response
// on a rejection (403 approver_predicate_unmet) or an unresolvable forge
// (503 forge_unavailable) — in both cases no approval row is inserted, so a
// corrected retry (or a retry once the forge is reachable) flows normally.
//
// Fail-open on the gate READ: a nil approvals block or a spec-read error
// falls through to today's path (matching checkApproverAuthorization's
// best-effort posture), NOT on the forge RESOLUTION, which fails closed.
func (s *Server) checkApprovalPredicates(w http.ResponseWriter, r *http.Request, stage *run.Stage, subject string, delegated bool) (*predicateResolution, bool) {
	approvals, err := s.fetchApprovalsForStage(r.Context(), stage)
	if err != nil || approvals == nil {
		return nil, true
	}
	// Only the two forge predicates gate here. A count-only approvals block
	// keeps the pure-quorum path.
	if approvals.MinPermission == "" && approvals.MemberOf == "" {
		return nil, true
	}
	// A delegated / agent-kind submission is recorded but never counted
	// toward the human quorum, so it is not forge-gated either.
	if delegated || actorKindForSubject(subject) == audit.ActorAgent {
		return nil, true
	}

	// Resolve the run's target repo ("owner/name"). A read failure or an
	// empty repo leaves resolvePredicates to fail closed (unavailable)
	// rather than wave the approver through.
	var repo string
	if runRow, rerr := s.cfg.RunRepo.GetRun(r.Context(), stage.RunID); rerr == nil {
		repo = runRow.Repo
	}

	outcome, resolution, predicate := s.resolvePredicates(r.Context(), repo, subject, approvals)
	switch outcome {
	case predicateSatisfied:
		return resolution, true
	case predicateRejected:
		// Durably record the rejection in a predicate_snapshot audit entry
		// even though no approval row is inserted.
		s.writePredicateRejectionAudit(r.Context(), stage, subject, approvals, resolution)
		s.writeError(w, r, http.StatusForbidden, "approver_predicate_unmet",
			"the approver does not meet the gate's forge predicate (permission tier or membership)",
			map[string]any{
				"stage_id":            stage.ID.String(),
				"subject":             subject,
				"required_permission": approvals.MinPermission,
				"resolved_permission": resolution.ResolvedPermission,
				"member_of":           approvals.MemberOf,
				"member_resolved":     resolution.MemberResolved,
				"result":              "rejected",
			})
		return nil, false
	default: // predicateUnavailable
		s.writeError(w, r, http.StatusServiceUnavailable, "forge_unavailable",
			"the forge permission/membership API was unavailable; the approval gate failed closed",
			map[string]any{
				"stage_id":  stage.ID.String(),
				"retryable": true,
				"predicate": predicate,
				"ref":       approvals.MemberOf,
				"next_actions": []string{
					"Retry the approval once the forge is reachable; the forge permission/membership API was unavailable",
				},
			})
		return nil, false
	}
}

// writePredicateRejectionAudit appends a best-effort audit entry recording a
// rejected forge-predicate evaluation (E39.5 / #1710) so the rejection is
// durably captured in a predicate_snapshot even though no approval row is
// inserted. The category "approval_predicate_rejected" is a new string value
// (no closed-set category validator exists in package server) and posts no
// issue comment. Best-effort: a failure logs but never unwinds the 403.
func (s *Server) writePredicateRejectionAudit(ctx context.Context, stage *run.Stage, subject string, approvals *spec.Approvals, resolution *predicateResolution) {
	if s.cfg.AuditRepo == nil {
		return
	}
	snapshot := map[string]any{
		"result": "rejected",
	}
	if approvals.MinPermission != "" {
		snapshot["min_permission"] = approvals.MinPermission
		snapshot["resolved_permission"] = resolution.ResolvedPermission
	}
	if approvals.MemberOf != "" {
		snapshot["member_of"] = approvals.MemberOf
		snapshot["member_resolved"] = resolution.MemberResolved
	}
	actorKind := actorKindForSubject(subject)
	subj := subject
	payload, _ := json.Marshal(map[string]any{
		"stage_id":           stage.ID.String(),
		"subject":            subject,
		"predicate_snapshot": snapshot,
	})
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:        stage.RunID,
		StageID:      &stage.ID,
		Timestamp:    time.Now().UTC(),
		Category:     "approval_predicate_rejected",
		ActorKind:    &actorKind,
		ActorSubject: &subj,
		Payload:      payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"approval: predicate rejection audit append failed",
			slog.String("stage_id", stage.ID.String()),
			slog.String("subject", subject),
			slog.String("error", err.Error()),
		)
	}
}

// campaignOperatorIdentity builds the in-process Identity the campaign
// auto-driver (E25.6 / ADR-047) acts under when it takes a delegated gate
// action via the extracted approveStageAs/fixupStageAs/retryStageAs methods.
// The subject is the stable operator-agent attribution
// (operatorrole.CampaignActorSubject), which actorKindForSubject stamps as
// audit.ActorAgent, and the scope set is the gate-action write scopes the
// handlers enforce. TokenID is set NON-empty so requireWriteScope applies the
// same scope check it applies to an HTTP bearer token (scope-acceptance
// parity) rather than the cookie-session bypass — the in-process actor must
// hold the scopes, not be waved through.
func campaignOperatorIdentity() Identity {
	return Identity{
		Subject: operatorrole.CampaignActorSubject,
		TokenID: "operator-agent-campaign",
		Scopes:  operatorrole.CampaignActorScopes(),
	}
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
		// #1912: the local park is now an explicit stage state
		// (awaiting_host_dispatch), not the conflated 'dispatched'. Compose the
		// drive-audit To string as implement:awaiting_host_dispatch so the audit
		// trail names the exact parked state the orchestrator wrote, and the
		// MCP host-dispatch marker (or an auto-dispatching drive loop) flips it
		// to dispatched at spawn time.
		adv.To = "implement:awaiting_host_dispatch"
		adv.Event = "plan gate approved; runner_kind local parks the implement stage at awaiting_host_dispatch for a host-side dispatch"
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

	allowed, err := s.cfg.RoleResolver.CanApprove(r.Context(), gate.scope, gate.approvers, gate.roles, subject)
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
// the run's credential scope (so the resolver can reach the forge).
type gateContext struct {
	approvers *spec.Approvers
	roles     map[string]spec.Role
	scope     forge.CredentialScope
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
					approvers: gate.Approvers,
					roles:     parsed.Roles,
					scope:     forge.FromGitHubInstallationID(*runRow.InstallationID),
				}, nil
			}
		}
		// Stage exists but has no approval gate.
		return &gateContext{roles: parsed.Roles, scope: forge.FromGitHubInstallationID(*runRow.InstallationID)}, nil
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
// authMethod records how the acting bearer api_token was authenticated
// (E39.3 / #1708): "static" for operator-minted tokens, "oauth" for
// device-flow tokens. Recorded under auth_method when non-empty so a
// decision's audit provenance names the credential kind; empty for
// cookie-session / MCP-token / operator-agent-driver identities, where
// the key is omitted (byte-identical to pre-#1708 payloads).
//
// ADR-055 additive enrichment (E39.4 / #1709): EVERY approval_submitted row
// — legacy gates included — additionally records identity{provider,subject}
// (the submitter's provider-qualified identity) and channel
// (interactive|api|delegated). snapshot, when non-nil, records the quorum
// predicate evaluation under predicate_snapshot; it is nil (key omitted) for
// gates with no approvals block. All new keys ride INSIDE the existing
// hashed payload JSONB — no new top-level audit.Entry / Export v1 field — so
// the hash chain and the E9 verifier's strict decode are unaffected.
func (s *Server) writeApprovalAudit(ctx context.Context, stage *run.Stage, app *approval.Approval, comment, approverGithubLogin string, addScopeFiles, removeScopeFiles []string, bindingAssertions []bindingAssertion, claimsConcernIDs []string, delegatedRule, authMethod, channel string, snapshot *predicateSnapshot) {
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
	if authMethod != "" {
		auditPayload["auth_method"] = authMethod
	}
	// ADR-055 additive identity enrichment (#1709): recorded on every
	// approval_submitted row, legacy gates included. The subject stays the
	// full provider-qualified subject (provenance); provider is the parsed
	// prefix ("" for a bare / prefixless subject).
	provider, _ := splitProviderSubject(app.ApproverSubject)
	auditPayload["identity"] = map[string]any{
		"provider": provider,
		"subject":  app.ApproverSubject,
	}
	if channel != "" {
		auditPayload["channel"] = channel
	}
	// predicate_snapshot is present IFF the gate declares an approvals block
	// (operator binding condition 2); legacy-gate rows pass a nil snapshot.
	if snapshot != nil {
		auditPayload["predicate_snapshot"] = snapshot
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
	// Gate-time scope removal (#1726): record the authoritative paths removed
	// from the implement scope, plus the before/after effective-scope file
	// lists so the removal is durably auditable. Only on approve with a
	// non-empty removal set; the prompt builder reads remove_scope_files back
	// via loadApprovalRemoveScopeFiles. The before/after lists come from the
	// single-source effectiveScopePathSet helper (remove=nil vs
	// remove=removeScopeFiles), the same set the cap gate counts and the
	// prompt builder assembles. A fail-open unresolved set (ok=false) omits
	// the before/after keys but still records remove_scope_files.
	if app.Decision == approval.DecisionApprove && len(removeScopeFiles) > 0 {
		auditPayload["remove_scope_files"] = removeScopeFiles
		if before, _, ok := s.effectiveScopePathSet(ctx, stage.RunID, addScopeFiles, nil); ok {
			auditPayload["scope_files_before"] = before
			if after, _, ok := s.effectiveScopePathSet(ctx, stage.RunID, addScopeFiles, removeScopeFiles); ok {
				auditPayload["scope_files_after"] = after
			}
		}
	}
	// Binding-assertion declaration (#1171): record the operator's declared
	// assertions so the prompt builder reads them back via
	// loadApprovalBindingAssertions. Only on approve with a non-empty slice;
	// the key is omitted otherwise so a no-declaration approve is
	// byte-identical to today.
	if app.Decision == approval.DecisionApprove && len(bindingAssertions) > 0 {
		auditPayload["binding_assertions"] = bindingAssertions
	}
	// Condition-claim declaration (E48.9 / #1956): record the plan-stage
	// concern ids this approval's binding condition answers, so the confirming
	// implement-review hook reads them back via loadApprovalConcernClaims. Only
	// on approve with a non-empty slice; the key is omitted otherwise so a
	// no-claim approve is byte-identical to today.
	if app.Decision == approval.DecisionApprove && len(claimsConcernIDs) > 0 {
		auditPayload["claims_concern_ids"] = claimsConcernIDs
	}
	if delegatedRule != "" {
		auditPayload["delegated"] = delegatedRule
	}
	payload, _ := json.Marshal(auditPayload)

	approver := app.ApproverSubject
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
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

// checkStageModelsAllowed is the plan/review allow-list gate (#1416), the
// plan-stage parity of checkPlanModelAllowed. It validates the RESOLVED plan and
// review models — the very ladders writeStageModelResolutions re-resolves and
// emits — against PlanAllowedModels / ReviewAllowedModels. Returns true to
// proceed; returns false after writing a 422 (plan_model_not_allowed /
// review_model_not_allowed, naming the resolved SOURCE) on the first disallowed
// model.
//
// Fail-OPEN throughout, matching the sibling implement gate:
//   - GetRun failure returns true: proceed, leaving the resolution to the
//     post-advance writeStageModelResolutions, rather than blocking on a read
//     error.
//   - An empty resolved model (ModelSourceNone, today's default spawn) skips its
//     check — there is nothing to validate.
//   - An empty/unconfigured allow-list — or an adapter/provider with no set —
//     accepts any model via IsAllowed, byte-identical to today.
//
// The plan model is keyed by the plan stage's executor.agent adapter
// (specPlanExecutorAgent → adapterForImplementAgent, the same agent→adapter map
// the implement gate uses). The review model is validated against EACH distinct
// implement-review reviewer provider, because the review_model override is
// applied to every heterogeneous reviewer (resolveReviewerInvocationsWithReviewModel);
// a run with no agent reviewers has no provider to validate against and so
// fails open.
func (s *Server) checkStageModelsAllowed(w http.ResponseWriter, r *http.Request, stage *run.Stage, planOverride, reviewOverride string) bool {
	runRow, err := s.cfg.RunRepo.GetRun(r.Context(), stage.RunID)
	if err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn, "plan/review model gate: get run failed",
			slog.String("stage_id", stage.ID.String()),
			slog.String("error", err.Error()),
		)
		return true
	}

	if planRM := s.gateResolvePlanModel(runRow, planOverride); planRM.Value != "" {
		adapter := adapterForImplementAgent(specPlanExecutorAgent(runRow.WorkflowSpec, runRow.WorkflowID))
		if !s.cfg.PlanAllowedModels.IsAllowed(adapter, planRM.Value) {
			s.writeError(w, r, http.StatusUnprocessableEntity, "plan_model_not_allowed",
				fmt.Sprintf("resolved plan model %q (source %s) is not in the configured allow-list for adapter %q; choose an allowed model via the plan stage executor.model or the plan_model approval override, or widen the deployment allow-list",
					planRM.Value, planRM.Source, adapter),
				map[string]any{
					"stage_id":     stage.ID.String(),
					"model":        planRM.Value,
					"model_source": string(planRM.Source),
					"adapter":      adapter,
				})
			return false
		}
	}

	if reviewRM := s.gateResolveReviewModel(runRow, reviewOverride); reviewRM.Value != "" {
		for _, provider := range s.reviewProvidersForRun(r.Context(), runRow) {
			if s.cfg.ReviewAllowedModels.IsAllowed(provider, reviewRM.Value) {
				continue
			}
			s.writeError(w, r, http.StatusUnprocessableEntity, "review_model_not_allowed",
				fmt.Sprintf("resolved review model %q (source %s) is not in the configured allow-list for reviewer provider %q; choose an allowed model via the review stage executor.model or the review_model approval override, or widen the deployment allow-list",
					reviewRM.Value, reviewRM.Source, provider),
				map[string]any{
					"stage_id":     stage.ID.String(),
					"model":        reviewRM.Value,
					"model_source": string(reviewRM.Source),
					"provider":     provider,
				})
			return false
		}
	}

	return true
}

// reviewProvidersForRun returns the distinct agent reviewer providers the
// review_model override would be applied to — the implement stage's reviewers
// (#1416), where the heterogeneous agent reviewers live and which the
// post-plan-gate (implement) review runs. Order is deterministic (config order,
// first occurrence wins) so the gate rejects on a stable provider. An absent
// reviewers config — or a bare-count form with no declared providers — yields an
// empty slice, so the review allow-list check fails open (nothing to validate).
func (s *Server) reviewProvidersForRun(ctx context.Context, runRow *run.Run) []string {
	reviewersCfg := s.resolveStageReviewers(ctx, runRow, spec.StageTypeImplement)
	if reviewersCfg == nil {
		return nil
	}
	var providers []string
	seen := map[string]bool{}
	for _, a := range reviewersCfg.Agents {
		p := strings.TrimSpace(a.Provider)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		providers = append(providers, p)
	}
	return providers
}

// specPlanExecutorAgent reads executor.agent on the plan stage of the given
// workflow from raw workflow-spec bytes via a local YAML probe, returning ""
// when the spec is empty, malformed, or declares no executor.agent. It mirrors
// specImplementExecutorAgent exactly but targets the PLAN stage (prefer a stage
// whose id == "plan", else the first stage whose type == "plan"). The gate maps
// the returned id to the allow-list adapter key via adapterForImplementAgent,
// so an empty/absent agent keys the default-spawn adapter ("claudecode").
func specPlanExecutorAgent(specBytes []byte, workflowID string) string {
	if len(specBytes) == 0 {
		return ""
	}
	var probe struct {
		Workflows map[string]struct {
			Stages []struct {
				ID       string `yaml:"id"`
				Type     string `yaml:"type"`
				Executor struct {
					Agent string `yaml:"agent"`
				} `yaml:"executor"`
			} `yaml:"stages"`
		} `yaml:"workflows"`
	}
	if err := yaml.Unmarshal(specBytes, &probe); err != nil {
		return ""
	}
	wf, ok := probe.Workflows[workflowID]
	if !ok {
		return ""
	}
	for _, st := range wf.Stages {
		if st.ID == "plan" {
			return strings.TrimSpace(st.Executor.Agent)
		}
	}
	for _, st := range wf.Stages {
		if st.Type == "plan" {
			return strings.TrimSpace(st.Executor.Agent)
		}
	}
	return ""
}

// writeStageModelResolutions emits the per-stage model_resolved audit entries on
// a valid plan-stage approve (#1416), extending the implement-only emission of
// #1013. It writes one entry per stamped stage, each keyed to its TARGET stage's
// StageID (so the observability slice reads a stage's model by StageID) and
// tagged with the stage_type discriminator (so the implement runner-spawn reader
// filters to the implement entry regardless of write order):
//
//   - implement: the already-resolved, allow-list-validated value the model gate
//     produced (implementRM), keyed to the implement stage. ALWAYS emitted.
//   - plan: gateResolvePlanModel(planOverride), keyed to the approved plan stage —
//     only when the plan ladder resolves to a non-empty model.
//   - review: gateResolveReviewModel(reviewOverride), keyed to the review stage —
//     only when the workflow has a review stage, the review ladder resolves to a
//     non-empty model, AND at least one agent reviewer provider exists
//     (reviewProvidersForRun > 0) — the same condition checkStageModelsAllowed
//     validates the review model against, so an entry is recorded only when the
//     resolved review model would actually have been allow-list-validated (#1427).
//
// The plan/review entries are suppressed when their resolution is empty: their
// readers (resolvePlanModelForRun, gateResolvedReviewModel) fall back to the
// spec-only / empty resolution when no entry exists, so an empty entry would be
// byte-identical to none — and emitting one would shadow the #1013 single-entry
// surface for a run with no plan/review pin or override. The implement entry is
// NOT suppressed: the runner-spawn reader needs the explicit empty "default
// spawn" decision.
//
// planStage is the approved plan stage. Fail-OPEN throughout, matching the
// sibling gates: a GetRun/ListStagesForRun failure degrades to the legacy
// keying (the implement entry on the plan stage) or skips the per-stage entries
// rather than unwinding the approval. The implement entry is ALWAYS emitted
// (even on a stage-lookup miss) so the runner-spawn route is never starved.
func (s *Server) writeStageModelResolutions(ctx context.Context, planStage *run.Stage, app *approval.Approval, implementRM ResolvedModel, planOverride, reviewOverride string) {
	// Default the implement entry's key to the plan stage (the legacy #1013
	// keying) so a stage-lookup failure still routes the runner spawn; upgrade
	// to the implement stage's id when the lookup succeeds.
	implStageID := planStage.ID
	var stages []*run.Stage
	if runStages, err := s.cfg.RunRepo.ListStagesForRun(ctx, planStage.RunID); err == nil {
		stages = runStages
		if id, ok := findStageIDByType(stages, run.StageTypeImplement); ok {
			implStageID = id
		}
	} else {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "model resolution: list stages failed; falling back to legacy keying",
			slog.String("run_id", planStage.RunID.String()),
			slog.String("error", err.Error()),
		)
	}

	s.writeModelResolvedAudit(ctx, planStage.RunID, implStageID, app, implementRM, string(run.StageTypeImplement))

	runRow, err := s.cfg.RunRepo.GetRun(ctx, planStage.RunID)
	if err != nil {
		// Implement entry already landed; without the run row the plan/review
		// ladders cannot be resolved, so degrade to implement-only (the #1013
		// surface) rather than unwinding the approval.
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "model resolution: get run failed; skipping plan/review entries",
			slog.String("run_id", planStage.RunID.String()),
			slog.String("error", err.Error()),
		)
		return
	}

	// The plan and review entries are emitted ONLY when their ladder resolves to
	// a NON-EMPTY model. Unlike the implement entry — which the runner-spawn
	// reader (gateResolvedModel) must see even as an explicit empty "default
	// spawn" decision — the plan and review readers (resolvePlanModelForRun,
	// gateResolvedReviewModel) already fall back to the spec-only / empty
	// resolution when no entry exists, so an empty entry is byte-identical to no
	// entry. Suppressing it keeps a run with no plan/review pin or override
	// carrying exactly the single implement entry (#1013's surface), rather than
	// shadow plan/review rows the readers would resolve identically.
	if planRM := s.gateResolvePlanModel(runRow, planOverride); planRM.Value != "" {
		s.writeModelResolvedAudit(ctx, planStage.RunID, planStage.ID, app, planRM, string(run.StageTypePlan))
	}

	// Gate the review entry on the SAME condition checkStageModelsAllowed
	// validates against — at least one agent reviewer provider
	// (reviewProvidersForRun, empty for a review stage with no declared agent
	// reviewers, #1427). Without this, a workflow with a review stage + a
	// non-empty review ladder but NO agent reviewers would record a
	// review_model that the allow-list gate never validated (the validate side
	// only loops over reviewProvidersForRun). Aligning emit with validate keeps
	// the recorded review resolution to runs where the override would actually
	// have been allow-list-checked. Fail-open and best-effort like the rest of
	// this function: no approval unwind.
	if reviewStageID, ok := findStageIDByType(stages, run.StageTypeReview); ok {
		if reviewRM := s.gateResolveReviewModel(runRow, reviewOverride); reviewRM.Value != "" {
			if len(s.reviewProvidersForRun(ctx, runRow)) > 0 {
				s.writeModelResolvedAudit(ctx, planStage.RunID, reviewStageID, app, reviewRM, string(run.StageTypeReview))
			}
		}
	}
}

// findStageIDByType returns the id of the first stage of the given type in the
// run's materialized stage list, or ok=false when none exists (e.g. a workflow
// with no review stage). All stages are materialized at run creation
// (CreateStagesFromSpec), so the implement and review rows exist at plan-approve
// time.
func findStageIDByType(stages []*run.Stage, t run.StageType) (uuid.UUID, bool) {
	for _, st := range stages {
		if st.Type == t {
			return st.ID, true
		}
	}
	return uuid.Nil, false
}

// writeModelResolvedAudit emits one source-tagged model_resolved audit entry
// (CategoryModelResolved, #1013/#1416) for a target stage. The payload is the
// ResolvedModel's {model, model_source} json shape plus a stage_type
// discriminator (modelResolvedPayload): the per-stage readers
// (gateResolvedModelForStage) filter by stage_type, and the observability slice
// reads a stage's model by the entry's StageID. Actor attribution mirrors
// writeApprovalAudit (the acting subject selects agent vs user). Best-effort: a
// logged append failure never unwinds the approval the gate already recorded.
func (s *Server) writeModelResolvedAudit(ctx context.Context, runID, targetStageID uuid.UUID, app *approval.Approval, rm ResolvedModel, stageType string) {
	actorKind := actorKindForSubject(app.ApproverSubject)
	approver := app.ApproverSubject
	payload, _ := json.Marshal(modelResolvedPayload{ResolvedModel: rm, StageType: stageType})
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:        runID,
		StageID:      &targetStageID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryModelResolved,
		ActorKind:    &actorKind,
		ActorSubject: &approver,
		Payload:      payload,
	}); err != nil {
		s.cfg.Logger.Error("audit append failed for model_resolved",
			"run_id", runID,
			"stage_id", targetStageID,
			"stage_type", stageType,
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

// checkDecomposedAddScopeFiles enforces the single-owner-file invariant on a
// plan-stage approve's add_scope_files (#2103). An operator-added path is
// persisted as a flat []string and folded into the effective scope at
// implement-prompt-build time by resolveApprovalAddScopeFiles, which returns
// the SAME parent-approval paths to EVERY decomposition child with no per-slice
// filtering — so an added path lands in every slice's effective scope,
// violating the single-owner-file rule checkCrossSliceSharedFiles already
// enforces for PLANNED files and producing a guaranteed add/add fan-in conflict
// (run bc47d2c4). There is no per-slice targeting channel for add_scope_files,
// so the gate fails CLOSED at approval time, before any row is recorded.
//
// Returns true (proceed) ONLY when add_scope_files is empty, OR the plan
// positively loads AND is confirmed non-decomposed (Decomposition == nil).
// Every other state with a NON-EMPTY add_scope_files FAILS CLOSED (writes a 422
// and returns false):
//   - a positively decomposed plan → the guaranteed-conflict case, naming every
//     slice that would inherit each added path;
//   - a load error or a nil/indeterminate plan whose decomposition status cannot
//     be positively determined → refused rather than let through, because an
//     add_scope_files approve must never be recorded without positive
//     confirmation the plan is flat.
//
// This deliberately DIVERGES from checkPlanBudget's fail-open posture on a
// per-request load failure: that gate is an override-able upper-bound heuristic,
// so a transient blip fails open; this gate is categorical (guaranteed conflict,
// no override), so a transient blip must not let the offending approval through.
// With the artifact subsystem wired, loadApprovedPlanForRun returns a non-nil
// *plan.Plan for every real flat plan (a nil/error return means no plan was
// found or a read failed, never a valid flat state), so failing closed on nil
// does not over-block a legitimate flat-plan approve.
//
// The ONE fail-open precondition is a config-level absence — ArtifactRepo or
// RunRepo unset — where no decomposition can exist at all and there is no
// fan-out surface to protect. This mirrors checkPlanBudget/checkPlanScopeCap's
// ArtifactRepo==nil posture and is unreachable in production (both repos are
// always wired), so the universal fail-closed guarantee holds where it matters.
func (s *Server) checkDecomposedAddScopeFiles(w http.ResponseWriter, r *http.Request, stage *run.Stage, addScopeFiles []string) bool {
	// Empty add is always safe — nothing to fan out.
	if len(addScopeFiles) == 0 {
		return true
	}

	// Plan-artifact subsystem not configured: no decomposition can exist, so
	// there is nothing to fan into. Fail OPEN, matching the sibling plan gates.
	// Production always wires both repos, so this branch never fires there.
	if s.cfg.ArtifactRepo == nil || s.cfg.RunRepo == nil {
		return true
	}

	approvedPlan, err := s.loadApprovedPlanForRun(r.Context(), stage.RunID)
	if err != nil || approvedPlan == nil {
		// Indeterminate: the plan's decomposition status cannot be positively
		// determined, so a non-empty add fails closed (see the doc comment).
		if err != nil {
			s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn, "add-scope-files gate: load plan failed",
				slog.String("stage_id", stage.ID.String()),
				slog.String("error", err.Error()),
			)
		}
		s.writeError(w, r, http.StatusUnprocessableEntity, "plan_add_scope_files_fans_into_slices",
			"add_scope_files was supplied but the run's plan could not be confirmed non-decomposed; the approve is refused so an added path cannot silently fan into every decomposition slice (single-owner-file). Retry once the plan is loadable, or re-plan the decomposition so each added file is declared in exactly one slice's scope.files",
			map[string]any{
				"stage_id":        stage.ID.String(),
				"add_scope_files": addScopeFiles,
				"reason":          "plan_indeterminate",
			})
		return false
	}

	// Positively flat — the safe case, proceed.
	if approvedPlan.Decomposition == nil {
		return true
	}

	// Positively decomposed with a non-empty add: the guaranteed-conflict case.
	// Every slice inherits every added path today, so name them all, in declared
	// order, so the reject surfaces exactly which slices would collide.
	subPlans := approvedPlan.Decomposition.SubPlans
	slices := make([]map[string]any, 0, len(subPlans))
	for i, sp := range subPlans {
		slices = append(slices, map[string]any{
			"index": i,
			"title": sp.Title,
		})
	}
	details := map[string]any{
		"stage_id":        stage.ID.String(),
		"add_scope_files": addScopeFiles,
		"slice_count":     len(subPlans),
		"slices":          slices,
	}

	if s.cfg.AuditRepo != nil {
		auditPayload, _ := json.Marshal(details)
		systemKind := audit.ActorKind("system")
		if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
			RunID:     stage.RunID,
			StageID:   &stage.ID,
			Timestamp: time.Now().UTC(),
			Category:  "plan_add_scope_files_fans_into_slices",
			ActorKind: &systemKind,
			Payload:   auditPayload,
		}); err != nil {
			s.cfg.Logger.Error("audit append failed for add-scope-files fan-in",
				"run_id", stage.RunID, "stage_id", stage.ID, "error", err.Error())
		}
	}

	s.writeError(w, r, http.StatusUnprocessableEntity, "plan_add_scope_files_fans_into_slices",
		"add_scope_files on a decomposed plan fans into EVERY sub-plan slice, violating single-owner-file and guaranteeing an add/add fan-in conflict; there is no per-slice add channel and no override. Re-plan the decomposition so each added file is declared in exactly one slice's scope.files",
		details)
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

// validateRemoveScopeFiles trims and validates the remove_scope_files paths
// (#1726), mirroring recover.go's validateExemptScopeFiles: each must be
// non-empty after trim and repo-relative (no leading '/' or ".." traversal —
// the containment contract isRepoRelativePath enforces). Returns the trimmed
// paths or an error describing the first bad entry; empty input yields nil
// with no error.
func validateRemoveScopeFiles(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(in))
	for _, raw := range in {
		p := strings.TrimSpace(raw)
		if p == "" {
			return nil, errors.New("remove_scope_files entries must name a non-empty repo-relative path")
		}
		if !isRepoRelativePath(p) {
			return nil, fmt.Errorf("remove_scope_files path %q must be repo-relative (no leading '/' or '..')", p)
		}
		out = append(out, p)
	}
	return out, nil
}

// checkRemoveScopeFiles validates a plan-stage approve's remove_scope_files
// (#1726) PRE-Submit. It enforces three fail-closed modes, each with a
// dedicated test:
//
//	(shape)   a non-repo-relative / empty path → 400 validation_failed
//	          field=remove_scope_files (validateRemoveScopeFiles).
//	(present) a removal path absent from the CURRENT effective scope (plan
//	          scope.files ∪ prior add folds ∪ approved amendments ∪ THIS
//	          call's add_scope_files) → 400 (catches operator typos).
//	(empty)   a removal that would empty a NON-empty effective scope → 400
//	          (an empty scope re-enables the runner's `git add -A` fallback,
//	          disabling enforcement).
//
// Returns (trimmed, true) to proceed — the trimmed slice the caller MUST
// thread back into the request so every downstream consumer (checkPlanScopeCap,
// writeApprovalAudit, the prompt-builder subtraction) subtracts the actual
// normalized path rather than the raw whitespace-padded input. An empty removal
// set returns (nil, true) — byte-identical to today. On any refusal it returns
// (nil, false) after writing the response.
//
// Read-error posture matches the sibling plan gates: if the effective scope
// cannot be computed (fail-open ok=false), the semantic presence/empty checks
// are skipped with a WARN so a transient backend hiccup never bricks the gate;
// the shape check still applies, and the prompt-builder subtraction is a
// no-op on a non-present path regardless.
func (s *Server) checkRemoveScopeFiles(w http.ResponseWriter, r *http.Request, stage *run.Stage, addScopeFiles, removeScopeFiles []string) ([]string, bool) {
	trimmed, err := validateRemoveScopeFiles(removeScopeFiles)
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			err.Error(), map[string]any{"field": "remove_scope_files"})
		return nil, false
	}
	if len(trimmed) == 0 {
		return nil, true
	}

	before, _, ok := s.effectiveScopePathSet(r.Context(), stage.RunID, addScopeFiles, nil)
	if !ok {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"remove-scope gate: effective scope unresolved; skipping presence/empty checks",
			slog.String("stage_id", stage.ID.String()),
		)
		return trimmed, true
	}
	present := make(map[string]struct{}, len(before))
	for _, p := range before {
		present[p] = struct{}{}
	}
	for _, p := range trimmed {
		if _, in := present[p]; !in {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				fmt.Sprintf("remove_scope_files path %q is not in the current effective scope; nothing to remove", p),
				map[string]any{
					"field":           "remove_scope_files",
					"path":            p,
					"effective_scope": before,
				})
			return nil, false
		}
		delete(present, p)
	}
	// A removal that empties a non-empty effective scope is refused: an empty
	// scope silently re-enables the runner's `git add -A` fallback, disabling
	// scope enforcement (foldScopePaths / effectiveFixupScope short-circuit on
	// an empty scope).
	if len(before) > 0 && len(present) == 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"remove_scope_files would empty the effective scope; an empty scope disables scope enforcement — keep at least one path or re-plan",
			map[string]any{
				"field":              "remove_scope_files",
				"remove_scope_files": trimmed,
			})
		return nil, false
	}
	return trimmed, true
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
func (s *Server) checkPlanScopeCap(w http.ResponseWriter, r *http.Request, stage *run.Stage, comment string, addScopeFiles, removeScopeFiles []string) bool {
	// Subtract the gate-time removals (#1726) so a cap overflow can be
	// reconciled entirely at the gate (remove or remove+add-replace) without a
	// re-plan. Use the shared effectiveScopePathSet directly (the same helper
	// effectiveScopeHeadroom wraps) so the removal is threaded without widening
	// effectiveScopeHeadroom's signature for its other callers.
	paths, maxFiles, ok := s.effectiveScopePathSet(r.Context(), stage.RunID, addScopeFiles, removeScopeFiles)
	effectiveCount := len(paths)
	if !ok || maxFiles <= 0 || effectiveCount <= maxFiles {
		return true
	}

	auditPayload, _ := json.Marshal(map[string]any{
		"stage_id":                 stage.ID.String(),
		"scoped_files":             effectiveCount,
		"max_files_changed":        maxFiles,
		"add_scope_files_count":    len(addScopeFiles),
		"remove_scope_files_count": len(removeScopeFiles),
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
		"effective scope.files (plan scope plus add_scope_files minus remove_scope_files) exceeds the implement stage's max_files_changed; re-scope the plan, remove paths via remove_scope_files, or include --override-scope-cap in the comment",
		map[string]any{
			"stage_id":                 stage.ID.String(),
			"scoped_files":             effectiveCount,
			"max_files_changed":        maxFiles,
			"add_scope_files_count":    len(addScopeFiles),
			"remove_scope_files_count": len(removeScopeFiles),
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
	// Delegated-action enforcement runs outside any campaign context: pass a
	// nil campaign override so resolution falls through to the workflow
	// contract (the campaign-level override is applied only by the campaign
	// auto-driver, E25.12).
	res, err := ev.Evaluate(r.Context(), runRow, &wf, nil)
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

// deployEvalRun resolves WHICH run the deploy pre-flight gate evaluates
// (E23.11 / #1417). For an appended-deploy run (UpstreamRunID nil) the gate
// evaluates the run itself — byte-for-byte today's behavior. For a standalone
// deploy-only release run (UpstreamRunID set) the gate evaluates the
// referenced upstream feature_change run's ci_green / review_merged instead:
// such a run has no implement/review stage of its own, so the upstream is the
// only thing the pre-flight can evaluate. Returns nil when a SET upstream
// cannot be resolved (load error / not-found) — the caller fails the gate
// closed (the safe direction for a pre-execution deploy gate). One resolver so
// the self-vs-upstream decision and its fail-closed semantics live in one
// place. NOTE: the cross-run reference is upstream_run_id, NOT parent_run_id
// (#216) — a deploy-gate safety pointer kept off the follow-up/lineage column.
//
// PROVENANCE VALIDATION (#1417 review): the FK guarantees upstream_run_id names
// a real run, but not an APPROPRIATE one. The deploy safety gate keys off this
// run's ci_green / review_merged, so the resolved upstream must be the kind of
// run the plan/docs describe — a feature_change run IN THE SAME REPO. Without
// these checks a release run in repo A could be pointed at an unrelated green/
// merged run in repo B (or a non-feature_change workflow whose ci_green means
// something else), satisfying a safety-critical gate against the wrong run's CI
// state. A mismatch is treated as UNRESOLVED (return nil) so the caller fails
// the gate closed — the safe direction for a pre-execution deploy gate.
func (s *Server) deployEvalRun(ctx context.Context, runRow *run.Run) *run.Run {
	if runRow.UpstreamRunID == nil {
		return runRow
	}
	up, err := s.cfg.RunRepo.GetRun(ctx, *runRow.UpstreamRunID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "deploy gate: resolve upstream run failed",
			slog.String("run_id", runRow.ID.String()),
			slog.String("upstream_run_id", runRow.UpstreamRunID.String()),
			slog.String("error", err.Error()),
		)
		return nil
	}
	if up.Repo != runRow.Repo {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "deploy gate: upstream run is in a different repo; treating as unresolved (fail-closed)",
			slog.String("run_id", runRow.ID.String()),
			slog.String("upstream_run_id", up.ID.String()),
			slog.String("repo", runRow.Repo),
			slog.String("upstream_repo", up.Repo),
		)
		return nil
	}
	if up.WorkflowID != "feature_change" {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "deploy gate: upstream run is not a feature_change run; treating as unresolved (fail-closed)",
			slog.String("run_id", runRow.ID.String()),
			slog.String("upstream_run_id", up.ID.String()),
			slog.String("upstream_workflow_id", up.WorkflowID),
		)
		return nil
	}
	return up
}

// deployCIGreen evaluates the required_upstream `ci_green` pre-flight signal
// (#1384): every required status check has reported green on the evaluated
// run's implement stage, reusing aggregateCIGreen over that run's
// RequiredChecksSnapshot. The evaluated run is resolved by deployEvalRun
// (E23.11 / #1417) — the current run for an appended deploy, or the referenced
// upstream feature_change run for a standalone deploy-only release run.
// Returns false (not satisfied) when the upstream is unresolvable, the
// snapshot or the stage-check repo is unwired, the implement stage is absent,
// the check read errors, or the aggregate is nil/false — the safe direction
// for a pre-execution deploy gate.
func (s *Server) deployCIGreen(ctx context.Context, runRow *run.Run) bool {
	evalRun := s.deployEvalRun(ctx, runRow)
	if evalRun == nil {
		return false
	}
	if evalRun.RequiredChecksSnapshot == nil || s.cfg.StageCheckRepo == nil {
		return false
	}
	implStage := s.findImplementStage(ctx, evalRun.ID)
	if implStage == nil {
		return false
	}
	checks, err := s.cfg.StageCheckRepo.LatestForStage(ctx, implStage.ID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "deploy gate: list stage checks failed",
			slog.String("run_id", evalRun.ID.String()),
			slog.String("error", err.Error()),
		)
		return false
	}
	g := aggregateCIGreen(evalRun.RequiredChecksSnapshot.Contexts, checks)
	return g != nil && *g
}

// deployReviewMerged evaluates the required_upstream `review_merged`
// pre-flight signal (#1384): the evaluated run carries a pull_request_url AND
// a succeeded review stage — a proxy for "the change merged", since merged
// state is not tracked on the run row today (the precise signal tightens when
// the deploy executor lands, E23.5/6/10). The evaluated run is resolved by
// deployEvalRun (E23.11 / #1417) — the current run for an appended deploy, or
// the referenced upstream feature_change run for a standalone deploy-only
// release run. Returns false when the upstream is unresolvable, the evaluated
// run has no pull_request_url, no succeeded review stage, or the stage-list
// read errors — the safe direction.
func (s *Server) deployReviewMerged(ctx context.Context, runRow *run.Run) bool {
	evalRun := s.deployEvalRun(ctx, runRow)
	if evalRun == nil {
		return false
	}
	if evalRun.PullRequestURL == nil || *evalRun.PullRequestURL == "" {
		return false
	}
	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, evalRun.ID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "deploy gate: list stages failed",
			slog.String("run_id", evalRun.ID.String()),
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
