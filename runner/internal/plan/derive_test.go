package plan

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/kuhlman-labs/fishhawk/runner/internal/plan/planfixture"
)

// rejectedKeywords are the JSON Schema keywords the claude CLI structured-output
// subset rejects (verified 2026-06-23). The derivation must strip every one
// ANYWHERE in the document — a single survivor makes the CLI silently emit no
// structured_output.
var rejectedKeywords = []string{"$ref", "$defs", "format", "$schema", "$id", "x-coerce-principal", "x-coerce-defaults", "x-intended-required", "oneOf"}

// walkKeys invokes fn for every object key anywhere in the decoded JSON tree.
func walkKeys(node any, fn func(key string)) {
	switch n := node.(type) {
	case map[string]any:
		for k, v := range n {
			fn(k)
			walkKeys(v, fn)
		}
	case []any:
		for _, v := range n {
			walkKeys(v, fn)
		}
	}
}

func TestStructuredOutputSchema_StripsRejectedKeywords(t *testing.T) {
	b, err := StructuredOutputSchema()
	if err != nil {
		t.Fatalf("StructuredOutputSchema: %v", err)
	}
	var tree any
	if err := json.Unmarshal(b, &tree); err != nil {
		t.Fatalf("derived schema is not valid JSON: %v", err)
	}
	for _, kw := range rejectedKeywords {
		walkKeys(tree, func(key string) {
			if key == kw {
				t.Errorf("derived schema still contains rejected keyword %q (CLI subset would drop structured_output)", kw)
			}
		})
	}
}

// TestStructuredOutputSchema_StripsAllVendorExtensionKeys pins the #1741
// prefix rule directly: claude CLI 2.1.205's strict --json-schema validation
// rejects ANY unknown keyword, so no "x-"-prefixed key of any name may
// survive the derivation — not just the ones enumerated in rejectedKeywords.
// This fails if a future annotation (the next x-something the schema
// checklist invents) reaches the CLI and re-runs the outage.
func TestStructuredOutputSchema_StripsAllVendorExtensionKeys(t *testing.T) {
	b, err := StructuredOutputSchema()
	if err != nil {
		t.Fatalf("StructuredOutputSchema: %v", err)
	}
	var tree any
	if err := json.Unmarshal(b, &tree); err != nil {
		t.Fatalf("derived schema is not valid JSON: %v", err)
	}
	walkKeys(tree, func(key string) {
		if strings.HasPrefix(key, "x-") {
			t.Errorf("derived schema still contains vendor-extension key %q (claude CLI 2.1.205 strict mode hard-rejects it, #1741)", key)
		}
	})
}

// compileDerived compiles the derived schema with the jsonschema library, the
// same library the runner uses to validate plans — so the derived schema is
// itself a well-formed compilable schema, not just keyword-stripped text.
func compileDerived(t *testing.T) *jsonschema.Schema {
	t.Helper()
	b, err := StructuredOutputSchema()
	if err != nil {
		t.Fatalf("StructuredOutputSchema: %v", err)
	}
	var raw any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal derived schema: %v", err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("derived.json", raw); err != nil {
		t.Fatalf("add derived schema: %v", err)
	}
	s, err := c.Compile("derived.json")
	if err != nil {
		t.Fatalf("compile derived schema: %v", err)
	}
	return s
}

func TestStructuredOutputSchema_ValidatesRepresentativePlans(t *testing.T) {
	s := compileDerived(t)

	cases := map[string]map[string]any{
		"minimal":       planfixture.Valid(),
		"decomposition": planfixture.Decomposed(),
	}
	for name, fixture := range cases {
		t.Run(name, func(t *testing.T) {
			b, err := json.Marshal(fixture)
			if err != nil {
				t.Fatalf("marshal fixture: %v", err)
			}
			var inst any
			if err := json.Unmarshal(b, &inst); err != nil {
				t.Fatalf("unmarshal fixture: %v", err)
			}
			if err := s.Validate(inst); err != nil {
				t.Errorf("representative %s plan failed the derived schema: %v", name, err)
			}
		})
	}
}

// TestStructuredOutputSchema_InlinedShapesArePreserved is the cross-boundary
// faithfulness assertion: inlining $refs must keep the nested object shapes
// (not collapse them to bare strings), so the derived schema still rejects the
// string-elision class the coercion path exists to fix.
func TestStructuredOutputSchema_InlinedShapesArePreserved(t *testing.T) {
	b, err := StructuredOutputSchema()
	if err != nil {
		t.Fatalf("StructuredOutputSchema: %v", err)
	}
	var root map[string]any
	if err := json.Unmarshal(b, &root); err != nil {
		t.Fatalf("unmarshal derived schema: %v", err)
	}
	props, ok := root["properties"].(map[string]any)
	if !ok {
		t.Fatal("derived schema missing top-level properties")
	}

	// ticket_reference was a $ref to an object def — after inlining it must be
	// an inline object schema with its own properties (path/url/id), not a
	// dangling $ref or a bare-string type.
	tr, ok := props["ticket_reference"].(map[string]any)
	if !ok {
		t.Fatal("ticket_reference is not an object schema after inlining")
	}
	if tr["type"] != "object" {
		t.Errorf("ticket_reference type = %v, want object", tr["type"])
	}
	if _, ok := tr["properties"].(map[string]any); !ok {
		t.Error("ticket_reference lost its inlined properties")
	}
	// format:uri on ticket_reference.url must have been stripped.
	if trProps, ok := tr["properties"].(map[string]any); ok {
		if url, ok := trProps["url"].(map[string]any); ok {
			if _, has := url["format"]; has {
				t.Error("ticket_reference.url still carries a format keyword")
			}
		}
	}

	// scope.files items must remain an object schema (path + operation), the
	// shape the string-elision coercion exists to repair.
	scope, ok := props["scope"].(map[string]any)
	if !ok {
		t.Fatal("scope is not an object schema after inlining")
	}
	scopeProps, _ := scope["properties"].(map[string]any)
	files, _ := scopeProps["files"].(map[string]any)
	items, ok := files["items"].(map[string]any)
	if !ok {
		t.Fatal("scope.files.items is not an object schema after inlining")
	}
	if items["type"] != "object" {
		t.Errorf("scope.files items type = %v, want object (not a bare string)", items["type"])
	}

	// approach items must remain objects (step + description).
	approach, _ := props["approach"].(map[string]any)
	aItems, ok := approach["items"].(map[string]any)
	if !ok || aItems["type"] != "object" {
		t.Errorf("approach items not an object schema after inlining: %v", approach["items"])
	}
}

// TestResolveRef_Errors pins the derivation-error branches: an unsupported ref
// form and an unresolvable name each error rather than emitting a dangling
// reference. This is the unit-level half of the graceful-degradation guarantee
// (the runner-level half lives in main_test).
func TestResolveRef_Errors(t *testing.T) {
	defs := map[string]any{"known": map[string]any{"type": "object"}}
	if _, err := resolveRef("https://example.com/x.json", defs); err == nil {
		t.Error("expected error for non-local $ref")
	}
	if _, err := resolveRef("#/$defs/missing", defs); err == nil {
		t.Error("expected error for unresolvable $ref")
	}
	if _, err := resolveRef("#/$defs/known", defs); err != nil {
		t.Errorf("unexpected error for resolvable $ref: %v", err)
	}
}

// TestInlineNode_CycleGuard pins the max-depth guard: a self-referential def
// errors instead of recursing without bound, so a future cyclic schema
// degrades to the runner's graceful fallback rather than a stack overflow.
func TestInlineNode_CycleGuard(t *testing.T) {
	defs := map[string]any{
		"loop": map[string]any{"$ref": "#/$defs/loop"},
	}
	if _, err := inlineNode(map[string]any{"$ref": "#/$defs/loop"}, defs, 0); err == nil {
		t.Error("expected a max-depth error on a self-referential $ref cycle")
	}
}
