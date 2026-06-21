package plan_test

import (
	"crypto/sha256"
	"encoding/hex"
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

// --- model_recommendation (#1013) ---

// TestParse_ModelRecommendation_RoundTrips covers the additive top-level
// model_recommendation: a plan carrying it validates and decodes all three
// fields into the typed Plan.ModelRecommendation.
func TestParse_ModelRecommendation_RoundTrips(t *testing.T) {
	m := planfixture.Valid(func(m map[string]any) {
		m["model_recommendation"] = map[string]any{
			"implement_model":     "claude-opus-4-8",
			"rationale":           "cross-layer change with non-trivial seams",
			"complexity_assessed": "high",
		}
	})
	p, err := plan.Parse(marshalFixture(t, m))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p.ModelRecommendation == nil {
		t.Fatal("ModelRecommendation should be non-nil")
	}
	if got, want := p.ModelRecommendation.ImplementModel, "claude-opus-4-8"; got != want {
		t.Errorf("ImplementModel = %q, want %q", got, want)
	}
	if got, want := p.ModelRecommendation.Rationale, "cross-layer change with non-trivial seams"; got != want {
		t.Errorf("Rationale = %q, want %q", got, want)
	}
	if got, want := p.ModelRecommendation.ComplexityAssessed, plan.ComplexityHigh; got != want {
		t.Errorf("ComplexityAssessed = %q, want %q", got, want)
	}
}

// TestParse_WithoutModelRecommendation_StillValidates confirms the field is
// additive-optional: the minimal valid plan (no recommendation) parses and the
// typed field is nil. Failure here would indicate an accidental required
// promotion.
func TestParse_WithoutModelRecommendation_StillValidates(t *testing.T) {
	p, err := plan.Parse(marshalFixture(t, planfixture.Valid()))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p.ModelRecommendation != nil {
		t.Errorf("ModelRecommendation = %+v, want nil (field omitted)", p.ModelRecommendation)
	}
}

// TestParse_SubPlanModelRecommendation_RoundTrips covers the per-child
// recommendation: a sub-plan carrying model_recommendation decodes into the
// typed SubPlanSummary.ModelRecommendation, while a sibling without it decodes
// to nil (additive-optional, per-slice).
func TestParse_SubPlanModelRecommendation_RoundTrips(t *testing.T) {
	m := planfixture.Valid(func(m map[string]any) {
		m["decomposition"] = map[string]any{
			"rationale": "split by layer",
			"sub_plans": []any{
				map[string]any{
					"title": "Part A", "scope_hint": "first slice",
					"predicted_runtime_minutes": 10, "predicted_runtime_confidence": "high",
					"model_recommendation": map[string]any{
						"implement_model":     "claude-sonnet-4-6",
						"rationale":           "mechanical slice",
						"complexity_assessed": "low",
					},
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
	if subs[0].ModelRecommendation == nil {
		t.Fatal("sub_plans[0].ModelRecommendation should be non-nil")
	}
	if got, want := subs[0].ModelRecommendation.ImplementModel, "claude-sonnet-4-6"; got != want {
		t.Errorf("sub_plans[0].ModelRecommendation.ImplementModel = %q, want %q", got, want)
	}
	if got, want := subs[0].ModelRecommendation.ComplexityAssessed, plan.ComplexityLow; got != want {
		t.Errorf("sub_plans[0].ModelRecommendation.ComplexityAssessed = %q, want %q", got, want)
	}
	if subs[1].ModelRecommendation != nil {
		t.Errorf("sub_plans[1].ModelRecommendation = %+v, want nil (field omitted)", subs[1].ModelRecommendation)
	}
}

// TestParse_ModelRecommendation_BadComplexity_IsSchemaError locks the closed
// complexity_assessed enum: an out-of-set value is rejected structurally.
func TestParse_ModelRecommendation_BadComplexity_IsSchemaError(t *testing.T) {
	m := planfixture.Valid(func(m map[string]any) {
		m["model_recommendation"] = map[string]any{
			"implement_model":     "claude-opus-4-8",
			"rationale":           "x",
			"complexity_assessed": "extreme",
		}
	})
	_, err := plan.Parse(marshalFixture(t, m))
	var se *plan.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError", err)
	}
}

// TestParse_ModelRecommendation_MissingImplementModel_IsSchemaError confirms
// the three fields are required WHEN the object is present (additive at the
// top level, complete within): a recommendation without its load-bearing
// implement_model is rejected.
func TestParse_ModelRecommendation_MissingImplementModel_IsSchemaError(t *testing.T) {
	m := planfixture.Valid(func(m map[string]any) {
		m["model_recommendation"] = map[string]any{
			"rationale":           "x",
			"complexity_assessed": "low",
		}
	})
	_, err := plan.Parse(marshalFixture(t, m))
	var se *plan.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError", err)
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

// --- depends_on / plan.Waves (#1258) ---

// subPlanDep builds a minimal sub_plans entry with an optional depends_on.
// A nil deps slice omits the field entirely (additive-optional path).
func subPlanDep(title string, deps []int) map[string]any {
	sp := map[string]any{
		"title": title, "scope_hint": title,
		"predicted_runtime_minutes": 5, "predicted_runtime_confidence": "low",
	}
	if deps != nil {
		anyDeps := make([]any, len(deps))
		for i, d := range deps {
			anyDeps[i] = d
		}
		sp["depends_on"] = anyDeps
	}
	return sp
}

// decompositionWith wraps the given sub_plans entries in a plan fixture.
func decompositionWith(subPlans ...map[string]any) []byte {
	entries := make([]any, len(subPlans))
	for i, sp := range subPlans {
		entries[i] = sp
	}
	m := planfixture.Valid(func(m map[string]any) {
		m["decomposition"] = map[string]any{
			"rationale": "split with dependencies",
			"sub_plans": entries,
		}
	})
	b, _ := json.Marshal(m)
	return b
}

// TestParse_DependsOn_RoundTrips covers the additive depends_on field: a
// decomposition whose sub_plans carry depends_on validates and decodes into
// the typed SubPlanSummary.DependsOn, while a sibling that omits it decodes to
// a nil slice (additive-optional).
func TestParse_DependsOn_RoundTrips(t *testing.T) {
	data := decompositionWith(
		subPlanDep("A", nil),
		subPlanDep("B", nil),
		subPlanDep("C", []int{0, 1}),
	)
	p, err := plan.Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	subs := p.Decomposition.SubPlans
	if subs[0].DependsOn != nil {
		t.Errorf("sub_plans[0].DependsOn = %v, want nil (field omitted)", subs[0].DependsOn)
	}
	if got, want := subs[2].DependsOn, []int{0, 1}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("sub_plans[2].DependsOn = %v, want %v", got, want)
	}
}

// TestWaves_BackCompat_SingleWave covers the back-compat collapse: a
// decomposition with no depends_on anywhere yields a single wave containing
// every index in ascending order.
func TestWaves_BackCompat_SingleWave(t *testing.T) {
	d := &plan.Decomposition{SubPlans: []plan.SubPlanSummary{
		{Title: "A"}, {Title: "B"}, {Title: "C"},
	}}
	waves, err := plan.Waves(d)
	if err != nil {
		t.Fatalf("Waves: %v", err)
	}
	if got, want := waves, [][]int{{0, 1, 2}}; !equalWaves(got, want) {
		t.Errorf("Waves = %v, want %v", got, want)
	}
}

// TestWaves_MultiWaveDAG covers the happy multi-wave path: independent 0 and 1,
// 2 depends on 0, and 3 depends on 1 and 2 layers into [[0,1],[2],[3]].
func TestWaves_MultiWaveDAG(t *testing.T) {
	d := &plan.Decomposition{SubPlans: []plan.SubPlanSummary{
		{Title: "A"},
		{Title: "B"},
		{Title: "C", DependsOn: []int{0}},
		{Title: "D", DependsOn: []int{1, 2}},
	}}
	waves, err := plan.Waves(d)
	if err != nil {
		t.Fatalf("Waves: %v", err)
	}
	if got, want := waves, [][]int{{0, 1}, {2}, {3}}; !equalWaves(got, want) {
		t.Errorf("Waves = %v, want %v", got, want)
	}
}

// TestWaves_NilAndEmpty covers the defensive guard: a nil decomposition and an
// empty sub_plans list both return (nil, nil).
func TestWaves_NilAndEmpty(t *testing.T) {
	if waves, err := plan.Waves(nil); err != nil || waves != nil {
		t.Errorf("Waves(nil) = (%v, %v), want (nil, nil)", waves, err)
	}
	if waves, err := plan.Waves(&plan.Decomposition{}); err != nil || waves != nil {
		t.Errorf("Waves(empty) = (%v, %v), want (nil, nil)", waves, err)
	}
}

// TestParse_DependsOn_Cycle_IsSemanticError covers cycle detection: 0 depends
// on 1 and 1 depends on 0 is rejected as a *SemanticError naming the cyclic
// indices.
func TestParse_DependsOn_Cycle_IsSemanticError(t *testing.T) {
	data := decompositionWith(
		subPlanDep("A", []int{1}),
		subPlanDep("B", []int{0}),
	)
	_, err := plan.Parse(data)
	var se *plan.SemanticError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SemanticError", err)
	}
	if !strings.Contains(se.Error(), "cycle") {
		t.Errorf("SemanticError message should mention a cycle, got %q", se.Error())
	}
}

// TestParse_DependsOn_OutOfRange_IsSemanticError covers the out-of-range guard:
// a depends_on index >= len(sub_plans) is rejected as a *SemanticError.
func TestParse_DependsOn_OutOfRange_IsSemanticError(t *testing.T) {
	data := decompositionWith(
		subPlanDep("A", nil),
		subPlanDep("B", []int{99}),
	)
	_, err := plan.Parse(data)
	var se *plan.SemanticError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SemanticError", err)
	}
	if !strings.Contains(se.Error(), "out of range") {
		t.Errorf("SemanticError message should mention out of range, got %q", se.Error())
	}
}

// TestParse_DependsOn_SelfIndex_IsSemanticError covers the self-dependency
// guard: a node depending on its own index is rejected as a *SemanticError.
func TestParse_DependsOn_SelfIndex_IsSemanticError(t *testing.T) {
	data := decompositionWith(
		subPlanDep("A", []int{0}),
		subPlanDep("B", nil),
	)
	_, err := plan.Parse(data)
	var se *plan.SemanticError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SemanticError", err)
	}
	if !strings.Contains(se.Error(), "itself") {
		t.Errorf("SemanticError message should mention self-dependency, got %q", se.Error())
	}
}

// TestParse_DependsOn_NegativeIndex_IsSchemaError confirms a negative index is
// caught structurally by the schema (items minimum 0), surfacing as a
// *SchemaError rather than reaching the semantic Waves check.
func TestParse_DependsOn_NegativeIndex_IsSchemaError(t *testing.T) {
	data := decompositionWith(
		subPlanDep("A", nil),
		subPlanDep("B", []int{-1}),
	)
	_, err := plan.Parse(data)
	var se *plan.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError", err)
	}
}

// equalWaves reports whether two wave slices are element-wise equal.
func equalWaves(a, b [][]int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if len(a[i]) != len(b[i]) {
			return false
		}
		for j := range a[i] {
			if a[i][j] != b[i][j] {
				return false
			}
		}
	}
	return true
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

// --- clarification_request sibling (#1057 slice 1) ---

// validClarificationJSON returns a schema-conformant clarification_request
// artifact. The mutate callback can tamper with the decoded map before it is
// re-marshalled, mirroring planfixture.Valid's option style.
func validClarificationJSON(t *testing.T, mutate ...func(map[string]any)) []byte {
	t.Helper()
	m := map[string]any{
		"kind": "clarification_request",
		"ticket_reference": map[string]any{
			"type": "github_issue",
			"url":  "https://github.com/kuhlman-labs/fishhawk/issues/1057",
			"id":   "kuhlman-labs/fishhawk#1057",
		},
		"generated_by": map[string]any{
			"agent":     "claude-code",
			"model":     "claude-opus-4-8",
			"timestamp": "2026-06-13T21:00:00Z",
		},
		"summary": "The issue needs an operator decision not derivable from the codebase.",
		"questions": []any{
			map[string]any{
				"id":                  "store",
				"question":            "Which store backs the limiter?",
				"recommended_default": "in-process",
				"tradeoffs":           "resets on restart vs an added dependency",
			},
			map[string]any{
				"id":                  "policy",
				"question":            "What ceiling?",
				"recommended_default": "60/min",
				"tradeoffs":           "tighter protects, looser permits bursts",
			},
		},
	}
	for _, fn := range mutate {
		fn(m)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal clarification fixture: %v", err)
	}
	return b
}

func TestDetectArtifactKind(t *testing.T) {
	k, err := plan.DetectArtifactKind(validClarificationJSON(t))
	if err != nil {
		t.Fatalf("DetectArtifactKind(clarification): %v", err)
	}
	if k != plan.ArtifactKindClarificationRequest {
		t.Errorf("kind = %q, want %q", k, plan.ArtifactKindClarificationRequest)
	}

	k, err = plan.DetectArtifactKind(readFixture(t, "valid/example.json"))
	if err != nil {
		t.Fatalf("DetectArtifactKind(plan): %v", err)
	}
	if k != plan.ArtifactKindPlan {
		t.Errorf("kind = %q, want %q", k, plan.ArtifactKindPlan)
	}

	var perr *plan.ParseError
	if _, err := plan.DetectArtifactKind(nil); !errors.As(err, &perr) {
		t.Errorf("empty: err = %v, want *ParseError", err)
	}
	if _, err := plan.DetectArtifactKind([]byte("{ not json")); !errors.As(err, &perr) {
		t.Errorf("malformed: err = %v, want *ParseError", err)
	}
}

func TestValidateArtifact_RoutesByKind(t *testing.T) {
	if err := plan.ValidateArtifact(validClarificationJSON(t)); err != nil {
		t.Errorf("ValidateArtifact(clarification): %v", err)
	}
	if err := plan.ValidateArtifact(readFixture(t, "valid/example.json")); err != nil {
		t.Errorf("ValidateArtifact(plan): %v", err)
	}
}

func TestParseClarificationRequest_Valid(t *testing.T) {
	cr, err := plan.ParseClarificationRequest(validClarificationJSON(t))
	if err != nil {
		t.Fatalf("ParseClarificationRequest: %v", err)
	}
	if cr.Kind != plan.KindClarificationRequest {
		t.Errorf("Kind = %q", cr.Kind)
	}
	if len(cr.Questions) != 2 {
		t.Fatalf("questions = %d, want 2", len(cr.Questions))
	}
	if cr.Questions[0].ID != "store" || cr.Questions[0].RecommendedDefault != "in-process" {
		t.Errorf("question[0] = %+v", cr.Questions[0])
	}
	if cr.TicketReference.Type != plan.TicketTypeGitHubIssue {
		t.Errorf("ticket type = %q", cr.TicketReference.Type)
	}
}

// TestValidateClarificationRequest_DuplicateID locks the binding-condition
// fix: a duplicate question id must be rejected by the VALIDATE path (not only
// the typed parse path), because operator answers are keyed by id.
func TestValidateClarificationRequest_DuplicateID(t *testing.T) {
	dup := validClarificationJSON(t, func(m map[string]any) {
		qs := m["questions"].([]any)
		qs[1].(map[string]any)["id"] = "store" // collide with questions[0]
	})

	var se *plan.SemanticError
	if err := plan.ValidateClarificationRequest(dup); !errors.As(err, &se) {
		t.Fatalf("ValidateClarificationRequest: err = %v, want *SemanticError", err)
	} else if !strings.Contains(se.Message, "duplicate id") {
		t.Errorf("message = %q, want it to mention duplicate id", se.Message)
	}

	// The discriminating entry point and the typed parse must reject it too.
	if err := plan.ValidateArtifact(dup); !errors.As(err, &se) {
		t.Errorf("ValidateArtifact: err = %v, want *SemanticError", err)
	}
	if _, err := plan.ParseClarificationRequest(dup); !errors.As(err, &se) {
		t.Errorf("ParseClarificationRequest: err = %v, want *SemanticError", err)
	}
}

func TestValidateClarificationRequest_SchemaViolations(t *testing.T) {
	// Missing recommended_default — required by the calibration guard.
	missingDefault := validClarificationJSON(t, func(m map[string]any) {
		q := m["questions"].([]any)[0].(map[string]any)
		delete(q, "recommended_default")
	})
	var se *plan.SchemaError
	if err := plan.ValidateClarificationRequest(missingDefault); !errors.As(err, &se) {
		t.Errorf("missing recommended_default: err = %v, want *SchemaError", err)
	}

	// Wrong discriminator value is rejected by the const constraint.
	badKind := validClarificationJSON(t, func(m map[string]any) {
		m["kind"] = "something_else"
	})
	// DetectArtifactKind routes a non-clarification kind to the plan schema,
	// which rejects it (no plan_version etc). ValidateArtifact must error.
	if err := plan.ValidateArtifact(badKind); err == nil {
		t.Error("ValidateArtifact(bad kind): want error, got nil")
	}

	var pe *plan.ParseError
	if err := plan.ValidateClarificationRequest(nil); !errors.As(err, &pe) {
		t.Errorf("empty: err = %v, want *ParseError", err)
	}
}

// TestPlanSchemaFrozen pins the embedded plan-standard-v1 schema's sha256
// as a drift guard: any byte change (an unintended edit, or a docs/spec
// sync that did not land in the embedded copy) fails this test deliberately.
// The hash is re-pinned only for a sanctioned additive-optional change within
// standard_v1.x — most recently the #1013 model_recommendation field. A
// standard_v1 plan that omits the new optional fields must still validate
// unchanged through the plan-only Validate entry point (asserted below),
// which is the proof the change did not break the schema in place.
func TestPlanSchemaFrozen(t *testing.T) {
	const wantHash = "ec31a64b33bd131bb8bc9cea4175bccf2ff9ac57f722d7baf88fd496dba2f9e3"
	b, err := os.ReadFile("schemas/plan-standard-v1.schema.json")
	if err != nil {
		t.Fatalf("read embedded plan schema: %v", err)
	}
	sum := sha256.Sum256(b)
	if got := hex.EncodeToString(sum[:]); got != wantHash {
		t.Errorf("plan-standard-v1.schema.json sha256 = %s, want %s (the plan schema is frozen — do not edit it)", got, wantHash)
	}

	if err := plan.Validate(readFixture(t, "valid/example.json")); err != nil {
		t.Errorf("standard_v1 plan no longer validates through Validate: %v", err)
	}
}
