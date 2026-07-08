package latency

import (
	"testing"
	"time"
)

// base is a fixed reference instant so the tests are deterministic (the
// package itself never calls time.Now).
var base = time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)

// at returns base + d, a terse way to place an event on the timeline.
func at(d time.Duration) time.Time { return base.Add(d) }

// ev is a terse GateEvent constructor.
func ev(category string, d time.Duration) GateEvent {
	return GateEvent{Category: category, Timestamp: at(d)}
}

// gateByName finds a gate in a rollup by name, or nil.
func gateByName(r Rollup, name string) *GateWait {
	for i := range r.Gates {
		if r.Gates[i].Gate == name {
			return &r.Gates[i]
		}
	}
	return nil
}

// TestAggregate_FullyGatedChain drives a complete feature_change chain through
// all three gates and asserts each interval, the wait total, the wall clock,
// and the deterministic gate order. It also reconciles: each gate's
// ClosedAt−OpenedAt equals its WaitSeconds, and the sum equals
// TotalWaitOnHumanSeconds.
func TestAggregate_FullyGatedChain(t *testing.T) {
	events := []GateEvent{
		ev(CategoryPlanGenerated, 1*time.Minute),
		ev(CategoryApprovalSubmitted, 6*time.Minute), // plan_approval = 5min
		ev(CategoryImplementReviewed, 20*time.Minute),
		ev(CategoryImplementReviewed, 22*time.Minute),    // latest terminal review
		ev(CategoryAcceptanceDispatched, 25*time.Minute), // review→dispatch = 3min
		ev(CategoryCIGreen, 30*time.Minute),
		ev(CategoryPRMerged, 40*time.Minute), // checks_green→merge = 10min
	}
	r := AggregateGateLatency(events, at(0), at(40*time.Minute))

	if len(r.Gates) != 3 {
		t.Fatalf("want 3 gates, got %d: %+v", len(r.Gates), r.Gates)
	}
	// Deterministic order.
	wantOrder := []string{GatePlanApproval, GateImplementReviewToDispatch, GateChecksGreenToMerge}
	for i, name := range wantOrder {
		if r.Gates[i].Gate != name {
			t.Errorf("gate[%d] = %q, want %q", i, r.Gates[i].Gate, name)
		}
	}

	wantWait := map[string]float64{
		GatePlanApproval:              300,
		GateImplementReviewToDispatch: 180,
		GateChecksGreenToMerge:        600,
	}
	var sum float64
	for _, g := range r.Gates {
		if g.WaitSeconds != wantWait[g.Gate] {
			t.Errorf("%s wait = %g, want %g", g.Gate, g.WaitSeconds, wantWait[g.Gate])
		}
		// Reconciliation: the reported wait must equal the timestamp delta.
		if delta := g.ClosedAt.Sub(g.OpenedAt).Seconds(); delta != g.WaitSeconds {
			t.Errorf("%s ClosedAt−OpenedAt = %g, but WaitSeconds = %g", g.Gate, delta, g.WaitSeconds)
		}
		sum += g.WaitSeconds
	}
	if r.TotalWaitOnHumanSeconds != sum {
		t.Errorf("TotalWaitOnHumanSeconds = %g, want sum %g", r.TotalWaitOnHumanSeconds, sum)
	}
	if r.TotalWaitOnHumanSeconds != 1080 {
		t.Errorf("TotalWaitOnHumanSeconds = %g, want 1080", r.TotalWaitOnHumanSeconds)
	}
	if r.WallClockSeconds != 2400 {
		t.Errorf("WallClockSeconds = %g, want 2400", r.WallClockSeconds)
	}
}

// TestAggregate_LatestImplementReviewedIsTerminal proves the review→dispatch
// gate opens at the LATEST implement_reviewed (a feature_change run has two
// reviewers), not the first.
func TestAggregate_LatestImplementReviewedIsTerminal(t *testing.T) {
	events := []GateEvent{
		ev(CategoryImplementReviewed, 10*time.Minute), // first reviewer
		ev(CategoryImplementReviewed, 15*time.Minute), // terminal (latest)
		ev(CategoryAcceptanceDispatched, 18*time.Minute),
	}
	r := AggregateGateLatency(events, at(0), at(18*time.Minute))
	g := gateByName(r, GateImplementReviewToDispatch)
	if g == nil {
		t.Fatal("implement_review_to_dispatch gate missing")
	}
	if g.OpenedAt != at(15*time.Minute) {
		t.Errorf("OpenedAt = %v, want the latest implement_reviewed at +15m", g.OpenedAt)
	}
	if g.WaitSeconds != 180 {
		t.Errorf("wait = %g, want 180 (18m−15m)", g.WaitSeconds)
	}
}

// TestAggregate_AcceptanceAbsentFallsBackToMerge proves the review→dispatch
// gate closes on pr_merged when the workflow has no acceptance stage (the
// documented approximation).
func TestAggregate_AcceptanceAbsentFallsBackToMerge(t *testing.T) {
	events := []GateEvent{
		ev(CategoryImplementReviewed, 10*time.Minute),
		ev(CategoryPRMerged, 25*time.Minute), // no acceptance_dispatched → fall back
	}
	r := AggregateGateLatency(events, at(0), at(25*time.Minute))
	g := gateByName(r, GateImplementReviewToDispatch)
	if g == nil {
		t.Fatal("implement_review_to_dispatch gate missing on acceptance-absent chain")
	}
	if g.ClosedAt != at(25*time.Minute) || g.WaitSeconds != 900 {
		t.Errorf("gate = %+v, want ClosedAt +25m / wait 900", *g)
	}
}

// TestAggregate_PlanApprovalIgnoresLaterDeployApproval proves the plan gate
// pairs plan_generated with the FIRST following approval_submitted, so a later
// deploy approval_submitted (same category) is never mis-attributed.
func TestAggregate_PlanApprovalIgnoresLaterDeployApproval(t *testing.T) {
	events := []GateEvent{
		ev(CategoryPlanGenerated, 1*time.Minute),
		ev(CategoryApprovalSubmitted, 4*time.Minute),  // plan approval → wait 3min
		ev(CategoryApprovalSubmitted, 90*time.Minute), // later deploy approval — must be ignored
	}
	r := AggregateGateLatency(events, at(0), at(90*time.Minute))
	g := gateByName(r, GatePlanApproval)
	if g == nil {
		t.Fatal("plan_approval gate missing")
	}
	if g.WaitSeconds != 180 {
		t.Errorf("plan_approval wait = %g, want 180 (paired with the FIRST approval)", g.WaitSeconds)
	}
}

// TestAggregate_MissingOpeningMarkerOmitsGate: an approval_submitted with no
// preceding plan_generated yields no plan_approval gate.
func TestAggregate_MissingOpeningMarkerOmitsGate(t *testing.T) {
	events := []GateEvent{
		ev(CategoryApprovalSubmitted, 5*time.Minute),
	}
	r := AggregateGateLatency(events, at(0), at(5*time.Minute))
	if g := gateByName(r, GatePlanApproval); g != nil {
		t.Errorf("plan_approval must be omitted when plan_generated is absent; got %+v", *g)
	}
	if len(r.Gates) != 0 {
		t.Errorf("want no gates, got %+v", r.Gates)
	}
}

// TestAggregate_MissingClosingMarkerOmitsGate: a plan_generated with no
// following approval_submitted yields no plan_approval gate.
func TestAggregate_MissingClosingMarkerOmitsGate(t *testing.T) {
	events := []GateEvent{
		ev(CategoryPlanGenerated, 1*time.Minute),
		// no approval_submitted
	}
	r := AggregateGateLatency(events, at(0), at(1*time.Minute))
	if g := gateByName(r, GatePlanApproval); g != nil {
		t.Errorf("plan_approval must be omitted when approval_submitted is absent; got %+v", *g)
	}
}

// TestAggregate_NegativeDeltaClampsToZero: a closing marker earlier than its
// opening marker (clock skew) emits the gate with a 0 wait, not a negative
// one.
func TestAggregate_NegativeDeltaClampsToZero(t *testing.T) {
	events := []GateEvent{
		ev(CategoryPlanGenerated, 10*time.Minute),
		ev(CategoryApprovalSubmitted, 4*time.Minute), // BEFORE the plan_generated timestamp
	}
	r := AggregateGateLatency(events, at(0), at(10*time.Minute))
	g := gateByName(r, GatePlanApproval)
	if g == nil {
		t.Fatal("plan_approval gate should still be present with both markers")
	}
	if g.WaitSeconds != 0 {
		t.Errorf("negative delta must clamp to 0, got %g", g.WaitSeconds)
	}
	if r.TotalWaitOnHumanSeconds != 0 {
		t.Errorf("total wait = %g, want 0 (only a clamped gate)", r.TotalWaitOnHumanSeconds)
	}
}

// TestAggregate_EmptyEventsAllZero: an empty slice yields an all-zero rollup.
func TestAggregate_EmptyEventsAllZero(t *testing.T) {
	r := AggregateGateLatency(nil, time.Time{}, time.Time{})
	if len(r.Gates) != 0 {
		t.Errorf("want no gates, got %+v", r.Gates)
	}
	if r.TotalWaitOnHumanSeconds != 0 || r.WallClockSeconds != 0 {
		t.Errorf("want all-zero rollup, got total %g wall %g", r.TotalWaitOnHumanSeconds, r.WallClockSeconds)
	}
}

// TestAggregate_WallClockClampedNonNegative: a runEnd before runStart yields a
// 0 wall clock, never a negative one.
func TestAggregate_WallClockClampedNonNegative(t *testing.T) {
	r := AggregateGateLatency(nil, at(10*time.Minute), at(0))
	if r.WallClockSeconds != 0 {
		t.Errorf("WallClockSeconds = %g, want 0 for a reversed bound", r.WallClockSeconds)
	}
}
