-- 0096: user_report_cursors, the per-(account, repository, source) READ
-- POSITION of the user-report source reader (E81.1 / #3771,
-- backend/internal/userreport).
--
--   * cursor_at is the inclusive lower bound the NEXT scan lists issue and
--     comment activity from. It is in the FORGE's clock domain (the forge's
--     Date header minus a fixed overlap), except for a first row, which Scan
--     seeds ONCE as fishhawkd's now minus the initial lookback through an
--     insert-if-absent (ON CONFLICT DO NOTHING) so a failed first scan's
--     retry reads with the identical bound.
--   * The row advances ONLY after the scan's report is durably recorded (the
--     ordering lives in backend/internal/userreport/scan.go). It never moves
--     backwards: the upsert's ON CONFLICT ... WHERE cursor_at <
--     EXCLUDED.cursor_at guard.
--   * source keys one row per activity source ("issues" today), so a source
--     that is unreachable never drags another source's cursor past unread
--     items.
--   * Uniqueness is a UNIQUE expression index over COALESCE(account_id, the nil
--     UUID), the 0089 captain_read_watermarks shape, so an untenanted
--     repository cannot accumulate duplicate rows.
--   * The row is operational read position only, NOT a chain fact: dropping
--     the table loses nothing but scan positions, and the next scan simply
--     starts again from the initial lookback.
--
-- Tenant isolation is byte-identical to 0089's captain_read_watermarks policy
-- (itself 0057's audit_entries predicate): account_id IS NULL rows stay visible
-- (the untenanted #1829 window) and an unset/empty app.account_id fails CLOSED
-- to NULL-account rows only. FORCE is required because the application
-- connects as the table owner; the 0057 superuser caveat applies unchanged.

CREATE TABLE user_report_cursors (
    id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id UUID,
    repo       TEXT        NOT NULL,
    source     TEXT        NOT NULL,
    cursor_at  TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX user_report_cursors_key_idx ON user_report_cursors
    (repo, source, COALESCE(account_id, '00000000-0000-0000-0000-000000000000'::uuid));

ALTER TABLE user_report_cursors ENABLE ROW LEVEL SECURITY;
ALTER TABLE user_report_cursors FORCE ROW LEVEL SECURITY;
CREATE POLICY user_report_cursors_tenant_isolation ON user_report_cursors
    USING (account_id IS NULL OR account_id = NULLIF(current_setting('app.account_id', true), '')::uuid)
    WITH CHECK (account_id IS NULL OR account_id = NULLIF(current_setting('app.account_id', true), '')::uuid);
