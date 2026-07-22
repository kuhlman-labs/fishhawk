package handoff

import (
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"
)

const testSecret = "s3cr3t-handoff-key"

func testParams() Params {
	return Params{
		Provider:   "github",
		AccountKey: "acme-corp",
		HomeRegion: "us-east",
		ExpiresAt:  time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC),
		Nonce:      "0123456789abcdef",
	}
}

func signOK(t *testing.T, p Params) url.Values {
	t.Helper()
	v, err := Sign(testSecret, p)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	return v
}

func TestSignVerifyRoundTrip(t *testing.T) {
	p := testParams()
	v := signOK(t, p)

	got, err := Verify(testSecret, v, p.ExpiresAt.Add(-time.Minute))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Provider != p.Provider || got.AccountKey != p.AccountKey ||
		got.HomeRegion != p.HomeRegion || got.Nonce != p.Nonce {
		t.Fatalf("round trip mismatch: got %+v want %+v", got, p)
	}
	if !got.ExpiresAt.Equal(p.ExpiresAt) {
		t.Fatalf("expires_at: got %v want %v", got.ExpiresAt, p.ExpiresAt)
	}
}

// Sign must not disturb a caller's own query parameters — the router merges
// the fh_* set into the original query.
func TestSignOnlyEmitsPrefixedParams(t *testing.T) {
	v := signOK(t, testParams())
	for name := range v {
		if !strings.HasPrefix(name, "fh_") {
			t.Fatalf("Sign emitted non-prefixed parameter %q", name)
		}
	}
	if len(v) != 6 {
		t.Fatalf("expected 6 parameters, got %d (%v)", len(v), v)
	}
}

func TestVerifyTamperedSignature(t *testing.T) {
	v := signOK(t, testParams())
	v.Set(ParamSignature, strings.Repeat("ab", 32))

	_, err := Verify(testSecret, v, testParams().ExpiresAt.Add(-time.Minute))
	if !errors.Is(err, ErrBadSignature) {
		t.Fatalf("expected ErrBadSignature, got %v", err)
	}
}

// Every authenticated field must be covered by the MAC: tampering with any
// one of them must fail verification, not just the signature itself.
func TestVerifyTamperedFieldsRejected(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{ParamProvider, "gitlab"},
		{ParamAccountKey, "someone-else"},
		{ParamRegion, "eu-west"},
		{ParamNonce, "ffffffffffffffff"},
		{ParamExpiresAt, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := signOK(t, testParams())
			v.Set(tc.name, tc.value)
			_, err := Verify(testSecret, v, testParams().ExpiresAt.Add(-time.Minute))
			if !errors.Is(err, ErrBadSignature) {
				t.Fatalf("tampering with %s: expected ErrBadSignature, got %v", tc.name, err)
			}
		})
	}
}

func TestVerifyWrongSecret(t *testing.T) {
	v := signOK(t, testParams())
	_, err := Verify("a-different-secret", v, testParams().ExpiresAt.Add(-time.Minute))
	if !errors.Is(err, ErrBadSignature) {
		t.Fatalf("expected ErrBadSignature, got %v", err)
	}
}

func TestVerifyExpired(t *testing.T) {
	p := testParams()
	v := signOK(t, p)

	// Exactly at expires_at counts as expired (the boundary is exclusive).
	if _, err := Verify(testSecret, v, p.ExpiresAt); !errors.Is(err, ErrExpired) {
		t.Fatalf("at expiry: expected ErrExpired, got %v", err)
	}
	if _, err := Verify(testSecret, v, p.ExpiresAt.Add(time.Second)); !errors.Is(err, ErrExpired) {
		t.Fatalf("past expiry: expected ErrExpired, got %v", err)
	}
}

func TestVerifyMissingParams(t *testing.T) {
	for _, name := range []string{
		ParamSignature, ParamProvider, ParamAccountKey,
		ParamRegion, ParamExpiresAt, ParamNonce,
	} {
		t.Run(name, func(t *testing.T) {
			v := signOK(t, testParams())
			v.Del(name)
			_, err := Verify(testSecret, v, testParams().ExpiresAt.Add(-time.Minute))
			if !errors.Is(err, ErrMissingParam) {
				t.Fatalf("missing %s: expected ErrMissingParam, got %v", name, err)
			}
		})
	}
}

func TestVerifyEmptyParamIsMalformed(t *testing.T) {
	v := signOK(t, testParams())
	v.Set(ParamAccountKey, "")
	_, err := Verify(testSecret, v, testParams().ExpiresAt.Add(-time.Minute))
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("expected ErrMalformed, got %v", err)
	}
}

func TestVerifyMalformedExpiry(t *testing.T) {
	v := signOK(t, testParams())
	v.Set(ParamExpiresAt, "tomorrow-ish")
	_, err := Verify(testSecret, v, testParams().ExpiresAt.Add(-time.Minute))
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("expected ErrMalformed, got %v", err)
	}
}

func TestVerifyNonHexSignature(t *testing.T) {
	v := signOK(t, testParams())
	v.Set(ParamSignature, "not-hex-at-all")
	_, err := Verify(testSecret, v, testParams().ExpiresAt.Add(-time.Minute))
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("expected ErrMalformed, got %v", err)
	}
}

func TestEmptySecretIsAConfigurationError(t *testing.T) {
	if _, err := Sign("", testParams()); !errors.Is(err, ErrNoSecret) {
		t.Fatalf("Sign with empty secret: expected ErrNoSecret, got %v", err)
	}
	v := signOK(t, testParams())
	if _, err := Verify("", v, testParams().ExpiresAt.Add(-time.Minute)); !errors.Is(err, ErrNoSecret) {
		t.Fatalf("Verify with empty secret: expected ErrNoSecret, got %v", err)
	}
}

func TestSignRejectsEmptyFields(t *testing.T) {
	base := testParams()
	for _, tc := range []struct {
		name  string
		mutET func(*Params)
	}{
		{"provider", func(p *Params) { p.Provider = "" }},
		{"account_key", func(p *Params) { p.AccountKey = "" }},
		{"region", func(p *Params) { p.HomeRegion = "" }},
		{"nonce", func(p *Params) { p.Nonce = "" }},
		{"expires_at", func(p *Params) { p.ExpiresAt = time.Time{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := base
			tc.mutET(&p)
			if _, err := Sign(testSecret, p); !errors.Is(err, ErrMalformed) {
				t.Fatalf("expected ErrMalformed, got %v", err)
			}
		})
	}
}

// The canonical serialization must be injective. These two parameter sets
// differ only in where the boundary between account_key and region falls;
// a naive separator-joined concatenation ("a|b|c") would produce identical
// MACs and let a handoff for one account authenticate another.
func TestCanonicalSerializationIsUnambiguous(t *testing.T) {
	exp := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	a := Params{Provider: "github", AccountKey: "acme|us-east", HomeRegion: "x", ExpiresAt: exp, Nonce: "n"}
	b := Params{Provider: "github", AccountKey: "acme", HomeRegion: "us-east|x", ExpiresAt: exp, Nonce: "n"}

	va := signOK(t, a)
	vb := signOK(t, b)
	if va.Get(ParamSignature) == vb.Get(ParamSignature) {
		t.Fatal("distinct field vectors produced the same MAC: serialization is ambiguous")
	}

	// And the cross-substitution must not verify.
	swapped := url.Values{}
	for k, vs := range va {
		swapped[k] = append([]string(nil), vs...)
	}
	swapped.Set(ParamAccountKey, b.AccountKey)
	swapped.Set(ParamRegion, b.HomeRegion)
	if _, err := Verify(testSecret, swapped, exp.Add(-time.Minute)); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("expected ErrBadSignature on cross-substitution, got %v", err)
	}
}

func TestHas(t *testing.T) {
	if Has(url.Values{}) {
		t.Fatal("empty query reported as carrying a handoff")
	}
	if Has(url.Values{"state": {"abc"}}) {
		t.Fatal("caller-only query reported as carrying a handoff")
	}
	if !Has(signOK(t, testParams())) {
		t.Fatal("signed query not reported as carrying a handoff")
	}
}

func TestNewNonce(t *testing.T) {
	a, err := NewNonce()
	if err != nil {
		t.Fatalf("NewNonce: %v", err)
	}
	if len(a) != NonceBytes*2 {
		t.Fatalf("nonce length: got %d want %d", len(a), NonceBytes*2)
	}
	b, err := NewNonce()
	if err != nil {
		t.Fatalf("NewNonce: %v", err)
	}
	if a == b {
		t.Fatal("two nonces collided")
	}
}
