// Package auditrehash rewrites every audit_entries row's
// entry_hash + prev_hash using the canonical hash algorithm from
// #302. One-shot data migration: deploy the new
// audit.ComputeEntryHash, run RehashAllChains once, then every
// Fishhawk-managed PR's `fishhawk_audit_complete` check stops
// false-failing on chain_invalid for entries written before the
// algorithm change.
//
// Since ADR-057 / #1828 the run-less (run_id IS NULL) rows are
// chained PER account_id partition (nil = the untenanted legacy
// partition), so the same one-shot doubles as the per-account
// RE-ANCHOR: it walks each account's run-less partition as its own
// chain and rewrites prev_hash + entry_hash so every partition
// links from a nil-prev_hash genesis. Idempotent — a corpus whose
// partitions already chain correctly reports EntriesChanged == 0.
// Rollout ordering: backfill account_id onto historical run-less
// rows first, then run the re-anchor, then rely on per-account
// verification; until the backfill lands every historical row sits
// in the untenanted partition and the re-anchor is a no-op.
//
// Why a separate package: the rehash needs the audit package's
// ComputeEntryHash + the pgx pool, but it's not part of the
// regular request hot-path. Keeping it here keeps cmd/fishhawkd
// thin (flag parsing + wiring) and lets the heavy lifting live
// somewhere that can be testcontainers-tested without dragging in
// the rest of the command.
package auditrehash

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
)

// Summary aggregates per-run counters for the rehash report.
type Summary struct {
	// Chains is the number of distinct chains walked: one per run
	// plus one per run-less account partition (run_id NULL grouped
	// by account_id, the NULL account being the untenanted legacy
	// partition — ADR-057 / #1828).
	Chains int
	// EntriesTotal is the number of rows the walker visited.
	EntriesTotal int
	// EntriesChanged is the number of rows whose recomputed hash
	// differed from the stored value — the actually-rehashed count.
	// On an already-canonical chain this is zero (idempotent).
	EntriesChanged int
}

// DB narrows pgxpool.Pool to the methods RehashAllChains needs so
// tests can stub it without spinning up a pool.
type DB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Begin(ctx context.Context) (pgx.Tx, error)
}

// RehashAllChains finds every run_id with at least one audit row +
// every run-less account partition (run_id NULL, segmented by
// account_id per ADR-057 / #1828), and rehashes each in turn.
//
// The implementation walks chains, not individual rows: rebuilding
// a chain requires processing entries in sequence order and
// threading the new prev_hash forward, so per-row parallelism
// would corrupt the link integrity.
//
// Everything runs in one transaction: the append-only triggers on
// audit_entries (migration 0002) refuse UPDATE/DELETE under any
// role, so the rehash temporarily disables them inside the tx. If
// the tx commits, the disable/enable pair are durable; if it
// aborts, both roll back and the table stays append-only — the
// integrity story holds at every visible boundary.
//
// dryRun=true reports the per-chain summary without writing
// (the tx is always rolled back).
func RehashAllChains(ctx context.Context, db DB, dryRun bool) (Summary, error) {
	var summary Summary

	tx, err := db.Begin(ctx)
	if err != nil {
		return summary, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Disable the append-only enforcement just inside this tx.
	// ALTER TABLE inside a transaction rolls back on abort, so the
	// triggers come back if anything below fails. The migration role
	// must own audit_entries; in production deploys, the rehash
	// runs under the same role that ran the schema migrations.
	if _, err := tx.Exec(ctx,
		`ALTER TABLE audit_entries DISABLE TRIGGER audit_entries_no_update`); err != nil {
		return summary, fmt.Errorf("disable append-only trigger: %w", err)
	}

	// Per-run chains. ORDER BY id keeps the report deterministic so
	// the same input produces the same log output.
	rows, err := tx.Query(ctx,
		`SELECT DISTINCT run_id FROM audit_entries WHERE run_id IS NOT NULL ORDER BY run_id`)
	if err != nil {
		return summary, fmt.Errorf("list run chains: %w", err)
	}
	var runIDs []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return summary, fmt.Errorf("scan run id: %w", err)
		}
		runIDs = append(runIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return summary, fmt.Errorf("iterate run chains: %w", err)
	}

	for _, id := range runIDs {
		visited, changed, err := rehashChain(ctx, tx, whereRunChain, &id, dryRun)
		if err != nil {
			return summary, fmt.Errorf("run chain %s: %w", id, err)
		}
		summary.Chains++
		summary.EntriesTotal += visited
		summary.EntriesChanged += changed
	}

	// Run-less partitions (run_id NULL), one chain PER account_id
	// (ADR-057 / #1828). Enumerate the distinct partitions present —
	// including the NULL (untenanted) one — and re-anchor each as an
	// independent chain from a nil-prev_hash genesis. NULLS FIRST
	// keeps the report order deterministic across Postgres defaults.
	accountIDs, err := listRunlessPartitions(ctx, tx)
	if err != nil {
		return summary, err
	}
	for _, acct := range accountIDs {
		visited, changed, err := rehashChain(ctx, tx, whereRunlessPartition, acct, dryRun)
		if err != nil {
			return summary, fmt.Errorf("run-less partition %s: %w", partitionLabel(acct), err)
		}
		summary.Chains++
		summary.EntriesTotal += visited
		summary.EntriesChanged += changed
	}

	// Re-enable the append-only enforcement before commit so the
	// post-rehash state matches the pre-rehash invariant.
	if _, err := tx.Exec(ctx,
		`ALTER TABLE audit_entries ENABLE TRIGGER audit_entries_no_update`); err != nil {
		return summary, fmt.Errorf("re-enable append-only trigger: %w", err)
	}

	if dryRun {
		// Roll back via the deferred Rollback. ALTER TABLE +
		// UPDATEs both unwind; trigger comes back on its own.
		return summary, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return summary, fmt.Errorf("commit: %w", err)
	}
	return summary, nil
}

// Chain-selection predicates for rehashChain. Both take one
// nullable uuid argument.
const (
	// whereRunChain selects one per-run chain.
	whereRunChain = `run_id = $1`
	// whereRunlessPartition selects one run-less account partition
	// (ADR-057 / #1828): the given account's rows, or the untenanted
	// account_id IS NULL rows when the argument is NULL.
	whereRunlessPartition = `run_id IS NULL AND (($1::uuid IS NULL AND account_id IS NULL) OR account_id = $1)`
)

// listRunlessPartitions enumerates the distinct account_id values
// present among run-less rows — each is one chain partition; a nil
// element is the untenanted partition.
func listRunlessPartitions(ctx context.Context, tx pgx.Tx) ([]*uuid.UUID, error) {
	rows, err := tx.Query(ctx,
		`SELECT DISTINCT account_id FROM audit_entries WHERE run_id IS NULL ORDER BY account_id NULLS FIRST`)
	if err != nil {
		return nil, fmt.Errorf("list run-less partitions: %w", err)
	}
	defer rows.Close()
	var out []*uuid.UUID
	for rows.Next() {
		var id *uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan run-less partition: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate run-less partitions: %w", err)
	}
	return out, nil
}

// partitionLabel names a run-less partition for error reports.
func partitionLabel(accountID *uuid.UUID) string {
	if accountID == nil {
		return "untenanted"
	}
	return accountID.String()
}

// rehashChain walks the entries belonging to one chain — a per-run
// chain (whereRunChain) or one run-less account partition
// (whereRunlessPartition) — in sequence order, computes each
// entry's new canonical hash, and updates entry_hash + prev_hash
// so the chain links to the new predecessor.
//
// The caller owns the surrounding transaction; rehashChain just
// runs the SELECT + UPDATEs against it. Idempotent on already-
// canonical chains: every recomputed hash matches what's stored,
// so no UPDATE fires.
func rehashChain(ctx context.Context, tx pgx.Tx, where string, arg *uuid.UUID, dryRun bool) (visited, changed int, err error) {
	selectChain := `
		SELECT id, run_id, stage_id, ts, category, actor_kind, actor_subject, payload, prev_hash, entry_hash
		FROM audit_entries
		WHERE ` + where + `
		ORDER BY sequence ASC`
	rows, err := tx.Query(ctx, selectChain, arg)
	if err != nil {
		return 0, 0, fmt.Errorf("select chain: %w", err)
	}

	type rowFields struct {
		id           uuid.UUID
		runID        *uuid.UUID
		stageID      *uuid.UUID
		ts           time.Time
		category     string
		actorKind    *string
		actorSubject *string
		payload      []byte
		oldPrevHash  *string
		oldEntryHash string
	}
	var entries []rowFields
	for rows.Next() {
		var r rowFields
		if err := rows.Scan(&r.id, &r.runID, &r.stageID, &r.ts, &r.category,
			&r.actorKind, &r.actorSubject, &r.payload, &r.oldPrevHash, &r.oldEntryHash); err != nil {
			rows.Close()
			return 0, 0, fmt.Errorf("scan row: %w", err)
		}
		entries = append(entries, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("iterate chain: %w", err)
	}

	var prevNewHash *string
	for _, r := range entries {
		visited++
		var kind *audit.ActorKind
		if r.actorKind != nil {
			k := audit.ActorKind(*r.actorKind)
			kind = &k
		}
		newHash, err := audit.ComputeEntryHash(audit.HashInputs{
			RunID:        r.runID,
			StageID:      r.stageID,
			Timestamp:    r.ts,
			Category:     r.category,
			ActorKind:    kind,
			ActorSubject: r.actorSubject,
			Payload:      r.payload,
			PrevHash:     prevNewHash,
		})
		if err != nil {
			return 0, 0, fmt.Errorf("compute entry %s: %w", r.id, err)
		}
		// Three things may differ from the stored row:
		// 1. The recomputed entry_hash (when the stored value was
		//    computed with a non-canonical algorithm).
		// 2. The stored prev_hash (now points to the new predecessor).
		// 3. Both, when the predecessor's hash also moved.
		// The combined "row would change" check covers all three.
		samePrev := ptrStringsEqual(prevNewHash, r.oldPrevHash)
		sameEntry := newHash == r.oldEntryHash
		if samePrev && sameEntry {
			// Already canonical for this row; advance the cursor and
			// continue without an UPDATE.
			cur := newHash
			prevNewHash = &cur
			continue
		}
		changed++
		if !dryRun {
			if _, err := tx.Exec(ctx,
				`UPDATE audit_entries SET prev_hash = $1, entry_hash = $2 WHERE id = $3`,
				prevNewHash, newHash, r.id); err != nil {
				return 0, 0, fmt.Errorf("update entry %s: %w", r.id, err)
			}
		}
		cur := newHash
		prevNewHash = &cur
	}

	return visited, changed, nil
}

// ptrStringsEqual returns true when both pointers are nil OR both
// are non-nil and point at equal strings. Used to decide whether a
// row's prev_hash actually shifted.
func ptrStringsEqual(a, b *string) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}
