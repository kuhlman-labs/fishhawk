-- 0093: widen runs_trigger_source_check to admit 'scheduled' — the CADENCE
-- trigger form minted by the in-process scheduler (E79.1 / #3725).
--
-- WHY. A workflow declaring `applies_to: {trigger: [scheduled, ...]}` plus a
-- workflow-level `schedule` is started by fishhawkd's scheduler through
-- server.StartScheduledRun, which stamps run.TriggerScheduled on the run row.
-- 0075 deliberately left 'scheduled' out because nothing could mint it; the
-- scheduler is that producer. Without this migration every INSERT of a
-- scheduled run is refused by the four-value CHECK from 0075.
--
-- 'scheduled' is SYSTEM-ONLY: POST /v0/runs refuses it from any caller other
-- than the in-process scheduler (400 trigger_source_reserved). The CHECK only
-- governs what is STORABLE; admission is enforced in the handler.
--
-- A CHECK expression cannot be altered in place, so this DROPs and re-ADDs.
-- golang-migrate wraps the file in one transaction and PostgreSQL's ALTER
-- TABLE ... DROP/ADD CONSTRAINT is transactional DDL, so the swap is ATOMIC.
--
-- The re-ADD is the CONTROL, not decoration: dropping the constraint alone
-- would also make 'scheduled' insertable while silently admitting every
-- unrecognized string. TestMigrateDown_RunsTriggerSourceScheduledReversal
-- pins the difference by asserting a 'nonsense' source is rejected in BOTH
-- migration states.
ALTER TABLE runs
    DROP CONSTRAINT runs_trigger_source_check;

ALTER TABLE runs
    ADD CONSTRAINT runs_trigger_source_check CHECK (
        trigger_source IN ('github_issue', 'cli', 'ui', 'on_demand', 'scheduled')
    );
