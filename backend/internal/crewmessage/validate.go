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

	// ErrDuplicateMember is returned when an object in the document repeats a
	// member name. JSON permits it and both decoders accept it, but they
	// DISAGREE on what it means: decoding into map[string]any REPLACES the
	// earlier value, while decoding into a struct whose field is itself a
	// struct MERGES the two objects field by field. So
	//
	//	{"anchor":{"run_id":"…"},"anchor":{"issue_ref":"…"}}
	//
	// presents ONE anchor to the schema layer (the oneOf passes) and TWO to
	// the typed layer — exactly-one-anchor escapes into the decoded Message.
	// Refusing duplicates up front is what makes the package's standing claim
	// true: the schema layer and the Go layer read one interpretation of the
	// document. It is a Go-layer-only rule; check-jsonschema accepts
	// duplicates (Python's json is last-wins), so this is another place the
	// Go validator is deliberately the stricter of the two.
	ErrDuplicateMember = errors.New("crewmessage: object repeats a member name")
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
// applies the semantic rules JSON Schema cannot express (see the package
// doc). It returns *ParseError, *SchemaError, ErrDuplicateMember,
// ErrRecipientNotAddressable or ErrResponseNotAnswerable.
//
// Both directional rules read the DECODED value produced here, never a second
// independent decode of the raw bytes, so the schema layer and the Go layer
// agree on one interpretation of the document. ErrDuplicateMember is what
// makes that claim true: a repeated member name is the one construct the two
// decoders inside Parse read differently, so it is refused before either can
// form a verdict on it.
func Validate(data []byte) error {
	_, err := Parse(data)
	return err
}

// Parse validates data and returns the decoded Message. The typed decode is
// decodeStrict, which uses DisallowUnknownFields so the typed path can never
// silently accept a field the schema rejects — a drift between Message's json
// tags and the schema's property names surfaces there rather than as a
// silently dropped field.
func Parse(data []byte) (*Message, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, &ParseError{Msg: "empty document"}
	}
	// Ahead of BOTH decodes: a duplicate member name is the one construct on
	// which the generic and typed decoders disagree, so it is refused before
	// either of them can form a verdict on it.
	if err := rejectDuplicateMembers(data); err != nil {
		return nil, err
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

	msg, err := decodeStrict(data)
	if err != nil {
		return nil, err
	}

	if err := checkSemantics(msg); err != nil {
		return nil, err
	}
	return msg, nil
}

// decodeStrict is Parse's typed-decode step, and the SINGLE site of the
// unknown-field guard. It is its own function so the guard is directly
// testable: no document can both pass the schema (every object is
// additionalProperties:false) and carry a field Message does not know, so a
// test driving Parse can never reach this branch, and an inline
// DisallowUnknownFields would be a control no test could redden. Deleting the
// call below turns TestDecodeStrict_RejectsUnknownField red.
//
// The guard exists for drift the schema cannot see: a Message json tag that
// stops matching a schema property name leaves the field schema-valid but
// silently dropped on decode.
func decodeStrict(data []byte) (*Message, error) {
	var msg Message
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&msg); err != nil {
		return nil, &ParseError{Msg: err.Error(), Cause: err}
	}
	return &msg, nil
}

// rejectDuplicateMembers refuses any object in the document that repeats a
// member name, at any depth. It is a TOKEN walk rather than a decode
// precisely because every decode target this package has already collapses
// duplicates — into a replacement or into a merge — so the duplication is
// only observable before one is chosen.
//
// Only ErrDuplicateMember escapes: a malformed document is left to
// json.Unmarshal on the next line, which produces the *ParseError callers
// expect, and a document nested past maxDuplicateScanDepth is deferred to
// encoding/json's own depth limit rather than recursing without bound on
// untrusted input.
func rejectDuplicateMembers(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := scanValue(dec, 0); err != nil && errors.Is(err, ErrDuplicateMember) {
		return err
	}
	return nil
}

// maxDuplicateScanDepth bounds scanValue's recursion. The schema's deepest
// legal document nests four levels (root → evidence → element → members), so
// any document reaching this cap is rejected by the schema moments later
// regardless.
const maxDuplicateScanDepth = 64

// errScanTooDeep abandons the scan without claiming a verdict.
var errScanTooDeep = errors.New("crewmessage: document nested too deeply to scan")

// scanValue consumes exactly one JSON value, descending into objects and
// arrays.
func scanValue(dec *json.Decoder, depth int) error {
	if depth > maxDuplicateScanDepth {
		return errScanTooDeep
	}
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil // a scalar: nothing to descend into
	}
	switch d {
	case '{':
		seen := make(map[string]bool)
		for dec.More() {
			nameTok, err := dec.Token()
			if err != nil {
				return err
			}
			name, ok := nameTok.(string)
			if !ok {
				return errors.New("crewmessage: non-string object member name")
			}
			if seen[name] {
				return fmt.Errorf("%w: %q", ErrDuplicateMember, name)
			}
			seen[name] = true
			if err := scanValue(dec, depth+1); err != nil {
				return err
			}
		}
		_, err := dec.Token() // closing '}'
		return err
	case '[':
		for dec.More() {
			if err := scanValue(dec, depth+1); err != nil {
				return err
			}
		}
		_, err := dec.Token() // closing ']'
		return err
	}
	return nil
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
