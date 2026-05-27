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

// SchemaViolation is a single field-level violation within a SchemaError.
// Path is a JSON Pointer (RFC 6901) and Message is the schema's reported
// reason for that specific location.
type SchemaViolation struct {
	Path    string
	Message string
}

// SchemaError is returned for schema violations. Path and Message identify
// the primary (first) violation; Violations enumerates all leaf-level
// failures so callers can surface every broken field in one shot.
type SchemaError struct {
	Path       string
	Message    string
	Violations []SchemaViolation
}

func (e *SchemaError) Error() string {
	// When the validator found more than one leaf violation, list them
	// all so the failure reason surfaced to the operator names every
	// broken field in a single round-trip (#555). The single-violation
	// case keeps the original format for backward compatibility.
	if len(e.Violations) > 1 {
		parts := make([]string, 0, len(e.Violations))
		for _, v := range e.Violations {
			if v.Path == "" || v.Path == "/" {
				parts = append(parts, v.Message)
			} else {
				parts = append(parts, fmt.Sprintf("%s: %s", v.Path, v.Message))
			}
		}
		return fmt.Sprintf("plan: %d schema violations: %s", len(e.Violations), strings.Join(parts, "; "))
	}
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

// schemaErrorFrom collects all leaf-level failures from a
// jsonschema.ValidationError tree. Mirrors the helper in
// backend/internal/plan; kept independent so each side stays
// self-contained.
func schemaErrorFrom(verr *jsonschema.ValidationError) *SchemaError {
	leaves := allLeaves(verr)
	if len(leaves) == 0 {
		leaves = []*jsonschema.ValidationError{verr}
	}
	violations := make([]SchemaViolation, 0, len(leaves))
	for _, leaf := range leaves {
		loc := leaf.InstanceLocation
		if len(loc) == 0 {
			loc = []string{""}
		}
		violations = append(violations, SchemaViolation{
			Path:    "/" + joinPointer(loc),
			Message: kindMessage(leaf),
		})
	}
	return &SchemaError{
		Path:       violations[0].Path,
		Message:    violations[0].Message,
		Violations: violations,
	}
}

func kindMessage(v *jsonschema.ValidationError) string {
	full := v.Error()
	if idx := strings.LastIndex(full, ": "); idx >= 0 {
		return full[idx+2:]
	}
	return full
}

// allLeaves collects every terminal node from a ValidationError tree.
// A node is terminal when none of its children have an InstanceLocation
// at least as long as the node's own. Mirrors backend/internal/plan.
func allLeaves(v *jsonschema.ValidationError) []*jsonschema.ValidationError {
	var out []*jsonschema.ValidationError
	var walk func(node *jsonschema.ValidationError)
	walk = func(node *jsonschema.ValidationError) {
		hasDeeper := false
		for _, c := range node.Causes {
			if len(c.InstanceLocation) >= len(node.InstanceLocation) {
				hasDeeper = true
				walk(c)
			}
		}
		if !hasDeeper {
			out = append(out, node)
		}
	}
	walk(v)
	return out
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
