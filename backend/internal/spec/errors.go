package spec

import "fmt"

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
