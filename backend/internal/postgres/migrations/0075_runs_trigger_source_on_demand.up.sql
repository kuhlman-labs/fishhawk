-- 0075: widen runs_trigger_source_check to admit 'on_demand' — the
-- OPERATOR-INITIATED NON-DIFF trigger form (E54.22 / #2826).
--
-- WHY. ADR-065's backlog_grooming workflow declares
-- `applies_to: {trigger: [scheduled, on_demand]}`, but no trigger source
-- mapped onto a non-diff form, so no run could ever satisfy that declaration:
-- the workflow shipped unselectable. run.TriggerOnDemand is that producer, and
-- appliesto.TriggerFormForSource maps it to spec.TriggerOnDemand. Without this
-- migration the domain value exists but every INSERT of an on_demand run is
-- refused by the CHECK from 0001 (0001_runs_stages.up.sql:22), which enumerates
-- only the three v0 sources.
--
-- 'scheduled' is deliberately NOT added: no scheduler exists to mint it, and a
-- persistable-but-unmintable value is the same dead surface #2826 closes.
--
-- A CHECK expression cannot be altered in place, so this DROPs and re-ADDs.
-- golang-migrate wraps the file in one transaction and PostgreSQL's ALTER
-- TABLE ... DROP/ADD CONSTRAINT is transactional DDL, so the swap is ATOMIC: a
-- failure to add the widened constraint leaves the original in place rather
-- than a table with no constraint at all.
--
-- The re-ADD is the CONTROL, not decoration: dropping the constraint alone
-- would also make on_demand insertable, while silently admitting every
-- unrecognized string. TestMigrateDown_RunsTriggerSourceOnDemandReversal pins
-- the difference by asserting a 'nonsense' source is rejected in BOTH
-- migration states.
ALTER TABLE runs
    DROP CONSTRAINT runs_trigger_source_check;

ALTER TABLE runs
    ADD CONSTRAINT runs_trigger_source_check CHECK (
        trigger_source IN ('github_issue', 'cli', 'ui', 'on_demand')
    );
