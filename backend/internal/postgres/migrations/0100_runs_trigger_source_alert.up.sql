-- 0100: widen runs_trigger_source_check to admit 'alert' — the INCIDENT
-- trigger form minted by the HMAC-authenticated POST /v0/triggers/alert
-- ingress's optional auto-start (E35.4 / #1601, ADR-053 option A). The
-- ingress's dedup ledger, alert_incidents, is 0099.
--
-- WHY. A verified alert files an incident issue; when the alert source's
-- per-source auto_start is true (it defaults to false) the ingress starts a
-- hotfix run on that issue through server.StartAlertRun, which stamps
-- run.TriggerAlert on the run row. Without this widening every INSERT of an
-- alert run is refused by the five-value CHECK from 0093.
--
-- 'alert' is SYSTEM-ONLY: POST /v0/runs refuses it from any caller other than
-- the in-process auto-start (400 trigger_source_reserved). The CHECK only
-- governs what is STORABLE; admission is enforced in the handler.
--
-- A CHECK expression cannot be altered in place, so this DROPs and re-ADDs.
-- golang-migrate wraps the file in one transaction and PostgreSQL's ALTER
-- TABLE ... DROP/ADD CONSTRAINT is transactional DDL, so the swap is ATOMIC.
--
-- The re-ADD is the CONTROL, not decoration: dropping the constraint alone
-- would also make 'alert' insertable while silently admitting every
-- unrecognized string. TestMigrateDown_RunsTriggerSourceAlertReversal pins
-- the difference by asserting a 'nonsense' source is rejected in BOTH
-- migration states.
ALTER TABLE runs
    DROP CONSTRAINT runs_trigger_source_check;

ALTER TABLE runs
    ADD CONSTRAINT runs_trigger_source_check CHECK (
        trigger_source IN ('github_issue', 'cli', 'ui', 'on_demand', 'scheduled', 'alert')
    );
