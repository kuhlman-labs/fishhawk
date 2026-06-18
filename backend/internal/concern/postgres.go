package concern

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	concerndb "github.com/kuhlman-labs/fishhawk/backend/internal/concern/db"
)

// postgresRepo is the production Repository implementation.
type postgresRepo struct {
	pool *pgxpool.Pool
}

// NewPostgresRepository wraps a pgxpool.Pool to satisfy Repository.
// Caller retains ownership of pool.
func NewPostgresRepository(pool *pgxpool.Pool) Repository {
	return &postgresRepo{pool: pool}
}

func (r *postgresRepo) InsertRaised(ctx context.Context, p InsertRaisedParams) ([]*Concern, error) {
	if p.RunID == uuid.Nil {
		return nil, errors.New("concern: run_id required")
	}
	if p.StageID == uuid.Nil {
		return nil, errors.New("concern: stage_id required")
	}
	if p.StageKind != StageKindPlan && p.StageKind != StageKindImplement {
		return nil, fmt.Errorf("concern: stage_kind must be %q or %q, got %q",
			StageKindPlan, StageKindImplement, p.StageKind)
	}
	var reviewerModel *string
	if p.ReviewerModel != "" {
		m := p.ReviewerModel
		reviewerModel = &m
	}
	q := concerndb.New(r.pool)
	out := make([]*Concern, 0, len(p.Concerns))
	for _, c := range p.Concerns {
		row, err := q.InsertReviewConcern(ctx, concerndb.InsertReviewConcernParams{
			ID:                   uuid.New(),
			RunID:                p.RunID,
			StageID:              p.StageID,
			StageKind:            p.StageKind,
			OriginReviewSequence: p.OriginReviewSequence,
			ReviewerModel:        reviewerModel,
			Severity:             c.Severity,
			Category:             c.Category,
			Note:                 c.Note,
			SuggestedPatch:       c.SuggestedPatch,
		})
		if err != nil {
			return nil, fmt.Errorf("concern: insert: %w", err)
		}
		out = append(out, rowToConcern(row))
	}
	return out, nil
}

func (r *postgresRepo) GetByIDs(ctx context.Context, ids []uuid.UUID) ([]*Concern, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	q := concerndb.New(r.pool)
	rows, err := q.GetReviewConcernsByIDs(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("concern: get by ids: %w", err)
	}
	byID := make(map[uuid.UUID]*Concern, len(rows))
	for _, row := range rows {
		byID[row.ID] = rowToConcern(row)
	}
	out := make([]*Concern, 0, len(ids))
	for _, id := range ids {
		c, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		out = append(out, c)
	}
	return out, nil
}

func (r *postgresRepo) ListByRun(ctx context.Context, runID uuid.UUID) ([]*Concern, error) {
	q := concerndb.New(r.pool)
	rows, err := q.ListReviewConcernsByRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("concern: list by run: %w", err)
	}
	return rowsToConcerns(rows), nil
}

func (r *postgresRepo) ListOpenByRun(ctx context.Context, runID uuid.UUID) ([]*Concern, error) {
	q := concerndb.New(r.pool)
	rows, err := q.ListOpenReviewConcernsByRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("concern: list open by run: %w", err)
	}
	return rowsToConcerns(rows), nil
}

func (r *postgresRepo) MarkAddressedPending(ctx context.Context, ids []uuid.UUID, reason string) error {
	current, err := r.GetByIDs(ctx, ids)
	if err != nil {
		return err
	}
	for _, c := range current {
		if c.State == StateAddressedPending {
			// Idempotent: a re-routed concern (forced second pass) is
			// already in the target state.
			continue
		}
		if _, err := r.transition(ctx, c, StateAddressedPending, reason); err != nil {
			return err
		}
	}
	return nil
}

func (r *postgresRepo) ApplyResolution(ctx context.Context, id uuid.UUID, to State, reason string) (*Concern, error) {
	current, err := r.GetByIDs(ctx, []uuid.UUID{id})
	if err != nil {
		return nil, err
	}
	return r.transition(ctx, current[0], to, reason)
}

// transition validates the lifecycle edge against the Go state machine
// and applies it with a from-state guard so a concurrent writer cannot
// race past the validation (the guarded UPDATE matching zero rows means
// the state moved underneath us — surfaced as an error, never silently
// applied).
func (r *postgresRepo) transition(ctx context.Context, c *Concern, to State, reason string) (*Concern, error) {
	if err := Transition(c.State, to); err != nil {
		return nil, err
	}
	q := concerndb.New(r.pool)
	row, err := q.UpdateReviewConcernState(ctx, concerndb.UpdateReviewConcernStateParams{
		ID:          c.ID,
		State:       string(to),
		StateReason: reason,
		FromState:   string(c.State),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("concern: %s state changed concurrently (was %s)", c.ID, c.State)
	}
	if err != nil {
		return nil, fmt.Errorf("concern: update state: %w", err)
	}
	return rowToConcern(row), nil
}

func rowsToConcerns(rows []concerndb.ReviewConcern) []*Concern {
	out := make([]*Concern, 0, len(rows))
	for _, row := range rows {
		out = append(out, rowToConcern(row))
	}
	return out
}

func rowToConcern(r concerndb.ReviewConcern) *Concern {
	out := &Concern{
		ID:                   r.ID,
		RunID:                r.RunID,
		StageID:              r.StageID,
		StageKind:            r.StageKind,
		OriginReviewSequence: r.OriginReviewSequence,
		ReviewerModel:        r.ReviewerModel,
		Severity:             r.Severity,
		Category:             r.Category,
		Note:                 r.Note,
		State:                State(r.State),
		StateReason:          r.StateReason,
		SuggestedPatch:       r.SuggestedPatch,
	}
	if r.CreatedAt.Valid {
		out.CreatedAt = r.CreatedAt.Time
	}
	if r.UpdatedAt.Valid {
		out.UpdatedAt = r.UpdatedAt.Time
	}
	return out
}

// Compile-time check.
var _ Repository = (*postgresRepo)(nil)
