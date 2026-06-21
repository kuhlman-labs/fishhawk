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

import (
	"fmt"
	"time"
)

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
	// ModelRecommendation is the agent's optional complexity-informed
	// recommendation for which model executes the implement stage (#1013).
	// Advisory: the operator ratifies or overrides it at the plan gate, and
	// the resolved model is validated against the deployment's per-adapter
	// allowed-model set. Nil means no recommendation — the implement-model
	// resolver falls through to the spec executor.model, then the
	// deployment default.
	ModelRecommendation *ModelRecommendation `json:"model_recommendation,omitempty"`
}

// ModelRecommendation is the agent's optional recommendation for the
// implement-stage model, keyed to its complexity assessment (#1013). It
// is one rung of the implement-model resolution ladder (deployment
// default < spec executor.model < plan model_recommendation < operator
// gate decision); the operator ratifies or overrides it at the approval
// gate. JSON tags mirror the model-recommendation $def in the schema.
type ModelRecommendation struct {
	ImplementModel     string     `json:"implement_model"`
	Rationale          string     `json:"rationale"`
	ComplexityAssessed Complexity `json:"complexity_assessed"`
}

// Complexity is the agent's assessment of a change's complexity, drawn
// from a closed set. It informs the model recommendation and is stamped
// onto calibration history.
type Complexity string

// Complexity levels per the schema.
const (
	ComplexityLow    Complexity = "low"
	ComplexityMedium Complexity = "medium"
	ComplexityHigh   Complexity = "high"
)

// KindClarificationRequest is the top-level discriminator value carried
// by a clarification_request artifact. A plan artifact has no "kind"
// field (it carries plan_version), so the two are routed apart before
// validation without touching the frozen plan schema.
const KindClarificationRequest = "clarification_request"

// ArtifactKind identifies which plan-stage artifact a document is, as
// determined by the top-level discriminator. The plan artifact is the
// default (it predates the discriminator and carries plan_version).
type ArtifactKind string

// Artifact kinds produced by the plan stage.
const (
	ArtifactKindPlan                 ArtifactKind = "plan"
	ArtifactKindClarificationRequest ArtifactKind = "clarification_request"
)

// ClarificationRequest is a parsed and schema-validated clarification_request
// artifact — the additive standard_v1 sibling the plan stage emits when an
// issue is not yet plannable (lacks a non-derivable fact or needs an operator
// decision). JSON tags mirror clarification-request-v1.schema.json.
type ClarificationRequest struct {
	Kind            string                  `json:"kind"`
	TicketReference TicketReference         `json:"ticket_reference"`
	GeneratedBy     GeneratedBy             `json:"generated_by"`
	Summary         string                  `json:"summary"`
	Questions       []ClarificationQuestion `json:"questions"`
}

// ClarificationQuestion is one parked question within a ClarificationRequest.
// IDs must be unique within Questions — operator answers are keyed by ID on
// resume, so a duplicate is ambiguous.
type ClarificationQuestion struct {
	ID                 string `json:"id"`
	Question           string `json:"question"`
	WhatICanInfer      string `json:"what_i_can_infer,omitempty"`
	RecommendedDefault string `json:"recommended_default"`
	Tradeoffs          string `json:"tradeoffs"`
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
	// ModelRecommendation is this sub-plan's optional per-child model
	// recommendation (#1013). When set, the decomposition child minted for
	// this sub-plan resolves it through the same chokepoint as a top-level
	// recommendation. Nil means the child has no per-slice recommendation.
	ModelRecommendation *ModelRecommendation `json:"model_recommendation,omitempty"`
	// DependsOn lists the 0-based indices of sibling sub_plans this slice
	// depends on within the same Decomposition (#1258). Waves consumes it to
	// derive the topological dispatch order: a slice runs only after every
	// slice it depends on has completed. Omitted/empty means no dependency —
	// the slice is eligible in the first wave. Validated by Waves (wired into
	// semanticCheck): out-of-range, negative, self, or cyclic indices are
	// rejected at the plan gate.
	DependsOn []int `json:"depends_on,omitempty"`
}

// Decomposition holds the rationale and sub-plan summaries when
// the agent's runtime estimate exceeds the implement-stage budget.
// Stored in the audit log but not acted upon until D3/D4.
type Decomposition struct {
	Rationale string           `json:"rationale"`
	SubPlans  []SubPlanSummary `json:"sub_plans"`
}

// Waves derives the topological dispatch order for a decomposition's
// sub_plans from their depends_on edges (#1258), returning ordered waves of
// 0-based sub-plan indices. Wave 0 holds every slice with no unsatisfied
// dependency; each subsequent wave holds the slices whose dependencies all
// appeared in an earlier wave. Within a wave, indices are emitted in
// ascending order for deterministic output. A decomposition with no
// depends_on anywhere collapses to a single wave [[0,1,...,n-1]]
// (back-compat). It is a pure Kahn topological sort with no side effects;
// no downstream dispatch consumes the result yet (slice B, #1278).
//
// Waves fails loud rather than silently dropping a slice:
//   - a depends_on index outside [0, len(sub_plans)) (out-of-range or negative);
//   - a slice depending on itself (self-dependency);
//   - a dependency cycle that leaves some slices unplaceable.
//
// A nil decomposition or one with no sub_plans returns (nil, nil).
func Waves(d *Decomposition) ([][]int, error) {
	if d == nil || len(d.SubPlans) == 0 {
		return nil, nil
	}
	n := len(d.SubPlans)

	// Validate every edge before layering so a malformed index is reported
	// as such rather than masquerading as a cycle.
	for i, sp := range d.SubPlans {
		for _, dep := range sp.DependsOn {
			if dep < 0 || dep >= n {
				return nil, fmt.Errorf("sub_plan %d depends_on index %d out of range [0,%d)", i, dep, n)
			}
			if dep == i {
				return nil, fmt.Errorf("sub_plan %d depends_on itself (index %d)", i, dep)
			}
		}
	}

	// Kahn layering: a node joins a wave once every node it depends on has
	// been placed in an earlier wave. Iterating i ascending keeps each wave
	// (and the cycle report below) in ascending index order.
	placed := make([]bool, n)
	remaining := n
	var waves [][]int
	for remaining > 0 {
		var wave []int
		for i := 0; i < n; i++ {
			if placed[i] {
				continue
			}
			ready := true
			for _, dep := range d.SubPlans[i].DependsOn {
				if !placed[dep] {
					ready = false
					break
				}
			}
			if ready {
				wave = append(wave, i)
			}
		}
		if len(wave) == 0 {
			// No node became ready this pass: every unplaced node sits on a
			// dependency cycle.
			var stuck []int
			for i := 0; i < n; i++ {
				if !placed[i] {
					stuck = append(stuck, i)
				}
			}
			return nil, fmt.Errorf("dependency cycle among sub_plans %v", stuck)
		}
		for _, i := range wave {
			placed[i] = true
		}
		remaining -= len(wave)
		waves = append(waves, wave)
	}
	return waves, nil
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
