package budget

import (
	"testing"
	"time"
)

func floatPtr(f float64) *float64 { return &f }

func TestPeriodRange_WeeklyResetsOnMonday(t *testing.T) {
	// 2026-06-03 is a Wednesday; the ISO week began Monday 2026-06-01 and
	// ends the following Monday 2026-06-08, both at local midnight.
	now := time.Date(2026, 6, 3, 14, 30, 0, 0, time.UTC)

	start, end := PeriodRange(PeriodWeekly, now, time.UTC)

	wantStart := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	wantEnd := time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC)
	if !start.Equal(wantStart) {
		t.Errorf("weekly start = %v, want %v", start, wantStart)
	}
	if !end.Equal(wantEnd) {
		t.Errorf("weekly end = %v, want %v", end, wantEnd)
	}
}

func TestPeriodRange_WeeklyOnSundayStaysInPriorMonday(t *testing.T) {
	// Sunday 2026-06-07 still belongs to the week that began Monday
	// 2026-06-01 — the reset is Monday, not Sunday.
	now := time.Date(2026, 6, 7, 23, 59, 0, 0, time.UTC)

	start, _ := PeriodRange(PeriodWeekly, now, time.UTC)

	want := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if !start.Equal(want) {
		t.Errorf("Sunday weekly start = %v, want %v", start, want)
	}
}

func TestPeriodRange_MonthlyResetsOnFirst(t *testing.T) {
	now := time.Date(2026, 6, 17, 9, 0, 0, 0, time.UTC)

	start, end := PeriodRange(PeriodMonthly, now, time.UTC)

	wantStart := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	wantEnd := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	if !start.Equal(wantStart) {
		t.Errorf("monthly start = %v, want %v", start, wantStart)
	}
	if !end.Equal(wantEnd) {
		t.Errorf("monthly end = %v, want %v", end, wantEnd)
	}
}

func TestPeriodRange_MonthlyDecemberWrapsToJanuary(t *testing.T) {
	now := time.Date(2026, 12, 25, 0, 0, 0, 0, time.UTC)

	start, end := PeriodRange(PeriodMonthly, now, time.UTC)

	wantStart := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	wantEnd := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	if !start.Equal(wantStart) {
		t.Errorf("December monthly start = %v, want %v", start, wantStart)
	}
	if !end.Equal(wantEnd) {
		t.Errorf("December monthly end = %v, want %v", end, wantEnd)
	}
}

func TestPeriodRange_TimezoneBoundary(t *testing.T) {
	// A non-UTC location whose local midnight differs from UTC midnight.
	// At 2026-06-01T02:00Z it is still 2026-05-31T22:00 in America/New_York
	// (UTC-4 in June), so the monthly period there is still May, while in
	// UTC it has already rolled to June. This pins the reset to the
	// supplied location rather than UTC.
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	instant := time.Date(2026, 6, 1, 2, 0, 0, 0, time.UTC)

	utcStart, _ := PeriodRange(PeriodMonthly, instant, time.UTC)
	if want := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC); !utcStart.Equal(want) {
		t.Errorf("UTC monthly start = %v, want %v", utcStart, want)
	}

	nyStart, _ := PeriodRange(PeriodMonthly, instant, ny)
	if want := time.Date(2026, 5, 1, 0, 0, 0, 0, ny); !nyStart.Equal(want) {
		t.Errorf("NY monthly start = %v, want %v (local time was still May)", nyStart, want)
	}
}

func TestPeriodRange_NilLocationIsUTC(t *testing.T) {
	now := time.Date(2026, 6, 17, 9, 0, 0, 0, time.UTC)

	gotStart, gotEnd := PeriodRange(PeriodMonthly, now, nil)
	wantStart, wantEnd := PeriodRange(PeriodMonthly, now, time.UTC)
	if !gotStart.Equal(wantStart) || !gotEnd.Equal(wantEnd) {
		t.Errorf("nil location = [%v,%v), want UTC [%v,%v)", gotStart, gotEnd, wantStart, wantEnd)
	}
}

func TestPeriodRange_UnknownPeriodIsZero(t *testing.T) {
	now := time.Date(2026, 6, 17, 9, 0, 0, 0, time.UTC)

	start, end := PeriodRange("quarterly", now, time.UTC)
	if !start.IsZero() || !end.IsZero() {
		t.Errorf("unknown period = [%v,%v), want zero range", start, end)
	}
}

func TestEvaluate_UnderBudgetNoCrossing(t *testing.T) {
	now := time.Date(2026, 6, 17, 9, 0, 0, 0, time.UTC)

	d := Evaluate(40.0, 100.0, floatPtr(0.8), PeriodMonthly, now, time.UTC)

	if d.Over {
		t.Error("did not expect Over at 40% spend")
	}
	if d.WarnCrossed {
		t.Error("did not expect WarnCrossed below the 80% warn_at")
	}
	if d.Fraction != 0.4 {
		t.Errorf("Fraction = %v, want 0.4", d.Fraction)
	}
	if d.Spent != 40.0 || d.Limit != 100.0 {
		t.Errorf("echoed Spent/Limit = %v/%v, want 40/100", d.Spent, d.Limit)
	}
	if want := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC); !d.PeriodStart.Equal(want) {
		t.Errorf("PeriodStart = %v, want %v", d.PeriodStart, want)
	}
}

func TestEvaluate_WarnAtCrossing(t *testing.T) {
	now := time.Date(2026, 6, 17, 9, 0, 0, 0, time.UTC)
	warn := floatPtr(0.8)

	// Just below the warn threshold: 79% < 80%.
	below := Evaluate(79.0, 100.0, warn, PeriodMonthly, now, time.UTC)
	if below.WarnCrossed {
		t.Error("did not expect WarnCrossed at 79%")
	}

	// Exactly at the warn threshold crosses (>= warn_at).
	at := Evaluate(80.0, 100.0, warn, PeriodMonthly, now, time.UTC)
	if !at.WarnCrossed {
		t.Error("expected WarnCrossed at exactly 80%")
	}
	if at.Over {
		t.Error("did not expect Over at 80%")
	}
}

func TestEvaluate_NilWarnAtNeverWarns(t *testing.T) {
	now := time.Date(2026, 6, 17, 9, 0, 0, 0, time.UTC)

	d := Evaluate(99.0, 100.0, nil, PeriodMonthly, now, time.UTC)
	if d.WarnCrossed {
		t.Error("expected no WarnCrossed when warn_at is nil")
	}
	if d.Over {
		t.Error("did not expect Over at 99%")
	}
}

func TestEvaluate_ExactHundredPercentIsOver(t *testing.T) {
	now := time.Date(2026, 6, 17, 9, 0, 0, 0, time.UTC)

	d := Evaluate(100.0, 100.0, floatPtr(0.8), PeriodMonthly, now, time.UTC)
	if !d.Over {
		t.Error("expected Over at exactly 100% (spent == limit)")
	}
	if !d.WarnCrossed {
		t.Error("expected WarnCrossed at 100% (fraction >= warn_at)")
	}
	if d.Fraction != 1.0 {
		t.Errorf("Fraction = %v, want 1.0", d.Fraction)
	}
}

func TestEvaluate_OverBudget(t *testing.T) {
	now := time.Date(2026, 6, 17, 9, 0, 0, 0, time.UTC)

	d := Evaluate(150.0, 100.0, floatPtr(0.8), PeriodMonthly, now, time.UTC)
	if !d.Over {
		t.Error("expected Over at 150% spend")
	}
	if d.Fraction != 1.5 {
		t.Errorf("Fraction = %v, want 1.5", d.Fraction)
	}
}

func TestEvaluate_NonPositiveLimitNeverBlocks(t *testing.T) {
	now := time.Date(2026, 6, 17, 9, 0, 0, 0, time.UTC)

	d := Evaluate(50.0, 0, floatPtr(0.8), PeriodMonthly, now, time.UTC)
	if d.Over {
		t.Error("a non-positive limit must not register as Over")
	}
	if d.WarnCrossed {
		t.Error("a non-positive limit must not warn")
	}
	if d.Fraction != 0 {
		t.Errorf("Fraction = %v, want 0 for non-positive limit", d.Fraction)
	}
}

// decisionAtFraction builds a Decision with the given Fraction and the
// Over/WarnCrossed flags Evaluate would set for it against warnAt, so
// TestTier exercises Tier with realistic decisions rather than hand-set
// flag combinations.
func decisionAtFraction(fraction float64, warnAt *float64) Decision {
	d := Decision{Fraction: fraction, Over: fraction >= 1.0}
	if warnAt != nil && *warnAt > 0 {
		d.WarnCrossed = fraction >= *warnAt
	}
	return d
}

func TestTier(t *testing.T) {
	warn := floatPtr(0.8)
	const ack, page = 2.0, 3.0

	cases := []struct {
		name     string
		fraction float64
		want     string
	}{
		{"below warn", 0.5, TierOK},
		{"just below warn", 0.79, TierOK},
		{"at warn", 0.8, TierWarn},
		{"between warn and limit", 0.95, TierWarn},
		{"just below limit", 0.999, TierWarn},
		{"at limit", 1.0, TierOver},
		{"over but below ack", 1.5, TierOver},
		{"just below ack", 1.999, TierOver},
		{"at ack", 2.0, TierAckRequired},
		{"between ack and page", 2.5, TierAckRequired},
		{"just below page", 2.999, TierAckRequired},
		{"at page", 3.0, TierPage},
		{"above page", 5.0, TierPage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := decisionAtFraction(tc.fraction, warn)
			if got := Tier(d, ack, page); got != tc.want {
				t.Errorf("Tier(fraction=%g) = %q, want %q", tc.fraction, got, tc.want)
			}
		})
	}
}

// TestTier_NoWarnAt confirms that with a nil warn_at the sub-limit bands
// never reach 'warn' — a Decision Evaluate produced with WarnCrossed=false
// stays 'ok' right up to the limit, then escalates normally.
func TestTier_NoWarnAt(t *testing.T) {
	const ack, page = 2.0, 3.0
	if got := Tier(decisionAtFraction(0.95, nil), ack, page); got != TierOK {
		t.Errorf("Tier(0.95, nil warn_at) = %q, want %q", got, TierOK)
	}
	if got := Tier(decisionAtFraction(1.0, nil), ack, page); got != TierOver {
		t.Errorf("Tier(1.0, nil warn_at) = %q, want %q", got, TierOver)
	}
}

// TestTier_DefensiveFallback pins the zero/inverted-multiple guard: an
// unconfigured (zero-value) Config must NOT collapse every positive
// fraction into 'page', and an inverted pair (page <= ack) must fall back
// to the 2x/3x defaults rather than honoring the bad ordering.
func TestTier_DefensiveFallback(t *testing.T) {
	warn := floatPtr(0.8)

	// Zero multiples → defaults (2x ack, 3x page). A 1.5x fraction is
	// merely 'over', not 'page'; a 2.5x fraction is 'ack_required'.
	if got := Tier(decisionAtFraction(1.5, warn), 0, 0); got != TierOver {
		t.Errorf("Tier(1.5, zero multiples) = %q, want %q (defaults applied)", got, TierOver)
	}
	if got := Tier(decisionAtFraction(2.5, warn), 0, 0); got != TierAckRequired {
		t.Errorf("Tier(2.5, zero multiples) = %q, want %q (defaults applied)", got, TierAckRequired)
	}
	if got := Tier(decisionAtFraction(3.0, warn), 0, 0); got != TierPage {
		t.Errorf("Tier(3.0, zero multiples) = %q, want %q (defaults applied)", got, TierPage)
	}

	// Inverted pair (page <= ack) → defaults, not the bad ordering. With
	// the bad pair honored, a 2.0x fraction would be 'page' (>= page=1.5);
	// with the fallback it is 'ack_required' (>= 2.0 default ack).
	if got := Tier(decisionAtFraction(2.0, warn), 2.0, 1.5); got != TierAckRequired {
		t.Errorf("Tier(2.0, inverted page<=ack) = %q, want %q (defaults applied)", got, TierAckRequired)
	}

	// A negative multiple is non-positive → defaults.
	if got := Tier(decisionAtFraction(2.5, warn), -1, 3.0); got != TierAckRequired {
		t.Errorf("Tier(2.5, negative ack) = %q, want %q (defaults applied)", got, TierAckRequired)
	}
}

func TestAckRequired(t *testing.T) {
	for _, tier := range []string{TierOK, TierWarn, TierOver} {
		if AckRequired(tier) {
			t.Errorf("AckRequired(%q) = true, want false", tier)
		}
	}
	for _, tier := range []string{TierAckRequired, TierPage} {
		if !AckRequired(tier) {
			t.Errorf("AckRequired(%q) = false, want true", tier)
		}
	}
}

func TestEvaluate_WeeklyPeriodStart(t *testing.T) {
	// Wednesday 2026-06-03; weekly period started Monday 2026-06-01.
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)

	d := Evaluate(10.0, 100.0, nil, PeriodWeekly, now, time.UTC)

	want := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if !d.PeriodStart.Equal(want) {
		t.Errorf("weekly PeriodStart = %v, want %v", d.PeriodStart, want)
	}
}
