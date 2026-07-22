-- Roll back 0001 (dev only; production is forward-only per ADR-006).
DROP INDEX IF EXISTS install_states_expires_at_idx;
DROP TABLE IF EXISTS install_states;
DROP TABLE IF EXISTS account_regions;
