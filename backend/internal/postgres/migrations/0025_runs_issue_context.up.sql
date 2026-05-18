-- 0025: cache the triggering GitHub issue's title/body/url/number
-- alongside the run row (#415).
--
-- Background: the prompt builder's fillIssueContext (server/prompt.go)
-- fetches the issue's title + body via the GitHub App's installation
-- token. CLI-minted runs don't carry an installation_id, so for the
-- local-runner dev loop (E22 / #389) the prompt was degraded — the
-- agent got a URL but no body. #415 closes that gap by having the
-- operator's authed `gh` CLI fetch the issue at run-create time and
-- ship the content inline on POST /v0/runs. This column is where the
-- backend caches it.
--
-- Shape: a small JSONB blob with title, body, url, number. The
-- prompt builder reads it back into a prompt.Trigger. Stored as
-- JSONB (not separate columns) because the field is opaque to the
-- query layer — nothing filters or sorts on it — and the schema
-- can evolve (e.g. labels, assignees) without another migration.
--
-- Nullable: webhook-dispatched runs leave this field empty and the
-- prompt builder falls back to the existing GitHub-fetch path
-- (unchanged behavior on the GHA flow).
--
-- No index: never queried, only fetched alongside its run row.

ALTER TABLE runs
    ADD COLUMN issue_context JSONB NULL;
