package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// applies_to routing enforcement — PHASE TWO, the plan gate (E53.3 / #2226,
// ADR-066 fork 4 as refined by the operator's 2026-07-30 ruling §1).
//
// Phase one (applies_to.go) evaluates the criteria with a producer at
// start_run: `labels` and `trigger`. This file evaluates the one criterion
// that has no producer until a plan exists: `paths`. At admission a code
// change has proposed no diff, so evaluating `paths` there could only match
// against zero paths — which the AND-across-types rule turns into a blanket
// refusal of every run. The first authoritative path set is the approved
// plan's scope.files.
//
// THE DEFERRAL IS WHAT MAKES THE DESIGN SOUND, NOT WHAT WEAKENS IT.
// scope.files is BINDING rather than descriptive: the existing scope gate
// (scope_precheck.go plus the runner's post-hoc constraint check) confines the
// implement stage to it. So a run admitted under a workflow declaring
// `paths: ["docs/**"]` and cleared here is CONFINED to docs-only for the rest
// of the run, not merely claimed to be. Both rejection points fire before any
// implement work, which is what ADR-066 fork 4 protects: a refusal costs a
// re-run, never half-applied work.
//
// This file is deliberately SEPARATE from applies_to.go so the admission slice
// is never re-edited; it reuses that file's phase split, satisfying-workflows
// enumeration, message renderer and override lookup rather than restating any
// of them, so the two rejection points cannot drift apart.

// planGateScopePaths returns the slash-normalized, de-duplicated, sorted UNION
// of every path the plan commits to touching:
//
//   - the top-level scope.files,
//   - every decomposition sub_plans[].scope.files, and
//   - every split_proposal.phases[].scope.files.
//
// The union — not the top-level list — is the object the criterion is
// evaluated against, and that is load-bearing rather than tidy. A decomposed
// plan's top-level scope can be clean while a slice's own scope reaches
// outside the declaration; the fan-out child then runs bounded to the SLICE
// scope (scope_handoff), so checking only the top level would let a slice
// escape the routing declaration entirely. The same reasoning applies to a
// split_proposal phase, which likewise carries its own scope. This mirrors
// scopedPaths in scope_regression.go, which had to cover sub-plan scopes for
// the same structural reason (#1257).
func planGateScopePaths(p *plan.Plan) []string {
	if p == nil {
		return nil
	}
	seen := make(map[string]struct{})
	var out []string
	add := func(sc *plan.Scope) {
		if sc == nil {
			return
		}
		for _, f := range sc.Files {
			if f.Path == "" {
				continue
			}
			norm := filepath.ToSlash(f.Path)
			if _, dup := seen[norm]; dup {
				continue
			}
			seen[norm] = struct{}{}
			out = append(out, norm)
		}
	}
	add(&p.Scope)
	if p.Decomposition != nil {
		for i := range p.Decomposition.SubPlans {
			add(p.Decomposition.SubPlans[i].Scope)
		}
	}
	if p.SplitProposal != nil {
		for i := range p.SplitProposal.Phases {
			add(p.SplitProposal.Phases[i].Scope)
		}
	}
	sort.Strings(out)
	return out
}

// maxRenderedUnmatchedPaths caps how many offending paths the rejection
// message names, so a 300-file plan yields an actionable message rather than a
// wall of text. The count of the remainder is still reported — a truncated
// list that hides its own truncation would understate the violation.
const maxRenderedUnmatchedPaths = 10

// planGateUnmatchedPaths reports which of paths the declared globs do NOT
// accept, evaluated with UNIVERSAL ("every path") semantics.
//
// WHY UNIVERSAL, AND WHY THAT IS NOT A SECOND MATCHER. spec.Predicate.Match's
// `paths` rule is existential — ANY change path matching ANY glob satisfies it
// — which is the right rule for its other consumers (E53.4 `escalations` and
// ADR-068's review conventions both ask "does this change TOUCH such a
// file?"). It is the wrong rule for a confinement control: under existential
// semantics a plan scoping [docs/x.md, backend/everything.go] would satisfy
// `paths: ["docs/**"]`, and the guarantee this gate exists to provide — that a
// run admitted under a docs-only declaration is confined to docs — would be
// false, as would the contract already published in docs/spec/workflow-v2.md
// ("A run admitted under a workflow declaring paths: ["docs/**"] is therefore
// *confined* to docs/**, not merely claimed to be").
//
// So the quantifier is applied HERE, over the ratified matcher used verbatim:
// each path is handed to spec.Predicate.Match one at a time and every one must
// be accepted. No second matcher is written and predicate.go is untouched
// (operator ruling §3) — Match still decides what a glob means, including its
// fail-closed malformed-glob error, which is returned rather than swallowed.
//
// The ZERO-PATH case delegates to the predicate's own ratified answer rather
// than to vacuous truth: a well-formed paths predicate returns (false, nil)
// against zero paths, so a plan committing to no files is REFUSED, not
// admitted on an empty ∀. Fail-closed is the whole posture; an empty ∀ would
// be the one hole in it.
func planGateUnmatchedPaths(globs, paths []string) ([]string, error) {
	p := spec.Predicate{Paths: globs}
	if len(paths) == 0 {
		// Delegate the no-paths verdict to the predicate itself: a well-formed
		// declaration answers (false, nil) — refuse — and an ill-formed one
		// answers an error the caller fails closed on. Either way there is no
		// specific offending path to name, so the caller's len(paths)==0 leg
		// owns the refusal.
		if _, err := p.Match(spec.Change{}); err != nil {
			return nil, err
		}
		return nil, nil
	}
	var unmatched []string
	for _, path := range paths {
		ok, err := p.Match(spec.Change{Paths: []string{path}})
		if err != nil {
			return nil, err
		}
		if !ok {
			unmatched = append(unmatched, path)
		}
	}
	return unmatched, nil
}

// renderUnmatchedPaths caps the offending-path list for the message while
// reporting how many were elided.
func renderUnmatchedPaths(unmatched []string) []string {
	if len(unmatched) <= maxRenderedUnmatchedPaths {
		return unmatched
	}
	out := make([]string, 0, maxRenderedUnmatchedPaths+1)
	out = append(out, unmatched[:maxRenderedUnmatchedPaths]...)
	out = append(out, fmt.Sprintf("… and %d more", len(unmatched)-maxRenderedUnmatchedPaths))
	return out
}

// planGateSatisfyingWorkflows enumerates, in name order, every workflow that
// would accept this run AT BOTH PHASES — the "where can I take this instead?"
// half of the rejection message.
//
// IT EVALUATES BOTH PHASES, NOT JUST paths, AND THAT IS THE WHOLE POINT OF THE
// LIST. The operator's next move after this refusal is to re-start the run
// under a named alternative, and that re-start goes through ADMISSION first.
// A candidate filtered only on `paths` can therefore be a workflow whose
// `labels` or `trigger` criterion the very same run cannot satisfy — a
// labels-only workflow would be listed unconditionally, because it does not
// constrain the plan gate at all — and the message would send the operator
// straight into a second, fail-closed admission rejection. The run's labels and
// trigger are IMMUTABLE for its lifetime (the issue-context snapshot on the run
// row and its trigger_source), so both are knowable here and both are checked:
// adm is the admission-phase change rebuilt from the run row.
//
// It deliberately does NOT reuse satisfyingWorkflows (applies_to.go) for the
// PATHS half, which evaluates via Predicate.Match and is therefore EXISTENTIAL
// on paths. Using it there would let the message recommend a workflow that then
// refuses the very same plan at this very same gate. The enumeration must be
// quantified the same way each decision is, so the admission half goes through
// evaluateAppliesTo (the admission gate's own core) and the plan-gate half
// through planGateUnmatchedPaths (this gate's own ∀).
//
// A workflow declaring no applies_to, or none that constrains a phase, accepts
// at that phase. A workflow whose declaration cannot be evaluated at EITHER
// phase is excluded — a declaration we cannot evaluate must never be
// recommended.
func planGateSatisfyingWorkflows(parsed *spec.Spec, paths []string, adm spec.Change) []string {
	if parsed == nil {
		return nil
	}
	var out []string
	for name, wf := range parsed.Workflows {
		// Phase one: would this run be ADMITTED under the candidate? A
		// recommendation the operator cannot act on is worse than none.
		admitted, _, aerr := evaluateAppliesTo(wf, adm, appliesToPhaseAdmission)
		if aerr != nil || !admitted {
			continue
		}
		if wf.AppliesTo == nil {
			out = append(out, name)
			continue
		}
		sub, constrains := appliesToPhasePredicate(*wf.AppliesTo, appliesToPhasePlanGate)
		if !constrains {
			out = append(out, name)
			continue
		}
		if len(paths) == 0 {
			// A paths-declaring workflow cannot accept a zero-path plan, for
			// the same reason the gate refuses one.
			continue
		}
		unmatched, err := planGateUnmatchedPaths(sub.Paths, paths)
		if err != nil || len(unmatched) > 0 {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// runAdmissionChange rebuilds the ADMISSION-phase spec.Change from a run row —
// the same value checkAppliesTo evaluated at start_run, read back from the
// immutable snapshot rather than re-derived from mutable forge state. It is the
// run-row twin of admissionChange (applies_to.go), which reads the create
// request; both must agree, which is why both go through triggerFormForSource
// and treat a nil issue context as an EMPTY label set rather than as match-all.
func runAdmissionChange(r *run.Run) spec.Change {
	c := spec.Change{Trigger: triggerFormForSource(string(r.TriggerSource))}
	if r.IssueContext != nil {
		c.Labels = r.IssueContext.Labels
	}
	return c
}

// resolveRunWorkflowDef resolves the run's workflow definition from the
// workflow-spec SNAPSHOT stored on the run row — the same bytes admission
// parsed, not a re-read of the repo's current governance file. One immutable
// snapshot is what keeps the two enforcement points from reaching two answers
// about one run.
//
// Returns ok=false on every leg where no declaration EXISTS to resolve, and a
// non-nil error on the one leg where a declaration may well exist but could not
// be READ. Mirrors resolveImplementConstraints (scope_precheck.go) so the
// plan-time gates agree on how a run's workflow is resolved.
//
// THE TWO OUTCOMES ARE NOT INTERCHANGEABLE and the caller must not collapse
// them. "There is no declaration" is a fact about the workflow and fails OPEN.
// "The store did not answer" is an absence of knowledge — a transient database
// blip, a timeout, a connection reset — and folding it into the first would
// make a governance control that any repository hiccup silently switches off,
// at exactly the moment the plan being gated is the one that would have been
// refused. That leg fails CLOSED at the call site.
//
// run.ErrNotFound is deliberately on the fail-OPEN side: a run id with no row
// is a genuine "nothing to enforce", not an outage, and the plan gate only ever
// runs for a run handleShipPlan already loaded.
// The fourth return is the ADMISSION-phase change rebuilt from the same row
// (runAdmissionChange), so the alternatives the rejection message names are
// filtered by the criteria a re-started run would meet FIRST rather than by
// `paths` alone.
func (s *Server) resolveRunWorkflowDef(ctx context.Context, runID uuid.UUID) (*spec.Spec, spec.Workflow, string, spec.Change, bool, error) {
	if s.cfg.RunRepo == nil {
		return nil, spec.Workflow{}, "", spec.Change{}, false, nil
	}
	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "applies_to plan gate: get run failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
		if errors.Is(err, run.ErrNotFound) {
			return nil, spec.Workflow{}, "", spec.Change{}, false, nil
		}
		return nil, spec.Workflow{}, "", spec.Change{}, false, fmt.Errorf("read run %s: %w", runID, err)
	}
	adm := runAdmissionChange(runRow)
	if runRow.WorkflowSpec == nil {
		return nil, spec.Workflow{}, "", adm, false, nil
	}
	parsed, perr := spec.ParseBytes(runRow.WorkflowSpec)
	if perr != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "applies_to plan gate: parse workflow spec failed",
			slog.String("run_id", runID.String()),
			slog.String("error", perr.Error()))
		return nil, spec.Workflow{}, "", adm, false, nil
	}
	wf, ok := parsed.Workflows[runRow.WorkflowID]
	if !ok {
		return nil, spec.Workflow{}, "", adm, false, nil
	}
	return parsed, wf, runRow.WorkflowID, adm, true, nil
}

// appliesToPlanGateRejection is the fail-closed `paths` enforcement point. It
// returns a non-empty, actionable reject reason when the run's workflow
// declares an applies_to `paths` criterion the plan's scope does not satisfy,
// and "" when the plan is acceptable, the workflow declares no paths
// criterion, or an audited admission-time override suppresses the refusal.
//
// It is called from handleShipPlan (plan.go) beside overCapSplitRejection and
// routed through the IDENTICAL terminal path — emitReviewFailed +
// run.FailStage(run.FailureB) + advanceAfterFailure, reusing the existing
// plan_review_failed category so this control adds no new audit category. The
// plan artifact is stored and audited before the gate runs, so a rejected plan
// is still readable via fishhawk_get_plan; the operator can see exactly what
// was refused.
//
// DEGRADE POSTURE IS DELIBERATELY ASYMMETRIC TO THIS FILE'S NEIGHBOURS, and
// the asymmetry is drawn at a specific line rather than applied as a mood:
//
//   - Once a declaration IS IN HAND, an evaluation failure REJECTS. A
//     spec.Predicate.Match error (empty predicate, malformed glob) and an
//     override-lookup error are both refusals. runScopePrecheck,
//     runSurfaceSweep, runTestSweep and overCapSplitRejection all correctly
//     fail OPEN on their error legs because they are ADVISORY sweeps whose
//     worst case is a missing hint; this is a governance control whose worst
//     case is an unenforced routing declaration. Writing `if err != nil {
//     return "" }` here by analogy with the four neighbours sitting in the
//     same handler is the tempting bug this comment exists to name.
//   - Where NO declaration EXISTS to resolve (nil RunRepo, run not found, nil
//     workflow spec, unparseable spec, workflow absent from the spec) the gate
//     fails OPEN, because there is no declaration to enforce and refusing every
//     plan in a deployment that cannot resolve one would be a denial of
//     service, not a control. That leg is narrow by construction: the bytes
//     parsed here are the SNAPSHOT admission already parsed successfully to
//     reach checkAppliesTo, so a parse failure at plan time is an internal
//     inconsistency rather than a reachable bypass. It is warn-logged so it is
//     visible if it ever does occur.
//   - A nil parsedPlan is a ZERO-PATH plan, not a skip. handleShipPlan hands
//     nil when the plan body could not be decoded — an internal inconsistency
//     (the bytes already passed plan.Validate), but one whose safe reading is
//     the same as an empty scope.files: a plan that demonstrates no paths has
//     not demonstrated it falls inside the declaration, so it is refused.
//     Reading nil as "nothing to check" would hand this fail-closed control a
//     fail-open leg the enumeration below does not carry.
//   - Where the declaration could not be READ — a repository error that is NOT
//     run-not-found: a transient database failure, a timeout, a reset
//     connection — the gate fails CLOSED. This is the line between "there is
//     nothing to enforce" and "we do not know what to enforce", and only the
//     first is a safe reason to admit. Collapsing the second into it would
//     leave a governance control that a repository blip switches off, and a
//     retry-shaped one at that: the failure is transient, so re-shipping the
//     plan recovers, whereas a silent admission does not.
func (s *Server) appliesToPlanGateRejection(ctx context.Context, runID uuid.UUID, parsedPlan *plan.Plan) string {
	parsed, wf, workflowID, adm, ok, rerr := s.resolveRunWorkflowDef(ctx, runID)
	if rerr != nil {
		// Fail CLOSED. Rendered as its own message rather than through
		// renderAppliesToRejection: nothing that renderer reports —
		// criterion, observed value, satisfying workflows — is knowable
		// when the declaration itself could not be read, and inventing
		// empty values for them would be a less actionable message, not a
		// more consistent one.
		return fmt.Sprintf("the applies_to routing declaration for this run could not be read (%s), so the plan is refused rather than admitted — a governance declaration we cannot read is never treated as absent. This failure is transient: ship the plan again once the backend's run store is reachable", rerr.Error())
	}
	if !ok {
		return ""
	}
	if wf.AppliesTo == nil {
		// No declaration: the workflow accepts any change (back-compat).
		return ""
	}
	sub, constrains := appliesToPhasePredicate(*wf.AppliesTo, appliesToPhasePlanGate)
	if !constrains {
		// The declaration constrains only admission-phase criteria (labels /
		// trigger), which checkAppliesTo already decided. Nothing to do here.
		return ""
	}

	paths := planGateScopePaths(parsedPlan)
	unmatched, matchErr := planGateUnmatchedPaths(sub.Paths, paths)
	if matchErr == nil && len(unmatched) == 0 && len(paths) > 0 {
		return ""
	}

	rj := appliesToRejection{
		WorkflowID:    workflowID,
		Criterion:     "paths",
		Required:      sub.Paths,
		Observed:      renderUnmatchedPaths(unmatched),
		ObservedLabel: "out-of-declaration scope.files",
		Satisfying:    planGateSatisfyingWorkflows(parsed, paths, adm),
		MatchErr:      matchErr,
		Phase:         appliesToPhasePlanGate,
	}
	if matchErr == nil && len(paths) == 0 {
		// The plan commits to no files at all, so nothing demonstrates it
		// falls inside the declaration. Name scope.files itself rather than an
		// offending entry there is none of.
		rj.ObservedLabel = "scope.files"
	}
	message := renderAppliesToRejection(rj)

	// The audited admission-time override carries forward. Its source of truth
	// is the RUN-SCOPED run_admitted_applies_to_override audit entry, never the
	// create request: a run whose creation carried an override but whose entry
	// is absent has no override here and is refused. A LOOKUP ERROR is a
	// refusal too — an override we cannot confirm is not an override.
	overridden, oerr := s.runHasAppliesToOverride(ctx, runID)
	if oerr != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"applies_to plan gate: override lookup failed; refusing (fail-closed)",
			slog.String("run_id", runID.String()),
			slog.String("workflow_id", workflowID),
			slog.String("error", oerr.Error()))
		return message
	}
	if overridden {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
			"applies_to plan gate: refusal suppressed by audited admission-time override",
			slog.String("run_id", runID.String()),
			slog.String("workflow_id", workflowID),
			slog.String("suppressed_rejection", message))
		return ""
	}
	return message
}
