-- 0028 down: drop the cost-accounting columns. The per-invocation
-- cost_recorded audit entries (JSONB payloads carrying model + token
-- split + usd) survive as the canonical ledger; only the
-- denormalized per-run rollup + resolved_model pin are lost until a
-- re-migration.
ALTER TABLE runs
    DROP COLUMN IF EXISTS cost_usd_total,
    DROP COLUMN IF EXISTS resolved_model;
