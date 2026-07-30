package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// applies_to routing enforcement — the shared evaluation core (E53.3 / #2226,
// ADR-066 fork 4 as refined by the operator's 2026-07-30 ruling §1).
//
// A workflow's `applies_to` is a spec.Predicate (E53.1 / #2224) declaring
// which changes may be routed through it. Enforcement is FAIL-CLOSED and runs
// in TWO PHASES, because the predicate's criteria do not all have a producer
// at the same moment:
//
//   - ADMISSION (this file, wired into POST /v0/runs): `labels` and `trigger`.
//     Both are known before the run row exists — labels from the caller's
//     issue-context snapshot, trigger from trigger_source.
//   - PLAN GATE (applies_to_plan_gate.go, a sibling slice): `paths`. No path
//     set exists at start_run; the first authoritative one is the approved
//     plan's scope.files, which is BINDING rather than descriptive — the
//     existing scope gate confines the implement stage to it — so a run
//     admitted under a `paths: [docs/**]` declaration is CONFINED to docs-only,
//     not merely claimed to be.
//
// Both points fire before any implement work, and both render through the ONE
// message renderer below so an operator refused at either sees the same shape:
// the workflow, the criterion that failed, the value observed, and which
// workflows the change WOULD satisfy.
//
// FAIL-CLOSED IS UNIFORM AND DELIBERATELY ASYMMETRIC TO THIS PACKAGE'S
// ADVISORY NEIGHBOURS. spec.Predicate.Match returns (false, error) for an
// empty predicate or a malformed glob rather than silently not matching; both
// call sites treat that error as a REJECTION. runScopePrecheck, runSurfaceSweep,
// runTestSweep and overCapSplitRejection all correctly fail OPEN because they
// are advisory sweeps — writing `if err != nil { admit }` here by analogy with
// them is the tempting bug this comment exists to name.
//
// TRUST BOUNDARY: labels are fetched by the CALLER's `gh` and shipped inline on
// the create request, so this control prevents MISROUTING, not a determined
// authorized caller who attests whatever labels they like. That is an accepted
// boundary for a routing control whose sanctioned exception is an audited
// override; a server-side fetch is the named hardening follow-up.

// appliesToPhase names which half of the two-phase split an evaluation is
// running. The phase decides which criteria of the predicate are in play and
// how the rejection message describes the observed value.
type appliesToPhase int

const (
	// appliesToPhaseAdmission evaluates the criteria with a producer at
	// start_run: labels and trigger.
	appliesToPhaseAdmission appliesToPhase = iota
	// appliesToPhasePlanGate evaluates the criterion whose producer is the
	// approved plan: paths. Consumed by the sibling plan-gate slice.
	appliesToPhasePlanGate
)

// appliesToPhasePredicate splits p into the sub-predicate belonging to phase.
// The second return reports whether that phase has ANY criterion to evaluate;
// false means the phase does not constrain and the caller must ADMIT without
// calling Match (calling Match on an empty predicate would fail closed, which
// is right for a caller that skipped Validate and wrong for a deliberately
// deferred phase).
//
// The split is exhaustive over the predicate grammar: paths belong to the plan
// gate, labels and trigger to admission. change_kind is rejected at spec-parse
// time inside applies_to (no producer, no ratified token set), so it can never
// reach here — but it is carried into the admission half rather than dropped
// so a future producer landing the criterion cannot silently go unenforced.
func appliesToPhasePredicate(p spec.Predicate, phase appliesToPhase) (spec.Predicate, bool) {
	var sub spec.Predicate
	switch phase {
	case appliesToPhaseAdmission:
		sub.Labels = p.Labels
		sub.Triggers = p.Triggers
		sub.ChangeKinds = p.ChangeKinds
	case appliesToPhasePlanGate:
		sub.Paths = p.Paths
	}
	constrains := len(sub.Paths) > 0 || len(sub.Labels) > 0 || len(sub.ChangeKinds) > 0 || len(sub.Triggers) > 0
	return sub, constrains
}

// evaluateAppliesTo reports whether change c satisfies workflow wf's
// applies_to declaration for the given phase.
//
// Returns (matched, evaluated, err):
//   - a workflow declaring NO applies_to matches everything (true, false, nil)
//     — the back-compat reading that keeps every existing workflow selectable;
//   - a phase the predicate does not constrain matches (true, false, nil);
//   - otherwise the phase sub-predicate's Match decides, and a Match ERROR is
//     returned rather than swallowed so the caller can fail closed.
func evaluateAppliesTo(wf spec.Workflow, c spec.Change, phase appliesToPhase) (bool, bool, error) {
	if wf.AppliesTo == nil {
		return true, false, nil
	}
	sub, constrains := appliesToPhasePredicate(*wf.AppliesTo, phase)
	if !constrains {
		return true, false, nil
	}
	ok, err := sub.Match(c)
	if err != nil {
		return false, true, err
	}
	return ok, true, nil
}

// satisfyingWorkflows enumerates, in name order, every workflow in parsed that
// WOULD accept change c at this phase — the "where can I take this instead?"
// half of the rejection message. A workflow declaring no applies_to accepts
// any change and is included. A workflow whose predicate ERRORS is excluded:
// a declaration we cannot evaluate must never be recommended.
func satisfyingWorkflows(parsed *spec.Spec, c spec.Change, phase appliesToPhase) []string {
	if parsed == nil {
		return nil
	}
	var out []string
	for name, wf := range parsed.Workflows {
		ok, _, err := evaluateAppliesTo(wf, c, phase)
		if err != nil || !ok {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// appliesToRejection is everything the ONE renderer needs. Both rejection
// points (admission here, the plan gate in the sibling slice) build this and
// call renderAppliesToRejection, so the two messages cannot drift apart.
type appliesToRejection struct {
	// WorkflowID is the workflow the operator NAMED. applies_to filters an
	// operator-named workflow; it never selects one, which is why two
	// matching workflows is benign rather than a coin flip.
	WorkflowID string
	// Criterion is the predicate criterion that failed: labels, trigger or
	// paths. Empty when MatchErr is set (the declaration could not be
	// evaluated at all).
	Criterion string
	// Required is the criterion's declared values; Observed is what the
	// change carried, described by ObservedLabel (e.g. "scope.files").
	Required      []string
	Observed      []string
	ObservedLabel string
	// AbsentIssueContext marks the EMPTY-label-set run: the change's label
	// set is empty rather than merely non-matching. This gets its own
	// sentence — "your labels don't match" is actively misleading when
	// there are no labels to match with.
	AbsentIssueContext bool
	// IssueContextPresent discriminates the TWO ways a label set comes out
	// empty, which have different fixes and must not be described with one
	// sentence: an issue-LESS run (false — trigger_source=cli/ui, no issue
	// context at all) versus an UNLABELLED issue (true — issue context is
	// present and carries zero labels). Telling the operator of an
	// issue-triggered run that it "carries NO issue context" sends them
	// looking for a trigger problem they do not have.
	IssueContextPresent bool
	TriggerSource       string
	// Satisfying is the workflows that WOULD accept this change.
	Satisfying []string
	// MatchErr is the fail-closed-on-error branch: the predicate could not be
	// evaluated (empty predicate, malformed glob). Rendered as a rejection,
	// never degraded into an admit.
	MatchErr error
	// Phase names where the refusal happened, so the two messages are
	// distinguishable in a log without being differently shaped.
	Phase appliesToPhase
}

// renderAppliesToRejection is THE message renderer both rejection points use.
// Shape (binding, asserted by test): the workflow, the criterion that failed,
// the value observed, and which workflows the change WOULD satisfy — plus the
// three ways forward. An operator refused at the plan gate is further into a
// run than one refused at admission and needs more help, not less, so the two
// carry the same content.
func renderAppliesToRejection(rj appliesToRejection) string {
	var b strings.Builder
	fmt.Fprintf(&b, "workflow %q does not accept this change", rj.WorkflowID)
	label := rj.ObservedLabel
	if label == "" {
		label = rj.Criterion
	}
	observed := "[]"
	if len(rj.Observed) > 0 {
		observed = "[" + strings.Join(rj.Observed, ", ") + "]"
	}
	switch {
	case rj.MatchErr != nil:
		fmt.Fprintf(&b, ": its applies_to declaration could not be evaluated (%s), so the run is refused rather than admitted — a routing declaration that cannot be evaluated is never treated as match-all", rj.MatchErr.Error())
	case rj.AbsentIssueContext && rj.IssueContextPresent:
		// The issue exists and carries no labels. The observed value is
		// still reported — the shape is binding on EVERY rejection path,
		// and "[]" is the actionable fact here, not a detail to elide.
		fmt.Fprintf(&b, ": its applies_to %s criterion requires one of [%s], but the change's %s is %s — the run's issue context carries no labels at all; an unlabelled issue cannot satisfy a labels criterion",
			rj.Criterion, strings.Join(rj.Required, ", "), label, observed)
	case rj.AbsentIssueContext:
		fmt.Fprintf(&b, ": its applies_to %s criterion requires one of [%s], but the change's %s is %s because this run carries NO issue context (trigger_source=%s); an issue-less run cannot satisfy a labels criterion",
			rj.Criterion, strings.Join(rj.Required, ", "), label, observed, rj.TriggerSource)
	default:
		fmt.Fprintf(&b, ": its applies_to %s criterion requires one of [%s], but the change's %s is %s",
			rj.Criterion, strings.Join(rj.Required, ", "), label, observed)
	}
	if len(rj.Satisfying) > 0 {
		fmt.Fprintf(&b, ". Workflows that would accept this change: %s", strings.Join(rj.Satisfying, ", "))
	} else {
		b.WriteString(". No workflow in this spec accepts this change")
	}
	if rj.Phase == appliesToPhasePlanGate {
		b.WriteString(". Amend the workflow's applies_to declaration, narrow the plan's scope.files to satisfy it, or re-start the run with applies_to_override and a reason")
	} else {
		b.WriteString(". Amend the workflow's applies_to declaration, start the run under a workflow that accepts this change, or pass applies_to_override with a reason to force this run")
	}
	return b.String()
}

// firstFailingCriterion re-walks sub's DECLARED criteria in evaluation order
// and reports the first one c does not satisfy, with its declared and observed
// values, so the message names a specific criterion instead of "the
// predicate". Returns ok=false only when every declared criterion is
// satisfied, which a caller reaching here after a non-match will not see.
func firstFailingCriterion(sub spec.Predicate, c spec.Change) (criterion string, required, observed []string, ok bool) {
	if len(sub.Paths) > 0 {
		if m, err := (spec.Predicate{Paths: sub.Paths}).Match(spec.Change{Paths: c.Paths}); err != nil || !m {
			return "paths", sub.Paths, c.Paths, true
		}
	}
	if len(sub.Labels) > 0 {
		if m, err := (spec.Predicate{Labels: sub.Labels}).Match(spec.Change{Labels: c.Labels}); err != nil || !m {
			return "labels", sub.Labels, c.Labels, true
		}
	}
	if len(sub.ChangeKinds) > 0 {
		if m, err := (spec.Predicate{ChangeKinds: sub.ChangeKinds}).Match(spec.Change{ChangeKind: c.ChangeKind}); err != nil || !m {
			return "change_kind", sub.ChangeKinds, []string{c.ChangeKind}, true
		}
	}
	if len(sub.Triggers) > 0 {
		if m, err := (spec.Predicate{Triggers: sub.Triggers}).Match(spec.Change{Trigger: c.Trigger}); err != nil || !m {
			return "trigger", triggerFormStrings(sub.Triggers), []string{string(normalizeTriggerForm(c.Trigger))}, true
		}
	}
	return "", nil, nil, false
}

func triggerFormStrings(tf []spec.TriggerForm) []string {
	out := make([]string, len(tf))
	for i, t := range tf {
		out[i] = string(t)
	}
	return out
}

// normalizeTriggerForm mirrors Predicate.Match's back-compat seam so the
// rejection message reports the trigger form actually matched against rather
// than an empty string.
func normalizeTriggerForm(t spec.TriggerForm) spec.TriggerForm {
	if t == "" {
		return spec.TriggerDiff
	}
	return t
}

// triggerFormForSource maps a run's trigger_source onto the predicate's
// TriggerForm vocabulary. Every trigger source v0 mints — github_issue, cli,
// ui — produces a code diff, so all three map to TriggerDiff. The non-diff
// forms (scheduled, on_demand) have NO producer today: ADR-065's groomer and
// ADR-053's incident intake will emit them. A predicate declaring only a
// non-diff form therefore matches nothing yet, which is documented rather than
// rejected because — unlike change_kind — `trigger: [diff, scheduled]` is
// partially usable against real runs today.
func triggerFormForSource(triggerSource string) spec.TriggerForm {
	switch triggerSource {
	case string(run.TriggerGitHubIssue), string(run.TriggerCLI), string(run.TriggerUI):
		return spec.TriggerDiff
	default:
		return spec.TriggerDiff
	}
}

// admissionChange builds the spec.Change evaluated at start_run from the
// create request's own inputs. Labels come from the issue-context snapshot the
// caller shipped inline; a nil issue context yields a nil label set, which is
// an EMPTY label set and fails closed against a labels criterion.
//
// Paths are deliberately absent: no authoritative path set exists at
// admission, and the plan gate — not a caller self-attestation — supplies one.
func admissionChange(triggerSource string, ic *issueContextPayload) spec.Change {
	c := spec.Change{Trigger: triggerFormForSource(triggerSource)}
	if ic != nil {
		c.Labels = ic.Labels
	}
	return c
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
	var rj appliesToRejection
	if sub, constrains := appliesToPhasePredicate(*workflowDef.AppliesTo, appliesToPhaseAdmission); constrains {
		c := admissionChange(req.TriggerSource, req.IssueContext)
		matched, matchErr := sub.Match(c)
		if matchErr != nil || !matched {
			rj = appliesToRejection{
				WorkflowID:    req.WorkflowID,
				TriggerSource: req.TriggerSource,
				Satisfying:    satisfyingWorkflows(parsed, c, appliesToPhaseAdmission),
				MatchErr:      matchErr,
				Phase:         appliesToPhaseAdmission,
			}
			if matchErr == nil {
				crit, required, observed, ok := firstFailingCriterion(sub, c)
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
			message = renderAppliesToRejection(rj)
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
// bypassed the declaration with nothing in the log saying so. This is the
// asymmetry to the REFUSAL audit, whose append failure is correctly best-effort:
// there the decision already went the safe way, so an audit problem must not
// soften it. Here the decision goes the UNSAFE way, so the audit is a
// precondition of it.
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
// already been decided and must not be softened by an audit problem.
func (s *Server) appendAppliesToRejectedAudit(r *http.Request, req *createRunRequest, rj appliesToRejection, message string) {
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
