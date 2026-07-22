# Hosted regional (Mode 2): operator runbook

ADR-057's **Mode 2** — the multi-tenant hosted service, many customer accounts
across N regional cells behind one global directory. For **Mode 1** (one
customer, own perimeter, single-tenant profile) see
[self-hosted.md](self-hosted.md).

This page is **procedural**: the order operations happen in and what an operator
does when one fails. The topology, the handoff protocol, the full env matrix,
and the fail-closed response table are specified once in
[regional-cells.md](regional-cells.md) — read that first and treat it as
normative; nothing here restates it.

## Prerequisites

- One directory database and one Postgres per cell. Cells share nothing.
- One handoff secret, shared by the directory and **every** cell.
- A `region → cell_base_url` map. There is no service discovery and no default
  cell.

## Bring-up order

Directory first, then cells. A cell brought up before the directory is
harmless (it serves ordinary traffic; only routed `fh_*` requests need the
handoff), but a directory brought up before its cells will 302 callers at URLs
that answer nothing.

1. **Directory.** Set `FISHHAWK_DIRECTORY_DATABASE_URL`,
   `FISHHAWK_DIRECTORY_REGIONS` (`us=https://us.example.com,eu=…`),
   `FISHHAWK_DIRECTORY_HANDOFF_SECRET`, `FISHHAWK_DIRECTORY_ADMIN_TOKEN`, then
   `fishhawk-directory serve`. It validates config and migrates its own
   database at startup; an unparsable region map, a non-absolute cell URL, a
   missing secret, or an unsupported entry in
   `FISHHAWK_DIRECTORY_ROUTED_PATHS` fails startup naming the variable.
2. **Each cell.** Set `FISHHAWKD_HOME_REGION` to that cell's region and
   `FISHHAWKD_HANDOFF_SECRET` to the shared secret (plus the usual
   `FISHHAWKD_DATABASE_URL` and forge/OAuth config), then deploy `fishhawkd`.
   Confirm the startup log reports the region-pin surface **enabled**; with
   either value missing it logs *disabled* and every routed request is refused
   503.
3. **Do not set any `FISHHAWKD_SINGLE_TENANT_*` variable on a cell.** Mode 2 is
   multi-tenant; a cell that bootstrapped an implicit account would hold an
   account nobody assigned it.

## Adding a region

1. Stand the new cell up (step 2 above) and verify `/healthz`.
2. Add `region=url` to `FISHHAWK_DIRECTORY_REGIONS` and restart the directory.
   The map is read at startup only.
3. Only then home accounts there. Assigning an account to a region absent from
   the map leaves it reachable only through a 500 naming the missing config —
   never a redirect to some other region.

Removing a region is the reverse and is **not** safe while accounts are still
homed there: re-homing is not an operation the pin supports (it is
first-write-wins in SQL), so migrate the accounts' data and delete their
directory rows first.

## Homing an account, and verifying the pin

Assignment is operator-gated at the directory; the pin itself is written by the
cell on the first routed request.

```sh
curl -sS -X POST https://directory.example.com/v0/directory/assign \
  -H "Authorization: Bearer $FISHHAWK_DIRECTORY_ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"provider":"github","account_key":"acme","region":"eu"}'
```

The `Bearer` scheme is required — a bare `Authorization: <token>` is 401 even
with the right token.

Then send the account through the routed onboarding surface and confirm the
cell stamped it:

```sh
curl -sS -i -H "Authorization: Bearer $FISHHAWK_DIRECTORY_ADMIN_TOKEN" \
  "https://directory.example.com/v0/onboarding/start?provider=github&account_key=acme"
# → 302 to <eu cell>/v0/onboarding/start?…&fh_provider=…&fh_sig=…

# on the eu cell's database:
psql "$EU_DATABASE_URL" -c \
  "SELECT account_key, home_region FROM accounts WHERE provider='github' AND account_key='acme'"
```

`home_region` empty after a 200 means the request never carried a handoff (it
did not come through the directory). The pin is UPDATE-only: an account the
cell has no row for is refused **404**, never created.

## Rotating the handoff secret

The secret is a single shared HMAC key with no key-id in the parameter set, so
rotation is a **brief coordinated cutover**, not a rolling one:

1. Schedule a window. Routed onboarding requests issued with the old secret and
   verified with the new one are refused 403 (signature) — ordinary traffic
   carrying no `fh_*` parameters is unaffected throughout.
2. Roll every cell to the new secret, then the directory. Doing the directory
   first would sign handoffs no cell can verify.
3. Handoffs are short-lived (`FISHHAWK_DIRECTORY_HANDOFF_TTL`, default 5m), so
   in-flight redirects drain within one TTL. Re-issuing an onboarding request
   after the window is the recovery for any that did not.

Adding a key-id and overlapping keys would make this rolling; it is not in the
protocol today.

## Per-cell health checks

| Check | Expectation |
|---|---|
| `GET <cell>/healthz` | 200; `git_sha` matches the intended build, `schemas` carries both embedded workflow-schema hashes |
| Cell startup log | region-pin surface **enabled**, naming the home region |
| Directory startup log | region map parsed, one entry per live cell |
| A routed request for a known account | 302 to that account's cell, then 200 there |

## Failures an operator hits, and what they mean

Full table in
[regional-cells.md](regional-cells.md#fail-closed-postures). The four that map
to an operator action:

| Symptom | Cause | Action |
|---|---|---|
| Startup fails naming a `FISHHAWK_DIRECTORY_*` variable | unconfigured region map / secret / routed path | fix the variable; the directory never starts half-configured |
| 503 `region_pin_disabled` from a cell | that cell has no `FISHHAWKD_HOME_REGION` or no `FISHHAWKD_HANDOFF_SECRET` | set both and redeploy the cell |
| 409 `region_mismatch` / `region_conflict` | the handoff names another cell's region, or the account is already homed elsewhere | do not "fix" by re-pinning — first-write-wins is enforced in SQL; correct the directory assignment |
| 404 on a routed request | the cell has no row for that account | the account has never onboarded on this cell; the pin never creates one |
| 500 naming a missing region config | the account is homed in a region absent from `FISHHAWK_DIRECTORY_REGIONS` | add the region to the map and restart the directory |

## Region-scoped inference

Per-cell and process-level: set `FISHHAWKD_MODEL_BASE_URL` and
`FISHHAWKD_MODEL_API_KEY` **together** on each cell, or neither. Setting one
without the other fails closed in the two asymmetric ways described in
[regional-cells.md](regional-cells.md#region-scoped-inference) — the
endpoint-without-key case makes calls fail, the key-without-endpoint case
withholds the SDK adapter entirely rather than egress the region's credential
and the review text to the SDK's global default endpoint.
