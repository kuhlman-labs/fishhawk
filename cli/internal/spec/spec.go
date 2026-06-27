// Package spec validates `.fishhawk/workflows.yaml` files locally
// against the version-routed workflow JSON Schemas (the same
// docs/spec/workflow-v*.schema.json files CI enforces in sync with
// the backend's copies).
//
// Version routing (ADR-046): both the workflow-v0 and workflow-v1
// schemas are embedded and compiled at init. ValidateBytes reads the
// spec's version, parses its major component, and validates against
// the schema for that major (0.x -> v0, 1.x -> v1). A
// missing/unparseable version falls through to the v0 schema so the
// existing required-version error is preserved; a well-formed but
// unsupported major (>= 2) fails closed naming the supported majors.
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
	"sort"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

//go:embed schemas/workflow-v0.schema.json schemas/workflow-v1.schema.json
var schemaFS embed.FS

// embeddedSchema names one embedded canonical schema and the spec
// version major it validates (ADR-046 version routing).
type embeddedSchema struct {
	Major int
	Path  string
}

// embeddedSchemas is the version routing table. Adding a major
// version means embedding its schema above and appending an entry
// here; the routing in ValidateBytes flows from this list.
var embeddedSchemas = []embeddedSchema{
	{Major: 0, Path: "schemas/workflow-v0.schema.json"},
	{Major: 1, Path: "schemas/workflow-v1.schema.json"},
}

// compiledSchemas maps a version major to its compiled JSON Schema.
// Compiled at package init; a malformed embedded copy panics here, at
// process start, not the first ValidateBytes call.
var compiledSchemas = mustCompileSchemas()

// supportedMajors lists the routable majors ascending, for the
// fail-closed error naming what is supported.
var supportedMajors = computeSupportedMajors()

func computeSupportedMajors() []int {
	majors := make([]int, 0, len(embeddedSchemas))
	for _, es := range embeddedSchemas {
		majors = append(majors, es.Major)
	}
	sort.Ints(majors)
	return majors
}

func mustCompileSchemas() map[int]*jsonschema.Schema {
	out := make(map[int]*jsonschema.Schema, len(embeddedSchemas))
	for _, es := range embeddedSchemas {
		out[es.Major] = mustCompileSchema(es.Path)
	}
	return out
}

func mustCompileSchema(path string) *jsonschema.Schema {
	data, err := schemaFS.ReadFile(path)
	if err != nil {
		panic(fmt.Sprintf("spec: read embedded schema %s: %v", path, err))
	}
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		panic(fmt.Sprintf("spec: parse embedded schema %s: %v", path, err))
	}
	// Resource name = the basename so the two majors register under
	// distinct names and the v6 compiler holds them independently.
	resource := path
	if idx := strings.LastIndex(resource, "/"); idx >= 0 {
		resource = resource[idx+1:]
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(resource, raw); err != nil {
		panic(fmt.Sprintf("spec: register embedded schema %s: %v", path, err))
	}
	s, err := c.Compile(resource)
	if err != nil {
		panic(fmt.Sprintf("spec: compile embedded schema %s: %v", path, err))
	}
	return s
}

// schemaForVersion routes a spec's raw decoded document to its
// compiled schema by version major. A missing / non-string /
// unparseable version falls through to the v0 schema (preserving the
// existing required-version error); a well-formed but unsupported
// major (>= 2) fails closed with a *ValidationError naming the
// supported majors.
func schemaForVersion(raw any) (*jsonschema.Schema, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return compiledSchemas[0], nil
	}
	vs, ok := m["version"].(string)
	if !ok {
		return compiledSchemas[0], nil
	}
	majorPart := vs
	if idx := strings.IndexByte(majorPart, '.'); idx >= 0 {
		majorPart = majorPart[:idx]
	}
	major, err := strconv.Atoi(majorPart)
	if err != nil {
		return compiledSchemas[0], nil
	}
	s, ok := compiledSchemas[major]
	if !ok {
		return nil, &ValidationError{Errors: []ValidationErrorEntry{{
			Path:    "/version",
			Message: fmt.Sprintf("unsupported spec version %q: major %d is not recognized (supported majors: %s)", vs, major, formatMajors(supportedMajors)),
		}}}
	}
	return s, nil
}

// formatMajors renders the supported-majors list (e.g. "0, 1").
func formatMajors(majors []int) string {
	parts := make([]string, len(majors))
	for i, m := range majors {
		parts[i] = strconv.Itoa(m)
	}
	return strings.Join(parts, ", ")
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
// document against the version-routed workflow schema (v0 or v1, by
// the spec's version major; ADR-046). Returns a *ParseError for
// empty / malformed YAML, a *ValidationError for schema failures
// (including an unsupported version major), and nil on success.
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

	// Route to the schema for the spec's version major (ADR-046).
	schema, err := schemaForVersion(raw)
	if err != nil {
		return err
	}
	if err := schema.Validate(raw); err != nil {
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
