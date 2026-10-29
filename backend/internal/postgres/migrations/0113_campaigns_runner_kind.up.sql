-- 0113: record the runner kind ONCE at fishhawk_start_campaign so every item
-- run minted from the campaign inherits it (#4485). Before this, an item run
-- whose call omitted runner_kind silently defaulted to github_actions even when
-- the campaign was bound to a local checkout (0071's working_dir), so the
-- operator re-typed runner_kind on every start_campaign_item_run call.
--
-- NOT NULL DEFAULT '' rather than nullable — the exact shape working_dir took
-- in 0071 — so the Go mapping stays a plain string and 'not recorded' is
-- exactly ONE value (empty) instead of two (NULL vs ''). Every pre-existing
-- campaign reads back unrecorded, and the item-run handler then resolves the
-- runner kind from the campaign's working_dir binding ('local' when bound, the
-- github_actions default otherwise).
--
-- No CHECK constraint. Validation against the known runner kinds lives in the
-- handler (run.ValidRunnerKinds); a CHECK here would need a migration for every
-- new runner kind. Touches campaigns and nothing else.

ALTER TABLE campaigns ADD COLUMN IF NOT EXISTS runner_kind TEXT NOT NULL DEFAULT '';
