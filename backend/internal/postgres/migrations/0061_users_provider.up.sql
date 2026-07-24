-- 0061: users.provider discriminator (E44.22 / #2109) — make the sign-in
-- identity forge-neutral so a GitLab browser sign-in can land alongside GitHub.
--
-- The users table (0010) keyed identity on github_user_id ALONE with a single-
-- column UNIQUE. GitLab numeric ids and GitHub numeric ids are independent
-- sequences, so a GitLab user id N and a GitHub user id N would collide on that
-- constraint — the GitLab sign-in would silently upsert (overwrite) the GitHub
-- user's row. This migration adds a `provider` discriminator (mirroring
-- accounts/installations 0052) and swaps the single-column UNIQUE for the
-- composite UNIQUE (provider, github_user_id), so the same numeric id in two
-- forges is two distinct rows.
--
-- Backward-compatible for existing github-only rows: provider defaults to
-- 'github', so every pre-existing row keeps its identity and the composite is a
-- strict superset of the old single-column UNIQUE for those rows.

ALTER TABLE users
    ADD COLUMN provider TEXT NOT NULL DEFAULT 'github',
    ADD CONSTRAINT users_provider_check CHECK (provider IN ('github', 'gitlab'));

-- Drop the single-column UNIQUE (named by Postgres's default: <table>_<col>_key)
-- and replace it with the forge-scoped composite so a GitLab id can never
-- overwrite a GitHub user's row.
ALTER TABLE users DROP CONSTRAINT users_github_user_id_key;
ALTER TABLE users ADD CONSTRAINT users_provider_github_user_id_key UNIQUE (provider, github_user_id);
