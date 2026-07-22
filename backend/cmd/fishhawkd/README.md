# fishhawkd

Fishhawk control-plane daemon binary: the backend HTTP API server plus its operational subcommands
(`serve.go`, `migrate.go`, `token.go`, `audit_rehash.go`).

## Webhook receiver secrets

`FISHHAWKD_GITHUB_WEBHOOK_SECRET` (`--github-webhook-secret`) enables `POST /webhooks/github` (HMAC-verified);
when unset the endpoint responds 503 and `serve.go` warns.

`FISHHAWKD_GITLAB_WEBHOOK_SECRET` (`--gitlab-webhook-secret`) enables `POST /webhooks/gitlab` (E45.6 / #1860).
GitLab sends this secret VERBATIM in `X-Gitlab-Token` (no HMAC); when unset the endpoint responds 503.
Deliberately asymmetric with GitHub: an absent GitLab secret logs nothing (GitLab is optional — an absent-warn
would nag every GitHub-only deployment). The shared webhook delivery store (`webhook_deliveries` on Postgres,
else in-memory) is created when EITHER secret is set, so a GitLab-only deployment gets the store too.

## Configurable GitHub / OAuth endpoints (E44.2 / #1826)

For GitHub Enterprise Server (Mode 1, self-hosted) and data-resident GitHub
Enterprise Cloud `<slug>.ghe.com` (Mode 2, EMU), the GitHub REST / App API and
OAuth hosts are configurable. All are **optional**: an empty value keeps the
`github.com` / `api.github.com` default, so an existing github.com deployment is
unchanged. `resolveGitHubEndpoints` (in `serve.go`) maps each env var onto the
matching per-client override; the four GitHub clients already accept a base-URL
override, so this is wiring only.

| Env var | Flag | Overrides | Default |
|---|---|---|---|
| `FISHHAWKD_GITHUB_API_URL` | `--github-api-url` | App installation-token mint + REST client base | `https://api.github.com` |
| `FISHHAWKD_GITHUB_UPLOAD_URL` | `--github-upload-url` | REST client release-asset upload host | `https://uploads.github.com` |
| `FISHHAWKD_OAUTH_AUTHORIZE_URL` | `--oauth-authorize-url` | OAuth web-flow authorize URL | `https://github.com/login/oauth/authorize` |
| `FISHHAWKD_OAUTH_TOKEN_URL` | `--oauth-token-url` | OAuth web-flow token URL | `https://github.com/login/oauth/access_token` |
| `FISHHAWKD_OAUTH_USER_URL` | `--oauth-user-url` | OAuth web-flow user-profile URL | `https://api.github.com/user` |
| `FISHHAWKD_OAUTH_ORGS_URL` | `--oauth-orgs-url` | OAuth web-flow user-orgs URL | `https://api.github.com/user/orgs` |

The forge-neutral **identity provider** (device flow + REST reads) is threaded
too: its REST base comes from `FISHHAWKD_GITHUB_API_URL`, and its device-flow /
OAuth host is derived from the scheme+host of `FISHHAWKD_OAUTH_AUTHORIZE_URL`
(an unset or unparseable value keeps `github.com`).

**Mode 2 (per-installation)** rides on top of Mode 1: when a DB pool is present,
`githubapp.Client.ResolveBaseURL` is late-bound (after the pool) to
`account.EndpointResolver`, which reads `installations.forge_base_url` for the
minting installation. A SET column overrides the deployment default for that
install; a NULL column or unknown installation falls back to the deployment
default; a **real DB error FAILS the mint** (fail-closed) rather than silently
targeting the default host. See `backend/internal/account/README.md`.

## Single-tenant deployment profile (ADR-057 Mode 1, E44.9 / #1833)

Five optional env vars bootstrap the ONE implicit account a self-hosted install admits through.
All five default **empty**, and an all-empty deployment is byte-identical to the hosted
multi-tenant posture (no bootstrap, no write).

| Env var | Flag | Effect | Empty means |
|---|---|---|---|
| `FISHHAWKD_SINGLE_TENANT_ACCOUNT_KEY` | `--single-tenant-account-key` | enterprise slug / org login / GitLab group path — **and the sole enablement signal** | hosted multi-tenant; bootstrap skipped |
| `FISHHAWKD_SINGLE_TENANT_GRANULARITY` | `--single-tenant-granularity` | `enterprise` \| `organization` \| `group` | `enterprise`, once the key is set |
| `FISHHAWKD_SINGLE_TENANT_AUTO_JOIN_ROLE` | `--single-tenant-auto-join-role` | role minted on auto-joining members | `member`, once the key is set |
| `FISHHAWKD_SINGLE_TENANT_DISPLAY_NAME` | `--single-tenant-display-name` | cosmetic | NULL |
| `FISHHAWKD_SINGLE_TENANT_PROVIDER` | `--single-tenant-provider` | `github` \| `gitlab` | `github`, once the key is set |

**The account key alone enables the profile.** The defaults above are applied INTERNALLY after
enablement, never as flag defaults — otherwise every hosted boot would look configured. So the
three states are unambiguous: nothing set → skip; key set → bootstrap with defaults filled; any
other field set with the key EMPTY → **startup error** naming `--single-tenant-account-key`. That
last one is deliberate: degrading a half-configured profile to hosted mode boots a deployment
with no admitting account, where every sign-in is denied and nothing says why.

Startup also fails on an out-of-set granularity or provider (naming the flag and the accepted
values, instead of a raw SQLSTATE 23514 from the `accounts` CHECK constraints), on a configured
profile with no `FISHHAWKD_DATABASE_URL`, and on a bootstrap write error. The upsert is idempotent
and never touches `home_region`. Contract: `backend/internal/account/README.md`; operator guide:
`docs/deploy/self-hosted.md`.

## Regional cells: handoff surface + region-scoped inference (ADR-062, E44.7 / #1831)

Four optional env vars turn this process into a *regional cell*. All four default empty, and an
all-empty deployment is byte-identical to a single-cell one.

| Env var | Flag | Effect | Empty means |
|---|---|---|---|
| `FISHHAWKD_HOME_REGION` | `--home-region` | the region THIS cell serves (`us`, `eu`, …) | region-pin surface **disabled** |
| `FISHHAWKD_HANDOFF_SECRET` | `--handoff-secret` | HMAC-SHA256 key shared with the directory plane | region-pin surface **disabled** |
| `FISHHAWKD_MODEL_BASE_URL` | `--model-base-url` | region-scoped inference endpoint for the Anthropic SDK reviewer | SDK default (`api.anthropic.com`) |
| `FISHHAWKD_MODEL_API_KEY` | `--model-api-key` | credential presented to that endpoint | falls back to `FISHHAWKD_ANTHROPIC_API_KEY` **only when `FISHHAWKD_MODEL_BASE_URL` is also empty** |

The two region-inference knobs are set **together or not at all**: either half alone is refused,
in the direction each half fails (see below).

**Pin surface construction is all-or-nothing.** `resolveRegionPin` (in `serve.go`) returns the
`(server.Config.HandoffSecret, server.Config.RegionPinner)` pair only when the region, the secret
**and** an account query surface (i.e. `FISHHAWKD_DATABASE_URL`) are all present; any one missing
returns `("", nil)` and logs once naming what is missing. Returning an empty secret even when a
secret *was* supplied is deliberate — half-configured is not a distinct posture, and it keeps the
downstream fail-closed guard depending on one condition instead of two.

Disabled is **fail closed, not permissive**: a request to the routed surface
(`server.RoutedOnboardingPath` = `GET /v0/onboarding/start`) that carries directory handoff
parameters is refused with 503 `region_pin_disabled`. Requests carrying no `fh_*` parameters pass
through untouched, which is what makes the flags a no-op for a single-cell deployment. Only that one
path is routed; the OAuth login/callback pair is deliberately not (see
`docs/deploy/regional-cells.md`).

**Region-scoped inference is process-level** (ADR-062 Q3(a)) — there is no per-account endpoint
registry. `FISHHAWKD_MODEL_BASE_URL` + `FISHHAWKD_MODEL_API_KEY` are threaded into
`anthropic.Config{BaseURL, APIKey}` by `planReviewerSet.newAnthropic`, so **both** the plan-review
and implement-review calls (one adapter, two prompt shapes) target the cell's in-region endpoint and
the review text never leaves the region. It governs the Anthropic **SDK** adapter only — the
`claudecode` and `codex` adapters are subprocesses whose endpoint is the CLI's own configuration.
`FISHHAWKD_MODEL_API_KEY` redirects a credential; it does **not** select the anthropic adapter,
which `FISHHAWKD_ANTHROPIC_API_KEY` still does.

The credential fallback is confined to the **default** endpoint. With `FISHHAWKD_MODEL_BASE_URL`
set and `FISHHAWKD_MODEL_API_KEY` unset, `inferenceAPIKey` resolves to the empty string rather than
the deployment's `FISHHAWKD_ANTHROPIC_API_KEY`: falling back there would send a production
credential — and the plan/review text it authenticates — to an operator-supplied host, which is
secret exfiltration via configurable egress. That half-configured posture fails closed (the endpoint
refuses an uncredentialed call) and logs a startup warning naming the missing key.

The **mirror** half-configuration fails closed harder. With `FISHHAWKD_MODEL_API_KEY` set and
`FISHHAWKD_MODEL_BASE_URL` unset, the SDK would fall back to its **global default** endpoint and
send both the region-scoped credential and the plan/implement-review text out of the region.
Withholding the credential is not sufficient there — the request body still travels — so the
Anthropic SDK adapter is withheld **entirely**: `planReviewerSet.Default()` skips it (falling
through to `claudecode`/`codex` if either is configured) and `For("anthropic")` refuses by name,
with a startup warning naming `FISHHAWKD_MODEL_BASE_URL`. Set the endpoint, or unset the region key
to run on the default endpoint with `FISHHAWKD_ANTHROPIC_API_KEY`.

## Work-management provider registration at startup (#1104)

`workmgmt_wiring.go` — `registerWorkmgmtProviders(cfg.GitHub, jiraClient, gitlabClient)`, called from
`serve.go`, registers each work-management provider gated on its OWN client:

- A configured **GitHub** client registers the `github_projects` work-item provider
  (`*githubclient.Client` satisfies the work-item `API` interface directly) **and** the
  product-feedback provider — the latter via `feedbackAPIAdapter`, since
  `FeedbackAPI.SearchOpenIssues` returns the workmgmt/github `MatchedIssue` type.
- A configured **Jira** client registers the `jira` work-item provider.
- A configured **GitLab** client registers the `gitlab` work-item provider
  (`*gitlabclient.Client` satisfies the gitlab `API` interface directly). It is gated on
  `FISHHAWKD_GITLAB_BASE_URL` + `FISHHAWKD_GITLAB_TOKEN` (all-or-warn, the jira precedent), built by
  `resolveGitLabClient` in `serve.go` (ADR-058 Phase 2, #1856).

An unconfigured client leaves that provider unregistered, and the affected endpoint keeps returning
**501** — the v0 not-yet-wired posture. This is the wiring behind #1104: `fishhawk_file_issue` /
`fishhawk_report_product_issue` answer 501 unless the providers are registered.

### The two GitLab surfaces are configured ASYMMETRICALLY (E44.8 / #1832)

`FISHHAWKD_GITLAB_BASE_URL` alone now enables a GitLab surface, so the
"gitlab partially configured … leaving it disabled" warning above does **not**
describe every GitLab path. Both facts, explicitly:

| Surface | Requires | Without the token |
|---|---|---|
| **Login-gate group auto-join** (`GitLabMembershipLister`, `backend/internal/auth/`) | `FISHHAWKD_GITLAB_BASE_URL` **only** — it reads `GET /api/v4/groups` with the **signing-in user's** OAuth access token | **enabled** |
| **Forge + work-item provider** (`gitlab` adapter, `fishhawk_file_issue`, …) | `FISHHAWKD_GITLAB_BASE_URL` **and** `FISHHAWKD_GITLAB_TOKEN` (PRIVATE-TOKEN) | disabled, endpoint 501 |

Startup logs both: the partial-config warning covers only the token-gated
provider, and a separate `gitlab login-gate group auto-join enabled …` line
names the lister. Note that the lister ships **seam-first** — no GitLab browser
sign-in flow exists yet, so it is not reachable in production until one lands.

### EMU enterprise auto-join (E44.8 / #1832)

Pointing `FISHHAWKD_OAUTH_AUTHORIZE_URL` at a data-resident GitHub Enterprise
Cloud host (`https://<slug>.ghe.com/login/oauth/authorize`) additionally enables
**enterprise-granularity** login-gate auto-join: the enterprise short code is
split off the EMU login (`<username>_<shortcode>`) and matched against
`enterprise`-granularity accounts carrying an `auto_join_role`. No new flag and
no extra forge call. On github.com / GHES posture no enterprise key is derived
at all — a public login cannot contain an underscore, so an ungated derivation
would be a spoofing surface. Seed such an account keyed by the enterprise SHORT
CODE. Startup logs `emu_enterprise_auto_join` and `membership_providers`.

## Per-repo work-management conventions loader + break-glass override (E45.16 / #2022)

`serve.go` installs the per-repo conventions loader after `server.New`:
`buildRepoConventionsLoader` assembles `server.RepoConventionsLoader` from the forge registry
(`registeredFileFetcher("github")` / `("gitlab")` — an absent forge yields a nil fetcher and that
provider falls through), the server's GitHub repo-scope resolution
(`srv.GitHubRepoScopeResolver()`), the deployment gitlab credential scope (non-zero exactly when
the gitlab forge is registered; the E45.5 static-token provider ignores the ref), and the
accounts provider discriminator (`account.NewResolver` over the pool — nil without a database, so
every filing then falls through to override/Default, the pre-#2022 posture). The loader fetches
`.fishhawk/work-management.yaml` from the filing repo's **own** forge, resolved via
`accounts.provider`; full contract in `backend/internal/server/README.md`.

`FISHHAWKD_WORKMGMT_CONVENTIONS` (ADR-058 Phase 2, #1856) is retained as the loader's
**break-glass fallback**, no longer THE loader: `loadConventionsOverride` still reads and parses
it fail-fast at startup — an unreadable or invalid file aborts serve with a precise error naming
the path + cause — but the parsed document is now served only when the per-repo resolution falls
through (provider not found/ambiguous, unregistered forge, no credential scope, or no committed
file). The run-absent GitHub installation-resolution branch in `workitems.go` remains gated on
`provider == github_projects`, so a gitlab filing never attempts GitHub egress.
