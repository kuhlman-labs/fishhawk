-- 0046: refinement_drafts — the durable intake-drafting artifact (ADR-052
-- option A drafting half, E34.1 / #1592).
--
-- A refinement draft is the structured epic/children draft an agent produces
-- from a natural-language brief BEFORE anything is filed. It is keyed by a
-- refinement session id (session_id) rather than a run/stage, because drafting
-- happens ahead of any run — no stage exists yet, so the stage-keyed artifacts
-- table cannot hold it. The decoded EpicDraft is stored verbatim as JSONB so a
-- later preview (E34.2) and filing executor (E34.3) reload it byte-for-byte.
--
-- Nothing here files: the row is a draft, not a provider write. The
-- never-files invariant (ADR-052 decision 1) is enforced in the drafting code
-- path, not the schema.
--
-- Schema mirrors 0039_campaigns.up.sql: now()-defaulted timestamps and a
-- gen_random_uuid() PK default (also used by 0012). session_id is indexed so
-- ListForSession is a single index scan.

CREATE TABLE refinement_drafts (
    id          UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    session_id  UUID         NOT NULL,
    brief       TEXT         NOT NULL,
    draft       JSONB        NOT NULL,
    model       TEXT,
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE INDEX refinement_drafts_session_idx ON refinement_drafts (session_id);
