# internal/account — tenancy identity persistence (E44.1)

The persistence surface for the ADR-057 / ADR-058 tenancy identity tables:
`accounts`, `installations`, and `account_members`. Stood up by migration
`0055` (#1825) on top of `0052`'s (#1854) `accounts` + `installations`
foundation.

This package **adds no reader or writer** into the server. It carries only the
sqlc surface (`accountdb`) — `Account` / `Installation` / `AccountMember` models
plus basic upsert/get queries — that later E44 children build on: endpoint
resolution (#1826), handler authz (#1829), and RLS (#1830). Like the other
`internal/*/db` packages, sqlc is **not regenerated locally** (established
convention); `db/*.go` is hand-written to match sqlc's output shape.

## The three identity tables

- **`accounts`** — one row per tenant forge account. Forge-neutral natural key
  `UNIQUE (provider, account_key)`; `UNIQUE (id, provider)` anchors the
  composite FKs below. `provider TEXT NOT NULL DEFAULT 'github'` with a CHECK
  admitting `('github','gitlab')`.
- **`installations`** — one row per credential scope. `installation_ref TEXT`
  is the forge-neutral credential-scope key. A composite
  `FOREIGN KEY (account_id, provider) REFERENCES accounts (id, provider)
  ON DELETE CASCADE` pins an installation's provider to its account's. Carries
  the relocated `forge_base_url` / `oauth_base_url` endpoint columns (see
  Amendment A1 below).
- **`account_members`** — forge-neutral membership grants, the login-gate source
  (materialized from GitHub Enterprise / GitLab group membership by a later
  child). `member_ref TEXT` is the member key; `role TEXT` is nullable.
  `UNIQUE (account_id, provider, member_ref)`, the same composite FK as
  installations with `ON DELETE CASCADE` (a grant has no meaning without its
  account), and a `BEFORE UPDATE` trigger reusing the shared
  `fishhawk_set_updated_at()` function from `0001`.

## account_id threading

Migration `0055` threads a **nullable** `account_id UUID` column through the
eight root entities — `runs`, `campaigns`, the four `refinement_*` tables,
`api_tokens`, `audit_entries` — each with a per-table
`<t>_account_id_fkey FOREIGN KEY (account_id) REFERENCES accounts (id)
ON DELETE SET NULL` and an index. `ON DELETE SET NULL` (not CASCADE): deleting
an account nulls the reference rather than erasing runs or audit history.

`account_id` is **nullable throughout** — isolation is not enforced here. RLS
predicates (#1830) and handler authz (#1829) land in later E44 children; a later
child tightens `account_id` to `NOT NULL` once every row is populated. The
`0055` backfill sets `runs.account_id` from the `installations` mapping
(`installation_id::text = installation_ref`) — a no-op today because no writer
populates `installations` yet, so nil-`installation_id` CLI/local runs stay
NULL, bound to the single implicit Mode-1 account by a later child.

## Amendment A1 — per-forge endpoints live on installations

ADR-057 Amendment A1: the per-forge endpoint columns `forge_base_url` /
`oauth_base_url` (NULL = provider default endpoints, api.github.com /
github.com today) were relocated by `0055` **from `accounts` to
`installations`**. A forge-agnostic workspace spanning both a github.com install
and a gitlab.com group cannot share one per-account base URL, so the endpoints
belong per-installation. `0055` owns only column **location**; endpoint
**resolution** lands in E44.2 (#1826, `endpoints.go`).

## EndpointResolver — the per-installation endpoint reader (E44.2 / #1826)

`endpoints.go` is the first production reader of the Amendment A1 columns.
`EndpointResolver.ResolveInstallationEndpoints(ctx, provider, installationRef)`
looks up the installation via `GetInstallationByRef` and returns its recorded
`(forge_base_url, oauth_base_url)`:

- **both columns SET** → `(forgeBaseURL, oauthBaseURL, nil)` — the data-resident
  override the caller routes its per-installation client to. NULL columns are
  honored independently (a set forge with a NULL oauth returns `("...", "")`).
- **NULL column / not-found row (`pgx.ErrNoRows`)** → the empty string with a
  `nil` error: the intentional absence of an override, so the caller keeps its
  deployment default. A `nil` resolver / `nil` getter reports the same
  no-override default without a query (the no-database posture).
- **a REAL DB error** → propagated (`("", "", err)`) so the caller **FAILS
  CLOSED**. An endpoint-resolution fault must never silently fall back to the
  default host for a data-resident install — only an intentional absence
  (NULL/not-found) falls back.

The GitHub App token-mint consumer lives in `serve.go`: it late-binds
`githubapp.Client.ResolveBaseURL` (after the DB pool exists) to a closure that
calls this resolver with `provider="github"` and the `installationRef` the
githubapp client hands it — the stringified numeric GitHub App installation id,
which is exactly `installations.installation_ref`. The int64 stays inside the
GitHub-specific githubapp package (which owns the id → ref stringification); the
serve.go closure is a thin forge-neutral passthrough. Per-installation
REST-client routing and per-installation GitLab-client construction (both
needing a per-installation client factory) build on this resolver as follow-ups.

## RegionPinner — the directory-decided home-region write path (E44.7 / #1831)

`region.go` is the cell-side write path for ADR-062 data residency. Onboarding
is **directory-first**: a global directory decides an account's home region,
records it, and 302s the caller into that region's cell with an HMAC-signed
handoff (`directory/pkg/handoff` — the one codec, imported by both sides). The
cell stamps `accounts.home_region` from that decision and does nothing else:

- it **never derives** the region in-cell, and
- it **never writes back** to the directory. The data flow is strictly
  directory → cell.

The write reuses the existing `UpsertAccount` query — `home_region` has existed
on `accounts` since migration `0052`, so **no migration is added**. Because that
query's `ON CONFLICT` clause overwrites `home_region` unconditionally, the
ordering guarantees live in Go, ahead of the statement.

`RegionPinner.Pin` enforces three gates, each fail-closed with no partial write:

- **Supported-region check** (`ErrRegionUnsupported`) — the region string
  crosses a trust boundary, so only the closed `SupportedRegions` set
  (`au`, `eu`, `us`; case-insensitive) is accepted. An unrecognized value is
  never persisted verbatim, where it would later resolve to no cell at all.
- **The residency invariant** (`ErrRegionForeign`) — the pin must name the
  region THIS cell serves (`FISHHAWKD_HOME_REGION`, passed to
  `NewRegionPinner`). A valid EU pin arriving at a US cell is a routing fault:
  honoring it would place EU data in the US. An **empty** cell tag disables the
  check — the single-region deployment where every cell is the only cell.
- **The replay bound** (`ErrRegionConflict`) — `home_region` is
  **first-write-wins**. A pin is accepted only when the column is currently
  `NULL` or already equals the incoming value, so replaying an old signed pin
  is idempotent and can never move an account between regions.

A real DB read fault propagates rather than falling through to a write (which
would create a duplicate account row on a transient error), and a nil querier
reports `ErrRegionUnavailable` — the no-database posture, not a panic. On the
existing-row path the account's `display_name` and `granularity` are carried
through unchanged, so a pin never clobbers them.

The HTTP surface is `GET /v0/onboarding/region-pin`
(`internal/server/onboarding.go`), which verifies the handoff signature and
expiry before calling `Pin`.
