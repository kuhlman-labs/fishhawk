package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

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

// ---------------------------------------------------------------------------
// --emit-resolved (E52.22 / #2351)
// ---------------------------------------------------------------------------

const (
	// emitFixtureRelPath / emitGoldenRelPath are the SHIPPED cross-validator
	// reuse fixture and its generated golden, read over the module wall from
	// the same paths cli/internal/spec/v2reuse_test.go reads. cli/cmd/fishhawk
	// sits at the same depth from the repo root as cli/internal/spec.
	emitFixtureRelPath = "../../../docs/spec/examples/workflow-v2-reuse.yaml"
	emitGoldenRelPath  = "../../../docs/spec/examples/workflow-v2-reuse.resolved.json"
)

// emitResolved drives the real runValidate under --emit-resolved with
// separated stdout/stderr and returns all three observables.
func emitResolved(t *testing.T, path string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb strings.Builder
	code = runValidate([]string{"--emit-resolved", path}, &out, &errb)
	return code, out.String(), errb.String()
}

// canonicalizeYAML decodes YAML into `any` and re-encodes it as canonical
// JSON, so two documents can be compared for SEMANTIC equality regardless of
// key order, comments or anchors (none of which ResolveReuse preserves).
func canonicalizeYAML(t *testing.T, data []byte) string {
	t.Helper()
	var raw any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		t.Fatalf("decode YAML: %v", err)
	}
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal canonical: %v", err)
	}
	return string(b)
}

// TestValidateEmitResolved_MatchesSharedGoldenResolvedForm is the DONE-MEANS
// test. It drives the SHIPPED cross-validator fixture through the real CLI
// surface and asserts the emitted bytes canonicalize equal to the generated
// golden resolved document, AND that every stage in the emitted document
// declares an `executor` while stages in the SOURCE do not — the observable
// property that makes a bare non-resolving schema reader accept the output.
func TestValidateEmitResolved_MatchesSharedGoldenResolvedForm(t *testing.T) {
	code, stdout, stderr := emitResolved(t, emitFixtureRelPath)
	if code != exitOK {
		t.Fatalf("exit = %d, want 0\nstderr: %s", code, stderr)
	}

	goldenBytes, err := os.ReadFile(emitGoldenRelPath)
	if err != nil {
		t.Fatalf("read %s: %v", emitGoldenRelPath, err)
	}
	var golden map[string]any
	if err := json.Unmarshal(goldenBytes, &golden); err != nil {
		t.Fatalf("decode %s: %v", emitGoldenRelPath, err)
	}
	// $comment is the golden's file header (JSON has no comments) and is the
	// one key deleted before comparing — cli/internal/spec's twin does the same.
	delete(golden, "$comment")
	want, err := json.Marshal(golden)
	if err != nil {
		t.Fatalf("re-marshal golden: %v", err)
	}
	if got := canonicalizeYAML(t, []byte(stdout)); got != string(want) {
		t.Errorf("emitted document diverged from the golden resolved form\n got: %s\nwant: %s", got, want)
	}

	// The property a bare check-jsonschema run depends on: every RESOLVED
	// stage declares an executor, while the SOURCE has stages that do not.
	srcBytes, err := os.ReadFile(emitFixtureRelPath)
	if err != nil {
		t.Fatalf("read %s: %v", emitFixtureRelPath, err)
	}
	srcMissing := stagesMissingExecutor(t, srcBytes)
	if srcMissing == 0 {
		t.Fatalf("fixture %s no longer has any stage inheriting its executor; the test asserts nothing", emitFixtureRelPath)
	}
	if got := stagesMissingExecutor(t, []byte(stdout)); got != 0 {
		t.Errorf("%d resolved stage(s) declare no executor; a bare schema run would reject the emitted document", got)
	}
}

// stagesMissingExecutor counts the stages in a workflow document that declare
// no `executor` key.
func stagesMissingExecutor(t *testing.T, data []byte) int {
	t.Helper()
	var doc struct {
		Workflows map[string]struct {
			Stages []map[string]any `yaml:"stages"`
		} `yaml:"workflows"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decode workflows: %v", err)
	}
	n := 0
	for _, wf := range doc.Workflows {
		for _, st := range wf.Stages {
			if _, ok := st["executor"]; !ok {
				n++
			}
		}
	}
	return n
}

// TestValidateEmitResolved_StdoutCarriesOnlyResolvedYAML pins stdout purity:
// neither the `<path>: OK` line nor the residual-scope line may reach stdout,
// and the loss warning must land on STDERR so the stream pipes cleanly.
func TestValidateEmitResolved_StdoutCarriesOnlyResolvedYAML(t *testing.T) {
	code, stdout, stderr := emitResolved(t, emitFixtureRelPath)
	if code != exitOK {
		t.Fatalf("exit = %d, want 0\nstderr: %s", code, stderr)
	}
	if strings.Contains(stdout, ": OK") {
		t.Errorf("stdout carries the OK line under --emit-resolved:\n%s", stdout)
	}
	if strings.Contains(stdout, validateResidualLine) {
		t.Errorf("stdout carries the residual-scope line under --emit-resolved:\n%s", stdout)
	}
	if strings.Contains(stdout, emitResolvedLossWarning) {
		t.Errorf("the loss warning landed on stdout, which breaks a byte comparison downstream:\n%s", stdout)
	}
	if !strings.Contains(stderr, emitResolvedLossWarning) {
		t.Errorf("stderr is missing the loss warning: %q", stderr)
	}
	// Whatever else is true, stdout must be a decodable workflow document.
	var raw map[string]any
	if err := yaml.Unmarshal([]byte(stdout), &raw); err != nil {
		t.Fatalf("stdout is not decodable YAML: %v\n%s", err, stdout)
	}
	if _, ok := raw["workflows"]; !ok {
		t.Errorf("stdout decoded without a `workflows` key: %v", raw)
	}
}

// emitSchemaInvalidButResolvable is a v2 document that RESOLVES cleanly (no
// removed form, no reuse error, no duplicate stage id) and fails ONLY the
// schema phase: the `apply` stage omits the required `type` scalar.
const emitSchemaInvalidButResolvable = `
version: "2"
defaults:
  executor:
    agent: claude-code
workflows:
  feature_change:
    description: "Resolvable, schema-invalid."
    stages:
      - id: apply
`

// TestValidateEmitResolved_SchemaInvalidButResolvableStillEmits pins that
// emit mode resolves WITHOUT validating. The CONTROL asserts the paired claim
// with error IDENTITY: plain `validate` on the SAME bytes exits 1 with a
// SCHEMA error (the missing required `type` reported at the stage's pointer),
// not a resolution error — without that, a fixture failing for the wrong
// reason would look like it exercised the branch.
func TestValidateEmitResolved_SchemaInvalidButResolvableStillEmits(t *testing.T) {
	path := writeTempSpec(t, emitSchemaInvalidButResolvable)

	code, stdout, stderr := emitResolved(t, path)
	if code != exitOK {
		t.Fatalf("emit exit = %d, want 0 — the document resolves, so emit must not validate it\nstderr: %s", code, stderr)
	}
	if strings.TrimSpace(stdout) == "" {
		t.Fatalf("emit wrote nothing to stdout")
	}

	// CONTROL: the same bytes without the flag must be REFUSED, and refused
	// by the SCHEMA, not by resolution.
	var cout, cerr strings.Builder
	if got := runValidate([]string{path}, &cout, &cerr); got != exitFailure {
		t.Fatalf("plain validate exit = %d, want 1 — the fixture must be schema-invalid\nstderr: %s", got, cerr.String())
	}
	control := cerr.String()
	if !strings.Contains(control, "/workflows/feature_change/stages/0") {
		t.Errorf("control error is not anchored at the offending stage (so it may not be a schema failure): %q", control)
	}
	if !strings.Contains(control, "type") {
		t.Errorf("control error does not name the missing required `type` property: %q", control)
	}
	// Error IDENTITY: a resolution failure would name a reuse/version/duplicate
	// condition. None of those may appear.
	for _, resolutionWord := range []string{"extends", "unsupported", "duplicate"} {
		if strings.Contains(control, resolutionWord) {
			t.Errorf("control error mentions %q — the fixture failed RESOLUTION, not the schema: %q", resolutionWord, control)
		}
	}
}

// emitGroomingNoCharter declares a workflow producing a grooming_report, so
// the E54.11 static charter check applies to it.
const emitGroomingNoCharter = `
version: "2"
workflows:
  backlog_grooming:
    description: "Grooming."
    stages:
      - id: groom
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: grooming_report
`

// TestValidateEmitResolved_SkipsCharterCheck pins that emit mode does not run
// the E54.11 static mandatory-charter check: it reads a sibling file from the
// working tree and is unrelated to resolution. The CONTROL is plain validate
// on the SAME fixture, which must refuse.
func TestValidateEmitResolved_SkipsCharterCheck(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "workflows.yaml")
	if err := os.WriteFile(path, []byte(emitGroomingNoCharter), 0o600); err != nil {
		t.Fatal(err)
	}
	// A sibling conventions file that exists but declares NO charter — the
	// charter_absent refusal reason, seeded BY CONSTRUCTION.
	sibling := filepath.Join(dir, "work-management.yaml")
	if err := os.WriteFile(sibling, []byte("version: \"0\"\nprovider: github\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := emitResolved(t, path)
	if code != exitOK {
		t.Fatalf("emit exit = %d, want 0 — the charter check must not run on the emit path\nstderr: %s", code, stderr)
	}
	if strings.TrimSpace(stdout) == "" {
		t.Fatalf("emit wrote nothing to stdout")
	}

	var cout, cerr strings.Builder
	if got := runValidate([]string{path}, &cout, &cerr); got != exitFailure {
		t.Fatalf("plain validate exit = %d, want 1 — the fixture must trip the charter rule\nstderr: %s", got, cerr.String())
	}
	if !strings.Contains(cerr.String(), "charter") {
		t.Errorf("control refusal does not name the charter rule, so the fixture failed for another reason: %q", cerr.String())
	}
}

// TestValidateEmitResolved_FailureLeavesStdoutEmpty is the table over EVERY
// input class ResolveReuse refuses (they share one error path, so each is a
// two-line fixture): an unsupported version major, a dotted v2 minor, an
// `extends` naming an absent workflow, an `extends` cycle, a duplicate stage
// id, a removed v2 form, and malformed / empty YAML. Each must exit 1 with the
// error on STDERR and stdout EMPTY.
func TestValidateEmitResolved_FailureLeavesStdoutEmpty(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want string // a substring the stderr line must carry
	}{
		{
			name: "unsupported major",
			doc: `version: "9"
workflows: {}`,
			want: "version",
		},
		{
			name: "dotted v2 minor",
			doc: `version: "2.1"
workflows: {}`,
			want: "version",
		},
		{
			name: "extends names an absent workflow",
			doc: `version: "2"
workflows:
  a:
    extends: nope
    stages:
      - id: s
        type: plan
        executor:
          agent: claude-code`,
			want: "extends",
		},
		{
			name: "extends cycle",
			doc: `version: "2"
workflows:
  a:
    extends: b
    stages: []
  b:
    extends: a
    stages: []`,
			want: "extends",
		},
		{
			name: "duplicate stage id",
			doc: `version: "2"
workflows:
  a:
    stages:
      - id: s
        type: plan
        executor:
          agent: claude-code
      - id: s
        type: implement
        executor:
          agent: claude-code`,
			want: "duplicate stage id",
		},
		{
			name: "removed v2 form (top-level roles)",
			doc: `version: "2"
roles:
  tech_lead:
    members: ["@org/tech-leads"]
workflows:
  a:
    stages:
      - id: s
        type: plan
        executor:
          agent: claude-code`,
			want: "roles",
		},
		{
			// The want is the DECODE-phase fragment, not a bare ":" that every
			// `<path>: <msg>` line satisfies: only a *spec.ParseError carries
			// the yaml decoder's own text, so this row proves the refusal came
			// from the decode phase rather than from any other path.
			name: "malformed YAML",
			doc:  "version: \"2\"\n\tworkflows: [",
			want: "yaml: line 2:",
		},
		{
			// Likewise: "empty document" is spec.ParseError's exact message for
			// an empty input (spec.go's decodeAndResolve), not a generic line
			// shape.
			name: "empty document",
			doc:  "",
			want: "empty document",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTempSpec(t, tc.doc)
			code, stdout, stderr := emitResolved(t, path)
			if code != exitFailure {
				t.Fatalf("exit = %d, want 1\nstdout: %q\nstderr: %s", code, stdout, stderr)
			}
			if stdout != "" {
				t.Errorf("stdout must be EMPTY on a failure, got %q", stdout)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr %q does not carry %q", stderr, tc.want)
			}
			if strings.Contains(stderr, emitResolvedLossWarning) {
				t.Errorf("the loss warning was printed on a failure path: %q", stderr)
			}
		})
	}
}

// errAfterN is an io.Writer that accepts at most n bytes in total and then
// fails, modelling the two ways a real stdout write can go wrong: a consumer
// that closed the pipe early (n == 0, an immediate error) and a redirect to a
// full disk (n > 0, a short accepted prefix then an error). It records
// everything it accepted so the test can assert the truncation it produced.
type errAfterN struct {
	n        int   // bytes still acceptable
	err      error // returned once the budget is exhausted
	shortNil bool  // return (n < len(p), nil) instead — a non-conforming writer
	accepted strings.Builder
}

func (w *errAfterN) Write(p []byte) (int, error) {
	take := len(p)
	if take > w.n {
		take = w.n
	}
	w.accepted.Write(p[:take])
	w.n -= take
	if take == len(p) {
		return take, nil
	}
	if w.shortNil {
		// Deliberately violates the io.Writer contract (a short write with a
		// nil error). The CLI must still refuse rather than trust the count.
		return take, nil
	}
	return take, w.err
}

// TestValidateEmitResolved_StdoutWriteFailureIsNotSuccess pins that a failed or
// short stdout write is REPORTED, not swallowed: emit must exit non-zero and
// name the failure on stderr rather than handing a consumer a truncated
// document under exit 0. The rows cover an immediate failure (a closed pipe),
// a partial-then-failing write (a redirect to a full disk), and the
// contract-violating short write with a nil error.
func TestValidateEmitResolved_StdoutWriteFailureIsNotSuccess(t *testing.T) {
	// Sanity anchor: the same input on a healthy writer succeeds, so a RED row
	// below is the writer's doing and not a broken fixture.
	if code, _, stderr := emitResolved(t, emitFixtureRelPath); code != exitOK {
		t.Fatalf("control: healthy-writer emit exit = %d, want 0\nstderr: %s", code, stderr)
	}

	sentinel := errors.New("boom: downstream consumer went away")
	cases := []struct {
		name       string
		w          *errAfterN
		wantErrTxt string
	}{
		{
			name:       "closed pipe (immediate failure)",
			w:          &errAfterN{n: 0, err: sentinel},
			wantErrTxt: sentinel.Error(),
		},
		{
			name:       "disk full (partial write then failure)",
			w:          &errAfterN{n: 16, err: sentinel},
			wantErrTxt: sentinel.Error(),
		},
		{
			name:       "short write with a nil error",
			w:          &errAfterN{n: 16, shortNil: true},
			wantErrTxt: io.ErrShortWrite.Error(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stderr strings.Builder
			code := runValidate([]string{"--emit-resolved", emitFixtureRelPath}, tc.w, &stderr)
			if code == exitOK {
				t.Fatalf("exit = 0 on a failing stdout write; the consumer got %d truncated byte(s) under a success exit",
					tc.w.accepted.Len())
			}
			if code != exitUsage {
				t.Errorf("exit = %d, want %d (the I/O class the usage banner documents)", code, exitUsage)
			}
			if !strings.Contains(stderr.String(), "writing the resolved document to stdout") {
				t.Errorf("stderr does not name the stdout write failure: %q", stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.wantErrTxt) {
				t.Errorf("stderr %q does not carry the underlying error %q", stderr.String(), tc.wantErrTxt)
			}
		})
	}
}

// TestValidateEmitResolved_MissingFile pins the I/O branch: exit 2 (usage),
// stdout EMPTY.
func TestValidateEmitResolved_MissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.yaml")
	code, stdout, stderr := emitResolved(t, path)
	if code != exitUsage {
		t.Fatalf("exit = %d, want 2\nstderr: %s", code, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout must be EMPTY, got %q", stdout)
	}
}

// TestValidateEmitResolved_TooManyArgs pins that the shared argument handling
// still refuses two positionals under the flag.
func TestValidateEmitResolved_TooManyArgs(t *testing.T) {
	var stdout, stderr strings.Builder
	code := runValidate([]string{"--emit-resolved", "a.yaml", "b.yaml"}, &stdout, &stderr)
	if code != exitUsage {
		t.Fatalf("exit = %d, want 2\nstderr: %s", code, stderr.String())
	}
	if stdout.String() != "" {
		t.Errorf("stdout must be EMPTY, got %q", stdout.String())
	}
}

// TestValidateEmitResolved_LowerMajorPassthrough pins ResolveReuse's
// routed-major-below-2 contract at the CLI surface: a v0 document carries no
// reuse primitives and comes back SEMANTICALLY unchanged. The comparison is
// on decoded documents, not bytes — the re-marshal drops comments and reorders
// keys, which is correct behavior, not a defect.
func TestValidateEmitResolved_LowerMajorPassthrough(t *testing.T) {
	path := writeTempSpec(t, validateValidYAML)
	code, stdout, stderr := emitResolved(t, path)
	if code != exitOK {
		t.Fatalf("exit = %d, want 0\nstderr: %s", code, stderr)
	}
	if got, want := canonicalizeYAML(t, []byte(stdout)), canonicalizeYAML(t, []byte(validateValidYAML)); got != want {
		t.Errorf("v0 document was not passed through semantically unchanged\n got: %s\nwant: %s", got, want)
	}
}

// TestValidate_NoFlagOutputUnchanged pins that the default path is unaffected:
// with the flag absent, `validate` still prints the `<path>: OK` line and the
// residual-scope note to stdout and writes no loss warning.
func TestValidate_NoFlagOutputUnchanged(t *testing.T) {
	path := writeTempSpec(t, validateValidYAML)
	var stdout, stderr strings.Builder
	if got := runValidate([]string{path}, &stdout, &stderr); got != exitOK {
		t.Fatalf("exit = %d, want 0\nstderr: %s", got, stderr.String())
	}
	if !strings.Contains(stdout.String(), path+": OK\n") {
		t.Errorf("stdout lost the OK line: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), validateResidualLine) {
		t.Errorf("stdout lost the residual-scope line: %q", stdout.String())
	}
	if strings.Contains(stderr.String(), emitResolvedLossWarning) {
		t.Errorf("the emit-mode loss warning leaked onto the default path: %q", stderr.String())
	}
}
