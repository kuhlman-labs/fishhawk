-- 0106: campaign_item_expedites + campaign_dispatch_holds — the campaign
-- hotfix verb's persistence (E83.100 / #4234).
--
-- campaign_item_expedites marks an item as EXPEDITED: it was added by the
-- hotfix verb, heads the campaign queue (its queue_position is
-- min(existing) - 1, so it may be NEGATIVE — 0074 put no CHECK on
-- queue_position), and is run under its recorded workflow. A normal item has
-- no row. reason is the operator's free-text justification; it is
-- untrusted-origin text and is rendered only as data.
--
-- campaign_dispatch_holds is the OPTIONAL dispatch hold: while a row exists,
-- the host-dispatch marker refuses every NEW implement admission of the
-- campaign's non-expedited items. The primary key allows ONE hold per
-- campaign. held_at / expires_at are stamped by fishhawkd from its own clock
-- (never Postgres now()) and compared against that same clock, so the
-- release decision stays in one clock domain (AGENTS.md trap #3048). The
-- release DELETES the row, recorded before commit by a chained
-- campaign_hold_released audit entry.
--
-- Both are hand-written-pgx child tables of campaigns / campaign_items rather
-- than new campaign_items columns, so the sqlc SELECT * / RETURNING * scans of
-- campaign_items are untouched. Like campaign_items they carry no account_id
-- and no RLS: tenancy is the parent campaign row's, enforced by the caller
-- before any read or write. Both rows cascade with their campaign and item.
CREATE TABLE campaign_item_expedites (
    item_id     UUID        PRIMARY KEY REFERENCES campaign_items (id) ON DELETE CASCADE,
    campaign_id UUID        NOT NULL REFERENCES campaigns (id) ON DELETE CASCADE,
    kind        TEXT        NOT NULL CONSTRAINT campaign_item_expedites_kind_check
                            CHECK (kind IN ('hotfix')),
    workflow    TEXT        NOT NULL CONSTRAINT campaign_item_expedites_workflow_check
                            CHECK (workflow <> ''),
    reason      TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX campaign_item_expedites_campaign_idx ON campaign_item_expedites (campaign_id);

CREATE TABLE campaign_dispatch_holds (
    campaign_id    UUID        PRIMARY KEY REFERENCES campaigns (id) ON DELETE CASCADE,
    hotfix_item_id UUID        NOT NULL REFERENCES campaign_items (id) ON DELETE CASCADE,
    held_at        TIMESTAMPTZ NOT NULL,
    expires_at     TIMESTAMPTZ NOT NULL,
    CONSTRAINT campaign_dispatch_holds_window_check CHECK (expires_at > held_at)
);
