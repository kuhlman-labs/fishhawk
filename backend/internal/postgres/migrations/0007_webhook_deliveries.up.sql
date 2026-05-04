-- 0007: webhook delivery dedup table (E3.9 / #110).
--
-- The webhook receiver dedups by X-GitHub-Delivery (a per-delivery
-- UUID GitHub assigns and retains across the retry window).
-- v0 self-execution ran with an in-memory map; this migration
-- moves dedup to Postgres so it survives restarts and works
-- across multiple instances.
--
-- TEXT primary key, not UUID: the header value is already a
-- string from the HTTP layer and storing it raw avoids a parse +
-- re-format round trip on the hot insert path. Length is bounded
-- by the GitHub header (UUID, ~36 bytes).

CREATE TABLE webhook_deliveries (
    delivery_id  TEXT          PRIMARY KEY,
    received_at  TIMESTAMPTZ   NOT NULL DEFAULT now()
);

CREATE INDEX webhook_deliveries_received_at_idx
    ON webhook_deliveries (received_at);
