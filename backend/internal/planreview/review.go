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

// Usage is the token usage a reviewer backend reports for one review
// invocation (#681). It is captured at the reviewer CONTRACT boundary so
// the server can record advisory reviewer agent cost backend-agnostically
// at the plan_reviewed / implement_reviewed call site, never branching on
// which adapter (local subprocess, SDK, future) actually ran.
//
// Known is the graceful-degradation marker: a backend that cannot report
// usage leaves Known false (with zero-value token counts), and the server
// records the cost at usd=0 with known_usage=false rather than guessing —
// mirroring the cost/pricing unknown-model ok=false contract.
type Usage struct {
	InputTokens  int
	OutputTokens int
	Known        bool
}

// ReviewVerdict is the structured response emitted by a review agent.
// The review-agent prompt instructs agents to return only this shape
// as a JSON object — no prose, no re-planning.
type ReviewVerdict struct {
	Verdict  Verdict   `json:"verdict"`
	Concerns []Concern `json:"concerns,omitempty"`
	FreeForm string    `json:"free_form,omitempty"`

	// Usage is the reviewer backend's token usage for this invocation,
	// populated by the adapter AFTER it decodes the verdict JSON — usage
	// comes from the API/CLI envelope, not the model-emitted verdict body.
	// The `json:"-"` tag isolates it from the agent-JSON decode so a model
	// that echoes a "usage" key in its response cannot spoof the recorded
	// cost figure. Zero-value with Known=false when the backend cannot
	// report usage (graceful degradation).
	Usage Usage `json:"-"`
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
//
// The agent count is the effective count (ReviewersConfig.AgentCount):
// a heterogeneous `agents` list (#955) supersedes the bare integer, so
// heterogeneity changes who reviews, never the gating semantics.
func ResolveAuthority(r spec.ReviewersConfig) AuthorityMode {
	switch {
	case r.AgentCount() > 0 && r.Human == 0:
		return AuthorityGating
	case r.AgentCount() > 0 && r.Human > 0:
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

// ReviewStartedPayload is the JSON payload stored in an audit entry with
// category "plan_review_started" / "implement_review_started" (#600). It
// marks that a review agent was actually dispatched — emitted once per
// stage at dispatch, only when agent>0 AND a PlanReviewer is wired (never
// for the agent==0 "none" or nil-reviewer "skipped" branches). It is the
// MCP-readable proxy that distinguishes a configured-and-running review
// ('pending') from no review configured ('none'): the started entry is
// appended before the per-reviewer loop writes the terminal *_reviewed
// entries, so its audit sequence always precedes them under both gating
// (synchronous) and advisory (detached) authority.
type ReviewStartedPayload struct {
	ConfiguredAgents int           `json:"configured_agents"`
	Authority        AuthorityMode `json:"authority"`

	// HeadSHA is the implement-review idempotency key (#797): the bundle's
	// verify_run committed-tree head_sha, recorded on the
	// implement_review_started entry so a retried raw upload (transient 5xx
	// after the review already dispatched) is deduped on (stage_id,
	// head_sha) before re-dispatching a second review. omitempty keeps the
	// plan path byte-identical — plan_review_started has no diff/head_sha
	// and passes "", so its payload is unchanged.
	HeadSHA string `json:"head_sha,omitempty"`
}

// ReviewFailedPayload is the JSON payload stored in an audit entry with
// category "plan_review_failed" / "implement_review_failed" (#664). It is
// the terminal entry written when a wired reviewer invocation errors or
// times out (a reviewer killed at FISHHAWKD_PLAN_REVIEW_TIMEOUT surfaces as
// a Review error). One entry is appended per failed reviewer invocation.
//
// It is kept deliberately distinct from plan_reviewed / implement_reviewed
// (which carry only the closed approve / approve_with_concerns / reject
// verdict set) so a timeout or transport failure is never decoded as a real
// verdict. Authority records whether the failure degraded a gating or
// advisory gate; this entry is observability-only and does NOT change
// gating advance/degrade semantics (#574). ReviewerModel is best-effort —
// it is empty when the adapter failed before reporting which model ran.
type ReviewFailedPayload struct {
	Reason        string        `json:"reason"`
	ReviewerModel string        `json:"reviewer_model,omitempty"`
	Authority     AuthorityMode `json:"authority"`

	// Timeout distinguishes a per-invocation budget kill from any other
	// failure (#747): true when the reviewer was killed by the size-aware
	// budget deadline (context.DeadlineExceeded at the call site), false for
	// transport/decode/other errors. It is the additive discriminator the
	// issue's "distinguish a timeout" requirement asks for — the audit
	// category stays plan_review_failed / implement_review_failed so existing
	// #664 and MCP await-review readers are unaffected. omitempty keeps the
	// payload byte-identical to pre-#747 entries on the non-timeout path.
	Timeout bool `json:"timeout,omitempty"`
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

// ImplementReviewedPayload is the JSON payload stored in an audit entry
// with category "implement_reviewed" (ADR-027 impl 2/2). It records one
// implement-review agent invocation against the implement-stage diff. The
// shape is identical to PlanReviewedPayload — the verdict, authority, and
// concern semantics are the same; only the reviewed artifact (diff vs.
// plan) differs. scope.files drift is surfaced as a {category:"scope"}
// concern here rather than an auto-reject (ADR-027 Decision Q6).
//
// The companion "implement_review_skipped" category reuses
// ReviewSkippedPayload — same reviewer-not-configured degradation story
// as the plan stage.
type ImplementReviewedPayload struct {
	ReviewerKind  string        `json:"reviewer_kind"`
	ReviewerModel string        `json:"reviewer_model,omitempty"`
	Authority     AuthorityMode `json:"authority"`
	Verdict       Verdict       `json:"verdict"`
	Concerns      []Concern     `json:"concerns,omitempty"`
	FreeForm      string        `json:"free_form,omitempty"`
}
