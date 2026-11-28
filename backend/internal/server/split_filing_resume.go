package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/dberr"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Operator resume verb for a split-proposal child filing (#3925, E50.25):
// POST /v0/runs/{run_id}/split-filing/resume.
//
// The on-approval split filing runs DETACHED (split_filing_detach.go). One that
// stopped part-way — budget expiry, a provider or audit error, a process exit —
// leaves durable per-phase work_item_filed markers and NO split_children_filed
// completion marker. This verb re-enters the same hook, which files only the
// phases with no durable marker. It adds no authority beyond completing an
// approval the plan gate already recorded: it files only from the run's OWN
// plan stage, only when that stage was approved at a gate with a recorded
// approve decision, and only from that stage's own newest plan artifact when
// the artifact was created no later than the stage's EndedAt — the transition
// that actually succeeded the plan stage (AA1). Approve rows never move that
// binding; its two-clock residual is documented on loadSplitPlanForStage.
// Every refusal precedes any write and names its remedy.
//
// A detached filing that aborts writes no terminal marker, so a missing
// split_children_filed entry is NOT proof of progress. The longest a caller
// waits is the reported budget_seconds; the probe is this call itself: 409
// split_filing_in_progress means a filing for the run still holds the registry.
//
// Children filed BY HAND carry no SplitChildKey idempotency marker and no
// work_item_filed record, so a resume files those phases again.

// splitFilingResumeRequestedCategory is the INTERNAL attribution entry written
// for every ADMITTED resume (202 resuming or 200 completed) before any launch.
// It is not an issue-comment surface and has no Notifier method; its payload
// carries only this run's ids, the artifact id and the actor subject, so it is
// run-bound readable.
const splitFilingResumeRequestedCategory = "split_filing_resume_requested"

// Resume outcomes recorded in the attribution entry and the response state.
const (
	splitFilingResumeStateResuming  = "resuming"
	splitFilingResumeStateCompleted = "completed"
)

// Error codes the resume verb answers with.
const (
	splitFilingResumeCodeUnconfigured   = "split_filing_resume_unconfigured"
	splitFilingResumeCodeFailed         = "split_filing_resume_failed"
	splitFilingResumeCodeNotApproved    = "plan_not_approved"
	splitFilingResumeCodeNoSplit        = "no_split_proposal"
	splitFilingResumeCodePlanChanged    = "plan_changed_since_approval"
	splitFilingResumeCodeRunTerminal    = "run_terminal"
	splitFilingResumeCodeRepoMalformed  = "split_filing_repo_malformed"
	splitFilingResumeCodeNoParentIssue  = "split_filing_no_parent_issue"
	splitFilingResumeCodeInProgress     = "split_filing_in_progress"
	splitFilingResumeCodeFilingRefused  = "split_filing_refused"
	splitFilingResumeRequiredScope      = "write:approvals"
	splitFilingResumeHandFiledRemedyTxt = "children filed by hand carry no idempotency marker, so a later resume would file them again"
)

// splitFilingResumeRequestedPayload is the attribution entry's payload.
type splitFilingResumeRequestedPayload struct {
	StageID      string `json:"stage_id"`
	ArtifactID   string `json:"artifact_id"`
	Outcome      string `json:"outcome"`
	PhaseCount   int    `json:"phase_count"`
	AlreadyFiled int    `json:"already_filed"`
	Actor        string `json:"actor"`
}

// splitFilingResumeResponse is the 200 (completed) / 202 (resuming) body.
// budget_seconds is set only on 202: the longest the launched filing can run.
type splitFilingResumeResponse struct {
	RunID         string `json:"run_id"`
	StageID       string `json:"stage_id"`
	State         string `json:"state"`
	PhaseCount    int    `json:"phase_count"`
	AlreadyFiled  int    `json:"already_filed"`
	BudgetSeconds int    `json:"budget_seconds,omitempty"`
}

// splitFilingResumeAnonymousMsg is the 401 message: it names the remedy scope.
const splitFilingResumeAnonymousMsg = "an authenticated operator token carrying write:approvals is required to resume a split filing"

// splitFilingResumeRequireAuthenticated refuses an anonymous identity 401
// BEFORE requireRunAccount, whose ownership check would otherwise answer an
// anonymous caller 403 account_forbidden on an account-owned run (V3). Modelled
// on escapeRequireAuthenticated, with a message naming the required scope.
func (s *Server) splitFilingResumeRequireAuthenticated(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if IdentityFrom(r.Context()).IsAnonymous() {
			s.writeError(w, r, http.StatusUnauthorized, "authentication_required", splitFilingResumeAnonymousMsg, nil)
			return
		}
		next(w, r)
	}
}

// handleResumeSplitFiling is POST /v0/runs/{run_id}/split-filing/resume.
//
// Auth: memberWrite via requireRunAccount (the tier plan approval uses), behind
// the authentication-first wrapper. The caller needs write:approvals — the
// scope plan approval needs (AA2); cookie sessions pass the scope check. A
// run-bound token (any mcp:run: subject, malformed or not) is refused. A
// delegated operator-agent token holding write:approvals is ADMITTED: the verb
// applies no approver-eligibility gate, and it can file only what a recorded
// approval already authorized.
func (s *Server) handleResumeSplitFiling(w http.ResponseWriter, r *http.Request) {
	id := IdentityFrom(r.Context())
	if id.IsAnonymous() {
		s.writeError(w, r, http.StatusUnauthorized, "authentication_required", splitFilingResumeAnonymousMsg, nil)
		return
	}
	// A PREFIX check (AA3), so a malformed mcp:run:<non-uuid> subject is refused.
	if isRunBoundSubject(id.Subject) {
		s.writeError(w, r, http.StatusForbidden, "run_token_forbidden",
			"a run-bound agent token may not resume a split filing; completing an approved filing is an operator action", nil)
		return
	}
	if id.TokenID != "" && !hasScope(id, splitFilingResumeRequiredScope) {
		s.writeError(w, r, http.StatusForbidden, "insufficient_scope",
			"token is missing required scope: "+splitFilingResumeRequiredScope,
			map[string]any{"required_scope": splitFilingResumeRequiredScope})
		return
	}
	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil || s.cfg.ArtifactRepo == nil || s.cfg.ApprovalRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, splitFilingResumeCodeUnconfigured,
			"the split-filing resume verb needs the run, audit, artifact and approval repositories", nil)
		return
	}
	ctx := r.Context()
	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if errors.Is(err, run.ErrNotFound) {
		s.writeError(w, r, http.StatusNotFound, "run_not_found",
			"no run with that id", map[string]any{"run_id": runID.String()})
		return
	}
	if err != nil {
		s.splitFilingResumeReadFailed(w, r, runID, "could not read the run", "internal_error", err)
		return
	}
	// V4: a failed or cancelled run is refused; a succeeded run stays admitted.
	if runRow.State == run.StateFailed || runRow.State == run.StateCancelled {
		s.writeError(w, r, http.StatusConflict, splitFilingResumeCodeRunTerminal,
			"run is "+string(runRow.State)+"; start a new run for the remaining phases, or file them by hand ("+splitFilingResumeHandFiledRemedyTxt+")",
			map[string]any{"run_id": runID.String(), "run_state": string(runRow.State)})
		return
	}

	planStage, ok := s.splitFilingResumePlanStage(w, r, runID)
	if !ok {
		return
	}
	if !s.splitFilingResumeApproved(w, r, runID, planStage) {
		return
	}

	src, status, err := s.loadSplitPlanForStage(ctx, planStage)
	if err != nil {
		s.splitFilingResumeReadFailed(w, r, runID, "could not load the plan stage's own plan artifact", splitFilingResumeCodeFailed, err)
		return
	}
	switch status {
	case splitPlanOK:
	case splitPlanChangedSinceApproval:
		details := map[string]any{
			"run_id": runID.String(), "stage_id": planStage.ID.String(),
			"artifact_id": src.artifactID.String(), "artifact_created_at": src.createdAt.UTC().Format(time.RFC3339Nano),
		}
		msg := "the plan stage's newest plan artifact was created after the plan gate closed, so nothing is filed from it; start a new run, because the artifact changed after the plan gate closed"
		if src.boundAt.IsZero() {
			// No EndedAt: the comparison never ran, so the text must not claim
			// the artifact changed after the gate closed.
			msg = "the plan stage records no ended_at, so its newest plan artifact cannot be shown to predate the plan gate's close and nothing is filed from it; start a new run"
		} else {
			details["stage_ended_at"] = src.boundAt.UTC().Format(time.RFC3339Nano)
		}
		s.writeError(w, r, http.StatusConflict, splitFilingResumeCodePlanChanged, msg, details)
		return
	case splitPlanUndecodable:
		s.splitFilingResumeNoSplit(w, r, runID, planStage, "plan_artifact_undecodable")
		return
	default:
		s.splitFilingResumeNoSplit(w, r, runID, planStage, "no_plan_artifact")
		return
	}
	if src.plan == nil || src.plan.SplitProposal == nil || len(src.plan.SplitProposal.Phases) == 0 {
		s.splitFilingResumeNoSplit(w, r, runID, planStage, "no_split_proposal")
		return
	}
	phaseCount := len(src.plan.SplitProposal.Phases)

	// S5: the hook returns silently on both, so refuse loudly here.
	if _, _, ok := splitRepoFullName(runRow.Repo); !ok {
		s.writeError(w, r, http.StatusConflict, splitFilingResumeCodeRepoMalformed,
			"the run's repo is not owner/name, so no child can be filed; nothing was recorded",
			map[string]any{"run_id": runID.String(), "repo": runRow.Repo})
		return
	}
	parentIssue := 0
	if runRow.TriggerRef != nil {
		if n, ok := parseIssueRef(*runRow.TriggerRef); ok {
			parentIssue = n
		}
	}
	if parentIssue == 0 {
		s.writeError(w, r, http.StatusConflict, splitFilingResumeCodeNoParentIssue,
			"the run has no originating issue trigger_ref, so split children have no parent to reference; nothing was recorded",
			map[string]any{"run_id": runID.String()})
		return
	}

	budget := splitFilingBudgetFor(phaseCount)
	hold := s.splitFiling.claim(splitHoldResume, runID, planStage.ID)
	if hold == nil {
		s.writeError(w, r, http.StatusConflict, splitFilingResumeCodeInProgress,
			"a split filing for this run is still in flight; await split_filing on fishhawk_get_plan or a split_children_filed entry, then retry. "+
				"A missing split_children_filed is not proof of progress: wait at most budget_seconds, then call this resume again — 409 split_filing_in_progress means it is still running",
			map[string]any{"run_id": runID.String(), "budget_seconds": int(budget / time.Second)})
		return
	}
	launched := false
	defer func() {
		if !launched {
			hold.release()
		}
	}()

	// S4: every marker read surfaces its error; nothing is launched on one.
	completion, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, splitChildrenFiledCategory)
	if err != nil {
		s.splitFilingResumeReadFailed(w, r, runID, "could not read split_children_filed", "internal_error", err)
		return
	}
	refusals, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, splitFilingRefusedCategory)
	if err != nil {
		s.splitFilingResumeReadFailed(w, r, runID, "could not read split_filing_refused", "internal_error", err)
		return
	}
	filed, err := s.loadFiledSplitChildren(ctx, runID)
	if err != nil {
		s.splitFilingResumeReadFailed(w, r, runID, "could not read work_item_filed", "internal_error", err)
		return
	}
	if len(refusals) > 0 {
		s.writeError(w, r, http.StatusConflict, splitFilingResumeCodeFilingRefused,
			"the split filing was refused (a phase's own scope exceeds the implement cap); a resume files nothing. Re-plan with in-cap phases in a new run",
			map[string]any{"run_id": runID.String(), "stage_id": planStage.ID.String()})
		return
	}
	alreadyFiled := 0
	for idx := range filed {
		if idx >= 0 && idx < phaseCount {
			alreadyFiled++
		}
	}
	outcome := splitFilingResumeStateResuming
	if len(completion) > 0 {
		outcome = splitFilingResumeStateCompleted
	}

	// S7: one chained attribution entry BEFORE any launch; an append failure
	// launches nothing.
	subject := id.Subject
	actorKind := actorKindForSubject(subject)
	raw, err := json.Marshal(splitFilingResumeRequestedPayload{
		StageID: planStage.ID.String(), ArtifactID: src.artifactID.String(), Outcome: outcome,
		PhaseCount: phaseCount, AlreadyFiled: alreadyFiled, Actor: subject,
	})
	if err == nil {
		_, err = s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
			RunID:        runID,
			Timestamp:    time.Now().UTC(),
			Category:     splitFilingResumeRequestedCategory,
			ActorKind:    &actorKind,
			ActorSubject: &subject,
			Payload:      raw,
		})
	}
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, splitFilingResumeCodeFailed,
			"could not record the resume request; nothing was filed",
			map[string]any{"run_id": runID.String(), internalCauseKey: err.Error()})
		return
	}

	resp := splitFilingResumeResponse{
		RunID: runID.String(), StageID: planStage.ID.String(), State: outcome,
		PhaseCount: phaseCount, AlreadyFiled: alreadyFiled,
	}
	if outcome == splitFilingResumeStateCompleted {
		s.writeJSON(w, r, http.StatusOK, resp)
		return
	}
	launched = true
	resp.BudgetSeconds = int(s.spawnSplitFiling(ctx, planStage, hold, src) / time.Second)
	s.writeJSON(w, r, http.StatusAccepted, resp)
}

// splitFilingResumePlanStage returns the run's OWN newest plan-type stage (no
// parent walk) when it has succeeded; otherwise it writes 409 plan_not_approved.
func (s *Server) splitFilingResumePlanStage(w http.ResponseWriter, r *http.Request, runID uuid.UUID) (*run.Stage, bool) {
	stages, err := s.cfg.RunRepo.ListStagesForRun(r.Context(), runID)
	if err != nil {
		s.splitFilingResumeReadFailed(w, r, runID, "could not list the run's stages", "internal_error", err)
		return nil, false
	}
	var planStage *run.Stage
	for _, st := range stages {
		if st.Type != run.StageTypePlan {
			continue
		}
		if planStage == nil || st.CreatedAt.After(planStage.CreatedAt) {
			planStage = st
		}
	}
	if planStage == nil {
		s.writeError(w, r, http.StatusConflict, splitFilingResumeCodeNotApproved,
			"the run has no plan stage of its own; a parent run's plan is never filed from here",
			map[string]any{"run_id": runID.String(), "reason": "no_plan_stage"})
		return nil, false
	}
	if planStage.State != run.StageStateSucceeded {
		s.writeError(w, r, http.StatusConflict, splitFilingResumeCodeNotApproved,
			"the run's plan stage is "+string(planStage.State)+", not succeeded; approve the plan at the plan gate first",
			map[string]any{"run_id": runID.String(), "stage_id": planStage.ID.String(),
				"stage_state": string(planStage.State), "reason": "plan_stage_not_succeeded"})
		return nil, false
	}
	return planStage, true
}

// splitFilingResumeApproved is S3: the plan stage must be gated and carry a
// recorded approve decision; otherwise it writes 409 plan_not_approved.
func (s *Server) splitFilingResumeApproved(w http.ResponseWriter, r *http.Request, runID uuid.UUID, planStage *run.Stage) bool {
	if !planStage.RequiresApproval {
		s.writeError(w, r, http.StatusConflict, splitFilingResumeCodeNotApproved,
			"the run's plan stage has no approval gate, so no approval authorizes a split filing",
			map[string]any{"run_id": runID.String(), "stage_id": planStage.ID.String(), "reason": "gateless_plan_stage"})
		return false
	}
	rows, err := s.cfg.ApprovalRepo.ListForStage(r.Context(), planStage.ID)
	if err != nil {
		s.splitFilingResumeReadFailed(w, r, runID, "could not read the plan stage's approvals", "internal_error", err)
		return false
	}
	for _, a := range rows {
		if a.Decision == approval.DecisionApprove {
			return true
		}
	}
	s.writeError(w, r, http.StatusConflict, splitFilingResumeCodeNotApproved,
		"the run's plan stage carries no approve decision; approve the plan at the plan gate first",
		map[string]any{"run_id": runID.String(), "stage_id": planStage.ID.String(), "reason": "no_approve_decision"})
	return false
}

// splitFilingResumeNoSplit writes 409 no_split_proposal.
func (s *Server) splitFilingResumeNoSplit(w http.ResponseWriter, r *http.Request, runID uuid.UUID, planStage *run.Stage, reason string) {
	s.writeError(w, r, http.StatusConflict, splitFilingResumeCodeNoSplit,
		"this run's own plan stage carries no approved split_proposal; a parent run's plan is never filed from here",
		map[string]any{"run_id": runID.String(), "stage_id": planStage.ID.String(), "reason": reason})
}

// splitFilingResumeReadFailed maps a store read error: unavailable → the 503
// service_unavailable envelope, anything else → 500 with code. Nothing was
// recorded or launched.
func (s *Server) splitFilingResumeReadFailed(w http.ResponseWriter, r *http.Request, runID uuid.UUID, what, code string, err error) {
	if dberr.IsUnavailable(err) {
		s.writeDBUnavailable(w, r)
		return
	}
	s.writeError(w, r, http.StatusInternalServerError, code,
		what+"; nothing was recorded or filed",
		map[string]any{"run_id": runID.String(), internalCauseKey: err.Error()})
}
