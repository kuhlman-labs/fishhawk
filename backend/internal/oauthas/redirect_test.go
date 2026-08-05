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

// TestResolveRedirectURI_DeliversToRequestedLoopbackPort is the #2470 primary
// regression pin (M1/M2/M3/M7/M9): the DELIVERY URI carries the port the client
// requested, while everything else comes from the registration.
//
// COUNTERFACTUAL: with the port substitution deleted (ResolveRedirectURI
// returning the matched registration verbatim) M1, M7 and M9 go RED — each
// registered fixture is PORTLESS by construction, so the delivery and the
// registration are definitionally different strings and no setup call consults
// the control.
func TestResolveRedirectURI_DeliversToRequestedLoopbackPort(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		registered []string
		requested  string
		want       string
	}{
		{
			// M1: the Claude Code case. Portless loopback registration (RFC 8252
			// §7.3), ephemeral listening port on the request.
			name:       "M1 portless registration delivers to the requested ephemeral port",
			registered: []string{"http://127.0.0.1/callback"},
			requested:  "http://127.0.0.1:57121/callback",
			want:       "http://127.0.0.1:57121/callback",
		},
		{
			// M2: the inverse. A portless REQUEST against a port-bearing
			// registration delivers portless — the response mirrors the request,
			// it does not impose the registration's port.
			name:       "M2 portless request delivers portless",
			registered: []string{"http://127.0.0.1:3000/callback"},
			requested:  "http://127.0.0.1/callback",
			want:       "http://127.0.0.1/callback",
		},
		{
			// M3: non-loopback. That branch of the matcher required byte equality,
			// so the delivery is the registration verbatim and nothing changes.
			name:       "M3 non-loopback delivery is the registration verbatim",
			registered: []string{"https://app.example/cb"},
			requested:  "https://app.example/cb",
			want:       "https://app.example/cb",
		},
		{
			// M7: IPv6 bracketing round-trips. Hostname() strips the brackets, so
			// a naive hostname+":"+port join would produce http://::1:5000/cb.
			name:       "M7 IPv6 literal re-brackets around the requested port",
			registered: []string{"http://[::1]/cb"},
			requested:  "http://[::1]:5000/cb",
			want:       "http://[::1]:5000/cb",
		},
		{
			// M9: the multi-URI registration Claude Code actually publishes — both
			// members portless. Resolution picks the matching member and echoes
			// the port onto THAT member's host.
			name:       "M9 real multi-URI CIMD registration resolves against the matching member",
			registered: []string{"http://localhost/callback", "http://127.0.0.1/callback"},
			requested:  "http://127.0.0.1:57121/callback",
			want:       "http://127.0.0.1:57121/callback",
		},
		{
			// M9': the same registration, the OTHER member requested.
			name:       "M9 multi-URI registration resolves the localhost member",
			registered: []string{"http://localhost/callback", "http://127.0.0.1/callback"},
			requested:  "http://localhost:51000/callback",
			want:       "http://localhost:51000/callback",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ResolveRedirectURI(tc.registered, tc.requested)
			if err != nil {
				t.Fatalf("ResolveRedirectURI(%v, %q) = %v, want a delivery URI", tc.registered, tc.requested, err)
			}
			if got != tc.want {
				t.Fatalf("delivery = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestResolveRedirectURI_ProvenanceOnlyThePortCrossesOver is M8, the load-bearing
// counterfactual: the registration's host differs from the request's ONLY in
// CASE, so an implementation that echoed the REQUESTED string would return
// http://localhost:5000/cb while substituting only the port returns
// http://LOCALHOST:5000/cb. The case difference is invisible to the matcher
// (which compares hosts case-insensitively) but visible in the delivery, which
// is exactly what proves scheme/host/path come from the registered side.
//
// It is simultaneously the port counterfactual: with the substitution deleted
// the delivery is the portless http://LOCALHOST/cb.
func TestResolveRedirectURI_ProvenanceOnlyThePortCrossesOver(t *testing.T) {
	t.Parallel()
	got, err := ResolveRedirectURI([]string{"http://LOCALHOST/cb"}, "http://localhost:5000/cb")
	if err != nil {
		t.Fatalf("ResolveRedirectURI = %v, want a delivery URI", err)
	}
	const want = "http://LOCALHOST:5000/cb"
	if got != want {
		t.Fatalf("delivery = %q, want %q — scheme/host/path must come from the REGISTERED URI and only the port from the request", got, want)
	}
}

// TestResolveRedirectURI_Refusals covers every refusal branch, each returning an
// EMPTY delivery so a caller can never redirect on error.
//
// COUNTERFACTUAL for the open-redirect refusal: deleting the MatchRedirectURI
// acceptance guard inside ResolveRedirectURI's loop turns the M4 subtests RED —
// the evil inputs are well-formed absolute URIs with no userinfo/query/fragment,
// so the deletion cannot be absorbed by a component rejection, and the loop
// would return the first registration with no error.
func TestResolveRedirectURI_Refusals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		registered []string
		requested  string
	}{
		// M4: host, scheme and path mismatch against a loopback registration.
		{"M4 differing host", []string{"http://127.0.0.1/cb"}, "https://evil.example/cb"},
		{"M4 differing scheme", []string{"http://127.0.0.1/cb"}, "https://127.0.0.1:5000/cb"},
		{"M4 differing path", []string{"http://127.0.0.1/cb"}, "http://127.0.0.1:5000/other"},
		// M5: no registrations at all.
		{"M5 empty registration list", nil, "https://a.example/cb"},
		// M6: a non-numeric port — net/url refuses it at parse time, so it can
		// never reach the delivery builder as anything but a digit string.
		{"M6 non-numeric port", []string{"http://127.0.0.1/callback"}, "http://127.0.0.1:notaport/callback"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ResolveRedirectURI(tc.registered, tc.requested)
			assertRedirectRefused(t, err)
			if got != "" {
				t.Fatalf("a refused resolution returned delivery %q, want empty", got)
			}
		})
	}
}

// TestDeliveryRedirectURI_FailsClosedOnAnInvalidComponent pins the delivery
// builder's own fail-closed branch. Through ResolveRedirectURI it is unreachable
// (MatchRedirectURI validated both sides first), so it is exercised directly:
// a component failure must be an ERROR with an empty delivery, never a silent
// fallback to the requested string.
func TestDeliveryRedirectURI_FailsClosedOnAnInvalidComponent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		registered string
		requested  string
	}{
		{"invalid registered", "http://127.0.0.1/cb?a=1", "http://127.0.0.1:5000/cb"},
		{"invalid requested", "http://127.0.0.1/cb", "http://u@127.0.0.1:5000/cb"},
		{"unparseable requested", "http://127.0.0.1/cb", "http://127.0.0.1:notaport/cb"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := deliveryRedirectURI(tc.registered, tc.requested)
			assertRedirectRefused(t, err)
			if got != "" {
				t.Fatalf("delivery = %q, want empty on a component failure", got)
			}
		})
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
