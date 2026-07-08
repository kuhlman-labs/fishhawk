-- Down-migration for 0050: drop api_tokens.provider and auth_method (with its
-- fail-closed CHECK). Both columns are additive with no dependent objects — the
-- provider column is plain nullable and auth_method carried a DEFAULT 'static'
-- — so the rollback drops them cleanly with no data normalization. Bearer auth
-- behaves exactly as pre-0050 after the roll back (every token authenticates as
-- before; the auth_method/provider projection simply disappears).
ALTER TABLE api_tokens
    DROP COLUMN provider,
    DROP COLUMN auth_method;
