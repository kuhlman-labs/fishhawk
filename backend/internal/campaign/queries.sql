-- Campaign / campaign-item queries consumed by the postgres adapter for the
-- campaign.Repository interface (ADR-047 / #1437, E25.2). sqlc generates
-- typed Go into ./db per the `campaign` block in /backend/sqlc.yaml. Mirrors
-- internal/run/queries.sql.

-- name: CreateCampaign :one
-- grooming_source is the DURABLE provenance of a campaign built from an
-- approved grooming order (E54.6 / #2238), written by this SAME single-row
-- INSERT so a campaign can never exist unprovenanced — campaign.Persist is not
-- transactional and the audit emit is best-effort after it. NULL for every
-- epic_ref / explicit-items campaign.
INSERT INTO campaigns (id, repo, epic_ref, state, pause_policy, operator_agent, idempotency_key, working_dir, grooming_source)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: GetCampaign :one
SELECT * FROM campaigns WHERE id = $1;

-- name: GetCampaignAccountID :one
-- The cheap tenant-account lookup for the campaign ownership check
-- (ADR-057 / #1830): returns just account_id (nullable) so the handler can
-- bound GET /v0/campaigns/{id} to the caller's account. Mirrors
-- internal/run/queries.sql GetRunAccountID.
SELECT account_id FROM campaigns WHERE id = $1;

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
-- same created_at microsecond. account_id (ADR-057 / #1830) scopes to a
-- tenant workspace account: a set filter keeps the account's rows PLUS
-- untenanted (NULL account_id) rows, same contract as run.ListRuns.
SELECT * FROM campaigns
 WHERE (sqlc.arg('repo')::text = '' OR repo = sqlc.arg('repo'))
   AND (sqlc.arg('state')::text = '' OR state = sqlc.arg('state'))
   AND (sqlc.narg('account_id')::uuid IS NULL OR account_id = sqlc.narg('account_id') OR account_id IS NULL)
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
-- queue_position is the item's 0-based place in the campaign queue (migration
-- 0074). It is written EXPLICITLY rather than inferred from insertion sequence:
-- every item of one campaign is inserted inside one transaction, so their
-- now()-defaulted created_at values are IDENTICAL and (created_at, id) ordering
-- collapses to the random-UUID tiebreak.
INSERT INTO campaign_items (id, campaign_id, issue_ref, depends_on, state, autonomy, queue_position)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetCampaignItem :one
SELECT * FROM campaign_items WHERE id = $1;

-- name: ListCampaignItemsForCampaign :many
-- QUEUE ORDER: queue_position ASC first (migration 0074), then the historical
-- (created_at, id) tiebreak. queue_position is the durable assembled order —
-- for a grooming-order campaign it IS the ratified rank order, and it is the
-- order the engine's Eligible slice is built in. The (created_at, id) tiebreak
-- is retained deliberately so every pre-0074 row, which carries the DEFAULT 0,
-- lists in exactly its pre-0074 order rather than an undefined one.
SELECT * FROM campaign_items
 WHERE campaign_id = $1
 ORDER BY queue_position ASC, created_at ASC, id ASC;

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

-- name: SetCampaignItemAutonomy :one
-- Sets the item's autonomy tier — routing metadata re-read from the issue's
-- autonomy:* label on the reconcile-on-read refresh (#2355). NOT a lifecycle
-- transition, so it needs no FOR UPDATE state guard; the caller normalizes the
-- tier to the CHECK-permitted set ("", low, medium, high) before this write.
UPDATE campaign_items
   SET autonomy = $2
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
