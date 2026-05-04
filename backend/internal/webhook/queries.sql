-- Webhook delivery dedup queries (E3.9 / #110). sqlc generates
-- typed Go into ./db per backend/sqlc.yaml.

-- name: MarkDelivery :one
-- INSERT ... ON CONFLICT DO NOTHING RETURNING. Empty result
-- means the row already existed (= duplicate); a returned
-- delivery_id means the row was newly inserted.
INSERT INTO webhook_deliveries (delivery_id)
VALUES ($1)
ON CONFLICT (delivery_id) DO NOTHING
RETURNING delivery_id;

-- name: EvictOldDeliveries :execrows
-- Returns the number of rows deleted so the eviction ticker can
-- log a counter. The index on received_at makes this an index
-- range scan rather than a full table sweep.
DELETE FROM webhook_deliveries
 WHERE received_at < $1;
