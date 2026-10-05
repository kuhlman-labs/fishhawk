// Package concurrency is the slot store behind local stage concurrency groups
// (#3964 / ADR-087): a host-dispatched stage asks for a slot in a group before
// its spawn marker transitions it to dispatched, and either is ADMITTED (the
// stage CAS and the held mark commit in one transaction) or QUEUED FIFO behind
// the group's live holders. Holding is derived from stage state, so a settled,
// parked or crashed holder releases without a write here. README.md is the
// contract.
package concurrency

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Shipped defaults. README.md § "Constants" records why each has its value.
const (
	// DefaultGroupPrefix prefixes the per-host default group every local
	// implement stage joins: "local-implement:<host>".
	DefaultGroupPrefix = "local-implement:"
	// UnknownHost is the host label used when the client sends none.
	UnknownHost = "unknown"
	// DefaultLimit is the default group's slot count.
	DefaultLimit = 1
	// MaxLimit bounds a declared limit (matches the 0097 CHECK).
	MaxLimit = 64
	// QueueTTL is how long a queued row stays live without a refresh. A row
	// older than this is skipped by everyone behind it and, when its owner
	// polls again, restarts at the TAIL.
	QueueTTL = 60 * time.Second
	// WaiterPollInterval is the cadence a slot waiter re-asks at; well inside
	// QueueTTL so a live waiter never goes stale.
	WaiterPollInterval = 5 * time.Second
	// RunningStaleAfter bounds a running holder with no heartbeat: past it the
	// holder no longer counts (crash release backstop).
	RunningStaleAfter = 45 * time.Minute
	// DispatchedStaleAfter bounds a holder admitted but never spawned.
	DispatchedStaleAfter = 15 * time.Minute
)

// SlotState is a stage_concurrency_slots.state value.
type SlotState string

// Slot states.
const (
	SlotQueued SlotState = "queued"
	SlotHeld   SlotState = "held"
)

// ErrInvalidRequest is returned (wrapped) for a Request the store refuses
// before touching the database.
var ErrInvalidRequest = errors.New("concurrency: invalid admission request")

// Request asks for a slot for one host-dispatched stage.
type Request struct {
	StageID uuid.UUID
	// RunID must be the stage's own run; a mismatch is refused as not found.
	RunID uuid.UUID
	// From is the stage state the caller loaded (pending or
	// awaiting_host_dispatch); the admission CAS is pinned to it.
	From     run.StageState
	GroupKey string
	// Limit is the admitting stage's slot count (1..MaxLimit); it governs
	// when stages in one group disagree.
	Limit int
	// Host is the client-supplied host label recorded on the row.
	Host string
	// AdmissionNonce is the client-supplied slot-waiter nonce ("" for none).
	// An admission records it on the held row (a queued row never carries
	// one), so a waiter that lost its admission response can recognise its
	// own admission (README.md § "Admission nonce").
	AdmissionNonce string
}

// Holder is one stage currently holding a slot in a group.
type Holder struct {
	RunID   uuid.UUID
	StageID uuid.UUID
	Since   time.Time
}

// Admission is Admit's verdict.
type Admission struct {
	// Admitted reports the stage was transitioned to dispatched and holds a
	// slot; Stage is the post-transition stage.
	Admitted bool
	Stage    *run.Stage
	// Position is the 1-based queue position when queued (0 when admitted).
	Position int
	// Holders is the group's live holders, never nil (an empty list renders
	// as []).
	Holders    []Holder
	EnqueuedAt time.Time
	// NewlyQueued reports the row was inserted or restarted at the tail by
	// this call — once per queue episode.
	NewlyQueued bool
	// QueuedBefore reports an admitted stage had waited in this episode.
	QueuedBefore bool
	// Contended reports the group lock was held by another admission, so this
	// call queued without deciding.
	Contended     bool
	WaitedSeconds int
}

// Status is a stage's ACTIVE slot state for read surfaces.
type Status struct {
	State      SlotState
	GroupKey   string
	Limit      int
	Position   int
	Holders    []Holder
	EnqueuedAt time.Time
	AcquiredAt *time.Time
	// WaiterLive reports a queued row was refreshed within QueueTTL.
	WaiterLive bool
	// Host and HeldDispatchedAt describe the admission a held row records
	// (the host label and the admitted attempt). They are informational: two
	// sessions on one host share a label, so ownership is AdmissionNonce.
	Host             string
	HeldDispatchedAt *time.Time
	// AdmissionNonce is the nonce the admitting request carried ("" when it
	// carried none); set only on a held status.
	AdmissionNonce string
}

// Store is the slot store.
type Store interface {
	Admit(ctx context.Context, req Request) (Admission, error)
	StatusForStages(ctx context.Context, stageIDs []uuid.UUID) (map[uuid.UUID]Status, error)
}

// DefaultGroupKey is the host default group key.
func DefaultGroupKey(host string) string {
	if host == "" {
		host = UnknownHost
	}
	return DefaultGroupPrefix + host
}

// NamedGroupKey is a spec-declared group key, scoped to its repository.
func NamedGroupKey(repo, name string) string {
	return "spec:" + repo + ":" + name
}

// lockKey derives the transaction-scoped advisory-lock key for one group in
// one account. It mirrors captain.captainLockKey (first 8 bytes of a sha256
// over a fishhawk-namespaced input), with its own "fishhawk:concurrency:"
// domain so the key space is disjoint from every other advisory lock. A
// collision only produces a spurious contended/queued answer.
func lockKey(accountID *uuid.UUID, group string) int64 {
	h := sha256.New()
	h.Write([]byte("fishhawk:concurrency:"))
	if accountID != nil {
		h.Write(accountID[:])
	} else {
		h.Write([]byte("untenanted"))
	}
	h.Write([]byte{0})
	h.Write([]byte(group))
	sum := h.Sum(nil)
	return int64(binary.BigEndian.Uint64(sum[:8])) //nolint:gosec // deliberate wrap: advisory-lock keys are opaque int64s
}
