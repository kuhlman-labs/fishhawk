// Package mergequeue is the persisted per-(repository, base ref) held-pass
// queue behind ADR-092 D1 (#4200): at most ONE merge-candidate verify pass is
// live per base, and every other eligible pass is held FIFO by eligibility
// time, consuming no runner slot. Every mutation is one transaction under a
// per-base advisory lock, scoped by account through runs.account_id. The rows
// are shaped so ADR-093's merge queue extends them without a second
// migration. README.md is the contract.
package mergequeue

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// State is a merge_candidate_queue_entries.state value.
type State string

// Entry states. Held and live are ACTIVE; the rest are TERMINAL.
const (
	StateHeld    State = "held"
	StateLive    State = "live"
	StateMerged  State = "merged"
	StateEjected State = "ejected"
	StateDropped State = "dropped"
)

// Active reports whether s is held or live.
func (s State) Active() bool { return s == StateHeld || s == StateLive }

// Terminal reports whether s is merged, ejected or dropped.
func (s State) Terminal() bool {
	return s == StateMerged || s == StateEjected || s == StateDropped
}

// Phase is a live entry's progress. The column has no CHECK: these are the
// phases ADR-092 writes, and ADR-093 adds its own without a migration.
type Phase string

// ADR-092 phases.
const (
	// PhaseAdmitted is stamped by AdmitNext: the admitting pump owns the
	// admission work (advance, re-anchor, pass start) until it moves on.
	PhaseAdmitted   Phase = "admitted"
	PhaseVerifying  Phase = "verifying"
	PhasePassed     Phase = "passed"
	PhaseUnverified Phase = "unverified"
)

// ReasonBaseChanged is the drop reason Enqueue records when a run with an
// active entry re-enters on a different base ref. Every other eject/drop
// reason is the caller's (README.md § "Reasons").
const ReasonBaseChanged = "base_changed"

// AdmissionStaleAfter bounds how long a live entry may stay in
// PhaseAdmitted: past it AdmitNext re-claims the admission (a crash
// mid-admission recovers on the next pump). README.md § "Constants".
const AdmissionStaleAfter = 10 * time.Minute

// Errors. Each is returned wrapped; match with errors.Is.
var (
	// ErrInvalidRequest is a request refused before any SQL runs.
	ErrInvalidRequest = errors.New("mergequeue: invalid request")
	// ErrNotFound is an unknown run or entry. It wraps run.ErrNotFound, so
	// errors.Is(err, run.ErrNotFound) also matches.
	ErrNotFound = fmt.Errorf("mergequeue: %w", run.ErrNotFound)
	// ErrStateConflict is an entry not in a state the call accepts (for
	// example Unadmit on a held entry, or Reanchor on a terminal one).
	ErrStateConflict = errors.New("mergequeue: entry state conflict")
)

// Base is one queue: a repository base ref inside one account. AccountID is
// the run's runs.account_id as a string ("" for an untenanted run), the same
// shape run.Run.AccountID carries.
type Base struct {
	AccountID string
	Repo      string
	BaseRef   string
}

// Entry is one merge_candidate_queue_entries row. Phase, EjectReason and
// AnchoredBaseSHA are "" for NULL. AccountID is read through runs, never
// stored on the row.
type Entry struct {
	ID              uuid.UUID
	RunID           uuid.UUID
	AccountID       string
	Repo            string
	BaseRef         string
	State           State
	Phase           Phase
	EjectReason     string
	EnqueuedAt      time.Time
	AnchoredHeadSHA string
	AnchoredBaseSHA string
	AnchoredAt      time.Time
	AdmittedAt      *time.Time
	SettledAt       *time.Time
	UpdatedAt       time.Time
}

// Base returns the queue the entry belongs to.
func (e Entry) Base() Base {
	return Base{AccountID: e.AccountID, Repo: e.Repo, BaseRef: e.BaseRef}
}

// EnqueueRequest asks to put a run's pass in its base's queue. The
// repository and account come from the run row, never the caller.
type EnqueueRequest struct {
	RunID   uuid.UUID
	BaseRef string
	// HeadSHA is the PR head the pass is anchored to (required).
	HeadSHA string
	// BaseSHA is the base tip the head was produced against ("" if unknown).
	BaseSHA string
}

// EnqueueOutcome says what Enqueue did.
type EnqueueOutcome string

// Enqueue outcomes.
const (
	// EnqueueInserted: a new held row at the tail (the run had no active
	// entry, or only terminal ones).
	EnqueueInserted EnqueueOutcome = "inserted"
	// EnqueueReanchored: the run's active entry on the same base was
	// re-anchored in place, keeping its state, phase and enqueued_at.
	EnqueueReanchored EnqueueOutcome = "reanchored"
	// EnqueueBaseChanged: the run's active entry on another base was dropped
	// base_changed and a new held row inserted at the new base's tail.
	EnqueueBaseChanged EnqueueOutcome = "base_changed"
)

// EnqueueResult is Enqueue's verdict.
type EnqueueResult struct {
	Entry   Entry
	Outcome EnqueueOutcome
	// AnchorChanged reports a re-anchor moved the head or base SHA (false on
	// a same-anchor re-enqueue, and on the insert outcomes).
	AnchorChanged bool
	// Dropped is the entry dropped base_changed (EnqueueBaseChanged only).
	// When it was live its base has no live entry now; pump that base.
	Dropped *Entry
}

// Admission is AdmitNext's verdict.
type Admission struct {
	// Admitted reports THIS call made Entry the base's live entry: either a
	// held entry was admitted, or (Readmitted) a stale admission was
	// re-claimed. The caller owns the admission work.
	Admitted   bool
	Readmitted bool
	// Entry is the admitted entry when Admitted; otherwise the base's live
	// entry holding the line, or nil when the base has no live and no held
	// entry.
	Entry *Entry
}

// SettleResult is Settle's verdict.
type SettleResult struct {
	// Entry is the row after the call. On an already-terminal row it is that
	// row unchanged, carrying its FIRST terminal state and reason.
	Entry Entry
	// Settled reports this call moved the row to a terminal state.
	Settled bool
}

// Status is a run's active entry for read surfaces.
type Status struct {
	Entry Entry
	// Position is the 1-based place among the base's held entries (same
	// account) by (enqueued_at, id); 0 for a live entry.
	Position int
	// LiveRunID is the run holding the base's line (the entry's own run when
	// it is live), or nil when the base has no live entry.
	LiveRunID *uuid.UUID
}

// Store is the held-pass queue. README.md § "Contract" defines every method.
type Store interface {
	Enqueue(ctx context.Context, req EnqueueRequest) (EnqueueResult, error)
	AdmitNext(ctx context.Context, base Base) (Admission, error)
	Unadmit(ctx context.Context, entryID uuid.UUID) (Entry, error)
	SetPhase(ctx context.Context, entryID uuid.UUID, phase Phase) (Entry, error)
	Reanchor(ctx context.Context, entryID uuid.UUID, headSHA, baseSHA string) (Entry, error)
	Settle(ctx context.Context, entryID uuid.UUID, state State, reason string) (SettleResult, error)
	ActiveForRun(ctx context.Context, runID uuid.UUID) (Status, bool, error)
	ListForBase(ctx context.Context, base Base) ([]Entry, error)
}

// validate refuses a malformed Base before any SQL runs.
func (b Base) validate() error {
	if b.Repo == "" || b.BaseRef == "" {
		return fmt.Errorf("%w: repo and base ref are required", ErrInvalidRequest)
	}
	if b.AccountID != "" {
		if _, err := uuid.Parse(b.AccountID); err != nil {
			return fmt.Errorf("%w: account id %q: %v", ErrInvalidRequest, b.AccountID, err)
		}
	}
	return nil
}

// lockKey derives the transaction-scoped advisory-lock key for one base in
// one account: the first 8 bytes of a sha256 over a "fishhawk:mergequeue:"
// namespaced input, a domain disjoint from every other advisory lock
// (concurrency.lockKey uses "fishhawk:concurrency:"). A collision only
// serializes two unrelated bases.
func lockKey(accountID, repo, baseRef string) int64 {
	h := sha256.New()
	h.Write([]byte("fishhawk:mergequeue:"))
	if id, err := uuid.Parse(accountID); err == nil {
		h.Write(id[:])
	} else {
		h.Write([]byte("untenanted"))
	}
	h.Write([]byte{0})
	h.Write([]byte(repo))
	h.Write([]byte{0})
	h.Write([]byte(baseRef))
	sum := h.Sum(nil)
	return int64(binary.BigEndian.Uint64(sum[:8])) //nolint:gosec // deliberate wrap: advisory-lock keys are opaque int64s
}

// runLockKey derives the per-run advisory-lock key Enqueue takes before any
// base lock, so two enqueues of one run never race the base_changed path.
func runLockKey(runID uuid.UUID) int64 {
	h := sha256.New()
	h.Write([]byte("fishhawk:mergequeue-run:"))
	h.Write(runID[:])
	sum := h.Sum(nil)
	return int64(binary.BigEndian.Uint64(sum[:8])) //nolint:gosec // deliberate wrap: advisory-lock keys are opaque int64s
}
