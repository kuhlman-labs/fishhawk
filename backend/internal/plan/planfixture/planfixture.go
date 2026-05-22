// Package planfixture provides centralized test-fixture builders for
// standard_v1 plan artifacts. Import in test files to avoid per-test
// inline JSON that drifts when the schema gains required fields.
//
// Three entry points:
//   - Valid returns a minimal schema-valid plan fixture.
//   - Invalid returns Valid() minus one top-level field, for negative tests.
//   - Decomposed returns Valid() extended with a two-sub-plan decomposition.
//
// All three accept optional Option values to override individual fields.
package planfixture

// Option modifies a fixture map in place. Pass to Valid, Invalid, or
// Decomposed to override defaults for a specific test case.
type Option func(map[string]any)

// Valid returns the minimal valid standard_v1 plan fixture as a map.
// All required schema fields are set. Apply Option values to extend or
// override specific fields before marshalling to JSON.
func Valid(opts ...Option) map[string]any {
	m := map[string]any{
		"plan_version": "standard_v1",
		"ticket_reference": map[string]any{
			"type": "github_issue",
			"url":  "https://github.com/x/y/issues/1",
			"id":   "x/y#1",
		},
		"generated_by": map[string]any{
			"agent":     "claude-code",
			"model":     "claude-opus-4-7",
			"timestamp": "2026-01-01T00:00:00Z",
		},
		"summary": "Add a thing.",
		"scope": map[string]any{
			"files": []any{
				map[string]any{"path": "a.go", "operation": "create"},
			},
		},
		"approach": []any{
			map[string]any{"step": 1, "description": "Do the work."},
		},
		"verification": map[string]any{
			"test_strategy": "Run the tests.",
			"rollback_plan": "Revert the PR.",
		},
		"predicted_runtime_minutes":    20,
		"predicted_runtime_confidence": "medium",
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Invalid deep-copies Valid (applying opts first) then deletes the
// named top-level field. Use for negative-path tests that need a plan
// missing exactly one required field.
func Invalid(field string, opts ...Option) map[string]any {
	m := Valid(opts...)
	delete(m, field)
	return m
}

// Decomposed extends Valid with a two-sub-plan decomposition block.
// Sub-plan titles are distinct so semantic duplicate-title checks pass.
func Decomposed(opts ...Option) map[string]any {
	m := Valid(opts...)
	m["decomposition"] = map[string]any{
		"rationale": "scope exceeded single-stage budget",
		"sub_plans": []any{
			map[string]any{
				"title":                        "Part A",
				"scope_hint":                   "first half",
				"predicted_runtime_minutes":    10,
				"predicted_runtime_confidence": "medium",
			},
			map[string]any{
				"title":                        "Part B",
				"scope_hint":                   "second half",
				"predicted_runtime_minutes":    10,
				"predicted_runtime_confidence": "medium",
			},
		},
	}
	return m
}
