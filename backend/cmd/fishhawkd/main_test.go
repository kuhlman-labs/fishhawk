package main

import (
	"io"
	"testing"
)

// TestRun_BadFlag asserts run returns a non-zero exit code when a
// flag is invalid. Quick guard against accidentally swallowing
// flag.Parse errors.
func TestRun_BadFlag(t *testing.T) {
	if got := run([]string{"--no-such-flag"}, io.Discard); got == 0 {
		t.Error("run with bad flag returned 0, want non-zero")
	}
}

// TestRun_HelpFlag pins that --help exits without starting a listener.
// flag.ContinueOnError surfaces help as ErrHelp from Parse.
func TestRun_HelpFlag(t *testing.T) {
	if got := run([]string{"--help"}, io.Discard); got == 0 {
		t.Error("run --help returned 0, want non-zero")
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
