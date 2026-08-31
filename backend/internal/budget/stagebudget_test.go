package budget

import "testing"

// TestEvaluateStage tables EvaluateStage's boundary. The key case is the
// documented no-guard shape (limit 0 -> Over=true): EvaluateStage owns NO
// non-positive-limit guard, so a zero limit reports Over. StageEnforcementApplies
// is the sole owner of the unset check (see TestStageEnforcementApplies).
func TestEvaluateStage(t *testing.T) {
	cases := []struct {
		name     string
		cost     float64
		limit    float64
		wantOver bool
	}{
		{"under", 1.00, 2.00, false},
		{"exactly_at_is_over", 2.00, 2.00, true},
		{"over", 3.00, 2.00, true},
		// The documented no-guard shape: EvaluateStage does NOT guard a
		// non-positive limit — StageEnforcementApplies owns that check, which
		// is what makes the limit clause's counterfactual (C2) observable. A
		// caller reaching EvaluateStage with limit 0 gets Over=true.
		{"zero_limit_is_over_no_guard", 1.00, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := EvaluateStage(tc.cost, tc.limit)
			if d.Over != tc.wantOver {
				t.Errorf("EvaluateStage(%v,%v).Over = %v, want %v", tc.cost, tc.limit, d.Over, tc.wantOver)
			}
			if d.CostUSD != tc.cost || d.LimitUSD != tc.limit {
				t.Errorf("EvaluateStage echoed %v/%v, want %v/%v", d.CostUSD, d.LimitUSD, tc.cost, tc.limit)
			}
		})
	}
}

// TestStageEnforcementApplies pins both clauses of the single-owner predicate,
// with two ISOLATING fixtures so each clause's counterfactual is observable:
//
//   - major 0 with a POSITIVE limit (5.00): the limit clause is satisfied, so
//     a false can only come from the version clause. Deleting `specVersionMajor
//     >= 2 &&` flips this to true (C1 at the unit level).
//   - major 2 with a ZERO limit: the version clause is satisfied, so a false
//     can only come from the limit clause. Deleting `&& limitUSD > 0` flips
//     this to true (C2).
func TestStageEnforcementApplies(t *testing.T) {
	cases := []struct {
		name  string
		major int
		limit float64
		want  bool
	}{
		// Isolates the version clause: limit clause satisfied.
		{"major0_positive_limit_isolates_version", 0, 5.00, false},
		// Isolates the limit clause: version clause satisfied.
		{"major2_zero_limit_isolates_limit", 2, 0, false},
		{"major2_positive_limit_arms", 2, 5.00, true},
		{"major1_positive_limit_off", 1, 5.00, false},
		// A future major must not silently disarm.
		{"major3_positive_limit_arms", 3, 5.00, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := StageEnforcementApplies(tc.major, tc.limit); got != tc.want {
				t.Errorf("StageEnforcementApplies(%d,%v) = %v, want %v", tc.major, tc.limit, got, tc.want)
			}
		})
	}
}
