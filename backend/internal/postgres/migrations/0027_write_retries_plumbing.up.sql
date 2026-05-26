ALTER TABLE mcp_tokens ADD COLUMN scopes text[] NOT NULL DEFAULT ARRAY['mcp:read'];
ALTER TABLE stages ADD COLUMN self_retry_count int4 NOT NULL DEFAULT 0;
