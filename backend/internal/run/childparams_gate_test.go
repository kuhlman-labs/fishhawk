package run

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// This file is the USE pin for ChildParamsFrom (E67.17 / #2589). The
// reflection pins in childparams_test.go prove the helper is CORRECT;
// only a source scan proves it is USED. Without one, a sixth mint site
// can hand-roll a run.CreateRunParams literal, drop working_dir again,
// and every test stays green — which is exactly how the first five
// sites diverged.
//
// Detection is AST-based, not textual, and is modelled on the in-repo
// precedent backend/internal/forge/credential_scope_gate_test.go: same
// walker shape, same allow-list-with-reasons discipline, same
// fail-closed-on-parse-error rule. A line regex would miss
// `&run.CreateRunParams{}`, an aliased import, an elided element type
// inside `[]run.CreateRunParams{{...}}`, and `var p run.CreateRunParams`
// — every one of which constructs the same value.
//
// The allow-list is keyed by (file path, ENCLOSING FUNCTION), not by
// path prefix. That is load-bearing: webhook/dispatcher.go legitimately
// holds the event-driven fresh mint in Handle while handleCIFailureRetry
// in the SAME file must stay behind the helper.

// runPkgPath is the import path whose qualifier binds CreateRunParams.
const runPkgPath = "github.com/kuhlman-labs/fishhawk/backend/internal/run"

// childParamsTypeName is the type the gate refuses to see hand-rolled.
const childParamsTypeName = "CreateRunParams"

// gateSkipDirs are directory NAMES pruned anywhere in the walk. "db"
// mirrors the AGENTS.md coverage-gate convention for sqlc-generated
// packages: run/db declares its OWN CreateRunParams (the sqlc row
// struct), which is a different type reached through a different
// qualifier and is not a run-from-run mint.
var gateSkipDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"frontend":     true,
	"testdata":     true,
	"db":           true,
}

// childParamsAllowed maps "<repo-relative path>::<enclosing func>" to
// WHY that site legitimately builds CreateRunParams by hand. Everything
// else must go through ChildParamsFrom. Keep the reasons — they are the
// contract, and a new entry should be hard to add without stating one.
var childParamsAllowed = map[string]string{
	// The event-driven fresh mint: every field comes from the webhook
	// event payload. There is no parent run to inherit from.
	"backend/internal/webhook/dispatcher.go::(*Dispatcher).Handle": "webhook event-driven fresh mint — no parent run exists",

	// The GitLab half of that same event-driven fresh mint (E45.22 /
	// #2043). It lives in its own file because the GitLab create path
	// reads its spec through the forge-neutral FileFetcher and takes no
	// branch-protection snapshot, but the reason is identical: the webhook
	// event, not a parent run, is the source of every field.
	"backend/internal/webhook/gitlab_dispatch.go::(*Dispatcher).handleGitLabCreateRun": "webhook event-driven fresh mint — no parent run exists",

	// The request-driven mint behind POST /v0/runs and the campaign
	// driver. Same reason: the request, not a parent run, is the source.
	"backend/internal/server/runs.go::(*Server).CreateRunForTrigger": "request-driven fresh mint — no parent run exists",

	// The helper itself is where the sanctioned literal lives.
	"backend/internal/run/childparams.go::ChildParamsFrom": "the sanctioned construction point",
}

// literalSite is one hand-rolled construction of the type, located for
// reporting and for allow-list matching.
type literalSite struct {
	rel  string
	line int
	fn   string
}

func (s literalSite) key() string { return s.rel + "::" + s.fn }

func (s literalSite) String() string {
	return s.rel + ":" + strconv.Itoa(s.line) + " (in " + s.fn + ")"
}

// funcSpan records one top-level func/method's source range so a site
// can be attributed to its enclosing function.
type funcSpan struct {
	name       string
	start, end token.Pos
}

// funcNameOf renders a FuncDecl's reportable name: "Name" for a plain
// function, "(*Recv).Name" for a method.
func funcNameOf(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	typ := fn.Recv.List[0].Type
	star := ""
	if s, ok := typ.(*ast.StarExpr); ok {
		typ = s.X
		star = "*"
	}
	if id, ok := typ.(*ast.Ident); ok {
		return "(" + star + id.Name + ")." + fn.Name.Name
	}
	return fn.Name.Name
}

// runQualifierIn returns the identifier that binds the run package in
// this file. An ImportSpec with a nil Name binds the package's declared
// name, which for every package in this repo is the last path segment —
// so `runpkg "…/internal/run"` binds "runpkg" and a default import binds
// "run". Returns "" when the file does not import the run package.
func runQualifierIn(file *ast.File) string {
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || path != runPkgPath {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return path[strings.LastIndex(path, "/")+1:]
	}
	return ""
}

// childParamsLiteralsIn parses src and returns every hand-rolled
// CONSTRUCTION of run.CreateRunParams in it: composite literals
// (qualified, aliased, bare-in-package, address-of, and elided inside a
// slice/map literal) and var/const declarations of the type.
//
// It deliberately does NOT report *ast.Field — function parameters and
// results, interface method signatures, struct fields. Those are the
// legitimate signatures in repository.go / fake.go / postgres.go: they
// pass an already-constructed value, they do not construct one.
func childParamsLiteralsIn(rel, src string) ([]literalSite, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, rel, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}

	qualifier := runQualifierIn(file)
	inPackageRun := file.Name != nil && file.Name.Name == "run"

	// isType reports whether an ast.Expr names run.CreateRunParams as
	// reached from THIS file. A bare identifier matches only inside
	// package run; a selector matches only under the file's own binding
	// for the run import — so rundb.CreateRunParams (the sqlc row type
	// under .../run/db) and a same-named local type in a package that
	// does not import run both correctly miss.
	isType := func(expr ast.Expr) bool {
		switch e := expr.(type) {
		case *ast.Ident:
			return inPackageRun && e.Name == childParamsTypeName
		case *ast.SelectorExpr:
			id, ok := e.X.(*ast.Ident)
			return ok && qualifier != "" && id.Name == qualifier && e.Sel.Name == childParamsTypeName
		}
		return false
	}

	// Enclosing-function spans, for allow-list keying.
	var spans []funcSpan
	for _, d := range file.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok {
			spans = append(spans, funcSpan{name: funcNameOf(fn), start: fn.Pos(), end: fn.End()})
		}
	}
	enclosing := func(pos token.Pos) string {
		for _, s := range spans {
			if pos >= s.start && pos < s.end {
				return s.name
			}
		}
		return "" // package-level declaration
	}

	seen := map[int]bool{}
	var sites []literalSite
	record := func(pos token.Pos) {
		p := fset.Position(pos)
		if seen[p.Offset] {
			return
		}
		seen[p.Offset] = true
		sites = append(sites, literalSite{rel: rel, line: p.Line, fn: enclosing(pos)})
	}

	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CompositeLit:
			if node.Type == nil {
				// An element whose type is elided inside an enclosing
				// composite literal; recorded from the parent below.
				return true
			}
			if isType(node.Type) {
				record(node.Pos())
				return true
			}
			// Elided element types: go/ast reports a nil Type on the
			// inner literal, so the element type is resolved from the
			// enclosing literal instead of read off the inner node.
			var elem ast.Expr
			switch t := node.Type.(type) {
			case *ast.ArrayType:
				elem = t.Elt
			case *ast.MapType:
				elem = t.Value
			}
			if elem == nil || !isType(elem) {
				return true
			}
			for _, el := range node.Elts {
				if kv, ok := el.(*ast.KeyValueExpr); ok {
					el = kv.Value
				}
				if inner, ok := el.(*ast.CompositeLit); ok {
					record(inner.Pos())
				}
			}
		case *ast.ValueSpec:
			// `var p run.CreateRunParams` — declaring then assigning
			// field-by-field constructs the same value, so the gate is
			// not evadable by avoiding the literal syntax.
			if node.Type != nil && isType(node.Type) {
				record(node.Pos())
			}
		}
		return true
	})
	sort.Slice(sites, func(i, j int) bool { return sites[i].line < sites[j].line })
	return sites, nil
}

// collectChildParamsLiterals walks <root>/backend's non-test Go sources
// and returns every construction site plus the number of files scanned.
//
// It RETURNS AN ERROR — never a skip — on a missing backend tree, a walk
// failure, or a parse failure. An unparseable file is an unscanned file,
// and an unscanned file is a hole in the gate.
func collectChildParamsLiterals(root string) ([]literalSite, int, error) {
	backend := filepath.Join(root, "backend")
	if _, err := os.Stat(backend); err != nil {
		return nil, 0, fmt.Errorf("stat backend module under %q: %w", root, err)
	}
	var sites []literalSite
	scanned := 0
	err := filepath.WalkDir(backend, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walk %s: %w", path, err)
		}
		if d.IsDir() {
			if gateSkipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return fmt.Errorf("relativise %s: %w", path, err)
		}
		rel = filepath.ToSlash(rel)
		found, err := childParamsLiteralsIn(rel, string(b))
		if err != nil {
			return fmt.Errorf("parse %s: %w (the gate cannot scan what it cannot parse)", rel, err)
		}
		scanned++
		sites = append(sites, found...)
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	return sites, scanned, nil
}

// gateRepoRoot returns the workspace root by hopping four parents up
// from this file's directory (<root>/backend/internal/run).
func gateRepoRoot(t *testing.T) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed: cannot locate the repo root")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(self))))
	anchor := filepath.Join(root, "backend", "internal", "run", "repository.go")
	if _, err := os.Stat(anchor); err != nil {
		t.Fatalf("anchor %q not found under derived repo root %q: %v"+
			" (the workspace layout moved; fix gateRepoRoot's parent hops"+
			" rather than letting the gate scan nothing)", anchor, root, err)
	}
	return root
}

// TestNoHandRolledChildRunParamsLiteral is the contract: a run minted
// from another run is constructed by run.ChildParamsFrom, so field
// inheritance is the default and every divergence is one reviewable
// line at the call site.
func TestNoHandRolledChildRunParamsLiteral(t *testing.T) {
	root := gateRepoRoot(t)
	sites, scanned, err := collectChildParamsLiterals(root)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	// Blind-gate guards: a scan that silently sees nothing reports
	// "no offenders" and is indistinguishable from a passing gate.
	if scanned == 0 {
		t.Fatal("scanned 0 Go files: the gate saw nothing, which is not the same as finding nothing")
	}
	if len(sites) == 0 {
		t.Fatalf("found 0 %s constructions in %d scanned files: the detector is not seeing"+
			" the real literals (the allow-listed request-driven mints must be found)",
			childParamsTypeName, scanned)
	}

	var offenders []string
	for _, s := range sites {
		if _, ok := childParamsAllowed[s.key()]; !ok {
			offenders = append(offenders, s.String())
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("%d hand-rolled run.%s construction(s) — use run.ChildParamsFrom(parent) and set only"+
			" what makes the site distinct; if the site is genuinely NOT a run-from-run mint, add a"+
			" childParamsAllowed entry keyed \"<path>::<func>\" stating why:\n\t%s",
			len(offenders), childParamsTypeName, strings.Join(offenders, "\n\t"))
	}
}

// TestChildParamsAllowListEntriesStillMatch fails on a dead allow-list
// entry. Without it the allow-list rots into a silent re-opening: a
// renamed or deleted function leaves its exemption behind, ready to
// sanction whatever lands at that key next.
func TestChildParamsAllowListEntriesStillMatch(t *testing.T) {
	root := gateRepoRoot(t)
	sites, _, err := collectChildParamsLiterals(root)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	live := map[string]bool{}
	for _, s := range sites {
		live[s.key()] = true
	}
	for key := range childParamsAllowed {
		if !live[key] {
			t.Errorf("allow-list entry %q matches no %s construction: remove it, or fix the key"+
				" (a dead entry silently sanctions whatever lands there next)", key, childParamsTypeName)
		}
	}
}

// TestChildParamsGateFailsClosed drives the pure collector against
// fixtures that already exhibit each failure mode. Each subtest IS its
// own counterfactual: a collector that skipped instead of erroring
// fails here with no source deletion needed.
func TestChildParamsGateFailsClosed(t *testing.T) {
	t.Run("missing_backend_module", func(t *testing.T) {
		root := t.TempDir() // no backend/ under it
		if _, _, err := collectChildParamsLiterals(root); err == nil {
			t.Fatal("want an error for a root with no backend module, got nil (a skip is a hole)")
		}
	})

	t.Run("unparseable_source_file", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, "backend", "internal", "broken")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		bad := filepath.Join(dir, "broken.go")
		if err := os.WriteFile(bad, []byte("package broken\nfunc ( {\n"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		_, _, err := collectChildParamsLiterals(root)
		if err == nil {
			t.Fatal("want an error for an unparseable file, got nil (an unscanned file is a hole)")
		}
		if !strings.Contains(err.Error(), "broken.go") {
			t.Errorf("error does not name the offending file: %v", err)
		}
	})

	t.Run("unreadable_directory", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root: chmod 0000 is not enforced, so the OS produces no permission error")
		}
		root := t.TempDir()
		dir := filepath.Join(root, "backend", "internal", "locked")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte("package locked\n"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := os.Chmod(dir, 0o000); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
		if _, _, err := collectChildParamsLiterals(root); err == nil {
			t.Fatal("want an error for an unreadable directory, got nil (a skip is a hole)")
		}
	})
}

// TestChildParamsLiteralDetectionForms pins the detector against the
// spellings a line regex misses and the near-misses it must not flag.
func TestChildParamsLiteralDetectionForms(t *testing.T) {
	cases := []struct {
		name  string
		src   string
		match bool
	}{
		{"qualified_literal", `package x
import "` + runPkgPath + `"
func f() { _ = run.CreateRunParams{Repo: "r"} }
`, true},
		{"aliased_import_literal", `package x
import runpkg "` + runPkgPath + `"
func f() { _ = runpkg.CreateRunParams{Repo: "r"} }
`, true},
		{"bare_in_package_run", `package run
func f() { _ = CreateRunParams{Repo: "r"} }
`, true},
		{"address_of_literal", `package x
import "` + runPkgPath + `"
func f() { _ = &run.CreateRunParams{Repo: "r"} }
`, true},
		{"elided_slice_element", `package x
import "` + runPkgPath + `"
func f() { _ = []run.CreateRunParams{{Repo: "r"}} }
`, true},
		{"elided_map_value", `package x
import "` + runPkgPath + `"
func f() { _ = map[string]run.CreateRunParams{"k": {Repo: "r"}} }
`, true},
		{"var_declaration", `package x
import "` + runPkgPath + `"
func f() { var p run.CreateRunParams; _ = p }
`, true},

		{"sqlc_row_type_is_not_ours", `package x
import (
	"` + runPkgPath + `"
	rundb "` + runPkgPath + `/db"
)
func f() { _ = rundb.CreateRunParams{}; _ = run.State("") }
`, false},
		{"same_named_type_without_run_import", `package x
type CreateRunParams struct{ Repo string }
func f() { _ = CreateRunParams{Repo: "r"} }
`, false},
		{"func_parameter_signature", `package x
import "` + runPkgPath + `"
func f(p run.CreateRunParams) {}
`, false},
		{"interface_method_signature", `package x
import "` + runPkgPath + `"
type R interface{ CreateRun(p run.CreateRunParams) error }
`, false},
		{"struct_field", `package x
import "` + runPkgPath + `"
type holder struct{ P run.CreateRunParams }
`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sites, err := childParamsLiteralsIn(tc.name+".go", tc.src)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := len(sites) > 0; got != tc.match {
				t.Errorf("detected=%v (sites=%v), want detected=%v", got, sites, tc.match)
			}
		})
	}
}

// TestChildParamsLiteralEnclosingFunction pins the allow-list KEY: two
// literals in one file must be distinguishable by enclosing function,
// which is what lets webhook/dispatcher.go keep Handle open while
// handleCIFailureRetry stays closed.
func TestChildParamsLiteralEnclosingFunction(t *testing.T) {
	src := `package x
import "` + runPkgPath + `"
type D struct{}
func (d *D) Handle() { _ = run.CreateRunParams{} }
func (d *D) handleCIFailureRetry() { _ = run.CreateRunParams{} }
`
	sites, err := childParamsLiteralsIn("d.go", src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(sites) != 2 {
		t.Fatalf("sites = %d, want 2: %v", len(sites), sites)
	}
	want := []string{"d.go::(*D).Handle", "d.go::(*D).handleCIFailureRetry"}
	for i, s := range sites {
		if s.key() != want[i] {
			t.Errorf("site %d key = %q, want %q", i, s.key(), want[i])
		}
	}
}
