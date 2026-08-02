-- Reverse 0064 (E66.20 / #2437): restore oauth_clients to its exact 0063 shape —
-- the `provider` column with its DEFAULT and CHECK, and the composite
-- UNIQUE (provider, client_id) — and drop the single-column
-- oauth_clients_client_id_key this migration added.
--
-- LOSSY IN PRINCIPLE, and stated rather than glossed: a re-added column cannot
-- recover which forge context a row was first seen under, so every surviving row
-- reads back the DEFAULT 'github' whatever it was before. That is harmless for
-- exactly the reason the up-migration needs no dedup step — oauth_clients has no
-- production writer (#2438), so the table is empty in every deployment and there
-- is no history to lose. The reversal itself is pinned by
-- TestMigrateDown_OAuthClientsProviderReversal.
--
-- Order matters: the single-column unique is dropped BEFORE the composite is
-- added, so the intermediate state never carries both keys.

ALTER TABLE oauth_clients DROP CONSTRAINT IF EXISTS oauth_clients_client_id_key;

ALTER TABLE oauth_clients ADD COLUMN provider TEXT NOT NULL DEFAULT 'github';

ALTER TABLE oauth_clients
    ADD CONSTRAINT oauth_clients_provider_check CHECK (provider IN ('github', 'gitlab'));

ALTER TABLE oauth_clients
    ADD CONSTRAINT oauth_clients_provider_client_id_key UNIQUE (provider, client_id);
