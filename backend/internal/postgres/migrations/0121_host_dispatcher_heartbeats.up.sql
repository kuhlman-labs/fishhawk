-- 0121: host_dispatcher_heartbeats, the latest per-tick heartbeat each host
-- dispatcher POSTs to /v0/host-dispatchers/heartbeat (#5489, ADR-104
-- Amendment A2). One row per (account, host label); the campaign status
-- awaiting block reads these rows to classify host-dispatcher state.
--
--   * CLOCK DOMAIN: received_at is fishhawkd's clock (the handler stamps it
--     from the server clock seam), never now() and never the dispatcher's own
--     last_tick_at. A reader compares it only against fishhawkd's clock, so
--     cross-host clock skew cannot keep a dead dispatcher fresh. There is no
--     DEFAULT on purpose.
--   * TENANCY BASIS: `fishhawkd token issue` mints untenanted dispatcher
--     tokens by design (#4506, cmd/fishhawkd/token.go), but
--     apitoken.IssueForAccount permits a tenanted host-dispatcher token and
--     bearerAuth sets Identity.AccountID from the token row. A dispatcher token
--     can therefore be account-bound, and its heartbeat must neither reach nor
--     be overwritten by another account's dispatcher with the same label. So
--     the row key is (host_label, account) and the table carries the 0089
--     tenancy pattern. Uniqueness is a UNIQUE expression index over
--     COALESCE(account_id, the nil UUID), as 0089 states: a plain UNIQUE over
--     the nullable account_id treats NULLs as distinct, so an untenanted
--     dispatcher would accumulate duplicate rows.
--   * host_label carries hostdispatcher.validateLabel's grammar: 1..64
--     characters of [A-Za-z0-9._-].
--   * The row is operational state, NOT a chain fact: a heartbeat writes no
--     audit row, and dropping the table loses nothing but the latest
--     snapshots, which the next tick rewrites.
--
-- Tenant isolation is byte-identical to 0089's captain_read_watermarks
-- policy: account_id IS NULL rows stay visible (the untenanted #1829 window)
-- and an unset/empty app.account_id fails CLOSED to NULL-account rows only.
-- FORCE is required because the application connects as the table owner; the
-- 0057 superuser caveat applies unchanged, so the repository's List also
-- carries an explicit account WHERE (backend/internal/dispatcherheartbeat).

CREATE TABLE host_dispatcher_heartbeats (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id  UUID,
    host_label  TEXT        NOT NULL,
    token_id    TEXT,
    payload     JSONB       NOT NULL,
    received_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT host_dispatcher_heartbeats_label_check
        CHECK (host_label ~ '^[A-Za-z0-9._-]{1,64}$')
);

CREATE UNIQUE INDEX host_dispatcher_heartbeats_key_idx ON host_dispatcher_heartbeats
    (host_label, COALESCE(account_id, '00000000-0000-0000-0000-000000000000'::uuid));

ALTER TABLE host_dispatcher_heartbeats ENABLE ROW LEVEL SECURITY;
ALTER TABLE host_dispatcher_heartbeats FORCE ROW LEVEL SECURITY;
CREATE POLICY host_dispatcher_heartbeats_tenant_isolation ON host_dispatcher_heartbeats
    USING (account_id IS NULL OR account_id = NULLIF(current_setting('app.account_id', true), '')::uuid)
    WITH CHECK (account_id IS NULL OR account_id = NULLIF(current_setting('app.account_id', true), '')::uuid);
