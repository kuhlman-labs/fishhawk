-- Run / stage queries consumed by the postgres adapter for the
-- run.Repository interface (E3.3 / #43). sqlc generates typed Go
-- into ./db per the config in /backend/sqlc.yaml.

-- name: CreateRun :one
INSERT INTO runs (id, repo, workflow_id, workflow_sha, trigger_source, trigger_ref, state)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetRun :one
SELECT * FROM runs WHERE id = $1;

-- name: LockRunForUpdate :one
SELECT * FROM runs WHERE id = $1 FOR UPDATE;

-- name: UpdateRunState :one
UPDATE runs
   SET state = $2
 WHERE id = $1
RETURNING *;

-- name: CreateStage :one
INSERT INTO stages (id, run_id, sequence, stage_type, executor_kind, executor_ref, state)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetStage :one
SELECT * FROM stages WHERE id = $1;

-- name: ListStagesForRun :many
SELECT * FROM stages WHERE run_id = $1 ORDER BY sequence ASC;

-- name: LockStageForUpdate :one
SELECT * FROM stages WHERE id = $1 FOR UPDATE;

-- name: UpdateStageState :one
UPDATE stages
   SET state            = $2,
       started_at       = COALESCE(started_at, $3),
       ended_at         = $4,
       failure_category = $5,
       failure_reason   = $6
 WHERE id = $1
RETURNING *;
