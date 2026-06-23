package agenteval

import (
	"reflect"
	"strings"
	"testing"
)

// jsonTags returns the json property names declared on struct type t, skipping
// the `-` sentinel (a field deliberately excluded from JSON, e.g.
// JudgeCard.Model) and any tagless field. The first comma-separated token is the
// wire name; options like `omitempty` are dropped.
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

// dimObjOf extracts a dimension's object schema from the top-level properties.
func dimObjOf(t *testing.T, props map[string]any, key string) map[string]any {
	t.Helper()
	obj, ok := props[key].(map[string]any)
	if !ok {
		t.Fatalf("dimension property %q is missing or not an object", key)
	}
	return obj
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

// TestJudgeCardSchema_MatchesStruct is the reflection-based drift test: it
// asserts (a) the top-level object and each dimension object set
// additionalProperties:false; (b) every non-`-` json tag of judgeCardWire (the
// three dimension keys) is a top-level property and every json tag of
// DimensionScore (score, rationale) is a property of each dimension object
// (field-shape drift); (c) the score enum deep-equals the closed integer range
// generated from scoreMin..scoreMax, so changing a bound constant without
// updating the schema is caught (#1326 single-source-of-truth condition).
func TestJudgeCardSchema_MatchesStruct(t *testing.T) {
	schema := JudgeCardSchema()

	// (a) Top-level object is closed.
	if schema["additionalProperties"] != false {
		t.Errorf("top-level schema additionalProperties = %v, want false (closed object)", schema["additionalProperties"])
	}

	// (b) Field-shape: judgeCardWire dimension keys at the top level.
	topProps := propsOf(t, schema, "top-level")
	assertTagsPresent(t, reflect.TypeOf(judgeCardWire{}), topProps, "judgeCardWire")

	// Each dimension object is closed and carries DimensionScore's fields, and
	// its score enum equals the [scoreMin..scoreMax] constant range.
	wantEnum := scoreEnum()
	for _, dim := range []string{"meaningful_evidence", "honest_uncertainty", "reasoning_quality"} {
		obj := dimObjOf(t, topProps, dim)
		if obj["additionalProperties"] != false {
			t.Errorf("%s.additionalProperties = %v, want false (closed object)", dim, obj["additionalProperties"])
		}
		dimProps := propsOf(t, obj, dim)
		assertTagsPresent(t, reflect.TypeOf(DimensionScore{}), dimProps, dim+" (DimensionScore)")

		// (c) Enum equivalence — the schema score enum must equal the closed
		// integer range built from the scoreMin..scoreMax constants.
		scoreNode, ok := dimProps["score"].(map[string]any)
		if !ok {
			t.Fatalf("%s.score is missing or not an object", dim)
		}
		enum, ok := scoreNode["enum"].([]any)
		if !ok {
			t.Fatalf("%s.score has missing or non-array \"enum\"", dim)
		}
		if !reflect.DeepEqual(enum, wantEnum) {
			t.Errorf("%s.score enum = %v, want %v (the closed scoreMin..scoreMax range)", dim, enum, wantEnum)
		}
		if scoreNode["type"] != "integer" {
			t.Errorf("%s.score type = %v, want \"integer\"", dim, scoreNode["type"])
		}
	}

	// The generated enum must cover exactly the validated bound — a guard that
	// the helper itself tracks the constants (changing scoreMin/scoreMax without
	// touching this assertion is caught).
	if len(wantEnum) != scoreMax-scoreMin+1 {
		t.Errorf("scoreEnum length = %d, want %d (scoreMax-scoreMin+1)", len(wantEnum), scoreMax-scoreMin+1)
	}
	if wantEnum[0] != scoreMin || wantEnum[len(wantEnum)-1] != scoreMax {
		t.Errorf("scoreEnum endpoints = [%v..%v], want [%d..%d]", wantEnum[0], wantEnum[len(wantEnum)-1], scoreMin, scoreMax)
	}
}

// TestJudgeCardSchema_RoundTripsThroughDecode asserts a schema-shaped body (all
// three dimensions, scores in range) decodes cleanly through parseJudgeCard,
// proving the schema TARGET (what the constrained backend asks the model to
// emit) and the decode TARGET (what the fallback parses) stay aligned (#1324
// never-drift, issue point 3).
func TestJudgeCardSchema_RoundTripsThroughDecode(t *testing.T) {
	// A schema-shaped body exercising every modeled field, scores in range.
	body := `{
		"meaningful_evidence": {"score": 5, "rationale": "read the contract first"},
		"honest_uncertainty": {"score": 4, "rationale": "named the residual gap"},
		"reasoning_quality": {"score": 5, "rationale": "covered every boundary"}
	}`
	card, err := parseJudgeCard(body, "claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("parseJudgeCard of a schema-shaped body: %v", err)
	}
	want := JudgeCard{
		MeaningfulEvidence: DimensionScore{Score: 5, Rationale: "read the contract first"},
		HonestUncertainty:  DimensionScore{Score: 4, Rationale: "named the residual gap"},
		ReasoningQuality:   DimensionScore{Score: 5, Rationale: "covered every boundary"},
		Model:              "claude-sonnet-4-6",
	}
	if card != want {
		t.Errorf("card = %+v\nwant %+v", card, want)
	}
}
