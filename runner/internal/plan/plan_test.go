package plan

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/runner/internal/plan/planfixture"
)

func fixtureJSON(t *testing.T, m map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestValidate_Valid(t *testing.T) {
	if err := Validate(fixtureJSON(t, planfixture.Valid())); err != nil {
		t.Errorf("Validate: unexpected error: %v", err)
	}
}

func TestValidate_EmptyDocument(t *testing.T) {
	var perr *ParseError
	if err := Validate(nil); !errors.As(err, &perr) {
		t.Fatalf("err = %v, want *ParseError", err)
	}
	if err := Validate([]byte("   ")); !errors.As(err, &perr) {
		t.Fatalf("whitespace err = %v, want *ParseError", err)
	}
}

func TestValidate_MalformedJSON(t *testing.T) {
	var perr *ParseError
	err := Validate([]byte("{ not json"))
	if !errors.As(err, &perr) {
		t.Fatalf("err = %v, want *ParseError", err)
	}
	if perr.Cause == nil {
		t.Error("expected ParseError.Cause to be set")
	}
}

func TestValidate_MissingRequired(t *testing.T) {
	// Drop the `summary` field — schema requires it.
	src := fixtureJSON(t, planfixture.Valid())
	tampered := strings.Replace(string(src), `"summary":`, `"summary_renamed":`, 1)
	var serr *SchemaError
	err := Validate([]byte(tampered))
	if !errors.As(err, &serr) {
		t.Fatalf("err = %v, want *SchemaError", err)
	}
	if !strings.Contains(serr.Error(), "schema violation") {
		t.Errorf("error message missing 'schema violation': %v", serr)
	}
}

func TestValidate_BadEnum(t *testing.T) {
	// scope.files[].operation only accepts a closed set; "demolish"
	// is not a member.
	src := fixtureJSON(t, planfixture.Valid())
	tampered := strings.Replace(string(src), `"create"`, `"demolish"`, 1)
	var serr *SchemaError
	err := Validate([]byte(tampered))
	if !errors.As(err, &serr) {
		t.Fatalf("err = %v, want *SchemaError", err)
	}
	if serr.Path == "" {
		t.Errorf("SchemaError.Path is empty: %+v", serr)
	}
}

func TestValidate_BadVersion(t *testing.T) {
	// plan_version is constrained to "standard_v1".
	src := fixtureJSON(t, planfixture.Valid())
	tampered := strings.Replace(string(src), `"standard_v1"`, `"standard_v2"`, 1)
	if err := Validate([]byte(tampered)); err == nil {
		t.Fatal("expected version error")
	}
}

func TestSchemaErrorFormatting(t *testing.T) {
	// Branch coverage for the no-path error fallback.
	se := &SchemaError{Path: "", Message: "bad"}
	if !strings.Contains(se.Error(), "bad") {
		t.Errorf("SchemaError.Error missing message: %s", se.Error())
	}
	se2 := &SchemaError{Path: "/scope/files/0/operation", Message: "not in enum"}
	if !strings.Contains(se2.Error(), "/scope/files/0/operation") {
		t.Errorf("SchemaError.Error missing path: %s", se2.Error())
	}
}

func TestSchemaError_MultiViolation(t *testing.T) {
	// approach must be an array; verification must be an object.
	// Both as bare strings should produce violations at both paths.
	m := planfixture.Valid(func(m map[string]any) {
		m["approach"] = "should be an array"
		m["verification"] = "should be an object"
	})
	var serr *SchemaError
	if err := Validate(fixtureJSON(t, m)); !errors.As(err, &serr) {
		t.Fatalf("err = %v, want *SchemaError", err)
	}
	hasApproach := false
	hasVerification := false
	for _, v := range serr.Violations {
		if v.Path == "/approach" {
			hasApproach = true
		}
		if v.Path == "/verification" {
			hasVerification = true
		}
	}
	if !hasApproach {
		t.Errorf("Violations missing /approach entry; got %+v", serr.Violations)
	}
	if !hasVerification {
		t.Errorf("Violations missing /verification entry; got %+v", serr.Violations)
	}
}

func TestParseErrorUnwrap(t *testing.T) {
	cause := errors.New("underlying")
	pe := &ParseError{Msg: "wrapped", Cause: cause}
	if errors.Unwrap(pe) != cause {
		t.Errorf("Unwrap returned %v, want %v", errors.Unwrap(pe), cause)
	}
}

// TestValidate_NewArchitecturalDecision_MirrorSynced proves the runner's
// embedded schema mirror carries new_architectural_decision (E78.4 / #3748): a
// plan with the field is accepted, and one whose declaration omits rationale is
// rejected with an error naming rationale. An un-synced mirror would reject the
// whole key via the root additionalProperties:false instead, so the error would
// name the property, not rationale.
func TestValidate_NewArchitecturalDecision_MirrorSynced(t *testing.T) {
	decl := func() map[string]any {
		return map[string]any{
			"rationale":        "introduces a durable crew message bus",
			"related_adrs":     []any{"ADR-082"},
			"decision_summary": "crew messages move to a durable queue",
		}
	}
	ok := planfixture.Valid(func(m map[string]any) {
		m["new_architectural_decision"] = decl()
	})
	if err := Validate(fixtureJSON(t, ok)); err != nil {
		t.Fatalf("runner mirror should accept new_architectural_decision, got %v", err)
	}

	d := decl()
	delete(d, "rationale")
	bad := planfixture.Valid(func(m map[string]any) {
		m["new_architectural_decision"] = d
	})
	var serr *SchemaError
	if err := Validate(fixtureJSON(t, bad)); !errors.As(err, &serr) {
		t.Fatalf("err = %v, want *SchemaError for a declaration missing rationale", err)
	}
	if !strings.Contains(serr.Error(), "rationale") {
		t.Errorf("SchemaError should name rationale, got %q", serr.Error())
	}
}

// TestValidate_PartialDelivery_MirrorSynced pins that the runner's embedded
// schema mirror carries delivery / remaining_scope and the root if/then
// (E83.52 / #4085): a partial plan with remaining_scope is accepted, and a
// partial plan without it is rejected with an error naming remaining_scope, so
// the runner refuses it before the plan stage succeeds. An un-synced mirror
// would reject the delivery key itself via the root additionalProperties:false,
// so the error would name delivery, not remaining_scope.
func TestValidate_PartialDelivery_MirrorSynced(t *testing.T) {
	ok := planfixture.Valid(func(m map[string]any) {
		m["delivery"] = "partial"
		m["remaining_scope"] = "the merge-time issue comment lands in a later run"
	})
	if err := Validate(fixtureJSON(t, ok)); err != nil {
		t.Fatalf("runner mirror should accept a partial delivery with remaining_scope, got %v", err)
	}

	bad := planfixture.Valid(func(m map[string]any) {
		m["delivery"] = "partial"
	})
	var serr *SchemaError
	if err := Validate(fixtureJSON(t, bad)); !errors.As(err, &serr) {
		t.Fatalf("err = %v, want *SchemaError for a partial delivery missing remaining_scope", err)
	}
	if !strings.Contains(serr.Error(), "remaining_scope") {
		t.Errorf("SchemaError should name remaining_scope, got %q", serr.Error())
	}
}

// TestValidate_Counterfactuals_MirrorSynced proves the runner's embedded schema
// mirror carries counterfactuals (E85.3 / #4143): a plan with a find/replace
// and a patch entry is accepted, and an entry carrying an unknown member is
// rejected at /counterfactuals/0. An un-synced mirror would reject the whole key
// via the root additionalProperties:false instead, so the error would sit at
// the root, not inside the entry. The semantic rules (the 8-entry cap, the
// exactly-one-form rule) are the backend's: this module validates schema only.
func TestValidate_Counterfactuals_MirrorSynced(t *testing.T) {
	entry := func() map[string]any {
		return map[string]any{
			"control_path": "backend/internal/plan/validate.go",
			"mutation":     map[string]any{"find": "a > b", "replace": "true"},
			"tests":        map[string]any{"run": "^TestX$", "package": "./backend/internal/plan"},
			"expect":       "red",
		}
	}
	patch := entry()
	patch["mutation"] = map[string]any{"patch": "--- a/x\n+++ b/x\n"}
	ok := planfixture.Valid(func(m map[string]any) {
		m["counterfactuals"] = []any{entry(), patch}
	})
	if err := Validate(fixtureJSON(t, ok)); err != nil {
		t.Fatalf("runner mirror should accept counterfactuals, got %v", err)
	}

	extra := entry()
	extra["extra"] = "x"
	bad := planfixture.Valid(func(m map[string]any) {
		m["counterfactuals"] = []any{extra}
	})
	var serr *SchemaError
	if err := Validate(fixtureJSON(t, bad)); !errors.As(err, &serr) {
		t.Fatalf("err = %v, want *SchemaError for an entry carrying an unknown member", err)
	}
	if serr.Path != "/counterfactuals/0" {
		t.Errorf("SchemaError.Path = %q, want /counterfactuals/0 (%v)", serr.Path, serr)
	}
}

// structuredOutputSupportedKeywords is the set of JSON Schema keywords the
// derived structured-output schema may carry (operator condition C1, E85.3 /
// #4143). It is derive.go's documented preserved subset (enum, const, pattern,
// minLength/maxLength/minItems/minimum, additionalProperties, required), the
// structural keywords that subset presupposes (type, properties, items), the
// annotations (description, title, default), and the conditional keywords
// (allOf, if, then) the canonical schema already ships in every plan stage's
// --json-schema. A keyword outside it (maxItems, oneOf, anyOf, …) is
// UNVERIFIED against the claude CLI subset, and an unsupported keyword silently
// degrades structured output (#1741). Widen this list only after verifying the
// keyword against the CLI, and record it in derive.go's doc comment.
var structuredOutputSupportedKeywords = map[string]bool{
	"type": true, "properties": true, "items": true, "required": true,
	"additionalProperties": true, "enum": true, "const": true, "pattern": true,
	"minLength": true, "maxLength": true, "minItems": true, "minimum": true,
	"description": true, "title": true, "default": true,
	"allOf": true, "if": true, "then": true,
}

// unsupportedDerivedKeywords walks a schema node KEYWORD-AWARE — the keys of a
// `properties` map are property names, not keywords, and enum/const/default/
// required values are data — and returns "<pointer>: <keyword>" for every
// keyword outside structuredOutputSupportedKeywords.
func unsupportedDerivedKeywords(node any, at string) []string {
	m, ok := node.(map[string]any)
	if !ok {
		return nil
	}
	var out []string
	for k, v := range m {
		if !structuredOutputSupportedKeywords[k] {
			out = append(out, at+": "+k)
		}
		switch k {
		case "properties":
			if props, ok := v.(map[string]any); ok {
				for name, sub := range props {
					out = append(out, unsupportedDerivedKeywords(sub, at+"/properties/"+name)...)
				}
			}
		case "items", "if", "then", "else", "not", "additionalProperties":
			out = append(out, unsupportedDerivedKeywords(v, at+"/"+k)...)
		case "allOf", "anyOf", "oneOf":
			if list, ok := v.([]any); ok {
				for i, sub := range list {
					out = append(out, unsupportedDerivedKeywords(sub, fmt.Sprintf("%s/%s/%d", at, k, i))...)
				}
			}
		}
	}
	return out
}

// TestStructuredOutputSchema_OnlySupportedKeywords is operator condition C1
// (E85.3 / #4143): the derived plan schema every plan stage hands the claude
// CLI as --json-schema carries no keyword outside the verified-supported set.
// The counterfactuals array is the motivating case — its 8-entry cap is
// enforced by the backend's checkCounterfactuals, not by a schema maxItems. The
// walker is self-checked on a planted maxItems nested under a property, and the
// derived tree must reach the counterfactual mutation's members, so neither the
// walk nor the derivation can pass vacuously.
func TestStructuredOutputSchema_OnlySupportedKeywords(t *testing.T) {
	planted := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"maxItems": map[string]any{"type": "string"},
			"list":     map[string]any{"type": "array", "maxItems": 8, "items": map[string]any{"type": "string"}},
		},
	}
	if got := unsupportedDerivedKeywords(planted, ""); len(got) != 1 || got[0] != "/properties/list: maxItems" {
		t.Fatalf("walker self-check = %v, want exactly [/properties/list: maxItems] (a property NAMED maxItems is not a keyword)", got)
	}

	b, err := StructuredOutputSchema()
	if err != nil {
		t.Fatalf("StructuredOutputSchema: %v", err)
	}
	var tree map[string]any
	if err := json.Unmarshal(b, &tree); err != nil {
		t.Fatalf("derived schema is not valid JSON: %v", err)
	}
	for _, bad := range unsupportedDerivedKeywords(tree, "") {
		t.Errorf("derived plan schema carries an unverified keyword at %s — it reaches every plan stage's --json-schema; enforce the rule semantically instead (#1741)", bad)
	}

	props, _ := tree["properties"].(map[string]any)
	cf, _ := props["counterfactuals"].(map[string]any)
	items, _ := cf["items"].(map[string]any)
	itemProps, _ := items["properties"].(map[string]any)
	mutation, _ := itemProps["mutation"].(map[string]any)
	mutProps, _ := mutation["properties"].(map[string]any)
	for _, k := range []string{"find", "replace", "patch"} {
		if _, ok := mutProps[k]; !ok {
			t.Errorf("derived schema lost counterfactuals[].mutation.%s; the planning agent could not emit it", k)
		}
	}
	if _, has := cf["maxItems"]; has {
		t.Error("derived counterfactuals carries maxItems; the cap belongs to checkCounterfactuals")
	}
}
