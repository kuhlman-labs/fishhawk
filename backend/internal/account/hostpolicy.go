package account

import (
	"fmt"
	"net/url"
	"strings"
)

// ValidateResolvedBaseURL rejects a per-installation override host that is not
// a well-formed absolute https URL. It is the forge-neutral endpoint-policy
// contract shared by every per-installation forge consumer (the GitHub App
// installation-token mint, the githubclient REST client, the gitlabclient
// factory, and the identity provider): each of those ships a live credential to
// the resolved host, so an override resolved from the installations row must
// pass a scheme/parse sanity check before it can become the target host. An
// http:// value would transmit the credential without TLS, and a malformed or
// hostless value could send it somewhere unintended (ADR-057 Amendment A1 /
// E44.2 #1826, promoted forge-neutral in E44.16 #2094).
//
// This is a scheme/parse check, NOT a host allowlist — per-installation host
// pinning is layered on top via HostAllowed when an operator configures an
// allowlist. The rules were ported VERBATIM from githubapp's former private
// validateResolvedBaseURL so exactly one contract exists and cannot drift;
// githubapp now delegates here.
func ValidateResolvedBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("account: malformed installation base url %q: %w", raw, err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("account: installation base url %q must use https, got scheme %q", raw, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("account: installation base url %q missing host", raw)
	}
	return nil
}

// HostAllowed reports whether resolved's host is permitted by allowlist. An
// EMPTY allowlist is NOT consulted here — callers gate on len(allowlist) > 0
// before calling, so an empty allowlist means "no restriction" at the call
// site; HostAllowed itself returns false for an empty allowlist (nothing
// matches), matching the former githubapp.hostAllowed contract.
//
// resolved is expected already validated by ValidateResolvedBaseURL (an
// absolute https URL with a host), so url.Parse cannot fail on the routed path;
// a parse failure is nonetheless treated as NOT allowed (fail closed) rather
// than trusted. The host is lower-cased and port-stripped before matching.
func HostAllowed(resolved string, allowlist []string) bool {
	u, err := url.Parse(resolved)
	if err != nil {
		return false
	}
	return matchesHostAllowlist(strings.ToLower(u.Hostname()), allowlist)
}

// matchesHostAllowlist reports whether host matches any entry in allowlist.
// host is expected already lower-cased and port-stripped; allowlist entries are
// expected already lower-cased by the configuration parser. An entry is either:
//
//   - an exact host ("acme.ghe.com") — matched by equality; or
//   - a leading-dot suffix (".ghe.com") — matched when host ENDS WITH the
//     dotted suffix, so the dot is a TRUE label boundary. This admits any
//     subdomain ("acme.ghe.com") while rejecting the look-alike "notghe.com"
//     (whose final labels are "…tghe.com", not ".ghe.com") and the bare apex
//     "ghe.com" (unless "ghe.com" is also listed explicitly). Using the
//     dot-prefixed suffix — NOT a raw strings.HasSuffix(host, "ghe.com") — is
//     what pins label-boundary matching over substring matching (#2093).
func matchesHostAllowlist(host string, allowlist []string) bool {
	for _, entry := range allowlist {
		if strings.HasPrefix(entry, ".") {
			if strings.HasSuffix(host, entry) {
				return true
			}
			continue
		}
		if host == entry {
			return true
		}
	}
	return false
}
