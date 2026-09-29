package crewmessage

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Sentinel errors for the two semantic rules the schema cannot express.
// Callers (and tests) assert error IDENTITY with errors.Is rather than
// substring-matching a message.
var (
	// ErrRecipientNotAddressable is returned when recipient_role names a role
	// an implement stage executes. docs/ARCHITECTURE.md §6 invariant #8 and
	// ADR-081 rule 3: no message is ever delivered to an implement stage.
	//
	// The schema CANNOT catch this: its crew-role enum admits
	// RoleImplementer because the implementer is a legal SENDER, so a
	// message addressed to one is schema-valid by construction and this
	// rule is the only gate in its path.
	ErrRecipientNotAddressable = errors.New("crewmessage: recipient role is not addressable")

	// ErrResponseNotAnswerable is returned when response_required is true on
	// a type ADR-081 D1 does not answer synchronously. Only consult and
	// escalation are answered; a response_required notice, finding or
	// work_request is an unanswerable request, so it is refused at the
	// contract rather than silently never answered.
	ErrResponseNotAnswerable = errors.New("crewmessage: response_required is not legal on this message type")
)

// ParseError reports a document that is not well-formed JSON.
type ParseError struct {
	Msg   string
	Cause error
}

func (e *ParseError) Error() string { return "crew message parse error: " + e.Msg }
func (e *ParseError) Unwrap() error { return e.Cause }

// SchemaError reports a schema-layer violation, carrying the failing
// instance location so a caller can name the offending property.
type SchemaError struct {
	Path    string
	Message string
}

func (e *SchemaError) Error() string {
	return fmt.Sprintf("crew message schema error at %s: %s", e.Path, e.Message)
}

// Validate checks data against the embedded crew-message-v1 schema and then
// applies the two semantic rules JSON Schema cannot express (see the package
// doc). It returns *ParseError, *SchemaError, ErrRecipientNotAddressable or
// ErrResponseNotAnswerable.
//
// Both semantic rules read the DECODED value produced here, never a second
// independent decode of the raw bytes, so the schema layer and the Go layer
// agree on one interpretation of the document.
func Validate(data []byte) error {
	_, err := Parse(data)
	return err
}

// Parse validates data and returns the decoded Message. Decoding uses
// DisallowUnknownFields so the typed path can never silently accept a field
// the schema rejects — a drift between Message's json tags and the schema's
// property names surfaces here rather than as a silently dropped field.
func Parse(data []byte) (*Message, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, &ParseError{Msg: "empty document"}
	}
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, &ParseError{Msg: err.Error(), Cause: err}
	}
	if err := compiledSchema.Validate(raw); err != nil {
		var verr *jsonschema.ValidationError
		if errors.As(err, &verr) {
			return nil, schemaErrorFrom(verr)
		}
		return nil, &SchemaError{Path: "/", Message: err.Error()}
	}

	var msg Message
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&msg); err != nil {
		return nil, &ParseError{Msg: err.Error(), Cause: err}
	}

	if err := checkSemantics(&msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

// checkSemantics applies the two directional rules, each wrapping its own
// sentinel so a caller can tell them apart with errors.Is.
func checkSemantics(msg *Message) error {
	if !CanReceive(msg.RecipientRole) {
		return fmt.Errorf("%w: %q is delivered by an implement stage (ARCHITECTURE §6 invariant #8) — route it through the captain instead",
			ErrRecipientNotAddressable, msg.RecipientRole)
	}
	if msg.ResponseRequired && !answerableTypes[msg.Type] {
		return fmt.Errorf("%w: %q is not answered synchronously (ADR-081 D1 answers only %q and %q), so response_required would never be satisfied",
			ErrResponseNotAnswerable, msg.Type, TypeConsult, TypeEscalation)
	}
	return nil
}

// schemaErrorFrom renders a jsonschema validation error into a SchemaError.
// The rendered Message is the FULL causal tree, not just the root wrapper, so
// it names the offending property or keyword — a test asserting a rejection
// happened for the RIGHT reason can match on that name rather than accepting
// any non-nil error.
func schemaErrorFrom(verr *jsonschema.ValidationError) *SchemaError {
	loc := "/"
	if len(verr.InstanceLocation) > 0 {
		loc = "/" + strings.Join(verr.InstanceLocation, "/")
	}
	return &SchemaError{Path: loc, Message: verr.Error()}
}
