package basedelta

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// pkgSrcDir anchors repository paths (testanchor rule: never runtime.Caller).
var pkgSrcDir = mustGetwd()

func mustGetwd() string {
	dir, err := os.Getwd()
	if err != nil {
		panic(fmt.Sprintf("pkgSrcDir: os.Getwd: %v", err))
	}
	return dir
}

func repoRoot() string { return filepath.Join(pkgSrcDir, "..", "..", "..") }

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func requireGo(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Fatalf("go toolchain not on PATH: %v", err)
	}
}

// emptyModCache points GOMODCACHE at an empty dir, so nothing third-party is
// resolvable (the loader itself pins GOPROXY=off).
func emptyModCache(t *testing.T) []string {
	return []string{"GOMODCACHE=" + t.TempDir()}
}

func importDirs(t *testing.T, g *Graph, importPath string) []string {
	t.Helper()
	p, ok := g.Package(importPath)
	if !ok {
		t.Fatalf("graph has no package %s", importPath)
	}
	var out []string
	for _, imp := range p.Imports {
		q, _ := g.Package(imp)
		out = append(out, q.Dir)
	}
	sort.Strings(out)
	return out
}

const twoModuleWork = "go 1.25.0\n\nuse (\n\t./m1\n\t./m2\n)\n"

// L1: edges (a cross-module TEST import included) and repo-relative dirs,
// with a non-package testdata dir, loaded through the symlinked TempDir path.
func TestGoListLoader_EdgesAndDirs(t *testing.T) {
	t.Parallel()
	requireGo(t)
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"go.work":                  twoModuleWork,
		"m1/go.mod":                "module example.com/m1\n\ngo 1.25.0\n",
		"m1/a/a.go":                "package a\n",
		"m1/a/testdata/fixture.go": "package fixture\n\nimport _ \"example.com/m2/b\"\n",
		"m2/go.mod":                "module example.com/m2\n\ngo 1.25.0\n",
		"m2/b/b.go":                "package b\n",
		"m2/b/b_test.go":           "package b\n\nimport _ \"example.com/m1/a\"\n",
		"m2/c/c.go":                "package c\n\nimport _ \"example.com/m2/b\"\n",
	})
	g, err := (&GoListLoader{Env: emptyModCache(t)}).Load(context.Background(), root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if g.Len() != 3 {
		t.Fatalf("Len = %d, want 3 (testdata is no package)", g.Len())
	}
	if got := importDirs(t, g, "example.com/m2/b"); !reflect.DeepEqual(got, []string{"m1/a"}) {
		t.Fatalf("b imports %v, want [m1/a] (test import)", got)
	}
	if got := importDirs(t, g, "example.com/m2/c"); !reflect.DeepEqual(got, []string{"m2/b"}) {
		t.Fatalf("c imports %v, want [m2/b]", got)
	}
	a, _ := g.Package("example.com/m1/a")
	if a.Dir != "m1/a" || a.ModuleDir != "m1" {
		t.Fatalf("a = %+v, want Dir m1/a ModuleDir m1", a)
	}
}

// L2 + approval condition C1: an OS-tagged file's imports are in the graph on
// every host, a package whose files are all excluded on the host is still
// discovered, and a third-party module imported only from a _windows.go file
// is never needed (empty GOMODCACHE, GOPROXY=off).
func TestGoListLoader_OSTaggedImportsWithoutModuleCache(t *testing.T) {
	t.Parallel()
	requireGo(t)
	if runtime.GOOS == "windows" {
		t.Skip("the fixture's exclusion is _windows.go; the host must not be windows")
	}
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"go.work":                 "go 1.25.0\n\nuse ./m1\n",
		"m1/go.mod":               "module example.com/m1\n\ngo 1.25.0\n\nrequire example.com/thirdparty v1.0.0\n",
		"m1/x/x.go":               "package x\n",
		"m1/x/x_windows.go":       "package x\n\nimport (\n\t_ \"example.com/m1/onlywin\"\n\t_ \"example.com/m1/w\"\n\t_ \"example.com/thirdparty\"\n)\n",
		"m1/w/w.go":               "package w\n",
		"m1/onlywin/o_windows.go": "package onlywin\n",
	})
	g, err := (&GoListLoader{Env: emptyModCache(t)}).Load(context.Background(), root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := importDirs(t, g, "example.com/m1/x"); !reflect.DeepEqual(got, []string{"m1/onlywin", "m1/w"}) {
		t.Fatalf("x imports %v, want [m1/onlywin m1/w]", got)
	}
	if o, ok := g.Package("example.com/m1/onlywin"); !ok || o.Dir != "m1/onlywin" || o.ModuleDir != "m1" {
		t.Fatalf("host-excluded package not discovered: %+v ok=%v", o, ok)
	}
}

// L3: an import of a missing workspace package fails closed, and Classify
// turns the load error into undecidable.
func TestGoListLoader_MissingWorkspaceImport(t *testing.T) {
	t.Parallel()
	requireGo(t)
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"go.work":   "go 1.25.0\n\nuse ./m1\n",
		"m1/go.mod": "module example.com/m1\n\ngo 1.25.0\n",
		"m1/a/a.go": "package a\n\nimport _ \"example.com/m1/missing\"\n",
	})
	l := &GoListLoader{Env: emptyModCache(t)}
	if _, err := l.Load(context.Background(), root); err == nil || !strings.Contains(err.Error(), "example.com/m1/a") {
		t.Fatalf("Load err = %v, want a package error naming example.com/m1/a", err)
	}
	got := (&Classifier{Loader: l}).Classify(context.Background(), Input{
		DeltaPaths: []string{"m1/a/notes.txt"}, PRPaths: []string{"m1/b.txt"}, Workspaces: []string{root},
	})
	if got.Independent || !strings.HasPrefix(got.Reason, "undecidable:load:") {
		t.Fatalf("Classify = %+v, want undecidable:load:", got)
	}
}

// The go/parser pass: a host-excluded file importing a workspace-module path
// that is no package fails closed even though go list saw nothing wrong.
func TestGoListLoader_UnresolvedImportInExcludedFile(t *testing.T) {
	t.Parallel()
	requireGo(t)
	if runtime.GOOS == "windows" {
		t.Skip("the fixture's exclusion is _windows.go; the host must not be windows")
	}
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"go.work":           "go 1.25.0\n\nuse ./m1\n",
		"m1/go.mod":         "module example.com/m1\n\ngo 1.25.0\n",
		"m1/a/a.go":         "package a\n",
		"m1/a/a_windows.go": "package a\n\nimport _ \"example.com/m1/missing\"\n",
	})
	_, err := (&GoListLoader{Env: emptyModCache(t)}).Load(context.Background(), root)
	if err == nil || !strings.Contains(err.Error(), "imports example.com/m1/missing") {
		t.Fatalf("Load err = %v, want the unresolved workspace import", err)
	}
}

func TestGoListLoader_UnparsableFile(t *testing.T) {
	t.Parallel()
	requireGo(t)
	if runtime.GOOS == "windows" {
		t.Skip("the fixture's exclusion is _windows.go; the host must not be windows")
	}
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"go.mod":         "module example.com/r\n\ngo 1.25.0\n",
		"a/a.go":         "package a\n",
		"a/a_windows.go": "package a\n\nimport \"unterminated\n",
	})
	if _, err := (&GoListLoader{Env: emptyModCache(t)}).Load(context.Background(), root); err == nil || !strings.Contains(err.Error(), "parse imports") {
		t.Fatalf("Load err = %v, want a parse error", err)
	}
}

// fakeGo writes an executable stand-in for the go binary.
func fakeGo(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "go")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func goModOnly(t *testing.T) string {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"go.mod": "module example.com/r\n\ngo 1.25.0\n", "a/a.go": "package a\n"})
	return root
}

// L4
func TestGoListLoader_NonZeroExit(t *testing.T) {
	t.Parallel()
	l := &GoListLoader{GoBin: fakeGo(t, "echo boom >&2; exit 3")}
	_, err := l.Load(context.Background(), goModOnly(t))
	if err == nil || !strings.Contains(err.Error(), "exit status 3") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Load err = %v, want exit status 3 with the stderr tail", err)
	}
}

// L5
func TestGoListLoader_MalformedOutput(t *testing.T) {
	t.Parallel()
	l := &GoListLoader{GoBin: fakeGo(t, "echo '{not json'")}
	_, err := l.Load(context.Background(), goModOnly(t))
	if err == nil || !strings.Contains(err.Error(), "malformed output") {
		t.Fatalf("Load err = %v, want malformed output", err)
	}
}

func TestGoListLoader_MalformedWorkEdit(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeTree(t, root, map[string]string{"go.work": "go 1.25.0\n"})
	l := &GoListLoader{GoBin: fakeGo(t, "echo '{not json'")}
	if _, err := l.Load(context.Background(), root); err == nil || !strings.Contains(err.Error(), "go work edit -json: malformed output") {
		t.Fatalf("Load err = %v", err)
	}
	for body, want := range map[string]string{
		`echo '{"Use":[]}'`:                      "lists no modules",
		`echo '{"Use":[{"DiskPath":"../out"}]}'`: "not an in-workspace path",
		`echo '{"Use":[{"DiskPath":"/abs/m"}]}'`: "not an in-workspace path",
	} {
		l := &GoListLoader{GoBin: fakeGo(t, body)}
		if _, err := l.Load(context.Background(), root); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: Load err = %v, want %q", body, err, want)
		}
	}
}

// Package-level failure fields and out-of-workspace dirs, each fed through a
// fake go list.
func TestGoListLoader_PackageFailureFields(t *testing.T) {
	t.Parallel()
	root := goModOnly(t)
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	mod := fmt.Sprintf(`"Module":{"Path":"example.com/r","Dir":%q}`, real)
	for name, tc := range map[string]struct{ json, want string }{
		"error":       {fmt.Sprintf(`{"ImportPath":"example.com/r/a","Dir":%q,%s,"Error":{"Err":"bad"}}`, real+"/a", mod), "example.com/r/a: bad"},
		"deps_errors": {fmt.Sprintf(`{"ImportPath":"example.com/r/a","Dir":%q,%s,"DepsErrors":[{"Err":"dep bad"}]}`, real+"/a", mod), "dependency error: dep bad"},
		"incomplete":  {fmt.Sprintf(`{"ImportPath":"example.com/r/a","Dir":%q,%s,"Incomplete":true}`, real+"/a", mod), "incomplete"},
		"no_module":   {fmt.Sprintf(`{"ImportPath":"example.com/r/a","Dir":%q}`, real+"/a"), "has no module"},
		"outside":     {fmt.Sprintf(`{"ImportPath":"example.com/r/a","Dir":"/elsewhere/a",%s}`, mod), "outside the workspace"},
		"zero":        {"", "zero packages"},
	} {
		l := &GoListLoader{GoBin: fakeGo(t, "cat <<'EOF'\n"+tc.json+"\nEOF")}
		if _, err := l.Load(context.Background(), root); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: Load err = %v, want %q", name, err, tc.want)
		}
	}
}

// L6: a wedged go binary is killed at Timeout. The sleep is a BACKGROUNDED
// grandchild holding the stdout pipe, so only a process-group kill
// (procgroup.Harden) returns before it exits.
func TestGoListLoader_Timeout(t *testing.T) {
	t.Parallel()
	timeout := timescale.D(200 * time.Millisecond)
	l := &GoListLoader{GoBin: fakeGo(t, "sleep 30 &\nwait"), Timeout: timeout}
	start := time.Now()
	_, err := l.Load(context.Background(), goModOnly(t))
	if err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("Load err = %v, want deadline exceeded", err)
	}
	if el := time.Since(start); el > timeout+killGrace+timescale.D(2*time.Second) {
		t.Fatalf("Load took %s past a %s timeout", el, timeout)
	}
}

// L7: a module with no packages fails closed.
func TestGoListLoader_ZeroPackages(t *testing.T) {
	t.Parallel()
	requireGo(t)
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"go.work":   "go 1.25.0\n\nuse ./m1\n",
		"m1/go.mod": "module example.com/m1\n\ngo 1.25.0\n",
	})
	if _, err := (&GoListLoader{Env: emptyModCache(t)}).Load(context.Background(), root); err == nil || !strings.Contains(err.Error(), "zero packages") {
		t.Fatalf("Load err = %v, want zero packages", err)
	}
}

// L8: a non-Go repository loads as an empty graph.
func TestGoListLoader_NonGoRepo(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeTree(t, root, map[string]string{"README.md": "hi\n"})
	g, err := (&GoListLoader{GoBin: fakeGo(t, "exit 9")}).Load(context.Background(), root)
	if err != nil || g.Len() != 0 {
		t.Fatalf("Load = %v, %v; want empty graph, nil", g, err)
	}
	if _, err := (&GoListLoader{}).Load(context.Background(), filepath.Join(root, "absent")); err == nil {
		t.Fatal("Load of a missing dir succeeded")
	}
}

// L9 + approval condition C7: an inherited GOWORK naming a DIFFERENT valid
// workspace changes nothing. The other workspace uses ./o1, so an unpinned
// `go work edit -json` would hand back ./o1 (absent in the target), and an
// unpinned `go list` would reject ./m1 as outside its workspace. Not parallel:
// t.Setenv.
func TestGoListLoader_GOWORKPinned(t *testing.T) {
	requireGo(t)
	other := t.TempDir()
	writeTree(t, other, map[string]string{
		"go.work":   "go 1.25.0\n\nuse ./o1\n",
		"o1/go.mod": "module example.com/o1\n\ngo 1.25.0\n",
		"o1/o/o.go": "package o\n",
	})
	t.Setenv("GOWORK", filepath.Join(other, "go.work"))
	target := t.TempDir()
	writeTree(t, target, map[string]string{
		"go.work":   "go 1.25.0\n\nuse ./m1\n",
		"m1/go.mod": "module example.com/m1\n\ngo 1.25.0\n",
		"m1/a/a.go": "package a\n",
	})
	g, err := (&GoListLoader{Env: emptyModCache(t)}).Load(context.Background(), target)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := g.Package("example.com/m1/a"); !ok || g.Len() != 1 {
		t.Fatalf("Load returned %d packages, want exactly example.com/m1/a", g.Len())
	}
}

// Approval condition C7: GOTOOLCHAIN=local, so a go.work demanding a newer Go
// fails closed naming the pin instead of attempting a toolchain download. Not
// parallel: t.Setenv.
func TestGoListLoader_ToolchainMismatchFailsLocal(t *testing.T) {
	requireGo(t)
	t.Setenv("GOTOOLCHAIN", "auto")
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"go.work":   "go 1.99.0\n\nuse ./m1\n",
		"m1/go.mod": "module example.com/m1\n\ngo 1.25.0\n",
		"m1/a/a.go": "package a\n",
	})
	_, err := (&GoListLoader{Env: emptyModCache(t)}).Load(context.Background(), root)
	if err == nil || !strings.Contains(err.Error(), "GOTOOLCHAIN=local") {
		t.Fatalf("Load err = %v, want a GOTOOLCHAIN=local refusal", err)
	}
}

// L10: the real repository loads error-free, with every go.work module
// present and a known edge.
func TestGoListLoader_RealRepository(t *testing.T) {
	t.Parallel()
	requireGo(t)
	g, err := (&GoListLoader{}).Load(context.Background(), repoRoot())
	if err != nil {
		t.Fatalf("Load(real repo): %v", err)
	}
	mods := map[string]bool{}
	for _, ip := range sortedKeys(g.pkgs) {
		mods[g.pkgs[ip].ModuleDir] = true
	}
	for _, m := range []string{"backend", "cli", "credstore", "directory", "pricing", "redaction", "runner", "verifier"} {
		if !mods[m] {
			t.Errorf("no package from go.work module %s", m)
		}
	}
	server, ok := g.Package(modPrefix + "backend/internal/server")
	if !ok {
		t.Fatal("no backend/internal/server package")
	}
	found := false
	for _, imp := range server.Imports {
		if imp == modPrefix+"backend/internal/audit" {
			found = true
		}
	}
	if !found {
		t.Fatal("edge backend/internal/server -> backend/internal/audit missing")
	}
}

func TestTailBuffer(t *testing.T) {
	t.Parallel()
	b := &tailBuffer{max: 4}
	_, _ = b.Write([]byte("abcdef"))
	_, _ = b.Write([]byte("gh"))
	if b.String() != "efgh" {
		t.Fatalf("tail = %q", b.String())
	}
}
