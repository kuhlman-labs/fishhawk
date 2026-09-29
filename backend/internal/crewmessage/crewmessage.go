// Package crewmessage is the Go half of the crew-message contract
// (ADR-081 / #3727, rules 1-3): the typed, addressed message one crew role
// sends another.
//
// It is deliberately NOT a plan ArtifactKind. backend/internal/plan stays
// frozen and routes nothing here; a crew message carries its own standalone
// canonical schema (docs/spec/crew-message-v1.schema.json), its own embedded
// mirror under schemas/, and its own validator. That keeps plan-standard-v1's
// additionalProperties:false contract untouched and lets the message shape
// evolve on its own major.
//
// # Two layers, one contract
//
// The SCHEMA layer carries what JSON Schema can express declaratively: a
// closed crew-role enum (rule 3 — a model id or an agent instance name is
// structurally unaddressable), a fully closed object shape at every level
// (rule 2 — no field can express scope paths, constraints, delegation or gate
// outcomes), exactly-one-anchor, a closed type enum, and a per-type payload.
//
// The GO layer carries the DIRECTIONAL rules JSON Schema cannot:
//
//   - No message may be addressed to a role an implement stage executes
//     (docs/ARCHITECTURE.md §6 invariant #8). CanReceive is the single source
//     of truth for addressability, and Validate returns
//     ErrRecipientNotAddressable.
//   - response_required is legal only on the two synchronously-answered types,
//     consult and escalation (ADR-081 D1). Validate returns
//     ErrResponseNotAnswerable otherwise.
//   - No object repeats a member name. Validate returns ErrDuplicateMember.
//     This is what keeps the generic and typed decodes inside Parse reading
//     ONE interpretation of the document; see ErrDuplicateMember.
//
// The compiler is built with AssertFormat, so anchor.run_id must be a uuid and
// deadline an RFC 3339 date-time. Draft 2020-12 makes format an annotation by
// default, which would leave this layer WEAKER than check-jsonschema on the
// canonical copy — the wrong direction for the layer documented as the
// authority.
//
// SCHEMA-ONLY VALIDATION IS NOT SUFFICIENT, and the gap is wider than one
// rule: the response_required rule and the duplicate-member refusal are also
// Go-only, and check-jsonschema ACCEPTS a duplicate member (Python's json is
// last-wins). The crew-role enum admits
// RoleImplementer on purpose: the implementer is a real crew role and a legal
// SENDER, so a bare `check-jsonschema --schemafile` run (or any non-Go
// consumer of the schema) ACCEPTS recipient_role: implementer. Invariant #8 is
// enforced here and only here. Every consumer must call Validate or Parse. A
// later E77 child that ships this schema to a non-backend process must
// re-evaluate that split rather than assume the schema carries the rule.
package crewmessage

import (
	"embed"
	"encoding/json"
	"fmt"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

//go:embed schemas/crew-message-v1.schema.json
var schemaFS embed.FS

// embeddedSchemaPath is the embed-relative path of the mirrored canonical
// schema. scripts/sync-schemas' crew-message-* case arm writes it; the
// backend/internal/server surface- and test-sweep registries flag a canonical
// edit that skips it at the plan gate.
const embeddedSchemaPath = "schemas/crew-message-v1.schema.json"

// SchemaVersion is the value the schema_version discriminator must carry.
const SchemaVersion = "crew-message-v1"

// compiledSchema is the crew-message-v1 JSON Schema, compiled once at package
// init. A malformed embedded schema panics at process start rather than
// serving a wrong verdict on the first call (the backend/internal/plan idiom).
var compiledSchema = mustCompileSchema()

func mustCompileSchema() *jsonschema.Schema {
	data, err := schemaFS.ReadFile(embeddedSchemaPath)
	if err != nil {
		panic(fmt.Sprintf("crewmessage: read embedded schema %s: %v", embeddedSchemaPath, err))
	}
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		panic(fmt.Sprintf("crewmessage: parse embedded schema %s: %v", embeddedSchemaPath, err))
	}
	c := jsonschema.NewCompiler()
	// Under Draft 2020-12 `format` is an ANNOTATION by default, so without
	// this the Go layer would ACCEPT a run_id of "not-a-uuid" and a deadline
	// of "not-a-date" that check-jsonschema — which asserts formats — rejects
	// against the canonical copy. That is a cross-layer divergence in the
	// wrong direction: the Go validator is documented as the STRICTER
	// authority, so it must not be the weaker one on the two format-bearing
	// properties (anchor.run_id, deadline). Asserting here makes the two
	// layers agree, and the reject table pins both.
	c.AssertFormat()
	// Registered under the document's own absolute $id rather than a bare
	// filename: a relative resource name is resolved against the process cwd,
	// which would leak an absolute host path into every validation error
	// message (and those messages travel to callers).
	const resourceName = "https://fishhawk.dev/spec/crew-message-v1.schema.json"
	if err := c.AddResource(resourceName, raw); err != nil {
		panic(fmt.Sprintf("crewmessage: register embedded schema %s: %v", embeddedSchemaPath, err))
	}
	s, err := c.Compile(resourceName)
	if err != nil {
		panic(fmt.Sprintf("crewmessage: compile embedded schema %s: %v", embeddedSchemaPath, err))
	}
	return s
}

// EmbeddedSchema returns the raw bytes of the embedded canonical mirror. Tests
// use it to assert byte equality against docs/spec/crew-message-v1.schema.json
// and to walk the shipped document.
func EmbeddedSchema() []byte {
	data, err := schemaFS.ReadFile(embeddedSchemaPath)
	if err != nil {
		panic(fmt.Sprintf("crewmessage: read embedded schema %s: %v", embeddedSchemaPath, err))
	}
	return data
}

// Role is a crew role. Addresses are roles, never model instances (ADR-081
// rule 3): the schema's crew-role enum is closed, so "claude-opus-5" is not a
// Role and cannot be written into sender_role or recipient_role.
type Role string

// The closed crew vocabulary. Mirrors the schema's crew-role enum member for
// member; AllRoles below is the derived set and is asserted against the
// shipped schema by the package tests.
const (
	RoleCaptain     Role = "captain"
	RolePlanner     Role = "planner"
	RoleArchitect   Role = "architect"
	RoleHistorian   Role = "historian"
	RoleSecurity    Role = "security"
	RoleReviewer    Role = "reviewer"
	RoleImplementer Role = "implementer"
)

// AllRoles is the full crew vocabulary, in schema order. It INCLUDES
// RoleImplementer: the implementer is a real crew role and a legal
// sender_role. What invariant #8 forbids is ADDRESSING one — see CanReceive.
var AllRoles = []Role{
	RoleCaptain,
	RolePlanner,
	RoleArchitect,
	RoleHistorian,
	RoleSecurity,
	RoleReviewer,
	RoleImplementer,
}

// implementStageRoles is the set of roles an implement stage executes. It is
// the complement CanReceive is defined against, kept as its own declaration so
// a future implement-executed role is added in exactly one place.
var implementStageRoles = map[Role]bool{
	RoleImplementer: true,
}

// CanReceive reports whether a message may be ADDRESSED to role. It is the
// SINGLE source of truth for addressability: the schema's crew-role enum is
// the role VOCABULARY and deliberately does not encode this rule.
//
// docs/ARCHITECTURE.md §6 invariant #8: the implement prompt never re-ingests
// untrusted text, and the human approval gate is its trust boundary. A crew
// message is untrusted text from that prompt's point of view, so no message is
// ever delivered to an implement stage (ADR-081 rule 3). Anything an
// implementer should act on goes through the captain.
//
// A role outside AllRoles is not addressable either — the schema rejects it
// upstream, and returning false keeps this predicate fail-closed for any
// caller that reaches it with an unvalidated value.
func CanReceive(role Role) bool {
	if implementStageRoles[role] {
		return false
	}
	for _, r := range AllRoles {
		if r == role {
			return true
		}
	}
	return false
}

// MessageType is one member of the closed five-type set (ADR-081 rule 1).
type MessageType string

// The closed type set.
const (
	// TypeConsult asks a question, answered synchronously in-process.
	TypeConsult MessageType = "consult"
	// TypeFinding records an observation. A finding is NOT a concern: it
	// never enters the merge gate.
	TypeFinding MessageType = "finding"
	// TypeWorkRequest asks for an item to be filed. It never dispatches a run.
	TypeWorkRequest MessageType = "work_request"
	// TypeNotice informs and expects no answer.
	TypeNotice MessageType = "notice"
	// TypeEscalation surfaces a disagreement to the captain. It produces
	// binding text only once the captain answers.
	TypeEscalation MessageType = "escalation"
)

// answerableTypes are the types ADR-081 D1 answers synchronously. Only these
// may carry response_required; see ErrResponseNotAnswerable.
var answerableTypes = map[MessageType]bool{
	TypeConsult:    true,
	TypeEscalation: true,
}

// EvidenceKind names what an EvidenceReference's Ref points at. The set is
// closed and deliberately omits any file/path member: a repo-relative path is
// the nearest thing this contract has to a scope expression, and ADR-081
// rule 2 forbids a field that can express scope. Cite a file in a payload
// prose field, which carries no authority.
type EvidenceKind string

// The closed evidence vocabulary.
const (
	EvidenceAuditEntry     EvidenceKind = "audit_entry"
	EvidenceRun            EvidenceKind = "run"
	EvidenceStage          EvidenceKind = "stage"
	EvidenceIssue          EvidenceKind = "issue"
	EvidenceDecisionRecord EvidenceKind = "decision_record"
	EvidenceURL            EvidenceKind = "url"
)

// Anchor carries exactly one of the three anchors. The schema's oneOf enforces
// the exactly-one rule; the decoded form leaves the other two empty.
type Anchor struct {
	RunID            string `json:"run_id,omitempty"`
	IssueRef         string `json:"issue_ref,omitempty"`
	DecisionRecordID string `json:"decision_record_id,omitempty"`
}

// EvidenceReference is one pointer into recorded state.
type EvidenceReference struct {
	Kind EvidenceKind `json:"kind"`
	Ref  string       `json:"ref"`
}

// Payload is the union of the five per-type payload shapes. The schema
// narrows payload to exactly one closed $def per type, so a decoded Payload
// carries only the fields of its message's type.
type Payload struct {
	// consult
	Question      string `json:"question,omitempty"`
	WhatICanInfer string `json:"what_i_can_infer,omitempty"`
	Context       string `json:"context,omitempty"`

	// finding, notice, work_request, escalation
	Summary string `json:"summary,omitempty"`
	// finding, notice
	Detail string `json:"detail,omitempty"`
	// finding
	Severity string `json:"severity,omitempty"`

	// work_request
	Title     string `json:"title,omitempty"`
	Rationale string `json:"rationale,omitempty"`

	// escalation
	RecommendedDefault string `json:"recommended_default,omitempty"`
	Tradeoffs          string `json:"tradeoffs,omitempty"`
}

// Message is the decoded crew message. Its json tags byte-match the schema's
// property names; Parse decodes with DisallowUnknownFields so the typed path
// can never silently accept a field the schema rejects.
type Message struct {
	SchemaVersion    string              `json:"schema_version"`
	Type             MessageType         `json:"type"`
	SenderRole       Role                `json:"sender_role"`
	RecipientRole    Role                `json:"recipient_role"`
	Anchor           Anchor              `json:"anchor"`
	Payload          Payload             `json:"payload"`
	Evidence         []EvidenceReference `json:"evidence,omitempty"`
	ResponseRequired bool                `json:"response_required,omitempty"`
	Deadline         string              `json:"deadline,omitempty"`
}
