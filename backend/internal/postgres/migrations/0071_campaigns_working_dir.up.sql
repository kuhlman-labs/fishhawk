-- 0071: bind the caller's checkout ONCE at fishhawk_start_campaign so every
-- item run minted from the campaign inherits it (E48.87 / #2527). #2498 bound
-- the checkout per RUN (0065); a campaign is a batch of runs, so the operator
-- re-typed an identical absolute path on every start_campaign_item_run call.
-- This column records the checkout once at the CAMPAIGN and the item-run
-- handler resolves the minted run's binding through it.
--
-- NOT NULL DEFAULT '' rather than nullable — the exact shape runs.working_dir
-- took in 0065 — so the Go mapping stays a plain string and 'no binding' is
-- exactly ONE value (empty) instead of two (NULL vs ''). Every pre-existing
-- campaign reads back unbound, which is the unchanged-behavior default: the
-- item-run handler then falls through to the explicit per-item parameter #2498
-- shipped, exactly as it does today.
--
-- No CHECK constraint. Absolute-path validation is transport- and
-- runner-kind-conditional (a github_actions campaign has no local checkout at
-- all) and belongs above the database, in the handler and the MCP tool.
-- Touches campaigns and nothing else.

ALTER TABLE campaigns ADD COLUMN IF NOT EXISTS working_dir TEXT NOT NULL DEFAULT '';
