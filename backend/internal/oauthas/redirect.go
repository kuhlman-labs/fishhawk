package oauthas

import (
	"net/netip"
	"net/url"
	"strings"
)

// MatchRedirectURI implements the two-branch RFC 8252 §7.3 redirect-URI matcher.
//
// Policy (stated deliberately per the #2427 approval): the port relaxation RFC
// 8252 §7.3 requires is confined to the loopback branch; NOTHING else is relaxed.
// userinfo, a query and a fragment are refused on BOTH the registered AND the
// requested URI on BOTH branches. Private-use scheme redirect URIs (RFC 8252 §7.1,
// e.g. myapp:/callback) are DELIBERATELY NOT SUPPORTED: our only CIMD client uses
// loopback, and admitting hostless absolute URIs widens the matcher surface for no
// current consumer. Such URIs are refused by the empty-host check. If a
// private-scheme client ever appears, that is an additive change with its own
// review.
//
// If both URIs are loopback, scheme (case-insensitive), host (case-insensitive)
// and EscapedPath (byte-exact) must match and the port is ignored. If neither is
// loopback, the ORIGINAL raw strings must be byte-exactly equal with no
// normalization. If exactly one is loopback the match is refused. Every refusal
// returns ErrRedirectMismatch wrapped in an invalid_request *Error whose
// Description names the offending component and never echoes the requested URI.
//
// MATCH is not DELIVER (#2470). Ignoring the port at MATCH time is only half of
// RFC 8252 §7.3: a native client registers portless precisely because it listens
// on an ephemeral port, so the authorization RESPONSE must go to the port it
// asked for — delivering to the registered portless URI means the client never
// receives its code. ResolveRedirectURI is the function that produces the
// DELIVERY URI; MatchRedirectURI stays purely a predicate. ONLY the port ever
// crosses from the requested URI into the delivery URI: scheme, host and path,
// and the guarantee that there is no userinfo, query or fragment, all come from
// the validated REGISTERED side.
func MatchRedirectURI(registered, requested string) error {
	regU, err := validateRedirectURIComponents(registered)
	if err != nil {
		return err
	}
	reqU, err := validateRedirectURIComponents(requested)
	if err != nil {
		return err
	}
	regLoop := isLoopbackHost(regU)
	reqLoop := isLoopbackHost(reqU)
	switch {
	case regLoop && reqLoop:
		if !strings.EqualFold(regU.Scheme, reqU.Scheme) ||
			!strings.EqualFold(regU.Hostname(), reqU.Hostname()) ||
			regU.EscapedPath() != reqU.EscapedPath() {
			return newError(ErrCodeInvalidRequest, "redirect URI does not match the registered loopback URI (scheme, host or path differs)").withCause(ErrRedirectMismatch)
		}
		return nil
	case !regLoop && !reqLoop:
		if registered != requested {
			return newError(ErrCodeInvalidRequest, "redirect URI does not exactly match the registered URI").withCause(ErrRedirectMismatch)
		}
		return nil
	default:
		return newError(ErrCodeInvalidRequest, "redirect URI loopback status does not match the registered URI").withCause(ErrRedirectMismatch)
	}
}

// ResolveRedirectURI returns the DELIVERY redirect URI for requested against a
// client's registered set, for the multi-URI registration case: the first
// registration MatchRedirectURI accepts, with the requested URI's PORT
// substituted on the loopback branch (see deliveryRedirectURI).
//
// It deliberately REPLACES the older MatchRedirectURIAny, which returned the
// registered URI. Keeping both would leave a function that returns the
// registered URI beside one that returns the delivery URI, re-arming the #2470
// footgun: every caller wants the delivery URI, and the two are
// indistinguishable at a call site.
//
// Every refusal (an empty registration list; no registration accepting
// requested) is the same ErrRedirectMismatch-wrapped invalid_request as before,
// and returns an EMPTY delivery URI — a caller must never redirect on error.
func ResolveRedirectURI(registered []string, requested string) (string, error) {
	if len(registered) == 0 {
		return "", newError(ErrCodeInvalidRequest, "no registered redirect URIs").withCause(ErrRedirectMismatch)
	}
	for _, reg := range registered {
		if err := MatchRedirectURI(reg, requested); err == nil {
			return deliveryRedirectURI(reg, requested)
		}
	}
	return "", newError(ErrCodeInvalidRequest, "redirect URI does not match any registered URI").withCause(ErrRedirectMismatch)
}

// deliveryRedirectURI builds the URI the authorization response is delivered to,
// from an ALREADY-MATCHED (registered, requested) pair.
//
// The construction is registered-side-authoritative: it copies the PARSED
// registered *url.URL — keeping Scheme, Path and RawPath exactly as registered,
// and inheriting validateRedirectURIComponents' guarantee of no userinfo, query
// or fragment — and replaces ONLY the Host, with the registered hostname joined
// to the REQUESTED port. Nothing else is ever read from the requested URI, so no
// requested scheme, host or path can reach a Location header.
//
// Non-loopback is a no-op by construction: that branch of MatchRedirectURI
// required byte equality, so registered and requested are the same string and the
// registration is returned verbatim. Only the loopback branch, which ignores the
// port, has a port to carry over (RFC 8252 §7.3).
//
// Both raw strings are re-parsed through the SAME validator MatchRedirectURI
// used, so a parse or component failure here is a fail-closed error — never a
// silent fallback to the requested string. Through ResolveRedirectURI that
// branch is unreachable (the matcher validated both sides first); it is the
// contract for any future direct caller, and is pinned by a direct unit test.
func deliveryRedirectURI(registered, requested string) (string, error) {
	regU, err := validateRedirectURIComponents(registered)
	if err != nil {
		return "", err
	}
	reqU, err := validateRedirectURIComponents(requested)
	if err != nil {
		return "", err
	}
	if !isLoopbackHost(regU) || !isLoopbackHost(reqU) {
		return registered, nil
	}
	delivery := *regU
	host := regU.Hostname()
	if strings.Contains(host, ":") {
		// Hostname() strips an IPv6 literal's brackets, and re-joining
		// hostname+":"+port unbracketed yields the unparseable http://::1:5000/cb.
		host = "[" + host + "]"
	}
	// net/url rejects a non-numeric port at parse time, so Port() is a digit
	// string: it cannot inject a path, query or authority component.
	if port := reqU.Port(); port != "" {
		host += ":" + port
	}
	delivery.Host = host
	return delivery.String(), nil
}

// validateRedirectURIComponents applies the shared component rules to a single
// redirect URI (registered or requested, either branch): it rejects a fragment,
// a parse failure, a relative/scheme-less URI, an empty host (which also refuses
// hostless private-use-scheme URIs, see MatchRedirectURI), userinfo and a query.
func validateRedirectURIComponents(raw string) (*url.URL, error) {
	// The fragment check is LEXICAL and runs before parsing: net/url records no
	// "fragment present but empty" flag (unlike ForceQuery for '?'), so a bare
	// trailing '#' (http://127.0.0.1/callback#) is invisible in the parsed form
	// and would compare equal to a URI without it. Rejecting '#' on the raw string
	// is the only way to enforce the symmetric fragment refusal (#2427 condition 1).
	if strings.Contains(raw, "#") {
		return nil, newError(ErrCodeInvalidRequest, "redirect URI must not contain a fragment").withCause(ErrRedirectMismatch)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, newError(ErrCodeInvalidRequest, "redirect URI is not a valid URL").withCause(ErrRedirectMismatch)
	}
	if u.Scheme == "" || !u.IsAbs() {
		return nil, newError(ErrCodeInvalidRequest, "redirect URI must be an absolute URL with a scheme").withCause(ErrRedirectMismatch)
	}
	if u.Host == "" {
		return nil, newError(ErrCodeInvalidRequest, "redirect URI must have a host; hostless private-use-scheme URIs are not supported").withCause(ErrRedirectMismatch)
	}
	if u.User != nil {
		return nil, newError(ErrCodeInvalidRequest, "redirect URI must not contain userinfo").withCause(ErrRedirectMismatch)
	}
	if u.RawQuery != "" || u.ForceQuery {
		return nil, newError(ErrCodeInvalidRequest, "redirect URI must not contain a query").withCause(ErrRedirectMismatch)
	}
	// Defense in depth: also refuse a parsed fragment. The lexical guard above is
	// authoritative, but this catches any non-empty fragment explicitly.
	if u.Fragment != "" || u.RawFragment != "" {
		return nil, newError(ErrCodeInvalidRequest, "redirect URI must not contain a fragment").withCause(ErrRedirectMismatch)
	}
	return u, nil
}

// isLoopbackHost reports whether u's host is localhost, an address in 127.0.0.0/8,
// or ::1.
func isLoopbackHost(u *url.URL) bool {
	host := strings.ToLower(u.Hostname())
	if host == "localhost" {
		return true
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	return addr.Unmap().IsLoopback()
}
