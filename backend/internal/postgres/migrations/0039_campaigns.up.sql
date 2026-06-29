-- 0039: campaigns and campaign_items tables — the durable campaign object
-- (Track B keystone of ADR-047 / #1437, E25.2).
--
-- A campaign is the parent record for an epic-driven multi-issue run: it
-- owns an ordered set of campaign_items, one per issue under the epic. The
-- campaign state is a reduction of its items (pending → running →
-- {succeeded, failed, cancelled}); an item walks pending → blocked →
-- running → {succeeded, failed, cancelled}, where `blocked` means its
-- depends_on edges are not yet satisfied.
--
-- The run ↔ campaign cross-boundary link lives on campaign_items.run_id (a
-- nullable FK to runs, ON DELETE SET NULL) so a campaign's issue-runs are
-- discoverable via the item rows without touching the hot runs table.
-- SET NULL (not CASCADE) so deleting a run preserves campaign history —
-- the item row survives with run_id nulled.
--
-- Schema mirrors 0001_runs_stages.up.sql: TEXT state columns with CHECK
-- constraints (human-readable audit values), now()-defaulted timestamps,
-- and BEFORE UPDATE triggers reusing the shared fishhawk_set_updated_at()
-- function defined in 0001 (NOT redefined here).

CREATE TABLE campaigns (
    id          UUID         PRIMARY KEY,
    repo        TEXT         NOT NULL,
    epic_ref    TEXT         NOT NULL,
    state       TEXT         NOT NULL DEFAULT 'pending',
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT campaigns_state_check CHECK (
        state IN ('pending', 'running', 'succeeded', 'failed', 'cancelled')
    )
);

CREATE INDEX campaigns_repo_state_idx ON campaigns (repo, state);

CREATE TABLE campaign_items (
    id           UUID         PRIMARY KEY,
    campaign_id  UUID         NOT NULL REFERENCES campaigns (id) ON DELETE CASCADE,
    issue_ref    TEXT         NOT NULL,
    depends_on   JSONB        NOT NULL DEFAULT '[]',
    run_id       UUID         REFERENCES runs (id) ON DELETE SET NULL,
    state        TEXT         NOT NULL DEFAULT 'pending',
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (campaign_id, issue_ref),
    CONSTRAINT campaign_items_state_check CHECK (
        state IN ('pending', 'blocked', 'running', 'succeeded', 'failed', 'cancelled')
    )
);

CREATE INDEX campaign_items_campaign_idx ON campaign_items (campaign_id);
CREATE INDEX campaign_items_run_idx ON campaign_items (run_id);

CREATE TRIGGER campaigns_set_updated_at      BEFORE UPDATE ON campaigns      FOR EACH ROW EXECUTE FUNCTION fishhawk_set_updated_at();
CREATE TRIGGER campaign_items_set_updated_at BEFORE UPDATE ON campaign_items FOR EACH ROW EXECUTE FUNCTION fishhawk_set_updated_at();
