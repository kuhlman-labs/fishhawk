package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validateValidYAML = `
version: "0.3"
roles:
  tech_lead:
    members: ["@org/tech-leads"]
workflows:
  feature_change:
    description: "Default workflow."
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        constraints:
          - max_files_changed: 30
        gates:
          - type: approval
            approvers:
              any_of: [tech_lead]
            sla: 4_hours
`

func writeTempSpec(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "workflows.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestValidate_HappyPath(t *testing.T) {
	path := writeTempSpec(t, validateValidYAML)
	var stdout, stderr strings.Builder

	got := runValidate([]string{path}, &stdout, &stderr)
	if got != exitOK {
		t.Fatalf("exit = %d, want 0:\nstderr: %s", got, stderr.String())
	}
	if !strings.Contains(stdout.String(), "OK") {
		t.Errorf("stdout missing OK: %q", stdout.String())
	}
}

func TestValidate_DefaultsToWorkflowsYaml(t *testing.T) {
	dir := t.TempDir()
	hidden := filepath.Join(dir, ".fishhawk")
	if err := os.MkdirAll(hidden, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(hidden, "workflows.yaml")
	if err := os.WriteFile(path, []byte(validateValidYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	// Switch CWD to the temp dir so the default path resolves.
	prev, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(prev) }()

	var stdout, stderr strings.Builder
	got := runValidate(nil, &stdout, &stderr)
	if got != exitOK {
		t.Fatalf("exit = %d, want 0:\nstderr: %s", got, stderr.String())
	}
}

func TestValidate_FileNotFound(t *testing.T) {
	var stdout, stderr strings.Builder
	got := runValidate([]string{"/no/such/path.yaml"}, &stdout, &stderr)
	if got != exitUsage {
		t.Errorf("exit = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "/no/such/path.yaml") {
		t.Errorf("stderr missing path: %q", stderr.String())
	}
}

func TestValidate_TooManyArgs(t *testing.T) {
	var stdout, stderr strings.Builder
	got := runValidate([]string{"a.yaml", "b.yaml"}, &stdout, &stderr)
	if got != exitUsage {
		t.Errorf("exit = %d, want exitUsage", got)
	}
}

func TestValidate_BadFlag(t *testing.T) {
	var stdout, stderr strings.Builder
	got := runValidate([]string{"--no-such-flag"}, &stdout, &stderr)
	if got != exitUsage {
		t.Errorf("exit = %d, want exitUsage", got)
	}
}

func TestValidate_HelpExits(t *testing.T) {
	var stdout, stderr strings.Builder
	got := runValidate([]string{"--help"}, &stdout, &stderr)
	if got != exitUsage {
		t.Errorf("exit = %d, want exitUsage (--help via flag.ContinueOnError)", got)
	}
	if !strings.Contains(stderr.String(), "Usage:") {
		t.Errorf("stderr missing usage: %q", stderr.String())
	}
}

func TestValidate_EmptyFile_ReturnsParseError(t *testing.T) {
	path := writeTempSpec(t, "")
	var stdout, stderr strings.Builder
	got := runValidate([]string{path}, &stdout, &stderr)
	if got != exitFailure {
		t.Errorf("exit = %d, want exitFailure", got)
	}
	if !strings.Contains(stderr.String(), "empty document") {
		t.Errorf("stderr missing empty document message: %q", stderr.String())
	}
}

func TestValidate_SchemaError_ReturnsValidationError(t *testing.T) {
	bad := strings.Replace(validateValidYAML, `version: "0.3"`, "", 1)
	path := writeTempSpec(t, bad)
	var stdout, stderr strings.Builder
	got := runValidate([]string{path}, &stdout, &stderr)
	if got != exitFailure {
		t.Errorf("exit = %d, want exitFailure", got)
	}
	// Each leaf is one stderr line; should include the file path.
	if !strings.Contains(stderr.String(), path) {
		t.Errorf("stderr missing path: %q", stderr.String())
	}
}

func TestValidate_StderrUsesPathPrefix(t *testing.T) {
	bad := strings.Replace(validateValidYAML, `type: implement`, `type: bogus`, 1)
	path := writeTempSpec(t, bad)
	var stdout, stderr strings.Builder
	got := runValidate([]string{path}, &stdout, &stderr)
	if got != exitFailure {
		t.Errorf("exit = %d, want exitFailure", got)
	}
	// Format: "<path>/<json-pointer>: <message>"
	for _, line := range strings.Split(strings.TrimSpace(stderr.String()), "\n") {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, path) {
			t.Errorf("stderr line %q doesn't start with %q", line, path)
		}
	}
}
