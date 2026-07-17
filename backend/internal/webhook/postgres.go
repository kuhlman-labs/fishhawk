package webhook

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	webhookdb "github.com/kuhlman-labs/fishhawk/backend/internal/webhook/db"
)

// PostgresStore is a DeliveryStore backed by the webhook_deliveries
// table. Unlike MemoryStore, dedup state survives restarts and is
// shared across instances — required for any horizontally-scaled
// deploy.
//
// Mark uses INSERT ... ON CONFLICT DO NOTHING RETURNING. An empty
// RETURNING means the row already existed and the delivery is a
// duplicate; that path returns ErrDeliveryDuplicate.
//
// Eviction is a separate concern; callers run a periodic Evict()
// pass to bound the table size. The serve startup wires a 1h tick
// for that, with a 24h retention window comfortably exceeding
// GitHub's ~3h retry window.
type PostgresStore struct {
	pool *pgxpool.Pool
}

// NewPostgresStore returns a DeliveryStore backed by pool. Caller
// retains ownership of pool and is responsible for Close.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

// Mark records id as processed. Returns ErrDeliveryDuplicate when
// the id was already recorded; ErrDeliveryMissing on empty input;
// nil on first write.
func (s *PostgresStore) Mark(id string) error {
	if id == "" {
		return ErrDeliveryMissing
	}
	q := webhookdb.New(s.pool)
	_, err := q.MarkDelivery(context.Background(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		// ON CONFLICT DO NOTHING + RETURNING produces zero rows
		// on conflict — the row already existed, so this is a
		// duplicate.
		return ErrDeliveryDuplicate
	}
	if err != nil {
		return fmt.Errorf("webhook: mark delivery: %w", err)
	}
	return nil
}

// Unmark deletes id's delivery row so a later Mark(id) is a first write
// again. The receiver calls it to undo the pre-dispatch record when a
// delivery's processing fails and it returns a 5xx: the delivery is
// recorded before dispatch (to dedup concurrent redeliveries), so without
// removing the record GitLab's retry would hit the recorded row, Mark would
// return ErrDeliveryDuplicate, and the receiver would answer 202 —
// permanently dropping an event whose processing actually failed.
//
// A raw single-row DELETE on the delivery_id primary key rather than a
// sqlc query, to keep this compensation confined to the store file (it has
// no companion :one/:exec query and needs no generated model). Idempotent:
// deleting an absent id affects zero rows and returns nil, matching
// MemoryStore.Unmark.
func (s *PostgresStore) Unmark(id string) error {
	if id == "" {
		return ErrDeliveryMissing
	}
	if _, err := s.pool.Exec(context.Background(),
		"DELETE FROM webhook_deliveries WHERE delivery_id = $1", id); err != nil {
		return fmt.Errorf("webhook: unmark delivery: %w", err)
	}
	return nil
}

// Evict deletes every webhook_deliveries row whose received_at is
// strictly older than before. Returns the number of rows deleted
// so callers can log a counter.
//
// before should be `now - retention`; the serve startup uses 24h
// retention but tests may pass smaller values. The
// webhook_deliveries.received_at index makes this an index range
// scan, not a full table sweep.
func (s *PostgresStore) Evict(ctx context.Context, before time.Time) (int64, error) {
	q := webhookdb.New(s.pool)
	n, err := q.EvictOldDeliveries(ctx, pgtype.Timestamptz{Time: before, Valid: true})
	if err != nil {
		return 0, fmt.Errorf("webhook: evict old deliveries: %w", err)
	}
	return n, nil
}

// Compile-time check.
var _ DeliveryStore = (*PostgresStore)(nil)
