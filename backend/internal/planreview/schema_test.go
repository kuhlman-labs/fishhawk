package planreview

import (
	"reflect"
	"strings"
	"testing"
)

// jsonTags returns the json property names declared on struct type t, skipping
// the `-` sentinel (a field deliberately excluded from JSON, e.g.
// ReviewVerdict.Usage) and any tagless field. The first comma-separated token
// is the wire name; options like `omitempty` are dropped.
func jsonTags(t reflect.Type) []string {
	var tags []string
	for i := 0; i < t.NumField(); i++ {
		name := strings.Split(t.Field(i).Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		tags = append(tags, name)
	}
	return tags
}

// propsOf extracts the "properties" object of a JSON-schema object node,
// failing the test if the shape is not as expected.
func propsOf(t *testing.T, node map[string]any, where string) map[string]any {
	t.Helper()
	props, ok := node["properties"].(map[string]any)
	if !ok {
		t.Fatalf("%s: missing or non-object \"properties\"", where)
	}
	return props
}

// itemsOf extracts the "items" object schema of a JSON-schema array node.
func itemsOf(t *testing.T, props map[string]any, key string) map[string]any {
	t.Helper()
	arr, ok := props[key].(map[string]any)
	if !ok {
		t.Fatalf("property %q is missing or not an object", key)
	}
	items, ok := arr["items"].(map[string]any)
	if !ok {
		t.Fatalf("property %q has missing or non-object \"items\"", key)
	}
	return items
}

// assertTagsPresent asserts every reflected json tag of structType appears as a
// property key in the schema object node — the field-shape drift guard.
func assertTagsPresent(t *testing.T, structType reflect.Type, props map[string]any, where string) {
	t.Helper()
	for _, tag := range jsonTags(structType) {
		if _, ok := props[tag]; !ok {
			t.Errorf("%s: struct json tag %q has no matching schema property (field-shape drift)", where, tag)
		}
	}
}

// TestVerdictSchema_MatchesStruct is the reflection-based drift test: it asserts
// (a) every non-`-` json tag of ReviewVerdict / Concern / ConcernResolution is a
// property in the corresponding schema object (field-shape drift), and (b) the
// schema's verdict + severity enum arrays equal the single-source-of-truth
// constant lists (AllVerdicts / AllConcernSeverities) AND the closed set of
// named constants — so a new Verdict/ConcernSeverity constant cannot land in the
// validated set while being silently absent from the schema (#1324 binding
// enum-drift condition).
func TestVerdictSchema_MatchesStruct(t *testing.T) {
	schema := VerdictSchema()

	// Every object is closed (additionalProperties:false) per the plan/condition.
	if schema["additionalProperties"] != false {
		t.Errorf("top-level schema additionalProperties = %v, want false (closed object)", schema["additionalProperties"])
	}

	// (a) Field-shape: ReviewVerdict at the top level.
	topProps := propsOf(t, schema, "top-level")
	assertTagsPresent(t, reflect.TypeOf(ReviewVerdict{}), topProps, "ReviewVerdict")

	// Concern lives under concerns[].items; ConcernResolution under
	// concern_resolutions[].items.
	concernItems := itemsOf(t, topProps, "concerns")
	if concernItems["additionalProperties"] != false {
		t.Errorf("concerns.items additionalProperties = %v, want false", concernItems["additionalProperties"])
	}
	assertTagsPresent(t, reflect.TypeOf(Concern{}), propsOf(t, concernItems, "concerns.items"), "Concern")

	resolutionItems := itemsOf(t, topProps, "concern_resolutions")
	if resolutionItems["additionalProperties"] != false {
		t.Errorf("concern_resolutions.items additionalProperties = %v, want false", resolutionItems["additionalProperties"])
	}
	assertTagsPresent(t, reflect.TypeOf(ConcernResolution{}), propsOf(t, resolutionItems, "concern_resolutions.items"), "ConcernResolution")

	// (b) Enum equivalence — the schema enum must equal the single source-of-
	// truth list, which in turn must equal the closed set of named constants.
	wantVerdicts := []any{string(VerdictApprove), string(VerdictApproveWithConcerns), string(VerdictReject)}
	if got := mustEnum(t, topProps, "verdict"); !reflect.DeepEqual(got, wantVerdicts) {
		t.Errorf("verdict enum = %v, want %v (the closed named-constant set)", got, wantVerdicts)
	}
	if got := verdictsAsAny(AllVerdicts); !reflect.DeepEqual(got, wantVerdicts) {
		t.Errorf("AllVerdicts = %v, want %v — single source-of-truth list drifted from the named constants", got, wantVerdicts)
	}

	wantSeverities := []any{string(SeverityHigh), string(SeverityMedium), string(SeverityLow)}
	severityProps := propsOf(t, concernItems, "concerns.items")
	if got := mustEnum(t, severityProps, "severity"); !reflect.DeepEqual(got, wantSeverities) {
		t.Errorf("severity enum = %v, want %v (the closed named-constant set)", got, wantSeverities)
	}
	if got := severitiesAsAny(AllConcernSeverities); !reflect.DeepEqual(got, wantSeverities) {
		t.Errorf("AllConcernSeverities = %v, want %v — single source-of-truth list drifted from the named constants", got, wantSeverities)
	}

	// Every named verdict/severity constant is a member of its source list
	// (the list is not missing a known constant).
	for _, v := range []Verdict{VerdictApprove, VerdictApproveWithConcerns, VerdictReject} {
		if !v.Valid() {
			t.Errorf("named constant %q is not in AllVerdicts (Valid()==false)", v)
		}
	}
	for _, s := range []ConcernSeverity{SeverityHigh, SeverityMedium, SeverityLow} {
		if !s.Valid() {
			t.Errorf("named constant %q is not in AllConcernSeverities (Valid()==false)", s)
		}
	}
}

func mustEnum(t *testing.T, props map[string]any, key string) []any {
	t.Helper()
	node, ok := props[key].(map[string]any)
	if !ok {
		t.Fatalf("property %q is missing or not an object", key)
	}
	enum, ok := node["enum"].([]any)
	if !ok {
		t.Fatalf("property %q has missing or non-array \"enum\"", key)
	}
	return enum
}

func verdictsAsAny(vs []Verdict) []any {
	out := make([]any, len(vs))
	for i, v := range vs {
		out[i] = string(v)
	}
	return out
}

func severitiesAsAny(ss []ConcernSeverity) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = string(s)
	}
	return out
}

// TestVerdictSchema_RoundTripsThroughDecode asserts VerdictSchemaJSON() marshals
// without error and that a representative verdict — approve_with_concerns with
// one concern carrying a suggested_patch, plus a concern_resolution — survives a
// round-trip through DecodeVerdict. This proves the schema TARGET (what the
// constrained backends ask the model to emit) and the decode TARGET (what the
// fallback parses) stay aligned, so the two cannot silently diverge (#1324
// never-drift, issue point 3).
func TestVerdictSchema_RoundTripsThroughDecode(t *testing.T) {
	if _, err := VerdictSchemaJSON(); err != nil {
		t.Fatalf("VerdictSchemaJSON: %v", err)
	}

	// A schema-shaped body exercising every modeled field.
	body := `{
		"verdict": "approve_with_concerns",
		"free_form": "looks fine with one nit",
		"concerns": [
			{"severity": "low", "category": "style", "note": "rename x", "suggested_patch": "--- a\n+++ b\n"}
		],
		"concern_resolutions": [
			{"id": "c-1", "resolution": "confirmed", "note": "fixed in HEAD"}
		]
	}`
	got, err := DecodeVerdict([]byte(body))
	if err != nil {
		t.Fatalf("DecodeVerdict of a schema-shaped body: %v", err)
	}
	if got.Verdict != VerdictApproveWithConcerns {
		t.Errorf("Verdict = %q, want %q", got.Verdict, VerdictApproveWithConcerns)
	}
	if !got.Verdict.Valid() {
		t.Errorf("decoded verdict %q is not Valid()", got.Verdict)
	}
	if len(got.Concerns) != 1 || got.Concerns[0].Severity != SeverityLow {
		t.Fatalf("Concerns = %+v, want one low-severity concern", got.Concerns)
	}
	if !got.Concerns[0].Severity.Valid() {
		t.Errorf("decoded severity %q is not Valid()", got.Concerns[0].Severity)
	}
	if got.Concerns[0].SuggestedPatch == "" {
		t.Error("SuggestedPatch was dropped in the round-trip")
	}
	if len(got.ConcernResolutions) != 1 || got.ConcernResolutions[0].ID != "c-1" {
		t.Errorf("ConcernResolutions = %+v, want one entry id c-1", got.ConcernResolutions)
	}
}
