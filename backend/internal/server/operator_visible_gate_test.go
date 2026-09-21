package server

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/issuecomment"
)

// This file is the cross-package "writer intends the anchor" gate (#3406).
//
// issuecomment.activityCategories is a closed set in another package: an
// audit category a server writer appends and refreshes the status comment
// for, but never registers there, renders NOTHING on the anchor — silently.
// The same-file parity gate in backend/internal/issuecomment cannot see that
// shape (there is no case and no entry to compare). This gate closes it from
// the writer side by TYPE-CHECKING the server package's non-test files and
// sweeping every notifyOperatorVisible / notifyStatusUpdate call:
//
//   - every notifyOperatorVisible category must resolve (a string literal, or
//     an identifier / selector whose types.Info.Uses object is a string-kinded
//     *types.Const — a *types.Var, a parameter, a shadowing local, a call, or
//     a concatenation FAILS CLOSED naming file:line) and must be admitted by
//     issuecomment.RendersActivity AND audit.IsKnownCategory;
//   - no notifyStatusUpdate source may be an audit.KnownCategories member
//     unless auditOnlyStatusRefreshSources names it with a reason; an
//     exemption whose category has since become renderable fails as STALE,
//     and one no site references fails as ORPHANED.
//
// Resolution goes through go/types deliberately, never through an
// identifier-name table: a name-keyed lookup would resolve a shadowing local
// `category := "..."` to the package const of the same name and report the
// wrong value. TestCollectNotifyCalls_ShapeCoverage carries that exact row as
// the counterfactual.
//
// The sweep fails closed on every degrade (parse failure, type-check error,
// no files, an unresolvable argument) — a completeness gate that skips when
// it cannot tell still looks like coverage.

// notifyCall is one swept call site of notifyOperatorVisible /
// notifyStatusUpdate.
type notifyCall struct {
	fn       string // callee base name
	category string // resolved third argument, when resolved
	resolved bool   // false when the argument is not a literal / string const
	reason   string // why it did not resolve (unresolved rows only)
	pos      token.Position
}

// sweptNotifyFns is the set of callee base names the collector matches.
var sweptNotifyFns = map[string]struct{}{
	"notifyOperatorVisible": {},
	"notifyStatusUpdate":    {},
}

// typeCheckedPackage is a parsed + type-checked package plus the position
// information the collector reports against.
type typeCheckedPackage struct {
	fset  *token.FileSet
	files []*ast.File
	info  *types.Info
}

// serverPackageCheck caches the one type-check of the server package the
// gate tests share: the "source" importer type-checks every transitive
// dependency from source, which is the dominant cost, so it is paid once per
// test binary.
var serverPackageCheck struct {
	once sync.Once
	pkg  *typeCheckedPackage
	err  error
}

// loadTypeCheckedServerPackage parses every non-test .go file in this test's
// own directory and type-checks them with the source importer. Any parse or
// type error is fatal (never a skip).
func loadTypeCheckedServerPackage(t *testing.T) *typeCheckedPackage {
	t.Helper()
	serverPackageCheck.once.Do(func() {
		serverPackageCheck.pkg, serverPackageCheck.err = typeCheckServerPackageDir(".")
	})
	if serverPackageCheck.err != nil {
		t.Fatalf("type-checking backend/internal/server failed closed (not skipped): %v", serverPackageCheck.err)
	}
	return serverPackageCheck.pkg
}

func typeCheckServerPackageDir(dir string) (*typeCheckedPackage, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, fmt.Errorf("glob %s: %w", dir, err)
	}
	sort.Strings(paths)
	fset := token.NewFileSet()
	var files []*ast.File
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", p, err)
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no non-test .go files found under %s", dir)
	}
	var typeErrs []string
	conf := types.Config{
		Importer: importer.ForCompiler(fset, "source", nil),
		Error:    func(e error) { typeErrs = append(typeErrs, e.Error()) },
	}
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}}
	if _, err := conf.Check("github.com/kuhlman-labs/fishhawk/backend/internal/server", fset, files, info); err != nil {
		return nil, fmt.Errorf("type-check: %w (first errors: %s)", err, strings.Join(head(typeErrs, 5), "; "))
	}
	if len(typeErrs) > 0 {
		return nil, fmt.Errorf("type-check reported %d error(s): %s", len(typeErrs), strings.Join(head(typeErrs, 5), "; "))
	}
	return &typeCheckedPackage{fset: fset, files: files, info: info}, nil
}

func head(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// collectNotifyCalls sweeps every file in pkg for calls whose callee base
// name (a bare Ident or the Sel of a SelectorExpr) is a sweptNotifyFns
// member and resolves the third argument through go/types. Rows are returned
// in file order, then source order within a file.
func collectNotifyCalls(pkg *typeCheckedPackage) []notifyCall {
	var out []notifyCall
	for _, f := range pkg.files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var name string
			switch fn := call.Fun.(type) {
			case *ast.Ident:
				name = fn.Name
			case *ast.SelectorExpr:
				name = fn.Sel.Name
			default:
				return true
			}
			if _, swept := sweptNotifyFns[name]; !swept {
				return true
			}
			row := notifyCall{fn: name, pos: pkg.fset.Position(call.Pos())}
			if len(call.Args) < 3 {
				row.reason = fmt.Sprintf("call has %d argument(s), expected the category as the third", len(call.Args))
			} else {
				row.category, row.resolved, row.reason = resolveStringArg(call.Args[2], pkg.info)
			}
			out = append(out, row)
			return true
		})
	}
	return out
}

// resolveStringArg accepts ONLY (a) a string BasicLit, or (b) an Ident or
// SelectorExpr whose types.Info.Uses object is a string-kinded *types.Const,
// taking the value with constant.StringVal. Everything else — a *types.Var
// (local, parameter, field), a call, a concatenation, a conversion — is
// reported unresolved with a reason, so the caller fails closed.
func resolveStringArg(arg ast.Expr, info *types.Info) (value string, ok bool, reason string) {
	var ident *ast.Ident
	switch e := arg.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false, fmt.Sprintf("literal of kind %s, not a string", e.Kind)
		}
		v, err := strconv.Unquote(e.Value)
		if err != nil {
			return "", false, fmt.Sprintf("unquote %s: %v", e.Value, err)
		}
		return v, true, ""
	case *ast.Ident:
		ident = e
	case *ast.SelectorExpr:
		ident = e.Sel
	default:
		return "", false, fmt.Sprintf("argument is a %T, not a string literal or a string constant", arg)
	}
	obj := info.Uses[ident]
	if obj == nil {
		return "", false, fmt.Sprintf("identifier %q resolves to no object in types.Info.Uses", ident.Name)
	}
	c, isConst := obj.(*types.Const)
	if !isConst {
		return "", false, fmt.Sprintf("identifier %q resolves to a %T, not a *types.Const", ident.Name, obj)
	}
	basic, isBasic := c.Type().Underlying().(*types.Basic)
	if !isBasic || basic.Info()&types.IsString == 0 {
		return "", false, fmt.Sprintf("constant %q has type %s, not a string kind", ident.Name, c.Type())
	}
	if c.Val().Kind() != constant.String {
		return "", false, fmt.Sprintf("constant %q has a %s value, not a string", ident.Name, c.Val().Kind())
	}
	return constant.StringVal(c.Val()), true, ""
}

// auditOnlyStatusRefreshSources names every notifyStatusUpdate source that
// is ALSO an audit.KnownCategories member and is deliberately kept
// audit-only: the writer refreshes the status comment for a state transition
// that shares its name with the audit row it appended, but the category is
// not admitted by issuecomment.activityCategories, so the row itself does
// not render on the anchor timeline. Each entry carries the reason the
// category stays audit-only TODAY (inspected per site in #3406); every one is
// a candidate for a render follow-up, at which point its site converts to
// notifyOperatorVisible and the exemption is dropped — the STALE check below
// forces that drop the moment the category becomes renderable, and the
// ORPHAN check drops an entry no site references any more.
var auditOnlyStatusRefreshSources = map[string]string{
	"acceptance_recorded":                 "transition tag of the acceptance-verdict handler (acceptance.go); the row that site appends is acceptance_outcome_recorded, which already renders — no backend writer emits an acceptance_recorded row, the Known entry is a reservation",
	"acceptance_reopened":                 "appended when a fix-up push or a stage retry invalidates a settled acceptance verdict (acceptance.go, retry.go); the acceptance stage row flipping back to pending on the status comment is the operator-facing signal",
	"stage_conflict_resolution_failed":    "appended when the runner reports a failed conflict-resolution pass (conflictresolution.go); the stage's failed row on the status comment carries the outcome, the audit row holds the diagnostic detail",
	"stage_conflict_resolution_triggered": "appended when the implement stage is re-opened for a conflict-resolution pass (conflictresolution_trigger.go); the re-opened stage row is the visible signal, the row records the spent budget slot",
	"conflict_resolution_pushed":          "ledger row for the runner's conflict-resolution commit (pullrequest.go); the PR head advance and the re-review it triggers are what the operator watches",
	"acceptance_scenarios_pushed":         "ledger row for the runner's scenario-corpus commit (pullrequest.go); the acceptance verdict that follows, not the push, is the operator-facing event",
	"acceptance_verdict_unshipped":        "reap-time bookkeeping that an acceptance verdict never reached the backend (reap_failure.go); surfaced through the stage's failed state, the row is forensic",
	"stage_fixup_recovered":               "recovery of a fix-up stage stranded by a prior failure (fixup.go); the stage row returning to a live state is the visible signal, the row records the closure",
	"lineage_violation":                   "transition tag of the foreign-commit detection paths (lineage.go); the row those paths append is invariantmonitor.CategoryInvariantViolation — no backend writer emits a lineage_violation row, the Known entry is a reservation",
	"branch_rebased":                      "operator-driven rebase of the run branch (rebase_branch.go); the base advance is visible on the PR and in the REST response, the row is a ledger entry",
	"branch_reset":                        "operator-driven reset of a contaminated run branch (reset_branch.go); the dropped commit is named in the REST response, the row is the recovery ledger",
	"child_pushed":                        "ledger row for a decomposition child's push (pullrequest.go); the parent's slices_integrated / slice_integration_conflict outcome is the rendered event",
	"fixup_no_changes":                    "a fix-up pass that produced no diff (pullrequest.go); the review gate stays where it was and the row records why — the strongest render-follow-up candidate here",
	"plan_revised":                        "operator-driven plan revision (revise.go); the plan stage flips back to pending on the status comment and the revised plan re-posts via the plan-ready path",
}

// TestNotifyOperatorVisibleCategoriesRender asserts every
// notifyOperatorVisible call site's category resolves and is admitted by
// BOTH issuecomment.RendersActivity and audit.IsKnownCategory.
func TestNotifyOperatorVisibleCategoriesRender(t *testing.T) {
	pkg := loadTypeCheckedServerPackage(t)
	var sites []notifyCall
	for _, c := range collectNotifyCalls(pkg) {
		if c.fn == "notifyOperatorVisible" {
			sites = append(sites, c)
		}
	}
	// Vacuity floor: six sites exist today; a broken walk that found fewer
	// than five must not pass by finding nothing.
	const minSites = 5
	if len(sites) < minSites {
		t.Fatalf("collected only %d notifyOperatorVisible site(s) (< %d); the AST sweep is likely broken and this gate would otherwise false-pass", len(sites), minSites)
	}
	for _, c := range sites {
		if !c.resolved {
			t.Errorf("%s: notifyOperatorVisible category argument is not resolvable (%s); pass a string literal or a string constant (in-package or imported) so the gate can check it", c.pos, c.reason)
			continue
		}
		if !issuecomment.RendersActivity(c.category) {
			t.Errorf("%s: notifyOperatorVisible(%q) but the category is NOT admitted by issuecomment.activityCategories — it would render nothing on the anchor; "+
				"add `%q: {}` to activityCategories AND a `case %q:` to renderActivityLine in backend/internal/issuecomment/status_template.go",
				c.pos, c.category, c.category, c.category)
		}
		if !audit.IsKnownCategory(c.category) {
			t.Errorf("%s: notifyOperatorVisible(%q) but the category is NOT in audit.KnownCategories; register it in backend/internal/audit/categories.go",
				c.pos, c.category)
		}
	}
}

// TestNotifyStatusUpdateSourceIsNotAnAuditCategory asserts no
// notifyStatusUpdate source (outside the delegating call in
// operator_visible.go) is an audit.KnownCategories member unless
// auditOnlyStatusRefreshSources names it with a reason, and that the
// exemption table itself is neither stale nor orphaned.
func TestNotifyStatusUpdateSourceIsNotAnAuditCategory(t *testing.T) {
	pkg := loadTypeCheckedServerPackage(t)
	var sites []notifyCall
	for _, c := range collectNotifyCalls(pkg) {
		if c.fn != "notifyStatusUpdate" || filepath.Base(c.pos.Filename) == "operator_visible.go" {
			continue
		}
		sites = append(sites, c)
	}
	// Vacuity floor: ~40 sites exist today; guard a broken walk, not a count.
	const minSites = 30
	if len(sites) < minSites {
		t.Fatalf("collected only %d notifyStatusUpdate site(s) (< %d); the AST sweep is likely broken and this gate would otherwise false-pass", len(sites), minSites)
	}
	referenced := map[string]struct{}{}
	for _, c := range sites {
		if !c.resolved {
			t.Errorf("%s: notifyStatusUpdate source argument is not resolvable (%s); pass a string literal or a string constant so the gate can check it", c.pos, c.reason)
			continue
		}
		referenced[c.category] = struct{}{}
		if !audit.IsKnownCategory(c.category) {
			continue
		}
		if _, exempt := auditOnlyStatusRefreshSources[c.category]; exempt {
			continue
		}
		t.Errorf("%s: notifyStatusUpdate source %q is an audit.KnownCategories member — naming an audit category as a refresh trigger must be an explicit choice. "+
			"Either convert the site to notifyOperatorVisible and register %q in issuecomment.activityCategories (backend/internal/issuecomment/status_template.go), "+
			"or add a reasoned auditOnlyStatusRefreshSources entry in backend/internal/server/operator_visible_gate_test.go",
			c.pos, c.category, c.category)
	}
	for category, reason := range auditOnlyStatusRefreshSources {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("auditOnlyStatusRefreshSources[%q] has an empty reason; every exemption states why the category stays audit-only", category)
		}
		if issuecomment.RendersActivity(category) {
			t.Errorf("auditOnlyStatusRefreshSources[%q] is STALE: the category now renders on the anchor; convert its notifyStatusUpdate site(s) to notifyOperatorVisible and drop the exemption", category)
		}
		if !audit.IsKnownCategory(category) {
			t.Errorf("auditOnlyStatusRefreshSources[%q] is not an audit.KnownCategories member; the exemption is inert — drop it", category)
		}
		if _, ok := referenced[category]; !ok {
			t.Errorf("auditOnlyStatusRefreshSources[%q] is ORPHANED: no notifyStatusUpdate site passes it any more; drop the exemption", category)
		}
	}
}

// shapeFixtureOther is the imported package of the shape fixture: it carries
// the cross-package string constant a SelectorExpr must resolve through
// types.Info.Uses (the audit.CategoryX shape).
const shapeFixtureOther = `package other

const Cross = "cross_pkg_const"
`

// shapeFixture is a synthetic package exercising every argument shape the
// collector must classify. The `category` rows are the SHADOWING
// counterfactual for name-keyed resolution: the package const is
// "fixup_pushed", and shadowing() declares a local of the same name — the
// local's row must NEVER resolve to "fixup_pushed".
const shapeFixture = `package fixture

import "other"

const category = "fixup_pushed"
const pkgConst = "pkg_const_value"
const notString = 7

type Server struct{ field string }

func (s *Server) notifyOperatorVisible(ctx any, runID any, category string) {}
func (s *Server) notifyStatusUpdate(ctx any, runID any, source string)      {}
func (s *Server) notifyPageClass(ctx any, runID any, source string)         {}
func (s *Server) notify(ctx any, runID any, source string)                  {}
func pick() string                                                          { return "x" }

func (s *Server) sites(ctx any, id any, param string) {
	s.notifyOperatorVisible(ctx, id, "literal_ov")
	s.notifyOperatorVisible(ctx, id, pkgConst)
	s.notifyOperatorVisible(ctx, id, other.Cross)
	s.notifyOperatorVisible(ctx, id, category)
	local := "local_var"
	s.notifyOperatorVisible(ctx, id, local)
	s.notifyOperatorVisible(ctx, id, param)
	s.notifyOperatorVisible(ctx, id, s.field)
	s.notifyOperatorVisible(ctx, id, pick())
	s.notifyOperatorVisible(ctx, id, "con"+"cat")
	s.notifyOperatorVisible(ctx, id, string(rune(notString)))
	s.notifyStatusUpdate(ctx, id, "literal_su")
	s.notifyStatusUpdate(ctx, id, pkgConst)
	s.notifyPageClass(ctx, id, "page_class_not_collected")
	s.notify(ctx, id, "unrelated_not_collected")
}

func (s *Server) shadowing(ctx any, id any) {
	category := "unregistered_category"
	s.notifyOperatorVisible(ctx, id, category)
}
`

// mapImporter resolves the fixture's import from an in-memory package set.
type mapImporter map[string]*types.Package

func (m mapImporter) Import(path string) (*types.Package, error) {
	p, ok := m[path]
	if !ok {
		return nil, fmt.Errorf("fixture importer: unknown package %q", path)
	}
	return p, nil
}

// typeCheckFixture type-checks the two-package shape fixture in memory.
func typeCheckFixture(t *testing.T) *typeCheckedPackage {
	t.Helper()
	fset := token.NewFileSet()
	otherFile, err := parser.ParseFile(fset, "other.go", shapeFixtureOther, 0)
	if err != nil {
		t.Fatalf("parse fixture other.go: %v", err)
	}
	otherPkg, err := (&types.Config{}).Check("other", fset, []*ast.File{otherFile}, nil)
	if err != nil {
		t.Fatalf("type-check fixture other: %v", err)
	}
	f, err := parser.ParseFile(fset, "fixture.go", shapeFixture, 0)
	if err != nil {
		t.Fatalf("parse fixture.go: %v", err)
	}
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}}
	conf := types.Config{Importer: mapImporter{"other": otherPkg}}
	if _, err := conf.Check("fixture", fset, []*ast.File{f}, info); err != nil {
		t.Fatalf("type-check fixture: %v", err)
	}
	return &typeCheckedPackage{fset: fset, files: []*ast.File{f}, info: info}
}

// TestCollectNotifyCalls_ShapeCoverage is the collector's self-test: a
// future edit that drops a shape (or starts resolving one it must not) fails
// here rather than degrading the main gates into a false pass. The rows are
// asserted in source order as exact (fn, category, resolved) triples.
func TestCollectNotifyCalls_ShapeCoverage(t *testing.T) {
	got := collectNotifyCalls(typeCheckFixture(t))
	type row struct {
		fn, category string
		resolved     bool
	}
	want := []row{
		{"notifyOperatorVisible", "literal_ov", true},
		{"notifyOperatorVisible", "pkg_const_value", true},
		{"notifyOperatorVisible", "cross_pkg_const", true}, // SelectorExpr → imported *types.Const
		{"notifyOperatorVisible", "fixup_pushed", true},    // the unshadowed package const
		{"notifyOperatorVisible", "", false},               // local variable
		{"notifyOperatorVisible", "", false},               // parameter
		{"notifyOperatorVisible", "", false},               // field selector (*types.Var)
		{"notifyOperatorVisible", "", false},               // call
		{"notifyOperatorVisible", "", false},               // concatenation
		{"notifyOperatorVisible", "", false},               // conversion
		{"notifyStatusUpdate", "literal_su", true},
		{"notifyStatusUpdate", "pkg_const_value", true},
		{"notifyOperatorVisible", "", false}, // SHADOWING row: local `category`, never "fixup_pushed"
	}
	if len(got) != len(want) {
		t.Fatalf("collected %d rows, want %d:\n%s", len(got), len(want), formatRows(got))
	}
	for i := range want {
		g := row{got[i].fn, got[i].category, got[i].resolved}
		if g != want[i] {
			t.Errorf("row %d at %s: got %+v (reason: %s), want %+v", i, got[i].pos, g, got[i].reason, want[i])
		}
	}
	// The shadowing row is the counterfactual for name-keyed resolution: a
	// collector that looked the identifier up by name would report the
	// package const's value here.
	last := got[len(got)-1]
	if last.category == "fixup_pushed" || last.resolved {
		t.Errorf("shadowing row at %s resolved to %q (resolved=%v); a local shadowing a package const must never resolve to the const's value", last.pos, last.category, last.resolved)
	}
	if !strings.Contains(last.reason, "*types.Var") {
		t.Errorf("shadowing row reason = %q, want it to name the *types.Var it resolved to", last.reason)
	}
	for _, c := range got {
		if strings.Contains(c.category, "not_collected") {
			t.Errorf("over-collected %s(%q) at %s; only notifyOperatorVisible / notifyStatusUpdate are swept", c.fn, c.category, c.pos)
		}
	}
}

func formatRows(rows []notifyCall) string {
	var b strings.Builder
	for i, r := range rows {
		fmt.Fprintf(&b, "  %d: %s %s(%q) resolved=%v %s\n", i, r.pos, r.fn, r.category, r.resolved, r.reason)
	}
	return b.String()
}
