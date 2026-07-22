# Regional cells

How Fishhawk realizes ADR-057 Approach D (data residency) as **regional cells**
fronted by a small **global directory**, and what an operator has to configure
to run one.

Anchors: ADR-062 (#2099), E44.7 / #1831. Component contracts live next to the
code — `directory/README.md` (the directory service),
`backend/internal/account/README.md` (the cell-side region pin),
`backend/cmd/fishhawkd/README.md` (region-scoped inference config).

## Shape

A **cell** is a complete, self-contained Fishhawk deployment — its own
`fishhawkd`, its own Postgres, its own trace bucket, its own model endpoint —
that serves exactly one region. Customer data never leaves its cell.

The **directory** (`fishhawk-directory`) is the only non-regional component. It
holds no customer data: its entire database is `(provider, account_key) →
home_region` plus a single-use install-state nonce table. Its job is to answer
one question — *which cell owns this account?* — and to send the browser there.

```
                     ┌───────────────────────────┐
  browser ──GET──▶   │  fishhawk-directory       │   (global, no customer data)
                     │  (provider, account_key)  │
                     │        → home_region      │
                     └───────────┬───────────────┘
                                 │ 302 Found, Location =
                                 │   cell_base_url + original path + original
                                 │   query + signed handoff params
                                 ▼
        ┌────────────────┐  ┌────────────────┐  ┌────────────────┐
        │  us cell       │  │  eu cell       │  │  au cell       │
        │  fishhawkd     │  │  fishhawkd     │  │  fishhawkd     │
        │  Postgres      │  │  Postgres      │  │  Postgres      │
        │  trace bucket  │  │  trace bucket  │  │  trace bucket  │
        │  model endpoint│  │  model endpoint│  │  model endpoint│
        └────────────────┘  └────────────────┘  └────────────────┘
```

### Routing is a redirect, never a proxy

The directory answers `302 Found` (RFC 9110 §15.4.3). The browser re-issues the
request at the cell, so **no request body ever transits the global plane** — the
OAuth `code` and App-install credentials go straight to the region that will
hold the data.

Every routed surface is **GET-only by construction**: the directory mounts
`GET`-qualified patterns (`GET /v0/login`, …) and answers `405` otherwise, and
it never reads a request body. The classic "302 rewrites a POST to a GET" hazard
is therefore moot here — there is no POST to rewrite.

The redirect **preserves the request**. The `Location` is the resolved cell base
URL joined with the *original request path* and the *full original query
string* — `code`, `state`, `installation_id`, `setup_action` all survive — with
the signed handoff parameters appended. A caller-supplied `fh_*` parameter is
overwritten by the directory's own, never merged.

### Onboarding is directory-first

1. The operator (or the sign-up surface) calls
   `GET /v0/onboarding/start?provider=…&account_key=…&region=…`. The region is
   **explicit input**, validated against the configured supported-region list.
   Region *discovery* — e.g. reading an enterprise's GHEC data-residency
   region — is deliberately out of scope for this iteration.
2. The directory records `(provider, account_key) → home_region`
   (first-write-wins), mints a single-use install-state nonce, and 302s into
   that region's cell with a signed region pin appended.
3. The cell's `GET /v0/onboarding/region-pin` verifies the pin and stamps
   `accounts.home_region`. The cell is authoritative-on-write for its own
   `accounts` row and nothing more: it never derives a region itself and never
   writes back to the directory.

Thereafter `GET /v0/login` looks the account up and redirects; it never assigns.
An account with no recorded region fails closed with `404` — "onboard first".

### Single source of truth for cell endpoints

The directory store maps `(provider, account_key) → home_region` and **nothing
else**. There is deliberately no per-account `cell_base_url` column: `region →
cell base URL` resolves *exclusively* from environment configuration. A cell can
be re-pointed by redeploying config rather than migrating rows, and a cell
endpoint is defined in exactly one place.

## Handoff trust

`directory/pkg/handoff` is the **one codec** for a region pin — a public,
stdlib-only package that the backend imports for cell-side validation, so there
is no second serialization to drift. The directory signs

| Parameter | Meaning |
|---|---|
| `fh_provider` | forge provider (`github`, `gitlab`) |
| `fh_account_key` | forge-neutral account key |
| `fh_home_region` | the region the directory assigned |
| `fh_expires_at` | Unix-seconds expiry (short TTL) |
| `fh_nonce` | per-redirect nonce |
| `fh_sig` | HMAC-SHA256 over the canonically-ordered payload |

The canonical string is `url.Values.Encode()` — sorted keys, percent-escaped
values — so no value can inject a separator. The secret is shared with every
cell through environment config.

The cell rejects, fail-closed and each with its own error code:

| Rejection | Cell response |
|---|---|
| Missing / malformed handoff parameters | `400 validation_failed` |
| Forged or tampered signature | `403 region_pin_rejected` |
| Expired pin | `403 region_pin_rejected` |
| Pin for a region this cell does not serve | `421 region_pin_misdirected` |
| Account already pinned to a *different* region | `409 region_pin_conflict` |
| Region outside the cell's supported set | `400 validation_failed` |
| No account store / no handoff secret wired | `503 region_pin_unavailable` |

### The replay bound

A signature is **not** a replay defence on its own: a signed pin can be replayed
until it expires. The replay bound lives in the cell — it accepts a pin only
when `accounts.home_region` is currently `NULL` or already equals the incoming
value (**first-write-wins**). A replayed or re-issued pin is therefore
idempotent and can never move an account between regions. The directory's own
`AssignRegion` is first-write-wins for the same reason.

### The residency invariant

The cell also rejects any pin whose `home_region` differs from **its own**
configured region (`FISHHAWKD_HOME_REGION`). A validly signed EU pin that
reaches a US cell is a routing fault, and honoring it would place EU data in the
US, so it fails closed with `421 Misdirected Request` rather than being written.

## Region-scoped inference

Model selection is **per-cell and process-level**, not per-account. Each cell
reads its own region's Messages endpoint and reviewer credential from
environment config; `anthropic.Config.BaseURL` carries the endpoint onto the
reviewer's Messages client. There is no per-account `region → endpoint` lookup
inside a cell — that would be a different design and needs its own ADR.

A cell that declares a home region but has no in-region model endpoint (or no
credential for it) **aborts at startup** rather than silently serving inference
out of region.

## Configuration

### Directory (`fishhawk-directory`)

| Variable | Required | Meaning |
|---|---|---|
| `FISHHAWK_DIRECTORY_DATABASE_URL` | yes | Postgres URL for the directory database. |
| `FISHHAWK_DIRECTORY_SUPPORTED_REGIONS` | yes | Comma-separated region list, e.g. `us,eu,au`. |
| `FISHHAWK_DIRECTORY_CELL_BASE_URLS` | yes | Comma-separated `region=url` pairs; every supported region needs one. |
| `FISHHAWK_DIRECTORY_HANDOFF_SECRET` | yes | HMAC secret shared with every cell. |
| `FISHHAWK_DIRECTORY_HANDOFF_TTL` | no | Region-pin lifetime (default `2m`). |
| `FISHHAWK_DIRECTORY_ADDR` | no | Listen address (default `:8090`). |

Startup aborts on any incomplete routing configuration: no supported regions, no
cell base URLs, a malformed `region=url` pair, a URL for an unsupported region,
a supported region with no URL, a non-absolute `http(s)` URL, no handoff secret,
or a non-positive TTL.

At request time, an account whose recorded region has no configured cell gets an
explicit `503` naming the region — **never** a fall-through to another region's
cell.

**Trust boundary: do not expose the directory to untrusted networks.**
`GET /v0/onboarding/start` is unauthenticated and region assignment is
first-write-wins with no move path, so anyone who can reach the directory can
pre-register an arbitrary `(provider, account_key)` for an enterprise that has
not onboarded yet and permanently pin it to a region of their choosing — and the
signed handoff that results creates the account row on that cell with no
forge-verified identity behind it. There is no recovery path short of editing
the directory database by hand. Reachability is therefore the access control:
confine the directory to an operator network (or front it with an
authenticating proxy) until the endpoint is gated on an operator credential or a
forge-verified identity. `/v0/login` and `/v0/install/callback` never assign a
region and do not carry this exposure.

### Cell (`fishhawkd`)

| Variable | Meaning |
|---|---|
| `FISHHAWKD_HOME_REGION` | This cell's region tag (`us` / `eu` / `au`). Empty = unregionalized single-cell deployment. |
| `FISHHAWKD_ANTHROPIC_BASE_URL` | The in-region Messages endpoint. Required whenever `FISHHAWKD_HOME_REGION` is set. |
| `FISHHAWKD_ANTHROPIC_API_KEY` | The region's reviewer credential. Required whenever `FISHHAWKD_HOME_REGION` is set. |
| `FISHHAWKD_DATABASE_URL` | This cell's own Postgres. One database per cell — never shared across regions. |
| `FISHHAWKD_S3_BUCKET` | This cell's own trace bucket (see §5.2 of `docs/ARCHITECTURE.md`: bucket-per-region). |
| `FISHHAWKD_HANDOFF_SECRET` | HMAC secret shared with the directory. MUST equal `FISHHAWK_DIRECTORY_HANDOFF_SECRET`. Empty leaves `GET /v0/onboarding/region-pin` failing closed with `503 region_pin_unavailable`. |

`fishhawkd`'s `serve.go` reads `FISHHAWKD_HANDOFF_SECRET` and injects it,
together with the accounts store bound to `FISHHAWKD_HOME_REGION`, through
`server.(*Server).ConfigureRegionPin`. Both are required: with no database pool
or no secret the route stays failing-closed at `503 region_pin_unavailable`,
which is the correct posture for a deployment that is not part of a regional
topology. A cell whose secret does not match the directory's rejects every pin
with `403 region_pin_rejected` — a secret mismatch fails closed, it never
degrades to trusting the parameters.

An empty `FISHHAWKD_HOME_REGION` is the untenanted, single-region posture: the
residency self-check is disabled (every cell is the only cell) and inference
config is unconstrained. Full per-variable contract:
`backend/cmd/fishhawkd/README.md`.

## Rollout worked example (`us` + `eu`)

```sh
# directory (global)
FISHHAWK_DIRECTORY_SUPPORTED_REGIONS=us,eu
FISHHAWK_DIRECTORY_CELL_BASE_URLS=us=https://us.fishhawk.example,eu=https://eu.fishhawk.example
FISHHAWK_DIRECTORY_HANDOFF_SECRET=<shared secret>

# us cell
FISHHAWKD_HOME_REGION=us
FISHHAWKD_ANTHROPIC_BASE_URL=https://api.anthropic.com
FISHHAWKD_S3_BUCKET=fishhawk-traces-prod-us
FISHHAWKD_HANDOFF_SECRET=<the same shared secret>

# eu cell
FISHHAWKD_HOME_REGION=eu
FISHHAWKD_ANTHROPIC_BASE_URL=https://eu.api.example
FISHHAWKD_S3_BUCKET=fishhawk-traces-prod-eu
FISHHAWKD_HANDOFF_SECRET=<the same shared secret>
```

Add a region by (1) standing up the cell with its own Postgres, bucket, and
model endpoint, (2) appending it to both directory lists, (3) redeploying the
directory. Existing accounts are untouched — their recorded region does not
change, and first-write-wins means it cannot be moved by a later pin.

## Deliberately out of scope

The following are **human-led follow-ups**, not part of E44.7:

- **Per-region deploy topology** — `deploy/helm/fishhawk/` chart changes,
  per-region `values-*.yaml`, and the Kubernetes manifests for a multi-cell
  install (`docs/deploy/kubernetes.md` covers the single-cell local path only).
- **`.github/workflows/**` per-region build/release wiring** — agent workflows
  cannot author `.github/workflows/**`.
- **The directory's own container image** — there is no `directory/Dockerfile`
  and no release job for `fishhawk-directory` yet; it builds from source.
- **Region discovery** — deriving an enterprise's GHEC data-residency region
  instead of taking it as explicit onboarding input.
- **Moving an account between regions** — no migration path exists, and
  first-write-wins deliberately forbids it in place.
- **Authenticating `/v0/onboarding/start`** — the endpoint takes the region as
  explicit input with no operator credential or forge-verified identity behind
  it. Until that lands, network reachability is the access control (see the
  trust-boundary note under *Directory* above).
