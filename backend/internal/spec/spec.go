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

// Spec is a parsed and validated workflow specification document.
type Spec struct {
	Version   string              `json:"version" yaml:"version"`
	Roles     map[string]Role     `json:"roles,omitempty" yaml:"roles,omitempty"`
	Workflows map[string]Workflow `json:"workflows" yaml:"workflows"`
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
	Description string  `json:"description,omitempty" yaml:"description,omitempty"`
	Stages      []Stage `json:"stages" yaml:"stages"`
}

// Stage is one unit of work in a workflow. The closed set of types
// (plan / implement / review) is enforced by the schema.
type Stage struct {
	ID          string       `json:"id" yaml:"id"`
	Type        StageType    `json:"type" yaml:"type"`
	Executor    Executor     `json:"executor" yaml:"executor"`
	Inputs      []Input      `json:"inputs,omitempty" yaml:"inputs,omitempty"`
	Produces    []Produces   `json:"produces,omitempty" yaml:"produces,omitempty"`
	Constraints []Constraint `json:"constraints,omitempty" yaml:"constraints,omitempty"`
	Budget      *Budget      `json:"budget,omitempty" yaml:"budget,omitempty"`
	Gates       []Gate       `json:"gates,omitempty" yaml:"gates,omitempty"`
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
	Agent string `json:"agent,omitempty" yaml:"agent,omitempty"`
	Human bool   `json:"human,omitempty" yaml:"human,omitempty"`
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
