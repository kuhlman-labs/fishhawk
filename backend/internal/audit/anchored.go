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

// AcceptanceTriageArbitratedOnceIndex is the name of the partial unique index
// (migration 0080, #2536) enforcing at most one acceptance_triage_arbitrated
// audit entry per (run, discharged acceptance outcome): CREATE UNIQUE INDEX ...
// ON audit_entries (run_id, (payload->>'outcome_sequence')) WHERE category =
// 'acceptance_triage_arbitrated'.
//
// It is a store-layer BACKSTOP, not the primary control — see
// AppendChainedAnchoredTx, whose run-row lock + in-transaction scan is what
// actually closes the check-to-append window. The narrowing helper
// IsAcceptanceArbitrationDuplicate scopes the benign-collision catch to a 23505
// on THIS index specifically, so an unrelated 23505 stays a hard error.
const AcceptanceTriageArbitratedOnceIndex = "audit_entries_acceptance_triage_arbitrated_once_idx"

// ErrAcceptanceArbitrationDuplicate is a sentinel a fake Repository can return
// from AppendChained to simulate the already-recorded outcome (the deterministic
// loser of the AcceptanceTriageArbitratedOnceIndex collision).
// IsAcceptanceArbitrationDuplicate recognizes it alongside a real
// driver-surfaced unique_violation on that index, so a caller's benign path can
// be exercised without importing pgconn or standing up real Postgres.
var ErrAcceptanceArbitrationDuplicate = errors.New("audit: duplicate acceptance_triage_arbitrated entry")

// IsAcceptanceArbitrationDuplicate reports whether err is the SPECIFIC benign
// already-recorded collision: a unique_violation on the
// AcceptanceTriageArbitratedOnceIndex partial unique index, or the
// ErrAcceptanceArbitrationDuplicate sentinel (for fakes). It deliberately does
// NOT match a 23505 on any OTHER constraint touched by the AppendChained insert
// (the entry-hash / (run_id, sequence) uniqueness): swallowing those would treat
// an unrelated integrity failure as the benign repeat-POST case and silently
// drop a real error (mirrors the #1983 merge-verdict, #2594 parent-awaiting and
// #2622 approval-truncated narrowings in postgres.go).
func IsAcceptanceArbitrationDuplicate(err error) bool {
	return errors.Is(err, ErrAcceptanceArbitrationDuplicate) ||
		IsDuplicateOnConstraint(err, AcceptanceTriageArbitratedOnceIndex)
}

// AnchorSpec describes the ANCHOR an anchored append is bound to, and the
// payload key that makes the append idempotent against that anchor.
//
// It is authored generically (anchor category + sequence + dedupe payload key)
// rather than hard-coded to acceptance arbitration. #2178 was the same shape but
// is CLOSED as NOT_PLANNED, so there is no second consumer today: the genericity
// is speculative-but-cheap, not a live coordination.
type AnchorSpec struct {
	// AnchorCategory is the audit category whose NEWEST entry the append is
	// bound to (for acceptance arbitration: acceptance_outcome_recorded).
	AnchorCategory string
	// AnchorSequence is the sequence the caller's guards evaluated. The append
	// refuses when the newest AnchorCategory entry no longer carries it.
	AnchorSequence int64
	// DedupePayloadKey is the payload key on the APPENDED category that binds
	// an entry to its anchor (for acceptance arbitration: "outcome_sequence").
	DedupePayloadKey string
	// DedupeValue is the value that key must hold for an existing entry to be
	// a duplicate of this append.
	DedupeValue int64
	// ConstraintName is the store-layer backstop index whose 23505 the repo
	// wrapper treats as a benign duplicate (for acceptance arbitration:
	// AcceptanceTriageArbitratedOnceIndex).
	ConstraintName string
}

// AnchorMovedError is returned when the newest AnchorCategory entry no longer
// carries AnchorSpec.AnchorSequence at append time — a newer anchor committed
// between the caller's guard evaluation and the lock being granted. Nothing is
// written. Recorded is false when NO anchor entry exists at all.
type AnchorMovedError struct {
	Expected int64
	Current  int64
	Recorded bool
}

func (e *AnchorMovedError) Error() string {
	if !e.Recorded {
		return fmt.Sprintf("audit: anchor moved: expected sequence %d, no anchor entry recorded", e.Expected)
	}
	return fmt.Sprintf("audit: anchor moved: expected sequence %d, current sequence %d", e.Expected, e.Current)
}

// AnchoredDuplicateError is returned when an entry bound to the SAME anchor
// already exists. Existing is that committed entry, so the caller can report its
// sequence in an idempotent already-recorded response. It is returned from BOTH
// duplicate paths — the in-transaction dedupe scan and the out-of-transaction
// recovery after a backstop-index 23505 — so the caller has ONE duplicate
// branch.
type AnchoredDuplicateError struct {
	Existing *Entry
}

func (e *AnchoredDuplicateError) Error() string {
	if e.Existing == nil {
		return "audit: anchored append duplicate"
	}
	return fmt.Sprintf("audit: anchored append duplicate (existing entry sequence %d)", e.Existing.Sequence)
}

// AnchoredChainAppender is an OPTIONAL capability on the concrete audit
// Repository implementation: an atomic anchor-revalidate-and-append that
// re-reads the anchor AND scans for a prior duplicate UNDER the run-row lock,
// then appends, all in ONE transaction (#2536).
//
// It closes the acceptance-arbitration endpoint's check-to-append window. Before
// it, guard 7 re-read the newest acceptance outcome immediately before
// AppendChained and the idempotence scan ran several reads earlier — both
// OUTSIDE the append's transaction — so (mode 1) two concurrent valid POSTs
// could each pass and each append, and (mode 2) a newer acceptance outcome could
// commit between the final re-read and the append, persisting an arbitration
// that named an already-superseded outcome while returning 200.
//
// THE GUARANTEE, stated at the strength the mechanism delivers: no entry is ever
// persisted that named an ALREADY-SUPERSEDED anchor AT APPEND TIME. It is NOT a
// permanent-newest property. Anchored-append-commits-then-newer-anchor-lands is
// a legal, unpreventable interleaving and is NOT a defect: the entry was valid
// when written, the newer anchor supersedes it, and the authoritative gate
// (which requires the entry to name the NEWEST anchor) re-wedges — fail-closed.
//
// It is deliberately kept OFF the Repository interface — mirroring
// RetryBudgetAppender (#2518) and run.StageCASTransitioner — because adding a
// method to Repository would break the ~20 manually-written full-interface test
// fakes across backend/internal that do not embed a base fake. The server
// type-asserts this capability and drives the atomic path through it when
// present (production postgresRepo), falling back to the prior non-atomic
// re-read-then-append for in-memory fakes that do not implement it. The
// compile-time assertion in postgres.go keeps a production repo that silently
// loses the capability a build failure, not a runtime degrade.
type AnchoredChainAppender interface {
	// AppendChainedAnchored re-validates spec's anchor and rejects a prior
	// duplicate under the run-row lock, then appends a chained entry — all in
	// one transaction. It returns *AnchorMovedError (writing nothing) when the
	// anchor moved, and *AnchoredDuplicateError carrying the surviving entry
	// when one bound to the same anchor already exists.
	AppendChainedAnchored(ctx context.Context, p ChainAppendParams, spec AnchorSpec) (*Entry, error)
}

// AppendChainedAnchoredTx is the transaction-aware core of the atomic anchored
// append (#2536). Ordering is LOAD-BEARING and mirrors
// AppendChainedUnderBudgetTx:
//
//  1. LockRunForUpdate(p.RunID) FIRST — the same run-row lock AppendChainedTx
//     takes, held for the whole transaction.
//  2. Re-read the newest spec.AnchorCategory entry. Because the read runs AFTER
//     the row lock is granted, and Postgres READ COMMITTED takes a fresh
//     snapshot per statement, it observes any competing anchor append that
//     committed before the lock was granted. Anchor moved (or none recorded) →
//     *AnchorMovedError, nothing written.
//  3. Dedupe scan on the same tx: an existing p.Category entry already carrying
//     spec.DedupePayloadKey == spec.DedupeValue → *AnchoredDuplicateError,
//     nothing written.
//  4. Delegate to AppendChainedTx so the hashing/chaining path is byte-identical
//     to every other chained append (its re-entrant LockRunForUpdate inside the
//     same tx is a harmless no-op).
//
// The caller owns the transaction lifecycle. The transaction MUST run at the
// server-default READ COMMITTED isolation (do NOT set TxOptions): a REPEATABLE
// READ snapshot would predate the lock and could observe the stale pre-append
// state, which is exactly the failure this ordering exists to prevent.
//
// DECODE ASYMMETRY, flagged deliberately: step 3 decodes the dedupe key into an
// *int64, so a payload writing that key as a JSON STRING ("7") is MISSED here
// while the backstop index — keyed on the TEXT projection payload->>'key' —
// still collides on it. That divergence is why the repo wrapper's post-rollback
// recovery matches on the INDEX's own TEXT semantics in SQL rather than
// re-running this typed decode: only the text rule is guaranteed to find
// whatever row the index actually collided with. No writer emits a string-typed
// sequence today.
func AppendChainedAnchoredTx(ctx context.Context, tx pgx.Tx, p ChainAppendParams, spec AnchorSpec) (*Entry, error) {
	// Step 1: lock the run row before reading anything. Everything below
	// observes a consistent, serialized view of this run's chain.
	rq := rundb.New(tx)
	if _, err := rq.LockRunForUpdate(ctx, p.RunID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("audit: run %s not found", p.RunID)
		}
		return nil, fmt.Errorf("audit: lock run for anchored append: %w", err)
	}

	aq := auditdb.New(tx)
	runIDPtr := p.RunID

	// Step 2: re-read the anchor AFTER the lock.
	anchors, err := aq.ListAuditEntriesByCategory(ctx, auditdb.ListAuditEntriesByCategoryParams{
		RunID:    &runIDPtr,
		Category: spec.AnchorCategory,
	})
	if err != nil {
		return nil, fmt.Errorf("audit: re-read anchor category: %w", err)
	}
	var newest int64
	var recorded bool
	for _, a := range anchors {
		if !recorded || a.Sequence > newest {
			newest, recorded = a.Sequence, true
		}
	}
	if !recorded || newest != spec.AnchorSequence {
		return nil, &AnchorMovedError{Expected: spec.AnchorSequence, Current: newest, Recorded: recorded}
	}

	// Step 3: in-transaction dedupe scan.
	existing, err := aq.ListAuditEntriesByCategory(ctx, auditdb.ListAuditEntriesByCategoryParams{
		RunID:    &runIDPtr,
		Category: p.Category,
	})
	if err != nil {
		return nil, fmt.Errorf("audit: scan anchored duplicates: %w", err)
	}
	for i := range existing {
		if v, ok := payloadInt64(existing[i].Payload, spec.DedupePayloadKey); ok && v == spec.DedupeValue {
			row := existing[i]
			return nil, &AnchoredDuplicateError{Existing: rowToEntry(row)}
		}
	}

	// Step 4: delegate to the shared chained-append path.
	return AppendChainedTx(ctx, tx, p)
}

// payloadInt64 decodes key out of a JSON object payload as an int64. It returns
// ok=false for an absent key, a malformed payload, or a value of any other JSON
// type — including a STRING-typed number, which the backstop index's TEXT
// projection nonetheless treats as the same key (see AppendChainedAnchoredTx's
// decode-asymmetry note). Mirrors server.arbitrationOutcomeSequence's shape.
func payloadInt64(payload []byte, key string) (int64, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return 0, false
	}
	raw, ok := fields[key]
	if !ok {
		return 0, false
	}
	var v *int64
	if err := json.Unmarshal(raw, &v); err != nil || v == nil {
		return 0, false
	}
	return *v, true
}
