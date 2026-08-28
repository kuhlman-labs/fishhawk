package spec

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

// The STATIC half of the mandatory-charter rule (E54.11 / #2801).
//
// A backlog-grooming run ranks the backlog against the repo's charter — every
// ranking it proposes cites a rubric line by id — so ADR-065-as-amended rules
// out an unanchored-grooming mode. The LOAD-BEARING enforcement is run
// admission (backend/internal/server/charter_gate.go): a grooming run in a
// charter-less repo is refused before any run row exists. This file is the CLI
// twin that lets `fishhawk validate` refuse the SAME spec statically, as an
// earlier warning — not a substitute.
//
// The two Go modules share no code (a cross-module import would couple the
// CLI's release cadence to the backend's — see spec.go's package doc), so this
// is an INDEPENDENT reimplementation of the backend's pure predicate + reason
// core, held byte-identical by TestCharterMessageParityAcrossModules, which
// reads backend/internal/server/charter_gate.go over the repo root and requires
// the message templates and the three reason VALUES declared verbatim there.
// Asserting the VALUES (not the const names — the backend's are unexported
// reason*, the CLI's exported Reason*) is what makes the guard non-vacuous.
//
// SINGLE OWNERSHIP of the RULES stays intact: the conventions file is validated
// against the canonical work-management-v0 JSON Schema (mirrored from docs/spec
// by scripts/sync-schemas), so the CLI is a second CONSUMER of the schema, never
// a second AUTHOR. See ValidateConventionsDocument for the deliberate residual —
// the CLI can be LESS strict than run admission, never more.

// ArtifactGroomingReport is the produced-artifact id that marks a workflow as a
// backlog-grooming workflow for the charter rule. Structural discriminator,
// mirroring backend/internal/spec.ArtifactGroomingReport — deliberately NOT the
// workflow's name (a name-keyed gate is evaded by renaming) and NOT a `kind:`
// field.
const ArtifactGroomingReport = "grooming_report"

// The refusal reasons, mirrored byte-identically (by VALUE) from the backend's
// unexported reasonCharter*/reasonConventions* declarations. Exported here
// because the cmd/fishhawk wiring reads them back from CharterAdmissionReason.
const (
	// ReasonCharterAbsent: conventions loaded, no charter block declared.
	ReasonCharterAbsent = "charter_absent"
	// ReasonCharterPathEmpty: a charter block whose path is empty/whitespace.
	ReasonCharterPathEmpty = "charter_path_empty"
	// ReasonConventionsUnavailable: the conventions could not be read at all —
	// FAIL-CLOSED, mirroring run admission (an unreadable/invalid conventions
	// file is refused rather than assumed to declare a charter).
	ReasonConventionsUnavailable = "conventions_unavailable"
)

// MsgFmtCharterRequired is the actionable refusal template, byte-identical to
// backend/internal/server.MsgFmtCharterRequired. Two %s verbs: the workflow id,
// then the LOCATION subject — the CLI passes the spec path (it has no repo
// identity). Single-line interpreted string literal (its `charter:`/`path:`
// backticks forbid a raw string) so TestCharterMessageParityAcrossModules can
// match it verbatim.
const MsgFmtCharterRequired = "workflow %s in %s produces a grooming report, but no backlog charter is declared: a grooming run ranks the backlog against the charter's rubric, and there is no unanchored-grooming mode. Declare a `charter:` block with its `path:` key in .fishhawk/work-management.yaml pointing at the checked-in charter document (conventionally .fishhawk/charter.md), then start the run again."

// MsgCharterConventionsUnreadableSuffix is appended to MsgFmtCharterRequired only
// for ReasonConventionsUnavailable, byte-identical to
// backend/internal/server.MsgCharterConventionsUnreadableSuffix. Leading space
// intentional.
const MsgCharterConventionsUnreadableSuffix = " The work-management conventions could not be read for this repo, and an unreadable conventions file is refused rather than assumed to declare a charter."

// CharterRefusalMessage renders the actionable refusal text, exactly as the
// backend's charterRefusalMessage does. subject is the location the message
// names (the CLI passes the spec path); workflowID names the offending
// grooming workflow; reason selects the conventions-unreadable suffix.
func CharterRefusalMessage(subject, workflowID, reason string) string {
	message := fmt.Sprintf(MsgFmtCharterRequired, workflowID, subject)
	if reason == ReasonConventionsUnavailable {
		message += MsgCharterConventionsUnreadableSuffix
	}
	return message
}

// CharterAdmissionReason maps a conventions-load OUTCOME to a refusal reason, or
// "" to admit — the direct twin of the backend's charterAdmissionReason, taking
// a bool + path rather than a workmgmt.Conventions so the CLI never imports the
// backend. charterDeclared is the twin of (conv.Charter != nil); a non-nil
// loadErr is the twin of the loader's error return and wins (fail closed).
func CharterAdmissionReason(charterDeclared bool, charterPath string, loadErr error) string {
	switch {
	case loadErr != nil:
		return ReasonConventionsUnavailable
	case !charterDeclared:
		return ReasonCharterAbsent
	case strings.TrimSpace(charterPath) == "":
		return ReasonCharterPathEmpty
	}
	return ""
}

// WorkflowsRequiringCharter returns the sorted ids of every workflow in the spec
// that declares a stage producing the grooming_report artifact — the same
// structural discriminator the backend's WorkflowRequiresCharter applies per
// workflow.
//
// It runs the package's decodeAndResolve prefix FIRST, so workflow-v2
// `defaults`/`extends` reuse is RESOLVED before the walk: a stage that inherits
// its `produces` from a defaults block or an extends base still counts, matching
// the backend (whose ParseBytes resolves reuse before the typed predicate runs).
// decodeAndResolve returns the raw yaml.v3 map[string]any/[]any tree (mutated in
// place by the resolver), accepts v0/v1/v2, and for a routed major < 2 leaves
// the document semantically unchanged — so the walk below is correct for every
// major. Every shape mismatch is tolerated by SKIPPING, the same discipline
// graphshape.go uses: the schema layer (run by the caller before this) has
// already rejected genuinely malformed structure.
func WorkflowsRequiringCharter(data []byte) ([]string, error) {
	raw, _, _, err := decodeAndResolve(data)
	if err != nil {
		return nil, err
	}
	root, ok := raw.(map[string]any)
	if !ok {
		return nil, nil
	}
	workflows, ok := root["workflows"].(map[string]any)
	if !ok {
		return nil, nil
	}
	var ids []string
	for name, wfRaw := range workflows {
		wf, ok := wfRaw.(map[string]any)
		if !ok {
			continue
		}
		stages, ok := wf["stages"].([]any)
		if !ok {
			continue
		}
		if stagesProduceGroomingReport(stages) {
			ids = append(ids, name)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// stagesProduceGroomingReport reports whether any stage declares
// produces: [{artifact: grooming_report}]. Non-map/non-slice nodes are skipped.
func stagesProduceGroomingReport(stages []any) bool {
	for _, stRaw := range stages {
		st, ok := stRaw.(map[string]any)
		if !ok {
			continue
		}
		produces, ok := st["produces"].([]any)
		if !ok {
			continue
		}
		for _, pRaw := range produces {
			p, ok := pRaw.(map[string]any)
			if !ok {
				continue
			}
			if art, ok := p["artifact"].(string); ok && art == ArtifactGroomingReport {
				return true
			}
		}
	}
	return false
}

//go:embed schemas/work-management-v0.schema.json
var workMgmtSchemaFS embed.FS

// compiledWorkMgmtSchema is the canonical work-management-v0 schema, compiled at
// package init from the copy scripts/sync-schemas mirrors from docs/spec. A
// malformed embedded copy panics here, at process start.
var compiledWorkMgmtSchema = mustCompileWorkMgmtSchema()

func mustCompileWorkMgmtSchema() *jsonschema.Schema {
	const path = "schemas/work-management-v0.schema.json"
	data, err := workMgmtSchemaFS.ReadFile(path)
	if err != nil {
		panic(fmt.Sprintf("spec: read embedded work-management schema %s: %v", path, err))
	}
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		panic(fmt.Sprintf("spec: parse embedded work-management schema %s: %v", path, err))
	}
	c := jsonschema.NewCompiler()
	const resource = "work-management-v0.schema.json"
	if err := c.AddResource(resource, raw); err != nil {
		panic(fmt.Sprintf("spec: register embedded work-management schema %s: %v", path, err))
	}
	s, err := c.Compile(resource)
	if err != nil {
		panic(fmt.Sprintf("spec: compile embedded work-management schema %s: %v", path, err))
	}
	return s
}

// ValidateConventionsDocument validates a work-management conventions document
// against the CANONICAL work-management-v0 schema the CLI now mirrors — the
// front half of the fail-closed loader contract the backend's run-admission gate
// carries. It rejects, mapping each to a conventions-unreadable outcome for the
// caller:
//
//   - empty / malformed YAML (a *ParseError),
//   - a MULTI-DOCUMENT stream (a *ParseError) — mirroring workmgmt.parse, which
//     rejects it because a trailing document would escape the schema entirely,
//   - any schema violation (a *ValidationError).
//
// It is a second CONSUMER of the canonical schema, NEVER a second author: it
// deliberately does NOT reproduce the backend's cross-field SEMANTIC checks (the
// mandatory-field trio, provider-connection requirements, ADR numbering,
// transitions->states) — those live in backend Go, not the schema. So a
// conventions file that violates ONLY a semantic rule is ADMITTED here though
// run admission refuses it with conventions_unavailable. That divergence
// direction is intentional and load-bearing: the CLI can be LESS strict than run
// admission (a missed early warning — cheap), never MORE strict (a false refusal
// of a spec the product accepts — the worse failure for an advisory check).
func ValidateConventionsDocument(data []byte) error {
	if len(bytes.TrimSpace(data)) == 0 {
		return &ParseError{Msg: "empty document"}
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(false) // permissive at YAML layer; the schema is the gate
	var raw any
	if err := dec.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			return &ParseError{Msg: "empty document"}
		}
		return &ParseError{Msg: err.Error()}
	}
	// Reject a multi-document stream, mirroring workmgmt.parse: only the first
	// document is validated, so any trailing document would escape the schema.
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return &ParseError{Msg: "multiple YAML documents in input: a work-management config must be a single document"}
	}
	if err := compiledWorkMgmtSchema.Validate(raw); err != nil {
		var verr *jsonschema.ValidationError
		if errors.As(err, &verr) {
			return validationErrorFrom(verr)
		}
		return &ValidationError{Errors: []ValidationErrorEntry{{Path: "/", Message: err.Error()}}}
	}
	return nil
}
