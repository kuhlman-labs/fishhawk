// Package concern persists the durable review-concern lifecycle behind
// stable concern IDs (E22.X / #964). Every plan_reviewed /
// implement_reviewed verdict's concerns[] is recorded here with a
// server-minted UUID and the audit sequence of the originating review
// entry, so fix-up routing addresses concerns by stable ID instead of a
// flattened positional index (ambiguous once multiple heterogeneous
// review entries exist per stage). The audit payload remains the
// authoritative record; this store is a derived index over it —
// persistence is best-effort/warn-only around the audit appends.
//
// Mirrors scopeamendment's layout: domain types + Repository here,
// queries.sql + sqlc-generated ./db, postgres.go implementing the
// Repository against pgx.
package concern

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// State is one concern's lifecycle position. The full enum ships now —
// waived's production writer is the operator waive verb; superseded's is
// the plan-gate revise handler (fishhawk_revise_plan, #2065), which
// supersedes a plan stage's open plan-review concerns when a revise
// re-plans in place. Defining the full enum up front means each such
// writer needs no schema or enum change.
type State string

// States. Raised is the creation state. The open states (raised,
// addressed_pending, reopened) are the ones the run-status surface
// lists and fix-up routing accepts. waived, superseded, deferred, and
// addressed_by_condition are terminal: waived is the operator "this does
// not block" judgment, superseded the re-review supersession — a
// prior-revision plan-review concern superseded when the plan-gate revise
// handler (fishhawk_revise_plan, #2065) re-plans a plan stage in place —
// deferred
// the operator "file a follow-up and resolve" verb (E22.X / #1202) — a
// concern converted into a tracked work item, its state_reason naming the
// filed issue — and addressed_by_condition the condition-claim resolution
// (E48.9 / #1956): a plan-stage concern whose binding approval condition
// one implement review confirmed delivered, its state_reason naming the
// claiming approval and confirming review.
const (
	StateRaised               State = "raised"
	StateAddressedPending     State = "addressed_pending"
	StateAddressed            State = "addressed"
	StateReopened             State = "reopened"
	StateWaived               State = "waived"
	StateSuperseded           State = "superseded"
	StateDeferred             State = "deferred"
	StateAddressedByCondition State = "addressed_by_condition"
)

// StageKind values for the stage a concern originated from.
const (
	StageKindPlan      = "plan"
	StageKindImplement = "implement"
)

// Errors callers switch on.
var (
	// ErrNotFound means no concern row matches the lookup.
	ErrNotFound = errors.New("concern: not found")
)

// InvalidTransitionError reports a state-machine violation. It is a
// distinct type (not a sentinel) so callers can log the from/to pair —
// notably the deferred re-review threading, which must surface (never
// silently swallow) a confirm that arrives after a reopen.
type InvalidTransitionError struct {
	From State
	To   State
}

func (e InvalidTransitionError) Error() string {
	return fmt.Sprintf("concern: invalid transition %s -> %s", e.From, e.To)
}

// validTransitions is the concern state machine. It encodes REOPEN WINS
// OVER CONFIRM, order-independently:
//
//   - addressed -> reopened is VALID: a reopen applies even after a
//     confirm landed first;
//   - reopened -> addressed is ABSENT: a confirm arriving after a reopen
//     is rejected with InvalidTransitionError (the caller logs it), never
//     a silent downgrade — and a reopen is NEVER warn-dropped.
//
// addressed (confirm) is only reachable from addressed_pending. A
// reopened concern can be routed through another fix-up
// (reopened -> addressed_pending). waived/superseded/deferred are
// terminal and reachable from every open state (the operator waive and
// defer verbs' edges). addressed_by_condition (E48.9 / #1956) is likewise
// terminal and reachable from every open state — a plan-stage concern
// whose binding approval condition an implement review confirmed. It is
// deliberately NOT reachable from addressed: an already-confirmed concern
// needs no condition resolution.
var validTransitions = map[State]map[State]struct{}{
	StateRaised: {
		StateAddressedPending:     {},
		StateWaived:               {},
		StateSuperseded:           {},
		StateDeferred:             {},
		StateAddressedByCondition: {},
	},
	StateAddressedPending: {
		StateAddressed:            {},
		StateReopened:             {},
		StateWaived:               {},
		StateSuperseded:           {},
		StateDeferred:             {},
		StateAddressedByCondition: {},
	},
	StateAddressed: {
		StateReopened: {},
	},
	StateReopened: {
		StateAddressedPending:     {},
		StateWaived:               {},
		StateSuperseded:           {},
		StateDeferred:             {},
		StateAddressedByCondition: {},
	},
	StateWaived:               {},
	StateSuperseded:           {},
	StateDeferred:             {},
	StateAddressedByCondition: {},
}

// Transition validates a state change against the lifecycle machine.
// Returns InvalidTransitionError when the edge does not exist.
func Transition(from, to State) error {
	if _, ok := validTransitions[from][to]; !ok {
		return InvalidTransitionError{From: from, To: to}
	}
	return nil
}

// IsOpen reports whether the state counts as unresolved: listed by the
// run-status surface and addressable by fix-up routing. The terminal
// states (addressed, waived, superseded, deferred, addressed_by_condition)
// return false — a deferred concern has been converted into a follow-up
// work item, and an addressed_by_condition concern's binding approval
// condition has been confirmed delivered, so neither appears on the
// open-concerns surface.
func (s State) IsOpen() bool {
	switch s {
	case StateRaised, StateAddressedPending, StateReopened:
		return true
	}
	return false
}

// Concern is one persisted reviewer concern.
type Concern struct {
	ID                   uuid.UUID
	RunID                uuid.UUID
	StageID              uuid.UUID
	StageKind            string
	OriginReviewSequence int64
	ReviewerModel        *string
	Severity             string
	Category             string
	Note                 string
	State                State
	StateReason          string
	// SuggestedPatch is the reviewer-emitted unified diff that mechanically
	// resolves the concern (#1165), empty when the reviewer left it absent.
	// Persisted verbatim; it is the input to the near-deterministic fix-up
	// apply path.
	SuggestedPatch string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// RaisedConcern is one concern as decoded from a review verdict, before
// persistence mints its ID. Severity/category are stored verbatim
// (tolerant-decode posture — no enum check at this boundary).
type RaisedConcern struct {
	Severity string
	Category string
	Note     string
	// SuggestedPatch is the reviewer-emitted unified diff carried through
	// from the decoded verdict (#1165); empty when absent.
	SuggestedPatch string
}

// InsertRaisedParams bundles the inputs to InsertRaised: every concern
// from ONE *_reviewed audit entry, stamped with the sequence
// AppendChained returned for that entry.
type InsertRaisedParams struct {
	RunID                uuid.UUID
	StageID              uuid.UUID
	StageKind            string // StageKindPlan or StageKindImplement
	ReviewerModel        string // empty -> stored NULL
	OriginReviewSequence int64
	Concerns             []RaisedConcern
}

// Repository persists concerns.
type Repository interface {
	// InsertRaised persists one review entry's concerns in state
	// raised, minting a UUID per concern. Returns the created rows in
	// input order.
	InsertRaised(ctx context.Context, p InsertRaisedParams) ([]*Concern, error)

	// GetByIDs returns the concerns matching the given IDs, in input
	// order. ErrNotFound (wrapped with the missing ID) when any ID has
	// no row.
	GetByIDs(ctx context.Context, ids []uuid.UUID) ([]*Concern, error)

	// ListByRun returns every concern for the run, origin-sequence
	// order (oldest review first).
	ListByRun(ctx context.Context, runID uuid.UUID) ([]*Concern, error)

	// ListOpenByRun returns the run's concerns in an open state
	// (raised, addressed_pending, reopened), origin-sequence order.
	ListOpenByRun(ctx context.Context, runID uuid.UUID) ([]*Concern, error)

	// MarkAddressedPending transitions the given concerns to
	// addressed_pending (the fix-up routed them back to the agent),
	// recording reason as state_reason. Idempotent for rows already in
	// addressed_pending (skipped); any other invalid transition fails
	// with InvalidTransitionError.
	MarkAddressedPending(ctx context.Context, ids []uuid.UUID, reason string) error

	// ApplyResolution transitions one concern to the given state after
	// validating the lifecycle machine — the entry point the deferred
	// re-review delta threading will confirm/reopen through. A confirm
	// (addressed) arriving after a reopen fails with
	// InvalidTransitionError for the caller to log; it never downgrades
	// the reopened state.
	ApplyResolution(ctx context.Context, id uuid.UUID, to State, reason string) (*Concern, error)
}
