package budget

// This file adds the PER-STAGE budget enforcement decision core (E48.55 /
// #2328), the stage-scoped sibling of the whole-run tripwire (EvaluateRun,
// runbudget.go). Where EvaluateRun gates a run's accumulated spend against
// operator-configured ceilings, EvaluateStage gates a SINGLE stage's summed
// cost against the stage's own workflow-declared limit_usd ceiling
// (spec.Stage.Budget.LimitUSD). Like the other two axes of ADR-030's budget
// story it is a pure function with NO repository dependency: the caller sums
// the stage's cost_recorded ledger and supplies two figures.
//
// The enforcement PREDICATE (StageEnforcementApplies) is deliberately the
// SINGLE owner of both arming clauses — the spec-major gate and the
// positive-limit opt-in — and EvaluateStage deliberately carries NO
// non-positive-limit guard of its own. That asymmetry with EvaluateRun (which
// folds `maxUSD > 0` into its own body) is the whole point: with the unset
// check owned in exactly one place, deleting EITHER clause is observable by a
// test rather than masked by a second layer. See the doc comments below.

// StageDecision is the outcome of an EvaluateStage call. It is fully populated
// regardless of whether the ceiling was crossed so the caller can log the
// figures either way; Over is the gate the caller keys the halt off.
type StageDecision struct {
	// Over is true when the stage's summed cost has reached or exceeded the
	// ceiling (exact equality counts as over, matching Evaluate's
	// TestEvaluate_ExactHundredPercentIsOver convention).
	Over bool
	// CostUSD / LimitUSD echo the figures the caller passed in.
	CostUSD  float64
	LimitUSD float64
}

// EvaluateStage compares a stage's summed cost against its declared ceiling
// and reports whether the ceiling has been reached or exceeded. The caller
// supplies costUSD (the sum of the stage's cost_recorded ledger) and limitUSD
// (spec.Stage.Budget.LimitUSD); EvaluateStage never queries.
//
// Over is `costUSD >= limitUSD`, so exact equality is a breach — the same
// exact-100%-is-over convention as Evaluate (budget.go, pinned by
// TestEvaluate_ExactHundredPercentIsOver).
//
// This function deliberately carries NO non-positive-limit guard. Unlike
// EvaluateRun (which disables its tripwire when maxUSD <= 0 inside its own
// body), the unset check here is owned SOLELY by StageEnforcementApplies. That
// is what makes each of that predicate's two clauses observable by deletion:
// a second guard inside EvaluateStage would mask the limit clause's
// counterfactual. A caller that skips StageEnforcementApplies and passes
// limitUSD <= 0 gets Over=true (cost >= 0), and that is the documented,
// intended shape — every production caller (checkStageBudget) gates on
// StageEnforcementApplies first.
func EvaluateStage(costUSD, limitUSD float64) StageDecision {
	return StageDecision{
		Over:     costUSD >= limitUSD,
		CostUSD:  costUSD,
		LimitUSD: limitUSD,
	}
}

// StageEnforcementApplies is the SINGLE owner of the two clauses that arm
// per-stage cost enforcement. It returns true only when BOTH hold:
//
//   - specVersionMajor >= 2: the explicit spec-major gate for #2328's trap 1
//     (a v0/v1 document must never newly fire). This is defence in depth on
//     top of the STRUCTURAL guarantee that $defs/budget on workflow-v0 and
//     workflow-v1 is additionalProperties:false and declares only max_tokens /
//     max_runtime_minutes / enforcement — no limit_usd — so a legacy document
//     cannot even EXPRESS the field that arms enforcement, and a legacy
//     document carrying limit_usd fails schema validation (fails open before
//     this predicate is reached). The clause remains as a belt-and-braces rail
//     so a future schema slip or a programmatically built legacy *Spec cannot
//     silently arm the ceiling.
//   - limitUSD > 0: the operator opt-in. An absent or zero limit_usd means no
//     ceiling on this stage; enforcement is off by default.
//
// Both clauses live HERE and only here (EvaluateStage has no unset guard), so a
// counterfactual that deletes either clause flips the result of a fixture that
// isolates it (see stagebudget_test.go).
func StageEnforcementApplies(specVersionMajor int, limitUSD float64) bool {
	return specVersionMajor >= 2 && limitUSD > 0
}
