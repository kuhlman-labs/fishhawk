-- 0022: extend the approvals.surface CHECK constraint with the
-- `github_reply_comment` value (E17.4 / #339).
--
-- ADR-020 / #321 chose plan-as-issue-comment-thread as the canonical
-- plan-review surface. The lightweight approval signal is a typed
-- reply comment (`+1`, `lgtm`, `👍`, etc.) on the originating issue;
-- E17.3 / #338 added the matcher, and this migration is what lets
-- E17.4 / #339 record those approvals with a distinct Surface value
-- so the audit log can tell a slash command apart from a reply.
--
-- Why not reuse `github_comment`: existing rows tagged
-- `github_comment` came from explicit `/fishhawk approve` slash
-- commands. The reply-comment path is a different operator action
-- (no command word required) with different UX semantics (silent
-- skip on missing context vs. explicit help reply). The Surface
-- field is the discriminator a post-hoc reviewer / compliance
-- consumer reads to attribute the decision; collapsing the two
-- would lose that distinction.
--
-- A future `github_reaction` value will land alongside the polling
-- worker in E17.3b / #360. Filing one migration per Surface value
-- keeps the upgrade history obvious.

ALTER TABLE approvals
    DROP CONSTRAINT approvals_surface_check;

ALTER TABLE approvals
    ADD CONSTRAINT approvals_surface_check
    CHECK (surface IN ('api', 'ui', 'cli', 'github_comment', 'github_reply_comment'));
