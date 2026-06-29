-- 0042: idempotency keys on campaigns (E25.13 / #1455).
--
-- POST /v0/campaigns already ACCEPTS an Idempotency-Key header but,
-- pre-0042, could not honor it: the campaigns table had no key column
-- and the repository exposed no dedup lookup, so every retry minted a
-- duplicate campaign. This mirrors the run-create idempotency contract
-- (0011 on runs): the server stores the key with the campaign and
-- returns the existing row on a duplicate POST.
--
-- Scope: (repo, idempotency_key). The same repo can't double-trigger
-- with the same key; cross-repo collisions don't matter — each caller
-- picks their own key.
--
-- Nullable: existing/pre-0042 campaigns and any driver-created campaign
-- with no key legitimately have none. The unique index is partial over
-- WHERE idempotency_key IS NOT NULL so NULLs never collide.

ALTER TABLE campaigns
    ADD COLUMN idempotency_key TEXT;

CREATE UNIQUE INDEX campaigns_idempotency_key_repo_idx
    ON campaigns (repo, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
