package decisionindex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
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
// value; Limit <= 0 means unbounded.
type ListFilter struct {
	Repo          string
	DecisionClass DecisionClass
	StageKind     string
	Limit         int
	// AccountID narrows the listing to one tenant workspace account
	// (E75.3 / #3731). NON-NIL matches a row whose account_id EQUALS it OR IS
	// NULL — the same predicate as the decision_index_tenant_isolation RLS
	// policy, so a tenanted caller sees its own rows plus the untenanted
	// single-tenant rows and never another account's. NIL matches any row: the
	// un-narrowed backfill / CLI path every pre-#3731 caller depends on.
	//
	// It is the ROW-set predicate only. A gate-reference read is a RUN read and
	// carries the stricter GateRef.AccountID rule instead — see GateContext.
	AccountID *uuid.UUID
	// Newest flips the source-sequence ordering to DESC so a bounded Limit
	// yields the NEWEST N rows rather than the oldest N — what a precedent
	// candidate window needs. Consumers re-sort, so no wire order depends on
	// this flag. Zero (false) is the pre-#3731 ascending order.
	Newest bool
}

// listSQL renders the List query. The ORDER BY direction is the ONLY part that
// varies, and it is chosen from a bool rather than interpolated from caller
// input, so there is no injection surface.
func listSQL(newest bool) string {
	order := "ORDER BY source_sequence"
	if newest {
		order = "ORDER BY source_sequence DESC"
	}
	return `SELECT ` + rowColumns + ` FROM decision_index
		WHERE ($1 = '' OR repo = $1)
		  AND ($2 = '' OR decision_class = $2)
		  AND ($3 = '' OR stage_kind = $3)
		  AND ($5::uuid IS NULL OR account_id = $5 OR account_id IS NULL)
		` + order + `
		LIMIT $4`
}

// List returns the rows matching f, ordered by source sequence.
func (s *Store) List(ctx context.Context, f ListFilter) ([]Row, error) {
	var limit *int
	if f.Limit > 0 {
		limit = &f.Limit
	}
	rows, err := s.db.Query(ctx, listSQL(f.Newest),
		f.Repo, string(f.DecisionClass), f.StageKind, limit, f.AccountID)
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

// Gaps runs the missing-row check: every decision-bearing entry on the chain
// with no decision_index row, split into true gaps (run row present) and
// orphaned entries (run row absent). limit bounds each returned list, not the
// counts; limit <= 0 means unbounded.
func (s *Store) Gaps(ctx context.Context, limit int) (*GapReport, error) {
	rows, err := s.db.Query(ctx, `SELECT ae.sequence, ae.category, ae.run_id, ae.ts, r.id IS NULL
		FROM audit_entries ae
		LEFT JOIN decision_index di ON di.source_sequence = ae.sequence
		LEFT JOIN runs r ON r.id = ae.run_id
		WHERE ae.category = ANY($1) AND di.source_sequence IS NULL
		ORDER BY ae.sequence`, DecisionBearingCategories())
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

// GateContext is the (repo, stage kind, touched paths, escalation keys) tuple a
// GATE-REFERENCE precedent query ranks against (E75.3 / #3731). Every field is
// derived through the SAME joins and the SAME two helpers the indexer used
// (contextSQL's LATERAL plan join with TouchedPathsFromPlan, and
// LatestEscalationKeys over escalationSQL's candidates), which is what makes a
// gate query rank against the same key VALUES the rows were indexed with. A
// divergent second derivation would make a gate query score differently from
// the very rows it is compared against.
type GateContext struct {
	Repo            string
	WorkflowID      string
	DoctrineVersion string
	StageKind       string
	TouchedPaths    []string
	EscalationKeys  []string
}

// GateRef names the run (and optionally the stage) a gate-reference query
// resolves, TOGETHER WITH the account the read is performed under.
//
// ACCOUNT SCOPE IS PART OF THE REFERENCE, not a separate later check (#3731
// binding condition 1): the resolve itself is narrowed, so a run in another
// account is INDISTINGUISHABLE from a nonexistent one — GateContext returns
// ErrRunMissing either way and no derived context is ever produced to leak.
//
// The predicate mirrors server.enforceAccount's ownership rule byte for byte:
// a row matches when its account_id IS NULL (the untenanted single-tenant
// window #1830 closes) or EQUALS AccountID. A NIL AccountID — a caller whose
// identity carries no workspace account, e.g. a bearer token — therefore
// matches ONLY untenanted runs, exactly as requireRunAccount already refuses
// such a caller on a tenanted run. This is deliberately STRICTER than
// ListFilter.AccountID's nil-matches-all: that one is the backfill's row-set
// predicate, this one is a run READ.
type GateRef struct {
	RunID     uuid.UUID
	StageID   *uuid.UUID
	AccountID *uuid.UUID
}

// gateContextSQL resolves the run, the named stage's kind, and the run's LATEST
// plan artifact. The plan LATERAL is contextSQL's, unchanged.
//
// The stage predicate is an EXISTS in the WHERE, not the LEFT JOIN's condition:
// a stage id that names a stage on a DIFFERENT run must make the whole resolve
// miss (the same 404 as a nonexistent run), where a LEFT JOIN would merely
// yield an empty stage_type and answer with a partially-derived context.
const gateContextSQL = `SELECT r.repo, r.workflow_id, r.workflow_sha,
	COALESCE(s.stage_type, ''), pa.content
FROM runs r
LEFT JOIN stages s ON s.id = $2 AND s.run_id = r.id
LEFT JOIN LATERAL (
	SELECT a.content FROM artifacts a
	JOIN stages ps ON ps.id = a.stage_id
	WHERE ps.run_id = r.id AND a.kind = 'plan'
	ORDER BY a.created_at DESC, a.id DESC
	LIMIT 1
) pa ON true
WHERE r.id = $1
  AND (r.account_id IS NULL OR r.account_id = $3)
  AND ($2::uuid IS NULL OR EXISTS (
	SELECT 1 FROM stages s2 WHERE s2.id = $2 AND s2.run_id = r.id))`

// GateContext derives the ranking keys for one run+stage pair, under ref's
// account scope. It returns ErrRunMissing when the run does not exist, belongs
// to another account, or the named stage is not on it — one indistinguishable
// outcome, on purpose.
func (s *Store) GateContext(ctx context.Context, ref GateRef) (GateContext, error) {
	var (
		gc      GateContext
		content []byte
	)
	err := s.db.QueryRow(ctx, gateContextSQL, ref.RunID, ref.StageID, ref.AccountID).
		Scan(&gc.Repo, &gc.WorkflowID, &gc.DoctrineVersion, &gc.StageKind, &content)
	if errors.Is(err, pgx.ErrNoRows) {
		return GateContext{}, fmt.Errorf("%w: run %s", ErrRunMissing, ref.RunID)
	}
	if err != nil {
		return GateContext{}, fmt.Errorf("decisionindex: gate context for run %s: %w", ref.RunID, err)
	}
	paths, err := TouchedPathsFromPlan(content)
	if err != nil {
		return GateContext{}, fmt.Errorf("decisionindex: gate context for run %s: %w", ref.RunID, err)
	}
	gc.TouchedPaths = paths
	gc.EscalationKeys = []string{}
	if ref.StageID == nil {
		return gc, nil
	}
	keys, err := s.latestStageEscalationKeys(ctx, *ref.StageID)
	if err != nil {
		return GateContext{}, err
	}
	gc.EscalationKeys = keys
	return gc, nil
}

// latestStageEscalationKeys returns the fired_keys of the LATEST
// escalation_fired entry on stageID, reusing escalationSQL and
// LatestEscalationKeys unchanged. math.MaxInt64 as the sequence ceiling is what
// "latest on this stage, with no decision to sit below" means: the indexer
// passes the decision's own sequence, a gate query has none.
func (s *Store) latestStageEscalationKeys(ctx context.Context, stageID uuid.UUID) ([]string, error) {
	rows, err := s.db.Query(ctx, escalationSQL, int64(math.MaxInt64),
		[]uuid.UUID{stageID}, []string{stageID.String()})
	if err != nil {
		return nil, fmt.Errorf("decisionindex: gate context escalations for stage %s: %w", stageID, err)
	}
	defer rows.Close()
	var cands []*audit.Entry
	for rows.Next() {
		c := &audit.Entry{Category: escalationFiredCategory}
		var payload []byte
		if err := rows.Scan(&c.Sequence, &c.StageID, &payload); err != nil {
			return nil, fmt.Errorf("decisionindex: gate context escalations: scan: %w", err)
		}
		c.Payload = json.RawMessage(payload)
		cands = append(cands, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("decisionindex: gate context escalations: %w", err)
	}
	return LatestEscalationKeys(cands, &stageID, math.MaxInt64), nil
}
