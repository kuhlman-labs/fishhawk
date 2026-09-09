package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// The four VERBATIM golangci-lint v2.13.2 renderings captured while planning,
// against this repo's .golangci.yml on a go.work workspace fixture. Seeded as
// data so every assertion below lands on the classifier, never on setup.
const (
	// gofmt-only, one module.
	gofmtOnlyLintOutput = `backend/internal/pkg/pkg.go:5:1: File is not properly formatted (gofmt)
"fmt"
^
1 issues:
* gofmt: 1`

	// gofmt AND goimports, one module.
	gofmtGoimportsLintOutput = `backend/internal/pkg/pkg.go:5:1: File is not properly formatted (gofmt)
"fmt"
^
backend/internal/other/other.go:6:1: File is not properly formatted (goimports)
	"github.com/kuhlman-labs/fishhawk/backend/internal/dep"
^
2 issues:
* gofmt: 1
* goimports: 1`

	// The same run with one revive finding added.
	gofmtGoimportsReviveLintOutput = `backend/internal/pkg/pkg.go:5:1: File is not properly formatted (gofmt)
"fmt"
^
backend/internal/other/other.go:6:1: File is not properly formatted (goimports)
	"github.com/kuhlman-labs/fishhawk/backend/internal/dep"
^
backend/internal/dep/dep.go:7:1: exported: exported function Undocumented should have comment or be unexported (revive)
func Undocumented() {}
^
3 issues:
* gofmt: 1
* goimports: 1
* revive: 1`

	// scripts/test lint iterates modules, so two blocks can appear: the first
	// format-only, the second carrying a revive finding.
	twoModuleMixedLintOutput = `==> backend
backend/internal/pkg/pkg.go:5:1: File is not properly formatted (gofmt)
"fmt"
^
1 issues:
* gofmt: 1
==> runner
runner/internal/dep/dep.go:7:1: exported: exported function Undocumented should have comment or be unexported (revive)
func Undocumented() {}
^
1 issues:
* revive: 1`
)

// TestIsFormatOnlyLintFailure pins the classifier over the verbatim v2.13.2
// summary-block renderings plus each fail-closed shape.
func TestIsFormatOnlyLintFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output string
		want   bool
	}{
		{"gofmt only (verbatim v2.13.2)", gofmtOnlyLintOutput, true},
		{"gofmt + goimports (verbatim v2.13.2)", gofmtGoimportsLintOutput, true},
		{"gofmt + goimports + revive (verbatim v2.13.2)", gofmtGoimportsReviveLintOutput, false},
		{"two modules, second carries revive", twoModuleMixedLintOutput, false},
		{"two modules, both format-only", `==> backend
1 issues:
* gofmt: 1
==> runner
1 issues:
* goimports: 1`, true},
		{"no summary block at all (plain go test failure)", ordinaryFailOutput, false},
		{"clean run prints `0 issues.` with a period, not a block", "0 issues.\n", false},
		{"bullets do not sum to the declared total", `3 issues:
* gofmt: 1
* goimports: 1`, false},
		{"header with no bullets (truncated block)", "2 issues:\n", false},
		{"unknown linter bullet", `1 issues:
* revive: 1`, false},
		{"format bullet plus an unknown linter that sums correctly", `2 issues:
* gofmt: 1
* staticcheck: 1`, false},
		{"empty output", "", false},
		{"bullet before any header is ignored, leaving no block", "* gofmt: 1\n", false},
		{"block closed by an interposed line then a stray bullet", `2 issues:
* gofmt: 1
some other line
* goimports: 1`, false},
		{"CRLF line endings", "1 issues:\r\n* gofmt: 1\r\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isFormatOnlyLintFailure(tc.output); got != tc.want {
				t.Errorf("isFormatOnlyLintFailure = %t, want %t\n--- output ---\n%s", got, tc.want, tc.output)
			}
		})
	}
}

// mustSymlink creates a symlink or SKIPS the test — os.Symlink can fail on
// filesystems and platforms without symlink support, and a silent pass would
// be worse than an explicit skip.
func mustSymlink(t *testing.T, oldname, newname string) {
	t.Helper()
	if err := os.Symlink(oldname, newname); err != nil {
		t.Skipf("os.Symlink(%s, %s): %v (filesystem without symlink support)", oldname, newname, err)
	}
}

// TestEligibleAutoformatFiles drives the filter over REAL filesystem fixtures
// — every entry, including every symlink, is created on disk, never simulated.
func TestEligibleAutoformatFiles(t *testing.T) {
	repo := t.TempDir()
	outside := t.TempDir()

	mustWrite(t, filepath.Join(repo, "ok.go"), "package p\n")
	for _, d := range []string{"nested/deep", "unscoped"} {
		if err := os.MkdirAll(filepath.Join(repo, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(t, filepath.Join(repo, "nested", "deep", "ok2.go"), "package q\n")
	mustWrite(t, filepath.Join(repo, "notgo.txt"), "text\n")
	if err := os.Mkdir(filepath.Join(repo, "dir.go"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Symlink targets, whose bytes each case asserts are untouched.
	const outsideBody = "package outside\n"
	const insideBody = "package inside\n"
	outsideTarget := filepath.Join(outside, "target.go")
	mustWrite(t, outsideTarget, outsideBody)
	insideTarget := filepath.Join(repo, "unscoped", "target.go")
	mustWrite(t, insideTarget, insideBody)

	// (i) an in-scope .go path that IS a symlink to a file OUTSIDE repoDir.
	mustSymlink(t, outsideTarget, filepath.Join(repo, "link-outside.go"))
	// (ii) an in-scope .go path that IS a symlink to a file INSIDE repoDir but
	// outside the scope set.
	mustSymlink(t, insideTarget, filepath.Join(repo, "link-inside.go"))
	// (iii) an in-scope .go path sitting under a SYMLINKED PARENT directory.
	mustSymlink(t, outside, filepath.Join(repo, "linkdir"))
	mustWrite(t, filepath.Join(outside, "under.go"), outsideBody)

	scope := []upload.ScopeFile{
		{Path: "ok.go", Operation: "modify"},
		{Path: "nested/deep/ok2.go", Operation: "modify"},
		{Path: "notgo.txt", Operation: "modify"},
		{Path: "dir.go", Operation: "modify"},
		{Path: "deleted.go", Operation: "modify"},
		{Path: "../escape.go", Operation: "modify"},
		{Path: "/abs/escape.go", Operation: "modify"},
		{Path: "nested/../../escape2.go", Operation: "modify"},
		{Path: "link-outside.go", Operation: "modify"},
		{Path: "link-inside.go", Operation: "modify"},
		{Path: "linkdir/under.go", Operation: "modify"},
	}

	got := eligibleAutoformatFiles(repo, scope)
	want := []string{"ok.go", filepath.Join("nested", "deep", "ok2.go")}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("eligibleAutoformatFiles = %v, want %v", got, want)
	}

	// Containment, not merely list membership: every symlink target's bytes
	// must be exactly what was written.
	for _, tc := range []struct{ path, want string }{
		{outsideTarget, outsideBody},
		{insideTarget, insideBody},
		{filepath.Join(outside, "under.go"), outsideBody},
	} {
		b, err := os.ReadFile(tc.path)
		if err != nil {
			t.Fatalf("read %s: %v", tc.path, err)
		}
		if string(b) != tc.want {
			t.Errorf("symlink target %s changed during the eligibility pass: %q, want %q", tc.path, b, tc.want)
		}
	}
}

// stubFormatter installs a stub autoformatBinary for the duration of the test
// and returns a func reporting how many times it was invoked. body is the
// shell body appended after the shebang; it receives the formatter's argv.
func stubFormatter(t *testing.T, body string) func() int {
	t.Helper()
	dir := t.TempDir()
	counter := filepath.Join(dir, "invocations")
	script := filepath.Join(dir, "stub-formatter")
	mustWrite(t, script, "#!/bin/sh\necho invoked >> "+counter+"\n"+body+"\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	prev := autoformatBinary
	autoformatBinary = script
	t.Cleanup(func() { autoformatBinary = prev })
	return func() int {
		b, err := os.ReadFile(counter)
		if err != nil {
			return 0
		}
		return strings.Count(string(b), "invoked\n")
	}
}

func TestRunAutoformat(t *testing.T) {
	t.Run("rewriting formatter reports the file changed", func(t *testing.T) {
		repo := t.TempDir()
		mustWrite(t, filepath.Join(repo, "a.go"), "unformatted\n")
		mustWrite(t, filepath.Join(repo, "b.go"), "untouched\n")
		count := stubFormatter(t, `printf 'formatted\n' > "$2"; exit 0`)
		changed, err := runAutoformat(context.Background(), repo, []string{"a.go", "b.go"}, time.Minute)
		if err != nil {
			t.Fatalf("runAutoformat: %v", err)
		}
		if len(changed) != 1 || changed[0] != "a.go" {
			t.Errorf("changed = %v, want [a.go]", changed)
		}
		if n := count(); n != 1 {
			t.Errorf("formatter invocations = %d, want 1", n)
		}
	})

	t.Run("byte-identical result reports nothing changed", func(t *testing.T) {
		repo := t.TempDir()
		mustWrite(t, filepath.Join(repo, "a.go"), "already formatted\n")
		stubFormatter(t, `exit 0`)
		changed, err := runAutoformat(context.Background(), repo, []string{"a.go"}, time.Minute)
		if err != nil {
			t.Fatalf("runAutoformat: %v", err)
		}
		if len(changed) != 0 {
			t.Errorf("changed = %v, want empty", changed)
		}
	})

	t.Run("non-zero exit returns an error and an empty changed set", func(t *testing.T) {
		repo := t.TempDir()
		mustWrite(t, filepath.Join(repo, "a.go"), "x\n")
		stubFormatter(t, `printf 'boom\n' >&2; exit 3`)
		changed, err := runAutoformat(context.Background(), repo, []string{"a.go"}, time.Minute)
		if err == nil {
			t.Fatal("runAutoformat = nil error, want a non-zero-exit error")
		}
		if len(changed) != 0 {
			t.Errorf("changed = %v, want empty on error", changed)
		}
	})

	t.Run("missing binary returns an error and an empty changed set", func(t *testing.T) {
		repo := t.TempDir()
		mustWrite(t, filepath.Join(repo, "a.go"), "x\n")
		prev := autoformatBinary
		autoformatBinary = filepath.Join(t.TempDir(), "definitely-not-installed")
		t.Cleanup(func() { autoformatBinary = prev })
		changed, err := runAutoformat(context.Background(), repo, []string{"a.go"}, time.Minute)
		if err == nil {
			t.Fatal("runAutoformat = nil error, want a missing-binary error")
		}
		if len(changed) != 0 {
			t.Errorf("changed = %v, want empty on a missing binary", changed)
		}
	})

	t.Run("empty file list never invokes the formatter", func(t *testing.T) {
		count := stubFormatter(t, `exit 0`)
		changed, err := runAutoformat(context.Background(), t.TempDir(), nil, time.Minute)
		if err != nil || len(changed) != 0 {
			t.Fatalf("runAutoformat(nil) = %v, %v; want nil, nil", changed, err)
		}
		if n := count(); n != 0 {
			t.Errorf("formatter invocations = %d, want 0", n)
		}
	})
}
