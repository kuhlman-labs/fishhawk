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
belong per-installation. This child owns only column **location**; endpoint
**resolution** (reading `installations.forge_base_url`, threading it through the
OAuth/App clients + GitLab endpoints) is deferred to E44.2 (#1826).
