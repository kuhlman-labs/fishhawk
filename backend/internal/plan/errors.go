package plan

import "fmt"

// ParseError is returned when the input is empty or not parseable as
// JSON. Cause is the underlying encoding/json error, exposed via
// Unwrap so callers can errors.As against it.
type ParseError struct {
	Msg   string
	Cause error
}

func (e *ParseError) Error() string {
	if e.Msg != "" {
		return "plan: parse: " + e.Msg
	}
	if e.Cause != nil {
		return "plan: parse: " + e.Cause.Error()
	}
	return "plan: parse: unknown error"
}

// Unwrap exposes the underlying encoding/json error so callers can
// errors.As against its location-aware types if needed.
func (e *ParseError) Unwrap() error { return e.Cause }

// SchemaError is returned when the JSON parses but doesn't satisfy
// the standard_v1 schema. Path is a JSON Pointer (RFC 6901)
// pointing at the offending instance location; Message is the
// schema's reported reason.
type SchemaError struct {
	Path    string
	Message string
}

func (e *SchemaError) Error() string {
	return fmt.Sprintf("plan: schema: %s: %s", e.Path, e.Message)
}
