package plan

// acceptance_check.go holds the pure, deterministic acceptance-criteria rule
// set (#1596, E34.5 / ADR-052). It lives in the plan package — which already
// owns Verification/AcceptanceCriterion and imports no project packages — so it
// is the SINGLE source both the server's plan-gate pre-check
// (runAcceptancePrecheck) and the refinement intake pre-check
// (refinement.EvaluateDraftCriteria) dispatch through. Keeping the rules here
// means there is no second copy to drift: a rule added to the set applies to
// both surfaces at once.

// AcceptanceFinding is one deterministic defect the acceptance rule set
// flagged. Rule is the machine-readable classifier (no_blocking_criterion,
// missing_source_ref, missing_rationale, empty_id, duplicate_id). CriterionID
// names the offending criterion; it is empty for the presence-level
// no_blocking_criterion finding, which has no single criterion to point at.
// Detail is a short human-readable explanation. The JSON tags match the shape
// the plan gate and refinement session view both render.
type AcceptanceFinding struct {
	Rule        string `json:"rule"`
	CriterionID string `json:"criterion_id,omitempty"`
	Detail      string `json:"detail"`
}

// Acceptance-criteria finding rules. These are the machine-readable contract:
// consumers key on the rule name, not the human-readable detail prose.
const (
	RuleNoBlockingCriterion = "no_blocking_criterion"
	RuleMissingSourceRef    = "missing_source_ref"
	RuleMissingRationale    = "missing_rationale"
	RuleEmptyID             = "empty_id"
	RuleDuplicateID         = "duplicate_id"
)

// EvaluateAcceptanceCriteria runs the deterministic acceptance-criteria rules
// over a decoded Verification and returns the findings. It always returns a
// non-nil slice so a payload records [] (not null) on a clean-and-checked
// input — the "checked and clean" contract shared with the scope pre-check.
//
// Rules:
//   - no_blocking_criterion — no criterion is effectively blocking AND
//     out_of_scope is empty. A non-empty out_of_scope is the justified escape
//     hatch: it declares what the change deliberately does not cover, so an
//     absent blocking criterion is not necessarily a gap.
//   - missing_source_ref — an explicit criterion with no source_ref.
//   - missing_rationale — an inferred criterion with no rationale
//     (defense-in-depth: the schema conditional normally rejects this
//     upstream, but the pre-check stays order-independent).
//   - empty_id / duplicate_id — id integrity for the join key.
func EvaluateAcceptanceCriteria(v Verification) []AcceptanceFinding {
	findings := []AcceptanceFinding{}

	hasBlocking := false
	seen := make(map[string]struct{}, len(v.AcceptanceCriteria))
	for _, c := range v.AcceptanceCriteria {
		if CriterionBlocking(c) {
			hasBlocking = true
		}
		if c.ID == "" {
			findings = append(findings, AcceptanceFinding{
				Rule:   RuleEmptyID,
				Detail: "acceptance criterion has an empty id (ids are the join key across execution, evidence, and feedback)",
			})
		} else if _, dup := seen[c.ID]; dup {
			findings = append(findings, AcceptanceFinding{
				Rule:        RuleDuplicateID,
				CriterionID: c.ID,
				Detail:      "duplicate acceptance criterion id (ids must be unique within a plan)",
			})
		} else {
			seen[c.ID] = struct{}{}
		}
		if c.Source == CriterionSourceExplicit && c.SourceRef == "" {
			findings = append(findings, AcceptanceFinding{
				Rule:        RuleMissingSourceRef,
				CriterionID: c.ID,
				Detail:      "explicit criterion is missing source_ref (an explicit criterion must cite where the ticket/spec states it)",
			})
		}
		if c.Source == CriterionSourceInferred && c.Rationale == "" {
			findings = append(findings, AcceptanceFinding{
				Rule:        RuleMissingRationale,
				CriterionID: c.ID,
				Detail:      "inferred criterion is missing rationale (an inferred criterion must justify why it was derived)",
			})
		}
	}

	if !hasBlocking && len(v.OutOfScope) == 0 {
		findings = append(findings, AcceptanceFinding{
			Rule:   RuleNoBlockingCriterion,
			Detail: "no blocking acceptance criterion and no verification.out_of_scope justification (a plan must carry at least one blocking criterion or declare what is deliberately out of scope)",
		})
	}

	return findings
}

// CriterionBlocking applies the schema's blocking default: an omitted (nil)
// blocking is true, matching the AcceptanceCriterion.Blocking pointer contract.
func CriterionBlocking(c AcceptanceCriterion) bool {
	return c.Blocking == nil || *c.Blocking
}

// AcceptanceSkippableOutOfScope reports whether a plan's verification declares
// out_of_scope with ZERO acceptance_criteria — the single canonical condition
// (#1657) under which the acceptance stage carries no observable criterion to
// validate and can be auto-terminated rather than dispatched. It is the
// out_of_scope escape hatch (the same justification that suppresses
// no_blocking_criterion in EvaluateAcceptanceCriteria) applied to the acceptance
// stage: a plan that declares what it deliberately does NOT cover AND enumerates
// no acceptance criteria has nothing for a validator to check, so dispatching a
// degenerate no-observable-change acceptance stage only stalls the run.
//
// This is the sole source of the skip condition. The pre-existing inlined
// predicate at internal/prompt/prompt.go (the #1612 trivial-pass branch)
// computes the identical condition; it is intentionally NOT refactored to call
// this — prompt.go is out of this change's scope, and both compute the same
// boolean, so the transient duplication is behavior-neutral and DRY-able in a
// follow-up when prompt.go is legitimately in a run's scope.
func AcceptanceSkippableOutOfScope(v Verification) bool {
	return len(v.OutOfScope) > 0 && len(v.AcceptanceCriteria) == 0
}

// Acceptance short-circuit audit-payload contract (#1728). The orchestrator's
// pre-spawn acceptance short-circuit records an acceptance_outcome_recorded
// entry whose payload carries a `basis` field naming WHY the verdict was
// recorded without a runner spawn; auditcomplete reads the SAME field to exempt
// the no-trace short-circuited stage from the trace-required rule. Defining the
// key and its sole legal value ONCE here — the plan package is imported by both
// backend/internal/orchestrator and backend/internal/auditcomplete and imports
// no project packages, so there is no import cycle — makes a producer/consumer
// payload-shape drift a compile error rather than a silent runtime miss. The
// emit helper, the auditcomplete reader, and both packages' tests all reference
// these constants instead of free-typed strings.
const (
	// AcceptanceBasisKey is the acceptance_outcome_recorded payload key naming
	// the short-circuit basis. A normally server-recorded verdict never sets
	// it, so its presence unambiguously discriminates the pre-spawn
	// short-circuit from an ordinary validator-shipped verdict.
	AcceptanceBasisKey = "basis"
	// AcceptanceBasisEmptyCriteria is the ONLY basis value auditcomplete honors
	// for the trace exemption (#1728): an approved plan with ZERO
	// acceptance_criteria AND ZERO verification.out_of_scope. A future
	// "all-skip-with-basis" basis is added by #1748 when it ships; until then,
	// any other basis value is NOT exempted.
	AcceptanceBasisEmptyCriteria = "empty-criteria"
)

// AcceptanceSkippableEmptyCriteria reports whether a plan's verification carries
// ZERO acceptance_criteria AND ZERO verification.out_of_scope — the sole
// canonical #1728 condition under which the acceptance stage has no observable
// criterion to validate AND no out_of_scope justification, so the orchestrator
// short-circuits it straight to succeeded with a deterministic verdict=passed
// entry (basis AcceptanceBasisEmptyCriteria) instead of spawning a runner for a
// no-op stage.
//
// It is deliberately DISJOINT from AcceptanceSkippableOutOfScope, which fires
// when out_of_scope is present with zero acceptance_criteria (the E38.3 domain):
// that predicate requires len(OutOfScope) > 0, this one requires
// len(OutOfScope) == 0, so at most one fires for any given plan. Together the
// two partition the "zero acceptance_criteria" space by whether an out_of_scope
// justification is present.
func AcceptanceSkippableEmptyCriteria(v Verification) bool {
	return len(v.AcceptanceCriteria) == 0 && len(v.OutOfScope) == 0
}
