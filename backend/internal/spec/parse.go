package spec

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

//go:embed schemas/workflow-v0.schema.json
var schemaFS embed.FS

// compiledSchema is the JSON Schema used by Parse. Compiled once at
// package init; if the embedded schema is malformed we want to crash
// loudly at process start, not on the first Parse call.
var compiledSchema = mustCompileSchema()

func mustCompileSchema() *jsonschema.Schema {
	const path = "schemas/workflow-v0.schema.json"
	data, err := schemaFS.ReadFile(path)
	if err != nil {
		panic(fmt.Sprintf("spec: read embedded schema %s: %v", path, err))
	}
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		panic(fmt.Sprintf("spec: parse embedded schema %s: %v", path, err))
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("workflow-v0.schema.json", raw); err != nil {
		panic(fmt.Sprintf("spec: register embedded schema %s: %v", path, err))
	}
	s, err := c.Compile("workflow-v0.schema.json")
	if err != nil {
		panic(fmt.Sprintf("spec: compile embedded schema %s: %v", path, err))
	}
	return s
}

// Parse reads YAML from r, validates it against the workflow-v0 JSON
// Schema and the semantic rules, and returns the typed *Spec. The
// returned error is one of *YAMLError (parse-time), *SchemaError
// (structural), or *ValidationError (semantic). Use errors.As to
// distinguish them.
func Parse(r io.Reader) (*Spec, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	return ParseBytes(data)
}

// ParseBytes is the in-memory variant of Parse.
func ParseBytes(data []byte) (*Spec, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, &YAMLError{Msg: "empty document"}
	}

	// yaml.v3 decodes a document into a generic Go value with
	// map[string]any keys (unlike yaml.v2 which produces
	// map[any]any). That makes it directly compatible with the JSON
	// Schema validator.
	var raw any
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(false) // permissive at YAML layer; schema is the gate
	if err := dec.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, &YAMLError{Msg: "empty document"}
		}
		return nil, &YAMLError{Msg: err.Error(), Cause: err}
	}

	if err := compiledSchema.Validate(raw); err != nil {
		var verr *jsonschema.ValidationError
		if errors.As(err, &verr) {
			return nil, schemaErrorFrom(verr)
		}
		return nil, &SchemaError{Path: "/", Message: err.Error()}
	}

	// Round-trip through JSON to populate the typed struct. The
	// schema has already enforced shapes/enums, so this only fails
	// on internal bugs.
	jsonBytes, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("internal: re-marshal to JSON: %w", err)
	}
	var spec Spec
	dec2 := json.NewDecoder(bytes.NewReader(jsonBytes))
	dec2.DisallowUnknownFields()
	if err := dec2.Decode(&spec); err != nil {
		return nil, fmt.Errorf("internal: decode to Spec: %w", err)
	}

	if err := Validate(&spec); err != nil {
		return nil, err
	}
	return &spec, nil
}

// schemaErrorFrom collapses a jsonschema.ValidationError tree to the
// most actionable leaf for callers. The library produces nested
// errors covering each rule; the leaf with a non-empty
// InstanceLocation usually points at the offending field.
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

// kindMessage returns a human-readable description of a single
// validation failure. The library's ErrorKind.LocalizedString takes
// a non-nil *message.Printer (passing nil panics inside x/text), so
// instead we lean on the leaf's Error() output and trim its prefix
// so the caller-formatted Path isn't repeated.
func kindMessage(v *jsonschema.ValidationError) string {
	full := v.Error()
	if idx := strings.LastIndex(full, ": "); idx >= 0 {
		return full[idx+2:]
	}
	return full
}

// deepestLeaf walks the validation error tree to the most specific
// failure; the v6 library wraps each rule violation, so the deepest
// node is closest to the offending field.
func deepestLeaf(v *jsonschema.ValidationError) *jsonschema.ValidationError {
	for _, c := range v.Causes {
		// Prefer leaves that touch a concrete instance location.
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
