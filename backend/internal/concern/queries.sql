-- Review-concern queries (E22.X / #964). sqlc generates typed Go
-- into ./db per backend/sqlc.yaml.

-- name: InsertReviewConcern :one
INSERT INTO review_concerns (
    id, run_id, stage_id, stage_kind, origin_review_sequence,
    reviewer_model, severity, category, note, suggested_patch
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING *;

-- name: GetReviewConcernsByIDs :many
SELECT * FROM review_concerns
 WHERE id = ANY(sqlc.arg(ids)::uuid[]);

-- name: ListReviewConcernsByRun :many
-- Origin-sequence order so concerns read oldest review first.
SELECT * FROM review_concerns
 WHERE run_id = $1
 ORDER BY origin_review_sequence ASC, created_at ASC, id ASC;

-- name: ListOpenReviewConcernsByRun :many
-- Open states only (raised, addressed_pending, reopened). The state set
-- is duplicated from the Go state machine's IsOpen — keep in sync.
SELECT * FROM review_concerns
 WHERE run_id = $1
   AND state IN ('raised', 'addressed_pending', 'reopened')
 ORDER BY origin_review_sequence ASC, created_at ASC, id ASC;

-- name: UpdateReviewConcernState :one
-- from-state guard in the WHERE clause makes the transition atomic
-- under concurrent writers: a racing update matches zero rows and the
-- caller re-reads to re-validate against the state machine.
UPDATE review_concerns
   SET state = $2,
       state_reason = $3,
       updated_at = now()
 WHERE id = $1
   AND state = sqlc.arg(from_state)
RETURNING *;
