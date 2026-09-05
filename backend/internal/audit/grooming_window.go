package audit

// grooming_window.go carries the grooming capture/apply CONCURRENCY PROTOCOL
// (E54.48 / #2991): the audit-layer capability that makes it safe for the
// on-approval apply hook to CONSUME the per-entry dispositions #2843 captures.
//
// THE PROTOCOL, and the three properties it delivers SIMULTANEOUSLY:
//
//   - ONE CAPTURE IS ONE TRANSACTION. AppendChainedGroomingDispositionBatch
//     appends a whole capture batch under the run-row lock after checking for an
//     artifact-bound closing WATERMARK. A watermark cannot land between two rows
//     of one capture, and a mid-batch failure rolls the WHOLE batch back rather
//     than leaving durable partial rows.
//   - THE FIRST WATERMARK IS PERMANENT. AppendChainedGroomingWindowClose scans
//     for an existing artifact-bound watermark and, if one exists, returns it
//     UNCHANGED — a repeated settlement can never extend the consumption bound,
//     so rows outside the first window stay outside it forever.
//   - REJECTION SETTLES AS DECISIVELY AS APPROVAL. The settlement side reads the
//     dispositions and appends the watermark in ONE transaction whatever the
//     settlement string, so the capture/apply TOCTOU is closed on both paths.
//
// Both cores take LockRunForUpdate(RunID) FIRST, then read under the lock, then
// write — all in ONE transaction at the server-default READ COMMITTED isolation
// (TxOptions deliberately unset: a REPEATABLE READ snapshot would predate the
// lock and could observe stale pre-append state, the residual anchored.go
// documents). This is the SAME mechanism AppendChainedTx / AppendChainedAnchoredTx
// / AppendChainedUnderBudgetTx already depend on.
//
// CONDITION 1 (the consumed set is artifact-scoped): the settlement's consumed
// set is always {dispositions recorded against THIS artifact, below THIS
// artifact's watermark}. A run can carry MULTIPLE grooming-report artifacts, so
// a stale disposition captured against a DIFFERENT artifact must not enter the
// consumed set and match an entry id — the same wrong-decision-applied failure
// the whole protocol exists to prevent. Both the first-settlement and the
// permanence paths apply the artifact + below-watermark filter.
//
// The capability is kept OFF the Repository interface (the anchored.go /
// RetryBudgetAppender precedent): adding a Repository method would break the ~20
// manually-written full-interface fakes. The server type-asserts it and drives
// the atomic path when present (production postgresRepo), falling back to a
// non-atomic read-then-append for in-memory fakes. The compile-time assertion in
// postgres.go keeps a production repo that silently loses the capability a build
// failure, not a runtime degrade.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	auditdb "github.com/kuhlman-labs/fishhawk/backend/internal/audit/db"
	rundb "github.com/kuhlman-labs/fishhawk/backend/internal/run/db"
)

// GroomingApplyWindowClosedCategory is the audit category for the WATERMARK the
// capture/apply concurrency protocol appends at settlement (#2991). It closes
// the disposition-capture window for ONE grooming-report artifact: after it
// lands, a capture arriving for that artifact is refused, and the settlement
// consumes exactly the dispositions recorded below it against that artifact. It
// is registered in KnownCategories (categories.go) so an operator can await it
// and GET /v0/runs/{id}/audit?category= can read it.
const GroomingApplyWindowClosedCategory = "grooming_apply_window_closed"

// GroomingDispositionRecordedCategory is the audit category one per-entry
// disposition an operator records against a grooming-report entry lands under
// (#2843). It is DEFINED here — beside the protocol that consumes it — so the
// audit-layer settlement scan and the server capture handler share ONE source
// of truth (server.CategoryGroomingDispositionRecorded aliases it). Registered
// in KnownCategories.
const GroomingDispositionRecordedCategory = "grooming_disposition_recorded"

// GroomingWindowClosedError is returned when a capture arrives for an artifact
// whose window has already been settled — nothing is written. Settlement is the
// settlement string of the closing watermark (approved / rejected), Sequence its
// chain sequence, ClosedAt its timestamp, so the handler's 409 names the facts.
type GroomingWindowClosedError struct {
	ArtifactID string
	Settlement string
	Sequence   int64
	ClosedAt   time.Time
}

func (e *GroomingWindowClosedError) Error() string {
	return fmt.Sprintf("audit: grooming capture window for artifact %s is closed (settlement=%s, watermark sequence %d)",
		e.ArtifactID, e.Settlement, e.Sequence)
}

// GroomingWindowAppender is the OPTIONAL capability the concrete audit
// Repository carries (#2991). It is deliberately kept OFF audit.Repository, the
// AnchoredChainAppender / RetryBudgetAppender precedent.
type GroomingWindowAppender interface {
	// AppendChainedGroomingDispositionBatch appends a whole capture batch under
	// the run-row lock in ONE transaction, after checking for an artifact-bound
	// closing watermark. It returns *GroomingWindowClosedError (writing NOTHING)
	// when the artifact's window is already closed; otherwise it returns the
	// appended entries. artifactID is the closing artifact the capture attaches
	// to; every param must carry the same RunID.
	AppendChainedGroomingDispositionBatch(ctx context.Context, artifactID string, ps []ChainAppendParams) ([]*Entry, error)
	// AppendChainedGroomingWindowClose settles the capture window for artifactID
	// in ONE transaction under the run-row lock, returning the watermark entry
	// and the consumed dispositions ({this artifact, below the watermark}). When
	// a watermark for artifactID already exists it returns that EXISTING entry
	// unchanged (permanence), appending nothing.
	AppendChainedGroomingWindowClose(ctx context.Context, p ChainAppendParams, artifactID string) (*Entry, []*Entry, error)
}

// AppendChainedGroomingDispositionBatchTx is the transaction-aware core of the
// batch capture. Ordering is LOAD-BEARING and mirrors AppendChainedAnchoredTx:
//
//  1. LockRunForUpdate(RunID) FIRST, held for the whole transaction.
//  2. Scan the run's grooming_apply_window_closed entries for one bound to
//     artifactID; if found, return *GroomingWindowClosedError writing NOTHING.
//  3. Delegate EACH param to AppendChainedTx in sequence (its re-entrant
//     LockRunForUpdate inside the same tx is a harmless no-op). A mid-batch
//     failure returns the error, so the caller's BeginFunc rolls the WHOLE batch
//     back — one capture is one transaction.
//
// The caller owns the transaction lifecycle and MUST run it at READ COMMITTED
// (do NOT set TxOptions), for the reason the file header states.
func AppendChainedGroomingDispositionBatchTx(ctx context.Context, tx pgx.Tx, artifactID string, ps []ChainAppendParams) ([]*Entry, error) {
	if len(ps) == 0 {
		return nil, nil
	}
	runID := ps[0].RunID

	rq := rundb.New(tx)
	if _, err := rq.LockRunForUpdate(ctx, runID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("audit: run %s not found", runID)
		}
		return nil, fmt.Errorf("audit: lock run for grooming disposition batch: %w", err)
	}

	if closed, err := existingGroomingWatermark(ctx, tx, runID, artifactID); err != nil {
		return nil, err
	} else if closed != nil {
		return nil, closed
	}

	out := make([]*Entry, 0, len(ps))
	for i := range ps {
		e, err := AppendChainedTx(ctx, tx, ps[i])
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// AppendChainedGroomingWindowCloseTx is the transaction-aware core of the
// settlement. Ordering mirrors the batch core:
//
//  1. LockRunForUpdate(p.RunID) FIRST.
//  2. Scan for an EXISTING grooming_apply_window_closed entry bound to
//     artifactID. If one exists, return it UNCHANGED alongside the dispositions
//     below it — the PERMANENCE property — appending nothing.
//  3. Otherwise append the watermark via AppendChainedTx and return it with the
//     consumed dispositions ({artifactID, below the new watermark's sequence}).
//     Reading the dispositions and appending the watermark in ONE transaction is
//     what closes the capture/apply TOCTOU.
func AppendChainedGroomingWindowCloseTx(ctx context.Context, tx pgx.Tx, p ChainAppendParams, artifactID string) (*Entry, []*Entry, error) {
	rq := rundb.New(tx)
	if _, err := rq.LockRunForUpdate(ctx, p.RunID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, fmt.Errorf("audit: run %s not found", p.RunID)
		}
		return nil, nil, fmt.Errorf("audit: lock run for grooming window close: %w", err)
	}

	existing, err := lowestGroomingWatermarkEntry(ctx, tx, p.RunID, artifactID)
	if err != nil {
		return nil, nil, err
	}
	if existing != nil {
		consumed, cerr := consumedGroomingDispositions(ctx, tx, p.RunID, artifactID, existing.Sequence)
		if cerr != nil {
			return nil, nil, cerr
		}
		return existing, consumed, nil
	}

	watermark, err := AppendChainedTx(ctx, tx, p)
	if err != nil {
		return nil, nil, err
	}
	consumed, err := consumedGroomingDispositions(ctx, tx, p.RunID, artifactID, watermark.Sequence)
	if err != nil {
		return nil, nil, err
	}
	return watermark, consumed, nil
}

// existingGroomingWatermark returns a *GroomingWindowClosedError describing the
// LOWEST-sequence watermark bound to artifactID, or nil when none exists.
func existingGroomingWatermark(ctx context.Context, tx pgx.Tx, runID uuid.UUID, artifactID string) (*GroomingWindowClosedError, error) {
	entry, err := lowestGroomingWatermarkEntry(ctx, tx, runID, artifactID)
	if err != nil || entry == nil {
		return nil, err
	}
	return &GroomingWindowClosedError{
		ArtifactID: artifactID,
		Settlement: groomingWatermarkSettlement(entry.Payload),
		Sequence:   entry.Sequence,
		ClosedAt:   entry.Timestamp.UTC(),
	}, nil
}

// lowestGroomingWatermarkEntry returns the watermark entry bound to artifactID
// with the LOWEST sequence (the first one written — the permanent one), or nil.
func lowestGroomingWatermarkEntry(ctx context.Context, tx pgx.Tx, runID uuid.UUID, artifactID string) (*Entry, error) {
	id := runID
	rows, err := auditdb.New(tx).ListAuditEntriesByCategory(ctx, auditdb.ListAuditEntriesByCategoryParams{
		RunID:    &id,
		Category: GroomingApplyWindowClosedCategory,
	})
	if err != nil {
		return nil, fmt.Errorf("audit: scan grooming watermarks: %w", err)
	}
	var best *Entry
	for i := range rows {
		if groomingWatermarkArtifactID(rows[i].Payload) != artifactID {
			continue
		}
		e := rowToEntry(rows[i])
		if best == nil || e.Sequence < best.Sequence {
			best = e
		}
	}
	return best, nil
}

// consumedGroomingDispositions lists the run's grooming_disposition_recorded
// entries and returns those recorded against artifactID with sequence STRICTLY
// BELOW belowSeq — the artifact-scoped consumed set (condition 1). It returns
// the raw entries; the caller collapses them last-wins per entry id.
func consumedGroomingDispositions(ctx context.Context, tx pgx.Tx, runID uuid.UUID, artifactID string, belowSeq int64) ([]*Entry, error) {
	id := runID
	rows, err := auditdb.New(tx).ListAuditEntriesByCategory(ctx, auditdb.ListAuditEntriesByCategoryParams{
		RunID:    &id,
		Category: GroomingDispositionRecordedCategory,
	})
	if err != nil {
		return nil, fmt.Errorf("audit: list grooming dispositions: %w", err)
	}
	out := make([]*Entry, 0, len(rows))
	for i := range rows {
		if rows[i].Sequence >= belowSeq {
			continue
		}
		if groomingWatermarkArtifactID(rows[i].Payload) != artifactID {
			continue
		}
		out = append(out, rowToEntry(rows[i]))
	}
	return out, nil
}

// groomingWatermarkArtifactID decodes the shared "artifact_id" payload field,
// written by the server for BOTH the disposition rows and the watermark. It
// returns "" for an absent key or malformed payload, which never matches a real
// artifact id — the fail-safe direction.
func groomingWatermarkArtifactID(payload []byte) string {
	var p struct {
		ArtifactID string `json:"artifact_id"`
	}
	if json.Unmarshal(payload, &p) != nil {
		return ""
	}
	return p.ArtifactID
}

// groomingWatermarkSettlement decodes the watermark's "settlement" field.
func groomingWatermarkSettlement(payload []byte) string {
	var p struct {
		Settlement string `json:"settlement"`
	}
	if json.Unmarshal(payload, &p) != nil {
		return ""
	}
	return p.Settlement
}
