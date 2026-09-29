package decisionindex

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DBTX is the query surface the store and the resolver run against. Both
// *pgxpool.Pool and pgx.Tx satisfy it, so a caller can run the store inside a
// postgres.WithTenant transaction (the RLS test does) or straight on the pool
// (the backfill and the live writer do).
//
// The store is hand-written pgx rather than sqlc-generated: a local
// `sqlc generate` regenerates every package in backend/sqlc.yaml. The queries
// are covered by the pgtest-backed store tests instead of a compile check.
type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Store reads and writes decision_index rows.
type Store struct {
	db DBTX
}

// NewStore returns a Store over db.
func NewStore(db DBTX) *Store {
	return &Store{db: db}
}

// rowColumns is the full column list, in Row field order. Every read and write
// names it so a new column cannot be written but not read (or vice versa).
const rowColumns = `source_sequence, source_entry_hash, run_id, stage_id, account_id,
	repo, workflow_id, doctrine_version, decision_class, stage_kind, outcome,
	reject_class, concern_category_raw, concern_category, concern_category_unmapped,
	severity, touched_paths, escalation_keys, delegated, actor_kind, actor_subject,
	decided_at, reason_sequence, reason_key`

// upsertSQL is idempotent on the chain sequence: re-indexing an entry whose
// joined context changed converges every derived column instead of raising a
// duplicate-key error or leaving the stale value behind.
const upsertSQL = `INSERT INTO decision_index (` + rowColumns + `)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16,
        $17, $18, $19, $20, $21, $22, $23, $24)
ON CONFLICT (source_sequence) DO UPDATE SET
	source_entry_hash         = EXCLUDED.source_entry_hash,
	run_id                    = EXCLUDED.run_id,
	stage_id                  = EXCLUDED.stage_id,
	account_id                = EXCLUDED.account_id,
	repo                      = EXCLUDED.repo,
	workflow_id               = EXCLUDED.workflow_id,
	doctrine_version          = EXCLUDED.doctrine_version,
	decision_class            = EXCLUDED.decision_class,
	stage_kind                = EXCLUDED.stage_kind,
	outcome                   = EXCLUDED.outcome,
	reject_class              = EXCLUDED.reject_class,
	concern_category_raw      = EXCLUDED.concern_category_raw,
	concern_category          = EXCLUDED.concern_category,
	concern_category_unmapped = EXCLUDED.concern_category_unmapped,
	severity                  = EXCLUDED.severity,
	touched_paths             = EXCLUDED.touched_paths,
	escalation_keys           = EXCLUDED.escalation_keys,
	delegated                 = EXCLUDED.delegated,
	actor_kind                = EXCLUDED.actor_kind,
	actor_subject             = EXCLUDED.actor_subject,
	decided_at                = EXCLUDED.decided_at,
	reason_sequence           = EXCLUDED.reason_sequence,
	reason_key                = EXCLUDED.reason_key`

func upsertArgs(r Row) []any {
	return []any{
		r.SourceSequence, r.SourceEntryHash, r.RunID, r.StageID, r.AccountID,
		r.Repo, r.WorkflowID, r.DoctrineVersion, string(r.DecisionClass), r.StageKind, r.Outcome,
		r.RejectClass, r.ConcernCategoryRaw, r.ConcernCategory, r.ConcernCategoryUnmapped,
		r.Severity, nonNil(r.TouchedPaths), nonNil(r.EscalationKeys), r.Delegated, r.ActorKind, r.ActorSubject,
		r.DecidedAt.UTC(), r.ReasonSequence, r.ReasonKey,
	}
}

// nonNil keeps a nil slice from binding as SQL NULL against a NOT NULL array.
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// Upsert writes one row, keyed on its source sequence.
func (s *Store) Upsert(ctx context.Context, r Row) error {
	if _, err := s.db.Exec(ctx, upsertSQL, upsertArgs(r)...); err != nil {
		return fmt.Errorf("decisionindex: upsert sequence %d: %w", r.SourceSequence, err)
	}
	return nil
}

// UpsertBatch writes rows in ONE transaction: either every row lands or none.
func (s *Store) UpsertBatch(ctx context.Context, rows []Row) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("decisionindex: upsert batch: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	b := &pgx.Batch{}
	for _, r := range rows {
		b.Queue(upsertSQL, upsertArgs(r)...)
	}
	br := tx.SendBatch(ctx, b)
	for _, r := range rows {
		if _, err := br.Exec(); err != nil {
			_ = br.Close()
			return fmt.Errorf("decisionindex: upsert batch: sequence %d: %w", r.SourceSequence, err)
		}
	}
	if err := br.Close(); err != nil {
		return fmt.Errorf("decisionindex: upsert batch: close: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("decisionindex: upsert batch: commit: %w", err)
	}
	return nil
}

// Truncate empties the index — the from-scratch rebuild path. Safe because the
// table is derived: the chain still holds every fact.
func (s *Store) Truncate(ctx context.Context) error {
	if _, err := s.db.Exec(ctx, `TRUNCATE decision_index`); err != nil {
		return fmt.Errorf("decisionindex: truncate: %w", err)
	}
	return nil
}

// ListFilter is ADR-082 rule 3's hard filter. An empty field matches any
// value; Limit <= 0 means unbounded. FromSequence and ToSequence bound
// source_sequence INCLUSIVELY (the digest's windowed read, E75.6 / #3734);
// 0 leaves that side unbounded, so the zero value reads exactly what it did
// before the bounds existed.
type ListFilter struct {
	Repo          string
	DecisionClass DecisionClass
	StageKind     string
	FromSequence  int64
	ToSequence    int64
	Limit         int
}

// List returns the rows matching f, ordered by source sequence.
func (s *Store) List(ctx context.Context, f ListFilter) ([]Row, error) {
	var limit *int
	if f.Limit > 0 {
		limit = &f.Limit
	}
	rows, err := s.db.Query(ctx, `SELECT `+rowColumns+` FROM decision_index
		WHERE ($1 = '' OR repo = $1)
		  AND ($2 = '' OR decision_class = $2)
		  AND ($3 = '' OR stage_kind = $3)
		  AND ($5::bigint = 0 OR source_sequence >= $5)
		  AND ($6::bigint = 0 OR source_sequence <= $6)
		ORDER BY source_sequence
		LIMIT $4`, f.Repo, string(f.DecisionClass), f.StageKind, limit, f.FromSequence, f.ToSequence)
	if err != nil {
		return nil, fmt.Errorf("decisionindex: list: %w", err)
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return nil, fmt.Errorf("decisionindex: list: scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("decisionindex: list: %w", err)
	}
	return out, nil
}

func scanRow(rows pgx.Rows) (Row, error) {
	var (
		r     Row
		class string
	)
	err := rows.Scan(
		&r.SourceSequence, &r.SourceEntryHash, &r.RunID, &r.StageID, &r.AccountID,
		&r.Repo, &r.WorkflowID, &r.DoctrineVersion, &class, &r.StageKind, &r.Outcome,
		&r.RejectClass, &r.ConcernCategoryRaw, &r.ConcernCategory, &r.ConcernCategoryUnmapped,
		&r.Severity, &r.TouchedPaths, &r.EscalationKeys, &r.Delegated, &r.ActorKind, &r.ActorSubject,
		&r.DecidedAt, &r.ReasonSequence, &r.ReasonKey,
	)
	if err != nil {
		return Row{}, err
	}
	r.DecisionClass = DecisionClass(class)
	r.DecidedAt = r.DecidedAt.UTC()
	r.TouchedPaths = nonNil(r.TouchedPaths)
	r.EscalationKeys = nonNil(r.EscalationKeys)
	return r, nil
}

// Gap is one decision-bearing audit entry with NO index row while its run row
// still exists — the visible-degradation signal: the chain recorded a decision
// the index cannot answer about.
type Gap struct {
	SourceSequence int64
	Category       string
	RunID          uuid.UUID
	Timestamp      time.Time
}

// GapReport is the result of the missing-row check.
//
// ORPHANED entries are NOT gaps (#3730 approval condition 6): a decision-bearing
// entry whose run row no longer exists (or that carries no run id at all)
// cannot be indexed — decision_index.run_id references runs — so no backfill
// can ever close it. Counting it as a gap would make `check` fail forever on a
// state the operator cannot repair. It is reported separately instead.
type GapReport struct {
	// GapCount is the TOTAL number of true gaps; Gaps holds at most the
	// requested limit of them, lowest sequence first.
	GapCount int
	Gaps     []Gap
	// OrphanedCount is the TOTAL number of orphaned entries; OrphanedSequences
	// holds at most the requested limit of their sequences, ascending.
	OrphanedCount     int
	OrphanedSequences []int64
}

// GapFilter narrows the missing-row check. An empty Repo matches every
// repository; FromSequence/ToSequence bound the entry's chain sequence
// INCLUSIVELY, 0 leaving that side unbounded. Limit bounds each returned list
// (not the counts); Limit <= 0 means unbounded.
//
// A Repo filter matches through the entry's run row, so it necessarily
// excludes ORPHANED entries (no run row, hence no repository): a repo-scoped
// report carries OrphanedCount 0 by construction.
type GapFilter struct {
	Repo         string
	FromSequence int64
	ToSequence   int64
	Limit        int
}

// Gaps runs the missing-row check over the whole chain: every decision-bearing
// entry with no decision_index row, split into true gaps (run row present) and
// orphaned entries (run row absent). limit bounds each returned list, not the
// counts; limit <= 0 means unbounded. It is GapsInWindow with an empty filter,
// so `fishhawkd decision-index check` and the digest share ONE query path.
func (s *Store) Gaps(ctx context.Context, limit int) (*GapReport, error) {
	return s.GapsInWindow(ctx, GapFilter{Limit: limit})
}

// GapsInWindow runs the missing-row check narrowed by f — the digest's
// "decision-bearing entries the best-effort writer never indexed" read over its
// (repo, sequence) window (E75.6 / #3734).
func (s *Store) GapsInWindow(ctx context.Context, f GapFilter) (*GapReport, error) {
	limit := f.Limit
	rows, err := s.db.Query(ctx, `SELECT ae.sequence, ae.category, ae.run_id, ae.ts, r.id IS NULL
		FROM audit_entries ae
		LEFT JOIN decision_index di ON di.source_sequence = ae.sequence
		LEFT JOIN runs r ON r.id = ae.run_id
		WHERE ae.category = ANY($1) AND di.source_sequence IS NULL
		  AND ($2 = '' OR r.repo = $2)
		  AND ($3::bigint = 0 OR ae.sequence >= $3)
		  AND ($4::bigint = 0 OR ae.sequence <= $4)
		ORDER BY ae.sequence`, DecisionBearingCategories(), f.Repo, f.FromSequence, f.ToSequence)
	if err != nil {
		return nil, fmt.Errorf("decisionindex: gaps: %w", err)
	}
	defer rows.Close()
	rep := &GapReport{Gaps: []Gap{}, OrphanedSequences: []int64{}}
	for rows.Next() {
		var (
			g        Gap
			runID    *uuid.UUID
			orphaned bool
		)
		if err := rows.Scan(&g.SourceSequence, &g.Category, &runID, &g.Timestamp, &orphaned); err != nil {
			return nil, fmt.Errorf("decisionindex: gaps: scan: %w", err)
		}
		if orphaned {
			rep.OrphanedCount++
			if limit <= 0 || len(rep.OrphanedSequences) < limit {
				rep.OrphanedSequences = append(rep.OrphanedSequences, g.SourceSequence)
			}
			continue
		}
		rep.GapCount++
		if limit <= 0 || len(rep.Gaps) < limit {
			g.RunID = *runID
			g.Timestamp = g.Timestamp.UTC()
			rep.Gaps = append(rep.Gaps, g)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("decisionindex: gaps: %w", err)
	}
	return rep, nil
}
