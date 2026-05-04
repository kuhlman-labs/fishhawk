// Package githuboidc verifies GitHub Actions OIDC tokens (the
// JWTs the runner can mint via the `id-token: write` permission)
// and binds them to a Fishhawk run.
//
// The signing-key endpoint is the auth gateway for the entire
// runner → backend trace flow. Without OIDC the run_id is the only
// secret a caller needs; with OIDC, a caller must additionally hold
// a valid GitHub-signed token whose claims match the run's repo +
// workflow_id.
//
// Hand-rolled JWT verification (rather than golang-jwt) for the
// same reason as internal/githubapp/signer.go: GitHub OIDC is a
// fixed shape — RS256, JWKS at a stable URL, a small known claim
// set — and the verification path is ~80 lines. An external crypto
// dependency in the trust path doesn't earn its keep at that size.
package githuboidc

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// DefaultIssuer is GitHub Actions' OIDC issuer. The token's `iss`
// claim must match exactly.
const DefaultIssuer = "https://token.actions.githubusercontent.com"

// DefaultJWKSURL is GitHub's published JWKS endpoint. The Verifier
// fetches keys from here unless overridden in tests.
const DefaultJWKSURL = DefaultIssuer + "/.well-known/jwks"

// Errors callers may want to switch on.
var (
	// ErrInvalidToken covers shape errors (not three segments,
	// header doesn't decode, signature doesn't verify, etc.).
	ErrInvalidToken = errors.New("oidc: invalid token")

	// ErrTokenExpired means the JWT's exp claim is in the past
	// (or nbf is in the future).
	ErrTokenExpired = errors.New("oidc: token expired or not yet valid")

	// ErrUnknownKID means the token's `kid` header doesn't match
	// any key currently in JWKS. Often transient (key rollover);
	// caller may want to refresh and retry.
	ErrUnknownKID = errors.New("oidc: unknown key id")

	// ErrClaimMismatch means a required claim is missing or
	// doesn't match the configured Expectations (issuer, audience,
	// repository, workflow, event_name).
	ErrClaimMismatch = errors.New("oidc: claim mismatch")
)

// Claims is the slice of an OIDC token's claims Fishhawk surfaces.
// We deliberately don't decode every GitHub-specific field —
// Repository, Workflow, EventName, and Ref are the ones load-bearing
// for binding to a Run; adding fields is opt-in as new gating logic
// needs them.
type Claims struct {
	Issuer    string `json:"iss"`
	Audience  string `json:"aud"`
	Subject   string `json:"sub"`
	IssuedAt  int64  `json:"iat"`
	NotBefore int64  `json:"nbf"`
	ExpiresAt int64  `json:"exp"`

	Repository string `json:"repository"`
	Workflow   string `json:"workflow"`
	EventName  string `json:"event_name"`
	Ref        string `json:"ref"`
	SHA        string `json:"sha"`
	RunID      string `json:"run_id"`
}

// Expectations names the values a token must match to be accepted.
// Issuer defaults to DefaultIssuer when empty. Audience is required.
// Repository and Workflow bind the token to a specific Fishhawk Run;
// AllowedEvents limits the GitHub Actions trigger types that may
// produce a valid token (typically "issues", "issue_comment",
// "workflow_dispatch").
type Expectations struct {
	Issuer        string
	Audience      string
	Repository    string
	Workflow      string
	AllowedEvents []string

	// Now is the clock for exp/nbf checks. Tests inject a fake;
	// production leaves it nil for time.Now.
	Now func() time.Time
}

// Verifier verifies a single OIDC token against a set of
// Expectations. The HTTP-backed implementation fetches JWKS from
// DefaultJWKSURL and caches it; tests substitute a stub backed by
// a static JWKS.
type Verifier interface {
	Verify(ctx context.Context, rawToken string, exp Expectations) (*Claims, error)
}

// httpVerifier is the production Verifier: it pulls JWKS from the
// configured URL via a cached HTTP fetch, caches the parsed RSA
// public keys, and verifies + validates incoming tokens.
type httpVerifier struct {
	jwks *jwksCache
}

// New returns a Verifier that fetches JWKS from DefaultJWKSURL.
// The HTTP client uses a 30s timeout; JWKS responses are cached
// honoring Cache-Control max-age (defaulting to 1h when absent).
func New() Verifier {
	return &httpVerifier{jwks: newJWKSCache(DefaultJWKSURL)}
}

// NewWithJWKSURL returns a Verifier that fetches JWKS from the
// given URL. Tests use this to point at an httptest.Server.
func NewWithJWKSURL(url string) Verifier {
	return &httpVerifier{jwks: newJWKSCache(url)}
}

// Verify decodes the token, looks up the RSA key for its kid,
// verifies the RS256 signature, and validates the claims against
// exp. Returns (*Claims, nil) on success.
func (v *httpVerifier) Verify(ctx context.Context, rawToken string, exp Expectations) (*Claims, error) {
	header, claimsBytes, signature, signingInput, err := splitJWT(rawToken)
	if err != nil {
		return nil, err
	}
	if header.Alg != "RS256" {
		return nil, fmt.Errorf("%w: alg %q is not RS256", ErrInvalidToken, header.Alg)
	}

	key, err := v.jwks.Get(ctx, header.KID)
	if err != nil {
		return nil, err
	}

	digest := sha256.Sum256(signingInput)
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature); err != nil {
		return nil, fmt.Errorf("%w: signature verification failed: %v", ErrInvalidToken, err)
	}

	var claims Claims
	if err := json.Unmarshal(claimsBytes, &claims); err != nil {
		return nil, fmt.Errorf("%w: claims not JSON: %v", ErrInvalidToken, err)
	}
	if err := validateClaims(&claims, exp); err != nil {
		return nil, err
	}
	return &claims, nil
}

// jwtHeader is the bits of the protected header we read.
type jwtHeader struct {
	Alg string `json:"alg"`
	KID string `json:"kid"`
	Typ string `json:"typ"`
}

// splitJWT decodes the three base64url-encoded parts of a compact
// JWT. signingInput is the bytes the signature was computed over —
// returned separately so the caller doesn't have to recompute the
// concatenation.
func splitJWT(token string) (jwtHeader, []byte, []byte, []byte, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return jwtHeader{}, nil, nil, nil, fmt.Errorf("%w: expected 3 segments, got %d", ErrInvalidToken, len(parts))
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return jwtHeader{}, nil, nil, nil, fmt.Errorf("%w: header decode: %v", ErrInvalidToken, err)
	}
	var hdr jwtHeader
	if err := json.Unmarshal(headerBytes, &hdr); err != nil {
		return jwtHeader{}, nil, nil, nil, fmt.Errorf("%w: header parse: %v", ErrInvalidToken, err)
	}
	claims, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return jwtHeader{}, nil, nil, nil, fmt.Errorf("%w: claims decode: %v", ErrInvalidToken, err)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return jwtHeader{}, nil, nil, nil, fmt.Errorf("%w: signature decode: %v", ErrInvalidToken, err)
	}
	signingInput := []byte(parts[0] + "." + parts[1])
	return hdr, claims, signature, signingInput, nil
}

// validateClaims runs the standard + business claim checks against
// exp. Returns ErrClaimMismatch (wrapped with detail) for missing or
// non-matching claims; ErrTokenExpired for clock-bound failures.
func validateClaims(c *Claims, exp Expectations) error {
	now := time.Now
	if exp.Now != nil {
		now = exp.Now
	}
	currentSec := now().UTC().Unix()

	wantIssuer := exp.Issuer
	if wantIssuer == "" {
		wantIssuer = DefaultIssuer
	}
	if c.Issuer != wantIssuer {
		return fmt.Errorf("%w: iss = %q, want %q", ErrClaimMismatch, c.Issuer, wantIssuer)
	}
	if exp.Audience == "" {
		return fmt.Errorf("%w: Expectations.Audience is required", ErrClaimMismatch)
	}
	if c.Audience != exp.Audience {
		return fmt.Errorf("%w: aud = %q, want %q", ErrClaimMismatch, c.Audience, exp.Audience)
	}

	if c.ExpiresAt == 0 {
		return fmt.Errorf("%w: missing exp claim", ErrClaimMismatch)
	}
	if c.ExpiresAt < currentSec {
		return fmt.Errorf("%w: exp %d < now %d", ErrTokenExpired, c.ExpiresAt, currentSec)
	}
	if c.NotBefore > 0 && c.NotBefore > currentSec {
		return fmt.Errorf("%w: nbf %d > now %d", ErrTokenExpired, c.NotBefore, currentSec)
	}

	if exp.Repository != "" && c.Repository != exp.Repository {
		return fmt.Errorf("%w: repository = %q, want %q", ErrClaimMismatch, c.Repository, exp.Repository)
	}
	if exp.Workflow != "" && c.Workflow != exp.Workflow {
		return fmt.Errorf("%w: workflow = %q, want %q", ErrClaimMismatch, c.Workflow, exp.Workflow)
	}
	if len(exp.AllowedEvents) > 0 && !contains(exp.AllowedEvents, c.EventName) {
		return fmt.Errorf("%w: event_name = %q, allowed %v", ErrClaimMismatch, c.EventName, exp.AllowedEvents)
	}
	return nil
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
