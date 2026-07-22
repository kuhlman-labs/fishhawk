# directory

The Fishhawk **directory plane** (ADR-062, E44.7 / #1831). A minimal control
plane that answers exactly one question — *which region owns this account?* —
and routes the caller to that region's cell.

It is deliberately the smallest thing that can be globally shared: no run
state, no tenant data, no inference. Everything else is per-region and lives
in a cell (`backend/`).

## Layout

| Path | Contract |
|---|---|
| `pkg/handoff/` | The **single owning** codec for the signed handoff parameter set. Public because both planes import it. |
| `internal/store/` | The directory's whole persistent state: `(provider, account_key) -> home_region`. |
| `internal/store/migrations/` | Embedded golang-migrate SQL, applied by `MigrateUp`. |

`pkg/routing/` and `cmd/fishhawk-directory/` land in sibling slices of the
same plan; this module currently ships the codec and the store.

## `pkg/handoff` — the signed handoff

The directory appends an `fh_*` parameter set to the redirect it issues; the
cell verifies it on arrival. Both sides call this package, so signer and
verifier cannot drift (ADR-062 A2.6).

Parameters: `fh_provider`, `fh_account_key`, `fh_region`, `fh_expires_at`
(RFC3339), `fh_nonce`, `fh_sig`.

- **MAC**: HMAC-SHA256 over a *length-prefixed* canonical serialization of the
  five fields in fixed order (`<len>:<value>` segments), compared with
  `hmac.Equal`. Length prefixing makes the encoding injective — a naive
  separator-joined concatenation would let an account key containing the
  separator collide with a different field split under the same MAC, which is
  a cross-account authentication bug, not a cosmetic one.
- **Order of checks in `Verify`**: presence → well-formedness → signature →
  expiry, so an unsigned guess learns nothing about the expiry window.
- **No unsigned mode**: an empty secret is `ErrNoSecret` at *both* `Sign` and
  `Verify`. There is no configuration in which an unsigned request passes.
- **`Has(q)`** distinguishes "no handoff at all — pass through, single-cell
  deployment" from "handoff present but invalid — refuse".

### The nonce is a binder, not a ticket

`fh_nonce` and `fh_expires_at` bind each handoff to one issuance. The nonce is
**not** consumed against a store, and this module deliberately ships no
nonce/install-state table: nothing on the cell side could read it (Go's
internal-package rule stops `internal/store` at the module boundary, and
cell → directory write-back is out of scope by ADR-062), so such a table
would have a writer and no reader.

Replay safety comes from the cell instead: the cell stamps `home_region` with
a **conditional, first-write-wins UPDATE**. Replaying an unexpired handoff
verbatim is therefore a harmless no-op, and a handoff naming a *different*
region for an already-pinned account is refused by the SQL predicate rather
than by a Go check-then-act.

## `internal/store` — first-write-wins by construction

One table, `account_regions`, keyed by `(provider, account_key)`.

`AssignRegion` is a single statement:

```sql
INSERT INTO account_regions (provider, account_key, home_region)
VALUES ($1, $2, $3)
ON CONFLICT (provider, account_key)
DO UPDATE SET home_region = account_regions.home_region
RETURNING home_region
```

The returned region is the region that **actually owns** the account — the
first assignment ever made, not necessarily the one proposed. Callers must
compare it against their proposal; a difference is a normal outcome (someone
else won), not an error. `DO UPDATE ... = account_regions.home_region` rather
than `DO NOTHING` because `DO NOTHING` suppresses `RETURNING` for the
conflicting row, leaving the losing caller with no answer at all.

Postgres takes a row lock on the conflicting row, so concurrent callers
serialize and every one of them reads back the same winner. That property is
pinned by a 16-goroutine `-race` test, not argued in prose.

Region → cell base URL is **not** stored here. It resolves from the
directory's env config, so a cell can be re-homed without a migration and a
stale row can never point traffic at a dead cell.

`ErrNotFound` (no assignment) and `ErrInvalidInput` (empty provider, key, or
region) are the only typed errors.

## Test harness

`internal/store` runs its **own** Postgres container, not the shared
`fishhawk-test-postgres`. The directory module cannot import
`backend/internal/pgtest`, and attaching to the shared container by
reuse+name would mean re-implementing pgtest's entire hardening ladder
(first-start name-conflict attach-retry, stale-reuse re-create for a
daemon-evicted container, advisory-locked cross-process template bootstrap).
A naive `WithReuse` + `WithName` bootstrap is precisely the flake source
#1174 removed, so it is not shipped here.

Exactly one package in this module needs Postgres, so the container is
started once in `TestMain`, given a random name (no cross-invocation
collision), and **terminated explicitly** — `scripts/test` disables ryuk and
only reaps the shared container by name, so an unterminated container here
would leak. Each test gets its own freshly-migrated throwaway database.

`FISHHAWK_SKIP_INTEGRATION` and an absent Docker daemon both skip.

## Adding this module to a build

`directory/` is in `/go.work`, so `scripts/test` picks it up automatically.
It is also wired into both COPY phases of `backend/Dockerfile`: every module
listed in `go.work` must have its `go.mod` present in the image before
`go mod download`, or the build fails with `cannot load module ../directory`
(AGENTS.md "Adding a Go module"; #733 / #672).
