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

// Repository persists refinement drafts + decisions and resolves them by id or
// by refinement session. A draft is NEVER filed here — the repository stores
// the draft artifact and the append-only decision record; the E34.3 filing
// executor is what turns an approved draft into provider work items.
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
}
