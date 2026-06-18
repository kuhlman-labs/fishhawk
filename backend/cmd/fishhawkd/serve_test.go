package main

import (
	"flag"
	"io"
	"testing"
)

// resolveMaxParallelChildren mirrors runServe's --max-parallel-children
// flag wiring (E24.6 / #1146) so the resolution precedence — explicit flag
// arg > FISHHAWKD_MAX_PARALLEL_CHILDREN env > the built-in 0 default — is
// unit-testable without booting the whole server. It is the same shape as
// the live `fs.Int("max-parallel-children", envOrInt(...), ...)` call.
func resolveMaxParallelChildren(t *testing.T, args []string) int {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	v := fs.Int("max-parallel-children",
		envOrInt("FISHHAWKD_MAX_PARALLEL_CHILDREN", 0),
		"test")
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse: %v", err)
	}
	return *v
}

// TestResolveMaxParallelChildren covers the FISHHAWKD_MAX_PARALLEL_CHILDREN
// resolution branches: the default applies when unset, the env value wins
// over the default, an explicit env 0 is honored as the unlimited semantic
// (not coerced), and a flag arg wins over the env.
func TestResolveMaxParallelChildren(t *testing.T) {
	const key = "FISHHAWKD_MAX_PARALLEL_CHILDREN"

	t.Run("unset resolves to default 0 (unlimited)", func(t *testing.T) {
		t.Setenv(key, "")
		if got := resolveMaxParallelChildren(t, nil); got != 0 {
			t.Errorf("got %d, want 0 (default unlimited)", got)
		}
	})

	t.Run("env value wins over default", func(t *testing.T) {
		t.Setenv(key, "4")
		if got := resolveMaxParallelChildren(t, nil); got != 4 {
			t.Errorf("got %d, want 4 (env over default)", got)
		}
	})

	t.Run("explicit env 0 is honored as unlimited", func(t *testing.T) {
		t.Setenv(key, "0")
		if got := resolveMaxParallelChildren(t, nil); got != 0 {
			t.Errorf("got %d, want 0 (explicit 0 must reach the cap as unlimited, not be coerced)", got)
		}
	})

	t.Run("flag arg wins over env", func(t *testing.T) {
		t.Setenv(key, "4")
		if got := resolveMaxParallelChildren(t, []string{"--max-parallel-children", "7"}); got != 7 {
			t.Errorf("got %d, want 7 (explicit flag over env)", got)
		}
	})
}

// TestEnvOrInt_MaxParallelChildren pins the FISHHAWKD_MAX_PARALLEL_CHILDREN
// env name the flag default resolves through envOrInt, so the env name can't
// silently drift from the flag wiring in runServe. Mirrors the explicit-0
// discipline of the plan-review-max-retries test: an env "0" must reach the
// setter as 0 (unlimited), not be treated as empty.
func TestEnvOrInt_MaxParallelChildren(t *testing.T) {
	const key = "FISHHAWKD_MAX_PARALLEL_CHILDREN"
	t.Run("unset returns default 0", func(t *testing.T) {
		t.Setenv(key, "")
		if got := envOrInt(key, 0); got != 0 {
			t.Errorf("got %d, want 0", got)
		}
	})
	t.Run("explicit 0 resolves to 0", func(t *testing.T) {
		t.Setenv(key, "0")
		if got := envOrInt(key, 0); got != 0 {
			t.Errorf("got %d, want 0 (explicit 0 is the unlimited sentinel, not empty)", got)
		}
	})
	t.Run("positive value resolves verbatim", func(t *testing.T) {
		t.Setenv(key, "5")
		if got := envOrInt(key, 0); got != 5 {
			t.Errorf("got %d, want 5", got)
		}
	})
}
