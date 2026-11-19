// Package concurrency is the slot store behind local stage concurrency groups
// (#3964 / ADR-087): a host-dispatched stage asks for a slot in a group (or, under
// ADR-094 D2, in every group of its set, all-or-nothing) before its spawn
// marker transitions it to dispatched, and either is ADMITTED (the stage CAS
// and the held mark commit in one transaction) or QUEUED FIFO behind the
// group's live holders. Group keys are scoped identities (ScopedGroupKey,
// ParseGroupKey; ADR-094 Amendment A1 keeps the legacy strings canonical). Admission is FIFO except for a waiter an operator
// PROMOTED (#4191), which is ordered just ahead of every other fresh waiter for
// the rest of its queue episode. Holding is derived from stage state, so a
// settled, parked or crashed holder releases without a write here. README.md
// is the contract.
package concurrency

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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
	// VerifyGroupPrefix prefixes the per-host group a merge-candidate
	// verify-only pass joins (ADR-092 D3 / #4200): "local-verify:<host>". A
	// pass runs no agent, so it never waits behind a queued full implement.
	VerifyGroupPrefix = "local-verify:"
	// DefaultVerifyLimit is the verify group's slot count.
	DefaultVerifyLimit = 1
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
	// MaxGroups bounds the size of one admission's group set (ADR-094 D2 /
	// #4240). It is deliberately ABOVE the workflow-v2 schema's
	// concurrency.groups maxItems (8): ADR-094 slice 3 (#4241) adds the host
	// capacity group to a full spec-declared set, and that must not need a
	// store change. TestMaxGroupsAboveSchemaMaxItems pins the ordering.
	MaxGroups = 16
)

// Scope is the scope of a concurrency group identity (ADR-094 D3).
type Scope string

// Group scopes. ScopeAccount is reserved for review-capacity groups (#4242);
// no spec can declare it.
const (
	ScopeHost       Scope = "host"
	ScopeRepository Scope = "repository"
	ScopeAccount    Scope = "account"
)

// ScopedGroup is a group identity: a name inside one scope instance
// (ADR-094 D3). Its key string comes from ScopedGroupKey, which keeps the
// legacy key strings canonical (ADR-094 Amendment A1).
type ScopedGroup struct {
	Scope   Scope
	ScopeID string
	Name    string
}

// ErrInvalidGroupKey is returned (wrapped) by ScopedGroupKey for an identity
// that has no canonical key.
var ErrInvalidGroupKey = errors.New("concurrency: invalid group identity")

// GroupRequest is one group of an admission's group set.
type GroupRequest struct {
	Key string
	// Limit is this group's slot count (1..MaxLimit).
	Limit int
}

// GroupState is one group's view inside an Admission or a Status. Scope is
// "" when Key does not parse (ParseGroupKey).
type GroupState struct {
	Key   string
	Scope Scope
	Limit int
	// Position is the 1-based queue position in this group when queued (0
	// when held or admitted).
	Position int
	// Holders is this group's live holders, never nil.
	Holders []Holder
}

// PromotionGroup is one group's view inside a Promotion.
type PromotionGroup struct {
	Key         string
	Limit       int
	OldPosition int
	NewPosition int
	// Holders is this group's live holders, never nil.
	Holders []Holder
}

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

// Promote refusals (README.md § "Promote"). An unknown stage wraps
// run.ErrNotFound instead.
var (
	// ErrNotQueued: the stage has no active queued slot row (it never asked
	// for a slot, it holds a previous episode's row, or it is no longer
	// pending/awaiting_host_dispatch).
	ErrNotQueued = errors.New("concurrency: stage is not queued in a concurrency group")
	// ErrSlotHeld: the stage holds its slot. Holders are never preempted.
	ErrSlotHeld = errors.New("concurrency: stage holds its slot; holders are never preempted")
	// ErrWaiterNotLive: the stage's queued row is stale past QueueTTL. It
	// counts for nobody and restarts at the tail on its next poll, which
	// would lose a promotion, so the operator re-dispatches it first.
	ErrWaiterNotLive = errors.New("concurrency: queued waiter is not live (stale past QueueTTL)")
	// ErrSlotChanged: the slot row moved to another group between the
	// promote's unlocked read and its locked re-read.
	ErrSlotChanged = errors.New("concurrency: slot row changed group during the promote")
)

// Request asks for a slot for one host-dispatched stage.
type Request struct {
	StageID uuid.UUID
	// RunID must be the stage's own run; a mismatch is refused as not found.
	RunID uuid.UUID
	// From is the stage state the caller loaded (pending or
	// awaiting_host_dispatch); the admission CAS is pinned to it.
	From run.StageState
	// GroupKey and Limit are the single-group shorthand.
	GroupKey string
	// Limit is the admitting stage's slot count (1..MaxLimit); it governs
	// when stages in one group disagree.
	Limit int
	// Groups is the stage's group SET (ADR-094 D2), mutually exclusive with
	// the GroupKey/Limit shorthand; at most MaxGroups entries. It takes
	// effect in the multi-group Admit (store.go, #4240 slice 1a), which
	// admits all-or-nothing across the set; the store ignored the field
	// before that slice.
	Groups []GroupRequest
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

// Admission is Admit's verdict. Its top-level Position and Holders describe
// the BINDING group: while queued, the group with the highest position (ties
// broken in canonical key order); once admitted, the first group in canonical
// key order. Groups lists every group of the set.
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
	// Groups is the per-group view of the set, in canonical key order.
	Groups []GroupState
}

// Status is a stage's ACTIVE slot state for read surfaces. Its top-level
// GroupKey, Limit, Position and Holders describe the BINDING group (the
// Admission rule); Groups lists every group of the stage's set.
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
	// Promoted reports an ACTIVE queued row an operator promoted in its
	// current queue episode (order_key set); never set on a held status.
	Promoted bool
	// Groups is the per-group view of the set, in canonical key order.
	Groups []GroupState
}

// PromoteRequest asks to move one queued waiter to the front of its group.
type PromoteRequest struct {
	StageID uuid.UUID
	// Record is REQUIRED. It runs inside the promotion transaction after the
	// order key is written and before COMMIT, and only when the order
	// changed; a non-nil error rolls the whole promotion back. The server
	// appends the audit row through it, so a promotion never commits
	// unaudited.
	Record func(ctx context.Context, tx pgx.Tx, p Promotion) error
}

// Promotion is Promote's verdict. Its top-level GroupKey, Limit, positions
// and Holders describe the BINDING group (the Admission rule); Groups lists
// every group of the stage's set.
type Promotion struct {
	StageID  uuid.UUID
	RunID    uuid.UUID
	GroupKey string
	Limit    int
	// Changed is false when the stage was already first (a no-op: nothing
	// written, Record not called).
	Changed     bool
	OldPosition int
	NewPosition int
	// Holders is the group's live holders, never nil.
	Holders []Holder
	// Groups is the per-group view of the set, in canonical key order.
	Groups []PromotionGroup
}

// Store is the slot store.
type Store interface {
	Admit(ctx context.Context, req Request) (Admission, error)
	StatusForStages(ctx context.Context, stageIDs []uuid.UUID) (map[uuid.UUID]Status, error)
	Promote(ctx context.Context, req PromoteRequest) (Promotion, error)
}

// DefaultGroupKey is the host default group key.
func DefaultGroupKey(host string) string {
	if host == "" {
		host = UnknownHost
	}
	return DefaultGroupPrefix + host
}

// VerifyGroupKey is the host verify group key a merge-candidate verify-only
// pass is admitted through (ADR-092 D3).
func VerifyGroupKey(host string) string {
	if host == "" {
		host = UnknownHost
	}
	return VerifyGroupPrefix + host
}

// NamedGroupKey is a spec-declared group key, scoped to its repository.
func NamedGroupKey(repo, name string) string {
	return "spec:" + repo + ":" + name
}

// Group-identity names that map onto the legacy host keys.
const (
	hostImplementName = "implement"
	hostVerifyName    = "verify"
	specGroupPrefix   = "spec:"
)

// ScopedGroupKey returns the CANONICAL key of a scoped group identity
// (ADR-094 D3 as amended by A1). The legacy strings stay canonical:
// host/<h>/implement is DefaultGroupKey(h), host/<h>/verify is
// VerifyGroupKey(h), and repository/<repo>/<name> is NamedGroupKey(repo,
// name). Every other identity renders as <scope>:<scope-id>:<name>. An empty
// host scope-id becomes UnknownHost. It returns ErrInvalidGroupKey (and "")
// for an unknown scope, an empty name, an empty non-host scope-id, or a
// scope-id containing ':' — a key has exactly one spelling, and
// ParseGroupKey must split it back at the first two separators.
func ScopedGroupKey(scope Scope, scopeID, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("%w: empty group name", ErrInvalidGroupKey)
	}
	if strings.Contains(scopeID, ":") {
		return "", fmt.Errorf("%w: scope id %q contains ':'", ErrInvalidGroupKey, scopeID)
	}
	switch scope {
	case ScopeHost:
		switch name {
		case hostImplementName:
			return DefaultGroupKey(scopeID), nil
		case hostVerifyName:
			return VerifyGroupKey(scopeID), nil
		}
		if scopeID == "" {
			scopeID = UnknownHost
		}
		return string(scope) + ":" + scopeID + ":" + name, nil
	case ScopeRepository:
		if scopeID == "" {
			return "", fmt.Errorf("%w: empty repository scope id", ErrInvalidGroupKey)
		}
		return NamedGroupKey(scopeID, name), nil
	case ScopeAccount:
		if scopeID == "" {
			return "", fmt.Errorf("%w: empty account scope id", ErrInvalidGroupKey)
		}
		return string(scope) + ":" + scopeID + ":" + name, nil
	}
	return "", fmt.Errorf("%w: unknown scope %q", ErrInvalidGroupKey, scope)
}

// ParseGroupKey is the strict inverse of ScopedGroupKey: it accepts exactly
// the canonical spellings (local-implement:<host>, local-verify:<host>,
// spec:<repo>:<name>, host:<id>:<name>, account:<id>:<name>) and reports
// ok=false for anything else, including the non-canonical aliases
// repository:<repo>:<name>, host:<id>:implement and host:<id>:verify
// (ADR-094 A1). The scope-id is the text between the first and second ':'
// and must be non-empty; the name is the remainder and may contain ':'. For
// every accepted key, ScopedGroupKey(ParseGroupKey(key)) == key.
func ParseGroupKey(key string) (ScopedGroup, bool) {
	for _, legacy := range []struct {
		prefix string
		name   string
	}{{DefaultGroupPrefix, hostImplementName}, {VerifyGroupPrefix, hostVerifyName}} {
		if host, ok := strings.CutPrefix(key, legacy.prefix); ok {
			if host == "" || strings.Contains(host, ":") {
				return ScopedGroup{}, false
			}
			return ScopedGroup{Scope: ScopeHost, ScopeID: host, Name: legacy.name}, true
		}
	}
	prefix, rest, ok := strings.Cut(key, ":")
	if !ok {
		return ScopedGroup{}, false
	}
	var scope Scope
	switch prefix + ":" {
	case specGroupPrefix:
		scope = ScopeRepository
	case string(ScopeHost) + ":":
		scope = ScopeHost
	case string(ScopeAccount) + ":":
		scope = ScopeAccount
	default:
		// Includes the "repository:" alias: its canonical spelling is spec:.
		return ScopedGroup{}, false
	}
	scopeID, name, ok := strings.Cut(rest, ":")
	if !ok || scopeID == "" || name == "" {
		return ScopedGroup{}, false
	}
	if scope == ScopeHost && (name == hostImplementName || name == hostVerifyName) {
		// Alias: the canonical spellings are local-implement:/local-verify:.
		return ScopedGroup{}, false
	}
	return ScopedGroup{Scope: scope, ScopeID: scopeID, Name: name}, true
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
