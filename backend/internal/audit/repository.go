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
// EntryHash within the same chain — either the run's chain or the
// global chain) and EntryHash. Use AppendChained / AppendGlobalChained
// for the high-level paths that compute both.
//
// RunID is nullable: nil writes a global-chain entry (E2.7).
type AppendParams struct {
	RunID        *uuid.UUID
	StageID      *uuid.UUID
	Timestamp    time.Time
	Category     string
	ActorKind    *ActorKind
	ActorSubject *string
	Payload      json.RawMessage
	PrevHash     *string
	EntryHash    string
	// AccountID stamps the tenant workspace account on the row
	// (ADR-057 / #1828). nil = untenanted. Callers of the raw path
	// own chain correctness, so they also own passing the account
	// their PrevHash lookup was scoped to.
	AccountID *uuid.UUID
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

	// AppendGlobalChained writes an entry to the run-less ("global")
	// chain (E2.7) — events not tied to a specific workflow run.
	// Since ADR-057 / #1828 the run-less partition is keyed by
	// account_id: PrevHash is computed from the previous entry
	// WITHIN p.AccountID's partition (nil = the untenanted
	// account_id IS NULL partition), each partition an independent
	// chain from a nil-prev_hash genesis. Appends to one partition
	// are serialized by a Postgres advisory transaction lock so
	// concurrent writers cannot fork the chain. RunID on the
	// resulting Entry is nil; StageID is also nil for these entries.
	AppendGlobalChained(ctx context.Context, p GlobalChainAppendParams) (*Entry, error)

	Get(ctx context.Context, id uuid.UUID) (*Entry, error)

	// ListForRun returns every entry for the run, ordered by
	// sequence ascending. Used for run-detail UI and verification.
	ListForRun(ctx context.Context, runID uuid.UUID) ([]*Entry, error)

	// ListGlobal returns every run-less entry in append order,
	// across ALL account partitions. Used by the compliance export
	// and the audit verifier; per-partition verification walks
	// ListGlobalByAccount instead.
	ListGlobal(ctx context.Context) ([]*Entry, error)

	// ListGlobalByAccount returns the run-less entries of ONE chain
	// partition in append order (ADR-057 / #1828): accountID's
	// partition when non-nil, the untenanted account_id IS NULL
	// partition when nil. Each result is an independently
	// verifiable chain from a nil-prev_hash genesis.
	ListGlobalByAccount(ctx context.Context, accountID *uuid.UUID) ([]*Entry, error)

	// LastForRun returns the most recently appended entry in the run,
	// or ErrNotFound if no entries exist yet.
	LastForRun(ctx context.Context, runID uuid.UUID) (*Entry, error)

	// ListForRunByCategory filters entries within a run to those of
	// the given category. Used for "show only failures" / "show only
	// approvals" views and for the compliance export.
	ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*Entry, error)

	// ListAll returns entries across both chains (per-run rows and
	// global-chain rows), ordered by ts descending — the audit-log
	// search surface (#211) feeds off this. Filters are AND-combined
	// and any subset may be nil. Note this is *not* the same as
	// ListGlobal: the latter is the verifier's view of the global
	// chain partition only; ListAll mixes both chains for human
	// search.
	ListAll(ctx context.Context, p ListAllParams) ([]*Entry, error)

	// ChainsByParent returns audit entries for parentRunID and all its
	// linked descendants, following parent_run_id links recursively.
	// When includeDecomposed is false, runs where decomposed_from IS
	// NOT NULL are excluded from the walk (CI-retry chain view).
	// When true, all descendants are included (compliance export view).
	ChainsByParent(ctx context.Context, parentRunID uuid.UUID, includeDecomposed bool) ([]*Entry, error)
}

// ListAllParams collects the optional filters for ListAll. nil means
// "no filter on that field".
type ListAllParams struct {
	Category *string
	RunID    *uuid.UUID
	// AccountID scopes the listing to a tenant workspace account
	// (ADR-057 / #1830). Empty = no constraint, mirroring
	// run.ListRunsFilter.AccountID — the internal system readers
	// (calibration, cost/alert scans, prompt + acceptance stats) leave it
	// empty so their cross-account reads are unnarrowed. When set, the
	// query keeps rows whose account_id equals it OR whose account_id is
	// NULL (untenanted rows stay universally visible — the #1829
	// NULL-allow window); the user-facing audit-list handler passes the
	// caller's Identity.AccountID.
	AccountID string
}
