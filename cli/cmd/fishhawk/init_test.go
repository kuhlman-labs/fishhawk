package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/cli/internal/bridge"
	"github.com/kuhlman-labs/fishhawk/cli/internal/spec"
)

// stubDoctorSeams makes the closing doctor preflight hermetic: every
// external command fails and every backend probe is unreachable. This
// mirrors the doctor-soft contract (an unreachable backend must not fail
// init) without touching the real Docker/git/gh/network environment.
func stubDoctorSeams(t *testing.T) {
	t.Helper()
	origRun := doctorRunOutput
	origHTTP := doctorHTTPDo
	doctorRunOutput = func(name string, _ ...string) (string, error) {
		return "", fmt.Errorf("stubbed: %s unavailable", name)
	}
	doctorHTTPDo = func(_ *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("stubbed: backend unreachable")
	}
	t.Cleanup(func() {
		doctorRunOutput = origRun
		doctorHTTPDo = origHTTP
	})
}

// newInitRepo returns a fresh temp dir carrying a `.git` marker so
// resolveRepoRoot treats it as the repo root.
func newInitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("create .git marker: %v", err)
	}
	return dir
}

func initSpecPath(dir string) string {
	return filepath.Join(dir, ".fishhawk", "workflows.yaml")
}

func TestInit_Golden(t *testing.T) {
	stubDoctorSeams(t)
	dir := newInitRepo(t)

	var stdout strings.Builder
	got := run([]string{"init", "--working-dir", dir, "--backend-url", "http://127.0.0.1:0"}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("status = %d, want exitOK\n%s", got, stdout.String())
	}

	// SHIPPED spec is schema-valid — not merely that the path was touched.
	data, err := os.ReadFile(initSpecPath(dir))
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	if err := spec.ValidateBytes(data); err != nil {
		t.Errorf("written spec fails ValidateBytes: %v", err)
	}

	// Bridge files: AGENTS.md carries the managed marker; CLAUDE.md imports it.
	agents, err := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if err != nil {
		t.Fatalf("read AGENTS.md: %v", err)
	}
	if !strings.Contains(string(agents), bridge.BeginMarker) {
		t.Errorf("AGENTS.md missing managed marker %q:\n%s", bridge.BeginMarker, agents)
	}
	claude, err := os.ReadFile(filepath.Join(dir, "CLAUDE.md"))
	if err != nil {
		t.Fatalf("read CLAUDE.md: %v", err)
	}
	if !strings.Contains(string(claude), bridge.ImportLine) {
		t.Errorf("CLAUDE.md missing %q import:\n%s", bridge.ImportLine, claude)
	}
}

func TestInit_NonDestructive(t *testing.T) {
	stubDoctorSeams(t)
	dir := newInitRepo(t)
	specPath := initSpecPath(dir)
	if err := os.MkdirAll(filepath.Dir(specPath), 0o755); err != nil {
		t.Fatal(err)
	}
	original := []byte("version: 0.1\n# hand-written; must not be clobbered\n")
	if err := os.WriteFile(specPath, original, 0o644); err != nil {
		t.Fatal(err)
	}

	// Refuse: without --force init exits failure and leaves the file
	// byte-identical.
	var stderr strings.Builder
	if got := run([]string{"init", "--working-dir", dir}, io.Discard, &stderr); got != exitFailure {
		t.Fatalf("status = %d, want exitFailure", got)
	}
	if !strings.Contains(stderr.String(), "--force") {
		t.Errorf("refuse message missing --force escape hatch: %s", stderr.String())
	}
	after, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Errorf("refuse path modified the spec:\n%s", after)
	}

	// --force overwrites, and the result still validates.
	var stdout strings.Builder
	if got := run([]string{"init", "--working-dir", dir, "--force"}, &stdout, io.Discard); got != exitOK {
		t.Fatalf("status = %d, want exitOK\n%s", got, stdout.String())
	}
	forced, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(forced, original) {
		t.Error("--force did not overwrite the existing spec")
	}
	if err := spec.ValidateBytes(forced); err != nil {
		t.Errorf("--force spec fails ValidateBytes: %v", err)
	}
}

func TestInit_ChecklistNamesOutOfBandPrereqs(t *testing.T) {
	stubDoctorSeams(t)
	dir := newInitRepo(t)

	var stdout strings.Builder
	if got := run([]string{"init", "--working-dir", dir}, &stdout, io.Discard); got != exitOK {
		t.Fatalf("status = %d, want exitOK", got)
	}
	out := stdout.String()
	for _, want := range []string{
		"https://github.com/apps/fishhawk/installations/new", // (a) App install
		"fishhawkd token issue",                              // (b) token issue
		"fishhawk.yml",                                       // (c) execution-path trio
		"FISHHAWK_BACKEND_URL",
		"ANTHROPIC_API_KEY",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("checklist missing %q:\n%s", want, out)
		}
	}
}

func TestInit_PresetHighMatchesGenerate(t *testing.T) {
	stubDoctorSeams(t)
	dir := newInitRepo(t)

	if got := run([]string{"init", "--working-dir", dir, "--preset", "high"}, io.Discard, io.Discard); got != exitOK {
		t.Fatalf("status = %d, want exitOK", got)
	}
	data, err := os.ReadFile(initSpecPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	want, err := spec.Generate(spec.PresetHigh, spec.Deltas{})
	if err != nil {
		t.Fatalf("reference Generate: %v", err)
	}
	if !bytes.Equal(data, want) {
		t.Errorf("--preset high bytes differ from spec.Generate(PresetHigh, {})\ngot:\n%s\nwant:\n%s", data, want)
	}
}

func TestInit_DeltasApplied(t *testing.T) {
	stubDoctorSeams(t)
	dir := newInitRepo(t)

	if got := run([]string{
		"init", "--working-dir", dir,
		"--preset", "medium",
		"--budget-usd", "250",
		"--single-reviewer",
		"--human-gates", "plan",
	}, io.Discard, io.Discard); got != exitOK {
		t.Fatalf("status = %d, want exitOK", got)
	}
	data, err := os.ReadFile(initSpecPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	limit := 250
	want, err := spec.Generate(spec.PresetMedium, spec.Deltas{
		BudgetLimitUSD: &limit,
		SingleReviewer: true,
		HumanGates:     []string{"plan"},
	})
	if err != nil {
		t.Fatalf("reference Generate: %v", err)
	}
	if !bytes.Equal(data, want) {
		t.Errorf("delta-applied bytes differ from the equivalent spec.Generate call\ngot:\n%s", data)
	}
	if err := spec.ValidateBytes(data); err != nil {
		t.Errorf("delta-applied spec fails ValidateBytes: %v", err)
	}
}

func TestInit_UnknownPreset(t *testing.T) {
	dir := newInitRepo(t)

	var stderr strings.Builder
	if got := run([]string{"init", "--working-dir", dir, "--preset", "bogus"}, io.Discard, &stderr); got != exitUsage {
		t.Fatalf("status = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "unknown --preset") {
		t.Errorf("stderr missing 'unknown --preset': %s", stderr.String())
	}
	// The bad preset must short-circuit before any spec is written.
	if _, err := os.Stat(initSpecPath(dir)); !os.IsNotExist(err) {
		t.Errorf("spec written despite unknown preset (stat err = %v)", err)
	}
}

func TestInit_DoctorSoft_UnreachableBackendStillExitsOK(t *testing.T) {
	stubDoctorSeams(t) // backend unreachable + every doctor rung degraded
	dir := newInitRepo(t)

	var stdout strings.Builder
	if got := run([]string{"init", "--working-dir", dir, "--backend-url", "http://127.0.0.1:0"}, &stdout, io.Discard); got != exitOK {
		t.Fatalf("status = %d, want exitOK (doctor failure must not fail init)", got)
	}
	// The scaffold itself still landed.
	if _, err := os.Stat(initSpecPath(dir)); err != nil {
		t.Errorf("spec not written on the doctor-soft path: %v", err)
	}
	// And init flagged that doctor reported issues rather than swallowing them.
	if !strings.Contains(stdout.String(), "doctor reported issues") {
		t.Errorf("doctor-soft note missing from stdout:\n%s", stdout.String())
	}
}
