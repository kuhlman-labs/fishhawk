package stagecheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	stagecheckdb "github.com/kuhlman-labs/fishhawk/backend/internal/stagecheck/db"
)

type postgresRepo struct {
	pool *pgxpool.Pool
}

// NewPostgresRepository wraps a pgxpool.Pool to satisfy Repository.
// Caller retains ownership of the pool.
func NewPostgresRepository(pool *pgxpool.Pool) Repository {
	return &postgresRepo{pool: pool}
}

func (r *postgresRepo) Append(ctx context.Context, p AppendParams) (*Check, error) {
	q := stagecheckdb.New(r.pool)
	payload := p.Payload
	if payload == nil {
		payload = json.RawMessage("{}")
	}
	row, err := q.InsertStageCheck(ctx, stagecheckdb.InsertStageCheckParams{
		ID:               uuid.New(),
		StageID:          p.StageID,
		CheckName:        p.Name,
		Status:           p.Status,
		Conclusion:       p.Conclusion,
		HeadSha:          p.HeadSHA,
		GithubCheckRunID: p.GitHubCheckRunID,
		Ts:               pgtype.Timestamptz{Time: p.Timestamp, Valid: true},
		Payload:          payload,
	})
	if err != nil {
		return nil, fmt.Errorf("insert stage check: %w", err)
	}
	return rowToCheck(row), nil
}

func (r *postgresRepo) LatestForStage(ctx context.Context, stageID uuid.UUID) ([]*Check, error) {
	q := stagecheckdb.New(r.pool)
	rows, err := q.ListStageChecksLatest(ctx, stageID)
	if err != nil {
		return nil, fmt.Errorf("list stage checks: %w", err)
	}
	out := make([]*Check, 0, len(rows))
	for _, row := range rows {
		out = append(out, rowToCheck(row))
	}
	return out, nil
}

func (r *postgresRepo) LatestForStageAndName(ctx context.Context, stageID uuid.UUID, name string) (*Check, error) {
	q := stagecheckdb.New(r.pool)
	row, err := q.GetStageCheckLatest(ctx, stagecheckdb.GetStageCheckLatestParams{
		StageID:   stageID,
		CheckName: name,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get stage check: %w", err)
	}
	return rowToCheck(row), nil
}

func (r *postgresRepo) FindMatchingStages(ctx context.Context, prNumber int, headSHA, checkName string) ([]uuid.UUID, error) {
	q := stagecheckdb.New(r.pool)
	rows, err := q.FindRunStagesForCheckRun(ctx, stagecheckdb.FindRunStagesForCheckRunParams{
		PrNumber:  int32(prNumber),
		HeadSha:   headSHA,
		CheckName: checkName,
	})
	if err != nil {
		return nil, fmt.Errorf("find matching stages: %w", err)
	}
	out := make([]uuid.UUID, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.ID)
	}
	return out, nil
}

func rowToCheck(r stagecheckdb.StageCheck) *Check {
	out := &Check{
		ID:               r.ID,
		StageID:          r.StageID,
		Name:             r.CheckName,
		State:            DeriveState(r.Status, r.Conclusion),
		Status:           r.Status,
		Conclusion:       r.Conclusion,
		HeadSHA:          r.HeadSha,
		GitHubCheckRunID: r.GithubCheckRunID,
		Timestamp:        r.Ts.Time,
		Payload:          json.RawMessage(r.Payload),
	}
	return out
}
