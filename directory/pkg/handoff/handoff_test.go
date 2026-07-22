package handoff_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/directory/pkg/handoff"
)

const secret = "s3cr3t-shared-by-both-planes"

func fixture(now time.Time) handoff.Params {
	return handoff.Params{
		Provider:   "github",
		AccountKey: "kuhlman-labs",
		HomeRegion: "eu",
		ExpiresAt:  now.Add(2 * time.Minute),
		Nonce:      "n-0001",
	}
}

func TestSignVerifyRoundTrip(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	want := fixture(now)

	v, err := handoff.Sign(want, secret)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if v.Get(handoff.ParamSignature) == "" {
		t.Fatal("Sign produced no signature")
	}

	got, err := handoff.Verify(v, secret, now)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Provider != want.Provider || got.AccountKey != want.AccountKey ||
		got.HomeRegion != want.HomeRegion || got.Nonce != want.Nonce {
		t.Fatalf("round trip mismatch: got %+v want %+v", got, want)
	}
	if !got.ExpiresAt.Equal(want.ExpiresAt.Truncate(time.Second)) {
		t.Fatalf("expires_at: got %s want %s", got.ExpiresAt, want.ExpiresAt)
	}
}

// The signature must survive a trip through a real URL — that is the
// serialization boundary the cell actually reads the pin from.
func TestVerifySurvivesURLSerialization(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	// An account key with characters that MUST be escaped, so a naive
	// canonical string would be forgeable / mis-parsed.
	p := fixture(now)
	p.AccountKey = "acct&fh_home_region=us"

	v, err := handoff.Sign(p, secret)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	u, err := url.Parse("https://eu.example.test/login?code=abc&state=xyz&" + v.Encode())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, err := handoff.Verify(u.Query(), secret, now)
	if err != nil {
		t.Fatalf("Verify after URL round trip: %v", err)
	}
	if got.AccountKey != p.AccountKey {
		t.Fatalf("account_key: got %q want %q", got.AccountKey, p.AccountKey)
	}
	if got.HomeRegion != "eu" {
		t.Fatalf("home_region smuggled: got %q want eu", got.HomeRegion)
	}
}

func TestSignRejectsEmptySecret(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	if _, err := handoff.Sign(fixture(now), ""); !errors.Is(err, handoff.ErrNoSecret) {
		t.Fatalf("got %v want ErrNoSecret", err)
	}
}

func TestVerifyRejectsEmptySecret(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	v, err := handoff.Sign(fixture(now), secret)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if _, err := handoff.Verify(v, "", now); !errors.Is(err, handoff.ErrNoSecret) {
		t.Fatalf("got %v want ErrNoSecret", err)
	}
}

func TestSignRejectsIncompleteParams(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	base := fixture(now)

	for name, mutate := range map[string]func(*handoff.Params){
		"provider":    func(p *handoff.Params) { p.Provider = "" },
		"account_key": func(p *handoff.Params) { p.AccountKey = "" },
		"home_region": func(p *handoff.Params) { p.HomeRegion = "" },
		"nonce":       func(p *handoff.Params) { p.Nonce = "" },
		"expires_at":  func(p *handoff.Params) { p.ExpiresAt = time.Time{} },
	} {
		t.Run(name, func(t *testing.T) {
			p := base
			mutate(&p)
			if _, err := handoff.Sign(p, secret); !errors.Is(err, handoff.ErrMissing) {
				t.Fatalf("got %v want ErrMissing", err)
			}
		})
	}
}

// Unsigned: a bare request with no handoff parameters at all.
func TestVerifyRejectsUnsigned(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	q := url.Values{"code": {"abc"}, "state": {"xyz"}}
	if _, err := handoff.Verify(q, secret, now); !errors.Is(err, handoff.ErrMissing) {
		t.Fatalf("got %v want ErrMissing", err)
	}
}

// A payload present but with the signature stripped.
func TestVerifyRejectsPayloadWithoutSignature(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	v, err := handoff.Sign(fixture(now), secret)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	v.Del(handoff.ParamSignature)
	if _, err := handoff.Verify(v, secret, now); !errors.Is(err, handoff.ErrMissing) {
		t.Fatalf("got %v want ErrMissing", err)
	}
}

func TestVerifyRejectsMissingPayloadField(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	for _, field := range []string{
		handoff.ParamProvider, handoff.ParamAccountKey,
		handoff.ParamHomeRegion, handoff.ParamExpiresAt, handoff.ParamNonce,
	} {
		t.Run(field, func(t *testing.T) {
			v, err := handoff.Sign(fixture(now), secret)
			if err != nil {
				t.Fatalf("Sign: %v", err)
			}
			v.Del(field)
			if _, err := handoff.Verify(v, secret, now); !errors.Is(err, handoff.ErrMissing) {
				t.Fatalf("got %v want ErrMissing", err)
			}
		})
	}
}

// Forged: a valid-shaped pin signed with a DIFFERENT secret.
func TestVerifyRejectsForgedSignature(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	v, err := handoff.Sign(fixture(now), "attacker-secret")
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if _, err := handoff.Verify(v, secret, now); !errors.Is(err, handoff.ErrBadSignature) {
		t.Fatalf("got %v want ErrBadSignature", err)
	}
}

// Tampered: a legitimately signed pin whose payload was edited in flight
// (the residency-relevant case: flipping the region).
func TestVerifyRejectsTamperedValue(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	for _, field := range []string{
		handoff.ParamProvider, handoff.ParamAccountKey,
		handoff.ParamHomeRegion, handoff.ParamExpiresAt, handoff.ParamNonce,
	} {
		t.Run(field, func(t *testing.T) {
			v, err := handoff.Sign(fixture(now), secret)
			if err != nil {
				t.Fatalf("Sign: %v", err)
			}
			v.Set(field, "9999999999")
			if _, err := handoff.Verify(v, secret, now); !errors.Is(err, handoff.ErrBadSignature) {
				t.Fatalf("got %v want ErrBadSignature", err)
			}
		})
	}
}

func TestVerifyRejectsExpired(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	v, err := handoff.Sign(fixture(now), secret)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	// One second past the 2-minute TTL.
	late := now.Add(2*time.Minute + time.Second)
	if _, err := handoff.Verify(v, secret, late); !errors.Is(err, handoff.ErrExpired) {
		t.Fatalf("got %v want ErrExpired", err)
	}
}

func TestVerifyAcceptsExactlyAtExpiry(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	v, err := handoff.Sign(fixture(now), secret)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if _, err := handoff.Verify(v, secret, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("boundary should still verify: %v", err)
	}
}

// A non-numeric expires_at re-signed with the real secret: the signature
// passes, so the malformed-parse branch is the one that must reject.
func TestVerifyRejectsMalformedExpiry(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	v := url.Values{
		handoff.ParamProvider:   {"github"},
		handoff.ParamAccountKey: {"kuhlman-labs"},
		handoff.ParamHomeRegion: {"eu"},
		handoff.ParamExpiresAt:  {"not-a-number"},
		handoff.ParamNonce:      {"n-0001"},
	}
	// Re-sign the malformed payload so we isolate the parse branch from
	// the signature branch.
	v.Set(handoff.ParamSignature, resign(t, v))
	if _, err := handoff.Verify(v, secret, now); !errors.Is(err, handoff.ErrMalformed) {
		t.Fatalf("got %v want ErrMalformed", err)
	}
}

// resign recomputes a signature over v with an INDEPENDENT re-implementation
// of the canonical encoding, so the test does not reach into unexported
// helpers and also pins the canonical form itself.
func resign(t *testing.T, v url.Values) string {
	t.Helper()
	canonical := url.Values{}
	for k, vals := range v {
		if k == handoff.ParamSignature {
			continue
		}
		canonical.Set(k, vals[0])
	}
	mac := hmac.New(sha256.New, []byte(secret))
	if _, err := mac.Write([]byte(canonical.Encode())); err != nil {
		t.Fatalf("hmac write: %v", err)
	}
	return hex.EncodeToString(mac.Sum(nil))
}
