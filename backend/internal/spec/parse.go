package spec

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
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

// embeddedSchema names one embedded canonical schema and the major
// version of the spec it validates. ADR-046 introduces version
// routing: a spec is dispatched to the schema whose Major matches the
// spec's version major component.
type embeddedSchema struct {
	Major int
	Path  string
}

// embeddedSchemas is the version routing table. Adding a major version
// (workflow-v2…) means embedding its schema above and appending an
// entry here — the routing in ParseBytes and the per-major hashes flow
// from this list.
var embeddedSchemas = []embeddedSchema{
	{Major: 0, Path: "schemas/workflow-v0.schema.json"},
	{Major: 1, Path: "schemas/workflow-v1.schema.json"},
}

// compiledSchemas maps a version major to its compiled JSON Schema.
// Compiled once at package init; if any embedded schema is malformed we
// want to crash loudly at process start, not on the first Parse call.
var compiledSchemas = mustCompileSchemas()

// embeddedSchemaHashes maps a version major to the hex-encoded SHA-256
// of that schema's canonical JSON bytes. Computed once at init so
// /healthz can serve each per-major hash cheaply.
var embeddedSchemaHashes = computeSchemaHashes()

// supportedMajors lists the routable version majors in ascending order,
// for the fail-closed error message naming what is supported.
var supportedMajors = computeSupportedMajors()

func computeSupportedMajors() []int {
	majors := make([]int, 0, len(embeddedSchemas))
	for _, es := range embeddedSchemas {
		majors = append(majors, es.Major)
	}
	sort.Ints(majors)
	return majors
}

func computeSchemaHashes() map[int]string {
	out := make(map[int]string, len(embeddedSchemas))
	for _, es := range embeddedSchemas {
		data, err := schemaFS.ReadFile(es.Path)
		if err != nil {
			panic(fmt.Sprintf("spec: read embedded schema for hash %s: %v", es.Path, err))
		}
		var raw any
		if err := json.Unmarshal(data, &raw); err != nil {
			panic(fmt.Sprintf("spec: parse embedded schema for hash %s: %v", es.Path, err))
		}
		canonical, err := json.Marshal(raw)
		if err != nil {
			panic(fmt.Sprintf("spec: re-marshal embedded schema for hash %s: %v", es.Path, err))
		}
		sum := sha256.Sum256(canonical)
		out[es.Major] = hex.EncodeToString(sum[:])
	}
	return out
}

// EmbeddedSchemaHash returns the hex-encoded SHA-256 of the canonical JSON
// bytes of the embedded workflow-v0 schema. Callers use this to detect
// schema drift between components at startup. Retained for back-compat;
// EmbeddedSchemaHashV1 advertises the v1 hash alongside it.
func EmbeddedSchemaHash() string { return embeddedSchemaHashes[0] }

// EmbeddedSchemaHashV1 returns the hex-encoded SHA-256 of the canonical
// JSON bytes of the embedded workflow-v1 schema (ADR-046). /healthz
// advertises it next to the v0 hash so a component can detect v1 drift.
func EmbeddedSchemaHashV1() string { return embeddedSchemaHashes[1] }

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
	// Resource name = the basename, so the two majors register under
	// distinct names (workflow-v0.schema.json vs workflow-v1.schema.json)
	// and the v6 compiler holds them independently in one process.
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

// schemaForVersion routes a spec's raw version string to its compiled
// schema by major component. A missing / non-string / unparseable
// version falls through to the v0 schema, which then emits the existing
// required-version / enum SchemaError — so a malformed version never
// silently passes. A well-formed but unsupported major (>= 2) fails
// closed with a *SchemaError naming the supported majors.
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
		return nil, &SchemaError{
			Path:    "/version",
			Message: fmt.Sprintf("unsupported spec version %q: major %d is not recognized (supported majors: %s)", vs, major, formatMajors(supportedMajors)),
		}
	}
	return s, nil
}

// formatMajors renders the supported-majors list as a comma-separated
// string (e.g. "0, 1") for the fail-closed error message.
func formatMajors(majors []int) string {
	parts := make([]string, len(majors))
	for i, m := range majors {
		parts[i] = strconv.Itoa(m)
	}
	return strings.Join(parts, ", ")
}

// Parse reads YAML from r, validates it against the workflow-v0 JSON
// Schema and the semantic rules, and returns the typed *Spec. The
// returned error is one of *YAMLError (parse-time), *SchemaError
// (structural), or *ValidationError (semantic). Use errors.As to
// distinguish them.
func Parse(r io.Reader) (*Spec, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	return ParseBytes(data)
}

// ParseBytes is the in-memory variant of Parse.
func ParseBytes(data []byte) (*Spec, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, &YAMLError{Msg: "empty document"}
	}

	// yaml.v3 decodes a document into a generic Go value with
	// map[string]any keys (unlike yaml.v2 which produces
	// map[any]any). That makes it directly compatible with the JSON
	// Schema validator.
	var raw any
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(false) // permissive at YAML layer; schema is the gate
	if err := dec.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, &YAMLError{Msg: "empty document"}
		}
		return nil, &YAMLError{Msg: err.Error(), Cause: err}
	}

	// Route to the schema for the spec's version major (ADR-046). A
	// missing/unparseable version falls through to v0 so the existing
	// required-version error is preserved; an unsupported major fails
	// closed naming the supported majors.
	schema, err := schemaForVersion(raw)
	if err != nil {
		return nil, err
	}
	if err := schema.Validate(raw); err != nil {
		var verr *jsonschema.ValidationError
		if errors.As(err, &verr) {
			return nil, schemaErrorFrom(verr)
		}
		return nil, &SchemaError{Path: "/", Message: err.Error()}
	}

	// Round-trip through JSON to populate the typed struct. The
	// schema has already enforced shapes/enums, so this only fails
	// on internal bugs.
	jsonBytes, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("internal: re-marshal to JSON: %w", err)
	}
	var spec Spec
	dec2 := json.NewDecoder(bytes.NewReader(jsonBytes))
	dec2.DisallowUnknownFields()
	if err := dec2.Decode(&spec); err != nil {
		return nil, fmt.Errorf("internal: decode to Spec: %w", err)
	}

	if err := Validate(&spec); err != nil {
		return nil, err
	}
	return &spec, nil
}

// schemaErrorFrom collapses a jsonschema.ValidationError tree to the
// most actionable leaf for callers. The library produces nested
// errors covering each rule; the leaf with a non-empty
// InstanceLocation usually points at the offending field.
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

// kindMessage returns a human-readable description of a single
// validation failure. The library's ErrorKind.LocalizedString takes
// a non-nil *message.Printer (passing nil panics inside x/text), so
// instead we lean on the leaf's Error() output and trim its prefix
// so the caller-formatted Path isn't repeated.
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
		// Prefer leaves that touch a concrete instance location.
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
