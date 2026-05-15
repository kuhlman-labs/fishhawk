// Package mcptoken issues, authenticates, and revokes the short-
// lived per-run bearer tokens that runner-side Claude Code agents
// use to call the Fishhawk MCP server (E19.8 / #348).
//
// Separate from apitoken because the surfaces have different
// semantics:
//
//   - apitoken: operator-issued, long-lived, bound to a GitHub
//     identity. The user manages issuance + revocation via the
//     API-token surface (E13 / future).
//   - mcptoken: backend-issued at stage start, short-lived
//     (TTL is the load-bearing protection per ADR-021 / #322),
//     bound to a specific run_id. The runner provisions one per
//     stage and the in-runner agent uses it to call the MCP
//     server's read-only tool surface.
//
// Format: "fhm_" prefix + 32 random bytes URL-safe base64.
// Mirrors apitoken's "fhk_" shape so leaked tokens are easy to
// grep for, but the distinct prefix lets the bearer-auth
// middleware route to the right authenticator without colliding
// with the operator-facing tokens.
//
// Auditability: callers should pair every Issue with an audit
// entry (category `mcp_token_issued` per ADR-021 sub-design).
// Per-auth audit is intentionally NOT done here — the volume
// would dwarf the per-run-issuance signal and the load-bearing
// security boundary is the TTL, not the auth log.
package mcptoken

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// TokenPrefix marks every plaintext MCP token so they're easy to
// grep for in customer-side logs. Distinct from apitoken.TokenPrefix
// ("fhk_") so the middleware can route MCP tokens to the right
// authenticator without a database round-trip.
const TokenPrefix = "fhm_"

// tokenSecretBytes is the entropy we ask crypto/rand for. The
// total token length is ~ len(TokenPrefix) + base64(32) chars.
const tokenSecretBytes = 32

// DefaultTTL is the per-token TTL when the caller doesn't specify
// one. 60 minutes aligns with the signing-key flow per ADR-021's
// "similar to the existing signing-key pattern" framing. Long
// enough to cover a typical stage (most claude-code invocations
// finish in single digits of minutes); short enough that a leaked
// token has bounded blast radius.
const DefaultTTL = 60 * time.Minute

// Errors callers may want to switch on. Mirror apitoken's shape so
// the bearer-auth middleware can treat both packages uniformly.
var (
	// ErrNotFound means no token row matches the lookup; could be
	// "never issued", "revoked", or "expired." Callers don't
	// disambiguate in user-visible errors — all three are 401
	// from a bearer-auth perspective.
	ErrNotFound = errors.New("mcptoken: not found")

	// ErrMalformedToken means the bearer string didn't have the
	// expected prefix or shape. Distinct from ErrNotFound so
	// authenticate paths can return 401 without touching the DB.
	ErrMalformedToken = errors.New("mcptoken: malformed token string")

	// ErrExpired distinguishes an expired-but-otherwise-valid
	// row from a revoked one. The middleware returns 401 either
	// way; logs can surface the distinction.
	ErrExpired = errors.New("mcptoken: token has expired")
)

// Token is the public-facing record. PlainText is set ONLY by
// Issue, exactly once.
type Token struct {
	ID         uuid.UUID
	RunID      uuid.UUID
	IssuedAt   time.Time
	ExpiresAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time

	// PlainText is the bearer string the caller hands to the
	// agent. Set only on Issue's return value; empty for tokens
	// loaded from the repository.
	PlainText string
}

// IsRevoked reports whether the token has been revoked.
func (t Token) IsRevoked() bool {
	return t.RevokedAt != nil
}

// IsExpired reports whether the token's TTL has lapsed relative
// to now.
func (t Token) IsExpired(now time.Time) bool {
	return !t.ExpiresAt.IsZero() && now.After(t.ExpiresAt)
}

// IssueParams bundles the inputs to Issue. Kept as a struct so
// future fields (e.g. additional scopes when per-endpoint
// enforcement lands) don't churn callers.
type IssueParams struct {
	RunID uuid.UUID
	// TTL is the lifetime from now until expires_at. Zero defaults
	// to DefaultTTL.
	TTL time.Duration
}

// Repository persists tokens.
type Repository interface {
	// Issue mints a fresh token for runID with the given TTL and
	// returns the populated Token (including PlainText).
	Issue(ctx context.Context, p IssueParams) (*Token, error)

	// Authenticate hashes plaintext, looks up the row, and returns
	// the matched Token. ErrMalformedToken on bad format,
	// ErrNotFound if the hash doesn't match an active row, and
	// ErrExpired when the row is otherwise valid but past its
	// expires_at. last_used_at is updated best-effort on success.
	Authenticate(ctx context.Context, plaintext string) (*Token, error)

	// Revoke marks the token revoked. Idempotent — a second call
	// on an already-revoked token returns the existing row.
	Revoke(ctx context.Context, id uuid.UUID) (*Token, error)

	// RevokeForRun marks every active token for a run revoked.
	// Called when a run cancels (best-effort; TTL is the load-
	// bearing protection if the call fails). Returns the count
	// of newly-revoked rows.
	RevokeForRun(ctx context.Context, runID uuid.UUID) (int, error)

	// GetByID returns the token (including revoked + expired rows)
	// so handlers can map ID → 404 cleanly.
	GetByID(ctx context.Context, id uuid.UUID) (*Token, error)
}

// generatePlaintext produces a fresh random bearer string with
// TokenPrefix.
func generatePlaintext() (string, error) {
	buf := make([]byte, tokenSecretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("mcptoken: random bytes: %w", err)
	}
	return TokenPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// HashPlaintext returns the canonical hex-encoded sha256 hash of a
// plaintext token. Exposed so the bearer-auth middleware can
// pre-screen format issues before a DB round-trip. ErrMalformedToken
// on missing prefix or implausibly short bodies.
func HashPlaintext(plaintext string) (string, error) {
	if !strings.HasPrefix(plaintext, TokenPrefix) {
		return "", ErrMalformedToken
	}
	if len(plaintext) <= len(TokenPrefix)+8 {
		return "", ErrMalformedToken
	}
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:]), nil
}

// HasPrefix is a cheap check the bearer-auth middleware uses to
// decide whether to dispatch to the mcptoken authenticator vs the
// apitoken one. No allocations; no DB.
func HasPrefix(plaintext string) bool {
	return strings.HasPrefix(plaintext, TokenPrefix)
}
