-- 0023: mcp_tokens table for runner-scoped MCP server credentials
-- (E19.8 / #348). Separate from api_tokens so the surface stays
-- distinct from the operator-facing CLI / UI tokens:
--
--   - Per-run scoping: each token is bound to a run_id (FK with
--     ON DELETE CASCADE so revoking the run revokes the token).
--     Future per-endpoint scope enforcement reads this column
--     directly rather than parsing a subject string.
--   - Short TTL: expires_at is NOT NULL. The auth path rejects
--     expired rows without revoking them — the partial index on
--     (token_hash) WHERE revoked_at IS NULL covers the auth-time
--     hot path; expiry is a row-level check.
--   - Independent audit policy: mcp_tokens can grow per-auth
--     audit later (the security-sensitive surface argument for
--     option 2 in the E19.8 design conversation) without
--     inflating api_tokens audit volume.
--   - Format-independent: today the plaintext is the `fhm_`-
--     prefixed hash-shadowed shape mirroring apitoken's. If we
--     ever migrate to JWTs (so the MCP server can verify
--     locally), the schema stays as a revocation log rather than
--     the source of truth — same shape, different semantics.
--
-- Mirrors signing_keys' per-run-lifecycle pattern (#218) at the
-- infrastructure level: indexed by run_id, expires_at the
-- load-bearing protection, append-then-revoke immutability via
-- triggers (added in a future hardening migration if needed —
-- mcp_tokens is too small to justify the trigger now).

CREATE TABLE mcp_tokens (
    id           UUID         PRIMARY KEY,
    run_id       UUID         NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    token_hash   TEXT         NOT NULL UNIQUE,
    issued_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ  NOT NULL,
    last_used_at TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ
);

-- Auth-time lookup: WHERE token_hash = $1 AND revoked_at IS NULL.
-- Partial index keeps revoked rows out of the hot path.
CREATE UNIQUE INDEX mcp_tokens_active_hash_idx
    ON mcp_tokens (token_hash)
    WHERE revoked_at IS NULL;

-- Revocation by run scope: when a run cancels, mark every token
-- for that run revoked. Cascade FK handles the delete case; this
-- index covers the revoke-but-keep-the-history case.
CREATE INDEX mcp_tokens_run_idx ON mcp_tokens (run_id);
