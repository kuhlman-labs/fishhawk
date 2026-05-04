-- 0011: idempotency keys on runs (E8.2 / #40).
--
-- Per MVP_SPEC §6, all four failure categories support
-- re-execution. A retry that goes through POST /v0/runs (CLI /
-- UI) needs to be safe to repeat: a network blip + replay must
-- not double-trigger the agent. The standard answer is an
-- Idempotency-Key header (RFC-style) where the server stores the
-- key with the resource and returns the existing row on
-- duplicate.
--
-- Scope: (idempotency_key, repo). Same repo can't double-trigger
-- with the same key. Cross-repo collisions don't matter — each
-- caller picks their own key. The webhook-driven path
-- (/webhooks/github) already dedups via X-GitHub-Delivery
-- (E3.9 / #110); idempotency_key serves the manually-triggered
-- entry points.
--
-- Nullable: webhook-driven runs and any pre-0011 rows
-- legitimately have no key. The unique index is partial over
-- WHERE idempotency_key IS NOT NULL so NULLs don't collide.

ALTER TABLE runs
    ADD COLUMN idempotency_key TEXT;

CREATE UNIQUE INDEX runs_idempotency_key_repo_idx
    ON runs (repo, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
