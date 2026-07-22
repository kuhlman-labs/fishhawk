# Self-hosted (Mode 1): the single-tenant profile

ADR-057 defines two deployment modes. This is **Mode 1** — one customer runs
`fishhawkd` inside their own perimeter against their own forge (GitHub
Enterprise Server, an EMU / data-resident `<slug>.ghe.com` tenant, or a
self-managed GitLab). For **Mode 2**, the multi-region hosted service, see
[hosted-regional.md](hosted-regional.md).

Mode 1 is not a separate build or a separate code path. It is the same
multi-tenant core (E44.1–E44.8) with tenancy **short-circuited to one implicit
tenant**: a single `accounts` row, created at startup from deployment config,
carrying an `auto_join_role`. Every member of the customer's enterprise / org /
group auto-joins that one account through the existing login gate. There is no
Mode-1 admission logic — the same E44.3 / E44.8 walk runs, with exactly one
account to match.

## Why the profile exists

`auth.MembershipResolver` admits a sign-in only against an EXISTING `accounts`
row and denies when none matches. Nothing else in the product creates that first
row, so without the profile a fresh install has **no admitting account**: every
sign-in is denied, and hand-written SQL is the only way out. The single-tenant
profile is the supported way to create it.

## Enablement: the account key, and only the account key

Every `FISHHAWKD_SINGLE_TENANT_*` variable defaults to empty.
`FISHHAWKD_SINGLE_TENANT_ACCOUNT_KEY` alone decides the mode:

| Configuration | Startup behavior |
|---|---|
| Nothing set | Bootstrap skipped. Hosted multi-tenant behavior, unchanged. |
| Account key set | Bootstrap runs. Any omitted field is filled from the internal defaults (`github` / `enterprise` / `member`). |
| Another `SINGLE_TENANT_*` field set, key EMPTY | **Startup ERROR** naming the missing `--single-tenant-account-key`. |

The third row is the load-bearing one. Silently reading a half-configured
profile as "hosted" boots a deployment with no admitting account, in which
nobody can sign in and nothing says why — the exact failure this profile
exists to prevent.

The bootstrap is idempotent (`ON CONFLICT (provider, account_key) DO UPDATE`),
so every restart converges the row on the configured profile without minting a
second account, and it never writes `home_region` — the regional pin
(`PinAccountHomeRegion`) owns that column.

### Fail-closed postures

| Configuration | Behavior |
|---|---|
| Any `SINGLE_TENANT_*` field set with an empty account key | startup error naming `--single-tenant-account-key` |
| Granularity outside `enterprise` / `organization` / `group` | startup error naming `--single-tenant-granularity` and the accepted set (rather than a raw SQLSTATE 23514 from `accounts_granularity_check`) |
| Provider outside `github` / `gitlab` | startup error naming `--single-tenant-provider` |
| Empty auto-join role (direct construction only; the flag path defaults to `member`) | startup error — `ListAutoJoinAccountsByKeys` selects only accounts whose `auto_join_role IS NOT NULL`, so a NULL role is invisible to the login gate and the account would admit nobody |
| Account key set, `FISHHAWKD_DATABASE_URL` unset | startup error — a configured profile with no database is never a silent skip |
| Bootstrap write fails | startup error carrying the DB error |

## Profile env matrix

| Env var | Flag | Empty means |
|---|---|---|
| `FISHHAWKD_SINGLE_TENANT_ACCOUNT_KEY` | `--single-tenant-account-key` | hosted multi-tenant (no bootstrap) |
| `FISHHAWKD_SINGLE_TENANT_GRANULARITY` | `--single-tenant-granularity` | `enterprise`, once the key is set |
| `FISHHAWKD_SINGLE_TENANT_AUTO_JOIN_ROLE` | `--single-tenant-auto-join-role` | `member`, once the key is set |
| `FISHHAWKD_SINGLE_TENANT_DISPLAY_NAME` | `--single-tenant-display-name` | NULL |
| `FISHHAWKD_SINGLE_TENANT_PROVIDER` | `--single-tenant-provider` | `github`, once the key is set |

The account key is the forge-neutral natural key: a GitHub enterprise slug, a
GitHub org login, or a GitLab group full path — matching the granularity.

## GHES / EMU endpoints

A self-hosted install almost always overrides the GitHub endpoints too (E44.2 /
#1826). All empty is the `github.com` / `api.github.com` posture; set them
together.

| Env var | Flag | Empty means |
|---|---|---|
| `FISHHAWKD_GITHUB_API_URL` | `--github-api-url` | `https://api.github.com` |
| `FISHHAWKD_GITHUB_UPLOAD_URL` | `--github-upload-url` | `https://uploads.github.com` |
| `FISHHAWKD_OAUTH_AUTHORIZE_URL` | `--oauth-authorize-url` | `https://github.com/login/oauth/authorize` |
| `FISHHAWKD_OAUTH_TOKEN_URL` | `--oauth-token-url` | `https://github.com/login/oauth/access_token` |
| `FISHHAWKD_OAUTH_USER_URL` | `--oauth-user-url` | `https://api.github.com/user` |
| `FISHHAWKD_OAUTH_ORGS_URL` | `--oauth-orgs-url` | `https://api.github.com/user/orgs` |

A GitLab-backed install sets `FISHHAWKD_GITLAB_BASE_URL` instead; note the two
GitLab surfaces are configured asymmetrically — the login-gate group lister
needs only the base URL (it authenticates as the signing-in user), while the
forge / work-item provider additionally needs `FISHHAWKD_GITLAB_TOKEN`. See
[gitlab.md](gitlab.md).

Also required, as in any deployment: `FISHHAWKD_DATABASE_URL`, the GitHub App
credentials (`FISHHAWKD_GITHUB_APP_ID` +
`FISHHAWKD_GITHUB_APP_PRIVATE_KEY_FILE`), and the OAuth trio
(`FISHHAWKD_OAUTH_CLIENT_ID` / `_SECRET` / `_CALLBACK_URL`) — all three of the
last must be set together or startup fails.

## Install

The image and chart are the ones the hosted service runs; only the values
differ.

```sh
helm upgrade --install fishhawk deploy/helm/fishhawk \
  -f deploy/helm/fishhawk/values-prod.yaml \
  --set singleTenant.accountKey=acme-corp \
  --set singleTenant.granularity=enterprise \
  --set config.githubApiUrl=https://ghes.acme.example/api/v3 \
  --set config.oauthAuthorizeUrl=https://ghes.acme.example/login/oauth/authorize \
  --set config.oauthTokenUrl=https://ghes.acme.example/login/oauth/access_token \
  --set config.oauthUserUrl=https://ghes.acme.example/api/v3/user \
  --set config.oauthOrgsUrl=https://ghes.acme.example/api/v3/user/orgs
```

`values-prod.yaml` carries the same block commented out as a worked example.
The chart's ConfigMap omits every empty key, so an unset profile renders no
`FISHHAWKD_SINGLE_TENANT_*` entry at all. Chart reference:
[deploy/helm/fishhawk/README.md](../../deploy/helm/fishhawk/README.md);
cluster walkthrough: [kubernetes.md](kubernetes.md).

Verify after the rollout: the startup log line
`single-tenant profile bootstrapped` names the resolved account id, key,
granularity, and auto-join role. A sign-in by a member of that
enterprise/org/group then mints an `origin='auto_join'` `account_members` row.

## How admission ends up scoped to the customer

1. The user signs in through the deployment's OAuth app (the GHES/EMU host, if
   overridden).
2. The login gate reads the user's grants. An `origin='invited'` row admits
   **DB-only** — no forge call at all. That is the forge-independent fallback:
   an operator can invite a specific member (an outside collaborator, a
   contractor) whose enterprise/org membership the forge would not report.
3. With no invited grant, the auto-join path runs one live forge read and
   intersects it with accounts whose `auto_join_role` is set — under Mode 1,
   exactly the bootstrapped account. A match mints an audited
   `origin='auto_join'` grant and admits.
4. Every derived membership key stays BOUND to the granularity it was derived
   from, so an org key never admits an enterprise-granularity account of the
   same name. A Mode-1 profile configured at `enterprise` granularity therefore
   requires EMU posture for the enterprise short code to be derivable; on a
   github.com-style posture use `organization` granularity.

Auto-join grants are re-verified against their predicate at every subsequent
login: a user who leaves the org stops being admitted, and the row is kept for
audit rather than deleted.

## What stays untenanted

CLI / bearer-token runs are not bound to an account: the account-scoped authz
check allows an untenanted run (the #1830 NULL-allow window). Under Mode 1 that
is not a cross-tenant exposure — there is exactly one tenant, so there is no
other tenant's data to reach.

## Residency posture

One cell, one database, no directory plane. Residency is a property of where the
operator runs the deployment, so none of the regional handoff configuration
(`FISHHAWKD_HOME_REGION` / `FISHHAWKD_HANDOFF_SECRET`) applies — leave both
unset and the region-pin surface stays disabled, which a single-cell deployment
never reaches. Region-scoped inference is still available per-process if the
install wants an in-region model endpoint: set `FISHHAWKD_MODEL_BASE_URL` and
`FISHHAWKD_MODEL_API_KEY` **together** (see
[regional-cells.md](regional-cells.md#region-scoped-inference) for the
fail-closed rules, which are identical here).

Per-account audit chaining is on regardless of mode — see
[ARCHITECTURE.md](../ARCHITECTURE.md) §5.1.1.
