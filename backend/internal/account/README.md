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

## The auto-join intersection query is PAIR-WISE (E44.3, generalized in E44.8 / #1832)

`ListAutoJoinAccountsByKeys` takes TWO string arrays — `account_keys` and
`granularities` — that are **positionally paired**, `unnest`ed together and
joined against `accounts`, so index *i*'s key only ever matches index *i*'s
granularity. It is deliberately NOT
`account_key = ANY(keys) AND granularity = ANY(granularities)`: those are
independent predicates whose cartesian product would admit a user who is merely
an org member of "acme" into an `enterprise`-granularity account keyed "acme"
(and a derived enterprise short code into an `organization` account of the same
key) — unauthorized admission in the login gate. The caller
(`auth.MembershipResolver`) derives each key already bound to the granularity it
came from (`organization` / `enterprise` / `group`); see
`backend/internal/auth/README.md`.

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

## RegionPinner — the cell-side region pin (E44.7 / #1831, ADR-062)

`region.go` records which region owns an account, from a signed handoff the
regional directory issued. The directory plane owns `(provider, account_key) ->
region`; this type is the cell's local record of that assignment, so the cell's
own reads can answer "is this account mine?" without a directory round-trip.

`Pin(ctx, provider, accountKey, region)` refuses in this order, each with a
typed error the HTTP layer maps onto a status:

- **`ErrRegionDisabled`** — the cell has no configured home region (or no query
  surface). The pin surface is then disabled ENTIRELY (ADR-062 A2.4): a cell
  that does not know which region it is cannot honor a residency claim, so it
  refuses rather than stamping an unverifiable value. A **nil** `*RegionPinner`
  reports the same thing rather than panicking, which is why the fail-closed
  decision lives here and not in every caller.
- **`ErrInvalidPin`** — empty provider, account key, or region.
- **`ErrRegionMismatch`** — the handoff names a region OTHER than this cell's
  own. The residency self-check: a valid signature is not authority to record
  another region's account here.
- **`ErrUnknownAccount`** / **`ErrAlreadyPinned`** — the conditional UPDATE
  matched no row, disambiguated by a follow-up read.

**First-write-wins lives in SQL, not in Go.** `PinAccountHomeRegion` is

```sql
UPDATE accounts SET home_region = $3, updated_at = now()
 WHERE provider = $1 AND account_key = $2
   AND (home_region IS NULL OR home_region = $3)
RETURNING *;
```

The guard clause matches only a row that is unpinned or already pinned to the
SAME region, so two concurrent pins proposing different regions serialize on the
row lock and exactly one can match — there is no check-then-act window to race
(ADR-062 A2.3). `region_test.go` pins this with a four-goroutine concurrent test
under `-race` against real Postgres: exactly one winner, and the stored value
never moves afterwards.

The statement is **UPDATE-only on purpose**: it must never create an account. A
handoff naming an account this cell has never heard of matches no row and is
refused (`ErrUnknownAccount`), not silently materialized — otherwise a signed
handoff would be an account-creation primitive (ADR-062 A2.5).

Re-pinning an account to the region it already holds MATCHES the row and is a
no-op. That idempotence is what makes replay safe: the handoff's nonce and
expiry bind one issuance but are not consumed against a store, so replaying an
unexpired handoff verbatim changes nothing. See
`backend/internal/server/regionpin.go` for the HTTP middleware that verifies the
handoff before calling this, and `docs/deploy/regional-cells.md` for the
two-plane topology.
