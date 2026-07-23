# `backend/internal/repoacl` — per-identity forge repo-ACL mirror

Anchor: #2071 (E44.10), ADR-057 Amendment A2. Migration `0059_repo_acl_mirror`.

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

`PermissionNone` **is** cached: a legitimate deny is worth memoizing, and
caching it is what stops a no-access caller re-asking the forge on every page.
A forge *error* is never cached — memoizing a transient fault would either
plant a phantom deny or, worse, refresh `checked_at` on a stale grant.

## Login purge

`InvalidateSubject(ctx, provider, subject)` drops every mirrored row for one
identity, called at the end of the OAuth callback so a fresh sign-in
re-resolves rather than inheriting pre-login answers.

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

## Not account-scoped, and outside the RLS regime

`repo_acl_entries` carries no `account_id`. The fact it mirrors — "does forge
subject S hold ≥ `read` on repo R" — is a property of the (identity, repo)
pair and is identical across every account the subject belongs to; adding
`account_id` purely to satisfy migration 0057's RLS regime would duplicate a
row per account and invite the copies to disagree. The rationale lives in
0059's header where it stays challengeable, and
`TestMigrateUp` asserts both the absent column and the absent
`relrowsecurity` so the choice cannot drift silently.

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
| `repoacl.go` | `Mirror` (`Permission` / `Visible` / `InvalidateSubject`), the `Store` and `PermissionResolver` seams, `DefaultTTL`, the three sentinels, `SubjectRef` |
| `postgres.go` | the pgx/sqlc-backed `Store` |
| `queries.sql` + `db/` | the sqlc surface (hand-written to sqlc's output shape — see the header in `queries.sql`) |
