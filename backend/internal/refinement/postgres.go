package refinement

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

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
