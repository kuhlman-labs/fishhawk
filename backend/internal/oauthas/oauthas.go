// Package oauthas is the pure OAuth 2.1 authorization-server domain core for
// Fishhawk (ADR-076 / E66.14, #2427): a typed RFC 6749 error set, issuer and
// resource-identifier helpers, the ratified operator scope vocabulary, S256-only
// PKCE verification, the RFC 8252 §7.3 redirect-URI matcher, and CIMD
// fetch/validation behind a connect-time SSRF guard.
//
// This package is domain-only. It has NO database, NO HTTP handlers and NO
// config, and it MUST NOT import backend/internal/server (or the persistence
// packages). #2391 consumes this package and owns the storage and HTTP surface.
// Every exported helper returns the typed *Error values in errors.go so #2391's
// handlers can render Code plus Description to the client and log Cause only.
package oauthas

import (
	"net/url"
	"strings"
)

// Issuer is a normalized https issuer URL (RFC 8414). ParseIssuer strips a single
// trailing slash so https://as.example/ and https://as.example are one issuer.
type Issuer struct {
	raw string
}

// String returns the normalized issuer URL.
func (i Issuer) String() string { return i.raw }

// ParseIssuer validates and normalizes an issuer identifier: it requires the
// https scheme and a non-empty host, forbids userinfo, a query and a fragment,
// and strips a single trailing slash from the path. The fragment check is lexical
// (see validateRedirectURIComponents for why net/url cannot express a bare '#').
func ParseIssuer(raw string) (Issuer, error) {
	if raw == "" {
		return Issuer{}, newError(ErrCodeInvalidRequest, "issuer is empty")
	}
	if strings.Contains(raw, "#") {
		return Issuer{}, newError(ErrCodeInvalidRequest, "issuer must not contain a fragment")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return Issuer{}, newError(ErrCodeInvalidRequest, "issuer is not a valid URL").withCause(err)
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return Issuer{}, newError(ErrCodeInvalidRequest, "issuer scheme must be https")
	}
	if u.Host == "" {
		return Issuer{}, newError(ErrCodeInvalidRequest, "issuer host is empty")
	}
	if u.User != nil {
		return Issuer{}, newError(ErrCodeInvalidRequest, "issuer must not contain userinfo")
	}
	if u.RawQuery != "" || u.ForceQuery {
		return Issuer{}, newError(ErrCodeInvalidRequest, "issuer must not contain a query")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Path = strings.TrimSuffix(u.Path, "/")
	return Issuer{raw: u.String()}, nil
}

// ResourceIdentifier is an RFC 8707 §2 resource identifier: an absolute URI with
// a non-empty host and no fragment. A query is permitted and significant.
type ResourceIdentifier struct {
	raw string
}

// String returns the resource identifier as supplied.
func (r ResourceIdentifier) String() string { return r.raw }

// ParseResourceIdentifier validates an RFC 8707 §2 resource identifier: an
// absolute URI with a non-empty host and NO fragment (RFC 8707 forbids it). A
// query is permitted and preserved.
func ParseResourceIdentifier(raw string) (ResourceIdentifier, error) {
	if raw == "" {
		return ResourceIdentifier{}, newError(ErrCodeInvalidTarget, "resource identifier is empty")
	}
	if strings.Contains(raw, "#") {
		return ResourceIdentifier{}, newError(ErrCodeInvalidTarget, "resource identifier must not contain a fragment")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ResourceIdentifier{}, newError(ErrCodeInvalidTarget, "resource identifier is not a valid URI").withCause(err)
	}
	if u.Scheme == "" || !u.IsAbs() {
		return ResourceIdentifier{}, newError(ErrCodeInvalidTarget, "resource identifier must be an absolute URI")
	}
	if u.Host == "" {
		return ResourceIdentifier{}, newError(ErrCodeInvalidTarget, "resource identifier host is empty")
	}
	return ResourceIdentifier{raw: raw}, nil
}

// ResourceMatches reports whether a requested resource matches a token audience
// per RFC 8707: scheme and host are compared case-insensitively with the scheme's
// default port elided; path and query are compared byte-exactly on their ESCAPED
// (percent-encoded) form; an empty path is treated as equal to "/" and nothing
// else is normalized.
//
// The path is compared via EscapedPath, NOT the decoded Path: url.Parse decodes
// percent-escapes into Path, so comparing Path would conflate distinct resource
// identifiers whose only difference is encoding (e.g. "/a%2Fb" and "/a/b" both
// decode to "/a/b"), silently widening token audience binding.
func ResourceMatches(tokenAudience, requested string) bool {
	a, err := url.Parse(tokenAudience)
	if err != nil {
		return false
	}
	b, err := url.Parse(requested)
	if err != nil {
		return false
	}
	if !strings.EqualFold(a.Scheme, b.Scheme) {
		return false
	}
	if canonicalHost(a) != canonicalHost(b) {
		return false
	}
	if normalizeEmptyPath(a.EscapedPath()) != normalizeEmptyPath(b.EscapedPath()) {
		return false
	}
	return a.RawQuery == b.RawQuery
}

// canonicalHost lower-cases the host and elides the scheme's default port.
func canonicalHost(u *url.URL) string {
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "" {
		return host
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		if port == "80" {
			return host
		}
	case "https":
		if port == "443" {
			return host
		}
	}
	// u.Hostname() strips the brackets from an IPv6 literal, so re-bracket
	// before appending the port; otherwise "[2001:db8::1]:8443" (host
	// "2001:db8::1", port "8443") and "[2001:db8::1:8443]" (host
	// "2001:db8::1:8443", no port) both canonicalize to "2001:db8::1:8443",
	// conflating two distinct IPv6 authorities and widening audience binding.
	if strings.Contains(host, ":") {
		return "[" + host + "]:" + port
	}
	return host + ":" + port
}

// normalizeEmptyPath maps an empty path to "/" and leaves everything else exact.
func normalizeEmptyPath(p string) string {
	if p == "" {
		return "/"
	}
	return p
}
