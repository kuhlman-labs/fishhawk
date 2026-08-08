package server

import (
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
)

// TestValidateBindingAssertions covers the #1171 declaration-validation matrix:
// each valid type passes, and each malformed declaration (unknown type, missing
// path, non-repo-relative path, empty literal, a test_asserts path not ending
// in _test.go) is rejected. An empty/nil slice is the byte-identical
// no-declaration path and must pass.
func TestValidateBindingAssertions(t *testing.T) {
	cases := []struct {
		name       string
		assertions []bindingAssertion
		wantErr    bool
	}{
		{
			name:       "nil slice valid",
			assertions: nil,
			wantErr:    false,
		},
		{
			name:       "empty slice valid",
			assertions: []bindingAssertion{},
			wantErr:    false,
		},
		{
			name: "valid file_contains",
			assertions: []bindingAssertion{
				{Type: "file_contains", Path: "backend/internal/yaml/pad.go", Literal: "pad: 3"},
			},
			wantErr: false,
		},
		{
			name: "valid test_asserts",
			assertions: []bindingAssertion{
				{Type: "test_asserts", Path: "backend/internal/yaml/pad_test.go", Literal: "TestPad"},
			},
			wantErr: false,
		},
		{
			name: "multiple valid",
			assertions: []bindingAssertion{
				{Type: "file_contains", Path: "a/b.go", Literal: "x"},
				{Type: "test_asserts", Path: "a/b_test.go", Literal: "y"},
			},
			wantErr: false,
		},
		{
			name: "unknown type rejected",
			assertions: []bindingAssertion{
				{Type: "file_matches", Path: "a/b.go", Literal: "x"},
			},
			wantErr: true,
		},
		{
			name: "empty type rejected",
			assertions: []bindingAssertion{
				{Type: "", Path: "a/b.go", Literal: "x"},
			},
			wantErr: true,
		},
		{
			name: "missing path rejected",
			assertions: []bindingAssertion{
				{Type: "file_contains", Path: "", Literal: "x"},
			},
			wantErr: true,
		},
		{
			name: "absolute path rejected",
			assertions: []bindingAssertion{
				{Type: "file_contains", Path: "/etc/passwd", Literal: "x"},
			},
			wantErr: true,
		},
		{
			name: "parent-traversal path rejected",
			assertions: []bindingAssertion{
				{Type: "file_contains", Path: "../secrets.go", Literal: "x"},
			},
			wantErr: true,
		},
		{
			name: "empty literal rejected",
			assertions: []bindingAssertion{
				{Type: "file_contains", Path: "a/b.go", Literal: ""},
			},
			wantErr: true,
		},
		{
			name: "test_asserts non-test path rejected",
			assertions: []bindingAssertion{
				{Type: "test_asserts", Path: "a/b.go", Literal: "TestX"},
			},
			wantErr: true,
		},
		{
			name: "first valid second invalid rejected",
			assertions: []bindingAssertion{
				{Type: "file_contains", Path: "a/b.go", Literal: "x"},
				{Type: "test_asserts", Path: "a/b.go", Literal: "y"},
			},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateBindingAssertions(tc.assertions)
			if tc.wantErr && err == nil {
				t.Fatalf("validateBindingAssertions(%v) = nil, want error", tc.assertions)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("validateBindingAssertions(%v) = %v, want nil", tc.assertions, err)
			}
		})
	}
}

// --- Weak-assertion warnings (#2501 proposal 3) --------------------------

// scopeFiles builds a plan.Scope from repo-relative paths, all as modify.
func scopeFiles(paths ...string) plan.Scope {
	files := make([]plan.ScopeFile, 0, len(paths))
	for _, p := range paths {
		files = append(files, plan.ScopeFile{Path: p, Operation: plan.FileOpModify})
	}
	return plan.Scope{Files: files}
}

// TestWarnBindingAssertions covers one behavioral case per NAMED heuristic
// (W1 path-not-in-scope, W2 literal-absent-from-plan, W3
// literal-paired-with-another-path) plus every quiet case: a correctly-paired
// assertion, a nil plan (fail-open), an empty declaration, a scope-less plan,
// and a decomposition slice's scope satisfying W1.
func TestWarnBindingAssertions(t *testing.T) {
	// The #2501 plan shape verbatim: the literal `checks_unresolved` lives in
	// an approach step that names drive_test.go, while the operator's
	// assertion named policy_reeval_test.go. Both files are in scope, so W1
	// is silent and W3 is the only heuristic that can catch the typo.
	plan2501 := &plan.Plan{
		PlanVersion: "standard_v1",
		Summary:     "Re-evaluate the drive policy when a gate check is unresolved.",
		Scope: scopeFiles(
			"backend/internal/mcpserver/drive_test.go",
			"backend/internal/server/policy_reeval_test.go",
		),
		Approach: []plan.ApproachStep{
			{Step: 10, Description: "Assert the checks_unresolved stop reason in drive_test.go so the driver's park is pinned."},
		},
		Verification: plan.Verification{TestStrategy: "Go table tests per package."},
	}

	cases := []struct {
		name       string
		assertions []bindingAssertion
		plan       *plan.Plan
		wantCount  int
		// wantKinds are the heuristic names the warnings must name, in order.
		wantKinds []string
		// wantContains are substrings every warning set must carry
		// collectively (the path / literal / paired path the message names).
		wantContains []string
	}{
		{
			name: "W3 literal paired with another path fires and names that path (#2501 verbatim)",
			assertions: []bindingAssertion{
				{Type: bindingAssertTestAsserts, Path: "backend/internal/server/policy_reeval_test.go", Literal: "checks_unresolved"},
			},
			plan:      plan2501,
			wantCount: 1,
			wantKinds: []string{warnKindLiteralOtherPath},
			wantContains: []string{
				"backend/internal/mcpserver/drive_test.go",
				"checks_unresolved",
				"backend/internal/server/policy_reeval_test.go",
			},
		},
		{
			name: "W3 quiet when the pairing is correct",
			assertions: []bindingAssertion{
				{Type: bindingAssertTestAsserts, Path: "backend/internal/mcpserver/drive_test.go", Literal: "checks_unresolved"},
			},
			plan:      plan2501,
			wantCount: 0,
		},
		{
			// The counterfactual vehicle for W3's correct-pairing early
			// return: the literal's sentence names the asserted path AND
			// another scope path, so only the early return keeps this quiet.
			name: "W3 quiet when the literal sentence names the asserted path alongside another",
			assertions: []bindingAssertion{
				{Type: bindingAssertFileContains, Path: "backend/internal/server/a.go", Literal: "pairedLiteral"},
			},
			plan: &plan.Plan{
				Scope: scopeFiles("backend/internal/server/a.go", "backend/internal/server/b.go"),
				Approach: []plan.ApproachStep{
					{Step: 1, Description: "Add pairedLiteral to backend/internal/server/a.go and thread it through backend/internal/server/b.go."},
				},
			},
			wantCount: 0,
		},
		{
			name: "W1 path not in scope fires and names the path",
			assertions: []bindingAssertion{
				// The literal IS present and IS paired with this very path in
				// the plan text, so W2/W3 stay quiet and W1 is isolated.
				{Type: bindingAssertFileContains, Path: "backend/internal/server/untouched.go", Literal: "sentinelLiteral"},
			},
			plan: &plan.Plan{
				Summary: "Add sentinelLiteral to backend/internal/server/untouched.go eventually.",
				Scope:   scopeFiles("backend/internal/server/other.go"),
			},
			wantCount:    1,
			wantKinds:    []string{warnKindPathNotInScope},
			wantContains: []string{"backend/internal/server/untouched.go", "scope.files"},
		},
		{
			name: "W2 literal absent from plan fires and names the literal",
			assertions: []bindingAssertion{
				{Type: bindingAssertFileContains, Path: "backend/internal/server/other.go", Literal: "neverPromisedLiteral"},
			},
			plan: &plan.Plan{
				Summary:      "Touch other.go.",
				Scope:        scopeFiles("backend/internal/server/other.go"),
				Approach:     []plan.ApproachStep{{Step: 1, Description: "Edit backend/internal/server/other.go."}},
				Verification: plan.Verification{TestStrategy: "unit tests"},
			},
			wantCount:    1,
			wantKinds:    []string{warnKindLiteralAbsent},
			wantContains: []string{"neverPromisedLiteral"},
		},
		{
			name: "W1 and W2 both fire for one assertion",
			assertions: []bindingAssertion{
				{Type: bindingAssertFileContains, Path: "backend/internal/server/nope.go", Literal: "alsoAbsent"},
			},
			plan: &plan.Plan{
				Summary: "Touch other.go.",
				Scope:   scopeFiles("backend/internal/server/other.go"),
			},
			wantCount: 2,
			wantKinds: []string{warnKindPathNotInScope, warnKindLiteralAbsent},
		},
		{
			name: "all three heuristics across three assertions",
			assertions: []bindingAssertion{
				{Type: bindingAssertFileContains, Path: "backend/internal/server/nope.go", Literal: "checks_unresolved"},
				{Type: bindingAssertFileContains, Path: "backend/internal/mcpserver/drive_test.go", Literal: "absentEverywhere"},
				{Type: bindingAssertTestAsserts, Path: "backend/internal/server/policy_reeval_test.go", Literal: "checks_unresolved"},
			},
			plan:      plan2501,
			wantCount: 4, // assertion 0 draws W1 + W3, assertion 1 draws W2, assertion 2 draws W3
			wantKinds: []string{
				warnKindPathNotInScope, warnKindLiteralOtherPath,
				warnKindLiteralAbsent,
				warnKindLiteralOtherPath,
			},
		},
		{
			name: "nil plan fails open",
			assertions: []bindingAssertion{
				{Type: bindingAssertFileContains, Path: "backend/internal/server/nope.go", Literal: "absent"},
			},
			plan:      nil,
			wantCount: 0,
		},
		{
			name:       "empty declaration is quiet",
			assertions: nil,
			plan:       plan2501,
			wantCount:  0,
		},
		{
			name: "scope-less plan suppresses W1 but not W2",
			assertions: []bindingAssertion{
				{Type: bindingAssertFileContains, Path: "backend/internal/server/anything.go", Literal: "absentLiteral"},
			},
			plan:      &plan.Plan{Summary: "A plan with no scope.files at all."},
			wantCount: 1,
			wantKinds: []string{warnKindLiteralAbsent},
		},
		{
			name: "a decomposition slice's scope satisfies W1",
			assertions: []bindingAssertion{
				{Type: bindingAssertFileContains, Path: "backend/internal/server/slice_owned.go", Literal: "sliceLiteral"},
			},
			plan: &plan.Plan{
				Summary: "Parent plan. The slice adds sliceLiteral to backend/internal/server/slice_owned.go.",
				Scope:   scopeFiles("backend/internal/server/parent.go"),
				Decomposition: &plan.Decomposition{
					Rationale: "two slices",
					SubPlans: []plan.SubPlanSummary{
						{Title: "slice", Scope: ptrScope(scopeFiles("backend/internal/server/slice_owned.go"))},
					},
				},
			},
			wantCount: 0,
		},
		{
			// validateBindingAssertions rejects an empty literal before this
			// runs, so this is the defensive guard: without it
			// strings.Contains(planText, "") is trivially true and W3 would
			// evaluate an empty literal against EVERY sentence, naming an
			// arbitrary scope path.
			name: "empty literal is skipped rather than matching everything",
			assertions: []bindingAssertion{
				{Type: bindingAssertFileContains, Path: "backend/internal/server/a.go", Literal: ""},
			},
			plan: &plan.Plan{
				Scope:    scopeFiles("backend/internal/server/a.go", "backend/internal/server/b.go"),
				Approach: []plan.ApproachStep{{Step: 1, Description: "Edit backend/internal/server/b.go."}},
			},
			wantCount: 0,
		},
		{
			// mentionsPath's basename fallback requires a dot and >3 chars, so
			// a short extension-less path segment cannot match a bare word in
			// prose. Without the guard the scope path .../api would match the
			// word "api" in the literal's sentence and W3 would fire.
			name: "a short extension-less scope path does not match a bare word",
			assertions: []bindingAssertion{
				{Type: bindingAssertFileContains, Path: "backend/internal/server/a.go", Literal: "guardedLiteral"},
			},
			plan: &plan.Plan{
				Scope: scopeFiles("backend/internal/server/a.go", "backend/internal/server/api"),
				Approach: []plan.ApproachStep{
					{Step: 1, Description: "Add guardedLiteral behind the api boundary."},
				},
			},
			wantCount: 0,
		},
		{
			name: "W3 fires across sentences inside one approach step",
			assertions: []bindingAssertion{
				{Type: bindingAssertFileContains, Path: "backend/internal/server/a.go", Literal: "crossSentence"},
			},
			plan: &plan.Plan{
				Scope: scopeFiles("backend/internal/server/a.go", "backend/internal/server/b.go"),
				Approach: []plan.ApproachStep{
					{Step: 1, Description: "Edit backend/internal/server/a.go. Then add crossSentence to backend/internal/server/b.go."},
				},
			},
			wantCount:    1,
			wantKinds:    []string{warnKindLiteralOtherPath},
			wantContains: []string{"backend/internal/server/b.go"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := warnBindingAssertions(tc.assertions, tc.plan)
			if len(got) != tc.wantCount {
				t.Fatalf("warnBindingAssertions returned %d warnings, want %d:\n%s",
					len(got), tc.wantCount, strings.Join(got, "\n"))
			}
			for i, kind := range tc.wantKinds {
				if !strings.Contains(got[i], kind) {
					t.Errorf("warning[%d] = %q, want it to name heuristic %q", i, got[i], kind)
				}
			}
			joined := strings.Join(got, "\n")
			for _, want := range tc.wantContains {
				if !strings.Contains(joined, want) {
					t.Errorf("warnings missing %q:\n%s", want, joined)
				}
			}
		})
	}
}

// ptrScope returns a pointer to s, for the sub-plan Scope field.
func ptrScope(s plan.Scope) *plan.Scope { return &s }
