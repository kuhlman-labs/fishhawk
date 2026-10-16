-- 0105: a terminal `lapsed` status for scope_amendments (E83.64 / #4127).
--
-- lapsed = the request was still undecided (`pending`) when the run could no
-- longer use a decision: its stage reached succeeded, cancelled or
-- superseded, or its run reached succeeded or cancelled. A stage that FAILS,
-- or a run that FAILS (fishhawk_retry_stage reopens a failed run), does NOT
-- lapse its requests: they stay pending so a late decision plus
-- fishhawk_retry_stage still folds the paths. A lapsed row is never
-- approvable; Decide refuses it.
--
-- Rows are written to `lapsed` ONLY by fishhawkd (the run repository's
-- post-commit terminal observers and the one-shot startup backfill), never by
-- SQL here: every lapse carries a chained scope_amendment_lapsed audit entry,
-- and the chain hash is computed in Go, so a SQL backfill could not write a
-- valid entry. The historical stale rows are lapsed on the first fishhawkd
-- start after this migration.

ALTER TABLE scope_amendments
    DROP CONSTRAINT scope_amendments_status_check,
    ADD CONSTRAINT scope_amendments_status_check CHECK (
        status IN ('pending', 'approved', 'denied', 'lapsed')
    );
