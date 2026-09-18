package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	auditdb "github.com/kuhlman-labs/fishhawk/backend/internal/audit/db"
	rundb "github.com/kuhlman-labs/fishhawk/backend/internal/run/db"
)

// DedupeSpec describes the idempotency key an atomic deduped append is
// enforced against: an optional stage scope plus ONE string-typed payload key
// that must equal PayloadValue for an existing entry of the appended category
// to count as a duplicate.
//
// It is authored generically (stage + payload key=value) rather than hard-coded
// to the acceptance_scenario_retirement_dropped writers that drove it (#3439);
// the three of them share one key shape, (stage_id, reason), and are today the
// only consumers.
type DedupeSpec struct {
	// StageID, when non-nil, restricts the duplicate scan to entries carrying
	// EXACTLY this stage_id — an entry with a nil or different stage_id is
	// never a duplicate of a stage-scoped spec. nil scans the whole run.
	StageID *uuid.UUID
	// PayloadKey is the payload key on the APPENDED category that carries the
	// idempotency value (for the retirement-drop writers: "reason").
	PayloadKey string
	// PayloadValue is the JSON STRING that key must hold for an existing entry
	// to be a duplicate of this append.
	PayloadValue string
}

// DedupedDuplicateError is returned when an entry carrying the same
// (stage, PayloadKey=PayloadValue) already exists on the run's chain for the
// appended category. Existing is that committed entry, so the caller can name
// its sequence in a DEBUG line or an already-recorded response. Nothing is
// written when it is returned.
type DedupedDuplicateError struct {
	Existing *Entry
}

func (e *DedupedDuplicateError) Error() string {
	if e.Existing == nil {
		return "audit: deduped append duplicate"
	}
	return fmt.Sprintf("audit: deduped append duplicate (existing entry sequence %d)", e.Existing.Sequence)
}

// DedupedChainAppender is an OPTIONAL capability on the concrete audit
// Repository implementation: an atomic scan-and-append that looks for a prior
// entry carrying the same (stage, payload key=value) UNDER the run-row lock,
// then appends, all in ONE transaction (#3439).
//
// It closes the check-then-act window in the three
// acceptance_scenario_retirement_dropped writers
// (server.recordAcceptanceRetirementsDroppedOnCancel,
// recordAcceptanceScenarioRetirementDropped, recordAcceptanceRetirementsUnserved).
// Before it, each writer listed the category via ListForRunByCategory and
// matched (stage_id, reason) OUTSIDE the append's transaction, so two cancel
// sinks racing on one run (operator cancel + a budget tripwire, or the
// orchestrator's stage_cancelled resolution) could each pass the scan and each
// append, over-reporting one drop as two.
//
// THE GUARANTEE: no second row for the same (run, stage, category, key=value)
// ever commits. That is the full strength of the run-row lock + in-transaction
// scan; there is no store-layer backstop index behind it, deliberately — see
// AppendChainedDedupedTx.
//
// It is kept OFF the Repository interface — mirroring AnchoredChainAppender
// (#2536) and RetryBudgetAppender (#2518) — because adding a method to
// Repository would break the ~20 manually-written full-interface test fakes
// across backend/internal that do not embed a base fake. The server type-asserts
// this capability and drives the atomic path through it when present
// (production postgresRepo), falling back to the prior non-atomic
// list-then-append for in-memory fakes that do not implement it. The
// compile-time assertion in postgres.go keeps a production repo that silently
// loses the capability a build failure, not a runtime degrade.
type DedupedChainAppender interface {
	// AppendChainedDeduped rejects a prior duplicate under the run-row lock,
	// then appends a chained entry — all in one transaction. It returns
	// *DedupedDuplicateError carrying the surviving entry (writing nothing)
	// when one matching spec already exists.
	AppendChainedDeduped(ctx context.Context, p ChainAppendParams, spec DedupeSpec) (*Entry, error)
}

// AppendChainedDedupedTx is the transaction-aware core of the atomic deduped
// append (#3439). Ordering is LOAD-BEARING and mirrors AppendChainedAnchoredTx:
//
//  1. LockRunForUpdate(p.RunID) FIRST — the same run-row lock AppendChainedTx
//     takes, held for the whole transaction.
//  2. Scan the run's p.Category entries on the SAME tx. Because the read runs
//     AFTER the row lock is granted, and Postgres READ COMMITTED takes a fresh
//     snapshot per statement, it observes any competing append that committed
//     before the lock was granted. A row is a duplicate when it carries
//     spec.StageID (when set) AND its payload holds spec.PayloadKey as a JSON
//     STRING equal to spec.PayloadValue → *DedupedDuplicateError, nothing
//     written. A malformed payload, an absent key, or a non-string value is
//     SKIPPED (payloadString — the string-typed mirror of payloadInt64).
//  3. Delegate to AppendChainedTx so the hashing/chaining path is byte-identical
//     to every other chained append (its re-entrant LockRunForUpdate inside the
//     same tx is a harmless no-op).
//
// The caller owns the transaction lifecycle. The transaction MUST run at the
// server-default READ COMMITTED isolation (do NOT set TxOptions): a REPEATABLE
// READ snapshot would predate the lock and could observe the stale pre-append
// state, which is exactly the failure this ordering exists to prevent.
//
// NO BACKSTOP UNIQUE INDEX, deliberately. The #2536 IndexDropped test proves the
// lock + in-transaction scan closes the window standing alone, so an index adds
// no guarantee here. A partial unique index on (run_id, stage_id,
// payload->>'reason') would also FAIL TO BUILD on a production chain that
// already carries a raced duplicate — the race was live before this landed —
// and audit rows cannot be deleted to repair it. The failure direction of a
// missed duplicate is a duplicate over-report on the status comment, not a
// lost record, so the migration cost is not justified.
func AppendChainedDedupedTx(ctx context.Context, tx pgx.Tx, p ChainAppendParams, spec DedupeSpec) (*Entry, error) {
	// Step 1: lock the run row before reading anything. Everything below
	// observes a consistent, serialized view of this run's chain.
	rq := rundb.New(tx)
	if _, err := rq.LockRunForUpdate(ctx, p.RunID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("audit: run %s not found", p.RunID)
		}
		return nil, fmt.Errorf("audit: lock run for deduped append: %w", err)
	}

	// Step 2: in-transaction dedupe scan.
	aq := auditdb.New(tx)
	runIDPtr := p.RunID
	existing, err := aq.ListAuditEntriesByCategory(ctx, auditdb.ListAuditEntriesByCategoryParams{
		RunID:    &runIDPtr,
		Category: p.Category,
	})
	if err != nil {
		return nil, fmt.Errorf("audit: scan deduped duplicates: %w", err)
	}
	for i := range existing {
		row := existing[i]
		if spec.StageID != nil && (row.StageID == nil || *row.StageID != *spec.StageID) {
			continue
		}
		if v, ok := payloadString(row.Payload, spec.PayloadKey); ok && v == spec.PayloadValue {
			return nil, &DedupedDuplicateError{Existing: rowToEntry(row)}
		}
	}

	// Step 3: delegate to the shared chained-append path.
	return AppendChainedTx(ctx, tx, p)
}

// payloadString decodes key out of a JSON object payload as a string. It
// returns ok=false for an absent key, a malformed payload, or a value of any
// other JSON type (a number, null, an object) — the string-typed mirror of
// payloadInt64, with the same skip-not-fail posture: an unusable row is not a
// duplicate, so the append proceeds.
func payloadString(payload []byte, key string) (string, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return "", false
	}
	raw, ok := fields[key]
	if !ok {
		return "", false
	}
	var v *string
	if err := json.Unmarshal(raw, &v); err != nil || v == nil {
		return "", false
	}
	return *v, true
}
