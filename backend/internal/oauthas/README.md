# backend/internal/oauthas

Pure OAuth 2.1 authorization-server domain core (ADR-076 / E66.14, #2427).

## Role and boundary

This package is the security-critical algorithmic core of the Fishhawk OAuth 2.1
authorization server, and NOTHING else. It holds:

- a typed RFC 6749 §4.1.2.1 / §5.2 (+ RFC 8707 `invalid_target`) error set,
- issuer / resource-identifier parsing and RFC 8707 audience matching,
- the ratified operator scope vocabulary,
- S256-only PKCE verification,
- the RFC 8252 §7.3 redirect-URI matcher, and
- CIMD (Client ID Metadata Document) fetch + validation behind a connect-time
  SSRF guard.

It has **no database, no HTTP handlers and no config**, and it **must not import
`backend/internal/server`** (or the persistence packages). #2391 consumes this
package and owns the storage, endpoints, metadata documents, middleware and
config. The hard boundary is machine-enforced by
`TestPackage_ImportsNeitherServerNorDatabase`, which fails the build if this
package ever reaches across it.

Every exported helper returns the typed `*Error` values in `errors.go`. `Error`
carries a client-returnable `Code` + `Description` and an internal-only `Cause`;
#2391's handlers render `Code`/`Description` to the client and log `Cause` only,
so a CIMD fetch failure cannot leak an internal address into an OAuth error
response. `errors.Is` works on both the code axis
(`errors.Is(err, &Error{Code: ErrCodeInvalidGrant})`) and the sentinel axis
(`errors.Is(err, ErrPKCEMismatch)`).

## Scope vocabulary (single source of truth)

`SupportedScopes` is a read-only **mirror** of `operatorDefaultScopes` in
`backend/cmd/fishhawkd/token.go` (the single source of truth): #2391 ratified one
scope language for the product, with no parallel MCP-only vocabulary.

The two lists cannot share a Go symbol — `operatorDefaultScopes` lives in a `main`
package and is unimportable. Rather than assert the mirror with a hardcoded
duplicate (which would be a false drift detector), the drift test
`TestSupportedScopes_MatchesOperatorDefault` **AST-parses `token.go`** and compares
the parsed literal against `SupportedScopes` by value and order (#2427 condition
5, option **a** — real detection, not a restated constant). Real drift fails the
in-loop verify gate. Publishing `scopes_supported` makes later narrowing additive.

## PKCE contract

S256 **only**. `plain` is refused and never advertised. An **absent**
`code_challenge_method` is refused, never defaulted: RFC 7636 §4.3 defaults an
absent method to `plain`, which this server does not support. A missing
`code_challenge` is refused by `ValidateCodeChallenge`, so a downgrade to no-PKCE
is impossible through `VerifyPKCE`. The final comparison is constant-time
(`crypto/subtle`).

## Redirect matcher (RFC 8252 §7.3)

`MatchRedirectURI` has two branches:

| | loopback branch | non-loopback branch |
|---|---|---|
| when | both URIs are loopback (`localhost`, `127.0.0.0/8`, `::1`) | neither is loopback |
| compare | scheme (case-insensitive), host (case-insensitive), `EscapedPath` (byte-exact) | the ORIGINAL raw strings, byte-exact |
| port | **ignored** (RFC 8252 §7.3 permits any loopback port) | significant |

If exactly one side is loopback the match is refused (no cross-branch match).

**userinfo, query and fragment are refused on BOTH the registered AND the
requested URI on BOTH branches.** The port relaxation is the ONLY thing loopback
relaxes.

**The fragment check is LEXICAL** (it rejects any raw redirect URI containing
`#`, before parsing). This is deliberate and load-bearing: `net/url` records no
"fragment was present but empty" flag (unlike `ForceQuery` for `?`), so a bare
trailing `#` (`http://127.0.0.1/callback#`) is invisible in the parsed form and
would compare equal to a URI without it. Only a raw-string check can enforce the
symmetric fragment refusal (#2427 condition 1). A parsed-form guard remains as
defense in depth for a non-empty fragment.

**Private-use scheme redirect URIs are deliberately NOT supported** (#2427
condition 3). RFC 8252 §7.1 permits private-use / app-scheme URIs like
`myapp:/callback`; this AS does not admit them today. Our only CIMD client uses
loopback, and admitting hostless absolute URIs widens the matcher surface for no
current consumer. Such URIs are refused by the empty-host check. If a
private-scheme client ever appears, that is an additive change with its own
review — it is not left ambiguous now.

## Audience matching (RFC 8707)

`ResourceMatches` compares a requested resource against a token audience: scheme
and host case-insensitively with the scheme's default port elided, an empty path
treated as `/`, and **path plus query compared byte-exactly on their ESCAPED
(percent-encoded) form**. The path is compared via `EscapedPath`, not the decoded
`Path`: `url.Parse` decodes percent-escapes into `Path`, so comparing `Path` would
conflate distinct resource identifiers differing only in encoding (`"/a%2Fb"` and
`"/a/b"` both decode to `"/a/b"`), silently widening token audience binding. Query
comparison uses the raw `RawQuery`, which is already unescaped-preserving. Nothing
else is normalized.

## What the connect guard does and does not guarantee

`PublicOnlyAddrGuard` + `guardedDialer` enforce a **point-in-time enumeration of
IANA IPv4/IPv6 special-use prefixes** (`specialUsePrefixes`) at **connect time**,
against the socket's **actual peer address**. The dialer resolves a hostname once
through the injected lookup seam, **fails closed if ANY resolved candidate is
blocked** (one private A record among public ones is an attack, not a fallback),
and dials the **guarded IP literal** so the transport never re-resolves the
hostname. The inner `net.Dialer.ControlContext` re-runs the guard on the peer
address as the load-bearing enforcement point. Together these close the
DNS-rebinding hole a pre-flight resolve leaves open. The guard is NOT built on
`net.IP.IsGlobalUnicast`, which returns true for CGNAT (`100.64.0.0/10`), IETF
protocol assignments (`192.0.0.0/24`), benchmarking (`198.18.0.0/15`) and
deprecated IPv6 site-local (`fec0::/10`).

It does **NOT**:

- consult live IANA registries (the prefix list is a static, reviewed snapshot);
- protect against a genuinely public host that proxies to an internal service;
- cover non-TCP or non-HTTP egress;
- substitute for network-level egress policy;
- constrain a non-loopback redirect URI's scheme.

`http.Transport` still derives TLS SNI and certificate verification from the URL
host even though `DialContext` connects to a pinned literal, so certificate
validation is unaffected.

The `Fetcher` wires the guarded transport whenever its `HTTPClient` is nil **or**
the injected client's `Transport` is nil, so a caller passing
`&http.Client{Timeout: …}` is not silently unguarded (a nil `Transport` otherwise
falls through to `http.DefaultTransport`). A caller who injects a client with a
non-nil `Transport` owns that transport's egress policy — it is used as-is.

## The "validate one thing, then let something else become the real thing" bug class

This area has a recurring, named defect class worth hunting for in every review
here — validate one representation of a value, then let a DIFFERENT representation
become the value actually used:

1. **#2390 self URL** — validate a configured self URL, then advertise a
   different one.
2. **#2390 listener bind** — a healthy `/healthz` from a stale daemon while the
   fresh bind failed.
3. **pre-flight resolve** — guard a resolved address, then let the transport
   re-resolve (DNS rebinding). Closed by pinning the dialed literal.
4. **connect-only redirect revalidation** — guard the connect address of the
   initial request but not each redirect hop. Closed by revalidating every hop
   against the full predicate and sharing the guarded dialer across hops.

Each is closed by design here and pinned by a named falsifying test.

## Tests

Every test is a Go unit test with **zero network egress**: the CIMD tests drive an
in-process `httptest` TLS server bound to loopback (in-process, not egress) or a
scripted dialer through the `Fetcher`'s injected `HTTPClient` / `Guard` / `Lookup`
seams. See the plan's per-failure-mode checklist for the one-named-test-per-branch
matrix. Wall-clock durations in the timeout test derive from
`backend/internal/timescale` `D(base)` per the AGENTS.md rule.
