-- Review-concern queries (E22.X / #964). sqlc generates typed Go
-- into ./db per backend/sqlc.yaml.

-- name: InsertReviewConcern :one
INSERT INTO review_concerns (
    id, run_id, stage_id, stage_kind, origin_review_sequence,
    reviewer_model, severity, category, note, suggested_patch,
    new_evidence, settled_ref, reviewer_role, quote_unverified,
    severity_clamped_from, provenance, check_key
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
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
-- E94.2 / #4693 (ADR-117 D1): a non-NULL outcome is recorded in the same
-- UPDATE (ApplyDisposition). A NULL outcome leaves it alone EXCEPT that a move
-- back into an open state clears an outcome of exactly 'fixed', so a reopened
-- concern is again eligible for StampReviewConcernsOpenAtMerge (which only
-- stamps an empty outcome); carried and open_at_merge are never cleared. The open
-- state set is duplicated from the Go state machine's IsOpen — keep in sync.
-- No judgement column is ever written here.
UPDATE review_concerns
   SET state = $2,
       state_reason = $3,
       outcome = COALESCE(sqlc.narg(outcome)::text,
                          CASE WHEN $2 IN ('raised', 'addressed_pending', 'reopened') AND outcome = 'fixed'
                               THEN '' ELSE outcome END),
       updated_at = now()
 WHERE id = $1
   AND state = sqlc.arg(from_state)
RETURNING *;

-- name: ImportRolledUpReviewConcern :one
-- E94.1 / #4692 (ADR-117 D2): copy one decomposition child's concern into its
-- parent's ledger as a raised implement-stage row. The source's origin round,
-- reviewer, severity, category, note, evidence, ingest markers, provenance and
-- check_key are copied VERBATIM; rolled_up_from links back to the source.
-- review_concerns_rolled_up_once_idx (0112) is the idempotence authority: a
-- source already imported into this parent matches the conflict target and
-- returns no row (pgx.ErrNoRows), never a second copy.
-- The source is read under FOR NO KEY UPDATE and must still be OPEN (the
-- state set duplicated from the Go state machine's IsOpen — keep in sync), so
-- a concurrent retry sweep's supersession of the source either commits first
-- (this statement re-checks the committed row, matches nothing and returns
-- pgx.ErrNoRows: no copy) or waits for this transaction to commit (and the
-- sweep's later parent-ledger scan then sees the copy and supersedes it). No
-- interleaving leaves a raised copy of a superseded source.
WITH src AS (
    SELECT * FROM review_concerns
     WHERE id = sqlc.arg(source_id)::uuid
       AND state IN ('raised', 'addressed_pending', 'reopened')
       FOR NO KEY UPDATE
)
INSERT INTO review_concerns (
    id, run_id, stage_id, stage_kind, origin_review_sequence,
    reviewer_model, severity, category, note, state, state_reason,
    suggested_patch, new_evidence, settled_ref, reviewer_role,
    quote_unverified, severity_clamped_from, provenance, check_key,
    rolled_up_from
)
SELECT sqlc.arg(id)::uuid, sqlc.arg(parent_run_id)::uuid, sqlc.arg(parent_stage_id)::uuid, 'implement', src.origin_review_sequence,
       src.reviewer_model, src.severity, src.category, src.note, 'raised', sqlc.arg(state_reason)::text,
       src.suggested_patch, src.new_evidence, src.settled_ref, src.reviewer_role,
       src.quote_unverified, src.severity_clamped_from, src.provenance, src.check_key,
       src.id
  FROM src
ON CONFLICT (run_id, rolled_up_from) WHERE rolled_up_from IS NOT NULL DO NOTHING
RETURNING *;

-- name: MarkReviewConcernCarried :exec
-- E94.1 / #4692: record the carried outcome on a source concern imported into
-- its parent. Touches outcome only — never state, state_reason or updated_at.
-- Only a source that HAS a copy is carried: a source the import skipped
-- because a concurrent sweep closed it first keeps its outcome.
UPDATE review_concerns AS src SET outcome = 'carried'
 WHERE src.id = $1 AND src.outcome IN ('', 'carried')
   AND EXISTS (SELECT 1 FROM review_concerns AS cp WHERE cp.rolled_up_from = src.id);

-- name: StampReviewConcernsOpenAtMerge :execrows
-- E94.1 / #4692 (ADR-117 D1): at merge, stamp open_at_merge on every concern
-- of the given runs still open or superseded with no outcome recorded. Touches
-- outcome only; a recorded outcome is never overwritten.
UPDATE review_concerns SET outcome = 'open_at_merge' WHERE run_id = ANY(sqlc.arg(run_ids)::uuid[]) AND outcome = '' AND state IN ('raised', 'addressed_pending', 'reopened', 'superseded');
