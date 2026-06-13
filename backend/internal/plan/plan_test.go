package plan_test

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan/planfixture"
)

func readFixture(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + path)
	if err != nil {
		t.Fatalf("read %q: %v", path, err)
	}
	return b
}

// --- Happy paths ---

func TestParse_Example(t *testing.T) {
	p, err := plan.Parse(readFixture(t, "valid/example.json"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p.PlanVersion != "standard_v1" {
		t.Errorf("PlanVersion = %q", p.PlanVersion)
	}
	if p.TicketReference.Type != plan.TicketTypeGitHubIssue {
		t.Errorf("TicketReference.Type = %q", p.TicketReference.Type)
	}
	if p.GeneratedBy.Agent != "claude-code" {
		t.Errorf("Agent = %q", p.GeneratedBy.Agent)
	}
	if p.GeneratedBy.Timestamp.IsZero() {
		t.Error("Timestamp should parse to a non-zero time.Time")
	}
	if got, want := p.GeneratedBy.Timestamp.UTC(), time.Date(2026, 4, 30, 14, 22, 11, 0, time.UTC); !got.Equal(want) {
		t.Errorf("Timestamp = %v, want %v", got, want)
	}
	if got, want := len(p.Scope.Files), 2; got != want {
		t.Errorf("scope.files len = %d, want %d", got, want)
	}
	if p.Scope.Files[0].Operation != plan.FileOpCreate {
		t.Errorf("first file op = %q", p.Scope.Files[0].Operation)
	}
	if got, want := len(p.Approach), 2; got != want {
		t.Errorf("approach len = %d, want %d", got, want)
	}
	if p.Approach[0].Step != 1 {
		t.Errorf("first step = %d", p.Approach[0].Step)
	}
}

func TestValidate_Example(t *testing.T) {
	if err := plan.Validate(readFixture(t, "valid/example.json")); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// --- ParseError cases ---

func TestParse_Empty(t *testing.T) {
	_, err := plan.Parse(nil)
	var pe *plan.ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want *ParseError", err)
	}
}

func TestParse_Whitespace(t *testing.T) {
	_, err := plan.Parse([]byte("   \n\t  "))
	var pe *plan.ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want *ParseError", err)
	}
}

func TestParse_MalformedJSON(t *testing.T) {
	_, err := plan.Parse([]byte(`{"plan_version": "standard_v1",`))
	var pe *plan.ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want *ParseError", err)
	}
}

// --- SchemaError cases ---

func TestParse_MissingPlanVersion(t *testing.T) {
	_, err := plan.Parse([]byte(`{
  "ticket_reference": {"type": "github_issue", "url": "https://x", "id": "x"},
  "generated_by": {"agent": "a", "model": "m", "timestamp": "2026-01-01T00:00:00Z"},
  "summary": "x",
  "scope": {"files": [{"path": "a.go", "operation": "create"}]},
  "approach": [{"step": 1, "description": "x"}],
  "verification": {"test_strategy": "x", "rollback_plan": "x"}
}`))
	var se *plan.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError", err)
	}
}

func TestParse_WrongPlanVersion(t *testing.T) {
	_, err := plan.Parse([]byte(`{
  "plan_version": "v999",
  "ticket_reference": {"type": "github_issue", "url": "https://x", "id": "x"},
  "generated_by": {"agent": "a", "model": "m", "timestamp": "2026-01-01T00:00:00Z"},
  "summary": "x",
  "scope": {"files": [{"path": "a.go", "operation": "create"}]},
  "approach": [{"step": 1, "description": "x"}],
  "verification": {"test_strategy": "x", "rollback_plan": "x"}
}`))
	var se *plan.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError", err)
	}
}

func TestParse_UnknownFileOperation(t *testing.T) {
	_, err := plan.Parse([]byte(`{
  "plan_version": "standard_v1",
  "ticket_reference": {"type": "github_issue", "url": "https://x", "id": "x"},
  "generated_by": {"agent": "a", "model": "m", "timestamp": "2026-01-01T00:00:00Z"},
  "summary": "x",
  "scope": {"files": [{"path": "a.go", "operation": "rename"}]},
  "approach": [{"step": 1, "description": "x"}],
  "verification": {"test_strategy": "x", "rollback_plan": "x"}
}`))
	var se *plan.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError", err)
	}
}

func TestParse_EmptyApproach(t *testing.T) {
	_, err := plan.Parse([]byte(`{
  "plan_version": "standard_v1",
  "ticket_reference": {"type": "github_issue", "url": "https://x", "id": "x"},
  "generated_by": {"agent": "a", "model": "m", "timestamp": "2026-01-01T00:00:00Z"},
  "summary": "x",
  "scope": {"files": [{"path": "a.go", "operation": "create"}]},
  "approach": [],
  "verification": {"test_strategy": "x", "rollback_plan": "x"}
}`))
	var se *plan.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError", err)
	}
}

func TestParse_AdditionalPropertyRejected(t *testing.T) {
	_, err := plan.Parse([]byte(`{
  "plan_version": "standard_v1",
  "ticket_reference": {"type": "github_issue", "url": "https://x", "id": "x"},
  "generated_by": {"agent": "a", "model": "m", "timestamp": "2026-01-01T00:00:00Z"},
  "summary": "x",
  "scope": {"files": [{"path": "a.go", "operation": "create"}]},
  "approach": [{"step": 1, "description": "x"}],
  "verification": {"test_strategy": "x", "rollback_plan": "x"},
  "extra_field": "not allowed"
}`))
	var se *plan.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError", err)
	}
}

func TestParse_NonRFC3339Timestamp(t *testing.T) {
	_, err := plan.Parse([]byte(`{
  "plan_version": "standard_v1",
  "ticket_reference": {"type": "github_issue", "url": "https://x", "id": "x"},
  "generated_by": {"agent": "a", "model": "m", "timestamp": "yesterday"},
  "summary": "x",
  "scope": {"files": [{"path": "a.go", "operation": "create"}]},
  "approach": [{"step": 1, "description": "x"}],
  "verification": {"test_strategy": "x", "rollback_plan": "x"}
}`))
	// JSON Schema's "format: date-time" is annotation-only by default in
	// many validators. If the schema we ship asserts it, this test
	// passes via *SchemaError; if not, the typed json.Unmarshal of
	// time.Time will fail in Parse and surface as an internal error.
	// Either path is acceptable; we just require the call to fail.
	if err == nil {
		t.Fatal("expected error on non-RFC-3339 timestamp")
	}
	var se *plan.SchemaError
	var pe *plan.ParseError
	if !errors.As(err, &se) && !errors.As(err, &pe) && !strings.Contains(err.Error(), "decode to Plan") {
		t.Errorf("err = %v, want *SchemaError, *ParseError, or internal decode error", err)
	}
}

// --- Validate-only path ---

func TestValidate_ReturnsSameErrorAsParseSchemaPath(t *testing.T) {
	bad := []byte(`{"plan_version": "standard_v1"}`) // missing required fields
	if err := plan.Validate(bad); err == nil {
		t.Fatal("Validate(bad) should error")
	}
	if _, err := plan.Parse(bad); err == nil {
		t.Fatal("Parse(bad) should error")
	}
}

// --- Error formatting + unwrap ---

func TestParseError_FormatsMessageWithPrefix(t *testing.T) {
	err := &plan.ParseError{Msg: "broken"}
	if got, want := err.Error(), "plan: parse: broken"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestParseError_FormatsCauseWhenMsgEmpty(t *testing.T) {
	cause := errors.New("syntax error at line 4")
	err := &plan.ParseError{Cause: cause}
	if got := err.Error(); !strings.Contains(got, "syntax error") {
		t.Errorf("Error() = %q, want it to contain the cause", got)
	}
}

func TestParseError_UnknownErrorWhenBothEmpty(t *testing.T) {
	err := &plan.ParseError{}
	if got, want := err.Error(), "plan: parse: unknown error"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestParseError_UnwrapExposesCause(t *testing.T) {
	cause := errors.New("eof")
	err := &plan.ParseError{Cause: cause}
	if !errors.Is(err, cause) {
		t.Errorf("errors.Is should resolve through Unwrap")
	}
}

func TestSchemaError_FormatsPathAndMessage(t *testing.T) {
	err := &plan.SchemaError{Path: "/scope/files/0", Message: "missing 'path'"}
	if got, want := err.Error(), "plan: schema: /scope/files/0: missing 'path'"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

// marshalFixture marshals a fixture map to JSON, failing the test on error.
func marshalFixture(t *testing.T, m map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// --- predicted_runtime_minutes / predicted_runtime_confidence schema errors ---

func TestParse_MissingPredictedRuntimeMinutes(t *testing.T) {
	// Regression guard: plans produced before this PR (without the new required
	// fields) fail validation after deploy — this is the intentional breaking change.
	_, err := plan.Parse([]byte(`{
  "plan_version": "standard_v1",
  "ticket_reference": {"type": "github_issue", "url": "https://x", "id": "x"},
  "generated_by": {"agent": "a", "model": "m", "timestamp": "2026-01-01T00:00:00Z"},
  "summary": "x",
  "scope": {"files": [{"path": "a.go", "operation": "create"}]},
  "approach": [{"step": 1, "description": "x"}],
  "verification": {"test_strategy": "x", "rollback_plan": "x"},
  "predicted_runtime_confidence": "medium"
}`))
	var se *plan.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError", err)
	}
}

func TestParse_MissingPredictedRuntimeConfidence(t *testing.T) {
	_, err := plan.Parse([]byte(`{
  "plan_version": "standard_v1",
  "ticket_reference": {"type": "github_issue", "url": "https://x", "id": "x"},
  "generated_by": {"agent": "a", "model": "m", "timestamp": "2026-01-01T00:00:00Z"},
  "summary": "x",
  "scope": {"files": [{"path": "a.go", "operation": "create"}]},
  "approach": [{"step": 1, "description": "x"}],
  "verification": {"test_strategy": "x", "rollback_plan": "x"},
  "predicted_runtime_minutes": 10
}`))
	var se *plan.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError", err)
	}
}

// --- Pre-D2 shape regression guard ---

func TestParse_PreD2Shape_NoRuntimeFields_IsSchemaError(t *testing.T) {
	// Explicit regression guard: the pre-D2 wire format (no predicted_runtime_minutes
	// or predicted_runtime_confidence) must be rejected as *SchemaError after this PR.
	_, err := plan.Parse([]byte(`{
  "plan_version": "standard_v1",
  "ticket_reference": {"type": "github_issue", "url": "https://x", "id": "x"},
  "generated_by": {"agent": "a", "model": "m", "timestamp": "2026-01-01T00:00:00Z"},
  "summary": "x",
  "scope": {"files": [{"path": "a.go", "operation": "create"}]},
  "approach": [{"step": 1, "description": "x"}],
  "verification": {"test_strategy": "x", "rollback_plan": "x"}
}`))
	var se *plan.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("pre-D2 plan shape should produce *SchemaError, got %v", err)
	}
}

// --- decomposition schema errors ---

func TestParse_DecompositionOneSubPlan_IsSchemaError(t *testing.T) {
	// minItems:2 violation — a single sub-plan is rejected structurally.
	m := planfixture.Valid(func(m map[string]any) {
		m["decomposition"] = map[string]any{
			"rationale": "too big",
			"sub_plans": []any{
				map[string]any{
					"title": "Part A", "scope_hint": "first half",
					"predicted_runtime_minutes": 10, "predicted_runtime_confidence": "medium",
				},
			},
		}
	})
	_, err := plan.Parse(marshalFixture(t, m))
	var se *plan.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError", err)
	}
}

// --- decomposition happy path ---

func TestParse_DecompositionTwoSubPlans_Succeeds(t *testing.T) {
	p, err := plan.Parse(marshalFixture(t, planfixture.Decomposed()))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p.Decomposition == nil {
		t.Fatal("Decomposition should be non-nil")
	}
	if got, want := len(p.Decomposition.SubPlans), 2; got != want {
		t.Errorf("sub_plans len = %d, want %d", got, want)
	}
}

// TestParse_SubPlanScope_RoundTrips covers the additive per-sub-plan
// scope field (#676): a decomposition whose sub_plans carry their own
// scope.files validates against the schema and decodes into the typed
// SubPlanSummary.Scope.
func TestParse_SubPlanScope_RoundTrips(t *testing.T) {
	m := planfixture.Valid(func(m map[string]any) {
		m["decomposition"] = map[string]any{
			"rationale": "split by layer",
			"sub_plans": []any{
				map[string]any{
					"title": "Part A", "scope_hint": "first slice",
					"scope": map[string]any{
						"files": []any{
							map[string]any{"path": "pkg/a/a.go", "operation": "create"},
						},
					},
					"predicted_runtime_minutes": 10, "predicted_runtime_confidence": "high",
				},
				map[string]any{
					"title": "Part B", "scope_hint": "second slice",
					"predicted_runtime_minutes": 15, "predicted_runtime_confidence": "medium",
				},
			},
		}
	})
	p, err := plan.Parse(marshalFixture(t, m))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	subs := p.Decomposition.SubPlans
	if subs[0].Scope == nil {
		t.Fatal("sub_plans[0].Scope should be non-nil")
	}
	if got, want := len(subs[0].Scope.Files), 1; got != want {
		t.Fatalf("sub_plans[0].Scope.Files len = %d, want %d", got, want)
	}
	if got, want := subs[0].Scope.Files[0].Path, "pkg/a/a.go"; got != want {
		t.Errorf("sub_plans[0].Scope.Files[0].Path = %q, want %q", got, want)
	}
	// Sub-plan without scope decodes to a nil pointer — the field is
	// absent (additive optional), confirming backward compatibility.
	if subs[1].Scope != nil {
		t.Errorf("sub_plans[1].Scope = %+v, want nil (field omitted)", subs[1].Scope)
	}
}

// TestParse_SubPlanWithoutScope_StillValidates confirms the additive field
// did not become required: an existing decomposition shape lacking
// sub-plan scope continues to validate.
func TestParse_SubPlanWithoutScope_StillValidates(t *testing.T) {
	if _, err := plan.Parse(marshalFixture(t, planfixture.Decomposed())); err != nil {
		t.Fatalf("Parse decomposed fixture without sub-plan scope: %v", err)
	}
}

// --- decomposition semantic errors ---

func TestParse_DecompositionDuplicateTitles_IsSemanticError(t *testing.T) {
	m := planfixture.Valid(func(m map[string]any) {
		m["decomposition"] = map[string]any{
			"rationale": "too big",
			"sub_plans": []any{
				map[string]any{
					"title": "Same Title", "scope_hint": "first",
					"predicted_runtime_minutes": 5, "predicted_runtime_confidence": "low",
				},
				map[string]any{
					"title": "Same Title", "scope_hint": "second",
					"predicted_runtime_minutes": 5, "predicted_runtime_confidence": "low",
				},
			},
		}
	})
	_, err := plan.Parse(marshalFixture(t, m))
	var se *plan.SemanticError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SemanticError", err)
	}
	if !strings.Contains(se.Error(), "duplicate title") {
		t.Errorf("SemanticError message should mention duplicate title, got %q", se.Error())
	}
}

// subPlanWithScope builds a sub_plans entry whose scope.files lists the given
// modify-operation paths.
func subPlanWithScope(title, hint string, paths ...string) map[string]any {
	files := make([]any, len(paths))
	for i, p := range paths {
		files[i] = map[string]any{"path": p, "operation": "modify"}
	}
	return map[string]any{
		"title": title, "scope_hint": hint,
		"scope":                        map[string]any{"files": files},
		"predicted_runtime_minutes":    5,
		"predicted_runtime_confidence": "low",
	}
}

// TestParse_DecompositionCrossSliceSharedFile_IsSemanticError covers #1062:
// two sub-plans whose declared scope.files share a path are rejected because
// the non-owning slice's edit to that file would be drift-excluded.
func TestParse_DecompositionCrossSliceSharedFile_IsSemanticError(t *testing.T) {
	const shared = "backend/internal/server/server.go"
	m := planfixture.Valid(func(m map[string]any) {
		m["decomposition"] = map[string]any{
			"rationale": "split by layer",
			"sub_plans": []any{
				subPlanWithScope("slice 1", "first", shared, "backend/internal/server/a.go"),
				subPlanWithScope("slice 2", "second", shared, "backend/internal/server/b.go"),
			},
		}
	})
	_, err := plan.Parse(marshalFixture(t, m))
	var se *plan.SemanticError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SemanticError", err)
	}
	if !strings.Contains(se.Error(), shared) {
		t.Errorf("SemanticError message should name the shared path %q, got %q", shared, se.Error())
	}
	if !strings.Contains(se.Error(), "slice 1") || !strings.Contains(se.Error(), "slice 2") {
		t.Errorf("SemanticError message should name both slices, got %q", se.Error())
	}
}

// TestParse_DecompositionDisjointSliceScopes_Succeeds confirms the check does
// not fire when each slice scopes a distinct set of files.
func TestParse_DecompositionDisjointSliceScopes_Succeeds(t *testing.T) {
	m := planfixture.Valid(func(m map[string]any) {
		m["decomposition"] = map[string]any{
			"rationale": "split by layer",
			"sub_plans": []any{
				subPlanWithScope("slice 1", "first", "backend/internal/server/a.go"),
				subPlanWithScope("slice 2", "second", "backend/internal/server/b.go"),
			},
		}
	})
	if _, err := plan.Parse(marshalFixture(t, m)); err != nil {
		t.Fatalf("Parse disjoint-slice decomposition: %v", err)
	}
}

// TestParse_DecompositionNoSubPlanScope_Succeeds confirms the check is
// additive: sub-plans without a declared scope (inheriting the parent's full
// scope.files) cannot partition unsoundly and must still parse.
func TestParse_DecompositionNoSubPlanScope_Succeeds(t *testing.T) {
	if _, err := plan.Parse(marshalFixture(t, planfixture.Decomposed())); err != nil {
		t.Fatalf("Parse decomposed fixture without sub-plan scope: %v", err)
	}
}

// TestParse_DecompositionSingleSliceRepeatedPath_Succeeds confirms only
// distinct slices trip the check: one slice listing the same path twice is a
// single claimant, not a cross-slice conflict.
func TestParse_DecompositionSingleSliceRepeatedPath_Succeeds(t *testing.T) {
	const dup = "backend/internal/server/server.go"
	m := planfixture.Valid(func(m map[string]any) {
		m["decomposition"] = map[string]any{
			"rationale": "split by layer",
			"sub_plans": []any{
				subPlanWithScope("slice 1", "first", dup, dup),
				subPlanWithScope("slice 2", "second", "backend/internal/server/b.go"),
			},
		}
	})
	if _, err := plan.Parse(marshalFixture(t, m)); err != nil {
		t.Fatalf("Parse single-slice repeated-path decomposition: %v", err)
	}
}

// --- Warnings ---

func TestWarnings_SubPlanSumLessThanParent_Explicit(t *testing.T) {
	p, err := plan.Parse([]byte(`{
  "plan_version": "standard_v1",
  "ticket_reference": {"type": "github_issue", "url": "https://x", "id": "x"},
  "generated_by": {"agent": "a", "model": "m", "timestamp": "2026-01-01T00:00:00Z"},
  "summary": "x",
  "scope": {"files": [{"path": "a.go", "operation": "create"}]},
  "approach": [{"step": 1, "description": "x"}],
  "verification": {"test_strategy": "x", "rollback_plan": "x"},
  "predicted_runtime_minutes": 30,
  "predicted_runtime_confidence": "medium",
  "decomposition": {
    "rationale": "split",
    "sub_plans": [
      {"title": "Part A", "scope_hint": "a", "predicted_runtime_minutes": 8, "predicted_runtime_confidence": "medium"},
      {"title": "Part B", "scope_hint": "b", "predicted_runtime_minutes": 6, "predicted_runtime_confidence": "medium"}
    ]
  }
}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	warns := plan.Warnings(p)
	if len(warns) == 0 {
		t.Fatal("expected at least one warning when sub-plan sum < parent runtime")
	}
}

func TestWarnings_SubPlanSumGeParent_NoWarnings(t *testing.T) {
	p, err := plan.Parse([]byte(`{
  "plan_version": "standard_v1",
  "ticket_reference": {"type": "github_issue", "url": "https://x", "id": "x"},
  "generated_by": {"agent": "a", "model": "m", "timestamp": "2026-01-01T00:00:00Z"},
  "summary": "x",
  "scope": {"files": [{"path": "a.go", "operation": "create"}]},
  "approach": [{"step": 1, "description": "x"}],
  "verification": {"test_strategy": "x", "rollback_plan": "x"},
  "predicted_runtime_minutes": 10,
  "predicted_runtime_confidence": "medium",
  "decomposition": {
    "rationale": "split",
    "sub_plans": [
      {"title": "Part A", "scope_hint": "a", "predicted_runtime_minutes": 8, "predicted_runtime_confidence": "medium"},
      {"title": "Part B", "scope_hint": "b", "predicted_runtime_minutes": 6, "predicted_runtime_confidence": "medium"}
    ]
  }
}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if warns := plan.Warnings(p); len(warns) != 0 {
		t.Errorf("expected no warnings when sub-plan sum >= parent, got %v", warns)
	}
}

func TestWarnings_ExpensiveTestStrategy(t *testing.T) {
	cases := []struct {
		name     string
		strategy string
		runtime  int
		wantWarn bool
	}{
		{
			name:     "count100_race_fullrepo_low_runtime",
			strategy: "go test -count 100 -race ./...",
			runtime:  10,
			wantWarn: true,
		},
		{
			name:     "race_fullrepo_low_runtime",
			strategy: "go test -race ./...",
			runtime:  5,
			wantWarn: true,
		},
		{
			name:     "count100_high_runtime",
			strategy: "go test -count 100 ./internal/foo/...",
			runtime:  25,
			wantWarn: false,
		},
		{
			name:     "no_expensive_flags",
			strategy: "go test ./internal/foo/...",
			runtime:  5,
			wantWarn: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &plan.Plan{
				Verification:            plan.Verification{TestStrategy: tc.strategy},
				PredictedRuntimeMinutes: tc.runtime,
			}
			warns := plan.Warnings(p)
			hasWarn := false
			for _, w := range warns {
				if strings.Contains(w, "expensive gate") {
					hasWarn = true
					break
				}
			}
			if hasWarn != tc.wantWarn {
				t.Errorf("wantWarn=%v hasWarn=%v strategy=%q runtime=%d warns=%v",
					tc.wantWarn, hasWarn, tc.strategy, tc.runtime, warns)
			}
		})
	}
}

func TestSchemaError_MultiViolation(t *testing.T) {
	// approach must be an array of step-objects; verification must be an
	// object. Providing bare strings for both should produce violations at
	// both /approach and /verification rather than only the first one.
	_, err := plan.Parse([]byte(`{
  "plan_version": "standard_v1",
  "ticket_reference": {"type": "github_issue", "url": "https://x", "id": "x"},
  "generated_by": {"agent": "a", "model": "m", "timestamp": "2026-01-01T00:00:00Z"},
  "summary": "x",
  "scope": {"files": [{"path": "a.go", "operation": "create"}]},
  "approach": "should be an array",
  "verification": "should be an object",
  "predicted_runtime_minutes": 10,
  "predicted_runtime_confidence": "medium"
}`))
	var se *plan.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError", err)
	}
	hasApproach := false
	hasVerification := false
	for _, v := range se.Violations {
		if v.Path == "/approach" {
			hasApproach = true
		}
		if v.Path == "/verification" {
			hasVerification = true
		}
	}
	if !hasApproach {
		t.Errorf("Violations missing /approach entry; got %+v", se.Violations)
	}
	if !hasVerification {
		t.Errorf("Violations missing /verification entry; got %+v", se.Violations)
	}
}

func TestSemanticError_FormatsMessage(t *testing.T) {
	err := &plan.SemanticError{Message: "duplicate title \"Foo\""}
	want := "plan: semantic: duplicate title \"Foo\""
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}
