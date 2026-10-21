package campaign

// expedite_postgres.go is the hand-written pgx persistence of the hotfix
// verb's two migration-0106 tables (#4234): campaign_item_expedites and
// campaign_dispatch_holds. They are deliberately NOT sqlc: new campaign_items
// columns would change every sqlc SELECT * / RETURNING * scan of that table,
// so the expedite record lives in a child table attached on the item read
// paths by one item_id = ANY($1) query.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	campaigndb "github.com/kuhlman-labs/fishhawk/backend/internal/campaign/db"
)

// attachExpedites sets Item.Expedite on every item carrying a
// campaign_item_expedites row, and leaves it nil on the rest. A read error is
// returned rather than swallowed: a caller deciding the dispatch-hold
// exemption must never mistake an unreadable expedite for an ordinary item.
func attachExpedites(ctx context.Context, db campaigndb.DBTX, items []*Item) error {
	if len(items) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(items))
	byID := make(map[uuid.UUID][]*Item, len(items))
	for _, it := range items {
		if _, seen := byID[it.ID]; !seen {
			ids = append(ids, it.ID)
		}
		byID[it.ID] = append(byID[it.ID], it)
	}
	rows, err := db.Query(ctx,
		`SELECT item_id, kind, workflow, reason, created_at FROM campaign_item_expedites WHERE item_id = ANY($1)`, ids)
	if err != nil {
		return fmt.Errorf("list campaign item expedites: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var e Expedite
		if err := rows.Scan(&id, &e.Kind, &e.Workflow, &e.Reason, &e.CreatedAt); err != nil {
			return fmt.Errorf("scan campaign item expedite: %w", err)
		}
		for _, it := range byID[id] {
			cp := e
			it.Expedite = &cp
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list campaign item expedites: %w", err)
	}
	return nil
}

// insertExpedite writes an item's campaign_item_expedites row.
func insertExpedite(ctx context.Context, tx pgx.Tx, itemID, campaignID uuid.UUID, p ExpediteParams) (*Expedite, error) {
	e := Expedite{Kind: p.Kind, Workflow: p.Workflow, Reason: p.Reason}
	if err := tx.QueryRow(ctx,
		`INSERT INTO campaign_item_expedites (item_id, campaign_id, kind, workflow, reason)
VALUES ($1, $2, $3, $4, $5) RETURNING created_at`,
		itemID, campaignID, p.Kind, p.Workflow, p.Reason,
	).Scan(&e.CreatedAt); err != nil {
		return nil, fmt.Errorf("insert campaign item expedite: %w", err)
	}
	return &e, nil
}

const dispatchHoldColumns = `campaign_id, hotfix_item_id, held_at, expires_at`

func scanDispatchHold(row pgx.Row) (*DispatchHold, error) {
	var h DispatchHold
	if err := row.Scan(&h.CampaignID, &h.HotfixItemID, &h.HeldAt, &h.ExpiresAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &h, nil
}

// lockDispatchHold reads the campaign's hold row FOR UPDATE, or (nil, nil).
// The caller already holds the campaign row lock, which is what serializes
// every hold writer; the row lock is the second step of the package order.
func lockDispatchHold(ctx context.Context, tx pgx.Tx, campaignID uuid.UUID) (*DispatchHold, error) {
	h, err := scanDispatchHold(tx.QueryRow(ctx,
		`SELECT `+dispatchHoldColumns+` FROM campaign_dispatch_holds WHERE campaign_id = $1 FOR UPDATE`, campaignID))
	if err != nil {
		return nil, fmt.Errorf("lock campaign dispatch hold: %w", err)
	}
	return h, nil
}

// insertDispatchHold writes the campaign's hold row with the caller's
// fishhawkd-clock stamps, never Postgres now().
func insertDispatchHold(ctx context.Context, tx pgx.Tx, campaignID, itemID uuid.UUID, heldAt, expiresAt time.Time) (*DispatchHold, error) {
	h, err := scanDispatchHold(tx.QueryRow(ctx,
		`INSERT INTO campaign_dispatch_holds (`+dispatchHoldColumns+`) VALUES ($1, $2, $3, $4) RETURNING `+dispatchHoldColumns,
		campaignID, itemID, heldAt, expiresAt))
	if err != nil {
		return nil, fmt.Errorf("insert campaign dispatch hold: %w", err)
	}
	return h, nil
}

// GetDispatchHold returns the campaign's hold row, or (nil, nil) when none.
func (r *postgresRepo) GetDispatchHold(ctx context.Context, campaignID uuid.UUID) (*DispatchHold, error) {
	h, err := scanDispatchHold(r.pool.QueryRow(ctx,
		`SELECT `+dispatchHoldColumns+` FROM campaign_dispatch_holds WHERE campaign_id = $1`, campaignID))
	if err != nil {
		return nil, fmt.Errorf("get campaign dispatch hold: %w", err)
	}
	return h, nil
}

// ReleaseDispatchHold deletes the campaign's hold row, recorded before commit
// (see the Repository doc for the contract). Lock order: campaign row, then
// hold row. A missing campaign has no hold (the row cascades with it), so it
// is the same idempotent no-op as a missing or foreign hold.
func (r *postgresRepo) ReleaseDispatchHold(ctx context.Context, p ReleaseDispatchHoldParams) (*DispatchHold, error) {
	if err := ValidateReleaseDispatchHold(p); err != nil {
		return nil, err
	}
	var released *DispatchHold
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		if _, err := campaigndb.New(tx).LockCampaignForUpdate(ctx, p.CampaignID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return fmt.Errorf("lock campaign: %w", err)
		}
		h, err := lockDispatchHold(ctx, tx, p.CampaignID)
		if err != nil {
			return err
		}
		if h == nil || h.HotfixItemID != p.HotfixItemID {
			return nil // already released (a lost race), or a different hold
		}
		if _, err := tx.Exec(ctx, `DELETE FROM campaign_dispatch_holds WHERE campaign_id = $1`, p.CampaignID); err != nil {
			return fmt.Errorf("delete campaign dispatch hold: %w", err)
		}
		// Record before commit: an error rolls the delete back, so the hold
		// stays active rather than vanishing unaudited.
		if err := p.Record(ctx, *h); err != nil {
			return fmt.Errorf("%w: %w", ErrHoldReleaseUnrecorded, err)
		}
		released = h
		return nil
	})
	if err != nil {
		return nil, err
	}
	return released, nil
}
