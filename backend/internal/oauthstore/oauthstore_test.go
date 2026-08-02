package oauthstore_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/oauthstore"
)

// TestGeneratePlaintext_PrefixAndEntropy pins the credential format: each class
// carries its own prefix and 32 bytes (256 bits) of entropy, which is the
// unguessability 0063's isolation argument rests on.
func TestGeneratePlaintext_PrefixAndEntropy(t *testing.T) {
	t.Parallel()
	for _, prefix := range []string{
		oauthstore.CodePrefix,
		oauthstore.AccessTokenPrefix,
		oauthstore.RefreshTokenPrefix,
	} {
		got, err := oauthstore.GeneratePlaintext(prefix)
		if err != nil {
			t.Fatalf("GeneratePlaintext(%q): %v", prefix, err)
		}
		if !strings.HasPrefix(got, prefix) {
			t.Errorf("GeneratePlaintext(%q) = %q, want the prefix", prefix, got)
		}
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(got, prefix))
		if err != nil {
			t.Fatalf("secret is not RawURLEncoding base64: %v", err)
		}
		if len(raw) != 32 {
			t.Errorf("entropy = %d bytes, want 32", len(raw))
		}
	}
}

// TestGeneratePlaintext_DistinctPerCall guards against a constant or a seeded
// generator: two draws of the same class must differ.
func TestGeneratePlaintext_DistinctPerCall(t *testing.T) {
	t.Parallel()
	a, err := oauthstore.GeneratePlaintext(oauthstore.CodePrefix)
	if err != nil {
		t.Fatalf("GeneratePlaintext: %v", err)
	}
	b, err := oauthstore.GeneratePlaintext(oauthstore.CodePrefix)
	if err != nil {
		t.Fatalf("GeneratePlaintext: %v", err)
	}
	if a == b {
		t.Fatalf("two draws produced the same credential %q", a)
	}
}

// TestHashPlaintext_MatchesIndependentSHA256 computes the expected digest in
// the test from crypto/sha256 rather than restating the implementation, so a
// change of hash or encoding is caught rather than mirrored.
func TestHashPlaintext_MatchesIndependentSHA256(t *testing.T) {
	t.Parallel()
	plaintext := oauthstore.AccessTokenPrefix + "abcdefghijklmnopqrstuvwxyz012345"
	got, err := oauthstore.HashPlaintext(oauthstore.AccessTokenPrefix, plaintext)
	if err != nil {
		t.Fatalf("HashPlaintext: %v", err)
	}
	sum := sha256.Sum256([]byte(plaintext))
	if want := hex.EncodeToString(sum[:]); got != want {
		t.Errorf("HashPlaintext = %q, want %q (hex sha256 of the FULL plaintext)", got, want)
	}
}

// TestHashPlaintext_RejectsMalformed covers both malformed branches: a wrong /
// missing prefix and an implausibly short string. Each must return
// ErrMalformedCredential, which is what lets the repository refuse junk without
// a database round-trip.
func TestHashPlaintext_RejectsMalformed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		prefix    string
		plaintext string
	}{
		{"no prefix", oauthstore.CodePrefix, "abcdefghijklmnop"},
		{"wrong class prefix", oauthstore.CodePrefix, oauthstore.AccessTokenPrefix + "abcdefghijklmnop"},
		{"prefix only", oauthstore.CodePrefix, oauthstore.CodePrefix},
		{"too short", oauthstore.CodePrefix, oauthstore.CodePrefix + "12345678"},
		{"empty", oauthstore.CodePrefix, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := oauthstore.HashPlaintext(tc.prefix, tc.plaintext)
			if !errors.Is(err, oauthstore.ErrMalformedCredential) {
				t.Fatalf("HashPlaintext(%q) error = %v, want ErrMalformedCredential", tc.plaintext, err)
			}
			if got != "" {
				t.Errorf("HashPlaintext returned %q alongside the error, want empty", got)
			}
		})
	}
}

// TestHashPlaintext_AcceptsOneByteOverTheFloor pins the boundary of the
// short-string guard: the check is `<= len(prefix)+8`, so nine secret
// characters is the first accepted length. Without this the guard could be
// tightened arbitrarily and reject real credentials unnoticed.
func TestHashPlaintext_AcceptsOneByteOverTheFloor(t *testing.T) {
	t.Parallel()
	plaintext := oauthstore.CodePrefix + "123456789"
	if _, err := oauthstore.HashPlaintext(oauthstore.CodePrefix, plaintext); err != nil {
		t.Fatalf("HashPlaintext(%q) = %v, want accepted (one byte over the floor)", plaintext, err)
	}
}

// TestAuthorizationCode_Predicates pins the expiry boundary as INCLUSIVE (a
// code is expired AT exactly expires_at) and the consumed predicate, which the
// repository's classification order depends on.
func TestAuthorizationCode_Predicates(t *testing.T) {
	t.Parallel()
	exp := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	code := oauthstore.AuthorizationCode{ExpiresAt: exp}

	if code.IsExpired(exp.Add(-time.Nanosecond)) {
		t.Error("IsExpired one nanosecond before expires_at = true, want false")
	}
	if !code.IsExpired(exp) {
		t.Error("IsExpired AT expires_at = false, want true (boundary is inclusive)")
	}
	if !code.IsExpired(exp.Add(time.Nanosecond)) {
		t.Error("IsExpired after expires_at = false, want true")
	}

	if code.IsConsumed() {
		t.Error("IsConsumed on a nil ConsumedAt = true, want false")
	}
	consumed := code
	at := exp.Add(-time.Minute)
	consumed.ConsumedAt = &at
	if !consumed.IsConsumed() {
		t.Error("IsConsumed on a set ConsumedAt = false, want true")
	}
}

// TestTokenPredicates covers the access- and refresh-token predicates on the
// same inclusive boundary, since AuthenticateAccessToken and RotateRefreshToken
// classify through them.
func TestTokenPredicates(t *testing.T) {
	t.Parallel()
	exp := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	at := exp.Add(-time.Minute)

	access := oauthstore.AccessToken{ExpiresAt: exp}
	if access.IsExpired(exp.Add(-time.Nanosecond)) || !access.IsExpired(exp) {
		t.Error("AccessToken.IsExpired boundary is not inclusive at expires_at")
	}
	if access.IsRevoked() {
		t.Error("AccessToken.IsRevoked on a nil RevokedAt = true, want false")
	}
	access.RevokedAt = &at
	if !access.IsRevoked() {
		t.Error("AccessToken.IsRevoked on a set RevokedAt = false, want true")
	}

	refresh := oauthstore.RefreshToken{ExpiresAt: exp}
	if refresh.IsExpired(exp.Add(-time.Nanosecond)) || !refresh.IsExpired(exp) {
		t.Error("RefreshToken.IsExpired boundary is not inclusive at expires_at")
	}
	if refresh.IsRevoked() || refresh.IsConsumed() {
		t.Error("RefreshToken predicates on nil stamps = true, want false")
	}
	refresh.RevokedAt = &at
	refresh.ConsumedAt = &at
	if !refresh.IsRevoked() || !refresh.IsConsumed() {
		t.Error("RefreshToken predicates on set stamps = false, want true")
	}
}
