package mcptoken

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	tokendb "github.com/kuhlman-labs/fishhawk/backend/internal/mcptoken/db"
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

func (r *postgresRepo) Issue(ctx context.Context, p IssueParams) (*Token, error) {
	if p.RunID == uuid.Nil {
		return nil, errors.New("mcptoken: run_id required")
	}
	ttl := p.TTL
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	plaintext, err := generatePlaintext()
	if err != nil {
		return nil, err
	}
	hash, err := HashPlaintext(plaintext)
	if err != nil {
		return nil, err
	}

	expiresAt := r.now().Add(ttl)
	q := tokendb.New(r.pool)
	row, err := q.CreateMCPToken(ctx, tokendb.CreateMCPTokenParams{
		ID:        uuid.New(),
		RunID:     p.RunID,
		TokenHash: hash,
		ExpiresAt: pgtype.Timestamptz{Time: expiresAt, Valid: true},
	})
	if err != nil {
		return nil, fmt.Errorf("mcptoken: create: %w", err)
	}
	tok := rowToToken(row)
	tok.PlainText = plaintext
	return tok, nil
}

func (r *postgresRepo) Authenticate(ctx context.Context, plaintext string) (*Token, error) {
	hash, err := HashPlaintext(plaintext)
	if err != nil {
		return nil, err
	}
	q := tokendb.New(r.pool)
	row, err := q.GetMCPTokenByHash(ctx, hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("mcptoken: lookup: %w", err)
	}
	tok := rowToToken(row)
	// TTL check is row-level. Returning ErrExpired (vs ErrNotFound)
	// lets the middleware log the cause without leaking the
	// distinction to clients (both surface as 401).
	if tok.IsExpired(r.now()) {
		return nil, ErrExpired
	}
	// Best-effort touch of last_used_at; a failure here doesn't
	// invalidate the auth decision.
	_ = q.TouchMCPTokenLastUsed(ctx, tokendb.TouchMCPTokenLastUsedParams{
		ID:         row.ID,
		LastUsedAt: pgtype.Timestamptz{Time: r.now(), Valid: true},
	})
	return tok, nil
}

func (r *postgresRepo) Revoke(ctx context.Context, id uuid.UUID) (*Token, error) {
	q := tokendb.New(r.pool)
	existing, err := q.GetMCPTokenByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("mcptoken: get: %w", err)
	}
	updated, err := q.RevokeMCPToken(ctx, tokendb.RevokeMCPTokenParams{
		ID:        id,
		RevokedAt: pgtype.Timestamptz{Time: r.now(), Valid: true},
	})
	if err != nil {
		return nil, fmt.Errorf("mcptoken: revoke: %w", err)
	}
	_ = existing
	return rowToToken(updated), nil
}

func (r *postgresRepo) RevokeForRun(ctx context.Context, runID uuid.UUID) (int, error) {
	q := tokendb.New(r.pool)
	n, err := q.RevokeMCPTokensForRun(ctx, tokendb.RevokeMCPTokensForRunParams{
		RunID:     runID,
		RevokedAt: pgtype.Timestamptz{Time: r.now(), Valid: true},
	})
	if err != nil {
		return 0, fmt.Errorf("mcptoken: revoke for run: %w", err)
	}
	return int(n), nil
}

func (r *postgresRepo) GetByID(ctx context.Context, id uuid.UUID) (*Token, error) {
	q := tokendb.New(r.pool)
	row, err := q.GetMCPTokenByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("mcptoken: get: %w", err)
	}
	return rowToToken(row), nil
}

func rowToToken(r tokendb.McpToken) *Token {
	out := &Token{
		ID:    r.ID,
		RunID: r.RunID,
	}
	if r.IssuedAt.Valid {
		out.IssuedAt = r.IssuedAt.Time
	}
	if r.ExpiresAt.Valid {
		out.ExpiresAt = r.ExpiresAt.Time
	}
	if r.LastUsedAt.Valid {
		t := r.LastUsedAt.Time
		out.LastUsedAt = &t
	}
	if r.RevokedAt.Valid {
		t := r.RevokedAt.Time
		out.RevokedAt = &t
	}
	return out
}

// Compile-time check.
var _ Repository = (*postgresRepo)(nil)
