# credstore

Shared credential-store module (#2389 / ADR-076) — persists minted Fishhawk bearer tokens on the local filesystem so both the `fishhawk` CLI and `fishhawk-mcp` can authenticate without re-passing a secret on every call. Promoted out of `cli/internal/credstore` so the backend-module `fishhawk-mcp` binary can import it without inverting the module hierarchy (a backend → cli dependency).

## File location

A single JSON file under the XDG config directory: `$XDG_CONFIG_HOME/fishhawk/credentials`, falling back to `~/.config/fishhawk/credentials`. `Path()` returns the resolved absolute path.

## Permission posture

The file holds live bearer secrets, so it is written `0600` (owner read/write only) and its containing directory `0700`. A wider mode would let a co-tenant read a live token.

## Key normalization

Each credential is keyed by its backend URL, canonicalized by `normalizeURL` (trailing-slash and surrounding-whitespace trimmed) so `http://localhost:8080` and `http://localhost:8080/` address the same record. An operator can hold tokens for several backends at once; a consumer picks the one matching its resolved `--backend-url` / `FISHHAWK_BACKEND_URL`.

## Atomic write

`Store` writes to a temp file in the same directory and `rename`s it into place, so a crash mid-write never truncates an existing credential set. `Store` merges into any existing store — re-storing one backend leaves the others untouched.

## API

- `Load(backendURL) (Credential, error)` — the credential for a backend, or `ErrNotFound` if none is stored. A present-but-corrupt store surfaces the wrapped parse error (distinct from `ErrNotFound`) so a corrupt store is loud rather than silently read as "no credential".
- `List() (map[string]Credential, error)` — every stored credential keyed by normalized backend URL. A missing file is an empty map, not an error.
- `Store(backendURL, Credential) error` — persist a credential (atomic, merging). The read-merge-write runs under the SAME exclusive `credentials.lock` the refresh path takes (see *Multi-process serialization*), so a `token login` write cannot clobber a concurrent rotation.
- `Path() (string, error)` — the credentials file path.
- `RefreshStored(ctx, *http.Client, backendURL) (Credential, error)` / `Refresh(ctx, *http.Client, Credential) (Credential, error)` / `NeedsRefresh` / `RefreshSkew` — the refresh contract below.
- `RefuseRedirect` / `*RedirectRefusedError` — the `http.Client` `CheckRedirect` policy every secret-bearing OAuth request uses, exported so the CLI's authorization-code exchange installs the SAME policy (see *Redirect refusal*).

`Credential.ExpiresAt` is `*time.Time`; a nil value means non-expiring (v0 device-flow tokens do not expire).

## Refresh contract (#2393 / ADR-076)

An OAuth access token minted by the Fishhawk authorization server is short-lived (15 minutes by default — `--oauth-access-token-ttl`), so a stored credential must be able to refresh itself. Four fields carry what an RFC 6749 §6 refresh needs, every one `json:",omitempty"` so a store written before they existed (a device-flow `fhk_` record) still decodes unchanged:

| Field | JSON key | Role |
|---|---|---|
| `RefreshToken` | `refresh_token` | the rotating secret presented at the token endpoint |
| `ClientID` | `client_id` | the public OAuth client the grant was issued to |
| `TokenEndpoint` | `token_endpoint` | the AS token endpoint to present it at |
| `IssuedAt` | `issued_at` | when the access token was minted; lets the skew be derived from the credential's own lifetime |

- `Credential.Refreshable()` — true only when all three of `RefreshToken`/`ClientID`/`TokenEndpoint` are set. A credential missing any of them is never presented at a token endpoint.
- `RefreshSkew(lifetime)` — the proactive-refresh margin, DERIVED from the lifetime rather than fixed: `lifetime*2/15` (exactly 2 minutes at the shipped 15-minute default), floored at 30s so a very short TTL cannot leave a margin thinner than one refresh round-trip, and capped at `lifetime/2` so the floor can never exceed the credential's own life. A non-positive lifetime has no margin.
- `Credential.Lifetime()` — `ExpiresAt − IssuedAt` when both are known; `DefaultLifetime` (15 minutes, pinned equal to the AS default by `backend/internal/server/oauthttlskew_test.go`) when `IssuedAt` is absent; 0 for a non-expiring credential.
- `NeedsRefresh(c, now)` — false for a nil `ExpiresAt` (non-expiring), false when not `Refreshable()` (nothing to refresh WITH), otherwise true once `now` is within `RefreshSkew(Lifetime())` of `ExpiresAt`. An already-expired refreshable credential ALSO needs refresh — the refresh token outlives the access token, so the right move is to refresh, not to fail.
- `Refresh(ctx, hc, c)` — the RFC 6749 §6 public-client refresh: `POST` `application/x-www-form-urlencoded` to `c.TokenEndpoint` with `grant_type=refresh_token`, `refresh_token`, `client_id`, and NO `client_secret` and NO `Authorization` header (the token endpoint refuses both outright for a `token_endpoint_auth_method=none` client). Returns a credential carrying the new access token, the ROTATED refresh token, `IssuedAt=now`, `ExpiresAt=now+expires_in`, preserving `Subject`/`Provider` and taking `Scopes` from the response `scope` when present. A non-2xx decodes the §5.2 `{error,error_description}` envelope into a typed `*RefreshError` (assert on `.Code`, e.g. `invalid_grant`); a 2xx with no `access_token` is an error, never a silently empty bearer; a non-JSON body is an error. The caller's `*http.Client` is COPIED and given `RefuseRedirect` unconditionally — a caller cannot opt its refresh out of the control, and its own client is not mutated.
- `RefreshStored(ctx, hc, backendURL)` — what consumers call. Runs load → refresh → store under an EXCLUSIVE file lock and RE-READS the credential after acquiring it, returning the credential to use now. The lock wait honours `ctx` and is bounded (see *Multi-process serialization*), so a contended lock cannot outlive the caller's deadline.

### Persist-the-rotation invariant

The AS consumes the presented refresh token the moment the rotation commits. A rotated refresh token that is not written back to the store is therefore BURNED: the next consumer would present the consumed token and trip the AS's reuse detection, which revokes the whole lineage. `RefreshStored` persists before returning and treats a persist failure as an error; a consumer that calls `Refresh` directly owns the same obligation.

### Multi-process serialization

The CLI and `fishhawk-mcp` (and two concurrent CLI invocations) share one store. Without serialization both would read the same refresh token, both would present it, and the loser would trip reuse detection. `RefreshStored` takes an exclusive `flock` on a sibling `credentials.lock` file — NOT on the credentials file itself, because `Store` replaces that file by rename and a lock on the old inode would not survive a sibling's write — and only then loads the credential. The loser of the race re-reads the winner's freshly stored credential, finds it outside its skew, and uses it without dialing; exactly one rotation happens. `flock` is advisory and unix-only, which matches the release matrix (darwin/linux). `TestRefreshStored_TwoConsumersRotateOnce` pins it: deleting the lock, or moving the load ahead of it, makes the loser present the consumed token.

**Every writer participates, not just refreshers.** `Store` takes the same lock around its own read-merge-write. A `token login` write is a read-merge-write over ONE shared JSON file, so a login racing a refresh would otherwise read the pre-rotation map and rename its stale merge over the rotation — restoring a refresh token the AS has already consumed, so the next use trips reuse detection and revokes the lineage. That race is cross-BACKEND too: a login for one backend URL can clobber another backend's rotation, because both live in the same file. `TestStore_SerializesWithRefreshRotation` pins it in COMMITTED STATE (a `storeMergeBarrier` test seam holds the writer inside the read-merge-write window so the interleaving is deterministic rather than clock-raced); with the lock removed from `Store` the rotated token is replaced by the consumed seed. `RefreshStored` calls the internal `storeLocked` rather than `Store`, since `flock` would block on a second descriptor for a lock this goroutine already holds.

**The wait is BOUNDED and CANCELLABLE.** `syscall.Flock` has no timeout and no way to abort, so a blocking `LOCK_EX` behind a SUSPENDED peer (SIGSTOP, a debugger, a swapped-out process) would ignore `ctx` entirely and outlive the CLI/MCP 30-second refresh deadline. The acquire is therefore `LOCK_EX|LOCK_NB` retried every 20ms against `ctx`, capped at `lockWait` (10s), and the failure wraps `ctx.Err()` so `errors.Is(err, context.DeadlineExceeded)` holds. `Store`, which takes no `ctx`, uses the same 10s bound. `TestRefreshStored_LockWaitIsBoundedAndCancellable` holds the lock on an INDEPENDENT descriptor for longer than the caller's deadline and asserts the call returns the deadline error without dialing the token endpoint; restoring the blocking `LOCK_EX` makes it hang until the test binary's timeout.

### Redirect refusal on secret-bearing requests

An OAuth token-endpoint POST carries a refresh token (or an authorization code and a PKCE `code_verifier`) in its BODY. Under Go's default `http.Client` policy a `307`/`308` replays that body verbatim at whatever origin the response names, and a `301`/`302`/`303` re-issues the request against it — either way the secret leaves for a destination the credential does not name, and an `https`→`http` hop delivers it in the clear.

`RefuseRedirect` is the `CheckRedirect` policy that follows NOTHING, returning a typed `*RedirectRefusedError` naming the `scheme://host/path` of both ends (never the attacker-chosen query). Allowing ZERO hops enforces "the intended destination and secure transport across redirects" BY CONSTRUCTION: the destination is exactly the configured endpoint, so there is no hop left to origin- or scheme-check. The refusal happens before the redirected request is sent, so the unauthorized destination receives no bytes at all.

It is installed on both secret-bearing legs — `Refresh` here (copying the caller's client, overriding any `CheckRedirect` it supplied) and the CLI's authorization-code exchange, which imports this same symbol rather than re-declaring the policy. `TestRefresh_RefusesRedirectAndLeaksNothingToTheHOP` and the CLI's `TestOAuthLogin_TokenExchangeRefusesRedirectAndLeaksNothing` point the hop at a REACHABLE in-test server that records every request it sees (an unreachable address would produce a dial error indistinguishable from the refusal) and assert it saw ZERO across `302`/`307`/`308`.

### Refresh failure is fatal for every consumer

When a stored credential needs refresh and the refresh fails, every consumer ladder (the CLI's `newClient`, `fishhawk doctor`, and `fishhawk-mcp` startup) stops with an actionable error naming `fishhawk token login --backend-url <url>` — never a silent degrade to the stale bearer or to an unauthenticated client. An expired credential that is not refreshable (an `fhk_` record carrying an expiry) is refused the same way. A nil `ExpiresAt` still means non-expiring and is accepted everywhere.

## Consumers

- **CLI** — `cli/cmd/fishhawk/token.go` (`token login` writes, `token list` reads), `cli/cmd/fishhawk/run.go` (the `newClient` fallback when no `--token`/env is set, refreshing via `RefreshStored` when the stored credential is inside its skew) and `cli/cmd/fishhawk/doctor.go` (the same ladder, reporting a refresh or a refresh failure on the `token valid` rung).
- **fishhawk-mcp** — `backend/cmd/fishhawk-mcp/main.go`'s startup token-resolution ladder: `FISHHAWK_API_TOKEN` wins when set; when empty, the credential keyed by the resolved backend URL is loaded and refreshed if inside its skew; a not-usable credential (including one whose refresh failed) fails startup rather than degrading to an empty or stale bearer.
