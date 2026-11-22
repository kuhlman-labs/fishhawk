-- 0119: campaign_item_start_claims — a campaign item start reserves its item
-- BEFORE it mints a run (#5266).
--
-- Why: the operator item start and the campaign driver can race to start the
-- same item. Each claims the item here first, under the campaign_items row
-- lock, and links its minted run only while it still holds the claim. A
-- losing starter is refused before it mints, so it never creates a run the
-- host dispatcher could admit (it admits unlinked auto_dispatch runs:
-- mcpserver/host_dispatcher.go eligible / campaignPermits). The primary key
-- allows ONE claim per item; an expired claim (a crashed starter) is
-- replaced by the next claimant, and a link or release deletes the row.
--
-- Clock domain: claimed_at / expires_at are stamped by fishhawkd from its own
-- clock (never Postgres now()) and compared only against that same clock
-- (docs/agents/testing.md, #3048).
--
-- Tenancy: like campaign_items and the 0106 tables it carries no account_id
-- and no RLS; tenancy is the parent campaign row's, enforced by the caller.
-- It is a hand-written-pgx child table rather than new campaign_items
-- columns, so the sqlc SELECT * / RETURNING * scans of campaign_items are
-- untouched. A claim cascades with its item.
CREATE TABLE campaign_item_start_claims (
    item_id    UUID        PRIMARY KEY REFERENCES campaign_items (id) ON DELETE CASCADE,
    claim_id   UUID        NOT NULL,
    starter    TEXT        NOT NULL CONSTRAINT campaign_item_start_claims_starter_check
                           CHECK (starter IN ('operator', 'campaign_driver')),
    claimed_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT campaign_item_start_claims_window_check CHECK (expires_at > claimed_at)
);
