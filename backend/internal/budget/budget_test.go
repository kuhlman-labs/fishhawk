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

func TestEvaluate_WeeklyPeriodStart(t *testing.T) {
	// Wednesday 2026-06-03; weekly period started Monday 2026-06-01.
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)

	d := Evaluate(10.0, 100.0, nil, PeriodWeekly, now, time.UTC)

	want := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if !d.PeriodStart.Equal(want) {
		t.Errorf("weekly PeriodStart = %v, want %v", d.PeriodStart, want)
	}
}
