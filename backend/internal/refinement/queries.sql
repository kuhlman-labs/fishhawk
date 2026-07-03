-- Refinement-draft queries consumed by the postgres adapter for the
-- refinement.Repository interface (E34.1 / #1592). A draft is keyed by its
-- refinement session id; nothing here files a provider work item.

-- name: CreateRefinementDraft :one
INSERT INTO refinement_drafts (id, session_id, brief, draft, model)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetRefinementDraft :one
SELECT * FROM refinement_drafts WHERE id = $1;

-- name: ListRefinementDraftsForSession :many
SELECT * FROM refinement_drafts
WHERE session_id = $1
ORDER BY created_at ASC;
