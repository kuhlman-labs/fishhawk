package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/scopeamendment"
)

// The three scope-completeness audit kinds (#1231) are internal audit
// kinds, NOT issue-comment surfaces. They have no Notifier methods and
// nothing in `issuecomment` posts them — see docs/issue-comment-surfaces.md.
const (
	// CategoryScopeCompletenessParked is written by the pull-request park
	// handler (server/pullrequest.go::parkScopeCompletenessStage) when the
	// runner reports {outcome:"scope_park"}: the implement stage's ONLY
	// committed-tree gate failure was a shortfall of EITHER class — the
	// missing-declared-scope-file check (#1151) or the binding-assertion
	// check (#1171, #2501) — the verified commit is held on the run branch,
	// and the stage parked in awaiting_scope_decision. Payload carries
	// {run_id, stage_id, branch, head_sha, base_sha, verified_tree_sha,
	// missing_paths, unsatisfied_assertions, auth_method}.
	CategoryScopeCompletenessParked = "scope_completeness_parked"

	// CategoryScopeCompletenessExempted is written by the decision handler
	// on an `exempt` decision: the operator accepted the already-committed
	// tree, so the stage returns to dispatch and the held commit's PR is
	// opened with NO agent re-run. Payload carries {run_id, stage_id,
	// decision, reason, decided_by, held_commit_sha, run_branch,
	// verified_tree_sha, missing_paths, unsatisfied_assertions,
	// gate_evidence}. The gate_evidence field reuses the #1153 channel so a
	// downstream implement-review gate sees the shortfall (of either class)
	// was operator-exempted rather than re-failing on it.
	CategoryScopeCompletenessExempted = "scope_completeness_exempted"

	// CategoryScopeCompletenessFailed is written by the decision handler on
	// a `fail` decision: the operator rejected the exemption, so the stage
	// fails category-B (today's restore path). Payload carries {run_id,
	// stage_id, decision, reason, decided_by, missing_paths,
	// unsatisfied_assertions}.
	CategoryScopeCompletenessFailed = "scope_completeness_failed"

	// CategoryScopeCompletenessAmended is written by the decision handler on
	// an `amend` decision (#2591): the operator WIDENED the parked implement
	// stage's effective scope with a pre-approved #961 scope-amendment row and
	// resumed the stage, so the agent re-runs against the wider scope instead
	// of the pass failing category-B. Payload carries the exempted/failed keys
	// plus {paths, amendment_id, owning_slices, owner_attribution_unresolved,
	// owner_unresolved_paths, acknowledged_owner_unresolved}.
	//
	// It is ALSO an INVALIDATOR of a stale `exempted` decision: prompt.go's
	// resolveHeldCommitExemption walks this category so an amend DEMOTES an
	// earlier exempt on the same stage (an amend means "do NOT ship this tree",
	// the #2630 sticky-re-entry shape).
	CategoryScopeCompletenessAmended = "scope_completeness_amended"
)

// unsatisfiedAssertion is one operator-declared binding assertion (#1171) the
// held commit did not satisfy — the SECOND scope-completeness park shortfall
// class (#2501).
//
// CROSS-MODULE WIRE CONTRACT: the json tags (type/path/literal) MUST stay
// byte-identical to the runner's upload.BindingAssertionReport
// (runner/internal/upload/upload.go) and to run.UnsatisfiedAssertion, the
// durable JSONB shape. Same independent-struct-by-tag convention as
// scopeExemption (#1229) and bindingAssertion (#1171).
type unsatisfiedAssertion struct {
	Type    string `json:"type"`
	Path    string `json:"path"`
	Literal string `json:"literal"`
}

// toRunUnsatisfiedAssertions maps the decoded park-report entries into the
// durable domain shape. nil in, nil out — a missing-scope-class park's park
// payload is byte-identical to the pre-#2501 row.
func toRunUnsatisfiedAssertions(in []unsatisfiedAssertion) []run.UnsatisfiedAssertion {
	if len(in) == 0 {
		return nil
	}
	out := make([]run.UnsatisfiedAssertion, 0, len(in))
	for _, a := range in {
		out = append(out, run.UnsatisfiedAssertion{Type: a.Type, Path: a.Path, Literal: a.Literal})
	}
	return out
}

// scopeCompletenessDecisionRequest is the JSON body of POST
// /v0/runs/{run_id}/scope-completeness/decision.
type scopeCompletenessDecisionRequest struct {
	Decision string `json:"decision"` // exempt | amend | fail
	Reason   string `json:"reason"`
	// Paths are the files an `amend` decision folds into the parked implement
	// stage's effective scope (#2591). Required and non-empty on `amend`,
	// rejected on any other decision. Same {path, operation} vocabulary and
	// the same scopeamendment.ValidatePaths validation as a #961 mid-stage
	// amendment, because an amend IS one — a pre-approved row on this stage.
	Paths []scopeamendment.PathEntry `json:"paths,omitempty"`
	// AcknowledgeOwnerUnresolved re-admits an `amend` whose owner-slice guard
	// could not RESOLVE ownership for one or more submitted paths (#2591
	// binding condition 1). The guard fails CLOSED by default — unknown
	// ownership is exactly the case in which a sibling slice might already hold
	// divergent edits to the file — so an unresolved path refuses the amend 409
	// amend_owner_attribution_unresolved. Setting this to true makes the
	// operator take that risk KNOWINGLY: the amend proceeds and the
	// acknowledgement is recorded on both the response and the
	// scope_completeness_amended audit entry, so it is an audited decision
	// rather than a silent default. It never relaxes the RESOLVED-and-started
	// refusal (amend_refused_owner_slice_active).
	AcknowledgeOwnerUnresolved bool `json:"acknowledge_owner_unresolved,omitempty"`
}

// scopeCompletenessDecisionResponse echoes the resolved decision back to
// the operator. On `exempt`, HeldCommitSHA / RunBranch carry the commit the
// follow-on push_and_open_pr dispatch opens the PR from (no agent re-run).
type scopeCompletenessDecisionResponse struct {
	StageID       uuid.UUID `json:"stage_id"`
	Decision      string    `json:"decision"`
	State         string    `json:"state"`
	HeldCommitSHA string    `json:"held_commit_sha,omitempty"`
	RunBranch     string    `json:"run_branch,omitempty"`
	// MissingPaths / UnsatisfiedAssertions / BuildRequiredPaths echo the resolved
	// park's shortfall (#2501, third class #2548) so the operator's client sees
	// WHICH class it decided without a second audit read. Exactly one is
	// populated. BuildRequiredPaths can only appear on a `fail` response — the
	// exempt path refuses that class outright.
	MissingPaths          []string                   `json:"missing_paths,omitempty"`
	UnsatisfiedAssertions []run.UnsatisfiedAssertion `json:"unsatisfied_assertions,omitempty"`
	BuildRequiredPaths    []string                   `json:"build_required_paths,omitempty"`
	// OwningSlices attributes each build-required path to the sibling slice that
	// declares it, so the operator's `fail` response already names the boundary
	// to correct before re-running. On an `amend` response it names the RESOLVED
	// owning slice of each amended path — the sibling that will see the same file
	// edited on another branch.
	OwningSlices []run.BuildRequiredOwner `json:"owning_slices,omitempty"`

	// --- `amend` only (#2591) ---

	// AmendedPaths are the validated, normalized paths folded into the parked
	// stage's effective scope, each keeping its declared operation.
	AmendedPaths []scopeamendment.PathEntry `json:"amended_paths,omitempty"`
	// AmendmentID is the pre-approved #961 scope-amendment row the widening
	// landed on. Stable across a retry of a failed amend (the row is REUSED,
	// never duplicated) so the budget below cannot silently drain.
	AmendmentID string `json:"amendment_id,omitempty"`
	// OwnerAttributionUnresolved is true when the owner-slice guard could not
	// establish ownership for at least one submitted path. It can only be true
	// alongside AcknowledgedOwnerUnresolved — the unresolved arm otherwise
	// refuses the amend outright (409 amend_owner_attribution_unresolved).
	OwnerAttributionUnresolved bool `json:"owner_attribution_unresolved,omitempty"`
	// OwnerUnresolvedPaths names each path whose ownership could not be
	// resolved, with the reason, so the acknowledgement is auditable in detail.
	OwnerUnresolvedPaths []amendUnresolvedOwner `json:"owner_unresolved_paths,omitempty"`
	// AcknowledgedOwnerUnresolved echoes the operator's explicit
	// acknowledge_owner_unresolved, so the response records that the relaxation
	// was a deliberate operator decision.
	AcknowledgedOwnerUnresolved bool `json:"acknowledged_owner_unresolved,omitempty"`
	// AgentAmendmentBudgetRemaining is how many of the stage's #961 mid-stage
	// amendment slots the RE-RUN agent still has. An operator amend consumes one
	// (maxScopeAmendmentsPerStage counts ROWS), so the cost is surfaced rather
	// than hidden.
	AgentAmendmentBudgetRemaining int `json:"agent_amendment_budget_remaining,omitempty"`
}

// handleDecideScopeCompleteness implements POST
// /v0/runs/{run_id}/scope-completeness/decision (#1231).
//
// Auth: operator bearer/session ONLY, requiring write:stages for token
// callers — the same posture as the scope-amendment decision endpoint.
// Run-bound fhm_ tokens are rejected outright (403 self_decision): the
// implement agent that produced the parked commit must never decide its
// own exemption; the decision is an operator action.
//
// The implement stage MUST currently be parked in awaiting_scope_decision
// (409 otherwise). On `exempt` the stage resumes to PENDING and the run is
// advanced, so the orchestrator re-dispatches it and the runner opens the
// held commit's PR with no agent re-run (#2501); on `fail` the stage drops
// to today's category-B restore path.
func (s *Server) handleDecideScopeCompleteness(w http.ResponseWriter, r *http.Request) {
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "scope_completeness_unconfigured",
			"scope-completeness decision endpoint requires run and audit repositories", nil)
		return
	}

	id := IdentityFrom(r.Context())
	if id.IsAnonymous() {
		s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
			"an authenticated token is required", nil)
		return
	}
	if _, runBound := runBoundTokenRunID(id); runBound {
		s.writeError(w, r, http.StatusForbidden, "self_decision",
			"a run-bound agent token may not decide a scope-completeness park; the decision is an operator action",
			nil)
		return
	}
	if id.TokenID != "" && !hasScope(id, "write:stages") {
		s.writeError(w, r, http.StatusForbidden, "insufficient_scope",
			"token is missing required scope: write:stages",
			map[string]any{"required_scope": "write:stages"})
		return
	}

	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}

	var reqBody scopeCompletenessDecisionRequest
	if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"request body must be valid JSON {decision, reason}",
			map[string]any{"error": err.Error()})
		return
	}
	switch reqBody.Decision {
	case "exempt", "amend", "fail":
	default:
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"decision must be \"exempt\", \"amend\", or \"fail\"",
			map[string]any{"field": "decision", "got": reqBody.Decision})
		return
	}
	// paths belong to `amend` alone: accepting them on exempt/fail would let a
	// caller believe a widening was recorded when nothing folded them anywhere.
	if reqBody.Decision != "amend" && len(reqBody.Paths) > 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"paths may only be supplied with decision \"amend\"",
			map[string]any{"field": "paths", "decision": reqBody.Decision})
		return
	}
	// acknowledge_owner_unresolved belongs to `amend` alone for the same reason:
	// only the amend arm runs an owner-slice guard, so on exempt/fail the flag
	// would be silently ignored and the caller would believe they had made an
	// audited risk decision that was never recorded anywhere.
	if reqBody.Decision != "amend" && reqBody.AcknowledgeOwnerUnresolved {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"acknowledge_owner_unresolved may only be supplied with decision \"amend\"",
			map[string]any{"field": "acknowledge_owner_unresolved", "decision": reqBody.Decision})
		return
	}
	if strings.TrimSpace(reqBody.Reason) == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"reason is required: state why the parked scope-completeness shortfall is exempted or failed",
			map[string]any{"field": "reason"})
		return
	}

	runRow, stage, err := s.resolveParkedScopeStage(r, runID)
	if err != nil {
		if errors.Is(err, run.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "run_not_found",
				"no run with that id", nil)
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"resolve parked stage failed", map[string]any{"error": err.Error()})
		return
	}
	if stage == nil {
		s.writeError(w, r, http.StatusConflict, "stage_not_parked",
			"the implement stage is not parked awaiting a scope-completeness decision",
			nil)
		return
	}

	switch reqBody.Decision {
	case "exempt":
		s.exemptScopeCompleteness(w, r, runID, stage, reqBody.Reason, id.Subject)
	case "amend":
		s.amendScopeCompleteness(w, r, runID, runRow, stage, reqBody, id.Subject)
	default:
		s.failScopeCompleteness(w, r, runID, stage, reqBody.Reason, id.Subject)
	}
}

// resolveParkedScopeStage returns the run row and its implement stage when that
// stage is currently parked in awaiting_scope_decision; a nil STAGE when the run
// exists but no implement stage is parked (→ 409). run.ErrNotFound when the run
// itself doesn't exist.
//
// The run row is RETURNED rather than discarded because the `amend` arm's
// owner-slice guard needs it (DecomposedFrom / SliceIndex) — re-fetching it
// there would be a second round-trip whose error branch is unreachable anyway,
// since any GetRun failure is already refused here.
func (s *Server) resolveParkedScopeStage(r *http.Request, runID uuid.UUID) (*run.Run, *run.Stage, error) {
	runRow, err := s.cfg.RunRepo.GetRun(r.Context(), runID)
	if err != nil {
		return nil, nil, err
	}
	stages, err := s.cfg.RunRepo.ListStagesForRun(r.Context(), runID)
	if err != nil {
		return nil, nil, err
	}
	for _, st := range stages {
		if st.Type == run.StageTypeImplement && st.State == run.StageStateAwaitingScopeDecision {
			return runRow, st, nil
		}
	}
	return runRow, nil, nil
}

// exemptScopeCompleteness resolves an `exempt` decision: it resumes the
// parked implement stage to PENDING and advances the run, so the orchestrator
// re-dispatches the stage uniformly (a local run parks at
// awaiting_host_dispatch for the operator's spawn; a github_actions run
// dispatches the workflow). The re-dispatch is agent-FREE: the prompt response
// carries open_pr_from_held_commit / held_commit_sha / held_commit_branch for
// an exempt-resolved park (prompt.go::resolveHeldCommitExemption), and the
// runner short-circuits to openHeldCommitPR before the agent invoker is wired.
//
// Pending, NOT Running (#2501): host_dispatch.go's admission switch accepts
// only {pending, awaiting_host_dispatch} and refuses a `running` stage with
// dispatch_not_admissible, and no orchestrator path dispatches a running stage
// either — so the pre-#2501 Running transition was a dead end on BOTH runner
// kinds and no runner ever spawned to open the held commit's PR.
//
// ORDERING IS LOAD-BEARING (#2501): the scope_completeness_exempted audit entry
// is appended FIRST, and a failed append REFUSES the decision (500) instead of
// logging on. That entry is not a record of the decision — since #2501 it IS the
// functional authorization proof the prompt emission gate reads
// (prompt.go::resolveHeldCommitExemption), so:
//
//   - Appending it before the stage leaves awaiting_scope_decision closes the
//     window in which the transition + Advance below could spawn a runner whose
//     prompt fetch raced the append, read the older `parked` entry, emitted no
//     held-commit fields, and re-invoked the agent — the ~$23/~50-minute re-run
//     #2501 exists to eliminate.
//   - Refusing on an append failure keeps the same property when the chain write
//     fails outright: a stage the gate can never prove exempt is left parked (the
//     operator can simply re-decide) rather than dispatched into a guaranteed
//     agent re-run with no exemption recorded in the audit chain.
//
// failScopeCompleteness keeps its best-effort append: nothing downstream reads
// the `failed` entry as authorization, and its stage transition is terminal.
func (s *Server) exemptScopeCompleteness(w http.ResponseWriter, r *http.Request, runID uuid.UUID,
	stage *run.Stage, reason, decidedBy string) {
	// #2548 EXEMPT REFUSAL for the build-required class. This runs FIRST — before
	// the load-bearing scope_completeness_exempted append, before the stage
	// leaves awaiting_scope_decision, before any Advance — because exempt resumes
	// the stage and drives a PR-open from the held commit, whose committed tree
	// is RED BY CONSTRUCTION for this class (that redness IS the shortfall the
	// park records). Opening a PR from it is the one outcome this design says
	// must never happen. The stage MUST remain parked and NO exempted entry may
	// be appended, so an operator can still resolve it with the only admissible
	// decision: `fail`, then correct the decomposition and re-run — the held
	// commit stays on the slice branch either way.
	if park := stage.ScopeCompletenessPark; park != nil && len(park.BuildRequiredPaths) > 0 {
		s.writeError(w, r, http.StatusConflict, "exempt_refused_build_required",
			"this park is a build-required scope-drift shortfall on a decomposition child: the held commit's committed tree is red because a sibling slice owns a build-required file, so exempting it would open a PR from a red tree. "+
				"The only admissible decision is \"fail\"; then correct the decomposition (move the coupled file into this slice, or merge the slices) and re-run. The held commit stays on the slice branch.",
			map[string]any{
				"shortfall_class":      ShortfallClassBuildRequired,
				"build_required_paths": park.BuildRequiredPaths,
				"owning_slices":        park.OwningSlices,
				"admissible_decisions": []string{"fail"},
			})
		return
	}
	if err := s.writeScopeCompletenessDecisionAudit(r, runID, stage,
		CategoryScopeCompletenessExempted, reason, decidedBy, nil); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "exemption_unrecorded",
			"the exemption could not be recorded in the audit chain; the stage is left parked — re-POST the decision",
			map[string]any{"error": err.Error()})
		return
	}

	if _, err := s.cfg.RunRepo.TransitionStage(r.Context(), stage.ID, run.StageStatePending, nil); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"resume parked stage failed", map[string]any{"error": err.Error()})
		return
	}

	if s.cfg.Orchestrator != nil {
		if _, err := s.cfg.Orchestrator.Advance(r.Context(), runID); err != nil {
			s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
				"scope-completeness exempt decision: orchestrator advance failed",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stage.ID.String()),
				slog.String("error", err.Error()))
		}
	}

	s.notifyStatusUpdate(r.Context(), runID, "scope_exempted")

	resp := scopeCompletenessDecisionResponse{
		StageID:  stage.ID,
		Decision: "exempt",
		State:    string(run.StageStatePending),
	}
	if park := stage.ScopeCompletenessPark; park != nil {
		resp.HeldCommitSHA = park.HeldCommitSHA
		resp.RunBranch = park.RunBranch
		resp.MissingPaths = park.MissingPaths
		resp.UnsatisfiedAssertions = park.UnsatisfiedAssertions
	}
	s.writeJSON(w, r, http.StatusOK, resp)
}

// failScopeCompleteness resolves a `fail` decision: it fails the parked
// implement stage category-B (today's restore path) and appends a
// scope_completeness_failed audit entry, then advances the run so the
// orchestrator walks it forward — mirroring failPullRequestStage.
func (s *Server) failScopeCompleteness(w http.ResponseWriter, r *http.Request, runID uuid.UUID,
	stage *run.Stage, reason, decidedBy string) {
	if _, err := run.FailStage(r.Context(), s.cfg.RunRepo, stage.ID, run.FailureB, reason); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"fail parked stage failed", map[string]any{"error": err.Error()})
		return
	}

	if s.cfg.Orchestrator != nil {
		if _, err := s.cfg.Orchestrator.Advance(r.Context(), runID); err != nil {
			s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
				"scope-completeness fail decision: orchestrator advance failed",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stage.ID.String()),
				slog.String("error", err.Error()))
		}
	}

	// Best-effort, deliberately (see exemptScopeCompleteness): the `failed` entry
	// authorizes nothing downstream and the stage is already terminally failed.
	if err := s.writeScopeCompletenessDecisionAudit(r, runID, stage,
		CategoryScopeCompletenessFailed, reason, decidedBy, nil); err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			CategoryScopeCompletenessFailed+" audit append failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stage.ID.String()),
			slog.String("error", err.Error()))
	}
	s.notifyStatusUpdate(r.Context(), runID, "scope_failed")

	resp := scopeCompletenessDecisionResponse{
		StageID:  stage.ID,
		Decision: "fail",
		State:    string(run.StageStateFailed),
	}
	// #2548: echo the decided class so the operator's client sees which shortfall
	// it failed — and, for the build-required class, which sibling slice owns
	// each coupled path, i.e. the boundary to correct before re-running.
	if park := stage.ScopeCompletenessPark; park != nil {
		resp.MissingPaths = park.MissingPaths
		resp.UnsatisfiedAssertions = park.UnsatisfiedAssertions
		resp.BuildRequiredPaths = park.BuildRequiredPaths
		resp.OwningSlices = park.OwningSlices
	}
	s.writeJSON(w, r, http.StatusOK, resp)
}

// writeScopeCompletenessDecisionAudit appends the exempted/failed/amended
// decision audit entry, pinning the operator's reason and the parked
// held-commit coordinates into the chain. It RETURNS the append error rather
// than swallowing it (#2501): each caller decides how load-bearing its entry is
// — exemptScopeCompleteness refuses the decision (the entry is the emission
// gate's authorization proof), amendScopeCompleteness refuses it too (a stage
// whose widening the chain never recorded must never be dispatched),
// failScopeCompleteness logs and continues.
//
// extra carries the caller-specific payload keys and MUST be nil for the
// exempted/failed categories: those two payload key sets are a wire contract
// #2548 already promised not to touch, and #2591 keeps that promise the same
// way — by ADDING keys only on the new category's entry.
func (s *Server) writeScopeCompletenessDecisionAudit(r *http.Request, runID uuid.UUID,
	stage *run.Stage, category, reason, decidedBy string, extra map[string]any) error {
	actorKind := actorKindForSubject(decidedBy)
	stageID := stage.ID
	payloadMap := map[string]any{
		"run_id":     runID.String(),
		"stage_id":   stageID.String(),
		"decision":   strings.TrimPrefix(category, "scope_completeness_"),
		"reason":     reason,
		"decided_by": decidedBy,
	}
	if park := stage.ScopeCompletenessPark; park != nil {
		payloadMap["missing_paths"] = park.MissingPaths
		// #2501: carry the assertion-class shortfall beside missing_paths on
		// BOTH the exempted and failed entries.
		payloadMap["unsatisfied_assertions"] = park.UnsatisfiedAssertions
		// #2548: carry the build-required class + its sibling-slice attribution
		// too. In practice only the `failed` entry can carry it — the exempt path
		// refuses this class before reaching here — which is exactly where the
		// operator's record of WHICH boundary was cut belongs.
		//
		// GATED on a non-empty class so the two OLDER park classes' entries stay
		// BYTE-IDENTICAL: writing these unconditionally added two JSON-null keys
		// to every missing-paths / binding-assertion exempted+failed payload, a
		// wire-visible change on paths #2548 promised not to touch.
		if len(park.BuildRequiredPaths) > 0 {
			payloadMap["build_required_paths"] = park.BuildRequiredPaths
			payloadMap["owning_slices"] = park.OwningSlices
		}
		if category == CategoryScopeCompletenessExempted {
			payloadMap["held_commit_sha"] = park.HeldCommitSHA
			payloadMap["run_branch"] = park.RunBranch
			payloadMap["verified_tree_sha"] = park.VerifiedTreeSHA
			// Reuse the #1153 gate_evidence channel so a downstream
			// implement-review gate reads the missing-file shortfall as
			// operator-exempted rather than re-failing on it.
			payloadMap["gate_evidence"] = CategoryScopeCompletenessExempted
		}
	}
	for k, v := range extra {
		payloadMap[k] = v
	}
	payload, _ := json.Marshal(payloadMap)
	if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:        runID,
		StageID:      &stageID,
		Timestamp:    time.Now().UTC(),
		Category:     category,
		ActorKind:    &actorKind,
		ActorSubject: &decidedBy,
		Payload:      payload,
	}); err != nil {
		return err
	}
	return nil
}
