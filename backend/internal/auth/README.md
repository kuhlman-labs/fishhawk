# backend/internal/auth

Authentication helpers for fishhawkd: the GitHub OAuth sign-in flow, the browser session model, the workspace-membership login gate, and the GitHub App manifest conversion flow.

## OAuth sign-in + sessions (E4.2 / ADR-005)

`GitHubOAuth` wraps GitHub's authorize/token/user endpoints (`OAuthURLs` substitutes httptest servers). `Repository.SignIn` upserts the `users` row and creates a `sessions` row whose sha256 hash is stored server-side; the plaintext goes into the `fishhawk_session` cookie (HttpOnly + Secure + SameSite=Lax, sliding 24h / absolute 7d). The auth middleware resolves the cookie to an `Identity` carrying `Subject="github:<login>" + UserID + SessionID + AccountID`; cookie auth is tried before bearer so a browser carrying both prefers the user-bound credential. Handlers: `/v0/auth/github/login`, `/v0/auth/github/callback`, `/v0/auth/me`, `/v0/auth/logout`. Configured via `--oauth-client-id` / `--oauth-client-secret` / `--oauth-callback-url`. Requested scopes: `read:user user:email read:org` (`read:org` so the membership gate's `/user/orgs` read sees private org memberships).

### Endpoint binding — deployment default only (E44.16 / #2094)

The web sign-in flow carries **zero** per-installation (Mode 2) endpoint routing, by sequencing not omission. Every operation runs at or **before** the user is identified, so none can know which installation (hence which data-resident host) applies:

| Operation | Read | Identification stage | Host |
|---|---|---|---|
| `AuthorizeURL` | browser redirect | anonymous (only a state nonce) | deployment default |
| `ExchangeCode` | POST token | anonymous (bare code) — **excluded** from any overridable set | deployment default |
| `FetchProfile` | GET `/user` | the INITIAL identifying read (subject established *by* this call) | deployment default |
| `ListUserOrgKeys` | GET `/user/orgs` | org-discovery that establishes *which* installation | deployment default |

The installation only becomes known **after** `ListUserOrgKeys` returns, at which point the flow is over. `GitHubOAuth` therefore has no `ResolveBaseURL` hook at all — each request targets the Mode 1 `FISHHAWKD_OAUTH_*` / `api.github.com` endpoints configured on the client, and there is structurally nothing to route through. The `oauth_base_url` per-installation override (migration 0055) has **no genuine post-identification web-OAuth consumer**, so its per-installation leg is deferred alongside the identity provider's (see `backend/internal/identity`). Pinned by `TestWebOAuthFlow_DeploymentDefaultHostOnly` (all four operations asserted against one host).

## Workspace-membership login gate (E44.3 / #1827, ADR-057 Amendment A2)

The OAuth callback consults `Config.AuthMembership` (a `MembershipResolver`) AFTER the profile fetch and BEFORE `SignIn` — a successful GitHub login is NOT admission. The resolved account binds the session (`sessions.account_id`, migration 0056, FK `ON DELETE SET NULL`), is stamped onto `server.Identity.AccountID` by the cookie path, and surfaces as `account_id` on `/v0/auth/me`.

**Admission source = `account_members` rows (authoritative), not a live forge match.** `account_members.origin` distinguishes the two grant kinds:

- **`invited`** — operator-granted. Admits **DB-only**: no forge call on this path, so forge-API availability can never lock out an invited member. This is the reliable grant.
- **`auto_join`** — minted at login by the bootstrap below, and **re-verified against its policy predicate at every login**: the account must still carry `auto_join_role` and its `(account_key, granularity)` pair must still be one the user's memberships derive. A failed predicate stops admitting; the row is kept for audit.

**Auto-join bootstrap** is the ONLY live-forge read. E44.8 (#1832) generalized it from one membership source to three, all behind the SAME `ForgeMembershipLister` seam, dispatched through a **provider-keyed lister map** (`NewMembershipResolver(store, listers, opts...)`). A provider absent from the map denies **the auto-join path only** — the lister lookup happens AFTER the invited-grant check, so an invited grant for a provider whose forge is unconfigured still admits. Conditioning invited admission on lister registration would break the DB-only, forge-independent contract above.

| Granularity | Provider | Source | Live forge call |
|---|---|---|---|
| `organization` | github | `GitHubOAuth.ListUserOrgKeys` — GET `/user/orgs` with the USER's OAuth token, never an App token | yes |
| `enterprise` | github, **EMU posture only** | the enterprise short code split off the EMU login itself (`emu.go`) | **no** |
| `group` | gitlab | `GitLabMembershipLister.ListUserOrgKeys` — GET `/api/v4/groups` with the USER's OAuth token, never `FISHHAWKD_GITLAB_TOKEN`; **paginated** to exhaustion (Link `rel="next"`, else a full page implies another) under a 50-page cap, each page body read under a 4 MiB byte cap | yes |

A match with no existing grant upserts an audited `origin='auto_join'` row (role = the policy's `auto_join_role`, `member_ref` = the forge login) and admits.

**Keys stay bound to their granularity.** The derived membership set is a list of `(key, granularity)` PAIRS, and `ListAutoJoinAccountsByKeys` matches them **pair-wise** (two positionally-paired arrays `unnest`ed together) — never `account_key = ANY(keys) AND granularity = ANY(granularities)`, whose cartesian product would admit a mere org member of "acme" into an ENTERPRISE account keyed "acme", and a derived enterprise short code into an ORGANIZATION account of the same key. The identical pairing governs re-verification of an existing `auto_join` grant: it re-admits only when its account's own `(account_key, granularity)` is a derived pair.

**EMU posture gate.** An Enterprise Managed User login is IdP-assigned as `<username>_<shortcode>`. A public github.com login may contain only alphanumerics and hyphens, so it cannot contain an underscore — which is precisely why the short-code derivation is gated on `IsEMUOAuthHost` (the deployment's configured OAuth host being a data-resident `<slug>.ghe.com` endpoint, E44.2/#1826). Ungated, a crafted `alice_acme` login on a github.com deployment would claim the "acme" enterprise. The FULL login (short code included) stays the identity key everywhere else — `member_ref`, `Subject`, `canonicalGitHubLogin` are unchanged.

**GitLab browser sign-in is LIVE (E44.22 / #2109).** The group-membership seam (`GitLabMembershipLister`, #1832) shipped ahead of a sign-in flow; that flow now exists. `GitLabOAuth` (`gitlab_oauth.go`) mirrors `GitHubOAuth` — `AuthorizeURL` → `{base}/oauth/authorize`, `ExchangeCode` → `{base}/oauth/token`, `FetchProfile` → `GET {base}/api/v4/user` — and `GET /v0/auth/gitlab/{login,callback}` (`server/auth.go`, via the provider-parameterized `handleForgeCallback`) threads `provider="gitlab"` through `MembershipResolver.ResolveAccounts`, `Repository.SignIn`, and the repo-acl mirror purge, so `provider="gitlab"` is reachable in production once `FISHHAWKD_GITLAB_OAUTH_*` is configured (endpoint host: `FISHHAWKD_GITLAB_BASE_URL`, the same base the lister uses). The requested OAuth scope is **`read_api`**, which authorizes BOTH `GET /api/v4/user` (the profile) and `GET /api/v4/groups` (the auto-join list) — `read_user` grants only the former and would deny every group auto-join. Identity is forge-scoped: `users` carries a `provider` discriminator (migration 0061) with `UNIQUE (provider, github_user_id)`, so a GitLab numeric id never overwrites a GitHub user of the same id.

**SSO boundary — what lands now vs deferred.** Landing: SSO **delegated to the forge's OAuth** — GitHub Enterprise OAuth on the data-resident endpoint (so the IdP-backed EMU login is what Fishhawk sees), and GitLab OAuth on its instance. Deferred to the v1 SSO/SAML roadmap item: Fishhawk acting as its own SAML SP, and SCIM provisioning. Enterprise membership here is derived from the login, not from a SAML assertion or an enterprise-membership API.

**Fail-closed modes** (each pinned by a test):

| Mode | Behavior |
|---|---|
| `Config.AuthMembership == nil` | deny ALL sign-ins: 302 to the access-denied redirect, no session, no cookie |
| Forge error during auto-join eval, no invited grant | resolver error → callback 502 `membership_resolution_failed`, no session |
| Forge error during auto-join eval, invited grant present | invited admission stands (DB-only); auto-join eval degrades closed |
| No admitting account | 302 to `Config.AuthAccessDeniedRedirect` (default `/access-denied`, validated by `isSafeRelativeRedirect`), no session, no cookie |
| Provider with NO registered lister (gitlab with `FISHHAWKD_GITLAB_BASE_URL` unset), no invited grant | deny — no auto-join eval is possible without a live membership read |
| Provider with NO registered lister, invited grant present | **admits** — invited grants are DB-only and forge-independent, so they cannot be gated on forge configuration |
| Underscore-bearing login on github.com posture | no enterprise key derived at all → no enterprise admission (spoofing guard) |
| EMU posture, login with no underscore / empty half (`alice_`, `_acme`) | no enterprise key contributed; org auto-join unaffected |
| GitLab group listing errors, non-200, undecodable, exceeds the 50-page cap, or returns a page body over the 4 MiB read cap | error → auto-join eval fails closed (whole sign-in, absent an invited grant). The forge body is semi-trusted input on an auth path, so an oversized page is rejected outright rather than truncated-and-parsed — a truncated listing is a partial membership set, and admitting on one is what this contract forbids |
| `UpsertAutoJoinGrant` write fails | error, no admission — the minted row IS the audit record |
| Session with no resolvable account (deleted account → FK SET NULL, or a pre-gate session) | `/v0/auth/me` 403 `account_unresolved` |

Multi-account members are admitted deterministically: the resolver returns a sorted set and the callback binds the FIRST account (a picker is out of scope for v0).

**`/user/orgs` caveat.** GET `/user/orgs` can omit an org when the OAuth app is blocked by that org's third-party-application access restrictions (and the client reads a single `per_page=100` page), so a genuine org member may fail AUTO-JOIN. That failure is safe (fail-closed, access denied) and the remedy is an explicit `invited` `account_members` row, which admits DB-only regardless of what the forge reports. Auto-join is a best-effort bootstrap; invited rows are the reliable grant.

Wiring: `serve.go` builds `NewMembershipResolver(NewAccountMembershipStore(accountdb.New(pool)), resolveMembershipListers(…), WithEMUOAuthHost(githubEndpoints.OAuth.AuthorizeURL))` when both OAuth and the database are configured. `resolveMembershipListers` registers `github` whenever OAuth is configured and `gitlab` when `FISHHAWKD_GITLAB_BASE_URL` is set (no token needed — see `backend/cmd/fishhawkd/README.md` for the config asymmetry against the token-gated gitlab forge provider). EMU posture needs no new flag: it is derived from the existing endpoint config. The admission queries live in `backend/internal/account/queries.sql` (`ListMemberGrantsByRef`, `ListAutoJoinAccountsByKeys`, `UpsertAccountMemberWithOrigin`).

Under the **single-tenant deployment profile** (ADR-057 Mode 1, E44.9 / #1833) this gate is unchanged: `account.EnsureSingleTenantAccount` bootstraps ONE `accounts` row with an `auto_join_role` at startup, so it is the only account auto-join can match and admission is scoped to the customer's enterprise / org / group. Invited rows remain the forge-independent fallback. See `backend/internal/account/README.md` and `docs/deploy/self-hosted.md`.

## GitHub App manifest flow (E4.7)

`github_manifest.go` implements `GitHubManifest.Convert(ctx, code)`, which POSTs to `https://api.github.com/app-manifests/{code}/conversions` and returns App ID + slug + OAuth client ID/secret + webhook secret + PEM.

Two handlers in `backend/internal/server/manifest.go`:

- `GET /v0/auth/github/manifest-flow-start` mints state, sets a short-lived `fishhawk_manifest_state` cookie (separate from the OAuth state cookie), and renders an auto-submitting form to GitHub's manifest endpoint.
- `GET /v0/auth/github/manifest-callback` validates state (single-use; cookie cleared on entry), exchanges the one-shot `code`, and renders an HTML success page with the secrets and a copy-paste `.env` block. `Cache-Control: no-store` keeps the page out of browser history.

Operator-facing flow in `docs/github-app/README.md`. The hosted-deploy "persist secrets to the configured backend" path is deferred.
