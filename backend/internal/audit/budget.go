package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	auditdb "github.com/kuhlman-labs/fishhawk/backend/internal/audit/db"
	rundb "github.com/kuhlman-labs/fishhawk/backend/internal/run/db"
)

// ErrRetryBudgetExhausted is returned by AppendChainedUnderBudget /
// AppendChainedUnderBudgetTx when the run already holds maxEntries entries of
// the requested category — the in-run retry budget is spent, so no new entry
// is written. It is a sentinel the server helper distinguishes from a genuine
// read/write failure: exhaustion is the ordinary bound (log at info, decline
// the retry), any other error is an anomaly (log at warn, decline the retry).
var ErrRetryBudgetExhausted = errors.New("audit: retry budget exhausted")

// RetryBudgetAppender is an OPTIONAL capability on the concrete audit
// Repository implementation: an atomic check-and-consume append that counts a
// run's existing entries of one category UNDER a row lock and appends a fresh
// chained entry only when the count is below maxEntries. It makes the
// plan-retry budget check-and-consume atomic across concurrent ships (#2518) —
// the count and the append happen in ONE transaction, so two ships racing on
// the same (run, category) can no longer both read a below-budget count and
// both consume the budget.
//
// It is deliberately kept OFF the Repository interface — mirroring the
// run.StageCASTransitioner / ResumeAwaitingInputAndAppend precedent — because
// adding a method to Repository would break the ~20 manually-written
// full-interface test fakes across backend/internal that do not embed a base
// fake. The server type-asserts this capability and drives the atomic path
// through it when present (production postgresRepo), falling back to a
// non-atomic count-then-append for in-memory fakes that do not implement it.
// The compile-time assertion in postgres.go keeps a production repo that
// silently loses the capability a build failure, not a runtime degrade.
type RetryBudgetAppender interface {
	// AppendChainedUnderBudget counts p.RunID's existing entries of
	// p.Category and, when that count is below maxEntries, appends a fresh
	// chained entry whose payload is built by stamp(attempt) with attempt =
	// count+1 (a truthful 1-based attempt number). It returns
	// ErrRetryBudgetExhausted (and writes nothing) when the budget is spent,
	// and the stamp function's error (writing nothing) when the payload cannot
	// be built. The whole check-and-consume is atomic: the count runs after a
	// row lock on the run, so a concurrent caller observes this call's
	// committed entry.
	AppendChainedUnderBudget(ctx context.Context, p ChainAppendParams, maxEntries int, stamp func(attempt int) (json.RawMessage, error)) (*Entry, error)
}

// AppendChainedUnderBudgetTx is the transaction-aware core of the atomic
// check-and-consume append (#2518). Ordering is LOAD-BEARING:
//
//  1. LockRunForUpdate(p.RunID) FIRST — the same run-row lock AppendChainedTx
//     takes, held for the whole transaction.
//  2. Count p.Category's existing entries for the run. Because the count runs
//     AFTER the row lock is granted, and Postgres READ COMMITTED takes a fresh
//     snapshot per statement, a second transaction's count observes the first
//     transaction's committed entry — so two racing ships cannot both read a
//     below-budget count. If the count were taken BEFORE the lock (or under
//     REPEATABLE READ, whose snapshot predates the lock), both could read the
//     stale pre-append count and both consume the budget.
//  3. Exhaustion check: count >= maxEntries → ErrRetryBudgetExhausted, nothing
//     written.
//  4. stamp(count+1) builds the payload with the truthful attempt number.
//  5. Delegate to AppendChainedTx so the hashing/chaining path is
//     byte-identical to every other chained append (AppendChainedTx re-locks
//     the same run row inside the same transaction — a harmless re-entrant
//     no-op).
//
// The caller owns the transaction lifecycle. The transaction MUST run at the
// server-default READ COMMITTED isolation (do not set TxOptions) — see the
// ordering note above.
func AppendChainedUnderBudgetTx(ctx context.Context, tx pgx.Tx, p ChainAppendParams, maxEntries int, stamp func(attempt int) (json.RawMessage, error)) (*Entry, error) {
	// Step 1: lock the run row before counting. Everything below observes a
	// consistent, serialized view of this run's chain.
	rq := rundb.New(tx)
	if _, err := rq.LockRunForUpdate(ctx, p.RunID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("audit: run %s not found", p.RunID)
		}
		return nil, fmt.Errorf("audit: lock run for budget: %w", err)
	}

	// Step 2: count this category's entries. Runs AFTER the lock, so a racing
	// transaction's count sees this one's committed entry once it commits.
	runIDPtr := p.RunID
	existing, err := auditdb.New(tx).ListAuditEntriesByCategory(ctx, auditdb.ListAuditEntriesByCategoryParams{
		RunID:    &runIDPtr,
		Category: p.Category,
	})
	if err != nil {
		return nil, fmt.Errorf("audit: count budget category: %w", err)
	}

	// Step 3: exhaustion check.
	count := len(existing)
	if count >= maxEntries {
		return nil, ErrRetryBudgetExhausted
	}

	// Step 4: build the payload with the truthful 1-based attempt number.
	payload, err := stamp(count + 1)
	if err != nil {
		return nil, fmt.Errorf("audit: stamp budget payload: %w", err)
	}
	p.Payload = payload

	// Step 5: delegate to the shared chained-append path.
	return AppendChainedTx(ctx, tx, p)
}
