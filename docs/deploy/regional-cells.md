# Regional cells

Two-plane topology for data residency (ADR-062, E44.7 / #1831). One globally
shared **directory plane** answers *which region owns this account?* and routes
the caller there; N independent **cell planes** hold everything else — runs,
tenant data, artifacts, and inference.

The directory holds no run state, no tenant data, and performs no inference, so
nothing regulated crosses a region boundary through it.

## The two planes

| Plane | Binary | Holds | Count |
|---|---|---|---|
| Directory | `fishhawk-directory` (`directory/`) | `(provider, account_key) → home_region` and nothing else | one, global |
| Cell | `fishhawkd` (`backend/`) | runs, stages, audit, artifacts, accounts, inference | one per region |

`region → cell_base_url` resolves **exclusively** from the directory's env
config. There is no service discovery and no default cell: a region the
directory has no URL for is a startup error, or — if an account is already
homed there — a 500 naming the missing config, never a redirect to some other
region.

## Request flow

1. The caller hits the directory at a **routed path** carrying explicit account
   identity: `GET /v0/onboarding/start?provider=github&account_key=acme`.
2. The directory looks the account's region up and answers **302** to
   `<cell_base_url><original path><original query>` with the signed handoff
   parameters appended. The original path and the caller's own query parameters
   survive byte-for-byte.
3. The cell's `withRegionPin` middleware — mounted on the routed path *itself*,
   so the redirect target and the verifier cannot drift apart — verifies the
   handoff and stamps `accounts.home_region`, then serves the handler.

The handoff parameter set is `fh_provider`, `fh_account_key`, `fh_region`,
`fh_expires_at`, `fh_nonce`, `fh_sig`; the MAC is HMAC-SHA256 over a
length-prefixed canonical serialization. Both planes call the one owning codec
(`directory/pkg/handoff`), so signer and verifier are the same code.

## Environment matrix

### Directory plane (`fishhawk-directory serve`)

| Env var | Required | Meaning |
|---|---|---|
| `FISHHAWK_DIRECTORY_DATABASE_URL` | yes | the directory's own Postgres |
| `FISHHAWK_DIRECTORY_REGIONS` | yes | `region=url,…`, e.g. `us=https://us.example.com,eu=https://eu.example.com` |
| `FISHHAWK_DIRECTORY_HANDOFF_SECRET` | yes | HMAC key shared with every cell |
| `FISHHAWK_DIRECTORY_ADMIN_TOKEN` | yes | operator credential; **unset refuses every surface** |
| `FISHHAWK_DIRECTORY_ROUTED_PATHS` | no | default `/v0/onboarding/start`; a path outside the supported set (today that one surface) is a **startup error** — see below |
| `FISHHAWK_DIRECTORY_HANDOFF_TTL` | no | default 5m |
| `FISHHAWK_DIRECTORY_ADDR` | no | default `:8081` |

### Cell plane (`fishhawkd serve`)

| Env var | Flag | Meaning | Empty means |
|---|---|---|---|
| `FISHHAWKD_HOME_REGION` | `--home-region` | the region this cell serves | pin surface **disabled** |
| `FISHHAWKD_HANDOFF_SECRET` | `--handoff-secret` | must equal the directory's `FISHHAWK_DIRECTORY_HANDOFF_SECRET` | pin surface **disabled** |
| `FISHHAWKD_MODEL_BASE_URL` | `--model-base-url` | in-region inference endpoint | SDK default (`api.anthropic.com`) |
| `FISHHAWKD_MODEL_API_KEY` | `--model-api-key` | credential for that endpoint | falls back to `FISHHAWKD_ANTHROPIC_API_KEY` **only when `FISHHAWKD_MODEL_BASE_URL` is also empty** |

`FISHHAWKD_DATABASE_URL` is also a precondition for the pin surface — without a
database there is no `accounts` row to stamp.

## Fail-closed postures

Every one of these is a refusal, never a degrade-to-permissive.

| Configuration | Behavior |
|---|---|
| Directory `ADMIN_TOKEN` unset | **both** the assign endpoint and the routed onboarding GET answer 503. An unset credential opens nothing. |
| Directory region map empty / unparsable / a non-absolute cell URL / no handoff secret | startup fails with a message naming the env var. |
| Account homed in a region absent from the region map | 500 naming the missing config. Never a default cell. |
| Cell `HOME_REGION` unset (secret set) | a request carrying `fh_*` is refused 503 `region_pin_disabled`. The middleware is mounted in this posture precisely so the refusal happens — not mounting it would silently bypass. |
| Cell `HANDOFF_SECRET` unset (region set) | same 503. A cell that cannot verify a residency claim must not serve the routed surface as though the claim were absent. |
| Handoff signature bad / expired / malformed | 403 / 403 / 400. Never passed through unpinned. |
| Handoff names a region other than this cell's | 409 `region_mismatch`. |
| Account already homed in a different region | 409 `region_conflict`. The pin is a first-write-wins conditional UPDATE, so this holds in SQL rather than check-then-act in Go. |
| Handoff for an account this cell does not have | 404. The pin is **UPDATE-only**: it never inserts an account row. |
| Request carries no `fh_*` parameters | passes through untouched. A single-cell deployment behaves identically with or without any of these env vars. |
| `FISHHAWK_DIRECTORY_ROUTED_PATHS` names a path the cell does not verify | startup fails naming the supported set. The routed-path list is a **closed set** kept in lockstep with the cell surfaces `withRegionPin` is mounted on; routing anything else would deliver a signed redirect to an endpoint that verifies no handoff and pins no account. |
| Cell `MODEL_BASE_URL` set, `MODEL_API_KEY` unset | the anthropic reviewer presents **no** credential (its calls fail) and startup warns. `FISHHAWKD_ANTHROPIC_API_KEY` is deliberately never sent to a non-default endpoint — that would ship a production secret, and the review text it authenticates, to an operator-supplied host. |
| Cell pin fails for an unclassified reason | 500 `region_pin_failed` with a **generic** message. The routed surface answers before any auth decision (the handoff is itself the credential), so a driver/query/host detail is logged, never returned. |

### Replay

A handoff is bound by its signed nonce and expiry, and the pin itself is the
replay bound: re-pinning an account to the region it already holds matches the
row and is a harmless no-op, while a tampered handoff naming a different region
is refused with the typed conflict. There is no consumed-nonce store — a table
whose only reader would be a cell→directory write-back that ADR-062 puts out of
scope.

## Deferred: routing the OAuth login/callback pair

Only `/v0/onboarding/start` is routed today, because it is the only surface that
carries explicit account identity in its query. The OAuth pair
(`/v0/auth/github/login`, `/v0/auth/github/callback`) is deliberately **not**
routed: a callback arrives from the forge with `code` + `state` and no account
parameter, so the directory cannot resolve `(provider, account_key)` and would
have to guess — routing a caller's traffic on a coin flip. Pre-registering a
correlation does not work either, since the cell mints the OAuth `state` only
*after* the redirect. Routing that pair needs a correlation design that does not
exist yet; it is tracked as a follow-up.

`FISHHAWK_DIRECTORY_ROUTED_PATHS` cannot be used to route them anyway: the
directory validates each configured path against a closed set of cell surfaces
that mount `withRegionPin` (one entry today), and refuses to start otherwise.
Adding a routed surface is therefore a two-sided change — the cell mounts the
middleware, and the directory's supported set gains the path.

## Region-scoped inference

Inference stays per-cell and **process-level** (ADR-062 Q3(a)) — there is no
per-account endpoint registry. `FISHHAWKD_MODEL_BASE_URL` +
`FISHHAWKD_MODEL_API_KEY` are threaded into the Anthropic SDK client, so both
the plan-review and the implement-review call (one adapter, two prompt shapes)
target the cell's in-region endpoint with the region's own credential and the
review text never leaves the region.

Setting `FISHHAWKD_MODEL_BASE_URL` **without** `FISHHAWKD_MODEL_API_KEY` fails
closed: the adapter presents an empty credential rather than the deployment's
`FISHHAWKD_ANTHROPIC_API_KEY`, and startup warns. Sending the deployment key to
an operator-configured endpoint would combine a production secret with
configurable network egress; the endpoint refusing an uncredentialed call is the
safer failure.

This governs the Anthropic SDK adapter only. The `claudecode` and `codex`
reviewers are subprocesses whose endpoint is the CLI's own configuration; a
region-resident deployment using those adapters must constrain them there.

## Out of scope here

Per-region deploy topology — helm values, k8s manifests, CI workflows, and the
`fishhawk-directory` container image — is human-led and not part of this change.
