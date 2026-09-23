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
// One lifecycle edge is deliberately NOT terminal: superseded ->
// addressed_pending (E45.83 / #3618). A retry discards the prior attempt's
// open implement-review concerns by superseding them, which is correct — they
// were raised against a tree that no longer exists — but the discarded rows
// must stay RECOVERABLE, because the defects they name may well survive the
// retry. So an operator fix-up may route a superseded implement-stage concern
// back into the open set with its reviewer, round and severity intact. It
// stays CLOSED until that happens (IsOpen() is false for superseded), so no
// open-concern surface, merge-gate count or auto-resolve path observes a
// difference until the operator deliberately routes the id.
//
// Mirrors scopeamendment's layout: domain types + Repository here,
// queries.sql + sqlc-generated ./db, postgres.go implementing the
// Repository against pgx.
package concern

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
// lists and fix-up routing accepts by default. waived, deferred, and
// addressed_by_condition are terminal: waived is the operator "this does
// not block" judgment, deferred
// the operator "file a follow-up and resolve" verb (E22.X / #1202) — a
// concern converted into a tracked work item, its state_reason naming the
// filed issue — and addressed_by_condition the condition-claim resolution
// (E48.9 / #1956): a plan-stage concern whose binding approval condition
// one implement review confirmed delivered, its state_reason naming the
// claiming approval and confirming review.
//
// superseded is CLOSED but NOT terminal (E45.83 / #3618). It is the
// re-review supersession — a prior-revision plan-review concern superseded
// when the plan-gate revise handler (fishhawk_revise_plan, #2065) re-plans a
// plan stage in place, or a prior attempt's open implement-review concerns
// discarded when a retry re-runs the stage (#3593). It has exactly ONE
// outgoing edge, superseded -> addressed_pending, so an operator fix-up can
// re-route a discarded concern back into the open set; every other exit is
// refused.
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
// reachable from every open state (the operator waive and
// defer verbs' edges, and the revise/retry supersessions).
// addressed_by_condition (E48.9 / #1956) is likewise
// reachable from every open state — a plan-stage concern
// whose binding approval condition an implement review confirmed. It is
// deliberately NOT reachable from addressed: an already-confirmed concern
// needs no condition resolution.
//
// waived, deferred and addressed_by_condition are genuinely TERMINAL: no
// outgoing edge at all. superseded is the ONE closed state that is not
// (E45.83 / #3618): it carries exactly one edge, superseded ->
// addressed_pending, so an operator fix-up can re-route a retry-discarded
// concern back into the open set with its reviewer, round and severity
// intact, and the existing delta-verification machinery then tracks it to
// closure like any routed concern. Deliberately absent are
// superseded -> addressed (a discarded concern was never answered, so it
// cannot be confirmed), -> reopened (re-routing goes through
// addressed_pending, the state the fix-up machinery keys on), and
// -> waived / deferred / addressed_by_condition (a closed concern needs no
// second disposition). State.IsOpen() is UNCHANGED — superseded still
// reports false — so this edge is invisible to every open-concern surface
// until an operator deliberately routes the id.
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
	StateWaived: {},
	// The ONE edge out of a closed state (E45.83 / #3618): an operator fix-up
	// re-routing a retry-discarded implement-review concern. Reachable only
	// through MarkAddressedPending, whose production caller already scopes ids
	// to implement-stage concerns of the fix-up's target stage.
	StateSuperseded: {
		StateAddressedPending: {},
	},
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
// run-status surface and addressable by fix-up routing. The closed
// states (addressed, waived, superseded, deferred, addressed_by_condition)
// return false — a deferred concern has been converted into a follow-up
// work item, and an addressed_by_condition concern's binding approval
// condition has been confirmed delivered, so neither appears on the
// open-concerns surface.
//
// superseded returns false here even though it now carries one outgoing
// edge (E45.83 / #3618): a retry-discarded concern is NOT open, it is
// closed-and-recoverable. Widening IsOpen would put discarded concerns back
// on every open-concern surface and back into the merge-gate count, which is
// exactly the noise the supersede exists to remove; the recovery path is the
// operator explicitly naming the id in a fix-up, not a state reclassification.
// The run-status surface instead reports how many were discarded
// (concerns.superseded_implement) and the gate view's settled[] ledger carries
// their full rows.
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
	// NewEvidence is the reviewer's supporting evidence for the concern,
	// mirroring planreview.Concern.NewEvidence (#1913). Named for its
	// re-litigation-disarm role — a non-empty NewEvidence lets a re-raise of a
	// settled concern through the guard — but reviewers write substantive
	// backing here on first-round concerns too, which is why every operator
	// read surface renders it (E60.8 / #2353). Empty is the common case;
	// every row minted before migration 0069 reads ''.
	NewEvidence string
	// SettledRef is the stable id of the settled concern this one re-raises,
	// mirroring planreview.Concern.SettledRef (#1913). Empty for a concern
	// that re-raises nothing (the common case), and '' for every row minted
	// before migration 0069.
	SettledRef string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// MissingNoteMarker leads every synthesized stand-in for a blank reviewer
// note (#2555). It is deliberately a literal an operator can grep for: a
// note carrying it was NOT authored by the reviewer.
const MissingNoteMarker = "[no reviewer note recorded]"

// missingNoteUnknownReviewer renders in place of a blank/absent reviewer model.
const missingNoteUnknownReviewer = "an unknown reviewer"

// MissingNotePointer builds the deterministic stand-in text for a concern
// whose reviewer note is blank (#2555): the marker plus a pointer to the
// originating *_reviewed audit entry, which IS the authoritative record of
// what the reviewer emitted. It fabricates no substance — it only guarantees
// the operator, a fix-up agent, and a later re-review get a non-blank field
// naming exactly where to read next.
//
// It is exported because the write-side backfill (server's persistReviewConcerns)
// synthesizes the same text when a blank-note concern arrives with no free_form
// to recover from, so the stored row and this read-side fallback speak ONE
// vocabulary rather than two near-identical ones.
func MissingNotePointer(stageKind, reviewerModel string, originSequence int64) string {
	reviewer := strings.TrimSpace(reviewerModel)
	if reviewer == "" {
		reviewer = missingNoteUnknownReviewer
	}
	kind := strings.TrimSpace(stageKind)
	if kind == "" {
		kind = "unknown"
	}
	return fmt.Sprintf("%s — raised by %s in the %s-stage review recorded at audit sequence %d. "+
		"Read that *_reviewed audit entry for the reviewer's own output; this concern's note was empty.",
		MissingNoteMarker, reviewer, kind, originSequence)
}

// DisplayNote returns the note every operator-facing and prompt-facing surface
// should render for this concern (#2555). It returns c.Note verbatim whenever
// the stored note carries anything but whitespace; when the stored note is
// blank — the legacy rows minted BEFORE the write-side backfill landed — it
// returns MissingNotePointer built from the row itself.
//
// The contract is narrow on purpose: it never fabricates substance and never
// rewrites an authored note. It guarantees only that no surface renders an
// empty field where a concern's substance belongs, and that the empty field is
// replaced by a pointer to where the reviewer's actual output lives.
func (c Concern) DisplayNote() string {
	if strings.TrimSpace(c.Note) != "" {
		return c.Note
	}
	model := ""
	if c.ReviewerModel != nil {
		model = *c.ReviewerModel
	}
	return MissingNotePointer(c.StageKind, model, c.OriginReviewSequence)
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
	// NewEvidence and SettledRef mirror planreview.Concern's fields of the
	// same names (#1913), carried through from the decoded verdict so the
	// persisted row — and every surface reading it — keeps the reviewer's
	// supporting evidence and re-raise lineage (E60.8 / #2353). Empty is the
	// common case for both.
	NewEvidence string
	SettledRef  string
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
	//
	// This is the sole path along the superseded -> addressed_pending edge
	// (E45.83 / #3618): a SUPERSEDED row re-enters the open set here, keeping
	// its reviewer, origin round and severity, with reason as its new
	// state_reason. A waived / deferred / addressed_by_condition row still
	// fails InvalidTransitionError, in the Go machine and in the store's
	// from-state-guarded UPDATE alike.
	MarkAddressedPending(ctx context.Context, ids []uuid.UUID, reason string) error

	// ApplyResolution transitions one concern to the given state after
	// validating the lifecycle machine — the entry point the deferred
	// re-review delta threading will confirm/reopen through. A confirm
	// (addressed) arriving after a reopen fails with
	// InvalidTransitionError for the caller to log; it never downgrades
	// the reopened state.
	ApplyResolution(ctx context.Context, id uuid.UUID, to State, reason string) (*Concern, error)
}
