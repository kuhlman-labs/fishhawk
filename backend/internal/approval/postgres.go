package approval

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	approvaldb "github.com/kuhlman-labs/fishhawk/backend/internal/approval/db"
)

// postgresRepo is the production Repository implementation.
type postgresRepo struct {
	pool *pgxpool.Pool
}

// NewPostgresRepository wraps a pgxpool.Pool to satisfy Repository.
// Caller retains ownership of the pool.
func NewPostgresRepository(pool *pgxpool.Pool) Repository {
	return &postgresRepo{pool: pool}
}

func (r *postgresRepo) Submit(ctx context.Context, p SubmitParams) (*SubmitResult, error) {
	if !p.Decision.Valid() {
		return nil, fmt.Errorf("%w: %q", ErrInvalidDecision, p.Decision)
	}
	if p.Surface == "" {
		p.Surface = SurfaceAPI
	}
	if !p.Surface.Valid() {
		return nil, fmt.Errorf("%w: %q", ErrInvalidSurface, p.Surface)
	}
	if p.ApproverSubject == "" {
		return nil, ErrEmptyApprover
	}
	if p.StageID == uuid.Nil {
		return nil, fmt.Errorf("approval: StageID required")
	}

	q := approvaldb.New(r.pool)
	row, err := q.CreateApproval(ctx, approvaldb.CreateApprovalParams{
		ID:              uuid.New(),
		StageID:         p.StageID,
		ApproverSubject: p.ApproverSubject,
		Decision:        string(p.Decision),
		Comment:         p.Comment,
		Surface:         string(p.Surface),
	})
	if err == nil {
		return &SubmitResult{Approval: rowToApproval(row), Inserted: true}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("approval: insert: %w", err)
	}

	// ON CONFLICT DO NOTHING returned no rows; fetch the existing
	// approval so the caller sees the same shape regardless of
	// whether they're the first or second submitter.
	existing, err := q.GetApprovalByApprover(ctx, approvaldb.GetApprovalByApproverParams{
		StageID:         p.StageID,
		ApproverSubject: p.ApproverSubject,
	})
	if err != nil {
		return nil, fmt.Errorf("approval: fetch existing: %w", err)
	}
	return &SubmitResult{Approval: rowToApproval(existing), Inserted: false}, nil
}

func (r *postgresRepo) ListForStage(ctx context.Context, stageID uuid.UUID) ([]*Approval, error) {
	q := approvaldb.New(r.pool)
	rows, err := q.ListApprovalsForStage(ctx, stageID)
	if err != nil {
		return nil, fmt.Errorf("approval: list: %w", err)
	}
	out := make([]*Approval, 0, len(rows))
	for _, row := range rows {
		out = append(out, rowToApproval(row))
	}
	return out, nil
}

func rowToApproval(row approvaldb.Approval) *Approval {
	return &Approval{
		ID:              row.ID,
		StageID:         row.StageID,
		ApproverSubject: row.ApproverSubject,
		Decision:        Decision(row.Decision),
		Comment:         row.Comment,
		Surface:         Surface(row.Surface),
		SubmittedAt:     row.SubmittedAt.Time,
	}
}

// Compile-time check.
var _ Repository = (*postgresRepo)(nil)
