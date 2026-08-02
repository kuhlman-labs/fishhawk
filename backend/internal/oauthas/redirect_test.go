package oauthas

import (
	"errors"
	"net/url"
	"testing"
)

// parseNoLexical parses without the lexical '#' guard, used only to demonstrate
// that net/url cannot express a bare trailing '#'.
func parseNoLexical(raw string) (*url.URL, error) { return url.Parse(raw) }

func TestMatchRedirectURI_LoopbackIgnoresPort(t *testing.T) {
	t.Parallel()
	// The Claude Code case that MUST match: registered portless, requested with a
	// dynamic loopback port (RFC 8252 §7.3).
	cases := [][2]string{
		{"http://127.0.0.1/callback", "http://127.0.0.1:3118/callback"},
		{"http://localhost/callback", "http://localhost:51000/callback"},
		{"http://[::1]/callback", "http://[::1]:51000/callback"},
		{"http://127.0.0.1:3000/callback", "http://127.0.0.1/callback"}, // registered-with-port
	}
	for _, c := range cases {
		if err := MatchRedirectURI(c[0], c[1]); err != nil {
			t.Errorf("MatchRedirectURI(%q, %q) = %v, want match", c[0], c[1], err)
		}
	}
}

func TestMatchRedirectURI_LoopbackNegatives(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		registered string
		requested  string
	}{
		{"differing path", "http://127.0.0.1/callback", "http://127.0.0.1:9/other"},
		{"differing scheme", "http://127.0.0.1/callback", "https://127.0.0.1/callback"},
		{"differing loopback host", "http://localhost/callback", "http://127.0.0.1/callback"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertRedirectRefused(t, MatchRedirectURI(tc.registered, tc.requested))
		})
	}
}

func TestMatchRedirectURI_NonLoopbackExactMatches(t *testing.T) {
	t.Parallel()
	if err := MatchRedirectURI("https://app.example/callback", "https://app.example/callback"); err != nil {
		t.Fatalf("exact non-loopback match rejected: %v", err)
	}
}

func TestMatchRedirectURI_NonLoopbackPortDifferenceRefused(t *testing.T) {
	t.Parallel()
	// The exact inverse of the loopback port relaxation: the redirect-URI-bypass
	// direction. A non-loopback port difference must NOT match.
	assertRedirectRefused(t, MatchRedirectURI("https://app.example/callback", "https://app.example:8443/callback"))
}

func TestMatchRedirectURI_NonLoopbackTrailingSlashRefused(t *testing.T) {
	t.Parallel()
	assertRedirectRefused(t, MatchRedirectURI("https://app.example/callback", "https://app.example/callback/"))
}

func TestMatchRedirectURI_NonLoopbackHostCaseRefused(t *testing.T) {
	t.Parallel()
	// Byte-exact comparison, no normalization: host case difference does not match.
	assertRedirectRefused(t, MatchRedirectURI("https://App.Example/callback", "https://app.example/callback"))
}

func TestMatchRedirectURI_LoopbackRegisteredNonLoopbackRequestedRefused(t *testing.T) {
	t.Parallel()
	assertRedirectRefused(t, MatchRedirectURI("http://127.0.0.1/callback", "https://app.example/callback"))
}

func TestMatchRedirectURI_NonLoopbackRegisteredLoopbackRequestedRefused(t *testing.T) {
	t.Parallel()
	assertRedirectRefused(t, MatchRedirectURI("https://app.example/callback", "http://127.0.0.1/callback"))
}

// Component-rejection matrix: userinfo, query, fragment, each on the registered
// and requested side, on the loopback and non-loopback branch.
func TestMatchRedirectURI_ComponentRejectionMatrix(t *testing.T) {
	t.Parallel()
	// good pairs for each branch, into which we inject a bad component on one side.
	loopGoodReg := "http://127.0.0.1/callback"
	loopGoodReq := "http://127.0.0.1:5000/callback"
	nonGood := "https://app.example/callback"

	type variant struct {
		name       string
		registered string
		requested  string
	}
	_ = nonGood
	variants := []variant{
		// Loopback branch: the malformed URI is paired with a CLEAN URI whose
		// scheme/host/path already match (only the port differs, which the
		// loopback branch ignores). So byte-exact equality does NOT save the
		// case — deleting the component guard would let the pair MATCH. These
		// falsify the guards on the registered and the requested side.
		// userinfo
		{"userinfo registered loopback", "http://u@127.0.0.1/callback", loopGoodReq},
		{"userinfo requested loopback", loopGoodReg, "http://u@127.0.0.1:5000/callback"},
		// query
		{"query registered loopback", "http://127.0.0.1/callback?a=1", loopGoodReq},
		{"query requested loopback", loopGoodReg, "http://127.0.0.1:5000/callback?a=1"},
		// fragment (non-empty)
		{"fragment registered loopback", "http://127.0.0.1/callback#f", loopGoodReq},
		{"fragment requested loopback", loopGoodReg, "http://127.0.0.1:5000/callback#f"},

		// Non-loopback branch: this branch is a byte-exact string comparison, so
		// pairing a malformed URI with a DIFFERENT clean one would be refused by
		// the byte comparison alone even with the component guards deleted — a
		// vacuous test. To actually falsify component validation the SAME
		// malformed URI is supplied on BOTH sides: byte-exact equality WOULD
		// accept it, so a refusal proves the guard fired at validation rather
		// than the pair being caught as unequal (#2427 fix-up).
		{"userinfo identical nonloopback", "https://u@app.example/callback", "https://u@app.example/callback"},
		{"query identical nonloopback", "https://app.example/callback?a=1", "https://app.example/callback?a=1"},
		{"fragment identical nonloopback", "https://app.example/callback#f", "https://app.example/callback#f"},
	}
	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			t.Parallel()
			assertRedirectRefused(t, MatchRedirectURI(v.registered, v.requested))
		})
	}
}

// TestMatchRedirectURI_EmptyFragmentRefused is the #2427 condition-1 pin: a bare
// trailing '#' is invisible in the parsed form, so it must be caught lexically.
func TestMatchRedirectURI_EmptyFragmentRefused(t *testing.T) {
	t.Parallel()
	// requested side, loopback branch — the attacker-controlled direction.
	assertRedirectRefused(t, MatchRedirectURI("http://127.0.0.1/callback", "http://127.0.0.1:5000/callback#"))
	// registered side too.
	assertRedirectRefused(t, MatchRedirectURI("http://127.0.0.1/callback#", "http://127.0.0.1:5000/callback"))
	// non-loopback: supply the identical bare-'#' URI on BOTH sides so the
	// byte-exact non-loopback comparison WOULD accept it — only the lexical
	// empty-fragment guard can refuse, which is what this pins (a
	// clean-vs-'#' pair would be caught as unequal regardless of the guard).
	assertRedirectRefused(t, MatchRedirectURI("https://app.example/callback#", "https://app.example/callback#"))
	// Confirm the parsed guard alone would MISS this (documents why lexical).
	u, err := parseNoLexical("http://127.0.0.1/callback#")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if u.Fragment != "" || u.RawFragment != "" {
		t.Fatalf("expected parsed form to show no fragment for a bare '#', got %q/%q", u.Fragment, u.RawFragment)
	}
}

// TestMatchRedirectURI_HostlessPrivateSchemeRefused pins the #2427 condition-3
// policy: private-use / hostless absolute URIs are deliberately unsupported.
func TestMatchRedirectURI_HostlessPrivateSchemeRefused(t *testing.T) {
	t.Parallel()
	assertRedirectRefused(t, MatchRedirectURI("myapp:/callback", "myapp:/callback"))
	assertRedirectRefused(t, MatchRedirectURI("myapp:callback", "myapp:callback"))
	assertRedirectRefused(t, MatchRedirectURI("https://app.example/callback", "myapp:/callback"))
}

func TestMatchRedirectURI_RelativeAndUnparseableRefused(t *testing.T) {
	t.Parallel()
	assertRedirectRefused(t, MatchRedirectURI("/callback", "/callback"))
	assertRedirectRefused(t, MatchRedirectURI("https://app.example/callback", "://bad"))
}

func TestMatchRedirectURIAny(t *testing.T) {
	t.Parallel()
	registered := []string{"https://a.example/cb", "http://127.0.0.1/cb"}
	got, err := MatchRedirectURIAny(registered, "http://127.0.0.1:9000/cb")
	if err != nil {
		t.Fatalf("MatchRedirectURIAny = %v, want match", err)
	}
	if got != "http://127.0.0.1/cb" {
		t.Fatalf("matched %q, want the loopback registration", got)
	}
	if _, err := MatchRedirectURIAny(registered, "https://evil.example/cb"); err == nil {
		t.Fatalf("MatchRedirectURIAny matched an unregistered URI")
	}
	if _, err := MatchRedirectURIAny(nil, "https://a.example/cb"); err == nil {
		t.Fatalf("MatchRedirectURIAny with no registrations should error")
	}
}

func assertRedirectRefused(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected redirect refusal, got nil")
	}
	assertCode(t, err, ErrCodeInvalidRequest)
	if !errors.Is(err, ErrRedirectMismatch) {
		t.Fatalf("refusal %v does not wrap ErrRedirectMismatch", err)
	}
}
