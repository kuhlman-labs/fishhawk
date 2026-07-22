package auth

import (
	"net/url"
	"strings"
)

// Enterprise Managed User (EMU) enterprise-membership derivation
// (E44.8 / #1832).
//
// An EMU login is IdP-assigned as "<username>_<shortcode>", where the
// trailing short code names the GitHub Enterprise Cloud enterprise the
// user belongs to. A public github.com login may contain only
// alphanumerics and hyphens, so an underscore cannot appear in one —
// which is exactly why the derivation below is GATED on EMU posture:
// on a github.com deployment a crafted login carrying an underscore
// must never be read as an enterprise claim.
//
// Neither function makes a forge call: enterprise membership is read
// off the already-canonicalized login, not from an enterprise API.
// The FULL login (short code included) stays the identity key
// everywhere else — see canonicalGitHubLogin in github_oauth.go and
// identity.canonicalGitHubLogin, which this derivation does NOT alter.

// IsEMUOAuthHost reports whether the deployment's configured OAuth base
// URL points at a data-resident GitHub Enterprise Cloud host (a
// "<slug>.ghe.com" endpoint, per the E44.2/#1826 endpoint config). That
// posture — and only that posture — makes an underscore-bearing login an
// EMU login whose suffix is an enterprise short code.
//
// An empty, unparseable, or non-.ghe.com URL reports false, so the
// default (github.com) deployment derives no enterprise key at all.
func IsEMUOAuthHost(oauthBaseURL string) bool {
	raw := strings.TrimSpace(oauthBaseURL)
	if raw == "" {
		return false
	}
	host := raw
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		host = u.Host
	}
	// Strip any port and normalize case before matching.
	if h, _, ok := strings.Cut(host, ":"); ok {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	// A bare "ghe.com" is not a tenant endpoint; require a slug label.
	return strings.HasSuffix(host, ".ghe.com") && host != ".ghe.com"
}

// EnterpriseShortCode splits a canonical EMU login on its LAST
// underscore and returns the trailing enterprise short code. It reports
// ok=false for a login with no underscore, an empty short code
// ("alice_"), or an empty username half ("_acme").
//
// Only ever consulted under EMU posture (IsEMUOAuthHost).
func EnterpriseShortCode(login string) (string, bool) {
	i := strings.LastIndex(login, "_")
	if i < 0 {
		return "", false
	}
	user, code := login[:i], login[i+1:]
	if user == "" || code == "" {
		return "", false
	}
	return code, true
}
