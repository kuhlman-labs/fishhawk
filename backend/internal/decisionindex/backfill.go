package decisionindex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
)

// ErrRunMissing is returned by the per-entry resolver when the entry carries no
// run id or its run row does not exist. The writer logs it; the backfill counts
// it as rows_skipped_no_run; the gap check reports such entries as orphaned.
var ErrRunMissing = errors.New("decisionindex: run row not found")

// ContextResolver resolves the joined RowContext for one audit entry. It is the
// seam the live writer resolves through; the backfill resolves a whole page
// through the SAME resolveContexts query, so both paths hand Extract the same
// input for the same entry.
type ContextResolver interface {
	Resolve(ctx context.Context, e *audit.Entry) (RowContext, error)
}

// PoolResolver is the database-backed ContextResolver.
type PoolResolver struct {
	db DBTX
}

// NewPoolResolver returns a resolver over db.
func NewPoolResolver(db DBTX) *PoolResolver {
	return &PoolResolver{db: db}
}

// Resolve resolves e's RowContext, or ErrRunMissing when e has no run row.
func (r *PoolResolver) Resolve(ctx context.Context, e *audit.Entry) (RowContext, error) {
	if e == nil {
		return RowContext{}, ErrNilEntry
	}
	m, err := resolveContexts(ctx, r.db, []*audit.Entry{e})
	if err != nil {
		return RowContext{}, err
	}
	rc, ok := m[e.Sequence]
	if !ok {
		return RowContext{}, fmt.Errorf("%w: %s entry at sequence %d", ErrRunMissing, e.Category, e.Sequence)
	}
	return rc, nil
}

// concernIDPattern guards the review_concerns join: only a payload concern_id
// shaped like a UUID is cast, so a malformed id joins nothing rather than
// aborting the whole page on a cast error.
const concernIDPattern = `^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`

// contextSQL is the batched per-page join: runs (INNER — an entry whose run
// row is gone yields no context and is reported, never guessed at), the
// entry's stage, the run's LATEST plan artifact, and the review_concerns row
// the payload's concern_id names.
const contextSQL = `SELECT ae.sequence, r.repo, r.workflow_id, r.workflow_sha,
	COALESCE(s.stage_type, ''), pa.content,
	COALESCE(rc.category, ''), COALESCE(rc.severity, '')
FROM audit_entries ae
JOIN runs r ON r.id = ae.run_id
LEFT JOIN stages s ON s.id = ae.stage_id
LEFT JOIN LATERAL (
	SELECT a.content FROM artifacts a
	JOIN stages ps ON ps.id = a.stage_id
	WHERE ps.run_id = ae.run_id AND a.kind = 'plan'
	ORDER BY a.created_at DESC, a.id DESC
	LIMIT 1
) pa ON true
LEFT JOIN review_concerns rc ON rc.id = CASE
	WHEN ae.payload->>'concern_id' ~ '` + concernIDPattern + `'
	THEN (ae.payload->>'concern_id')::uuid END
WHERE ae.sequence = ANY($1)`

// escalationSQL fetches the escalation_fired candidates for a page: every such
// entry below the page's highest sequence on one of the page's stages (by
// column, or by payload stage_id when the column is unset). LatestEscalationKeys
// then picks, per decision, the latest one strictly below it on its own stage.
const escalationSQL = `SELECT sequence, stage_id, payload FROM audit_entries
WHERE category = '` + escalationFiredCategory + `' AND sequence < $1
  AND (stage_id = ANY($2) OR (stage_id IS NULL AND payload->>'stage_id' = ANY($3)))
ORDER BY sequence`

// resolveContexts resolves the RowContext of every entry whose run row exists,
// keyed by sequence, in two bounded queries regardless of page size. An entry
// with no run id or no run row is ABSENT from the map.
func resolveContexts(ctx context.Context, db DBTX, entries []*audit.Entry) (map[int64]RowContext, error) {
	out := map[int64]RowContext{}
	var (
		seqs      []int64
		stageSet  = map[uuid.UUID]struct{}{}
		maxSeq    int64
		withStage bool
	)
	for _, e := range entries {
		if e == nil || e.RunID == nil {
			continue
		}
		seqs = append(seqs, e.Sequence)
		if e.Sequence > maxSeq {
			maxSeq = e.Sequence
		}
		if e.StageID != nil {
			stageSet[*e.StageID] = struct{}{}
			withStage = true
		}
	}
	if len(seqs) == 0 {
		return out, nil
	}

	rows, err := db.Query(ctx, contextSQL, seqs)
	if err != nil {
		return nil, fmt.Errorf("decisionindex: resolve context: %w", err)
	}
	for rows.Next() {
		var (
			seq     int64
			rc      RowContext
			content []byte
		)
		if err := rows.Scan(&seq, &rc.Repo, &rc.WorkflowID, &rc.DoctrineVersion, &rc.StageKind,
			&content, &rc.ConcernCategory, &rc.ConcernSeverity); err != nil {
			rows.Close()
			return nil, fmt.Errorf("decisionindex: resolve context: scan: %w", err)
		}
		paths, err := TouchedPathsFromPlan(content)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("decisionindex: resolve context for sequence %d: %w", seq, err)
		}
		rc.TouchedPaths = paths
		out[seq] = rc
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("decisionindex: resolve context: %w", err)
	}

	if !withStage {
		return out, nil
	}
	stages := make([]uuid.UUID, 0, len(stageSet))
	stageStrs := make([]string, 0, len(stageSet))
	for id := range stageSet {
		stages = append(stages, id)
		stageStrs = append(stageStrs, id.String())
	}
	erows, err := db.Query(ctx, escalationSQL, maxSeq, stages, stageStrs)
	if err != nil {
		return nil, fmt.Errorf("decisionindex: resolve escalations: %w", err)
	}
	var cands []*audit.Entry
	for erows.Next() {
		c := &audit.Entry{Category: escalationFiredCategory}
		var payload []byte
		if err := erows.Scan(&c.Sequence, &c.StageID, &payload); err != nil {
			erows.Close()
			return nil, fmt.Errorf("decisionindex: resolve escalations: scan: %w", err)
		}
		c.Payload = json.RawMessage(payload)
		cands = append(cands, c)
	}
	erows.Close()
	if err := erows.Err(); err != nil {
		return nil, fmt.Errorf("decisionindex: resolve escalations: %w", err)
	}
	for _, e := range entries {
		if e == nil {
			continue
		}
		rc, ok := out[e.Sequence]
		if !ok {
			continue
		}
		rc.EscalationKeys = LatestEscalationKeys(cands, e.StageID, e.Sequence)
		out[e.Sequence] = rc
	}
	return out, nil
}

// DefaultPageSize is the backfill's keyset page size.
const DefaultPageSize = 500

// Options configures a Backfill.
type Options struct {
	// Rebuild truncates the index first (the from-scratch path). Without it
	// the run is a converging top-up over the existing rows.
	Rebuild bool
	// DryRun extracts and counts but writes nothing (and truncates nothing).
	DryRun bool
	// PageSize bounds each keyset page; <= 0 selects DefaultPageSize.
	PageSize int
}

// Summary reports a Backfill.
type Summary struct {
	Rebuild bool `json:"rebuild"`
	DryRun  bool `json:"dry_run"`
	// EntriesScanned counts decision-bearing entries read from the chain.
	EntriesScanned int `json:"entries_scanned"`
	// RowsExtracted counts rows Extract produced; RowsWritten those upserted
	// (zero on a dry run).
	RowsExtracted int `json:"rows_extracted"`
	RowsWritten   int `json:"rows_written"`
	// RowsSkippedNoRun counts entries with no run id or no run row, named by
	// SkippedNoRunSequences — reported, never guessed at.
	RowsSkippedNoRun      int     `json:"rows_skipped_no_run"`
	SkippedNoRunSequences []int64 `json:"skipped_no_run_sequences"`
	// UnmappedCategories is the sorted distinct set of concern categories the
	// normalization table does not map; those rows are still written, indexed
	// as themselves.
	UnmappedCategories []string `json:"unmapped_categories"`
	// GapsRemaining / OrphanedEntries are the post-run gap check.
	GapsRemaining   int `json:"gaps_remaining"`
	OrphanedEntries int `json:"orphaned_entries"`
}

// entrySQL pages the decision-bearing entries ascending by sequence. It is a
// bounded keyset page — never audit.Repository.ListAll, which has no LIMIT.
const entrySQL = `SELECT id, sequence, run_id, stage_id, ts, category, actor_kind,
	actor_subject, payload, prev_hash, entry_hash, account_id
FROM audit_entries
WHERE category = ANY($1) AND sequence > $2
ORDER BY sequence
LIMIT $3`

// Backfill reconstructs the index from the chain. Every row goes through the
// same Extract the live writer uses, so a rebuild equals what the writer built.
// A decision-bearing entry whose payload is not a JSON object fails the run
// LOUDLY, naming the sequence: an index silently missing a decision is the
// failure the gap check exists to surface, not one to paper over.
func Backfill(ctx context.Context, db DBTX, opts Options) (*Summary, error) {
	pageSize := opts.PageSize
	if pageSize <= 0 {
		pageSize = DefaultPageSize
	}
	store := NewStore(db)
	sum := &Summary{
		Rebuild:               opts.Rebuild,
		DryRun:                opts.DryRun,
		SkippedNoRunSequences: []int64{},
		UnmappedCategories:    []string{},
	}
	if opts.Rebuild && !opts.DryRun {
		if err := store.Truncate(ctx); err != nil {
			return nil, err
		}
	}
	categories := DecisionBearingCategories()
	unmapped := map[string]struct{}{}
	var after int64
	for {
		page, err := readPage(ctx, db, categories, after, pageSize)
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			break
		}
		after = page[len(page)-1].Sequence
		sum.EntriesScanned += len(page)

		contexts, err := resolveContexts(ctx, db, page)
		if err != nil {
			return nil, err
		}
		rows := make([]Row, 0, len(page))
		for _, e := range page {
			rc, ok := contexts[e.Sequence]
			if !ok {
				sum.RowsSkippedNoRun++
				sum.SkippedNoRunSequences = append(sum.SkippedNoRunSequences, e.Sequence)
				continue
			}
			row, err := Extract(e, rc)
			if err != nil {
				return nil, err
			}
			if row.ConcernCategoryUnmapped {
				unmapped[row.ConcernCategory] = struct{}{}
			}
			rows = append(rows, *row)
		}
		sum.RowsExtracted += len(rows)
		if !opts.DryRun {
			if err := store.UpsertBatch(ctx, rows); err != nil {
				return nil, err
			}
			sum.RowsWritten += len(rows)
		}
		if len(page) < pageSize {
			break
		}
	}
	for c := range unmapped {
		sum.UnmappedCategories = append(sum.UnmappedCategories, c)
	}
	sort.Strings(sum.UnmappedCategories)

	rep, err := store.Gaps(ctx, 1)
	if err != nil {
		return nil, err
	}
	sum.GapsRemaining = rep.GapCount
	sum.OrphanedEntries = rep.OrphanedCount
	return sum, nil
}

func readPage(ctx context.Context, db DBTX, categories []string, after int64, limit int) ([]*audit.Entry, error) {
	rows, err := db.Query(ctx, entrySQL, categories, after, limit)
	if err != nil {
		return nil, fmt.Errorf("decisionindex: read page after sequence %d: %w", after, err)
	}
	defer rows.Close()
	var out []*audit.Entry
	for rows.Next() {
		var (
			e         audit.Entry
			actorKind *string
			payload   []byte
		)
		if err := rows.Scan(&e.ID, &e.Sequence, &e.RunID, &e.StageID, &e.Timestamp, &e.Category,
			&actorKind, &e.ActorSubject, &payload, &e.PrevHash, &e.EntryHash, &e.AccountID); err != nil {
			return nil, fmt.Errorf("decisionindex: read page: scan: %w", err)
		}
		if actorKind != nil {
			k := audit.ActorKind(*actorKind)
			e.ActorKind = &k
		}
		e.Payload = json.RawMessage(payload)
		out = append(out, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("decisionindex: read page: %w", err)
	}
	return out, nil
}
