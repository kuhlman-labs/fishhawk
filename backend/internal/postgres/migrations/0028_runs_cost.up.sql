-- 0028: per-run cost accounting + reproducibility pin (E22 / #649).
--
-- cost_usd_total accumulates the estimated US-dollar cost of every
-- model invocation in the run, rolled control-plane-side from the
-- signed bundle manifest's token counts via the shared `pricing`
-- table (NOT trusted from a runner-emitted span — a dropped or
-- tampered span can't corrupt the ledger). The figure is an
-- ESTIMATE: pricing is a point-in-time, model-keyed table (see
-- pricing.AsOf), and an unknown model id contributes 0.
--
-- resolved_model pins the agent model id the run actually executed
-- under (read from the manifest), backing the G6 reproducibility
-- capture. Paired with the per-bundle cost_recorded audit entries
-- (model + token split + usd), the run's audit trail IS the
-- trajectory pointer linking the run to its model-call history.
--
-- Both columns are additive with safe defaults: legacy rows read 0 /
-- '' and every run-detail surface tolerates them. No index — read by
-- PK (id) on the same row every run-detail surface already fetches.
--
-- Verifier impact (verifier/internal/audit/): the rollup is metadata
-- atop the tamper-evidence chain, not a hash input; adding the
-- columns doesn't change the rehash invariant.

ALTER TABLE runs
    ADD COLUMN cost_usd_total DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN resolved_model TEXT NOT NULL DEFAULT '';
