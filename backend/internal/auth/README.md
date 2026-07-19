# backend/internal/auth

Authentication helpers for fishhawkd: the GitHub OAuth sign-in flow, the browser session model, the workspace-membership login gate, and the GitHub App manifest conversion flow.

## OAuth sign-in + sessions (E4.2 / ADR-005)

`GitHubOAuth` wraps GitHub's authorize/token/user endpoints (`OAuthURLs` substitutes httptest servers). `Repository.SignIn` upserts the `users` row and creates a `sessions` row whose sha256 hash is stored server-side; the plaintext goes into the `fishhawk_session` cookie (HttpOnly + Secure + SameSite=Lax, sliding 24h / absolute 7d). The auth middleware resolves the cookie to an `Identity` carrying `Subject="github:<login>" + UserID + SessionID + AccountID`; cookie auth is tried before bearer so a browser carrying both prefers the user-bound credential. Handlers: `/v0/auth/github/login`, `/v0/auth/github/callback`, `/v0/auth/me`, `/v0/auth/logout`. Configured via `--oauth-client-id` / `--oauth-client-secret` / `--oauth-callback-url`. Requested scopes: `read:user user:email read:org` (`read:org` so the membership gate's `/user/orgs` read sees private org memberships).

## Workspace-membership login gate (E44.3 / #1827, ADR-057 Amendment A2)

The OAuth callback consults `Config.AuthMembership` (a `MembershipResolver`) AFTER the profile fetch and BEFORE `SignIn` — a successful GitHub login is NOT admission. The resolved account binds the session (`sessions.account_id`, migration 0056, FK `ON DELETE SET NULL`), is stamped onto `server.Identity.AccountID` by the cookie path, and surfaces as `account_id` on `/v0/auth/me`.

**Admission source = `account_members` rows (authoritative), not a live forge match.** `account_members.origin` distinguishes the two grant kinds:

- **`invited`** — operator-granted. Admits **DB-only**: no forge call on this path, so forge-API availability can never lock out an invited member. This is the reliable grant.
- **`auto_join`** — minted at login by the bootstrap below, and **re-verified against its policy predicate at every login**: the account must still carry `auto_join_role` and the user's live org list must still contain the account's org key. A failed predicate stops admitting; the row is kept for audit.

**Auto-join bootstrap** is the ONLY live-forge read: `GitHubOAuth.ListUserOrgKeys` (GET `/user/orgs` with the USER's OAuth token, never an App token) is intersected with `accounts WHERE provider='github' AND granularity='organization' AND auto_join_role IS NOT NULL`. A match with no existing grant upserts an audited `origin='auto_join'` row (role = the policy's `auto_join_role`, `member_ref` = the GitHub login) and admits. Auto-join anchors to ORGANIZATION granularity only — the enterprise-membership API is not used; enterprise tenants are workspaces owning several org installations, admitted via invited rows or org-scoped auto-join.

**Fail-closed modes** (each pinned by a test):

| Mode | Behavior |
|---|---|
| `Config.AuthMembership == nil` | deny ALL sign-ins: 302 to the access-denied redirect, no session, no cookie |
| Forge error during auto-join eval, no invited grant | resolver error → callback 502 `membership_resolution_failed`, no session |
| Forge error during auto-join eval, invited grant present | invited admission stands (DB-only); auto-join eval degrades closed |
| No admitting account | 302 to `Config.AuthAccessDeniedRedirect` (default `/access-denied`, validated by `isSafeRelativeRedirect`), no session, no cookie |
| Provider with no resolver impl (gitlab today) | deny (additive follow-on) |
| Session with no resolvable account (deleted account → FK SET NULL, or a pre-gate session) | `/v0/auth/me` 403 `account_unresolved` |

Multi-account members are admitted deterministically: the resolver returns a sorted set and the callback binds the FIRST account (a picker is out of scope for v0).

**`/user/orgs` caveat.** GET `/user/orgs` can omit an org when the OAuth app is blocked by that org's third-party-application access restrictions (and the client reads a single `per_page=100` page), so a genuine org member may fail AUTO-JOIN. That failure is safe (fail-closed, access denied) and the remedy is an explicit `invited` `account_members` row, which admits DB-only regardless of what the forge reports. Auto-join is a best-effort bootstrap; invited rows are the reliable grant.

Wiring: `serve.go` builds `NewMembershipResolver(NewAccountMembershipStore(accountdb.New(pool)), githubOAuth)` when both OAuth and the database are configured. The admission queries live in `backend/internal/account/queries.sql` (`ListMemberGrantsByRef`, `ListAutoJoinAccountsByKeys`, `UpsertAccountMemberWithOrigin`).

## GitHub App manifest flow (E4.7)

`github_manifest.go` implements `GitHubManifest.Convert(ctx, code)`, which POSTs to `https://api.github.com/app-manifests/{code}/conversions` and returns App ID + slug + OAuth client ID/secret + webhook secret + PEM.

Two handlers in `backend/internal/server/manifest.go`:

- `GET /v0/auth/github/manifest-flow-start` mints state, sets a short-lived `fishhawk_manifest_state` cookie (separate from the OAuth state cookie), and renders an auto-submitting form to GitHub's manifest endpoint.
- `GET /v0/auth/github/manifest-callback` validates state (single-use; cookie cleared on entry), exchanges the one-shot `code`, and renders an HTML success page with the secrets and a copy-paste `.env` block. `Cache-Control: no-store` keeps the page out of browser history.

Operator-facing flow in `docs/github-app/README.md`. The hosted-deploy "persist secrets to the configured backend" path is deferred.
