package plan

import (
	"regexp"
	"slices"
	"strings"
)

// acceptance_check.go holds the pure, deterministic acceptance-criteria rule
// set (#1596, E34.5 / ADR-052). It lives in the plan package — which already
// owns Verification/AcceptanceCriterion and imports no project package (the
// E72.2 / #3326 seeded-scenario NAMES are the closed set seedScenarioNames
// below, carried inline so the plan package stays a leaf and can never form a
// cycle with the packages that import it) — so it is the SINGLE source both the server's plan-gate pre-check
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
	// RuleUndecidableCriterion flags a criterion whose STATEMENT requires a
	// capability the sandboxed acceptance executor does not have (#2512,
	// layer 3). It is ADVISORY: it never refuses a plan, because a criterion
	// may legitimately name a capability in prose while still being drivable.
	// Its purpose is preventive — an author who sees it up front declares the
	// criterion up front. For a genuinely undecidable capability that means
	// skip_expected + expectation_basis (or requires_live_validation); but for
	// a hermetic in-process check of an external TRIGGER the correct fix is to
	// NAME its in-repository harness in verify_hint (#3163) — or, for state
	// the acceptance preview can seed, the seeded-scenario CATALOG NAME
	// (E72.2 / #3326) — which suppresses the finding rather than skipping a
	// check the executor could actually perform. See UnevaluableCriteria for
	// the conjunctive suppression.
	RuleUndecidableCriterion = "undecidable_criterion"
	// RuleMissingLiveValidationMarker flags a criterion whose STATEMENT names a
	// LIVE forge/deploy/external TARGET but which is NOT marked
	// requires_live_validation (#2845, E54.31). It is ADVISORY: like
	// undecidable_criterion it never refuses a plan.
	//
	// It is DELIBERATELY not a duplicate of undecidable_criterion. That rule's
	// exemption is EITHER sanctioned declaration — skip_expected-with-basis OR
	// requires_live_validation — which is correct for the external-TRIGGER
	// class it covers. For a live TARGET the weaker marking is not enough:
	// only requires_live_validation auto-files the tracked
	// operator-validation walk on plan approval, so a live-target criterion
	// marked skip_expected-with-basis alone is silent under
	// undecidable_criterion and silently loses its walk. This rule's exemption
	// is therefore requires_live_validation ALONE — that gap is the defect
	// #2845 documents across four runs.
	//
	// There is NO cross-rule suppression: a wholly-unmarked live-target
	// criterion draws one finding from EACH rule. That is complementary, not
	// redundant — undecidable_criterion says "declare it (either marking)",
	// this rule says "the weaker marking will not suffice here" — and it
	// avoids a two-step in which an author applies the weaker remedy and only
	// then learns it was insufficient.
	RuleMissingLiveValidationMarker = "missing_live_validation_marker"
	// RuleAllCriteriaSkipExpected flags a PLAN-LEVEL shape: the plan declares
	// at least one acceptance criterion and EVERY one is marked skip_expected
	// with a non-whitespace expectation_basis (#3026, E32.50). That is the
	// #1748 condition the orchestrator short-circuits on, so the plan is
	// already known AT APPROVAL TIME to skip acceptance entirely — no runner
	// spawn, no preview, ZERO criteria verified, and a recorded not_validated
	// verdict rather than a pass (#2347).
	//
	// It is ADVISORY and NEVER refuses a plan. A change may genuinely have no
	// in-sandbox observable, and the issue's own counter-example (#2894) is
	// exactly that case — the rule exists so the approver is asked the
	// question, not so the shape is banned.
	//
	// It fires exactly when AcceptanceSkippableAllSkipWithBasis(v) is true AND
	// the plan does not declare acceptance_surface: none, and deliberately
	// REUSES that predicate rather than re-deriving the condition, so the
	// plan-gate advisory and the orchestrator's runtime short-circuit are the
	// SAME boolean by construction and cannot drift — including on the
	// whitespace-only expectation_basis edge, where both agree the plan is NOT
	// all-skip and acceptance dispatches normally. The one narrowing (E72.1 /
	// #3325) reads "fires when the short-circuit WOULD run": under
	// acceptance_surface: none the stage is omitted at plan approval, so the
	// short-circuit never runs and the advisory is suppressed as moot; the
	// predicate itself is untouched, so a stage that still exists
	// short-circuits exactly as before.
	//
	// Like the two rules above it applies NO cross-rule suppression: an
	// all-skip plan whose criteria name a live target still draws its
	// missing_live_validation_marker findings, one per criterion.
	RuleAllCriteriaSkipExpected = "all_criteria_skip_expected"
	// RuleCriterionRestatesTest flags a criterion whose verify_hint names ONLY
	// in-repository Go tests — a `_test.go` path, a bare `TestFoo` name, `go
	// test`, `scripts/test`, a unit/table/golden test, pgtest/httptest — and
	// NO operator-observable surface (E72.1 / #3325). Such a criterion RESTATES
	// the plan's test_strategy: the acceptance agent cannot observe a Go test
	// passing on the localhost preview, so the criterion adds nothing the
	// implement-verify gate does not already prove. The five surfaces the
	// acceptance agent CAN observe are an HTTP route/response/status code, an
	// MCP tool result (`fishhawk_*`), a CLI exit code/stderr/stdout, a
	// rendered prompt, and a persisted audit row.
	//
	// It is ADVISORY and never refuses a plan (promotion to a refusal is
	// deferred by the issue). EXEMPT: a criterion already declared
	// skip_expected-with-basis or requires_live_validation (the sanctioned
	// declarations — re-flagging them trains the operator to ignore the
	// rule), and an EMPTY verify_hint (the rule is conservative: it fires on
	// positive test-only evidence, never on the absence of a hint). RESIDUAL,
	// stated honestly: an author can clear the finding by naming a surface in
	// prose; acceptable for an advisory rule. Refinement intake is
	// structurally inert here because drafts carry no verify_hint.
	RuleCriterionRestatesTest = "criterion_restates_test"
	// RuleNoObservableCriterion is the PLAN-LEVEL companion of
	// criterion_restates_test (E72.1 / #3325): the plan declares at least one
	// criterion, is NOT the all-skip shape, does NOT declare
	// acceptance_surface: none, and EVERY criterion is either a sanctioned
	// declaration (skip_expected-with-basis / requires_live_validation) or
	// drew criterion_restates_test — so the acceptance stage would dispatch a
	// runner with nothing it can observe. One finding per plan, empty
	// CriterionID. ADVISORY. The honest remedies are the same two the
	// per-criterion rule names: give one criterion an observable surface, or
	// declare acceptance_surface: none so the stage is omitted at approval.
	// It is NOT emitted for an all-skip plan (that shape already draws
	// all_criteria_skip_expected — the two advisories would say the same
	// thing twice) and NOT under acceptance_surface: none (the declaration IS
	// the answer). Those are the only two cross-rule suppressions this file
	// applies.
	RuleNoObservableCriterion = "no_observable_criterion"
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
//   - undecidable_criterion — a criterion whose statement requires a capability
//     the sandboxed acceptance executor lacks (a live MCP client, a real
//     operator session, a running external instance, a live forge round-trip, a
//     real webhook delivery), and which is NOT already marked skip_expected
//     with a basis or requires_live_validation. As of #3163 it ALSO exempts a
//     NON-liveTarget capability match whose verify_hint (verify_hint alone —
//     never expectation_basis, never the statement) names an in-repository /
//     repository-local harness: a hermetic in-process test genuinely CAN drive
//     an external TRIGGER (an MCP client, an operator session, a webhook
//     delivery), so a named harness is positive evidence the executor can
//     decide the criterion. As of E72.2 / #3326 the same non-liveTarget
//     exemption is ALSO earned by a verify_hint carrying a KNOWN seeded-
//     scenario catalog name as a whole token (verifyHintNamesSeedScenario —
//     the acceptance preview's dev-only fixture surface materializes that
//     scenario for the sandbox). A liveTarget capability (a live forge/deploy/
//     external instance) is never exempted either way — no in-repository
//     harness or seeded scenario stands one up (#2845 preserved). Advisory only.
//   - missing_live_validation_marker — a criterion whose statement names a LIVE
//     forge/deploy/external TARGET and which is NOT marked
//     requires_live_validation. Its exemption is that marker ALONE:
//     skip_expected-with-basis does not exempt, because only the marker files
//     the tracked operator-validation walk (#2845). Advisory only.
//   - criterion_restates_test — a criterion whose verify_hint names only
//     in-repository Go test evidence and no operator-observable surface, so it
//     restates test_strategy (E72.1 / #3325). Exempt for skip_expected-with-
//     basis / requires_live_validation; silent on an empty hint. Advisory only.
//   - no_observable_criterion — PLAN-LEVEL: at least one criterion, not
//     all-skip, no acceptance_surface: none, and every criterion is a declared
//     skip or restates a test (E72.1 / #3325). One finding per plan. Advisory.
//   - all_criteria_skip_expected — the plan declares at least one acceptance
//     criterion and EVERY one is skip_expected-with-basis, so acceptance will
//     short-circuit to not_validated having verified ZERO criteria (#3026).
//     One finding per PLAN, never one per criterion. Advisory only. Suppressed
//     under acceptance_surface: none, where the stage is omitted at approval
//     and the short-circuit never runs (E72.1 / #3325).
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

	// Layer 3 (#2512): the undecidable-criterion matcher rides THIS call so
	// both consumers of the shared rule set (the server plan gate and
	// refinement.EvaluateDraftCriteria) get it from one place, per this file's
	// single-source contract.
	findings = append(findings, UnevaluableCriteria(v)...)

	// #2845 (E54.31): the live-validation-marker matcher rides the SAME call
	// for the same reason — one evaluator, both surfaces, no second copy.
	findings = append(findings, MissingLiveValidationMarker(v)...)

	// E72.1 (#3325): the test-only-evidence matcher rides the same call. The
	// per-criterion findings are computed ONCE and the plan-level
	// no_observable_criterion rule reads that same slice, so the two cannot
	// disagree about which criteria restate a test.
	restates := TestOnlyCriteria(v)
	findings = append(findings, restates...)
	findings = append(findings, noObservableCriterion(v, restates)...)

	// #3026 (E32.50): the all-skip short-circuit advisory rides the same call,
	// for the same single-source reason. It is PLAN-LEVEL, so it emits at most
	// ONE finding with an empty CriterionID — the shape no_blocking_criterion
	// already uses for a presence-level condition. The condition is READ from
	// AcceptanceSkippableAllSkipWithBasis, the very predicate the orchestrator
	// short-circuits on, so gate advisory and runtime behaviour are one boolean.
	//
	// E72.1 (#3325) NARROWS the invariant to "fires when the short-circuit
	// WOULD run": under acceptance_surface: none the acceptance stage is
	// omitted at plan approval, so the short-circuit never runs and the
	// advisory about it is moot — it is suppressed. A plan carrying the stage
	// (no declaration) keeps the exact prior boolean.
	if AcceptanceSkippableAllSkipWithBasis(v) && !DeclaresNoAcceptanceSurface(v) {
		findings = append(findings, AcceptanceFinding{
			Rule: RuleAllCriteriaSkipExpected,
			Detail: "every acceptance criterion is marked skip_expected with an expectation_basis, so the acceptance stage " +
				"will short-circuit server-side to a not_validated verdict with no runner spawn and no preview — ZERO criteria " +
				"are verified. The run stays merge-eligible but this is NOT a validated pass (#2347). Author a drivable " +
				"criterion if one genuinely exists; this finding is advisory and never refuses the plan.",
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
// nothing from the repo, so there is no import cycle — makes a producer/consumer
// payload-shape drift a compile error rather than a silent runtime miss. The
// emit helper, the auditcomplete reader, and both packages' tests all reference
// these constants instead of free-typed strings.
const (
	// AcceptanceBasisKey is the acceptance_outcome_recorded payload key naming
	// the short-circuit basis. A normally server-recorded verdict never sets
	// it, so its presence unambiguously discriminates the pre-spawn
	// short-circuit from an ordinary validator-shipped verdict.
	AcceptanceBasisKey = "basis"
	// AcceptanceBasisEmptyCriteria is a basis value auditcomplete honors for the
	// trace exemption (#1728): an approved plan with ZERO acceptance_criteria AND
	// ZERO verification.out_of_scope.
	AcceptanceBasisEmptyCriteria = "empty-criteria"
	// AcceptanceBasisAllSkipWithBasis is the second basis value auditcomplete
	// honors for the trace exemption (#1748): an approved plan whose EVERY
	// acceptance criterion carries skip_expected with a non-empty
	// expectation_basis — so there is nothing the sandboxed acceptance agent
	// could observe and the stage short-circuits with no runner spawn. Any basis
	// value OTHER than these two is NOT exempted.
	AcceptanceBasisAllSkipWithBasis = "all-skip-with-basis"
)

// Acceptance short-circuit verdict vocabulary (#2347). The pre-spawn
// short-circuit verified exactly ZERO criteria — no runner, no preview, no
// observation — yet it used to record the same `passed`/`accepted` words a
// validator-shipped pass records. Downstream that word gates the merge (ADR-049
// decision #6) and is what an operator reads in the status comment, so an
// ABSENCE of verification rendered as certification. These two constants are the
// third, honest disposition the short-circuit emits instead.
//
// SERVER-INTERNAL ONLY — no WIRE producer may ship this verdict. The acceptance
// ship endpoint (POST /v0/runs/{run_id}/acceptance) deliberately still rejects
// any verdict other than passed/failed (acceptanceBody.validate), so
// not_validated can ONLY originate server-side from the orchestrator
// short-circuit. That keeps it unforgeable by a validator and keeps an existing
// recorded `passed` verdict at its exact prior meaning (no migration).
//
// Defining them HERE — the plan package imports nothing from the repo and is
// already imported by orchestrator, server, and auditcomplete — makes a
// producer/consumer drift a compile error rather than a silent runtime miss.
const (
	// AcceptanceVerdictNotValidated is the acceptance_outcome_recorded `verdict`
	// value for a short-circuited stage: merge-eligible, but recorded as having
	// verified nothing.
	AcceptanceVerdictNotValidated = "not_validated"
	// AcceptanceOutcomeNotValidated is the render-vocabulary twin of
	// accepted/rejected — the `outcome` field the issue-comment and PR-comment
	// status templates read.
	AcceptanceOutcomeNotValidated = "not_validated"
	// AcceptanceCriteriaLiveValidationKey is the acceptance_outcome_recorded
	// payload key carrying how many of the plan's acceptance criteria are marked
	// requires_live_validation. It distinguishes a skip with a TRACKED
	// operator-validation walk (#2338 / #2345) from one skipped on any other
	// basis — the part of a not-validated outcome an operator actually acts on.
	AcceptanceCriteriaLiveValidationKey = "criteria_live_validation"
)

// LiveValidationCriteriaCount counts the acceptance criteria a plan marks
// RequiresLiveValidation. A thin count wrapper over LiveValidationCriteria so
// the short-circuit emit site records the criteria_live_validation payload field
// without re-walking the criteria itself — one selector, no second copy to
// drift.
func LiveValidationCriteriaCount(v Verification) int {
	return len(LiveValidationCriteria(v))
}

// AcceptanceSkippableEmptyCriteria reports whether a plan's verification carries
// ZERO acceptance_criteria AND ZERO verification.out_of_scope — the sole
// canonical #1728 condition under which the acceptance stage has no observable
// criterion to validate AND no out_of_scope justification, so the orchestrator
// short-circuits it straight to succeeded with a deterministic
// verdict=AcceptanceVerdictNotValidated entry (basis
// AcceptanceBasisEmptyCriteria) instead of spawning a runner for a no-op stage.
// Zero criteria were verified, so the recorded verdict says so (#2347).
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

// AcceptanceSkippableAllSkipWithBasis reports whether a plan's verification
// carries at least one acceptance criterion AND EVERY criterion is marked
// skip_expected with a non-empty expectation_basis — the #1748 condition under
// which no criterion can be validated against the localhost preview, so the
// orchestrator short-circuits the acceptance stage straight to a
// AcceptanceVerdictNotValidated verdict (basis AcceptanceBasisAllSkipWithBasis)
// with no runner spawn and no preview — zero criteria were verified, and the
// recorded verdict says so rather than certifying a pass (#2347).
//
// It requires len(AcceptanceCriteria) > 0, so it is disjoint from
// AcceptanceSkippableEmptyCriteria (which requires zero criteria): at most one
// of the two short-circuit predicates fires for any given plan. A single
// criterion that is drivable (SkipExpected==false) or marked but missing a
// basis (whitespace-only ExpectationBasis) makes this false — the stage then
// dispatches normally so the drivable criterion is actually validated.
func AcceptanceSkippableAllSkipWithBasis(v Verification) bool {
	if len(v.AcceptanceCriteria) == 0 {
		return false
	}
	for _, c := range v.AcceptanceCriteria {
		if !c.SkipExpected || strings.TrimSpace(c.ExpectationBasis) == "" {
			return false
		}
	}
	return true
}

// LiveValidationCriteria returns exactly the acceptance criteria a plan marks
// RequiresLiveValidation — those whose true verification needs a LIVE
// forge/deploy/external target the default-deny sandbox lacks (#2045). It is the
// single selector both the approval side-effect (which files-or-links an
// operator-validation walk when the result is non-empty) and the run/gate-view
// surface dispatch through, mirroring AcceptanceSkippableAllSkipWithBasis as the
// shared source of truth. Returns a non-nil empty slice when nothing is marked
// (a plan with no live-validation criterion), so the approval hook no-ops via a
// len==0 check with no nil special-casing.
func LiveValidationCriteria(v Verification) []AcceptanceCriterion {
	out := []AcceptanceCriterion{}
	for _, c := range v.AcceptanceCriteria {
		if c.RequiresLiveValidation {
			out = append(out, c)
		}
	}
	return out
}

// Undecidable acceptance vocabulary (#2512, E48.78 layer 4). These three
// constants name the third acceptance disposition, and they exist here — in the
// plan package, which imports nothing from the repo and is already imported by
// server, orchestrator and auditcomplete — so a producer/consumer drift is a
// COMPILE error rather than a silent runtime miss, exactly as #2347 did for
// not_validated.
//
// THE PARTITION. Three names share ONE contract (merge-eligible, never a pass,
// a distinct state string, operator acknowledgement in the merge verdict) and
// are partitioned by a single total question — was there evidence, and what did
// it say?
//
//   - NO EVIDENCE WAS POSSIBLE: the orchestrator's PRE-SPAWN short-circuit
//     settles from the PLAN alone (zero criteria, or every criterion
//     skip_expected-with-basis). No runner, no preview, no observation. That is
//     AcceptanceVerdictNotValidated, and it is unchanged.
//   - EVIDENCE SAYS A CRITERION FAILED: a real defect. `failed` ->
//     acceptance_triage, unchanged, discharged only by the #2474 arbitration
//     verb.
//   - EVIDENCE SAYS A CRITERION COULD NOT BE DECIDED: the stage RAN, drove the
//     preview, and reported per-criterion rows of which at least one is
//     undecidable. That is the new `undecidable`.
//
// The three are MUTUALLY EXCLUSIVE BY CONSTRUCTION, not by convention:
// not_validated skips dispatch entirely so no criteria rows can exist, and the
// precedence ladder puts failed strictly above undecidable so a single failed
// row keeps the run in triage exactly as today.
//
// SERVER-DERIVED AND UNFORGEABLE. Only the PER-CRITERION row value
// (AcceptanceResultUndecidable) may cross the wire: the acceptance ship
// endpoint's top-level verdict enum keeps admitting nothing but passed/failed,
// so AcceptanceVerdictUndecidable can ONLY originate server-side from the
// aggregation ladder over the rows.
const (
	// AcceptanceResultUndecidable is the PER-CRITERION `result` value — the
	// only one of these three a wire producer may ship. It says the acceptance
	// agent drove the target and genuinely could not decide this criterion; it
	// is NOT a defect (a defect is `failed`) and NOT a pass. It travels with a
	// non-empty undecidable_reason.
	AcceptanceResultUndecidable = "undecidable"
	// AcceptanceVerdictUndecidable is the SERVER-DERIVED
	// acceptance_outcome_recorded `verdict` value: at least one non-retired row
	// is undecidable and no row failed. Merge-eligible under operator
	// acknowledgement, never a silent pass.
	AcceptanceVerdictUndecidable = "undecidable"
	// AcceptanceOutcomeUndecidable is the render-vocabulary twin — the
	// `outcome` field the issue-comment and PR-comment status templates read,
	// mirroring AcceptanceOutcomeNotValidated.
	AcceptanceOutcomeUndecidable = "undecidable"
)

// unevaluableCapability is one capability the sandboxed acceptance executor
// does not have, plus the lowercase statement phrases that name it. The phrases
// are deliberately MULTI-WORD: a bare "github" or "webhook" appears in ordinary
// drivable prose, and an advisory rule that fires on ordinary prose trains the
// operator to ignore it.
//
// liveTarget marks the entries whose capability is a LIVE forge/deploy/external
// TARGET rather than merely an external TRIGGER EVENT (#2845, E54.31). It is
// what MissingLiveValidationMarker's M1 matcher reads, so that rule REUSES this
// one corpus instead of carrying a second phrase list that would drift from it.
// The flag is metadata only: no phrase string moves or changes, so
// UnevaluableCriteria's behaviour is byte-identical.
type unevaluableCapability struct {
	capability string
	phrases    []string
	liveTarget bool
}

// unevaluableCapabilities is the deterministic term corpus behind
// UnevaluableCriteria. Order is fixed so the findings a given plan produces are
// stable across runs (the pre-check payload is compared byte-for-byte in the
// audit log).
//
// liveTarget (#2845) partitions the corpus for MissingLiveValidationMarker. The
// MCP-client, operator-session and webhook-delivery entries are deliberately
// FALSE: the plan artifact schema scopes requires_live_validation to a criterion
// needing "a LIVE forge/deploy/external target the default-deny sandbox lacks
// (not merely an external trigger event, which skip_expected covers)". For those
// three, skip_expected with an expectation_basis is the doctrinally COMPLETE
// marking and no operator-validation walk is owed — demanding the marker there
// would fire on correctly-authored criteria and auto-file spurious walks, which
// is the habituation failure #2845 documents. Widening the rule to one of them
// is a one-line flip here; the excluded-class control test records the decision.
var unevaluableCapabilities = []unevaluableCapability{
	{
		capability: "a live MCP client / MCP tool call",
		phrases: []string{
			"mcp client", "mcp tool call", "mcp tool invocation", "mcp tool from",
			"via the mcp server", "live mcp", "through the mcp",
		},
	},
	{
		capability: "a real operator session",
		phrases: []string{
			"operator session", "real operator", "interactive operator",
			"a human operator", "operator drives",
		},
	},
	{
		capability: "a running external instance / deployed environment",
		phrases: []string{
			"deployed environment", "live deployment", "production environment",
			"staging environment", "external instance", "running cluster",
			"deployed instance",
		},
		liveTarget: true,
	},
	{
		capability: "a live forge round-trip",
		phrases: []string{
			"live github", "real github", "github api", "live gitlab", "real gitlab",
			"gitlab api", "live forge", "real forge", "against github.com",
		},
		liveTarget: true,
	},
	{
		capability: "a real webhook delivery",
		phrases: []string{
			"real webhook", "live webhook", "webhook delivery", "actual webhook",
		},
	},
}

// UnevaluableCriteria flags every acceptance criterion whose STATEMENT requires
// a capability the sandboxed acceptance executor lacks (#2512, layer 3) and
// that is not already marked as such. It is the DETECTIVE half of layer 3; the
// preventive half is the planner prompt's authoring guardrail, which tells the
// author to mark such a criterion up front.
//
// Matching is deterministic and case-insensitive over the statement against the
// fixed unevaluableCapabilities corpus. A criterion matching more than one
// capability yields exactly ONE finding, naming the first capability matched in
// corpus order — one criterion, one finding, so a downstream count is a count
// of criteria.
//
// EXEMPTIONS. A criterion already carrying SkipExpected with a non-whitespace
// ExpectationBasis, or RequiresLiveValidation, is NOT flagged: those are the
// SANCTIONED declarations of exactly this condition. Re-flagging a criterion
// whose author already did the right thing trains the operator to ignore the
// rule, which is how an advisory rule dies. criterionDeclaresUnevaluable runs
// first and applies that exemption unchanged.
//
// VERIFY-HINT SUPPRESSION (#3163). After a corpus phrase matches, the finding
// is additionally suppressed when the matched capability is NOT a liveTarget
// AND the criterion's verify_hint names an in-repository / repository-local
// harness (verifyHintDeclaresInRepo). The conjunction is load-bearing:
//   - !liveTarget preserves #2845 — no in-repository harness can stand up a
//     live forge/deploy/external instance, so those keep firing regardless of
//     the hint. Only the external-TRIGGER classes (a live MCP client, a real
//     operator session, a real webhook delivery), which a hermetic in-process
//     test genuinely can fabricate, become suppressible.
//   - verifyHintDeclaresInRepo reads verify_hint ALONE — positive evidence the
//     sandboxed executor CAN decide the criterion (buildAcceptance's Posture B
//     sanctions repository-local validation on exactly that signal).
//   - verifyHintNamesSeedScenario (E72.2 / #3326) is OR-ed with it under the
//     SAME !liveTarget conjunct: a verify_hint naming a known seeded-scenario
//     catalog name is positive evidence the sandbox can materialize the state
//     the criterion needs (buildAcceptance's "Seeded fixtures" section), so an
//     external-TRIGGER match whose hint says which scenario to seed is
//     sandbox-decidable. The disjunct never widens the liveTarget classes.
//
// The suppression uses `continue`, NOT `break`: a suppressed non-liveTarget
// match must not stop the scan, so a LATER liveTarget capability in the SAME
// statement still fires. The emit path keeps the original `break` (one
// criterion, one finding, first capability in corpus order).
//
// ADVISORY ONLY. The finding never refuses a plan — a criterion may
// legitimately name a capability in prose while being perfectly drivable.
// Returns a non-nil empty slice when nothing is flagged.
func UnevaluableCriteria(v Verification) []AcceptanceFinding {
	findings := []AcceptanceFinding{}
	for _, c := range v.AcceptanceCriteria {
		if criterionDeclaresUnevaluable(c) {
			continue
		}
		statement := strings.ToLower(c.Statement)
		for _, uc := range unevaluableCapabilities {
			if !containsAnyPhrase(statement, uc.phrases) {
				continue
			}
			// #3163: a non-liveTarget capability whose verify_hint names an
			// in-repository harness is sandbox-decidable; E72.2 / #3326 extends
			// the same conjunct to a hint naming a known seeded scenario.
			// continue (not break) so a later liveTarget capability in the same
			// statement still fires.
			if !uc.liveTarget && (verifyHintDeclaresInRepo(c) || verifyHintNamesSeedScenario(c)) {
				continue
			}
			detail := "criterion statement requires " + uc.capability +
				", which the sandboxed acceptance executor does not have; mark it skip_expected with an expectation_basis (or requires_live_validation) so it is declared up front rather than reported undecidable at acceptance"
			if !uc.liveTarget {
				// #3163 conditional guidance, mirroring the #3016 precedent on the
				// sibling rule. Left BYTE-IDENTICAL for a liveTarget match so a
				// live-target plan's audit payload bytes do not move.
				detail += ". If this criterion is in fact verified by an in-repository / in-process harness, name that harness in verify_hint rather than marking it skip_expected — marking a sandbox-decidable check skips verification the executor could actually perform (#3163)"
			}
			findings = append(findings, AcceptanceFinding{
				Rule:        RuleUndecidableCriterion,
				CriterionID: c.ID,
				Detail:      detail,
			})
			break
		}
	}
	return findings
}

// criterionDeclaresUnevaluable reports whether a criterion already carries the
// sanctioned declaration that it cannot be validated in the sandbox — either
// skip_expected with a non-whitespace expectation_basis, or
// requires_live_validation. It is the exemption predicate for
// UnevaluableCriteria, kept separate so the exemption is one named idea rather
// than an inline negation.
func criterionDeclaresUnevaluable(c AcceptanceCriterion) bool {
	if c.RequiresLiveValidation {
		return true
	}
	return c.SkipExpected && strings.TrimSpace(c.ExpectationBasis) != ""
}

// containsAnyPhrase reports whether lowered contains any of phrases. The caller
// lowercases once; the phrases in the corpus are already lowercase.
func containsAnyPhrase(lowered string, phrases []string) bool {
	for _, p := range phrases {
		if strings.Contains(lowered, p) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// missing_live_validation_marker (#2845, E54.31)
// ---------------------------------------------------------------------------

// livenessQualifiers are the adjectives that assert a statement is about the
// REAL thing rather than a sandbox stand-in. A qualifier alone is not a signal —
// "the real repo path is resolved" is perfectly drivable — so M2 below requires
// two further conjuncts on top of it.
var livenessQualifiers = []string{"live", "real", "actual", "production", "genuine"}

// liveActionNouns are the ACTIONS a live target is exercised through. The
// generic target nouns (issue, label, repository, tracker, backlog, board) are
// deliberately ABSENT: they appear constantly in drivable prose about parsing or
// rendering, and including them made the matcher fire on ordinary statements.
// "trip" carries the un-hyphenated "round trip" spelling; "round-trip" survives
// tokenization as one token.
var liveActionNouns = []string{
	"run", "runs", "walk", "walks", "apply", "applies",
	"round-trip", "round-trips", "trip", "dispatch", "dispatches",
}

// externalTargetNouns are the objects a live action is performed AGAINST. They
// are only ever consulted inside an "against …" phrase (conjunct 2), which is
// what separates "a real grooming run AGAINST this repo's backlog" from "a real
// run OF THE TEST SUITE".
var externalTargetNouns = []string{
	"repo", "repos", "repository", "repositories", "backlog", "tracker",
	"issue", "issues", "board", "project", "org", "organization",
	"forge", "github", "gitlab", "instance", "environment", "api",
}

// sandboxMarkers name a stand-in. Their presence is a NEGATION: a statement
// driving a fake/stub/localhost target is sandbox-validatable however live its
// prose reads.
var sandboxMarkers = []string{
	"fake", "stub", "mock", "localhost", "preview", "sandbox",
	"testdata", "fixture", "in-test",
}

// livenessProximityWindow is the maximum token distance for both proximity
// conjuncts. Four tokens spans an ordinary adjective/preposition run ("a real
// grooming run", "against this repo's backlog") without letting two unrelated
// clauses of one sentence pair up.
const livenessProximityWindow = 4

// acceptanceTokens splits a lowercased statement into comparable tokens: it
// trims surrounding punctuation and a trailing possessive, so "repo's" and
// "round-trip," normalize to "repo" and "round-trip". Interior hyphens are
// PRESERVED, which is what keeps "round-trip" a single token.
func acceptanceTokens(lowered string) []string {
	raw := strings.Fields(lowered)
	tokens := make([]string, 0, len(raw))
	for _, t := range raw {
		t = strings.Trim(t, ".,;:!?()[]{}\"'`")
		t = strings.TrimSuffix(t, "'s")
		t = strings.TrimSuffix(t, "’s")
		if t != "" {
			tokens = append(tokens, t)
		}
	}
	return tokens
}

// tokenIn reports whether tok is in set. The sets are small fixed slices, so a
// linear scan is both simplest and stable in ordering.
func tokenIn(tok string, set []string) bool {
	for _, s := range set {
		if tok == s {
			return true
		}
	}
	return false
}

// containsSandboxMarker reports whether a lowercased span names a stand-in
// ANYWHERE within it. It is the primitive behind M1's negation; the SPAN M1
// hands it is one CLAUSE, not the whole statement (see acceptanceClauses and
// liveTargetCorpusMatch). M2 uses the narrower token-windowed form below.
func containsSandboxMarker(lowered string) bool {
	return containsAnyPhrase(lowered, sandboxMarkers)
}

// clauseBoundary reports whether r ends a clause for acceptanceClauses.
//
// '.' is DELIBERATELY absent: splitting on it would cut "against github.com" —
// a liveTarget corpus phrase — in half, so the corpus entry could never match
// and M1 would lose a true positive to the clause split itself.
func clauseBoundary(r rune) bool {
	switch r {
	case ',', ';', ':', '\n', '\r', '—', '–':
		return true
	}
	return false
}

// acceptanceClauses splits a lowercased statement into clauses at ordinary
// clause punctuation. It exists so M1's sandbox-marker negation can be scoped
// to the clause the live-target phrase actually sits in.
//
// WHY THE SCOPE MATTERS. A whole-statement negation is a false-NEGATIVE hole:
// "a live GitHub round-trip closes the issue, unlike the fake transport used in
// the unit test" names a genuine live target, yet a single "fake" anywhere in
// the sentence used to disable M1 outright — recreating the exact defect #2845
// exists to close. A stand-in mentioned in a DIFFERENT clause does not make the
// live target sandbox-validatable, so it must not disarm the rule.
//
// RESIDUAL, stated honestly: a corpus phrase that straddles a clause boundary
// ("a live, real-forge round-trip") no longer matches. That shape does not
// occur in the corpus, whose phrases are short adjacent word pairs, and the
// fail direction is a missed advisory finding rather than a false refusal.
func acceptanceClauses(lowered string) []string {
	return strings.FieldsFunc(lowered, clauseBoundary)
}

// windowHasSandboxMarker reports whether any token in tokens[lo:hi] (clamped)
// is a sandbox marker. This is M2's conjunct 3, scoped to the against-phrase so
// a marker in an unrelated clause does not silently disarm the rule.
func windowHasSandboxMarker(tokens []string, lo, hi int) bool {
	// Clamped as EXPRESSIONS, not branches: lo arrives as a slice index so the
	// lower clamp is unreachable from the only caller, and a dead `if` would be
	// an untestable branch. min/max keep the bound safe without one.
	lo = max(lo, 0)
	hi = min(hi, len(tokens)-1)
	for i := lo; i <= hi; i++ {
		if tokenIn(tokens[i], sandboxMarkers) {
			return true
		}
	}
	return false
}

// livenessProximityMatch is M2: the three-conjunct proximity matcher. ALL THREE
// must hold.
//
//	(1) a liveness qualifier within livenessProximityWindow tokens BEFORE a live
//	    ACTION noun — "a real grooming run", "a live walk".
//	(2) an external-target preposition phrase: "against" within the same window
//	    BEFORE an external-target noun — "against this repo's backlog".
//	(3) NO sandbox marker inside that against-phrase window.
//
// Conjunct 1 alone is provably insufficient, which is why 2 and 3 exist: "a real
// run of the test suite regenerates the pages" satisfies conjunct 1 outright
// ("real" and "run" are adjacent) and is entirely sandbox-validatable. Conjunct
// 2 is what separates it from "a real grooming run against this repo's backlog";
// conjunct 3 separates that from "against the fake tracker in the integration
// test".
func livenessProximityMatch(tokens []string) bool {
	if !qualifierNearAction(tokens) {
		return false
	}
	for k, tok := range tokens {
		if tok != "against" {
			continue
		}
		for m := k + 1; m <= k+livenessProximityWindow && m < len(tokens); m++ {
			if !tokenIn(tokens[m], externalTargetNouns) {
				continue
			}
			if windowHasSandboxMarker(tokens, k, m+2) {
				continue
			}
			return true
		}
	}
	return false
}

// qualifierNearAction is conjunct 1 of livenessProximityMatch, named so the
// conjunct is one idea rather than an inline nested loop. It is a thin
// existence wrapper over qualifierActionIndices, which #3016 extracted so the
// polarity post-filter reads the SAME pairs the matcher fires on rather than
// re-deriving them. Behaviour is unchanged: a non-empty index set is exactly
// the old early `return true`.
func qualifierNearAction(tokens []string) bool {
	return len(qualifierActionIndices(tokens)) > 0
}

// qualifierActionIndices returns the index of EVERY liveness qualifier that
// satisfies conjunct 1 — a live ACTION noun within livenessProximityWindow
// tokens after it. Indices come out ascending because the scan is left to
// right, and each qualifier is reported at most once (the inner loop breaks on
// its first partner).
func qualifierActionIndices(tokens []string) []int {
	var idx []int
	for i, tok := range tokens {
		if !tokenIn(tok, livenessQualifiers) {
			continue
		}
		for j := i + 1; j <= i+livenessProximityWindow && j < len(tokens); j++ {
			if tokenIn(tokens[j], liveActionNouns) {
				idx = append(idx, i)
				break
			}
		}
	}
	return idx
}

// ---------------------------------------------------------------------------
// #3016 — polarity awareness: an ABSENCE assertion verified in-repository
// ---------------------------------------------------------------------------
//
// THE DEFECT. The two matchers above are polarity-BLIND: they ask whether a
// statement NAMES a live target, never whether it asserts that target is
// ABSENT. So "… is admitted even when no live forge membership lister is
// registered" — a criterion whose whole point is that NO forge call happens,
// decided by a Go test in this repository — drew the finding, and the finding's
// remedy told the author to mark it skip_expected, which SKIPS a criterion the
// sandbox can actually verify. That is the harm #3016 documents.
//
// THE POST-FILTER. A criterion that fired is skipped only when BOTH conjuncts
// hold:
//
//	(P) EVERY anchor in the statement carries a negation token within
//	    livenessProximityWindow tokens BEFORE it, and
//	(S) the criterion's stated verification method — verify_hint and
//	    expectation_basis, never the statement — names an IN-REPOSITORY harness.
//
// The conjunction is what preserves #2845. A genuinely live-dependent criterion
// carrying an in-repository basis ("validated by the httptest fake forge") has
// S true and P false, so it STILL fires: an in-repository basis alone can never
// suppress. And a single un-negated anchor anywhere makes P false, so a
// statement that negates one live target while asserting another still fires.
//
// (a) THE WINDOW IS livenessProximityWindow = 4 TOKENS, REUSED NOT REDEFINED.
// No second constant is introduced and nothing is widened, so the rule's
// suppression surface does not grow by one token.
//
// (b) THE WINDOW IS MEASURED BACK FROM AN ANCHOR, and an anchor is the
// LIVENESS-BEARING token — for M1 the head token of a liveTarget corpus-phrase
// occurrence, for M2 the liveness qualifier that satisfies conjunct 1. See
// liveTargetAnchors.
//
// (c) M2's EXTERNAL-TARGET NOUN IS NOT AN ANCHOR. In "no real grooming run
// against this repo's backlog is performed" the liveness claim is carried by
// "real … run"; "repo" and "backlog" are bystander objects. Negating the
// qualifier negates the assertion, whereas demanding a negator adjacent to the
// object noun would demand phrasing no criterion actually writes ("against this
// no repo") — the rule would then suppress nothing and the defect would stand.
//
// (d) THE PRIOR REVISION'S DEFECT was leaving (b) unspecified: measured to the
// object noun, the very fixture this rule must suppress sits 6 and 8 tokens
// from its negator and could not pass. Every negation fixture in the test file
// therefore carries its token count in a comment.

// acceptanceNegators are the tokens that negate a liveness claim. THE COST IS
// EXPLICIT: each entry is one more token that can suppress a finding, so the
// list is short and contains no word that reads as a negator only in context
// ("only", "except", "unless" are deliberately absent — they scope clauses, and
// this matcher does not parse clauses).
//
// The contracted forms are included because acceptanceTokens trims only
// LEADING and TRAILING punctuation, so "doesn't" survives tokenization intact
// as one token rather than splitting — a property the tokenizer test pins
// rather than this comment merely asserting.
var acceptanceNegators = []string{
	"no", "not", "never", "without", "absent", "none", "cannot",
	"lacks", "lacking", "isn't", "doesn't", "aren't", "won't",
}

// inRepoVerificationMarkers name an IN-REPOSITORY verification harness. Their
// presence in a criterion's stated verification method is conjunct S.
//
// WHY THIS IS NOT sandboxMarkers, despite three overlapping entries.
// sandboxMarkers names the acceptance PREVIEW's stand-ins (localhost, preview,
// sandbox) and is evidence about the acceptance executor's TARGET. This list is
// evidence about the criterion's stated VERIFICATION METHOD. Conflating them
// would let "validated against the localhost preview" count as in-repository
// verification, which it is not — that is a claim about the sandbox executor,
// the very thing the criterion is being excused from.
var inRepoVerificationMarkers = []string{
	"_test.go", "pgtest", "httptest", "testdata", "go test", "unit test",
	"integration test", "table test", "table-driven", "in-process", "in-repo",
	"fake", "stub", "mock", "golden file",
}

// liveTargetAnchors returns the token indices that carry the statement's
// liveness claim — the points conjunct P measures its negation window back
// from. The union of both matchers' anchors, ascending and de-duplicated.
//
// M1 anchors: the index of the FIRST token of every liveTarget corpus-phrase
// occurrence. The scan covers the WHOLE statement, NOT only the clauses that
// survived M1's own sandbox-marker filter: P is an EVERY-anchor conjunct, so an
// anchor M1 chose to ignore must still be counted, or a statement could be
// suppressed on the strength of one negated anchor while an un-negated one sat
// in a marker-bearing clause.
//
// M2 anchors: every liveness qualifier satisfying conjunct 1 — exactly the
// pairs qualifierActionIndices finds, shared with the matcher rather than
// re-derived.
//
// The M1 scan is TOKEN-SEQUENCE based while liveTargetCorpusMatch is a
// SUBSTRING test, so a corpus phrase matched only as a substring of a longer
// word yields no anchor here. That direction is safe ONLY when the anchor set
// comes out FULLY EMPTY: everyLiveTargetAnchorNegated returns false on an empty
// set, so the finding FIRES.
//
// THE MIXED CASE IS NOT SAFE, and is an accepted residual. A statement in which
// a corpus phrase matches only as a substring (contributing no anchor) AND a
// NEGATED M2 qualifier-action anchor is present has a non-empty anchor set
// whose every COUNTED anchor is negated, so conjunct P holds and an
// in-repository basis suppresses the finding — even though the live-target
// occurrence M1 fired on carries no negator of its own. The failure direction
// is an advisory MISS, consistent with this rule's other residuals, and closing
// it would mean either anchoring on substring fragments inside unrelated words
// or dropping the substring matcher M1 has used since #2845. So it is PINNED by
// TestMissingLiveValidationMarker_AcceptedSubstringAnchorMixedCase rather than
// papered over here.
func liveTargetAnchors(lowered string, tokens []string) []int {
	seen := make(map[int]struct{})
	var anchors []int
	add := func(i int) {
		if _, dup := seen[i]; dup {
			return
		}
		seen[i] = struct{}{}
		anchors = append(anchors, i)
	}

	for _, uc := range unevaluableCapabilities {
		if !uc.liveTarget {
			continue
		}
		for _, phrase := range uc.phrases {
			if !strings.Contains(lowered, phrase) {
				continue
			}
			for _, i := range phraseHeadIndices(tokens, acceptanceTokens(phrase)) {
				add(i)
			}
		}
	}
	for _, i := range qualifierActionIndices(tokens) {
		add(i)
	}

	slices.Sort(anchors)
	return anchors
}

// phraseHeadIndices returns the index of every position in tokens where the
// token sequence phrase occurs. Phrase tokenization goes through
// acceptanceTokens so a corpus phrase and a statement normalize identically
// (possessives trimmed, interior hyphens kept).
func phraseHeadIndices(tokens, phrase []string) []int {
	var idx []int
	if len(phrase) == 0 || len(phrase) > len(tokens) {
		return idx
	}
	for i := 0; i+len(phrase) <= len(tokens); i++ {
		match := true
		for j, w := range phrase {
			if tokens[i+j] != w {
				match = false
				break
			}
		}
		if match {
			idx = append(idx, i)
		}
	}
	return idx
}

// everyLiveTargetAnchorNegated is conjunct P: EVERY anchor carries a negation
// token within livenessProximityWindow tokens BEFORE it.
//
// An EMPTY anchor set returns FALSE. From the only caller that set is
// unreachable (P is evaluated only after a matcher fired), but the fail
// direction matters: returning false keeps an unanchored statement FIRING
// rather than silently suppressing it.
func everyLiveTargetAnchorNegated(anchors []int, tokens []string) bool {
	if len(anchors) == 0 {
		return false
	}
	for _, a := range anchors {
		negated := false
		for i := max(0, a-livenessProximityWindow); i < a && i < len(tokens); i++ {
			if tokenIn(tokens[i], acceptanceNegators) {
				negated = true
				break
			}
		}
		if !negated {
			return false
		}
	}
	return true
}

// hasInRepoVerification is conjunct S: the criterion's STATED VERIFICATION
// METHOD names an in-repository harness. It reads VerifyHint and
// ExpectationBasis joined — NEVER the statement, whose prose is what the
// matchers already judged and which an author could otherwise use to talk the
// rule out of firing.
//
// DELIBERATELY NOT SHARED with verifyHintDeclaresInRepo (#3163). The two answer
// different questions: this one asks whether a polarity-NEGATED live-target
// assertion is verified in-repository (a both-fields question — a negated live
// dependency is legitimately documented in either verify_hint or
// expectation_basis), while verifyHintDeclaresInRepo asks whether a
// sandbox-decidable external TRIGGER's harness is NAMED in the hint (a
// hint-only question — reading expectation_basis there would let an unmarked,
// genuinely-undecidable criterion escape the finding by citing a test). Merging
// them would import expectation_basis into the #3163 evidence and reopen that
// hole, so they are kept separate and independently listed.
func hasInRepoVerification(c AcceptanceCriterion) bool {
	stated := strings.ToLower(c.VerifyHint + " " + c.ExpectationBasis)
	return containsAnyPhrase(stated, inRepoVerificationMarkers)
}

// verifyHintHarnessMarkers name an in-repository / repository-local harness in
// HINT-SHAPED prose that inRepoVerificationMarkers (a list of concrete
// test-harness tokens) does not carry. They are a SEPARATE list, consulted only
// by verifyHintDeclaresInRepo (#3163), so the #3016 conjunct-S list
// inRepoVerificationMarkers does not silently grow to carry them and change that
// rule's suppression surface.
var verifyHintHarnessMarkers = []string{
	"scripts/test", "in-repository", "repository-local", "hermetic",
	"signs its own", "no live forge", "no live network", "same-process",
}

// verifyHintDeclaresInRepo is the #3163 evidence predicate: the criterion's
// verify_hint — and ONLY verify_hint — names an in-repository / repository-local
// verification harness. It is what UnevaluableCriteria consults to suppress a
// NON-liveTarget undecidable_criterion finding.
//
// WHY IT READS ONLY VerifyHint:
//   - expectation_basis is the field that ACCOMPANIES a skip DECLARATION, and
//     that shape is already exempt via criterionDeclaresUnevaluable. Reading it
//     here would let an unmarked, genuinely-undecidable criterion cite an
//     integration test and escape the very finding that exists to force its
//     marking.
//   - the STATEMENT is the prose the matcher already judged; admitting it as
//     evidence would let an author talk the rule out of firing in the same text
//     the rule keys on.
//
// It is DELIBERATELY NOT hasInRepoVerification and shares nothing with it beyond
// the inRepoVerificationMarkers list — see that predicate's comment for why the
// two answer different questions and must not be merged.
func verifyHintDeclaresInRepo(c AcceptanceCriterion) bool {
	hint := strings.ToLower(c.VerifyHint)
	return containsAnyPhrase(hint, inRepoVerificationMarkers) ||
		containsAnyPhrase(hint, verifyHintHarnessMarkers)
}

// verifyHintNamesSeedScenario is the E72.2 / #3326 evidence predicate, the
// sibling of verifyHintDeclaresInRepo under the same !liveTarget conjunct in
// UnevaluableCriteria: the criterion's verify_hint — and ONLY verify_hint, for
// the same two reasons that predicate states — carries a KNOWN seeded-scenario
// catalog name as a WHOLE token. The acceptance preview's dev-only fixture
// surface (POST /v0/dev/fixtures) materializes that scenario, so a hint that
// names one is positive evidence the sandbox can produce the state the
// criterion needs.
//
// The membership test IS seedScenarioNames membership over acceptanceTokens: there is
// deliberately no generic "seed" branch and no route-substring branch, so a
// hint that merely says "seed the run" or mentions POST /v0/dev/fixtures with
// an unknown name earns nothing. Tokenization is the shared acceptanceTokens
// (whitespace split, surrounding punctuation trimmed, interior hyphens kept),
// so "plan-gate-parked," matches while "plan-gate-parked-v2", "xgrooming-
// confirm-gate" and "seedling" stay single non-matching tokens. Lowercased
// first because every catalog name is lowercase and the lookup is case-sensitive.
func verifyHintNamesSeedScenario(c AcceptanceCriterion) bool {
	for _, tok := range acceptanceTokens(strings.ToLower(c.VerifyHint)) {
		if seedScenarioNames[tok] {
			return true
		}
	}
	return false
}

// seedScenarioNames is the closed set of seeded-scenario names the acceptance
// preview's dev-only POST /v0/dev/fixtures route can materialize (E72.2 /
// #3326). It is the catalog verifyHintNamesSeedScenario tests membership
// against; adding a scenario to the dev fixture surface means adding its name
// here, or a hint naming it earns nothing. Carried inline (not imported) so
// this package's production import graph stays free of repo packages;
// TestSeedScenarioNamesMatchCatalog binds it to devfixtures/catalog.Names()
// in both directions through a test-only import, so a name added on either
// side without the other fails in-loop.
var seedScenarioNames = map[string]bool{
	"grooming-confirm-gate": true,
	"plan-gate-parked":      true,
	"trace-upload-target":   true,
}

// liveTargetCorpusMatch is M1: the statement names a live TARGET via a phrase
// already in the shared unevaluableCapabilities corpus, on an entry flagged
// liveTarget. It REUSES that corpus rather than carrying a second phrase list,
// so the two rules cannot drift apart on what counts as a live forge or a
// deployed environment.
//
// M1 honours the sandbox-marker negation (#2845, operator condition C2): those
// phrases were written for a rule with the LOOSER either-marking exemption, so
// reusing them under the marker-only exemption makes previously-silent criteria
// fire. The negation narrows what M1 CONSIDERS without touching a single phrase
// string, keeping UnevaluableCriteria byte-identical.
//
// THE NEGATION IS SCOPED TO ONE CLAUSE, not to the whole statement. A
// whole-statement negation is itself a false-NEGATIVE hole: any stray "fake" /
// "mock" / "preview" anywhere in a sentence disabled M1 even when the sentence
// named a genuine live target, which recreates the defect this rule closes. So
// M1 asks the question per clause — a clause carrying a liveTarget phrase and
// NO stand-in of its own fires, however many stand-ins the neighbouring clauses
// mention. A marker only disarms the rule where it plausibly qualifies the
// target: in the same clause as the phrase.
//
// RESIDUAL 1 — FALSE POSITIVE, stated honestly: the negation rescues a
// statement whose clause names its own stand-in ("the github api client retries
// in the fake transport test") but NOT one that carries a live-target phrase in
// sandbox-validatable prose with no marker at all — "the deployed environment
// config template is rendered" still fires. Narrowing further (say, also
// demanding a live-action noun) would drop the genuine true positive "the
// deployed environment serves the new endpoint", so the residual is accepted
// and pinned by a test rather than papered over.
//
// RESIDUAL 2 — FALSE NEGATIVE, stated honestly: WITHIN one clause the negation
// is still an absolute kill switch, and containsSandboxMarker is a SUBSTRING
// test, so an inflected form ("sandboxed", "previews", "fixtures") counts as a
// marker too. A single-clause statement naming a genuine live target and a
// stand-in in the same breath therefore draws no M1 finding — "a live GitHub
// round-trip closes the issue from the sandboxed runner", "a real GitHub API
// call is made with the preview token" — and M2 does not rescue either: neither
// carries an "against …" phrase, so its conjunct 2 fails. Telling "in the fake
// transport test" (the marker qualifies the whole check) from "from the
// sandboxed runner" (it qualifies a bystander) needs parsing this deterministic
// word-list matcher deliberately does not do. So the residual is accepted and
// PINNED by TestMissingLiveValidationMarker_M1SameClauseMarkerResidual — a
// later narrowing or widening of sandboxMarkers flips that test visibly instead
// of moving this boundary in silence.
func liveTargetCorpusMatch(lowered string) bool {
	for _, clause := range acceptanceClauses(lowered) {
		if containsSandboxMarker(clause) {
			continue
		}
		for _, uc := range unevaluableCapabilities {
			if uc.liveTarget && containsAnyPhrase(clause, uc.phrases) {
				return true
			}
		}
	}
	return false
}

// MissingLiveValidationMarker flags every acceptance criterion whose STATEMENT
// names a LIVE forge/deploy/external target but which is not marked
// requires_live_validation (#2845, E54.31). It is the detective half of the
// live-validation classification rule; the preventive half is the planner
// prompt's Live-validation criteria guidance.
//
// A criterion is flagged when EITHER matcher fires:
//
//	M1 — the statement contains a phrase from a liveTarget entry of the shared
//	     unevaluableCapabilities corpus, in a CLAUSE that names no sandbox
//	     stand-in of its own.
//	M2 — the three-conjunct liveness-proximity matcher fires, catching the
//	     named-system prose ("a real backlog_grooming run against this
//	     repository") that no fixed phrase list anticipates.
//
// EXEMPTION — requires_live_validation ALONE. skip_expected with a basis
// deliberately does NOT exempt: it is the correct marking for an external
// trigger EVENT, but for a live TARGET it silently loses the auto-filed
// operator-validation walk. That gap is the defect this rule exists to close.
//
// ADVISORY ONLY, and at most ONE finding per criterion (the loop breaks on the
// first match) so a downstream count is a count of criteria. Returns a non-nil
// empty slice when nothing is flagged.
func MissingLiveValidationMarker(v Verification) []AcceptanceFinding {
	findings := []AcceptanceFinding{}
	for _, c := range v.AcceptanceCriteria {
		if c.RequiresLiveValidation {
			continue
		}
		lowered := strings.ToLower(c.Statement)
		tokens := acceptanceTokens(lowered)
		if !liveTargetCorpusMatch(lowered) && !livenessProximityMatch(tokens) {
			continue
		}
		// POLARITY POST-FILTER (#3016). Both matchers keep their own behaviour
		// untouched — the filter runs AFTER one of them fired, so M1's
		// clause-scoped negation and M2's three conjuncts are unchanged.
		if everyLiveTargetAnchorNegated(liveTargetAnchors(lowered, tokens), tokens) && hasInRepoVerification(c) {
			continue
		}
		findings = append(findings, AcceptanceFinding{
			Rule:        RuleMissingLiveValidationMarker,
			CriterionID: c.ID,
			Detail: "criterion statement names a LIVE forge/deploy/external target the sandboxed acceptance executor cannot stand up; " +
				"set requires_live_validation: true and pair it with skip_expected: true plus an expectation_basis — that pairing is what " +
				"auto-files the tracked operator-validation walk on plan approval. A skip_expected-only marking silently loses that walk. " +
				"If instead this criterion asserts the ABSENCE of a live dependency and its verification runs IN-REPOSITORY, do NOT add the " +
				"marker triple: marking it skips a criterion the sandbox can verify (#3016). Fix it by placing the negation directly before " +
				"the live-target phrase and naming the in-repository harness in expectation_basis.",
		})
	}
	return findings
}

// ---------------------------------------------------------------------------
// criterion_restates_test / no_observable_criterion (E72.1, #3325)
// ---------------------------------------------------------------------------

// testOnlyHintMarkers are the SUBSTRING markers that name in-repository Go
// test evidence in a verify_hint. Lowercase; matched against the lowered hint.
// A bare Go test function name is matched separately by goTestNamePattern,
// because `TestFoo` is case-bearing and a substring list cannot express it.
var testOnlyHintMarkers = []string{
	"_test.go", "go test", "scripts/test", "unit test", "table test",
	"table-driven", "golden file", "pgtest", "httptest",
}

// goTestNamePattern matches a bare Go test function name (`TestFoo`,
// `TestParse_Example`) in the ORIGINAL-cased hint. It is applied before
// lowering: `TestX` is the evidence, and "test" in ordinary prose is not.
var goTestNamePattern = regexp.MustCompile(`\bTest[A-Z][A-Za-z0-9_]*`)

// observableSurfaceTokenMarkers are the single-TOKEN markers naming one of the
// five operator-observable surfaces. They are matched via acceptanceTokens /
// tokenIn, NOT as substrings, so `cli` does not match `client` and `prompt`
// does not match `prompted` by accident — a token match is what keeps the rule
// from being cleared by an unrelated longer word.
var observableSurfaceTokenMarkers = []string{
	"http", "https", "endpoint", "response", "curl", "cli", "stderr", "stdout",
	"prompt", "localhost", "preview", "healthz", "next_actions", "gate-view",
}

// observableSurfaceSubstringMarkers are the multi-word / path-shaped markers
// naming an observable surface. Lowercase; matched as substrings of the
// lowered hint because they carry internal spaces, slashes or underscores that
// tokenization would split.
var observableSurfaceSubstringMarkers = []string{
	"get /", "post /", "put /", "patch /", "delete /", "/v0/",
	"status code", "response body",
	"mcp tool", "tool result", "tool call", "fishhawk_",
	"exit code", "exit status",
	"rendered prompt",
	"audit row", "audit entry", "audit log", "audit_",
	"issue comment", "pr comment", "status comment",
}

// verifyHintNamesTestOnly reports whether a verify_hint names in-repository
// Go test evidence — a test-only marker substring in the lowered hint, or a
// bare Go test function name in the original-cased hint.
func verifyHintNamesTestOnly(hint string) bool {
	return containsAnyPhrase(strings.ToLower(hint), testOnlyHintMarkers) ||
		goTestNamePattern.MatchString(hint)
}

// verifyHintNamesObservableSurface reports whether a verify_hint names one of
// the five operator-observable surfaces, via a token marker or a substring
// marker. A single named surface is enough: the acceptance agent then has
// something to observe, however many tests the hint also cites.
func verifyHintNamesObservableSurface(hint string) bool {
	lowered := strings.ToLower(hint)
	if containsAnyPhrase(lowered, observableSurfaceSubstringMarkers) {
		return true
	}
	for _, tok := range acceptanceTokens(lowered) {
		if tokenIn(tok, observableSurfaceTokenMarkers) {
			return true
		}
	}
	return false
}

// criterionRestatesTestDetail is the criterion_restates_test finding text. It
// names the five surfaces and BOTH remedies so the author is not sent on a
// two-step (fix the hint, then learn the declaration existed).
const criterionRestatesTestDetail = "criterion verify_hint names only in-repository Go tests and no operator-observable surface, so it RESTATES the " +
	"plan's test_strategy — the acceptance agent cannot observe a Go test passing on the localhost preview. Name the surface the " +
	"acceptance agent observes in verify_hint (an HTTP route/response/status code, an MCP tool result / fishhawk_* tool, a CLI exit " +
	"code/stderr/stdout, a rendered prompt, or a persisted audit row), or — if the change genuinely has no observable surface — " +
	"declare verification.acceptance_surface: none so the acceptance stage is omitted at plan approval. Advisory; never refuses the plan."

// TestOnlyCriteria flags every acceptance criterion whose verify_hint names
// ONLY in-repository Go test evidence and NO operator-observable surface
// (criterion_restates_test, E72.1 / #3325). It reads verify_hint ALONE — never
// the statement, never expectation_basis — for the same reason
// verifyHintDeclaresInRepo does: the hint is the field that states how the
// criterion is verified.
//
// A criterion already declared skip_expected-with-basis or
// requires_live_validation is exempt (criterionDeclaresUnevaluable, the
// exemption every advisory rule in this file shares), and an empty or
// whitespace-only verify_hint is never flagged (no marker matches it, so the
// rule fires on positive test-only evidence, never on absence). Returns a
// non-nil empty slice when nothing is flagged; findings come out in criteria
// order.
func TestOnlyCriteria(v Verification) []AcceptanceFinding {
	findings := []AcceptanceFinding{}
	for _, c := range v.AcceptanceCriteria {
		if criterionDeclaresUnevaluable(c) {
			continue
		}
		// An empty / whitespace-only hint is silent BY CONSTRUCTION — no
		// test-only marker matches it — so there is deliberately no explicit
		// emptiness guard here (it would be a dead branch no test could redden).
		if !verifyHintNamesTestOnly(c.VerifyHint) || verifyHintNamesObservableSurface(c.VerifyHint) {
			continue
		}
		findings = append(findings, AcceptanceFinding{
			Rule:        RuleCriterionRestatesTest,
			CriterionID: c.ID,
			Detail:      criterionRestatesTestDetail,
		})
	}
	return findings
}

// noObservableCriterion is the plan-level rule (no_observable_criterion). It
// takes the criterion_restates_test findings ALREADY computed by the caller so
// both rules read one evaluation. Fires exactly when:
//   - at least one criterion exists,
//   - the plan does NOT declare acceptance_surface: none,
//   - the plan is NOT the all-skip shape (that draws
//     all_criteria_skip_expected instead — no double advisory), and
//   - EVERY criterion is either a sanctioned declaration or drew
//     criterion_restates_test.
//
// Returns a non-nil empty slice when silent, one finding when it fires.
func noObservableCriterion(v Verification, restates []AcceptanceFinding) []AcceptanceFinding {
	findings := []AcceptanceFinding{}
	if len(v.AcceptanceCriteria) == 0 || DeclaresNoAcceptanceSurface(v) || AcceptanceSkippableAllSkipWithBasis(v) {
		return findings
	}
	restated := make(map[string]struct{}, len(restates))
	for _, f := range restates {
		restated[f.CriterionID] = struct{}{}
	}
	for _, c := range v.AcceptanceCriteria {
		if criterionDeclaresUnevaluable(c) {
			continue
		}
		if _, ok := restated[c.ID]; ok {
			continue
		}
		return findings
	}
	return append(findings, AcceptanceFinding{
		Rule: RuleNoObservableCriterion,
		Detail: "no acceptance criterion names an operator-observable surface: every criterion is either a declared skip or restates an " +
			"in-repository Go test, so the acceptance stage would dispatch a runner with nothing it can observe on the localhost preview. " +
			"Give at least one criterion a verify_hint naming an HTTP route/response/status code, an MCP tool result / fishhawk_* tool, " +
			"a CLI exit code/stderr/stdout, a rendered prompt, or a persisted audit row — or declare verification.acceptance_surface: none " +
			"so the acceptance stage is omitted at plan approval instead. Advisory; never refuses the plan.",
	})
}
