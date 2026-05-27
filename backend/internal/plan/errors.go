package plan

import (
	"fmt"
	"strings"
)

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

// SchemaViolation is a single field-level violation within a SchemaError.
// Path is a JSON Pointer (RFC 6901) and Message is the schema's reported
// reason for that specific location.
type SchemaViolation struct {
	Path    string
	Message string
}

// SchemaError is returned when the JSON parses but doesn't satisfy
// the standard_v1 schema. Path and Message identify the primary
// (first) violation; Violations enumerates all leaf-level failures so
// callers can surface every broken field in a single round-trip.
type SchemaError struct {
	Path       string
	Message    string
	Violations []SchemaViolation
}

func (e *SchemaError) Error() string {
	// Multiple leaf violations are listed together so the plan_invalid
	// 400 body names every broken field in one response (#555). The
	// single-violation case keeps the original format for backward
	// compatibility.
	if len(e.Violations) > 1 {
		parts := make([]string, 0, len(e.Violations))
		for _, v := range e.Violations {
			parts = append(parts, fmt.Sprintf("%s: %s", v.Path, v.Message))
		}
		return fmt.Sprintf("plan: schema: %d violations: %s", len(e.Violations), strings.Join(parts, "; "))
	}
	return fmt.Sprintf("plan: schema: %s: %s", e.Path, e.Message)
}

// SemanticError is returned when the plan passes schema validation but
// violates a semantic invariant (e.g. duplicate sub-plan titles). It is
// a hard rejection — Parse returns this error and the runner treats the
// plan as invalid.
type SemanticError struct {
	Message string
}

func (e *SemanticError) Error() string {
	return "plan: semantic: " + e.Message
}
