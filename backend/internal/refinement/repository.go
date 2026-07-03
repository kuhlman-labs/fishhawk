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

// CreateParams collects the inputs needed to persist a refinement draft. The
// Draft is the decoded, validated EpicDraft; the adapter marshals it to the
// JSONB column. Model is the inference model id (empty when unknown), stored
// nullable.
type CreateParams struct {
	SessionID uuid.UUID
	Brief     string
	Draft     EpicDraft
	Model     string
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
	CreatedAt time.Time
}

// Repository persists refinement drafts and resolves them by id or by
// refinement session. A draft is NEVER filed here — the repository stores the
// draft artifact; the E34.3 filing executor is what turns an approved draft
// into provider work items.
type Repository interface {
	CreateDraft(ctx context.Context, p CreateParams) (*StoredDraft, error)
	GetDraft(ctx context.Context, id uuid.UUID) (*StoredDraft, error)
	ListForSession(ctx context.Context, sessionID uuid.UUID) ([]*StoredDraft, error)
}
