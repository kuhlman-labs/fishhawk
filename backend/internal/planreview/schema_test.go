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

// schemaExcludedTags are struct json tags DELIBERATELY absent from the
// reviewer-facing verdict schema. Concern.Provenance (ADR-050 / E31.8 / #1613)
// is a server-internal trust marker stamped only at synthesis; it must never
// appear in VerdictSchema()'s closed concern object, so a review agent cannot
// populate it. TestVerdictSchema_OmitsProvenance (review_test.go) pins its
// absence positively; this set keeps the struct-vs-schema drift guard from
// flagging that intentional omission.
var schemaExcludedTags = map[string]bool{"provenance": true}

// assertTagsPresent asserts every reflected json tag of structType (except the
// deliberately schema-excluded server-internal ones) appears as a property key
// in the schema object node — the field-shape drift guard.
func assertTagsPresent(t *testing.T, structType reflect.Type, props map[string]any, where string) {
	t.Helper()
	for _, tag := range jsonTags(structType) {
		if schemaExcludedTags[tag] {
			continue
		}
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

// assertRequiredEnumeratesAll asserts an object node's `required` array lists
// EVERY key in its `properties` — the codex strict-mode invariant (#1330).
func assertRequiredEnumeratesAll(t *testing.T, node map[string]any, where string) {
	t.Helper()
	props := propsOf(t, node, where)
	req, ok := node["required"].([]any)
	if !ok {
		t.Fatalf("%s: missing or non-array \"required\"", where)
	}
	reqSet := make(map[string]bool, len(req))
	for _, r := range req {
		if s, ok := r.(string); ok {
			reqSet[s] = true
		}
	}
	for k := range props {
		if !reqSet[k] {
			t.Errorf("%s: property %q is absent from required (strict mode needs every property key)", where, k)
		}
	}
	if len(reqSet) != len(props) {
		t.Errorf("%s: required has %d keys, want %d (one per property)", where, len(reqSet), len(props))
	}
}

// assertNullable asserts a property's declared type is a union that includes
// "null" — the strict-mode encoding of an originally-optional field.
func assertNullable(t *testing.T, props map[string]any, key, where string) {
	t.Helper()
	p, ok := props[key].(map[string]any)
	if !ok {
		t.Fatalf("%s: property %q missing or not an object", where, key)
	}
	arr, ok := p["type"].([]any)
	if !ok {
		t.Errorf("%s: property %q type = %v, want a nullable type-union array", where, key, p["type"])
		return
	}
	for _, e := range arr {
		if s, ok := e.(string); ok && s == "null" {
			return
		}
	}
	t.Errorf("%s: property %q type-union %v does not include \"null\"", where, key, arr)
}

// assertNonNull asserts a property keeps a plain (non-union) type — an
// originally-required field must NOT be widened to nullable (the `severity` enum
// stays a plain enum string, never enum+null).
func assertNonNull(t *testing.T, props map[string]any, key, where string) {
	t.Helper()
	p, ok := props[key].(map[string]any)
	if !ok {
		t.Fatalf("%s: property %q missing or not an object", where, key)
	}
	if _, isArr := p["type"].([]any); isArr {
		t.Errorf("%s: property %q type = %v, want a plain non-null type (it was originally required)", where, key, p["type"])
	}
}

// TestStrictVerdictSchema_SatisfiesStrictRequired asserts the codex strict
// variant satisfies OpenAI strict structured-output rules: for the top-level
// object and both items sub-objects, `required` enumerates EVERY property key,
// every property that was OPTIONAL in the lenient VerdictSchema() is nullable
// (its type-union includes "null"), and the originally-required `severity` enum
// stays a plain non-null enum string. It also asserts deriving the strict variant
// does NOT mutate VerdictSchema()'s shared map (#1330 deep-copy condition).
func TestStrictVerdictSchema_SatisfiesStrictRequired(t *testing.T) {
	strict := StrictVerdictSchema()

	if strict["additionalProperties"] != false {
		t.Errorf("strict top-level additionalProperties = %v, want false (closed object preserved)", strict["additionalProperties"])
	}

	// Top-level: every property required; verdict stays non-null; the three
	// originally-optional fields are nullable.
	topProps := propsOf(t, strict, "strict top-level")
	assertRequiredEnumeratesAll(t, strict, "strict top-level")
	assertNonNull(t, topProps, "verdict", "strict top-level")
	for _, k := range []string{"concerns", "free_form", "concern_resolutions"} {
		assertNullable(t, topProps, k, "strict top-level")
	}

	// concerns.items: severity required & non-null enum; category/note/
	// suggested_patch nullable.
	concernItems := itemsOf(t, topProps, "concerns")
	assertRequiredEnumeratesAll(t, concernItems, "strict concerns.items")
	concernProps := propsOf(t, concernItems, "strict concerns.items")
	assertNonNull(t, concernProps, "severity", "strict concerns.items")
	if _, ok := concernProps["severity"].(map[string]any)["enum"]; !ok {
		t.Error("strict concerns.items: severity lost its enum in the strict transform")
	}
	for _, k := range []string{"category", "note", "suggested_patch"} {
		assertNullable(t, concernProps, k, "strict concerns.items")
	}

	// concern_resolutions.items: id/resolution required & non-null; note nullable.
	resolutionItems := itemsOf(t, topProps, "concern_resolutions")
	assertRequiredEnumeratesAll(t, resolutionItems, "strict concern_resolutions.items")
	resProps := propsOf(t, resolutionItems, "strict concern_resolutions.items")
	assertNonNull(t, resProps, "id", "strict concern_resolutions.items")
	assertNonNull(t, resProps, "resolution", "strict concern_resolutions.items")
	assertNullable(t, resProps, "note", "strict concern_resolutions.items")

	// Deriving the strict variant must NOT mutate the lenient source map: its
	// top-level required stays the partial [verdict].
	lenient := VerdictSchema()
	req, ok := lenient["required"].([]any)
	if !ok || len(req) != 1 || req[0] != "verdict" {
		t.Errorf("VerdictSchema() top-level required = %v, want the unmutated partial [verdict] (the strict derive mutated the source)", lenient["required"])
	}
	// The lenient severity stays a plain enum string too (no nullable leak).
	lenientConcern := itemsOf(t, propsOf(t, lenient, "lenient top-level"), "concerns")
	assertNonNull(t, propsOf(t, lenientConcern, "lenient concerns.items"), "severity", "lenient concerns.items")
}

// TestStrictVerdictSchema_DecodeNullOptionals asserts a strict-shaped body whose
// optional fields are JSON null (the form the codex strict path emits for an
// absent optional) decodes cleanly through DecodeVerdict — null unmarshals into
// the Go string/slice fields as their zero value (#1330 nullable-optional
// contract).
func TestStrictVerdictSchema_DecodeNullOptionals(t *testing.T) {
	if _, err := StrictVerdictSchemaJSON(); err != nil {
		t.Fatalf("StrictVerdictSchemaJSON: %v", err)
	}
	body := `{
		"verdict": "approve_with_concerns",
		"free_form": null,
		"concerns": [
			{"severity": "low", "category": null, "note": null, "suggested_patch": null}
		],
		"concern_resolutions": null
	}`
	got, err := DecodeVerdict([]byte(body))
	if err != nil {
		t.Fatalf("DecodeVerdict of a strict body with null optionals: %v", err)
	}
	if got.Verdict != VerdictApproveWithConcerns {
		t.Errorf("Verdict = %q, want %q", got.Verdict, VerdictApproveWithConcerns)
	}
	if len(got.Concerns) != 1 || got.Concerns[0].Severity != SeverityLow {
		t.Fatalf("Concerns = %+v, want one low-severity concern", got.Concerns)
	}
	if got.Concerns[0].Category != "" || got.Concerns[0].Note != "" || got.Concerns[0].SuggestedPatch != "" {
		t.Errorf("null concern fields did not decode to zero values: %+v", got.Concerns[0])
	}
	if got.FreeForm != "" || len(got.ConcernResolutions) != 0 {
		t.Errorf("null top-level optionals did not decode to zero values: free_form=%q concern_resolutions=%v", got.FreeForm, got.ConcernResolutions)
	}
}

// nullCount returns how many "null" string entries a type-union slice contains.
// Used to pin the dedupe invariant: the union branch must add "null" exactly
// once and never duplicate an existing one.
func nullCount(typ any) int {
	arr, ok := typ.([]any)
	if !ok {
		return 0
	}
	n := 0
	for _, e := range arr {
		if s, ok := e.(string); ok && s == "null" {
			n++
		}
	}
	return n
}

// TestMakeNullable exercises makeNullable's currently-unreached type-array
// (union) widening branch directly. makeNullable is invoked by strictTransform
// only on originally-optional VerdictSchema() properties, which all declare a
// bare string type ("string"/"array"), so the existing suite never drives the
// []any branch nor its duplicate-"null" guard. Each row builds a FRESH input
// (makeNullable mutates and returns the passed-in map, so a shared map would
// alias across cases) and asserts the returned value DeepEquals the expected
// node, plus the "null" occurrence count where a union is involved.
func TestMakeNullable(t *testing.T) {
	cases := []struct {
		name          string
		input         any
		want          any
		wantNullCount int // expected count of "null" in the result type-union
	}{
		{
			// Union branch: a type-union lacking "null" gains it exactly once.
			name:          "union without null gains null once",
			input:         map[string]any{"type": []any{"string"}},
			want:          map[string]any{"type": []any{"string", "null"}},
			wantNullCount: 1,
		},
		{
			// Dedupe guard: a union ALREADY containing "null" is unchanged — no
			// duplicate appended (the regression this test guards).
			name:          "union already with null is unchanged",
			input:         map[string]any{"type": []any{"string", "null"}},
			want:          map[string]any{"type": []any{"string", "null"}},
			wantNullCount: 1,
		},
		{
			// The dedupe scans the whole array, not just the head — "null" first
			// still counts as present, so the array is returned unchanged.
			name:          "union with null not at head is unchanged",
			input:         map[string]any{"type": []any{"null", "array"}},
			want:          map[string]any{"type": []any{"null", "array"}},
			wantNullCount: 1,
		},
		{
			// Early-return guard: a node with no `type` key is returned unchanged.
			name:          "node without type is unchanged",
			input:         map[string]any{"description": "no type here"},
			want:          map[string]any{"description": "no type here"},
			wantNullCount: 0,
		},
		{
			// Early-return guard: a non-map input is returned unchanged.
			name:          "non-map input is unchanged",
			input:         "string",
			want:          "string",
			wantNullCount: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := makeNullable(tc.input)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("makeNullable(%#v) = %#v, want %#v", tc.input, got, tc.want)
			}
			if m, ok := got.(map[string]any); ok {
				if n := nullCount(m["type"]); n != tc.wantNullCount {
					t.Errorf("makeNullable(%#v) type %#v has %d \"null\" entries, want %d", tc.input, m["type"], n, tc.wantNullCount)
				}
			}
		})
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
