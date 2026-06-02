package spendalert

import (
	"testing"
	"time"
)

// base is a fixed UTC instant mid-hour so Truncate(hour) lands on a
// predictable bucket and the tests don't depend on wall-clock timing.
var base = time.Date(2026, 6, 2, 12, 30, 0, 0, time.UTC)

func TestEvaluate_AnomalyTripsWithLowThreshold(t *testing.T) {
	// Three prior hours at ~$1 each (avg $1), then a current hour at $5.
	// With a low 2x threshold, $5 > 2*$1 trips.
	samples := []Sample{
		{Time: base.Add(-3 * time.Hour), USD: 1.0},
		{Time: base.Add(-2 * time.Hour), USD: 1.0},
		{Time: base.Add(-1 * time.Hour), USD: 1.0},
		{Time: base, USD: 5.0},
	}

	d := Evaluate(samples, base, 2.0)

	if !d.Tripped {
		t.Fatalf("expected trip: latest=%v avg=%v ratio=%v multiple=%v",
			d.LatestHourUSD, d.RollingAvgUSD, d.Ratio, d.Multiple)
	}
	if d.PriorHours != 3 {
		t.Errorf("PriorHours = %d, want 3", d.PriorHours)
	}
	if d.RollingAvgUSD != 1.0 {
		t.Errorf("RollingAvgUSD = %v, want 1.0", d.RollingAvgUSD)
	}
	if d.LatestHourUSD != 5.0 {
		t.Errorf("LatestHourUSD = %v, want 5.0", d.LatestHourUSD)
	}
	if d.Ratio != 5.0 {
		t.Errorf("Ratio = %v, want 5.0", d.Ratio)
	}
}

func TestEvaluate_NoAlertUnderNormalSpend(t *testing.T) {
	// Steady ~$1/hour including the current hour. With the default 3x
	// threshold nothing should trip — $1 is not > 3*$1.
	samples := []Sample{
		{Time: base.Add(-3 * time.Hour), USD: 1.0},
		{Time: base.Add(-2 * time.Hour), USD: 1.0},
		{Time: base.Add(-1 * time.Hour), USD: 1.0},
		{Time: base, USD: 1.0},
	}

	d := Evaluate(samples, base, DefaultMultiple)

	if d.Tripped {
		t.Fatalf("did not expect trip under steady spend: latest=%v avg=%v ratio=%v",
			d.LatestHourUSD, d.RollingAvgUSD, d.Ratio)
	}
	if d.RollingAvgUSD != 1.0 {
		t.Errorf("RollingAvgUSD = %v, want 1.0", d.RollingAvgUSD)
	}
}

func TestEvaluate_NoBaselineNeverTrips(t *testing.T) {
	// Only a current-hour sample, however large, with no prior history:
	// there's nothing to be anomalous against, so no alert.
	samples := []Sample{{Time: base, USD: 1000.0}}

	d := Evaluate(samples, base, DefaultMultiple)

	if d.Tripped {
		t.Fatal("expected no trip without a baseline")
	}
	if d.PriorHours != 0 {
		t.Errorf("PriorHours = %d, want 0", d.PriorHours)
	}
	if d.RollingAvgUSD != 0 {
		t.Errorf("RollingAvgUSD = %v, want 0", d.RollingAvgUSD)
	}
}

func TestEvaluate_DefaultsMultipleWhenNonPositive(t *testing.T) {
	samples := []Sample{
		{Time: base.Add(-1 * time.Hour), USD: 1.0},
		{Time: base, USD: 10.0},
	}

	d := Evaluate(samples, base, 0)

	if d.Multiple != DefaultMultiple {
		t.Errorf("Multiple = %v, want default %v", d.Multiple, DefaultMultiple)
	}
	// 10 > 3*1, so it still trips under the defaulted threshold.
	if !d.Tripped {
		t.Error("expected trip under defaulted multiple")
	}
}

func TestEvaluate_ExcludesSamplesOlderThanWindow(t *testing.T) {
	// A big spend just outside the window must not seed a baseline; with
	// only one in-window prior hour at $1 and a current hour at $1, the
	// out-of-window $1000 is ignored and nothing trips.
	samples := []Sample{
		{Time: base.Add(-Window - time.Hour), USD: 1000.0},
		{Time: base.Add(-1 * time.Hour), USD: 1.0},
		{Time: base, USD: 1.0},
	}

	d := Evaluate(samples, base, DefaultMultiple)

	if d.PriorHours != 1 {
		t.Errorf("PriorHours = %d, want 1 (out-of-window sample excluded)", d.PriorHours)
	}
	if d.RollingAvgUSD != 1.0 {
		t.Errorf("RollingAvgUSD = %v, want 1.0", d.RollingAvgUSD)
	}
	if d.Tripped {
		t.Error("did not expect trip when the only baseline is in-window steady spend")
	}
}

func TestEvaluate_SumsMultipleSamplesPerHour(t *testing.T) {
	// Two prior hours, the most recent split across two samples summing
	// to $4; average over the two hours is (2+4)/2 = $3. Current hour $10
	// > 3*3=9 trips at the default multiple.
	samples := []Sample{
		{Time: base.Add(-2 * time.Hour), USD: 2.0},
		{Time: base.Add(-1 * time.Hour), USD: 1.5},
		{Time: base.Add(-1 * time.Hour).Add(10 * time.Minute), USD: 2.5},
		{Time: base, USD: 10.0},
	}

	d := Evaluate(samples, base, DefaultMultiple)

	if d.PriorHours != 2 {
		t.Errorf("PriorHours = %d, want 2", d.PriorHours)
	}
	if d.RollingAvgUSD != 3.0 {
		t.Errorf("RollingAvgUSD = %v, want 3.0", d.RollingAvgUSD)
	}
	if !d.Tripped {
		t.Errorf("expected trip: latest=%v avg=%v", d.LatestHourUSD, d.RollingAvgUSD)
	}
}
