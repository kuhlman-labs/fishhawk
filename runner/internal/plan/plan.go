// Package plan validates plan artifacts against the standard_v1
// JSON Schema. The runner uses this after an agent invocation to
// reject malformed plans before they're bundled into a trace and
// shipped to the backend.
//
// The schema's canonical home is docs/spec/plan-standard-v1.schema.json;
// the embedded copy under schemas/ keeps this package self-contained
// at runtime, with the CI's schema-sync guard ensuring the two stay
// in lockstep. Same approach used by backend/internal/plan — see
// CLAUDE.md "Canonical references".
//
// This package intentionally exposes only Validate (pass/fail). The
// typed *Plan struct + decoder live on the backend side; the runner
// only needs to enforce that the artifact is structurally sound
// before declaring the stage successful (MVP_SPEC §5.2 step 4).
package plan

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

//go:embed schemas/plan-standard-v1.schema.json
var schemaFS embed.FS

// compiledSchema is the JSON Schema used by Validate. Compiled once
// at package init; if the embedded schema is malformed we want to
// crash loudly at process start, not on the first call.
var compiledSchema = mustCompileSchema()

// embeddedSchemaHash is the hex-encoded SHA-256 of the canonical JSON
// bytes of the embedded plan-standard-v1 schema. Computed once at init.
var embeddedSchemaHash = computeSchemaHash()

func computeSchemaHash() string {
	const path = "schemas/plan-standard-v1.schema.json"
	data, err := schemaFS.ReadFile(path)
	if err != nil {
		panic(fmt.Sprintf("plan: read embedded schema for hash %s: %v", path, err))
	}
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		panic(fmt.Sprintf("plan: parse embedded schema for hash %s: %v", path, err))
	}
	canonical, err := json.Marshal(raw)
	if err != nil {
		panic(fmt.Sprintf("plan: re-marshal embedded schema for hash %s: %v", path, err))
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

// EmbeddedSchemaHash returns the hex-encoded SHA-256 of the canonical JSON
// bytes of the embedded plan-standard-v1 schema. Used by the runner's
// version subcommand so the doctor can detect schema drift.
func EmbeddedSchemaHash() string { return embeddedSchemaHash }

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

// ParseError is returned for malformed-JSON inputs.
type ParseError struct {
	Msg   string
	Cause error
}

func (e *ParseError) Error() string { return "plan: " + e.Msg }
func (e *ParseError) Unwrap() error { return e.Cause }

// SchemaError is returned for schema violations. Path is a JSON
// Pointer into the input document; Message is the leaf-level
// failure message from the validator, the most actionable line for
// surfacing to the user.
type SchemaError struct {
	Path    string
	Message string
}

func (e *SchemaError) Error() string {
	if e.Path == "" || e.Path == "/" {
		return "plan: schema violation: " + e.Message
	}
	return fmt.Sprintf("plan: schema violation at %s: %s", e.Path, e.Message)
}

// Validate validates plan bytes against the standard_v1 schema. The
// returned error is *ParseError (malformed JSON) or *SchemaError
// (schema violation). Use errors.As to distinguish.
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

// schemaErrorFrom collapses a jsonschema.ValidationError tree to
// the most actionable leaf. Mirrors the helper in
// backend/internal/plan; kept independent so each side stays
// self-contained.
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
