// Package planreview defines the verdict types, authority resolution
// logic, and audit payload for plan-review agents (ADR-027).
//
// Authority modes control whether agent verdicts can block stage
// advancement (gating) or are surfaced for human consumption only
// (advisory). The three modes are derived from the stage's
// ReviewersConfig via ResolveAuthority.
package planreview

import "github.com/kuhlman-labs/fishhawk/backend/internal/spec"

// Verdict is the review agent's conclusion on a plan artifact.
// The closed set matches the verdict JSON schema emitted by the
// review-agent prompt.
type Verdict string

// Closed verdict set per ADR-027.
const (
	VerdictApprove             Verdict = "approve"
	VerdictApproveWithConcerns Verdict = "approve_with_concerns"
	VerdictReject              Verdict = "reject"
)

// ConcernSeverity classifies the weight of a reviewer concern.
type ConcernSeverity string

// Concern severity levels per ADR-027 / #560 issue body.
const (
	SeverityHigh   ConcernSeverity = "high"
	SeverityMedium ConcernSeverity = "medium"
	SeverityLow    ConcernSeverity = "low"
)

// Concern is one flagged issue within a review verdict.
// Severity drives how the concern is surfaced to the operator;
// Category is a short classifier (e.g. "scope", "security");
// Note is the reviewer's free-form explanation.
type Concern struct {
	Severity ConcernSeverity `json:"severity"`
	Category string          `json:"category"`
	Note     string          `json:"note"`
}

// ReviewVerdict is the structured response emitted by a review agent.
// The review-agent prompt instructs agents to return only this shape
// as a JSON object — no prose, no re-planning.
type ReviewVerdict struct {
	Verdict  Verdict   `json:"verdict"`
	Concerns []Concern `json:"concerns,omitempty"`
	FreeForm string    `json:"free_form,omitempty"`
}

// AuthorityMode determines whether agent verdicts gate stage advancement.
type AuthorityMode string

// Authority modes per ADR-027 §3 decision table.
const (
	// AuthorityGating means an agent rejection blocks the plan stage
	// from advancing to awaiting_approval. Applies when agent>0 and
	// human==0: no human approver is present to override the agent.
	AuthorityGating AuthorityMode = "gating"

	// AuthorityAdvisory means agent verdicts are recorded and surfaced
	// but cannot block stage advancement. Applies when agent>0 and
	// human>0: the human approver is the authoritative gate.
	AuthorityAdvisory AuthorityMode = "advisory"

	// AuthorityGateless means no review agents are configured and the
	// plan stage proceeds without agent review. Applies when agent==0.
	AuthorityGateless AuthorityMode = "gateless"
)

// ResolveAuthority maps a ReviewersConfig to the applicable authority
// mode using the ADR-027 §3 decision table:
//
//	agent>0 && human==0 → gating
//	agent>0 && human>0  → advisory
//	agent==0            → gateless
func ResolveAuthority(r spec.ReviewersConfig) AuthorityMode {
	switch {
	case r.Agent > 0 && r.Human == 0:
		return AuthorityGating
	case r.Agent > 0 && r.Human > 0:
		return AuthorityAdvisory
	default:
		return AuthorityGateless
	}
}

// ReviewSkippedPayload is the JSON payload stored in an audit
// entry with category "plan_review_skipped" (#574). It records that
// the stage's reviewers config requested agent review (agent > 0) but
// no PlanReviewer was wired, so the agent layer was skipped. Authority
// captures whether the skip degraded a gating or advisory gate; in
// advisory mode the human gate remains authoritative.
type ReviewSkippedPayload struct {
	Reason           string        `json:"reason"`
	ConfiguredAgents int           `json:"configured_agents"`
	Authority        AuthorityMode `json:"authority"`
}

// PlanReviewedPayload is the JSON payload stored in an audit entry
// with category "plan_reviewed". One entry is appended per review
// agent invocation.
type PlanReviewedPayload struct {
	ReviewerKind  string        `json:"reviewer_kind"`
	ReviewerModel string        `json:"reviewer_model,omitempty"`
	Authority     AuthorityMode `json:"authority"`
	Verdict       Verdict       `json:"verdict"`
	Concerns      []Concern     `json:"concerns,omitempty"`
	FreeForm      string        `json:"free_form,omitempty"`
}
