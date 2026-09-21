package issuecomment

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
)

// TestActivityCategoriesMatchRenderActivityLine is the completeness gate for
// the pair of tables status_template.go maintains in lockstep (#3392):
// renderActivityLine's `switch e.Category` and the activityCategories closed
// set. pickActivity drops every audit row whose category is absent from
// activityCategories BEFORE renderActivityLine ever sees it — an audit
// category emitted by a writer but missing from activityCategories renders
// NOTHING on the anchor / status comment, in total silence. That silent drop
// is exactly what this issue reports: acceptance_scenario_retirement_dropped
// reached the audit chain but never reached the operator-visible timeline.
// Symmetrically, an activityCategories entry with no matching `case` in
// renderActivityLine's switch falls through to the `default` branch and
// renders the bare category name instead of a verb phrase — also a silent
// degradation, just a smaller one.
//
// This test asserts set-equality between the two tables in BOTH directions.
// It FAILS CLOSED: a parse failure or an unrecognizable switch statement is a
// hard test failure (t.Fatalf), never a skip — a completeness gate that
// degrades to "skip when I can't tell" is worse than no gate, because it
// still looks like coverage. Every mismatch failure names the missing
// category AND the file to edit, so the fix is one line away from the
// message.
//
// What this gate does NOT catch on its own: a category a server writer
// INTENDS for the anchor timeline — an AppendChained call followed by a
// status refresh — that has NEITHER a `case` NOR an activityCategories entry.
// That is the exact shape this issue's defect had before the fix landed: a
// same-file parity check has no case and no entry to compare against each
// other, so the mismatch is invisible here. That half is closed by the
// cross-package "writer intends the anchor" gate (#3406):
// backend/internal/server/operator_visible_gate_test.go type-checks the
// server package and asserts every category passed to
// (*Server).notifyOperatorVisible — the explicit writer-side intent marker —
// is admitted by RendersActivity (this registry) and by
// audit.IsKnownCategory, and that no notifyStatusUpdate refresh is tagged
// with an audit category unless a reasoned exemption names it. The residual
// that remains: a writer that appends a NEW category and refreshes with a
// non-category transition tag is bound by neither gate.
func TestActivityCategoriesMatchRenderActivityLine(t *testing.T) {
	cases, err := collectRenderActivityLineCases("status_template.go")
	if err != nil {
		t.Fatalf("parsing renderActivityLine's switch in status_template.go failed closed (not skipped): %v", err)
	}

	// Vacuity floor: a broken AST walk that collected ~nothing must not
	// silently pass. There are 25+ registered categories today.
	const minCases = 20
	if len(cases) < minCases {
		t.Fatalf("collected only %d case labels from renderActivityLine's switch (< %d); "+
			"the AST walk is likely broken and this gate would otherwise false-pass", len(cases), minCases)
	}

	for c := range cases {
		if _, ok := activityCategories[c]; !ok {
			t.Errorf("renderActivityLine has a case %q with NO entry in the activityCategories registry; "+
				"add `%q: {}` to activityCategories in backend/internal/issuecomment/status_template.go "+
				"(an audit row of this category is otherwise silently dropped by pickActivity before rendering)", c, c)
		}
	}
	for c := range activityCategories {
		if _, ok := cases[c]; !ok {
			t.Errorf("activityCategories registers %q with NO matching case in renderActivityLine's switch; "+
				"add a `case %q:` to renderActivityLine in backend/internal/issuecomment/status_template.go "+
				"(it currently falls through to the default raw-category-name rendering)", c, c)
		}
	}
}

// collectRenderActivityLineCases parses filename and returns the set of
// string-literal case labels in renderActivityLine's `switch e.Category`
// statement. Returns a non-nil error (never a silent empty set) when the file
// can't be parsed or the function/switch can't be located, so the caller
// fails closed instead of treating "found nothing" as "matches everything".
func collectRenderActivityLineCases(filename string) (map[string]struct{}, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, nil, 0)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", filename, err)
	}

	var sw *ast.SwitchStmt
	ast.Inspect(f, func(n ast.Node) bool {
		if sw != nil {
			return false
		}
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "renderActivityLine" || fn.Body == nil {
			return true
		}
		ast.Inspect(fn.Body, func(n2 ast.Node) bool {
			if sw != nil {
				return false
			}
			if s, ok := n2.(*ast.SwitchStmt); ok {
				sw = s
				return false
			}
			return true
		})
		return false
	})
	if sw == nil {
		return nil, fmt.Errorf("no switch statement found inside renderActivityLine in %s", filename)
	}

	out := map[string]struct{}{}
	for _, stmt := range sw.Body.List {
		cc, ok := stmt.(*ast.CaseClause)
		if !ok {
			continue
		}
		for _, expr := range cc.List {
			lit, ok := expr.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			v, err := strconv.Unquote(lit.Value)
			if err != nil {
				continue
			}
			out[v] = struct{}{}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("renderActivityLine's switch in %s had no string-literal case labels", filename)
	}
	return out, nil
}

// activityCategoryKnownCategoryExemptions lists activityCategories members
// deliberately absent from audit.KnownCategories — a forward registration
// ahead of a not-yet-landed writer, not a silent gap. Every entry must be
// named here with a reason; an un-exempted mismatch fails
// TestActivityCategoriesRegisteredInAuditKnownCategories.
var activityCategoryKnownCategoryExemptions = map[string]string{
	// Registered ahead of its E17.4 writer (the future GitHub-reaction plan
	// approval path); not yet in audit.KnownCategories because that writer
	// hasn't landed. A deliberate placeholder, not a drift.
	"plan_approved_via_reaction": "future, post-E17.4",
}

// TestActivityCategoriesRegisteredInAuditKnownCategories asserts every
// activityCategories member is also a canonical audit.KnownCategories entry
// (modulo the named exemption above), so a category this package renders is
// also one GET /v0/runs/{run_id}/audit and fishhawk_await_audit can actually
// return without a 400.
func TestActivityCategoriesRegisteredInAuditKnownCategories(t *testing.T) {
	for c := range activityCategories {
		if audit.IsKnownCategory(c) {
			continue
		}
		if _, exempt := activityCategoryKnownCategoryExemptions[c]; exempt {
			continue
		}
		t.Errorf("activityCategories member %q is absent from audit.KnownCategories; "+
			"register it in backend/internal/audit/categories.go, or add a named, commented exemption to "+
			"activityCategoryKnownCategoryExemptions in "+
			"backend/internal/issuecomment/activity_categories_completeness_test.go if this is a deliberate "+
			"forward registration", c)
	}
}

// TestRendersActivity pins the read accessor the server package's
// writer-intent marker and gate consult (#3406): true for a registered
// activityCategories member, false for an unregistered string and for the
// empty string — so the marker's runtime check and the static gate cannot
// silently invert.
func TestRendersActivity(t *testing.T) {
	cases := []struct {
		category string
		want     bool
	}{
		{"fixup_pushed", true},
		{"pr_merged", true},
		{"not_a_rendered_category", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := RendersActivity(tc.category); got != tc.want {
			t.Errorf("RendersActivity(%q) = %v, want %v", tc.category, got, tc.want)
		}
	}
}
