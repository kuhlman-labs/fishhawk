package server

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/auth"
)

// Pure unit tests for the consent csrf_token codec (#2442). Every refusal is
// asserted by error IDENTITY with errors.Is.

const unitSessionPlaintext = "fhs_0123456789abcdef0123456789abcdef"

func mustMint(t *testing.T, plaintext string, now time.Time) string {
	t.Helper()
	tok, err := mintConsentCSRFToken(plaintext, now)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	return tok
}

func decodeTok(t *testing.T, tok string) []byte {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return raw
}

func TestConsentCSRF_RoundTrip(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	tok := mustMint(t, unitSessionPlaintext, now)
	if err := verifyConsentCSRFToken(unitSessionPlaintext, tok, now); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if err := verifyConsentCSRFToken(unitSessionPlaintext, tok, now.Add(30*time.Minute)); err != nil {
		t.Fatalf("verify within TTL: %v", err)
	}
}

func TestConsentCSRF_MintsDiffer(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	a := mustMint(t, unitSessionPlaintext, now)
	b := mustMint(t, unitSessionPlaintext, now)
	if a == b {
		t.Fatal("two mints at the same instant produced the same token (nonce missing)")
	}
}

func TestConsentCSRF_EmptyPlaintextRefusedAtMint(t *testing.T) {
	if _, err := mintConsentCSRFToken("", time.Now()); err == nil {
		t.Fatal("mint with an empty session plaintext succeeded; must fail closed")
	}
}

func TestConsentCSRF_Malformed(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	good := mustMint(t, unitSessionPlaintext, now)
	raw := decodeTok(t, good)
	cases := map[string]string{
		"empty":      "",
		"not-base64": "!!!not base64url!!!",
		"short":      base64.RawURLEncoding.EncodeToString(raw[:len(raw)-1]),
		"long":       base64.RawURLEncoding.EncodeToString(append(append([]byte{}, raw...), 0)),
		"padded":     good + "=",
	}
	for name, tok := range cases {
		t.Run(name, func(t *testing.T) {
			err := verifyConsentCSRFToken(unitSessionPlaintext, tok, now)
			if !errors.Is(err, ErrConsentCSRFMalformed) {
				t.Fatalf("err = %v, want ErrConsentCSRFMalformed", err)
			}
		})
	}
}

func TestConsentCSRF_ExpiryBoundary(t *testing.T) {
	mintAt := time.Unix(1_800_000_000, 0)
	tok := mustMint(t, unitSessionPlaintext, mintAt)
	if err := verifyConsentCSRFToken(unitSessionPlaintext, tok, mintAt.Add(consentCSRFTokenTTL)); err != nil {
		t.Fatalf("at exactly ts+TTL: err = %v, want nil", err)
	}
	err := verifyConsentCSRFToken(unitSessionPlaintext, tok, mintAt.Add(consentCSRFTokenTTL+time.Second))
	if !errors.Is(err, ErrConsentCSRFExpired) {
		t.Fatalf("at ts+TTL+1s: err = %v, want ErrConsentCSRFExpired", err)
	}
}

func TestConsentCSRF_Invalid(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	good := mustMint(t, unitSessionPlaintext, now)
	flip := func(idx int) string {
		raw := decodeTok(t, good)
		raw[idx] ^= 0x01
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	cases := map[string]struct {
		plaintext string
		tok       string
	}{
		"mac-flip":   {unitSessionPlaintext, flip(len(decodeTok(t, good)) - 1)},
		"ts-flip":    {unitSessionPlaintext, flip(consentCSRFTsLen - 1)},
		"nonce-flip": {unitSessionPlaintext, flip(consentCSRFTsLen)},
		"other-sess": {"fhs_fedcba9876543210fedcba9876543210", good},
		"empty-sess": {"", good},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			err := verifyConsentCSRFToken(c.plaintext, c.tok, now)
			if !errors.Is(err, ErrConsentCSRFInvalid) {
				t.Fatalf("err = %v, want ErrConsentCSRFInvalid", err)
			}
		})
	}
}

// TestConsentCSRFKey_IsNotThePersistedSessionHash pins the domain separation:
// the per-session HMAC key must differ from auth.HashPlaintext's persisted
// SHA-256, so a sessions-table read never yields the CSRF key.
func TestConsentCSRFKey_IsNotThePersistedSessionHash(t *testing.T) {
	persisted, err := auth.HashPlaintext(unitSessionPlaintext)
	if err != nil {
		t.Fatalf("HashPlaintext: %v", err)
	}
	key := hex.EncodeToString(consentCSRFKey(unitSessionPlaintext))
	if strings.EqualFold(key, persisted) {
		t.Fatalf("consent CSRF key equals the persisted session hash %s", persisted)
	}
}
