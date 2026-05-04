// Package spec validates `.fishhawk/workflows.yaml` files locally
// against the workflow-v0 JSON Schema (the same one
// docs/spec/workflow-v0.schema.json defines and CI enforces in
// sync with the backend's copy).
//
// Why this lives in cli/ and not backend/internal/spec: the Go
// modules are separate, and a cross-module import would couple
// the CLI's release cadence to the backend's. Duplicating the
// schema + compiler is ~80 lines; the schema-sync diff in
// .github/workflows/ci.yml catches drift between this copy, the
// backend's copy, and the canonical docs/spec/ copy.
//
// Scope: JSON Schema validation only. The richer semantic checks
// (cross-stage references, role resolution against the spec's
// roles map) live on the backend; the CLI returns the same
// schema errors the backend would return as the first line of
// defense, which covers ~95% of "did I write valid YAML?"
// failures.
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

const schemaPath = "schemas/workflow-v0.schema.json"

// compiledSchema is the JSON Schema compiled at package init.
// Failing here panics so a malformed embedded copy is caught at
// process start, not the first ValidateBytes call.
var compiledSchema = mustCompileSchema()

func mustCompileSchema() *jsonschema.Schema {
	data, err := schemaFS.ReadFile(schemaPath)
	if err != nil {
		panic(fmt.Sprintf("spec: read embedded schema %s: %v", schemaPath, err))
	}
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		panic(fmt.Sprintf("spec: parse embedded schema %s: %v", schemaPath, err))
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("workflow-v0.schema.json", raw); err != nil {
		panic(fmt.Sprintf("spec: register embedded schema %s: %v", schemaPath, err))
	}
	s, err := c.Compile("workflow-v0.schema.json")
	if err != nil {
		panic(fmt.Sprintf("spec: compile embedded schema %s: %v", schemaPath, err))
	}
	return s
}

// ValidationError is the shape ValidateBytes returns on schema
// failures. Path is a JSON Pointer (e.g. "/workflows/feature_change/stages/1");
// Message is a human-readable description. Multiple errors can
// surface from one Validate call (the schema validator surfaces
// every leaf failure).
type ValidationError struct {
	Errors []ValidationErrorEntry
}

// ValidationErrorEntry is one leaf failure from the validator.
type ValidationErrorEntry struct {
	Path    string
	Message string
}

func (e *ValidationError) Error() string {
	if len(e.Errors) == 0 {
		return "spec: validation failed"
	}
	var b strings.Builder
	for i, ent := range e.Errors {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(ent.Path)
		b.WriteString(": ")
		b.WriteString(ent.Message)
	}
	return b.String()
}

// ParseError signals the input wasn't valid YAML or was empty.
// Distinct from ValidationError so callers can surface "your YAML
// is broken" separately from "your spec doesn't satisfy the
// schema."
type ParseError struct {
	Msg string
}

func (e *ParseError) Error() string { return "spec: " + e.Msg }

// ValidateBytes parses data as YAML and validates the resulting
// document against the workflow-v0 schema. Returns a *ParseError
// for empty / malformed YAML, a *ValidationError for schema
// failures, and nil on success.
func ValidateBytes(data []byte) error {
	if len(bytes.TrimSpace(data)) == 0 {
		return &ParseError{Msg: "empty document"}
	}

	// yaml.v3 decodes into Go-native maps with string keys, which
	// the JSON Schema validator can consume directly without a
	// JSON round-trip.
	var raw any
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(false) // permissive at YAML layer; schema is the gate
	if err := dec.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			return &ParseError{Msg: "empty document"}
		}
		return &ParseError{Msg: err.Error()}
	}

	if err := compiledSchema.Validate(raw); err != nil {
		var verr *jsonschema.ValidationError
		if errors.As(err, &verr) {
			return validationErrorFrom(verr)
		}
		return &ValidationError{Errors: []ValidationErrorEntry{
			{Path: "/", Message: err.Error()},
		}}
	}
	return nil
}

// validationErrorFrom collapses the validator's nested error tree
// into the leaf failures most useful to a human. The library
// produces nested errors covering each rule; we keep every leaf
// with a concrete InstanceLocation so users see all the issues at
// once instead of fixing them one re-run at a time.
func validationErrorFrom(verr *jsonschema.ValidationError) *ValidationError {
	var leaves []ValidationErrorEntry
	collectLeaves(verr, &leaves)
	if len(leaves) == 0 {
		// Degenerate case: no leaves attached. Fall back to the
		// root error's text so the user still gets *something*.
		leaves = []ValidationErrorEntry{{Path: "/", Message: verr.Error()}}
	}
	return &ValidationError{Errors: leaves}
}

func collectLeaves(v *jsonschema.ValidationError, out *[]ValidationErrorEntry) {
	if len(v.Causes) == 0 {
		*out = append(*out, ValidationErrorEntry{
			Path:    "/" + joinPointer(v.InstanceLocation),
			Message: leafMessage(v),
		})
		return
	}
	for _, c := range v.Causes {
		collectLeaves(c, out)
	}
}

// leafMessage trims the validator's full error text to just the
// rule-specific message — the path is set by the caller, so we
// don't want it in the message too.
func leafMessage(v *jsonschema.ValidationError) string {
	full := v.Error()
	if idx := strings.LastIndex(full, ": "); idx >= 0 {
		return full[idx+2:]
	}
	return full
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
