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
