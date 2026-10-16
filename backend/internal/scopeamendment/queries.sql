-- Scope amendment queries (E22.X / #961). sqlc generates typed Go
-- into ./db per backend/sqlc.yaml.

-- name: CreateScopeAmendment :one
INSERT INTO scope_amendments (id, run_id, stage_id, paths, reason)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetScopeAmendmentByID :one
SELECT * FROM scope_amendments
 WHERE id = $1;

-- name: ListScopeAmendmentsByRun :many
-- Oldest first so the list endpoint and the fold both see requests
-- in submission order.
SELECT * FROM scope_amendments
 WHERE run_id = $1
 ORDER BY requested_at ASC, id ASC;

-- name: CountScopeAmendmentsByStage :one
-- Budget count is status-blind: a denied request still consumed an
-- operator interruption.
SELECT COUNT(*) FROM scope_amendments
 WHERE stage_id = $1;

-- name: DecideScopeAmendment :one
-- Pending-only guard in the WHERE clause makes the decision
-- transition atomic: a second decide matches zero rows and the
-- caller maps that to ErrAlreadyDecided (or ErrNotFound).
UPDATE scope_amendments
   SET status = $2,
       decision_reason = $3,
       decided_by = $4,
       decided_at = $5
 WHERE id = $1
   AND status = 'pending'
RETURNING *;

-- name: LapsePendingScopeAmendmentsForStage :many
-- E83.64 / #4127: flip the stage's still-undecided requests to the terminal
-- `lapsed` status. Pending-only, so an approved/denied row is never touched
-- and a second call matches zero rows (idempotent). decided_by stays NULL:
-- no operator decided a lapsed row.
UPDATE scope_amendments
   SET status = 'lapsed',
       decision_reason = $2,
       decided_at = $3
 WHERE stage_id = $1 AND status = 'pending'
RETURNING *;

-- name: LapsePendingScopeAmendmentsForRun :many
-- The run-terminal sibling of LapsePendingScopeAmendmentsForStage: every
-- still-undecided request on every stage of the run.
UPDATE scope_amendments
   SET status = 'lapsed',
       decision_reason = $2,
       decided_at = $3
 WHERE run_id = $1 AND status = 'pending'
RETURNING *;

-- name: ListStrandedPendingScopeAmendmentStages :many
-- The startup backfill's work list: each stage carrying a pending request
-- whose stage state or run state already lapses it. The stage literal set is
-- the lapsing subset of run.StageState.IsTerminal (failed is deliberately
-- absent: a failed stage keeps its requests pending); the run literal set
-- mirrors scopeamendment.RunStateLapses (failed is deliberately absent too:
-- fishhawk_retry_stage reopens a failed run).
SELECT DISTINCT sa.stage_id, s.state AS stage_state, r.state AS run_state
  FROM scope_amendments sa
  JOIN stages s ON s.id = sa.stage_id
  JOIN runs r ON r.id = sa.run_id
 WHERE sa.status = 'pending'
   AND (s.state IN ('succeeded', 'cancelled', 'superseded')
        OR r.state IN ('succeeded', 'cancelled'))
 ORDER BY sa.stage_id;
