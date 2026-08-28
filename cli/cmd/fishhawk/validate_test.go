package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/cli/internal/spec"
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

// TestRunValidate_SuccessPrintsResidualLine is the done-means for option 3: a
// valid spec exits 0 and prints BOTH the unchanged `<path>: OK` line and the
// residual-scope note, the latter asserted against the exported constant so a
// comment-only touch of validate.go cannot satisfy it.
func TestRunValidate_SuccessPrintsResidualLine(t *testing.T) {
	path := writeTempSpec(t, validateValidYAML)
	var stdout, stderr strings.Builder

	got := runValidate([]string{path}, &stdout, &stderr)
	if got != exitOK {
		t.Fatalf("exit = %d, want 0:\nstderr: %s", got, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, path+": OK\n") {
		t.Errorf("stdout missing the unchanged OK line: %q", out)
	}
	if !strings.Contains(out, validateResidualLine) {
		t.Errorf("stdout missing the residual-scope line:\nwant substring: %q\ngot: %q", validateResidualLine, out)
	}
}

// TestRunValidate_RejectsDanglingFromStage is the CROSS-BOUNDARY end-to-end
// vehicle (E52.13 / #2323): a spec whose ONLY defect is a dangling
// inputs[].from_stage now exits 1 with the shipped message on stderr, carrying
// the file path AND the JSON pointer. It exercises raw decode → schema → needs
// expansion → graph-shape → error rendering → exit code in one pass — the
// asymmetry this change closes was that `fishhawk validate` previously ACCEPTED
// this document while the backend rejected it at run creation.
func TestRunValidate_RejectsDanglingFromStage(t *testing.T) {
	const dangling = `version: "2"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        inputs:
          - artifact: plan
            from_stage: nope
        produces:
          - artifact: pull_request
`
	path := writeTempSpec(t, dangling)
	var stdout, stderr strings.Builder

	got := runValidate([]string{path}, &stdout, &stderr)
	if got != exitFailure {
		t.Fatalf("exit = %d, want exitFailure:\nstdout: %s\nstderr: %s", got, stdout.String(), stderr.String())
	}
	errOut := stderr.String()
	if !strings.Contains(errOut, path+"/workflows/feature_change/stages/0/inputs/0/from_stage") {
		t.Errorf("stderr missing path-prefixed JSON pointer: %q", errOut)
	}
	if !strings.Contains(errOut, `from_stage "nope" does not match any stage id in workflow "feature_change"`) {
		t.Errorf("stderr missing the shipped from_stage message: %q", errOut)
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

// --- Static charter check (E54.11 / #2801) ---

// groomingSpecForValidate is a schema-valid v2 spec whose plan stage produces a
// grooming_report, so runValidate reaches the charter check. The workflow key is
// NOT `backlog_grooming`: the discriminator is structural.
const groomingSpecForValidate = `version: "2"
workflows:
  tidy_the_backlog:
    stages:
      - id: groom
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: grooming_report
            schema: grooming_report_v1
      - id: apply
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`

// nonGroomingSpecForValidate is an ordinary code-change spec — no grooming_report.
const nonGroomingSpecForValidate = `version: "2"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`

// writeSpecAndConventions writes spec + (when conventions != "") a sibling
// work-management.yaml into a fresh temp dir, returning the spec path.
func writeSpecAndConventions(t *testing.T, spec, conventions string) string {
	t.Helper()
	dir := t.TempDir()
	specPath := filepath.Join(dir, "workflows.yaml")
	if err := os.WriteFile(specPath, []byte(spec), 0o600); err != nil {
		t.Fatal(err)
	}
	if conventions != "" {
		if err := os.WriteFile(filepath.Join(dir, "work-management.yaml"), []byte(conventions), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return specPath
}

// TestRunValidate_Charter_GroomingNoCharter_Refuses is the DONE-MEANS end-to-end
// case: a grooming spec with a sibling conventions file that declares no charter
// exits 1 with the EXACT refusal bytes on stderr. Counterfactual (1): delete the
// charter_absent branch of CharterAdmissionReason and this goes green.
func TestRunValidate_Charter_GroomingNoCharter_Refuses(t *testing.T) {
	path := writeSpecAndConventions(t, groomingSpecForValidate, validConventions) // validConventions declares no charter
	var stdout, stderr strings.Builder

	got := runValidate([]string{path}, &stdout, &stderr)
	if got != exitFailure {
		t.Fatalf("exit = %d, want exitFailure:\nstdout: %s\nstderr: %s", got, stdout.String(), stderr.String())
	}
	want := path + ": " + spec.CharterRefusalMessage(path, "tidy_the_backlog", spec.ReasonCharterAbsent) + "\n"
	if stderr.String() != want {
		t.Errorf("stderr =\n%q\nwant\n%q", stderr.String(), want)
	}
	if stdout.String() != "" {
		t.Errorf("stdout = %q, want empty on a refusal", stdout.String())
	}
}

// TestRunValidate_Charter_GroomingWithCharter_OK: a grooming spec plus a
// conventions file declaring a charter exits 0 with the unchanged OK output.
func TestRunValidate_Charter_GroomingWithCharter_OK(t *testing.T) {
	path := writeSpecAndConventions(t, groomingSpecForValidate, validConventions+"charter:\n  path: docs/charter.md\n")
	var stdout, stderr strings.Builder

	got := runValidate([]string{path}, &stdout, &stderr)
	if got != exitOK {
		t.Fatalf("exit = %d, want exitOK:\nstderr: %s", got, stderr.String())
	}
	if !strings.Contains(stdout.String(), path+": OK\n") {
		t.Errorf("stdout missing the unchanged OK line: %q", stdout.String())
	}
}

// TestRunValidate_Charter_GroomingNoConventionsFile_OK: a grooming spec with NO
// sibling conventions file at all exits 0 — the shipped default declares a
// charter (the analogue of the server loader's ErrNotFound fallback). This is
// the path this repository's own spec exercises.
func TestRunValidate_Charter_GroomingNoConventionsFile_OK(t *testing.T) {
	path := writeSpecAndConventions(t, groomingSpecForValidate, "")
	var stdout, stderr strings.Builder

	got := runValidate([]string{path}, &stdout, &stderr)
	if got != exitOK {
		t.Fatalf("exit = %d, want exitOK (absent conventions -> shipped default declares a charter):\nstderr: %s", got, stderr.String())
	}
	if !strings.Contains(stdout.String(), path+": OK\n") {
		t.Errorf("stdout missing the unchanged OK line: %q", stdout.String())
	}
}

// TestRunValidate_Charter_UnparseableConventions_FailsClosed: a grooming spec
// with unparseable conventions exits 1 AND the message carries the
// conventions-unreadable suffix — AC4's distinguishability from a genuinely
// absent charter. Counterfactual (2): make loadCharterDeclaration return
// declared=true on the unparseable branch and this goes green.
func TestRunValidate_Charter_UnparseableConventions_FailsClosed(t *testing.T) {
	path := writeSpecAndConventions(t, groomingSpecForValidate, "::: not yaml :::\n")
	var stdout, stderr strings.Builder

	got := runValidate([]string{path}, &stdout, &stderr)
	if got != exitFailure {
		t.Fatalf("exit = %d, want exitFailure:\nstderr: %s", got, stderr.String())
	}
	if !strings.Contains(stderr.String(), spec.MsgCharterConventionsUnreadableSuffix) {
		t.Errorf("stderr missing the conventions-unreadable suffix (AC4 distinguishability): %q", stderr.String())
	}
}

// TestRunValidate_Charter_EmptyCharterPath_Refuses: a whitespace-only charter
// path is schema-valid but trims to empty -> charter_path_empty. Same base
// message (no suffix), distinct reason from charter_absent.
func TestRunValidate_Charter_EmptyCharterPath_Refuses(t *testing.T) {
	path := writeSpecAndConventions(t, groomingSpecForValidate, validConventions+"charter:\n  path: \"   \"\n")
	var stdout, stderr strings.Builder

	got := runValidate([]string{path}, &stdout, &stderr)
	if got != exitFailure {
		t.Fatalf("exit = %d, want exitFailure:\nstderr: %s", got, stderr.String())
	}
	want := path + ": " + spec.CharterRefusalMessage(path, "tidy_the_backlog", spec.ReasonCharterPathEmpty) + "\n"
	if stderr.String() != want {
		t.Errorf("stderr =\n%q\nwant\n%q", stderr.String(), want)
	}
	// The path_empty message must NOT carry the conventions-unreadable suffix.
	if strings.Contains(stderr.String(), spec.MsgCharterConventionsUnreadableSuffix) {
		t.Errorf("charter_path_empty message must not carry the unreadable suffix: %q", stderr.String())
	}
}

// TestRunValidate_Charter_NonGroomingNoCharter_OK is AC3 and counterfactual (4):
// a NON-grooming spec with a sibling conventions file declaring NO charter exits
// 0 with byte-unchanged stdout. The conventions file is present-without-charter
// precisely so that deleting the empty-list early return in runValidate (which
// would then run the charter check and refuse) reddens THIS case.
func TestRunValidate_Charter_NonGroomingNoCharter_OK(t *testing.T) {
	path := writeSpecAndConventions(t, nonGroomingSpecForValidate, validConventions)
	var stdout, stderr strings.Builder

	got := runValidate([]string{path}, &stdout, &stderr)
	if got != exitOK {
		t.Fatalf("exit = %d, want exitOK (the charter rule governs grooming workflows only):\nstderr: %s", got, stderr.String())
	}
	wantOut := path + ": OK\n" + validateResidualLine + "\n"
	if stdout.String() != wantOut {
		t.Errorf("stdout =\n%q\nwant byte-unchanged\n%q", stdout.String(), wantOut)
	}
	if stderr.String() != "" {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}

// TestRunValidate_Charter_SchemaErrorBeatsCharter: a schema-invalid grooming
// spec reports its SCHEMA error and never the charter message — the charter
// check runs only after ValidateBytes succeeds (ordering).
func TestRunValidate_Charter_SchemaErrorBeatsCharter(t *testing.T) {
	// Break the schema: a stage with no type.
	const bad = `version: "2"
workflows:
  tidy_the_backlog:
    stages:
      - id: groom
        executor:
          agent: claude-code
        produces:
          - artifact: grooming_report
            schema: grooming_report_v1
`
	path := writeSpecAndConventions(t, bad, validConventions) // no charter, but schema fails first
	var stdout, stderr strings.Builder

	got := runValidate([]string{path}, &stdout, &stderr)
	if got != exitFailure {
		t.Fatalf("exit = %d, want exitFailure:\nstderr: %s", got, stderr.String())
	}
	if strings.Contains(stderr.String(), "no backlog charter is declared") {
		t.Errorf("stderr carries the charter message, but the schema error must win: %q", stderr.String())
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
