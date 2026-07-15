// Package spec parses and validates Fishhawk workflow specs (the YAML
// at .fishhawk/workflows.yaml in customer repos). The canonical schema
// lives at docs/spec/workflow-v0.schema.json; a copy is embedded under
// schemas/ in this package so the parser is self-contained at runtime.
// CI enforces that the two copies stay in sync.
//
// Two-stage validation:
//
//  1. Schema validation — the parsed YAML is round-tripped through JSON
//     Schema (Draft 2020-12) using the embedded workflow-v0 schema.
//     Catches structure errors: missing required fields, unknown stage
//     types, malformed identifiers, etc.
//  2. Semantic validation — graph-shape checks the schema can't express:
//     stage IDs unique within a workflow, from_stage references resolve,
//     approver role references resolve, plan-producing stages declare
//     schema: standard_v1.
//
// Both layers run inside Parse; callers usually don't need to invoke
// Validate directly. Build a *Spec programmatically in tests, then call
// Validate to exercise just the semantic layer.
package spec

import (
	"encoding/json"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration wraps time.Duration so YAML/JSON values like "30m" or "10m"
// round-trip cleanly via time.ParseDuration. A zero Duration means "not
// set" — callers should interpret zero as "fall through to the next
// precedence level."
type Duration struct {
	time.Duration
}

// UnmarshalJSON decodes a duration string like "30m" into a Duration.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if s == "" {
		return nil
	}
	dur, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = dur
	return nil
}

// UnmarshalYAML decodes a YAML scalar duration string into a Duration.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	if s == "" {
		return nil
	}
	dur, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = dur
	return nil
}

// Policy is the per-workflow execution policy.
type Policy struct {
	MaxStageRuntime Duration `json:"max_stage_runtime,omitempty" yaml:"max_stage_runtime,omitempty"`
}

// DefaultStageTimeout is the value ResolveStageTimeout uses when neither
// the stage executor nor the workflow policy declares a timeout.
const DefaultStageTimeout = 15 * time.Minute

// ResolveStageTimeout enforces the three-level timeout precedence:
// stage executor timeout > workflow policy max_stage_runtime > def.
// A zero Duration at any level means "not set" and falls through to the next.
// This is the single source of truth for stage timeout resolution; it is
// called by the prompt handler to populate agent_timeout_seconds on the
// fetch-prompt response.
func ResolveStageTimeout(wf Workflow, st Stage, def time.Duration) time.Duration {
	if st.Executor.Timeout.Duration != 0 {
		return st.Executor.Timeout.Duration
	}
	if wf.Policy != nil && wf.Policy.MaxStageRuntime.Duration != 0 {
		return wf.Policy.MaxStageRuntime.Duration
	}
	return def
}

// Spec is a parsed and validated workflow specification document.
type Spec struct {
	Version   string              `json:"version" yaml:"version"`
	Roles     map[string]Role     `json:"roles,omitempty" yaml:"roles,omitempty"`
	Workflows map[string]Workflow `json:"workflows" yaml:"workflows"`
	// TestConventions are optional per-repo test-location conventions
	// that generalize the plan-gate test sweep (#1004) beyond the
	// built-in Go + colocated-TS defaults. Declared entries are
	// additive to the defaults. Empty/absent → the sweep uses the
	// built-in defaults only. Round-trips through ParseBytes'
	// DisallowUnknownFields decode, so this field MUST stay in lockstep
	// with the schema's top-level test_conventions property.
	TestConventions []TestConvention `json:"test_conventions,omitempty" yaml:"test_conventions,omitempty"`
}

// TestConvention is one test-location convention (#1004): production
// files whose repo-relative path matches Match are expected to have a
// test at one of Candidates. Candidates are path templates with the
// variables {dir}, {name}, {ext}, {relpath}; see the workflow-v0 schema
// $defs/test_convention for their meaning. Consumed by the plan-gate
// test sweep (backend/internal/server/test_sweep.go).
type TestConvention struct {
	Match      string   `json:"match" yaml:"match"`
	Candidates []string `json:"candidates" yaml:"candidates"`
}

// Role names a group of GitHub user/team references that gates can
// resolve to. Member values follow GitHub conventions: "@user" or
// "@org/team".
type Role struct {
	Members []string `json:"members" yaml:"members"`
}

// Workflow is one named pipeline (e.g. "feature_change") with an
// ordered list of stages.
type Workflow struct {
	Description string           `json:"description,omitempty" yaml:"description,omitempty"`
	Stages      []Stage          `json:"stages" yaml:"stages"`
	OnCIFailure *OnCIFailure     `json:"on_ci_failure,omitempty" yaml:"on_ci_failure,omitempty"`
	Policy      *Policy          `json:"policy,omitempty" yaml:"policy,omitempty"`
	Budgets     []PeriodicBudget `json:"budgets,omitempty" yaml:"budgets,omitempty"`
	// Drive opts the workflow's runs into auto-advancement of
	// mechanical transitions (#1023 / #996 theme 1). Default false.
	// The workflow-level value is the per-run default; POST /v0/runs
	// accepts a per-run override that wins, and the resolved flag is
	// snapshotted onto the run row at create time.
	Drive bool `json:"drive,omitempty" yaml:"drive,omitempty"`
	// OperatorAgent is the workflow-level delegation default for the
	// operator agent (ADR-040 / #1026). Nil means nothing is delegated
	// — fail-closed, every judgment pages the human. A gate-level
	// block overrides it wholesale; see EffectiveOperatorAgent.
	OperatorAgent *OperatorAgent `json:"operator_agent,omitempty" yaml:"operator_agent,omitempty"`
	// Decomposition holds the per-workflow decomposition controls
	// (E24.6 / #1146). Nil means the block was absent — no per-workflow
	// override, so EffectiveMaxParallel falls through to the global
	// default. Round-trips through ParseBytes' DisallowUnknownFields
	// decode, so this field MUST stay in lockstep with the schema's
	// workflow.decomposition property.
	Decomposition *Decomposition `json:"decomposition,omitempty" yaml:"decomposition,omitempty"`
}

// Decomposition is the per-workflow decomposition control block (E24.6 /
// #1146). MaxParallel bounds how many decomposed child runs may dispatch
// concurrently for a run of the workflow; 0 (and an absent block) means
// unlimited. It is a per-workflow override of the global
// FISHHAWKD_MAX_PARALLEL_CHILDREN — see Workflow.EffectiveMaxParallel.
type Decomposition struct {
	MaxParallel int `json:"max_parallel,omitempty" yaml:"max_parallel,omitempty"`
}

// EffectiveMaxParallel resolves the concurrency cap for the workflow's
// decomposed children (E24.6 / #1146). Precedence: the per-workflow
// decomposition.max_parallel knob wins when set to a positive value;
// otherwise the supplied globalDefault (wired from
// FISHHAWKD_MAX_PARALLEL_CHILDREN) applies. The result is interpreted as
// 0 = unlimited — consistent with budget.ParallelDecision's cap semantics
// — so a zero knob and a zero global both resolve to unlimited. This
// resolves and surfaces the cap; concurrency enforcement that consumes it
// lands in E24.3 (#1143).
func (w *Workflow) EffectiveMaxParallel(globalDefault int) int {
	if w != nil && w.Decomposition != nil && w.Decomposition.MaxParallel > 0 {
		return w.Decomposition.MaxParallel
	}
	return globalDefault
}

// DelegationCondition names a backend-evaluable predicate under which
// the operator agent may take a delegated action (ADR-040 / #1026).
// v0 ships exactly one condition per knob; the schema's per-knob
// enums enforce the closed set, so an unknown condition never reaches
// this type.
type DelegationCondition string

// v0 delegation conditions, one per operator_agent knob.
const (
	// ConditionCleanDualApproval (may_approve): every configured
	// reviewer for the gated stage returned an approve verdict and
	// zero concerns are open.
	ConditionCleanDualApproval DelegationCondition = "clean_dual_approval"
	// ConditionConvergentConcerns (may_route_fixup): all reviewer
	// verdicts are in, at least one concern is open, no reviewer
	// rejected.
	ConditionConvergentConcerns DelegationCondition = "convergent_concerns"
	// ConditionSoloLow (may_waive): exactly one open concern and its
	// severity is low.
	ConditionSoloLow DelegationCondition = "solo_low"
	// ConditionInfraFlake (may_retry): the latest stage failure is
	// classified as an infrastructure flake.
	ConditionInfraFlake DelegationCondition = "infra_flake"
	// ConditionGatesResolvedCIGreen (may_merge): no pending gate
	// approvals, zero open concerns, PR open, required checks green.
	ConditionGatesResolvedCIGreen DelegationCondition = "gates_resolved_ci_green"
)

// must_page_human events — the closed v0 set of events that always
// page the human regardless of the may_* knobs. The reviewer-reject
// taxonomy now carries the explicit advisory/gating classes (#1378,
// workflow-v0.7) alongside the preserved legacy bare token.
const (
	// PageEventReviewerReject is the legacy bare reviewer-reject token.
	// Preserved for back-compat; it resolves to the gating sense
	// (PageEventGatingReviewerReject), so a bare reviewer_reject still
	// pages the human exactly as before #1378.
	PageEventReviewerReject = "reviewer_reject"
	// PageEventAdvisoryReviewerReject (#1378, workflow-v0.7): an agent
	// reject under advisory review authority (agent + human reviewers).
	// The human approver is the gate, so the reject is arbitrable /
	// auto-routed — it does not page on its own.
	PageEventAdvisoryReviewerReject = "advisory_reviewer_reject"
	// PageEventGatingReviewerReject (#1378, workflow-v0.7): an agent
	// reject under gating review authority (agent-only review). The
	// reject blocks and pages the human; this is the class the legacy
	// bare reviewer_reject resolves to.
	PageEventGatingReviewerReject   = "gating_reviewer_reject"
	PageEventPlanRejection          = "plan_rejection"
	PageEventScopeAmendment         = "scope_amendment"
	PageEventBudgetOverride         = "budget_override"
	PageEventPolicyOverride         = "policy_override"
	PageEventExceptionRequest       = "exception_request"
	PageEventRequirementArbitration = "requirement_arbitration"
	// PageEventClarificationRequest (#1057): the planner parked the plan
	// stage at awaiting_input with a clarification_request because the
	// issue was not yet plannable. The operator must answer the parked
	// questions before planning resumes — a judgment a delegation never
	// absorbs.
	PageEventClarificationRequest = "clarification_request"
)

// OperatorAgent holds the delegation knobs for the operator agent
// (ADR-040 / #1026). Each may_* knob names the single condition under
// which the corresponding action is delegated; an empty knob means
// that action is not delegated. MustPageHuman lists events that always
// page the human regardless of the knobs. Declared at workflow level
// (default) or on an approval gate (override; wins wholesale — knobs
// are never merged across levels).
type OperatorAgent struct {
	MayApprove    DelegationCondition `json:"may_approve,omitempty" yaml:"may_approve,omitempty"`
	MayRouteFixup DelegationCondition `json:"may_route_fixup,omitempty" yaml:"may_route_fixup,omitempty"`
	MayWaive      DelegationCondition `json:"may_waive,omitempty" yaml:"may_waive,omitempty"`
	MayRetry      DelegationCondition `json:"may_retry,omitempty" yaml:"may_retry,omitempty"`
	MayMerge      DelegationCondition `json:"may_merge,omitempty" yaml:"may_merge,omitempty"`
	// RouteFixupMinSeverity is the minimum open-concern severity (low |
	// medium | high) that satisfies may_route_fixup's convergent_concerns
	// condition when EVERY implement-review verdict is approve-class
	// (approve / approve_with_concerns) — the severity-aware tune (#1964).
	// When at least one verdict is a reject the threshold is BYPASSED:
	// advisory arbitration and the gating-reject page are unchanged. Absent
	// defaults to medium; "low" restores the legacy route-on-any-concern
	// behavior. No Go-side validation — the JSON schema enum enforces the
	// closed set for spec-declared blocks, and the delegation evaluator
	// treats an out-of-enum value (reachable only via campaign-override
	// bytes, which bypass JSON-schema validation) defensively as medium.
	// Inherited under the SAME wholesale-override semantics as the may_*
	// knobs (campaign > gate > workflow, never merged across levels).
	RouteFixupMinSeverity string   `json:"route_fixup_min_severity,omitempty" yaml:"route_fixup_min_severity,omitempty"`
	MustPageHuman         []string `json:"must_page_human,omitempty" yaml:"must_page_human,omitempty"`
	// ModelPolicy is the scenario-A operator-agent model-selection
	// contract (#1421). It is part of the operator_agent block and so is
	// inherited under the SAME wholesale-override semantics as the may_*
	// knobs — a gate-level operator_agent block replaces the
	// workflow-level one entirely; model_policy is never merged across
	// levels. Declarative only: surfaced on the run-status delegation
	// block for the operator agent to read and apply via #1416's per-stage
	// override channels, bounded by the deployment per-adapter allow-list.
	// No backend resolution/enforcement code reads it.
	ModelPolicy *ModelPolicy `json:"model_policy,omitempty" yaml:"model_policy,omitempty"`
}

// ModelPolicyStrategy selects how the operator agent decides each
// stage's model under the operator_agent.model_policy contract (#1421,
// scenario A). The values are a closed set mirrored by the schema enum.
type ModelPolicyStrategy string

const (
	// ModelPolicyFollowPlanRecommendation has the operator agent follow the
	// plan artifact's per-stage model recommendation.
	ModelPolicyFollowPlanRecommendation ModelPolicyStrategy = "follow_plan_recommendation"
	// ModelPolicyExplicitDefaults has the operator agent apply the
	// model_policy.defaults map, falling back to the deployment default for
	// any unset stage.
	ModelPolicyExplicitDefaults ModelPolicyStrategy = "explicit_defaults"
)

// ModelPolicyDefaults names the per-stage model the operator agent
// applies under the explicit_defaults strategy. Each field is optional;
// an unset stage falls back to the deployment default.
type ModelPolicyDefaults struct {
	Plan      string `json:"plan,omitempty" yaml:"plan,omitempty"`
	Implement string `json:"implement,omitempty" yaml:"implement,omitempty"`
	Review    string `json:"review,omitempty" yaml:"review,omitempty"`
}

// ModelPolicy is the scenario-A operator-agent model-selection contract
// (#1421): an operator agent pinned to a frontier model decides each
// stage's model from this spec-declared, per-repo-configurable policy
// rather than from ad-hoc per-gate overrides. It is declarative only —
// this issue adds NO backend resolution/enforcement code; the existing
// resolveImplementModel/resolvePlanModel/resolveReviewModel ladders are
// untouched. The operator agent reads the resolved policy from the
// run-status delegation block and applies it through #1416's existing
// per-stage override channels, bounded by — never widening — the
// deployment per-adapter allow-list. All sub-fields are optional; an
// absent ModelPolicy leaves behavior byte-identical to today.
type ModelPolicy struct {
	Strategy ModelPolicyStrategy  `json:"strategy,omitempty" yaml:"strategy,omitempty"`
	Defaults *ModelPolicyDefaults `json:"defaults,omitempty" yaml:"defaults,omitempty"`
	Allowed  []string             `json:"allowed,omitempty" yaml:"allowed,omitempty"`
}

// EffectiveOperatorAgent resolves the operator_agent block that
// governs a gate: the gate-level block when present (it wins wholesale
// — knobs from the two levels are never merged), else the
// workflow-level block, else nil. Nil means fail-closed: nothing is
// delegated and every judgment pages the human. g may be nil for
// callers evaluating outside any gate context (workflow-level only).
func (w *Workflow) EffectiveOperatorAgent(g *Gate) *OperatorAgent {
	if g != nil && g.OperatorAgent != nil {
		return g.OperatorAgent
	}
	if w != nil {
		return w.OperatorAgent
	}
	return nil
}

// ResolveOperatorAgent resolves the effective operator_agent block
// across the full three-level ladder, OUTERMOST first: a campaign-level
// override (E25.12 / #1451) wins over the gate-level block, which wins
// over the workflow-level block — campaign > gate > workflow. The win is
// WHOLESALE at every level: a higher level's block replaces the lower
// one entirely; knobs are never merged across levels (matching
// EffectiveOperatorAgent's gate-vs-workflow semantics, now extended with
// the campaign rung). campaignOverride is nil when the run has no
// campaign context or the campaign declares no override; in that case
// resolution falls through to EffectiveOperatorAgent(g) unchanged, so a
// run with no campaign override resolves byte-identically to today.
// Returns nil when no level declares a block — fail-closed: nothing is
// delegated and every judgment pages the human.
func ResolveOperatorAgent(campaignOverride *OperatorAgent, w *Workflow, g *Gate) *OperatorAgent {
	if campaignOverride != nil {
		return campaignOverride
	}
	return w.EffectiveOperatorAgent(g)
}

// PeriodicBudget is a workflow-level recurring cost ceiling (ADR-030).
// It caps total USD spend across all runs of the workflow within a
// calendar period (weekly or monthly), resetting at the period
// boundary. Distinct from the per-stage Budget (token/runtime caps on
// a single stage execution): a PeriodicBudget governs aggregate spend
// across runs, not a single stage's resource use.
//
// Enforcement reuses the BudgetEnforcement modes:
//
//   - advisory — a budget_alert audit entry + issue comment fires when
//     period spend crosses WarnAt and again at 100%; runs never block.
//   - blocking — a NEW run is refused at admission once the calendar
//     period's spend exhausts LimitUSD; in-flight runs are untouched
//     and an operator can override.
//
// WarnAt is an optional fraction in [0,1] (e.g. 0.8 for 80%) at which
// the advisory warning fires ahead of the 100% crossing. A nil WarnAt
// means only the 100% threshold is surfaced.
type PeriodicBudget struct {
	Period      string            `json:"period" yaml:"period"`
	LimitUSD    float64           `json:"limit_usd" yaml:"limit_usd"`
	Enforcement BudgetEnforcement `json:"enforcement,omitempty" yaml:"enforcement,omitempty"`
	WarnAt      *float64          `json:"warn_at,omitempty" yaml:"warn_at,omitempty"`
}

// Budget period values for PeriodicBudget.Period.
const (
	BudgetPeriodWeekly  = "weekly"
	BudgetPeriodMonthly = "monthly"
)

// OnCIFailure is the per-workflow auto-retry policy (#276 / #277).
// When set, the dispatcher fires a fresh implement workflow_dispatch
// on a required-check failure (#251 / branch protection snapshot)
// up to MaxRetries times, chaining each retry via `parent_run_id`.
//
// Nil-vs-zero distinction matters: a nil pointer means the workflow
// doesn't declare a policy at all (use the documented default of 1
// retry — `DefaultMaxRetries`). An explicit `max_retries: 0` is the
// opt-out signal — useful for low-autonomy workflows that prefer a
// human re-trigger. The consumer (dispatcher) reads MaxRetries
// directly when the pointer is non-nil; resolves to
// DefaultMaxRetries otherwise.
type OnCIFailure struct {
	MaxRetries int `json:"max_retries,omitempty" yaml:"max_retries,omitempty"`
}

// DefaultMaxRetries is the value the dispatcher uses when a
// workflow doesn't declare an `on_ci_failure` block. Centralized
// here so the consumer side has a single source of truth.
const DefaultMaxRetries = 1

// ReviewersConfig holds the plan-review reviewer counts for a plan stage
// (ADR-027). Authority is resolved by planreview.ResolveAuthority:
//
//   - agent>0 && human==0 → gating (agent rejections block stage advancement)
//   - agent>0 && human>0  → advisory (agent verdicts surfaced; cannot block)
//   - agent==0            → gateless
//
// A nil pointer on Stage.Reviewers means the field was absent in the spec;
// callers should treat nil as {Human:1} to preserve pre-ADR-027 behavior.
//
// Agents (#955) declares heterogeneous reviewers — one entry per
// invocation, each naming its provider (and optionally model). When the
// list is present and non-empty it supersedes the bare Agent count;
// AgentCount is the single source of truth for the effective count and
// must be used wherever authority or invocation counts are derived.
type ReviewersConfig struct {
	Agent  int             `json:"agent,omitempty" yaml:"agent,omitempty"`
	Agents []AgentReviewer `json:"agents,omitempty" yaml:"agents,omitempty"`
	Human  int             `json:"human,omitempty" yaml:"human,omitempty"`
	// ReviewTimeout is the optional per-stage review-budget floor (#1494). A
	// non-empty value OVERRIDES the FISHHAWKD_PLAN_REVIEW_TIMEOUT deployment
	// default — it sets the Floor rung of the size-aware review-wait budget
	// for this stage's agent reviews; the deployment-level PerKB and Cap are
	// unchanged. Parsed by time.ParseDuration via ResolveReviewTimeout; an
	// empty or unparseable value falls back to the deployment default. The
	// schema's reviewers_config is additionalProperties:false, so this field
	// MUST stay in lockstep with the schema's review_timeout property.
	ReviewTimeout string `json:"review_timeout,omitempty" yaml:"review_timeout,omitempty"`
}

// ResolveReviewTimeout resolves the review-wait budget floor for a stage,
// mirroring ResolveStageTimeout's precedence: the per-stage spec
// review_timeout WINS over the deployment default. When reviewers is non-nil
// and its ReviewTimeout parses via time.ParseDuration to a non-zero duration,
// that value is returned; on a nil block, an empty string, a parse error, or a
// zero duration, the supplied deflt (the FISHHAWKD_PLAN_REVIEW_TIMEOUT
// deployment default) applies. This is the single source of truth for
// review-timeout resolution. Returning deflt on a bad string (rather than zero)
// guarantees the Floor never collapses to zero, which would silently kill
// reviewers on tiny prompts.
func ResolveReviewTimeout(reviewers *ReviewersConfig, deflt time.Duration) time.Duration {
	if reviewers == nil || reviewers.ReviewTimeout == "" {
		return deflt
	}
	dur, err := time.ParseDuration(reviewers.ReviewTimeout)
	if err != nil || dur == 0 {
		return deflt
	}
	return dur
}

// AgentReviewer is one declared reviewer in the heterogeneous `agents`
// list (#955). Provider is the adapter name (anthropic | claudecode |
// codex — closed set enforced by the schema); Model optionally overrides
// the provider's deployment-configured default model.
type AgentReviewer struct {
	Provider string `json:"provider" yaml:"provider"`
	Model    string `json:"model,omitempty" yaml:"model,omitempty"`
	// ReasoningEffort is the optional per-reviewer reasoning-effort override
	// (#1493). It is CODEX-ONLY: one rung of the per-reviewer ladder
	// (deployment default FISHHAWKD_CODEX_REASONING_EFFORT < this value).
	// Empty falls back to the deployment default. The anthropic and claudecode
	// adapters take no reasoning-effort parameter and ignore it. The schema
	// enum (low|medium|high|xhigh|max) is the sole guard before the value
	// reaches the codex CLI as -c model_reasoning_effort=<effort>.
	ReasoningEffort string `json:"reasoning_effort,omitempty" yaml:"reasoning_effort,omitempty"`
	// AgentVersion is the optional per-reviewer agent-version compatibility
	// RANGE (E32.13 / #1743): a semver comparator range (e.g. ">=0.30
	// <0.31") of this reviewer's agent CLI versions the workflow was
	// validated against. Enforced ONLY for a codex reviewer — the backend
	// probes the codex CLI version and fails the review dispatch loudly on
	// an out-of-range version via MatchAgentVersionRange (the reviewer
	// enforcement is a sibling slice; this slice owns the field + matcher).
	// The anthropic and claudecode adapters take no CLI version and ignore
	// it. Empty falls back to no constraint. The schema's agents items are
	// additionalProperties:false, so this field MUST stay in lockstep with
	// the schema's agent_version property. Validated syntactically by
	// ValidAgentVersionRange in the semantic layer.
	AgentVersion string `json:"agent_version,omitempty" yaml:"agent_version,omitempty"`
	// Optional is the per-reviewer degradation policy (#1495). It frames the
	// FISHHAWKD_ENABLE_* / FISHHAWKD_ANTHROPIC_API_KEY env flags as deployment
	// CAPABILITY gates (is this provider available here) rather than policy
	// switches — the spec is authoritative for WHICH reviewers run. When this
	// reviewer's provider is unavailable on the deployment, run creation does
	// NOT hard-fail; the reviewer degrades at the runtime review loop with a
	// capability-framed *_review_skipped audit. false (default) means the
	// deployment SHOULD run it — an unavailable provider surfaces LOUDLY (ERROR
	// log + capability audit) but still does not block; true means a quiet,
	// graceful advisory-skip. The coarser case of NO reviewer backend wired at
	// all is a deployment-wide misconfiguration that still hard-fails run
	// creation irrespective of this flag (the run-create coarse gate, symmetric
	// with the webhook dispatcher's !PlanReviewerConfigured hard-fail).
	Optional bool `json:"optional,omitempty" yaml:"optional,omitempty"`
}

// AgentCount returns the effective number of agent reviewers: len(Agents)
// when the heterogeneous list is present and non-empty (it supersedes the
// bare count), else Agent.
func (r ReviewersConfig) AgentCount() int {
	if len(r.Agents) > 0 {
		return len(r.Agents)
	}
	return r.Agent
}

// Stage is one unit of work in a workflow. The closed set of types
// (plan / implement / review) is enforced by the schema.
type Stage struct {
	ID          string           `json:"id" yaml:"id"`
	Type        StageType        `json:"type" yaml:"type"`
	Executor    Executor         `json:"executor" yaml:"executor"`
	Inputs      []Input          `json:"inputs,omitempty" yaml:"inputs,omitempty"`
	Produces    []Produces       `json:"produces,omitempty" yaml:"produces,omitempty"`
	Constraints []Constraint     `json:"constraints,omitempty" yaml:"constraints,omitempty"`
	Budget      *Budget          `json:"budget,omitempty" yaml:"budget,omitempty"`
	Gates       []Gate           `json:"gates,omitempty" yaml:"gates,omitempty"`
	Reviewers   *ReviewersConfig `json:"reviewers,omitempty" yaml:"reviewers,omitempty"`
	// Egress is the acceptance-stage egress allowance (v1.3, ADR-050 /
	// #1532): the declared target-instance host(s) the acceptance agent may
	// reach through the runner's default-deny egress proxy. Valid only on an
	// acceptance stage — Validate rejects it on any other type.
	Egress *StageEgress `json:"egress,omitempty" yaml:"egress,omitempty"`
}

// StageEgress declares the customer-controlled slot of the acceptance
// agent's egress allow-list (ADR-050 decision #1). Entries are host or
// host:port — never URLs — because scheme and path are not egress-relevant.
// The runner adds the model API endpoint and the Fishhawk backend itself;
// they are not declarable here.
type StageEgress struct {
	TargetHosts []string `json:"target_hosts" yaml:"target_hosts"`
}

// StageType is the stage's kind, drawn from a closed set.
type StageType string

// Stage types. plan/implement/review are the v0 closed set per
// MVP_SPEC §4.1; deploy + acceptance are the v1 additions (deploy: the
// delegating release stage, ADR-038 / #925, E23.2; acceptance: the
// runner-hosted advisory acceptance stage, ADR-049 / #1519, E31.2). The
// v0 schema rejects both before Validate runs, so the semantic binding
// rules in validate.go stay version-agnostic.
const (
	StageTypePlan       StageType = "plan"
	StageTypeImplement  StageType = "implement"
	StageTypeReview     StageType = "review"
	StageTypeDeploy     StageType = "deploy"
	StageTypeAcceptance StageType = "acceptance"
)

// Executor describes what runs the stage. Exactly one of Agent, Human,
// or Delegate is set. The schema enforces the mutual exclusion (a
// three-branch oneOf); Validate enforces the type<->executor binding
// (Delegate is valid only on a deploy stage; Agent/Human only off one).
type Executor struct {
	Agent string `json:"agent,omitempty" yaml:"agent,omitempty"`
	// Model is the optional per-stage model override (#1013). One rung of
	// the implement-model resolution ladder: deployment default < this
	// executor.model < plan model_recommendation < operator gate decision.
	// Empty falls through to the next-lower rung (ultimately the deployment
	// default spawn). Declared in the agent branch of the executor oneOf;
	// the schema rejects it on a human executor.
	Model string `json:"model,omitempty" yaml:"model,omitempty"`
	// AgentVersion is the optional executor agent-version compatibility
	// RANGE (E32.13 / #1743): a semver comparator range (space-separated
	// AND list, e.g. ">=2.1 <2.2") of coding-agent CLI versions the stage
	// was validated against. Threaded to the runner (via
	// promptResponse.agent_version_range), which fails the stage loudly
	// pre-spawn (category C) when its resolved #1769-probed CLI version
	// falls outside the range, and degrades-and-proceeds on an unprobeable
	// version. Empty/absent = no constraint (mirrors MinRunnerVersion).
	// Declared in the agent branch of the executor oneOf; the schema
	// additionalProperties/unevaluatedProperties lockstep rejects it on a
	// human executor, so this field MUST stay in lockstep with the schema's
	// executor agent-branch agent_version property. Validated syntactically
	// by ValidAgentVersionRange in the semantic layer.
	AgentVersion   string        `json:"agent_version,omitempty" yaml:"agent_version,omitempty"`
	Human          bool          `json:"human,omitempty" yaml:"human,omitempty"`
	Timeout        Duration      `json:"timeout,omitempty" yaml:"timeout,omitempty"`
	Verify         *VerifyConfig `json:"verify,omitempty" yaml:"verify,omitempty"`
	AgentSelfRetry bool          `json:"agent_self_retry,omitempty" yaml:"agent_self_retry,omitempty"`
	// Delegate is the v1 delegating-executor declaration for a deploy
	// stage (ADR-038 / #925). Nil on non-deploy stages. Names the
	// external pipeline a deploy stage delegates to — Fishhawk holds no
	// deploy logic or credentials. The schema rejects it on a non-deploy
	// stage only via the validator (the executor oneOf itself permits it
	// on any stage type); Validate enforces the type<->executor binding.
	Delegate *DelegateConfig `json:"delegate,omitempty" yaml:"delegate,omitempty"`
}

// DelegateConfig is the delegating-executor declaration for a deploy
// stage (ADR-038 / #925). Target is the pipeline-kind discriminator:
//
//   - github_actions — dispatch a workflow via workflow_dispatch;
//     WorkflowRef is required (the workflow file or id), GitRef is the
//     optional branch/tag/sha to dispatch against.
//   - webhook — POST the deploy trigger to URL (required).
//
// The schema's nested oneOf enforces which fields each target requires;
// the unset fields stay empty.
type DelegateConfig struct {
	Target      string `json:"target" yaml:"target"`
	WorkflowRef string `json:"workflow_ref,omitempty" yaml:"workflow_ref,omitempty"`
	GitRef      string `json:"git_ref,omitempty" yaml:"git_ref,omitempty"`
	URL         string `json:"url,omitempty" yaml:"url,omitempty"`
}

// Delegate targets per the schema's delegate.target discriminator.
const (
	DelegateTargetGitHubActions = "github_actions"
	DelegateTargetWebhook       = "webhook"
)

// VerifyConfig holds the optional in-band test gate for a stage.
// Command is a shell expression (passed to sh -c) that must exit 0
// for the stage to succeed. Timeout caps the gate's wall-clock run;
// zero means the runner applies its own 10-minute fallback.
//
// MaxIterations is the verify-fix loop budget: 0 (default) preserves
// today's single-shot demote-on-failure gate; >0 enables a bounded
// evaluator-optimizer fix loop run against the committed scope-only
// tree, capping total verify-fix agent invocations across the stage at
// this value. No behavior consumes it yet — it is wired through the
// prompt response so the runner can read it.
type VerifyConfig struct {
	Command       string   `json:"command" yaml:"command"`
	Timeout       Duration `json:"timeout,omitempty" yaml:"timeout,omitempty"`
	MaxIterations int      `json:"max_iterations,omitempty" yaml:"max_iterations,omitempty"`
}

// Input is either a trigger (Source set) or an artifact handoff from
// a prior stage in the same run (Artifact + FromStage set).
type Input struct {
	Source    InputSource `json:"source,omitempty" yaml:"source,omitempty"`
	Required  bool        `json:"required,omitempty" yaml:"required,omitempty"`
	Artifact  string      `json:"artifact,omitempty" yaml:"artifact,omitempty"`
	FromStage string      `json:"from_stage,omitempty" yaml:"from_stage,omitempty"`
}

// InputSource is the trigger kind for inputs that come from outside
// the workflow (issue, PR).
type InputSource string

// Input source values per the schema.
const (
	InputSourceGitHubIssue InputSource = "github_issue"
	InputSourcePullRequest InputSource = "pull_request"
)

// Produces declares an artifact emitted by a stage and how it's
// persisted.
type Produces struct {
	Artifact    ArtifactKind  `json:"artifact" yaml:"artifact"`
	Schema      string        `json:"schema,omitempty" yaml:"schema,omitempty"`
	Persistence []Persistence `json:"persistence,omitempty" yaml:"persistence,omitempty"`
}

// ArtifactKind is the closed artifact set.
type ArtifactKind string

// Artifact kinds per the schema. plan/pull_request are the v0 set;
// deployment is the v1 deploy-stage artifact (ADR-038 / #925) — valid
// only on a deploy stage; acceptance is the v1.2 acceptance-stage
// artifact (ADR-049 / #1531) — valid only on an acceptance stage. Both
// stage-type bindings are enforced by Validate.
const (
	ArtifactPlan        ArtifactKind = "plan"
	ArtifactPullRequest ArtifactKind = "pull_request"
	ArtifactDeployment  ArtifactKind = "deployment"
	ArtifactAcceptance  ArtifactKind = "acceptance"
)

// Persistence says where an artifact is stored.
type Persistence struct {
	Target         PersistenceTarget `json:"target" yaml:"target"`
	Mode           PersistenceMode   `json:"mode" yaml:"mode"`
	UpdateOnChange bool              `json:"update_on_change,omitempty" yaml:"update_on_change,omitempty"`
}

// PersistenceTarget is where the artifact is written.
type PersistenceTarget string

// Persistence targets per the schema.
const (
	PersistenceOriginatingIssue PersistenceTarget = "originating_issue"
	PersistenceFishhawkAuditLog PersistenceTarget = "fishhawk_audit_log"
)

// PersistenceMode describes the form of the persisted artifact.
type PersistenceMode string

// Persistence modes per the schema.
const (
	ModeRenderedComment PersistenceMode = "rendered_comment"
	ModeCanonical       PersistenceMode = "canonical"
)

// Constraint is exactly one constraint kind per object; the schema
// enforces this with maxProperties: 1. Two families:
//
//   - Post-hoc diff constraints (max_files_changed, forbidden_paths,
//     allowed_paths, required_outcomes) — evaluated against a stage's
//     produced diff. The v0 closed set; valid on non-deploy stages.
//   - Pre-flight deploy constraints (allowed_environments, change_freeze,
//     required_upstream; ADR-038 / #925) — evaluated BEFORE a delegating
//     deploy stage executes. Valid only on a deploy stage.
//
// Validate enforces the type<->constraint binding. ChangeFreeze is a
// presence-aware *bool, NOT a plain bool: because each constraint object
// carries exactly one key, `{change_freeze: false}` is a VALID shape
// whose key is PRESENT, and the "pre-flight constraints are deploy-only"
// rule must reject it on a non-deploy stage. A plain bool zero-value
// cannot distinguish "present and false" from "absent"; the pointer
// makes presence (ChangeFreeze != nil) detectable.
type Constraint struct {
	MaxFilesChanged  int      `json:"max_files_changed,omitempty" yaml:"max_files_changed,omitempty"`
	ForbiddenPaths   []string `json:"forbidden_paths,omitempty" yaml:"forbidden_paths,omitempty"`
	AllowedPaths     []string `json:"allowed_paths,omitempty" yaml:"allowed_paths,omitempty"`
	RequiredOutcomes []string `json:"required_outcomes,omitempty" yaml:"required_outcomes,omitempty"`
	// Pre-flight deploy constraint kinds (ADR-038 / #925). See the type
	// doc; ChangeFreeze is *bool for presence detection.
	AllowedEnvironments []string `json:"allowed_environments,omitempty" yaml:"allowed_environments,omitempty"`
	ChangeFreeze        *bool    `json:"change_freeze,omitempty" yaml:"change_freeze,omitempty"`
	RequiredUpstream    []string `json:"required_upstream,omitempty" yaml:"required_upstream,omitempty"`
}

// isPreflight reports whether the constraint is one of the pre-flight
// deploy kinds (allowed_environments, change_freeze, required_upstream).
// change_freeze presence is detected via the pointer (ChangeFreeze !=
// nil), so `{change_freeze: false}` counts as a pre-flight constraint.
func (c Constraint) isPreflight() bool {
	return len(c.AllowedEnvironments) > 0 || c.ChangeFreeze != nil || len(c.RequiredUpstream) > 0
}

// isPostHoc reports whether the constraint is one of the post-hoc diff
// kinds (max_files_changed, forbidden_paths, allowed_paths,
// required_outcomes).
func (c Constraint) isPostHoc() bool {
	return c.MaxFilesChanged != 0 || len(c.ForbiddenPaths) > 0 ||
		len(c.AllowedPaths) > 0 || len(c.RequiredOutcomes) > 0
}

// Budget caps token / runtime usage for a stage.
type Budget struct {
	MaxTokens         int               `json:"max_tokens,omitempty" yaml:"max_tokens,omitempty"`
	MaxRuntimeMinutes int               `json:"max_runtime_minutes,omitempty" yaml:"max_runtime_minutes,omitempty"`
	Enforcement       BudgetEnforcement `json:"enforcement,omitempty" yaml:"enforcement,omitempty"`
}

// BudgetEnforcement says whether overruns are reported (advisory) or
// blocked (blocking; v0.x).
type BudgetEnforcement string

// Budget enforcement modes per the schema.
const (
	EnforcementAdvisory BudgetEnforcement = "advisory"
	EnforcementBlocking BudgetEnforcement = "blocking"
)

// Gate is either an approval gate (humans must act) or a check gate
// (a placeholder that delegates merge-readiness to GitHub branch
// protection). v0.2 dropped the gate-level `blocking_checks` field
// (ADR-017 / #254); required CI checks are derived from branch
// protection at run-create time and snapshotted onto the run row
// (#251). The check-gate variant carries no spec-level fields and
// is effectively a no-op until #255 wires routine_change to
// `gh pr merge --auto`.
type Gate struct {
	Type      GateType   `json:"type" yaml:"type"`
	Approvers *Approvers `json:"approvers,omitempty" yaml:"approvers,omitempty"`
	// Approvals is the forge-neutral approval predicate (E39.2 / #1707),
	// the additive alternative to the GitHub-handle Approvers allow-list.
	// An approval gate declares EXACTLY ONE of Approvers or Approvals; the
	// schema's inner oneOf enforces the mutual exclusion, so both-nil or
	// both-set never reaches a validated Spec. Nil when the gate uses the
	// legacy Approvers form. The re-decode into Spec uses
	// DisallowUnknownFields, so this field MUST stay in lockstep with the
	// schema's gate approval-branch `approvals` property.
	Approvals *Approvals `json:"approvals,omitempty" yaml:"approvals,omitempty"`
	SLA       string     `json:"sla,omitempty" yaml:"sla,omitempty"`
	// OperatorAgent is the per-gate delegation override (ADR-040 /
	// #1026, approval gates only — the schema rejects it on check
	// gates). When non-nil it wins wholesale over the workflow-level
	// block; resolve via Workflow.EffectiveOperatorAgent.
	OperatorAgent *OperatorAgent `json:"operator_agent,omitempty" yaml:"operator_agent,omitempty"`
}

// GateType is approval or check.
type GateType string

// Gate types per the schema.
const (
	GateTypeApproval GateType = "approval"
	GateTypeCheck    GateType = "check"
)

// Approvers names roles whose members can satisfy the gate. Exactly
// one of AnyOf or AllOf is set; the schema enforces the mutual
// exclusion.
type Approvers struct {
	AnyOf []string `json:"any_of,omitempty" yaml:"any_of,omitempty"`
	AllOf []string `json:"all_of,omitempty" yaml:"all_of,omitempty"`
}

// Approvals is the forge-neutral approval predicate for an approval gate
// (E39.2 / #1707) — the additive alternative to the GitHub-handle
// Approvers allow-list. It carries no repo-specific @-handle, so a gate
// can declare its approval requirement without a per-repo role map.
//
// Count is REQUIRED by the schema (integer >= 1, always explicit per
// ADR-055) so an empty `approvals: {}` is rejected as a no-op; it is a
// *int here only so the decoded value is observable and testable, never
// to model absence (a schema-valid Approvals always carries it). The
// other predicates are optional: Not excludes relationship classes
// (author / agent); MinPermission is the forge-neutral minimum
// repository permission tier (identity.Permission vocabulary, sans
// none); MemberOf is a forge-neutral org/team; Members are plain
// forge-neutral subject strings (NOT the @-prefixed GitHub member-ref).
// MinPermission and MemberOf are annotated x-intended-required in the
// schema — optional now, intended to become required in a future major.
type Approvals struct {
	Count         *int     `json:"count,omitempty" yaml:"count,omitempty"`
	Not           []string `json:"not,omitempty" yaml:"not,omitempty"`
	MinPermission string   `json:"min_permission,omitempty" yaml:"min_permission,omitempty"`
	MemberOf      string   `json:"member_of,omitempty" yaml:"member_of,omitempty"`
	Members       []string `json:"members,omitempty" yaml:"members,omitempty"`
}
