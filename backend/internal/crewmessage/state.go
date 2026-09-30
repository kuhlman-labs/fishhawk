package crewmessage

import "errors"

// State is a crew message's lifecycle state (ADR-081 D3). The machine is
//
//	open -> accepted | rejected | expired
//
// open is the ONLY non-terminal state and every terminal transition is
// one-way: a disposed message is never re-opened and never re-disposed. An
// invalid transition is REFUSED (ErrAlreadyDisposed, ErrInvalidDisposition),
// never coerced into the nearest legal one.
//
// The state lives on the derived crew_messages table (migration 0090), which
// is an INDEX over the audit chain: the chain's crew-message entries are the
// authority and the row is reconstructable from them.
type State string

// The closed state set.
const (
	StateOpen     State = "open"
	StateAccepted State = "accepted"
	StateRejected State = "rejected"
	StateExpired  State = "expired"
)

// Terminal reports whether s is one of the three one-way terminal states. It
// is fail-closed: open and any value outside the closed set are NOT terminal,
// so a caller can never treat an unknown state as a completed disposition.
func (s State) Terminal() bool {
	switch s {
	case StateAccepted, StateRejected, StateExpired:
		return true
	}
	return false
}

// Disposition is the closed set of answers that close an open message. Each
// maps to exactly one terminal State (StateFor).
type Disposition string

// The closed disposition set.
const (
	DispositionAccepted Disposition = "accepted"
	DispositionRejected Disposition = "rejected"
	DispositionExpired  Disposition = "expired"
)

// ValidDisposition reports whether d is in the closed disposition set. It is
// fail-closed: the empty value and anything else is invalid, and a caller must
// refuse it (ErrInvalidDisposition) BEFORE anything is appended to the chain.
func ValidDisposition(d Disposition) bool {
	switch d {
	case DispositionAccepted, DispositionRejected, DispositionExpired:
		return true
	}
	return false
}

// StateFor returns the terminal State a disposition transitions an open
// message into. An invalid disposition yields the empty State — never
// StateOpen, and never a guessed terminal state — so a caller that skipped
// ValidDisposition still cannot write a legal-looking transition.
func StateFor(d Disposition) State {
	switch d {
	case DispositionAccepted:
		return StateAccepted
	case DispositionRejected:
		return StateRejected
	case DispositionExpired:
		return StateExpired
	}
	return ""
}

// The state machine's sentinels. Callers match them with errors.Is; the store
// and the mailbox wrap them with context.
var (
	// ErrInvalidDisposition is returned for a disposition outside the closed
	// set, or a transition whose target state is not terminal.
	ErrInvalidDisposition = errors.New("crewmessage: invalid disposition")
	// ErrAlreadyDisposed is returned when a message is no longer open: the
	// pending-only transition matched no open row.
	ErrAlreadyDisposed = errors.New("crewmessage: message already disposed")
	// ErrMessageNotFound is returned when no message exists at the named sent
	// sequence.
	ErrMessageNotFound = errors.New("crewmessage: message not found")
	// ErrRoundBoundExhausted is returned when a rejection would exceed the
	// thread's reject-and-reply round bound.
	ErrRoundBoundExhausted = errors.New("crewmessage: thread round bound exhausted")
	// ErrNilEntry is returned when a projection is asked to build a row from a
	// nil chain entry.
	ErrNilEntry = errors.New("crewmessage: nil audit entry")
)
