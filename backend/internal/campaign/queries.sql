-- Campaign / campaign-item queries consumed by the postgres adapter for the
-- campaign.Repository interface (ADR-047 / #1437, E25.2). sqlc generates
-- typed Go into ./db per the `campaign` block in /backend/sqlc.yaml. Mirrors
-- internal/run/queries.sql.

-- name: CreateCampaign :one
INSERT INTO campaigns (id, repo, epic_ref, state, pause_policy, operator_agent, idempotency_key)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetCampaign :one
SELECT * FROM campaigns WHERE id = $1;

-- name: GetCampaignByIdempotencyKey :one
-- Used by POST /v0/campaigns to resolve an Idempotency-Key header to a
-- previously-created campaign. Active scope is (repo, idempotency_key);
-- the partial unique index covers this lookup with no full scan. Mirrors
-- internal/run/queries.sql GetRunByIdempotencyKey.
SELECT * FROM campaigns
 WHERE repo = $1
   AND idempotency_key = $2;

-- name: ListCampaigns :many
-- Empty string in any filter means "no constraint." created_at DESC + id
-- DESC tiebreak so paginations are stable across concurrent inserts at the
-- same created_at microsecond.
SELECT * FROM campaigns
 WHERE (sqlc.arg('repo')::text = '' OR repo = sqlc.arg('repo'))
   AND (sqlc.arg('state')::text = '' OR state = sqlc.arg('state'))
 ORDER BY created_at DESC, id DESC
 LIMIT sqlc.arg('lim') OFFSET sqlc.arg('off');

-- name: LockCampaignForUpdate :one
SELECT * FROM campaigns WHERE id = $1 FOR UPDATE;

-- name: UpdateCampaignState :one
UPDATE campaigns
   SET state = $2
 WHERE id = $1
RETURNING *;

-- name: CreateCampaignItem :one
INSERT INTO campaign_items (id, campaign_id, issue_ref, depends_on, state, autonomy)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetCampaignItem :one
SELECT * FROM campaign_items WHERE id = $1;

-- name: ListCampaignItemsForCampaign :many
-- Insertion order (created_at ASC + id tiebreak) so the campaign's items
-- render in the order they were assembled.
SELECT * FROM campaign_items
 WHERE campaign_id = $1
 ORDER BY created_at ASC, id ASC;

-- name: ListCampaignItemsForRun :many
-- The reverse-discovery query ("which campaign owns this run") served by
-- the campaign_items_run_idx index. Ordered created_at ASC for a stable
-- result.
SELECT * FROM campaign_items
 WHERE run_id = $1
 ORDER BY created_at ASC, id ASC;

-- name: LockCampaignItemForUpdate :one
SELECT * FROM campaign_items WHERE id = $1 FOR UPDATE;

-- name: SetCampaignItemRun :one
-- Attaches (or clears, when $2 IS NULL) the run linkage on an item.
-- Idempotent: re-setting the same value is a no-op the trigger keeps as a
-- no-op against updated_at (assignment of identical value).
UPDATE campaign_items
   SET run_id = $2
 WHERE id = $1
RETURNING *;

-- name: UpdateCampaignItemState :one
UPDATE campaign_items
   SET state = $2
 WHERE id = $1
RETURNING *;

-- name: SetCampaignItemPause :one
-- Pauses an item: sets state='paused' and records the pause_reason JSONB,
-- applied under the existing LockCampaignItemForUpdate FOR UPDATE lock so the
-- running→paused transition is serialized like the other state moves (E25.7).
UPDATE campaign_items
   SET state = 'paused', pause_reason = $2
 WHERE id = $1
RETURNING *;
