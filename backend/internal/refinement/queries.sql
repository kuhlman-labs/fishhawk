-- Refinement queries consumed by the postgres adapter for the
-- refinement.Repository interface (E34.1 / #1592, E34.2 / #1593). A draft is
-- keyed by its refinement session id; nothing here files a provider work item.
-- A decision is an append-only approve/reject verdict pinning one draft
-- revision + its content hash (E34.2's gate record).

-- name: CreateRefinementDraft :one
INSERT INTO refinement_drafts (id, session_id, brief, draft, model, origin)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetRefinementDraft :one
SELECT * FROM refinement_drafts WHERE id = $1;

-- name: ListRefinementDraftsForSession :many
SELECT * FROM refinement_drafts
WHERE session_id = $1
ORDER BY created_at ASC;

-- name: CreateRefinementDecision :one
INSERT INTO refinement_decisions (id, session_id, draft_id, decision, reason, draft_content_hash, decided_by)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: ListRefinementDecisionsForSession :many
SELECT * FROM refinement_decisions
WHERE session_id = $1
ORDER BY created_at ASC;
