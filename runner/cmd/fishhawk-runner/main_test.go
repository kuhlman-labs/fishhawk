package main

import (
	"io"
	"strings"
	"testing"
)

// TestRun_HappyPath exercises the no-op success path: every
// required flag set, run() returns 0 and writes a startup log line.
func TestRun_HappyPath(t *testing.T) {
	var out strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change",
		"--stage", "plan",
	}, &out)
	if got != exitOK {
		t.Errorf("run = %d, want %d", got, exitOK)
	}
	for _, want := range []string{
		`"event":"runner_started"`,
		`"run_id":"11111111-2222-3333-4444-555555555555"`,
		`"workflow":"feature_change"`,
		`"stage":"plan"`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("startup log missing %s:\n%s", want, out.String())
		}
	}
}

func TestRun_MissingRunID(t *testing.T) {
	var out strings.Builder
	got := run([]string{
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change",
		"--stage", "plan",
	}, &out)
	if got != exitUsage {
		t.Errorf("run = %d, want %d", got, exitUsage)
	}
}

func TestRun_BadFlag(t *testing.T) {
	got := run([]string{"--no-such-flag"}, io.Discard)
	if got != exitUsage {
		t.Errorf("run = %d, want %d", got, exitUsage)
	}
}

func TestRun_HelpExitsUsage(t *testing.T) {
	// flag.ContinueOnError + --help surfaces ErrHelp. We treat that
	// as a usage exit, same as a malformed flag.
	got := run([]string{"--help"}, io.Discard)
	if got != exitUsage {
		t.Errorf("run = %d, want %d", got, exitUsage)
	}
}

func TestRunnerVersion_NonEmpty(t *testing.T) {
	if runnerVersion() == "" {
		t.Fatal("runnerVersion() should never be empty")
	}
}
