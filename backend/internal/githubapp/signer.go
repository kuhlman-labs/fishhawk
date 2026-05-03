// Package githubapp provides the GitHub App authentication path
// the backend uses to act on behalf of customer installations:
//
//  1. Signer mints a short-lived RS256 JWT signed by the App's
//     private key. Used to authenticate as the App itself.
//  2. Client exchanges that JWT for a per-installation token via
//     POST /app/installations/{id}/access_tokens.
//  3. CachedProvider memoizes installation tokens, refreshing
//     before TTL expiry, with hit/miss telemetry.
//
// Production calls TokenProvider.Token(ctx, installationID) and
// gets a string ready to set as Authorization: Bearer; everything
// else (signing, exchange, caching) is invisible to callers.
//
// Why hand-rolled JWT and not golang-jwt/jwt: GitHub's App JWT is
// a fixed shape — RS256, three claims, ten-minute max TTL. The
// signing flow is ~50 lines; an external crypto dependency in
// the trust path doesn't earn its keep at that size.
package githubapp

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// MaxJWTTTL caps the JWT lifetime per GitHub's documented bound.
// Production uses 9 minutes to leave a small clock-skew margin.
const (
	MaxJWTTTL     = 10 * time.Minute
	DefaultJWTTTL = 9 * time.Minute
)

// Signer mints App JWTs from an RSA private key. Construct via
// NewSignerFromPEM; the resulting Signer is safe for concurrent
// use — *rsa.PrivateKey signing is goroutine-safe in Go's stdlib.
type Signer struct {
	appID int64
	key   *rsa.PrivateKey
	now   func() time.Time
}

// NewSignerFromPEM parses a PEM-encoded RSA private key and binds
// it to the given App ID. The key must be PKCS#1 ("RSA PRIVATE
// KEY") or PKCS#8 ("PRIVATE KEY"); other formats fail explicitly.
func NewSignerFromPEM(appID int64, pemBytes []byte) (*Signer, error) {
	if appID <= 0 {
		return nil, errors.New("githubapp: appID must be positive")
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("githubapp: PEM decode failed")
	}

	var key *rsa.PrivateKey
	switch block.Type {
	case "RSA PRIVATE KEY":
		k, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("githubapp: parse PKCS#1: %w", err)
		}
		key = k
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("githubapp: parse PKCS#8: %w", err)
		}
		rsaKey, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("githubapp: PKCS#8 key is not RSA (got %T)", k)
		}
		key = rsaKey
	default:
		return nil, fmt.Errorf("githubapp: unexpected PEM type %q", block.Type)
	}

	return &Signer{
		appID: appID,
		key:   key,
		now:   func() time.Time { return time.Now().UTC() },
	}, nil
}

// AppID returns the configured GitHub App identifier.
func (s *Signer) AppID() int64 { return s.appID }

// Sign produces a fresh App JWT with the given TTL. ttl is clamped
// to (0, MaxJWTTTL]; passing 0 uses DefaultJWTTTL. The `iat` claim
// is backdated 60 seconds to absorb clock drift between this host
// and GitHub's verifier.
func (s *Signer) Sign(ttl time.Duration) (string, error) {
	if ttl <= 0 {
		ttl = DefaultJWTTTL
	}
	if ttl > MaxJWTTTL {
		ttl = MaxJWTTTL
	}

	now := s.now()
	header := `{"alg":"RS256","typ":"JWT"}`
	claims := fmt.Sprintf(`{"iat":%d,"exp":%d,"iss":%d}`,
		now.Add(-60*time.Second).Unix(),
		now.Add(ttl).Unix(),
		s.appID,
	)

	headerEnc := base64URLEncode([]byte(header))
	claimsEnc := base64URLEncode([]byte(claims))
	signingInput := headerEnc + "." + claimsEnc

	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("githubapp: rsa sign: %w", err)
	}
	return signingInput + "." + base64URLEncode(signature), nil
}

// base64URLEncode is base64.RawURLEncoding (no padding). JWTs use
// this by spec; the stdlib encoder produces it directly.
func base64URLEncode(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

// formatInt64 wraps strconv to keep call sites readable when we
// build query strings or log lines. Tiny but pulls one less import.
func formatInt64(v int64) string { return strconv.FormatInt(v, 10) }
