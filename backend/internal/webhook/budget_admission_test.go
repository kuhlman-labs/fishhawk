package webhook

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// fakeSummer is a CostSummer that returns a fixed spend (or error) for
// every (repo, workflowID, range) query.
type fakeSummer struct {
	spent float64
	err   error
	calls int
}

func (f *fakeSummer) SumWorkflowCostInRange(_ context.Context, _, _ string, _, _ time.Time) (float64, error) {
	f.calls++
	if f.err != nil {
		return 0, f.err
	}
	return f.spent, nil
}

func blockingBudget(limit float64) spec.PeriodicBudget {
	return spec.PeriodicBudget{Period: spec.BudgetPeriodWeekly, LimitUSD: limit, Enforcement: spec.EnforcementBlocking}
}

func TestCheckBlockingBudget(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)

	t.Run("over-limit blocking budget blocks", func(t *testing.T) {
		s := &fakeSummer{spent: 60}
		blocked, off, d, err := CheckBlockingBudget(ctx, s, "x/y", "wf", []spec.PeriodicBudget{blockingBudget(50)}, now, time.UTC)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if !blocked {
			t.Fatal("blocked = false, want true (spend over limit)")
		}
		if off.LimitUSD != 50 {
			t.Errorf("offending limit = %v, want 50", off.LimitUSD)
		}
		if !d.Over {
			t.Error("decision Over = false, want true")
		}
	})

	t.Run("under-limit does not block", func(t *testing.T) {
		s := &fakeSummer{spent: 10}
		blocked, _, _, err := CheckBlockingBudget(ctx, s, "x/y", "wf", []spec.PeriodicBudget{blockingBudget(50)}, now, time.UTC)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if blocked {
			t.Fatal("blocked = true, want false (spend under limit)")
		}
	})

	t.Run("advisory budget over 100% does not block", func(t *testing.T) {
		s := &fakeSummer{spent: 999}
		b := spec.PeriodicBudget{Period: spec.BudgetPeriodWeekly, LimitUSD: 50, Enforcement: spec.EnforcementAdvisory}
		blocked, _, _, err := CheckBlockingBudget(ctx, s, "x/y", "wf", []spec.PeriodicBudget{b}, now, time.UTC)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if blocked {
			t.Fatal("blocked = true, want false (advisory never gates here)")
		}
		if s.calls != 0 {
			t.Errorf("summer called %d times, want 0 (advisory skipped before sum)", s.calls)
		}
	})

	t.Run("empty-enforcement treated as advisory does not block", func(t *testing.T) {
		s := &fakeSummer{spent: 999}
		b := spec.PeriodicBudget{Period: spec.BudgetPeriodWeekly, LimitUSD: 50}
		blocked, _, _, err := CheckBlockingBudget(ctx, s, "x/y", "wf", []spec.PeriodicBudget{b}, now, time.UTC)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if blocked {
			t.Fatal("blocked = true, want false (empty enforcement is advisory)")
		}
	})

	t.Run("summer error is fail-open", func(t *testing.T) {
		s := &fakeSummer{err: errors.New("db down")}
		blocked, _, _, err := CheckBlockingBudget(ctx, s, "x/y", "wf", []spec.PeriodicBudget{blockingBudget(50)}, now, time.UTC)
		if err == nil {
			t.Fatal("err = nil, want the sum error surfaced")
		}
		if blocked {
			t.Fatal("blocked = true, want false on sum error (fail-open)")
		}
	})

	t.Run("first over blocking budget of many is returned", func(t *testing.T) {
		s := &fakeSummer{spent: 100}
		budgets := []spec.PeriodicBudget{
			{Period: spec.BudgetPeriodWeekly, LimitUSD: 50, Enforcement: spec.EnforcementAdvisory}, // skipped
			blockingBudget(80),  // first blocking + over → returned
			blockingBudget(200), // not reached
		}
		blocked, off, _, err := CheckBlockingBudget(ctx, s, "x/y", "wf", budgets, now, time.UTC)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if !blocked {
			t.Fatal("blocked = false, want true")
		}
		if off.LimitUSD != 80 {
			t.Errorf("offending limit = %v, want 80 (first over blocking budget)", off.LimitUSD)
		}
	})

	t.Run("nil loc defaults to UTC", func(t *testing.T) {
		s := &fakeSummer{spent: 60}
		blocked, _, _, err := CheckBlockingBudget(ctx, s, "x/y", "wf", []spec.PeriodicBudget{blockingBudget(50)}, now, nil)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if !blocked {
			t.Fatal("blocked = false, want true (nil loc → UTC)")
		}
	})
}
