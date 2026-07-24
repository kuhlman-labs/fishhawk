// Package auth implements the GitHub OAuth sign-in flow and the
// browser session model from MVP_SPEC §5.4 + ADR-005 (#69).
//
// Flow:
//
//  1. GET /v0/auth/github/login mints a state value, stores it in
//     a short-lived browser cookie, and redirects to GitHub's
//     authorize URL.
//  2. GET /v0/auth/github/callback verifies the state, exchanges
//     the code for a GitHub access token, fetches the GitHub
//     user, upserts a row in `users`, and creates a `sessions`
//     row whose hash is stored server-side. The session id (the
//     plaintext) goes into an HttpOnly + Secure cookie.
//  3. Subsequent requests carry the cookie; the auth middleware
//     hashes it, looks up the session, and attaches the resolved
//     user to the request Identity.
//
// Sessions are server-side rows so revocation is immediate
// (per the ADR). Tokens are NOT JWTs — they're opaque IDs the
// backend hashes and looks up.
//
// PKCE is not used in v0: GitHub's web app flow uses a confidential
// client (the backend holds the client_secret), so PKCE adds no
// security beyond what the secret already provides. We still
// enforce a state check to prevent CSRF on the callback.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Cookie names. The session cookie is HttpOnly + Secure +
// SameSite=Lax per ADR-005. The state cookie is short-lived and
// cleared on callback completion. The next cookie carries the
// post-sign-in redirect target across the GitHub round-trip
// (E7.2.1 #153); also short-lived and cleared at callback.
const (
	SessionCookieName = "fishhawk_session"
	StateCookieName   = "fishhawk_oauth_state"
	NextCookieName    = "fishhawk_oauth_next"
)

// SessionTTL bounds (per ADR-005).
const (
	SessionSlidingTTL  = 24 * time.Hour
	SessionAbsoluteTTL = 7 * 24 * time.Hour
	StateCookieTTL     = 10 * time.Minute
)

// Token format prefixes. The session cookie value uses a Fishhawk
// product prefix mirroring the API-token convention so leaked
// cookies are easy to grep for in customer logs.
const (
	SessionTokenPrefix = "fhs_"
	tokenSecretBytes   = 32
)

// Errors callers may want to switch on.
var (
	// ErrInvalidState means the callback's state value didn't
	// match the cookie. CSRF prevention; surface as 400 rather
	// than starting an exchange.
	ErrInvalidState = errors.New("auth: invalid OAuth state")

	// ErrSessionNotFound covers "no row" + "revoked" + "expired."
	// Callers shouldn't disambiguate in user-visible errors —
	// every variant is "you're not signed in."
	ErrSessionNotFound = errors.New("auth: session not found or expired")

	// ErrMalformedToken means the cookie value didn't have the
	// product prefix or the right length.
	ErrMalformedToken = errors.New("auth: malformed session token")
)

// User mirrors the OpenAPI User schema. ID is Fishhawk's UUID;
// GitHubUserID is the forge's stable numeric id (preserved across
// login renames). GitHubLogin is the current handle. Provider is the
// forge the identity came from ("github" | "gitlab", E44.22 / #2109);
// it scopes GitHubUserID so a GitLab numeric id can never collide with
// a GitHub user of the same id.
type User struct {
	ID           string
	Provider     string
	GitHubUserID int64
	GitHubLogin  string
	Name         string
	Email        *string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Session is the server-side record for a browser session.
// PlainText is set ONLY by Issue, exactly once — the cookie value
// the browser carries. Subsequent reads from the repository return
// it as the empty string.
type Session struct {
	ID                string
	UserID            string
	IssuedAt          time.Time
	LastUsedAt        time.Time
	SlidingExpiresAt  time.Time
	AbsoluteExpiresAt time.Time
	RevokedAt         *time.Time

	// AccountID is the workspace account the membership gate resolved
	// at sign-in (E44.3 / ADR-057 Amendment A2). Empty when the
	// session carries no binding — a pre-gate row, or an account
	// deleted after sign-in (the FK is ON DELETE SET NULL); handlers
	// treat that as account_unresolved rather than admitting.
	AccountID string

	PlainText string
}

// IsExpired reports whether the session is past either of its
// TTL bounds at `now`. Callers shouldn't authenticate expired
// sessions even if the row hasn't been evicted yet.
func (s Session) IsExpired(now time.Time) bool {
	if s.RevokedAt != nil {
		return true
	}
	return now.After(s.AbsoluteExpiresAt) || now.After(s.SlidingExpiresAt)
}

// generatePlaintext mints a fresh session cookie value with
// SessionTokenPrefix. Caller stores only HashPlaintext(value).
func generatePlaintext() (string, error) {
	buf := make([]byte, tokenSecretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: random bytes: %w", err)
	}
	return SessionTokenPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// HashPlaintext returns the canonical hex-encoded sha256 hash of a
// session cookie value. Exposed so middleware can pre-compute and
// the repository agrees on the encoding. Returns ErrMalformedToken
// when the prefix or length is wrong.
func HashPlaintext(plaintext string) (string, error) {
	if !strings.HasPrefix(plaintext, SessionTokenPrefix) {
		return "", ErrMalformedToken
	}
	if len(plaintext) <= len(SessionTokenPrefix)+8 {
		return "", ErrMalformedToken
	}
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:]), nil
}

// GenerateState returns a random URL-safe string for the OAuth
// state parameter. Bound to the browser via the state cookie;
// matched at callback to prevent CSRF on /callback.
func GenerateState() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
