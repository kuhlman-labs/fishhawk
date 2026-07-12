package audit_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
)

// TestKnownCategoriesCoversEmittedCategories is the completeness guard for the
// audit-category registry (#1850). It AST-walks every non-test Go file under
// the backend module and collects the audit-category string literals that
// backend code actually emits, then asserts each is registered in
// audit.KnownCategories. Because GET /v0/runs/{run_id}/audit and
// fishhawk_await_audit reject any ?category= absent from the registry with a
// 400 (unless allow_unknown=true), a category emitted by the backend but
// missing from the registry is an unreadable audit stream — the exact
// fishhawk_get_plan-on-plan_test_sweep failure #1850 reported. Running in the
// committed-tree verify gate, this fails IN-LOOP when a new audit write skips
// the registry, rather than as a production get_plan 400 after the fact.
//
// The sweep collects five distinct emit shapes (see collectEmittedCategories);
// TestCollectEmittedCategories_ShapeCoverage is the fixture self-test proving
// each shape is actually exercised by the collector, so a future refactor that
// silently stops recognizing one shape fails there rather than degrading this
// sweep into a false pass.
func TestKnownCategoriesCoversEmittedCategories(t *testing.T) {
	backendRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("abs path: %v", err)
	}

	skipDirNames := map[string]struct{}{
		"node_modules": {},
		".git":         {},
	}

	fset := token.NewFileSet()
	var missing []string
	seen := map[string]struct{}{}
	collectedKnown := map[string]struct{}{}

	walkErr := filepath.WalkDir(backendRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if _, skip := skipDirNames[d.Name()]; skip {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		rel, _ := filepath.Rel(backendRoot, path)
		for _, c := range collectEmittedCategories(f) {
			if audit.IsKnownCategory(c.value) {
				collectedKnown[c.value] = struct{}{}
				continue
			}
			key := rel + ":" + c.value
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			missing = append(missing, rel+": emits audit category "+strconv.Quote(c.value)+
				" (shape="+c.shape+") that is absent from audit.KnownCategories")
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk: %v", walkErr)
	}

	// Guard against a silent false pass: if a walk/parse regression collected
	// nothing, `missing` would be empty and the sweep would vacuously pass. The
	// backend emits well over this many distinct known categories, so a floor
	// far below the true count still catches a collector that found ~nothing.
	const minCollected = 20
	if len(collectedKnown) < minCollected {
		t.Fatalf("sweep collected only %d known categories from backend source (< %d); "+
			"the AST walk is likely broken and this test would false-pass", len(collectedKnown), minCollected)
	}

	if len(missing) > 0 {
		t.Errorf("audit categories emitted by backend code are missing from the registry "+
			"(add them to backend/internal/audit/categories.go):\n  %s",
			strings.Join(missing, "\n  "))
	}
}

// TestKnownCategoriesRegistersIssueNamedCategories pins, independent of the AST
// sweep, that the specific categories #1850 named as broken reads are
// registered. This is a direct-assertion backstop: even if the sweep's AST
// matching regressed and silently collected nothing, these must stay green.
func TestKnownCategoriesRegistersIssueNamedCategories(t *testing.T) {
	// The #1850 report plus the two additional gaps the shape-(e) sweep
	// surfaced (refinement_draft_edited / refinement_filing_completed, both
	// emitted via appendRefinementAudit → AppendGlobalChained).
	named := []string{
		"plan_test_sweep",
		"api_token_issued",
		"api_token_revoked",
		"refinement_draft_approved",
		"refinement_draft_rejected",
		"refinement_draft_edited",
		"refinement_filing_completed",
		"runner_kind_mismatch",
		"runner_kind_resolved",
		"work_item_filed",
		"work_item_transitioned",
		"campaign_advanced",
		"campaign_gate_acted",
		"campaign_issue_started",
		"campaign_issue_settled",
		"campaign_issue_restarted",
		"campaign_paused",
		"deployment_dispatch_failed",
		"plan_acceptance_precheck",
		"plan_scope_regression",
		"product_report_filed",
	}
	for _, c := range named {
		if !audit.IsKnownCategory(c) {
			t.Errorf("IsKnownCategory(%q) = false; #1850 requires this category be registered", c)
		}
	}
}

// collected is one audit-category string literal the sweep found, tagged with
// the syntactic shape it was collected from (used by the fixture self-test to
// prove every shape is exercised).
type collected struct {
	value string
	shape string
}

// Emit-shape tags.
const (
	shapeValueSpec    = "value-spec"         // const/var whose name binds a category
	shapeAssign       = "assign"             // category := / category = string literal
	shapeLogTokenArg  = "log-token-event"    // logTokenEvent(r, "cat", …)
	shapeCompositeCat = "composite-category" // T{Category: "cat"} struct field
	shapeEmitArg      = "emit-arg"           // append*Audit / emitReview*(…, "cat", …)
)

// categoryValueRe bounds a collected literal to the canonical audit-category
// lexical shape: lowercase snake_case starting with a letter. This excludes
// run.FailureCategory's "A".."D" single-letter values (uppercase → no match),
// empty strings, and any mixed-case/identifier junk, so only real category
// literals are asserted against the registry.
var categoryValueRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// isCategoryBindingName reports whether an identifier name binds an audit
// category — its name contains "category" case-insensitively — while excluding
// the planreview concern-classifier family (operatorConcernCategory,
// requirementConcernCategory), which are free-form concern labels, not audit
// categories. Go's regexp has no lookahead, so the exclusion is a suffix test
// rather than a negative pattern.
func isCategoryBindingName(name string) bool {
	if !strings.Contains(strings.ToLower(name), "category") {
		return false
	}
	if strings.HasSuffix(name, "ConcernCategory") {
		return false
	}
	return true
}

// auditLiteralTypes are the audit-entry / append-params struct types whose
// Category-keyed composite-literal field holds a real audit category — the
// binding-condition-1 `audit.Entry{Category: "x"}` shape (d). Restricting the
// composite-literal sweep to these types (matched by the type's base name,
// so both qualified `audit.Entry{…}` and in-package `Entry{…}` count) excludes
// unrelated structs with a Category field — notably
// planreview.Concern{Category: "acceptance"}, a concern classifier, NOT an
// audit category.
var auditLiteralTypes = map[string]bool{
	"Entry":                   true,
	"AppendParams":            true,
	"GlobalChainAppendParams": true,
	"ChainAppendParams":       true,
}

// auditEmitCategoryArg maps an audit-emit helper's base name to the ARGUMENT
// INDEX at which it receives a positional category string literal — the
// binding-condition-1 "direct audit-append call arguments" shape (e). A
// per-helper index (rather than "collect every string arg") is required
// because these helpers ALSO take same-looking snake_case classifier args that
// are NOT categories: writePlanReusedFromAudit(r, childID, parentRunID,
// source, …) passes source="operator_recovery"/"decomposition_child_recovery"
// (a payload field), while its real category is the CategoryPlanReusedFrom
// const (caught by shape a). Collecting only the category-position arg is what
// keeps this shape from misclassifying those payload strings, and is why
// writePlanReusedFromAudit is deliberately ABSENT from this map. The
// repository Append/AppendChained/AppendGlobalChained methods are likewise
// absent: they take a params struct, so their Category is a struct field
// (shape d), never a positional literal.
//
// A new positional-category emit helper must be registered here to be swept;
// that is the acknowledged bound of shape (e). The const/var/assign/struct-
// field shapes (a/b/c/d) — which cover how nearly every category is emitted —
// need no such registration.
var auditEmitCategoryArg = map[string]int{
	"appendConsolidateAudit":  3, // (ctx, runID, stageID, category, payload)
	"appendRefinementAudit":   1, // (r, category, payload)
	"emitReviewStarted":       3, // (ctx, runID, stageID, category, …)
	"emitReviewerUnavailable": 3, // (ctx, runID, stageID, category, …)
	"emitReviewFailed":        3, // (ctx, runID, stageID, category, …)
}

// compositeLitTypeName returns the base name of a composite-literal type:
// "Entry" for Entry{…} or audit.Entry{…}. Empty for map/slice/array literals.
func compositeLitTypeName(t ast.Expr) string {
	switch e := t.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return e.Sel.Name
	}
	return ""
}

// collectEmittedCategories walks one parsed Go file and returns every audit-
// category string literal it emits, across five shapes:
//
//	(a) shapeValueSpec    — a const/var whose name binds a category:
//	                        `const categoryPlanTestSweep = "plan_test_sweep"`.
//	(b) shapeAssign       — an assignment to a category-named ident:
//	                        `category = "runner_kind_mismatch"`.
//	(c) shapeLogTokenArg  — logTokenEvent's 2nd arg: the token surface emits
//	                        api_token_issued/revoked as a bare literal, not via
//	                        a category-named binding.
//	(d) shapeCompositeCat — a Category-keyed field of an audit-struct composite
//	                        literal: `audit.Entry{Category: "x"}` (binding
//	                        condition 1), restricted to auditLiteralTypes.
//	(e) shapeEmitArg      — the positional category arg of an audit-emit helper
//	                        (auditEmitCategoryArg: appendConsolidateAudit /
//	                        appendRefinementAudit / emitReview*), the shape that
//	                        surfaced refinement_draft_edited &
//	                        refinement_filing_completed (binding condition 1:
//	                        direct audit-append args).
//
// All collected literals are filtered through categoryValueRe, so non-category
// literals (empty strings, single-letter FailureCategory values, payload junk)
// are dropped and never asserted against the registry.
func collectEmittedCategories(f *ast.File) []collected {
	var out []collected
	add := func(shape string, expr ast.Expr) {
		lit, ok := expr.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return
		}
		v, err := strconv.Unquote(lit.Value)
		if err != nil {
			return
		}
		if !categoryValueRe.MatchString(v) {
			return
		}
		out = append(out, collected{value: v, shape: shape})
	}

	ast.Inspect(f, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.ValueSpec: // (a)
			for i, name := range node.Names {
				if isCategoryBindingName(name.Name) && i < len(node.Values) {
					add(shapeValueSpec, node.Values[i])
				}
			}
		case *ast.AssignStmt: // (b)
			for i, lhs := range node.Lhs {
				id, ok := lhs.(*ast.Ident)
				if ok && isCategoryBindingName(id.Name) && i < len(node.Rhs) {
					add(shapeAssign, node.Rhs[i])
				}
			}
		case *ast.CompositeLit: // (d)
			if auditLiteralTypes[compositeLitTypeName(node.Type)] {
				for _, elt := range node.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Category" {
						add(shapeCompositeCat, kv.Value)
					}
				}
			}
		case *ast.CallExpr:
			name := calleeBaseName(node.Fun)
			if name == "logTokenEvent" { // (c)
				if len(node.Args) >= 2 {
					add(shapeLogTokenArg, node.Args[1])
				}
			} else if idx, ok := auditEmitCategoryArg[name]; ok && idx < len(node.Args) { // (e)
				add(shapeEmitArg, node.Args[idx])
			}
		}
		return true
	})
	return out
}

// calleeBaseName returns the un-qualified function name of a call target:
// "f" for f(), "Sel" for pkg.Sel() or recv.Sel(). Empty for anything else.
func calleeBaseName(fun ast.Expr) string {
	switch e := fun.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return e.Sel.Name
	}
	return ""
}

// TestCollectEmittedCategories_ShapeCoverage is the negative fixture-style
// self-test binding condition (1) requires: a synthetic source exercising ALL
// FIVE collection shapes, asserting each shape's literal is collected AND
// tagged with the right shape. If a future edit stops recognizing any one
// shape, its expected entry vanishes here and this test fails — so the main
// sweep can never silently degrade into a false pass by dropping a shape. The
// negative cases (a ConcernCategory-suffixed binding, an uppercase
// FailureCategory value, an empty string, a non-Category struct field) assert
// the collector does NOT over-collect.
func TestCollectEmittedCategories_ShapeCoverage(t *testing.T) {
	const src = `package fixture

// (a) value-spec: category-named const binds a literal.
const categoryFixtureA = "shape_value_spec"

// negative: a ConcernCategory-suffixed binding is a concern classifier, NOT
// an audit category — must be excluded.
const operatorConcernCategory = "operator"

// negative: FailureCategory constants bind uppercase single letters to a
// name WITHOUT "category" (FailureA); the value also fails the lowercase
// snake_case filter.
type FailureCategory string
const FailureA FailureCategory = "A"

func emit() {
	// (b) assign: category-named ident assigned a literal.
	category := "shape_assign"
	category = "shape_assign_reassigned"

	// (c) logTokenEvent 2nd arg.
	logTokenEvent(nil, "shape_log_token", nil)

	// (d) Category-keyed field of an audit-struct composite literal.
	_ = Entry{Category: "shape_composite"}
	// negative: a Category field on a NON-audit struct (concern classifier)
	// is not collected.
	_ = Concern{Category: "not_a_category"}
	// negative: a non-Category field on an audit struct is not collected.
	_ = Entry{Payload: "not_a_category"}

	// (e) positional category arg (index-precise) to an audit-emit helper.
	appendRefinementAudit(nil, "shape_emit_arg", nil)
	emitReviewStarted(nil, nil, nil, "shape_emit_review", nil)
	// negative: a same-looking snake_case literal at a NON-category arg index
	// (here arg 4, the payload) is not collected.
	appendConsolidateAudit(nil, nil, nil, "shape_consolidate_cat", "not_a_category")
	// negative: an un-mapped audit helper (its category is an internal const,
	// its positional string is a payload field) contributes nothing.
	writePlanReusedFromAudit(nil, nil, nil, "not_a_category", nil)
	// negative: an empty-string arg at the category index is filtered out.
	appendRefinementAudit(nil, "", nil)

	_ = category
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "fixture.go", src, 0)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}

	got := collectEmittedCategories(f)
	byShape := map[string]map[string]bool{}
	for _, c := range got {
		if byShape[c.shape] == nil {
			byShape[c.shape] = map[string]bool{}
		}
		byShape[c.shape][c.value] = true
	}

	// Every shape must have collected its fixture literal.
	wantByShape := map[string][]string{
		shapeValueSpec:    {"shape_value_spec"},
		shapeAssign:       {"shape_assign", "shape_assign_reassigned"},
		shapeLogTokenArg:  {"shape_log_token"},
		shapeCompositeCat: {"shape_composite"},
		shapeEmitArg:      {"shape_emit_arg", "shape_emit_review", "shape_consolidate_cat"},
	}
	for shape, wantValues := range wantByShape {
		if byShape[shape] == nil {
			t.Errorf("shape %q collected nothing; the collector no longer exercises it", shape)
			continue
		}
		for _, v := range wantValues {
			if !byShape[shape][v] {
				t.Errorf("shape %q did not collect %q; got %v", shape, v, byShape[shape])
			}
		}
	}

	// Negative cases: none of these may appear under ANY shape.
	forbidden := []string{"operator", "A", "not_a_category", ""}
	for _, c := range got {
		for _, bad := range forbidden {
			if c.value == bad {
				t.Errorf("collector over-collected %q (shape %q); it must be excluded", c.value, c.shape)
			}
		}
	}
}
