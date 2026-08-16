-- Revert 0070: drop the stages.progress heartbeat column. Additive column
-- drop only — no CHECK or constraint was touched, so the rollback is safe
-- regardless of any stage state (unlike 0035, whose down re-narrowed a
-- CHECK). Orphaned progress payloads are inert.
ALTER TABLE stages DROP COLUMN progress;
