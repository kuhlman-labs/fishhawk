package audit

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

// ErrNotFound signals a missing entry.
var ErrNotFound = errors.New("audit entry not found")

// AppendParams collects the inputs needed for one raw Append. The
// caller is responsible for computing PrevHash (= the prior entry's
// EntryHash within the same run, or nil for the run's first entry)
// and EntryHash. Use AppendChained for the high-level path that
// computes both.
type AppendParams struct {
	RunID        uuid.UUID
	StageID      *uuid.UUID
	Timestamp    time.Time
	Category     string
	ActorKind    *ActorKind
	ActorSubject *string
	Payload      json.RawMessage
	PrevHash     *string
	EntryHash    string
}

// Repository is the append-only audit log. Note the deliberate
// absence of Update / Delete — the API surface itself enforces the
// invariant; the database triggers are belt-and-suspenders.
//
// Two write paths:
//
//   - Append (raw) — accepts precomputed PrevHash and EntryHash.
//     Used by AppendChained internally and by tests / backfills.
//   - AppendChained — the recommended public path: looks up the
//     run's last entry, computes PrevHash and EntryHash via
//     ComputeEntryHash, and writes atomically inside a transaction
//     that holds a row-level lock on the runs row, so concurrent
//     callers can't observe the same prev_hash and produce a fork.
type Repository interface {
	Append(ctx context.Context, p AppendParams) (*Entry, error)
	AppendChained(ctx context.Context, p ChainAppendParams) (*Entry, error)

	Get(ctx context.Context, id uuid.UUID) (*Entry, error)

	// ListForRun returns every entry for the run, ordered by
	// sequence ascending. Used for run-detail UI and verification.
	ListForRun(ctx context.Context, runID uuid.UUID) ([]*Entry, error)

	// LastForRun returns the most recently appended entry in the run,
	// or ErrNotFound if no entries exist yet.
	LastForRun(ctx context.Context, runID uuid.UUID) (*Entry, error)

	// ListForRunByCategory filters entries within a run to those of
	// the given category. Used for "show only failures" / "show only
	// approvals" views and for the compliance export.
	ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*Entry, error)
}
