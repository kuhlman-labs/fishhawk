-- Roll back 0012. Down is best-effort: if the table now has multiple
-- rows per run_id (post-rotation production data) the constraint
-- restoration will fail. Operators rolling back must first dedup
-- with a query like:
--   DELETE FROM signing_keys a USING signing_keys b
--   WHERE a.run_id = b.run_id AND a.issued_at < b.issued_at;
-- and then re-run this migration.

DROP INDEX IF EXISTS signing_keys_run_id_issued_at_idx;

ALTER TABLE signing_keys
    DROP CONSTRAINT signing_keys_pkey;

ALTER TABLE signing_keys
    DROP COLUMN id;

ALTER TABLE signing_keys
    ADD CONSTRAINT signing_keys_pkey PRIMARY KEY (run_id);
