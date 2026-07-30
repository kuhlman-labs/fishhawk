// Package spec validates `.fishhawk/workflows.yaml` files locally
// against the version-routed workflow JSON Schemas (the same
// docs/spec/workflow-v*.schema.json files CI enforces in sync with
// the backend's copies).
//
// Version routing (ADR-046 / ADR-067): the workflow-v0, workflow-v1,
// and workflow-v2 schemas are embedded and compiled at init.
// ValidateBytes reads the spec's version, parses its major component,
// and validates against the schema for that major (0.x -> v0, 1.x ->
// v1, 2.x -> v2). A missing/unparseable version falls through to the
// v0 schema so the existing required-version error is preserved; a
// well-formed but unsupported major (>= 3) fails closed naming the
// supported majors.
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

//go:embed schemas/workflow-v0.schema.json schemas/workflow-v1.schema.json schemas/workflow-v2.schema.json
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
	{Major: 2, Path: "schemas/workflow-v2.schema.json"},
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
// compiled schema by version major, returning the schema AND the routed
// major. A missing / non-string / unparseable version falls through to
// the v0 schema — returning major 0, so the version-gated sweeps below
// never fire on that path — preserving the existing required-version
// error; a well-formed but unsupported major (>= 3) fails closed with a
// *ValidationError naming the supported majors.
func schemaForVersion(raw any) (*jsonschema.Schema, int, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return compiledSchemas[0], 0, nil
	}
	vs, ok := m["version"].(string)
	if !ok {
		return compiledSchemas[0], 0, nil
	}
	majorPart := vs
	if idx := strings.IndexByte(majorPart, '.'); idx >= 0 {
		majorPart = majorPart[:idx]
	}
	major, err := strconv.Atoi(majorPart)
	if err != nil {
		return compiledSchemas[0], 0, nil
	}
	s, ok := compiledSchemas[major]
	if !ok {
		return nil, 0, &ValidationError{Errors: []ValidationErrorEntry{{
			Path:    "/version",
			Message: fmt.Sprintf("unsupported spec version %q: major %d is not recognized (supported majors: %s)", vs, major, formatMajors(supportedMajors)),
		}}}
	}
	return s, major, nil
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

// decodeAndResolve runs the shared prefix of every validating/resolving
// read: it decodes data as YAML, routes it to its version-major schema, and
// (for a routed major >= 2 only) runs the removed-forms sweep and then the
// same-document reuse resolution, IN THAT ORDER.
//
// THE ORDER IS LOAD-BEARING and mirrors backend/internal/spec/parse.go byte
// for byte (see v2reuse.go's ORDERING block):
//
//	decode -> schemaForVersion -> validateV2RemovedForms -> resolveV2Reuse
//
// Both ValidateBytes and ResolveReuse call this so they can never disagree
// about which document the schema sees. The removed-forms sweep runs BEFORE
// resolution so its actionable message beats a resolution failure on the same
// document; resolution runs BEFORE the caller's schema.Validate so an
// inherited executor satisfies $defs/stage's required list with no schema
// relaxation. raw is mutated in place by resolveV2Reuse.
//
// Returns a *ParseError for empty / malformed YAML and a *ValidationError for
// an unsupported version major, a dotted v2 minor, a removed v2 form, or a
// reuse error (unknown/cyclic extends base, duplicate stage id). The returned
// schema is nil only alongside a non-nil error.
func decodeAndResolve(data []byte) (raw any, schema *jsonschema.Schema, major int, err error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil, 0, &ParseError{Msg: "empty document"}
	}

	// yaml.v3 decodes into Go-native maps with string keys, which
	// the JSON Schema validator can consume directly without a
	// JSON round-trip.
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(false) // permissive at YAML layer; schema is the gate
	if decErr := dec.Decode(&raw); decErr != nil {
		if errors.Is(decErr, io.EOF) {
			return nil, nil, 0, &ParseError{Msg: "empty document"}
		}
		return nil, nil, 0, &ParseError{Msg: decErr.Error()}
	}

	// Route to the schema for the spec's version major (ADR-046).
	schema, major, err = schemaForVersion(raw)
	if err != nil {
		return nil, nil, 0, err
	}

	// workflow-v2 removed two back-compat surfaces (E52.3 / #2215): the
	// bare reviewer_reject page-event token and the reviewers.agent
	// integer count. The schema rejects both, but with a generic enum /
	// additionalProperties message naming no replacement — so sweep the
	// raw document FIRST, for a routed major >= 2 only, mirroring the
	// backend's ordering. Below major 2 the sweep never runs.
	if major >= 2 {
		if err := validateV2RemovedForms(raw); err != nil {
			return nil, nil, 0, err
		}
	}

	// Resolve workflow-v2's same-document reuse primitives (E52.4 / #2216) —
	// file/workflow `defaults` and a workflow's `extends` base — BEFORE
	// schema validation, mirroring the backend's ordering byte for byte, so
	// the schema validates the RESOLVED document and an inherited executor
	// satisfies $defs/stage's required list with no schema relaxation.
	if major >= 2 {
		if err := resolveV2Reuse(raw); err != nil {
			return nil, nil, 0, err
		}
	}

	return raw, schema, major, nil
}

// ValidateBytes parses data as YAML and validates the resulting
// document against the version-routed workflow schema (v0 or v1, by
// the spec's version major; ADR-046). Returns a *ParseError for
// empty / malformed YAML, a *ValidationError for schema failures
// (including an unsupported version major), and nil on success.
func ValidateBytes(data []byte) error {
	raw, schema, _, err := decodeAndResolve(data)
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
	// Semantic sweep the schema can't express: agent_version compatibility
	// ranges (#1743) are plain strings to the schema, so a malformed range
	// is caught here (the CLI's sole semantic check; richer graph-shape
	// checks stay on the backend). At major >= 2 it now sweeps the RESOLVED
	// document, so a defaults.executor.agent_version range is validated on
	// every stage that inherits it.
	return validateAgentVersions(raw)
}

// ResolveReuse decodes data as a workflow spec, runs the same
// decode -> version-route -> removed-forms-sweep -> reuse-resolution prefix
// ValidateBytes runs, and returns the RESOLVED document re-marshalled as
// YAML. It exists so a reader that must reason about resolved stages — e.g.
// `fishhawk doctor`'s execution-path check — can decode the SAME document the
// validator sees, and therefore can never disagree with it about which stages
// carry an executor.
//
// Contract:
//
//   - It resolves workflow-v2 same-document reuse — file/workflow `defaults`
//     and a workflow's `extends` base — exactly as ValidateBytes does, because
//     it shares decodeAndResolve. A stage that inherits its executor from a
//     `defaults` block or an `extends` base comes back with that executor
//     folded in.
//   - For a routed major < 2 the document carries no reuse primitives and
//     comes back semantically unchanged.
//   - Comments, key order and anchors are NOT preserved (yaml.Marshal of a
//     decoded `any`), so this is a MACHINE-READABLE form only — callers must
//     never write it back to disk.
//   - It is NOT a validator: it deliberately does not run schema.Validate, so
//     a nil error means 'resolvable', not 'valid'. Validity stays with
//     ValidateBytes / `fishhawk validate`.
//   - Errors are the package's existing types: *ParseError for empty /
//     malformed YAML, *ValidationError for an unsupported version major, a
//     dotted v2 minor, an `extends` naming an absent workflow, an `extends`
//     cycle, a duplicate stage id, or a removed v2 form (the whole prefix is
//     shared, so every error ValidateBytes raises before schema.Validate is
//     raised here too).
func ResolveReuse(data []byte) ([]byte, error) {
	raw, _, _, err := decodeAndResolve(data)
	if err != nil {
		return nil, err
	}
	out, err := yaml.Marshal(raw)
	if err != nil {
		return nil, &ParseError{Msg: err.Error()}
	}
	return out, nil
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
