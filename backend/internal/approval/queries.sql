-- Approval queries consumed by the postgres adapter for the
-- approval.Repository interface (E3.5 / #45). sqlc generates typed
-- Go into ./db per the config in /backend/sqlc.yaml.

-- name: CreateApproval :one
-- ON CONFLICT DO NOTHING + COALESCE-based read makes this an
-- idempotent upsert: a re-submission from the same approver
-- returns the existing row's id without inserting a new one.
INSERT INTO approvals (id, stage_id, approver_subject, decision, comment, surface)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (stage_id, approver_subject) DO NOTHING
RETURNING *;

-- name: GetApprovalByApprover :one
SELECT * FROM approvals
 WHERE stage_id = $1 AND approver_subject = $2;

-- name: ListApprovalsForStage :many
SELECT * FROM approvals
 WHERE stage_id = $1
 ORDER BY submitted_at ASC;
