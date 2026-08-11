package audit

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	auditdb "github.com/kuhlman-labs/fishhawk/backend/internal/audit/db"
	rundb "github.com/kuhlman-labs/fishhawk/backend/internal/run/db"
)

// MergeVerdictRecordedOnceIndex is the name of the partial unique index
// (migration 0062, #1983) enforcing at most one merge_verdict_recorded audit
// entry per run: CREATE UNIQUE INDEX ... ON audit_entries (run_id) WHERE
// category = 'merge_verdict_recorded'. The merge endpoint scopes its benign
// concurrent-race catch to a collision on THIS index specifically (see
// IsMergeVerdictDuplicate), so an unrelated 23505 stays a hard error.
const MergeVerdictRecordedOnceIndex = "audit_entries_merge_verdict_recorded_once_idx"

// pgUniqueViolation is the PostgreSQL SQLSTATE for a unique_violation.
const pgUniqueViolation = "23505"

// ErrMergeVerdictDuplicate is a sentinel a fake Repository can return from
// AppendChained to simulate losing the concurrent merge-verdict race (the
// deterministic race-loser of the MergeVerdictRecordedOnceIndex collision).
// IsMergeVerdictDuplicate recognizes it alongside a real driver-surfaced
// unique_violation on that index, so the server handler's benign path can be
// exercised without importing pgconn or standing up real Postgres.
var ErrMergeVerdictDuplicate = errors.New("audit: duplicate merge_verdict_recorded entry")

// IsDuplicateOnConstraint reports whether err (anywhere in its chain) is a
// PostgreSQL unique_violation (SQLSTATE 23505) whose ConstraintName equals
// name. AppendChained %w-wraps the driver error, so errors.As reaches the
// underlying *pgconn.PgError. A unique-INDEX violation populates
// PgError.ConstraintName with the index name, which is why matching on the
// index name works for a CREATE UNIQUE INDEX (not just a table constraint).
func IsDuplicateOnConstraint(err error, name string) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == pgUniqueViolation && pgErr.ConstraintName == name
	}
	return false
}

// IsMergeVerdictDuplicate reports whether err is the SPECIFIC benign
// merge-verdict-race collision: a unique_violation on the
// MergeVerdictRecordedOnceIndex partial unique index, or the
// ErrMergeVerdictDuplicate sentinel (for fakes). It deliberately does NOT match
// a 23505 on any OTHER constraint touched by the AppendChained insert (the
// hash-chain / entry-hash / (run_id, sequence) uniqueness): swallowing those
// would treat an unrelated integrity failure as the benign concurrent-merge
// race and wrongly dispatch the merge (binding condition 1, #1983).
func IsMergeVerdictDuplicate(err error) bool {
	return errors.Is(err, ErrMergeVerdictDuplicate) ||
		IsDuplicateOnConstraint(err, MergeVerdictRecordedOnceIndex)
}

// ParentAwaitingChildScopeDecisionOnceIndex is the name of the partial unique
// index (migration 0067, #2594) enforcing at most one
// parent_awaiting_child_scope_decision audit entry per (parent run, child
// stage): CREATE UNIQUE INDEX ... ON audit_entries (run_id,
// (payload->>'child_stage_id')) WHERE category =
// 'parent_awaiting_child_scope_decision'. Both emitters (server's park-time
// emitParentAwaitingChildScopeDecision and orchestrator's sibling-settle
// surfaceParkedChildren) scope their benign concurrent-race catch to a collision
// on THIS index specifically (see IsParentAwaitingChildScopeDecisionDuplicate),
// so an unrelated 23505 stays a hard error.
const ParentAwaitingChildScopeDecisionOnceIndex = "audit_entries_parent_awaiting_child_scope_decision_once_idx"

// ErrParentAwaitingChildScopeDecisionDuplicate is a sentinel a fake Repository
// can return from AppendChained to simulate losing the concurrent
// parent-awaiting-child-scope-decision race (the deterministic race-loser of the
// ParentAwaitingChildScopeDecisionOnceIndex collision).
// IsParentAwaitingChildScopeDecisionDuplicate recognizes it alongside a real
// driver-surfaced unique_violation on that index, so both emitters' benign paths
// can be exercised without importing pgconn or standing up real Postgres.
var ErrParentAwaitingChildScopeDecisionDuplicate = errors.New("audit: duplicate parent_awaiting_child_scope_decision entry")

// IsParentAwaitingChildScopeDecisionDuplicate reports whether err is the
// SPECIFIC benign parent-awaiting-child-scope-decision-race collision: a
// unique_violation on the ParentAwaitingChildScopeDecisionOnceIndex partial
// unique index, or the ErrParentAwaitingChildScopeDecisionDuplicate sentinel
// (for fakes). It deliberately does NOT match a 23505 on any OTHER constraint
// touched by the AppendChained insert (the entry-hash / (run_id, sequence)
// uniqueness): swallowing those would treat an unrelated integrity failure as
// the benign concurrent-park race and silently drop a real error (mirrors the
// #1983 merge-verdict narrowing above).
func IsParentAwaitingChildScopeDecisionDuplicate(err error) bool {
	return errors.Is(err, ErrParentAwaitingChildScopeDecisionDuplicate) ||
		IsDuplicateOnConstraint(err, ParentAwaitingChildScopeDecisionOnceIndex)
}

// ApprovalConditionsTruncatedOnceIndex is the name of the partial unique index
// (migration 0068, #2622) enforcing at most one approval_conditions_truncated
// audit entry per (run, source approval comment): CREATE UNIQUE INDEX ... ON
// audit_entries (run_id, (payload->>'source_entry_id')) WHERE category =
// 'approval_conditions_truncated'. loadApprovalConditions
// (server/prompt.go::appendApprovalConditionsTruncatedAudit) appends this entry
// on every implement-prompt build that loads a legacy over-cap approve comment,
// and prompt construction repeats (retries, prompt-render fetches), so one
// truncation would otherwise accumulate N entries; the emitter scopes its benign
// already-recorded catch to a collision on THIS index specifically (see
// IsApprovalConditionsTruncatedDuplicate), so an unrelated 23505 stays a hard
// error.
const ApprovalConditionsTruncatedOnceIndex = "audit_entries_approval_conditions_truncated_once_idx"

// ErrApprovalConditionsTruncatedDuplicate is a sentinel a fake Repository can
// return from AppendChained to simulate the already-recorded outcome (the
// deterministic loser of the ApprovalConditionsTruncatedOnceIndex collision).
// IsApprovalConditionsTruncatedDuplicate recognizes it alongside a real
// driver-surfaced unique_violation on that index, so the emitter's benign path
// can be exercised without importing pgconn or standing up real Postgres.
var ErrApprovalConditionsTruncatedDuplicate = errors.New("audit: duplicate approval_conditions_truncated entry")

// IsApprovalConditionsTruncatedDuplicate reports whether err is the SPECIFIC
// benign already-recorded collision: a unique_violation on the
// ApprovalConditionsTruncatedOnceIndex partial unique index, or the
// ErrApprovalConditionsTruncatedDuplicate sentinel (for fakes). It deliberately
// does NOT match a 23505 on any OTHER constraint touched by the AppendChained
// insert (the entry-hash / (run_id, sequence) uniqueness): swallowing those
// would treat an unrelated integrity failure as the benign repeated-build case
// and silently drop a real error (mirrors the #1983 merge-verdict and #2594
// parent-awaiting narrowings above).
func IsApprovalConditionsTruncatedDuplicate(err error) bool {
	return errors.Is(err, ErrApprovalConditionsTruncatedDuplicate) ||
		IsDuplicateOnConstraint(err, ApprovalConditionsTruncatedOnceIndex)
}

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
		AccountID:    p.AccountID,
	})
	if err != nil {
		return nil, fmt.Errorf("append audit entry: %w", err)
	}
	return rowToEntry(row), nil
}

// globalChainLockKey derives the pg_advisory_xact_lock key that
// serializes run-less chain appends within one partition (ADR-057 /
// #1828): the first 8 bytes of a domain-separated sha256 over the
// account UUID, or over the domain prefix alone for the untenanted
// NULL partition — a fixed sentinel key. Domain separation keeps the
// key space disjoint from other subsystems' advisory locks (e.g.
// refinement's raw-UUID filing locks); a residual 1-in-2^64 collision
// merely serializes two unrelated writers, never a correctness
// problem.
func globalChainLockKey(accountID *uuid.UUID) int64 {
	h := sha256.New()
	h.Write([]byte("fishhawk:audit:global-chain:"))
	if accountID != nil {
		h.Write(accountID[:])
	}
	sum := h.Sum(nil)
	return int64(binary.BigEndian.Uint64(sum[:8])) //nolint:gosec // deliberate wrap: advisory-lock keys are opaque int64s
}

// AppendGlobalChained writes an entry to the run-less ("global")
// chain (E2.7). Since ADR-057 / #1828 the run-less partition is keyed
// by account_id: PrevHash links the new entry to the previous entry
// WITHIN p.AccountID's partition (nil AccountID = the untenanted
// account_id IS NULL partition), or nil for the partition's genesis
// entry.
//
// There is no run row to SELECT FOR UPDATE here, and read committed
// alone is NOT enough — two concurrent transactions can both read the
// same last entry (or both see an empty partition) before either
// commits, forking the chain (there is no unique constraint to catch
// it). So the append first takes a Postgres advisory TRANSACTION lock
// keyed on the chain partition (globalChainLockKey); a concurrent
// append to the same partition blocks at the lock until the holder
// commits, exactly mirroring the per-run SELECT FOR UPDATE
// serialization. Appends to different partitions run in parallel. The
// xact-scoped lock releases automatically on commit or rollback.
func (r *postgresRepo) AppendGlobalChained(ctx context.Context, p GlobalChainAppendParams) (*Entry, error) {
	var result *Entry
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", globalChainLockKey(p.AccountID)); err != nil {
			return fmt.Errorf("audit: acquire global-chain partition lock: %w", err)
		}

		aq := auditdb.New(tx)
		var prev *string
		var last auditdb.AuditEntry
		var err error
		if p.AccountID != nil {
			last, err = aq.GetLastGlobalAuditEntryForAccount(ctx, p.AccountID)
		} else {
			last, err = aq.GetLastGlobalAuditEntryUntenanted(ctx)
		}
		switch {
		case err == nil:
			prev = &last.EntryHash
		case errors.Is(err, pgx.ErrNoRows):
			// Genesis entry of this partition's chain.
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
			AccountID:    p.AccountID,
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
//
// It is a thin pgx.BeginFunc wrapper delegating to AppendChainedTx;
// the chain-append logic itself lives there so it can be reused inside
// an externally-owned transaction (e.g. the run repo's combined
// resume-and-append, #1090) without duplicating the hashing/locking
// logic. Behavior is unchanged for all existing callers.
func (r *postgresRepo) AppendChained(ctx context.Context, p ChainAppendParams) (*Entry, error) {
	var result *Entry
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		entry, err := AppendChainedTx(ctx, tx, p)
		if err != nil {
			return err
		}
		result = entry
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// AppendChainedTx is the transaction-aware core of AppendChained: it
// performs the per-run chained append against the caller-supplied tx
// (SELECT … FOR UPDATE on the run row to serialize chain writes, read
// prev_hash from the run's last entry, ComputeEntryHash, insert). The
// caller owns the transaction lifecycle, so a run repo can fold this
// append into the SAME transaction as a stage transition and have
// both commit or roll back atomically (#1090). The hashing path is
// identical to AppendChained, so persisted entries are byte-identical.
func AppendChainedTx(ctx context.Context, tx pgx.Tx, p ChainAppendParams) (*Entry, error) {
	// SELECT FOR UPDATE on the run serializes chain writes within
	// the run. Concurrent appends to the same run block here
	// until the holder commits; appends to different runs run in
	// parallel.
	rq := rundb.New(tx)
	if _, err := rq.LockRunForUpdate(ctx, p.RunID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("audit: run %s not found", p.RunID)
		}
		return nil, fmt.Errorf("audit: lock run: %w", err)
	}

	// Stamp the locked run's account on the entry (ADR-057 / #1828).
	// LockRunForUpdate's generated SELECT list predates the 0055
	// account_id column, so the account is read via the dedicated
	// GetRunAccountID query — inside the same tx, with the row lock
	// already held, so the value cannot change under us. Per-run
	// chains stay keyed by run_id alone; the account is a stamp, not
	// a chain key, and stays out of the canonical hash.
	accountID, err := rq.GetRunAccountID(ctx, p.RunID)
	if err != nil {
		return nil, fmt.Errorf("audit: read run account: %w", err)
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
		return nil, fmt.Errorf("audit: read last entry: %w", err)
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
		return nil, err
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
		AccountID:    accountID,
	})
	if err != nil {
		return nil, fmt.Errorf("audit: append: %w", err)
	}
	return rowToEntry(row), nil
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

func (r *postgresRepo) ListGlobalByAccount(ctx context.Context, accountID *uuid.UUID) ([]*Entry, error) {
	q := auditdb.New(r.pool)
	rows, err := q.ListGlobalAuditEntriesByAccount(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("list global audit entries by account: %w", err)
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
		Category:  p.Category,
		RunID:     p.RunID,
		AccountID: accountIDArg(p.AccountID),
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

// accountIDArg maps a ListAllParams.AccountID (empty = no constraint,
// else an account UUID string) to the nullable *uuid.UUID the sqlc filter
// takes (ADR-057 / #1830). A malformed non-empty value maps to nil (no
// constraint) rather than erroring — the handler validates the account
// source (the caller's resolved Identity.AccountID), so this is defensive.
// Same shape as run's accountIDArg (unexported there, so mirrored here).
func accountIDArg(s string) *uuid.UUID {
	if s == "" {
		return nil
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return nil
	}
	return &id
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
		AccountID:    r.AccountID,
	}
	if r.ActorKind != nil {
		ak := ActorKind(*r.ActorKind)
		out.ActorKind = &ak
	}
	return out
}

// Compile-time check that postgresRepo implements Repository.
var _ Repository = (*postgresRepo)(nil)
