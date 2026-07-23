package repoacl

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/identity"
	repoacldb "github.com/kuhlman-labs/fishhawk/backend/internal/repoacl/db"
)

// postgresStore is the production Store implementation, mirroring the shape of
// internal/concern/postgres.go.
type postgresStore struct {
	q *repoacldb.Queries
}

// NewPostgresStore wraps a pgxpool.Pool to satisfy Store. Caller retains
// ownership of pool.
func NewPostgresStore(pool *pgxpool.Pool) Store {
	return &postgresStore{q: repoacldb.New(pool)}
}

func (s *postgresStore) Get(ctx context.Context, provider, subject, repo string) (Entry, bool, error) {
	row, err := s.q.GetRepoACLEntry(ctx, repoacldb.GetRepoACLEntryParams{
		Provider: provider,
		Subject:  subject,
		Repo:     repo,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// A miss, not a fault — the caller resolves live.
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, fmt.Errorf("repoacl: get entry: %w", err)
	}
	return Entry{
		Permission: identity.Permission(row.Permission),
		CheckedAt:  row.CheckedAt.Time,
	}, true, nil
}

func (s *postgresStore) Upsert(ctx context.Context, provider, subject, repo string, perm identity.Permission) error {
	// The ID is only consumed on INSERT; a conflicting row keeps its own PK.
	if _, err := s.q.UpsertRepoACLEntry(ctx, repoacldb.UpsertRepoACLEntryParams{
		ID:         uuid.New(),
		Provider:   provider,
		Subject:    subject,
		Repo:       repo,
		Permission: string(perm),
	}); err != nil {
		return fmt.Errorf("repoacl: upsert entry: %w", err)
	}
	return nil
}

func (s *postgresStore) DeleteForSubject(ctx context.Context, provider, subject string) error {
	if err := s.q.DeleteRepoACLEntriesForSubject(ctx, repoacldb.DeleteRepoACLEntriesForSubjectParams{
		Provider: provider,
		Subject:  subject,
	}); err != nil {
		return fmt.Errorf("repoacl: purge subject: %w", err)
	}
	return nil
}
