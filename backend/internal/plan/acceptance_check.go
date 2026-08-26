package plan

import "strings"

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
	// RuleUndecidableCriterion flags a criterion whose STATEMENT requires a
	// capability the sandboxed acceptance executor does not have (#2512,
	// layer 3). It is ADVISORY: it never refuses a plan, because a criterion
	// may legitimately name a capability in prose while still being drivable.
	// Its purpose is preventive — an author who sees it up front marks the
	// criterion skip_expected + expectation_basis (or
	// requires_live_validation) instead of shipping it to an executor that
	// can only report it undecidable.
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
//     with a basis or requires_live_validation. Advisory only.
//   - missing_live_validation_marker — a criterion whose statement names a LIVE
//     forge/deploy/external TARGET and which is NOT marked
//     requires_live_validation. Its exemption is that marker ALONE:
//     skip_expected-with-basis does not exempt, because only the marker files
//     the tracked operator-validation walk (#2845). Advisory only.
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
// Defining them HERE — the plan package imports no project packages and is
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
// plan package, which imports no project packages and is already imported by
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
// rule, which is how an advisory rule dies.
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
			findings = append(findings, AcceptanceFinding{
				Rule:        RuleUndecidableCriterion,
				CriterionID: c.ID,
				Detail: "criterion statement requires " + uc.capability +
					", which the sandboxed acceptance executor does not have; mark it skip_expected with an expectation_basis (or requires_live_validation) so it is declared up front rather than reported undecidable at acceptance",
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
// conjunct is one idea rather than an inline nested loop.
func qualifierNearAction(tokens []string) bool {
	for i, tok := range tokens {
		if !tokenIn(tok, livenessQualifiers) {
			continue
		}
		for j := i + 1; j <= i+livenessProximityWindow && j < len(tokens); j++ {
			if tokenIn(tokens[j], liveActionNouns) {
				return true
			}
		}
	}
	return false
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
		if !liveTargetCorpusMatch(lowered) && !livenessProximityMatch(acceptanceTokens(lowered)) {
			continue
		}
		findings = append(findings, AcceptanceFinding{
			Rule:        RuleMissingLiveValidationMarker,
			CriterionID: c.ID,
			Detail: "criterion statement names a LIVE forge/deploy/external target the sandboxed acceptance executor cannot stand up; " +
				"set requires_live_validation: true and pair it with skip_expected: true plus an expectation_basis — that pairing is what " +
				"auto-files the tracked operator-validation walk on plan approval. A skip_expected-only marking silently loses that walk.",
		})
	}
	return findings
}
