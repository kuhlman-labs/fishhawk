package testanchor

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// pkgSrcDir is this package's SOURCE directory, captured once at package
// initialization (#4179): `go test` runs the test binary from within the
// package's source directory (go help testflag), and package-level variables
// initialize before any test runs. It is the convention this package's guard
// enforces, so the guard's own test anchors on it too.
var pkgSrcDir = mustGetwd()

// mustGetwd returns os.Getwd() and panics on an error, so an unresolvable
// package dir fails the test binary closed at init.
func mustGetwd() string {
	dir, err := os.Getwd()
	if err != nil {
		panic(fmt.Sprintf("pkgSrcDir: os.Getwd: %v", err))
	}
	return dir
}

// writeTree materializes files (slash paths relative to the returned temp
// root) for a seeded scan.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestNoRuntimeCallerFileAnchorsInTests is the repo-wide guard (#4179): no
// `_test.go` file in any go.work module may bind runtime.Caller's file result,
// because under -trimpath that name is module-relative and every fixture path
// derived from it misses. Anchor fixtures on the package's init-captured
// pkgSrcDir instead (see README.md).
func TestNoRuntimeCallerFileAnchorsInTests(t *testing.T) {
	root, err := RepoRoot(pkgSrcDir)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := res.RequireModules("backend", "runner", "cli", "verifier"); err != nil {
		t.Fatal(err)
	}
	// Anti-vacuity for the allow path: the scan must have reached at least one
	// blank-file call (backend/internal/server/approvals_test.go's Caller(1)
	// today), or a guard that never parses a Caller call would pass green.
	if res.AllowedCalls < 1 {
		t.Fatalf("AllowedCalls = %d across %d scanned test files, want >= 1: the scan never reached a blank-file runtime.Caller call", res.AllowedCalls, res.ScannedFiles)
	}
	for _, f := range res.Findings {
		t.Errorf("%s: %s", f.Site, f.Reason)
	}
}

// TestScanFlagsSeededFileAnchors pins the detector and its precision over a
// seeded tree: every file-bound form the guard handles is flagged, the
// blank-file Caller(1) is allowed, a non-test file and a testdata/ file are not
// scanned.
func TestScanFlagsSeededFileAnchors(t *testing.T) {
	root := writeTree(t, map[string]string{
		"go.work":  "go 1.25.0\n\nuse ./m\n",
		"m/go.mod": "module example.com/m\n",
		"m/a_test.go": `package m
import "runtime"
func a() string { _, f, _, _ := runtime.Caller(0); return f }
`,
		"m/b_test.go": `package m
import rt "runtime"
func b() (string, bool) { _, f, _, ok := rt.Caller(1); return f, ok }
`,
		"m/c_test.go": `package m
import "runtime"
func c() (uintptr, bool) { pc, _, _, ok := runtime.Caller(1); return pc, ok }
`,
		"m/d_test.go": `package m
import "runtime"
var _, f, _, _ = runtime.Caller(0)
`,
		"m/e_test.go": `package m
import "runtime"
func e() (uintptr, string, int, bool) { return runtime.Caller(0) }
`,
		"m/g_test.go": `package m
import . "runtime"
func g() string { _, f, _, _ := Caller(0); return f }
`,
		"m/h.go": `package m
import "runtime"
func h() string { _, f, _, _ := runtime.Caller(0); return f }
`,
		"m/testdata/x_test.go": `package x
import "runtime"
func x() string { _, f, _, _ := runtime.Caller(0); return f }
`,
	})
	res, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range res.Findings {
		got = append(got, f.Path)
		if !strings.Contains(f.Reason, "pkgSrcDir") {
			t.Errorf("%s: reason %q does not name the pkgSrcDir fix", f.Site, f.Reason)
		}
	}
	sort.Strings(got)
	want := []string{"m/a_test.go", "m/b_test.go", "m/d_test.go", "m/e_test.go", "m/g_test.go"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("flagged = %v, want %v", got, want)
	}
	if res.AllowedCalls != 1 || len(res.Allowed) != 1 || res.Allowed[0] != (Site{Path: "m/c_test.go", Line: 3}) {
		t.Errorf("allowed = %d %v, want exactly m/c_test.go:3", res.AllowedCalls, res.Allowed)
	}
	if res.ScannedFiles != 6 {
		t.Errorf("ScannedFiles = %d, want 6 (a,b,c,d,e,g; not h.go, not testdata/)", res.ScannedFiles)
	}
}

// TestScanFlagsNonCallReference pins that a method value (`f := runtime.Caller`)
// — a reference outside any call the guard can analyze — is rejected, and that
// a blank `_ "runtime"` import binds nothing.
func TestScanFlagsNonCallReference(t *testing.T) {
	root := writeTree(t, map[string]string{
		"go.work":  "use ./m\n",
		"m/go.mod": "module example.com/m\n",
		"m/a_test.go": `package m
import "runtime"
var caller = runtime.Caller
`,
		"m/b_test.go": `package m
import _ "runtime"
func Caller(int) string { return "" }
var s = Caller(0)
`,
	})
	res, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Findings) != 1 || res.Findings[0].Site != (Site{Path: "m/a_test.go", Line: 3}) || res.Findings[0].Reason != reasonUnproven {
		t.Errorf("findings = %+v, want exactly m/a_test.go:3 (unproven)", res.Findings)
	}
}

// TestModulesNormalizesUseEntries pins approval condition 3's go.work
// normalization: block and single-line `use` forms, trailing `//` comments,
// comment-only and commented-out entries, a trailing slash, a quoted path, and
// a non-`use` block whose entries must not be read as modules.
func TestModulesNormalizesUseEntries(t *testing.T) {
	root := writeTree(t, map[string]string{
		"go.work": `// use ./prose-in-a-comment
go 1.25.0

use (
	./a // the a module
	b/
	// ./disabled
	"./q"
)

use ./c // single-line form

replace (
	example.com/r => ./r
)
`,
		"a/go.mod": "module a\n",
		"b/go.mod": "module b\n",
		"c/go.mod": "module c\n",
		"q/go.mod": "module q\n",
	})
	got, err := Modules(root)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a", "b", "q", "c"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Modules = %v, want %v", got, want)
	}
	res, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	// Each module holds no _test.go, so RequireModules must fail — and it must
	// MATCH "./a" to the cleaned "a" to reach the zero-files branch at all.
	if err := res.RequireModules("./a"); err == nil || !strings.Contains(err.Error(), "zero _test.go") {
		t.Errorf("RequireModules(./a) = %v, want the zero-test-files error (name matched on the cleaned path)", err)
	}
}

// TestScanSkipsNestedWorkModule pins attribution: a directory that is itself a
// go.work module is scanned under its own entry, not its parent's.
func TestScanSkipsNestedWorkModule(t *testing.T) {
	root := writeTree(t, map[string]string{
		"go.work":             "use (\n\t./p\n\t./p/n\n)\n",
		"p/go.mod":            "module p\n",
		"p/p_test.go":         "package p\n",
		"p/n/go.mod":          "module n\n",
		"p/n/n_test.go":       "package n\n",
		"p/.hidden/h_test.go": "package h\n",
		"p/_skip/s_test.go":   "package s\n",
		"p/_x_test.go":        "package p\n",
	})
	res, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []Module{{Name: "p", TestFiles: 1}, {Name: "p/n", TestFiles: 1}}
	if !reflect.DeepEqual(res.Modules, want) {
		t.Errorf("Modules = %+v, want %+v", res.Modules, want)
	}
}

// TestRepoRootFailsClosedWithoutGoWork: from a start with no go.work at or
// above it (a t.TempDir(), the same precondition as wirecontract's
// TestRepoRoot_NoGoWorkFailsClosed) RepoRoot errors, naming go.work and the
// start. The positive arm finds the real root from pkgSrcDir.
func TestRepoRootFailsClosedWithoutGoWork(t *testing.T) {
	start := t.TempDir()
	got, err := RepoRoot(start)
	if err == nil {
		t.Fatalf("RepoRoot(%s) = %q, nil; want an error (no go.work above a temp dir)", start, got)
	}
	if !strings.Contains(err.Error(), "go.work") || !strings.Contains(err.Error(), start) {
		t.Errorf("error %q does not name go.work and the start %s", err, start)
	}
	root, err := RepoRoot(pkgSrcDir)
	if err != nil {
		t.Fatalf("RepoRoot(pkgSrcDir): %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.work")); err != nil {
		t.Errorf("RepoRoot(pkgSrcDir) = %s, which holds no go.work: %v", root, err)
	}
}

// TestModulesFailsClosedOnEmptyGoWork: a go.work with no `use` entry (only a
// commented-out one), or no go.work at all, is an error, never an empty scan.
func TestModulesFailsClosedOnEmptyGoWork(t *testing.T) {
	root := writeTree(t, map[string]string{"go.work": "go 1.25.0\n// use ./m\n"})
	if got, err := Modules(root); err == nil || !strings.Contains(err.Error(), "no `use` modules") {
		t.Errorf("Modules(empty go.work) = %v, %v; want the no-use-modules error", got, err)
	}
	if _, err := Scan(root); err == nil {
		t.Error("Scan(empty go.work) = nil error, want the Modules error propagated")
	}
	if got, err := Modules(t.TempDir()); err == nil || !strings.Contains(err.Error(), "read go.work") {
		t.Errorf("Modules(no go.work) = %v, %v; want the read error", got, err)
	}
}

// TestModulesFailsClosedOnMissingModuleDir: a `use` entry with no directory
// behind it (or a regular file) is an error naming the entry.
func TestModulesFailsClosedOnMissingModuleDir(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"absent": {"go.work": "use ./absent\n"},
		"file":   {"go.work": "use ./absent\n", "absent": "not a dir\n"},
	} {
		t.Run(name, func(t *testing.T) {
			root := writeTree(t, files)
			if got, err := Modules(root); err == nil || !strings.Contains(err.Error(), "use absent") {
				t.Errorf("Modules = %v, %v; want the not-an-existing-directory error naming absent", got, err)
			}
		})
	}
}

// TestScanFailsClosedOnUnparseableTestFile: a `_test.go` that does not parse
// fails the whole scan with an error naming the file, never a skip.
func TestScanFailsClosedOnUnparseableTestFile(t *testing.T) {
	root := writeTree(t, map[string]string{
		"go.work":       "use ./m\n",
		"m/go.mod":      "module m\n",
		"m/bad_test.go": "package m\nfunc {\n",
	})
	if _, err := Scan(root); err == nil || !strings.Contains(err.Error(), "m/bad_test.go") {
		t.Errorf("Scan = %v, want a parse error naming m/bad_test.go", err)
	}
}

// TestRequireModulesFailsOnUnscannedModule pins the anti-vacuity check: a
// module whose only Go file is non-test, a name go.work does not declare, and
// an empty name list are each errors.
func TestRequireModulesFailsOnUnscannedModule(t *testing.T) {
	root := writeTree(t, map[string]string{
		"go.work":     "use (\n\t./m\n\t./k\n)\n",
		"m/go.mod":    "module m\n",
		"m/m.go":      "package m\n",
		"k/go.mod":    "module k\n",
		"k/k_test.go": "package k\n",
	})
	res, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := res.RequireModules("k"); err != nil {
		t.Errorf("RequireModules(k) = %v, want nil (k scanned one test file)", err)
	}
	if err := res.RequireModules("k", "m"); err == nil || !strings.Contains(err.Error(), `"m" scanned zero _test.go`) {
		t.Errorf("RequireModules(k, m) = %v, want the zero-test-files error for m", err)
	}
	if err := res.RequireModules("missing"); err == nil || !strings.Contains(err.Error(), "not a scanned go.work module") {
		t.Errorf("RequireModules(missing) = %v, want the not-scanned error", err)
	}
	if err := res.RequireModules(); err == nil {
		t.Error("RequireModules() = nil, want an error for an empty name list")
	}
}
