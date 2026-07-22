package handoff_test

import (
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/directory/pkg/handoff"
)

var testSecret = []byte("shared-handoff-secret")

func validParams(exp time.Time) handoff.Params {
	return handoff.Params{
		Provider:   "github",
		AccountKey: "kuhlman-labs",
		HomeRegion: "eu",
		ExpiresAt:  exp,
		Nonce:      "nonce-1",
	}
}

func TestValuesVerifyRoundTrip(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	want := validParams(now.Add(2 * time.Minute))

	v, err := handoff.Values(testSecret, want)
	if err != nil {
		t.Fatalf("Values: %v", err)
	}
	got, err := handoff.Verify(testSecret, v, now)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Provider != want.Provider || got.AccountKey != want.AccountKey || got.HomeRegion != want.HomeRegion || got.Nonce != want.Nonce {
		t.Fatalf("round trip mismatch: got %+v want %+v", got, want)
	}
	if !got.ExpiresAt.Equal(want.ExpiresAt) {
		t.Fatalf("expires_at: got %s want %s", got.ExpiresAt, want.ExpiresAt)
	}
}

// AppendTo is the routing contract of binding condition (1): the ORIGINAL
// path and every original query parameter survive the redirect, and the
// handoff is purely additive.
func TestAppendToPreservesPathAndQuery(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	orig := url.Values{
		"code":            {"abc123"},
		"state":           {"opaque/state+value"},
		"installation_id": {"4242"},
		"setup_action":    {"install"},
	}

	loc, err := handoff.AppendTo("https://eu.fishhawk.example.com", "/v0/auth/github/callback", orig, testSecret, validParams(now.Add(time.Minute)))
	if err != nil {
		t.Fatalf("AppendTo: %v", err)
	}
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("parse location: %v", err)
	}
	if u.Host != "eu.fishhawk.example.com" {
		t.Fatalf("host: got %q", u.Host)
	}
	if u.Path != "/v0/auth/github/callback" {
		t.Fatalf("path: got %q want /v0/auth/github/callback", u.Path)
	}
	q := u.Query()
	for k, vs := range orig {
		if q.Get(k) != vs[0] {
			t.Errorf("original param %q: got %q want %q", k, q.Get(k), vs[0])
		}
	}
	if _, err := handoff.Verify(testSecret, q, now); err != nil {
		t.Fatalf("appended handoff does not verify: %v", err)
	}
}

// A cell mounted under a path prefix keeps that prefix; the original path is
// joined, not substituted.
func TestAppendToJoinsBasePathPrefix(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	loc, err := handoff.AppendTo("https://cell.example.com/fishhawk/", "/v0/auth/github/callback", url.Values{"code": {"x"}}, testSecret, validParams(now.Add(time.Minute)))
	if err != nil {
		t.Fatalf("AppendTo: %v", err)
	}
	if !strings.Contains(loc, "/fishhawk/v0/auth/github/callback") {
		t.Fatalf("prefix not joined: %q", loc)
	}
}

// A caller-supplied fh_* parameter must not survive alongside the
// directory's own signed value.
func TestAppendToOverridesCallerSuppliedPin(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	orig := url.Values{handoff.ParamHomeRegion: {"us"}, handoff.ParamSignature: {"deadbeef"}}
	loc, err := handoff.AppendTo("https://eu.example.com", "/cb", orig, testSecret, validParams(now.Add(time.Minute)))
	if err != nil {
		t.Fatalf("AppendTo: %v", err)
	}
	u, _ := url.Parse(loc)
	q := u.Query()
	if len(q[handoff.ParamHomeRegion]) != 1 || q.Get(handoff.ParamHomeRegion) != "eu" {
		t.Fatalf("caller-supplied home_region survived: %v", q[handoff.ParamHomeRegion])
	}
	if _, err := handoff.Verify(testSecret, q, now); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestAppendToRejectsNonAbsoluteBase(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	if _, err := handoff.AppendTo("/relative", "/cb", nil, testSecret, validParams(now.Add(time.Minute))); err == nil {
		t.Fatal("expected an error for a non-absolute cell base url")
	}
}

// Every fail-closed branch of Verify, one assertion each.
func TestVerifyRejections(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	signed, err := handoff.Values(testSecret, validParams(now.Add(time.Minute)))
	if err != nil {
		t.Fatalf("Values: %v", err)
	}

	clone := func(mutate func(url.Values)) url.Values {
		out := url.Values{}
		for k, vs := range signed {
			out[k] = append([]string(nil), vs...)
		}
		mutate(out)
		return out
	}

	tests := []struct {
		name string
		q    url.Values
		now  time.Time
		want error
	}{
		{"absent", url.Values{}, now, handoff.ErrMissing},
		{"unsigned", clone(func(v url.Values) { v.Del(handoff.ParamSignature) }), now, handoff.ErrBadSignature},
		{"forged_signature", clone(func(v url.Values) { v.Set(handoff.ParamSignature, strings.Repeat("ab", 32)) }), now, handoff.ErrBadSignature},
		{"non_hex_signature", clone(func(v url.Values) { v.Set(handoff.ParamSignature, "zzzz") }), now, handoff.ErrBadSignature},
		{"tampered_region", clone(func(v url.Values) { v.Set(handoff.ParamHomeRegion, "us") }), now, handoff.ErrBadSignature},
		{"tampered_account", clone(func(v url.Values) { v.Set(handoff.ParamAccountKey, "someone-else") }), now, handoff.ErrBadSignature},
		{"tampered_expiry", clone(func(v url.Values) { v.Set(handoff.ParamExpiresAt, "9999999999") }), now, handoff.ErrBadSignature},
		{"expired", signed, now.Add(2 * time.Minute), handoff.ErrExpired},
		{"expired_exactly_at_boundary", signed, now.Add(time.Minute), handoff.ErrExpired},
		{"unparseable_expiry", clone(func(v url.Values) { v.Set(handoff.ParamExpiresAt, "soon") }), now, handoff.ErrMalformed},
		{"blank_provider", clone(func(v url.Values) { v.Set(handoff.ParamProvider, "") }), now, handoff.ErrMalformed},
		{"blank_nonce", clone(func(v url.Values) { v.Set(handoff.ParamNonce, "") }), now, handoff.ErrMalformed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := handoff.Verify(testSecret, tc.q, tc.now); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

// A pin signed with a DIFFERENT secret must not verify — the cross-deployment
// forgery case.
func TestVerifyRejectsWrongSecret(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	signed, err := handoff.Values([]byte("attacker-secret"), validParams(now.Add(time.Minute)))
	if err != nil {
		t.Fatalf("Values: %v", err)
	}
	if _, err := handoff.Verify(testSecret, signed, now); !errors.Is(err, handoff.ErrBadSignature) {
		t.Fatalf("got %v, want ErrBadSignature", err)
	}
}

func TestNoSecretFailsClosedOnBothSides(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	if _, err := handoff.Values(nil, validParams(now.Add(time.Minute))); !errors.Is(err, handoff.ErrNoSecret) {
		t.Fatalf("Values with no secret: got %v, want ErrNoSecret", err)
	}
	if _, err := handoff.Verify(nil, url.Values{handoff.ParamSignature: {"abcd"}}, now); !errors.Is(err, handoff.ErrNoSecret) {
		t.Fatalf("Verify with no secret: got %v, want ErrNoSecret", err)
	}
}

func TestValuesRejectsIncompleteParams(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	p := validParams(now.Add(time.Minute))
	p.HomeRegion = "  "
	if _, err := handoff.Values(testSecret, p); !errors.Is(err, handoff.ErrMalformed) {
		t.Fatalf("got %v, want ErrMalformed", err)
	}
	p = validParams(time.Time{})
	if _, err := handoff.Values(testSecret, p); !errors.Is(err, handoff.ErrMalformed) {
		t.Fatalf("zero expiry: got %v, want ErrMalformed", err)
	}
}

// The canonical string is delimiter-safe: two DIFFERENT parameter sets whose
// naive concatenation would collide must produce different signatures.
func TestCanonicalStringIsUnambiguous(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	a := validParams(now.Add(time.Minute))
	a.Provider, a.AccountKey = "git", "hubacme"
	b := validParams(now.Add(time.Minute))
	b.Provider, b.AccountKey = "github", "acme"

	va, err := handoff.Values(testSecret, a)
	if err != nil {
		t.Fatalf("Values(a): %v", err)
	}
	vb, err := handoff.Values(testSecret, b)
	if err != nil {
		t.Fatalf("Values(b): %v", err)
	}
	if va.Get(handoff.ParamSignature) == vb.Get(handoff.ParamSignature) {
		t.Fatal("signature collision between distinct parameter sets")
	}
}
