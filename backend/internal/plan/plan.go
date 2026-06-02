// Package plan parses and validates Fishhawk plan artifacts (the
// JSON output of `type: plan` workflow stages, schema standard_v1).
// The canonical schema lives at docs/spec/plan-standard-v1.schema.json;
// an embedded copy under schemas/ keeps the package self-contained at
// runtime, with the CI's schema-sync guard ensuring the two stay in
// lockstep.
//
// Two entry points:
//
//   - Validate validates raw bytes against the schema. Used by the
//     runner (E5.4) before declaring a plan-stage successful.
//   - Parse validates and returns the typed *Plan. Used by the
//     backend for rendering, persistence, and audit log writes.
//
// Semantic checks (title uniqueness within decomposition) run inside
// Parse after JSON decode. Cross-document checks against the producing
// workflow spec live at the runner / backend layer where both sides are
// available.
package plan

import "time"

// Plan is a parsed and schema-validated plan_version: standard_v1
// artifact. JSON tags mirror the schema; the canonical wire format
// is JSON.
type Plan struct {
	PlanVersion                string            `json:"plan_version"`
	TicketReference            TicketReference   `json:"ticket_reference"`
	GeneratedBy                GeneratedBy       `json:"generated_by"`
	Summary                    string            `json:"summary"`
	Scope                      Scope             `json:"scope"`
	Approach                   []ApproachStep    `json:"approach"`
	Verification               Verification      `json:"verification"`
	RisksAndAssumptions        []string          `json:"risks_and_assumptions,omitempty"`
	PredictedRuntimeMinutes    int               `json:"predicted_runtime_minutes"`
	PredictedRuntimeConfidence RuntimeConfidence `json:"predicted_runtime_confidence"`
	Decomposition              *Decomposition    `json:"decomposition,omitempty"`
}

// RuntimeConfidence is the agent's confidence level in a runtime estimate.
type RuntimeConfidence string

// Runtime confidence levels per the schema.
const (
	RuntimeConfidenceLow    RuntimeConfidence = "low"
	RuntimeConfidenceMedium RuntimeConfidence = "medium"
	RuntimeConfidenceHigh   RuntimeConfidence = "high"
)

// SubPlanSummary describes one sub-plan within a Decomposition.
type SubPlanSummary struct {
	Title     string `json:"title"`
	ScopeHint string `json:"scope_hint"`
	// Scope is the optional per-sub-plan file list. When set, the
	// decomposition fan-out child for this sub-plan bounds its run scope
	// (scope_handoff + scope-drift) to these files instead of the parent
	// plan's full scope.files. Nil means inherit the parent plan's full
	// scope.
	Scope                      *Scope            `json:"scope,omitempty"`
	PredictedRuntimeMinutes    int               `json:"predicted_runtime_minutes"`
	PredictedRuntimeConfidence RuntimeConfidence `json:"predicted_runtime_confidence"`
}

// Decomposition holds the rationale and sub-plan summaries when
// the agent's runtime estimate exceeds the implement-stage budget.
// Stored in the audit log but not acted upon until D3/D4.
type Decomposition struct {
	Rationale string           `json:"rationale"`
	SubPlans  []SubPlanSummary `json:"sub_plans"`
}

// TicketReference identifies the originating ticket. v0 closed set:
// type = "github_issue".
type TicketReference struct {
	Type TicketType `json:"type"`
	URL  string     `json:"url"`
	ID   string     `json:"id"`
}

// TicketType is the ticket-tracker enum.
type TicketType string

// Ticket types per the schema.
const (
	TicketTypeGitHubIssue TicketType = "github_issue"
)

// GeneratedBy identifies the agent + model + wall-clock time of plan
// generation. Recorded in the audit log alongside the trace.
type GeneratedBy struct {
	Agent     string    `json:"agent"`
	Model     string    `json:"model"`
	Version   string    `json:"version,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

// Scope lists the files the agent intends to touch. The runner's
// post-hoc constraint check (E5.5) compares this to the actual diff
// and against the stage's forbidden_paths / allowed_paths.
type Scope struct {
	Files                 []ScopeFile `json:"files"`
	EstimatedLinesChanged int         `json:"estimated_lines_changed,omitempty"`
}

// ScopeFile is one entry in Scope.Files.
type ScopeFile struct {
	Path      string        `json:"path"`
	Operation FileOperation `json:"operation"`
}

// FileOperation enumerates the per-file intent.
type FileOperation string

// File operations per the schema.
const (
	FileOpCreate FileOperation = "create"
	FileOpModify FileOperation = "modify"
	FileOpDelete FileOperation = "delete"
)

// ApproachStep is one entry in Plan.Approach. Steps are 1-indexed.
type ApproachStep struct {
	Step        int    `json:"step"`
	Description string `json:"description"`
}

// Verification describes how the change will be tested and rolled
// back if needed.
type Verification struct {
	TestStrategy string `json:"test_strategy"`
	RollbackPlan string `json:"rollback_plan"`
}
