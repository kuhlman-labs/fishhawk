-- 0116: the ADR-104 Q3 host-dispatcher mark (E88.2 / #4526).
--
-- runs.auto_dispatch is stamped by the run's STARTER at create time: the
-- campaign driver stamps it true, an operator's POST /v0/runs may opt in, and
-- a webhook-started run is never marked. A run minted FROM another run (a
-- CI-retry or operator-recovery child) inherits its parent's value via
-- run.ChildParamsFrom. The host dispatcher (fishhawk-mcp host-dispatch) spawns
-- the parked stages of a marked local run only; the mark grants no gate act.
--
-- Additive: NOT NULL DEFAULT false, so every existing row reads unmarked and
-- a reverted binary ignores the column.
ALTER TABLE runs
    ADD COLUMN auto_dispatch BOOLEAN NOT NULL DEFAULT false;
