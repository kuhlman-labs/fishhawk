-- Refinement queries consumed by the postgres adapter for the
-- refinement.Repository interface (E34.1 / #1592, E34.2 / #1593). A draft is
-- keyed by its refinement session id; nothing here files a provider work item.
-- A decision is an append-only approve/reject verdict pinning one draft
-- revision + its content hash (E34.2's gate record).

-- name: CreateRefinementDraft :one
INSERT INTO refinement_drafts (id, session_id, brief, draft, model, origin, account_id)
VALUES ($1, $2, $3, $4, $5, $6, $7)
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

-- E34.3 filing executor (#1594) — the idempotency ledger. A filing session
-- pins the target repo per approved draft; a filed-item row records one
-- ordinal->issue mapping recorded immediately after each provider File.

-- name: CreateRefinementFilingSession :one
INSERT INTO refinement_filing_sessions (draft_id, session_id, repo)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetRefinementFilingSession :one
SELECT * FROM refinement_filing_sessions WHERE draft_id = $1;

-- name: CompleteRefinementFilingSession :exec
UPDATE refinement_filing_sessions
SET completed_at = now()
WHERE draft_id = $1 AND completed_at IS NULL;

-- name: CreateRefinementFiledItem :one
INSERT INTO refinement_filed_items (id, draft_id, ordinal, issue_number, issue_url)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: ListRefinementFiledItems :many
SELECT * FROM refinement_filed_items
WHERE draft_id = $1
ORDER BY ordinal ASC;
