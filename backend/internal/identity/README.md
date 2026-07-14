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

**First consumer (E39.3 / #1708):** the token-login mint handler (`server/tokens.go::handleTokenLoginMint`, `POST /v0/tokens/login`) calls `VerifyAccessToken` then gates the mint on `PermissionLevel(OperatorRepo, subject).AtLeast(OperatorMinPermission)` plus an operator-default-scope ceiling; `GET /v0/tokens/login` advertises the OAuth `client_id` for the CLI's device flow.
