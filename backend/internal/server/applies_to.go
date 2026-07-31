package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/appliesto"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// applies_to routing enforcement — the server admission seam (E53.3 / #2226,
// ADR-066 fork 4 as refined by the operator's 2026-07-30 ruling §1).
//
// The pure evaluation core (the two-phase split, the satisfying-workflow
// enumeration, the ONE message renderer, and the trigger/label Change builders)
// lives in backend/internal/appliesto and is shared verbatim with the plan gate
// (applies_to_plan_gate.go) and the webhook dispatcher (E53.10 / #2361). This
// file is the HTTP/audit-shaped ADAPTER for POST /v0/runs: it delegates every
// evaluation to appliesto.* and owns only the create-request wiring, the 422
// response, the run_rejected_applies_to audit append, and the audited override
// grant + carry-forward.

// issueContextLabels reads the label set off a create request's inline
// issue-context snapshot, or nil when there is no snapshot. A nil snapshot is an
// EMPTY label set — it fails closed against a labels criterion, never match-all.
func issueContextLabels(ic *issueContextPayload) []string {
	if ic == nil {
		return nil
	}
	return ic.Labels
}

// appliesToOverrideRecord carries the audited-override decision from the
// pre-insert gate to the post-insert audit append. The override's source of
// truth is the RUN-SCOPED audit entry, which only exists once the run row does
// — so the decision is made pre-insert (no run row on refusal) and recorded
// post-insert.
type appliesToOverrideRecord struct {
	WorkflowID string
	Repo       string
	Reason     string
	// Rejection is the refusal the override suppressed, recorded verbatim so
	// the audit entry explains WHAT was bypassed and not merely that
	// something was.
	Rejection string
}

// checkAppliesTo is the fail-closed admission gate for POST /v0/runs, sitting
// beside checkBlockingBudget and pre-insert for the same reason: a refusal must
// leave no run row behind.
//
// Returns (admit, overrideRecord). On refusal it writes the 422 response AND a
// global-chain run_rejected_applies_to audit entry (there is no run id to scope
// it to yet) and returns false — the caller must return immediately. When an
// override is in play the returned record is non-nil and the caller must append
// the run-scoped entry once the run exists.
//
// THE OVERRIDE IS EVALUATED INDEPENDENTLY OF WHETHER ADMISSION REFUSES, which
// is what makes it reachable for the DEFERRED `paths` criterion. Deriving the
// record from the refusal — the obvious reading, since this is where the
// refusal happens — would produce an override that exists only when admission
// itself refuses, and so is unavailable in the COMMON case: a workflow whose
// labels/trigger the run satisfies (or which declares neither) and whose
// `paths` the plan will not. That operator's only recourse would be widening
// the declaration permanently, which is exactly the behaviour the override
// exists to avoid.
func (s *Server) checkAppliesTo(w http.ResponseWriter, r *http.Request, req *createRunRequest, parsed *spec.Spec, workflowDef spec.Workflow) (bool, *appliesToOverrideRecord) {
	if workflowDef.AppliesTo == nil {
		// No declaration at all: nothing to enforce and nothing to
		// override, at either phase.
		return true, nil
	}

	// message is the admission refusal, or "" when admission is satisfied
	// (including the paths-only declaration, which does not constrain this
	// phase at all — the positive proof that `paths` is DEFERRED to the plan
	// gate rather than silently evaluated against zero paths, which would
	// reject every run).
	var message string
	var rj appliesto.Rejection
	if sub, constrains := appliesto.PhasePredicate(*workflowDef.AppliesTo, appliesto.PhaseAdmission); constrains {
		c := appliesto.AdmissionChange(req.TriggerSource, issueContextLabels(req.IssueContext))
		matched, matchErr := sub.Match(c)
		if matchErr != nil || !matched {
			rj = appliesto.Rejection{
				WorkflowID:    req.WorkflowID,
				TriggerSource: req.TriggerSource,
				Satisfying:    appliesto.SatisfyingWorkflows(parsed, c, appliesto.PhaseAdmission),
				MatchErr:      matchErr,
				Phase:         appliesto.PhaseAdmission,
				Seam:          appliesto.SeamStartRun,
			}
			if matchErr == nil {
				crit, required, observed, ok := appliesto.FirstFailingCriterion(sub, c)
				if ok {
					rj.Criterion = crit
					rj.Required = required
					rj.Observed = observed
					rj.ObservedLabel = crit
					// An empty label set is not a mismatched label set, and
					// saying so is what tells the operator the fix is an
					// issue-triggered (or differently-labelled) issue rather
					// than different labels on this one.
					rj.AbsentIssueContext = crit == "labels" && (req.IssueContext == nil || len(req.IssueContext.Labels) == 0)
					rj.IssueContextPresent = req.IssueContext != nil
				}
			}
			message = appliesto.RenderRejection(rj)
		}
	}

	if req.AppliesToOverride {
		// Audited force-past. The bypass is audited BEFORE it takes effect;
		// an override that cannot be recorded refuses the run.
		if !s.grantAppliesToOverride(w, r, req, message) {
			return false, nil
		}
		return true, &appliesToOverrideRecord{
			WorkflowID: req.WorkflowID,
			Repo:       req.Repo,
			Reason:     strings.TrimSpace(req.AppliesToOverrideReason),
			Rejection:  message,
		}
	}

	if message == "" {
		return true, nil
	}

	s.appendAppliesToRejectedAudit(r, req, rj, message)
	s.writeError(w, r, http.StatusUnprocessableEntity, "workflow_not_applicable", message,
		map[string]any{
			"workflow_id":           req.WorkflowID,
			"criterion":             rj.Criterion,
			"required":              rj.Required,
			"observed":              rj.Observed,
			"satisfying_workflows":  rj.Satisfying,
			"absent_issue_context":  rj.AbsentIssueContext,
			"applies_to_evaluation": "start_run",
		})
	return false, nil
}

// grantAppliesToOverride audits the override BEFORE it takes effect and reports
// whether the run may be admitted. It writes the 503 response itself on refusal.
//
// THE AUDIT ENTRY IS THE OVERRIDE'S SOURCE OF TRUTH, and a source of truth that
// is written best-effort AFTER the bypass has already been granted is not one.
// The run-scoped entry cannot be written here — it needs a run id that does not
// exist until the insert — so the grant is recorded on the GLOBAL chain, the
// same chain the refusal uses and for the same reason, and an append failure
// (or an absent audit repository) REFUSES the run.
//
// Warn-logging this failure instead would be a governance bypass with no record
// at all, and for an ADMISSION-ONLY violation nothing downstream would ever
// catch it: `paths` is re-evaluated at the plan gate, so a lost override there
// merely costs the operator a re-run, but a `labels` or `trigger` violation has
// no second evaluation point — the run would proceed to completion having
// bypassed the declaration with nothing in the log saying so. THIS IS THE
// DELIBERATE ASYMMETRY TO THE REFUSAL AUDIT (appendAppliesToRejectedAudit,
// refusedByAppliesTo), whose append failure is correctly best-effort: a GRANT
// records an EXCEPTION being made (503, no run row on failure), so an unrecorded
// exception must not happen; a REFUSAL is the SAFE outcome, so its audit is
// best-effort — admitting a run because we could not write a log line would
// invert the control. Same control, opposite failure directions, both fail-safe.
//
// suppressed is the admission refusal the override forces past, or "" when
// admission was satisfied and the override is being carried forward for the
// plan gate's deferred `paths` check.
func (s *Server) grantAppliesToOverride(w http.ResponseWriter, r *http.Request, req *createRunRequest, suppressed string) bool {
	const unauditable = "applies_to_override cannot be granted: the bypass could not be recorded in the audit log, and an unaudited governance bypass is refused rather than admitted. Retry once the audit store is reachable, or start the run under a workflow that accepts this change"
	if s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "audit_unavailable", unauditable,
			map[string]any{"workflow_id": req.WorkflowID, "reason": "no audit repository configured"})
		return false
	}
	if suppressed == "" {
		suppressed = "none at admission; the override was requested up front and carries forward to the plan gate's deferred paths check"
	}
	payload, _ := json.Marshal(map[string]any{
		"workflow_id":          req.WorkflowID,
		"repo":                 req.Repo,
		"trigger_source":       req.TriggerSource,
		"reason":               strings.TrimSpace(req.AppliesToOverrideReason),
		"suppressed_rejection": suppressed,
		"phase":                "start_run",
	})
	systemKind := audit.ActorKind("system")
	if _, aerr := s.cfg.AuditRepo.AppendGlobalChained(r.Context(), audit.GlobalChainAppendParams{
		Timestamp: time.Now().UTC(),
		Category:  "run_admitted_applies_to_override",
		ActorKind: &systemKind,
		Payload:   payload,
		AccountID: identityAccountID(r.Context()),
	}); aerr != nil {
		s.cfg.Logger.Warn("append run_admitted_applies_to_override audit entry failed; refusing the override (fail-closed)",
			"repo", req.Repo, "workflow_id", req.WorkflowID, "error", aerr.Error())
		s.writeError(w, r, http.StatusServiceUnavailable, "audit_unavailable", unauditable,
			map[string]any{"workflow_id": req.WorkflowID, "error": aerr.Error()})
		return false
	}
	return true
}

// appendAppliesToRejectedAudit records the refusal on the GLOBAL chain: the
// run was refused pre-insert, so no run id exists to scope the entry to. A
// nil AuditRepo or an append failure is warn-logged — the refusal itself has
// already been decided and must not be softened by an audit problem. (See
// grantAppliesToOverride for the asymmetry: a grant fails closed on the same
// failure because it records an exception, not the safe outcome.)
func (s *Server) appendAppliesToRejectedAudit(r *http.Request, req *createRunRequest, rj appliesto.Rejection, message string) {
	if s.cfg.AuditRepo == nil {
		return
	}
	fields := map[string]any{
		"reason":               "workflow_not_applicable",
		"workflow_id":          req.WorkflowID,
		"repo":                 req.Repo,
		"trigger_source":       req.TriggerSource,
		"criterion":            rj.Criterion,
		"required":             rj.Required,
		"observed":             rj.Observed,
		"absent_issue_context": rj.AbsentIssueContext,
		"satisfying_workflows": rj.Satisfying,
		"message":              message,
	}
	if rj.MatchErr != nil {
		fields["evaluation_error"] = rj.MatchErr.Error()
	}
	payload, _ := json.Marshal(fields)
	systemKind := audit.ActorKind("system")
	if _, aerr := s.cfg.AuditRepo.AppendGlobalChained(r.Context(), audit.GlobalChainAppendParams{
		Timestamp: time.Now().UTC(),
		Category:  "run_rejected_applies_to",
		ActorKind: &systemKind,
		Payload:   payload,
		AccountID: identityAccountID(r.Context()),
	}); aerr != nil {
		s.cfg.Logger.Warn("append run_rejected_applies_to audit entry failed",
			"repo", req.Repo, "workflow_id", req.WorkflowID, "error", aerr.Error())
	}
}

// recordAppliesToOverride appends the RUN-SCOPED run_admitted_applies_to_override
// entry that is the override's carry-forward source of truth: the plan gate
// suppresses its own rejection by LOOKING THIS UP, never by re-reading request
// state.
//
// It is the CARRY-FORWARD half of a two-entry grant: grantAppliesToOverride has
// already recorded the bypass on the global chain as a PRECONDITION of
// admission, so the bypass itself is never unaudited. This entry is what the
// plan gate can look up by run id.
//
// AN APPEND FAILURE HERE IS RETURNED, NOT WARN-LOGGED, AND THE CALLER ABANDONS
// THE RUN. Warn-logging it and proceeding was the earlier posture, justified by
// "the plan gate will re-reject a deferred `paths` violation anyway" — which is
// true for `paths` and FALSE for the violation this override most often forces
// past. An ADMISSION-ONLY violation (labels, trigger) has no second evaluation
// point: that run would proceed to completion on a bypass its OWN audit chain
// does not carry, so every run-scoped read — the run's audit trail, the
// operator reconstructing why a label-violating change was routed here — sees a
// run that simply satisfied its declaration. A global-chain entry that no
// run-scoped query returns is not the run's source of truth for the bypass. So
// the entry is a precondition of the run SURVIVING, exactly as the grant entry
// is a precondition of it being admitted, and the two failure modes get the one
// answer: no entry, no run.
func (s *Server) recordAppliesToOverride(ctx context.Context, runID uuid.UUID, rec *appliesToOverrideRecord) error {
	if rec == nil {
		return nil
	}
	if s.cfg.AuditRepo == nil {
		// Unreachable in practice — grantAppliesToOverride refuses the run
		// pre-insert when no audit repository is wired, so no record exists to
		// carry forward. Fail closed rather than silently skipping, because the
		// only way to reach here is an internal inconsistency.
		return fmt.Errorf("record applies_to override for run %s: no audit repository configured", runID)
	}
	payload, _ := json.Marshal(map[string]any{
		"workflow_id":          rec.WorkflowID,
		"repo":                 rec.Repo,
		"reason":               rec.Reason,
		"suppressed_rejection": rec.Rejection,
	})
	systemKind := audit.ActorKind("system")
	if _, aerr := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		Timestamp: time.Now().UTC(),
		Category:  "run_admitted_applies_to_override",
		ActorKind: &systemKind,
		Payload:   payload,
	}); aerr != nil {
		return fmt.Errorf("append run-scoped run_admitted_applies_to_override for run %s: %w", runID, aerr)
	}
	return nil
}

// unauditedOverrideMessage is the operator-facing text for a run abandoned
// because its override could not be recorded run-scoped. It says what happened,
// what state the run is in, and that a retry recovers.
const unauditedOverrideMessage = "applies_to_override was granted but its run-scoped audit entry could not be recorded, so the run has been cancelled rather than left running on a bypass its own audit chain does not carry. The bypass itself is recorded on the global chain; only the carry-forward entry the plan gate reads is missing. This failure is transient: start the run again once the audit store is reachable"

// abandonUnauditedOverrideRun is the fail-closed answer to a carry-forward
// append failure: the run row exists (the entry needs its id), so "refuse"
// means CANCEL it and report 503, not leave it running.
//
// The cancel is best-effort — if it also fails there is nothing further this
// handler can do — but the 503 is not conditional on it: the operator is told
// the run is not usable either way, and a stuck run row is visible and
// cancellable, whereas a 201 would report a healthy run carrying an unrecorded
// governance bypass.
func (s *Server) abandonUnauditedOverrideRun(w http.ResponseWriter, r *http.Request, runID uuid.UUID, rec *appliesToOverrideRecord, cause error) {
	s.cfg.Logger.Warn("run-scoped applies_to override entry could not be recorded; cancelling the run (fail-closed)",
		"run_id", runID.String(), "workflow_id", rec.WorkflowID, "error", cause.Error())
	if s.cfg.RunRepo != nil {
		if _, terr := s.cfg.RunRepo.TransitionRun(r.Context(), runID, run.StateCancelled); terr != nil {
			s.cfg.Logger.Warn("cancelling the unaudited-override run failed; it is left for operator cancellation",
				"run_id", runID.String(), "error", terr.Error())
		}
	}
	s.writeError(w, r, http.StatusServiceUnavailable, "audit_unavailable", unauditedOverrideMessage,
		map[string]any{"run_id": runID.String(), "workflow_id": rec.WorkflowID, "error": cause.Error()})
}

// runHasAppliesToOverride reports whether runID carries the admission-time
// applies_to override — the NAMED carry-forward lookup the plan-gate slice
// consumes. The audit ENTRY is the source of truth, not the create request:
// a run whose creation carried an override but whose entry is absent has no
// override, and the plan gate rejects it.
//
// Fails closed on a lookup error: the error is returned and the caller must
// treat it as "no override" rather than as permission.
func (s *Server) runHasAppliesToOverride(ctx context.Context, runID uuid.UUID) (bool, error) {
	if s.cfg.AuditRepo == nil {
		return false, nil
	}
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, "run_admitted_applies_to_override")
	if err != nil {
		return false, fmt.Errorf("look up applies_to override for run %s: %w", runID, err)
	}
	return len(entries) > 0, nil
}
