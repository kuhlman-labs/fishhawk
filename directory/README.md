# fishhawk-directory

The **global directory**: the small, customer-data-free control-plane service
that maps a forge account to its **home region** and redirects login /
App-install callbacks into that region's cell.

Anchor: ADR-062 (#2099), E44.7 / #1831 (regional cells). This module is slice 1
of that work — the cell-side region pin, region-scoped inference, and the deploy
topology land elsewhere.

## Why a separate module

The directory is the only Fishhawk component that is *not* regional. It must
hold no customer data, so it has its own tiny Postgres, its own binary, and its
own Go module. `backend` has **no compile-time dependency** on it: the region
handoff is a runtime HTTP 302, not a Go import. The one shared surface is
`directory/pkg/handoff`, which the backend imports for cell-side validation.

## Design

### Routing is a 302, never a proxy

The directory answers `GET` with `302 Found` (RFC 9110 §15.4.3). The browser
re-issues the request at the cell, so **no request body ever transits the global
plane** — the OAuth code / App-install credentials go straight to the region
that will hold the data.

Every routed surface is **GET-only by construction** (`GET /v0/...` patterns on
the mux; a non-GET returns 405), so the classic "302 rewrites POST to GET"
hazard cannot arise.

The redirect **preserves the request**: the `Location` is the resolved cell base
URL joined with the *original request path* and the *full original query string*
(`code`, `state`, `installation_id`, `setup_action` … all survive), with the
signed handoff parameters appended. A caller-supplied `fh_*` parameter is
overwritten by the directory's own, never merged.

### Single source of truth for cells

`account_regions` maps `(provider, account_key) → home_region` and **nothing
else**. There is deliberately no `cell_base_url` column: region → cell base URL
resolves *exclusively* from the env config, so a cell can be re-pointed by
redeploying configuration rather than migrating data, and a cell endpoint is
defined in exactly one place.

### Fail closed

- A region not in `FISHHAWK_DIRECTORY_SUPPORTED_REGIONS` is rejected at
  onboarding **before any write**.
- An account whose recorded `home_region` has no configured cell gets an
  explicit `503` naming the region — **never** a fall-through to another
  region's cell.
- An absent, unknown, already-consumed, or expired install-state nonce is
  rejected; the directory never guesses a cell.
- Startup aborts on any incomplete routing configuration (see below).

### Handoff trust

`directory/pkg/handoff` is the **one codec** for the region pin. The directory
signs

```
provider, account_key, home_region, expires_at, nonce
```

with **HMAC-SHA256** over a canonically-ordered string
(`url.Values.Encode()` — sorted keys, percent-escaped values, so no value can
inject a separator), using a secret shared with every cell via environment
config. The cell verifies signature and expiry and rejects unsigned, forged,
tampered, and expired pins.

The package is stdlib-only and public (`pkg/`, not `internal/`) precisely so the
backend imports the *same* encoder and decoder — there is no second
serialization to drift.

**The signature is not a replay defence on its own.** A signed pin can be
replayed until it expires. The *replay bound* lives in the cell: it accepts a
pin only when the account's `home_region` is currently `NULL` or already equals
the incoming value (first-write-wins), so a replayed pin can never move an
account's region. The directory's own `AssignRegion` is first-write-wins for the
same reason.

### Install-state nonce

`install_states` holds a single-use nonce minted when onboarding starts and
consumed when the forge's App-install callback returns, binding the callback to
the account the directory already assigned. `ConsumeInstallState` is a
`DELETE … RETURNING`, so consumption is single-use even under concurrency;
expired rows are consumed too, so a stale nonce cannot be retried.

## Routes

| Path | Purpose |
|---|---|
| `GET /v0/onboarding/start` | Assign a region from **explicit** `region` input validated against the supported list, record the row, mint an install-state nonce, then 302 into the cell. |
| `GET /v0/install/callback` | Consume the install-state nonce carried back as `state`, then 302 into the account's cell. |
| `GET /v0/login` | Look up the account's recorded region and 302 into that cell. Never assigns. |
| `GET /healthz` | Liveness + the configured region list. |

Region **discovery** (e.g. reading an enterprise's GHEC data-residency region)
is out of scope: the region arrives as explicit input at
`/v0/onboarding/start`.

### Trust assumption: who may call `/v0/onboarding/start`

`GET /v0/onboarding/start` is **unauthenticated**, and region assignment is
first-write-wins with **no move path**. Those two facts compose into a
residency-squatting exposure: anyone who can reach the directory can
pre-register an arbitrary `(provider, account_key)` — a victim enterprise that
has not onboarded yet — and permanently pin it to a region of their choosing,
and the resulting signed handoff creates the account row on that cell without
any forge-verified identity. Nothing recovers from that except operating on the
directory database by hand.

So the deployment topology carries the access control this endpoint does not:
**the directory MUST NOT be exposed to untrusted networks.** Reachability of
`/v0/onboarding/start` is the trust boundary — confine it to an operator
network (or an authenticating reverse proxy) until the endpoint itself is gated
on an operator credential or a forge-verified identity. Do not run it on the
public internet as shipped. `/v0/login` and `/v0/install/callback` never assign
a region, so they do not carry this exposure.

## Configuration

| Variable | Required | Meaning |
|---|---|---|
| `FISHHAWK_DIRECTORY_DATABASE_URL` | yes | Postgres URL for the directory database. |
| `FISHHAWK_DIRECTORY_SUPPORTED_REGIONS` | yes | Comma-separated region list, e.g. `us,eu,au`. |
| `FISHHAWK_DIRECTORY_CELL_BASE_URLS` | yes | Comma-separated `region=url` pairs. Every supported region needs one; a URL for an unsupported region is an error. |
| `FISHHAWK_DIRECTORY_HANDOFF_SECRET` | yes | HMAC secret shared with every cell. |
| `FISHHAWK_DIRECTORY_HANDOFF_TTL` | no | Region-pin lifetime (default `2m`). |
| `FISHHAWK_DIRECTORY_ADDR` | no | Listen address (default `:8090`); `--addr` overrides. |

The comma-split shape follows the `FISHHAWKD_IMPLEMENT_ALLOWED_MODELS`
precedent. Every listed failure above aborts `serve` at startup.

```sh
fishhawk-directory migrate up
fishhawk-directory serve --addr :8090
```

## Tests

`internal/routing` and `pkg/handoff` are pure and need no container.

`internal/store` is Postgres-backed. This module **cannot** import
`backend/internal/pgtest` (Go `internal/` visibility forbids it), so
`store_test.go` carries its own minimal harness. It **attaches to the same
shared `fishhawk-test-postgres` container** by testcontainers
`WithReuseByName`, mirroring pgtest's attach-retry contract — retry on the
first-start name conflict (HTTP 409) and on a stale reuse reference (docker
`No such container`, a daemon-evicted container) — so the two harnesses can race
for the shared container without either failing. It deliberately does *not*
terminate the container: `scripts/test`'s lease-refcounted `EXIT` trap reaps it.

Unlike pgtest there is no `TEMPLATE` database — the directory schema is one tiny
migration, so each test creates an empty database and migrates it, sidestepping
template contention entirely. Docker-unavailable skips; `FISHHAWK_SKIP_INTEGRATION`
skips.
