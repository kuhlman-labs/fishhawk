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
