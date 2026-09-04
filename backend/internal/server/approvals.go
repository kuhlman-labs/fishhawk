package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
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
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/policy"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// AuditCompleteCheckName is the reserved name for the
// `fishhawk_audit_complete` check Fishhawk publishes (#229). Stage
// gates declare it like any other check; the backend self-derives
// its state from artifact + audit-log presence rather than pulling
// it from the stage_checks table.
//
// It is NOT a "blocking" check by construction (E64.44 / #3161): it
// blocks a merge only where the repository's branch protection makes
// it a required status check, which Fishhawk does not configure and
// did not read until the merge_gate readiness rung landed.
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
	// AddScopeFilesToSlice is the per-slice counterpart of AddScopeFiles for a
	// DECOMPOSED plan (#2515). Flat add_scope_files is refused outright on a
	// decomposed plan (#2103) because a folded path fans into EVERY slice; this
	// map targets exactly ONE slice per path, keyed by the sub-plan TITLE or its
	// 0-based decimal index. The gate (checkSliceAddScopeFiles) canonicalises it
	// to index-keyed form and refuses — pre-Submit, before any approval row is
	// inserted — an unresolvable/ambiguous key, two keys naming one slice, a path
	// under two slices, a path whose ownership overlaps a DIFFERENT slice's
	// declared scope.files, an invalid path, and (fail-closed) a plan not
	// positively confirmed decomposed. Recorded on the approval_submitted audit
	// payload in canonical form and folded at implement-prompt-build time into
	// ONLY the requesting child's own slice. The gate runs on the PLAN stage
	// alone, so this field is recordable from the plan stage alone: an approve
	// of any other stage type ignores it entirely rather than recording an
	// ungated raw map (see the sliceAddScopeFiles local in
	// handleSubmitApproval). Declared here so the
	// DisallowUnknownFields decode accepts it; callers omit it (omitempty) and
	// stay byte-identical to today.
	AddScopeFilesToSlice map[string][]string `json:"add_scope_files_to_slice,omitempty"`
	// MoveScopeFilesToSlice is the slice-boundary MOVE channel for a DECOMPOSED
	// plan (#2596), the narrower cut the per-slice ADD channel deliberately
	// refuses (path_owned_by_another_slice). It is keyed EXACTLY like
	// AddScopeFilesToSlice — sub-plan TITLE (exact match wins) or 0-based decimal
	// index — but the key names the DESTINATION slice, and each value must ALREADY
	// be declared in the plan's decomposition scope. The SOURCE slice is DERIVED
	// from ownership (single-owner-file guarantees exactly one owner), so a move
	// leaves the file with one owner before and after — it cannot create the
	// add/add fan-in the add channel's refusal exists to prevent. The move changes
	// no total file count, so it does NOT ride in unionScopeAdds and consumes no
	// max_files_changed headroom. The gate (checkSliceMoveScopeFiles) canonicalises
	// it to index-keyed form and refuses — pre-Submit, before any approval row is
	// inserted — an unresolvable/ambiguous/duplicate key, a path under two
	// destination keys, a path also in add_scope_files_to_slice (the fields compose
	// in one call but must name disjoint paths), a path not declared in any slice's
	// scope (pointing at the add channel), a containment-only overlap
	// (move_requires_exact_owned_path), a no-op move to the owning slice, a move
	// that would empty the source slice, a plan not positively decomposed
	// (fail-closed), and a move ordering after a source/destination fan-out child
	// has left `pending` (409, fail-closed). Recorded on the approval_submitted
	// audit payload in canonical form (plus the resolved [{path,from_slice,to_slice}]
	// list) and folded at implement-prompt-build time onto BOTH sides of the move.
	// The gate runs on the PLAN stage alone, so this field is recordable from the
	// plan stage alone (see the sliceMoveScopeFiles local in handleSubmitApproval).
	// Declared here so the DisallowUnknownFields decode accepts it; callers omit it
	// (omitempty) and stay byte-identical to today.
	MoveScopeFilesToSlice map[string][]string `json:"move_scope_files_to_slice,omitempty"`
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
	// AmendAcceptanceCriteria is the OPTIONAL operator channel for RETIRING or
	// RESTATING an approved plan's acceptance criteria by id at the plan gate
	// (#2581). Plan-approval conditions reshape the design but never rewrite the
	// criteria, so the acceptance stage can validate the shipped behaviour
	// against a superseded contract and fail a correct implementation; this is
	// the explicit channel that fixes the contract at the same gate that
	// reshaped it. Each entry requires a reason (the reconstructable why), and a
	// restate additionally requires the replacement statement. Validated
	// pre-Submit by checkAmendAcceptanceCriteria — nine named refusals including
	// the anti-silencing all-retired gate — so a malformed or contract-emptying
	// amendment inserts no approval row. Recorded on the SAME approval_submitted
	// payload as the reason/add_scope_files/remove_scope_files that motivated it.
	// Declared here so the DisallowUnknownFields decode accepts it; callers omit
	// it (omitempty) and stay byte-identical to today.
	AmendAcceptanceCriteria []acceptanceCriteriaAmendment `json:"amend_acceptance_criteria,omitempty"`
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

// validateApprovalComment enforces the approve-only binding-conditions byte cap
// (#2583). It refuses ONLY when decision == approve and the comment exceeds
// prompt.MaxApprovalConditionBytes, returning an actionable message naming the
// actual byte count, the cap, and the overflow — mirroring fixup.go's
// operator_concern refusal wording ("must not be silently truncated as a
// binding instruction"). The cap is measured in BYTES (len), matching the
// prompt builder's byte-based CapText, not characters. A reject is never
// refused: its comment feeds the advisory PriorRejectionFeedback channel, not
// binding conditions. Returns (true, "", nil) when the comment is admissible.
func validateApprovalComment(decision approval.Decision, comment string) (ok bool, message string, details map[string]any) {
	if decision != approval.DecisionApprove {
		return true, "", nil
	}
	if len(comment) <= prompt.MaxApprovalConditionBytes {
		return true, "", nil
	}
	msg := fmt.Sprintf(
		"approve comment is %d bytes; the maximum is %d (it is injected verbatim as a binding approval condition and must not be silently truncated as a binding instruction). Shorten the text, split it across multiple conditions, or move machine-checkable clauses into binding_assertions",
		len(comment), prompt.MaxApprovalConditionBytes)
	return false, msg, map[string]any{
		"field":          "comment",
		"bytes":          len(comment),
		"max_bytes":      prompt.MaxApprovalConditionBytes,
		"overflow_bytes": len(comment) - prompt.MaxApprovalConditionBytes,
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
	// Human-executor review-gate actor refusal (E54.53 / #3041), placed AHEAD
	// of the write-scope check on purpose: a real run-bound fhm_ token does NOT
	// carry write:approvals, so behind requireWriteScope the message an
	// operator sees in production is insufficient_scope, which names nothing
	// about the executor constraint. See refuseAgentOnHumanReviewGate. It
	// no-ops (false) for every non-agent identity and for every stage that is
	// not an ADMITTED human-executor review gate, so the ladder below is
	// unchanged for every other caller.
	if s.refuseAgentOnHumanReviewGate(w, r) {
		return
	}
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

	// Over-cap approve-comment refusal (#2583). An approve comment is injected
	// verbatim as BINDING approval conditions (#558) and capped at
	// prompt.MaxApprovalConditionBytes when the implement prompt is built. Refuse
	// an over-cap approve HERE — a pure input validation with no side effects,
	// placed before commentPtr is built and the stage is fetched — so the
	// operator sees the refusal (byte count, cap, overflow, how to shorten)
	// rather than having the tail of a binding instruction silently dropped at
	// prompt-build time, mirroring fixup.go's operator_concern refusal. A REJECT
	// is deliberately NOT refused: its comment feeds the advisory
	// PriorRejectionFeedback channel, not binding conditions, so an over-cap
	// reject stays admissible.
	if ok, msg, details := validateApprovalComment(decision, req.Comment); !ok {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed", msg, details)
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
	// ...but NOT every review stage: a review stage whose PERSISTED executor
	// is human, whose sole review spec stage also declares executor.human, and
	// which declares NO pull_request input is not GitHub-managed at all — it is
	// backlog_grooming's `confirm` gate, which had no approvable surface
	// anywhere (E54.53 / #3041). Admit exactly that shape; every other review
	// stage, and every resolution failure, keeps today's 409 (fail closed).
	if stage.Type == run.StageTypeReview {
		if reason := s.resolveReviewGateAdmission(r.Context(), stage); reason != reviewGateAdmitOK {
			s.rejectReviewStageApproval(w, r, stage, reason)
			return
		}
		if !s.requireReviewGateAttestation(w, r, stage, decision, req.Comment) {
			return
		}
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
	//
	// A DELEGATED submission is recorded under the distinct
	// operatorrole.DelegatedApprovalActorSubject identity (#2381), so its
	// duplicate pre-check must look up THAT subject — otherwise a repeated
	// delegated submission would miss the pre-check (finding no row for the real
	// subject) and re-run the pre-Submit gates before Submit's Inserted=false
	// layer caught it. effectiveApprovalSubject is a read-only lookup, so running
	// it here BEFORE checkDelegation re-evaluates the rule is safe. A
	// non-delegated approve keeps looking up the real subject, byte-identical to
	// today.
	dupSubject := effectiveApprovalSubject(subject, req.Delegated)
	if prior := s.findPriorApproval(r.Context(), stageID, dupSubject); prior != nil {
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

	// Author separation-of-duties (E39.4 / #1709, corrected by #2358): the
	// change author may not approve their own change when the stage's gate
	// declares `not:` INCLUDING "author" AND the run carries a human-
	// authorship signal (an authorship-category audit row — today only
	// operator_commit_vouched). Operator GATE PARTICIPATION —
	// run_auto_driven, clarification_answered, approval_submitted,
	// scope-amendment decisions, concern waivers — resolves no author, so
	// none of them wedges the approver out of their own gate. A gate whose
	// `not:` omits "author" skips author resolution entirely (one fewer
	// ListForRun on this path). PRE-Submit like the sibling gates so a
	// refused approval inserts no row and the quorum count is not
	// incremented. A legacy Approvers / no-approvals gate skips this
	// entirely (byte-identical to today). Fail-open on an unresolved author
	// or an unreadable spec: author-SoD is skipped while agent-SoD and
	// quorum still apply.
	if decision == approval.DecisionApprove {
		if approvals, aerr := s.fetchApprovalsForStage(r.Context(), stage); aerr == nil && approvals != nil && approvalsExcludeAuthor(approvals) {
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
	// sliceAddScopeFiles carries the CANONICAL per-slice add map
	// checkSliceAddScopeFiles produced, or nil. It is declared HERE, outside
	// the plan block, and assigned ONLY inside it, so the per-slice channel is
	// recordable from the PLAN STAGE ALONE — the one stage whose approve runs
	// the gate (#2515 fixup).
	//
	// This is deliberately NOT the write-back-onto-req threading
	// remove_scope_files uses: req outlives this block and feeds
	// approveActionParams unconditionally, so threading req.AddScopeFilesToSlice
	// would let a direct HTTP approve of a NON-plan stage (implement, review,
	// deploy) on the same run record a RAW, un-canonicalised, un-validated map
	// on its approval_submitted row — including the non-repo-relative paths
	// ("/etc/passwd", "../x") isRepoRelativePath refuses at the plan gate.
	// loadApprovalSliceAddScopeFiles scans by run + category with NO stage-type
	// filter, so numeric-keyed entries from such a row would then fold into a
	// decomposed child's implement scope, bypassing every enumerated refusal.
	// A local assigned only under the gate makes that unreachable by
	// construction rather than by a second check.
	var sliceAddScopeFiles map[string][]string
	// sliceMoveScopeFiles and sliceMovesResolved carry the CANONICAL per-slice
	// move map and its resolved [{path,from_slice,to_slice}] list
	// checkSliceMoveScopeFiles produced, or nil (#2596). Declared HERE, outside the
	// plan block, and assigned ONLY inside it — the identical #2598 plan-block-local
	// discipline sliceAddScopeFiles rests on: req outlives this block and feeds
	// approveActionParams unconditionally, so threading req.MoveScopeFilesToSlice
	// would let a direct HTTP approve of a NON-plan stage record a raw, ungated,
	// un-canonicalised move map, which the prompt-side loader (no stage-type filter
	// of its own beyond approvalEntryStageIsPlan) would then fold. A local assigned
	// only under the gate makes that unreachable by construction.
	var sliceMoveScopeFiles map[string][]string
	var sliceMovesResolved []movedPath
	// addScopeFiles and removeScopeFiles are the FLAT siblings of the per-slice
	// channel above, re-homed onto plan-block locals for the identical reason
	// (#2598). Both were previously threaded to approveActionParams straight off
	// req — add_scope_files verbatim, and remove_scope_files via a write-back
	// onto req.RemoveScopeFiles — and req outlives this block and feeds
	// approveActionParams UNCONDITIONALLY. So a direct HTTP approve of a NON-plan
	// stage (implement, review) on the same run recorded raw, un-trimmed,
	// un-validated lists on its approval_submitted row, because every gate that
	// validates them (checkRemoveScopeFiles, checkDecomposedAddScopeFiles) runs
	// only inside this block. loadApprovalAddScopeFiles /
	// loadApprovalRemoveScopeFiles then fold those lists into implement scope and
	// into the cap arithmetic. The removal channel is the sharper of the two:
	// validateRemoveScopeFiles' would-empty-a-non-empty-scope refusal is
	// plan-block-only, so an approve naming every scope path off the plan stage
	// would empty the effective scope and re-enable the runner's `git add -A`
	// fallback. Declared HERE and assigned ONLY inside the block, so non-plan
	// recording is unreachable by construction rather than by a second check.
	var addScopeFiles []string
	var removeScopeFiles []string
	// Acceptance-criteria amendment channel (#2581). Validated PRE-Submit like
	// every sibling gate — a refusal inserts no approval row, so a corrected
	// retry flows normally. Deliberately called OUTSIDE the approve+plan block:
	// the gate must REFUSE an amendment supplied on a reject or a non-plan stage
	// (R5) rather than silently dropping it, and a silent drop is exactly the
	// divergence between what the operator believes they retired and what the
	// chain records that this channel exists to prevent. Because the gate refuses
	// every non-plan/non-approve call, the returned slice can only be non-empty
	// for a plan-stage approve — so recording it is safe by construction, the
	// same property the #2598 plan-block locals give the scope channels. The
	// slash-command approval channel (issue_approval.go) passes no amendments and
	// is deliberately unchanged.
	amendAcceptanceCriteria, ok := s.checkAmendAcceptanceCriteria(w, r, stage, decision, req.AmendAcceptanceCriteria)
	if !ok {
		return
	}
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
		// Carry the trimmed removal paths to every downstream consumer
		// (checkPlanScopeCap, writeApprovalAudit, the prompt-builder subtraction)
		// so each subtracts the normalized scope path rather than the raw
		// whitespace-padded input (#1726). Without this a value like
		// " backend/b.go " passes the trimmed presence/empty checks yet fails to
		// subtract the actual scope entry backend/b.go downstream. Assigned to the
		// plan-block local (see its declaration above), never written back onto
		// req, so no non-plan-stage approve can carry this channel (#2598).
		removeScopeFiles = trimmedRemove
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
		// Carry the gate-cleared flat adds to the downstream consumers via the
		// plan-block local, never written back onto req, for the same reason as
		// the removal above and the per-slice map below (#2598).
		addScopeFiles = req.AddScopeFiles
		// Per-slice add channel (#2515): the decomposed-plan counterpart of the
		// flat add refused just above. Every ownership violation is enumerated
		// and refused PRE-Submit — before any approval row is inserted — for the
		// same ADR-036 reason as its siblings, and placed here so this
		// categorical (no-override) gate precedes the override-able cap error.
		sliceAdds, ok := s.checkSliceAddScopeFiles(w, r, stage, req.AddScopeFilesToSlice)
		if !ok {
			return
		}
		// Carry the CANONICAL (index-keyed, trimmed, sorted, deduped) map to the
		// downstream consumers — writeApprovalAudit and the prompt-side fold that
		// reads it back — so both see one canonical form regardless of whether
		// the operator keyed by title or by index. Assigned to the plan-block
		// local (see its declaration above), never written back onto req, so no
		// non-plan-stage approve can carry this channel.
		sliceAddScopeFiles = sliceAdds
		// Per-slice MOVE channel (#2596): the slice-boundary move the per-slice
		// add refuses (path_owned_by_another_slice). Called immediately AFTER
		// checkSliceAddScopeFiles so the add's canonical map is available for the
		// cross-channel disjointness check, and BEFORE checkPlanScopeCap so this
		// categorical (no-override) gate precedes the override-able cap error.
		// Every ownership/ordering violation is enumerated and refused PRE-Submit
		// for the same ADR-036 reason as its siblings. Deliberately NOT threaded
		// into unionScopeAdds below: a move changes neither the effective scope
		// union nor its count, so it consumes no cap or required-tests headroom.
		moves, movesResolved, ok := s.checkSliceMoveScopeFiles(w, r, stage, req.MoveScopeFilesToSlice, sliceAdds)
		if !ok {
			return
		}
		// Carry the CANONICAL (index-keyed, trimmed, sorted, deduped) move map
		// and its resolved [{path,from_slice,to_slice}] list to the downstream
		// consumers — writeApprovalAudit and the prompt-side two-sided fold that
		// reads them back. Assigned to the plan-block locals (see their
		// declaration above), never written back onto req.
		sliceMoveScopeFiles = moves
		sliceMovesResolved = movesResolved
		// Scope-cap gate (#983): refuse an approve whose effective scope
		// (plan scope.files ∪ add_scope_files ∖ remove_scope_files) exceeds
		// the implement stage's max_files_changed, unless the comment carries
		// --override-scope-cap. PRE-Submit for the same ADR-036 reason as
		// checkPlanReviewSettled: a refused approval must insert no row so
		// a retry after re-scope or with the override flows normally
		// (post-Submit, the idempotent-first-wins retry would skip gates).
		//
		// A per-slice add consumes cap headroom exactly like a flat add (#2515):
		// the flattened union rides in alongside add_scope_files so the number
		// the gate reports stays equal to the scope the prompt builder assembles.
		if !s.checkPlanScopeCap(w, r, stage, req.Comment,
			unionScopeAdds(addScopeFiles, sliceAdds), removeScopeFiles) {
			return
		}
		// Required-tests gate (#2660): refuse an approve whose effective
		// scope declares no test-shaped path while the implement stage
		// requires `tests_added_or_updated` and the scope carries testable
		// source — the deterministic category-B failure this shifts left.
		// Placed AFTER the cap gate (whose message is the more specific one
		// when a plan trips both) and BEFORE the budget gate. PRE-Submit for
		// the same ADR-036 reason as every sibling: a refused approve
		// inserts no row, so a corrected retry flows normally.
		if !s.checkPlanRequiredTests(w, r, stage, req.Comment,
			unionScopeAdds(addScopeFiles, sliceAdds), removeScopeFiles) {
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
	// GitHub branch protection blocks the merge until the repo's
	// required checks report green. fishhawk_audit_complete is
	// PUBLISHED as a Check Run (#231), but whether it is one of
	// those required checks is the repository's own configuration —
	// the merge_gate readiness rung reports it (E64.44 / #3161).
	// The live check state is still rendered on the review page via
	// GET /v0/stages/{id}/checks — it's informational, not a gate.

	// Gate-action core (E25.6 / ADR-047): the approval Submit + audit +
	// model-resolution emission + state advance + orchestrator handoff +
	// drive stamp + notifications are factored into approveStageAs, an
	// identity-parameterised service method the in-process campaign
	// auto-driver also calls. The HTTP handler owns every pre-Submit gate
	// above; the result/error it returns is rendered to HTTP here exactly as
	// the prior inline core did (duplicate 200, InvalidTransition 409, and
	// the two distinct submit/advance 500 messages).
	result, err := s.approveStageAs(r.Context(), ident, approveActionParams{
		Stage:                   stage,
		Decision:                decision,
		Comment:                 req.Comment,
		CommentPtr:              commentPtr,
		ApproverGithubLogin:     req.ApproverGithubLogin,
		AddScopeFiles:           addScopeFiles,
		RemoveScopeFiles:        removeScopeFiles,
		SliceAddScopeFiles:      sliceAddScopeFiles,
		SliceMoveScopeFiles:     sliceMoveScopeFiles,
		SliceMovesResolved:      sliceMovesResolved,
		BindingAssertions:       req.BindingAssertions,
		ClaimsConcernIDs:        req.ClaimsConcernIDs,
		AmendAcceptanceCriteria: amendAcceptanceCriteria,
		DelegatedRule:           delegatedRule,
		ResolvedModel:           resolvedModel,
		PlanModel:               req.PlanModel,
		ReviewModel:             req.ReviewModel,
		PredicateResolution:     predicateRes,
	})
	if err != nil {
		var aerr *approveActionError
		if errors.As(err, &aerr) && aerr.failedAt == gateActionAdvance {
			// Raced second approval (E50.15 / #2656): the approve advance is a
			// compare-and-swap anchored on the state this request observed, so a
			// concurrent approval that already advanced the stage refuses here
			// instead of silently re-running the post-approval hooks. Rendered
			// as the endpoint's ALREADY-DOCUMENTED 409 invalid_state_transition
			// — the losing approval is recorded, but it did not advance the run.
			var sce run.StageStateChangedError
			if errors.As(aerr.err, &sce) {
				s.writeError(w, r, http.StatusConflict, "invalid_state_transition",
					aerr.err.Error(),
					map[string]any{"stage_id": stageID.String(),
						"from": sce.Expected, "state": sce.Actual})
				return
			}
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
	// SliceAddScopeFiles is the CANONICAL (index-keyed) per-slice add map the
	// #2515 gate produced, or nil when the approve carried none. Recorded on the
	// approval_submitted payload; the in-process campaign auto-driver leaves it
	// nil (it never targets a single slice). Only the HTTP handler's PLAN-stage
	// branch ever sets it — the gate that canonicalises and validates the map
	// runs there and nowhere else, so a non-plan-stage approve passes nil and
	// records nothing on this channel.
	SliceAddScopeFiles map[string][]string
	// SliceMoveScopeFiles is the CANONICAL (index-keyed) per-slice move map the
	// #2596 gate produced, or nil when the approve carried none. SliceMovesResolved
	// is its resolved [{path,from_slice,to_slice}] list. Both are recorded on the
	// approval_submitted payload; the in-process campaign auto-driver leaves them
	// nil (it never targets a slice). Only the HTTP handler's PLAN-stage branch
	// ever sets them — the gate that canonicalises and validates the map runs there
	// and nowhere else, so a non-plan-stage approve passes nil and records nothing.
	SliceMoveScopeFiles map[string][]string
	SliceMovesResolved  []movedPath
	BindingAssertions   []bindingAssertion
	ClaimsConcernIDs    []string
	// AmendAcceptanceCriteria is the canonical amendment slice
	// checkAmendAcceptanceCriteria produced, or nil when the approve carried
	// none (#2581). Recorded on the approval_submitted payload; the in-process
	// campaign auto-driver leaves it nil. Only a PLAN-stage approve can carry a
	// non-empty value — the gate refuses the channel everywhere else.
	AmendAcceptanceCriteria []acceptanceCriteriaAmendment
	DelegatedRule           string
	ResolvedModel           *ResolvedModel
	PlanModel               string
	ReviewModel             string
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
	realSubject := id.Subject
	if realSubject == "" {
		realSubject = "anonymous"
	}
	// FIX 3 (#2381): a DELEGATED approval is recorded under the distinct
	// operatorrole.DelegatedApprovalActorSubject identity so #1709's
	// recorded-but-never-counted vote never occupies the operator's own approver
	// slot — leaving their later real approve free to insert a fresh counting row
	// instead of colliding as a #986 duplicate. effectiveApprovalSubject is the
	// single mapping (an already-agent / non-delegated subject is unchanged);
	// onBehalfOf preserves the real operator's subject on the audit row whenever
	// the remap fired.
	subject := effectiveApprovalSubject(realSubject, p.DelegatedRule != "")
	onBehalfOf := ""
	if subject != realSubject {
		onBehalfOf = realSubject
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
	// checkApproverAuthorization's best-effort posture — EXCEPT when an
	// escalation is firing, which the nil branch below refuses (#2374). The
	// error is CAPTURED rather than discarded because a nil block alone
	// conflates two different situations, and only one of them is safe to
	// advance on.
	var approvals *spec.Approvals
	var fetchErr error
	if p.Decision == approval.DecisionApprove {
		a, ferr := s.fetchApprovalsForStage(ctx, p.Stage)
		if ferr != nil {
			fetchErr = ferr
		} else {
			approvals = a
		}
	}

	// Channel is recorded on every approval row (ADR-055 additive
	// enrichment), independent of whether quorum applies.
	channel := approvalChannel(id, p.DelegatedRule != "")

	if approvals == nil {
		// The two situations a nil block conflates (#2374): "this gate
		// declares no approvals block" (nil, no error) and "we could not read
		// the spec" (nil after a fetch ERROR). Only the first is a gate whose
		// requirement is KNOWN to be the legacy first-vote one. On the second
		// the baseline is unknown, and falling through to first-vote-advances
		// would clear an ESCALATED gate on a single vote — skipping both the
		// escalated count and the baseline predicates. That path is reachable
		// WITHOUT the pre-Submit gate checkApprovalPredicates hardens: the
		// campaign auto-driver calls this method directly (autodrive.go), and
		// the legacy path advances on a first vote regardless of the agent
		// floor that would otherwise keep it uncounted.
		//
		// So on a fetch error, resolve the escalations and fail toward NOT
		// ADVANCING when one is firing — or when the resolver itself errored,
		// which leaves us unable to tell whether one is. Same rule and same
		// words as the post-Submit escErr branch below: the escalated
		// requirement is unknown, so an advance could clear a gate an
		// escalation had raised. A true no-approvals gate (nil block, NO fetch
		// error) keeps first-vote-advances byte for byte.
		if fetchErr != nil {
			escReq, escErr := s.resolveStageEscalations(ctx, p.Stage)
			if escErr != nil || !escReq.IsZero() {
				s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
					"approval: gate approvals block unreadable while an escalation is firing; not advancing the gate",
					slog.String("stage_id", p.Stage.ID.String()),
					slog.String("error", fetchErr.Error()),
					slog.Bool("escalation_unevaluable", escErr != nil),
				)
				// Recorded but not counted toward an unknown requirement: the
				// row stands (a corrected retry is a duplicate, not a second
				// vote), and the stage stays awaiting_approval until the
				// baseline is readable again. No predicate_snapshot — nothing
				// about the effective requirement is known to snapshot.
				s.writeApprovalAudit(ctx, p.Stage, res.Approval, p.Comment, p.ApproverGithubLogin, p.AddScopeFiles, p.RemoveScopeFiles, p.SliceAddScopeFiles, p.SliceMoveScopeFiles, p.SliceMovesResolved, p.BindingAssertions, p.ClaimsConcernIDs, p.AmendAcceptanceCriteria, p.DelegatedRule, id.AuthMethod, channel, onBehalfOf, nil)
				s.notifyStatusUpdate(ctx, p.Stage.RunID, "approval_submit")
				return &approveActionResult{Stage: p.Stage}, nil
			}
		}
		// Legacy / no-approvals path: first vote advances, unchanged from
		// today except the additive identity{provider,subject}/channel
		// enrichment (ADR-055 record leg) on the approval_submitted row. No
		// predicate_snapshot — the gate declares no approvals block
		// (operator binding condition 2).
		s.writeApprovalAudit(ctx, p.Stage, res.Approval, p.Comment, p.ApproverGithubLogin, p.AddScopeFiles, p.RemoveScopeFiles, p.SliceAddScopeFiles, p.SliceMoveScopeFiles, p.SliceMovesResolved, p.BindingAssertions, p.ClaimsConcernIDs, p.AmendAcceptanceCriteria, p.DelegatedRule, id.AuthMethod, channel, onBehalfOf, nil)
		return s.finishApprovalAdvance(ctx, p, res)
	}

	// Quorum path. Resolve the change author UNCONDITIONALLY (fail-open:
	// unresolved when no authorship-category actor exists yet) — it feeds
	// submitterClass, which is provenance for predicate_snapshot and must
	// keep labeling a resolved author "author" even on a gate that permits
	// them. Only the EXCLUSION is gated on the gate's `not:` (#2358). Then
	// classify this submitter: a delegated / agent-kind submission is
	// recorded but never counts toward the human quorum, and its channel is
	// forced to "delegated".
	changeAuthor, _ := s.resolveChangeAuthor(ctx, p.Stage.RunID)
	submitterAgent := actorKindForSubject(res.Approval.ApproverSubject) == audit.ActorAgent
	delegated := submitterAgent || p.DelegatedRule != ""
	if delegated {
		channel = "delegated"
	}

	// Threading `not:` through the COUNT as well as the pre-Submit 403 is
	// load-bearing, not belt-and-braces (#2358): gating only the 403 would
	// RECORD a permitted author's approval and then decline to COUNT it,
	// leaving quorum permanently one short — the same wedge presenting as a
	// stuck gate rather than a clean 403, and strictly harder to diagnose
	// than the bug being fixed. Both reads parse the same immutable cached
	// spec bytes off the same run row within one request, so the 403 gate
	// and the count can never disagree on `not:`.
	excludeAuthor := approvalsExcludeAuthor(approvals)
	// Enumerate (not just count) the distinct eligible approvers: the plain
	// count feeds the common path, and the SUBJECT list feeds the escalation
	// re-validation below (#2227).
	subjects := s.distinctEligibleApproverSubjects(ctx, p.Stage.RunID, p.Stage.ID, changeAuthor, excludeAuthor)
	eligibleCount := len(subjects)

	// The escalation raise is applied at the COUNT as well as at the
	// pre-Submit 403 (E53.4 / #2227), for the same #2358 reason `not:` had to
	// be: raising only the 403 would record an approval the gate then declines
	// to count, wedging it one short — a stuck gate is strictly harder to
	// diagnose than a clean refusal. Both reads resolve from the same
	// immutable cached spec bytes on the same run row within one request, so
	// the two can never disagree about what fired.
	//
	// A resolver ERROR here is not a 503: this is the post-Submit path, where
	// the approval row is already inserted, and the pre-Submit gate
	// (checkApprovalPredicates) has already refused an unevaluable escalation
	// with a retryable 503. Fail toward NOT ADVANCING — the escalated
	// requirement is unknown, so an advance could clear a gate an escalation
	// had raised.
	escReq, escErr := s.resolveStageEscalations(ctx, p.Stage)
	if escErr != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"approval: escalation resolution failed; not advancing the gate",
			slog.String("stage_id", p.Stage.ID.String()),
			slog.String("error", escErr.Error()),
		)
	}
	effective := effectiveApprovals(approvals, escReq)
	required := effective.count
	gateUnreachable := escErr != nil

	// Cross-request TOCTOU closure (#2227). The plan scope the escalation
	// matches against is MUTABLE (a scope amendment can move the plan into an
	// escalated path between two approvals), so an approval recorded while the
	// escalation was NOT firing was never membership-checked at Submit
	// (checkApprovalPredicates enforces the forge predicate at INSERT time) — yet
	// the plain distinct-approver count would still credit it toward a quorum the
	// escalation has since RAISED to require member_of / min_permission. That
	// would let a matching change clear on an approval that never satisfied its
	// current membership constraint. So when the fired escalation carries a forge
	// predicate, re-resolve it against the forge for every counted approver and
	// keep only those who satisfy it NOW. It runs ONLY when the ESCALATION raised
	// a forge predicate, so a run with no escalation, a count-only escalation, or
	// a purely BASELINE forge predicate (already enforced at every Submit, no
	// TOCTOU) pays no count-time forge calls — the E39.5 native member_of path is
	// byte-identical to today. A forge that cannot resolve a counted approver
	// makes the gate unreachable this pass (fail closed), the same not-advancing
	// posture the escErr branch takes, since the post-Submit path has no 503.
	if !gateUnreachable && (escReq.MinPermission != "" || len(escReq.MemberOf) > 0) {
		if satisfied, ok := s.countEscalatedForgeApprovers(ctx, p.Stage, subjects, effective); ok {
			eligibleCount = satisfied
		} else {
			gateUnreachable = true
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"approval: escalated forge predicate unresolved for a counted approver; not advancing the gate",
				slog.String("stage_id", p.Stage.ID.String()),
			)
		}
	}

	if gateUnreachable {
		// Unknown or unverifiable requirement: make the gate unreachable this
		// pass rather than advance it at a possibly-unescalated or unverified
		// count.
		required = eligibleCount + 1
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
		MinPermission:  effective.minPermission,
		MemberOf:       approvals.MemberOf,
		QuorumReached:  reached,
	}
	// Record that an escalation is what made this gate stricter, and the full
	// membership conjunction it enforced, so the raise is explainable from the
	// audit row alone. Additive + omitempty — a run with no fired escalation
	// serialises byte-identically to today.
	if !escReq.IsZero() {
		snapshot.Escalated = true
		snapshot.EscalatedMemberOf = effective.memberOf
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
	s.writeApprovalAudit(ctx, p.Stage, res.Approval, p.Comment, p.ApproverGithubLogin, p.AddScopeFiles, p.RemoveScopeFiles, p.SliceAddScopeFiles, p.SliceMoveScopeFiles, p.SliceMovesResolved, p.BindingAssertions, p.ClaimsConcernIDs, p.AmendAcceptanceCriteria, p.DelegatedRule, id.AuthMethod, channel, onBehalfOf, snapshot)

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
		// On-approval live-validation walk filing (#2045, E48.35): when the
		// approved plan carries any requires_live_validation acceptance
		// criterion, auto-file (or, on a re-approval, idempotently no-op on) a
		// chore-type operator-validation walk and record a durable marker so the
		// pending live check is surfaced rather than shipped silently
		// unvalidated. Best-effort like fileSplitProposalChildren above — a
		// failure logs and never unwinds the approval; a plan with no marked
		// criterion no-ops.
		s.fileOrLinkLiveValidationWalk(ctx, advanced)
		// On-approval predicted-runtime stamp (E48.62 / #2489): cache the
		// approved plan's predicted_runtime_minutes onto the run row so the
		// MCP surface can derive its advertised stage-wait poll cadence from
		// a plain column instead of an artifact fetch per status read.
		// Best-effort like the three above — a failure logs and never unwinds
		// the approval the gate already recorded; a plan with no prediction
		// (or a RunRepo that does not implement the setter) no-ops.
		s.recordPlanPredictedRuntime(ctx, advanced)
	}

	// Plan-comment re-render (#377): a plan-stage approve or reject re-fires
	// the plan-on-issue hook. Best-effort: notifyPlanReady logs but never
	// unwinds the approval.
	if advanced.Type == run.StageTypePlan {
		s.notifyPlanReady(ctx, advanced.RunID, advanced)
		// On-approval grooming apply (E54.19 / #2822): when the approved plan
		// stage carries a grooming_report, apply its HYGIENE-class mutations
		// through workmgmt.ApplyGrooming — the production caller that seam was
		// reserved for. Best-effort like the block above: the gate already
		// passed and its approval row is in place, so a failure logs and never
		// unwinds the approval.
		//
		// THE PLACEMENT IS DELIBERATE AND IS ITSELF PART OF THE CONTROL. This
		// is the TYPE-only plan block, not the DecisionApprove block above, and
		// the decision is PASSED rather than implied — so "a rejected report
		// applies nothing" lives in applyApprovedGrooming as a guard whose
		// deletion is observable on the reject path. Nesting the call inside the
		// approve-only block would make that governance property structurally
		// untestable, and therefore unproven.
		s.applyApprovedGrooming(ctx, advanced, p.Decision)
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
// best-effort posture), NOT on the forge RESOLUTION, which fails closed. The
// one exception is a spec-read error while an escalation IS firing (#2374):
// the effective requirement is then unknowable, so the gate returns the
// retryable 503 rather than enforcing a baseline-less approximation of it.
func (s *Server) checkApprovalPredicates(w http.ResponseWriter, r *http.Request, stage *run.Stage, subject string, delegated bool) (*predicateResolution, bool) {
	approvals, ferr := s.fetchApprovalsForStage(r.Context(), stage)

	// The escalation raise is resolved BEFORE the "is this gate predicate-
	// guarded" test (E53.4 / #2227): an escalation can add a `member_of` or a
	// `min_permission` to a gate that declares NEITHER, so testing the raw
	// block first would skip the forge gate on exactly the gate an escalation
	// just tightened. It is also resolved on a gate with no approvals block at
	// all — a `paths`-matched escalation raises membership there too.
	//
	// It is resolved BEFORE short-circuiting on a gate-fetch error too (#2227
	// fixup): a TRANSIENT fetchApprovalsForStage failure must not skip an
	// escalated member_of / min_permission predicate on a run whose spec
	// declares one — otherwise an out-of-group approval recorded during the
	// error window counts toward the escalated quorum at count time (the count
	// path counts distinct approvers, not group membership, so membership is
	// never re-checked there). resolveStageEscalations reads the run row
	// itself and fails closed on a transient run-row error (degrading only on
	// a missing/legacy row), so the two failure surfaces stay consistent.
	//
	// FAIL CLOSED on a resolver error: a Match error (a malformed glob reaching
	// the gate) or an unreadable plan while a paths-bearing escalation is
	// declared returns a retryable 503, never an unescalated advance.
	escReq, escErr := s.resolveStageEscalations(r.Context(), stage)
	if escErr != nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "escalation_unevaluable",
			"the workflow's escalations declaration could not be evaluated for this change; the approval gate failed closed rather than proceeding unescalated",
			map[string]any{
				"stage_id":  stage.ID.String(),
				"retryable": true,
				"error":     escErr.Error(),
				"next_actions": []string{
					"Retry the approval; if it persists, check the workflow's escalations `match` globs and that the run's plan artifact is readable",
				},
			})
		return nil, false
	}

	// A gate-fetch error with NOTHING escalated keeps the pre-existing
	// fail-open posture for the baseline predicates — they were always
	// best-effort on this read, matching checkApproverAuthorization.
	//
	// But when an escalation IS firing (escReq non-zero), the effective
	// requirement is UNKNOWABLE, not merely partially known (#2374). The
	// earlier posture here — enforce the escalated requirement against a nil
	// baseline, on the reasoning that an escalation only ever RAISES — is the
	// defect: an escalation raises RELATIVE to the baseline, so composing it
	// against a nil baseline DISCARDS the baseline's own member_of /
	// min_permission. The concrete bypass is a gate whose baseline declares
	// `member_of` with a firing COUNT-ONLY escalation: effectiveApprovals(nil,
	// escReq) is then "the raised count, no membership at all", which falls
	// straight through the not-predicate-guarded early return below and admits
	// an out-of-group approver on the escalated count alone. The count-time
	// forge re-validation does not cover that shape either — a count-only
	// escalation carries no escalated forge predicate for
	// countEscalatedForgeApprovers to re-resolve. So refuse with the same
	// retryable 503 every other unreadable-input branch in this file uses: a
	// control must not enforce a requirement it cannot compute.
	if ferr != nil {
		if escReq.IsZero() {
			return nil, true
		}
		s.writeError(w, r, http.StatusServiceUnavailable, "escalation_unevaluable",
			"the gate's baseline approvals block could not be read while an escalation is firing, so the effective (baseline plus escalation) requirement is unknowable; the approval gate failed closed rather than enforcing a partially-known requirement",
			map[string]any{
				"stage_id":  stage.ID.String(),
				"retryable": true,
				"reason":    "baseline_unreadable",
				"error":     ferr.Error(),
				"next_actions": []string{
					"Retry the approval; if it persists, check that the run's cached workflow spec declares this stage type",
				},
			})
		return nil, false
	}
	effective := effectiveApprovals(approvals, escReq)

	// Only the two forge predicates gate here. A count-only requirement keeps
	// the pure-quorum path.
	if effective.minPermission == "" && len(effective.memberOf) == 0 {
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

	outcome, resolution, predicate := s.resolvePredicates(r.Context(), repo, subject, effective)
	switch outcome {
	case predicateSatisfied:
		return resolution, true
	case predicateRejected:
		// Durably record the rejection in a predicate_snapshot audit entry
		// even though no approval row is inserted.
		s.writePredicateRejectionAudit(r.Context(), stage, subject, effective, resolution)
		s.writeError(w, r, http.StatusForbidden, "approver_predicate_unmet",
			"the approver does not meet the gate's forge predicate (permission tier or membership)",
			map[string]any{
				"stage_id":            stage.ID.String(),
				"subject":             subject,
				"required_permission": effective.minPermission,
				"resolved_permission": resolution.ResolvedPermission,
				"member_of":           renderMemberOf(effective.memberOf),
				"member_resolved":     resolution.MemberResolved,
				"escalated":           !escReq.IsZero(),
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
				"ref":       renderMemberOf(effective.memberOf),
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
func (s *Server) writePredicateRejectionAudit(ctx context.Context, stage *run.Stage, subject string, approvals escalatedApprovals, resolution *predicateResolution) {
	if s.cfg.AuditRepo == nil {
		return
	}
	snapshot := map[string]any{
		"result": "rejected",
	}
	if approvals.minPermission != "" {
		snapshot["min_permission"] = approvals.minPermission
		snapshot["resolved_permission"] = resolution.ResolvedPermission
	}
	if len(approvals.memberOf) > 0 {
		snapshot["member_of"] = renderMemberOf(approvals.memberOf)
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

// runPredictionRecorder is the narrow OPTIONAL capability the on-approval
// predicted-runtime stamp (E48.62 / #2489) needs from the wired RunRepo. It is
// declared here, at the CONSUMER, and type-asserted at runtime — the exact
// pattern runCostRecorder (trace.go) already uses for AddRunCost.
//
// Deliberately NOT a method on run.Repository. The interface is implemented by
// two dozen test doubles across webhook, childcompletion, dispatchwatchdog,
// reactionpoller, orchestrator and internal/server, none of which stamp or read
// a prediction; widening it would force a no-op stub into every one of them for
// a value nothing gates on. The concrete *postgresRepo carries the setter and
// the assertion finds it; a Config wired with a repo that lacks it silently
// records nothing and the advertised cadence falls back to the elapsed-based
// branch.
type runPredictionRecorder interface {
	SetRunPredictedRuntimeMinutes(ctx context.Context, runID uuid.UUID, minutes int) (*run.Run, error)
}

// recordPlanPredictedRuntime caches the approved plan's
// predicted_runtime_minutes onto the run row after a plan-gate approval
// (E48.62 / #2489), so the MCP stage-wait poll cadence derives from a plain
// column instead of an artifact fetch per status read.
//
// Best-effort throughout — it runs in the same post-advance neighbourhood as
// recordDrivePlanApproved / fileSplitProposalChildren, so it must never unwind
// an approval the gate already recorded. It no-ops when: no RunRepo is wired,
// the wired RunRepo does not satisfy runPredictionRecorder, the approved plan
// cannot be loaded (or the artifact subsystem is unwired, which yields a nil
// plan), or the plan carries a non-positive prediction. A setter failure logs
// at WARN and returns.
func (s *Server) recordPlanPredictedRuntime(ctx context.Context, stage *run.Stage) {
	if s.cfg.RunRepo == nil {
		return
	}
	recorder, ok := s.cfg.RunRepo.(runPredictionRecorder)
	if !ok {
		return
	}
	approvedPlan, err := s.loadApprovedPlanForRun(ctx, stage.RunID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"predicted-runtime stamp: load approved plan failed",
			slog.String("run_id", stage.RunID.String()),
			slog.String("error", err.Error()),
		)
		return
	}
	if approvedPlan == nil || approvedPlan.PredictedRuntimeMinutes <= 0 {
		return
	}
	if _, err := recorder.SetRunPredictedRuntimeMinutes(ctx, stage.RunID, approvedPlan.PredictedRuntimeMinutes); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"predicted-runtime stamp failed",
			slog.String("run_id", stage.RunID.String()),
			slog.Int("predicted_runtime_minutes", approvedPlan.PredictedRuntimeMinutes),
			slog.String("error", err.Error()),
		)
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
//
// APPROVE IS A COMPARE-AND-SWAP (E50.15 / #2656). The approve leg anchors the
// transition on the state the CALLER ALREADY OBSERVED (stage.State), so it
// means "nothing changed since I looked" rather than "the stage must be
// parked". A concurrent approval that flipped the stage between this caller's
// load and this call refuses atomically with run.StageStateChangedError instead
// of falling into transitionStage's `from == to` short-circuit, which returns a
// SILENT SUCCESS and would walk the loser straight into finishApprovalAdvance's
// post-approval hooks (fileSplitProposalChildren / fileOrLinkLiveValidationWalk)
// a second time. Anchoring on the observed state — not on the literal
// awaiting_approval — keeps every transition the endpoint admits today
// admissible: an approve of a non-parked stage that succeeds now still
// succeeds, so no live-surface 200 becomes a 409.
//
// The REJECT leg is deliberately left on run.FailStage, which already CASes
// internally (backend/internal/run/failure.go:113, re-anchoring on the actual
// state) and additionally refuses a terminal stage via ValidStageTransition. It
// needs no change here.
func (s *Server) advanceStage(ctx context.Context, stage *run.Stage, decision approval.Decision) (*run.Stage, error) {
	switch decision {
	case approval.DecisionApprove:
		return s.casTransitionFromObserved(ctx, stage, run.StageStateSucceeded)
	case approval.DecisionReject:
		return run.FailStage(ctx, s.cfg.RunRepo, stage.ID,
			run.FailureD, "gate rejected by approver")
	}
	// Unreachable — decision was validated earlier.
	return nil, errors.New("approval: unknown decision (programmer error)")
}

// casTransitionFromObserved moves stage to `to`, anchored under the repository
// row lock on the state the caller observed when it loaded the stage. It is the
// same runtime capability-assert convention markStageRunningOnPromptFetch
// (prompt.go) and handleHostDispatch (host_dispatch.go) already use: the
// production postgresRepo implements run.StageCASTransitioner and gets the
// atomic compare-and-swap; an in-memory RunRepo that does NOT implement it
// degrades to the plain table-validated TransitionStage — today's behavior
// exactly, no panic.
func (s *Server) casTransitionFromObserved(ctx context.Context, stage *run.Stage, to run.StageState) (*run.Stage, error) {
	if cas, ok := s.cfg.RunRepo.(run.StageCASTransitioner); ok {
		return cas.TransitionStageFrom(ctx, stage.ID, stage.State, to, nil)
	}
	return s.cfg.RunRepo.TransitionStage(ctx, stage.ID, to, nil)
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
//
// The deploy pre-execution advance is the SHARPER of the two approve legs and
// gets the same observed-state compare-and-swap (E50.15 / #2656): the hook it
// guards is an EXTERNAL delegating-pipeline fire, so a silently-succeeding
// second advance means a duplicate release trigger.
func (s *Server) advanceForDecision(ctx context.Context, stage *run.Stage, decision approval.Decision) (*run.Stage, error) {
	if decision == approval.DecisionApprove && stage.Type == run.StageTypeDeploy {
		dispatched, err := s.casTransitionFromObserved(ctx, stage, run.StageStateDispatched)
		if err != nil {
			return nil, err
		}
		return s.triggerDeploy(ctx, dispatched)
	}
	return s.advanceStage(ctx, stage, decision)
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
//
// LEGACY v0/v1 PATH (E52.2 / #2214). The `approvers` role allow-list and the
// top-level `roles` map it resolves against were removed from workflow-v2, so
// for a cached v2 spec fetchGateForStage's gate.Approvers != nil branch is
// structurally UNREACHABLE — a v2 run always returns the no-approvers context,
// and eligibility is decided solely by fetchApprovalsForStage + the quorum /
// resolvePredicates path. The role-check plumbing is retained for v0/v1 runs,
// whose gates still carry both surfaces.
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
// For a cached workflow-v2 spec the gate.Approvers != nil branch below never
// fires (E52.2 / #2214 removed the gate `approvers` allow-list), so a v2 run
// always returns the no-approvers context and the legacy role check is never
// consulted — v2 eligibility flows entirely through fetchApprovalsForStage +
// resolvePredicates.
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
//
// Since #3041 this is the NOT-ADMITTED arm of resolveReviewGateAdmission
// rather than an unconditional refusal, and `reason` names WHICH leg failed
// under details.admission_reason so a refused caller can tell a
// PR-merge-managed gate from an unparseable cached spec. The code, message and
// pull_request_url detail are byte-identical to pre-#3041 for every reason, so
// the ADR-018 contract existing clients match on is unchanged.
func (s *Server) rejectReviewStageApproval(w http.ResponseWriter, r *http.Request, stage *run.Stage, reason reviewGateAdmitReason) {
	details := map[string]any{
		"stage_id":         stage.ID.String(),
		"stage_type":       string(stage.Type),
		"admission_reason": reason.String(),
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
func (s *Server) writeApprovalAudit(ctx context.Context, stage *run.Stage, app *approval.Approval, comment, approverGithubLogin string, addScopeFiles, removeScopeFiles []string, sliceAddScopeFiles map[string][]string, sliceMoveScopeFiles map[string][]string, sliceMovesResolved []movedPath, bindingAssertions []bindingAssertion, claimsConcernIDs []string, amendAcceptanceCriteria []acceptanceCriteriaAmendment, delegatedRule, authMethod, channel, onBehalfOf string, snapshot *predicateSnapshot) {
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
	// Per-slice scope add (#2515): record the CANONICAL index-keyed map the gate
	// produced, so the prompt builder reads it back via
	// loadApprovalSliceAddScopeFiles and folds only the entry keyed to the
	// requesting child's own slice_index. The index — not the title — is the
	// durable join key: it matches the runs.slice_index column the fan-out
	// children carry, and a title-keyed request records byte-identically to the
	// equivalent index-keyed one. The key is omitted on a no-add approve so
	// prompt-hash replay stays byte-identical to today.
	if app.Decision == approval.DecisionApprove && len(sliceAddScopeFiles) > 0 {
		auditPayload["add_scope_files_to_slice"] = sliceAddScopeFiles
	}
	// Per-slice scope MOVE (#2596): record the CANONICAL index-keyed
	// DESTINATION map the gate produced (the prompt builder reads it back via
	// loadApprovalSliceMoveScopeFiles and folds BOTH sides onto the relevant
	// children), plus move_scope_files_resolved — the ordered
	// [{path,from_slice,to_slice}] list. The resolved list is the ONLY place the
	// true MOVE provenance survives: the destination side inherits the SAME
	// add-scope-files fold consumers use (resolveApprovalAddScopeFiles), so
	// trace.go/gate-view label a moved-IN path identically to a genuinely ADDED
	// one — the resolved list disambiguates. Both keys are OMITTED on a no-move
	// approve so prompt-hash replay stays byte-identical to today.
	if app.Decision == approval.DecisionApprove && len(sliceMoveScopeFiles) > 0 {
		auditPayload["move_scope_files_to_slice"] = sliceMoveScopeFiles
		if len(sliceMovesResolved) > 0 {
			auditPayload["move_scope_files_resolved"] = sliceMovesResolved
		}
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
	// Acceptance-criteria amendment (#2581): record the operator's retirements /
	// restatements on the SAME row as the reason and the scope channels that
	// motivated them, so each retirement's id, reason and source are
	// reconstructable from the chain alone. Only on approve with a non-empty
	// slice — the key is omitted otherwise, so an approve that does not use the
	// channel marshals byte-identically to today.
	if app.Decision == approval.DecisionApprove && len(amendAcceptanceCriteria) > 0 {
		auditPayload["amend_acceptance_criteria"] = amendAcceptanceCriteria
	}
	if app.Decision == approval.DecisionApprove && len(claimsConcernIDs) > 0 {
		auditPayload["claims_concern_ids"] = claimsConcernIDs
	}
	if delegatedRule != "" {
		auditPayload["delegated"] = delegatedRule
	}
	// on_behalf_of names the REAL operator whose driver recorded a DELEGATED
	// approval (#2381) when the row's subject was remapped to the distinct
	// operator-agent delegated identity — so actor_subject/actor_kind become the
	// agent identity while the row still names which operator acted. Additive key
	// inside the existing hashed payload JSONB: no strict decoder reads audit
	// payloads and the hash chain covers each row's own payload, so the E9 Export
	// v1 chain and its verifier are unaffected.
	if onBehalfOf != "" {
		auditPayload["on_behalf_of"] = onBehalfOf
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
// writes the error response) when the plan's runtime estimate
// exceeds the resolved implement-stage budget and neither
// decomposition nor --override-budget is present in the comment.
//
// The estimate the gate reads is plan.GateRuntimeMinutes() —
// max(predicted_runtime_minutes, raw_predicted_runtime_minutes) — NOT the
// calibrated predicted value alone (#2862). Calibration may only ADD
// structure: a fleet factor above 1.0 can still push a plan over the budget,
// but a factor below 1.0 can no longer pull one under it and dissolve a
// required decomposition. When the two estimates STRADDLE the budget the gate
// also records a plan_budget_calibration_crossing audit entry on every outcome
// branch — including the two that let the approval proceed — so a decision the
// factor influenced is reconstructable from the trail.
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

	// #2862: the gate reads max(predicted, raw), NOT predicted alone. The
	// plan-stage calibration hint tells the planner to fold the fleet factor
	// into predicted_runtime_minutes, so with a sub-1.0 factor a raw 90
	// lands at ~50 and a required decomposition dissolves silently.
	// GateRuntimeMinutes (backend/internal/plan) is the ONE place the
	// structure-ADDING-direction rule lives: an absent or zero raw estimate
	// returns PredictedRuntimeMinutes unchanged, so every legacy plan is
	// gated byte-for-byte as before, while a factor ABOVE 1.0 can still push
	// a plan over. The gate can only evaluate a raw estimate the planner
	// actually REPORTS; an unreported one is made conspicuous to the
	// approver by the plan_warnings unreported-raw advisory instead
	// (server/plan_warnings.go), not silently trusted.
	predictedMinutes := approvedPlan.PredictedRuntimeMinutes
	rawMinutes := approvedPlan.RawPredictedRuntimeMinutes
	gateMinutes := approvedPlan.GateRuntimeMinutes()

	// crossed is true exactly when calibration moved the estimate ACROSS the
	// budget threshold in either direction: one of the two values is over
	// budget and the other is not. A crossing therefore ALWAYS implies
	// gateMinutes > budgetMinutes (the gate takes the maximum, and one side
	// is over), so the entry is never written on a within-budget approval —
	// gate_outcome is one of the three over-budget branches and never
	// within_budget. A plan that omits the raw estimate (rawMinutes == 0)
	// can never cross: there is no second number to straddle with.
	crossed := rawMinutes > 0 &&
		((rawMinutes > budgetMinutes) != (predictedMinutes > budgetMinutes))
	if crossed && s.cfg.AuditRepo != nil {
		s.appendBudgetCalibrationCrossing(r.Context(), runRow, stage,
			budgetCrossingFacts{
				RawMinutes:        rawMinutes,
				PredictedMinutes:  predictedMinutes,
				GateMinutes:       gateMinutes,
				BudgetMinutes:     budgetMinutes,
				BudgetSource:      budgetSource,
				SpecBudgetMinutes: specBudgetMinutes,
				Outcome:           planBudgetOutcome(approvedPlan, comment),
			})
	}

	if gateMinutes <= budgetMinutes {
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
	// raw_predicted_minutes and gate_predicted_minutes (#2862) say which
	// number the gate actually read: gate_predicted_minutes is
	// max(predicted, raw), and a zero raw_predicted_minutes means the plan
	// reported no pre-calibration estimate (the legacy shape, gated on
	// predicted alone).
	auditPayload, _ := json.Marshal(map[string]any{
		"stage_id":               stage.ID.String(),
		"predicted_minutes":      approvedPlan.PredictedRuntimeMinutes,
		"raw_predicted_minutes":  rawMinutes,
		"gate_predicted_minutes": gateMinutes,
		"budget_minutes":         budgetMinutes,
		"budget_source":          budgetSource,
		"spec_budget_minutes":    specBudgetMinutes,
		"timeout_source":         timeoutSource,
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
		"plan runtime estimate exceeds the resolved implement-stage budget; the gate reads max(predicted_runtime_minutes, raw_predicted_runtime_minutes) — add decomposition.sub_plans or include --override-budget in the comment",
		map[string]any{
			"stage_id":               stage.ID.String(),
			"predicted_minutes":      approvedPlan.PredictedRuntimeMinutes,
			"raw_predicted_minutes":  rawMinutes,
			"gate_predicted_minutes": gateMinutes,
			"budget_minutes":         budgetMinutes,
			"budget_source":          budgetSource,
			"spec_budget_minutes":    specBudgetMinutes,
			"timeout_source":         timeoutSource,
		})
	return false
}

// planBudgetOutcome names the branch checkPlanBudget takes for an OVER-budget
// plan (#2862). It is called only from the calibration-crossing emit, which
// fires only when the two estimates straddle the budget — and a crossing
// implies the gate maximum is over budget — so the within_budget branch is
// unreachable here BY CONSTRUCTION and is deliberately not enumerated: the
// three returns below are the complete set of outcomes a crossing entry can
// carry. The precedence mirrors checkPlanBudget's own short-circuit order
// (decomposition, then --override-budget, then refusal).
func planBudgetOutcome(p *plan.Plan, comment string) string {
	if p.Decomposition != nil {
		return "decomposition_satisfied"
	}
	if strings.Contains(comment, "--override-budget") {
		return "override_acknowledged"
	}
	return "refused"
}

// budgetCrossingFacts carries the numbers a plan_budget_calibration_crossing
// entry records. Grouped into a struct so the emit helper's signature stays
// readable and the payload keys have one definition site.
type budgetCrossingFacts struct {
	RawMinutes        int
	PredictedMinutes  int
	GateMinutes       int
	BudgetMinutes     int
	BudgetSource      string
	SpecBudgetMinutes int
	Outcome           string
}

// appendBudgetCalibrationCrossing writes the plan_budget_calibration_crossing
// audit entry (#2862): the marker that the fleet calibration factor moved the
// plan's runtime estimate ACROSS the resolved implement budget. It is emitted
// on EVERY outcome branch — the two that let the approval proceed
// (decomposition_satisfied, override_acknowledged) as well as the refusal — so
// the trail records the crossing even where the gate did not stop the run.
//
// implied_factor is predicted/raw, the factor the planner actually applied,
// reported as a float so an operator can compare it against the fleet ratio.
// It is written only when RawMinutes > 0; every caller already guards on that
// (a crossing requires a non-zero raw estimate), so the check here is the
// division's own domain guard.
//
// fleet_calibration_ratio is best-effort: resolveCalibrationHint needs a
// workflow's runtime_observed history, so a nil hint (too few samples) or a
// resolution error simply OMITS the key rather than blocking. Like every
// sibling emit in this gate, an append failure logs and never blocks the
// approval — the audit entry is a record of the decision, not part of it.
func (s *Server) appendBudgetCalibrationCrossing(ctx context.Context, runRow *run.Run, stage *run.Stage, f budgetCrossingFacts) {
	payload := map[string]any{
		"stage_id":               stage.ID.String(),
		"raw_predicted_minutes":  f.RawMinutes,
		"predicted_minutes":      f.PredictedMinutes,
		"gate_predicted_minutes": f.GateMinutes,
		"budget_minutes":         f.BudgetMinutes,
		"budget_source":          f.BudgetSource,
		"spec_budget_minutes":    f.SpecBudgetMinutes,
		"gate_outcome":           f.Outcome,
	}
	if f.RawMinutes > 0 {
		payload["implied_factor"] = float64(f.PredictedMinutes) / float64(f.RawMinutes)
	}
	if runRow != nil {
		if hint, err := s.resolveCalibrationHint(ctx, runRow.WorkflowID); err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "budget crossing: resolve calibration hint failed",
				slog.String("stage_id", stage.ID.String()),
				slog.String("error", err.Error()),
			)
		} else if hint != nil {
			payload["fleet_calibration_ratio"] = hint.CalibrationRatio
		}
	}
	body, _ := json.Marshal(payload)
	systemKind := audit.ActorKind("system")
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     stage.RunID,
		StageID:   &stage.ID,
		Timestamp: time.Now().UTC(),
		Category:  "plan_budget_calibration_crossing",
		ActorKind: &systemKind,
		Payload:   body,
	}); err != nil {
		s.cfg.Logger.Error("audit append failed for budget calibration crossing",
			"run_id", stage.RunID, "stage_id", stage.ID, "error", err.Error())
	}
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
// The fail-closed guarantee is UNIVERSAL — there is NO config-absence carve-out
// (binding condition 2 / gpt-5.6-sol's HIGH internal-inconsistency,
// claude-fable-5's authz bypass). If ArtifactRepo or RunRepo is unset,
// loadApprovedPlanForRun returns (nil, nil), which this gate treats as
// indeterminate and fails CLOSED rather than passing an unconfirmed add through:
// a non-empty add_scope_files approve must never be recorded without POSITIVE
// confirmation the plan is non-decomposed. Diverging here from
// checkPlanBudget/checkPlanScopeCap's ArtifactRepo==nil fail-open is correct
// because those gates are override-able upper-bound heuristics while this one is
// categorical (guaranteed conflict, no override). Production always wires both
// repos, so the config-absent branch is unreachable there; the divergence only
// tightens a test/misconfiguration path.
func (s *Server) checkDecomposedAddScopeFiles(w http.ResponseWriter, r *http.Request, stage *run.Stage, addScopeFiles []string) bool {
	// Empty add is always safe — nothing to fan out.
	if len(addScopeFiles) == 0 {
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

// errCodeSliceAddRequiresDecomposed is the ONE new error code the per-slice
// add channel introduces (#2515). Every other refusal on this channel is a
// shape/ownership violation and reuses the existing 400 validation_failed code
// with details.field = add_scope_files_to_slice — so the channel adds exactly
// one code, not two, and the API docs enumerate the same single addition.
const errCodeSliceAddRequiresDecomposed = "plan_slice_add_scope_files_requires_decomposed_plan"

// normalizeOwnershipPath renders a scope path in the form ownership containment
// is compared on: whitespace-trimmed with every trailing '/' removed. A
// trailing slash marks a DIRECTORY on this channel (the #824 add_scope_files
// convention the request body inherits), so "pkg/foo/" and "pkg/foo" name the
// same owned subtree and must compare equal.
func normalizeOwnershipPath(p string) string {
	return strings.TrimRight(strings.TrimSpace(p), "/")
}

// scopePathsOverlap reports whether two scope paths overlap in OWNERSHIP —
// identical, or one an ANCESTOR of the other (binding condition 1). String
// equality is NOT sufficient here: a slice may own a DIRECTORY (trailing
// slash), so handing a file INSIDE that directory to a different slice passes
// every equality check while both slices stage the same file at fan-in —
// exactly the add/add conflict single-owner-file exists to prevent.
//
// Containment is compared on path SEGMENT boundaries, so "pkg/foo" does not
// spuriously conflict with the sibling "pkg/foobar". Two paths that both
// normalize to empty (degenerate) compare equal; an empty against a non-empty
// never overlaps.
func scopePathsOverlap(a, b string) bool {
	na, nb := normalizeOwnershipPath(a), normalizeOwnershipPath(b)
	if na == "" || nb == "" {
		return na == nb
	}
	if na == nb {
		return true
	}
	return strings.HasPrefix(nb, na+"/") || strings.HasPrefix(na, nb+"/")
}

// unionScopeAdds flattens a canonical per-slice add map into flat and returns
// the deduped union, so a slice-targeted add consumes scope-cap headroom
// exactly like a flat add (#2515). flat is never mutated; the per-slice paths
// are appended in canonical (index, then path) order so the result is
// deterministic across runs despite Go's randomized map iteration.
func unionScopeAdds(flat []string, perSlice map[string][]string) []string {
	if len(perSlice) == 0 {
		return flat
	}
	out := make([]string, 0, len(flat)+len(perSlice))
	seen := make(map[string]struct{}, len(flat)+len(perSlice))
	for _, p := range flat {
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	keys := make([]string, 0, len(perSlice))
	for k := range perSlice {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		ni, erri := strconv.Atoi(keys[i])
		nj, errj := strconv.Atoi(keys[j])
		if erri == nil && errj == nil {
			return ni < nj
		}
		return keys[i] < keys[j]
	})
	for _, k := range keys {
		for _, p := range perSlice[k] {
			if _, dup := seen[p]; dup {
				continue
			}
			seen[p] = struct{}{}
			out = append(out, p)
		}
	}
	return out
}

// sliceScopeChannelError is the typed refusal SHARED by both per-slice scope
// channels — the #2515 add (validateSliceAddScopeFiles) and the #2596 move
// (validateSliceMoveScopeFiles): a human-readable message plus the `details` map
// the 400 response carries. It is a value type rather than a bare error so the
// gate renders the offending key/path and the resolvable slice list without
// re-deriving them.
type sliceScopeChannelError struct {
	msg     string
	details map[string]any
}

func (e *sliceScopeChannelError) Error() string { return e.msg }

// sliceKeyTarget is one resolved key -> slice-index pairing produced by
// resolveSliceKeys, shared by the add and move per-slice scope channels.
type sliceKeyTarget struct {
	key string
	idx int
}

// resolveSliceKeys resolves each key of a per-slice scope-channel request map to
// a slice index, shared by the #2515 add and #2596 move channels. Resolution is
// deterministic and title-first: a key that exactly matches ONE sub-plan TITLE
// (after trimming) resolves to that slice; otherwise it must parse as a 0-based
// decimal index within range. Explicit intent beats positional coincidence, so a
// plan whose sub-plan title is literally "0" resolves a "0" key by TITLE.
//
// It refuses an AMBIGUOUS key (a duplicate title), an UNRESOLVABLE key (no title
// match and not an in-range index), and two keys resolving to the SAME slice.
// `field` names the request field in every message + details so each channel
// reports its own. Keys are iterated sorted so the first refusal a given input
// produces is stable across runs (Go map order is randomized). Returns the
// targets sorted ascending by slice index.
func resolveSliceKeys(m map[string][]string, subPlans []plan.SubPlanSummary, field string) ([]sliceKeyTarget, *sliceScopeChannelError) {
	titleIdx := make(map[string][]int, len(subPlans))
	for i, sp := range subPlans {
		t := strings.TrimSpace(sp.Title)
		if t == "" {
			continue
		}
		titleIdx[t] = append(titleIdx[t], i)
	}

	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	claimedBy := make(map[int]string, len(keys))
	targets := make([]sliceKeyTarget, 0, len(keys))
	for _, k := range keys {
		trimmed := strings.TrimSpace(k)
		var idx int
		switch matches := titleIdx[trimmed]; {
		case len(matches) > 1:
			return nil, &sliceScopeChannelError{
				msg: fmt.Sprintf("%s key %q is AMBIGUOUS: %d sub-plans share that title, so the target slice cannot be resolved; key by the 0-based slice index instead",
					field, k, len(matches)),
				details: map[string]any{
					"field":  field,
					"key":    k,
					"reason": "slice_key_ambiguous",
					"slices": sliceIndexTitles(subPlans),
				},
			}
		case len(matches) == 1:
			idx = matches[0]
		default:
			n, err := strconv.Atoi(trimmed)
			if err != nil || n < 0 || n >= len(subPlans) {
				return nil, &sliceScopeChannelError{
					msg: fmt.Sprintf("%s key %q resolves to no slice: it matches no sub-plan title and is not a 0-based index in [0,%d); key by an exact sub-plan title or its index",
						field, k, len(subPlans)),
					details: map[string]any{
						"field":  field,
						"key":    k,
						"reason": "slice_key_unresolvable",
						"slices": sliceIndexTitles(subPlans),
					},
				}
			}
			idx = n
		}
		if prior, dup := claimedBy[idx]; dup {
			return nil, &sliceScopeChannelError{
				msg: fmt.Sprintf("%s keys %q and %q both resolve to slice %d (%q); name each slice at most once",
					field, prior, k, idx, subPlans[idx].Title),
				details: map[string]any{
					"field":       field,
					"keys":        []string{prior, k},
					"slice_index": idx,
					"reason":      "duplicate_slice_key",
					"slices":      sliceIndexTitles(subPlans),
				},
			}
		}
		claimedBy[idx] = k
		targets = append(targets, sliceKeyTarget{key: k, idx: idx})
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].idx < targets[j].idx })
	return targets, nil
}

// canonicalizeSlicePaths trims, validates, dedupes and SORTS the path list for
// one resolved key of a per-slice scope channel, shared by the add and move
// channels. It refuses an empty/whitespace path, a non-repo-relative path, and
// an empty list (after trimming). `field` names the request field in the
// messages. Sorting (rather than preserving input order) is what makes two
// requests naming the same paths in different orders canonicalise identically —
// the byte-identical recording prompt-hash replay stability depends on.
func canonicalizeSlicePaths(key string, idx int, raw []string, field string) ([]string, *sliceScopeChannelError) {
	seen := make(map[string]struct{}, len(raw))
	paths := make([]string, 0, len(raw))
	for _, r := range raw {
		p := strings.TrimSpace(r)
		if p == "" {
			return nil, &sliceScopeChannelError{
				msg: fmt.Sprintf("%s key %q lists an empty path; every entry must name a non-empty repo-relative path", field, key),
				details: map[string]any{
					"field":       field,
					"key":         key,
					"slice_index": idx,
					"reason":      "empty_path",
				},
			}
		}
		if !isRepoRelativePath(p) {
			return nil, &sliceScopeChannelError{
				msg: fmt.Sprintf("%s path %q (key %q) must be repo-relative (no leading '/' or '..')", field, p, key),
				details: map[string]any{
					"field":       field,
					"key":         key,
					"path":        p,
					"slice_index": idx,
					"reason":      "path_not_repo_relative",
				},
			}
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		paths = append(paths, p)
	}
	if len(paths) == 0 {
		return nil, &sliceScopeChannelError{
			msg: fmt.Sprintf("%s key %q carries no paths; omit the key or name at least one path", field, key),
			details: map[string]any{
				"field":       field,
				"key":         key,
				"slice_index": idx,
				"reason":      "empty_path_list",
			},
		}
	}
	sort.Strings(paths)
	return paths, nil
}

// sliceIndexTitles renders the decomposition's slices as an ordered
// [{index,title}] list for an error's details, in DECLARED index order (never
// Go map order) so the message is byte-stable across runs.
func sliceIndexTitles(subPlans []plan.SubPlanSummary) []map[string]any {
	out := make([]map[string]any, 0, len(subPlans))
	for i, sp := range subPlans {
		out = append(out, map[string]any{"index": i, "title": sp.Title})
	}
	return out
}

// validateSliceAddScopeFiles resolves an add_scope_files_to_slice request map
// against the plan's decomposition and returns the CANONICAL index-keyed map
// (#2515), or a typed refusal naming the offending key/path.
//
// Key resolution is deterministic and title-first: a key that exactly matches
// one sub-plan TITLE (after trimming) resolves to that slice; otherwise it must
// parse as a 0-based decimal index within range. Explicit intent beats
// positional coincidence, so a plan whose sub-plan title is literally "0"
// resolves a "0" key by TITLE, not by index.
//
// CANONICAL FORM (one rule, pinned by test — the byte-identical
// title-vs-index recording and prompt-hash replay stability both depend on
// exactly one): each key becomes strconv.Itoa(index), and each path list is
// trimmed, deduped, then SORTED lexicographically. Sorting (rather than
// preserving input order) is what makes two requests naming the same paths in
// different orders record byte-identically.
//
// Refusals, each with its own test:
//
//	(a) a key resolving to no slice (unresolvable), or matching a DUPLICATE
//	    title (ambiguous — refused rather than silently first-wins);
//	(b) two keys resolving to the SAME slice;
//	(c) a path whose ownership OVERLAPS a path under a DIFFERENT key in this
//	    same request (containment, not equality — binding condition 1);
//	(d) a path whose ownership OVERLAPS a DIFFERENT slice's declared
//	    scope.files (single-owner-file against the plan itself);
//	(e) an empty/whitespace or non-repo-relative path;
//	(f) a key whose path list is empty after trimming.
//
// Every scan iterates slices by INDEX, never the request map directly, so the
// first refusal a given input produces is stable across runs.
func validateSliceAddScopeFiles(m map[string][]string, subPlans []plan.SubPlanSummary) (map[string][]string, *sliceScopeChannelError) {
	if len(m) == 0 {
		return nil, nil
	}

	targets, kerr := resolveSliceKeys(m, subPlans, "add_scope_files_to_slice")
	if kerr != nil {
		return nil, kerr
	}

	cleaned := make(map[int][]string, len(targets))
	out := make(map[string][]string, len(targets))
	for _, tg := range targets {
		paths, perr := canonicalizeSlicePaths(tg.key, tg.idx, m[tg.key], "add_scope_files_to_slice")
		if perr != nil {
			return nil, perr
		}
		cleaned[tg.idx] = paths
		out[strconv.Itoa(tg.idx)] = paths
	}

	// (c) Cross-key ownership overlap WITHIN this request. Containment, not
	// equality: one key naming a directory and another a file inside it is the
	// same add/add fan-in as naming the identical path twice.
	for a := 0; a < len(targets); a++ {
		for b := a + 1; b < len(targets); b++ {
			ia, ib := targets[a].idx, targets[b].idx
			for _, pa := range cleaned[ia] {
				for _, pb := range cleaned[ib] {
					if !scopePathsOverlap(pa, pb) {
						continue
					}
					return nil, &sliceScopeChannelError{
						msg: fmt.Sprintf("add_scope_files_to_slice paths %q (slice %d, %q) and %q (slice %d, %q) overlap in ownership; a path may be added to exactly ONE slice (single-owner-file)",
							pa, ia, subPlans[ia].Title, pb, ib, subPlans[ib].Title),
						details: map[string]any{
							"field":        "add_scope_files_to_slice",
							"path":         pa,
							"other_path":   pb,
							"slice_index":  ia,
							"other_slice":  ib,
							"reason":       "path_under_two_slices",
							"slices":       sliceIndexTitles(subPlans),
							"overlap_kind": overlapKind(pa, pb),
						},
					}
				}
			}
		}
	}

	// (d) Ownership against the plan's OWN per-slice scopes. This is the
	// refusal the motivating incident (#2515) lands on: a file already declared
	// in slice A that the operator wants alongside slice B's files. The channel
	// ADDS, it does not MOVE — so the message names the owning slice and the
	// remedy rather than leaving an operator to discover it at the gate.
	for _, tg := range targets {
		for _, p := range cleaned[tg.idx] {
			for j, sp := range subPlans {
				if j == tg.idx || sp.Scope == nil {
					continue
				}
				for _, f := range sp.Scope.Files {
					if !scopePathsOverlap(p, f.Path) {
						continue
					}
					return nil, &sliceScopeChannelError{
						msg: fmt.Sprintf("add_scope_files_to_slice path %q (requested for slice %d, %q) is already owned by slice %d (%q), whose declared scope.files carries %q. This channel ADDS a path to one slice; it does NOT MOVE a path between slices, so the add is refused rather than leaving two slices staging the same file. Remedy: re-plan the decomposition (fishhawk_revise_plan, or reject the plan with a re-plan) so %q is declared in the slice that needs it",
							p, tg.idx, subPlans[tg.idx].Title, j, sp.Title, f.Path, p),
						details: map[string]any{
							"field":            "add_scope_files_to_slice",
							"path":             p,
							"requested_slice":  tg.idx,
							"owning_slice":     j,
							"owning_title":     sp.Title,
							"owning_path":      f.Path,
							"reason":           "path_owned_by_another_slice",
							"channel_semantic": "add_not_move",
							"remedy":           "re-plan the decomposition so the path is declared in the slice that needs it; this channel does not move a path between slices",
							"slices":           sliceIndexTitles(subPlans),
							"overlap_kind":     overlapKind(p, f.Path),
						},
					}
				}
			}
		}
	}

	return out, nil
}

// overlapKind labels HOW two overlapping paths overlap, so an operator reading
// the refusal can tell an exact duplicate from a directory-containment
// conflict (the case string equality would have missed).
func overlapKind(a, b string) string {
	na, nb := normalizeOwnershipPath(a), normalizeOwnershipPath(b)
	switch {
	case na == nb:
		return "identical"
	case strings.HasPrefix(nb, na+"/"):
		return "ancestor_directory"
	default:
		return "descendant_path"
	}
}

// checkSliceAddScopeFiles enforces the per-slice add channel's contract on a
// plan-stage approve (#2515), PRE-Submit — before any approval row is inserted,
// for the same ADR-036 reason as its siblings: a refused approve records no row
// so a corrected retry flows normally.
//
// Returns (nil, true) — byte-identical to today — when the map is absent or
// empty. Otherwise it loads the run's plan and:
//
//   - a load error or a nil/indeterminate plan → 422
//     plan_slice_add_scope_files_requires_decomposed_plan with
//     details.reason = plan_indeterminate. FAIL CLOSED, mirroring
//     checkDecomposedAddScopeFiles' divergence from the fail-open sibling
//     gates: the single-owner-file invariant this channel upholds is
//     categorical, so a slice-targeted add must never be recorded without
//     POSITIVE confirmation the plan is decomposed (there would otherwise be no
//     slice to target and the recorded map would be unresolvable at fold time).
//     The fail-closed guarantee is universal — an unwired ArtifactRepo/RunRepo
//     makes loadApprovedPlanForRun return (nil, nil), which is treated as
//     indeterminate, not as a carve-out.
//   - a positively FLAT plan (Decomposition == nil) → the same 422 with
//     details.reason = plan_not_decomposed and a message pointing at plain
//     add_scope_files, which is the correct channel there.
//   - a decomposed plan → validateSliceAddScopeFiles; a refusal renders 400
//     validation_failed carrying the offending key/path plus the ordered
//     {index,title} slice list, so the operator can retry with a resolvable key
//     in one shot.
//
// No audit entry is appended on refusal: the 400-class validation refusals
// elsewhere on this handler (remove_scope_files) do not audit either, and no
// approval row is inserted, so there is nothing to reconcile.
func (s *Server) checkSliceAddScopeFiles(w http.ResponseWriter, r *http.Request, stage *run.Stage, addScopeFilesToSlice map[string][]string) (map[string][]string, bool) {
	if len(addScopeFilesToSlice) == 0 {
		return nil, true
	}

	approvedPlan, err := s.loadApprovedPlanForRun(r.Context(), stage.RunID)
	if err != nil || approvedPlan == nil {
		if err != nil {
			s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn, "slice-add-scope-files gate: load plan failed",
				slog.String("stage_id", stage.ID.String()),
				slog.String("error", err.Error()),
			)
		}
		s.writeError(w, r, http.StatusUnprocessableEntity, errCodeSliceAddRequiresDecomposed,
			"add_scope_files_to_slice was supplied but the run's plan could not be confirmed DECOMPOSED; the approve is refused so a slice-targeted add cannot be recorded against a plan with no resolvable slices. Retry once the plan is loadable",
			map[string]any{
				"stage_id": stage.ID.String(),
				"reason":   "plan_indeterminate",
			})
		return nil, false
	}

	if approvedPlan.Decomposition == nil {
		s.writeError(w, r, http.StatusUnprocessableEntity, errCodeSliceAddRequiresDecomposed,
			"add_scope_files_to_slice targets one slice of a DECOMPOSED plan, but this run's plan is flat (no decomposition); use the plain add_scope_files field instead",
			map[string]any{
				"stage_id": stage.ID.String(),
				"reason":   "plan_not_decomposed",
			})
		return nil, false
	}

	canonical, verr := validateSliceAddScopeFiles(addScopeFilesToSlice, approvedPlan.Decomposition.SubPlans)
	if verr != nil {
		details := verr.details
		if details == nil {
			details = map[string]any{"field": "add_scope_files_to_slice"}
		}
		details["stage_id"] = stage.ID.String()
		s.writeError(w, r, http.StatusBadRequest, "validation_failed", verr.Error(), details)
		return nil, false
	}
	return canonical, true
}

// errCodeSliceMoveRequiresDecomposed and errCodeSliceMoveAfterDispatch are the
// TWO new error codes the per-slice move channel introduces (#2596). Every other
// refusal on this channel is a shape/ownership violation and reuses the existing
// 400 validation_failed code with details.field = move_scope_files_to_slice.
const (
	errCodeSliceMoveRequiresDecomposed = "plan_slice_move_scope_files_requires_decomposed_plan"
	errCodeSliceMoveAfterDispatch      = "plan_slice_move_after_dispatch"
)

// moveFieldName is the request-field name every move-channel refusal reports, so
// the shared helpers (resolveSliceKeys / canonicalizeSlicePaths) and the
// move-specific ownership rules all name one field.
const moveFieldName = "move_scope_files_to_slice"

// movedPath is the durable, human-legible resolution of ONE moved path recorded
// on the approval_submitted audit as move_scope_files_resolved (#2596). It is
// the only surface on which the true MOVE provenance survives — every scope
// consumer folds the destination side identically to a genuine add — so it names
// the SOURCE slice (derived from ownership) and DESTINATION slice explicitly.
type movedPath struct {
	Path      string `json:"path"`
	FromSlice int    `json:"from_slice"`
	ToSlice   int    `json:"to_slice"`
}

// validateSliceMoveScopeFiles resolves a move_scope_files_to_slice request map
// against the plan's decomposition and returns the CANONICAL index-keyed
// DESTINATION map plus the resolved []movedPath list (sorted by ToSlice then
// Path), or a typed refusal (#2596). Keys name the DESTINATION slice and resolve
// exactly like the add channel (resolveSliceKeys); each value must ALREADY be
// declared in the decomposition's scope, and the SOURCE is derived from
// ownership. addCanonical is THIS call's canonical add map, for the cross-channel
// disjointness check.
//
// Rules, in order, each scanning slices by INDEX for stable first-refusal:
//
//	(a) path_under_two_slices — the same path listed under two destination keys;
//	(b) path_in_both_scope_channels — the path also appears in this call's
//	    add_scope_files_to_slice (the fields compose in ONE call but must name
//	    disjoint paths);
//	(c) locate the owner by EXACT ownership identity (normalizeOwnershipPath
//	    equality, not containment); a normalized-equal but NON-byte-exact
//	    request (a trailing-slash alias of a declared directory, e.g. "pkg/dir"
//	    for a declared "pkg/dir/") is refused move_requires_exact_owned_path so
//	    the verbatim destination fold cannot drop the trailing slash and narrow
//	    a directory to a file path;
//	(d) no exact owner but a containment overlap -> move_requires_exact_owned_path
//	    (a directory-valued scope entry must be re-planned, not split by a move);
//	(e) no owner and no overlap -> path_not_in_declared_scope, pointing at
//	    add_scope_files_to_slice as the channel that ADDS a net-new path;
//	(f) owner == destination -> path_already_owned_by_destination (a no-op move);
//	(g) move_would_empty_source_slice — the move would remove the LAST declared
//	    path from a source slice (an empty per-slice scope makes
//	    requireDecomposedScope 409 at dispatch, stranding the child).
func validateSliceMoveScopeFiles(m map[string][]string, subPlans []plan.SubPlanSummary, addCanonical map[string][]string) (map[string][]string, []movedPath, *sliceScopeChannelError) {
	if len(m) == 0 {
		return nil, nil, nil
	}

	targets, kerr := resolveSliceKeys(m, subPlans, moveFieldName)
	if kerr != nil {
		return nil, nil, kerr
	}

	cleaned := make(map[int][]string, len(targets))
	out := make(map[string][]string, len(targets))
	for _, tg := range targets {
		paths, perr := canonicalizeSlicePaths(tg.key, tg.idx, m[tg.key], moveFieldName)
		if perr != nil {
			return nil, nil, perr
		}
		cleaned[tg.idx] = paths
		out[strconv.Itoa(tg.idx)] = paths
	}

	// (a) The same path listed under two DESTINATION keys — an ambiguous move
	// with no single destination. Exact ownership identity (a move names an
	// exact owned path), so "pkg/foo" and "pkg/foo/" collide but a directory and
	// a file within it do not (that is the (d) containment refusal's job).
	for a := 0; a < len(targets); a++ {
		for b := a + 1; b < len(targets); b++ {
			ia, ib := targets[a].idx, targets[b].idx
			for _, pa := range cleaned[ia] {
				for _, pb := range cleaned[ib] {
					if normalizeOwnershipPath(pa) != normalizeOwnershipPath(pb) {
						continue
					}
					return nil, nil, &sliceScopeChannelError{
						msg: fmt.Sprintf("move_scope_files_to_slice path %q is listed under two destination slices (%d %q and %d %q); a path can move to exactly ONE slice",
							pa, ia, subPlans[ia].Title, ib, subPlans[ib].Title),
						details: map[string]any{
							"field":       moveFieldName,
							"path":        pa,
							"slice_index": ia,
							"other_slice": ib,
							"reason":      "path_under_two_slices",
							"slices":      sliceIndexTitles(subPlans),
						},
					}
				}
			}
		}
	}

	// (b) Cross-channel disjointness: a path may not be in BOTH the move map and
	// this call's add map. The two channels compose in one approve, but only over
	// disjoint paths — adding and moving the same path in one call is ambiguous.
	if len(addCanonical) > 0 {
		addByNorm := make(map[string]string, len(addCanonical))
		for _, paths := range addCanonical {
			for _, p := range paths {
				addByNorm[normalizeOwnershipPath(p)] = p
			}
		}
		for _, tg := range targets {
			for _, p := range cleaned[tg.idx] {
				if addPath, ok := addByNorm[normalizeOwnershipPath(p)]; ok {
					return nil, nil, &sliceScopeChannelError{
						msg: fmt.Sprintf("move_scope_files_to_slice path %q also appears in add_scope_files_to_slice (as %q); the two channels compose in one approve but must name DISJOINT paths — move OR add a given path, not both",
							p, addPath),
						details: map[string]any{
							"field":       moveFieldName,
							"path":        p,
							"add_path":    addPath,
							"slice_index": tg.idx,
							"reason":      "path_in_both_scope_channels",
						},
					}
				}
			}
		}
	}

	// (c)-(f) Per-path ownership resolution. Iterate targets in ascending slice
	// order (resolveSliceKeys sorted them) for a stable first refusal.
	var resolved []movedPath
	for _, tg := range targets {
		for _, p := range cleaned[tg.idx] {
			owner := -1
			var ownerPath string
			for j, sp := range subPlans {
				if sp.Scope == nil {
					continue
				}
				for _, f := range sp.Scope.Files {
					if normalizeOwnershipPath(f.Path) == normalizeOwnershipPath(p) {
						owner = j
						ownerPath = f.Path
						break
					}
				}
				if owner >= 0 {
					break
				}
			}
			if owner >= 0 && ownerPath != p {
				// (c') Normalized-equal but NOT byte-exact: the request is a
				// trailing-slash alias of the declared entry (normalizeOwnershipPath
				// trims "/"), e.g. "pkg/dir" for a declared directory "pkg/dir/".
				// The accepted spelling is folded VERBATIM into the destination
				// slice (out[dest] -> resolveApprovalSliceMoves -> the gained fold in
				// resolveDecomposedScopeConstraint), so admitting the alias would drop
				// the trailing slash that the scope format uses to mark a directory,
				// narrowing the destination to a FILE path while the source directory
				// is removed. A move names the owned path byte-for-byte; point the
				// operator at the exact declared spelling so a directory move keeps
				// its trailing slash.
				return nil, nil, &sliceScopeChannelError{
					msg: fmt.Sprintf("move_scope_files_to_slice path %q is not the EXACT declared spelling of slice %d (%q)'s owned entry %q — it matches only after trailing-slash normalization. A move folds the path verbatim into the destination, so an alias would drop the trailing slash that marks a directory. Retry naming the owned path exactly as %q",
						p, owner, subPlans[owner].Title, ownerPath, ownerPath),
					details: map[string]any{
						"field":         moveFieldName,
						"path":          p,
						"declared_path": ownerPath,
						"owner_slice":   owner,
						"reason":        "move_requires_exact_owned_path",
						"slices":        sliceIndexTitles(subPlans),
					},
				}
			}
			if owner < 0 {
				// (d) No exact owner — but a containment overlap means the operator
				// named half of a directory-valued declared entry, which a move
				// cannot express (neither side can split the entry). Re-plan.
				for j, sp := range subPlans {
					if sp.Scope == nil {
						continue
					}
					for _, f := range sp.Scope.Files {
						if !scopePathsOverlap(p, f.Path) {
							continue
						}
						return nil, nil, &sliceScopeChannelError{
							msg: fmt.Sprintf("move_scope_files_to_slice path %q is not an EXACT declared scope path: it only overlaps slice %d (%q)'s declared entry %q by directory containment (%s). A move names an exact owned path; a directory-valued scope entry must be re-planned rather than split by a move",
								p, j, sp.Title, f.Path, overlapKind(p, f.Path)),
							details: map[string]any{
								"field":           moveFieldName,
								"path":            p,
								"overlap_path":    f.Path,
								"overlap_slice":   j,
								"overlap_kind":    overlapKind(p, f.Path),
								"requested_slice": tg.idx,
								"reason":          "move_requires_exact_owned_path",
								"slices":          sliceIndexTitles(subPlans),
							},
						}
					}
				}
				// (e) No owner and no overlap — the path is not in the declared
				// scope at all, so there is nothing to move. This is the ADD case.
				return nil, nil, &sliceScopeChannelError{
					msg: fmt.Sprintf("move_scope_files_to_slice path %q (requested for slice %d, %q) is not declared in ANY slice's scope.files, so there is nothing to MOVE. This channel moves an already-in-scope path between slices; to add a NET-NEW path to a slice use add_scope_files_to_slice instead",
						p, tg.idx, subPlans[tg.idx].Title),
					details: map[string]any{
						"field":           moveFieldName,
						"path":            p,
						"requested_slice": tg.idx,
						"reason":          "path_not_in_declared_scope",
						"remedy":          "use add_scope_files_to_slice to add a net-new path to a slice; move_scope_files_to_slice only relocates an already-declared path",
						"add_channel":     "add_scope_files_to_slice",
						"slices":          sliceIndexTitles(subPlans),
					},
				}
			}
			if owner == tg.idx {
				// (f) Owner IS the destination — a no-op move, refused rather than
				// silently accepted so an operator typo surfaces.
				return nil, nil, &sliceScopeChannelError{
					msg: fmt.Sprintf("move_scope_files_to_slice path %q is ALREADY owned by destination slice %d (%q); the move is a no-op",
						p, tg.idx, subPlans[tg.idx].Title),
					details: map[string]any{
						"field":       moveFieldName,
						"path":        p,
						"owner_path":  ownerPath,
						"slice_index": tg.idx,
						"reason":      "path_already_owned_by_destination",
						"slices":      sliceIndexTitles(subPlans),
					},
				}
			}
			resolved = append(resolved, movedPath{Path: p, FromSlice: owner, ToSlice: tg.idx})
		}
	}

	// (g) move_would_empty_source_slice: refuse a move that removes the LAST
	// declared path from a source slice, for the same reason validateRemoveScope
	// refuses emptying a non-empty scope — an empty per-slice scope makes
	// requireDecomposedScope 409 decomposed_scope_unresolved at dispatch,
	// stranding the child. Aggregate the moved-away paths per source slice, then
	// scan source slices in ascending order for a stable first refusal.
	movedFrom := make(map[int]map[string]struct{})
	for _, mp := range resolved {
		if movedFrom[mp.FromSlice] == nil {
			movedFrom[mp.FromSlice] = make(map[string]struct{})
		}
		movedFrom[mp.FromSlice][normalizeOwnershipPath(mp.Path)] = struct{}{}
	}
	srcIdxs := make([]int, 0, len(movedFrom))
	for src := range movedFrom {
		srcIdxs = append(srcIdxs, src)
	}
	sort.Ints(srcIdxs)
	for _, src := range srcIdxs {
		declared := make(map[string]struct{})
		if subPlans[src].Scope != nil {
			for _, f := range subPlans[src].Scope.Files {
				declared[normalizeOwnershipPath(f.Path)] = struct{}{}
			}
		}
		if len(declared) == 0 {
			continue
		}
		allMoved := true
		for d := range declared {
			if _, ok := movedFrom[src][d]; !ok {
				allMoved = false
				break
			}
		}
		if allMoved {
			return nil, nil, &sliceScopeChannelError{
				msg: fmt.Sprintf("move_scope_files_to_slice would move EVERY declared path out of source slice %d (%q), emptying it; a slice with no scope.files strands its fan-out child at dispatch. Re-plan the decomposition to drop the slice instead",
					src, subPlans[src].Title),
				details: map[string]any{
					"field":        moveFieldName,
					"source_slice": src,
					"reason":       "move_would_empty_source_slice",
					"slices":       sliceIndexTitles(subPlans),
				},
			}
		}
	}

	sort.Slice(resolved, func(i, j int) bool {
		if resolved[i].ToSlice != resolved[j].ToSlice {
			return resolved[i].ToSlice < resolved[j].ToSlice
		}
		return resolved[i].Path < resolved[j].Path
	})
	return out, resolved, nil
}

// checkSliceMoveScopeFiles enforces the per-slice move channel's contract on a
// plan-stage approve (#2596), PRE-Submit — before any approval row is inserted,
// for the same ADR-036 reason as its siblings: a refused approve records no row
// so a corrected retry flows normally.
//
// Returns (nil, nil, true) — byte-identical to today — when the map is absent or
// empty. Otherwise it loads the run's plan and:
//
//   - a load error or a nil/indeterminate plan -> 422
//     plan_slice_move_scope_files_requires_decomposed_plan, details.reason
//     plan_indeterminate. FAIL CLOSED (universal, including an unwired
//     ArtifactRepo/RunRepo, which yields (nil, nil)): a move must never be
//     recorded without POSITIVE confirmation the plan is decomposed.
//   - a positively FLAT plan -> the same 422, details.reason plan_not_decomposed.
//   - a decomposed plan -> validateSliceMoveScopeFiles; a refusal renders 400
//     validation_failed carrying the offending key/path, the resolved
//     {index,title} slice list, and the remedy.
//
// After validation succeeds it runs the dispatched-sibling ordering guard: a
// minted fan-out child whose SliceIndex is a SOURCE or DESTINATION of any
// resolved move that is past run state 'pending' refuses 409
// plan_slice_move_after_dispatch (its work has begun; un-scoping it retroactively
// is the harm). A ListRuns error refuses the same code with details.reason
// dispatch_state_indeterminate rather than admitting a move whose safety could
// not be confirmed. Blockers are selected in ascending slice order so the named
// child is deterministic.
func (s *Server) checkSliceMoveScopeFiles(w http.ResponseWriter, r *http.Request, stage *run.Stage, moveScopeFilesToSlice, addCanonical map[string][]string) (map[string][]string, []movedPath, bool) {
	if len(moveScopeFilesToSlice) == 0 {
		return nil, nil, true
	}

	approvedPlan, err := s.loadApprovedPlanForRun(r.Context(), stage.RunID)
	if err != nil || approvedPlan == nil {
		if err != nil {
			s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn, "slice-move-scope-files gate: load plan failed",
				slog.String("stage_id", stage.ID.String()),
				slog.String("error", err.Error()),
			)
		}
		s.writeError(w, r, http.StatusUnprocessableEntity, errCodeSliceMoveRequiresDecomposed,
			"move_scope_files_to_slice was supplied but the run's plan could not be confirmed DECOMPOSED; the approve is refused so a slice-boundary move cannot be recorded against a plan with no resolvable slices. Retry once the plan is loadable",
			map[string]any{
				"stage_id": stage.ID.String(),
				"reason":   "plan_indeterminate",
			})
		return nil, nil, false
	}

	if approvedPlan.Decomposition == nil {
		s.writeError(w, r, http.StatusUnprocessableEntity, errCodeSliceMoveRequiresDecomposed,
			"move_scope_files_to_slice moves a path between slices of a DECOMPOSED plan, but this run's plan is flat (no decomposition); there are no slices to move between",
			map[string]any{
				"stage_id": stage.ID.String(),
				"reason":   "plan_not_decomposed",
			})
		return nil, nil, false
	}

	canonical, resolved, verr := validateSliceMoveScopeFiles(moveScopeFilesToSlice, approvedPlan.Decomposition.SubPlans, addCanonical)
	if verr != nil {
		details := verr.details
		if details == nil {
			details = map[string]any{"field": moveFieldName}
		}
		details["stage_id"] = stage.ID.String()
		s.writeError(w, r, http.StatusBadRequest, "validation_failed", verr.Error(), details)
		return nil, nil, false
	}

	// Dispatched-sibling ordering guard. A move re-scopes the source AND
	// destination slices; if a fan-out child of either has already left 'pending'
	// its work has begun and un-scoping it retroactively is the harm. FAIL CLOSED
	// on a ListRuns error.
	//
	// ACCEPTED TOCTOU WINDOW: this is a non-atomic check-then-act — a sibling that
	// transitions pending->running BETWEEN this listing and the approval Submit
	// below is admitted, the exact retroactive un-scoping the guard refuses. The
	// window is narrow and mostly unreachable on the normal flow (dispatch happens
	// only AFTER this approval), so it matters only on a repeat approve racing a
	// concurrent dispatch. The plan did not require a transactional guard; a
	// stronger guarantee (advisory-locked read-then-insert) is left as follow-up.
	affected := make(map[int]struct{}, len(resolved)*2)
	for _, mp := range resolved {
		affected[mp.FromSlice] = struct{}{}
		affected[mp.ToSlice] = struct{}{}
	}
	siblings, lerr := s.listDecomposedSiblings(r.Context(), stage.RunID)
	if lerr != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn, "slice-move-scope-files gate: list siblings failed",
			slog.String("stage_id", stage.ID.String()),
			slog.String("error", lerr.Error()),
		)
		s.writeError(w, r, http.StatusConflict, errCodeSliceMoveAfterDispatch,
			"move_scope_files_to_slice could not confirm no source/destination fan-out child has begun (listing minted children failed); the move is refused rather than admitted against unknown dispatch state. Retry once the backend is reachable",
			map[string]any{
				"stage_id": stage.ID.String(),
				"reason":   "dispatch_state_indeterminate",
			})
		return nil, nil, false
	}
	// Select the blocker in ascending slice order for determinism.
	var blocker *run.Run
	for _, sib := range siblings {
		if sib.SliceIndex == nil {
			continue
		}
		if _, ok := affected[*sib.SliceIndex]; !ok {
			continue
		}
		if sib.State == run.StatePending {
			continue
		}
		if blocker == nil || *sib.SliceIndex < *blocker.SliceIndex {
			blocker = sib
		}
	}
	if blocker != nil {
		// Name the resolved move touching the blocker's slice (as source or dest).
		from, to := -1, -1
		var path string
		for _, mp := range resolved {
			if mp.FromSlice == *blocker.SliceIndex || mp.ToSlice == *blocker.SliceIndex {
				from, to, path = mp.FromSlice, mp.ToSlice, mp.Path
				break
			}
		}
		s.writeError(w, r, http.StatusConflict, errCodeSliceMoveAfterDispatch,
			fmt.Sprintf("move_scope_files_to_slice touches slice %d, whose fan-out child (run %s) has already left 'pending' (state %s); re-scoping a slice whose work has begun is refused. Revise the plan or start a fresh run",
				*blocker.SliceIndex, blocker.ID.String(), blocker.State),
			map[string]any{
				"stage_id":     stage.ID.String(),
				"path":         path,
				"from_slice":   from,
				"to_slice":     to,
				"child_run_id": blocker.ID.String(),
				"child_state":  string(blocker.State),
				"slice_index":  *blocker.SliceIndex,
				"reason":       "slice_already_started",
			})
		return nil, nil, false
	}

	return canonical, resolved, true
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
// Override posture is now CONDITIONAL (#2415). Declared scope is an upper
// bound on the eventual diff, not a prediction, so --override-scope-cap still
// clears the DECLARED-scope pre-check when the landing can plausibly fit. But
// the override never reached the implement stage's max_files_changed
// re-evaluation, which counts the REAL physical diff: an over-cap change was
// approved with the override and then failed category-B at implement, after a
// full run. So when the plan PROVABLY cannot land — its minimum physical
// changed-file count (minPhysicalFileCount, the most generous reading of the
// number the hard gate counts) already exceeds the cap — the override is
// REFUSED at the plan gate (422 plan_scope_cap_override_unavailable) instead of
// granting a per-run exception the implement stage will not honor.
//
// The refusal is safe to make CATEGORICAL within a run because the cap is
// IMMUTABLE there: this gate resolves it via resolveImplementConstraints and
// the post-implement gate via loadStageConstraintsFromCache, and BOTH parse the
// same runRow.WorkflowSpec snapshot cached at run creation with the same
// min-wins merge — raising .fishhawk/workflows.yaml cannot change an in-flight
// run's cap, so an override that cannot fit today can never fit later in that
// run. --override-scope-cap otherwise mirrors checkPlanBudget's
// --override-budget posture, acknowledged via a
// plan_scope_cap_override_acknowledged audit entry recording that it covers the
// declared-scope pre-check ONLY.
//
// Fail-open matching checkPlanBudget: any read failure, absent spec, or
// missing plan skips the check (effectiveScopePathSetWithOps WARN-logs), so a
// degraded backend can never brick the approval gate. A cap of 0 means
// no cap is configured — nothing to enforce.
func (s *Server) checkPlanScopeCap(w http.ResponseWriter, r *http.Request, stage *run.Stage, comment string, addScopeFiles, removeScopeFiles []string) bool {
	// Subtract the gate-time removals (#1726) so a cap overflow can be
	// reconciled entirely at the gate (remove or remove+add-replace) without a
	// re-plan. Use effectiveScopePathSetWithOps so the per-path declared
	// operations are available for the minimum-physical-count estimate (#2415);
	// the three early returns below stay byte-identical to the prior gate.
	paths, ops, maxFiles, ok := s.effectiveScopePathSetWithOps(r.Context(), stage.RunID, addScopeFiles, removeScopeFiles)
	effectiveCount := len(paths)
	if !ok || maxFiles <= 0 || effectiveCount <= maxFiles {
		return true
	}

	// The minimum PHYSICAL changed-file count the hard gate counts (#2415).
	// Reported on every headroom surface below alongside the declared count,
	// unconditionally — an operator reasoning about headroom must always know
	// WHICH number the implement gate uses, even when the two coincide.
	minPhysical := minPhysicalFileCount(paths, ops)

	basePayload := map[string]any{
		"stage_id":                 stage.ID.String(),
		"scoped_files":             effectiveCount,
		"min_changed_files":        minPhysical,
		"max_files_changed":        maxFiles,
		"add_scope_files_count":    len(addScopeFiles),
		"remove_scope_files_count": len(removeScopeFiles),
	}
	systemKind := audit.ActorKind("system")

	if strings.Contains(comment, "--override-scope-cap") {
		// The override PROVABLY cannot authorize the landing: even the most
		// generous physical-count reading is over cap, and the cap is fixed for
		// this run. Refuse instead of granting an exception the implement stage
		// will not honor.
		if minPhysical > maxFiles {
			if s.cfg.AuditRepo != nil {
				refusedPayload, _ := json.Marshal(basePayload)
				if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
					RunID:     stage.RunID,
					StageID:   &stage.ID,
					Timestamp: time.Now().UTC(),
					Category:  "plan_scope_cap_override_refused",
					ActorKind: &systemKind,
					Payload:   refusedPayload,
				}); err != nil {
					s.cfg.Logger.Error("audit append failed for scope-cap override refusal",
						"run_id", stage.RunID, "stage_id", stage.ID, "error", err.Error())
				}
			}
			s.writeError(w, r, http.StatusUnprocessableEntity, "plan_scope_cap_override_unavailable",
				"--override-scope-cap cannot authorize this landing. The effective declared scope is "+
					strconv.Itoa(effectiveCount)+" files and its minimum physical changed-file count is "+
					strconv.Itoa(minPhysical)+", both against the implement stage's constraints.max_files_changed cap of "+
					strconv.Itoa(maxFiles)+". The override clears only the declared-scope pre-check; the implement stage "+
					"re-evaluates constraints.max_files_changed against the REAL diff, and that cap is fixed for this run "+
					"(the run-row spec snapshot), so an override that cannot fit today can never fit later in this run. "+
					"Drop declared paths the change will not touch via remove_scope_files, or raise "+
					"constraints.max_files_changed through a governed spec change and start a fresh run.",
				basePayload)
			return false
		}
		// Override honored: the declared scope is over cap but the minimum
		// physical count still fits, so the landing can plausibly succeed.
		// Record that the override covers the declared-scope pre-check ONLY.
		if s.cfg.AuditRepo != nil {
			ackPayload := map[string]any{
				"stage_id":                 stage.ID.String(),
				"scoped_files":             effectiveCount,
				"min_changed_files":        minPhysical,
				"max_files_changed":        maxFiles,
				"add_scope_files_count":    len(addScopeFiles),
				"remove_scope_files_count": len(removeScopeFiles),
				"note":                     "override covers the declared-scope pre-check only; the implement stage re-evaluates max_files_changed against the real diff",
			}
			ackJSON, _ := json.Marshal(ackPayload)
			if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
				RunID:     stage.RunID,
				StageID:   &stage.ID,
				Timestamp: time.Now().UTC(),
				Category:  "plan_scope_cap_override_acknowledged",
				ActorKind: &systemKind,
				Payload:   ackJSON,
			}); err != nil {
				s.cfg.Logger.Error("audit append failed for scope-cap override",
					"run_id", stage.RunID, "stage_id", stage.ID, "error", err.Error())
			}
		}
		return true
	}

	if s.cfg.AuditRepo != nil {
		violationPayload, _ := json.Marshal(basePayload)
		if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
			RunID:     stage.RunID,
			StageID:   &stage.ID,
			Timestamp: time.Now().UTC(),
			Category:  "plan_violates_scope_cap",
			ActorKind: &systemKind,
			Payload:   violationPayload,
		}); err != nil {
			s.cfg.Logger.Error("audit append failed for scope-cap violation",
				"run_id", stage.RunID, "stage_id", stage.ID, "error", err.Error())
		}
	}

	msg := "effective scope.files (plan scope plus add_scope_files minus remove_scope_files) exceeds the implement " +
		"stage's max_files_changed; re-scope the plan, remove paths via remove_scope_files, or include " +
		"--override-scope-cap in the comment"
	if minPhysical > maxFiles {
		// Tell the operator up front that the override will NOT help here — its
		// minimum physical count already exceeds the cap, so the override path
		// would be refused too.
		msg += ". Note: this scope's minimum physical changed-file count (" + strconv.Itoa(minPhysical) +
			") already exceeds the cap, so --override-scope-cap will be refused — raise constraints.max_files_changed " +
			"through a governed spec change and start a fresh run, or drop declared paths via remove_scope_files"
	}
	s.writeError(w, r, http.StatusUnprocessableEntity, "plan_violates_scope_cap",
		msg,
		basePayload)
	return false
}

// checkPlanRequiredTests enforces the required-tests gate on plan-stage
// approvals (#2660). Returns true when the approval should proceed; returns
// false (having written a 422 and appended the matching audit entry) when
// the plan PROVABLY cannot satisfy the implement stage's
// `required_outcomes: [tests_added_or_updated]`.
//
// The gate fires only when ALL of:
//   - the implement stage requires tests_added_or_updated, AND
//   - the effective scope (plan scope.files ∪ add_scope_files ∖
//     remove_scope_files, the same set the cap gate counts) contains at
//     least one testable-source path, AND
//   - NO effective path is test-shaped (policy.IsTestPath).
//
// The second condition is load-bearing: a docs-only scope satisfies the
// outcome at implement via the #610 vacuous branch, so refusing it would be
// wrong.
//
// `--comment-only` in the comment is the escape for the one case the
// envelope admits — a comment-only correction to a .go file (the
// policy.DetectCommentOnlyGo exemption). Its posture is CONDITIONAL, mirroring
// --override-scope-cap (#2415): honored when every testable-source path in
// scope is a .go file, and REFUSED CATEGORICALLY (422
// plan_comment_only_override_unavailable) when any is not, because the
// exemption is Go-only and no override can authorize that landing. The honored
// path records that the override covers the PLAN gate only — the implement
// stage re-derives the verdict from the REAL diff.
//
// Fail-open matching its siblings: an unreadable run, an unresolved spec or
// implement stage, and an unresolved effective scope all skip the check, so a
// degraded backend can never brick the approval path.
func (s *Server) checkPlanRequiredTests(w http.ResponseWriter, r *http.Request, stage *run.Stage, comment string, addScopeFiles, removeScopeFiles []string) bool {
	ctx := r.Context()
	if s.cfg.RunRepo == nil {
		return true
	}
	runRow, err := s.cfg.RunRepo.GetRun(ctx, stage.RunID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "required-tests gate: get run failed",
			slog.String("run_id", stage.RunID.String()),
			slog.String("error", err.Error()))
		return true
	}
	outcomes, ok := s.resolveImplementRequiredOutcomes(ctx, runRow)
	if !ok {
		return true
	}
	required := false
	for _, o := range outcomes {
		if o == "tests_added_or_updated" {
			required = true
			break
		}
	}
	if !required {
		return true
	}

	paths, _, ok := s.effectiveScopePathSet(ctx, stage.RunID, addScopeFiles, removeScopeFiles)
	if !ok {
		return true
	}

	var testable, nonGo []string
	for _, p := range paths {
		if policy.IsTestPath(p) {
			// A declared test path satisfies the outcome by construction.
			return true
		}
		if policy.IsTestableSourcePath(p) {
			testable = append(testable, p)
			if !strings.HasSuffix(strings.ToLower(p), ".go") {
				nonGo = append(nonGo, p)
			}
		}
	}
	if len(testable) == 0 {
		// Docs/scripts/config only: the #610 vacuous branch satisfies the
		// outcome at implement, so refusing here would be wrong.
		return true
	}

	basePayload := map[string]any{
		"stage_id":              stage.ID.String(),
		"required_outcome":      "tests_added_or_updated",
		"scoped_files":          len(paths),
		"testable_source_files": len(testable),
		"non_go_source_files":   nonGo,
		"test_paths_declared":   0,
	}
	systemKind := audit.ActorKind("system")
	appendEntry := func(category string) {
		if s.cfg.AuditRepo == nil {
			return
		}
		payload, _ := json.Marshal(basePayload)
		if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
			RunID:     stage.RunID,
			StageID:   &stage.ID,
			Timestamp: time.Now().UTC(),
			Category:  category,
			ActorKind: &systemKind,
			Payload:   payload,
		}); err != nil {
			s.cfg.Logger.Error("audit append failed for required-tests gate",
				"run_id", stage.RunID, "stage_id", stage.ID, "category", category, "error", err.Error())
		}
	}

	if strings.Contains(comment, "--comment-only") {
		if len(nonGo) > 0 {
			// Categorical: the comment-only exemption is .go-only, so no
			// override can authorize a landing that changes another
			// language's source without a test.
			appendEntry("plan_comment_only_override_refused")
			s.writeError(w, r, http.StatusUnprocessableEntity, "plan_comment_only_override_unavailable",
				"--comment-only cannot authorize this landing. The comment-only exemption for "+
					"required_outcomes: tests_added_or_updated covers .go files ONLY, and this scope declares "+
					"non-.go testable source ("+strings.Join(nonGo, ", ")+"). Declare the test file you intend to "+
					"add via add_scope_files, or re-plan the change so the non-.go source is out of scope.",
				basePayload)
			return false
		}
		basePayload["note"] = "override covers the plan gate only; the implement stage re-derives the comment-only verdict from the real diff"
		appendEntry("plan_comment_only_override_acknowledged")
		return true
	}

	appendEntry("plan_missing_required_tests")
	s.writeError(w, r, http.StatusUnprocessableEntity, "plan_missing_required_tests",
		"the implement stage declares required_outcomes: tests_added_or_updated, but the effective scope "+
			"(plan scope.files plus add_scope_files minus remove_scope_files) declares no test file while it "+
			"does declare testable source. The implement stage would fail this constraint as category-B after a "+
			"full run. Declare the test file you intend to add via add_scope_files, or — for a comment-only "+
			"correction to a .go file — approve with --comment-only in the comment.",
		basePayload)
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
	// The escalation resolver is REQUIRED at construction (E53.4 / #2227): a
	// delegated action must be refused when a fired escalation's
	// `max_autonomy` ceiling clamps its class, and an evaluator without a
	// resolver would act on the unclamped matrix. A constructor error reuses
	// this function's existing fail-closed 500.
	ev, err := delegation.NewEvaluator(s.cfg.RunRepo, s.cfg.ConcernRepo, s.cfg.AuditRepo, s.escalationResolver())
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"delegation evaluator could not be built", map[string]any{"action": action, "error": err.Error()})
		return "", false
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
	// Resolve the SPEC deploy stage for THIS stage row through the shared
	// selection chokepoint (E23.19 / #2642) — the SAME resolver the record and
	// trigger reach, so the gate cannot admit against one stage while they key
	// on another. On a multi-deploy-stage workflow this keys on the stage
	// actually being approved (by its deploy ordinal), not the first deploy
	// stage. The typed reason preserves each precondition's distinct refusal
	// message (operator binding condition 3).
	deployStage, reason, rerr := s.resolveDeploySpecStage(ctx, runRow, stage)
	if s.refuseDeployForResolveReason(w, r, stage, runRow, reason, rerr) {
		return false
	}

	// Collect the pre-flight constraints. NUANCE (#1384 condition 1): a
	// deploy stage that parses but declares NO pre-flight constraints passes
	// — there is nothing to enforce, and fail-closed targets the
	// can't-evaluate path, not the nothing-declared case.
	var (
		changeFreeze  bool
		requiredUp    []string
		hasConstraint bool
	)
	// allowed_environments uses the shared last-wins fold so the record-side
	// membership re-check (deployApprovedEnvironment) enforces the exact
	// allow-list this gate admits against (E23.18 / #2324). The change_freeze /
	// required_upstream arms stay inline — their folds are gate-only.
	allowedEnvs := lastWinsAllowedEnvironments(deployStage)
	if len(allowedEnvs) > 0 {
		hasConstraint = true
	}
	for _, c := range deployStage.Constraints {
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

// deployStageResolveReason classifies why resolveDeploySpecStage could not
// return a deploy spec stage (E23.19 / #2642). It exists so the deploy GATE
// (checkDeployPreflight) can keep its per-precondition refusal messages while
// the record (deploySpecStageForStage) and trigger (resolveDeployDelegate)
// sides collapse every non-OK reason to one fail-closed outcome. Routing the
// selection through a single chokepoint WITHOUT a typed reason would silently
// coarsen the gate's diagnostics (operator binding condition 3); the typed
// reason keeps one selection implementation AND the gate still tells the
// operator WHICH precondition failed.
type deployStageResolveReason int

const (
	deployStageResolveOK               deployStageResolveReason = iota
	deployStageResolveNoSpec                                    // the run carries no cached workflow spec
	deployStageResolveSpecParse                                 // the cached spec does not parse
	deployStageResolveWorkflowMissing                           // the run's workflow is not in its cached spec
	deployStageResolveRowsUnavailable                           // the run's stage rows could not be listed
	deployStageResolveOrdinalUnmatched                          // no deploy spec stage at the stage row's deploy ordinal
)

// refuseDeployForResolveReason maps a resolveDeploySpecStage reason to the
// deploy gate's fail-closed refusal (E23.19 / #2642). It returns false ONLY for
// deployStageResolveOK — meaning the caller may proceed to constraint
// collection with the resolved spec stage; for every other reason it writes the
// precondition's distinct 422 refusal (operator binding condition 3) and
// returns true so checkDeployPreflight refuses.
//
// The trailing default arm is load-bearing, not decorative: a future reason
// constant added to resolveDeploySpecStage without a matching case here must
// STILL refuse rather than proceed with a zero-value spec.Stage. Deleting it
// stops this function compiling (missing return) — the fail-closed posture is
// STRUCTURAL (ADR-038), not contingent on a future editor extending the switch.
func (s *Server) refuseDeployForResolveReason(w http.ResponseWriter, r *http.Request, stage *run.Stage, runRow *run.Run, reason deployStageResolveReason, rerr error) bool {
	switch reason {
	case deployStageResolveOK:
		return false
	case deployStageResolveNoSpec:
		s.refuseDeploy(w, r, stage, "deploy_preflight_unevaluable",
			"deploy pre-flight cannot be evaluated: the run carries no cached workflow spec; an unverifiable deploy is denied (fail-closed)", nil)
		return true
	case deployStageResolveSpecParse:
		s.refuseDeploy(w, r, stage, "deploy_preflight_unevaluable",
			"deploy pre-flight cannot be evaluated: the cached workflow spec does not parse; an unverifiable deploy is denied (fail-closed)",
			map[string]any{"error": rerr.Error()})
		return true
	case deployStageResolveWorkflowMissing:
		s.refuseDeploy(w, r, stage, "deploy_preflight_unevaluable",
			"deploy pre-flight cannot be evaluated: the run's workflow is not in its cached spec; an unverifiable deploy is denied (fail-closed)",
			map[string]any{"workflow_id": runRow.WorkflowID})
		return true
	case deployStageResolveRowsUnavailable:
		s.refuseDeploy(w, r, stage, "deploy_preflight_unevaluable",
			"deploy pre-flight cannot be evaluated: the run's stage rows could not be listed; an unverifiable deploy is denied (fail-closed)",
			map[string]any{"error": rerr.Error()})
		return true
	case deployStageResolveOrdinalUnmatched:
		s.refuseDeploy(w, r, stage, "deploy_preflight_unevaluable",
			"deploy pre-flight cannot be evaluated: no deploy stage in the run's workflow matches this stage row's deploy position; an unverifiable deploy is denied (fail-closed)", nil)
		return true
	default:
		s.refuseDeploy(w, r, stage, "deploy_preflight_unevaluable",
			fmt.Sprintf("deploy pre-flight cannot be evaluated: unrecognized deploy-stage resolve reason %d; an unverifiable deploy is denied (fail-closed)", reason), nil)
		return true
	}
}

// resolveDeploySpecStage is the ONE deploy-stage selection chokepoint the gate
// (checkDeployPreflight), the record (deploySpecStageForStage), and the trigger
// (resolveDeployDelegate) all reach, so none can key on a different deploy stage
// than the others (E23.19 / #2642). It parses the run's cached spec, looks up
// the run's workflow, lists the run's stage rows, and resolves the SPEC deploy
// stage matching `stage`'s deploy ordinal via deployStageForRunStage.
//
// The returned reason lets the gate keep distinct refusal messages; the record
// and trigger treat any non-OK reason as fail-closed. The error is non-nil only
// for SpecParse and RowsUnavailable, carrying the underlying error for the
// gate's refusal detail.
func (s *Server) resolveDeploySpecStage(ctx context.Context, runRow *run.Run, stage *run.Stage) (spec.Stage, deployStageResolveReason, error) {
	if len(runRow.WorkflowSpec) == 0 {
		return spec.Stage{}, deployStageResolveNoSpec, nil
	}
	parsed, err := spec.ParseBytes(runRow.WorkflowSpec)
	if err != nil {
		return spec.Stage{}, deployStageResolveSpecParse, err
	}
	wf, ok := parsed.Workflows[runRow.WorkflowID]
	if !ok {
		return spec.Stage{}, deployStageResolveWorkflowMissing, nil
	}
	rows, err := s.cfg.RunRepo.ListStagesForRun(ctx, stage.RunID)
	if err != nil {
		return spec.Stage{}, deployStageResolveRowsUnavailable, err
	}
	st, ok := deployStageForRunStage(wf, rows, stage)
	if !ok {
		return spec.Stage{}, deployStageResolveOrdinalUnmatched, nil
	}
	return st, deployStageResolveOK, nil
}

// deployStageForRunStage selects the SPEC deploy stage that corresponds to a
// specific persisted stage ROW, keyed on the row's DEPLOY ORDINAL among the
// run's stage rows (E23.19 / #2642). This replaces the former first-match
// firstDeployStage: a workflow may legally declare more than one deploy stage
// (the stage `type` enum includes deploy and $defs/workflow/properties/stages
// is an unconstrained array), and first-match gated and labelled EVERY deploy
// against the FIRST deploy stage — so a legal second deploy stage was checked
// against the wrong allowed_environments and mislabelled.
//
// The key is the deploy ORDINAL, not stage.Sequence-as-spec-index, because two
// production paths create a run's stages from a plan-FILTERED subset of the
// spec (webhook/dispatcher.go CI-retry children and server/recover.go recovery
// children, both via webhook.FilterOutPlanStages), which renumbers Sequence
// densely from 0 over the subset. FilterOutPlanStages drops ONLY plan stages
// and preserves relative order, so the k-th deploy ROW is always the k-th
// deploy SPEC stage — while an index-keyed lookup would land on a non-deploy
// spec stage and refuse EVERY deploy on a retry/recovery child (fail-closed,
// but a functional regression). webhook.CreateStagesFromSpec creates exactly
// one row per spec stage in spec order, establishing the row↔spec-order
// correspondence the ordinal relies on.
//
// It sorts a COPY of rows by Sequence itself (ties broken by stage ID for a
// total, stable order) rather than trusting the repo's ordering: the in-package
// fake ListStagesForRun iterates a Go map, whose order is randomized, so
// depending on repo order would make selection flaky.
//
// Returns ok=false on: a nil stage; a non-deploy stage; a stage absent from
// rows; or a workflow declaring fewer than k+1 deploy stages (a row/spec
// disagreement the caller must fail closed on).
func deployStageForRunStage(wf spec.Workflow, rows []*run.Stage, stage *run.Stage) (spec.Stage, bool) {
	if stage == nil || stage.Type != run.StageTypeDeploy {
		return spec.Stage{}, false
	}
	sorted := make([]*run.Stage, len(rows))
	copy(sorted, rows)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Sequence != sorted[j].Sequence {
			return sorted[i].Sequence < sorted[j].Sequence
		}
		return sorted[i].ID.String() < sorted[j].ID.String()
	})
	// k = the number of deploy-typed rows preceding this stage in sorted order
	// (its deploy ordinal). The walk also proves the stage is present in rows.
	k := 0
	found := false
	for _, st := range sorted {
		if st.ID == stage.ID {
			found = true
			break
		}
		if st.Type == run.StageTypeDeploy {
			k++
		}
	}
	if !found {
		return spec.Stage{}, false
	}
	// Return the k-th deploy stage in SPEC order.
	seen := 0
	for _, sp := range wf.Stages {
		if sp.Type != spec.StageTypeDeploy {
			continue
		}
		if seen == k {
			return sp, true
		}
		seen++
	}
	return spec.Stage{}, false
}

// lastWinsAllowedEnvironments folds a deploy stage's allowed_environments
// pre-flight constraint the way the approval gate admits an environment: the
// LAST non-empty allowed_environments entry across the stage's constraints wins
// (E23.18 / #2324). This is a literal transcription of checkDeployPreflight's
// in-loop `allowedEnvs = c.AllowedEnvironments` assignment, extracted so the
// record-side membership re-check enforces the SAME allow-list the gate did and
// cannot drift from it. Returns nil when no constraint declares one.
func lastWinsAllowedEnvironments(st spec.Stage) []string {
	var allowed []string
	for _, c := range st.Constraints {
		if len(c.AllowedEnvironments) > 0 {
			allowed = c.AllowedEnvironments
		}
	}
	return allowed
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
	if s.cfg.StageCheckRepo == nil {
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
	// A nil RequiredChecksSnapshot now resolves through aggregateCIGreen
	// returning nil (unknown, #2497), which `g != nil && *g` maps to false
	// — the same fail-closed verdict the removed `== nil` guard gave, but
	// routed through the single chokepoint so the aggregate's nil arm is
	// reachable from a caller and counterfactually testable.
	g := aggregateCIGreen(evalRun.RequiredChecksSnapshot, checks)
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
