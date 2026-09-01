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
came from (`organization` / `enterprise` / `group` / `user`); see
`backend/internal/auth/README.md`. The `user` tier (E44.35 / #2925) is the
personal-namespace pair `{login, "user"}` — bound to the login itself, so an
`organization` account keyed by that same login is NOT admitted by it, and a
`user` account is NOT admitted by an org key of the same name.

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

## Single-tenant deployment profile (ADR-057 Mode 1, E44.9 / #1833)

`singletenant.go` is the boot-time bootstrap that gives a self-hosted install
its ONE implicit tenant. It exists because the rest of the tenancy stack only
*reads* accounts: `auth.MembershipResolver` admits a sign-in against an existing
`accounts` row and denies when none matches, and nothing else creates the first
row — so without this a fresh install denies every sign-in with hand-written SQL
as the only remedy.

`EnsureSingleTenantAccount(ctx, q, cfg, logger)` upserts a single row through
`UpsertSingleTenantAccount`, the only query that writes `auto_join_role`. The
write is `ON CONFLICT (provider, account_key) DO UPDATE`, so every restart
converges the row on the configured profile without minting a second account,
and `home_region` is absent from BOTH the insert list and the update set —
`PinAccountHomeRegion` owns that column first-write-wins, and a boot-time upsert
re-running on every restart must never clear a pin
(`TestEnsureSingleTenantAccount_LeavesHomeRegionAlone`).

### Enablement is the account key, and only the account key

Every `FISHHAWKD_SINGLE_TENANT_*` flag defaults to EMPTY; the
`github` / `enterprise` / `member` defaults are applied INTERNALLY by
`resolveDefaults`, only after enablement. Making them flag defaults would force
a choice between enabling the profile on every hosted boot and making the
missing-key guard unreachable. The three states:

| Configuration | Behavior |
|---|---|
| Nothing set | `(uuid.Nil, false, nil)` — bootstrap skipped, no write, hosted behavior unchanged |
| Account key set | bootstrap runs; omitted fields filled from the internal defaults |
| Any other field set, key EMPTY | `ErrSingleTenantMissingAccountKey`, which the caller turns into a startup abort |

The third row is the load-bearing one. Reading a half-configured profile as
"hosted" boots a deployment with **no admitting account** — nobody can sign in,
and nothing says why. That is the failure this profile exists to prevent, so it
fails closed instead.

`SingleTenantConfig.Resolved()` is the exported, read-only **diagnostic** view
of the profile: it delegates verbatim to the internal `resolveDefaults`, so a
caller OUTSIDE this package (the server's no-admitting-account denial log,
#2468) can report the EFFECTIVE configuration — every field trimmed, the
`github` / `enterprise` / `member` defaults filled — rather than the raw, still
empty flag values. It is a view, not a predicate: `Enabled()` remains the SINGLE
enablement signal, and a caller must gate on `Enabled()` before reporting a
`Resolved()` view. Because both the diagnostic and `EnsureSingleTenantAccount`
resolve through the same `resolveDefaults`, they can never describe different
effective profiles.

### Fail-closed branches

| Branch | Result |
|---|---|
| Partial configuration (above) | `ErrSingleTenantMissingAccountKey` |
| Granularity outside `enterprise` / `organization` / `group` / `user` | error naming the flag + accepted set (mirrors `accounts_granularity_check`, so the operator sees a message instead of SQLSTATE 23514) |
| Provider outside `github` / `gitlab` | error naming the flag (mirrors `accounts_provider_check`) |
| Empty `AutoJoinRole` on a directly-constructed config | error — `ListAutoJoinAccountsByKeys` selects only `auto_join_role IS NOT NULL`, so a NULL role is invisible to the login gate and the account would admit nobody. Unreachable through the flag path, where `resolveDefaults` fills `member`; `Validate` is the guard for direct construction |
| Configured profile, nil query surface (no `FISHHAWKD_DATABASE_URL`) | error — never a silent skip |
| Upsert fails | wrapped error; no id, no partial success |

Validation runs BEFORE the write, so an invalid profile never leaves a row
behind. `serve.go` aborts startup on any of these.

`singletenant_test.go` covers each branch, and the create / idempotence /
in-place-update / home-region cases run against real Postgres through the
production `accountdb.Queries`. The cross-layer proof that the bootstrapped
account actually admits a member — real bootstrap, then the real
`MembershipResolver` — lives in
`backend/internal/auth/membership_test.go`
(`TestMembershipResolver_AdmitsViaSingleTenantBootstrap`, plus a
non-matching-granularity negative twin). Operator guide:
`docs/deploy/self-hosted.md`.

## Operator registry surface (`registry.go`, E45.33 / #2923)

`registry.go` is the multi-tenant analog of the single-tenant bootstrap: the
supported write path for the `accounts` / `installations` rows that GitLab run
creation is authorization-gated on. `gitLabProjectRegistry.AuthorizedGitLabProject`
(`backend/cmd/fishhawkd/serve.go`) admits a GitLab trigger only when an
`installations` row (provider `gitlab`, `installation_ref` `gitlab:<project-id>`)
resolves to an `accounts` row whose `account_key` equals the project path's
namespace segment — yet `UpsertInstallation` / `UpsertAccount` had no production
caller, so operators were reduced to hand-written SQL. `CreateAccount` /
`RegisterInstallation` back the `fishhawkd account create|list` /
`installation register|list` subcommands (`backend/cmd/fishhawkd/`), which stay
thin flag-parsing shells over this domain surface so the logic is unit-testable
against the `RegistryQueries` fake.

Since E45.26 / #2877 that gate binds the payload's project PATH exactly rather
than at namespace level, so `RegisterInstallation` additionally REQUIRES a
`ProjectPath` for a gitlab registration — see below.

Both validation vocabularies are reused from `singletenant.go`: `accountProviders`
/ `accountGranularities` (the `accounts_provider_check` /
`accounts_granularity_check` mirrors) are now package-scoped and shared, so the
constraint literals live in exactly one place.

### `CreateAccount` deliberately does NOT write `auto_join_role`

`auto_join_role` is the single-tenant profile's concept — `UpsertSingleTenantAccount`
is its only writer. A multi-tenant account admits members via invited grants, not
an auto-join policy, so `CreateAccount` calls `UpsertAccount` (which never touches
that column) with a `nil` `DisplayName` for the empty case. Granularity defaults
per provider: `organization` for github, `group` for gitlab (a GitLab namespace is
a group), exported as `DefaultGranularityGitHub` / `DefaultGranularityGitLab`.

### The account-existence check is a DISTINCT control, not FK reliance

`RegisterInstallation` resolves the owning account by `(provider, account_key)` and,
on `pgx.ErrNoRows`, refuses with an error wrapping `ErrAccountNotFound` that names
the `fishhawkd account create` remedy — it NEVER materializes the account. This is
the load-bearing gate: a GitLab payload is authenticated by a shared token with no
signature over the body, so the project it names is untrusted and authorization
must be an operator decision recorded server-side; a path that conjured the account
would hollow that out.

This check is **not redundant** with the composite
`FOREIGN KEY (account_id, provider) REFERENCES accounts (id, provider)` (migration
`0052`). Both reject the write, but the FK surfaces only an opaque SQLSTATE 23503 an
operator cannot act on and a typo'd namespace would read as an infrastructure fault.
This is why the counterfactual test (`TestRegisterInstallation_UnknownAccountRefuses`
and its CLI twin) asserts error IDENTITY (the message naming the key and the remedy)
**plus** a zero-row state read — deleting the Go check leaves the command still
exiting non-zero, so a bare non-zero-exit assertion would read as a false GREEN.

### Fail-closed branches

| Branch | Result |
|---|---|
| Empty account key / provider not in `accountProviders` / granularity not in `accountGranularities` | error wrapping `ErrValidation`, naming the offending value |
| `installation_ref` not matching the per-provider shape (`gitlab:<+int>` / bare `+int`) | error wrapping `ErrValidation` (`ValidateInstallationRef`) |
| Non-`https` / malformed `--forge-base-url` / `--oauth-base-url` | error wrapping `ErrValidation` (via `ValidateResolvedBaseURL`) |
| Named account does not exist | error wrapping `ErrAccountNotFound`, naming the `account create` remedy; installation write never reached |
| Any other DB fault from `GetAccountByKey` | propagated verbatim (neither validation nor not-found), so the CLI maps it to a failure exit |

`ErrValidation` and `ErrAccountNotFound` are exported so the CLI switches on error
KIND (`errors.Is`) for its usage-vs-failure exit-code split rather than matching a
message that could be reworded. `registry_test.go` pins one case per branch; the
committed-state, idempotence, and cross-boundary tests
(`TestInstallationRegister_AdmitsGitLabProjectThroughTheGate`, driving the CLI
through to the shipped `gitLabProjectRegistry`) live in
`backend/cmd/fishhawkd/{account,installation}_test.go`. Operator guide:
`docs/deploy/gitlab.md`.

## Operator membership surface (`members.go`, E44.34 / #2924)

`members.go` is the membership half of the operator registry: it admits the first
human on a fresh self-hosted install by writing an `account_members` row with
`origin='invited'`, which the login-gate admission walk (`backend/internal/auth/membership.go`)
already honors **DB-only** — an invited grant is checked BEFORE the sole live-forge
auto-join read (ADR-057 Amendment A2), so it admits with no forge round-trip at all.
`InviteMember` / `ListMembers` back the `fishhawkd member invite|list` subcommands,
composing with the shipped `account create` verb: the operator creates the account,
then invites the member into it. This replaces the hand-written SQL
`docs/deploy/self-hosted.md` used to point operators at.

### The verb pins `origin='invited'` rather than parameterizing it

`UpsertInvitedAccountMember` writes the literal `'invited'` — there is no `--origin`
flag. An operator verb that could mint `origin='auto_join'` would assert a
forge-derived provenance no forge ever reported, and the login gate re-verifies
`auto_join` grants against the live forge on each subsequent login, so such a row
would be silently revoked later. Consequence, asserted as committed state in
`members_test.go`: an existing `auto_join` grant re-invited by an operator is
**upgraded** to `invited` (forge-independent) — the `ON CONFLICT … DO UPDATE SET
origin = EXCLUDED.origin` is deliberate, not incidental.

### The role allow-list exists because there is no DB `CHECK`

`account_members.role` is nullable `TEXT` with **no** database `CHECK` (migration
`0055`), and `Store.MemberRole` (`roles.go`) resolves every non-`admin` value —
including a typo like `admn` — to member-tier (least privilege). So a mis-typed
`--role` would be accepted by the database and silently produce a member-tier grant
with no error at any layer. `InviteMemberRequest.Validate` is the ONLY guard: it
rejects any role outside `{admin, member}` (`memberRoles`) with `ErrValidation`
naming the value and the accepted set. `member_ref` is the forge **login** the
resolver matches on (`ListMemberGrants(ctx, provider, profile.Login)`), not a numeric
id or email.

### Fail-closed branches

| Branch | Result |
|---|---|
| Empty `--member-ref` / provider not in `accountProviders` / role not in `memberRoles` | error wrapping `ErrValidation`, naming the offending value |
| Named account does not exist | error wrapping `ErrAccountNotFound`, naming the `account create` remedy; membership write never reached |
| Any other DB fault from `GetAccountByKey` | propagated verbatim (neither validation nor not-found), so the CLI maps it to a failure exit |

`ErrValidation` / `ErrAccountNotFound` are reused from `registry.go` so the CLI's
usage-vs-failure exit-code split is by error KIND. `members_test.go` pins one case
per branch plus committed-state / idempotence / auto_join-upgrade reads; the
cross-boundary seam test (CLI → domain → sqlc → Postgres → the resolver with an
**empty lister registry**, proving DB-only admission) lives in
`backend/cmd/fishhawkd/member_test.go`. Operator guide: `docs/deploy/self-hosted.md`.

### `RegisterInstallation` requires a `ProjectPath` for gitlab (E45.26 / #2877)

The GitLab authorization gate now admits a trigger only when the payload's
`path_with_namespace` equals the installation's recorded `project_path`
EXACTLY (migration `0078`), so a gitlab installations row carrying none is
UNBOUND and refuses every trigger. `RegisterInstallation` therefore refuses to
create one: `ValidateGitLabProjectPath` requires a non-empty trimmed path of the
form `<namespace>/<project>` whose namespace segment equals the resolved
`account_key`, each failure wrapping `ErrValidation` and naming the offending
values. That keeps the fail-closed refusal a purely historical artifact — the
shape a pre-0078 row has — rather than something the supported write path can
newly produce.

The split is on the **FIRST** separator only. GitLab groups nest
(`acme/platform/widgets`), and the authorizer derives the namespace with the
same `strings.Cut`, so splitting any other way here would validate a shape the
gate then rejects.

The rule is **gitlab-only**. A github installation's identity arrives inside an
HMAC-signed payload and resolves through the installation id, so it records no
project path; any supplied one is ignored rather than persisted, and requiring
the flag there would be a gratuitous CLI contract break.

The path is stored **trimmed** and compared **case-sensitively**: GitLab
canonicalises project path case, so a case difference names a different project.
A stray flag-quoting space would otherwise produce a row that never admits
anything.
