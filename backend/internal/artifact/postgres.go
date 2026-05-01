package artifact

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	artifactdb "github.com/kuhlman-labs/fishhawk/backend/internal/artifact/db"
)

// postgresRepo is the production Repository implementation, backed
// by sqlc-generated queries and a pgxpool connection.
type postgresRepo struct {
	pool *pgxpool.Pool
}

// NewPostgresRepository wraps a pgxpool.Pool to satisfy Repository.
// Caller retains ownership of the pool.
func NewPostgresRepository(pool *pgxpool.Pool) Repository {
	return &postgresRepo{pool: pool}
}

func (r *postgresRepo) Create(ctx context.Context, p CreateParams) (*Artifact, error) {
	q := artifactdb.New(r.pool)
	row, err := q.CreateArtifact(ctx, artifactdb.CreateArtifactParams{
		ID:            uuid.New(),
		StageID:       p.StageID,
		Kind:          string(p.Kind),
		SchemaVersion: p.SchemaVersion,
		Content:       []byte(p.Content),
		ContentHash:   p.ContentHash,
	})
	if err != nil {
		return nil, fmt.Errorf("create artifact: %w", err)
	}
	return rowToArtifact(row), nil
}

func (r *postgresRepo) Get(ctx context.Context, id uuid.UUID) (*Artifact, error) {
	q := artifactdb.New(r.pool)
	row, err := q.GetArtifact(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get artifact: %w", err)
	}
	return rowToArtifact(row), nil
}

func (r *postgresRepo) ListForStage(ctx context.Context, stageID uuid.UUID) ([]*Artifact, error) {
	q := artifactdb.New(r.pool)
	rows, err := q.ListArtifactsForStage(ctx, stageID)
	if err != nil {
		return nil, fmt.Errorf("list artifacts: %w", err)
	}
	out := make([]*Artifact, 0, len(rows))
	for _, row := range rows {
		out = append(out, rowToArtifact(row))
	}
	return out, nil
}

func (r *postgresRepo) GetByHash(ctx context.Context, stageID uuid.UUID, contentHash string) (*Artifact, error) {
	q := artifactdb.New(r.pool)
	row, err := q.GetArtifactByHash(ctx, artifactdb.GetArtifactByHashParams{
		StageID:     stageID,
		ContentHash: contentHash,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get artifact by hash: %w", err)
	}
	return rowToArtifact(row), nil
}

func rowToArtifact(r artifactdb.Artifact) *Artifact {
	return &Artifact{
		ID:            r.ID,
		StageID:       r.StageID,
		Kind:          Kind(r.Kind),
		SchemaVersion: r.SchemaVersion,
		Content:       r.Content,
		ContentHash:   r.ContentHash,
		CreatedAt:     r.CreatedAt.Time,
	}
}

// Compile-time check that postgresRepo implements Repository.
var _ Repository = (*postgresRepo)(nil)
