package signing

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	signingdb "github.com/kuhlman-labs/fishhawk/backend/internal/signing/db"
)

// nowFunc is overridable for tests that want to drive the clock
// (e.g. force expiry without sleeping). Production code uses
// time.Now via the realClock default.
type postgresRepo struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// NewPostgresRepository wraps a pgxpool.Pool to satisfy Repository.
// Caller retains ownership of the pool.
func NewPostgresRepository(pool *pgxpool.Pool) Repository {
	return &postgresRepo{pool: pool, now: time.Now}
}

// NewPostgresRepositoryWithClock is the test-only constructor that
// lets a test inject a deterministic clock. Production code should
// use NewPostgresRepository.
func NewPostgresRepositoryWithClock(pool *pgxpool.Pool, now func() time.Time) Repository {
	if now == nil {
		now = time.Now
	}
	return &postgresRepo{pool: pool, now: now}
}

// Issue mints a new signing key for the run. Multi-call:
// each invocation appends a new row (per migration 0012), so each
// stage's GitHub Actions runner can issue its own private key
// without coordinating with prior stages. Verify accepts a
// signature from ANY unexpired key for the run (newest-first), so a
// key rotation by a sibling stage's runner does not invalidate an
// in-flight runner's still-open artifact upload; older rows also
// remain in the table so the standalone verifier can replay any
// signature that was valid at upload time.
func (r *postgresRepo) Issue(ctx context.Context, runID uuid.UUID, ttl time.Duration) (*IssuedKey, error) {
	if ttl <= 0 {
		return nil, fmt.Errorf("signing: ttl must be positive")
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("signing: generate ed25519 key: %w", err)
	}

	issuedAt := r.now().UTC()
	expiresAt := issuedAt.Add(ttl)

	q := signingdb.New(r.pool)
	row, err := q.IssueSigningKey(ctx, signingdb.IssueSigningKeyParams{
		RunID:     runID,
		PublicKey: pub,
		IssuedAt:  pgtype.Timestamptz{Time: issuedAt, Valid: true},
		ExpiresAt: pgtype.Timestamptz{Time: expiresAt, Valid: true},
	})
	if err != nil {
		return nil, fmt.Errorf("signing: issue: %w", err)
	}
	return &IssuedKey{
		RunID:      row.RunID,
		PublicKey:  ed25519.PublicKey(row.PublicKey),
		PrivateKey: priv,
		IssuedAt:   row.IssuedAt.Time,
		ExpiresAt:  row.ExpiresAt.Time,
	}, nil
}

// Get returns the latest signing key issued for the run. Used by
// Verify; external callers (e.g. the standalone verifier) that
// need the full history should use a different query — Get is
// intentionally narrow because every uploader path wants the
// "current" key.
func (r *postgresRepo) Get(ctx context.Context, runID uuid.UUID) (*Key, error) {
	q := signingdb.New(r.pool)
	row, err := q.GetLatestSigningKey(ctx, runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("signing: get: %w", err)
	}
	return &Key{
		RunID:     row.RunID,
		PublicKey: ed25519.PublicKey(row.PublicKey),
		IssuedAt:  row.IssuedAt.Time,
		ExpiresAt: row.ExpiresAt.Time,
	}, nil
}

// Verify accepts the signature if ANY unexpired key issued for the
// run verifies it, trying newest-first. A run accrues one key per
// runner start (plus the #1182 pre-terminal re-issue), so a sibling
// stage rotating in a fresh key must not invalidate an in-flight
// runner's still-open artifact upload signed under an earlier key.
//
// Error taxonomy (preserved for the upload handlers' HTTP mapping):
//   - no keys for the run          -> ErrNotFound
//   - keys exist but all expired   -> ErrExpired
//   - unexpired keys, none verify  -> ErrSignatureInvalid
func (r *postgresRepo) Verify(ctx context.Context, runID uuid.UUID, message, signature []byte) error {
	q := signingdb.New(r.pool)
	rows, err := q.ListSigningKeysForRun(ctx, runID)
	if err != nil {
		return fmt.Errorf("signing: verify: %w", err)
	}
	if len(rows) == 0 {
		return ErrNotFound
	}

	now := r.now().UTC()
	sawUnexpired := false
	// ListSigningKeysForRun orders issued_at ASC; walk newest-first so
	// the current key (the common case) is tried before older ones.
	for i := len(rows) - 1; i >= 0; i-- {
		if !now.Before(rows[i].ExpiresAt.Time) {
			continue
		}
		sawUnexpired = true
		if VerifyWith(ed25519.PublicKey(rows[i].PublicKey), message, signature) == nil {
			return nil
		}
	}
	if !sawUnexpired {
		return ErrExpired
	}
	return ErrSignatureInvalid
}

// Compile-time check that postgresRepo implements Repository.
var _ Repository = (*postgresRepo)(nil)
