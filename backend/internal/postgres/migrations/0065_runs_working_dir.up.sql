-- 0065: bind the caller's checkout to the run once at start_run (E66.42 /
-- #2482). The MCP working_dir refusal (#2479) otherwise costs a re-typed
-- absolute path on every runner-spawning verb; this column records the
-- checkout ONCE so every later verb inherits it.
--
-- NOT NULL DEFAULT '' rather than nullable so the Go mapping stays a plain
-- string and 'no binding' is exactly ONE value (empty) instead of two
-- (NULL vs ''). The empty string IS the legacy/unbound state the #2479
-- refusal still catches: a pre-#2482 row, or a run created without a
-- binding, reads back '' and the MCP resolver refuses over HTTP exactly as
-- it does today.
--
-- No CHECK constraint. Absolute-path validation is transport-conditional
-- (a github_actions run has no local checkout at all) and belongs in the
-- MCP layer, not the database. Touches runs and nothing else.

ALTER TABLE runs ADD COLUMN IF NOT EXISTS working_dir TEXT NOT NULL DEFAULT '';
