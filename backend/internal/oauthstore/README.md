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
`TestCredentials_PlaintextReturnedOnlyAtMint` asserts it **exhaustively over the
interface**, not illustratively: every exported method that can return a
credential struct appears. The three mint paths return a non-empty `PlainText`;
the five non-mint paths that return one — `LookupAuthorizationCode`, the code
carried on `RedeemAuthorizationCode`'s grant, `AuthenticateAccessToken`,
`RevokeAccessToken` and `RevokeRefreshToken` — return it empty.

`RevokeRefreshToken` is the load-bearing addition: it is the **only** public path
that hands back a *persisted* refresh-token row, so without it the refresh class
would be asserted on mint output alone — a row that never made the round trip
through the database. The assertions are paired with an identity/`revoked_at`
check so an empty struct could not satisfy them vacuously. (`GetClient` /
`UpsertClient` return a `*Client`, which carries no `PlainText` field at all;
`GetRefreshTokenByHash` lives only in the generated layer, as an implementation
detail of `RotateRefreshToken`.)

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
document updates the persisted row, that `first_seen_at` does not move, that
`updated_at` is **strictly** later, and that the same `client_id` under `gitlab`
is a distinct registration. The strictness matters: an `after || equal` form
would permit an unchanged `updated_at`, so dropping `updated_at = now()` from the
`DO UPDATE` would pass it — the assertion would no longer be load-bearing for the
"`updated_at` moves" half of the claim.

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

### The lineage lock — why the guarantee also holds under CONCURRENCY

Committing the sweep is necessary but **not sufficient**. Per-token row locks do
not serialize a rotation against a sweep of the same lineage: the sweep's
`UPDATE … WHERE authorization_code_id = $1` fixes its statement snapshot when the
statement starts, so a rotation that INSERTs a successor pair and commits after
that instant produces rows the sweep can never see. The reuse verdict would
return with a **live descendant credential still usable** — the exact failure the
guarantee forbids.

So the **authorization-code row is the mutex for its whole lineage**.
`RotateRefreshToken` and `RevokeGrantsForCode` both take it `FOR UPDATE`
(`LockAuthorizationCodeByID`) *before* reading or mutating any descendant;
`RedeemAuthorizationCode` already held the same lock via
`LockAuthorizationCodeByHash`, so the ordering is uniform across the package and
deadlock-free. Rotation therefore runs in three steps: (1) an unlocked read that
resolves **only** the immutable `authorization_code_id` and decides nothing, (2)
the lineage lock, (3) the authoritative re-read under it. Step 3 is what lets a
rotation queued behind a sweep observe `revoked_at` and mint nothing.

Only two interleavings remain, both safe: the rotation commits first and the
sweep's later snapshot includes its successor, or the sweep commits first and the
rotation returns `ErrRevoked`.

| Test | Role |
|---|---|
| `TestRotateRefreshToken_ReuseSweepSerializesOnLineage` | **counterfactual vehicle.** An outside transaction holds the code row's `FOR UPDATE` lock and the reuse call must BLOCK. Delete `lockLineage` and it sails through (the sweep's UPDATEs never touch the code row) → RED, deterministically |
| `TestRotateRefreshToken_ReuseLeavesNoLiveDescendantUnderConcurrency` | the end-to-end **invariant** under a real race: after a concurrent rotate + replay, zero live rows remain in the lineage and a winning rotation's access token does not authenticate. Deliberately *not* the counterfactual vehicle — the losing interleaving is timing dependent, so removing the lock would make it flaky rather than reliably red |

### Classification order: revoked → REUSED → expired

`revoked` first, so a third presentation of an already-swept token reports
`ErrRevoked` rather than replaying the sweep.

`reused` **before** `expired` is a deliberate decision, not an accident of
ordering. A replayed rotated token is precisely the compromise signal the sweep
exists for, and the *presented* token's expiry says nothing about its
**successors** — they carry later expiries and are the credentials actually at
risk. Classifying expiry first would silently drop the signal exactly when a
stolen token is replayed late, leaving the live successors unrevoked. So
`ErrExpired` on this path means "lapsed and **never rotated**";
`TestRotateRefreshToken_ExpiredReuseStillSweepsLineage` pins the combination
(counterfactual: hoist the `IsExpired` check above `IsConsumed` → red on both the
returned error and the unswept successor), and
`TestRotateRefreshToken_ExpiredUnknownAreDistinct` keeps the never-rotated case.

### The unreachable consume branch is not the reuse sentinel

`ConsumeRefreshToken`'s conditional UPDATE cannot match zero rows on this path:
step 3 read `consumed_at` as NULL while holding both the lineage lock and the
row's own `FOR UPDATE`. If it ever did, returning `ErrRefreshReused` there would
break the commit-then-signal guarantee above — that return **rolls the
transaction back**, so the caller would receive the reuse sentinel with no
committed revocation. It returns a wrapped internal error instead: an invariant
violation is not a reuse verdict.

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
