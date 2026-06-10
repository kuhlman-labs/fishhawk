package scopeamendment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	amendmentdb "github.com/kuhlman-labs/fishhawk/backend/internal/scopeamendment/db"
)

// postgresRepo is the production Repository implementation.
type postgresRepo struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// NewPostgresRepository wraps a pgxpool.Pool to satisfy Repository.
// Caller retains ownership of pool.
func NewPostgresRepository(pool *pgxpool.Pool) Repository {
	return &postgresRepo{
		pool: pool,
		now:  func() time.Time { return time.Now().UTC() },
	}
}

func (r *postgresRepo) Create(ctx context.Context, p CreateParams) (*Amendment, error) {
	if p.RunID == uuid.Nil {
		return nil, errors.New("scopeamendment: run_id required")
	}
	if p.StageID == uuid.Nil {
		return nil, errors.New("scopeamendment: stage_id required")
	}
	paths, err := ValidatePaths(p.Paths)
	if err != nil {
		return nil, fmt.Errorf("scopeamendment: %w", err)
	}
	pathsJSON, err := json.Marshal(paths)
	if err != nil {
		return nil, fmt.Errorf("scopeamendment: marshal paths: %w", err)
	}
	q := amendmentdb.New(r.pool)
	row, err := q.CreateScopeAmendment(ctx, amendmentdb.CreateScopeAmendmentParams{
		ID:      uuid.New(),
		RunID:   p.RunID,
		StageID: p.StageID,
		Paths:   pathsJSON,
		Reason:  p.Reason,
	})
	if err != nil {
		return nil, fmt.Errorf("scopeamendment: create: %w", err)
	}
	return rowToAmendment(row)
}

func (r *postgresRepo) GetByID(ctx context.Context, id uuid.UUID) (*Amendment, error) {
	q := amendmentdb.New(r.pool)
	row, err := q.GetScopeAmendmentByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scopeamendment: get: %w", err)
	}
	return rowToAmendment(row)
}

func (r *postgresRepo) ListByRun(ctx context.Context, runID uuid.UUID) ([]*Amendment, error) {
	q := amendmentdb.New(r.pool)
	rows, err := q.ListScopeAmendmentsByRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("scopeamendment: list: %w", err)
	}
	out := make([]*Amendment, 0, len(rows))
	for _, row := range rows {
		a, err := rowToAmendment(row)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

func (r *postgresRepo) CountByStage(ctx context.Context, stageID uuid.UUID) (int, error) {
	q := amendmentdb.New(r.pool)
	n, err := q.CountScopeAmendmentsByStage(ctx, stageID)
	if err != nil {
		return 0, fmt.Errorf("scopeamendment: count: %w", err)
	}
	return int(n), nil
}

func (r *postgresRepo) Decide(ctx context.Context, p DecideParams) (*Amendment, error) {
	if p.Status != StatusApproved && p.Status != StatusDenied {
		return nil, fmt.Errorf("scopeamendment: decision status must be %q or %q, got %q",
			StatusApproved, StatusDenied, p.Status)
	}
	q := amendmentdb.New(r.pool)
	reason := p.Reason
	decidedBy := p.DecidedBy
	row, err := q.DecideScopeAmendment(ctx, amendmentdb.DecideScopeAmendmentParams{
		ID:             p.ID,
		Status:         string(p.Status),
		DecisionReason: &reason,
		DecidedBy:      &decidedBy,
		DecidedAt:      pgtype.Timestamptz{Time: r.now(), Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// The pending-only WHERE matched nothing: either the row is
		// already decided, or it never existed. Disambiguate with a
		// plain read so handlers can map 409 vs 404.
		if _, gerr := r.GetByID(ctx, p.ID); gerr == nil {
			return nil, ErrAlreadyDecided
		}
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scopeamendment: decide: %w", err)
	}
	return rowToAmendment(row)
}

func rowToAmendment(r amendmentdb.ScopeAmendment) (*Amendment, error) {
	var paths []PathEntry
	if err := json.Unmarshal(r.Paths, &paths); err != nil {
		return nil, fmt.Errorf("scopeamendment: unmarshal paths: %w", err)
	}
	out := &Amendment{
		ID:             r.ID,
		RunID:          r.RunID,
		StageID:        r.StageID,
		Paths:          paths,
		Reason:         r.Reason,
		Status:         Status(r.Status),
		DecisionReason: r.DecisionReason,
		DecidedBy:      r.DecidedBy,
	}
	if r.RequestedAt.Valid {
		out.RequestedAt = r.RequestedAt.Time
	}
	if r.DecidedAt.Valid {
		t := r.DecidedAt.Time
		out.DecidedAt = &t
	}
	return out, nil
}

// Compile-time check.
var _ Repository = (*postgresRepo)(nil)
