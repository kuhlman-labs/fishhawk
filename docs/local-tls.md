# Local-dev TLS front end (E66.28 / #2453)

An **opt-in** loopback-only reverse proxy that terminates TLS on `:8443` and
forwards to the **unmodified** plain-http `fishhawkd` on `127.0.0.1:8080`, so the
OAuth 2.1 authorization server (E66 / #2436) can be exercised over `https` on a
workstation. It is **off by default** and touches no Go code.

## Why a front proxy, not in-process TLS

`fishhawkd` stays on `http://127.0.0.1:8080`. Terminating TLS *inside* `fishhawkd`
would collide with three loopback invariants:

- **MCP self-URL loopback pinning** (`backend/internal/server/mcproute.go:292`,
  `pinLoopbackSelfURL`) — in-process https would force an IP-SAN cert and an
  IP-literal issuer, the shape least likely to interop with clients.
- **The AS loopback gate** (`backend/internal/server/oauthas.go:155`).
- **The `#965`/`#1018` start-nonce listener-identity round-trip**, which proves
  the spawned pid *is* the listener. A proxy in that path would prove only that
  *something* forwards to a daemon holding our nonce.

A front proxy leaves all three untouched and mirrors the production posture,
where TLS terminates at ingress (see #2301 for the deployed-posture answer).

The proxy binds **loopback only** (`bind 127.0.0.1 ::1` inside the Caddy site
block). A bare `:8443` site address alone would bind `0.0.0.0` and publish the
loopback-only `fishhawkd` — the bare-bearer MCP tool surface and the whole
OAuth AS — to every host on the operator's network, which would be a worse
security posture than shipping no TLS at all.

## Enable it

Set in `.env`:

```sh
FISHHAWK_DEV_TLS=1              # opt in (default off; only the literal 1 enables)
# FISHHAWK_DEV_TLS_PORT=8443    # override the TLS port
# FISHHAWK_DEV_TLS_PROXY_BIN=/path/to/caddy   # explicit proxy binary
```

Install the proxy binary:

```sh
brew install caddy
```

The binary is **detected, never assumed**: an absent `caddy` (and no
`FISHHAWK_DEV_TLS_PROXY_BIN`) fails `scripts/dev up` with an actionable install
message rather than a silent skip that would leave a green `up` with no TLS
listener. `scripts/dev up` gates readiness on `/healthz` **through the proxy**
(`curl --cacert` against `https://localhost:8443/healthz`), so a dead proxy can
never read as green. `reload`/`post-merge`/`down` inherit the leg; the flag is
read from `.env`, so the switch survives the daily loop. `down` tears the proxy
down **unconditionally** (guarded on the pid file, never on the flag), so
disabling the flag between `up` and `down` never orphans the proxy.

## Client trust — `NODE_EXTRA_CA_CERTS`

Certificates land in the already-gitignored `.fishhawk/cache/tls/`:

- `ca.pem` — self-signed CA (`CN=Fishhawk local dev CA`).
- `server.pem` / `server.key` — a **dual-SAN leaf**: `DNS:localhost` **and**
  `IP:127.0.0.1`, plus `extendedKeyUsage=serverAuth`. The dual SAN is what makes
  **one** leaf verify for both `https://localhost:8443` and
  `https://127.0.0.1:8443`.

No system trust store install is required — **no `sudo`, no TouchID prompt, no
keychain mutation**. Installing the CA into the macOS system trust store would
**not** fix first-time MCP OAuth either (see below) — BoringSSL inside the Bun
runtime is the blocker, not the OS trust store, so a system install buys
nothing there. Point each client at the CA:

- **Node / Claude Code runtime:**
  `export NODE_EXTRA_CA_CERTS=<repo>/.fishhawk/cache/tls/ca.pem`
  (`scripts/dev up` prints this line). Without it, the client fails with
  `UNABLE_TO_VERIFY_LEAF_SIGNATURE`. This is the documented baseline and it IS
  what makes `claude mcp list` and token refresh work — but it is **not**
  sufficient for first-time MCP OAuth; see the subsection immediately below.
- **curl:** `curl --cacert <repo>/.fishhawk/cache/tls/ca.pem https://localhost:8443/…`

`fishhawkd`'s own internal self-dial stays on plain `http`, so no Go client needs
the CA.

### `NODE_EXTRA_CA_CERTS` is NOT sufficient for first-time MCP OAuth

Since Claude Code v2.1.113, the client ships as a Bun-compiled binary whose
global `fetch` uses BoringSSL, which does not honor `NODE_EXTRA_CA_CERTS`
(extra CAs must be supplied per-call via `init.tls.ca`); Claude Code threads
the CA through most HTTPS calls but not the OAuth initiation path, which calls
the MCP SDK's `auth()` with no CA-aware `fetchFn` — this is an **upstream**
defect, tracked at
[anthropics/claude-code#55760](https://github.com/anthropics/claude-code/issues/55760)
(open), not a Fishhawk one.

The local CA and TLS front end are **not** at fault: system Node v26 accepts
the CA over both `https` and undici `fetch`, and
`curl --cacert .fishhawk/cache/tls/ca.pem https://localhost:8443/healthz`
returns `200`. The failure is confined to the Bun runtime inside the `claude`
binary.

Per-path behavior (observed on Claude Code 2.1.222 — check whether
`anthropics/claude-code#55760` has since landed before relying on this):

- `claude mcp list` — **works**; `StreamableHTTPClientTransport` threads the CA
  into `init.tls.ca`.
- Token refresh for an already-authenticated server — **works**; passes a
  CA-aware `fetchFn`.
- First-time OAuth initiation — **fails** with
  `SDK auth failed: unable to verify the first certificate`; falls back to the
  global `Bun.fetch` with no CA override.

**One-time bootstrap** for first-time OAuth against the local dev CA:

```sh
NODE_TLS_REJECT_UNAUTHORIZED=0 claude
```

Then inside the session: `/mcp` → `<server>` → `Authenticate`. Quit and
relaunch normally, **without** the variable.

- This bypass is needed **exactly once** — once a refresh token exists, the
  token-refresh path is CA-aware and honors `NODE_EXTRA_CA_CERTS` on every
  later launch.
- It disables TLS verification process-wide for **that one session only**.
  **Do not** persist it into a shell profile, `.env`, or any other standing
  setting.

**Forward path:** the OAuth issuer is only an identifier, and TLS terminates
at the front proxy — so swapping the self-signed leaf for a publicly-trusted
certificate removes this problem entirely, with no code change on our side.
Publicly-trusted-cert / deployment posture is out of scope here; see #2301
above for the deployed-posture answer.

Certificates are regenerated only when **missing, within 7 days of expiry, or
mismatched** (the leaf no longer verifies against the CA). A routine `reload`
does **not** invalidate an exported `NODE_EXTRA_CA_CERTS`. When only the leaf is
renewing, the CA is **not** rotated, so the exported CA stays valid.

## AS config recipe

```sh
FISHHAWKD_ADDR=127.0.0.1:8080                 # fishhawkd stays plain-http loopback
FISHHAWKD_OAUTH_ISSUER=https://localhost:8443 # the https issuer (origin-only, no path)
FISHHAWKD_OAUTH_REQUIRE_LOOPBACK=true         # leave the loopback gate ON
FISHHAWKD_OAUTH_RESOURCE=https://localhost:8443/…   # carry :8443 exactly (see below)
```

`ParseIssuer` imposes no port constraint, and `resolveOAuthIssuer` accepts an
origin-only issuer with no path, so `https://localhost:8443` is valid.

### Audience-port foot-gun

**A non-default port IS significant in audience matching.**
`https://mcp.example/v0` does **not** match `https://mcp.example:8443/v0` — pinned
by `backend/internal/oauthas/oauthas_test.go:111`. Every resource identifier and
audience string in the local config must carry `:8443` **exactly as the client
sends it**, or validation fails with no obvious diagnosis. This is the single
most common way to misconfigure the local AS.

## Known limitation — no loopback CIMD

A Client ID Metadata Document (CIMD) cannot be hosted on loopback: the dialguard
(`backend/internal/oauthas/dialguard.go`, #2427/#2433) blocks `127.0.0.0/8` and
`::1/128` **by design**. So #2438 client pre-registration is the offline local
path for exercising the AS.

## Non-goal — no http-issuer escape hatch

There is deliberately **no** dev-only http-issuer mode. It would bypass the
control this slice exists to build, and RFC 8414 §2 grants no loopback
exemption. The TLS front end is the supported way to exercise the AS locally.

## Implementation

`scripts/dev` (helpers `_tls_*`, wired into `cmd_up`/`cmd_down`), pinned against
stubs by `scripts/test-dev`. Long-form contract: `scripts/README.md`.
