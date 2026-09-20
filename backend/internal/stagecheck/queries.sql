-- Stage-checks queries consumed by the postgres adapter for the
-- stagecheck.Repository interface. sqlc generates typed Go into
-- ./db per the config in /backend/sqlc.yaml.

-- name: InsertStageCheck :one
-- Append a stage-check row. Append-only — the latest-per-check
-- semantics live in the read query below.
INSERT INTO stage_checks (
    id, stage_id, check_name, status, conclusion, head_sha,
    github_check_run_id, ts, payload, gitlab_pipeline_id
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING *;

-- name: ListStageChecksLatest :many
-- Return one row per (stage_id, check_name) — the most recent state
-- the row table holds. Powers the review-stage page's checks panel
-- and the deploy gate's ci_green read.
--
-- PRECEDENCE IS STRUCTURAL (E45.55 / #3490): within (stage,
-- 'gitlab/pipeline') the highest gitlab_pipeline_id is authoritative
-- regardless of ts or webhook delivery order — the same head_sha can
-- carry several pipelines (retry / manual re-run) and GitLab ids are
-- monotonically increasing, so an older id appended LATER can never
-- become the latest row. GitHub rows carry NULL and sort LAST on that
-- key, so their ts ordering is unchanged. DISTINCT ON requires the
-- ORDER BY to lead with check_name. Mirrored verbatim in
-- db/queries.sql.go (the Go const is what executes).
SELECT DISTINCT ON (check_name) *
  FROM stage_checks
 WHERE stage_id = $1
 ORDER BY check_name, gitlab_pipeline_id DESC NULLS LAST, ts DESC;

-- name: GetStageCheckLatest :one
-- Single-check variant of ListStageChecksLatest. Used internally
-- when a reader asks "what's the latest state of this one check?"
-- (the GitLab pipeline ingester's write-avoidance read). Returns
-- ErrNoRows when the check has never been observed (caller maps to
-- not_tracked).
--
-- Same structural precedence as ListStageChecksLatest: the highest
-- gitlab_pipeline_id wins within (stage, check_name) regardless of ts
-- or delivery order; NULL-id GitHub rows sort last on that key and
-- keep pure ts ordering. Mirrored verbatim in db/queries.sql.go.
SELECT * FROM stage_checks
 WHERE stage_id = $1 AND check_name = $2
 ORDER BY gitlab_pipeline_id DESC NULLS LAST, ts DESC
 LIMIT 1;

-- name: FindRunStagesForCheckRun :many
-- Locate the review stage of every run whose pull_request artifact
-- matches the given (pr_number, head_sha). Used by the GitHub
-- check_run webhook ingest path: one event arrives, this query
-- returns every review stage that should record a row.
--
-- Walks artifacts → implement-stage → run → review-stage. Pre-#254
-- this filtered on the spec-level gate's blocking_checks list; that
-- field was dropped in v0.2 (ADR-017 / #249). Required CI checks
-- now live in branch protection (#251), and the review stage is the
-- canonical recording target — it's the only stage whose gate is
-- meaningfully tied to merge state.
--
-- check_name is accepted as a parameter so the existing call sites
-- don't need to change shape; v0 records every observed check
-- against the review stage regardless of declared list.
SELECT s.*
  FROM artifacts a
  JOIN stages s_pr ON s_pr.id = a.stage_id
  JOIN runs r ON r.id = s_pr.run_id
  JOIN stages s ON s.run_id = r.id
 WHERE a.kind = 'pull_request'
   AND (a.content->>'pr_number')::int = sqlc.arg('pr_number')::int
   AND (a.content->>'head_sha') = sqlc.arg('head_sha')::text
   AND s.stage_type = 'review'
   AND sqlc.arg('check_name')::text != ''
 ORDER BY s.sequence ASC;

-- name: FindRunStagesForGitLabPipeline :many
-- GitLab Pipeline Hook sibling of FindRunStagesForCheckRun (E45.55 /
-- #3490). Locate the review stage of every run whose pull_request
-- artifact matches the pipeline's head_sha (and, when the hook names a
-- merge request, its iid as pr_number; mr_iid = 0 disables that
-- predicate so a branch pipeline still matches by sha).
--
-- BOTH run predicates are LOAD-BEARING. GitLab pipeline hooks are
-- delivered per project, and a fork of the same project can carry the
-- SAME head_sha and the SAME merge-request iid; head_sha + iid alone
-- would therefore route a fork's pipeline onto the upstream run (or
-- vice versa). runs.repo pins the project path and
-- runs.installation_ref pins the credential scope ("gitlab:<project_id>")
-- the webhook was authorized under — deleting either admits a decoy
-- that shares the other (pinned by
-- TestFindMatchingStagesForGitLabPipeline_ProjectScoped with one decoy
-- per predicate).
--
-- Returns (stage_id, run_id) pairs: the ingester appends to the stage
-- and re-runs the post-CI policy evaluation for the run.
SELECT s.id AS stage_id, s.run_id
  FROM artifacts a
  JOIN stages s_pr ON s_pr.id = a.stage_id
  JOIN runs r ON r.id = s_pr.run_id
  JOIN stages s ON s.run_id = r.id
 WHERE a.kind = 'pull_request'
   AND (a.content->>'head_sha') = sqlc.arg('head_sha')::text
   AND (sqlc.arg('mr_iid')::int = 0 OR (a.content->>'pr_number')::int = sqlc.arg('mr_iid')::int)
   AND r.repo = sqlc.arg('repo')::text
   AND r.installation_ref = sqlc.arg('installation_ref')::text
   AND s.stage_type = 'review'
 ORDER BY r.created_at ASC, s.sequence ASC;
