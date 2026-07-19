-- 0055 (down): reverse E44.1 — drop the account_id threading, restore the
-- endpoint columns to accounts, and drop account_members (ADR-057 / ADR-058,
-- #1825). Children before parents. No data restore is needed: the up backfill
-- wrote only the nullable account_id column being dropped here, and the endpoint
-- relocation moved columns that have no production reader. The shared
-- fishhawk_set_updated_at() function is left in place (it predates this
-- migration, defined in 0001).

-- Drop the account_id FK + index + column from every root entity.
ALTER TABLE runs                        DROP CONSTRAINT runs_account_id_fkey;
ALTER TABLE campaigns                   DROP CONSTRAINT campaigns_account_id_fkey;
ALTER TABLE refinement_drafts           DROP CONSTRAINT refinement_drafts_account_id_fkey;
ALTER TABLE refinement_decisions        DROP CONSTRAINT refinement_decisions_account_id_fkey;
ALTER TABLE refinement_filing_sessions  DROP CONSTRAINT refinement_filing_sessions_account_id_fkey;
ALTER TABLE refinement_filed_items      DROP CONSTRAINT refinement_filed_items_account_id_fkey;
ALTER TABLE api_tokens                  DROP CONSTRAINT api_tokens_account_id_fkey;
ALTER TABLE audit_entries               DROP CONSTRAINT audit_entries_account_id_fkey;

DROP INDEX runs_account_id_idx;
DROP INDEX campaigns_account_id_idx;
DROP INDEX refinement_drafts_account_id_idx;
DROP INDEX refinement_decisions_account_id_idx;
DROP INDEX refinement_filing_sessions_account_id_idx;
DROP INDEX refinement_filed_items_account_id_idx;
DROP INDEX api_tokens_account_id_idx;
DROP INDEX audit_entries_account_id_idx;

ALTER TABLE runs                        DROP COLUMN account_id;
ALTER TABLE campaigns                   DROP COLUMN account_id;
ALTER TABLE refinement_drafts           DROP COLUMN account_id;
ALTER TABLE refinement_decisions        DROP COLUMN account_id;
ALTER TABLE refinement_filing_sessions  DROP COLUMN account_id;
ALTER TABLE refinement_filed_items      DROP COLUMN account_id;
ALTER TABLE api_tokens                  DROP COLUMN account_id;
ALTER TABLE audit_entries               DROP COLUMN account_id;

-- Reverse the Amendment A1 relocation: endpoints back on accounts, gone from
-- installations.
ALTER TABLE accounts ADD COLUMN forge_base_url TEXT;
ALTER TABLE accounts ADD COLUMN oauth_base_url TEXT;
ALTER TABLE installations DROP COLUMN forge_base_url;
ALTER TABLE installations DROP COLUMN oauth_base_url;

-- Drop account_members (its trigger + index drop with the table).
DROP TABLE account_members;
