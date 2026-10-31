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

-- name: UpdateReviewConcernStateJudged :one
-- E94.7 / #4840 (ADR-117 D1 phase 2): the HUMAN verbs' transition
-- (ApplyHumanDisposition). The same from-state guard as
-- UpdateReviewConcernState, plus the verb's outcome and every judgement column
-- in the SAME UPDATE: judgement_backfill is written false (the verb judges
-- now, not after the fact) and duplicate_of is never touched (phase 3, #4841).
-- A re-judgement overwrites the stored judgement.
UPDATE review_concerns
   SET state = $2,
       state_reason = $3,
       outcome = sqlc.arg(outcome),
       judgement = sqlc.arg(judgement),
       judgement_actor_kind = sqlc.arg(judgement_actor_kind),
       judgement_delegated = sqlc.arg(judgement_delegated),
       judgement_bulk = sqlc.arg(judgement_bulk),
       judgement_backfill = false,
       tradeoff_cost = sqlc.arg(tradeoff_cost),
       updated_at = now()
 WHERE id = $1
   AND state = sqlc.arg(from_state)
RETURNING *;

-- name: SetReviewConcernLabel :one
-- E94.8 / #4841 (ADR-117 D1 phase 3): the relabel verb's write (ApplyLabel).
-- ONE UPDATE setting the judgement columns and duplicate_of: judgement is the
-- label's (unlabelled for a duplicate label, whose effective judgement is
-- inherited), judgement_delegated and judgement_bulk are false (one human, one
-- concern), tradeoff_cost is '' for a duplicate, and duplicate_of is NULL for
-- a judgement label (clearing a prior link). The optimistic guard matches only
-- while the row still holds the prior judgement and duplicate_of the caller
-- read and audited; zero rows is a concurrent relabel (ErrLabelConflict). It
-- NEVER sets state, state_reason, outcome or updated_at: a label is not a
-- lifecycle event (the StampReviewConcernsOpenAtMerge posture).
UPDATE review_concerns
   SET judgement = sqlc.arg(judgement),
       judgement_actor_kind = sqlc.arg(judgement_actor_kind),
       judgement_delegated = false,
       judgement_bulk = false,
       judgement_backfill = sqlc.arg(judgement_backfill),
       tradeoff_cost = sqlc.arg(tradeoff_cost),
       duplicate_of = sqlc.narg(duplicate_of)::uuid
 WHERE id = sqlc.arg(id)
   AND judgement = sqlc.arg(prior_judgement)
   AND duplicate_of IS NOT DISTINCT FROM sqlc.narg(prior_duplicate_of)::uuid
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
