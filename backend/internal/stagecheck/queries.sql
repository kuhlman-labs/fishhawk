-- Stage-checks queries consumed by the postgres adapter for the
-- stagecheck.Repository interface. sqlc generates typed Go into
-- ./db per the config in /backend/sqlc.yaml.

-- name: InsertStageCheck :one
-- Append a stage-check row. Append-only — the latest-per-check
-- semantics live in the read query below.
INSERT INTO stage_checks (
    id, stage_id, check_name, status, conclusion, head_sha,
    github_check_run_id, ts, payload
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: ListStageChecksLatest :many
-- Return one row per (stage_id, check_name) — the most recent state
-- the row table holds. Powers the review-stage page's checks panel
-- and the approval handler's gate-enforcement read.
SELECT DISTINCT ON (check_name) *
  FROM stage_checks
 WHERE stage_id = $1
 ORDER BY check_name, ts DESC;

-- name: GetStageCheckLatest :one
-- Single-check variant of ListStageChecksLatest. Used internally
-- when the approval handler walks a gate's declared blocking_checks
-- and asks "what's the latest state?". Returns ErrNoRows when
-- the check has never been observed (caller maps to not_tracked).
SELECT * FROM stage_checks
 WHERE stage_id = $1 AND check_name = $2
 ORDER BY ts DESC
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
