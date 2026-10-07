-- 0099: the persistence half of the HMAC-authenticated POST /v0/triggers/alert
-- ingress (E35.4 / #1601, ADR-053 option A). Two changes, one migration:
--
--   1. runs_trigger_source_check admits 'alert' — the INCIDENT trigger form;
--   2. alert_incidents — the ingress's dedup ledger.
--
-- They ship together because 0098 was taken by a concurrently-merged change
-- (#4071) and this is the next free prefix; both serve the one ingress, and
-- golang-migrate applies (and reverts) the file in one transaction, so the
-- pair is atomic in both directions.
--
-- ---- 1. runs_trigger_source_check admits 'alert' ----
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
-- A CHECK expression cannot be altered in place, so this DROPs and re-ADDs;
-- PostgreSQL's ALTER TABLE ... DROP/ADD CONSTRAINT is transactional DDL, so
-- the swap is ATOMIC. The re-ADD is the CONTROL, not decoration: dropping the
-- constraint alone would also make 'alert' insertable while silently
-- admitting every unrecognized string.
-- TestMigrateDown_RunsTriggerSourceAlertReversal pins the difference by
-- asserting a 'nonsense' source is rejected in BOTH migration states.
ALTER TABLE runs
    DROP CONSTRAINT runs_trigger_source_check;

ALTER TABLE runs
    ADD CONSTRAINT runs_trigger_source_check CHECK (
        trigger_source IN ('github_issue', 'cli', 'ui', 'on_demand', 'scheduled', 'alert')
    );

-- ---- 2. alert_incidents ----
--
-- backend/internal/alerttrigger's PostgresStore is the only reader and writer
-- of this table; the package README is the contract.
--
-- One row per (source_id, repo, fingerprint): the FIRST alert for a
-- fingerprint inserts the row as a CLAIM (issue_number/issue_url NULL) and
-- files the incident issue; Complete then records the filed issue. A repeat
-- alert finds the row and comments on the existing issue instead of filing a
-- second one. Dedup lives here, not in forge search, because a forge search
-- index is eventually consistent and a fast re-fire could double-file; the
-- primary key makes this row the single writer across instances.
--
-- claim_token is the claimant's identity. claimed_at is DB-stamped (now())
-- and the stale-reclaim cutoff is computed against the DB clock only, so a
-- crashed filer's claim can be taken over without a cross-clock comparison
-- (#3048). Complete and Release are predicated on claim_token, so a late
-- original filer cannot overwrite a newer claimant.
--
-- No account_id and no RLS, like webhook_deliveries and
-- stage_concurrency_slots: this is deployment-level ingress metadata keyed by
-- repo. The ingress authenticates a SOURCE (per-source HMAC secret), not a
-- tenant; multi-tenant scoping of alert sources is a follow-up under E44.
--
-- run_id is the hotfix run the per-source auto_start (default off) started on
-- the filed issue, if any. ON DELETE SET NULL so deleting a run never blocks
-- on, or deletes, its incident's dedup row.
CREATE TABLE alert_incidents (
    source_id     TEXT        NOT NULL,
    repo          TEXT        NOT NULL,
    fingerprint   TEXT        NOT NULL,
    claim_token   UUID        NOT NULL,
    issue_number  INT         NULL CONSTRAINT alert_incidents_issue_number_check CHECK (issue_number > 0),
    issue_url     TEXT        NULL,
    run_id        UUID        NULL REFERENCES runs (id) ON DELETE SET NULL,
    occurrences   INT         NOT NULL DEFAULT 1 CONSTRAINT alert_incidents_occurrences_check CHECK (occurrences >= 1),
    claimed_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (source_id, repo, fingerprint),
    -- A filed row carries BOTH the number and the URL; a claim carries
    -- neither. A half-filed row would read as filed with no link to post.
    CONSTRAINT alert_incidents_issue_pair_check CHECK ((issue_number IS NULL) = (issue_url IS NULL))
);
