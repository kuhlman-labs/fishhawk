-- Run / stage queries consumed by the postgres adapter for the
-- run.Repository interface (E3.3 / #43). sqlc generates typed Go
-- into ./db per the config in /backend/sqlc.yaml.

-- name: CreateRun :one
INSERT INTO runs (id, repo, workflow_id, workflow_sha, trigger_source, trigger_ref, state, installation_id, idempotency_key)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: GetRun :one
SELECT * FROM runs WHERE id = $1;

-- name: GetRunByIdempotencyKey :one
-- Used by POST /v0/runs to resolve an Idempotency-Key header to
-- a previously-created run. Active scope is (repo, idempotency_key);
-- the partial unique index covers this lookup with no full scan.
SELECT * FROM runs
 WHERE repo = $1
   AND idempotency_key = $2;

-- name: ListRuns :many
-- Empty string in any filter means "no constraint." created_at DESC
-- + id DESC tiebreak so paginations are stable across concurrent
-- inserts at the same created_at microsecond.
SELECT * FROM runs
 WHERE (sqlc.arg('repo')::text = '' OR repo = sqlc.arg('repo'))
   AND (sqlc.arg('workflow_id')::text = '' OR workflow_id = sqlc.arg('workflow_id'))
   AND (sqlc.arg('state')::text = '' OR state = sqlc.arg('state'))
 ORDER BY created_at DESC, id DESC
 LIMIT sqlc.arg('lim') OFFSET sqlc.arg('off');

-- name: LockRunForUpdate :one
SELECT * FROM runs WHERE id = $1 FOR UPDATE;

-- name: UpdateRunState :one
UPDATE runs
   SET state = $2
 WHERE id = $1
RETURNING *;

-- name: CreateStage :one
INSERT INTO stages (id, run_id, sequence, stage_type, executor_kind, executor_ref, state, gate_sla, requires_approval)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: GetStage :one
SELECT * FROM stages WHERE id = $1;

-- name: ListStagesForRun :many
SELECT * FROM stages WHERE run_id = $1 ORDER BY sequence ASC;

-- name: LockStageForUpdate :one
SELECT * FROM stages WHERE id = $1 FOR UPDATE;

-- name: ListStagesAwaitingApproval :many
-- Used by the SLA ticker to find candidates for timeout. Filters
-- to stages in awaiting_approval state with a non-null gate_sla so
-- the ticker doesn't pay for SLA parsing on rows where it isn't
-- applicable. Ordered by updated_at ASC: the oldest entry is the
-- most likely to be past SLA, so the ticker can early-exit if the
-- first row hasn't elapsed (when the parsed durations are uniform).
SELECT * FROM stages
 WHERE state = 'awaiting_approval'
   AND gate_sla IS NOT NULL
 ORDER BY updated_at ASC;

-- name: ListStagesDispatched :many
-- Used by the dispatch watchdog (E8.4) to find stages stuck at
-- 'dispatched' past a configurable timeout. Ordered by updated_at
-- ASC so the oldest stuck stage is processed first; lets the
-- watchdog early-exit once it sees one that's still within the
-- window.
SELECT * FROM stages
 WHERE state = 'dispatched'
 ORDER BY updated_at ASC;

-- name: UpdateStageState :one
UPDATE stages
   SET state            = $2,
       started_at       = COALESCE(started_at, $3),
       ended_at         = $4,
       failure_category = $5,
       failure_reason   = $6
 WHERE id = $1
RETURNING *;
