// Package testanchor is the repo-level guard that keeps Go test fixtures
// path-independent (#4179). Under -trimpath, runtime.Caller reports a
// module-relative file name, so a fixture path derived from it resolves
// against the package cwd and misses; every test package instead anchors on an
// init-captured `var pkgSrcDir = mustGetwd()` (go test runs the binary from the
// package source dir). Scan parses every `_test.go` file of every go.work
// module with go/ast and reports each runtime.Caller call whose FILE result is
// used — or that sits in a context the scan cannot prove discards it. The one
// allowed form discards the file explicitly: `pc, _, _, ok := runtime.Caller(1)`.
//
// Like wirecontract, this package is production code consumed only by its own
// test, and it REQUIRES the full repository tree: every resolution failure
// (no go.work, an empty or dangling `use`, an unparseable test file, a module
// that scanned nothing) is an error, never a silent empty result, because a
// guard that can cover nothing passes vacuously. Contract: README.md.
package testanchor

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Site is a position in the scanned tree: a repo-relative, slash-separated
// path and a 1-based line.
type Site struct {
	Path string
	Line int
}

func (s Site) String() string { return fmt.Sprintf("%s:%d", s.Path, s.Line) }

// Finding is one runtime.Caller use the guard rejects, with a reason that
// names the fix.
type Finding struct {
	Site
	Reason string
}

// Module is one go.work module's share of a Scan.
type Module struct {
	// Name is the normalized go.work `use` path (see Modules).
	Name string
	// TestFiles counts the `_test.go` files scanned in this module.
	TestFiles int
	// AllowedCalls counts runtime.Caller calls on the blank-file allow path.
	AllowedCalls int
}

// Result is the outcome of Scan over every go.work module.
type Result struct {
	Findings     []Finding
	Allowed      []Site
	Modules      []Module
	ScannedFiles int
	AllowedCalls int
}

const (
	reasonFileBound = "runtime.Caller's file result is bound; under -trimpath it is module-relative and misses every fixture — derive the path from the package's init-captured pkgSrcDir instead"
	reasonUnproven  = "runtime.Caller used where the guard cannot prove the file result is discarded (only `pc, _, _, ok := runtime.Caller(n)` is allowed) — derive any fixture path from the package's init-captured pkgSrcDir instead"
)

// RepoRoot walks up from start to the directory containing go.work and returns
// it. It FAILS CLOSED with an error naming go.work and start when the walk
// reaches the filesystem root without one.
func RepoRoot(start string) (string, error) {
	abs, err := filepath.Abs(start)
	if err != nil {
		return "", fmt.Errorf("testanchor: resolve %q: %w", start, err)
	}
	for dir := abs; ; {
		if fi, err := os.Stat(filepath.Join(dir, "go.work")); err == nil && !fi.IsDir() {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("testanchor: no go.work at or above %q: the guard scans every go.work module and requires the full repo tree", start)
		}
		dir = parent
	}
}

// normalizeUse is the one normalization applied to a go.work `use` path and to
// a module name passed to RequireModules, so the two match on the cleaned
// path: unquote a quoted entry, strip a leading "./", then filepath.Clean and
// convert to slash form ("./backend", "backend/" and "backend" all become
// "backend"). The "./" strip is redundant with Clean for a relative path; it
// is kept because approval condition 3 names both steps.
func normalizeUse(entry string) string {
	if uq, err := strconv.Unquote(entry); err == nil {
		entry = uq
	}
	entry = strings.TrimPrefix(entry, "./")
	return filepath.ToSlash(filepath.Clean(entry))
}

// Modules parses root/go.work and returns its `use` paths, normalized by
// normalizeUse, in file order. It reads both the single-line `use ./x` and the
// block `use ( ... )` forms, drops trailing `//` comments and comment-only
// lines, and ignores the entries of any other block directive (`replace (`).
// It FAILS CLOSED on an unreadable go.work, on zero `use` entries, and on a
// `use` path that is not an existing directory.
func Modules(root string) ([]string, error) {
	raw, err := os.ReadFile(filepath.Join(root, "go.work"))
	if err != nil {
		return nil, fmt.Errorf("testanchor: read go.work: %w", err)
	}
	var mods []string
	block := "" // the directive of the open `name (` block, if any
	for _, line := range strings.Split(string(raw), "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if block != "" {
			if line == ")" {
				block = ""
			} else if block == "use" {
				mods = append(mods, normalizeUse(line))
			}
			continue
		}
		directive, rest, _ := strings.Cut(line, " ")
		rest = strings.TrimSpace(rest)
		switch {
		case rest == "(":
			block = directive
		case directive == "use":
			mods = append(mods, normalizeUse(rest))
		}
	}
	if len(mods) == 0 {
		return nil, fmt.Errorf("testanchor: %s/go.work declares no `use` modules: the guard would scan nothing", root)
	}
	for _, m := range mods {
		if fi, err := os.Stat(moduleDir(root, m)); err != nil || !fi.IsDir() {
			return nil, fmt.Errorf("testanchor: go.work `use %s` is not an existing directory under %s", m, root)
		}
	}
	return mods, nil
}

func moduleDir(root, name string) string {
	if filepath.IsAbs(name) {
		return name
	}
	return filepath.Join(root, filepath.FromSlash(name))
}

// Scan walks every go.work module under root and inspects each `_test.go`
// file. Directories the Go tool ignores (testdata, vendor, node_modules, and
// names starting with "." or "_"), files starting with "." or "_", and nested
// directories that are themselves go.work modules (scanned under their own
// entry) are skipped. A file that does not parse FAILS the scan with an error
// naming it.
func Scan(root string) (Result, error) {
	names, err := Modules(root)
	if err != nil {
		return Result{}, err
	}
	modDirs := make(map[string]bool, len(names))
	for _, n := range names {
		modDirs[moduleDir(root, n)] = true
	}
	var res Result
	fset := token.NewFileSet()
	for _, name := range names {
		mod := Module{Name: name}
		dir := moduleDir(root, name)
		walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			base := d.Name()
			if d.IsDir() {
				if path != dir && (modDirs[path] || base == "testdata" || base == "vendor" || base == "node_modules" ||
					strings.HasPrefix(base, ".") || strings.HasPrefix(base, "_")) {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(base, "_test.go") || strings.HasPrefix(base, ".") || strings.HasPrefix(base, "_") {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return fmt.Errorf("testanchor: parse %s: %w", rel, err)
			}
			mod.TestFiles++
			findings, allowed := inspectFile(fset, f, rel)
			res.Findings = append(res.Findings, findings...)
			res.Allowed = append(res.Allowed, allowed...)
			mod.AllowedCalls += len(allowed)
			return nil
		})
		if walkErr != nil {
			return Result{}, fmt.Errorf("testanchor: scan module %s: %w", name, walkErr)
		}
		res.Modules = append(res.Modules, mod)
		res.ScannedFiles += mod.TestFiles
		res.AllowedCalls += mod.AllowedCalls
	}
	return res, nil
}

// inspectFile reports every runtime.Caller reference in f. It resolves the
// file's local name(s) for the "runtime" import — default, aliased, or a dot
// import (a bare Caller call); a blank import binds nothing — and allows only
// a call that is the sole right-hand side of a four-value assignment or var
// spec whose SECOND target is the blank identifier.
func inspectFile(fset *token.FileSet, f *ast.File, rel string) ([]Finding, []Site) {
	local := map[string]bool{}
	dot := false
	for _, imp := range f.Imports {
		if p, err := strconv.Unquote(imp.Path.Value); err != nil || p != "runtime" {
			continue
		}
		switch {
		case imp.Name == nil:
			local["runtime"] = true
		case imp.Name.Name == ".":
			dot = true
		case imp.Name.Name != "_":
			local[imp.Name.Name] = true
		}
	}
	if len(local) == 0 && !dot {
		return nil, nil
	}
	isCallerRef := func(e ast.Expr) bool {
		switch x := e.(type) {
		case *ast.SelectorExpr:
			id, ok := x.X.(*ast.Ident)
			return ok && local[id.Name] && x.Sel.Name == "Caller"
		case *ast.Ident:
			return dot && x.Name == "Caller"
		}
		return false
	}
	asCall := func(e ast.Expr) *ast.CallExpr {
		if c, ok := ast.Unparen(e).(*ast.CallExpr); ok && isCallerRef(c.Fun) {
			return c
		}
		return nil
	}

	// classified maps each Caller call that is the sole RHS of a 4-value
	// assignment to whether its file (2nd) target is blank. ast.Inspect is
	// pre-order, so the statement is classified before its call is visited.
	classified := map[*ast.CallExpr]bool{}
	classify := func(lhs []ast.Expr, rhs []ast.Expr) {
		if len(rhs) != 1 || len(lhs) != 4 {
			return
		}
		if c := asCall(rhs[0]); c != nil {
			id, ok := lhs[1].(*ast.Ident)
			classified[c] = ok && id.Name == "_"
		}
	}
	var findings []Finding
	var allowed []Site
	handled := map[ast.Expr]bool{} // Caller refs already reported via their call
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			classify(x.Lhs, x.Rhs)
		case *ast.ValueSpec:
			lhs := make([]ast.Expr, len(x.Names))
			for i, id := range x.Names {
				lhs[i] = id
			}
			classify(lhs, x.Values)
		case *ast.CallExpr:
			if !isCallerRef(x.Fun) {
				return true
			}
			handled[x.Fun] = true
			site := Site{Path: rel, Line: fset.Position(x.Pos()).Line}
			blankFile, ok := classified[x]
			switch {
			case ok && blankFile:
				allowed = append(allowed, site)
			case ok:
				findings = append(findings, Finding{Site: site, Reason: reasonFileBound})
			default:
				findings = append(findings, Finding{Site: site, Reason: reasonUnproven})
			}
		case *ast.SelectorExpr:
			// A non-call reference (a method value such as `f := runtime.Caller`)
			// escapes the call-site analysis, so it is rejected as unproven.
			if isCallerRef(x) && !handled[x] {
				findings = append(findings, Finding{
					Site:   Site{Path: rel, Line: fset.Position(x.Pos()).Line},
					Reason: reasonUnproven,
				})
			}
		}
		return true
	})
	return findings, allowed
}

// RequireModules is the anti-vacuity check: it returns an error when no module
// is named, when a named module (matched on its normalized go.work path) was
// not scanned, or when it scanned zero `_test.go` files.
func (r Result) RequireModules(names ...string) error {
	if len(names) == 0 {
		return errors.New("testanchor: RequireModules called with no module names")
	}
	byName := make(map[string]Module, len(r.Modules))
	for _, m := range r.Modules {
		byName[m.Name] = m
	}
	for _, n := range names {
		m, ok := byName[normalizeUse(n)]
		if !ok {
			return fmt.Errorf("testanchor: module %q is not a scanned go.work module", n)
		}
		if m.TestFiles == 0 {
			return fmt.Errorf("testanchor: module %q scanned zero _test.go files: the guard covers nothing there", n)
		}
	}
	return nil
}
