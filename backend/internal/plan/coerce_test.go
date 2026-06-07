package plan_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan/planfixture"
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

// TestTryCoerce_GeneratedByString verifies that a bare string at /generated_by
// is wrapped into the canonical generated_by object shape.
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

// TestTryCoerce_ScopeFileString verifies that a bare string at /scope/files[0]
// is wrapped into {path, operation:"modify"}.
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

// TestTryCoerce_SubPlanString verifies that a bare string at
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

// TestTryCoerce_IntegerScopeFile verifies that a non-string type at
// /scope/files[0] (an integer) is not coercible and propagates a non-nil
// error — the caller falls through to the existing 400 path.
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

// TestTryCoerce_TicketReferenceObjectWrongType verifies that an object-form
// ticket_reference whose `type` is set to a non-canonical value is normalized
// to the sole valid enum value (github_issue) and validates on the first pass
// with exactly one coercion at /ticket_reference/type.
func TestTryCoerce_TicketReferenceObjectWrongType(t *testing.T) {
	m := planfixture.Valid()
	m["ticket_reference"] = map[string]any{
		"type": "issue", // wrong value — not in the single-element enum
		"url":  "https://github.com/x/y/issues/742",
		"id":   "x/y#742",
	}
	data := marshal(t, m)

	coercedBytes, coercions, err := plan.TryCoerce(data, testNow)
	if err != nil {
		t.Fatalf("TryCoerce: unexpected error: %v", err)
	}
	if len(coercions) != 1 {
		t.Fatalf("coercions = %d, want 1", len(coercions))
	}
	if got := coercions[0].FieldPath; got != "/ticket_reference/type" {
		t.Errorf("FieldPath = %q, want /ticket_reference/type", got)
	}
	if got := coercions[0].OriginalType; got != "string" {
		t.Errorf("OriginalType = %q, want string", got)
	}
	if got, ok := coercions[0].OriginalValue.(string); !ok || got != "issue" {
		t.Errorf("OriginalValue = %v, want string \"issue\"", coercions[0].OriginalValue)
	}
	if got := coercions[0].CoercedTo; got != "github_issue" {
		t.Errorf("CoercedTo = %v, want github_issue", got)
	}
	if coercedBytes == nil {
		t.Fatal("coercedBytes is nil")
	}
	if err := plan.Validate(coercedBytes); err != nil {
		t.Errorf("coerced plan does not validate: %v", err)
	}
}

// TestTryCoerce_TicketReferenceObjectMissingType verifies that an object-form
// ticket_reference with `type` missing entirely is normalized to the sole valid
// enum value and validates on the first pass with exactly one coercion.
func TestTryCoerce_TicketReferenceObjectMissingType(t *testing.T) {
	m := planfixture.Valid()
	m["ticket_reference"] = map[string]any{
		"url": "https://github.com/x/y/issues/742",
		"id":  "x/y#742",
	}
	data := marshal(t, m)

	coercedBytes, coercions, err := plan.TryCoerce(data, testNow)
	if err != nil {
		t.Fatalf("TryCoerce: unexpected error: %v", err)
	}
	if len(coercions) != 1 {
		t.Fatalf("coercions = %d, want 1", len(coercions))
	}
	if got := coercions[0].FieldPath; got != "/ticket_reference/type" {
		t.Errorf("FieldPath = %q, want /ticket_reference/type", got)
	}
	if got := coercions[0].OriginalType; got != "missing" {
		t.Errorf("OriginalType = %q, want missing", got)
	}
	if coercions[0].OriginalValue != nil {
		t.Errorf("OriginalValue = %v, want nil", coercions[0].OriginalValue)
	}
	if got := coercions[0].CoercedTo; got != "github_issue" {
		t.Errorf("CoercedTo = %v, want github_issue", got)
	}
	if coercedBytes == nil {
		t.Fatal("coercedBytes is nil")
	}
	if err := plan.Validate(coercedBytes); err != nil {
		t.Errorf("coerced plan does not validate: %v", err)
	}
}

// TestTryCoerce_TicketReferenceObjectWellFormed verifies that a well-formed
// object-form ticket_reference (type already github_issue) produces zero
// coercions and is left unchanged — the no-op / keep-original path.
func TestTryCoerce_TicketReferenceObjectWellFormed(t *testing.T) {
	m := planfixture.Valid()
	m["ticket_reference"] = map[string]any{
		"type": "github_issue",
		"url":  "https://github.com/x/y/issues/742",
		"id":   "x/y#742",
	}
	data := marshal(t, m)

	coercedBytes, coercions, err := plan.TryCoerce(data, testNow)
	if err != nil {
		t.Errorf("err = %v, want nil", err)
	}
	for _, c := range coercions {
		if strings.HasPrefix(c.FieldPath, "/ticket_reference") {
			t.Errorf("unexpected coercion at %q on well-formed ticket_reference", c.FieldPath)
		}
	}
	if len(coercions) != 0 {
		t.Errorf("coercions = %d, want 0 on already-valid plan", len(coercions))
	}
	if coercedBytes != nil {
		t.Errorf("coercedBytes = non-nil on already-valid plan")
	}
}

// TestTryCoerce_IntegerTicketReference verifies that a non-string type at
// /ticket_reference (an integer) is not coercible and propagates a non-nil
// error — the caller falls through to the existing 400 path.
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

// TestTryCoerce_PartialCoercionWithRemainingViolation exercises the production
// failure shape: agent emits a coercible field (generated_by as bare string)
// AND a non-coercible field (approach as bare string — expects array). Coercion
// fires on generated_by but the plan is still invalid after. The fix: TryCoerce
// returns (coercedBytes, coercions, err) so callers report the post-coercion
// violation (/approach) rather than the original /generated_by error.
func TestTryCoerce_PartialCoercionWithRemainingViolation(t *testing.T) {
	m := planfixture.Valid()
	m["generated_by"] = "my-agent" // coercible: bare string → object
	m["approach"] = "do the thing" // non-coercible: string where array required
	data := marshal(t, m)

	coercedBytes, coercions, err := plan.TryCoerce(data, testNow)

	if err == nil {
		t.Fatal("err = nil, want non-nil: approach is not coercible so plan remains invalid")
	}
	if len(coercions) != 1 {
		t.Fatalf("coercions = %d, want 1 (generated_by coerced; approach not coercible)", len(coercions))
	}
	if got := coercions[0].FieldPath; got != "/generated_by" {
		t.Errorf("coercions[0].FieldPath = %q, want /generated_by", got)
	}
	if coercedBytes == nil {
		t.Fatal("coercedBytes = nil, want non-nil: partial fix bytes must be returned so caller reports post-coercion error")
	}
	// The returned error must name the remaining violation, not generated_by.
	errStr := err.Error()
	if strings.Contains(errStr, "generated_by") {
		t.Errorf("err mentions generated_by (already coerced); want error naming remaining violation: %v", err)
	}
}

// TestTryCoerce_AlreadyValid verifies that a plan that already satisfies the
// schema produces (nil, nil, nil) — the caller must keep the original bytes
// unchanged so the content hash is stable.
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

// TestTryCoerce_NullDecompositionDropped verifies that an optional top-level
// field set to JSON null (decomposition) is dropped and treated as absent so
// the plan validates, rather than deterministically failing schema validation.
func TestTryCoerce_NullDecompositionDropped(t *testing.T) {
	m := planfixture.Valid()
	m["decomposition"] = nil
	data := marshal(t, m)

	coercedBytes, coercions, err := plan.TryCoerce(data, testNow)
	if err != nil {
		t.Fatalf("TryCoerce: unexpected error: %v", err)
	}
	if len(coercions) != 1 {
		t.Fatalf("coercions = %d, want 1", len(coercions))
	}
	if got := coercions[0].FieldPath; got != "/decomposition" {
		t.Errorf("FieldPath = %q, want /decomposition", got)
	}
	if got := coercions[0].OriginalType; got != "null" {
		t.Errorf("OriginalType = %q, want null", got)
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
	if _, present := result["decomposition"]; present {
		t.Error("decomposition key still present after coercion; want dropped")
	}
}

// TestTryCoerce_NullRequiredFieldNotDropped verifies that a JSON null in a
// REQUIRED field (summary) is NOT dropped — it must still fail Validate with a
// precise message rather than being silently removed.
func TestTryCoerce_NullRequiredFieldNotDropped(t *testing.T) {
	m := planfixture.Valid()
	m["summary"] = nil
	data := marshal(t, m)

	_, coercions, err := plan.TryCoerce(data, testNow)
	if err == nil {
		t.Fatal("err = nil, want non-nil: null required field must fail validation")
	}
	for _, c := range coercions {
		if c.FieldPath == "/summary" {
			t.Errorf("required field /summary was coerced/dropped; want left in place")
		}
	}
}

// corpusGlob resolves to the repo-root shared coercion corpus. The Go test
// binary runs with the package source directory as its working directory, so
// ../../../ from this package reaches repo root; the runner mirror
// (runner/internal/plan) uses the identical relative path.
const corpusGlob = "../../../testdata/coercion-corpus/*.json"

// corpusExpectedCoercion pins one Coercion record that must be present.
type corpusExpectedCoercion struct {
	FieldPath    string          `json:"field_path"`
	OriginalType string          `json:"original_type"`
	CoercedTo    json.RawMessage `json:"coerced_to,omitempty"`
}

// corpusCase is one self-describing coercion case. See
// testdata/coercion-corpus/README.md for the field semantics.
type corpusCase struct {
	Name                  string                   `json:"name"`
	Input                 json.RawMessage          `json:"input"`
	ExpectedOutput        json.RawMessage          `json:"expected_output,omitempty"`
	ExpectedCoercions     []corpusExpectedCoercion `json:"expected_coercions,omitempty"`
	ExpectedCoercionCount int                      `json:"expected_coercion_count"`
	ExpectError           bool                     `json:"expect_error,omitempty"`
}

// TestCoercionCorpus walks the shared repo-root corpus and runs this module's
// TryCoerce against every case, asserting the EXACT coercion count, the pinned
// coercion records, and the semantic equality of the coerced output. The runner
// module runs a near-duplicate test against the same corpus; if either mirror's
// TryCoerce drifts, that module's corpus test fails — the cross-module drift
// guard from #834 (root cause of #832).
func TestCoercionCorpus(t *testing.T) {
	files, err := filepath.Glob(corpusGlob)
	if err != nil {
		t.Fatalf("glob corpus %q: %v", corpusGlob, err)
	}
	if len(files) == 0 {
		t.Fatalf("coercion corpus is empty: glob %q matched no files", corpusGlob)
	}

	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read corpus case %s: %v", file, err)
		}
		var tc corpusCase
		if err := json.Unmarshal(data, &tc); err != nil {
			t.Fatalf("parse corpus case %s: %v", file, err)
		}
		name := tc.Name
		if name == "" {
			name = filepath.Base(file)
		}

		t.Run(name, func(t *testing.T) {
			coerced, coercions, err := plan.TryCoerce([]byte(tc.Input), testNow)

			if tc.ExpectError {
				if err == nil {
					t.Errorf("err = nil, want non-nil (expect_error case)")
				}
			} else if err != nil {
				t.Fatalf("TryCoerce: unexpected error: %v", err)
			}

			if len(coercions) != tc.ExpectedCoercionCount {
				t.Errorf("coercion count = %d, want %d; coercions = %+v", len(coercions), tc.ExpectedCoercionCount, coercions)
			}

			for _, want := range tc.ExpectedCoercions {
				assertPinnedCoercion(t, coercions, want)
			}

			if len(tc.ExpectedOutput) > 0 {
				got := coerced
				if got == nil {
					// Zero-coercion / already-valid path: TryCoerce returns the
					// original bytes unchanged (content-hash stability).
					got = []byte(tc.Input)
				}
				if !jsonSemanticEqual(t, tc.ExpectedOutput, got) {
					t.Errorf("coerced output mismatch\n got: %s\nwant: %s", got, tc.ExpectedOutput)
				}
			}
		})
	}
}

// assertPinnedCoercion finds a coercion at want.FieldPath and checks its
// OriginalType and (when pinned) CoercedTo match.
func assertPinnedCoercion(t *testing.T, coercions []plan.Coercion, want corpusExpectedCoercion) {
	t.Helper()
	for _, got := range coercions {
		if got.FieldPath != want.FieldPath {
			continue
		}
		if got.OriginalType != want.OriginalType {
			t.Errorf("coercion %q: original_type = %q, want %q", want.FieldPath, got.OriginalType, want.OriginalType)
		}
		if len(want.CoercedTo) > 0 {
			gotBytes, err := json.Marshal(got.CoercedTo)
			if err != nil {
				t.Fatalf("marshal coerced_to for %q: %v", want.FieldPath, err)
			}
			if !jsonSemanticEqual(t, want.CoercedTo, gotBytes) {
				t.Errorf("coercion %q: coerced_to = %s, want %s", want.FieldPath, gotBytes, want.CoercedTo)
			}
		}
		return
	}
	t.Errorf("expected coercion at %q not found; coercions = %+v", want.FieldPath, coercions)
}

// jsonSemanticEqual reports whether two JSON byte slices are equal ignoring key
// order and whitespace, by decoding both to any and comparing with DeepEqual.
func jsonSemanticEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		t.Fatalf("unmarshal %s: %v", a, err)
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		t.Fatalf("unmarshal %s: %v", b, err)
	}
	return reflect.DeepEqual(av, bv)
}
