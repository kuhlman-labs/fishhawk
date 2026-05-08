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
-- Locate the run + stages whose pull_request artifact matches the
-- given (pr_number, head_sha) and whose gate carries the given
-- check name. Used by the GitHub check_run webhook ingest path:
-- one event arrives, this query returns every stage that should
-- record a row.
--
-- Walks artifacts → stages → runs → stages-on-the-same-run filtered
-- by gate_blocking_checks containing the check name. v0 keeps it
-- as a single query so the ingest hot path doesn't roundtrip the
-- DB N times per event.
SELECT s.*
  FROM artifacts a
  JOIN stages s_pr ON s_pr.id = a.stage_id
  JOIN runs r ON r.id = s_pr.run_id
  JOIN stages s ON s.run_id = r.id
 WHERE a.kind = 'pull_request'
   AND (a.content->>'pr_number')::int = sqlc.arg('pr_number')::int
   AND (a.content->>'head_sha') = sqlc.arg('head_sha')::text
   AND sqlc.arg('check_name')::text = ANY(s.gate_blocking_checks)
 ORDER BY s.sequence ASC;
