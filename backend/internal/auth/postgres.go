package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	authdb "github.com/kuhlman-labs/fishhawk/backend/internal/auth/db"
)

type postgresRepo struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// NewPostgresRepository wraps a pgxpool.Pool to satisfy Repository.
// Caller retains ownership of the pool.
func NewPostgresRepository(pool *pgxpool.Pool) Repository {
	return &postgresRepo{
		pool: pool,
		now:  func() time.Time { return time.Now().UTC() },
	}
}

func (r *postgresRepo) SignIn(ctx context.Context, provider string, p GitHubProfile, accountID uuid.UUID) (*User, *Session, error) {
	if p.ID == 0 || p.Login == "" {
		return nil, nil, errors.New("auth: forge profile id + login required")
	}
	if provider == "" {
		provider = "github"
	}
	q := authdb.New(r.pool)

	userRow, err := q.UpsertUser(ctx, authdb.UpsertUserParams{
		ID:           uuid.New(),
		Provider:     provider,
		GithubUserID: p.ID,
		GithubLogin:  p.Login,
		Name:         p.Name,
		Email:        p.Email,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("auth: upsert user: %w", err)
	}

	plaintext, err := generatePlaintext()
	if err != nil {
		return nil, nil, err
	}
	hash, err := HashPlaintext(plaintext)
	if err != nil {
		return nil, nil, err
	}

	// uuid.Nil maps to a NULL account_id — a session with no gate
	// binding, which /v0/auth/me refuses with account_unresolved.
	var boundAccount *uuid.UUID
	if accountID != uuid.Nil {
		boundAccount = &accountID
	}

	now := r.now()
	sessionRow, err := q.CreateSession(ctx, authdb.CreateSessionParams{
		ID:                uuid.New(),
		UserID:            userRow.ID,
		TokenHash:         hash,
		SlidingExpiresAt:  pgtype.Timestamptz{Time: now.Add(SessionSlidingTTL), Valid: true},
		AbsoluteExpiresAt: pgtype.Timestamptz{Time: now.Add(SessionAbsoluteTTL), Valid: true},
		AccountID:         boundAccount,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("auth: create session: %w", err)
	}

	user := rowToUser(userRow)
	sess := rowToSession(sessionRow)
	sess.PlainText = plaintext
	return user, sess, nil
}

func (r *postgresRepo) Authenticate(ctx context.Context, plaintext string) (*User, *Session, error) {
	hash, err := HashPlaintext(plaintext)
	if err != nil {
		return nil, nil, err
	}
	q := authdb.New(r.pool)

	sessRow, err := q.GetSessionByHash(ctx, hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, ErrSessionNotFound
	}
	if err != nil {
		return nil, nil, fmt.Errorf("auth: lookup session: %w", err)
	}

	now := r.now()
	sess := rowToSession(sessRow)
	if sess.IsExpired(now) {
		return nil, nil, ErrSessionNotFound
	}

	userRow, err := q.GetUser(ctx, sessRow.UserID)
	if err != nil {
		return nil, nil, fmt.Errorf("auth: get user: %w", err)
	}

	// Slide the sliding-expiry forward. Best-effort — a transient
	// failure here doesn't invalidate the auth decision.
	_ = q.TouchSession(ctx, authdb.TouchSessionParams{
		ID:               sessRow.ID,
		LastUsedAt:       pgtype.Timestamptz{Time: now, Valid: true},
		SlidingExpiresAt: pgtype.Timestamptz{Time: now.Add(SessionSlidingTTL), Valid: true},
	})

	return rowToUser(userRow), sess, nil
}

func (r *postgresRepo) Revoke(ctx context.Context, sessionID uuid.UUID) error {
	q := authdb.New(r.pool)
	return q.RevokeSessionByID(ctx, authdb.RevokeSessionByIDParams{
		ID:        sessionID,
		RevokedAt: pgtype.Timestamptz{Time: r.now(), Valid: true},
	})
}

func (r *postgresRepo) GetUser(ctx context.Context, id uuid.UUID) (*User, error) {
	q := authdb.New(r.pool)
	row, err := q.GetUser(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSessionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("auth: get user: %w", err)
	}
	return rowToUser(row), nil
}

func (r *postgresRepo) EvictExpired(ctx context.Context, beforeUnix int64) (int64, error) {
	q := authdb.New(r.pool)
	n, err := q.EvictExpiredSessions(ctx, pgtype.Timestamptz{
		Time:  time.Unix(beforeUnix, 0).UTC(),
		Valid: true,
	})
	if err != nil {
		return 0, fmt.Errorf("auth: evict sessions: %w", err)
	}
	return n, nil
}

func rowToUser(r authdb.User) *User {
	return &User{
		ID:           r.ID.String(),
		Provider:     r.Provider,
		GitHubUserID: r.GithubUserID,
		GitHubLogin:  r.GithubLogin,
		Name:         r.Name,
		Email:        r.Email,
		CreatedAt:    r.CreatedAt.Time,
		UpdatedAt:    r.UpdatedAt.Time,
	}
}

func rowToSession(r authdb.Session) *Session {
	out := &Session{
		ID:                r.ID.String(),
		UserID:            r.UserID.String(),
		IssuedAt:          r.IssuedAt.Time,
		LastUsedAt:        r.LastUsedAt.Time,
		SlidingExpiresAt:  r.SlidingExpiresAt.Time,
		AbsoluteExpiresAt: r.AbsoluteExpiresAt.Time,
	}
	if r.RevokedAt.Valid {
		t := r.RevokedAt.Time
		out.RevokedAt = &t
	}
	if r.AccountID != nil {
		out.AccountID = r.AccountID.String()
	}
	return out
}

// Compile-time check.
var _ Repository = (*postgresRepo)(nil)
