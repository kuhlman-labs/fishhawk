// Package store is the global directory's Postgres persistence: the
// (provider, account_key) → home_region assignment table and the
// single-use install-state nonce table (ADR-062, E44.7 / #1831).
//
// It holds NO customer data and, deliberately, no cell_base_url column —
// region → cell base URL resolves exclusively from the directory's env
// config (see internal/routing).
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/directory/internal/routing"
)

// Store implements routing.Store over Postgres.
type Store struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// New returns a Store backed by pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, now: time.Now}
}

// WithClock overrides the wall clock used for expiry checks (tests).
func (s *Store) WithClock(now func() time.Time) *Store {
	s.now = now
	return s
}

// AssignRegion records provider/accountKey → region FIRST-WRITE-WINS and
// returns the effective assignment.
//
// An account that already has a home region keeps it: the ON CONFLICT
// branch is a no-op update whose RETURNING yields the pre-existing row.
// A second onboarding attempt naming a different region therefore reads
// back the original region rather than moving the account — the same
// idempotence the cell enforces on its side.
func (s *Store) AssignRegion(ctx context.Context, provider, accountKey, region string) (routing.Assignment, error) {
	provider, accountKey, region = norm(provider), strings.TrimSpace(accountKey), norm(region)
	if provider == "" || accountKey == "" || region == "" {
		return routing.Assignment{}, fmt.Errorf("store: provider, account_key and region are required")
	}
	const q = `
INSERT INTO account_regions (provider, account_key, home_region)
VALUES ($1, $2, $3)
ON CONFLICT (provider, account_key)
    DO UPDATE SET updated_at = account_regions.updated_at
RETURNING provider, account_key, home_region`
	var a routing.Assignment
	if err := s.pool.QueryRow(ctx, q, provider, accountKey, region).
		Scan(&a.Provider, &a.AccountKey, &a.HomeRegion); err != nil {
		return routing.Assignment{}, fmt.Errorf("store: assign region: %w", err)
	}
	return a, nil
}

// LookupRegion returns the recorded assignment for an account, or an
// error wrapping routing.ErrNotFound.
func (s *Store) LookupRegion(ctx context.Context, provider, accountKey string) (routing.Assignment, error) {
	const q = `
SELECT provider, account_key, home_region
FROM account_regions
WHERE provider = $1 AND account_key = $2`
	var a routing.Assignment
	err := s.pool.QueryRow(ctx, q, norm(provider), strings.TrimSpace(accountKey)).
		Scan(&a.Provider, &a.AccountKey, &a.HomeRegion)
	if errors.Is(err, pgx.ErrNoRows) {
		return routing.Assignment{}, fmt.Errorf("%w: account %s/%s", routing.ErrNotFound, provider, accountKey)
	}
	if err != nil {
		return routing.Assignment{}, fmt.Errorf("store: look up region: %w", err)
	}
	return a, nil
}

// PutInstallState records a freshly minted single-use nonce.
func (s *Store) PutInstallState(ctx context.Context, st routing.InstallState) error {
	if st.Nonce == "" {
		return fmt.Errorf("store: install state nonce is required")
	}
	const q = `
INSERT INTO install_states (nonce, provider, account_key, home_region, expires_at)
VALUES ($1, $2, $3, $4, $5)`
	if _, err := s.pool.Exec(ctx, q, st.Nonce, norm(st.Provider), strings.TrimSpace(st.AccountKey), norm(st.HomeRegion), st.ExpiresAt); err != nil {
		return fmt.Errorf("store: put install state: %w", err)
	}
	return nil
}

// ConsumeInstallState atomically deletes and returns a nonce.
//
// It errors with routing.ErrNotFound when the nonce is unknown OR was
// already consumed (the DELETE ... RETURNING makes consumption
// single-use even under concurrency), and with routing.ErrExpired when
// the row was past its lifetime — expired rows are consumed too, so a
// stale nonce cannot be retried.
func (s *Store) ConsumeInstallState(ctx context.Context, nonce string) (routing.InstallState, error) {
	nonce = strings.TrimSpace(nonce)
	if nonce == "" {
		return routing.InstallState{}, fmt.Errorf("%w: empty nonce", routing.ErrNotFound)
	}
	const q = `
DELETE FROM install_states
WHERE nonce = $1
RETURNING nonce, provider, account_key, home_region, expires_at`
	var st routing.InstallState
	err := s.pool.QueryRow(ctx, q, nonce).
		Scan(&st.Nonce, &st.Provider, &st.AccountKey, &st.HomeRegion, &st.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return routing.InstallState{}, fmt.Errorf("%w: install state %q", routing.ErrNotFound, nonce)
	}
	if err != nil {
		return routing.InstallState{}, fmt.Errorf("store: consume install state: %w", err)
	}
	if s.now().After(st.ExpiresAt) {
		return routing.InstallState{}, fmt.Errorf("%w: install state expired at %s", routing.ErrExpired, st.ExpiresAt.Format(time.RFC3339))
	}
	return st, nil
}

// PruneExpiredInstallStates deletes abandoned onboarding nonces and
// returns the number removed.
func (s *Store) PruneExpiredInstallStates(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM install_states WHERE expires_at < $1`, s.now())
	if err != nil {
		return 0, fmt.Errorf("store: prune install states: %w", err)
	}
	return tag.RowsAffected(), nil
}

func norm(v string) string { return strings.ToLower(strings.TrimSpace(v)) }
