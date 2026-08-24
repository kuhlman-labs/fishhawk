# backend/internal/identity

Forge-neutral identity: operator verification, repo permission tier, org/team membership (E39.1 / #1706).

## IdentityProvider interface and forge implementations

The `IdentityProvider` interface (`identity.go`) speaks provider-qualified subjects (`github:<login>`, `gitlab:<username>`) and a forge-neutral `Permission` vocabulary (`none`/`read`/`triage`/`write`/`maintain`/`admin`); NO `github.com/*` type crosses the boundary.

`GitHubIdentityProvider` (`github.go`) is a hand-rolled REST + OAuth-device-flow implementation (mirroring `githubclient`/`githuboidc`: net/http + encoding/json, test-overridable base URLs):

- `VerifyUser` drives the device flow to a subject.
- `VerifyAccessToken` re-verifies a CLI-obtained access token server-side to a subject (the "server-side re-verify" half of the CLI-driven device-flow login, E39.3 / #1708).
- `PermissionLevel` maps GitHub's `role_name`.
- `ResolveMembership` resolves org (`GET /orgs/{org}/members/{login}`) or team (`.../teams/{team}/memberships/{login}`).
- `Permission.AtLeast` orders the tiers for "at least" gating.
- It owns rate-limit detection net-new (`rateLimitError` → `ErrRateLimited` on 403/429 + `X-RateLimit-Remaining: 0`/`Retry-After`) because `githubclient` does none.

`NoOpIdentityProvider` (`noop.go`) is the deny-by-default fallback: `VerifyUser`/`VerifyAccessToken` fail closed with `ErrNotConfigured`.

### Forge-neutral seam helpers (E66.4 / #2392)

- `ProviderGitHub` / `ProviderGitLab` — the provider names that qualify a subject. Callers use the constants rather than re-spelling the discriminator as a string literal.
- `ProviderOf(subject)` — the substring before the first `:` of a `<provider>:<login>` subject; `""` for an unqualified one. Total by contract (never errors, never panics): it sits on the authorization path, where a parse failure must degrade to "unknown provider" — which every caller treats as a deny — rather than to a 500.
- `IsConfigured(p)` — reports whether `p` can actually authenticate somebody. **False for a nil interface, a typed-nil concrete provider, and `*NoOpIdentityProvider`.** This is the single primitive the server's "which providers are configured" enumeration is built on, and it lives here so the server and the fishhawkd wiring consult ONE definition. It is a **deny-list of known-inert providers, not an allow-list of blessed ones**: an unrecognized implementation is reported configured, so a future real provider is never silently dropped from discovery.

**The NoOp is never a configured provider.** It satisfies `IdentityProvider` so an unconfigured backend has something inert to hold, but it authenticates nobody. Counting non-nil interface values would advertise it as a forge and hand a client a provider from which every `VerifyAccessToken` returns `ErrNotConfigured` — the "advertises a provider that cannot authenticate anyone" state binding constraint 8 forbids. Enumerate through `IsConfigured`.

## GitLab implementation (`gitlab.go`, E66.4 / #2392)

`GitLabIdentityProvider` is the co-equal sibling of the GitHub one against a configurable GitLab base URL (SaaS or self-managed). Unlike GitHub, the OAuth endpoints and the REST API hang off the SAME host, so the provider carries one `baseURL` rather than an api/oauth pair. Constructed with `NewGitLabIdentityProvider(baseURL, deviceClientID, token)`; an empty base URL falls back to `DefaultGitLabBaseURL` and trailing slashes are trimmed. Concurrent use is safe — the struct holds only immutable config, and the production `token` accessor is a closure over an immutable captured string (no round-trip, no lock), deliberately unlike GitHub's `operatorRepoToken`.

No functional-option variadic is offered: in-package tests construct the struct directly (as `github_test.go`'s `newTestProvider` does), so an exported `Option` type with no implementations would be dead routing.

| Method | Endpoint | Notes |
|---|---|---|
| `VerifyUser` | `POST {base}/oauth/authorize_device` → `POST {base}/oauth/token` | RFC 8628 device flow, `scope=read_api`, grant `urn:ietf:params:oauth:grant-type:device_code` |
| `VerifyAccessToken` | `GET {base}/api/v4/user` | subject is `gitlab:<username>`; **submitted bearer only** |
| `PermissionLevel` | `GET {base}/api/v4/projects/{id}/members/all` | bounded paginated exact-match walk |
| `ResolveMembership` | `GET {base}/api/v4/groups/{id}/members/all` | same walk; `ref` is a group `full_path` |

### Subject shape and the foreign-provider guard

Every subject is `<provider>:<login>`. `PermissionLevel` and `ResolveMembership` accept ONLY a `gitlab:`-qualified subject; a `github:` subject, an unqualified bare login, and an empty login all deny **with zero network calls**. A `github:alice` and a `gitlab:alice` are distinct accounts, so resolving one against the other's forge would answer an authorization question about the wrong human.

### Credential separation (binding condition 1)

The deployment credential (`FISHHAWKD_GITLAB_TOKEN`, sent as `PRIVATE-TOKEN`) is **confined to `PermissionLevel` and `ResolveMembership`** — the reads that ask "what may this OTHER person do".

`VerifyAccessToken` sends **only** the submitted `Authorization: Bearer` and never the deployment credential, on any status. Two credentials on a request whose only job is to verify the caller's own token is an authentication-bypass shape: if the forge honours the deployment credential, an invalid or revoked submitted token resolves as the deployment-token user and the mint issues a fishhawkd token for an identity the caller never proved. Pinned by `TestGitLabVerifyAccessToken_DeploymentCredentialNotSent`, whose discriminating case answers 401 to the submitted token and 200 to the deployment credential and requires the call to FAIL.

A **nil** accessor sends no auth header — the anonymous-read degrade. It fails CLOSED (GitLab answers an unauthenticated caller 404 on a private project's members endpoint → `PermissionNone` → the mint gate denies) but is non-functional for the common private-repo case, so the fishhawkd wiring WARNs at boot. An accessor that **errors** propagates rather than falling back to anonymous: a broken credential must be loud, not a silent permission downgrade.

### Device application ≠ browser application (binding condition 7)

`deviceClientID` is the client_id of a **NON-Confidential** GitLab application. This provider never holds or sends a `client_secret`: RFC 8628 §3.4 specifies the device access-token request as client_id-only for a public client, while the browser sign-in leg (`backend/internal/auth/gitlab_oauth.go`) sends a `client_secret` and so requires a **Confidential** application. Hence a separately-named `FISHHAWKD_GITLAB_DEVICE_CLIENT_ID` with **no fallback** to the browser leg's client id. `TestGitLabDeviceFlow_SendsNoClientSecret` asserts neither device-flow POST carries a secret.

### `access_level` mapping

Deny-by-default: any value not listed maps to `PermissionNone` — including newly-introduced levels such as Planner or Minimal Access — matching `permissionRank`'s posture that an unrecognized tier ranks zero. A Guest therefore does not satisfy the mint's default `write` minimum.

| GitLab `access_level` | Role | `Permission` |
|---|---|---|
| 10 | Guest | `read` |
| 20 | Reporter | `triage` |
| 30 | Developer | `write` |
| 40 | Maintainer | `maintain` |
| 50 | Owner | `admin` |
| anything else | — | `none` |

### The members walk is COMPLETE, not first-page (binding constraint 9)

`PermissionLevel` and `ResolveMembership` share ONE unexported helper, `findMemberExact`, so the pagination rule, the byte cap, the page bound, the `Link` handling and the exact-match comparison exist in exactly one place and any fix reaches both. It mirrors the pattern this repo already ships and tests at `backend/internal/auth/gitlab_membership.go`:

- `per_page=100`, `page=N`; next page decided by an RFC 5988 `Link rel="next"` header when present, falling back to "the page was full" when the deployment or a proxy strips Link headers.
- **`query=` is never authoritative.** GitLab's `query` is a PARTIAL filter, so it is sent only as a server-side NARROWING optimization; the exact, case-sensitive username comparison happens client-side on every page. A GitLab that widens or ignores `query` changes only how many pages are walked, never the verdict.
- **Per-page byte cap** `gitLabMaxMemberPageBytes` (4 MiB): an oversized body is REJECTED, never truncated-and-parsed. A truncated page is a partial member set, and answering an authorization question from one is the same silent wrong answer the walk exists to prevent.
- **Page bound** `gitLabMaxMemberPages = 50` × `gitLabMembersPerPage = 100` = 5000 members. Exceeding it returns an **error** (the mint surfaces a 500), never a silent `PermissionNone`/`false`: a truncated walk that denies is an invisible wrong-authorization answer, so it must be loud.
- A **404** is a clean deny (`PermissionNone` / `false`, nil error): GitLab returns 404 rather than 403 for a resource the caller cannot see, which is also the anonymous-read degrade.
- Rate limiting → `ErrRateLimited` so the caller backs off. GitLab's headers are the un-prefixed `RateLimit-*` family; a **429** is unambiguously the rate limiter and counts with or without headers, while an ambiguous **403** counts only when it carries `Retry-After` or `RateLimit-Remaining: 0`.

`/members/all` — not `/members` — is deliberate: it includes members INHERITED from an ancestor group, so a user granted Developer on the parent group but never added directly to the project is not denied. That direction is over-permissive relative to `/members`, and the mint's `OperatorMinPermission` gate (default `write`) is the backstop.

## Wiring and first consumer

Wired via a config-gated factory: `serve.go::resolveIdentityProvider` constructs the GitHub impl only when OAuth client config is present; `server.New` defaults a nil `Config.IdentityProvider` to `identity.NewNoOp()`.

The GitLab provider ships here **ahead of its wiring** (E66.4 is decomposed producer-first): the `serve.go` factory, the `FISHHAWKD_GITLAB_DEVICE_CLIENT_ID` flag, the multi-provider `server.Config` maps and the discovery/mint plumbing land with the server slice. This package adds only new exported symbols, so it compiles and ships green with no caller changes.

### Endpoint binding — Mode 1 only; per-installation deferred (E44.16 / #2094)

`WithBaseURLs` sets `apiBaseURL` / `oauthBaseURL` **once at construction** from the deployment-default endpoints (threaded from `FISHHAWKD_GITHUB_API_URL` / `FISHHAWKD_OAUTH_*`). Per-installation (Mode 2) routing is **deferred, not wired** — the sibling REST/GitLab clients route per installation, this provider does not — because no genuine consumer exists in the current code:

- The provider is a **boot-time singleton** and its `IdentityProvider` interface carries **no installation ref** on any method.
- `oauthBaseURL` feeds the **device-flow / OAuth login host**, and login is pre-identification: the installation is unknown until *after* the default-host device flow resolves the subject. So `oauth_base_url` (the override's device-flow leg) has no post-identification consumer here.
- `PermissionLevel` / `ResolveMembership` run post-identification but take `repo` / `subject` / `ref` (no installation ref) and read the API host — there is no seam to resolve an installation from those inputs.

Shipping a per-installation construction path exercised only by tests would be dead routing (binding condition 1 forbids it). A per-installation identity leg needs an interface that carries installation context — an operator-filed follow-up, mirroring the deferred web-OAuth leg (`backend/internal/auth`). `serve.go` still consumes the resolver's `forge_base_url` for the REST client and gitlab forge via `installationBaseURLResolver`; only the `oauth_base_url`/identity leg is deferred.

**First consumer (E39.3 / #1708):** the token-login mint handler (`server/tokens.go::handleTokenLoginMint`, `POST /v0/tokens/login`) calls `VerifyAccessToken` then gates the mint on `PermissionLevel(OperatorRepo, subject).AtLeast(OperatorMinPermission)` plus an operator-default-scope ceiling; `GET /v0/tokens/login` advertises the OAuth `client_id` for the CLI's device flow.
