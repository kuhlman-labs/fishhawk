package main

import (
	"io"
	"strings"
	"testing"
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
