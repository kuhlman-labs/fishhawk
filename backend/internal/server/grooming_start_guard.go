package server

import (
	"fmt"
	"net/http"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// groomStartGuardScanLimit bounds how many NON-TERMINAL runs of the same repo
// and grooming workflow checkNoParkedGroom inspects. The terminal filter runs
// in SQL before this LIMIT (ListRunsFilter.NonTerminal), so the bound counts
// open grooms only; a window that fills without a parked stage fails OPEN.
const groomStartGuardScanLimit = 50

// groomingRunParkedCode is the 409 error code checkNoParkedGroom refuses with.
const groomingRunParkedCode = "grooming_run_parked"

// groomGuardLaterGatesNotEvaluated names the admission gates that sit below the
// guard in handleCreateRun, so a refusal says which decisions it pre-empted.
var groomGuardLaterGatesNotEvaluated = []string{"plan_review_capability", "blocking_budget", "applies_to", "charter"}

// checkNoParkedGroom is the groom start guard (#5225, gap 2). It refuses a new
// BACKLOG-GROOMING run with 409 grooming_run_parked while a non-terminal run of
// the same repo and workflow, visible to the caller's tenant, has a stage
// parked at awaiting_approval: an unconfirmed groom at its groom gate (plan
// stage) or its confirm gate (review stage). It returns true to ADMIT; on a
// refusal it has already written the 409.
//
// Why: an unconfirmed groom parks indefinitely, and the weekly scheduler kept
// starting new ones beside it, so grooms piled up silently at the gate. A
// refusal on the scheduled path lands as a visible scheduled_run_refused row.
//
// Contract:
//
//   - Hygiene, not authorization. The guard is NOT atomic: two simultaneous
//     starts can both read "nothing parked" and both insert. It is not a
//     uniqueness guarantee; it stops the silent weekly pile-up.
//   - Structural predicate. Only a workflow WorkflowRequiresCharter reports as
//     grooming (a stage producing grooming_report) is guarded; every other
//     workflow is admitted without a read.
//   - Bounded store scan. The terminal filter is the STORE's (NonTerminal, in
//     SQL before the LIMIT), so a parked groom behind any number of newer
//     terminal runs is still found. At most groomStartGuardScanLimit open runs
//     are inspected. A full window with no parked stage fails OPEN with a Warn
//     log, because a parked groom past the window may exist unseen.
//   - Tenancy. Visible rows are the caller's own tenant plus untenanted rows
//     (groomGuardTenantVisible). For a TENANTED caller that equals what
//     GET /v0/runs shows. For an UNTENANTED caller (the scheduler, whose
//     scheduledRunIdentity carries no account) it is deliberately NARROWER
//     than GET /v0/runs: untenanted rows only, so another tenant's parked groom
//     never blocks a scheduled window or appears in its 409. Runs are currently
//     untenanted at creation (CreateRun writes no account_id), so this arm is
//     defence in depth today.
//   - Fail OPEN. A ListRuns error, a ListStagesForRun error for one run (the
//     scan continues with the next), and a full scan window each log at Warn
//     with repo and workflow_id (and run_id where known) and admit. No metric:
//     the Warn line is the signal.
//   - Placement. handleCreateRun calls this directly AFTER the Idempotency-Key
//     replay (a replay still answers 200) and BEFORE every other admission
//     gate, so a refused groom leaves no run row and no
//     run_admitted_budget_override / run_admitted_applies_to_override grant.
//     The known trade-off: a request this guard and a later gate would both
//     refuse gets grooming_run_parked, and no run_rejected_missing_charter or
//     run_rejected_misconfigured row is written for it. An on-demand refusal
//     writes NO audit row (unlike the sibling admission gates); only the
//     scheduled path records one, as scheduled_run_refused. The charter control
//     is not weakened: the request is refused either way, and once the parked
//     groom clears it meets every later gate unchanged.
func (s *Server) checkNoParkedGroom(w http.ResponseWriter, r *http.Request, repo, workflowID string, wf spec.Workflow) bool {
	if !WorkflowRequiresCharter(wf) {
		return true
	}
	callerAcct := ""
	if acct := identityAccountID(r.Context()); acct != nil {
		callerAcct = acct.String()
	}
	runs, err := s.cfg.RunRepo.ListRuns(r.Context(), run.ListRunsFilter{
		Repo:        repo,
		WorkflowID:  workflowID,
		AccountID:   callerAcct,
		NonTerminal: true,
		Limit:       groomStartGuardScanLimit,
	})
	if err != nil {
		s.cfg.Logger.Warn("groom start guard: list runs failed; admitting (fail-open)",
			"repo", repo, "workflow_id", workflowID, "error", err.Error())
		return true
	}
	for _, rn := range runs {
		if rn == nil || !groomGuardTenantVisible(rn, callerAcct) {
			continue
		}
		stages, serr := s.cfg.RunRepo.ListStagesForRun(r.Context(), rn.ID)
		if serr != nil {
			s.cfg.Logger.Warn("groom start guard: list stages failed; skipping this run (fail-open)",
				"repo", repo, "workflow_id", workflowID, "run_id", rn.ID.String(), "error", serr.Error())
			continue
		}
		parked := parkedGroomStage(stages)
		if parked == nil {
			continue
		}
		s.cfg.Logger.Warn("groom start guard: refusing a new groom while another is parked at its gate; later admission gates (budget, applies_to, charter, capability) were not evaluated",
			"repo", repo, "workflow_id", workflowID, "run_id", rn.ID.String(),
			"parked_stage_id", parked.ID.String(), "parked_stage_type", string(parked.Type))
		s.writeError(w, r, http.StatusConflict, groomingRunParkedCode,
			groomingRunParkedMessage(rn.ID.String(), parked.Type),
			map[string]any{
				"workflow_id":               workflowID,
				"parked_run_id":             rn.ID.String(),
				"parked_stage_id":           parked.ID.String(),
				"parked_stage_type":         string(parked.Type),
				"later_gates_not_evaluated": groomGuardLaterGatesNotEvaluated,
			})
		return false
	}
	if len(runs) == groomStartGuardScanLimit {
		s.cfg.Logger.Warn("groom start guard: scan window full with no parked stage; a parked groom past the window may exist unseen; admitting (fail-open)",
			"repo", repo, "workflow_id", workflowID, "scan_limit", groomStartGuardScanLimit)
	}
	return true
}

// groomGuardTenantVisible reports whether a candidate run is visible to the
// caller for the guard's purposes: the caller's own tenant plus untenanted
// rows. For an untenanted caller (callerAccount "") that is untenanted rows
// only, deliberately narrower than GET /v0/runs.
func groomGuardTenantVisible(candidate *run.Run, callerAccount string) bool {
	return candidate.AccountID == "" || candidate.AccountID == callerAccount
}

// parkedGroomStage returns the first stage parked at awaiting_approval, or nil.
func parkedGroomStage(stages []*run.Stage) *run.Stage {
	for _, st := range stages {
		if st != nil && st.State == run.StageStateAwaitingApproval {
			return st
		}
	}
	return nil
}

// groomingRunParkedMessage renders the actionable refusal: which run is parked
// at which gate, and the three ways to clear it before starting again.
func groomingRunParkedMessage(parkedRunID string, stageType run.StageType) string {
	gate := "a gate"
	switch stageType {
	case run.StageTypePlan:
		gate = "its groom gate (plan stage)"
	case run.StageTypeReview:
		gate = "its confirm gate (review stage)"
	}
	return fmt.Sprintf("grooming run %s of this repo and workflow is parked at %s awaiting a decision, so a new groom is refused rather than piling up beside it. "+
		"Clear it first: decide the groom gate (fishhawk_approve_plan / fishhawk_reject_plan); "+
		"approve the confirm gate from the operator host with `fishhawk approve-review-gate %s --attest \"...\"` (human-only); "+
		"or cancel the parked run (fishhawk_cancel_run / POST /v0/runs/%s/cancel). Then start the groom again. "+
		"Later admission gates (budget, applies_to, charter, capability) were not evaluated.",
		parkedRunID, gate, parkedRunID, parkedRunID)
}
