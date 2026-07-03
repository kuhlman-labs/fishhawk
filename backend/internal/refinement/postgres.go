package refinement

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	refinementdb "github.com/kuhlman-labs/fishhawk/backend/internal/refinement/db"
)

// postgresRepo is the production Repository implementation, backed by
// sqlc-generated queries and a pgxpool connection.
type postgresRepo struct {
	pool *pgxpool.Pool
}

// NewPostgresRepository wraps a pgxpool.Pool to satisfy Repository. Caller
// retains ownership of the pool.
func NewPostgresRepository(pool *pgxpool.Pool) Repository {
	return &postgresRepo{pool: pool}
}

func (r *postgresRepo) CreateDraft(ctx context.Context, p CreateParams) (*StoredDraft, error) {
	raw, err := json.Marshal(p.Draft)
	if err != nil {
		return nil, fmt.Errorf("marshal draft: %w", err)
	}
	var model *string
	if p.Model != "" {
		model = &p.Model
	}
	origin := p.Origin
	if origin == "" {
		origin = OriginBrief
	}
	id := p.ID
	if id == uuid.Nil {
		id = uuid.New()
	}
	q := refinementdb.New(r.pool)
	row, err := q.CreateRefinementDraft(ctx, refinementdb.CreateRefinementDraftParams{
		ID:        id,
		SessionID: p.SessionID,
		Brief:     p.Brief,
		Draft:     raw,
		Model:     model,
		Origin:    origin,
	})
	if err != nil {
		return nil, fmt.Errorf("create refinement draft: %w", err)
	}
	return rowToStoredDraft(row)
}

// RecordDecision appends an approve/reject verdict. The FK on draft_id
// (ON DELETE RESTRICT) means a decision referencing an unknown draft fails at
// insert — surfaced as a wrapped error, not a silent no-op.
func (r *postgresRepo) RecordDecision(ctx context.Context, p DecisionParams) (*Decision, error) {
	var decidedBy *string
	if p.DecidedBy != "" {
		decidedBy = &p.DecidedBy
	}
	q := refinementdb.New(r.pool)
	row, err := q.CreateRefinementDecision(ctx, refinementdb.CreateRefinementDecisionParams{
		ID:               uuid.New(),
		SessionID:        p.SessionID,
		DraftID:          p.DraftID,
		Decision:         p.Decision,
		Reason:           p.Reason,
		DraftContentHash: p.DraftContentHash,
		DecidedBy:        decidedBy,
	})
	if err != nil {
		return nil, fmt.Errorf("create refinement decision: %w", err)
	}
	return rowToDecision(row), nil
}

func (r *postgresRepo) ListDecisions(ctx context.Context, sessionID uuid.UUID) ([]*Decision, error) {
	q := refinementdb.New(r.pool)
	rows, err := q.ListRefinementDecisionsForSession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list refinement decisions: %w", err)
	}
	out := make([]*Decision, 0, len(rows))
	for _, row := range rows {
		out = append(out, rowToDecision(row))
	}
	return out, nil
}

func (r *postgresRepo) GetDraft(ctx context.Context, id uuid.UUID) (*StoredDraft, error) {
	q := refinementdb.New(r.pool)
	row, err := q.GetRefinementDraft(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get refinement draft: %w", err)
	}
	return rowToStoredDraft(row)
}

func (r *postgresRepo) ListForSession(ctx context.Context, sessionID uuid.UUID) ([]*StoredDraft, error) {
	q := refinementdb.New(r.pool)
	rows, err := q.ListRefinementDraftsForSession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list refinement drafts: %w", err)
	}
	out := make([]*StoredDraft, 0, len(rows))
	for _, row := range rows {
		sd, err := rowToStoredDraft(row)
		if err != nil {
			return nil, err
		}
		out = append(out, sd)
	}
	return out, nil
}

// CreateFilingSession opens the per-draft filing session. The draft_id PK +
// FK (ON DELETE RESTRICT) mean a second open for the same draft, or an open for
// an unknown draft, fails at insert — surfaced wrapped, not a silent no-op.
func (r *postgresRepo) CreateFilingSession(ctx context.Context, p FilingSessionParams) (*FilingSession, error) {
	q := refinementdb.New(r.pool)
	row, err := q.CreateRefinementFilingSession(ctx, refinementdb.CreateRefinementFilingSessionParams{
		DraftID:   p.DraftID,
		SessionID: p.SessionID,
		Repo:      p.Repo,
	})
	if err != nil {
		return nil, fmt.Errorf("create refinement filing session: %w", err)
	}
	return rowToFilingSession(row), nil
}

func (r *postgresRepo) GetFilingSession(ctx context.Context, draftID uuid.UUID) (*FilingSession, error) {
	q := refinementdb.New(r.pool)
	row, err := q.GetRefinementFilingSession(ctx, draftID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get refinement filing session: %w", err)
	}
	return rowToFilingSession(row), nil
}

func (r *postgresRepo) CompleteFilingSession(ctx context.Context, draftID uuid.UUID) error {
	q := refinementdb.New(r.pool)
	if err := q.CompleteRefinementFilingSession(ctx, draftID); err != nil {
		return fmt.Errorf("complete refinement filing session: %w", err)
	}
	return nil
}

// RecordFiledItem durably records one ordinal->issue mapping. The unique
// (draft_id, ordinal) constraint rejects a duplicate record (surfaced wrapped)
// — the residual never-double-record backstop.
func (r *postgresRepo) RecordFiledItem(ctx context.Context, p FiledItemParams) (*FiledItem, error) {
	q := refinementdb.New(r.pool)
	row, err := q.CreateRefinementFiledItem(ctx, refinementdb.CreateRefinementFiledItemParams{
		ID:          uuid.New(),
		DraftID:     p.DraftID,
		Ordinal:     int32(p.Ordinal),
		IssueNumber: int32(p.IssueNumber),
		IssueUrl:    p.IssueURL,
	})
	if err != nil {
		return nil, fmt.Errorf("record refinement filed item: %w", err)
	}
	return rowToFiledItem(row), nil
}

func (r *postgresRepo) ListFiledItems(ctx context.Context, draftID uuid.UUID) ([]*FiledItem, error) {
	q := refinementdb.New(r.pool)
	rows, err := q.ListRefinementFiledItems(ctx, draftID)
	if err != nil {
		return nil, fmt.Errorf("list refinement filed items: %w", err)
	}
	out := make([]*FiledItem, 0, len(rows))
	for _, row := range rows {
		out = append(out, rowToFiledItem(row))
	}
	return out, nil
}

// WithFilingLock acquires a session-level advisory lock keyed on the draft UUID
// on a dedicated pooled connection, runs fn, then releases the lock and the
// connection. A concurrent WithFilingLock for the same draft blocks at the lock
// acquisition until this one releases — the per-draft mutual exclusion that
// stops two filing invocations from both observing an ordinal as unfiled
// (ADR-052 concurrent-duplication guard). It is session-level (pg_advisory_lock,
// the pgtest bootstrap precedent) rather than xact-level so the executor's
// record-after-file commits stay independent and durable per item; the
// serialization guarantee is identical.
func (r *postgresRepo) WithFilingLock(ctx context.Context, draftID uuid.UUID, fn func(context.Context) error) error {
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire filing-lock connection: %w", err)
	}
	defer conn.Release()
	key := advisoryLockKey(draftID)
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", key); err != nil {
		return fmt.Errorf("acquire filing advisory lock: %w", err)
	}
	// Release on a background context so a cancelled ctx still unlocks. Release
	// runs before conn.Release() (defer LIFO) so the connection is unlocked
	// before it returns to the pool.
	defer func() { _, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", key) }()
	return fn(ctx)
}

// advisoryLockKey derives a stable int64 advisory-lock key from a draft UUID by
// reading its first 8 bytes big-endian. Two different drafts collide only on a
// 1-in-2^64 hash accident, and a collision merely serializes two unrelated
// filings — never a correctness problem.
func advisoryLockKey(draftID uuid.UUID) int64 {
	return int64(binary.BigEndian.Uint64(draftID[:8]))
}

func rowToFilingSession(row refinementdb.RefinementFilingSession) *FilingSession {
	var completedAt *time.Time
	if row.CompletedAt.Valid {
		t := row.CompletedAt.Time
		completedAt = &t
	}
	return &FilingSession{
		DraftID:     row.DraftID,
		SessionID:   row.SessionID,
		Repo:        row.Repo,
		CreatedAt:   row.CreatedAt.Time,
		CompletedAt: completedAt,
	}
}

func rowToFiledItem(row refinementdb.RefinementFiledItem) *FiledItem {
	return &FiledItem{
		DraftID:     row.DraftID,
		Ordinal:     int(row.Ordinal),
		IssueNumber: int(row.IssueNumber),
		IssueURL:    row.IssueUrl,
		CreatedAt:   row.CreatedAt.Time,
	}
}

// rowToStoredDraft unmarshals the JSONB draft column back into an EpicDraft
// and maps the nullable model column to a string ("" when NULL).
func rowToStoredDraft(r refinementdb.RefinementDraft) (*StoredDraft, error) {
	var draft EpicDraft
	if err := json.Unmarshal(r.Draft, &draft); err != nil {
		return nil, fmt.Errorf("unmarshal draft %s: %w", r.ID, err)
	}
	model := ""
	if r.Model != nil {
		model = *r.Model
	}
	return &StoredDraft{
		ID:        r.ID,
		SessionID: r.SessionID,
		Brief:     r.Brief,
		Draft:     draft,
		Model:     model,
		Origin:    r.Origin,
		CreatedAt: r.CreatedAt.Time,
	}, nil
}

// rowToDecision maps a refinement_decisions row to the domain Decision,
// resolving the nullable decided_by column to a string ("" when NULL).
func rowToDecision(r refinementdb.RefinementDecision) *Decision {
	decidedBy := ""
	if r.DecidedBy != nil {
		decidedBy = *r.DecidedBy
	}
	return &Decision{
		ID:               r.ID,
		SessionID:        r.SessionID,
		DraftID:          r.DraftID,
		Decision:         r.Decision,
		Reason:           r.Reason,
		DraftContentHash: r.DraftContentHash,
		DecidedBy:        decidedBy,
		CreatedAt:        r.CreatedAt.Time,
	}
}

// Compile-time check that postgresRepo implements Repository.
var _ Repository = (*postgresRepo)(nil)
