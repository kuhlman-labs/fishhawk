package spec

import (
	"fmt"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// YAMLError is returned when the input is not parseable as YAML or is
// empty. The Cause is the underlying yaml package error, useful for
// extracting line/column info via errors.As.
type YAMLError struct {
	Msg   string
	Cause error
}

func (e *YAMLError) Error() string {
	if e.Msg != "" {
		return "spec: yaml: " + e.Msg
	}
	if e.Cause != nil {
		return "spec: yaml: " + e.Cause.Error()
	}
	return "spec: yaml: unknown error"
}

// Unwrap exposes the underlying error so callers can errors.As against
// yaml package error types when they need line/column information.
func (e *YAMLError) Unwrap() error { return e.Cause }

// SchemaError is returned when the YAML parses but doesn't satisfy
// the workflow-v0 JSON Schema. Path is a JSON Pointer (RFC 6901)
// pointing at the offending instance location; Message is the
// schema's reported reason.
type SchemaError struct {
	Path    string
	Message string

	// leafKind is the schema keyword kind of the deepest failing leaf
	// (e.g. *kind.Not, *kind.Pattern), recorded by schemaErrorFrom so a
	// test can pin WHICH rule rejected a document, not just where (#4311 /
	// f8ae15a1). It is nil on every hand-built SchemaError (v2removed,
	// v2reuse, version routing) and is not part of Error().
	leafKind jsonschema.ErrorKind
}

func (e *SchemaError) Error() string {
	return fmt.Sprintf("spec: schema: %s: %s", e.Path, e.Message)
}

// ValidationError is returned for semantic violations that the JSON
// Schema can't express (graph-shape checks: cross-stage references,
// role lookups, schema directives). Path is a JSON-Pointer-shaped
// path into the document.
type ValidationError struct {
	Path    string
	Message string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("spec: validation: %s: %s", e.Path, e.Message)
}
