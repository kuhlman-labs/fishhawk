package audit

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	auditdb "github.com/kuhlman-labs/fishhawk/backend/internal/audit/db"
	rundb "github.com/kuhlman-labs/fishhawk/backend/internal/run/db"
)

type postgresRepo struct {
	pool *pgxpool.Pool
}

// NewPostgresRepository wraps a pgxpool.Pool to satisfy Repository.
// Caller retains ownership of the pool.
func NewPostgresRepository(pool *pgxpool.Pool) Repository {
	return &postgresRepo{pool: pool}
}

func (r *postgresRepo) Append(ctx context.Context, p AppendParams) (*Entry, error) {
	q := auditdb.New(r.pool)
	var actorKind *string
	if p.ActorKind != nil {
		s := string(*p.ActorKind)
		actorKind = &s
	}
	row, err := q.AppendAuditEntry(ctx, auditdb.AppendAuditEntryParams{
		ID:           uuid.New(),
		RunID:        p.RunID,
		StageID:      p.StageID,
		Ts:           pgtype.Timestamptz{Time: p.Timestamp, Valid: true},
		Category:     p.Category,
		ActorKind:    actorKind,
		ActorSubject: p.ActorSubject,
		Payload:      []byte(p.Payload),
		PrevHash:     p.PrevHash,
		EntryHash:    p.EntryHash,
	})
	if err != nil {
		return nil, fmt.Errorf("append audit entry: %w", err)
	}
	return rowToEntry(row), nil
}

// AppendGlobalChained writes an entry to the global chain (E2.7).
// PrevHash links the new entry to the previous global-chain entry
// (or nil for the first one). The whole append runs in a
// transaction so concurrent calls can't both observe the same
// prev_hash.
//
// Note: there's no per-row lock here because there's no run row
// to lock — concurrent global writes serialize via the transaction
// + the single chain partition. Postgres' default isolation
// (read committed) is sufficient because GetLastGlobalAuditEntry
// + AppendAuditEntry are run inside the same tx; a second tx
// reading the last entry inside its own tx will see this tx's
// committed write.
func (r *postgresRepo) AppendGlobalChained(ctx context.Context, p GlobalChainAppendParams) (*Entry, error) {
	var result *Entry
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		aq := auditdb.New(tx)
		var prev *string
		last, err := aq.GetLastGlobalAuditEntry(ctx)
		switch {
		case err == nil:
			prev = &last.EntryHash
		case errors.Is(err, pgx.ErrNoRows):
			// First entry in the global chain.
		default:
			return fmt.Errorf("audit: read last global entry: %w", err)
		}

		hash, err := ComputeEntryHash(HashInputs{
			RunID:        nil,
			StageID:      nil,
			Timestamp:    p.Timestamp,
			Category:     p.Category,
			ActorKind:    p.ActorKind,
			ActorSubject: p.ActorSubject,
			Payload:      p.Payload,
			PrevHash:     prev,
		})
		if err != nil {
			return err
		}

		var actorKind *string
		if p.ActorKind != nil {
			s := string(*p.ActorKind)
			actorKind = &s
		}
		row, err := aq.AppendAuditEntry(ctx, auditdb.AppendAuditEntryParams{
			ID:           uuid.New(),
			RunID:        nil,
			StageID:      nil,
			Ts:           pgtype.Timestamptz{Time: p.Timestamp, Valid: true},
			Category:     p.Category,
			ActorKind:    actorKind,
			ActorSubject: p.ActorSubject,
			Payload:      []byte(p.Payload),
			PrevHash:     prev,
			EntryHash:    hash,
		})
		if err != nil {
			return fmt.Errorf("audit: append global: %w", err)
		}
		result = rowToEntry(row)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// AppendChained writes an entry inside a transaction that holds a
// row-level lock on runs.id, so concurrent callers can't race on
// reading prev_hash. PrevHash and EntryHash are computed inside this
// function — callers pass logical event details only.
func (r *postgresRepo) AppendChained(ctx context.Context, p ChainAppendParams) (*Entry, error) {
	var result *Entry
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		// SELECT FOR UPDATE on the run serializes chain writes within
		// the run. Concurrent appends to the same run block here
		// until the holder commits; appends to different runs run in
		// parallel.
		rq := rundb.New(tx)
		if _, err := rq.LockRunForUpdate(ctx, p.RunID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("audit: run %s not found", p.RunID)
			}
			return fmt.Errorf("audit: lock run: %w", err)
		}

		// Fetch prev_hash from the run's last entry (if any).
		aq := auditdb.New(tx)
		var prev *string
		runIDPtr := p.RunID
		last, err := aq.GetLastAuditEntryForRun(ctx, &runIDPtr)
		switch {
		case err == nil:
			prev = &last.EntryHash
		case errors.Is(err, pgx.ErrNoRows):
			// First entry in the run; prev_hash stays nil.
		default:
			return fmt.Errorf("audit: read last entry: %w", err)
		}

		runID := p.RunID
		hash, err := ComputeEntryHash(HashInputs{
			RunID:        &runID,
			StageID:      p.StageID,
			Timestamp:    p.Timestamp,
			Category:     p.Category,
			ActorKind:    p.ActorKind,
			ActorSubject: p.ActorSubject,
			Payload:      p.Payload,
			PrevHash:     prev,
		})
		if err != nil {
			return err
		}

		var actorKind *string
		if p.ActorKind != nil {
			s := string(*p.ActorKind)
			actorKind = &s
		}
		row, err := aq.AppendAuditEntry(ctx, auditdb.AppendAuditEntryParams{
			ID:           uuid.New(),
			RunID:        &runID,
			StageID:      p.StageID,
			Ts:           pgtype.Timestamptz{Time: p.Timestamp, Valid: true},
			Category:     p.Category,
			ActorKind:    actorKind,
			ActorSubject: p.ActorSubject,
			Payload:      []byte(p.Payload),
			PrevHash:     prev,
			EntryHash:    hash,
		})
		if err != nil {
			return fmt.Errorf("audit: append: %w", err)
		}
		result = rowToEntry(row)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (r *postgresRepo) Get(ctx context.Context, id uuid.UUID) (*Entry, error) {
	q := auditdb.New(r.pool)
	row, err := q.GetAuditEntry(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get audit entry: %w", err)
	}
	return rowToEntry(row), nil
}

func (r *postgresRepo) ListForRun(ctx context.Context, runID uuid.UUID) ([]*Entry, error) {
	q := auditdb.New(r.pool)
	rows, err := q.ListAuditEntriesForRun(ctx, &runID)
	if err != nil {
		return nil, fmt.Errorf("list audit entries: %w", err)
	}
	out := make([]*Entry, 0, len(rows))
	for _, row := range rows {
		out = append(out, rowToEntry(row))
	}
	return out, nil
}

func (r *postgresRepo) LastForRun(ctx context.Context, runID uuid.UUID) (*Entry, error) {
	q := auditdb.New(r.pool)
	row, err := q.GetLastAuditEntryForRun(ctx, &runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("last audit entry: %w", err)
	}
	return rowToEntry(row), nil
}

func (r *postgresRepo) ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*Entry, error) {
	q := auditdb.New(r.pool)
	rows, err := q.ListAuditEntriesByCategory(ctx, auditdb.ListAuditEntriesByCategoryParams{
		RunID:    &runID,
		Category: category,
	})
	if err != nil {
		return nil, fmt.Errorf("list audit entries by category: %w", err)
	}
	out := make([]*Entry, 0, len(rows))
	for _, row := range rows {
		out = append(out, rowToEntry(row))
	}
	return out, nil
}

func (r *postgresRepo) ListGlobal(ctx context.Context) ([]*Entry, error) {
	q := auditdb.New(r.pool)
	rows, err := q.ListGlobalAuditEntries(ctx)
	if err != nil {
		return nil, fmt.Errorf("list global audit entries: %w", err)
	}
	out := make([]*Entry, 0, len(rows))
	for _, row := range rows {
		out = append(out, rowToEntry(row))
	}
	return out, nil
}

func (r *postgresRepo) ListAll(ctx context.Context, p ListAllParams) ([]*Entry, error) {
	q := auditdb.New(r.pool)
	rows, err := q.ListAuditEntriesAll(ctx, auditdb.ListAuditEntriesAllParams{
		Category: p.Category,
		RunID:    p.RunID,
	})
	if err != nil {
		return nil, fmt.Errorf("list audit entries: %w", err)
	}
	out := make([]*Entry, 0, len(rows))
	for _, row := range rows {
		out = append(out, rowToEntry(row))
	}
	return out, nil
}

func (r *postgresRepo) ChainsByParent(ctx context.Context, parentRunID uuid.UUID, includeDecomposed bool) ([]*Entry, error) {
	q := auditdb.New(r.pool)
	rows, err := q.ListAuditEntriesForRunChain(ctx, auditdb.ListAuditEntriesForRunChainParams{
		ParentRunID:       parentRunID,
		IncludeDecomposed: includeDecomposed,
	})
	if err != nil {
		return nil, fmt.Errorf("chain audit entries: %w", err)
	}
	out := make([]*Entry, 0, len(rows))
	for _, row := range rows {
		out = append(out, rowToEntry(row))
	}
	return out, nil
}

func rowToEntry(r auditdb.AuditEntry) *Entry {
	out := &Entry{
		ID:           r.ID,
		Sequence:     r.Sequence,
		RunID:        r.RunID,
		StageID:      r.StageID,
		Timestamp:    r.Ts.Time,
		Category:     r.Category,
		ActorSubject: r.ActorSubject,
		Payload:      r.Payload,
		PrevHash:     r.PrevHash,
		EntryHash:    r.EntryHash,
	}
	if r.ActorKind != nil {
		ak := ActorKind(*r.ActorKind)
		out.ActorKind = &ak
	}
	return out
}

// Compile-time check that postgresRepo implements Repository.
var _ Repository = (*postgresRepo)(nil)
