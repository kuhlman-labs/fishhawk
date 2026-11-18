package audit

// Atomic guarded append (E80.28 / #5229, ADR-143 Amendment A1).
//
// GuardedChainAppender is the store primitive every waive-proposal state
// write goes through (propose, the lazy drop, confirm's concern_waived intent
// and reject). It is the whole contract for the primitive; the audit README
// section that will restate it is a deferred follow-up, so this comment is
// the authoritative statement until then.
//
// ORDERING (load-bearing, see AppendChainedGuardedTx):
//
//  1. LockRunForUpdate(p.RunID) FIRST, the same run-row lock every run-chain
//     append takes (AppendChainedTx and the anchored, deduped, under-budget
//     and window variants), held for the whole transaction.
//  2. The caller's guard runs AFTER the lock, with a ChainReader bound to the
//     SAME transaction. A non-nil guard return is propagated UNWRAPPED, so the
//     caller can errors.As its own typed refusal, and nothing is written.
//  3. Delegate to AppendChainedTx, whose re-entrant LockRunForUpdate is a
//     no-op inside the same transaction, so the hashing and chaining path is
//     byte-identical to every other chained append.
//
// ISOLATION: the transaction MUST run at the server-default READ COMMITTED
// isolation (no TxOptions). READ COMMITTED takes a fresh snapshot per
// statement, so the guard's reads after the lock is granted observe every
// append that committed before it was granted. A REPEATABLE READ snapshot
// could predate the lock and show the guard a stale chain, which is exactly
// the failure this ordering exists to prevent.
//
// THE GUARANTEE, at its real strength: no entry is persisted when the guard,
// evaluated under the run-row lock against the committed chain, refuses. It
// is not a permanent property of the chain. An append that commits AFTER this
// transaction (a head-recording push, say) is legal and is ordered after the
// guarded entry; that is the same window a direct human waive has today.
//
// READER SCOPING: the tx-bound reader answers only for p.RunID. Asking it for
// any other run returns ErrGuardReaderRunScope rather than reading, so a guard
// cannot reach another run's chain under this run's lock.
//
// FAIL CLOSED: a caller that requires this atomicity MUST refuse when the
// capability is absent (the server answers 503
// waive_proposal_atomic_append_unavailable). There is NO non-atomic fallback:
// unlike AnchoredChainAppender's re-read-then-append path for fakes, the
// waive-proposal guard is a security control (ADR-143 A1, V1).
//
// The capability is kept OFF the Repository interface, mirroring
// AnchoredChainAppender and DedupedChainAppender, so the ~20 hand-written
// full-interface fakes do not break. The decision-index decorator
// (decisionindex.IndexingRepository) forwards it and indexes what it appends:
// it is the ONLY path a proposal-linked concern_waived reaches the chain.

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	auditdb "github.com/kuhlman-labs/fishhawk/backend/internal/audit/db"
	rundb "github.com/kuhlman-labs/fishhawk/backend/internal/run/db"
)

// ChainReader is the read surface an AppendGuard sees. Its one method has the
// Repository method's own signature, so the repository itself satisfies it
// and a caller can run the SAME guard logic as an out-of-lock pre-check
// against the repository and as the in-lock guard against the tx reader.
type ChainReader interface {
	ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*Entry, error)
}

// AppendGuard decides, under the run-row lock, whether a guarded append may
// proceed. read is bound to the append's own transaction. A non-nil return
// refuses the append: nothing is written and the error is returned UNWRAPPED.
type AppendGuard func(ctx context.Context, read ChainReader) error

// GuardedChainAppender is an OPTIONAL capability on the concrete audit
// Repository implementation: an atomic lock-then-guard-then-append in ONE
// transaction (E80.28 / #5229). See this file's header for the full contract.
type GuardedChainAppender interface {
	// AppendChainedGuarded locks the run row, evaluates guard against the
	// committed chain on the same transaction, and appends p only when the
	// guard returns nil. A guard refusal writes nothing and is returned
	// unwrapped. A nil guard is refused (ErrNilAppendGuard): a caller that
	// needs no guard uses AppendChained.
	AppendChainedGuarded(ctx context.Context, p ChainAppendParams, guard AppendGuard) (*Entry, error)
}

// ErrGuardReaderRunScope is returned by the tx-bound ChainReader when a guard
// asks it for a run other than the one whose row lock the transaction holds.
var ErrGuardReaderRunScope = errors.New("audit: guarded append reader is scoped to the locked run")

// ErrNilAppendGuard is returned when AppendChainedGuarded is called with a nil
// guard. A guarded append without a guard would silently be a plain append.
var ErrNilAppendGuard = errors.New("audit: guarded append requires a non-nil guard")

// txChainReader reads a run's chain on the guarded append's own transaction.
type txChainReader struct {
	q     *auditdb.Queries
	runID uuid.UUID
}

// ListForRunByCategory implements ChainReader on the locked transaction. It
// refuses any run other than the locked one.
func (r txChainReader) ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*Entry, error) {
	if runID != r.runID {
		return nil, fmt.Errorf("%w: locked %s, asked for %s", ErrGuardReaderRunScope, r.runID, runID)
	}
	rid := runID
	rows, err := r.q.ListAuditEntriesByCategory(ctx, auditdb.ListAuditEntriesByCategoryParams{
		RunID:    &rid,
		Category: category,
	})
	if err != nil {
		return nil, fmt.Errorf("audit: guarded read %s: %w", category, err)
	}
	out := make([]*Entry, 0, len(rows))
	for _, row := range rows {
		out = append(out, rowToEntry(row))
	}
	return out, nil
}

// AppendChainedGuardedTx is the transaction-aware core of the guarded append.
// The caller owns the transaction lifecycle and MUST run it at READ COMMITTED.
// Ordering is load-bearing: lock, then guard, then AppendChainedTx (see the
// file header).
func AppendChainedGuardedTx(ctx context.Context, tx pgx.Tx, p ChainAppendParams, guard AppendGuard) (*Entry, error) {
	if guard == nil {
		return nil, ErrNilAppendGuard
	}
	// Step 1: lock the run row before the guard reads anything.
	rq := rundb.New(tx)
	if _, err := rq.LockRunForUpdate(ctx, p.RunID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("audit: run %s not found", p.RunID)
		}
		return nil, fmt.Errorf("audit: lock run for guarded append: %w", err)
	}

	// Step 2: the guard, on the same transaction, under the lock. Its error
	// is returned unwrapped so the caller's errors.As on a typed refusal
	// works; the caller's rollback discards nothing because nothing was
	// written.
	if err := guard(ctx, txChainReader{q: auditdb.New(tx), runID: p.RunID}); err != nil {
		return nil, err
	}

	// Step 3: the shared chained-append path.
	return AppendChainedTx(ctx, tx, p)
}

// AppendChainedGuarded implements GuardedChainAppender. It is a thin
// pgx.BeginFunc wrapper over AppendChainedGuardedTx. TxOptions are
// deliberately NOT set: the guard is correct only at the server-default READ
// COMMITTED isolation. A guard refusal rolls the transaction back (nothing was
// written) and propagates as-is.
func (r *postgresRepo) AppendChainedGuarded(ctx context.Context, p ChainAppendParams, guard AppendGuard) (*Entry, error) {
	var result *Entry
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		entry, aerr := AppendChainedGuardedTx(ctx, tx, p, guard)
		if aerr != nil {
			return aerr
		}
		result = entry
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// Compile-time proof the production repository carries the guarded append, so
// a refactor that drops it is a build failure, not a runtime 503.
var _ GuardedChainAppender = (*postgresRepo)(nil)
