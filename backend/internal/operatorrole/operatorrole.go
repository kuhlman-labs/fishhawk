// Package operatorrole parses and validates the operator role spec
// surface (ADR-040 D1). The base role spec is a product artifact: the
// shipped default (defaults/operator-role-default.yaml) and both
// schemas (schemas/) are embedded copies of the canonical files under
// docs/spec/, mirrored by scripts/sync-schemas and kept in lockstep by
// the schema-sync gate.
//
// Two surfaces:
//
//   - The full role spec (operator-role.schema.json) describes the
//     product-shipped role: mission, gate procedures, escalation,
//     conventions, forbidden actions. Default() returns the embedded
//     default, validated against the full schema at package init so the
//     shipped artifact can never drift from its own schema.
//   - The overlay (operator-role-overlay.schema.json) is the contract
//     for a repo's .fishhawk/operator.yaml: knob presets, local
//     conventions, and the work-management pointer ONLY. Procedure
//     fields are structurally excluded — ValidateOverlay surfaces a
//     violation as a *ThinnessError naming the offending field.
package operatorrole

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

//go:embed schemas defaults
var embedded embed.FS

const (
	roleSchemaPath    = "schemas/operator-role.schema.json"
	overlaySchemaPath = "schemas/operator-role-overlay.schema.json"
	defaultSpecPath   = "defaults/operator-role-default.yaml"
)

// Compiled once at package init; if an embedded artifact is malformed
// we want to crash loudly at process start, not on the first call.
var (
	roleSchema         = mustCompileSchema(roleSchemaPath)
	overlaySchema      = mustCompileSchema(overlaySchemaPath)
	embeddedSchemaHash = computeSchemaHash()
	defaultSpec        = mustParseDefault()
)

// RoleSpec is the typed form of an operator role spec document.
type RoleSpec struct {
	Role           string            `json:"role" yaml:"role"`
	SpecVersion    string            `json:"spec_version" yaml:"spec_version"`
	Mission        string            `json:"mission" yaml:"mission"`
	GateProcedures GateProcedures    `json:"gate_procedures" yaml:"gate_procedures"`
	Escalation     Escalation        `json:"escalation" yaml:"escalation"`
	Conventions    map[string]string `json:"conventions,omitempty" yaml:"conventions,omitempty"`
	Forbidden      []string          `json:"forbidden" yaml:"forbidden"`
}

// GateProcedures holds the playbook: ordered procedure steps per gate
// of the run lifecycle. The five fields are the closed v0 set.
type GateProcedures struct {
	PreFlight           []string `json:"pre_flight" yaml:"pre_flight"`
	PlanGate            []string `json:"plan_gate" yaml:"plan_gate"`
	ImplementReviewGate []string `json:"implement_review_gate" yaml:"implement_review_gate"`
	MergeRitual         []string `json:"merge_ritual" yaml:"merge_ritual"`
	Recovery            []string `json:"recovery" yaml:"recovery"`
}

// Escalation describes when and how the role pages the human.
type Escalation struct {
	AlwaysPage StringList `json:"always_page" yaml:"always_page"`
	PageFormat string     `json:"page_format" yaml:"page_format"`
}

// StringList accepts either a single string or a list of strings, per
// the schema's string_or_list shape (always_page is a passthrough
// reference or an explicit condition list).
type StringList []string

// UnmarshalJSON decodes a string or a list of strings into a StringList.
func (s *StringList) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*s = StringList{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*s = StringList(many)
	return nil
}

// Default returns the shipped default operator role spec, parsed from
// the embedded copy and validated against the embedded full schema at
// package init. Callers must treat the returned value as read-only:
// the slices and map are shared.
func Default() RoleSpec { return defaultSpec }

// EmbeddedSchemaHash returns the hex-encoded SHA-256 of the canonical
// JSON bytes of the embedded operator-role-v0 schema (the full role
// schema; the overlay schema derives from it). Callers use this to
// detect schema drift between components at startup, matching the
// spec and plan packages' convention.
func EmbeddedSchemaHash() string { return embeddedSchemaHash }

// procedureFields are the overlay's structurally excluded top-level
// keys: setting any of them in .fishhawk/operator.yaml violates the
// thinness rule (ADR-040 D1).
var procedureFields = map[string]bool{
	"mission":         true,
	"gate_procedures": true,
	"escalation":      true,
	"forbidden":       true,
}

// ThinnessError is returned by ValidateOverlay when an overlay sets a
// procedure field. Per-repo procedure is definitionally a product gap;
// the fix is to file an issue, not to extend the overlay.
type ThinnessError struct {
	Field string
}

func (e *ThinnessError) Error() string {
	return fmt.Sprintf(
		"operatorrole: overlay: %q is a procedure field and cannot be set in .fishhawk/operator.yaml — overlay may only set knob presets, local conventions, and the work-management pointer; procedure belongs in the product — file an issue (thinness rule, ADR-040 D1)",
		e.Field,
	)
}

// YAMLError is returned when the overlay input is not parseable as
// YAML or is empty.
type YAMLError struct {
	Msg   string
	Cause error
}

func (e *YAMLError) Error() string {
	if e.Msg != "" {
		return "operatorrole: yaml: " + e.Msg
	}
	if e.Cause != nil {
		return "operatorrole: yaml: " + e.Cause.Error()
	}
	return "operatorrole: yaml: unknown error"
}

// Unwrap exposes the underlying error so callers can errors.As against
// yaml package error types when they need line/column information.
func (e *YAMLError) Unwrap() error { return e.Cause }

// SchemaError is returned when the overlay parses but doesn't satisfy
// the overlay schema for a reason other than the thinness rule. Path
// is a JSON Pointer (RFC 6901) pointing at the offending instance
// location; Message is the schema's reported reason.
type SchemaError struct {
	Path    string
	Message string
}

func (e *SchemaError) Error() string {
	return fmt.Sprintf("operatorrole: schema: %s: %s", e.Path, e.Message)
}

// ValidateOverlay reads a .fishhawk/operator.yaml document from r and
// validates it against the embedded overlay schema. A procedure field
// in the overlay returns a *ThinnessError naming the field; other
// structural violations return a *SchemaError; unparseable input
// returns a *YAMLError. Use errors.As to distinguish them.
func ValidateOverlay(r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return &YAMLError{Msg: "empty document"}
	}

	var raw any
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(false) // permissive at YAML layer; schema is the gate
	if err := dec.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			return &YAMLError{Msg: "empty document"}
		}
		return &YAMLError{Msg: err.Error(), Cause: err}
	}

	if err := overlaySchema.Validate(raw); err != nil {
		var verr *jsonschema.ValidationError
		if errors.As(err, &verr) {
			if thin := findThinness(verr); thin != nil {
				return thin
			}
			return schemaErrorFrom(verr)
		}
		return &SchemaError{Path: "/", Message: err.Error()}
	}
	return nil
}

// findThinness walks the validation error tree looking for a failure
// rooted at one of the overlay's excluded procedure fields (those
// properties carry only an always-failing subschema, so any error
// under them is a thinness violation).
func findThinness(v *jsonschema.ValidationError) *ThinnessError {
	if len(v.InstanceLocation) > 0 && procedureFields[v.InstanceLocation[0]] {
		return &ThinnessError{Field: v.InstanceLocation[0]}
	}
	for _, c := range v.Causes {
		if t := findThinness(c); t != nil {
			return t
		}
	}
	return nil
}

func mustCompileSchema(path string) *jsonschema.Schema {
	data, err := embedded.ReadFile(path)
	if err != nil {
		panic(fmt.Sprintf("operatorrole: read embedded schema %s: %v", path, err))
	}
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		panic(fmt.Sprintf("operatorrole: parse embedded schema %s: %v", path, err))
	}
	name := strings.TrimPrefix(path, "schemas/")
	c := jsonschema.NewCompiler()
	if err := c.AddResource(name, raw); err != nil {
		panic(fmt.Sprintf("operatorrole: register embedded schema %s: %v", path, err))
	}
	s, err := c.Compile(name)
	if err != nil {
		panic(fmt.Sprintf("operatorrole: compile embedded schema %s: %v", path, err))
	}
	return s
}

func computeSchemaHash() string {
	data, err := embedded.ReadFile(roleSchemaPath)
	if err != nil {
		panic(fmt.Sprintf("operatorrole: read embedded schema for hash %s: %v", roleSchemaPath, err))
	}
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		panic(fmt.Sprintf("operatorrole: parse embedded schema for hash %s: %v", roleSchemaPath, err))
	}
	canonical, err := json.Marshal(raw)
	if err != nil {
		panic(fmt.Sprintf("operatorrole: re-marshal embedded schema for hash %s: %v", roleSchemaPath, err))
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

// mustParseDefault parses and validates the embedded default role spec
// against the embedded full schema. Any failure is a build artifact
// bug, so it panics at package init.
func mustParseDefault() RoleSpec {
	data, err := embedded.ReadFile(defaultSpecPath)
	if err != nil {
		panic(fmt.Sprintf("operatorrole: read embedded default %s: %v", defaultSpecPath, err))
	}

	var raw any
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(false)
	if err := dec.Decode(&raw); err != nil {
		panic(fmt.Sprintf("operatorrole: parse embedded default %s: %v", defaultSpecPath, err))
	}

	if err := roleSchema.Validate(raw); err != nil {
		panic(fmt.Sprintf("operatorrole: embedded default %s does not satisfy %s: %v", defaultSpecPath, roleSchemaPath, err))
	}

	// Round-trip through JSON to populate the typed struct. The schema
	// has already enforced shapes/enums, so this only fails on internal
	// bugs.
	jsonBytes, err := json.Marshal(raw)
	if err != nil {
		panic(fmt.Sprintf("operatorrole: re-marshal embedded default %s: %v", defaultSpecPath, err))
	}
	var spec RoleSpec
	dec2 := json.NewDecoder(bytes.NewReader(jsonBytes))
	dec2.DisallowUnknownFields()
	if err := dec2.Decode(&spec); err != nil {
		panic(fmt.Sprintf("operatorrole: decode embedded default %s: %v", defaultSpecPath, err))
	}
	return spec
}

// schemaErrorFrom collapses a jsonschema.ValidationError tree to the
// most actionable leaf for callers, mirroring the spec package's
// convention.
func schemaErrorFrom(verr *jsonschema.ValidationError) *SchemaError {
	leaf := deepestLeaf(verr)
	loc := leaf.InstanceLocation
	if len(loc) == 0 {
		loc = []string{""}
	}
	return &SchemaError{
		Path:    "/" + strings.Join(loc, "/"),
		Message: kindMessage(leaf),
	}
}

// kindMessage returns a human-readable description of a single
// validation failure. The library's ErrorKind.LocalizedString takes a
// non-nil *message.Printer (passing nil panics inside x/text), so
// instead we lean on the leaf's Error() output and trim its prefix so
// the caller-formatted Path isn't repeated.
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
		if len(c.InstanceLocation) >= len(v.InstanceLocation) {
			return deepestLeaf(c)
		}
	}
	return v
}
