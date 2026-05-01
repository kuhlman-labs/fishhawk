package artifact

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
)

// ErrNotFound signals a missing artifact. The Postgres adapter
// translates pgx.ErrNoRows into this so callers don't depend on
// the database driver's error type.
var ErrNotFound = errors.New("artifact not found")

// CreateParams collects the inputs needed to insert an artifact.
// ContentHash is computed by the caller (typically sha256 over the
// canonical bytes of Content) so the same value can be used for
// dedup lookups via GetByHash.
type CreateParams struct {
	StageID       uuid.UUID
	Kind          Kind
	SchemaVersion *string
	Content       json.RawMessage
	ContentHash   string
}

// Repository persists artifacts and resolves them by id, by stage,
// or by content hash within a stage.
type Repository interface {
	Create(ctx context.Context, p CreateParams) (*Artifact, error)
	Get(ctx context.Context, id uuid.UUID) (*Artifact, error)
	ListForStage(ctx context.Context, stageID uuid.UUID) ([]*Artifact, error)

	// GetByHash returns an existing artifact in the given stage that
	// has the given content hash, or ErrNotFound. Used to dedup the
	// re-upload case during retry, where an idempotent runner ships
	// identical plan bytes a second time.
	GetByHash(ctx context.Context, stageID uuid.UUID, contentHash string) (*Artifact, error)
}
