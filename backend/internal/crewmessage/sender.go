package crewmessage

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

var (
	// ErrSenderRoleNotSettable is returned by WithSenderRole when the caller's
	// document names a sender_role that is not the server-derived one. ADR-081
	// rule 3 / #3737: the sender role is derived from the caller's identity
	// (the token's executing stage, or `captain` for an operator), never
	// asserted by the caller. A document naming the SAME role is accepted — the
	// claim is true — so the rule is "present and different is refused", and
	// presence is decided on the raw member, so an explicit JSON null, an empty
	// string or a non-string value is present-and-different, not absent.
	ErrSenderRoleNotSettable = errors.New("crewmessage: sender_role is derived from the caller's identity and is not settable")

	// ErrUnknownRole is returned by WithSenderRole when the role it was asked
	// to inject is outside AllRoles. The role is server-derived, so this is a
	// caller programming error, never a client input error.
	ErrUnknownRole = errors.New("crewmessage: not a crew role")
)

// senderRoleMember is the crew-message-v1 member WithSenderRole owns.
const senderRoleMember = "sender_role"

// WithSenderRole admission-gates the server-derived sender role on a caller's
// raw crew-message-v1 document and returns the bytes Mailbox.Send should
// receive. It is an ADMISSION GATE, never a replacement for validation:
// Mailbox.Send's Parse re-runs every schema and Go rule over the returned
// bytes, so a document this helper passes can still be refused there.
//
// Order is load-bearing:
//
//  1. rejectDuplicateMembers runs over the CALLER'S RAW BYTES first. The
//     injection path below re-marshals through a map, and a map is last-wins,
//     so a document repeating a member name (e.g. two anchors of different
//     kinds) would be COLLAPSED to one interpretation by the re-marshal and
//     arrive at Parse with its duplicate laundered away — the exact escape
//     ErrDuplicateMember exists to refuse (README, "Two layers"). The walk
//     must see the bytes before anything can choose an interpretation.
//  2. role must be a member of AllRoles (ErrUnknownRole).
//  3. the document must be a JSON object (*ParseError otherwise, including a
//     top-level `null`, which decodes into a nil map).
//  4. sender_role PRESENT and different from role → ErrSenderRoleNotSettable;
//     present and equal → the raw bytes are returned UNCHANGED (no re-marshal,
//     so the byte stream Parse validates is the caller's own).
//  5. sender_role ABSENT → set it and re-marshal. Only this path rewrites the
//     document. The top-level values are carried as json.RawMessage, so every
//     nested value keeps its own bytes (compacted); only the root object's
//     member order and whitespace change.
func WithSenderRole(raw []byte, role Role) ([]byte, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, &ParseError{Msg: "empty document"}
	}
	if err := rejectDuplicateMembers(raw); err != nil {
		return nil, err
	}
	if !isKnownRole(role) {
		return nil, fmt.Errorf("%w: %q", ErrUnknownRole, role)
	}

	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return nil, &ParseError{Msg: err.Error(), Cause: err}
	}
	if members == nil {
		return nil, &ParseError{Msg: "document is not a JSON object"}
	}

	if present, ok := members[senderRoleMember]; ok {
		var named string
		if err := json.Unmarshal(present, &named); err != nil || Role(named) != role {
			// The caller's value is deliberately NOT echoed: it is untrusted,
			// unbounded input, and the error reaches an HTTP response.
			return nil, fmt.Errorf("%w: the document names a sender_role other than the caller-derived %q — omit sender_role or send %q",
				ErrSenderRoleNotSettable, role, role)
		}
		return raw, nil
	}

	encoded, err := json.Marshal(string(role))
	if err != nil {
		return nil, fmt.Errorf("crewmessage: encode sender_role: %w", err)
	}
	members[senderRoleMember] = encoded
	out, err := json.Marshal(members)
	if err != nil {
		return nil, fmt.Errorf("crewmessage: re-marshal with sender_role: %w", err)
	}
	return out, nil
}

// isKnownRole reports whether role is a member of the closed crew vocabulary.
func isKnownRole(role Role) bool {
	for _, r := range AllRoles {
		if r == role {
			return true
		}
	}
	return false
}
