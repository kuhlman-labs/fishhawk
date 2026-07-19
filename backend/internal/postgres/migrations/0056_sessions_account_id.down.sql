-- Revert 0056: drop the session->account binding and the Amendment A2
-- admission columns. Purely additive going up (no backfill), so the rollback
-- loses no pre-0056 data; sessions simply stop carrying an account binding.

ALTER TABLE accounts DROP COLUMN auto_join_role;

ALTER TABLE account_members DROP CONSTRAINT account_members_origin_check;
ALTER TABLE account_members DROP COLUMN origin;

DROP INDEX sessions_account_id_idx;
ALTER TABLE sessions DROP CONSTRAINT sessions_account_id_fkey;
ALTER TABLE sessions DROP COLUMN account_id;
