package server

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
)

// --- #3410 audit-category rule of the plan-gate surface sweep ---

// TestExtractAuditCategoryMentions pins the three regex shapes and the
// post-capture filter: one positive per shape, and the negatives a loosened
// pattern would start capturing (fishhawk_ tool names, no-underscore words,
// mixed-case tokens, unquoted keyword-first tokens, a snake token near
// "audit" with no category noun). The known-category filter lives in the
// evaluator, so a same-line known + unknown pair is captured raw here.
func TestExtractAuditCategoryMentions(t *testing.T) {
	cases := []struct {
		name string
		text string
		want []string
	}{
		// Shape A, keyword-first.
		{"A quoted entry", `write an audit entry "zz_new_cat" for it`, []string{"zz_new_cat"}},
		{"A backticked named category", "the audit category named `zz_new_cat`", []string{"zz_new_cat"}},
		{"A audit-log row", `an audit-log row 'zz_new_cat'`, []string{"zz_new_cat"}},
		// Shape B, token-first — the #3400 plan's verbatim phrasing.
		{"B backticked row", "appends one advisory `zz_new_cat` audit row (system actor)", []string{"zz_new_cat"}},
		{"B unquoted entry", "emit a zz_new_cat audit entry", []string{"zz_new_cat"}},
		{"B at line start", "zz_new_cat audit entry", []string{"zz_new_cat"}},
		// Shape C, field form.
		{"C field", `Category: "zz_new_cat"`, []string{"zz_new_cat"}},
		{"C field equals", "category = `zz_new_cat`", []string{"zz_new_cat"}},
		// Negatives.
		{"no underscore", `category "acceptance"`, nil},
		{"fishhawk_ tool name", "fishhawk_get_plan audit entry", nil},
		{"mixed-case quoted (filter)", `write an audit entry "Mixed_new_cat"`, nil},
		{"mixed-case token-first (boundary)", "Mixed_new_cat audit entry", nil},
		{"unquoted after keyword", "the audit entry precedes plan_reviewed", nil},
		{"snake token near audit, no category noun", "the scope_amendment_pending runner log event, not an audit category", nil},
		{"path segment is not a token", "see docs/audit_cat.md audit entry", nil},
		{"empty", "", nil},
		// Same-line known + unknown pair: both captured raw.
		{"known + unknown raw", "write an audit entry `plan_reviewed` and an audit entry `zz_new_cat`", []string{"plan_reviewed", "zz_new_cat"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractAuditCategoryMentions("summary", tc.text)
			var toks []string
			for _, m := range got {
				if m.Location != "summary" {
					t.Errorf("Location = %q, want summary", m.Location)
				}
				toks = append(toks, m.Category)
			}
			if !reflect.DeepEqual(toks, tc.want) {
				t.Errorf("tokens = %v, want %v", toks, tc.want)
			}
		})
	}

	// Binding condition (2): the token-first shape B itself must yield ZERO
	// matches on a mixed-case token — not a lowercase suffix such as
	// `ixed_new_cat` that the post-filter then happens to drop.
	if m := auditCategoryTokenFirstRE.FindAllStringSubmatch("Mixed_new_cat audit entry", -1); len(m) != 0 {
		t.Errorf("shape B must yield no token for a mixed-case word; got %q", m)
	}
	if m := auditCategoryTokenFirstRE.FindAllStringSubmatch("`Mixed_new_cat` audit entry", -1); len(m) != 0 {
		t.Errorf("shape B must yield no token for a quoted mixed-case word; got %q", m)
	}
}

// TestPlanAuditCategoryMentions_WalksEveryField: one distinct token per
// scanned field yields every one with the expected Location, and a token
// repeated in two fields is reported once at its first location.
func TestPlanAuditCategoryMentions_WalksEveryField(t *testing.T) {
	p := &plan.Plan{
		Summary:  "emit a sum_cat audit entry",
		Approach: []plan.ApproachStep{{Step: 1, Description: "no-op"}, {Step: 2, Description: "append an `appr_cat` audit row"}},
		Verification: plan.Verification{
			TestStrategy: `assert the audit category "strat_cat" is written`,
			AcceptanceCriteria: []plan.AcceptanceCriterion{
				{ID: "AC-1", Statement: "a stmt_cat audit entry exists", VerifyHint: `category: "hint_cat"`},
			},
		},
		RisksAndAssumptions: []string{"none", "the risk_cat audit event may double-fire"},
		Decomposition: &plan.Decomposition{SubPlans: []plan.SubPlanSummary{
			{Title: "slice-a", ScopeHint: "writes the `sub_cat` audit row"},
			{Title: "slice-b", ScopeHint: "also the sum_cat audit entry (repeat)"},
		}},
	}
	want := []auditCategoryMention{
		{"sum_cat", "summary"},
		{"appr_cat", "approach step 2"},
		{"strat_cat", "verification.test_strategy"},
		{"stmt_cat", "acceptance_criteria[AC-1].statement"},
		{"hint_cat", "acceptance_criteria[AC-1].verify_hint"},
		{"risk_cat", "risks_and_assumptions[1]"},
		{"sub_cat", "decomposition.sub_plans[slice-a].scope_hint"},
	}
	got := planAuditCategoryMentions(p)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mentions =\n%+v\nwant\n%+v", got, want)
	}
}

func auditRulePlan(files []string, text string) *plan.Plan {
	p := &plan.Plan{Summary: text}
	for _, f := range files {
		p.Scope.Files = append(p.Scope.Files, plan.ScopeFile{Path: f, Operation: plan.FileOpModify})
	}
	return p
}

// TestEvaluateAuditCategoryRule is the pure-evaluator table with isKnown
// injected: the finding shape, the known filter, the union-scope
// short-circuit (top-level and sub-plan), the backend/ repo-signature guard,
// the exemption lookup (matching vs wrong-sibling) and the sorted cap.
func TestEvaluateAuditCategoryRule(t *testing.T) {
	never := func(string) bool { return false }
	always := func(string) bool { return true }
	const text = "append an advisory `zz_new_cat` audit row"
	backendScope := []string{"backend/internal/server/foo.go"}

	t.Run("unknown token, backend scoped, registry absent → one finding", func(t *testing.T) {
		got, applied := evaluateAuditCategoryRule(auditRulePlan(backendScope, text), never, nil)
		want := []SurfaceSweepFinding{{
			Pattern:         "new audit category requires registry",
			TriggerPath:     `audit category "zz_new_cat" named at summary`,
			MissingSiblings: []string{"backend/internal/audit/categories.go"},
			Category:        "zz_new_cat",
		}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("findings = %+v, want %+v", got, want)
		}
		if applied != nil {
			t.Errorf("applied = %+v, want nil", applied)
		}
	})
	t.Run("known token → nil (isKnown filter)", func(t *testing.T) {
		got, applied := evaluateAuditCategoryRule(auditRulePlan(backendScope, text), always, nil)
		if got != nil || applied != nil {
			t.Errorf("got %+v / %+v, want nil / nil", got, applied)
		}
	})
	t.Run("registry in top-level scope → nil", func(t *testing.T) {
		got, _ := evaluateAuditCategoryRule(auditRulePlan(append(backendScope, "backend/internal/audit/categories.go"), text), never, nil)
		if got != nil {
			t.Errorf("got %+v, want nil", got)
		}
	})
	t.Run("registry only in a sub-plan scope → nil", func(t *testing.T) {
		p := auditRulePlan(backendScope, text)
		p.Decomposition = &plan.Decomposition{SubPlans: []plan.SubPlanSummary{
			{Title: "s1", Scope: &plan.Scope{Files: []plan.ScopeFile{{Path: `backend\internal\audit\categories.go`, Operation: plan.FileOpModify}}}},
		}}
		got, _ := evaluateAuditCategoryRule(p, never, nil)
		if got != nil {
			t.Errorf("got %+v, want nil", got)
		}
	})
	t.Run("no backend/ path scoped → nil (repo-signature guard)", func(t *testing.T) {
		got, _ := evaluateAuditCategoryRule(auditRulePlan([]string{"runner/cmd/x.go", "docs/a.md"}, text), never, nil)
		if got != nil {
			t.Errorf("got %+v, want nil", got)
		}
	})
	t.Run("backend/ path only in a sub-plan scope still fires", func(t *testing.T) {
		p := auditRulePlan([]string{"docs/a.md"}, text)
		p.Decomposition = &plan.Decomposition{SubPlans: []plan.SubPlanSummary{
			{Title: "s1", Scope: &plan.Scope{Files: []plan.ScopeFile{{Path: "backend/internal/x.go", Operation: plan.FileOpModify}}}},
		}}
		got, _ := evaluateAuditCategoryRule(p, never, nil)
		if len(got) != 1 {
			t.Errorf("got %+v, want one finding", got)
		}
	})
	t.Run("no mentions → nil", func(t *testing.T) {
		got, applied := evaluateAuditCategoryRule(auditRulePlan(backendScope, "no categories here"), never, nil)
		if got != nil || applied != nil {
			t.Errorf("got %+v / %+v, want nil / nil", got, applied)
		}
	})
	t.Run("matching exemption → no finding, one applied", func(t *testing.T) {
		ex := []plan.SurfaceSweepExemption{
			{Pattern: "actor @-mention render surfaces", Sibling: "backend/internal/audit/categories.go", Reason: "wrong pattern"},
			{Pattern: "new audit category requires registry", Sibling: `backend\internal\audit\categories.go`, Reason: "zz_new_cat is a runner log event, not an audit category"},
		}
		got, applied := evaluateAuditCategoryRule(auditRulePlan(backendScope, text), never, ex)
		if got != nil {
			t.Errorf("findings = %+v, want nil", got)
		}
		want := []AppliedExemption{{
			Pattern: "new audit category requires registry",
			Sibling: "backend/internal/audit/categories.go",
			Reason:  "zz_new_cat is a runner log event, not an audit category",
		}}
		if !reflect.DeepEqual(applied, want) {
			t.Errorf("applied = %+v, want %+v", applied, want)
		}
	})
	t.Run("wrong-sibling exemption still fires", func(t *testing.T) {
		ex := []plan.SurfaceSweepExemption{
			{Pattern: "new audit category requires registry", Sibling: "backend/internal/audit/other.go", Reason: "x"},
		}
		got, applied := evaluateAuditCategoryRule(auditRulePlan(backendScope, text), never, ex)
		if len(got) != 1 || applied != nil {
			t.Errorf("got %+v / %+v, want one finding / nil", got, applied)
		}
	})
	t.Run("12 tokens → 10 findings sorted", func(t *testing.T) {
		var sb strings.Builder
		// Emit in reverse so sorting is observable.
		for i := 11; i >= 0; i-- {
			fmt.Fprintf(&sb, "emit a tok_%02d audit entry. ", i)
		}
		got, _ := evaluateAuditCategoryRule(auditRulePlan(backendScope, sb.String()), never, nil)
		if len(got) != auditCategorySweepMaxFindings {
			t.Fatalf("len = %d, want %d", len(got), auditCategorySweepMaxFindings)
		}
		for i, f := range got {
			if want := fmt.Sprintf("tok_%02d", i); f.Category != want {
				t.Errorf("findings[%d].Category = %q, want %q", i, f.Category, want)
			}
		}
	})
	t.Run("nil plan → nil", func(t *testing.T) {
		got, applied := evaluateAuditCategoryRule(nil, never, nil)
		if got != nil || applied != nil {
			t.Errorf("got %+v / %+v, want nil / nil", got, applied)
		}
	})
}
