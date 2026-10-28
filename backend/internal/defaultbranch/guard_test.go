package defaultbranch

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The literal-main guard (#4660, R1): no production file this change routed
// through the resolver may carry a base branch spelled as a string literal.
// It parses EXACTLY the fixed list below with go/ast and fails on any
// token.STRING literal whose unquoted value is `main` or ends in `/main`
// (`refs/heads/main`, `origin/main`), unless its (file, enclosing function) is
// in the reviewed allow-list, which starts EMPTY.
//
// Scope, by design: an AST scan of a FIXED file list. It does not catch a
// branch name assembled at runtime, or a literal in a file outside the list.
//
// #4758 [E93.11] is where this list grows: when the four MCP spawn verbs
// resolve their base through this package, extend guardedFiles with
// ../mcpserver/run_stage.go, dispatch_stage.go, drive_run.go and
// run_children.go in that change.

// pkgSrcDir is the package source dir captured at init (go test runs the
// binary from it), never runtime.Caller (backend/internal/testanchor).
var pkgSrcDir = mustGetwd()

func mustGetwd() string {
	dir, err := os.Getwd()
	if err != nil {
		panic(fmt.Sprintf("pkgSrcDir: os.Getwd: %v", err))
	}
	return dir
}

// guardedFiles are the production files #4660 changed, relative to
// backend/internal.
var guardedFiles = []string{
	"defaultbranch/defaultbranch.go",
	"githubclient/client.go",
	"orchestrator/orchestrator.go",
	"server/server.go",
	"server/default_branch.go",
	"server/review_rerun.go",
	"server/deploy_trigger.go",
	"server/deploy_rollback.go",
	"server/runs.go",
	"../cmd/fishhawkd/serve.go",
}

// literalMainAllowList maps "<file>:<func>" to the reviewed reason a literal
// main is legitimate there. Every entry needs a non-empty reason.
var literalMainAllowList = map[string]string{}

// literalMainFinding is one offending literal.
type literalMainFinding struct {
	file, fn, value string
	line            int
}

func (f literalMainFinding) String() string {
	return fmt.Sprintf("%s:%d (func %s): literal %q", f.file, f.line, f.fn, f.value)
}

// isMainLiteral reports whether an unquoted string literal names the main
// branch.
func isMainLiteral(v string) bool {
	return v == "main" || strings.HasSuffix(v, "/main")
}

// scanLiteralMain reports every main-branch string literal in f that is not
// allow-listed for (name, enclosing func), plus the file's FuncDecl count for
// the anti-vacuity check. Literals outside any function report fn "<file>".
func scanLiteralMain(fset *token.FileSet, name string, f *ast.File, allow map[string]string) ([]literalMainFinding, int) {
	var findings []literalMainFinding
	funcs := 0
	check := func(n ast.Node, fn string) {
		ast.Inspect(n, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			v, err := strconv.Unquote(lit.Value)
			if err != nil || !isMainLiteral(v) {
				return true
			}
			if reason := allow[name+":"+fn]; reason != "" {
				return true
			}
			findings = append(findings, literalMainFinding{
				file: name, fn: fn, value: v, line: fset.Position(lit.Pos()).Line,
			})
			return true
		})
	}
	for _, decl := range f.Decls {
		if fd, ok := decl.(*ast.FuncDecl); ok {
			funcs++
			check(fd, fd.Name.Name)
			continue
		}
		check(decl, "<file>")
	}
	return findings, funcs
}

func TestNoLiteralMainBaseInResolvedPaths(t *testing.T) {
	for key, reason := range literalMainAllowList {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("allow-list entry %q has no reviewed reason", key)
		}
	}
	root := filepath.Join(pkgSrcDir, "..")
	var all []literalMainFinding
	for _, rel := range guardedFiles {
		path := filepath.Join(root, rel)
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("guarded file %s: %v (the guard must not pass over a missing file)", rel, err)
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, rel, src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		findings, funcs := scanLiteralMain(fset, rel, f, literalMainAllowList)
		if funcs == 0 {
			t.Fatalf("guarded file %s has no FuncDecl; the scan would be vacuous", rel)
		}
		all = append(all, findings...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].String() < all[j].String() })
	for _, f := range all {
		t.Errorf("literal main branch in a resolved path: %s — resolve the base through defaultbranch (or add a reviewed allow-list entry with a reason)", f)
	}
}

// TestNoLiteralMainBase_DetectsPlantedLiteral proves the scanner is not
// vacuous on the shapes it guards: one finding per planted literal, and none
// once that (file, func) is allow-listed.
func TestNoLiteralMainBase_DetectsPlantedLiteral(t *testing.T) {
	cases := []struct {
		name, src string
		want      int
	}{
		{"bare main", `package p
func base() string { b := "main"; return b }`, 1},
		{"refs/heads/main", `package p
func ref() string { return "refs/heads/main" }`, 1},
		{"package-level const", `package p
const fallback = "origin/main"
func f() {}`, 1},
		{"look-alikes are not findings", `package p
func f() []string { return []string{"maintain", "main.go", "domain", "mainline"} }`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "planted.go", tc.src, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			got, funcs := scanLiteralMain(fset, "planted.go", f, nil)
			if len(got) != tc.want {
				t.Errorf("findings = %v, want %d", got, tc.want)
			}
			if funcs != 1 {
				t.Errorf("FuncDecl count = %d, want 1", funcs)
			}
		})
	}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "planted.go", `package p
func base() string { return "main" }`, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	allowed := map[string]string{"planted.go:base": "reviewed: fixture"}
	if got, _ := scanLiteralMain(fset, "planted.go", f, allowed); len(got) != 0 {
		t.Errorf("allow-listed literal reported: %v", got)
	}
	if got, _ := scanLiteralMain(fset, "planted.go", f, map[string]string{"planted.go:base": ""}); len(got) != 1 {
		t.Errorf("an allow-list entry with an EMPTY reason must not suppress the finding; got %v", got)
	}
}
