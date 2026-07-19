package apitoken

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	tokendb "github.com/kuhlman-labs/fishhawk/backend/internal/apitoken/db"
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

func (r *postgresRepo) Issue(ctx context.Context, subject string, scopes []string) (*Token, error) {
	if subject == "" {
		return nil, errors.New("apitoken: subject required")
	}
	plaintext, err := generatePlaintext()
	if err != nil {
		return nil, err
	}
	hash, err := HashPlaintext(plaintext)
	if err != nil {
		return nil, err
	}

	if scopes == nil {
		scopes = []string{}
	}
	q := tokendb.New(r.pool)
	row, err := q.CreateToken(ctx, tokendb.CreateTokenParams{
		ID:        uuid.New(),
		Subject:   subject,
		TokenHash: hash,
		Scopes:    scopes,
	})
	if err != nil {
		return nil, fmt.Errorf("apitoken: create: %w", err)
	}
	tok := rowToToken(row)
	tok.PlainText = plaintext
	return tok, nil
}

func (r *postgresRepo) IssueOAuth(ctx context.Context, subject string, scopes []string, provider string) (*Token, error) {
	if subject == "" {
		return nil, errors.New("apitoken: subject required")
	}
	if provider == "" {
		return nil, errors.New("apitoken: provider required")
	}
	plaintext, err := generatePlaintext()
	if err != nil {
		return nil, err
	}
	hash, err := HashPlaintext(plaintext)
	if err != nil {
		return nil, err
	}

	if scopes == nil {
		scopes = []string{}
	}
	q := tokendb.New(r.pool)
	row, err := q.CreateOAuthToken(ctx, tokendb.CreateOAuthTokenParams{
		ID:        uuid.New(),
		Subject:   subject,
		TokenHash: hash,
		Scopes:    scopes,
		Provider:  pgtype.Text{String: provider, Valid: true},
	})
	if err != nil {
		return nil, fmt.Errorf("apitoken: create oauth: %w", err)
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
	row, err := q.GetTokenByHash(ctx, hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("apitoken: lookup: %w", err)
	}
	// Best-effort touch of last_used_at; a failure here doesn't
	// invalidate the auth decision.
	_ = q.TouchTokenLastUsed(ctx, tokendb.TouchTokenLastUsedParams{
		ID:         row.ID,
		LastUsedAt: pgtype.Timestamptz{Time: r.now(), Valid: true},
	})
	return rowToToken(row), nil
}

func (r *postgresRepo) ListForSubject(ctx context.Context, subject string) ([]*Token, error) {
	q := tokendb.New(r.pool)
	rows, err := q.ListTokensForSubject(ctx, subject)
	if err != nil {
		return nil, fmt.Errorf("apitoken: list: %w", err)
	}
	out := make([]*Token, 0, len(rows))
	for _, row := range rows {
		out = append(out, rowToToken(row))
	}
	return out, nil
}

func (r *postgresRepo) Revoke(ctx context.Context, id uuid.UUID, requesterSubject string) (*Token, error) {
	q := tokendb.New(r.pool)
	existing, err := q.GetTokenByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("apitoken: get: %w", err)
	}
	if existing.Subject != requesterSubject {
		return nil, ErrForbidden
	}
	updated, err := q.RevokeToken(ctx, tokendb.RevokeTokenParams{
		ID:        id,
		RevokedAt: pgtype.Timestamptz{Time: r.now(), Valid: true},
	})
	if err != nil {
		return nil, fmt.Errorf("apitoken: revoke: %w", err)
	}
	return rowToToken(updated), nil
}

func (r *postgresRepo) GetByID(ctx context.Context, id uuid.UUID) (*Token, error) {
	q := tokendb.New(r.pool)
	row, err := q.GetTokenByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("apitoken: get: %w", err)
	}
	return rowToToken(row), nil
}

func rowToToken(r tokendb.ApiToken) *Token {
	out := &Token{
		ID:        r.ID,
		Subject:   r.Subject,
		Scopes:    r.Scopes,
		CreatedAt: r.CreatedAt.Time,
	}
	if r.AuthMethod.Valid {
		out.AuthMethod = r.AuthMethod.String
	}
	if r.Provider.Valid {
		out.Provider = r.Provider.String
	}
	// NULL account_id → "" (untenanted, ADR-057 / E44.5). Only GetTokenByHash
	// selects the column; the other ApiToken-returning queries leave it nil,
	// which maps to "" here — those paths don't consume the account.
	if r.AccountID != nil {
		out.AccountID = r.AccountID.String()
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

// Compile-time checks: the production repo satisfies both the base
// Repository and the additive OAuthIssuer capability seam.
var (
	_ Repository  = (*postgresRepo)(nil)
	_ OAuthIssuer = (*postgresRepo)(nil)
)
