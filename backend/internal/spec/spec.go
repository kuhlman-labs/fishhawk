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
// page the human regardless of the may_* knobs.
const (
	PageEventReviewerReject         = "reviewer_reject"
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
	MustPageHuman []string            `json:"must_page_human,omitempty" yaml:"must_page_human,omitempty"`
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
}

// AgentReviewer is one declared reviewer in the heterogeneous `agents`
// list (#955). Provider is the adapter name (anthropic | claudecode |
// codex — closed set enforced by the schema); Model optionally overrides
// the provider's deployment-configured default model.
type AgentReviewer struct {
	Provider string `json:"provider" yaml:"provider"`
	Model    string `json:"model,omitempty" yaml:"model,omitempty"`
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
}

// StageType is the stage's kind, drawn from a closed v0 set.
type StageType string

// Stage types per MVP_SPEC §4.1.
const (
	StageTypePlan      StageType = "plan"
	StageTypeImplement StageType = "implement"
	StageTypeReview    StageType = "review"
)

// Executor describes what runs the stage. Exactly one of Agent or
// Human is set. The schema enforces the mutual exclusion.
type Executor struct {
	Agent          string        `json:"agent,omitempty" yaml:"agent,omitempty"`
	Human          bool          `json:"human,omitempty" yaml:"human,omitempty"`
	Timeout        Duration      `json:"timeout,omitempty" yaml:"timeout,omitempty"`
	Verify         *VerifyConfig `json:"verify,omitempty" yaml:"verify,omitempty"`
	AgentSelfRetry bool          `json:"agent_self_retry,omitempty" yaml:"agent_self_retry,omitempty"`
}

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

// ArtifactKind is the closed v0 artifact set.
type ArtifactKind string

// Artifact kinds per the schema.
const (
	ArtifactPlan        ArtifactKind = "plan"
	ArtifactPullRequest ArtifactKind = "pull_request"
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

// Constraint is exactly one of the closed-set rules (max_files_changed,
// forbidden_paths, allowed_paths, required_outcomes). At decode time
// every Constraint has exactly one non-zero field; the schema enforces
// this with maxProperties: 1.
type Constraint struct {
	MaxFilesChanged  int      `json:"max_files_changed,omitempty" yaml:"max_files_changed,omitempty"`
	ForbiddenPaths   []string `json:"forbidden_paths,omitempty" yaml:"forbidden_paths,omitempty"`
	AllowedPaths     []string `json:"allowed_paths,omitempty" yaml:"allowed_paths,omitempty"`
	RequiredOutcomes []string `json:"required_outcomes,omitempty" yaml:"required_outcomes,omitempty"`
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
