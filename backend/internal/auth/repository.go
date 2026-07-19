package auth

import (
	"context"

	"github.com/google/uuid"
)

// GitHubProfile is the slice of a GitHub user we persist on
// sign-in. Pulled from the OAuth-app's user endpoint by the
// callback handler before calling Repository.SignIn.
type GitHubProfile struct {
	ID    int64
	Login string
	Name  string
	Email *string
}

// Repository persists the user + session state. v0 has a single
// Postgres implementation; tests substitute a fake.
type Repository interface {
	// SignIn upserts the user (keyed by GitHub id) and creates a
	// fresh session bound to the account the membership gate
	// resolved (E44.3 / ADR-057 Amendment A2). accountID may be
	// uuid.Nil only where no gate ran (tests); the callback always
	// passes a resolved account. Returns the populated Session
	// including its PlainText (set exactly once at issue time).
	SignIn(ctx context.Context, p GitHubProfile, accountID uuid.UUID) (*User, *Session, error)

	// Authenticate hashes plaintext, looks up an active session,
	// validates sliding + absolute TTLs against the current
	// clock, and returns the resolved (User, Session). Refreshes
	// the sliding-expiry on success — best-effort, doesn't gate
	// authentication.
	Authenticate(ctx context.Context, plaintext string) (*User, *Session, error)

	// Revoke marks a session revoked. Idempotent (a second call
	// is a no-op). The handler decides authorization (typically
	// "the session being revoked must be the caller's own").
	Revoke(ctx context.Context, sessionID uuid.UUID) error

	// GetUser is exposed for the /v0/auth/me handler.
	GetUser(ctx context.Context, id uuid.UUID) (*User, error)

	// EvictExpired drops every session row whose
	// absolute_expires_at < before. Returns the number of rows
	// deleted so the eviction ticker can log a counter.
	EvictExpired(ctx context.Context, before int64) (int64, error)
}
