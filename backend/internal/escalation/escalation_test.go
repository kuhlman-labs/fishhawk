package escalation

import (
	"reflect"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

func ptr(i int) *int { return &i }

func esc(match spec.Predicate, req spec.EscalationRequirements) spec.Escalation {
	return spec.Escalation{Match: match, Require: req}
}

// TestEvaluate_FiresOnlyOnMatch pins the ordinary behaviour on both sides: a
// change the predicate accepts fires and raises; a change it does not accept
// leaves the zero requirement, so the gate stays at its declared baseline.
func TestEvaluate_FiresOnlyOnMatch(t *testing.T) {
	declared := []spec.Escalation{
		esc(spec.Predicate{Paths: []string{"backend/internal/server/**"}},
			spec.EscalationRequirements{Approvals: &spec.EscalatedApprovals{Count: ptr(2)}}),
	}

	t.Run("matching change fires", func(t *testing.T) {
		res, err := Evaluate(declared, spec.Change{Paths: []string{"backend/internal/server/runs.go"}})
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if !res.Any() || len(res.Fired) != 1 || res.Fired[0].Index != 0 {
			t.Fatalf("Fired = %+v, want exactly escalation 0", res.Fired)
		}
		if res.Requirements.Count == nil || *res.Requirements.Count != 2 {
			t.Fatalf("Count = %v, want 2", res.Requirements.Count)
		}
	})

	t.Run("non-matching change raises nothing", func(t *testing.T) {
		res, err := Evaluate(declared, spec.Change{Paths: []string{"docs/README.md"}})
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if res.Any() {
			t.Fatalf("Fired = %+v, want none", res.Fired)
		}
		if !res.Requirements.IsZero() {
			t.Fatalf("Requirements = %+v, want zero", res.Requirements)
		}
	})

	t.Run("no declarations is the zero result", func(t *testing.T) {
		res, err := Evaluate(nil, spec.Change{Paths: []string{"anything"}})
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if res.Any() || !res.Requirements.IsZero() {
			t.Fatalf("Result = %+v, want zero", res)
		}
	})
}

// TestEvaluate_MatchError_FailsClosed is the FAIL-CLOSED branch: a malformed
// glob that reached the gate is RETURNED as an error with the ZERO result,
// never degraded into "nothing fired". A caller that saw (Result{}, nil) here
// would proceed unescalated, which is indistinguishable from the control being
// absent.
func TestEvaluate_MatchError_FailsClosed(t *testing.T) {
	res, err := Evaluate([]spec.Escalation{
		esc(spec.Predicate{Paths: []string{"backend/[unclosed"}},
			spec.EscalationRequirements{Approvals: &spec.EscalatedApprovals{Count: ptr(2)}}),
	}, spec.Change{Paths: []string{"backend/x.go"}})
	if err == nil {
		t.Fatal("Evaluate returned nil error on a malformed glob; the gate would proceed unescalated")
	}
	if !strings.Contains(err.Error(), "escalation 0") {
		t.Errorf("error %q does not name the offending declaration index", err)
	}
	if res.Any() || !res.Requirements.IsZero() {
		t.Errorf("Result = %+v, want the zero result alongside the error", res)
	}
}

// multiMatchFixture is the criterion-3 fixture: three escalations that all
// match one change, differing on every dimension.
func multiMatchFixture() []spec.Escalation {
	return []spec.Escalation{
		esc(spec.Predicate{Paths: []string{"backend/**"}}, spec.EscalationRequirements{
			Approvals:   &spec.EscalatedApprovals{Count: ptr(2), MemberOf: "acme/security", MinPermission: "write"},
			MaxAutonomy: spec.TierMedium,
		}),
		esc(spec.Predicate{Paths: []string{"**/*.go"}}, spec.EscalationRequirements{
			Approvals: &spec.EscalatedApprovals{Count: ptr(4), MemberOf: "acme/leads", MinPermission: "maintain"},
		}),
		esc(spec.Predicate{Paths: []string{"backend/internal/**"}}, spec.EscalationRequirements{
			Approvals:   &spec.EscalatedApprovals{Count: ptr(3), MemberOf: "acme/security"},
			MaxAutonomy: spec.TierLow,
		}),
	}
}

// TestEvaluate_ComposesStrictest_OrderIndependent proves criterion 3: the
// composed result is max(count) / union(member_of) / strictest(min_permission)
// / lowest(max_autonomy), AND that shuffling the declaration order cannot
// change it — the structural refutation of last-match-wins.
func TestEvaluate_ComposesStrictest_OrderIndependent(t *testing.T) {
	change := spec.Change{Paths: []string{"backend/internal/server/runs.go"}}

	base, err := Evaluate(multiMatchFixture(), change)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(base.Fired) != 3 {
		t.Fatalf("Fired = %d, want all 3", len(base.Fired))
	}
	if base.Requirements.Count == nil || *base.Requirements.Count != 4 {
		t.Errorf("Count = %v, want max 4", base.Requirements.Count)
	}
	if want := []string{"acme/leads", "acme/security"}; !reflect.DeepEqual(base.Requirements.MemberOf, want) {
		t.Errorf("MemberOf = %v, want the sorted de-duplicated union %v", base.Requirements.MemberOf, want)
	}
	if base.Requirements.MinPermission != "maintain" {
		t.Errorf("MinPermission = %q, want the strictest tier maintain", base.Requirements.MinPermission)
	}
	if base.Requirements.MaxAutonomy != spec.TierLow {
		t.Errorf("MaxAutonomy = %q, want the lowest ceiling low", base.Requirements.MaxAutonomy)
	}

	// Reversed and shuffled orders must produce an IDENTICAL requirement.
	fx := multiMatchFixture()
	orders := map[string][]spec.Escalation{
		"reversed": {fx[2], fx[1], fx[0]},
		"shuffled": {fx[1], fx[2], fx[0]},
	}
	for name, order := range orders {
		got, gerr := Evaluate(order, change)
		if gerr != nil {
			t.Fatalf("%s: Evaluate: %v", name, gerr)
		}
		if !reflect.DeepEqual(got.Requirements, base.Requirements) {
			t.Errorf("%s: Requirements = %+v, want %+v (composition must be order-independent)",
				name, got.Requirements, base.Requirements)
		}
	}
}

// TestRenderFired_And_Fingerprint pins the ONE renderer both the audit payload
// and the run-status block use, and the stability the de-duplication rests on.
func TestRenderFired_And_Fingerprint(t *testing.T) {
	change := spec.Change{Paths: []string{"backend/internal/server/runs.go"}}
	res, err := Evaluate(multiMatchFixture(), change)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	summary := RenderFired(res)
	for _, want := range []string{
		"3 escalations fired",
		"escalation 0", "escalation 1", "escalation 2",
		"backend/**", "**/*.go", "backend/internal/**",
		"approvals.count=4",
		"approvals.member_of=acme/leads+acme/security",
		"approvals.min_permission=maintain",
		"max_autonomy=low",
	} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary %q does not name %q", summary, want)
		}
	}

	t.Run("empty result renders explicitly, not as the empty string", func(t *testing.T) {
		if got := RenderFired(Result{}); got != "no escalation fired" {
			t.Errorf("RenderFired(zero) = %q, want the explicit sentence", got)
		}
	})

	t.Run("fingerprint is stable and order-independent", func(t *testing.T) {
		fx := multiMatchFixture()
		shuffled, serr := Evaluate([]spec.Escalation{fx[2], fx[0], fx[1]}, change)
		if serr != nil {
			t.Fatalf("Evaluate: %v", serr)
		}
		// The FIRED list keeps declaration order, so a reordered document is a
		// different rendering and therefore a different fingerprint — which is
		// correct: the operator IS looking at a different declaration. What
		// must not change is the fingerprint of the SAME evaluation.
		again, aerr := Evaluate(multiMatchFixture(), change)
		if aerr != nil {
			t.Fatalf("Evaluate: %v", aerr)
		}
		if Fingerprint(res) != Fingerprint(again) {
			t.Error("the same evaluation fingerprinted differently; de-duplication would emit a duplicate every pass")
		}
		if Fingerprint(shuffled) == "" {
			t.Error("Fingerprint returned the empty string")
		}
	})

	t.Run("a changed requirement changes the fingerprint", func(t *testing.T) {
		stricter := multiMatchFixture()
		stricter[1].Require.Approvals.Count = ptr(9)
		changed, cerr := Evaluate(stricter, change)
		if cerr != nil {
			t.Fatalf("Evaluate: %v", cerr)
		}
		if Fingerprint(changed) == Fingerprint(res) {
			t.Error("a raised count did not change the fingerprint; the second entry an operator must see would be suppressed")
		}
	})
}

// TestRenderFired_NonPathCriteria covers the label / trigger / change_kind
// rendering arms so a predicate matched on something other than paths still
// names what fired.
func TestRenderFired_NonPathCriteria(t *testing.T) {
	res, err := Evaluate([]spec.Escalation{
		esc(spec.Predicate{Labels: []string{"security"}, Triggers: []spec.TriggerForm{spec.TriggerDiff}},
			spec.EscalationRequirements{MaxAutonomy: spec.TierLow}),
	}, spec.Change{Labels: []string{"security"}, Trigger: spec.TriggerDiff})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	summary := RenderFired(res)
	for _, want := range []string{"labels=security", "trigger=diff", "max_autonomy=low"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary %q does not name %q", summary, want)
		}
	}
}

// --- RuleKey: the stable, content-derived per-rule join key (E75.1 / #3729) --

// ruleKeyRuleA is the rule under test across the RuleKey cases: one paths
// criterion and one approvals clamp.
func ruleKeyRuleA() spec.Escalation {
	return esc(
		spec.Predicate{Paths: []string{"backend/internal/server/**", "backend/internal/audit/**"}},
		spec.EscalationRequirements{Approvals: &spec.EscalatedApprovals{Count: ptr(2)}},
	)
}

// ruleKeyRuleB is an UNRELATED sibling declaration: it exists only to be
// reordered around rule A.
func ruleKeyRuleB() spec.Escalation {
	return esc(
		spec.Predicate{Labels: []string{"area:runner"}},
		spec.EscalationRequirements{MaxAutonomy: spec.AutonomyTier("low")},
	)
}

// TestRuleKey_StableAcrossReordering is the defect the issue names: the
// positional Index moves when an unrelated declaration is inserted ahead of a
// rule, so an index-keyed decision index loses the rule. RuleKey must not
// move. The keys are read off the EVALUATION (matched declarations), not off
// the raw slice, so this asserts the coordinate a consumer actually records.
func TestRuleKey_StableAcrossReordering(t *testing.T) {
	change := spec.Change{Paths: []string{"backend/internal/server/runs.go"}}

	before := []spec.Escalation{ruleKeyRuleA(), ruleKeyRuleB()}
	after := []spec.Escalation{ruleKeyRuleB(), ruleKeyRuleA()}

	resBefore, err := Evaluate(before, change)
	if err != nil {
		t.Fatalf("Evaluate(before): %v", err)
	}
	resAfter, err := Evaluate(after, change)
	if err != nil {
		t.Fatalf("Evaluate(after): %v", err)
	}
	if len(resBefore.Fired) != 1 || len(resAfter.Fired) != 1 {
		t.Fatalf("fired counts = %d/%d, want 1/1", len(resBefore.Fired), len(resAfter.Fired))
	}
	// The INDEX moves — the coordinate the issue says is unstable.
	if resBefore.Fired[0].Index == resAfter.Fired[0].Index {
		t.Fatalf("fixture is not discriminating: the declaration index did not move (%d both times)",
			resBefore.Fired[0].Index)
	}
	keyBefore := RuleKey(resBefore.Fired[0].Escalation)
	keyAfter := RuleKey(resAfter.Fired[0].Escalation)
	if keyBefore != keyAfter {
		t.Errorf("RuleKey moved under an unrelated reordering: %q → %q; the key must carry no positional index",
			keyBefore, keyAfter)
	}
}

// TestRuleKey_ChangesWithRuleContent pins the other half of the contract: a
// key that never moves would be useless. Editing THIS rule's globs, or only
// its require clamp, must change the key.
func TestRuleKey_ChangesWithRuleContent(t *testing.T) {
	base := RuleKey(ruleKeyRuleA())

	editedGlobs := ruleKeyRuleA()
	editedGlobs.Match.Paths = []string{"backend/internal/server/**", "backend/internal/concern/**"}
	if got := RuleKey(editedGlobs); got == base {
		t.Errorf("RuleKey unchanged after editing the rule's path globs (%q); a rule whose meaning moved is a different rule", got)
	}

	editedClamp := ruleKeyRuleA()
	editedClamp.Require = spec.EscalationRequirements{Approvals: &spec.EscalatedApprovals{Count: ptr(3)}}
	if got := RuleKey(editedClamp); got == base {
		t.Errorf("RuleKey unchanged after editing only the require clamp (%q); two rules with identical predicates but different clamps must key differently", got)
	}

	editedMinPerm := ruleKeyRuleA()
	editedMinPerm.Require = spec.EscalationRequirements{Approvals: &spec.EscalatedApprovals{Count: ptr(2), MinPermission: "admin"}}
	if got := RuleKey(editedMinPerm); got == base {
		t.Errorf("RuleKey unchanged after adding a min_permission clamp (%q)", got)
	}

	editedAutonomy := ruleKeyRuleA()
	editedAutonomy.Require.MaxAutonomy = spec.AutonomyTier("low")
	if got := RuleKey(editedAutonomy); got == base {
		t.Errorf("RuleKey unchanged after adding a max_autonomy ceiling (%q)", got)
	}

	// require.reviewers (E55.9 / #3754) is part of the clamp: adding it, and
	// changing which persona it names, must each move the key.
	editedReviewers := ruleKeyRuleA()
	editedReviewers.Require.Reviewers = []string{"security"}
	withSecurity := RuleKey(editedReviewers)
	if withSecurity == base {
		t.Errorf("RuleKey unchanged after adding require.reviewers (%q); a rule that now attaches a persona is a different rule", withSecurity)
	}
	otherPersona := ruleKeyRuleA()
	otherPersona.Require.Reviewers = []string{"privacy"}
	if got := RuleKey(otherPersona); got == withSecurity {
		t.Errorf("RuleKey identical for require.reviewers [security] and [privacy] (%q)", got)
	}
}

// TestRuleKey_ReviewersLessRuleKeyIsStable pins the compatibility half of the
// require.reviewers fold-in (E55.9 / #3754): a rule that declares NO reviewers
// must key exactly as it did before the dimension existed, so every decision
// index joined on an existing rule's key keeps following it. The two literals
// were computed by running RuleKey from the PRE-#3754 escalation.go (main at
// d9d175bb) over these fixtures; a rendering change that moves them is a
// silent re-keying of every existing rule.
func TestRuleKey_ReviewersLessRuleKeyIsStable(t *testing.T) {
	for _, tc := range []struct {
		name string
		rule spec.Escalation
		want string
	}{
		{"approvals rule (ruleKeyRuleA)", ruleKeyRuleA(), "fef33f4dfb7f5b65"},
		{"max_autonomy rule (ruleKeyRuleB)", ruleKeyRuleB(), "fd35639f5946e999"},
	} {
		if got := RuleKey(tc.rule); got != tc.want {
			t.Errorf("%s: RuleKey = %q, want the pre-#3754 key %q — a reviewers-less rule must not be re-keyed", tc.name, got, tc.want)
		}
		empty := tc.rule
		empty.Require.Reviewers = []string{}
		if got := RuleKey(empty); got != tc.want {
			t.Errorf("%s: an EMPTY (non-nil) require.reviewers re-keyed the rule to %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestRuleKey_ReviewersPermutationInvariant pins that require.reviewers is
// canonicalized like a criterion list: the attachment is a SET, so permuting
// it is not a change to the rule. It also pins the sorted COPY — the caller's
// slice keeps its declared order.
func TestRuleKey_ReviewersPermutationInvariant(t *testing.T) {
	a := ruleKeyRuleA()
	a.Require.Reviewers = []string{"security", "privacy", "data"}
	b := ruleKeyRuleA()
	b.Require.Reviewers = []string{"data", "security", "privacy"}
	if RuleKey(a) != RuleKey(b) {
		t.Errorf("RuleKey moved under a require.reviewers permutation: %q vs %q", RuleKey(a), RuleKey(b))
	}
	if !reflect.DeepEqual(a.Require.Reviewers, []string{"security", "privacy", "data"}) {
		t.Errorf("RuleKey mutated require.reviewers to %v", a.Require.Reviewers)
	}
}

// personaFixture is three escalations: 0 fires on backend/** and attaches
// security; 1 fires on **/*.go and attaches privacy AND security (a repeat
// across escalations); 2 never fires for a backend change and attaches data.
func personaFixture() []spec.Escalation {
	return []spec.Escalation{
		esc(spec.Predicate{Paths: []string{"backend/**"}}, spec.EscalationRequirements{Reviewers: []string{"security"}}),
		esc(spec.Predicate{Paths: []string{"**/*.go"}}, spec.EscalationRequirements{Reviewers: []string{"security", "privacy", "privacy"}}),
		esc(spec.Predicate{Paths: []string{"frontend/**"}}, spec.EscalationRequirements{Reviewers: []string{"data"}}),
	}
}

// TestPersonaAttachments_UnionSortedAttributed pins the derivation: the union
// of every FIRED escalation's require.reviewers, de-duplicated (a persona two
// escalations name appears once, carrying both; a persona one escalation
// repeats is attributed to it once), sorted by persona name, each carrying its
// fired escalations in declaration order.
func TestPersonaAttachments_UnionSortedAttributed(t *testing.T) {
	fx := personaFixture()
	res, err := Evaluate(fx, spec.Change{Paths: []string{"backend/internal/spec/x.go"}})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	got := PersonaAttachments(res)
	want := []PersonaAttachment{
		{Persona: "privacy", Fired: []Fired{{Index: 1, Escalation: fx[1]}}},
		{Persona: "security", Fired: []Fired{{Index: 0, Escalation: fx[0]}, {Index: 1, Escalation: fx[1]}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("PersonaAttachments =\n%+v\nwant\n%+v", got, want)
	}

	// Reordering the declarations moves the fired indices but not the
	// attachment ORDER, which is a function of the fired set.
	shuffled, err := Evaluate([]spec.Escalation{fx[1], fx[2], fx[0]}, spec.Change{Paths: []string{"backend/internal/spec/x.go"}})
	if err != nil {
		t.Fatalf("Evaluate(shuffled): %v", err)
	}
	sg := PersonaAttachments(shuffled)
	if len(sg) != 2 || sg[0].Persona != "privacy" || sg[1].Persona != "security" {
		t.Fatalf("shuffled declarations reordered the attachments: %+v", sg)
	}
}

// TestPersonaAttachments_NonFiringContributesNothing pins "cost only where
// attached": an escalation that did not fire contributes no persona whatever
// it declares, a result with nothing fired attaches nothing, and a fired
// escalation declaring no reviewers attaches nothing.
func TestPersonaAttachments_NonFiringContributesNothing(t *testing.T) {
	fx := personaFixture()
	res, err := Evaluate(fx, spec.Change{Paths: []string{"docs/readme.md"}})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if res.Any() {
		t.Fatalf("fixture is not discriminating: %d escalations fired for a docs-only change", len(res.Fired))
	}
	if got := PersonaAttachments(res); got != nil {
		t.Errorf("nothing fired, yet PersonaAttachments = %+v", got)
	}

	// Only escalation 2 (frontend/**) fires: data attaches, and nothing from
	// the non-firing 0 and 1 leaks in.
	res, err = Evaluate(fx, spec.Change{Paths: []string{"frontend/app.ts"}})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	got := PersonaAttachments(res)
	if len(got) != 1 || got[0].Persona != "data" {
		t.Errorf("PersonaAttachments = %+v, want only data (from the one fired escalation)", got)
	}

	approvalsOnly, err := Evaluate([]spec.Escalation{ruleKeyRuleA()}, spec.Change{Paths: []string{"backend/internal/server/runs.go"}})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !approvalsOnly.Any() {
		t.Fatal("fixture is not discriminating: ruleKeyRuleA did not fire")
	}
	if got := PersonaAttachments(approvalsOnly); got != nil {
		t.Errorf("a fired escalation declaring no reviewers attached %+v", got)
	}
}

// TestRenderFired_ReviewersOnlyFiring pins what the escalation_fired audit
// entry carries for a reviewers-only firing (approval condition 2, E55.9 /
// #3754). The server's resolveEscalations writes that entry whenever
// res.Any() — NOT on Requirements.IsZero — so a reviewers-only escalation that
// fires at the approval / delegation seams DOES produce an entry; this pins
// both halves of what it renders: Any() is true (so the entry is written), the
// composed requirements are the zero value (so the approval gate and the
// delegation clamp raise nothing), and the rendering names the fired rule with
// NO "Raised:" clause — accurate, since nothing is raised at those seams.
func TestRenderFired_ReviewersOnlyFiring(t *testing.T) {
	rule := esc(spec.Predicate{Paths: []string{"backend/internal/spec/**"}}, spec.EscalationRequirements{Reviewers: []string{"security"}})
	res, err := Evaluate([]spec.Escalation{rule}, spec.Change{Paths: []string{"backend/internal/spec/x.go"}})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !res.Any() {
		t.Fatal("a reviewers-only escalation matching the change did not fire")
	}
	if !res.Requirements.IsZero() {
		t.Errorf("a reviewers-only firing composed to %+v, want the zero value (nothing raised at the approval / delegation seams)", res.Requirements)
	}
	got := RenderFired(res)
	if want := "1 escalation fired: escalation 0 (paths=backend/internal/spec/**)"; got != want {
		t.Errorf("RenderFired = %q, want %q (no Raised: clause)", got, want)
	}
}

// TestRuleKey_InvariantToWithinRulePermutation pins the canonicalization: each
// match criterion is an unordered OR, so permuting a rule's own globs is not a
// semantic change and must not move the key. This is one of the two cases that
// go RED if the sorted-copy canonicalization is removed.
func TestRuleKey_InvariantToWithinRulePermutation(t *testing.T) {
	a := esc(
		spec.Predicate{
			Paths:       []string{"a/**", "b/**", "c/**"},
			Labels:      []string{"area:api", "area:runner"},
			ChangeKinds: []string{"feature", "bugfix"},
			Triggers:    []spec.TriggerForm{spec.TriggerForm("issue"), spec.TriggerForm("cli")},
		},
		spec.EscalationRequirements{Approvals: &spec.EscalatedApprovals{Count: ptr(2)}},
	)
	permuted := esc(
		spec.Predicate{
			Paths:       []string{"c/**", "a/**", "b/**"},
			Labels:      []string{"area:runner", "area:api"},
			ChangeKinds: []string{"bugfix", "feature"},
			Triggers:    []spec.TriggerForm{spec.TriggerForm("cli"), spec.TriggerForm("issue")},
		},
		spec.EscalationRequirements{Approvals: &spec.EscalatedApprovals{Count: ptr(2)}},
	)
	if RuleKey(a) != RuleKey(permuted) {
		t.Errorf("RuleKey moved under a within-rule criterion permutation: %q vs %q; each criterion list is an unordered OR",
			RuleKey(a), RuleKey(permuted))
	}
}

// TestRuleKey_IdenticalDeclarationsKeyIdentically pins the equivalence two
// separately-constructed but structurally identical declarations must have.
func TestRuleKey_IdenticalDeclarationsKeyIdentically(t *testing.T) {
	// Built through two SEPARATE constructions so the comparison is between
	// two independently-built values, not one expression against itself.
	a, b := ruleKeyRuleA(), ruleKeyRuleA()
	if RuleKey(a) != RuleKey(b) {
		t.Error("two structurally identical declarations produced different RuleKeys")
	}
	if got := len(RuleKey(ruleKeyRuleA())); got != RuleKeyBytes {
		t.Errorf("RuleKey length = %d, want %d hex chars", got, RuleKeyBytes)
	}
}

// TestRuleKey_DoesNotMutateArgument is the second case pinning the sorted
// COPY: the parsed spec.Workflow is shared across gate evaluations, so an
// in-place sort inside RuleKey would silently reorder an operator's declared
// globs everywhere else they are read (including RenderFired's operator-facing
// output). The fixture's lists are deliberately UNSORTED.
func TestRuleKey_DoesNotMutateArgument(t *testing.T) {
	e := esc(
		spec.Predicate{
			Paths:       []string{"z/**", "a/**"},
			Labels:      []string{"zeta", "alpha"},
			ChangeKinds: []string{"z-kind", "a-kind"},
			Triggers:    []spec.TriggerForm{spec.TriggerForm("z"), spec.TriggerForm("a")},
		},
		spec.EscalationRequirements{Approvals: &spec.EscalatedApprovals{Count: ptr(2)}},
	)
	wantPaths := append([]string(nil), e.Match.Paths...)
	wantLabels := append([]string(nil), e.Match.Labels...)
	wantKinds := append([]string(nil), e.Match.ChangeKinds...)
	wantTriggers := append([]spec.TriggerForm(nil), e.Match.Triggers...)

	_ = RuleKey(e)

	if !reflect.DeepEqual(e.Match.Paths, wantPaths) {
		t.Errorf("RuleKey mutated Match.Paths: %v, want %v", e.Match.Paths, wantPaths)
	}
	if !reflect.DeepEqual(e.Match.Labels, wantLabels) {
		t.Errorf("RuleKey mutated Match.Labels: %v, want %v", e.Match.Labels, wantLabels)
	}
	if !reflect.DeepEqual(e.Match.ChangeKinds, wantKinds) {
		t.Errorf("RuleKey mutated Match.ChangeKinds: %v, want %v", e.Match.ChangeKinds, wantKinds)
	}
	if !reflect.DeepEqual(e.Match.Triggers, wantTriggers) {
		t.Errorf("RuleKey mutated Match.Triggers: %v, want %v", e.Match.Triggers, wantTriggers)
	}
}

// TestRuleKey_CriterionlessRuleKeysStably pins the degenerate declaration (no
// match criterion) so the renderer's "no criterion" branch is not vacuous: it
// must still key, and still key differently from a criterion-bearing rule with
// the same clamp.
func TestRuleKey_CriterionlessRuleKeysStably(t *testing.T) {
	bare := esc(spec.Predicate{}, spec.EscalationRequirements{Approvals: &spec.EscalatedApprovals{Count: ptr(2)}})
	if RuleKey(bare) == "" {
		t.Fatal("RuleKey of a criterionless declaration is empty")
	}
	if RuleKey(bare) == RuleKey(ruleKeyRuleA()) {
		t.Error("a criterionless declaration keys identically to a paths-bearing one with the same clamp")
	}
}

// TestRuleKey_DistinguishesDelimiterBearingDeclarations pins the INJECTIVITY
// of the canonical rendering against the delimiters the rendering itself uses:
// the "," that joins one criterion list's elements, the " " that joins the
// fields, and the "=" that separates a field's name from its value.
//
// Each pair below is TWO GENUINELY DIFFERENT declarations — one glob
// containing a comma matches a path literally containing that comma, two globs
// match either path — that a bare delimiter join renders identically
// (`paths=a,b` for both). Under such a rendering a decision index keyed on
// RuleKey would follow one rule and silently report the other's history.
//
// The `field=` cases are the level-up form of the same hazard: a value that
// carries a space plus a plausible field prefix could forge a second field in
// the space-joined part list.
func TestRuleKey_DistinguishesDelimiterBearingDeclarations(t *testing.T) {
	clamp := func() spec.EscalationRequirements {
		return spec.EscalationRequirements{Approvals: &spec.EscalatedApprovals{Count: ptr(2)}}
	}
	cases := []struct {
		name string
		a, b spec.Escalation
	}{
		{
			name: "paths: one comma-bearing glob vs two globs",
			a:    esc(spec.Predicate{Paths: []string{"a,b"}}, clamp()),
			b:    esc(spec.Predicate{Paths: []string{"a", "b"}}, clamp()),
		},
		{
			// Both sides are written in the SORTED order the canonical
			// rendering imposes, so a bare join renders them byte-identically
			// — without that the case would pass on the sort alone and prove
			// nothing about the encoding.
			name: "labels: one comma-bearing label vs two labels",
			a:    esc(spec.Predicate{Labels: []string{"area:audit,area:server"}}, clamp()),
			b:    esc(spec.Predicate{Labels: []string{"area:audit", "area:server"}}, clamp()),
		},
		{
			name: "paths: an empty element vs a leading-comma glob",
			a:    esc(spec.Predicate{Paths: []string{"a", ""}}, clamp()),
			b:    esc(spec.Predicate{Paths: []string{",a"}}, clamp()),
		},
		{
			name: "paths: a glob forging a second field vs the two real fields",
			a:    esc(spec.Predicate{Paths: []string{"a labels=x"}}, clamp()),
			b:    esc(spec.Predicate{Paths: []string{"a"}, Labels: []string{"x"}}, clamp()),
		},
		{
			name: "member_of: a group name forging a max_autonomy clamp",
			a: esc(spec.Predicate{Paths: []string{"a"}}, spec.EscalationRequirements{
				Approvals: &spec.EscalatedApprovals{MemberOf: "sec require.max_autonomy=low"},
			}),
			b: esc(spec.Predicate{Paths: []string{"a"}}, spec.EscalationRequirements{
				Approvals:   &spec.EscalatedApprovals{MemberOf: "sec"},
				MaxAutonomy: spec.AutonomyTier("low"),
			}),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Self-pairing guard: each declaration must key stably against
			// ITSELF, so a RED below is the two declarations colliding and
			// never an unstable renderer.
			keyA := stableRuleKey(t, "a", tc.a)
			keyB := stableRuleKey(t, "b", tc.b)
			if keyA == keyB {
				t.Errorf("two distinct declarations share a RuleKey (%q); the canonical rendering must be injective over its own delimiters", keyA)
			}
		})
	}
}

// TestRuleKey_DelimiterBearingValuesStayPermutationInvariant pins that the
// injectivity fix did not cost the canonicalization contract: a within-rule
// permutation of a list whose elements CONTAIN the join delimiter must still
// leave the key unchanged, because each list is an unordered OR whatever its
// elements hold.
func TestRuleKey_DelimiterBearingValuesStayPermutationInvariant(t *testing.T) {
	clamp := spec.EscalationRequirements{Approvals: &spec.EscalatedApprovals{Count: ptr(2)}}
	a := esc(spec.Predicate{Paths: []string{"a,b", "c d", "e=f"}}, clamp)
	permuted := esc(spec.Predicate{Paths: []string{"e=f", "a,b", "c d"}}, clamp)
	if RuleKey(a) != RuleKey(permuted) {
		t.Errorf("RuleKey moved under a permutation of delimiter-bearing globs: %q vs %q", RuleKey(a), RuleKey(permuted))
	}
}

// stableRuleKey keys one declaration TWICE and fails if the two disagree. It
// is the self-pairing half of the injectivity cases: a collision assertion is
// only meaningful once the renderer is known to be deterministic on each side
// in isolation.
func stableRuleKey(t *testing.T, side string, e spec.Escalation) string {
	t.Helper()
	keys := make([]string, 2)
	for i := range keys {
		keys[i] = RuleKey(e)
	}
	if keys[0] != keys[1] {
		t.Fatalf("RuleKey of declaration %s is not stable across calls: %q vs %q", side, keys[0], keys[1])
	}
	return keys[0]
}
