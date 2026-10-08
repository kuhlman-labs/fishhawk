-- 0099: alert_incidents — the dedup ledger of the HMAC-authenticated
-- POST /v0/triggers/alert ingress (E35.4 / #1601, ADR-053 option A).
--
-- The companion widening of runs_trigger_source_check to admit 'alert' is
-- 0100 (a separate migration so each change reverts on its own; roll 0100
-- back before 0099).
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
