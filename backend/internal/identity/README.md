# backend/internal/identity

Forge-neutral identity: operator verification, repo permission tier, org/team membership (E39.1 / #1706).

## IdentityProvider interface and GitHub implementation

The `IdentityProvider` interface (`identity.go`) speaks provider-qualified subjects (`github:<login>`) and a forge-neutral `Permission` vocabulary (`none`/`read`/`triage`/`write`/`maintain`/`admin`); NO `github.com/*` type crosses the boundary.

`GitHubIdentityProvider` (`github.go`) is a hand-rolled REST + OAuth-device-flow implementation (mirroring `githubclient`/`githuboidc`: net/http + encoding/json, test-overridable base URLs):

- `VerifyUser` drives the device flow to a subject.
- `VerifyAccessToken` re-verifies a CLI-obtained access token server-side to a subject (the "server-side re-verify" half of the CLI-driven device-flow login, E39.3 / #1708).
- `PermissionLevel` maps GitHub's `role_name`.
- `ResolveMembership` resolves org (`GET /orgs/{org}/members/{login}`) or team (`.../teams/{team}/memberships/{login}`).
- `Permission.AtLeast` orders the tiers for "at least" gating.
- It owns rate-limit detection net-new (`rateLimitError` → `ErrRateLimited` on 403/429 + `X-RateLimit-Remaining: 0`/`Retry-After`) because `githubclient` does none.

`NoOpIdentityProvider` (`noop.go`) is the deny-by-default fallback: `VerifyUser`/`VerifyAccessToken` fail closed with `ErrNotConfigured`.

## Wiring and first consumer

Wired via a config-gated factory: `serve.go::resolveIdentityProvider` constructs the GitHub impl only when OAuth client config is present; `server.New` defaults a nil `Config.IdentityProvider` to `identity.NewNoOp()`.

### Endpoint binding — Mode 1 only; per-installation deferred (E44.16 / #2094)

`WithBaseURLs` sets `apiBaseURL` / `oauthBaseURL` **once at construction** from the deployment-default endpoints (threaded from `FISHHAWKD_GITHUB_API_URL` / `FISHHAWKD_OAUTH_*`). Per-installation (Mode 2) routing is **deferred, not wired** — the sibling REST/GitLab clients route per installation, this provider does not — because no genuine consumer exists in the current code:

- The provider is a **boot-time singleton** and its `IdentityProvider` interface carries **no installation ref** on any method.
- `oauthBaseURL` feeds the **device-flow / OAuth login host**, and login is pre-identification: the installation is unknown until *after* the default-host device flow resolves the subject. So `oauth_base_url` (the override's device-flow leg) has no post-identification consumer here.
- `PermissionLevel` / `ResolveMembership` run post-identification but take `repo` / `subject` / `ref` (no installation ref) and read the API host — there is no seam to resolve an installation from those inputs.

Shipping a per-installation construction path exercised only by tests would be dead routing (binding condition 1 forbids it). A per-installation identity leg needs an interface that carries installation context — an operator-filed follow-up, mirroring the deferred web-OAuth leg (`backend/internal/auth`). `serve.go` still consumes the resolver's `forge_base_url` for the REST client and gitlab forge via `installationBaseURLResolver`; only the `oauth_base_url`/identity leg is deferred.

**First consumer (E39.3 / #1708):** the token-login mint handler (`server/tokens.go::handleTokenLoginMint`, `POST /v0/tokens/login`) calls `VerifyAccessToken` then gates the mint on `PermissionLevel(OperatorRepo, subject).AtLeast(OperatorMinPermission)` plus an operator-default-scope ceiling; `GET /v0/tokens/login` advertises the OAuth `client_id` for the CLI's device flow.
