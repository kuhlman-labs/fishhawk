package plan_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/plan"
	"github.com/kuhlman-labs/fishhawk/runner/internal/plan/planfixture"
)

var testNow = time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)

func marshal(t *testing.T, m map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestTryCoerce_GeneratedByString verifies bare string at /generated_by is
// wrapped into the canonical generated_by object shape.
func TestTryCoerce_GeneratedByString(t *testing.T) {
	m := planfixture.Valid()
	m["generated_by"] = "my-agent"
	data := marshal(t, m)

	coercedBytes, coercions, err := plan.TryCoerce(data, testNow)
	if err != nil {
		t.Fatalf("TryCoerce: unexpected error: %v", err)
	}
	if len(coercions) != 1 {
		t.Fatalf("coercions = %d, want 1", len(coercions))
	}
	if got := coercions[0].FieldPath; got != "/generated_by" {
		t.Errorf("FieldPath = %q, want /generated_by", got)
	}
	if got := coercions[0].OriginalType; got != "string" {
		t.Errorf("OriginalType = %q, want string", got)
	}
	if got, ok := coercions[0].OriginalValue.(string); !ok || got != "my-agent" {
		t.Errorf("OriginalValue = %v, want string \"my-agent\"", coercions[0].OriginalValue)
	}
	if coercedBytes == nil {
		t.Fatal("coercedBytes is nil")
	}
	if err := plan.Validate(coercedBytes); err != nil {
		t.Errorf("coerced plan does not validate: %v", err)
	}
}

// TestTryCoerce_ScopeFileString verifies bare string at /scope/files[0] is
// wrapped into {path, operation:"modify"}.
func TestTryCoerce_ScopeFileString(t *testing.T) {
	m := planfixture.Valid()
	scope := m["scope"].(map[string]any)
	scope["files"] = []any{"a.go"}
	data := marshal(t, m)

	coercedBytes, coercions, err := plan.TryCoerce(data, testNow)
	if err != nil {
		t.Fatalf("TryCoerce: unexpected error: %v", err)
	}
	if len(coercions) != 1 {
		t.Fatalf("coercions = %d, want 1", len(coercions))
	}
	if got := coercions[0].FieldPath; got != "/scope/files/0" {
		t.Errorf("FieldPath = %q, want /scope/files/0", got)
	}
	if got := coercions[0].OriginalType; got != "string" {
		t.Errorf("OriginalType = %q, want string", got)
	}
	if got, ok := coercions[0].OriginalValue.(string); !ok || got != "a.go" {
		t.Errorf("OriginalValue = %v, want string \"a.go\"", coercions[0].OriginalValue)
	}
	if coercedBytes == nil {
		t.Fatal("coercedBytes is nil")
	}
	if err := plan.Validate(coercedBytes); err != nil {
		t.Errorf("coerced plan does not validate: %v", err)
	}
}

// TestTryCoerce_SubPlanString verifies bare string at
// /decomposition/sub_plans[0] is wrapped into the sentinel default shape.
func TestTryCoerce_SubPlanString(t *testing.T) {
	m := planfixture.Valid(func(m map[string]any) {
		m["decomposition"] = map[string]any{
			"rationale": "scope too large",
			"sub_plans": []any{
				"Part A", // bare string — subject of coercion
				map[string]any{
					"title":                        "Part B",
					"scope_hint":                   "second half",
					"predicted_runtime_minutes":    10,
					"predicted_runtime_confidence": "medium",
				},
			},
		}
	})
	data := marshal(t, m)

	coercedBytes, coercions, err := plan.TryCoerce(data, testNow)
	if err != nil {
		t.Fatalf("TryCoerce: unexpected error: %v", err)
	}
	if len(coercions) != 1 {
		t.Fatalf("coercions = %d, want 1", len(coercions))
	}
	if got := coercions[0].FieldPath; got != "/decomposition/sub_plans/0" {
		t.Errorf("FieldPath = %q, want /decomposition/sub_plans/0", got)
	}
	if got := coercions[0].OriginalType; got != "string" {
		t.Errorf("OriginalType = %q, want string", got)
	}
	if got, ok := coercions[0].OriginalValue.(string); !ok || got != "Part A" {
		t.Errorf("OriginalValue = %v, want string \"Part A\"", coercions[0].OriginalValue)
	}
	if coercedBytes == nil {
		t.Fatal("coercedBytes is nil")
	}
	if err := plan.Validate(coercedBytes); err != nil {
		t.Errorf("coerced plan does not validate: %v", err)
	}
}

// TestTryCoerce_IntegerScopeFile verifies non-string types are not coercible
// and the original schema error propagates to the caller.
func TestTryCoerce_IntegerScopeFile(t *testing.T) {
	m := planfixture.Valid()
	scope := m["scope"].(map[string]any)
	scope["files"] = []any{42}
	data := marshal(t, m)

	coercedBytes, coercions, err := plan.TryCoerce(data, testNow)
	if coercedBytes != nil {
		t.Errorf("coercedBytes = non-nil, want nil for non-coercible type")
	}
	if len(coercions) != 0 {
		t.Errorf("coercions = %d, want 0", len(coercions))
	}
	if err == nil {
		t.Error("err = nil, want non-nil: integer is not coercible and original schema error should propagate")
	}
}

// TestTryCoerce_TicketReferenceString verifies that a bare URL string at
// /ticket_reference is wrapped into the canonical ticket_reference object shape.
func TestTryCoerce_TicketReferenceString(t *testing.T) {
	m := planfixture.Valid()
	m["ticket_reference"] = "https://github.com/x/y/issues/547"
	data := marshal(t, m)

	coercedBytes, coercions, err := plan.TryCoerce(data, testNow)
	if err != nil {
		t.Fatalf("TryCoerce: unexpected error: %v", err)
	}
	if len(coercions) != 1 {
		t.Fatalf("coercions = %d, want 1", len(coercions))
	}
	if got := coercions[0].FieldPath; got != "/ticket_reference" {
		t.Errorf("FieldPath = %q, want /ticket_reference", got)
	}
	if got := coercions[0].OriginalType; got != "string" {
		t.Errorf("OriginalType = %q, want string", got)
	}
	if got, ok := coercions[0].OriginalValue.(string); !ok || got != "https://github.com/x/y/issues/547" {
		t.Errorf("OriginalValue = %v, want string \"https://github.com/x/y/issues/547\"", coercions[0].OriginalValue)
	}
	if coercedBytes == nil {
		t.Fatal("coercedBytes is nil")
	}
	if err := plan.Validate(coercedBytes); err != nil {
		t.Errorf("coerced plan does not validate: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(coercedBytes, &result); err != nil {
		t.Fatalf("unmarshal coerced: %v", err)
	}
	tr, _ := result["ticket_reference"].(map[string]any)
	if tr["url"] != "https://github.com/x/y/issues/547" {
		t.Errorf("ticket_reference.url = %v, want https://github.com/x/y/issues/547", tr["url"])
	}
	if tr["type"] != "github_issue" {
		t.Errorf("ticket_reference.type = %v, want github_issue", tr["type"])
	}
	if tr["id"] != "unknown" {
		t.Errorf("ticket_reference.id = %v, want unknown", tr["id"])
	}
}

// TestTryCoerce_IntegerTicketReference verifies that a non-string type at
// /ticket_reference (an integer) is not coercible and propagates a non-nil
// error — the caller falls through to the existing rejection path.
func TestTryCoerce_IntegerTicketReference(t *testing.T) {
	m := planfixture.Valid()
	m["ticket_reference"] = 42
	data := marshal(t, m)

	coercedBytes, coercions, err := plan.TryCoerce(data, testNow)
	if coercedBytes != nil {
		t.Errorf("coercedBytes = non-nil, want nil for non-coercible type")
	}
	if len(coercions) != 0 {
		t.Errorf("coercions = %d, want 0", len(coercions))
	}
	if err == nil {
		t.Error("err = nil, want non-nil: integer is not coercible and original schema error should propagate")
	}
}

// TestTryCoerce_AlreadyValid verifies a plan that already satisfies the
// schema produces (nil, nil, nil) — caller MUST keep the original bytes
// unchanged so the content hash signed by the runner remains stable.
func TestTryCoerce_AlreadyValid(t *testing.T) {
	data := marshal(t, planfixture.Valid())

	coercedBytes, coercions, err := plan.TryCoerce(data, testNow)
	if coercedBytes != nil {
		t.Errorf("coercedBytes = non-nil on already-valid plan")
	}
	if len(coercions) != 0 {
		t.Errorf("coercions = %d, want 0", len(coercions))
	}
	if err != nil {
		t.Errorf("err = %v, want nil", err)
	}
}
