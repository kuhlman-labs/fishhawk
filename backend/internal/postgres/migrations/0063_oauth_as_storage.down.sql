-- Reverse 0063 (E66.18 / #2433): drop exactly the four OAuth AS storage tables
-- in reverse foreign-key order and touch NOTHING pre-existing — no ALTER of a
-- 0057-policied table, no index or policy drop outside these four. Their
-- indexes go with the tables.
--
-- Nothing else in the schema references them, so this returns the database to
-- its 0062 state. TestMigrateDown_OAuthASStorageReversal pins that: after this
-- rollback all four tables are absent while api_tokens and its 0057
-- api_tokens_tenant_isolation policy survive.

DROP TABLE IF EXISTS oauth_refresh_tokens;
DROP TABLE IF EXISTS oauth_access_tokens;
DROP TABLE IF EXISTS oauth_authorization_codes;
DROP TABLE IF EXISTS oauth_clients;
