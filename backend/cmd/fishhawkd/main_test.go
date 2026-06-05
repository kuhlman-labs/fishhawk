package main

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
)

// TestRun_NoArgs falls through to serve — but we don't actually want
// the test to bind a port. Substitute --help to short-circuit through
// flag parsing without booting a listener.
func TestRun_HelpExitsZero(t *testing.T) {
	if got := run([]string{"help"}, io.Discard); got != exitOK {
		t.Errorf("run(help) = %d, want %d", got, exitOK)
	}
}

func TestRun_UnknownSubcommand(t *testing.T) {
	var out strings.Builder
	got := run([]string{"banana"}, &out)
	if got != exitUsage {
		t.Errorf("exit = %d, want %d", got, exitUsage)
	}
	if !strings.Contains(out.String(), "unknown subcommand") {
		t.Errorf("output missing usage error: %s", out.String())
	}
}

func TestRun_ServeBadFlag(t *testing.T) {
	if got := run([]string{"serve", "--no-such-flag"}, io.Discard); got != exitFailure {
		t.Errorf("run(serve --no-such-flag) = %d, want %d", got, exitFailure)
	}
}

func TestRun_MigrateNoDirection(t *testing.T) {
	var out strings.Builder
	got := run([]string{"migrate"}, &out)
	if got != exitUsage {
		t.Errorf("exit = %d, want %d", got, exitUsage)
	}
	if !strings.Contains(out.String(), "direction") {
		t.Errorf("output missing direction hint: %s", out.String())
	}
}

func TestRun_MigrateNoDBURL(t *testing.T) {
	t.Setenv("FISHHAWKD_DATABASE_URL", "")
	var out strings.Builder
	got := run([]string{"migrate", "up"}, &out)
	if got != exitUsage {
		t.Errorf("exit = %d, want %d", got, exitUsage)
	}
	if !strings.Contains(out.String(), "--db") {
		t.Errorf("output missing --db hint: %s", out.String())
	}
}

func TestRun_MigrateUnknownDirection(t *testing.T) {
	var out strings.Builder
	got := run([]string{"migrate", "sideways", "--db", "postgres://x:y@nowhere/db"}, &out)
	if got != exitUsage {
		t.Errorf("exit = %d, want %d", got, exitUsage)
	}
	if !strings.Contains(out.String(), "unknown direction") {
		t.Errorf("output missing direction error: %s", out.String())
	}
}

func TestSplitCommand(t *testing.T) {
	cases := []struct {
		name string
		args []string
		cmd  string
		rest []string
	}{
		{"empty", nil, "", nil},
		{"flag only goes to implicit serve", []string{"--addr=:9090"}, "", []string{"--addr=:9090"}},
		{"serve subcommand", []string{"serve", "--addr=:9090"}, "serve", []string{"--addr=:9090"}},
		{"migrate up", []string{"migrate", "up"}, "migrate", []string{"up"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd, rest := splitCommand(tc.args)
			if cmd != tc.cmd {
				t.Errorf("cmd = %q, want %q", cmd, tc.cmd)
			}
			if len(rest) != len(tc.rest) {
				t.Errorf("rest len = %d, want %d", len(rest), len(tc.rest))
			}
		})
	}
}

func TestEnvOr(t *testing.T) {
	const key = "FISHHAWKD_TEST_X"
	t.Run("empty env returns default", func(t *testing.T) {
		t.Setenv(key, "")
		if got := envOr(key, "fallback"); got != "fallback" {
			t.Errorf("got %q, want fallback", got)
		}
	})
	t.Run("set env returns env value", func(t *testing.T) {
		t.Setenv(key, "explicit")
		if got := envOr(key, "fallback"); got != "explicit" {
			t.Errorf("got %q, want explicit", got)
		}
	})
}

// TestEnvOrInt_PlanReviewMaxRetries covers the FISHHAWKD_PLAN_REVIEW_MAX_RETRIES
// resolution through envOrInt: unset->default, explicit "0"->0 (the retry-
// disable path, NOT treated as empty), a positive value, and a non-integer
// falling back to the default.
func TestEnvOrInt_PlanReviewMaxRetries(t *testing.T) {
	const key = "FISHHAWKD_PLAN_REVIEW_MAX_RETRIES"
	t.Run("unset returns default 1", func(t *testing.T) {
		t.Setenv(key, "")
		if got := envOrInt(key, 1); got != 1 {
			t.Errorf("got %d, want 1", got)
		}
	})
	t.Run("explicit 0 resolves to 0", func(t *testing.T) {
		t.Setenv(key, "0")
		if got := envOrInt(key, 1); got != 0 {
			t.Errorf("got %d, want 0 (explicit 0 must reach the setter as disable)", got)
		}
	})
	t.Run("positive value resolves verbatim", func(t *testing.T) {
		t.Setenv(key, "3")
		if got := envOrInt(key, 1); got != 3 {
			t.Errorf("got %d, want 3", got)
		}
	})
	t.Run("non-integer falls back to default", func(t *testing.T) {
		t.Setenv(key, "notanint")
		if got := envOrInt(key, 1); got != 1 {
			t.Errorf("got %d, want 1 (garbage falls back to default)", got)
		}
	})
}

// TestResolveBudgetLocation covers the FISHHAWKD_BUDGET_TIMEZONE
// resolution (#688): a valid IANA name resolves to that zone, while a
// bogus name (missing zoneinfo / typo) falls back to time.UTC rather
// than crashing startup.
func TestResolveBudgetLocation(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("valid zone resolves", func(t *testing.T) {
		loc := resolveBudgetLocation("America/New_York", logger)
		if loc == nil || loc.String() != "America/New_York" {
			t.Errorf("got %v, want America/New_York", loc)
		}
	})
	t.Run("UTC resolves", func(t *testing.T) {
		loc := resolveBudgetLocation("UTC", logger)
		if loc == nil || loc.String() != "UTC" {
			t.Errorf("got %v, want UTC", loc)
		}
	})
	t.Run("bogus zone falls back to UTC", func(t *testing.T) {
		loc := resolveBudgetLocation("Not/AZone", logger)
		if loc != time.UTC {
			t.Errorf("got %v, want time.UTC fallback", loc)
		}
	})
}

// TestReviewBudgetEnvWiring asserts the #747 size-aware review budget resolves
// from FISHHAWKD_PLAN_REVIEW_TIMEOUT (floor), FISHHAWKD_REVIEW_BUDGET_PER_KB
// (per-KB allowance), and FISHHAWKD_REVIEW_BUDGET_CAP (ceiling) — and that the
// floor input still feeds planReviewTimeoutBelowDefault so the warn predicate
// continues to track the Floor.
func TestReviewBudgetEnvWiring(t *testing.T) {
	t.Run("unset falls back to documented defaults", func(t *testing.T) {
		t.Setenv("FISHHAWKD_PLAN_REVIEW_TIMEOUT", "")
		t.Setenv("FISHHAWKD_REVIEW_BUDGET_PER_KB", "")
		t.Setenv("FISHHAWKD_REVIEW_BUDGET_CAP", "")
		b := planreview.ReviewBudget{
			Floor: envOrDuration("FISHHAWKD_PLAN_REVIEW_TIMEOUT", defaultPlanReviewTimeout),
			PerKB: envOrDuration("FISHHAWKD_REVIEW_BUDGET_PER_KB", planreview.DefaultReviewBudget.PerKB),
			Cap:   envOrDuration("FISHHAWKD_REVIEW_BUDGET_CAP", planreview.DefaultReviewBudget.Cap),
		}
		if b != planreview.DefaultReviewBudget {
			t.Errorf("budget = %+v, want defaults %+v", b, planreview.DefaultReviewBudget)
		}
	})

	t.Run("explicit env values resolve and scale", func(t *testing.T) {
		t.Setenv("FISHHAWKD_PLAN_REVIEW_TIMEOUT", "120s")
		t.Setenv("FISHHAWKD_REVIEW_BUDGET_PER_KB", "5s")
		t.Setenv("FISHHAWKD_REVIEW_BUDGET_CAP", "600s")
		b := planreview.ReviewBudget{
			Floor: envOrDuration("FISHHAWKD_PLAN_REVIEW_TIMEOUT", defaultPlanReviewTimeout),
			PerKB: envOrDuration("FISHHAWKD_REVIEW_BUDGET_PER_KB", planreview.DefaultReviewBudget.PerKB),
			Cap:   envOrDuration("FISHHAWKD_REVIEW_BUDGET_CAP", planreview.DefaultReviewBudget.Cap),
		}
		if b.Floor != 120*time.Second || b.PerKB != 5*time.Second || b.Cap != 600*time.Second {
			t.Fatalf("budget = %+v, want {120s 5s 600s}", b)
		}
		// 10KB prompt: 120s + 10*5s = 170s, under the cap.
		if got := b.Budget(10 * 1024); got != 170*time.Second {
			t.Errorf("Budget(10KB) = %v, want 170s", got)
		}
		// The floor input still drives the #664 warn predicate.
		if !planReviewTimeoutBelowDefault(b.Floor) {
			t.Errorf("floor %v should trip the below-default warn predicate", b.Floor)
		}
	})

	t.Run("per-kb zero collapses to a flat floor", func(t *testing.T) {
		t.Setenv("FISHHAWKD_PLAN_REVIEW_TIMEOUT", "")
		t.Setenv("FISHHAWKD_REVIEW_BUDGET_PER_KB", "0s")
		t.Setenv("FISHHAWKD_REVIEW_BUDGET_CAP", "")
		b := planreview.ReviewBudget{
			Floor: envOrDuration("FISHHAWKD_PLAN_REVIEW_TIMEOUT", defaultPlanReviewTimeout),
			PerKB: envOrDuration("FISHHAWKD_REVIEW_BUDGET_PER_KB", planreview.DefaultReviewBudget.PerKB),
			Cap:   envOrDuration("FISHHAWKD_REVIEW_BUDGET_CAP", planreview.DefaultReviewBudget.Cap),
		}
		if got := b.Budget(500 * 1024); got != defaultPlanReviewTimeout {
			t.Errorf("Budget with PerKB=0 = %v, want flat floor %v", got, defaultPlanReviewTimeout)
		}
	})
}

// TestPlanReviewTimeoutBelowDefault pins the #664 warn-threshold predicate:
// strictly-below the 300s #606 floor warns; at-or-above does not. Driving the
// boundary through the pure helper keeps the assertion off startup-log
// capture and guarantees the warn threshold tracks defaultPlanReviewTimeout.
func TestPlanReviewTimeoutBelowDefault(t *testing.T) {
	cases := []struct {
		name       string
		configured time.Duration
		want       bool
	}{
		{"below", 180 * time.Second, true},
		{"just_below", defaultPlanReviewTimeout - time.Second, true},
		{"equal", defaultPlanReviewTimeout, false},
		{"above", 600 * time.Second, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := planReviewTimeoutBelowDefault(tc.configured); got != tc.want {
				t.Errorf("planReviewTimeoutBelowDefault(%v) = %v, want %v", tc.configured, got, tc.want)
			}
		})
	}
}
