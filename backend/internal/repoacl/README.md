# `backend/internal/repoacl` — per-identity forge repo-ACL mirror

Anchor: #2071 (E44.10), ADR-057 Amendment A2. Migrations
`0059_repo_acl_mirror`, `0060_repo_acl_purge_watermark` (#2116, E44.25 — the
purge-ordering discipline below).

Repo-scoped in-workspace visibility needs one question answered on every read:
**does this signed-in human hold at least `read` on this repo, on the forge?**
Asking the forge per (subject, repo) on every list page is a rate-limit
incident, so this package memoizes the answer with a TTL.

## What it is — and what it is not

`repo_acl_entries` is a **cache of a forge fact**, keyed
`(provider, subject, repo) → permission` with a `checked_at` stamp. The forge
stays **authoritative**. A miss or an expired row re-resolves live through
`identity.IdentityProvider.PermissionLevel`; the stale value is never served.

It is **not** an authorization store. Nothing grants permission here; the
mirror only remembers what the forge already said. Tier ranking is
`identity.Permission.AtLeast`, reused rather than re-implemented — which is
also why an unrecognized tier (a future forge's string, a hand-edited row)
denies instead of being ranked as write.

`subject` is the forge-neutral member ref — the identity subject with its
`"<provider>:"` prefix stripped **generically** by `SubjectRef`, never a
hard-coded `"github:"` literal — so a mirror row and an `account_members`
grant key on the same string for the same human.

## The two failure classes (binding — never collapsed)

This is the rule the package exists to keep honest. It is stated once, here,
and honored identically everywhere because the classification lives in
`Visible`'s return shape rather than at each call site:

| Class | Meaning | Mirror write | `Visible` returns | Caller behavior |
|---|---|---|---|---|
| **Forge error** (transport, 5xx, `identity.ErrRateLimited`) | permission is **UNKNOWN** | **none** | `(false, nil)` + **WARN** | that repo is not visible; the request otherwise **proceeds** — a list page omits it, a point read 403s `repo_forbidden` |
| **Store / DB error** (mirror read or write failed) | the filter **cannot function** | n/a | `(false, err)` wrapping `ErrStoreUnavailable` | the request fails **503 `service_unavailable`** |

Never collapse one into the other. A forge blip must not 503 an entire page,
and a database outage must not silently shorten one.

Because a forge fault **silently shortens a list**, `Visible` logs it at
**WARN** with the provider, the repo, whether it was a rate limit, and the
reason. That log line is the only way an operator can tell "you lack access"
from "we could not ask the forge" — a short page with no signal is the failure
mode this design most wants to avoid.

`Permission` surfaces the distinction directly for callers that need it
(`errors.Is(err, ErrForgeUnavailable)` / `ErrStoreUnavailable`), with the
underlying cause — including `identity.ErrRateLimited` — preserved in the
wrap chain.

A third class: a **nil or unwired** mirror returns `ErrNotConfigured`, which
callers translate into the untenanted-allow posture (filtering disabled — the
pre-#2071 visibility surface). It is deliberately an *error*, not a bare
`false`, so an accidentally-unwired mirror cannot masquerade as a deny-all.

## Staleness: which direction is acceptable

- **Stale-deny** (a permission was *granted* on the forge since the last
  check) is acceptable — the human waits at most `TTL` (or re-signs-in, which
  purges) to see the repo. Nothing is lost.
- **Stale-allow** (a permission was *revoked* since the last check) is the one
  that matters, and it is **bounded by `DefaultTTL` (15 minutes)** — never
  unbounded. This is the baseline exposure the whole design accepts.

That bound is enforced **in both directions of clock skew**. `checked_at` is
stamped by the *database* clock and TTL'd against the *application* clock, so a
row whose stamp is ahead of the app clock has a **negative** age; a bare
`age >= ttl` test would call it fresh until that future instant plus the TTL,
extending stale-allow past `DefaultTTL` by exactly the skew (and dramatically
after a clock jump). `expired` therefore treats a negative age as expired —
a forward-skewed row costs one extra live forge resolve, never an unbounded
stale grant.

`PermissionNone` **is** cached: a legitimate deny is worth memoizing, and
caching it is what stops a no-access caller re-asking the forge on every page.
A forge *error* is never cached — memoizing a transient fault would either
plant a phantom deny or, worse, refresh `checked_at` on a stale grant.

## Login purge

`InvalidateSubject(ctx, provider, subject)` bumps the purge watermark and then
drops every mirrored row for one identity, called at the end of the OAuth
callback so a fresh sign-in re-resolves rather than inheriting pre-login
answers. The bump-before-delete ordering that also invalidates an *in-flight*
resolution is the subject of the next section.

**The purge is NON-FATAL to sign-in**, and the reason is worth stating
precisely, because the obvious rationale is wrong. It is *not* true that a
failed purge "only means a re-resolve is needed later": if the purge fails,
previously cached entries survive — **grants included** — and stay readable
until they expire. What is true is that this returns the caller to *exactly*
the baseline TTL exposure the design already accepts everywhere else (any
permission revoked mid-TTL is visible until the entry expires). The exposure
is therefore bounded by `DefaultTTL`, not unbounded. Failing sign-in closed on
a transient DB blip would be the worse trade, so the callback logs and
continues.

## Purge ordering: closing the login-purge-vs-in-flight-resolution race (#2116)

A login purge (`InvalidateSubject`) and a concurrent cache-miss resolution race
each other. Two races exist, and this package treats them **differently and
states which is which precisely** — an over-claim here would be worse than an
honest bound:

- **Race B (login purge vs an in-flight resolution) is GENUINELY CLOSED.** This
  is the security-critical one: without ordering, a resolution could read a
  pre-purge world, the purge could delete the subject's rows, and the stale
  grant could then be written and SURVIVE the purge — exactly what a fresh
  sign-in must not inherit.
- **Race A (two concurrent NON-purge resolutions of the same key committing out
  of order) is NOT given a total order.** It remains last-writer-wins,
  **TTL-bounded convergence** — the loser's answer is at most `DefaultTTL` stale
  and re-resolves on expiry. The design deliberately does **not** claim a total
  order for two ordinary resolutions.

### The mechanism (race B)

A `repo_acl_purge_watermarks` table (migration 0060) holds a per-`(provider,
subject)` **BIGINT `generation`** counter. It is a **DB-side monotonic counter,
clock-INDEPENDENT** — chosen over a wall-clock `checked_at` because a wall clock
cannot survive the persistence boundary or an NTP rollback, and sub-second
precision collapses under ties. Crucially the counter **SURVIVES deletion of the
`repo_acl_entries` rows** (deleting an entry never touches the watermark), which
is what lets the ordering hold ACROSS the purge's row delete.

Three steps, in this order:

1. **Resolution start** — `Permission`, on a miss/expired path only, calls
   `EnsurePurgeGeneration` BEFORE the forge lookup. It upserts-and-returns the
   watermark row, **guaranteeing the row EXISTS** and capturing the current
   generation. Capturing *before* the forge read is load-bearing: any purge in
   the `[capture, write]` window bumps the generation, so the later guarded
   write sees `capturedGen < live` and is rejected; a resolution that captured
   *after* a purge captured the bumped generation and its forge read is
   post-purge, correctly allowed to write.
2. **Purge** — `InvalidateSubject` calls `BumpPurgeWatermark` (raises the
   generation, creating the row on a first-ever purge) **strictly BEFORE**
   `DeleteForSubject`. Bump-before-delete is the invariant: the raised watermark
   invalidates every in-flight write across the delete that follows.
3. **Memoizing write** — the guarded upsert (`UpsertRepoACLEntryGuarded`) is an
   `INSERT ... SELECT ... FROM repo_acl_purge_watermarks w WHERE $capturedGen >=
   w.generation FOR SHARE OF w ...`. The **`FOR SHARE` row lock is
   load-bearing**: it serializes the generation read against a concurrent
   `BumpPurgeWatermark`.

### Why `FOR SHARE`, not `FOR KEY SHARE`

`BumpPurgeWatermark` updates only the non-key `generation` column, so it takes a
**`FOR NO KEY UPDATE`** tuple lock. Per the PostgreSQL row-level-lock conflict
table, `FOR KEY SHARE` does **NOT** conflict with `FOR NO KEY UPDATE` (it exists
precisely so FK checks don't block on non-key updates) — a `FOR KEY SHARE`
guarded read would **not block** behind an in-flight bump and race B would stay
open. **`FOR SHARE` DOES conflict** with `FOR NO KEY UPDATE` (and `FOR UPDATE`),
so the guarded read **blocks** behind an in-flight bump and, on unblock, the
`EvalPlanQual` re-read under READ COMMITTED observes the **bumped** generation —
`capturedGen >= w.generation` is now false, the SELECT yields zero rows, and
nothing is inserted. The window closes in **both** directions: a bump also
blocks behind an in-flight guarded upsert. A rejected write returns zero rows
(`pgx.ErrNoRows`), which the store maps to a **benign non-memoized rejection**,
not an error — `Permission` still returns the resolved answer to *this* caller;
it is simply not memoized, and the next request re-resolves.

### The ensure-row requirement (and its cost)

`FOR SHARE` on an **absent** row locks **nothing** and silently reopens the
race, so the watermark row MUST exist before it is ever locked. That is exactly
what `EnsurePurgeGeneration` (at resolution start) and `BumpPurgeWatermark` (on a
first-ever purge) guarantee; watermark rows are never deleted, so once created
they persist. The cost, stated honestly: `EnsureRepoACLPurgeWatermark` is an
`INSERT ... ON CONFLICT DO UPDATE SET generation = generation` — a **no-op
self-assignment that still takes a `FOR NO KEY UPDATE` write lock and writes a
new row version on the read-only hot path**, i.e. dead-tuple churn that
autovacuum reclaims. This is the deliberate price of guaranteeing a lockable row
on every miss; a `DO NOTHING` + plain-`SELECT` fallback would avoid the churn
but at the cost of a second round-trip on the row-absent path, and the churn is
bounded (one dead tuple per cache miss, not per read served from the mirror).

Two prior-round notes fold in here: (i) `checked_at` is still `now()` on every
write and the `expired()` TTL prose above stays accurate — but the **generation
guard**, not `checked_at`, is the purge-ordering authority; (ii) `Permission`
remains an exported method with no production caller today.

## Not account-scoped, and outside the RLS regime

`repo_acl_entries` carries no `account_id`. The fact it mirrors — "does forge
subject S hold ≥ `read` on repo R" — is a property of the (identity, repo)
pair and is identical across every account the subject belongs to; adding
`account_id` purely to satisfy migration 0057's RLS regime would duplicate a
row per account and invite the copies to disagree. The rationale lives in
0059's header where it stays challengeable, and
`TestMigrateUp` asserts both the absent column and the absent
`relrowsecurity` so the choice cannot drift silently.

`repo_acl_purge_watermarks` (0060) is outside the RLS regime for the **same**
reason — it mirrors an identity-scoped purge ordering, not account-scoped
tenant data — and `TestMigrateUp` pins its absent `account_id` and
`relrowsecurity = 0` identically.

Related: 0057's RLS policies are **inert in production today** (the runtime
role is a superuser and superusers bypass RLS even under FORCE), so the
server-side handler filter this mirror feeds is the *effective* in-workspace
boundary until that rollout completes.

## Deferred

Webhook-driven invalidation (a forge `member`/`repository` event purging the
affected rows immediately, shrinking the stale-allow window below the TTL) is
**not** implemented here — the TTL plus the login purge is the v0 bound.

## Layout

| File | Role |
|---|---|
| `repoacl.go` | `Mirror` (`Permission` / `Visible` / `InvalidateSubject`), the `Store` (incl. `EnsurePurgeGeneration` / `BumpPurgeWatermark`) and `PermissionResolver` seams, `DefaultTTL`, the three sentinels, `SubjectRef` |
| `postgres.go` | the pgx/sqlc-backed `Store`, including the guarded upsert and the watermark ensure/bump |
| `queries.sql` + `db/` | the sqlc surface (hand-written to sqlc's output shape — see the header in `queries.sql`); includes the `FOR SHARE`-guarded `UpsertRepoACLEntryGuarded` and the watermark ensure/bump queries |
