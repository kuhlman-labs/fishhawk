package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/cli/internal/spec"
)

// v1Fixture is a small workflow-v1 spec that migrates cleanly. The CLI
// tests are about the OUTPUT MATRIX, not the translation — the codemod's
// behaviour is pinned in cli/internal/spec.
const v1Fixture = `version: "1.6"
roles:
  founder:
    members: ["@kuhlman-labs"]
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
        gates:
          - type: approval
            approvers:
              any_of: [founder]
`

// writeFixture drops src into a temp dir and returns its path.
func writeFixture(t *testing.T, src string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "workflows.yaml")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func runMigrate(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = runMigrateSpec(args, &out, &errb)
	return code, out.String(), errb.String()
}

// --- Cell (1): no output flag -----------------------------------------

func TestMigrateSpec_NoFlagWritesNothing(t *testing.T) {
	path := writeFixture(t, v1Fixture)
	code, stdout, stderr := runMigrate(t, path)
	if code != exitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "Approval-eligibility report") {
		t.Errorf("stdout must carry the report:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Nothing was written") {
		t.Errorf("stdout must say nothing was written:\n%s", stdout)
	}
	after, _ := os.ReadFile(path) //nolint:gosec // test-controlled path
	if string(after) != v1Fixture {
		t.Error("the source file must be byte-unchanged with no output flag")
	}
	// stdout is reserved for the report: the migrated YAML never lands
	// there, so piping the report can't accidentally produce a spec.
	if strings.Contains(stdout, "auto_advance") || strings.Contains(stdout, "version: \"2\"") {
		t.Errorf("stdout leaked the migrated YAML:\n%s", stdout)
	}
}

// --- Cell (2): --report-only ------------------------------------------

func TestMigrateSpec_ReportOnlyMatchesNoFlag(t *testing.T) {
	path := writeFixture(t, v1Fixture)
	code1, stdout1, _ := runMigrate(t, path)
	code2, stdout2, _ := runMigrate(t, path, "--report-only")
	if code1 != code2 || stdout1 != stdout2 {
		t.Errorf("--report-only must be identical to the no-flag cell:\n(%d)\n%s\n(%d)\n%s", code1, stdout1, code2, stdout2)
	}
	after, _ := os.ReadFile(path) //nolint:gosec // test-controlled path
	if string(after) != v1Fixture {
		t.Error("--report-only must leave the source byte-unchanged")
	}
}

// --- Cell (3): --out --------------------------------------------------

func TestMigrateSpec_OutWritesFile(t *testing.T) {
	path := writeFixture(t, v1Fixture)
	dest := filepath.Join(t.TempDir(), "migrated.yaml")
	code, stdout, stderr := runMigrate(t, path, "--out", dest)
	if code != exitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	got, err := os.ReadFile(dest) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("read migrated file: %v", err)
	}
	if !strings.Contains(string(got), `version: "2"`) {
		t.Errorf("the written file is not a v2 document:\n%s", got)
	}
	if !strings.Contains(stdout, "Approval-eligibility report") {
		t.Errorf("stdout must still carry the report:\n%s", stdout)
	}
	if strings.Contains(stdout, `version: "2"`) {
		t.Errorf("stdout leaked the migrated YAML:\n%s", stdout)
	}
}

func TestMigrateSpec_OutRefusesToClobber(t *testing.T) {
	path := writeFixture(t, v1Fixture)
	dest := filepath.Join(t.TempDir(), "existing.yaml")
	const sentinel = "# do not touch me\n"
	if err := os.WriteFile(dest, []byte(sentinel), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	code, _, stderr := runMigrate(t, path, "--out", dest)
	if code != exitFailure {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "refusing to overwrite") || !strings.Contains(stderr, dest) {
		t.Errorf("stderr must name the path it refused to overwrite:\n%s", stderr)
	}
	got, _ := os.ReadFile(dest) //nolint:gosec // test-controlled path
	if string(got) != sentinel {
		t.Error("the existing file must be left byte-unchanged")
	}
}

// --- Cell (4): --in-place ---------------------------------------------

func TestMigrateSpec_InPlaceRewritesSource(t *testing.T) {
	path := writeFixture(t, v1Fixture)
	code, stdout, stderr := runMigrate(t, path, "--in-place")
	if code != exitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	got, _ := os.ReadFile(path) //nolint:gosec // test-controlled path
	if !strings.Contains(string(got), `version: "2"`) || strings.Contains(string(got), "approvers:") {
		t.Errorf("the source was not migrated in place:\n%s", got)
	}
	if !strings.Contains(stdout, "migrated to workflow-v2 in place") {
		t.Errorf("stdout must confirm the in-place rewrite:\n%s", stdout)
	}
}

// --- Cells (5) and (6): contradictory flags ---------------------------

func TestMigrateSpec_ContradictoryFlags(t *testing.T) {
	path := writeFixture(t, v1Fixture)
	tests := []struct {
		name   string
		args   []string
		wantIn string
	}{
		{"out with in-place", []string{path, "--out", "x.yaml", "--in-place"}, "mutually exclusive"},
		{"report-only with out", []string{path, "--report-only", "--out", "x.yaml"}, "contradicts"},
		{"report-only with in-place", []string{path, "--report-only", "--in-place"}, "contradicts"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, _, stderr := runMigrate(t, tc.args...)
			if code != exitUsage {
				t.Fatalf("exit = %d, want 2", code)
			}
			if !strings.Contains(stderr, tc.wantIn) {
				t.Errorf("stderr must explain the conflict (%q):\n%s", tc.wantIn, stderr)
			}
			if _, err := os.Stat("x.yaml"); err == nil {
				t.Error("a usage error must not write anything")
			}
		})
	}
}

// --- Refusal, no-op and I/O paths -------------------------------------

func TestMigrateSpec_RefusalWritesNothing(t *testing.T) {
	// An undefined role: the R2 refusal.
	src := strings.Replace(v1Fixture, "any_of: [founder]", "any_of: [reviewers]", 1)
	path := writeFixture(t, src)
	dest := filepath.Join(t.TempDir(), "migrated.yaml")
	code, stdout, stderr := runMigrate(t, path, "--out", dest)
	if code != exitFailure {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "refusing to migrate; nothing was written") {
		t.Errorf("stderr must name the refusal:\n%s", stderr)
	}
	if !strings.Contains(stderr, "R2_undefined_role") {
		t.Errorf("stderr must name the refusal BRANCH:\n%s", stderr)
	}
	if !strings.Contains(stdout, "Approval-eligibility report") {
		t.Errorf("the report is emitted on the refusal path too:\n%s", stdout)
	}
	if _, err := os.Stat(dest); err == nil {
		t.Error("a refusal must write no output file")
	}
}

func TestMigrateSpec_AlreadyV2IsExitZero(t *testing.T) {
	path := writeFixture(t, v1Fixture)
	if code, _, stderr := runMigrate(t, path, "--in-place"); code != exitOK {
		t.Fatalf("seed migration failed: %d %s", code, stderr)
	}
	code, stdout, stderr := runMigrate(t, path)
	if code != exitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "already at workflow-v2") {
		t.Errorf("stdout must report the no-op:\n%s", stdout)
	}
}

func TestMigrateSpec_MissingFile(t *testing.T) {
	code, _, stderr := runMigrate(t, filepath.Join(t.TempDir(), "nope.yaml"))
	if code != exitUsage {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr, "nope.yaml") {
		t.Errorf("stderr must name the unreadable path:\n%s", stderr)
	}
}

func TestMigrateSpec_ParseErrorExitsOne(t *testing.T) {
	path := writeFixture(t, "\n\n")
	code, _, stderr := runMigrate(t, path)
	if code != exitFailure {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "empty document") {
		t.Errorf("stderr must name the parse failure:\n%s", stderr)
	}
}

func TestMigrateSpec_TooManyArgs(t *testing.T) {
	code, _, stderr := runMigrate(t, "a.yaml", "b.yaml")
	if code != exitUsage {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr, "at most one path argument") {
		t.Errorf("stderr must explain the arity:\n%s", stderr)
	}
}

func TestMigrateSpec_HelpCarriesTheMatrix(t *testing.T) {
	_, _, stderr := runMigrate(t, "--help")
	for _, want := range []string{
		"--report-only          print the report, write nothing",
		"--out PATH             write the migrated spec to PATH; refuses to overwrite",
		"--in-place             rewrite the source file",
		"--out with --in-place  usage error",
		"never fabricated",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("--help must state the output matrix (%q):\n%s", want, stderr)
		}
	}
}

// TestMigrateSpec_EndToEndAgainstCommittedGolden is the cross-boundary
// test: library -> CLI -> filesystem -> re-validation. Per-layer units
// each pass while the seam breaks — node edits that are correct in memory
// but serialize wrong. It runs the REAL subcommand over the committed v1
// fixture into a temp dir and compares the WRITTEN BYTES against the
// committed v2 golden.
func TestMigrateSpec_EndToEndAgainstCommittedGolden(t *testing.T) {
	const fixtures = "../../internal/spec/testdata/migrate"
	dest := filepath.Join(t.TempDir(), "migrated.yaml")
	code, _, stderr := runMigrate(t, filepath.Join(fixtures, "fishhawk-own-spec.v1.yaml"), "--out", dest)
	if code != exitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	got, err := os.ReadFile(dest) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	want, err := os.ReadFile(filepath.Join(fixtures, "fishhawk-own-spec.v2.yaml"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("bytes written through the CLI differ from the committed v2 golden:\n%s", got)
	}
	// Re-validate what actually landed on disk through the real gate
	// (removed-form sweep + reuse resolution + schema), not the in-memory
	// value the codemod thought it produced.
	if err := spec.ValidateBytes(got); err != nil {
		t.Fatalf("the bytes written to disk do not validate as workflow-v2: %v", err)
	}
}
