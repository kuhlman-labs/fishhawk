-- 0094: review_concerns provenance + check_key (E80.3 / #3760, ADR-084 D5 /
-- rule 5, secrets half).
--
-- Two columns recording WHO minted a review concern when it was not a model
-- reviewer's verdict:
--
--   provenance  '' = the row was written by a reviewer verdict, an operator
--               fix-up or a crew conversion — i.e. NOT a server check (every
--               row minted before this migration reads back ''). The one
--               non-empty value is 'server_check': the server synthesized the
--               concern itself from a deterministic check with no model call
--               (the diff secrets check). A server_check concern can be
--               cleared only by a HUMAN: waive, bulk waive and defer refuse
--               an agent token or a delegated request (403
--               concern_requires_human).
--   check_key   the server check's de-duplication key, so a re-run of the
--               same check across review rounds does not mint a duplicate
--               row; '' for every non-server_check row.
--
-- NOT NULL DEFAULT '', the additive shape 0092 used. NO BACKFILL: no
-- historical row was server-synthesized, so '' is the true value for all of
-- them.
--
-- Plain single-apply DDL (no IF NOT EXISTS), matching 0092: the migration
-- runner applies each version exactly once. Postgres APPENDS added columns,
-- so the sqlc column order is ... severity_clamped_from, provenance,
-- check_key.
ALTER TABLE review_concerns
    ADD COLUMN provenance TEXT NOT NULL DEFAULT '',
    ADD COLUMN check_key TEXT NOT NULL DEFAULT '';
