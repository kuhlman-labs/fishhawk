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

// Decision is one knob's evaluation: whether the named condition is
// satisfied by current run state, and — when it is not — the exact
// failed predicate, prefixed with the condition name (e.g.
// "clean_dual_approval: 1 of 2 reviewer verdicts received").
type Decision struct {
	Action      string
	Condition   spec.DelegationCondition
	Met         bool
	UnmetReason string
}

// Result carries every configured knob's Decision plus the effective
// block's must_page_human event list (static configuration, surfaced
// alongside the evaluations so the operator agent reads its full
// envelope in one response).
type Result struct {
	Actions       []Decision
	MustPageHuman []string
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
// repository surfaces. All three dependencies are required; the caller
// (handleGetRun, the delegated-action handlers) guards nil wiring and
// degrades by omitting the surface.
type Evaluator struct {
	Stages   StageLister
	Concerns ConcernLister
	Audit    AuditLister
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
func (e *Evaluator) Evaluate(ctx context.Context, runRow *run.Run, wf *spec.Workflow) (*Result, error) {
	if !Configured(wf) {
		return nil, nil
	}

	stages, err := e.Stages.ListStagesForRun(ctx, runRow.ID)
	if err != nil {
		return nil, fmt.Errorf("list stages: %w", err)
	}
	gated := currentGatedStage(stages)
	effective := wf.EffectiveOperatorAgent(approvalGateForStage(wf, gated))
	if effective == nil {
		return nil, nil
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
	rejectClass := reviewerRejectClass(wf)
	if parkedAwaitingInput(stages) {
		return &Result{MustPageHuman: effective.MustPageHuman, ReviewerRejectClass: rejectClass, ModelPolicy: effective.ModelPolicy}, nil
	}

	open, err := e.Concerns.ListOpenByRun(ctx, runRow.ID)
	if err != nil {
		return nil, fmt.Errorf("list open concerns: %w", err)
	}

	res := &Result{MustPageHuman: effective.MustPageHuman, ReviewerRejectClass: rejectClass, ModelPolicy: effective.ModelPolicy}
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
			return e.evalConvergentConcerns(ctx, runRow, wf, open)
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
		if k.condition == "" {
			continue
		}
		met, reason, err := k.eval()
		if err != nil {
			return nil, fmt.Errorf("evaluate %s: %w", k.condition, err)
		}
		d := Decision{Action: k.action, Condition: k.condition, Met: met}
		if !met {
			d.UnmetReason = reason
		}
		res.Actions = append(res.Actions, d)
	}
	return res, nil
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
	startedEntries, err := e.Audit.ListForRunByCategory(ctx, runRow.ID, startedCat)
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

	reviewedEntries, err := e.Audit.ListForRunByCategory(ctx, runRow.ID, reviewedCat)
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
func (e *Evaluator) evalConvergentConcerns(ctx context.Context, runRow *run.Run, wf *spec.Workflow, open []*concern.Concern) (bool, string, error) {
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
	for _, v := range verdicts {
		if v == planreview.VerdictReject && gating {
			return false, cond + ": a gating-authority reviewer rejected (" + spec.PageEventGatingReviewerReject + " pages the human)", nil
		}
	}
	if len(open) == 0 {
		return false, cond + ": 0 open concerns to route", nil
	}
	return true, "", nil
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
// testcontainers start-timeout signature (#972). The set mirrors the
// runner's isTestcontainersStartFlake matcher — the single emit site
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

// hasInfraFlakeSignature reports whether a failure reason carries the
// infra-flake classification: the literal verify_infra_flake_retry
// marker, or the conservative testcontainers signature ("context
// deadline exceeded" AND a container-start marker — an ordinary test
// failure that merely mentions a deadline never matches).
func hasInfraFlakeSignature(reason string) bool {
	if strings.Contains(reason, "verify_infra_flake_retry") {
		return true
	}
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
	entries, err := e.Audit.ListForRunByCategory(ctx, runRow.ID, drive.Category)
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
