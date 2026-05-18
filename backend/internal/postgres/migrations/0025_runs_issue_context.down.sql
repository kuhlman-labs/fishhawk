-- 0025 down: drop the issue_context column. CLI-minted runs lose
-- the cached issue body until a re-migration; prompts fall back to
-- the "URL only" shape for runs without an installation_id.
ALTER TABLE runs
    DROP COLUMN IF EXISTS issue_context;
