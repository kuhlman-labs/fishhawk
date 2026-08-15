// Package delegation evaluates the ADR-040 operator_agent delegation
// conditions (#1026) against current run state. Each v0 knob names
// exactly one backend-evaluable predicate (spec.DelegationCondition);
// the Evaluator answers "is that predicate satisfied right now" from
// the same repositories the server already reads — reviewer-verdict
// audit entries, the durable concern store, stage rows, and the drive
// engine's run_auto_advanced trail — so the operator agent never
// re-derives a condition client-side.
//
// Fail-closed by construction: a spec with no effective operator_agent
// block evaluates to nil (nothing delegated), and every unmet Decision
// names the exact failed predicate so a refusal is explainable. The
// evaluator only ANSWERS conditions; enforcement at action time (the
// delegated approve/fixup/retry/waive paths) re-evaluates through the
// same code rather than trusting a client-supplied verdict.
package delegation

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/drive"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// Actions are the delegable operator verbs, one per operator_agent
// knob. The strings are the wire values the GET /v0/runs/{id}
// delegation block carries.
const (
	ActionApprove    = "approve"
	ActionRouteFixup = "route_fixup"
	ActionWaive      = "waive"
	ActionRetry      = "retry"
	ActionMerge      = "merge"
)

// StageLister is the slice of run.Repository the evaluator needs.
type StageLister interface {
	ListStagesForRun(ctx context.Context, runID uuid.UUID) ([]*run.Stage, error)
}

// ConcernLister is the slice of concern.Repository the evaluator needs.
type ConcernLister interface {
	ListOpenByRun(ctx context.Context, runID uuid.UUID) ([]*concern.Concern, error)
}

// AuditLister is the slice of audit.Repository the evaluator needs.
type AuditLister interface {
	ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error)
}

// EscalationResolver answers "what did the run's `escalations` declaration
// raise for this change" (E53.4 / #2227). The delegation seam consumes ONE
// dimension of the answer — the `max_autonomy` CEILING — and applies it LAST,
// over the fully resolved matrix.
//
// stageID is the stage the seam is evaluating, threaded through so the
// resolver's audit de-duplication can key on (run, stage); uuid.Nil is
// legitimate (no stage is currently gated) and keys the run-level slot.
//
// The implementation lives server-side (backend/internal/server's
// escalation_gate.go) because answering needs the run's approved plan, its
// issue-label snapshot and the audit repository — none of which this package
// knows about. It is REQUIRED at construction (see NewEvaluator): the
// no-escalations case is answered INSIDE the resolver, which returns the zero
// ComposedRequirements when the workflow declares none, so "inert" is reached
// by a code path that exists rather than by a nil field nobody set.
type EscalationResolver interface {
	ResolveEscalations(ctx context.Context, runRow *run.Run, wf *spec.Workflow, stageID uuid.UUID) (spec.ComposedRequirements, error)
}

// actionClasses maps each delegable action verb to the workflow-v2 action
// CLASS that governs it (ADR-066 / #2222). The two vocabularies differ in
// exactly one place — the verb is route_fixup, the class is fixup — so the
// mapping is explicit rather than implied by string equality.
var actionClasses = map[string]string{
	ActionApprove:    spec.ActionApprove,
	ActionRouteFixup: spec.ActionFixup,
	ActionWaive:      spec.ActionWaive,
	ActionRetry:      spec.ActionRetry,
	ActionMerge:      spec.ActionMerge,
}

// actionConditions is each action verb's single backend-evaluable
// condition — the same registry spec.classConditions holds class-side. It
// is what lets a `mode: report` entry's declared `when` be CHECKED against
// the class it sits on: an entry naming another class's condition names a
// predicate this evaluator would not be answering, so it is refused rather
// than silently answered with the wrong one.
var actionConditions = map[string]spec.DelegationCondition{
	ActionApprove:    spec.ConditionCleanDualApproval,
	ActionRouteFixup: spec.ConditionConvergentConcerns,
	ActionWaive:      spec.ConditionSoloLow,
	ActionRetry:      spec.ConditionInfraFlake,
	ActionMerge:      spec.ConditionGatesResolvedCIGreen,
}

// ActionForClass maps a workflow-v2 action CLASS name back to the
// delegation action verb it governs, reporting false for an extension
// class — a class name no known verb backs, which therefore has no
// backend-observable gate and no evaluable condition. Exposed so the
// auto-driver's `mode: report` arm speaks one vocabulary (the verb) while
// reading the matrix, without re-deriving the mapping.
func ActionForClass(class string) (string, bool) {
	for action, c := range actionClasses {
		if c == class {
			return action, true
		}
	}
	return "", false
}

// Decision is one knob's evaluation: whether the named condition is
// satisfied by current run state, and — when it is not — the exact
// failed predicate, prefixed with the condition name (e.g.
// "clean_dual_approval: 1 of 2 reviewer verdicts received").
//
// Mode and Source carry the workflow-v2 provenance of the class this
// decision came from (ADR-066 / #2222): which mode resolved it and which
// input decided that mode. Both are EMPTY for a v0/v1 knob block, which has
// no matrix — the decision itself is unchanged.
type Decision struct {
	Action      string
	Condition   spec.DelegationCondition
	Met         bool
	UnmetReason string
	Mode        spec.ActionMode
	Source      spec.ResolutionSource
}

// Result carries every configured knob's Decision plus the effective
// block's must_page_human event list (static configuration, surfaced
// alongside the evaluations so the operator agent reads its full
// envelope in one response).
type Result struct {
	Actions       []Decision
	MustPageHuman []string
	// Tier is the workflow-v2 autonomy tier the effective block declared
	// (ADR-066 / #2222), empty when the block declared only `actions`, when
	// the effective block is a campaign override, and for every v0/v1 knob
	// block (which has no tier).
	Tier spec.AutonomyTier
	// Matrix is the RESOLVED action matrix: every action class with its
	// mode, condition, min_severity and per-class provenance source — the
	// legibility surface AC9 wants, and the input the auto-driver's
	// `mode: report` arm walks.
	//
	// The DECISION SET (Actions) is deliberately INDEPENDENT of it: Actions
	// still carries one entry per class the effective block actually
	// DELEGATES, so an all-`gated` matrix yields ZERO decisions exactly as a
	// knob-absent v0 block does. The matrix is what changed; what is
	// delegated did not.
	//
	// nil when no matrix governs the run — a v0/v1 workflow knob block with
	// no campaign override, and (deliberately) a run parked at
	// awaiting_input, which delegates and reports nothing.
	Matrix []spec.ResolvedAction
	// Reports carries the evaluation of every `mode: report` class that
	// declared a `when` condition. It is kept OUT of Actions because a
	// report class delegates nothing — it is a proposal surface, not
	// authority — so folding it into the decision set would make an
	// all-report matrix look delegated. A report class with no `when` has no
	// entry here: per the report-firing rule it surfaces on gate-live alone.
	//
	// A `when` that is not the class's own condition produces NO entry
	// either (fail-closed): this evaluator would be answering a different
	// predicate than the one the author named.
	Reports []Decision
	// ReviewerRejectClass names the reviewer-reject page-event class the
	// run's implement review currently resolves to (#1378): the explicit
	// spec.PageEventGatingReviewerReject when implement review authority is
	// gating (a reject pages the human), spec.PageEventAdvisoryReviewerReject
	// when advisory (a reject is arbitrable / auto-routed), and "" when the
	// implement stage is gateless (no agent-reviewer authority — omitted).
	// This only makes the authority-resolved class legible; it does not
	// change the page/auto decision, which stays resolved from
	// implementReviewAuthority.
	ReviewerRejectClass string
	// ModelPolicy is the effective operator_agent block's scenario-A
	// model-selection contract (#1421), surfaced as unevaluated static
	// config alongside MustPageHuman so the operator agent reads its full
	// envelope in one response. Passthrough only — no condition is
	// evaluated here; the operator agent applies it via #1416's per-stage
	// override channels, bounded by the deployment allow-list. nil when
	// the effective block declares no model_policy.
	ModelPolicy *spec.ModelPolicy
}

// Evaluator answers delegation conditions over the server's existing
// repository surfaces. All FOUR dependencies are required; the caller
// (handleGetRun, the delegated-action handlers, the auto-drive gate) guards
// nil wiring and degrades by omitting the surface / refusing the action.
//
// THE FIELDS ARE UNEXPORTED ON PURPOSE (E53.4 / #2227). Go forbids a composite
// literal from setting a non-exported field of a struct in another package
// (https://go.dev/ref/spec#Composite_literals), so `&delegation.Evaluator{…}`
// is a COMPILE ERROR everywhere outside this package and NewEvaluator is the
// only way in. That is what makes an Evaluator which cannot clamp
// UNCONSTRUCTIBLE rather than merely discouraged: the enforcement site someone
// adds six months from now cannot compile without supplying an escalation
// resolver, so the "we forgot to wire the ceiling at this seam" bug class — the
// one that left the auto-drive gate evaluating unclamped — is closed by the
// type system instead of by review.
//
// Honestly stated: the compile-time guarantee binds every OTHER package. A
// same-package literal is still possible, which is why this package's own
// tests construct through NewEvaluator too. The three enforcement sites all
// live in backend/internal/server, so the guarantee covers all of them.
type Evaluator struct {
	stages      StageLister
	concerns    ConcernLister
	audit       AuditLister
	escalations EscalationResolver
}

// NewEvaluator is the ONLY constructor. It returns an error naming any nil
// dependency, INCLUDING the escalation resolver — there is deliberately no
// optional/nil-means-inert resolver and no exported no-op resolver to reach
// for, because both re-open the bug class the unexported fields close.
func NewEvaluator(stages StageLister, concerns ConcernLister, audit AuditLister, escalations EscalationResolver) (*Evaluator, error) {
	switch {
	case stages == nil:
		return nil, fmt.Errorf("delegation: new evaluator: stages lister is required")
	case concerns == nil:
		return nil, fmt.Errorf("delegation: new evaluator: concerns lister is required")
	case audit == nil:
		return nil, fmt.Errorf("delegation: new evaluator: audit lister is required")
	case escalations == nil:
		return nil, fmt.Errorf("delegation: new evaluator: escalation resolver is required (an evaluator that cannot clamp the autonomy ceiling would silently under-enforce a fired escalation)")
	}
	return &Evaluator{stages: stages, concerns: concerns, audit: audit, escalations: escalations}, nil
}

// Decision returns the already-computed Decision for the named action
// (one of the Action* constants) and true, or a zero Decision and
// false when the Result evaluated no knob for that action — a nil
// Result is treated as "nothing delegated" and returns false.
//
// This is a READ-ONLY lookup over the Result a single Evaluate
// produced: it performs no repository reads, evaluates no condition,
// and does not mutate the receiver. The campaign auto-driver actor
// (E25.6 / ADR-047) uses it to find the knob governing the current
// gate from one Evaluate call without re-deriving any predicate
// client-side — the same fail-closed discipline checkDelegation
// applies at the HTTP action handlers.
func (r *Result) Decision(action string) (Decision, bool) {
	if r == nil {
		return Decision{}, false
	}
	for _, d := range r.Actions {
		if d.Action == action {
			return d, true
		}
	}
	return Decision{}, false
}

// Report returns the already-computed report-mode evaluation for the named
// action verb and true, or a zero Decision and false when the resolved
// matrix declares no evaluable `mode: report` entry for it (including a
// nil Result). Read-only over one Evaluate, like Decision.
func (r *Result) Report(action string) (Decision, bool) {
	if r == nil {
		return Decision{}, false
	}
	for _, d := range r.Reports {
		if d.Action == action {
			return d, true
		}
	}
	return Decision{}, false
}

// MatrixEntry returns the resolved matrix entry for a workflow-v2 action
// CLASS name and true, or false when the run resolves no matrix or the
// class is absent from it.
func (r *Result) MatrixEntry(class string) (spec.ResolvedAction, bool) {
	if r == nil {
		return spec.ResolvedAction{}, false
	}
	for _, a := range r.Matrix {
		if a.Action == class {
			return a, true
		}
	}
	return spec.ResolvedAction{}, false
}

// MergeCondition is the delegation condition the may_merge knob names
// (ConditionGatesResolvedCIGreen). Exposed so the auto-driver actor can
// reference the merge knob's required condition without re-importing the
// spec constant — keeping the actor's knob→condition knowledge sourced
// from the delegation package that owns the mapping.
func MergeCondition() spec.DelegationCondition { return spec.ConditionGatesResolvedCIGreen }

// Configured reports whether the workflow declares an operator_agent
// block anywhere — workflow level or on any stage gate. A false answer
// lets callers skip Evaluate entirely (no repository reads), keeping
// unconfigured specs' responses byte-identical to today.
func Configured(wf *spec.Workflow) bool {
	if wf == nil {
		return false
	}
	if wf.OperatorAgent != nil {
		return true
	}
	for _, st := range wf.Stages {
		for _, g := range st.Gates {
			if g.OperatorAgent != nil {
				return true
			}
		}
	}
	return false
}

// Evaluate resolves the effective operator_agent block for the run's
// current gate context and evaluates every configured knob. Returns
// (nil, nil) when no block governs the run — the fail-closed default:
// nothing is delegated and the caller omits the surface. Any
// repository read failure returns an error so the caller can apply its
// best-effort degradation (warn-log + omit), never a partial answer.
//
// campaignOverride is the OPTIONAL campaign-level operator_agent block
// (E25.12 / #1451): when non-nil it becomes the effective block
// WHOLESALE — the outermost rung of the campaign > gate > workflow
// ladder (spec.ResolveOperatorAgent) — so a campaign's issue-runs
// resolve their delegation against the campaign block, never merged with
// the per-workflow contract. nil means no campaign context (or the
// campaign declares no override): resolution falls through to the
// workflow's own EffectiveOperatorAgent, byte-identical to today.
func (e *Evaluator) Evaluate(ctx context.Context, runRow *run.Run, wf *spec.Workflow, campaignOverride *spec.OperatorAgent) (*Result, error) {
	// A campaign override governs even a workflow that declares no block of
	// its own, so the cheap Configured short-circuit only applies when there
	// is no override to consider.
	if campaignOverride == nil && !Configured(wf) {
		return nil, nil
	}

	stages, err := e.stages.ListStagesForRun(ctx, runRow.ID)
	if err != nil {
		return nil, fmt.Errorf("list stages: %w", err)
	}
	gated := currentGatedStage(stages)
	gate := approvalGateForStage(wf, gated)
	effective := spec.ResolveOperatorAgent(campaignOverride, wf, gate)
	if effective == nil {
		return nil, nil
	}
	matrix := resolveMatrix(campaignOverride, wf, gate)

	// THE ESCALATION CEILING IS APPLIED LAST (E53.4 / #2227), and "last" is
	// load-bearing rather than incidental. By this point the workflow tier has
	// resolved AND every explicit `actions` override has already won AND a
	// campaign override — the outermost rung — has been projected into the
	// matrix. Clamping here means the ceiling wins over all three; clamping the
	// tier or the declared entries instead would let an explicit
	// `actions: {merge: {mode: auto}}` re-widen the class afterwards, which is
	// the exact failure the acceptance criteria name.
	//
	// The DERIVED knob block is re-derived from the clamped matrix, not merely
	// the surfaced one: every enforcement site downstream (this function's five
	// knob evaluators, the four checkDelegation handlers, AutoDriveRunGate, the
	// campaign driver) reads the derived *OperatorAgent, so clamping only the
	// matrix would show `gated` on the run read while the agent stayed
	// authorized to act.
	//
	// A resolver ERROR is returned, never swallowed: each caller's existing
	// degradation (omit the delegation block / 500 / observe-only) delegates
	// nothing, so that mode is already fail-closed.
	//
	// A nil matrix means no matrix governs the run (a v0/v1 knob block with no
	// campaign override). `escalations` is a workflow-v2-only property, so such
	// a run can carry no declaration and there is nothing to clamp.
	var gatedStageID uuid.UUID
	if gated != nil {
		gatedStageID = gated.ID
	}
	req, err := e.escalations.ResolveEscalations(ctx, runRow, wf, gatedStageID)
	if err != nil {
		return nil, fmt.Errorf("resolve escalations: %w", err)
	}
	if req.MaxAutonomy != "" && matrix != nil {
		matrix = spec.ClampResolvedMatrix(matrix, req.MaxAutonomy)
		if derived := spec.DerivedOperatorAgent(matrix); derived != nil {
			effective = derived
		}
	}

	// A stage parked at awaiting_input (#1057) is waiting on a human to
	// answer the planner's clarification_request — a parked D-category
	// judgment, not a failure and not a delegable agent decision. While
	// the run is parked for direction the operator agent must page the
	// human rather than act, so delegate nothing: surface only the
	// effective block's must_page_human envelope with zero met actions.
	// This is fail-closed by intent — without it a stale open concern
	// could still satisfy a knob (e.g. solo_low) while the run is
	// genuinely blocked on operator answers.
	//
	// The resolved MATRIX is deliberately omitted on this path too: while the
	// run is parked for direction nothing is delegated AND nothing is
	// proposed, and the matrix is what the auto-driver's report arm walks —
	// leaving it nil is what keeps a report proposal from surfacing on a run
	// that is blocked on the human it would be proposing to.
	rejectClass := reviewerRejectClass(wf)
	if parkedAwaitingInput(stages) {
		return &Result{MustPageHuman: effective.MustPageHuman, ReviewerRejectClass: rejectClass, ModelPolicy: effective.ModelPolicy}, nil
	}

	open, err := e.concerns.ListOpenByRun(ctx, runRow.ID)
	if err != nil {
		return nil, fmt.Errorf("list open concerns: %w", err)
	}

	res := &Result{MustPageHuman: effective.MustPageHuman, ReviewerRejectClass: rejectClass, ModelPolicy: effective.ModelPolicy}
	if matrix != nil {
		res.Tier = matrix.Tier
		res.Matrix = matrix.Actions
	}
	type knob struct {
		action    string
		condition spec.DelegationCondition
		eval      func() (bool, string, error)
	}
	knobs := []knob{
		{ActionApprove, effective.MayApprove, func() (bool, string, error) {
			return e.evalCleanDualApproval(ctx, runRow, wf, gated, open)
		}},
		{ActionRouteFixup, effective.MayRouteFixup, func() (bool, string, error) {
			return e.evalConvergentConcerns(ctx, runRow, wf, effective, open)
		}},
		{ActionWaive, effective.MayWaive, func() (bool, string, error) {
			return evalSoloLow(open), soloLowUnmetReason(open), nil
		}},
		{ActionRetry, effective.MayRetry, func() (bool, string, error) {
			met, reason := evalInfraFlake(stages)
			return met, reason, nil
		}},
		{ActionMerge, effective.MayMerge, func() (bool, string, error) {
			return e.evalGatesResolvedCIGreen(ctx, runRow, stages, open)
		}},
	}
	for _, k := range knobs {
		entry, hasEntry := res.MatrixEntry(actionClasses[k.action])
		reportCond := reportCondition(k.action, entry, hasEntry)
		if k.condition == "" && reportCond == "" {
			continue
		}
		cond := k.condition
		if cond == "" {
			cond = reportCond
		}
		met, reason, err := k.eval()
		if err != nil {
			return nil, fmt.Errorf("evaluate %s: %w", cond, err)
		}
		d := Decision{Action: k.action, Condition: cond, Met: met, Mode: entry.Mode, Source: entry.Source}
		if !met {
			d.UnmetReason = reason
		}
		if k.condition != "" {
			res.Actions = append(res.Actions, d)
			continue
		}
		// A report class delegates nothing, so its evaluation lands in
		// Reports and NEVER in the decision set.
		res.Reports = append(res.Reports, d)
	}
	return res, nil
}

// reportCondition returns the condition a `mode: report` entry declared for
// the action, or "" when the class is not at report mode, declared no
// `when` (it fires on gate-live alone), or named a condition that is not
// this class's own — the last case fail-closed, because the evaluator would
// otherwise answer a predicate the author did not name.
func reportCondition(action string, entry spec.ResolvedAction, hasEntry bool) spec.DelegationCondition {
	if !hasEntry || entry.Mode != spec.ModeReport || entry.Condition == "" {
		return ""
	}
	if entry.Condition != actionConditions[action] {
		return ""
	}
	return entry.Condition
}

// resolveMatrix resolves the action matrix governing the run's gate, on the
// same outermost-wins ladder the effective block resolves on.
//
// A campaign-level override (E25.12) is an operator_agent-shaped blob, not a
// grammar version, so it is PROJECTED into a matrix: every knob it sets
// reads as `mode: auto` and every knob it leaves empty as `mode: gated`,
// with no tier and every class Source=explicit (the campaign named the whole
// block itself). Below that rung the workflow/gate blocks resolve through
// the SAME spec.ResolveAutonomy ladder parse time used, so the surfaced
// matrix cannot drift from the derived block the enforcement sites read.
//
// nil means no matrix governs the run: a v0/v1 knob block with no override
// (which has no matrix to resolve — its knobs are the whole grammar).
func resolveMatrix(campaignOverride *spec.OperatorAgent, wf *spec.Workflow, gate *spec.Gate) *spec.ResolvedMatrix {
	if campaignOverride != nil {
		return matrixFromOperatorAgent(campaignOverride)
	}
	return spec.ResolveAutonomy(wf, gate)
}

// matrixFromOperatorAgent projects a v0/v1-shaped operator_agent block onto
// a resolved matrix — DerivedOperatorAgent's inverse, used for the campaign
// override rung.
func matrixFromOperatorAgent(oa *spec.OperatorAgent) *spec.ResolvedMatrix {
	if oa == nil {
		return nil
	}
	out := &spec.ResolvedMatrix{PageHumanOn: oa.MustPageHuman, ModelPolicy: oa.ModelPolicy}
	knobs := []struct {
		class     string
		condition spec.DelegationCondition
	}{
		{spec.ActionApprove, oa.MayApprove},
		{spec.ActionFixup, oa.MayRouteFixup},
		{spec.ActionWaive, oa.MayWaive},
		{spec.ActionRetry, oa.MayRetry},
		{spec.ActionMerge, oa.MayMerge},
	}
	for _, k := range knobs {
		a := spec.ResolvedAction{Action: k.class, Mode: spec.ModeGated, Source: spec.SourceExplicit}
		if k.condition != "" {
			a.Mode = spec.ModeAuto
			a.Condition = k.condition
		}
		if k.class == spec.ActionFixup {
			a.MinSeverity = oa.RouteFixupMinSeverity
		}
		out.Actions = append(out.Actions, a)
	}
	return out
}

// parkedAwaitingInput reports whether any stage is parked at
// awaiting_input — the planner's clarification_request gate (#1057), a
// parked D-category judgment that pages the human rather than delegating.
func parkedAwaitingInput(stages []*run.Stage) bool {
	for _, st := range stages {
		if st.State == run.StageStateAwaitingInput {
			return true
		}
	}
	return false
}

// currentGatedStage returns the lowest-sequence stage parked in
// awaiting_approval, or nil when no gate is pending.
func currentGatedStage(stages []*run.Stage) *run.Stage {
	var gated *run.Stage
	for _, st := range stages {
		if st.State != run.StageStateAwaitingApproval {
			continue
		}
		if gated == nil || st.Sequence < gated.Sequence {
			gated = st
		}
	}
	return gated
}

// specStageFor finds the workflow's stage definition for a stage row,
// matching by spec stage ID first and falling back to type — the same
// two-step resolveSpecStageForRun (approvals.go) applies.
func specStageFor(wf *spec.Workflow, stageType run.StageType) *spec.Stage {
	for i := range wf.Stages {
		if wf.Stages[i].ID == string(stageType) {
			return &wf.Stages[i]
		}
	}
	for i := range wf.Stages {
		if string(wf.Stages[i].Type) == string(stageType) {
			return &wf.Stages[i]
		}
	}
	return nil
}

// approvalGateForStage returns the spec approval gate governing the
// gated stage row, or nil (no pending gate, stage not in spec, or no
// approval gate declared) — in which case the workflow-level block is
// the effective one.
func approvalGateForStage(wf *spec.Workflow, gated *run.Stage) *spec.Gate {
	if gated == nil {
		return nil
	}
	st := specStageFor(wf, gated.Type)
	if st == nil {
		return nil
	}
	for i := range st.Gates {
		if st.Gates[i].Type == spec.GateTypeApproval {
			return &st.Gates[i]
		}
	}
	return nil
}

// reviewCategories maps a stage type to its review-audit category pair.
// Only plan and implement stages have an agent-review surface; review
// stages' approval is GitHub-owned (ADR-018).
func reviewCategories(t run.StageType) (started, reviewed string, ok bool) {
	switch t {
	case run.StageTypePlan:
		return "plan_review_started", "plan_reviewed", true
	case run.StageTypeImplement:
		return "implement_review_started", "implement_reviewed", true
	}
	return "", "", false
}

// reviewRound reads the LATEST review round for a stage type: how many
// agents it was configured with (the *_review_started payload's
// configured_agents, falling back to the spec's reviewers when the
// entry predates #600 or is malformed) and the verdicts that landed
// after the round opened. Rounds are delimited by started entries —
// the same supersession rule the drive engine's settlement read uses,
// so a settled first round never satisfies a condition while a fix-up
// re-review is in flight.
func (e *Evaluator) reviewRound(ctx context.Context, runRow *run.Run, wf *spec.Workflow, stageType run.StageType) (configured int, verdicts []planreview.Verdict, started bool, err error) {
	startedCat, reviewedCat, ok := reviewCategories(stageType)
	if !ok {
		return 0, nil, false, fmt.Errorf("stage type %q has no reviewer surface", stageType)
	}
	startedEntries, err := e.audit.ListForRunByCategory(ctx, runRow.ID, startedCat)
	if err != nil {
		return 0, nil, false, fmt.Errorf("list %s: %w", startedCat, err)
	}

	specConfigured := 0
	if st := specStageFor(wf, stageType); st != nil && st.Reviewers != nil {
		specConfigured = st.Reviewers.AgentCount()
	}
	if len(startedEntries) == 0 {
		return specConfigured, nil, false, nil
	}

	latest := startedEntries[0]
	for _, en := range startedEntries {
		if en.Sequence > latest.Sequence {
			latest = en
		}
	}
	var startedPayload planreview.ReviewStartedPayload
	if json.Unmarshal(latest.Payload, &startedPayload) == nil {
		configured = startedPayload.ConfiguredAgents
	}
	if configured == 0 {
		configured = specConfigured
	}

	reviewedEntries, err := e.audit.ListForRunByCategory(ctx, runRow.ID, reviewedCat)
	if err != nil {
		return 0, nil, false, fmt.Errorf("list %s: %w", reviewedCat, err)
	}
	for _, en := range reviewedEntries {
		if en.Sequence <= latest.Sequence {
			continue
		}
		// PlanReviewedPayload and ImplementReviewedPayload share the
		// verdict field; either decodes the slice this read needs.
		var p planreview.ImplementReviewedPayload
		if json.Unmarshal(en.Payload, &p) != nil || p.Verdict == "" {
			continue
		}
		verdicts = append(verdicts, p.Verdict)
	}
	return configured, verdicts, true, nil
}

// evalCleanDualApproval answers may_approve's condition: every
// configured reviewer for the currently gated stage returned an
// approve verdict and zero concerns are open. A failed or skipped
// review never counts as an approve — the condition requires actual
// clean verdicts, fail-closed.
func (e *Evaluator) evalCleanDualApproval(ctx context.Context, runRow *run.Run, wf *spec.Workflow, gated *run.Stage, open []*concern.Concern) (bool, string, error) {
	const cond = string(spec.ConditionCleanDualApproval)
	if gated == nil {
		return false, cond + ": no stage is awaiting approval", nil
	}
	if _, _, ok := reviewCategories(gated.Type); !ok {
		return false, fmt.Sprintf("%s: stage type %q has no reviewer surface (review-stage approval is GitHub-owned per ADR-018)", cond, gated.Type), nil
	}
	configured, verdicts, started, err := e.reviewRound(ctx, runRow, wf, gated.Type)
	if err != nil {
		return false, "", err
	}
	if configured == 0 {
		return false, cond + ": no agent reviewers configured for the gated stage (the condition requires reviewer verdicts)", nil
	}
	if !started {
		return false, fmt.Sprintf("%s: 0 of %d reviewer verdicts received (review round not dispatched)", cond, configured), nil
	}
	if len(verdicts) < configured {
		return false, fmt.Sprintf("%s: %d of %d reviewer verdicts received", cond, len(verdicts), configured), nil
	}
	for _, v := range verdicts {
		if v != planreview.VerdictApprove {
			return false, fmt.Sprintf("%s: reviewer verdict %s (every verdict must be approve)", cond, v), nil
		}
	}
	if n := len(open); n > 0 {
		return false, fmt.Sprintf("%s: %d open concern(s)", cond, n), nil
	}
	return true, "", nil
}

// implementReviewAuthority resolves the ADR-027 reviewer authority
// (planreview.ResolveAuthority) for the implement stage's review round:
// advisory when agent AND human reviewers are configured (the human
// approver is the authoritative gate), gating when agent-only. A stage
// with no Reviewers block — or absent from the spec entirely — is
// gateless: no agent-reviewer authority governs the verdict, so a reject
// can only be advisory.
func implementReviewAuthority(wf *spec.Workflow) planreview.AuthorityMode {
	st := specStageFor(wf, run.StageTypeImplement)
	if st == nil || st.Reviewers == nil {
		return planreview.AuthorityGateless
	}
	return planreview.ResolveAuthority(*st.Reviewers)
}

// reviewerRejectClass maps the implement-stage review authority (#1378)
// to the legible reviewer-reject page-event class surfaced on the wire:
// gating authority -> spec.PageEventGatingReviewerReject (a reject pages
// the human), advisory -> spec.PageEventAdvisoryReviewerReject (a reject
// is arbitrable / auto-routed), and gateless -> "" (no agent-reviewer
// authority; omitted). This is the same authority resolution the
// page/auto decision uses — it only makes the resolved class explicit.
func reviewerRejectClass(wf *spec.Workflow) string {
	switch implementReviewAuthority(wf) {
	case planreview.AuthorityGating:
		return spec.PageEventGatingReviewerReject
	case planreview.AuthorityAdvisory:
		return spec.PageEventAdvisoryReviewerReject
	default:
		return ""
	}
}

// severityRank maps a planreview.ConcernSeverity string to its ordinal
// rank (low=1, medium=2, high=3). Anything unrecognized ranks 0 — below
// low — so a malformed/legacy severity row parks the gate rather than
// spending a fix-up pass (fail-closed). The closed set is
// planreview.AllConcernSeverities, enforced at verdict decode; an
// out-of-set value can only arrive via a legacy/malformed concern row.
func severityRank(severity string) int {
	switch planreview.ConcernSeverity(severity) {
	case planreview.SeverityLow:
		return 1
	case planreview.SeverityMedium:
		return 2
	case planreview.SeverityHigh:
		return 3
	default:
		return 0
	}
}

// routeFixupThreshold resolves the minimum open-concern severity RANK that
// satisfies convergent_concerns when every implement-review verdict is
// approve-class (#1964). It reads effective.RouteFixupMinSeverity and
// defaults to medium (rank 2) when the value is absent OR out of the
// closed low/medium/high enum — the out-of-enum case is reachable only via
// campaign-override bytes, which bypass JSON-schema validation, so the
// resolver defends against it by falling back to the documented default
// rather than routing on a value the schema would have rejected. A nil
// block also resolves to medium (the delegation-configured default).
func routeFixupThreshold(effective *spec.OperatorAgent) int {
	if effective != nil {
		if r := severityRank(effective.RouteFixupMinSeverity); r > 0 {
			return r
		}
	}
	return severityRank(string(planreview.SeverityMedium))
}

// evalConvergentConcerns answers may_route_fixup's condition: the
// implement-review round's verdicts are all in, no GATING-authority
// reject is present, and at least one concern is open to route. Pinned
// to the implement stage because fix-up routing is an implement-stage
// verb.
//
// The reject branch is ADR-027 authority-aware. A planreview.VerdictReject
// disqualifies route_fixup and pages the human (reviewer_reject) ONLY
// under AuthorityGating (agent-only review). Under AuthorityAdvisory the
// human approver is the gate, so an agent reject is advisory and
// arbitrable: it does NOT disqualify, and with an open concern the
// condition stays met so the operator agent may auto-route the fix-up.
// A human reviewer reject is not an implement_reviewed verdict this
// evaluator reads — it arrives via plan_rejection / gate rejection, which
// already pages — so reviewer_reject here means a gating-authority agent
// reject specifically.
//
// Severity/verdict-aware tune (#1964): when NO reject verdict is present
// (every verdict is approve-class), a fix-up auto-routes only when at
// least one open concern ranks at or above the route_fixup_min_severity
// threshold (default medium). A round whose sole open concern is below the
// threshold parks for the operator instead of spending a full fix-up pass.
// A reject verdict (necessarily advisory authority here, since a gating
// reject already returned above) BYPASSES the threshold: the ADR-027
// arbitration path stays met regardless of severities.
func (e *Evaluator) evalConvergentConcerns(ctx context.Context, runRow *run.Run, wf *spec.Workflow, effective *spec.OperatorAgent, open []*concern.Concern) (bool, string, error) {
	const cond = string(spec.ConditionConvergentConcerns)
	configured, verdicts, started, err := e.reviewRound(ctx, runRow, wf, run.StageTypeImplement)
	if err != nil {
		return false, "", err
	}
	if !started || configured == 0 {
		return false, cond + ": no implement review round recorded", nil
	}
	if len(verdicts) < configured {
		return false, fmt.Sprintf("%s: %d of %d reviewer verdicts received", cond, len(verdicts), configured), nil
	}
	gating := implementReviewAuthority(wf) == planreview.AuthorityGating
	hasReject := false
	for _, v := range verdicts {
		if v == planreview.VerdictReject {
			if gating {
				return false, cond + ": a gating-authority reviewer rejected (" + spec.PageEventGatingReviewerReject + " pages the human)", nil
			}
			hasReject = true
		}
	}
	if len(open) == 0 {
		return false, cond + ": 0 open concerns to route", nil
	}
	// With every verdict approve-class (no reject to arbitrate), only route
	// when an open concern meets the severity threshold; otherwise park.
	if !hasReject {
		threshold := routeFixupThreshold(effective)
		maxRank := 0
		for _, c := range open {
			if r := severityRank(c.Severity); r > maxRank {
				maxRank = r
			}
		}
		if maxRank < threshold {
			return false, fmt.Sprintf("%s: all verdicts approve and every open concern is below the route_fixup_min_severity threshold (%s); parking for the operator (waive, defer, or route deliberately)", cond, thresholdName(threshold)), nil
		}
	}
	return true, "", nil
}

// thresholdName renders a severity rank back to its low/medium/high name
// for the unmet reason. Ranks outside the closed set render as medium (the
// resolver's default), keeping the reason honest about the effective bar.
func thresholdName(rank int) string {
	switch rank {
	case 1:
		return string(planreview.SeverityLow)
	case 3:
		return string(planreview.SeverityHigh)
	default:
		return string(planreview.SeverityMedium)
	}
}

// evalSoloLow answers may_waive's condition: exactly one open concern
// and its severity is low.
func evalSoloLow(open []*concern.Concern) bool {
	return len(open) == 1 && open[0].Severity == string(planreview.SeverityLow)
}

// soloLowUnmetReason names the failed solo_low predicate. Empty when met.
func soloLowUnmetReason(open []*concern.Concern) string {
	const cond = string(spec.ConditionSoloLow)
	switch {
	case evalSoloLow(open):
		return ""
	case len(open) != 1:
		return fmt.Sprintf("%s: %d open concerns (the condition requires exactly one)", cond, len(open))
	default:
		return fmt.Sprintf("%s: the open concern's severity is %s (the condition requires low)", cond, open[0].Severity)
	}
}

// infraFlakeMarkers are the container-start markers of the
// testcontainers start-timeout signature (#972; the mirror was widened
// alongside the runner's by #2718 — see hasInfraFlakeSignature). The set
// mirrors the runner's isTestcontainersStartFlake matcher — the single emit site
// for the flake classification: a category-A verify failure's
// FailureReason embeds the verify output verbatim ("verify command %q
// still failing after %d iteration(s):\n<output>"), so the signature
// in that output IS the recorded evidence. The literal trace-event
// name is also accepted in case a future reason cites it directly
// (the posture the MCP next-actions classifier already takes).
var infraFlakeMarkers = []string{
	"/var/run/docker.sock",
	"%2Fvar%2Frun%2Fdocker.sock",
	"failed to start container",
	"mapped port",
	"wait until ready",
}

// testcontainersPortNotFoundRe mirrors the runner's matcher of
// testcontainers-go's DockerContainer.MappedPort port-not-found
// rendering (#2718): `port "9000/tcp" not found`. Requiring digits, a
// slash and a lowercase protocol inside the quotes keeps ordinary prose
// naming a non-numeric or unquoted port from matching, and the leading
// `\b` makes `port` the literal word rather than an unbounded substring
// (without it `airport "9000/tcp" not found` would classify as a flake).
var testcontainersPortNotFoundRe = regexp.MustCompile(`\bport "[0-9]+/[a-z]+" not found`)

// dockerDaemonUnavailableMarkers mirror the runner's daemon-unreachable
// marker set (#2718), itself kept in spirit with the isDockerUnavailable
// helpers in backend/internal/pgtest, backend/internal/postgres and
// backend/internal/tracestore.
var dockerDaemonUnavailableMarkers = []string{
	"cannot connect to the docker daemon",
	"is the docker daemon running",
	"dial unix /var/run/docker.sock",
}

// hasInfraFlakeSignature reports whether a failure reason carries the
// infra-flake classification: the literal verify_infra_flake_retry
// marker, the conservative testcontainers start signature ("context
// deadline exceeded" AND a container-start marker — an ordinary test
// failure that merely mentions a deadline never matches), the
// testcontainers port-not-found rendering, or a daemon-unreachable
// marker. The last two mirror the runner's #2718 widening; the two live
// in different Go modules and cannot share a fixture, so each is pinned
// against the same corpus of verbatim observed outputs.
func hasInfraFlakeSignature(reason string) bool {
	if strings.Contains(reason, "verify_infra_flake_retry") {
		return true
	}
	if hasTestcontainersStartSignature(reason) {
		return true
	}
	if testcontainersPortNotFoundRe.MatchString(reason) {
		return true
	}
	lowered := strings.ToLower(reason)
	for _, marker := range dockerDaemonUnavailableMarkers {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	return false
}

// hasTestcontainersStartSignature is the #972 container-start-timeout
// half: "context deadline exceeded" AND at least one container-start
// marker.
func hasTestcontainersStartSignature(reason string) bool {
	if !strings.Contains(reason, "context deadline exceeded") {
		return false
	}
	for _, marker := range infraFlakeMarkers {
		if strings.Contains(reason, marker) {
			return true
		}
	}
	return false
}

// evalInfraFlake answers may_retry's condition: the run's latest
// failed stage is a category-A failure whose recorded reason carries
// the infra-flake signature.
func evalInfraFlake(stages []*run.Stage) (bool, string) {
	const cond = string(spec.ConditionInfraFlake)
	var failed *run.Stage
	for _, st := range stages {
		if st.State != run.StageStateFailed {
			continue
		}
		if failed == nil || st.Sequence > failed.Sequence {
			failed = st
		}
	}
	if failed == nil {
		return false, cond + ": no failed stage on the run"
	}
	if failed.FailureCategory == nil || string(*failed.FailureCategory) != "A" {
		got := "unrecorded"
		if failed.FailureCategory != nil {
			got = string(*failed.FailureCategory)
		}
		return false, fmt.Sprintf("%s: failed stage category is %s (the condition requires a category-A failure)", cond, got)
	}
	if failed.FailureReason == nil || !hasInfraFlakeSignature(*failed.FailureReason) {
		return false, cond + ": the failure reason carries no infra-flake signature"
	}
	return true, ""
}

// evalGatesResolvedCIGreen answers may_merge's condition: the latest
// drive auto-advance is checks_green_awaiting_merge (review evidence
// terminal + required checks green, per the drive engine's stamp), the
// PR is open on the row, no concern is open, and no stage is parked at
// an approval gate. Evaluated and surfaced only — v0 has no backend
// merge endpoint to enforce it on; enforcement attaches when a merge
// action surface exists.
func (e *Evaluator) evalGatesResolvedCIGreen(ctx context.Context, runRow *run.Run, stages []*run.Stage, open []*concern.Concern) (bool, string, error) {
	const cond = string(spec.ConditionGatesResolvedCIGreen)
	entries, err := e.audit.ListForRunByCategory(ctx, runRow.ID, drive.Category)
	if err != nil {
		return false, "", fmt.Errorf("list %s: %w", drive.Category, err)
	}
	var latest *audit.Entry
	for _, en := range entries {
		if latest == nil || en.Sequence > latest.Sequence {
			latest = en
		}
	}
	if latest == nil {
		return false, cond + ": no checks_green_awaiting_merge auto-advance recorded", nil
	}
	var adv drive.Advance
	if json.Unmarshal(latest.Payload, &adv) != nil || adv.Rule != drive.RuleChecksGreenAwaitingMerge {
		return false, cond + ": the latest auto-advance is not checks_green_awaiting_merge", nil
	}
	if runRow.PullRequestURL == nil || *runRow.PullRequestURL == "" {
		return false, cond + ": no pull request recorded on the run", nil
	}
	if gated := currentGatedStage(stages); gated != nil {
		return false, fmt.Sprintf("%s: the %s stage is still awaiting approval", cond, gated.Type), nil
	}
	if n := len(open); n > 0 {
		return false, fmt.Sprintf("%s: %d open concern(s)", cond, n), nil
	}
	return true, "", nil
}
