// Package apitoken issues, authenticates, and revokes the scoped
// bearer tokens CLI / UI clients use to call the backend (per
// MVP_SPEC §5.4 + the OpenAPI's `bearerToken` security scheme).
//
// Tokens are minted by the backend and bound to a user identity
// (subject). The plaintext is shown to the caller exactly once at
// issue time; only its sha256 hash is stored, so a database
// compromise doesn't surrender live credentials.
//
// Format: "fhk_" prefix + 32 random bytes URL-safe base64. The
// prefix makes leaked tokens easy to grep for (in logs, .env
// files, source files). Total length ~47 bytes.
//
// Auditability: callers should pair every Issue / Authenticate /
// Revoke with an audit entry; this package returns enough
// information (the token id, never the plaintext) to construct
// those entries. The api_tokens table itself does NOT log every
// auth — that level of audit volume warrants a separate decision.
package apitoken

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

// TokenPrefix marks every plaintext token so they're easy to grep
// for in customer-side logs and revoke if leaked.
const TokenPrefix = "fhk_"

// tokenSecretBytes is the entropy we ask crypto/rand for. The
// total token length is ~ len(TokenPrefix) + base64(32) chars.
const tokenSecretBytes = 32

// Errors callers may want to switch on.
var (
	// ErrNotFound means no token row matches the lookup; could be
	// "never issued" or "revoked." Callers shouldn't disambiguate
	// in user-visible errors — both are 401 / 404 from a bearer-
	// auth perspective.
	ErrNotFound = errors.New("apitoken: not found")

	// ErrMalformedToken means the bearer string didn't have the
	// expected prefix or shape. Distinct from ErrNotFound so
	// authenticate paths can return 401 without touching the DB.
	ErrMalformedToken = errors.New("apitoken: malformed token string")

	// ErrForbidden means the caller is trying to revoke a token
	// they don't own.
	ErrForbidden = errors.New("apitoken: caller cannot revoke this token")
)

// Token is the public-facing record. PlainText is set ONLY by
// Issue, exactly once. Any later read returns it as the empty
// string — the plaintext is never re-derivable.
type Token struct {
	ID         uuid.UUID
	Subject    string
	Scopes     []string
	LastUsedAt *time.Time
	CreatedAt  time.Time
	RevokedAt  *time.Time

	// AuthMethod records how the token was authenticated at issue
	// time: "static" for operator-minted tokens (fishhawkd token
	// issue, via the column default) or "oauth" for tokens minted
	// through the OAuth device flow. Empty only for a row predating
	// the auth_method column (NULL); callers treat "" as "static".
	AuthMethod string

	// Provider is the identity provider that authenticated an OAuth
	// token (e.g. "github"). Empty for static tokens.
	Provider string

	// AccountID is the tenant workspace account this token acts within
	// (ADR-057 / E44.5, migration 0055's nullable api_tokens.account_id).
	// Empty string when the token is untenanted (NULL account_id — every
	// operator/static token and every legacy row until a later child
	// backfills). Populated by Authenticate (GetTokenByHash selects the
	// column); the bearer-auth path threads it onto Identity.AccountID so
	// the account-ownership middleware can bound a bearer request to its
	// account.
	AccountID string

	// PlainText is the bearer string the caller stores. Set only
	// on Issue's return value; empty for tokens loaded from the
	// repository.
	PlainText string
}

// IsRevoked reports whether the token has been revoked.
func (t Token) IsRevoked() bool {
	return t.RevokedAt != nil
}

// Repository persists tokens. Implementations MUST treat the
// token_hash as the index of record — Authenticate looks up by
// hash and never has the plaintext.
type Repository interface {
	// Issue mints a fresh token for subject with the given scopes
	// and returns the populated Token (including PlainText). The row
	// carries auth_method='static' via the column default.
	Issue(ctx context.Context, subject string, scopes []string) (*Token, error)

	// Authenticate hashes plaintext, looks up the row, and returns
	// the matched Token. ErrMalformedToken on bad format,
	// ErrNotFound if the hash doesn't match an active row. On
	// success, LastUsedAt is updated best-effort.
	Authenticate(ctx context.Context, plaintext string) (*Token, error)

	// ListForSubject returns the active (non-revoked) tokens for
	// subject, newest first.
	ListForSubject(ctx context.Context, subject string) ([]*Token, error)

	// Revoke marks the token revoked. requesterSubject must match
	// the token's subject; otherwise ErrForbidden. Idempotent: a
	// second Revoke on an already-revoked token returns the
	// existing row.
	Revoke(ctx context.Context, id uuid.UUID, requesterSubject string) (*Token, error)

	// GetByID returns the token (including revoked rows) so the
	// HTTP handlers can map ID → 404 / 403 cleanly.
	GetByID(ctx context.Context, id uuid.UUID) (*Token, error)
}

// OAuthIssuer is the additive capability seam for minting tokens
// authenticated through the OAuth device flow (E39.3 / #1708). It is a
// strict superset of Repository, kept SEPARATE from it so the base
// interface stays unchanged and every existing Repository implementer
// (including the in-memory handler-test fakes) keeps compiling without an
// edit — the "additive, no existing caller needs any edit" property the
// design requires. The OAuth mint endpoint type-asserts its Repository to
// OAuthIssuer; the production *postgresRepo satisfies it, and tests that
// exercise the OAuth path use the real repository (which does too).
type OAuthIssuer interface {
	Repository

	// IssueOAuth mints a token stamping auth_method='oauth' and the
	// originating provider (e.g. "github"). It is additive to Issue: the
	// static path is unchanged and keeps stamping 'static' via the column
	// default. subject and provider must be non-empty.
	IssueOAuth(ctx context.Context, subject string, scopes []string, provider string) (*Token, error)
}

// generatePlaintext produces a fresh random bearer string with
// TokenPrefix. Caller does NOT need to validate the prefix; that's
// what HashPlaintext expects.
func generatePlaintext() (string, error) {
	buf := make([]byte, tokenSecretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("apitoken: random bytes: %w", err)
	}
	return TokenPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// HashPlaintext returns the canonical hex-encoded sha256 hash of a
// plaintext token. Exposed so authenticators that need to
// pre-compute (e.g., a Redis cache layer in a later iteration) and
// the repository agree on the encoding. ErrMalformedToken on
// missing prefix.
func HashPlaintext(plaintext string) (string, error) {
	if !strings.HasPrefix(plaintext, TokenPrefix) {
		return "", ErrMalformedToken
	}
	if len(plaintext) <= len(TokenPrefix)+8 {
		// Defensive: a very-short token isn't a Fishhawk-issued
		// one. Keeps the hash space from being polluted by junk.
		return "", ErrMalformedToken
	}
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:]), nil
}
