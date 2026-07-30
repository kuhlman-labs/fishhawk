package server

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
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
// would accept this path set AT THE PLAN GATE — the "where can I take this
// instead?" half of the rejection message.
//
// It deliberately does NOT reuse satisfyingWorkflows (applies_to.go), which
// evaluates via Predicate.Match and is therefore EXISTENTIAL on paths. Using
// it here would let the message recommend a workflow that then refuses the
// very same plan at this very same gate — advice that costs the operator a
// second rejection. The enumeration must be quantified the same way the
// decision is, so it goes through planGateUnmatchedPaths like the gate itself.
//
// A workflow declaring no applies_to, or none that constrains this phase, is
// included: at THIS phase it accepts. A workflow whose declaration cannot be
// evaluated is excluded — a declaration we cannot evaluate must never be
// recommended.
func planGateSatisfyingWorkflows(parsed *spec.Spec, paths []string) []string {
	if parsed == nil {
		return nil
	}
	var out []string
	for name, wf := range parsed.Workflows {
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

// resolveRunWorkflowDef resolves the run's workflow definition from the
// workflow-spec SNAPSHOT stored on the run row — the same bytes admission
// parsed, not a re-read of the repo's current governance file. One immutable
// snapshot is what keeps the two enforcement points from reaching two answers
// about one run.
//
// Returns ok=false on every leg where no declaration can be resolved. Mirrors
// resolveImplementConstraints (scope_precheck.go) so the plan-time gates agree
// on how a run's workflow is resolved.
func (s *Server) resolveRunWorkflowDef(ctx context.Context, runID uuid.UUID) (*spec.Spec, spec.Workflow, string, bool) {
	if s.cfg.RunRepo == nil {
		return nil, spec.Workflow{}, "", false
	}
	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "applies_to plan gate: get run failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
		return nil, spec.Workflow{}, "", false
	}
	if runRow.WorkflowSpec == nil {
		return nil, spec.Workflow{}, "", false
	}
	parsed, perr := spec.ParseBytes(runRow.WorkflowSpec)
	if perr != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "applies_to plan gate: parse workflow spec failed",
			slog.String("run_id", runID.String()),
			slog.String("error", perr.Error()))
		return nil, spec.Workflow{}, "", false
	}
	wf, ok := parsed.Workflows[runRow.WorkflowID]
	if !ok {
		return nil, spec.Workflow{}, "", false
	}
	return parsed, wf, runRow.WorkflowID, true
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
//   - Where NO declaration can be resolved at all (nil RunRepo, GetRun error,
//     nil workflow spec, unparseable spec, workflow absent from the spec)
//     the gate fails OPEN, because there is no declaration to enforce and
//     refusing every plan in a deployment that cannot resolve one would be a
//     denial of service, not a control. That leg is narrow by construction:
//     the bytes parsed here are the SNAPSHOT admission already parsed
//     successfully to reach checkAppliesTo, so a parse failure at plan time is
//     an internal inconsistency rather than a reachable bypass. It is warn-
//     logged so it is visible if it ever does occur.
func (s *Server) appliesToPlanGateRejection(ctx context.Context, runID uuid.UUID, parsedPlan *plan.Plan) string {
	parsed, wf, workflowID, ok := s.resolveRunWorkflowDef(ctx, runID)
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
		Satisfying:    planGateSatisfyingWorkflows(parsed, paths),
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
