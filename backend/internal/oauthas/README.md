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

### Defaulting a request that carries no scope token (#2466)

RFC 6749 §3.3 permits exactly two behaviours when an authorization request omits
`scope`: process it with a **pre-defined default**, or fail `invalid_scope`. This
server takes the FIRST branch, via `ResolveRequestedScope(requested, registered)`
— a scope-omitting client (the shape of Claude Code's CIMD document) previously
could not get past the first authorize.

The contract is keyed on whether the request **carries a scope token**, not on
whether the parameter is present: `url.Values.Get` returns `""` for both an
absent `scope` key and a present-but-empty `scope=`, and the two are
DELIBERATELY equivalent — an empty value carries zero scope tokens, so making a
client that serializes an empty list fail where one that omits the key succeeds
would be exactly the onboarding trap this removes.

- **A non-empty `requested`** is delegated to `ParseScope` UNCHANGED, so every
  present-value branch keeps its behaviour and error identity. A whitespace-only
  `scope=%20%20` is PRESENT and still fails `invalid_scope`; so does an unknown
  scope; so does a scope exceeding the client's registered set (enforced one step
  later, in the server's authorize ladder).
- **An empty `requested`** defaults to the client's REGISTERED scope when the
  registration pins one, otherwise to the whole `SupportedScopes` vocabulary
  (what the PRM and AS metadata advertise as `scopes_supported`).

The registered default is **intersected** with `SupportedScopes` (registration
order preserved, de-duplicated) rather than taken verbatim. A registration may
pin scopes outside this server's vocabulary — the server splits the raw
registration string without validating its members — and such a scope is already
UNREQUESTABLE through the explicit path, because `ParseScope` rejects it before
the registered-scope restriction is ever reached. Defaulting to it verbatim would
grant through the default what the explicit path refuses.

A registration whose intersection is **EMPTY** fails CLOSED with `invalid_scope`
rather than minting a code carrying an empty grant.

The returned default is a defensive COPY: the caller persists it on the
authorization-code row, so handing out `SupportedScopes`' backing array would let
one request corrupt the vocabulary for every later one.

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

### MATCH is not DELIVER (#2470)

Ignoring the port at match time is only half of RFC 8252 §7.3. A native client
registers PORTLESS precisely because it listens on an ephemeral port, so the
authorization RESPONSE must go to the port it asked for — delivering to the
registered portless URI means the client never receives its code, and binds the
code row to a URI the client cannot present at the token endpoint.

`MatchRedirectURI` is therefore a pure PREDICATE. **`ResolveRedirectURI`** is the
function callers want: it returns the DELIVERY URI — the first registration the
matcher accepts, with the requested URI's port substituted on the loopback
branch.

| | matcher | delivery URI |
|---|---|---|
| loopback port | ignored | **preserved** — the requested port is what the response is sent to |
| scheme, host, path | compared | taken from the REGISTERED URI |
| userinfo, query, fragment | refused on both sides | absent by construction (inherited from the validated registered URI) |
| non-loopback | byte-exact equality required | the registration verbatim (identical to the request by definition) |

**ONLY the port ever crosses from the request into the delivery URI.** The
delivery URI is built by copying the PARSED registered `*url.URL` — preserving
`Scheme`, `Path` and `RawPath` exactly — and replacing only `Host` with the
registered hostname (re-bracketed for an IPv6 literal, since `Hostname()` strips
the brackets) joined to the requested port. `net/url` rejects a non-numeric port
at parse time, so the only value taken from the request is a digit string; it
cannot carry a path, query or authority component. The construction runs only
AFTER `MatchRedirectURI` has accepted, so no unvalidated request component can
reach a `Location` header, and a component failure inside the builder is a
fail-closed error, never a fallback to the requested string.

`MatchRedirectURIAny` (which returned the REGISTERED URI) was replaced rather
than kept alongside: two functions differing only in which URI they return are
indistinguishable at a call site, which is how #2470 shipped.

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

## Cache bounds (#2429)

The `Fetcher` caches validated CIMD documents keyed by `client_id` — an
**attacker-influenced string**. `ValidateClientIDURL` permits a query and the
self-referential check binds it, so one host can serve unlimited distinct valid
client_ids (`https://evil.example/id?v=1..N`). Before #2429 every successful
fetch was therefore a permanent allocation in an unbounded map, and TTL expiry
reclaimed nothing at all for those one-shot keys because reclamation only fired
on a later lookup of the SAME key, which never arrives.

The cache is now a **size-capped LRU** over stdlib `container/list` (a recency
list plus a key map, both under one mutex — no new dependency):

- **`MaxCacheEntries` caps residency, default 256.** A zero or negative value
  means the default, never "unbounded" and never "cache nothing". 256 is a
  judgment call, not a derived number: well above any plausible legitimate
  concurrent-client count, and trivially small in memory (a validated document
  with a few redirect URIs is well under 1 KiB). #2391 can raise it without
  touching this package.
- **Eviction is least-recently-USED, not FIFO.** Under exactly the flood this
  issue describes, a FIFO cap would evict a hot legitimate client after `cap`
  attacker insertions — converting a memory-exhaustion vector into an
  eviction-based denial of service. LRU keeps a client that is still in use.
- **Reclamation is STORE-TRIGGERED, not timer-driven.** The expiry sweep runs
  only inside `cacheStore`, so a below-capacity cache receiving no further stores
  holds its expired entries until the next store arrives. That is a deliberate
  property, not an oversight: this package has no lifecycle hooks and starting a
  background goroutine from a zero-value struct would be worse. The residual is
  bounded by the cap at a few hundred sub-KiB entries, so it is a liveness
  characteristic, not a memory-exhaustion vector — #2391 must not read a stronger
  liveness guarantee into it than exists.
- **Two sweep triggers.** (a) Amortized: at most once per TTL window, so a
  below-capacity cache still returns memory for one-shot keys. (b) Unconditional
  when the cache is at or over capacity, so an expired entry is always reclaimed
  in preference to evicting a live one.
- **The sweep is a FULL walk of the recency list, not a scan of the
  least-recently-used tail.** A cache hit moves an entry to the front WITHOUT
  re-stamping its expiry, so recency order is not expiry order: an expired entry
  can sit ahead of live ones. A tail-only scan would stop at the first live entry
  and then evict it, leaving the expired entry cached.
  `TestFetch_ExpiredEntriesEvictedBeforeLiveOnes` constructs that exact ordering
  and fails against a tail-only sweep.
- **The cache is per-`Fetcher` and in-memory**, so the cap bounds ONE instance.
  The intended use is one long-lived `Fetcher`, where the cap applies as designed.
  A `Fetcher` constructed per request is merely useless as a cache, not dangerous
  — there is no shared state to exhaust.

**What a size cap does NOT close.** An unauthenticated request that triggers an
outbound HTTPS CIMD fetch is an amplification vector regardless of cache size: a
capped cache bounds memory, not fetch rate, and a flood of distinct client_ids
still produces one outbound fetch each. Rate-limiting CIMD-triggering requests
belongs to **#2391's handler layer**, not to this pure domain core, which holds no
config and no HTTP. #2429 is a hard prerequisite for #2391 — no handler may reach
the `Fetcher` uncapped — but it is not the whole of the defence.

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
   against the predicate and sharing the guarded dialer across hops. One honest
   caveat: the hop is revalidated via `req.URL.String()`, and `url.URL.String()`
   cannot re-serialize a bare trailing `#` (net/url records no empty-fragment
   flag — the same limitation the redirect-URI matcher handles lexically), so the
   LEXICAL empty-fragment rule does not extend to hops; the hop check enforces the
   PARSED predicate (scheme, host, userinfo, non-empty fragment), not the
   raw-string one. Practical impact is negligible — fragments are never
   transmitted in requests.

Each is closed by design here and pinned by a named falsifying test.

## Tests

Every test is a Go unit test with **zero network egress**: the CIMD tests drive an
in-process `httptest` TLS server bound to loopback (in-process, not egress) or a
scripted dialer through the `Fetcher`'s injected `HTTPClient` / `Guard` / `Lookup`
seams. See the plan's per-failure-mode checklist for the one-named-test-per-branch
matrix. Wall-clock durations in the timeout test derive from
`backend/internal/timescale` `D(base)` per the AGENTS.md rule.
