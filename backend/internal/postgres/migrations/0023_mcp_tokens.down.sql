-- 0023 (down): drop the mcp_tokens table. The MCP runner-
-- integration path (E19.8 / #348) is unavailable after this
-- migration; in-runner agents fall back to no Fishhawk awareness
-- mid-execution.

DROP TABLE mcp_tokens;
