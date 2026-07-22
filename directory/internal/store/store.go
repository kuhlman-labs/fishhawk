// Package store is the directory plane's persistence: one table mapping
// (provider, account_key) to the region that owns the account (ADR-062).
//
// The only mutation is AssignRegion, and it is atomic by construction —
// a single INSERT ... ON CONFLICT DO UPDATE ... RETURNING under the
// primary key, so the FIRST writer's region wins and every concurrent
// caller reads that winner back instead of overwriting it. There is no
// read-then-write path to race.
//
// The package deliberately holds nothing else. Region -> cell base URL
// lives in the directory's env config, and install/OAuth correlation state
// is out of scope for this plane (see directory/README.md).
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Errors callers match with errors.Is.
var (
	// ErrNotFound reports that no region has been assigned to the account.
	ErrNotFound = errors.New("store: account has no assigned region")
	// ErrInvalidInput reports an empty provider, account key, or region.
	// Empty identity would key a row nobody can address, so it is refused
	// at the boundary rather than persisted.
	ErrInvalidInput = errors.New("store: provider, account key, and region must be non-empty")
)

// Store reads and writes account -> home region assignments.
type Store struct {
	pool *pgxpool.Pool
}

// New returns a Store backed by pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// AssignRegion assigns region to the account if it has none, and returns
// the region that actually owns the account afterwards — which is the
// FIRST assignment ever made, not necessarily the one proposed here.
//
// Callers must compare the returned region against the one they proposed:
// a difference means someone else won the assignment and the account lives
// elsewhere. That is a normal outcome, not an error.
//
// The DO UPDATE branch re-writes the existing home_region with itself
// rather than DO NOTHING, because DO NOTHING suppresses RETURNING for the
// conflicting row and the caller would get no answer at all.
func (s *Store) AssignRegion(ctx context.Context, provider, accountKey, region string) (string, error) {
	if provider == "" || accountKey == "" || region == "" {
		return "", ErrInvalidInput
	}

	const q = `
INSERT INTO account_regions (provider, account_key, home_region)
VALUES ($1, $2, $3)
ON CONFLICT (provider, account_key)
DO UPDATE SET home_region = account_regions.home_region
RETURNING home_region`

	var assigned string
	if err := s.pool.QueryRow(ctx, q, provider, accountKey, region).Scan(&assigned); err != nil {
		return "", fmt.Errorf("assign region: %w", err)
	}
	return assigned, nil
}

// Lookup returns the region owning the account, or ErrNotFound.
func (s *Store) Lookup(ctx context.Context, provider, accountKey string) (string, error) {
	if provider == "" || accountKey == "" {
		return "", ErrInvalidInput
	}

	const q = `SELECT home_region FROM account_regions WHERE provider = $1 AND account_key = $2`

	var region string
	err := s.pool.QueryRow(ctx, q, provider, accountKey).Scan(&region)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", fmt.Errorf("%w: %s/%s", ErrNotFound, provider, accountKey)
	case err != nil:
		return "", fmt.Errorf("lookup region: %w", err)
	}
	return region, nil
}
