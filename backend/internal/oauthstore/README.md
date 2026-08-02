# `backend/internal/oauthstore` — OAuth 2.1 authorization-server storage

Anchor: #2433 (E66.18), ADR-076. Migration
`0063_oauth_as_storage` (four tables: `oauth_clients`,
`oauth_authorization_codes`, `oauth_access_tokens`, `oauth_refresh_tokens`).
First consumer: #2391 (the HTTP endpoints), which has not landed.

## Why persistence lives here and not in `internal/oauthas`

`oauthas` is the pure domain core — PKCE, RFC 8252 redirect matching, typed RFC
6749 errors, CIMD fetch. Its `TestPackage_ImportsNeitherServerNorDatabase`
machine-enforces that it imports no `internal/server`, no `internal/postgres`,
no `internal/db` and no `jackc/pgx`, in **both** `Imports` and `TestImports`. A
storage file added there fails that test immediately.

So this is `oauthas`'s sibling persistence package. The dependency direction is
**one-way**: `oauthstore` imports `oauthas` for its value types
(`ClientMetadata`, `CodeChallengeMethod`), never the reverse — which is what
keeps that boundary test passing unchanged, and is itself the falsifying signal
if the placement were ever wrong.

## What this package is NOT

No HTTP handler, no `/.well-known` metadata document, no bearer middleware, no
CIMD fetching. #2391 owns the endpoints and composes this store with `oauthas`.

## Schema

| Table | Holds | Notes |
|---|---|---|
| `oauth_clients` | CIMD-derived registrations | `UNIQUE (provider, client_id)`; `client_id` is the CIMD document URL |
| `oauth_authorization_codes` | issued codes | `code_hash` UNIQUE; **both** `expires_at` and `consumed_at`, with distinct predicates |
| `oauth_access_tokens` | bearer tokens | `authorization_code_id` **NOT NULL** — the lineage edge |
| `oauth_refresh_tokens` | rotating refresh chain | `authorization_code_id` **NOT NULL** too; plus `access_token_id` and the nullable self-referential `replaced_by_id` |

All four carry the forge-neutral `provider` discriminator (`github`/`gitlab`,
CHECK-constrained) and a nullable `account_id` from day one, so Mode 2 workspace
issuance (ADR-057 / E44) needs no token-schema migration later.

## Credentials are hashed at rest; plaintext exists only at mint

Each credential is a prefixed random string over 32 crypto/rand bytes
(`fhc_` code, `fho_` access token, `fhr_` refresh token), RawURLEncoding base64.
Only its **hex sha256** is persisted — the api_tokens pattern.

`PlainText` on `AuthorizationCode` / `AccessToken` / `RefreshToken` is populated
**exactly once**, by the mint path that generated the value, and is never
re-derivable. Every row mapper leaves it empty by construction, so this is
structural rather than conventional.
`TestCredentials_PlaintextReturnedOnlyAtMint` asserts it across every read path
the `Repository` exposes — `LookupAuthorizationCode`, the code carried on
`RedeemAuthorizationCode`'s grant, `AuthenticateAccessToken`, and the
post-rotation grant. It deliberately does **not** claim a refresh-token-by-
plaintext read: the interface exposes none (`GetRefreshTokenByHash` lives only
in the generated layer, as an implementation detail of `RotateRefreshToken`), and
an acceptance claim the store API cannot express would be worse than a narrower
one that is true.

Distinct prefixes per class are load-bearing: presenting an access token where a
refresh token is expected fails at `HashPlaintext` with `ErrMalformedCredential`
and **no database round-trip**.

## Client registrations REFRESH on fetch — they are not pinned

`UpsertClient` **overwrites** the metadata columns on `(provider, client_id)`
conflict. A CIMD document is authoritative and client-controlled by design, so a
legitimate client that rotates its redirect URIs must not be permanently broken
by a first-use snapshot; the `oauthas` fetcher's TTL bounds staleness. Pinning
would also give only illusory protection — an attacker who can rewrite the
client's document already controls the client.

`first_seen_at` is preserved across refreshes (it is not in the `DO UPDATE` SET
list); `updated_at` moves.
`TestUpsertClient_RefreshesOnConflictAndIsProviderScoped` asserts that a changed
document updates the persisted row, that `first_seen_at` does not move, and that
the same `client_id` under `gitlab` is a distinct registration.

## Row-level security: these four tables sit OUTSIDE it, deliberately

0057 enabled `ENABLE + FORCE ROW LEVEL SECURITY` with a
`<table>_tenant_isolation` policy on every account-scoped root table. These four
have **no policy and no RLS flag**. The full four-part rationale is written into
0063's header; in summary:

- **(a) The bootstrap read is pre-identity by construction.** `POST /oauth/token`
  receives an authorization code and nothing else that names a tenant; the code
  **row** is what carries `account_id`. 0057's predicate compares against the
  very value the read exists to *discover*, so under that policy an unset GUC
  would make every tenanted code invisible to the only query that needs it.
- **(b) Wrong class of table.** These are credential-issuance tables on the
  authentication path — the same class as `users`, `mcp_tokens` and
  `signing_keys`, all of which 0057 deliberately leaves outside RLS.
- **(c) What isolation they actually have — stated precisely, no more.** Every
  *credential* read is keyed on a 256-bit unguessable hash, so reaching another
  tenant's row requires already holding that tenant's plaintext credential.
  Every *ID-based* operation (`RevokeAccessToken` / `RevokeRefreshToken` by id,
  `RevokeGrantsForCode`) carries **no database-level tenant check whatsoever**;
  those are reachable only from an already-authenticated caller in #2391, which
  is where the authorization decision belongs. That is an application-layer
  control, not a database one. **No shipped query in this package filters on
  `account_id`** — the column is present so a later policy needs no migration.
- **(d) The future mechanism is named**, not punted: either a `BYPASSRLS` system
  context for the single pre-identity code lookup, or a policy whose `USING`
  clause permits an unset-GUC read of `oauth_authorization_codes` only, with
  `SET LOCAL app.account_id` issued immediately after the code resolves. Neither
  is urgent — 0057's policies are inert in production today because the runtime
  role `fishhawk` is a superuser and superusers bypass RLS even under `FORCE`.

The **absence** is machine-enforced: `TestMigrateDown_OAuthASStorageReversal`
asserts `pg_policies` carries zero rows for all four tables and
`pg_class.relrowsecurity` is false for all four, so a later accidental policy
addition is caught rather than assumed away.

## Verify-before-consume is orderable by the caller

A consume-then-return signature would burn a legitimate client's code on a
failed check, so it is explicitly foreclosed by the interface shape:

- `LookupAuthorizationCode` is a **read-only** load. It never touches
  `consumed_at`, so #2391 can validate `client_id`, `redirect_uri`, PKCE and
  resource and render a typed `oauthas` error **without burning the grant**.
- `RedeemAuthorizationCode(ctx, plaintext, verify, mint)` is the transactional
  seam, with an ordering **contract**: one transaction → `SELECT … FOR UPDATE` →
  refuse consumed (`ErrCodeConsumed`) → refuse expired (`ErrExpired`) → call
  `verify` → conditional consume + mint **only** when `verify` returns nil. A
  consumed or expired code never reaches `verify`. A verify error is returned
  **unchanged** and rolls the transaction back.

`TestRedeemAuthorizationCode_VerifyRejectionRollsBack` is the concrete proof: an
intercepted code plus a wrong verifier leaves `consumed_at` NULL and mints zero
tokens, and the legitimate client's later redemption still succeeds.

**The client binding is the caller's, not the database's.** `RedeemAuthorizationCode`
takes no `client_id` and enforces no binding itself;
`TestRedeemAuthorizationCode_VerifySeesTheLoadedRow` exercises the caller-supplied
verify seam standing in for #2391's token endpoint. Do not read it as a
database-enforced binding.

## The single-use controls, and which one is load-bearing

There are two, deliberately redundant. **The design decision is recorded so the
redundancy is intentional rather than accidental:**

| Control | Role | Pinned by | Counterfactual |
|---|---|---|---|
| `ConsumeAuthorizationCode`'s `AND consumed_at IS NULL` | **LOAD-BEARING** — still holds if a future caller reaches the UPDATE without taking the row lock | `TestConsumeAuthorizationCode_PredicateRejectsConsumedRow` (issues the UPDATE on a plain pool connection, no lock, no repository check in front) | (a) delete the predicate → RED |
| the post-lock `IsConsumed()` check in `RedeemAuthorizationCode` | **redundant short-circuit** — kept because it yields a typed refusal without paying for the caller's `verify` | `TestRedeemAuthorizationCode_ConsumedCodeSkipsVerify` (verify-call counter) | (b) delete the check → RED on the counter |

`TestRedeemAuthorizationCode_ConcurrentSingleUse` proves the **conjunction**
under real contention (8 racers, exactly one winner, exactly N−1
`ErrCodeConsumed`, exactly one access-token row). It is **not** a counterfactual
vehicle for either control and cannot be made into one: because the two are
redundant, deleting either alone leaves the other rejecting every loser and the
test stays green. This was verified empirically, not just argued — see the PR's
Test plan. Mandating a red there would mandate an unattainable result, which is
why each control is pinned where it is *individually* observable.

## Lineage is store-owned; reuse revocation is COMMITTED before the error returns

`authorization_code_id` is `NOT NULL` on **both** token tables. That is what
makes `RevokeGrantsForCode` — the RFC 6749 §4.1.2 replay response — reachable at
all. The trade is named: a future grant type with no originating code
(`client_credentials`) needs a follow-up migration relaxing the column.

`MintRequest` deliberately carries **no** authorization-code id.
`RedeemAuthorizationCode` stamps the code it just consumed, and
`RotateRefreshToken` **derives** the id from the presented refresh token's own
row. A caller cannot supply a wrong value because the type cannot express one —
so a successor can never be minted outside the sweep's reach.
`TestRotateRefreshToken_LineageIsDerivedNotCallerSupplied` keeps that structural
with a reflection guard (a future refactor re-adding the field reddens it) plus
the behavioural half: rotating code A's token while the `MintRequest` describes
an unrelated grant B still produces a successor swept by
`RevokeGrantsForCode(A)` and untouched by `RevokeGrantsForCode(B)`.

### Why the reuse verdict cannot be returned from inside the transaction

`pgx.BeginFunc` **rolls back** whenever its callback returns a non-nil error.
The reuse path performs the whole-lineage revocation *inside* that callback — so
returning `ErrRefreshReused` from within it would undo the very revocation the
error signals, and a caller reading the store afterwards would observe an
un-revoked lineage behind a reuse error. The returned error would be identical
either way, so error identity alone cannot detect the bug.

The fix is **commit-then-signal-outside**: the reuse path sets a captured
`reused` flag and returns `nil`, so `BeginFunc` commits; the verdict is mapped to
`ErrRefreshReused` after the transaction closes. One transaction (rather than a
second standalone one) keeps the `FOR UPDATE` lock and the sweep atomic. This is
a **guarantee of the API**: when `RotateRefreshToken` returns `ErrRefreshReused`,
the lineage revocation is already committed.

`TestRotateRefreshToken_ReuseRevokesLineageAfterReturn` reads `revoked_at` back
over raw SQL from a pool connection acquired **after** the call returned — never
from inside the repository's transaction — which is exactly what a
rollback-on-error implementation cannot satisfy. Counterfactual (c) reintroduces
that shape and the test goes red on those assertions while the returned error is
unchanged.

Revoked is classified **before** consumed, so a third presentation of a
swept token reports `ErrRevoked` rather than replaying the sweep.

## Revoked is an observable state, not an absence

`GetAccessTokenByHash` and `GetRefreshTokenByHash` are **unfiltered** on
`revoked_at`. The filter's absence is the control: it lets the repository
classify `revoked → ErrRevoked`, `expired → ErrExpired`, and `ErrNotFound` **only**
when no row carries the hash at all. A future "optimization" re-adding
`AND revoked_at IS NULL` is a regression, not a speedup — the `token_hash` UNIQUE
constraint's btree index serves the lookup either way, which is also why no
active-only partial index exists.

`ErrRevoked` and `ErrNotFound` stay distinct at this API even though #2391 will
collapse them into one opaque HTTP response, because audit and the
lineage-revocation tests read the distinction.
`TestAuthenticateAccessToken_RevokedExpiredUnknownAreDistinct` asserts all three
in one test with `errors.Is` on each sentinel, so collapsing any pair fails.

## Store unavailability is never a sentinel

A database error must surface as a raw error from every entry point — never
`ErrNotFound`, which would tell the token endpoint "no such credential" when the
truth is "we could not look".
`TestRepository_DatabaseErrorsAreNotMisclassified` drives all ten entry points
against a closed pool and asserts none of the six sentinels is returned.

## Generated layer

`db/` is sqlc v1.31.1 output for `queries.sql`, registered in
`backend/sqlc.yaml` so a clean `sqlc generate` reproduces it. One hand step:
sqlc emits a model struct for **every** table in the migration schema, so
`db/models.go` is trimmed to the four `Oauth*` types — the same convention
`internal/repoacl/db` (2 types) and `internal/account/db` (3) already follow.
`Queries.WithTx` is load-bearing: `RedeemAuthorizationCode` and
`RotateRefreshToken` run through `oauthstoredb.New(tx)`.
