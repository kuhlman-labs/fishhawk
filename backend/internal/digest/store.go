package digest

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DBTX is the query surface the store runs against. *pgxpool.Pool and pgx.Tx
// both satisfy it, so a caller can run the store inside a postgres.WithTenant
// transaction (the RLS test does) or on the pool.
//
// Hand-written pgx, not sqlc: a local `sqlc generate` regenerates every
// package in backend/sqlc.yaml (the decisionindex precedent). Every read is
// LIMIT-bounded and index-served; none goes through audit.Repository.ListAll.
type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store reads the chain for the digest and persists read watermarks.
type Store struct {
	db DBTX
}

// NewStore returns a Store over db.
func NewStore(db DBTX) *Store {
	return &Store{db: db}
}

// ChainHead returns the highest audit_entries.sequence on any run in repo, or
// 0 when the repository has no entries. audit_entries.sequence is a
// table-wide BIGSERIAL, so this is a sound cross-run watermark ceiling.
func (s *Store) ChainHead(ctx context.Context, repo string) (int64, error) {
	var head *int64
	if err := s.db.QueryRow(ctx, `SELECT max(ae.sequence)
		FROM audit_entries ae JOIN runs r ON r.id = ae.run_id
		WHERE r.repo = $1`, repo).Scan(&head); err != nil {
		return 0, fmt.Errorf("digest: chain head: %w", err)
	}
	if head == nil {
		return 0, nil
	}
	return *head, nil
}

// EntryHashes returns the entry_hash of each audit entry among sequences that
// exists. A sequence absent from the result has no audit_entries row — the
// citation check that turns a missing source entry into a gap.
func (s *Store) EntryHashes(ctx context.Context, sequences []int64) (map[int64]string, error) {
	out := make(map[int64]string, len(sequences))
	if len(sequences) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(ctx, `SELECT sequence, entry_hash FROM audit_entries WHERE sequence = ANY($1)`, sequences)
	if err != nil {
		return nil, fmt.Errorf("digest: entry hashes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			seq  int64
			hash string
		)
		if err := rows.Scan(&seq, &hash); err != nil {
			return nil, fmt.Errorf("digest: entry hashes: scan: %w", err)
		}
		out[seq] = hash
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("digest: entry hashes: %w", err)
	}
	return out, nil
}

// PageRow is one page raised on the chain and its pairing verdict.
type PageRow struct {
	Sequence    int64
	EntryHash   string
	Category    string
	RunID       uuid.UUID
	StageID     *uuid.UUID
	StageKind   string
	At          time.Time
	AmendmentID string
	// AnsweredSequence is the sequence of the entry that ANSWERS this page
	// (0 when unanswered at or below the window's to_sequence).
	AnsweredSequence int64
}

// pageCategories are the chain entries the digest treats as a page raised to
// a captain. Each is answered ONLY by its paired entry (answeredBy): an
// unrelated later decision on the same run does NOT answer it (#3734 approval
// condition 4).
var pageCategories = []string{"clarification_requested", "escalation_fired", "scope_amendment_requested"}

// answeredBy documents the pairing the pages query implements in SQL:
//
//	clarification_requested   -> clarification_answered  on the SAME stage
//	scope_amendment_requested -> scope_amendment_decided for the SAME amendment_id
//	escalation_fired          -> approval_submitted      on the SAME stage
//
// In every case the answer must be LATER on the chain than the page and at or
// below the digest's to_sequence (a digest is a snapshot at to_sequence).
var answeredBy = map[string]string{
	"clarification_requested":   "clarification_answered",
	"scope_amendment_requested": "scope_amendment_decided",
	"escalation_fired":          "approval_submitted",
}

const pagesSQL = `SELECT ae.sequence, ae.entry_hash, ae.category, ae.run_id, ae.stage_id,
	COALESCE(s.stage_type, ''), ae.ts, COALESCE(ae.payload->>'amendment_id', ''), COALESCE(ans.sequence, 0)
FROM audit_entries ae
JOIN runs r ON r.id = ae.run_id
LEFT JOIN stages s ON s.id = ae.stage_id
LEFT JOIN LATERAL (
	SELECT a2.sequence FROM audit_entries a2
	WHERE a2.run_id = ae.run_id AND a2.sequence > ae.sequence AND a2.sequence <= $4
	  AND a2.category = $5::jsonb->>ae.category
	  AND CASE ae.category
	        WHEN 'scope_amendment_requested'
	          THEN a2.payload->>'amendment_id' = ae.payload->>'amendment_id'
	        ELSE a2.stage_id = ae.stage_id
	      END
	ORDER BY a2.sequence LIMIT 1
) ans ON true
WHERE r.repo = $1 AND ae.category = ANY($2) AND ae.sequence >= $3 AND ae.sequence <= $4
ORDER BY ae.sequence
LIMIT $6`

// Pages returns the pages raised in repo over the inclusive window
// [from, to], each with its paired answer (if any), ascending, at most limit.
func (s *Store) Pages(ctx context.Context, repo string, from, to int64, limit int) ([]PageRow, error) {
	pairs, err := jsonObject(answeredBy)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, pagesSQL, repo, pageCategories, from, to, pairs, limit)
	if err != nil {
		return nil, fmt.Errorf("digest: pages: %w", err)
	}
	defer rows.Close()
	out := []PageRow{}
	for rows.Next() {
		var p PageRow
		if err := rows.Scan(&p.Sequence, &p.EntryHash, &p.Category, &p.RunID, &p.StageID,
			&p.StageKind, &p.At, &p.AmendmentID, &p.AnsweredSequence); err != nil {
			return nil, fmt.Errorf("digest: pages: scan: %w", err)
		}
		p.At = p.At.UTC()
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("digest: pages: %w", err)
	}
	return out, nil
}

// ParkedRow is one stage parked awaiting a captain, and the chain entry that
// parked it (zero Sequence/EntryHash/Category for an uncited stage).
type ParkedRow struct {
	StageID   uuid.UUID
	RunID     uuid.UUID
	StageKind string
	State     string
	Sequence  int64
	EntryHash string
	Category  string
	At        time.Time
}

// parkingEntries maps each parked (awaiting-a-captain) stage state to the
// audit categories whose LATEST entry on the stage is the one that parked it
// (#3734 approval condition 3). A parked stage with no such entry is reported
// as a parked_without_citation gap, never cited with a made-up sequence.
//
//   - awaiting_approval: plan_generated (plan gate), implement_reviewed
//     (review gate), acceptance_outcome_recorded (acceptance gate) or
//     escalation_fired (an escalation parked the gate).
//   - awaiting_input: clarification_requested.
//   - awaiting_scope_decision: scope_completeness_parked (the #1151
//     exempt-or-fail park) or scope_amendment_requested.
//   - awaiting_deploy_approval: escalation_fired — the deploy gate emits no
//     dedicated parking entry, so an un-escalated deploy park is a gap.
var parkingEntries = map[string][]string{
	"awaiting_approval":        {"plan_generated", "implement_reviewed", "acceptance_outcome_recorded", "escalation_fired"},
	"awaiting_input":           {"clarification_requested"},
	"awaiting_scope_decision":  {"scope_completeness_parked", "scope_amendment_requested"},
	"awaiting_deploy_approval": {"escalation_fired"},
}

// parkingPairs flattens parkingEntries into two parallel, deterministic
// arrays for the SQL unnest join, plus the parked-state set.
func parkingPairs() (states, categories, parked []string) {
	for st := range parkingEntries {
		parked = append(parked, st)
	}
	sort.Strings(parked)
	for _, st := range parked {
		for _, c := range parkingEntries[st] {
			states = append(states, st)
			categories = append(categories, c)
		}
	}
	return states, categories, parked
}

// parkedBaseSQL selects parked stages on non-terminal runs in the repo with
// their latest parking entry (NULL when none).
const parkedBaseSQL = `SELECT s.id, s.run_id, s.stage_type, s.state,
	pe.sequence, pe.entry_hash, pe.category, pe.ts
FROM stages s
JOIN runs r ON r.id = s.run_id
LEFT JOIN LATERAL (
	SELECT ae.sequence, ae.entry_hash, ae.category, ae.ts
	FROM audit_entries ae
	JOIN unnest($2::text[], $3::text[]) AS m(state, category)
	  ON m.state = s.state AND m.category = ae.category
	WHERE ae.run_id = s.run_id AND ae.stage_id = s.id
	ORDER BY ae.sequence DESC
	LIMIT 1
) pe ON true
WHERE r.repo = $1 AND r.state IN ('pending', 'running') AND s.state = ANY($4)`

// ParkedStages returns the parked stages whose parking entry lies in the
// inclusive window [from, to], ordered by that entry's sequence ascending, at
// most limit.
func (s *Store) ParkedStages(ctx context.Context, repo string, from, to int64, limit int) ([]ParkedRow, error) {
	states, cats, parked := parkingPairs()
	return s.parked(ctx, parkedBaseSQL+`
		AND pe.sequence >= $5 AND pe.sequence <= $6
		ORDER BY pe.sequence
		LIMIT $7`, repo, states, cats, parked, from, to, limit)
}

// UncitedParkedStages returns parked stages with NO identifiable parking
// entry, ordered by (created_at, id) — a stable total order with no chain
// sequence to page by — skipping the first offset rows and returning at most
// limit. The (created_at, id) key + OFFSET is what makes every uncited parked
// stage beyond the constant-size floor retrievable through the
// SectionOpenDecisionsUncited cursor (#3734 fix-up condition 3).
func (s *Store) UncitedParkedStages(ctx context.Context, repo string, offset, limit int) ([]ParkedRow, error) {
	states, cats, parked := parkingPairs()
	return s.parked(ctx, parkedBaseSQL+`
		AND pe.sequence IS NULL
		ORDER BY s.created_at, s.id
		LIMIT $5 OFFSET $6`, repo, states, cats, parked, limit, offset)
}

func (s *Store) parked(ctx context.Context, sql string, args ...any) ([]ParkedRow, error) {
	rows, err := s.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("digest: parked stages: %w", err)
	}
	defer rows.Close()
	out := []ParkedRow{}
	for rows.Next() {
		var (
			p        ParkedRow
			seq      *int64
			hash     *string
			category *string
			at       *time.Time
		)
		if err := rows.Scan(&p.StageID, &p.RunID, &p.StageKind, &p.State, &seq, &hash, &category, &at); err != nil {
			return nil, fmt.Errorf("digest: parked stages: scan: %w", err)
		}
		if seq != nil {
			p.Sequence, p.EntryHash, p.Category, p.At = *seq, *hash, *category, at.UTC()
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("digest: parked stages: %w", err)
	}
	return out, nil
}

// getWatermarkSQL reads one captain's watermark row. A package constant so a
// test DBTX can target the advance path's separate-statement re-read.
const getWatermarkSQL = `SELECT sequence FROM captain_read_watermarks
	WHERE captain_subject = $1 AND repo = $2 AND account_id IS NOT DISTINCT FROM $3`

// GetWatermark returns the captain's last-read sequence for repo under the
// account (nil = the untenanted partition), and whether one is recorded.
func (s *Store) GetWatermark(ctx context.Context, accountID *uuid.UUID, subject, repo string) (int64, bool, error) {
	var seq int64
	err := s.db.QueryRow(ctx, getWatermarkSQL, subject, repo, accountID).Scan(&seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("digest: get watermark: %w", err)
	}
	return seq, true, nil
}

// upsertWatermarkSQL is MONOTONIC: the WHERE on the conflict arm refuses to
// move a row backwards, so a concurrent lower write never regresses it.
const upsertWatermarkSQL = `INSERT INTO captain_read_watermarks (account_id, captain_subject, repo, sequence)
VALUES ($1, $2, $3, $4)
ON CONFLICT (captain_subject, repo, COALESCE(account_id, '00000000-0000-0000-0000-000000000000'::uuid))
DO UPDATE SET sequence = EXCLUDED.sequence, updated_at = now()
WHERE captain_read_watermarks.sequence < EXCLUDED.sequence`

// UpsertWatermark records sequence as the captain's watermark, never moving an
// existing row backwards. Callers other than MarkRead exist only in tests: the
// product advances a watermark exclusively through MarkRead's
// append-before-advance path.
func (s *Store) UpsertWatermark(ctx context.Context, accountID *uuid.UUID, subject, repo string, sequence int64) error {
	if _, err := s.db.Exec(ctx, upsertWatermarkSQL, accountID, subject, repo, sequence); err != nil {
		return fmt.Errorf("digest: upsert watermark: %w", err)
	}
	return nil
}

// ErrWatermarkRowVanished is returned (wrapped) by MarkRead when the monotonic
// upsert refused to move the row — so an equal-or-higher watermark was
// committed — but the separate re-read of that row then found none. No product
// path deletes captain_read_watermarks rows, so only a concurrent manual DELETE
// reaches it; the digest_marked_read entry was already appended, so a retry
// converges the watermark (#3852).
var ErrWatermarkRowVanished = errors.New("digest: watermark row vanished between the refused advance and its re-read")

// advanceWatermarkSQL is the monotonic upsert (upsertWatermarkSQL) with
// RETURNING: it returns a row ONLY when this statement raised the watermark —
// the INSERT of a first mark, or the conflict-arm UPDATE to a strictly higher
// sequence. No row means the monotonic WHERE refused the update because an
// equal-or-higher value is committed.
//
// It deliberately does NOT re-read the row in the same statement. Every
// sub-statement of a WITH query runs against ONE snapshot
// (https://www.postgresql.org/docs/16/queries-with.html#QUERIES-WITH-MODIFYING),
// while INSERT ... ON CONFLICT DO UPDATE locks the conflicting row even when its
// WHERE is false and, under READ COMMITTED, waits for a concurrent writer and
// then acts on its COMMITTED row
// (https://www.postgresql.org/docs/16/transaction-iso.html#XACT-READ-COMMITTED).
// So a same-statement fallback read — the CTE + UNION ALL arm this replaced —
// predates a commit the statement blocked on: it reported the STALE pre-commit
// sequence when a concurrent higher mark won the row, and found NO row (a
// "no rows in result set" error) when a concurrent FIRST insert won the
// unique-index conflict (#3852).
const advanceWatermarkSQL = upsertWatermarkSQL + `
RETURNING sequence`

// advanceWatermark upserts the watermark monotonically and returns the
// COMMITTED sequence and whether this call raised it. When the upsert refuses
// (pgx.ErrNoRows: an equal-or-higher value is committed) it re-reads the row in
// a SEPARATE statement, which under READ COMMITTED takes a fresh snapshot that
// includes the conflicting writer's commit, so the reported sequence is the
// latest committed watermark with changed=false (#3734 fix-up condition 4,
// #3852). Any other upsert error is returned as an error — never treated as a
// refusal. A refusal whose re-read finds no row fails closed with
// ErrWatermarkRowVanished.
func (s *Store) advanceWatermark(ctx context.Context, accountID *uuid.UUID, subject, repo string, sequence int64) (committed int64, changed bool, err error) {
	err = s.db.QueryRow(ctx, advanceWatermarkSQL, accountID, subject, repo, sequence).Scan(&committed)
	if err == nil {
		return committed, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, false, fmt.Errorf("digest: advance watermark: %w", err)
	}
	committed, has, err := s.GetWatermark(ctx, accountID, subject, repo)
	if err != nil {
		return 0, false, fmt.Errorf("digest: advance watermark: re-read committed watermark: %w", err)
	}
	if !has {
		return 0, false, fmt.Errorf("digest: advance watermark: refused upsert of %d for %s/%s: %w", sequence, subject, repo, ErrWatermarkRowVanished)
	}
	return committed, false, nil
}
