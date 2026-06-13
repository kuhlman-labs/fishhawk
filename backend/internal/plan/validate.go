package plan

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

//go:embed schemas/plan-standard-v1.schema.json
var schemaFS embed.FS

// compiledSchema is the JSON Schema used by Validate / Parse. Compiled
// once at package init; if the embedded schema is malformed we want
// to crash loudly at process start, not on the first call.
var compiledSchema = mustCompileSchema()

// embeddedSchemaHash is the hex-encoded SHA-256 of the canonical JSON
// bytes of the embedded plan-standard-v1 schema. Computed once at init
// so /healthz can serve it cheaply.
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
// bytes of the embedded plan-standard-v1 schema. Callers use this to detect
// schema drift between the backend and runner at startup.
func EmbeddedSchemaHash() string { return embeddedSchemaHash }

// expensiveTestRuntimeThreshold is the minimum predicted_runtime_minutes
// value that suppresses the expensive-test-strategy advisory warning.
// Claiming a full-repo race run finishes in under 20 minutes is suspicious.
const expensiveTestRuntimeThreshold = 20

// expensiveCountRe matches -count N and -count=N flag forms in a test strategy string.
var expensiveCountRe = regexp.MustCompile(`(?i)-count[= ](\d+)`)

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

// Validate validates plan bytes against the standard_v1 schema. The
// returned error is *ParseError (malformed JSON) or *SchemaError
// (schema violation). Use errors.As to distinguish.
//
// Used by the runner (E5.4) which only needs the pass/fail signal.
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

// Parse validates plan bytes and returns the typed *Plan. Equivalent
// to calling Validate followed by json.Unmarshal — provided as a
// single call so the backend (E2 audit log writer, plan-review UI
// renderer) doesn't need to import encoding/json directly.
//
// After JSON decode, semantic checks run via semanticCheck. A non-nil
// result is a hard rejection (*SemanticError).
func Parse(data []byte) (*Plan, error) {
	if err := Validate(data); err != nil {
		return nil, err
	}
	var p Plan
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		// Schema accepted the bytes; this should only fail on an
		// internal type-mapping bug.
		return nil, fmt.Errorf("internal: decode to Plan: %w", err)
	}
	if err := semanticCheck(&p); err != nil {
		return nil, err
	}
	return &p, nil
}

// semanticCheck enforces invariants that JSON Schema cannot express:
//   - sub-plan titles must be unique within a decomposition;
//   - a file path may be scoped by at most one sub-plan (#1062): the
//     orchestrator partitions per-slice scope.files for commit bounding and
//     scope-drift detection, so a path claimed by two slices would have the
//     non-owning slice's edit drift-excluded, silently shipping inert code.
func semanticCheck(p *Plan) error {
	if p.Decomposition == nil {
		return nil
	}
	seen := make(map[string]struct{}, len(p.Decomposition.SubPlans))
	for _, sp := range p.Decomposition.SubPlans {
		if _, dup := seen[sp.Title]; dup {
			return &SemanticError{
				Message: fmt.Sprintf("decomposition.sub_plans: duplicate title %q", sp.Title),
			}
		}
		seen[sp.Title] = struct{}{}
	}
	return checkCrossSliceSharedFiles(p.Decomposition)
}

// checkCrossSliceSharedFiles rejects a decomposition whose sub-plans
// collectively scope the same file path across two or more distinct slices.
// Only sub-plans that DECLARE a scope are considered — an undeclared scope
// inherits the parent's full scope.files and so cannot partition unsoundly.
// A single slice listing the same path twice is collapsed to one claimant.
func checkCrossSliceSharedFiles(d *Decomposition) error {
	claimants := make(map[string]map[string]struct{})
	for _, sp := range d.SubPlans {
		if sp.Scope == nil {
			continue
		}
		for _, f := range sp.Scope.Files {
			titles, ok := claimants[f.Path]
			if !ok {
				titles = make(map[string]struct{})
				claimants[f.Path] = titles
			}
			titles[sp.Title] = struct{}{}
		}
	}
	var shared []string
	for path, titles := range claimants {
		if len(titles) >= 2 {
			shared = append(shared, path)
		}
	}
	if len(shared) == 0 {
		return nil
	}
	sort.Strings(shared)
	parts := make([]string, 0, len(shared))
	for _, path := range shared {
		titles := make([]string, 0, len(claimants[path]))
		for t := range claimants[path] {
			titles = append(titles, t)
		}
		sort.Strings(titles)
		quoted := make([]string, len(titles))
		for i, t := range titles {
			quoted[i] = strconv.Quote(t)
		}
		parts = append(parts, fmt.Sprintf("file %s is scoped by multiple slices (%s)", path, strings.Join(quoted, ", ")))
	}
	return &SemanticError{
		Message: fmt.Sprintf(
			"decomposition.sub_plans: %s; keep all edits to one file in a single slice or re-slice along file boundaries",
			strings.Join(parts, "; "),
		),
	}
}

// Warnings returns advisory strings for a successfully-parsed Plan.
// These are soft checks — the plan is valid but the caller may want
// to surface the messages in review UI or logs. Emits a warning when:
//   - the sum of sub-plan predicted_runtime_minutes is less than the
//     parent's (agent may have compressed work — soft signal for review);
//   - test_strategy names expensive gates (-count >= 50 or full-repo
//     -race) but predicted_runtime_minutes is below expensiveTestRuntimeThreshold
//     (runtime budget is likely too optimistic for the stated gates).
func Warnings(p *Plan) []string {
	var warns []string

	if p.Decomposition != nil && len(p.Decomposition.SubPlans) > 0 {
		sum := 0
		for _, sp := range p.Decomposition.SubPlans {
			sum += sp.PredictedRuntimeMinutes
		}
		if sum < p.PredictedRuntimeMinutes {
			warns = append(warns, fmt.Sprintf(
				"decomposition sub-plan runtime sum (%d min) is less than parent predicted_runtime_minutes (%d min); agent may have compressed scope",
				sum, p.PredictedRuntimeMinutes,
			))
		}
	}

	strategy := p.Verification.TestStrategy
	hasExpensiveCount := false
	if m := expensiveCountRe.FindStringSubmatch(strategy); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil && n >= 50 {
			hasExpensiveCount = true
		}
	}
	hasExpensiveRace := strings.Contains(strategy, "-race") && strings.Contains(strategy, "./...")
	if (hasExpensiveCount || hasExpensiveRace) && p.PredictedRuntimeMinutes < expensiveTestRuntimeThreshold {
		warns = append(warns, fmt.Sprintf(
			"test_strategy contains an expensive gate but predicted_runtime_minutes (%d) is below %d; allocate explicit time for the expensive step",
			p.PredictedRuntimeMinutes, expensiveTestRuntimeThreshold,
		))
	}

	return warns
}

// schemaErrorFrom collects all leaf-level failures from a
// jsonschema.ValidationError tree and returns a SchemaError whose
// Violations field enumerates every broken field. The primary Path and
// Message come from Violations[0] for backward compat with callers that
// only read those two fields.
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
// at least as long as the node's own (i.e., no children at a deeper or
// equal schema path). This produces one entry per independently broken
// field rather than only the first path the old deepestLeaf walked.
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
