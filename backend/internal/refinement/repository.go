package refinement

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// ErrNotFound signals a missing refinement draft. The Postgres adapter
// translates pgx.ErrNoRows into this so callers don't depend on the database
// driver's error type.
var ErrNotFound = errors.New("refinement draft not found")

// Draft origin values. A draft revision records HOW it came to exist so the
// brief-amendment budget is countable from rows (mirroring how revise counts
// plan_revised entries): OriginBrief for the initial draft, OriginAmendment
// for an agent-re-drafted revision from a brief amendment, OriginEdit for a
// direct field edit. They match the refinement_drafts.origin CHECK.
const (
	OriginBrief     = "brief"
	OriginAmendment = "amendment"
	OriginEdit      = "edit"
)

// Decision verdict values, matching the refinement_decisions.decision CHECK.
const (
	DecisionApproved = "approved"
	DecisionRejected = "rejected"
)

// CreateParams collects the inputs needed to persist a refinement draft. The
// Draft is the decoded, validated EpicDraft; the adapter marshals it to the
// JSONB column. Model is the inference model id (empty when unknown), stored
// nullable. Origin records how the revision came to exist (OriginBrief /
// OriginAmendment / OriginEdit); empty normalizes to OriginBrief in the adapter.
type CreateParams struct {
	// ID optionally pre-sets the draft revision's primary key. The zero value
	// (uuid.Nil) lets the adapter allocate one. A caller pre-generates the id
	// when it must name the new revision BEFORE the row is persisted — e.g. the
	// E34.2 edit path, which appends the durable-before-state-change audit
	// entry (carrying this draft_id) prior to the persist.
	ID        uuid.UUID
	SessionID uuid.UUID
	Brief     string
	Draft     EpicDraft
	Model     string
	Origin    string
}

// StoredDraft is a persisted refinement draft with its decoded EpicDraft. It
// is what the repository returns on create and read: the durable row plus the
// draft unmarshaled back from the JSONB column.
type StoredDraft struct {
	ID        uuid.UUID
	SessionID uuid.UUID
	Brief     string
	Draft     EpicDraft
	Model     string
	Origin    string
	CreatedAt time.Time
}

// Decision is an append-only approve/reject verdict on ONE draft revision. It
// pins the decided draft's id AND a content hash of the decoded EpicDraft, so
// the gate can derive "what was approved is what files": a decision counts only
// when it targets the session's latest revision and its pinned hash still
// matches the recomputed hash (see gate.go). DecidedBy is the auth identity
// subject (empty when unknown, stored nullable).
type Decision struct {
	ID               uuid.UUID
	SessionID        uuid.UUID
	DraftID          uuid.UUID
	Decision         string
	Reason           string
	DraftContentHash string
	DecidedBy        string
	CreatedAt        time.Time
}

// DecisionParams collects the inputs needed to persist a refinement decision.
type DecisionParams struct {
	SessionID        uuid.UUID
	DraftID          uuid.UUID
	Decision         string
	Reason           string
	DraftContentHash string
	DecidedBy        string
}

// FilingSession is the per-approved-draft filing record (E34.3 / #1594). It
// durably pins the target repo at first invoke (a re-invoke naming a different
// repo fails closed) and its CompletedAt flip is the session-closing state
// change. CompletedAt is nil until the executor finishes filing every item.
type FilingSession struct {
	DraftID     uuid.UUID
	SessionID   uuid.UUID
	Repo        string
	CreatedAt   time.Time
	CompletedAt *time.Time
}

// FilingSessionParams collects the inputs to open a filing session.
type FilingSessionParams struct {
	DraftID   uuid.UUID
	SessionID uuid.UUID
	Repo      string
}

// FiledItem is one durable ordinal->issue mapping recorded immediately after a
// provider File returns (E34.3 / #1594). Ordinal 0 is the epic, 1..N the draft
// children. The unique (DraftID, Ordinal) constraint is the DB-level
// never-double-record backstop that pairs with the executor's record-after-file
// ordering and the per-draft advisory lock.
type FiledItem struct {
	DraftID     uuid.UUID
	Ordinal     int
	IssueNumber int
	IssueURL    string
	CreatedAt   time.Time
}

// FiledItemParams collects the inputs to record one filed item.
type FiledItemParams struct {
	DraftID     uuid.UUID
	Ordinal     int
	IssueNumber int
	IssueURL    string
}

// Repository persists refinement drafts + decisions and resolves them by id or
// by refinement session, and (E34.3 / #1594) persists the filing idempotency
// ledger the executor consumes. A draft is NEVER filed here — the repository
// stores the draft artifact, the append-only decision record, and the durable
// filing ledger; the E34.3 filing executor is what turns an approved draft into
// provider work items.
//
// Both drafts and decisions are append-only: there is no update or delete path.
// A new revision (an edit) is a new draft row that structurally invalidates a
// prior approval (the decision no longer targets the latest revision), never a
// mutation — so the pinned revision E34.3 reads is stable.
type Repository interface {
	CreateDraft(ctx context.Context, p CreateParams) (*StoredDraft, error)
	GetDraft(ctx context.Context, id uuid.UUID) (*StoredDraft, error)
	ListForSession(ctx context.Context, sessionID uuid.UUID) ([]*StoredDraft, error)

	// RecordDecision appends an approve/reject verdict pinning one draft
	// revision + its content hash. Append-only: no update/delete.
	RecordDecision(ctx context.Context, p DecisionParams) (*Decision, error)
	// ListDecisions returns every decision for the session in append order.
	ListDecisions(ctx context.Context, sessionID uuid.UUID) ([]*Decision, error)

	// CreateFilingSession opens the per-draft filing session, pinning the
	// target repo. The draft_id PK means a second open for the same draft
	// fails at insert (surfaced wrapped), so the row is created exactly once.
	CreateFilingSession(ctx context.Context, p FilingSessionParams) (*FilingSession, error)
	// GetFilingSession returns the draft's filing session, or ErrNotFound when
	// no filing has been started for it.
	GetFilingSession(ctx context.Context, draftID uuid.UUID) (*FilingSession, error)
	// CompleteFilingSession flips completed_at to now() when it is still NULL —
	// the session-closing state change. A no-op on an already-completed row.
	CompleteFilingSession(ctx context.Context, draftID uuid.UUID) error
	// RecordFiledItem durably records one ordinal->issue mapping. The unique
	// (draft_id, ordinal) constraint rejects a duplicate record (surfaced
	// wrapped) — the residual at-least-once backstop.
	RecordFiledItem(ctx context.Context, p FiledItemParams) (*FiledItem, error)
	// ListFiledItems returns every recorded item for the draft, ordinal ASC.
	ListFiledItems(ctx context.Context, draftID uuid.UUID) ([]*FiledItem, error)

	// WithFilingLock runs fn while holding a per-draft mutual exclusion, so two
	// concurrent filing invocations for the same draft cannot both observe an
	// ordinal as unfiled (the gpt-5.5 concurrent-duplication guard, ADR-052).
	// The Postgres adapter acquires a session-level pg_advisory_lock keyed on
	// the draft UUID on a dedicated pooled connection and releases it when fn
	// returns; the second caller blocks until the first releases, then observes
	// the first's committed progress. fn's error is returned unchanged.
	WithFilingLock(ctx context.Context, draftID uuid.UUID, fn func(context.Context) error) error
}
