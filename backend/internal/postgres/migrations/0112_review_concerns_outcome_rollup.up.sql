-- 0112: review_concerns.outcome + rolled_up_from (E94.1 / #4692, ADR-117 D2
-- and the open_at_merge half of D1).
--
--   outcome         the concern's recorded OUTCOME, orthogonal to its
--                   lifecycle state. '' = no outcome recorded, the true value
--                   for every row minted before this migration. This change
--                   writes two values: 'carried' (a decomposition child's
--                   open HIGH/MEDIUM concern copied into its parent's ledger
--                   at fan-in) and 'open_at_merge' (a concern still open, or
--                   superseded, when its run merged). The rest of the D1
--                   vocabulary lands later with no further migration of this
--                   column. Tolerant TEXT with no CHECK, the 0030/0094
--                   posture. Writing it never touches state, state_reason or
--                   updated_at.
--   rolled_up_from  on a parent copy, the id of the child concern it was
--                   copied from; NULL on every other row.
--
-- review_concerns_rolled_up_once_idx is the durable cross-process
-- idempotence guard for the fan-in import: a re-run of the same fan-in (verb,
-- sweeper, event arm) inserts nothing twice. NULLs are distinct in a unique
-- index, so every non-copy row is unconstrained. backend/internal/concern's
-- README is the contract.
--
-- No backfill. Postgres APPENDS added columns, so the sqlc column order is
-- ... provenance, check_key, outcome, rolled_up_from.
ALTER TABLE review_concerns
    ADD COLUMN outcome TEXT NOT NULL DEFAULT '',
    ADD COLUMN rolled_up_from UUID NULL;

CREATE UNIQUE INDEX review_concerns_rolled_up_once_idx
    ON review_concerns (run_id, rolled_up_from)
    WHERE rolled_up_from IS NOT NULL;
