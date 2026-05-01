package signing

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
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
		// Primary key conflict surfaces as a Postgres unique-
		// violation. Map to ErrAlreadyIssued for callers.
		if isUniqueViolation(err) {
			return nil, ErrAlreadyIssued
		}
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

func (r *postgresRepo) Get(ctx context.Context, runID uuid.UUID) (*Key, error) {
	q := signingdb.New(r.pool)
	row, err := q.GetSigningKey(ctx, runID)
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

func (r *postgresRepo) Verify(ctx context.Context, runID uuid.UUID, message, signature []byte) error {
	key, err := r.Get(ctx, runID)
	if err != nil {
		return err
	}
	if !r.now().UTC().Before(key.ExpiresAt) {
		return ErrExpired
	}
	return VerifyWith(key.PublicKey, message, signature)
}

// isUniqueViolation reports whether err is a Postgres
// "unique_violation" (SQLSTATE 23505), which fires when Issue is
// called twice for the same run_id (the table's PRIMARY KEY
// constraint).
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	// pgx wraps SQLSTATE codes in *pgconn.PgError. Avoiding the
	// import keeps the dependency surface tight; string-match is
	// fine for the small set of codes we care about.
	return strings.Contains(err.Error(), "SQLSTATE 23505")
}

// Compile-time check that postgresRepo implements Repository.
var _ Repository = (*postgresRepo)(nil)
