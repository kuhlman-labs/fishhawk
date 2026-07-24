-- 0061 (down): revert the users.provider discriminator.
--
-- This reversal is NOT unconditionally lossless once GitLab sign-ins exist. The
-- down migration restores the single-column UNIQUE (github_user_id), but a
-- GitLab user id can equal a GitHub user id (independent forge sequences), so
-- two rows sharing a github_user_id would violate that constraint. Reversing to
-- a github-only schema therefore REMOVES the gitlab-provider rows first — they
-- have no representation in the pre-0061 schema — so the reversal succeeds even
-- after GitLab sign-ins and even when a GitLab id collides with a GitHub row.
-- The github-provider rows are left intact.

-- Drop gitlab-provider identities BEFORE restoring the single-column UNIQUE so a
-- (gitlab, N) row cannot collide with a surviving (github, N) row. ON DELETE
-- CASCADE on sessions.user_id drops their sessions with them.
DELETE FROM users WHERE provider = 'gitlab';

ALTER TABLE users DROP CONSTRAINT users_provider_github_user_id_key;
ALTER TABLE users ADD CONSTRAINT users_github_user_id_key UNIQUE (github_user_id);

ALTER TABLE users
    DROP CONSTRAINT users_provider_check,
    DROP COLUMN provider;
