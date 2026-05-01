package plan

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

//go:embed schemas/plan-standard-v1.schema.json
var schemaFS embed.FS

// compiledSchema is the JSON Schema used by Validate / Parse. Compiled
// once at package init; if the embedded schema is malformed we want
// to crash loudly at process start, not on the first call.
var compiledSchema = mustCompileSchema()

func mustCompileSchema() *jsonschema.Schema {
	const path = "schemas/plan-standard-v1.schema.json"
	data, err := schemaFS.ReadFile(path)
	if err != nil {
		panic(fmt.Sprintf("plan: read embedded schema %s: %v", path, err))
	}
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		panic(fmt.Sprintf("plan: parse embedded schema %s: %v", path, err))
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("plan-standard-v1.schema.json", raw); err != nil {
		panic(fmt.Sprintf("plan: register embedded schema %s: %v", path, err))
	}
	s, err := c.Compile("plan-standard-v1.schema.json")
	if err != nil {
		panic(fmt.Sprintf("plan: compile embedded schema %s: %v", path, err))
	}
	return s
}

// Validate validates plan bytes against the standard_v1 schema. The
// returned error is *ParseError (malformed JSON) or *SchemaError
// (schema violation). Use errors.As to distinguish.
//
// Used by the runner (E5.4) which only needs the pass/fail signal.
func Validate(data []byte) error {
	if len(bytes.TrimSpace(data)) == 0 {
		return &ParseError{Msg: "empty document"}
	}
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return &ParseError{Msg: err.Error(), Cause: err}
	}
	if err := compiledSchema.Validate(raw); err != nil {
		var verr *jsonschema.ValidationError
		if errors.As(err, &verr) {
			return schemaErrorFrom(verr)
		}
		return &SchemaError{Path: "/", Message: err.Error()}
	}
	return nil
}

// Parse validates plan bytes and returns the typed *Plan. Equivalent
// to calling Validate followed by json.Unmarshal — provided as a
// single call so the backend (E2 audit log writer, plan-review UI
// renderer) doesn't need to import encoding/json directly.
func Parse(data []byte) (*Plan, error) {
	if err := Validate(data); err != nil {
		return nil, err
	}
	var p Plan
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		// Schema accepted the bytes; this should only fail on an
		// internal type-mapping bug.
		return nil, fmt.Errorf("internal: decode to Plan: %w", err)
	}
	return &p, nil
}

// schemaErrorFrom collapses a jsonschema.ValidationError tree to the
// most actionable leaf for callers. Mirrors the helper in the spec
// package (kept independent rather than DRY-extracted to keep the
// schema packages self-contained).
func schemaErrorFrom(verr *jsonschema.ValidationError) *SchemaError {
	leaf := deepestLeaf(verr)
	loc := leaf.InstanceLocation
	if len(loc) == 0 {
		loc = []string{""}
	}
	return &SchemaError{
		Path:    "/" + joinPointer(loc),
		Message: kindMessage(leaf),
	}
}

func kindMessage(v *jsonschema.ValidationError) string {
	full := v.Error()
	if idx := strings.LastIndex(full, ": "); idx >= 0 {
		return full[idx+2:]
	}
	return full
}

func deepestLeaf(v *jsonschema.ValidationError) *jsonschema.ValidationError {
	for _, c := range v.Causes {
		if len(c.InstanceLocation) >= len(v.InstanceLocation) {
			return deepestLeaf(c)
		}
	}
	return v
}

func joinPointer(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += "/" + p
	}
	return out
}
