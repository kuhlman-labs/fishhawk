-- 0050: api_tokens.auth_method + provider — record how a token was
-- authenticated at issue time (E39.3 / #1708).
--
-- Static operator tokens (fishhawkd token issue) carry auth_method='static';
-- OAuth device-flow tokens minted via POST /v0/tokens/login carry
-- auth_method='oauth' plus the originating provider (e.g. 'github'). Recording
-- the method lets approvals attribute a decision to the credential class that
-- authorized it.
--
-- This migration is additive and backward-compatible: auth_method is nullable
-- with a DEFAULT 'static', so every existing row and every un-updated caller
-- (the static Issue path relies on the default) reads back 'static' with no
-- forced data migration. provider is nullable with no default (NULL for static
-- tokens).
--
-- The CHECK is fail-closed on the known method set; NULL passes the CHECK
-- (three-valued logic) so the nullable column stays legal.

ALTER TABLE api_tokens
    ADD COLUMN auth_method TEXT DEFAULT 'static'
        CONSTRAINT api_tokens_auth_method_check CHECK (auth_method IN ('static', 'oauth')),
    ADD COLUMN provider TEXT;
