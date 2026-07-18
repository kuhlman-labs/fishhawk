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

//go:embed schemas/plan-standard-v1.schema.json schemas/clarification-request-v1.schema.json
var schemaFS embed.FS

// compiledSchema is the JSON Schema used by Validate / Parse. Compiled
// once at package init; if the embedded schema is malformed we want
// to crash loudly at process start, not on the first call.
var compiledSchema = mustCompileSchema()

// compiledClarificationSchema is the JSON Schema for the clarification_request
// sibling artifact, compiled once at init for the same fail-loud reason.
var compiledClarificationSchema = mustCompileNamedSchema("schemas/clarification-request-v1.schema.json", "clarification-request-v1.schema.json")

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
	return mustCompileNamedSchema("schemas/plan-standard-v1.schema.json", "plan-standard-v1.schema.json")
}

// mustCompileNamedSchema reads, registers, and compiles an embedded schema
// under the given path, registered to the compiler under resourceName.
// Panics on any failure so a malformed embedded schema crashes at process
// start rather than on first use.
func mustCompileNamedSchema(path, resourceName string) *jsonschema.Schema {
	data, err := schemaFS.ReadFile(path)
	if err != nil {
		panic(fmt.Sprintf("plan: read embedded schema %s: %v", path, err))
	}
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		panic(fmt.Sprintf("plan: parse embedded schema %s: %v", path, err))
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(resourceName, raw); err != nil {
		panic(fmt.Sprintf("plan: register embedded schema %s: %v", path, err))
	}
	s, err := c.Compile(resourceName)
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

// DetectArtifactKind inspects the top-level "kind" discriminator and
// reports which plan-stage artifact the document is. A document carrying
// kind == "clarification_request" is ArtifactKindClarificationRequest;
// anything else (including the plan artifact, which has no "kind" field)
// defaults to ArtifactKindPlan. The bytes are only peeked, not fully
// validated — callers route to ValidateArtifact / Validate next. Returns
// *ParseError for empty or non-JSON input.
func DetectArtifactKind(data []byte) (ArtifactKind, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return "", &ParseError{Msg: "empty document"}
	}
	var disc struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(data, &disc); err != nil {
		return "", &ParseError{Msg: err.Error(), Cause: err}
	}
	if disc.Kind == KindClarificationRequest {
		return ArtifactKindClarificationRequest, nil
	}
	return ArtifactKindPlan, nil
}

// ValidateArtifact validates a plan-stage artifact, discriminating on the
// top-level "kind" field BEFORE validation so the frozen plan schema is
// never consulted for a clarification_request (and vice versa). A
// clarification_request is validated by ValidateClarificationRequest
// (schema + unique-id semantics); anything else is validated as a plan.
func ValidateArtifact(data []byte) error {
	kind, err := DetectArtifactKind(data)
	if err != nil {
		return err
	}
	switch kind {
	case ArtifactKindClarificationRequest:
		return ValidateClarificationRequest(data)
	default:
		return Validate(data)
	}
}

// ValidateClarificationRequest validates bytes against the
// clarification-request-v1 schema and then enforces the semantic
// invariant the schema cannot express: question ids must be unique
// (operator answers are keyed by id on resume, so a duplicate is
// ambiguous). The returned error is *ParseError, *SchemaError, or
// *SemanticError. This is the validate path used by the runner-equivalent
// backend ingest — the unique-id check lives here, not only in
// ParseClarificationRequest, so a duplicate-id artifact is rejected even
// by callers that never decode the typed struct.
func ValidateClarificationRequest(data []byte) error {
	if len(bytes.TrimSpace(data)) == 0 {
		return &ParseError{Msg: "empty document"}
	}
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return &ParseError{Msg: err.Error(), Cause: err}
	}
	if err := compiledClarificationSchema.Validate(raw); err != nil {
		var verr *jsonschema.ValidationError
		if errors.As(err, &verr) {
			return schemaErrorFrom(verr)
		}
		return &SchemaError{Path: "/", Message: err.Error()}
	}
	// Schema accepted the shape; decode just the question ids to enforce
	// uniqueness semantically.
	var ids struct {
		Questions []struct {
			ID string `json:"id"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(data, &ids); err != nil {
		return &ParseError{Msg: err.Error(), Cause: err}
	}
	seen := make(map[string]struct{}, len(ids.Questions))
	for _, q := range ids.Questions {
		if _, dup := seen[q.ID]; dup {
			return &SemanticError{Message: fmt.Sprintf("questions: duplicate id %q", q.ID)}
		}
		seen[q.ID] = struct{}{}
	}
	return nil
}

// ParseClarificationRequest validates clarification_request bytes and
// returns the typed *ClarificationRequest. Equivalent to
// ValidateClarificationRequest followed by a strict JSON decode.
func ParseClarificationRequest(data []byte) (*ClarificationRequest, error) {
	if err := ValidateClarificationRequest(data); err != nil {
		return nil, err
	}
	var cr ClarificationRequest
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cr); err != nil {
		// Schema accepted the bytes; this should only fail on an
		// internal type-mapping bug.
		return nil, fmt.Errorf("internal: decode to ClarificationRequest: %w", err)
	}
	return &cr, nil
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
//   - every sub-plan MUST declare its own non-empty scope.files (#1669): the
//     fan-out child for a slice is scoped (scope_handoff + scope-drift) and
//     prompted to those files, so a slice that omitted scope inherited the
//     parent's FULL scope.files and made every child implement the whole plan
//     — the disjoint slice branches then conflicted wholesale at fan-in;
//   - a file path may be scoped by at most one sub-plan (#1062): the
//     orchestrator partitions per-slice scope.files for commit bounding and
//     scope-drift detection, so a path claimed by two slices would have the
//     non-owning slice's edit drift-excluded, silently shipping inert code.
func semanticCheck(p *Plan) error {
	// acceptance_criteria ids must be unique — they are the join key across
	// plan → acceptance execution → evidence → triage → feedback, so a
	// duplicate is ambiguous. This runs BEFORE the decomposition early return
	// because acceptance_criteria are valid on a non-decomposed plan too.
	seenCriteria := make(map[string]struct{}, len(p.Verification.AcceptanceCriteria))
	for _, c := range p.Verification.AcceptanceCriteria {
		if _, dup := seenCriteria[c.ID]; dup {
			return &SemanticError{
				Message: fmt.Sprintf("verification.acceptance_criteria: duplicate id %q", c.ID),
			}
		}
		seenCriteria[c.ID] = struct{}{}
	}
	// split_proposal STRUCTURAL invariants (#2055, E50.3). These run on a
	// non-decomposed plan too, so they precede the decomposition early return.
	//
	// There is deliberately NO over_cap ⇒ split_proposal coupling here. over_cap
	// is a HINT-ONLY self-declaration (see Plan.OverCap): no detection or
	// enforcement path may branch on it. An earlier revision rejected a plan
	// self-declaring over_cap:true without a split_proposal as a defensive layer,
	// but because semanticCheck has no view of the resolved cap that check was
	// count-blind — it fired for an UNDER-cap plan that merely set the hint,
	// turning the advisory hint into a server rejection across every plan.Parse
	// caller (runPlanReviews plus the fail-open scope/surface/test gate checks)
	// and breaking the under-cap-unaffected guarantee (#2055 fixup). The
	// AUTHORITATIVE over-cap enforcement is the server-side count-derived
	// overCapSplitRejection gate in handleShipPlan (len(scope.files) > resolved
	// cap, which never reads over_cap); a genuinely over-cap plan missing a split
	// is rejected there regardless of the hint. checkSplitProposal below still
	// validates the STRUCTURE of any split_proposal that IS present.
	if err := checkSplitProposal(p.SplitProposal); err != nil {
		return err
	}
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
	if err := checkSubPlanScopesDeclared(p.Decomposition); err != nil {
		return err
	}
	if err := checkCrossSliceSharedFiles(p.Decomposition); err != nil {
		return err
	}
	// Reject a malformed depends_on DAG (out-of-range/negative/self index or a
	// dependency cycle) at the plan gate (#1258). Waves is pure and lives in
	// this package, so call it directly; a non-decomposed plan never reaches
	// here (guarded above).
	if _, err := Waves(p.Decomposition); err != nil {
		return &SemanticError{
			Message: fmt.Sprintf("decomposition.sub_plans: invalid depends_on: %v", err),
		}
	}
	return nil
}

// checkSubPlanScopesDeclared rejects a decomposition in which any sub-plan
// omits its own non-empty scope.files (#1669). Every slice MUST declare the
// files it owns: the fan-out child minted for a sub-plan is scoped
// (scope_handoff + scope-drift) and prompted to those files, so a sub-plan
// that inherited the parent's full scope.files silently made every child
// implement the ENTIRE plan — the disjoint slice branches then conflicted
// wholesale at fan-in and could not consolidate. The error names each
// offending slice so the plan author can add the missing per-slice scope.
func checkSubPlanScopesDeclared(d *Decomposition) error {
	var missing []string
	for _, sp := range d.SubPlans {
		if sp.Scope == nil || len(sp.Scope.Files) == 0 {
			missing = append(missing, strconv.Quote(sp.Title))
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return &SemanticError{
		Message: fmt.Sprintf(
			"decomposition.sub_plans: sub-plan(s) %s declare no scope.files; every slice must declare the files it owns so the fan-out child is scoped to its slice, not the whole plan",
			strings.Join(missing, ", "),
		),
	}
}

// checkCrossSliceSharedFiles rejects a decomposition whose sub-plans
// collectively scope the same file path across two or more distinct slices.
// Every sub-plan is guaranteed to declare a scope by the time this runs
// (checkSubPlanScopesDeclared runs first); the nil guard below is retained
// defensively. A single slice listing the same path twice is collapsed to
// one claimant.
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

// checkSplitProposal validates a plan's optional split_proposal (#2055): phase
// titles must be unique, every phase must declare a non-empty scope.files (each
// phase ships as its own within-cap plan, so an empty phase scope is
// meaningless), and the phase depends_on edges must form a valid DAG (in-range,
// non-negative, non-self, acyclic — reusing the Kahn sort behind Waves). A nil
// SplitProposal is a no-op (the field is additive-optional). These are the
// structural invariants JSON Schema cannot express. There is deliberately no
// over_cap ⇒ split_proposal coupling anywhere in the plan package — over_cap is
// hint-only; the authoritative over-cap reject is the server's count-derived
// overCapSplitRejection gate (#2055 fixup).
func checkSplitProposal(sp *SplitProposal) error {
	if sp == nil {
		return nil
	}
	seen := make(map[string]struct{}, len(sp.Phases))
	dependsOn := make([][]int, len(sp.Phases))
	for i, ph := range sp.Phases {
		if _, dup := seen[ph.Title]; dup {
			return &SemanticError{
				Message: fmt.Sprintf("split_proposal.phases: duplicate title %q", ph.Title),
			}
		}
		seen[ph.Title] = struct{}{}
		if ph.Scope == nil || len(ph.Scope.Files) == 0 {
			return &SemanticError{
				Message: fmt.Sprintf("split_proposal.phases: phase %q declares no scope.files; every phase must declare the files it owns so it ships as its own within-cap plan", ph.Title),
			}
		}
		dependsOn[i] = ph.DependsOn
	}
	if _, err := wavesFromDependsOn(dependsOn, "phase"); err != nil {
		return &SemanticError{
			Message: fmt.Sprintf("split_proposal.phases: invalid depends_on: %v", err),
		}
	}
	return nil
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

		if len(p.Decomposition.SubPlans) >= 2 {
			allEmpty := true
			for _, sp := range p.Decomposition.SubPlans {
				if len(sp.DependsOn) > 0 {
					allEmpty = false
					break
				}
			}
			if allEmpty {
				warns = append(warns, fmt.Sprintf(
					"decomposition has %d sub-plans and none declares depends_on; if any slice forms a producer->consumer chain "+
						"(a later slice references a symbol an earlier slice introduces), all slices will run in parallel in wave 0 "+
						"and the consumer may fail typecheck against the not-yet-integrated symbol — declare depends_on edges or "+
						"confirm the slices are independent (#1679)",
					len(p.Decomposition.SubPlans),
				))
			}
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
