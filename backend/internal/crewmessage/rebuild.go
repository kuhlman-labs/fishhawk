package crewmessage

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
)

// RebuildOptions tunes Rebuild.
type RebuildOptions struct {
	// Truncate empties crew_messages first, inside the SAME transaction as the
	// replay, so a failed rebuild rolls back to the prior index rather than
	// leaving it half-empty.
	Truncate bool
	// PageSize bounds each chain read; <= 0 selects 500.
	PageSize int
	// Logger receives one WARN per skipped entry; nil selects slog.Default().
	Logger *slog.Logger
}

// RebuildStats counts what one Rebuild did.
type RebuildStats struct {
	EntriesRead int
	// RowsUpserted counts send projections plus dispositions applied.
	RowsUpserted int
	// RowsSkippedUndecodable counts entries whose payload could not be decoded.
	// They are skipped with a WARN, never guessed at.
	RowsSkippedUndecodable int
	// DispositionsSkippedOrphan counts dispositions whose sent entry was not
	// replayed (missing or undecodable).
	DispositionsSkippedOrphan int
	// DispositionsSkippedSuperseded counts a second (or later) disposition of
	// one message: the FIRST in chain order wins, as in live processing.
	DispositionsSkippedSuperseded int
	EscalationsSeen               int
}

// Rebuild replays the chain's crew-message entries in ASCENDING sequence
// order through the SAME builders (projectSentRow, applyDisposed) and the SAME
// projections (Store.Upsert, projectDisposition) the live Mailbox uses. That
// sharing is what makes "a rebuild yields an identical row" structural.
//
// The whole replay runs in one transaction opened on db.
func Rebuild(ctx context.Context, db DBTX, opts RebuildOptions) (RebuildStats, error) {
	pageSize := opts.PageSize
	if pageSize <= 0 {
		pageSize = 500
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	var stats RebuildStats
	err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		store := NewStore(tx)
		if opts.Truncate {
			if err := store.Truncate(ctx); err != nil {
				return err
			}
		}
		r := &replayer{store: store, logger: logger, stats: &stats, sent: map[int64]Row{}}
		var after int64
		for {
			page, err := readChainPage(ctx, tx, after, pageSize)
			if err != nil {
				return err
			}
			for _, e := range page {
				if err := r.apply(ctx, e); err != nil {
					return err
				}
				after = e.Sequence
			}
			if len(page) < pageSize {
				return nil
			}
		}
	})
	if err != nil {
		return stats, fmt.Errorf("crewmessage: rebuild: %w", err)
	}
	return stats, nil
}

// readChainPage reads up to limit crew-message entries after sequence after.
func readChainPage(ctx context.Context, q DBTX, after int64, limit int) ([]*audit.Entry, error) {
	rows, err := q.Query(ctx, `SELECT `+chainEntryColumns+` FROM audit_entries
		WHERE category IN ($1, $2, $3) AND sequence > $4
		ORDER BY sequence LIMIT $5`,
		CategorySent, CategoryDisposed, CategoryEscalated, after, limit)
	if err != nil {
		return nil, fmt.Errorf("read chain page after %d: %w", after, err)
	}
	page, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*audit.Entry, error) {
		return scanChainEntry(row)
	})
	if err != nil {
		return nil, fmt.Errorf("read chain page after %d: %w", after, err)
	}
	return page, nil
}

type replayer struct {
	store  *Store
	logger *slog.Logger
	stats  *RebuildStats
	// sent is every replayed send projection, by sent sequence: a disposition
	// builds its terminal row from it.
	sent map[int64]Row
}

func (r *replayer) skip(e *audit.Entry, why string, err error) {
	r.stats.RowsSkippedUndecodable++
	r.logger.Warn("crewmessage: rebuild skipped undecodable entry",
		"sequence", e.Sequence, "category", e.Category, "reason", why, "error", err)
}

func (r *replayer) apply(ctx context.Context, e *audit.Entry) error {
	r.stats.EntriesRead++
	switch e.Category {
	case CategorySent:
		sp, msg, err := decodeSent(e)
		if err != nil {
			r.skip(e, "sent payload", err)
			return nil
		}
		row, err := projectSentRow(e, msg, sp.ThreadRootSequence)
		if err != nil {
			r.skip(e, "sent projection", err)
			return nil
		}
		if err := r.store.Upsert(ctx, row); err != nil {
			return err
		}
		r.sent[e.Sequence] = row
		r.stats.RowsUpserted++
	case CategoryDisposed:
		var dp disposedPayload
		if err := json.Unmarshal(e.Payload, &dp); err != nil || !ValidDisposition(dp.Disposition) {
			r.skip(e, "disposed payload", err)
			return nil
		}
		sent, ok := r.sent[dp.SentSequence]
		if !ok {
			r.stats.DispositionsSkippedOrphan++
			r.logger.Warn("crewmessage: rebuild skipped disposition with no replayed send",
				"sequence", e.Sequence, "sent_sequence", dp.SentSequence)
			return nil
		}
		applied, err := projectDisposition(ctx, r.store, applyDisposed(sent, e, dp))
		if err != nil {
			return err
		}
		if !applied {
			r.stats.DispositionsSkippedSuperseded++
			return nil
		}
		r.stats.RowsUpserted++
	case CategoryEscalated:
		r.stats.EscalationsSeen++
	}
	return nil
}
