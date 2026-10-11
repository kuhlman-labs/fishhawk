-- 0102 down: drop the promote ordering key. Pending promotions are lost and
-- every queued row falls back to enqueued_at FIFO; no held or queued row is
-- lost and no stage state is touched.
ALTER TABLE stage_concurrency_slots DROP COLUMN IF EXISTS order_key;
